package operation_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

const testShellPath = "/bin/sh"

func TestShellActorCapturesArtifactsAndCompletes(t *testing.T) {
	baseDirectory := t.TempDir()
	workingDirectory := t.TempDir()
	stdout := strings.Repeat("stdout-", 8*1024)
	stderr := strings.Repeat("stderr-", 7*1024)
	t.Setenv("SHELL_STDOUT", stdout)
	t.Setenv("SHELL_STDERR", stderr)
	current := newShellOperation(t, "shell-complete", operation.ShellInput{
		Shell:     testShellPath,
		Command:   "printf '%s' \"$SHELL_STDOUT\"; printf '%s' \"$SHELL_STDERR\" >&2; exit 7",
		Directory: workingDirectory,
	}, baseDirectory, 19)

	completed := runShellActor(t, t.Context(), current)
	if completed.Status != operation.StatusCompleted {
		t.Fatalf("status = %q, want %q", completed.Status, operation.StatusCompleted)
	}
	state := shellState(t, completed)
	if state.Result == nil || state.TerminalError != "" || state.Phase != "" {
		t.Fatalf("state = %#v", state)
	}
	result := state.Result
	if result.Out != stdout[:9]+"...57325 bytes truncated; complete output in "+state.OutPath+"..."+stdout[len(stdout)-10:] ||
		result.Err != stderr[:9]+"...50157 bytes truncated; complete output in "+state.ErrPath+"..."+stderr[len(stderr)-10:] ||
		result.OutSize != int64(len(stdout)) ||
		result.ErrSize != int64(len(stderr)) ||
		result.ExitCode != 7 {
		t.Fatalf("result = %#v", result)
	}

	directory := filepath.Join(baseDirectory, string(current.ID))
	assertFileContents(t, filepath.Join(directory, operation.ShellOutFilename), []byte(stdout))
	assertFileContents(t, filepath.Join(directory, operation.ShellErrFilename), []byte(stderr))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("shell artifacts = %v, want only stdout and stderr", entries)
	}
}

func TestShellSpecRejectsZeroInlineLength(t *testing.T) {
	if _, err := operation.NewShellSpec(operation.ShellInput{Shell: testShellPath}, t.TempDir(), 0); err == nil {
		t.Fatal("zero inline length was accepted")
	}
}

func TestShellActorStartsShellDirectlyWithCapturePaths(t *testing.T) {
	baseDirectory := t.TempDir()
	current := newShellOperation(t, "shell-request", operation.ShellInput{
		Shell:     "/bin/zsh",
		Command:   "printf direct",
		Directory: "/tmp",
	}, baseDirectory, 10)

	step, err := advanceShellOnce(t, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	initial := shellState(t, *step.Operation)
	if initial.OutPath != "" || initial.ErrPath != "" {
		t.Fatal("capture paths exposed before file creation")
	}
	for index := range 3 {
		request := oneDispatchData[primitives.IOCreateRequest](t, step, primitives.PrimitiveDispatchIOCreate)
		events := make(chan primitives.PrimitiveEvent)
		primitives.Create(t.Context(), request, events)
		event := receiveOnlyPrimitiveEvent(t, events)
		step, err = advanceShellOnce(t, *step.Operation, &event)
		if err != nil {
			t.Fatal(err)
		}
		state := shellState(t, *step.Operation)
		wantOut, wantErr := "", ""
		if index >= 1 {
			wantOut = filepath.Join(baseDirectory, string(current.ID), operation.ShellOutFilename)
		}
		if index >= 2 {
			wantErr = filepath.Join(baseDirectory, string(current.ID), operation.ShellErrFilename)
		}
		if state.OutPath != wantOut || state.ErrPath != wantErr {
			t.Fatalf("capture paths after creation %d = %q, %q; want %q, %q", index, state.OutPath, state.ErrPath, wantOut, wantErr)
		}
	}

	request := oneDispatchData[primitives.ProcessStartRequest](t, step, primitives.PrimitiveDispatchProcessStart)
	directory := filepath.Join(baseDirectory, string(current.ID))
	if request.Path != "/bin/zsh" ||
		!slices.Equal(request.Arguments, []string{"-c", "printf direct"}) ||
		request.Directory != "/tmp" ||
		request.Environment != nil ||
		request.Pipes != 0 ||
		request.StdoutPath != filepath.Join(directory, operation.ShellOutFilename) ||
		request.StderrPath != filepath.Join(directory, operation.ShellErrFilename) {
		t.Fatalf("process request = %#v", request)
	}
}

func TestShellActorPersistsSignalExitStatusBeforeReading(t *testing.T) {
	baseDirectory := t.TempDir()
	id := operation.ID("shell-signaled")
	directory := filepath.Join(baseDirectory, string(id))
	createShellArtifacts(t, directory, nil, nil)
	current := shellOperationWithState(t, id, operation.ShellState{
		Input:         operation.ShellInput{Shell: testShellPath},
		BaseDirectory: baseDirectory,

		Phase:          operation.ShellPhaseProcess,
		ProcessGroupID: 4321,
	})
	event := primitives.PrimitiveEvent{
		Type:          primitives.PrimitiveEventProcessExited,
		Source:        primitives.SourceID(id),
		CorrelationID: "process",
		Result:        primitives.ProcessExitResult{ExitCode: -1, Signal: syscall.SIGTERM},
	}

	step, err := advanceShellOnce(t, current, &event)
	if err != nil {
		t.Fatal(err)
	}
	request := oneDispatchData[primitives.IOReadRequest](t, step, primitives.PrimitiveDispatchIORead)
	if request.Path != filepath.Join(directory, operation.ShellOutFilename) {
		t.Fatalf("read request = %#v", request)
	}
	state := shellState(t, *step.Operation)
	if state.Phase != operation.ShellPhaseReadOut || state.ProcessGroupID != 0 ||
		state.PendingExitCode == nil || *state.PendingExitCode != 143 {
		t.Fatalf("state = %#v", state)
	}
}

func TestShellActorFailsUnknownProcessOutcomeWithoutRestarting(t *testing.T) {
	current := shellOperationWithState(t, "shell-unknown", operation.ShellState{
		Input:         operation.ShellInput{Shell: testShellPath},
		BaseDirectory: t.TempDir(),

		Phase: operation.ShellPhaseProcess,
	})

	step, err := advanceShellOnce(t, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := shellState(t, *step.Operation)
	if step.Operation.Status != operation.StatusFailed || len(step.Dispatches) != 0 ||
		!strings.Contains(state.TerminalError, "outcome is unknown") {
		t.Fatalf("step = %#v, state = %#v", step, state)
	}
}

func TestShellActorPreservesRecordedProcessWhenFailingRecovery(t *testing.T) {
	current := shellOperationWithState(t, "shell-recover", operation.ShellState{
		Input:         operation.ShellInput{Shell: testShellPath},
		BaseDirectory: t.TempDir(),

		Phase:          operation.ShellPhaseProcess,
		ProcessGroupID: 4321,
	})

	step, err := advanceShellOnce(t, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := shellState(t, *step.Operation)
	if step.Operation.Status != operation.StatusFailed || len(step.Dispatches) != 0 ||
		state.ProcessGroupID != 4321 ||
		!strings.Contains(state.TerminalError, "interrupted before an exit status") {
		t.Fatalf("step = %#v, state = %#v", step, state)
	}
}

func TestShellActorResumesOutputReadWithRecordedExitStatus(t *testing.T) {
	baseDirectory := t.TempDir()
	id := operation.ID("shell-resume-exit")
	directory := filepath.Join(baseDirectory, string(id))
	createShellArtifacts(t, directory, []byte("out"), []byte("err"))
	exitCode := 5
	current := shellOperationWithState(t, id, operation.ShellState{
		Input:         operation.ShellInput{Shell: testShellPath},
		BaseDirectory: baseDirectory,

		Phase:           operation.ShellPhaseReadOut,
		PendingExitCode: &exitCode,
	})

	completed := runShellActor(t, t.Context(), current)
	result := shellState(t, completed).Result
	if completed.Status != operation.StatusCompleted || result == nil ||
		result.Out != "out" || result.Err != "err" || result.ExitCode != 5 {
		t.Fatalf("operation = %#v, result = %#v", completed, result)
	}
}

func TestShellActorRereadsAndReplacesInline(t *testing.T) {
	for _, output := range []string{"prefixMOREtail", ""} {
		baseDirectory := t.TempDir()
		id := operation.ID("shell-resume-read")
		directory := filepath.Join(baseDirectory, string(id))
		createShellArtifacts(t, directory, []byte(output), []byte("error"))
		exitCode := 4
		current := shellOperationWithState(t, id, operation.ShellState{
			Input:           operation.ShellInput{Shell: testShellPath},
			BaseDirectory:   baseDirectory,
			Phase:           operation.ShellPhaseReadOut,
			PendingExitCode: &exitCode,
			InlineOut:       []byte("stale"),
			InlineOutTail:   []byte("stale tail"),
		})

		current.MaxOutputLength = 10
		resumed, err := advanceShellOnce(t, current, nil)
		if err != nil {
			t.Fatal(err)
		}
		request := oneDispatchData[primitives.IOReadRequest](t, resumed, primitives.PrimitiveDispatchIORead)
		if request.Offset != 0 || request.Count != 40 {
			t.Fatalf("read request = %#v", request)
		}

		completed := runShellActor(t, t.Context(), current)
		state := shellState(t, completed)
		result := state.Result
		want := ""
		if output != "" {
			want = "prefi...4 bytes truncated; complete output in " + state.OutPath + "...Etail"
		}
		if completed.Status != operation.StatusCompleted || result == nil ||
			result.Out != want || result.OutSize != int64(len(output)) ||
			result.Err != "error" || result.ExitCode != 4 {
			t.Fatalf("operation = %#v, result = %#v", completed, result)
		}
	}
}

func TestShellActorResumesEveryReplayablePhase(t *testing.T) {
	baseDirectory := t.TempDir()
	exitCode := 5
	tests := []struct {
		name        string
		state       operation.ShellState
		dispatch    primitives.PrimitiveDispatchType
		correlation primitives.CorrelationID
		artifact    string
		kind        primitives.IOCreateKind
		mode        os.FileMode
		offset      int64
		count       int64
	}{
		{
			name: "create directory", state: operation.ShellState{Phase: operation.ShellPhaseCreateDirectory},
			dispatch: primitives.PrimitiveDispatchIOCreate, correlation: "create_directory",
			kind: primitives.IOCreateDirectory, mode: 0o700,
		},
		{
			name: "create stdout", state: operation.ShellState{Phase: operation.ShellPhaseCreateOut},
			dispatch: primitives.PrimitiveDispatchIOCreate, correlation: "create_out",
			artifact: operation.ShellOutFilename, kind: primitives.IOCreateRegularFile, mode: 0o600,
		},
		{
			name: "create stderr", state: operation.ShellState{Phase: operation.ShellPhaseCreateErr},
			dispatch: primitives.PrimitiveDispatchIOCreate, correlation: "create_err",
			artifact: operation.ShellErrFilename, kind: primitives.IOCreateRegularFile, mode: 0o600,
		},
		{
			name: "read stdout", state: operation.ShellState{
				Phase: operation.ShellPhaseReadOut, PendingExitCode: &exitCode, InlineOut: []byte("ou"),
			},
			dispatch: primitives.PrimitiveDispatchIORead, correlation: "read_out",
			artifact: operation.ShellOutFilename, offset: 0, count: 40,
		},
		{
			name: "read stderr", state: operation.ShellState{
				Phase: operation.ShellPhaseReadErr, PendingExitCode: &exitCode, InlineErr: []byte("err"),
			},
			dispatch: primitives.PrimitiveDispatchIORead, correlation: "read_err",
			artifact: operation.ShellErrFilename, offset: 0, count: 40,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id := operation.ID("resume-" + strings.ReplaceAll(test.name, " ", "-"))
			state := test.state
			state.Input = operation.ShellInput{Shell: testShellPath}
			state.BaseDirectory = baseDirectory
			current := shellOperationWithState(t, id, state)
			current.MaxOutputLength = 10

			step, err := advanceShellOnce(t, current, nil)
			if err != nil {
				t.Fatal(err)
			}
			if step.Operation.Status != operation.StatusAwaiting || len(step.Dispatches) != 1 ||
				step.Dispatches[0].Type != test.dispatch {
				t.Fatalf("step = %#v", step)
			}
			directory := filepath.Join(baseDirectory, string(id))
			path := directory
			if test.artifact != "" {
				path = filepath.Join(directory, test.artifact)
			}
			switch test.dispatch {
			case primitives.PrimitiveDispatchIOCreate:
				request := dispatchData[primitives.IOCreateRequest](t, step.Dispatches[0])
				if request.Source != primitives.SourceID(id) || request.CorrelationID != test.correlation ||
					request.Path != path || request.Kind != test.kind || request.Mode != test.mode {
					t.Fatalf("create request = %#v", request)
				}
			case primitives.PrimitiveDispatchIORead:
				request := dispatchData[primitives.IOReadRequest](t, step.Dispatches[0])
				if request.Source != primitives.SourceID(id) || request.CorrelationID != test.correlation ||
					request.Path != path || request.Offset != test.offset || request.Count != test.count {
					t.Fatalf("read request = %#v", request)
				}
			default:
				t.Fatalf("unexpected dispatch %q", test.dispatch)
			}
		})
	}
}

func TestShellActorRequiresRecordedExitStatus(t *testing.T) {
	base := t.TempDir()
	id := operation.ID("missing-exit-status")
	createShellArtifacts(t, filepath.Join(base, string(id)), nil, nil)
	current := shellOperationWithState(t, id, operation.ShellState{
		Input: operation.ShellInput{Shell: testShellPath}, BaseDirectory: base,
		Phase: operation.ShellPhaseReadOut,
	})
	failed := runShellActor(t, t.Context(), current)
	state := shellState(t, failed)
	if failed.Status != operation.StatusFailed || !strings.Contains(state.TerminalError, "without an exit status") {
		t.Fatalf("operation = %#v, state = %#v", failed, state)
	}
}

func TestShellActorFailsWhenShellCannotStart(t *testing.T) {
	current := newShellOperation(t, "shell-start-failure", operation.ShellInput{
		Shell: filepath.Join(t.TempDir(), "missing-shell"),
	}, t.TempDir(), 10)

	failed := runShellActor(t, t.Context(), current)
	state := shellState(t, failed)
	if failed.Status != operation.StatusFailed || state.TerminalError == "" || state.Result != nil {
		t.Fatalf("operation = %#v, state = %#v", failed, state)
	}
}

func TestNewShellSpecRejectsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct {
		input         operation.ShellInput
		baseDirectory string
		maxInline     int
	}{
		{input: operation.ShellInput{}, baseDirectory: "/tmp"},
		{input: operation.ShellInput{Shell: "sh"}, baseDirectory: "/tmp"},
		{input: operation.ShellInput{Shell: testShellPath}, baseDirectory: "relative"},
		{input: operation.ShellInput{Shell: testShellPath}, baseDirectory: "/tmp", maxInline: -1},
	} {
		if _, err := operation.NewShellSpec(test.input, test.baseDirectory, test.maxInline); err == nil {
			t.Fatalf("NewShellSpec accepted %#v", test)
		}
	}
}

func TestShellActorValidatesPersistedStateAndIdentity(t *testing.T) {
	valid := operation.ShellState{
		Input:         operation.ShellInput{Shell: testShellPath},
		BaseDirectory: "/tmp",

		Phase: operation.ShellPhaseReadOut,
	}
	for _, mutate := range []func(*operation.ShellState){
		func(state *operation.ShellState) { state.ProcessGroupID = -1 },
		func(state *operation.ShellState) { state.ProcessGroupID = 1 },
		func(state *operation.ShellState) { state.OutSize = -1 },
		func(state *operation.ShellState) { state.ErrSize = -1 },
		func(state *operation.ShellState) { state.InlineOut = []byte("xxxxx") },
		func(state *operation.ShellState) { state.InlineErr = []byte("xxxxx") },
	} {
		state := valid
		mutate(&state)
		current := shellOperationWithState(t, "shell-invalid", state)
		current.MaxOutputLength = 1
		if _, err := advanceShellOnce(t, current, nil); err == nil {
			t.Fatalf("Shell accepted state %#v", state)
		}
	}

	current := shellOperationWithState(t, "../invalid", valid)
	step, err := advanceShellOnce(t, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if step.Operation.Status != operation.StatusFailed {
		t.Fatalf("operation = %#v", step.Operation)
	}
}

func TestShellActorCancellationIsTerminal(t *testing.T) {
	current := newShellOperation(t, "shell-cancel", operation.ShellInput{Shell: testShellPath}, t.TempDir(), 10)
	step, err := advanceShellOnce(t, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	step.Operation.Status = operation.StatusCanceling

	canceled, err := advanceShellOnce(t, *step.Operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := shellState(t, *canceled.Operation)
	if canceled.Operation.Status != operation.StatusCanceled || len(canceled.Dispatches) != 0 || state.TerminalError == "" {
		t.Fatalf("step = %#v, state = %#v", canceled, state)
	}
}

func TestShellActorCancellationPreservesRecordedProcess(t *testing.T) {
	current := shellOperationWithState(t, "shell-cancel-process", operation.ShellState{
		Input:         operation.ShellInput{Shell: testShellPath},
		BaseDirectory: t.TempDir(),

		Phase:          operation.ShellPhaseProcess,
		ProcessGroupID: 4321,
	})
	current.Status = operation.StatusCanceling

	canceled, err := advanceShellOnce(t, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := shellState(t, *canceled.Operation)
	if canceled.Operation.Status != operation.StatusCanceled ||
		len(canceled.Dispatches) != 0 || state.ProcessGroupID != 4321 || state.Phase != "" {
		t.Fatalf("operation = %#v, state = %#v", canceled.Operation, state)
	}
}

func TestShellActorCancellationStopsActiveProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	current := newShellOperation(t, "shell-cancel-active", operation.ShellInput{
		Shell:   testShellPath,
		Command: "exec sleep 30",
	}, t.TempDir(), 10)
	processGroupID := 0

	canceled := runShellActorObserved(t, ctx, current, func(current operation.Operation, event primitives.PrimitiveEvent) {
		if event.Type != primitives.PrimitiveEventProcessStarted {
			return
		}
		started, ok := event.Result.(primitives.ProcessStartedResult)
		if !ok || started.PID <= 1 {
			t.Fatalf("started = %#v", event.Result)
		}
		processGroupID = started.PID
		if state := shellState(t, current); state.ProcessGroupID != processGroupID {
			t.Fatalf("state process group = %d, want %d", state.ProcessGroupID, processGroupID)
		}
		cancel()
	})

	state := shellState(t, canceled)
	if canceled.Status != operation.StatusCanceled || state.Phase != "" ||
		state.ProcessGroupID != processGroupID || state.Result != nil {
		t.Fatalf("operation = %#v, state = %#v", canceled, state)
	}
	if err := syscall.Kill(-processGroupID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("inspect canceled process group %d: %v, want ESRCH", processGroupID, err)
	}
}

func runShellActor(t *testing.T, ctx context.Context, current operation.Operation) operation.Operation {
	t.Helper()
	return runShellActorObserved(t, ctx, current, nil)
}

func runShellActorObserved(
	t *testing.T,
	ctx context.Context,
	current operation.Operation,
	observe func(operation.Operation, primitives.PrimitiveEvent),
) operation.Operation {
	t.Helper()
	runtimeContext, cancelRuntime := context.WithCancel(t.Context())
	defer cancelRuntime()

	shell, err := operation.NewShell(current)
	if err != nil {
		t.Fatal(err)
	}
	step, err := shell.Handle(nil)
	if err != nil {
		t.Fatal(err)
	}
	var active *primitiveInvocation
	defer func() {
		cancelRuntime()
		if active != nil {
			drainPrimitiveInvocation(active.events)
		}
	}()

	current = *step.Operation
	for current.Status == operation.StatusAwaiting {
		if len(step.Dispatches) > 1 {
			t.Fatalf("dispatches = %#v, want at most one", step.Dispatches)
		}
		if len(step.Dispatches) == 1 {
			if active != nil {
				t.Fatalf("primitive %q is still active", active.correlationID)
			}
			invocation := startPrimitive(t, runtimeContext, step.Dispatches[0])
			active = &invocation
		}
		if active == nil {
			t.Fatal("shell actor is awaiting no active primitive")
		}

		select {
		case <-ctx.Done():
			cancelRuntime()
			drainPrimitiveInvocation(active.events)
			active = nil
			step, err = shell.Handle(&primitives.PrimitiveEvent{Type: primitives.PrimitiveEventCanceled, Source: primitives.SourceID(current.ID)})

		case event, open := <-active.events:
			if !open {
				t.Fatalf("primitive %q closed while actor was awaiting", active.correlationID)
			}
			if event.Source != active.source || event.CorrelationID != active.correlationID {
				t.Fatalf("event identity = (%q, %q), want (%q, %q)",
					event.Source, event.CorrelationID, active.source, active.correlationID)
			}
			if primitiveEventIsTerminal(event) {
				active = nil
			}
			step, err = shell.Handle(&event)
			if err == nil && step.Operation != nil && observe != nil {
				observe(*step.Operation, event)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		if step.Operation != nil {
			current = *step.Operation
		}
	}
	return current
}

type primitiveInvocation struct {
	source        primitives.SourceID
	correlationID primitives.CorrelationID
	events        <-chan primitives.PrimitiveEvent
}

func startPrimitive(
	t *testing.T,
	ctx context.Context,
	dispatch operation.PrimitiveDispatch,
) primitiveInvocation {
	t.Helper()
	events := make(chan primitives.PrimitiveEvent)
	switch dispatch.Type {
	case primitives.PrimitiveDispatchIOCreate:
		request := dispatchData[primitives.IOCreateRequest](t, dispatch)
		primitives.Create(ctx, request, events)
		return primitiveInvocation{request.Source, request.CorrelationID, events}
	case primitives.PrimitiveDispatchIORead:
		request := dispatchData[primitives.IOReadRequest](t, dispatch)
		primitives.ReadFile(ctx, request, events)
		return primitiveInvocation{request.Source, request.CorrelationID, events}
	case primitives.PrimitiveDispatchProcessStart:
		request := dispatchData[primitives.ProcessStartRequest](t, dispatch)
		primitives.StartProcess(ctx, request, events)
		return primitiveInvocation{request.Source, request.CorrelationID, events}
	default:
		t.Fatalf("unexpected dispatch type %q", dispatch.Type)
		return primitiveInvocation{}
	}
}

func primitiveEventIsTerminal(event primitives.PrimitiveEvent) bool {
	switch event.Type {
	case primitives.PrimitiveEventIOCreateCompleted,
		primitives.PrimitiveEventIOReadCompleted,
		primitives.PrimitiveEventProcessExited,
		primitives.PrimitiveEventFailed,
		primitives.PrimitiveEventCanceled:
		return true
	default:
		return false
	}
}

func drainPrimitiveInvocation(events <-chan primitives.PrimitiveEvent) {
	for {
		if primitiveEventIsTerminal(<-events) {
			return
		}
	}
}

func oneDispatchData[T any](
	t *testing.T,
	step operation.Step,
	dispatchType primitives.PrimitiveDispatchType,
) T {
	t.Helper()
	if len(step.Dispatches) != 1 || step.Dispatches[0].Type != dispatchType {
		t.Fatalf("dispatches = %#v, want one %q", step.Dispatches, dispatchType)
	}
	return dispatchData[T](t, step.Dispatches[0])
}

func dispatchData[T any](t *testing.T, dispatch operation.PrimitiveDispatch) T {
	t.Helper()
	data, ok := dispatch.Data.(T)
	if !ok {
		t.Fatalf("dispatch data = %T, want %T", dispatch.Data, *new(T))
	}
	return data
}

func receiveOnlyPrimitiveEvent(
	t *testing.T,
	events <-chan primitives.PrimitiveEvent,
) primitives.PrimitiveEvent {
	t.Helper()
	event, open := <-events
	if !open {
		t.Fatal("primitive closed without an event")
	}
	select {
	case event, open := <-events:
		if !open {
			t.Fatal("primitive closed the caller-owned channel")
		}
		t.Fatalf("primitive returned another event: %#v", event)
	default:
	}
	return event
}

func newShellOperation(
	t *testing.T,
	id operation.ID,
	input operation.ShellInput,
	baseDirectory string,
	maxOutputLength int,
) operation.Operation {
	t.Helper()
	spec, err := operation.NewShellSpec(input, baseDirectory, maxOutputLength)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{
		ID:              id,
		Type:            spec.Type,
		Version:         spec.Version,
		Status:          operation.StatusReady,
		MaxOutputLength: spec.MaxOutputLength, State: spec.State,
		Idempotency: spec.Idempotency,
	}
}

func shellOperationWithState(t *testing.T, id operation.ID, state operation.ShellState) operation.Operation {
	t.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{
		ID:              id,
		MaxOutputLength: operation.DefaultMaxOutputLength,
		Type:            operation.TypeShell,
		Version:         operation.VersionShell,
		Status:          operation.StatusAwaiting,
		State:           encoded,
	}
}

func shellState(t *testing.T, current operation.Operation) operation.ShellState {
	t.Helper()
	var state operation.ShellState
	if err := json.Unmarshal(current.State, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func createShellArtifacts(t *testing.T, directory string, out []byte, errOutput []byte) {
	t.Helper()
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string][]byte{
		operation.ShellOutFilename: out,
		operation.ShellErrFilename: errOutput,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertFileContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("contents of %q = %q, want %q", path, got, want)
	}
}

func TestShellReadsActualTailWithBoundedState(t *testing.T) {
	for _, test := range []struct {
		name, text, want string
		limit            int
	}{
		{"large ASCII", "HEAD" + strings.Repeat("x", 1024*1024) + "TAIL", "HEAD...1048575 bytes truncated...xTAIL", 9},
		{"four byte characters", strings.Repeat("🙂", 20), "🙂🙂...64 bytes truncated...🙂🙂", 4},
		{"one four byte character", strings.Repeat("🙂", 20), "...76 bytes truncated...🙂", 1},
		{"odd Unicode budget", strings.Repeat("🙂", 20), "🙂...68 bytes truncated...🙂🙂", 3},
		{"split UTF8 boundaries", "界é" + strings.Repeat("🙂", 20) + "好é界", "界é🙂...76 bytes truncated...好é界", 6},
		{"invalid UTF8", "AB" + strings.Repeat("x", 100) + "\xff\xfeZ", "AB...100 bytes truncated...��Z", 5},
		{"control characters", "\n\"" + strings.Repeat("x", 100) + "\x00\\\t", "\n\"xxxx...93 bytes truncated...xxx\x00\\\t", 12},
		{"one character", "start" + strings.Repeat("x", 100) + "end", "...107 bytes truncated...d", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			id := operation.ID("tail")
			directory := filepath.Join(base, string(id))
			createShellArtifacts(t, directory, []byte(test.text), []byte(test.text))
			exitCode := 0
			current := shellOperationWithState(t, id, operation.ShellState{
				Input: operation.ShellInput{Shell: testShellPath}, BaseDirectory: base,
				Phase: operation.ShellPhaseReadOut, PendingExitCode: &exitCode,
			})
			current.MaxOutputLength = test.limit
			tailReads := 0
			completed := runShellActorObserved(t, t.Context(), current, func(update operation.Operation, event primitives.PrimitiveEvent) {
				state := shellState(t, update)
				if len(state.InlineOut)+len(state.InlineOutTail) > 4*test.limit || len(state.InlineErr)+len(state.InlineErrTail) > 4*test.limit {
					t.Fatal("captured state exceeds its byte budget")
				}
				if event.Type == primitives.PrimitiveEventIOReadCompleted && (event.CorrelationID == "read_out_tail" || event.CorrelationID == "read_err_tail") {
					tailReads++
				}
			})
			state := shellState(t, completed)
			if completed.Status != operation.StatusCompleted || state.Result == nil {
				t.Fatalf("operation failed: %#v", state)
			}
			wantOut := strings.Replace(test.want, " bytes truncated...", " bytes truncated; complete output in "+state.OutPath+"...", 1)
			wantErr := strings.Replace(test.want, " bytes truncated...", " bytes truncated; complete output in "+state.ErrPath+"...", 1)
			if state.Result.Out != wantOut || state.Result.Err != wantErr || !state.OutTruncated || !state.ErrTruncated || tailReads != 2 {
				t.Fatalf("output = %q, error = %q, tail reads = %d; want %q", state.Result.Out, state.Result.Err, tailReads, wantOut)
			}
			if state.Result.OutSize != int64(len(test.text)) || state.Result.ErrSize != int64(len(test.text)) {
				t.Fatal("original output byte counts changed")
			}
			if len(state.InlineOut)+len(state.InlineErr)+len(state.InlineOutTail)+len(state.InlineErrTail) != 0 {
				t.Fatal("completed operation retained capture buffers")
			}
			assertFileContents(t, state.OutPath, []byte(test.text))
			assertFileContents(t, state.ErrPath, []byte(test.text))
		})
	}
}

func TestShellReadsTailOnlyForStreamsExceedingTheByteBudget(t *testing.T) {
	for _, test := range []struct {
		name, out, err, wantOut, wantErr string
		tailReads                        []primitives.CorrelationID
	}{
		{
			name: "empty", out: "", err: "", wantOut: "", wantErr: "",
		},
		{
			name: "byte budget boundary", out: "abcdefghijklmnop", err: "🙂🙂🙂🙂",
			wantOut: "ab...12 bytes truncated; complete output in {path}...op", wantErr: "🙂🙂🙂🙂",
		},
		{
			name: "stdout only", out: "abcdefghijklmnopq", err: "err",
			wantOut: "ab...13 bytes truncated; complete output in {path}...pq", wantErr: "err",
			tailReads: []primitives.CorrelationID{"read_out_tail"},
		},
		{
			name: "stderr only", out: "界é🙂", err: "abcdefghijklmnopq",
			wantOut: "界é🙂", wantErr: "ab...13 bytes truncated; complete output in {path}...pq",
			tailReads: []primitives.CorrelationID{"read_err_tail"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			id := operation.ID("mixed-output")
			createShellArtifacts(t, filepath.Join(base, string(id)), []byte(test.out), []byte(test.err))
			exitCode := 0
			current := shellOperationWithState(t, id, operation.ShellState{
				Input: operation.ShellInput{Shell: testShellPath}, BaseDirectory: base,
				Phase: operation.ShellPhaseReadOut, PendingExitCode: &exitCode,
			})
			current.MaxOutputLength = 4
			var tailReads []primitives.CorrelationID
			completed := runShellActorObserved(t, t.Context(), current, func(_ operation.Operation, event primitives.PrimitiveEvent) {
				if event.Type == primitives.PrimitiveEventIOReadCompleted && (event.CorrelationID == "read_out_tail" || event.CorrelationID == "read_err_tail") {
					tailReads = append(tailReads, event.CorrelationID)
				}
			})
			state := shellState(t, completed)
			if completed.Status != operation.StatusCompleted || state.Result == nil {
				t.Fatalf("operation failed: %#v", state)
			}
			wantOut := strings.ReplaceAll(test.wantOut, "{path}", state.OutPath)
			wantErr := strings.ReplaceAll(test.wantErr, "{path}", state.ErrPath)
			if state.Result.Out != wantOut || state.Result.Err != wantErr || !slices.Equal(tailReads, test.tailReads) {
				t.Fatalf("output = %q, error = %q, tail reads = %v; want %q, %q, %v", state.Result.Out, state.Result.Err, tailReads, wantOut, wantErr, test.tailReads)
			}
		})
	}
}

func TestShellRereadsAndReplacesTail(t *testing.T) {
	text := "HEAD" + strings.Repeat("x", 100) + "TAIL!"
	for _, phase := range []operation.ShellPhase{operation.ShellPhaseReadOutTail, operation.ShellPhaseReadErrTail} {
		t.Run(string(phase), func(t *testing.T) {
			base := t.TempDir()
			id := operation.ID("resume-tail")
			createShellArtifacts(t, filepath.Join(base, string(id)), []byte(text), []byte(text))
			exitCode := 0
			state := operation.ShellState{
				Input: operation.ShellInput{Shell: testShellPath}, BaseDirectory: base,
				Phase: phase, PendingExitCode: &exitCode,
				InlineOut: []byte(text[:20]), OutSize: int64(len(text)),
				InlineOutTail: []byte("stale"),
			}
			if phase == operation.ShellPhaseReadErrTail {
				state.InlineOutTail = []byte(text[len(text)-20:])
				state.InlineErr, state.ErrSize = []byte(text[:20]), int64(len(text))
				state.InlineErrTail = []byte("stale")
			}
			current := shellOperationWithState(t, id, state)
			current.MaxOutputLength = 10
			step, err := advanceShellOnce(t, current, nil)
			if err != nil {
				t.Fatal(err)
			}
			request := oneDispatchData[primitives.IOReadRequest](t, step, primitives.PrimitiveDispatchIORead)
			if request.Offset != int64(len(text)-20) || request.Count != 20 {
				t.Fatalf("tail resume request = %#v", request)
			}
			completed := runShellActor(t, t.Context(), *step.Operation)
			result := shellState(t, completed).Result
			if completed.Status != operation.StatusCompleted || result == nil || result.Out != "HEADx...99 bytes truncated; complete output in "+shellState(t, completed).OutPath+"...TAIL!" || result.Err != "HEADx...99 bytes truncated; complete output in "+shellState(t, completed).ErrPath+"...TAIL!" {
				t.Fatalf("result after resume = %#v", result)
			}
		})
	}
}

func TestShellRejectsInvalidTailEvents(t *testing.T) {
	for _, test := range []struct {
		name  string
		event primitives.PrimitiveEvent
	}{
		{"wrong offset", primitives.PrimitiveEvent{Type: primitives.PrimitiveEventIOReadOutput, Result: primitives.IOReadOutputResult{Offset: 79, Data: []byte("x")}}},
		{"oversized chunk", primitives.PrimitiveEvent{Type: primitives.PrimitiveEventIOReadOutput, Result: primitives.IOReadOutputResult{Offset: 80, Data: bytes.Repeat([]byte("x"), 21)}}},
		{"incomplete tail", primitives.PrimitiveEvent{Type: primitives.PrimitiveEventIOReadCompleted, Result: primitives.IOReadCompletedResult{Size: 100}}},
		{"changed size", primitives.PrimitiveEvent{Type: primitives.PrimitiveEventIOReadCompleted, Result: primitives.IOReadCompletedResult{Size: 101}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := operation.ShellState{
				Input: operation.ShellInput{Shell: testShellPath}, BaseDirectory: t.TempDir(),
				Phase: operation.ShellPhaseReadOutTail, OutSize: 100, InlineOut: bytes.Repeat([]byte("h"), 20),
			}
			current := shellOperationWithState(t, "invalid-tail", state)
			current.MaxOutputLength = 10
			event := test.event
			event.Source, event.CorrelationID = "invalid-tail", "read_out_tail"
			shell, err := operation.NewShell(current)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "changed size" {
				if _, err := shell.Handle(&primitives.PrimitiveEvent{
					Source: "invalid-tail", CorrelationID: "read_out_tail", Type: primitives.PrimitiveEventIOReadOutput,
					Result: primitives.IOReadOutputResult{Offset: 80, Data: bytes.Repeat([]byte("t"), 20)},
				}); err != nil {
					t.Fatal(err)
				}
			}
			step, err := shell.Handle(&event)
			if err != nil {
				t.Fatal(err)
			}
			if step.Operation.Status != operation.StatusFailed || len(step.Dispatches) != 0 {
				t.Fatalf("invalid tail event was accepted: %#v", step)
			}
		})
	}
}

func advanceShellOnce(t *testing.T, current operation.Operation, event *primitives.PrimitiveEvent) (operation.Step, error) {
	t.Helper()
	shell, err := operation.NewShell(current)
	if err != nil {
		return operation.Step{}, err
	}
	return shell.Handle(event)
}
