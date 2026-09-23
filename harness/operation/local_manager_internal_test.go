package operation

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

func TestShellReadChunksAreTransientAndRecoveryRereads(t *testing.T) {
	for _, phase := range []ShellPhase{ShellPhaseReadOut, ShellPhaseReadErr, ShellPhaseReadOutTail, ShellPhaseReadErrTail} {
		t.Run(string(phase), func(t *testing.T) {
			exitCode := 0
			state := ShellState{
				Input: ShellInput{Shell: "/bin/sh"}, BaseDirectory: t.TempDir(),
				Phase: phase, PendingExitCode: &exitCode,
			}
			contents := []byte("abcdefghijkl")
			fullSize := int64(len(contents))
			switch phase {
			case ShellPhaseReadOut:
				state.InlineOut = []byte("stale")
			case ShellPhaseReadErr:
				state.InlineErr = []byte("stale")
			case ShellPhaseReadOutTail:
				state.InlineOutTail = []byte("stale")
			case ShellPhaseReadErrTail:
				state.InlineErrTail = []byte("stale")
			}
			if phase == ShellPhaseReadOutTail {
				state.InlineOut, state.OutSize = []byte(strings.Repeat("h", 12)), 100
				fullSize = 100
			}
			if phase == ShellPhaseReadErrTail {
				state.InlineErr, state.ErrSize = []byte(strings.Repeat("h", 12)), 100
				fullSize = 100
			}
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			current := Operation{ID: "shell", Type: TypeShell, Version: VersionShell, Status: StatusAwaiting, State: encoded, MaxOutputLength: 6}
			execution, err := NewShell(current)
			if err != nil {
				t.Fatal(err)
			}
			start, err := execution.Handle(nil)
			if err != nil || start.Operation == nil {
				t.Fatalf("start read: %v", err)
			}
			durable := start.Operation
			request := start.Dispatches[0].Data.(primitives.IOReadRequest)
			original := durable.State.Clone()
			chunks := [][]byte{contents[:2], contents[2:]}
			offset := request.Offset
			for _, chunk := range chunks {
				step, err := execution.Handle(&primitives.PrimitiveEvent{
					Type: primitives.PrimitiveEventIOReadOutput, Source: request.Source, CorrelationID: request.CorrelationID,
					Result: primitives.IOReadOutputResult{Offset: offset, Data: chunk},
				})
				if err != nil || step.Operation != nil || !bytes.Equal(original, durable.State) {
					t.Fatalf("chunk produced a durable change: step=%#v, error=%v", step, err)
				}
				offset += int64(len(chunk))
			}
			if len(execution.chunks) != 2 || &execution.chunks[0][0] != &chunks[0][0] || &execution.chunks[1][0] != &chunks[1][0] {
				t.Fatal("read did not retain the original chunks")
			}

			recovered, err := NewShell(*durable)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := recovered.Handle(nil)
			if err != nil || replay.Operation == nil {
				t.Fatalf("resume read: %v", err)
			}
			if replay.Dispatches[0].Data.(primitives.IOReadRequest) != request {
				t.Fatal("recovery skipped transient chunks instead of rereading them")
			}
			step, err := recovered.Handle(&primitives.PrimitiveEvent{
				Type: primitives.PrimitiveEventIOReadOutput, Source: request.Source, CorrelationID: request.CorrelationID,
				Result: primitives.IOReadOutputResult{Offset: request.Offset, Data: contents},
			})
			if err != nil || step.Operation != nil {
				t.Fatalf("replayed chunk produced a step: %#v, %v", step, err)
			}
			completion := &primitives.PrimitiveEvent{
				Type: primitives.PrimitiveEventIOReadCompleted, Source: request.Source, CorrelationID: request.CorrelationID,
				Result: primitives.IOReadCompletedResult{Size: fullSize},
			}
			finished, err := execution.Handle(completion)
			if err != nil || finished.Operation == nil {
				t.Fatalf("finish read: %v", err)
			}
			assembled, err := DecodeShellState(*finished.Operation)
			if err != nil {
				t.Fatal(err)
			}
			switch phase {
			case ShellPhaseReadOut:
				if !bytes.Equal(assembled.InlineOut, contents) {
					t.Fatalf("assembled stdout = %q, want %q", assembled.InlineOut, contents)
				}
			case ShellPhaseReadOutTail:
				if !bytes.Equal(assembled.InlineOutTail, contents) {
					t.Fatalf("assembled stdout tail = %q, want %q", assembled.InlineOutTail, contents)
				}
			case ShellPhaseReadErr, ShellPhaseReadErrTail:
				if finished.Operation.Status != StatusCompleted || assembled.Result == nil || assembled.Result.ExitCode != 0 {
					t.Fatal("stderr read did not complete the operation")
				}
				want := "abc...6 bytes truncated; complete output in " + request.Path + "...jkl"
				if phase == ShellPhaseReadErrTail {
					want = "hhh...94 bytes truncated; complete output in " + request.Path + "...jkl"
				}
				if assembled.Result.Err != want || len(assembled.InlineErr)+len(assembled.InlineErrTail) != 0 {
					t.Fatalf("assembled stderr = %q, want %q and no retained buffers", assembled.Result.Err, want)
				}
			}
			replayed, err := recovered.Handle(completion)
			if err != nil || !reflect.DeepEqual(replayed, finished) {
				t.Fatalf("recovered result differs: %v", err)
			}
		})
	}
}

func TestShellReadValidatesChunksAndDiscardsThemOnTermination(t *testing.T) {
	for _, scenario := range []string{"wrong source", "wrong correlation", "wrong offset", "oversized", "invalid payload", "incomplete read", "invalid completion", "canceled", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			encoded, err := json.Marshal(ShellState{
				Input: ShellInput{Shell: "/bin/sh"}, BaseDirectory: t.TempDir(), Phase: ShellPhaseReadOut,
			})
			if err != nil {
				t.Fatal(err)
			}
			current := Operation{ID: "shell", Type: TypeShell, Version: VersionShell, Status: StatusAwaiting, State: encoded, MaxOutputLength: 3}
			execution, err := NewShell(current)
			if err != nil {
				t.Fatal(err)
			}
			start, err := execution.Handle(nil)
			if err != nil {
				t.Fatal(err)
			}
			if start.Operation == nil {
				t.Fatal("read start has no checkpoint")
			}
			event := primitives.PrimitiveEvent{
				Type: primitives.PrimitiveEventIOReadOutput, Source: "shell", CorrelationID: "read_out",
				Result: primitives.IOReadOutputResult{Offset: 0, Data: []byte("ab")},
			}
			if step, err := execution.Handle(&event); step.Operation != nil || err != nil {
				t.Fatalf("first chunk: %#v, %v", step, err)
			}
			event.Result = primitives.IOReadOutputResult{Offset: 2, Data: []byte("c")}
			switch scenario {
			case "wrong source":
				event.Source = "other"
			case "wrong correlation":
				event.CorrelationID = "read_err"
			case "wrong offset":
				event.Result = primitives.IOReadOutputResult{Offset: 0, Data: []byte("c")}
			case "oversized":
				event.Result = primitives.IOReadOutputResult{Offset: 2, Data: []byte(strings.Repeat("c", 11))}
			case "invalid payload":
				event.Result = "invalid"
			case "incomplete read":
				event.Type, event.Result = primitives.PrimitiveEventIOReadCompleted, primitives.IOReadCompletedResult{Size: 3}
			case "invalid completion":
				event.Type, event.Result = primitives.PrimitiveEventIOReadCompleted, "invalid"
			case "canceled":
				event.Type, event.Result = primitives.PrimitiveEventCanceled, nil
			case "failed":
				event.Type, event.Result = primitives.PrimitiveEventFailed, primitives.PrimitiveFailureResult{Error: "read failed"}
			}
			step, err := execution.Handle(&event)
			if err != nil || step.Operation == nil || (step.Operation.Status != StatusCanceled && step.Operation.Status != StatusFailed) {
				t.Fatalf("termination: %#v, %v", step, err)
			}
			state, err := DecodeShellState(*step.Operation)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.InlineOut) != 0 || len(execution.chunks) != 0 {
				t.Fatal("termination retained transient output")
			}
		})
	}
}

func TestLocalOperationManagerStartsMultiplePrimitives(t *testing.T) {
	id := ID("local-multiple-primitives")
	events := make(chan primitives.PrimitiveEvent)
	manager := &LocalOperationManager{primitiveEvents: events}
	ctx, cancel := context.WithCancel(t.Context())
	current := &localRunningOperation{
		ctx:    ctx,
		cancel: cancel,
		operation: Operation{
			ID:     id,
			Status: StatusAwaiting,
		},
	}
	operations := map[ID]*localRunningOperation{id: current}
	if started := manager.acceptLocalStep(operations, current, Step{}); started != 0 {
		t.Fatalf("empty step started %d primitives", started)
	}
	directory := t.TempDir()
	dispatch := func(correlation primitives.CorrelationID, name string) PrimitiveDispatch {
		return PrimitiveDispatch{
			Type: primitives.PrimitiveDispatchIOCreate,
			Data: primitives.IOCreateRequest{
				Source:        primitives.SourceID(id),
				CorrelationID: correlation,
				Kind:          primitives.IOCreateRegularFile,
				Path:          filepath.Join(directory, name),
			},
		}
	}
	step := Step{
		Dispatches: []PrimitiveDispatch{
			dispatch("first", "first"),
			dispatch("second", "second"),
		},
	}

	started := manager.acceptLocalStep(operations, current, step)
	if started != 2 {
		t.Fatalf("started primitives = %d, want 2", started)
	}
	if len(manager.pendingUpdates) != 0 || current.operation.Status != StatusAwaiting || ctx.Err() != nil {
		t.Fatal("step without a checkpoint changed the operation or published an update")
	}

	completed := current.operation
	completed.Status = StatusCompleted
	manager.acceptLocalStep(operations, current, Step{Operation: &completed})
	if len(operations) != 0 {
		t.Fatalf("operations = %d, want 0", len(operations))
	}
	if len(manager.pendingUpdates) != 1 || manager.pendingUpdates[0].Status != StatusCompleted {
		t.Fatalf("checkpoint updates = %#v", manager.pendingUpdates)
	}
	manager.drainLocalPrimitives(started)
}
