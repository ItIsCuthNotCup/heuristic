package localfile

import (
	"encoding/json/jsontext"
	"errors"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

var (
	stateCreatedAt = time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	stateUpdatedAt = time.Date(2026, 8, 27, 9, 1, 0, 0, time.UTC)
)

func TestStoredStateLifecycle(t *testing.T) {
	state := newStoredState("session-1", stateCreatedAt)
	input := inbox.Input{
		ID:      "input-1",
		Kind:    inbox.InputExternal,
		Payload: jsontext.Value(`{"message":"hello"}`),
	}
	if err := state.appendInput(input, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := state.appendTurn(session.Turn{ID: "turn-1", Type: session.TurnRegular}, stateUpdatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	response := sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleAssistant, Text: "hello"},
		}}},
	}
	if err := state.appendModelResponse(response, stateUpdatedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	operations := []operation.Operation{
		validOperation("ready", operation.StatusReady),
		validOperation("complete", operation.StatusCompleted),
	}
	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: "call-1",
		Status: tool.CallStatus{WaitingFor: []operation.ID{"ready", "complete"}},
	}
	if err := state.appendToolCallStatus(status, operations, stateUpdatedAt.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	updated := operations[0]
	updated.Status = operation.StatusAwaiting
	updated.State = jsontext.Value(`{"step":2}`)
	if err := state.saveOperation(updated); err != nil {
		t.Fatal(err)
	}

	wantKinds := []sessionstore.ItemKind{
		sessionstore.ItemInput,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemToolCallStatus,
	}
	if len(state.Items) != len(wantKinds) {
		t.Fatalf("items = %d, want %d", len(state.Items), len(wantKinds))
	}
	for index, wantKind := range wantKinds {
		item := state.Items[index]
		if item.Sequence != sessionstore.Sequence(index+1) || item.Kind != wantKind {
			t.Fatalf("item %d = %#v", index, item)
		}
	}
	if !state.Items[0].RecordedAt.Equal(stateUpdatedAt) {
		t.Fatalf("first recorded time = %v, want %v", state.Items[0].RecordedAt, stateUpdatedAt)
	}
	resume := state.resume()
	if len(resume.Operations) != 1 || !reflect.DeepEqual(resume.Operations[0], updated) {
		t.Fatalf("resumed operations = %#v, want %#v", resume.Operations, updated)
	}
	if !reflect.DeepEqual(resume.ExternalInputIDs, []inbox.ID{"input-1"}) {
		t.Fatalf("external input IDs = %#v", resume.ExternalInputIDs)
	}
}

func TestResumeReturnsOnlyExternalInputIDs(t *testing.T) {
	state := newStoredState("session-1", stateCreatedAt)
	for _, input := range []inbox.Input{
		{ID: "external-1", Kind: inbox.InputExternal},
		{ID: "control-1", Kind: inbox.InputControl, Payload: []byte(`{"Mode":"hard"}`)},
		{ID: "crash-1", Kind: inbox.InputCrash},
		{ID: "external-2", Kind: inbox.InputExternal},
	} {
		if err := state.appendInput(input, stateUpdatedAt); err != nil {
			t.Fatal(err)
		}
	}

	if got := state.resume().ExternalInputIDs; !reflect.DeepEqual(
		got,
		[]inbox.ID{"external-1", "external-2"},
	) {
		t.Fatalf("external input IDs = %#v", got)
	}
}

func TestStatusOperationReferences(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		state := stateWithTurn(t)
		status := sessionstore.ToolCallStatus{
			TurnID: "turn-1",
			CallID: "call-1",
			Status: tool.CallStatus{Error: "invalid arguments"},
		}
		if err := state.appendToolCallStatus(status, nil, stateUpdatedAt); err != nil {
			t.Fatal(err)
		}
		if len(state.Items) != 2 || len(state.Operations) != 0 {
			t.Fatalf("state = %#v", state)
		}
	})

	t.Run("exact set in different order", func(t *testing.T) {
		state := stateWithTurn(t)
		operations := []operation.Operation{
			validOperation("operation-1", operation.StatusReady),
			validOperation("operation-2", operation.StatusReady),
		}
		status := validStatus("turn-1", "call-1", "operation-2", "operation-1")
		if err := state.appendToolCallStatus(status, operations, stateUpdatedAt); err != nil {
			t.Fatal(err)
		}
		if len(state.Items) != 2 || !reflect.DeepEqual(state.Operations, operations) {
			t.Fatalf("state = %#v", state)
		}
	})

	tests := []struct {
		name       string
		status     tool.CallStatus
		operations []operation.Operation
		want       string
	}{
		{
			name: "empty success",
			want: "must initialize at least one operation",
		},
		{
			name:   "error waits for operation",
			status: tool.CallStatus{Error: "invalid", WaitingFor: []operation.ID{"operation-1"}},
			want:   "error status cannot wait for operations",
		},
		{
			name:       "error initializes operation",
			status:     tool.CallStatus{Error: "invalid"},
			operations: []operation.Operation{validOperation("operation-1", operation.StatusReady)},
			want:       "error status cannot initialize operations",
		},
		{
			name:       "reference count differs",
			status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
			operations: []operation.Operation{validOperation("operation-1", operation.StatusReady), validOperation("operation-2", operation.StatusReady)},
			want:       "waits for 1 operations, initialized 2",
		},
		{
			name:       "reference was not initialized",
			status:     tool.CallStatus{WaitingFor: []operation.ID{"operation-2"}},
			operations: []operation.Operation{validOperation("operation-1", operation.StatusReady)},
			want:       `waits for operation "operation-2" that was not initialized`,
		},
		{
			name:   "duplicate reference",
			status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1", "operation-1"}},
			operations: []operation.Operation{
				validOperation("operation-1", operation.StatusReady),
				validOperation("operation-2", operation.StatusReady),
			},
			want: `repeats operation "operation-1"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := stateWithTurn(t)
			before := cloneStoredState(state)
			err := state.appendToolCallStatus(sessionstore.ToolCallStatus{
				TurnID: "turn-1",
				CallID: "call-1",
				Status: test.status,
			}, test.operations, stateUpdatedAt)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if !reflect.DeepEqual(state, before) {
				t.Fatalf("rejected relationship changed state\nbefore: %#v\nafter:  %#v", before, state)
			}
		})
	}
}

func TestRepeatedToolCallStatusAddsItem(t *testing.T) {
	state := stateWithStatus(t)
	status := validStatus("turn-1", "call-1", "operation-1")
	operations := slices.Clone(state.Operations)

	if err := state.appendToolCallStatus(status, nil, stateUpdatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 3 || !reflect.DeepEqual(state.Items[2].Data, status) {
		t.Fatalf("items = %#v", state.Items)
	}
	if !reflect.DeepEqual(state.Operations, operations) {
		t.Fatalf("operations = %#v, want %#v", state.Operations, operations)
	}
}

func TestRejectedStateTransitionsDoNotMutateState(t *testing.T) {
	tests := []struct {
		name  string
		state func(*testing.T) storedState
		apply func(*storedState) error
		is    error
	}{
		{
			name:  "empty input ID",
			state: emptyTestState,
			apply: func(state *storedState) error {
				return state.appendInput(inbox.Input{}, stateUpdatedAt)
			},
		},
		{
			name:  "invalid input JSON",
			state: emptyTestState,
			apply: func(state *storedState) error {
				return state.appendInput(inbox.Input{
					ID: "input-1", Kind: inbox.InputExternal, Payload: jsontext.Value(`{`),
				}, stateUpdatedAt)
			},
		},
		{
			name:  "empty turn ID",
			state: emptyTestState,
			apply: func(state *storedState) error {
				return state.appendTurn(session.Turn{}, stateUpdatedAt)
			},
		},
		{
			name:  "wrong previous turn",
			state: stateWithTurn,
			apply: func(state *storedState) error {
				return state.appendTurn(session.Turn{ID: "turn-2", PreviousTurnID: "other", Type: session.TurnRegular}, stateUpdatedAt)
			},
		},
		{
			name:  "duplicate turn",
			state: stateWithTurn,
			apply: func(state *storedState) error {
				return state.appendTurn(session.Turn{ID: "turn-1", PreviousTurnID: "turn-1", Type: session.TurnRegular}, stateUpdatedAt)
			},
			is: fs.ErrExist,
		},
		{
			name:  "response for missing turn",
			state: emptyTestState,
			apply: func(state *storedState) error {
				return state.appendModelResponse(validResponse("missing"), stateUpdatedAt)
			},
			is: fs.ErrNotExist,
		},
		{
			name:  "duplicate response",
			state: stateWithResponse,
			apply: func(state *storedState) error {
				return state.appendModelResponse(validResponse("turn-1"), stateUpdatedAt)
			},
			is: fs.ErrExist,
		},
		{
			name:  "status for missing turn",
			state: emptyTestState,
			apply: func(state *storedState) error {
				return state.appendToolCallStatus(validStatus("missing", "call-1"), nil, stateUpdatedAt)
			},
			is: fs.ErrNotExist,
		},
		{
			name:  "empty call ID",
			state: stateWithTurn,
			apply: func(state *storedState) error {
				return state.appendToolCallStatus(validStatus("turn-1", ""), nil, stateUpdatedAt)
			},
		},
		{
			name:  "invalid operation after valid operation",
			state: stateWithTurn,
			apply: func(state *storedState) error {
				return state.appendToolCallStatus(
					validStatus("turn-1", "call-1", "operation-1", "operation-2"),
					[]operation.Operation{
						validOperation("operation-1", operation.StatusReady),
						{ID: "operation-2", Status: operation.StatusReady},
					},
					stateUpdatedAt,
				)
			},
		},
		{
			name:  "duplicate operation in batch",
			state: stateWithTurn,
			apply: func(state *storedState) error {
				value := validOperation("operation-1", operation.StatusReady)
				return state.appendToolCallStatus(
					validStatus("turn-1", "call-1", "operation-1", "operation-1"),
					[]operation.Operation{value, value},
					stateUpdatedAt,
				)
			},
			is: fs.ErrExist,
		},
		{
			name:  "operation already exists",
			state: stateWithStatus,
			apply: func(state *storedState) error {
				return state.appendToolCallStatus(
					validStatus("turn-1", "call-2", "operation-1"),
					[]operation.Operation{validOperation("operation-1", operation.StatusReady)},
					stateUpdatedAt,
				)
			},
			is: fs.ErrExist,
		},
		{
			name:  "invalid operation update",
			state: stateWithStatus,
			apply: func(state *storedState) error {
				return state.saveOperation(operation.Operation{
					ID:      "operation-1",
					Status:  operation.StatusCompleted,
					Version: 1,
				})
			},
		},
		{
			name:  "missing operation update",
			state: stateWithStatus,
			apply: func(state *storedState) error {
				return state.saveOperation(validOperation("missing", operation.StatusCompleted))
			},
			is: fs.ErrNotExist,
		},
		{
			name:  "operation type changes",
			state: stateWithStatus,
			apply: func(state *storedState) error {
				value := validOperation("operation-1", operation.StatusCompleted)
				value.Type = "changed"
				return state.saveOperation(value)
			},
		},
		{
			name:  "operation version changes",
			state: stateWithStatus,
			apply: func(state *storedState) error {
				value := validOperation("operation-1", operation.StatusCompleted)
				value.Version = 2
				return state.saveOperation(value)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := test.state(t)
			before := cloneStoredState(state)
			err := test.apply(&state)
			if err == nil {
				t.Fatal("transition succeeded")
			}
			if test.is != nil && !errors.Is(err, test.is) {
				t.Fatalf("error = %v, want %v", err, test.is)
			}
			if !reflect.DeepEqual(state, before) {
				t.Fatalf("rejected transition changed state\nbefore: %#v\nafter:  %#v", before, state)
			}
		})
	}
}

func TestValidateOperation(t *testing.T) {
	valid := validOperation("operation-1", operation.StatusReady)
	tests := []struct {
		name  string
		value operation.Operation
		want  string
	}{
		{name: "empty ID", value: operation.Operation{}, want: "operation ID is empty"},
		{
			name:  "empty type",
			value: operation.Operation{ID: "operation-1"},
			want:  "type is empty",
		},
		{
			name:  "zero version",
			value: operation.Operation{ID: "operation-1", Type: "test"},
			want:  "version is zero",
		},
		{
			name: "unsupported status",
			value: operation.Operation{
				ID: "operation-1", Type: "test", Version: 1, Status: "unknown",
			},
			want: "unsupported status",
		},
		{
			name: "invalid state",
			value: operation.Operation{
				ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
				State: jsontext.Value(`{`),
			},
			want: "state is not valid JSON",
		},
		{
			name: "invalid idempotency",
			value: operation.Operation{
				ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
				Idempotency: jsontext.Value(`{`),
			},
			want: "idempotency data is not valid JSON",
		},
		{name: "valid", value: valid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateOperation(test.value)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestForkStoredStateUsesLastRecordForTurn(t *testing.T) {
	parent := newStoredState("parent", stateCreatedAt)
	if err := parent.appendInput(inbox.Input{
		ID: "input-1", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendTurn(session.Turn{ID: "turn-1", Type: session.TurnRegular}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendModelResponse(validResponse("turn-1", "call-1"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendToolCallStatus(
		validStatus("turn-1", "call-1", "operation-1"),
		[]operation.Operation{validOperation("operation-1", operation.StatusReady)},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendInput(inbox.Input{
		ID: "input-2", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendTurn(
		session.Turn{ID: "turn-2", PreviousTurnID: "turn-1", Type: session.TurnRegular},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	before := cloneStoredState(parent)

	child, err := forkStoredState(parent, "child", "turn-1", stateUpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parent, before) {
		t.Fatal("fork changed parent state")
	}
	if child.Snapshot.Session.ID != "child" {
		t.Fatalf("child snapshot = %#v", child.Snapshot)
	}
	if len(child.Items) != 5 || child.Items[3].Kind != sessionstore.ItemToolCallStatus ||
		child.Items[4].Kind != sessionstore.ItemFork {
		t.Fatalf("child items = %#v", child.Items)
	}
	if len(child.Operations) != 0 {
		t.Fatalf("child operations = %#v", child.Operations)
	}
	if _, err := forkStoredState(parent, "missing", "unknown", stateUpdatedAt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing turn error = %v, want fs.ErrNotExist", err)
	}
}

func TestForkStoredStateIgnoresDelayedStatusBeyondNewerTurn(t *testing.T) {
	parent := newStoredState("parent", stateCreatedAt)
	if err := parent.appendTurn(session.Turn{ID: "turn-1", Type: session.TurnRegular}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendModelResponse(validResponse("turn-1", "call-1"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendTurn(
		session.Turn{ID: "turn-2", PreviousTurnID: "turn-1", Type: session.TurnRegular},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendModelResponse(validResponse("turn-2"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendToolCallStatus(
		validStatus("turn-1", "call-1", "operation-1"),
		[]operation.Operation{validOperation("operation-1", operation.StatusReady)},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	child, err := forkStoredState(parent, "child", "turn-1", stateUpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertItemKinds(t, child.Items,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemFork,
	)

	child, err = forkStoredState(parent, "child-2", "turn-2", stateUpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertItemKinds(t, child.Items,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemFork,
	)
}

func TestForkStoredStatePreservesInterleavedPrefix(t *testing.T) {
	parent := newStoredState("parent", stateCreatedAt)
	if err := parent.appendTurn(session.Turn{ID: "turn-1", Type: session.TurnRegular}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendModelResponse(validResponse("turn-1"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendInput(inbox.Input{
		ID: "steering", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendToolCallStatus(
		validStatus("turn-1", "call-1", "operation-1"),
		[]operation.Operation{validOperation("operation-1", operation.StatusReady)},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := parent.appendTurn(
		session.Turn{ID: "turn-2", PreviousTurnID: "turn-1", Type: session.TurnRegular},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}

	child, err := forkStoredState(parent, "child", "turn-1", stateUpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertItemKinds(t, child.Items,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemInput,
		sessionstore.ItemToolCallStatus,
		sessionstore.ItemFork,
	)
	encoded, err := encodeInitialLog(child.Snapshot.Session, child.Items)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeLog(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestForkStoredStateScopesDelayedStatusesToOwningBranch(t *testing.T) {
	root := newStoredState("root", stateCreatedAt)
	if err := root.appendTurn(session.Turn{ID: "root-turn", Type: session.TurnRegular}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := root.appendModelResponse(validResponse("root-turn"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}

	child, err := forkStoredState(root, "child", "root-turn", stateUpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	before := cloneStoredState(child)
	if err := child.appendToolCallStatus(
		validStatus("root-turn", "late-root-call", "root-operation"),
		[]operation.Operation{validOperation("root-operation", operation.StatusReady)},
		stateUpdatedAt,
	); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("inherited status error = %v, want fs.ErrNotExist", err)
	}
	if !reflect.DeepEqual(child, before) {
		t.Fatal("rejected inherited status changed child")
	}

	if err := child.appendTurn(
		session.Turn{ID: "child-turn-1", PreviousTurnID: "root-turn", Type: session.TurnRegular},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := child.appendModelResponse(validResponse("child-turn-1"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := child.appendTurn(
		session.Turn{ID: "child-turn-2", PreviousTurnID: "child-turn-1", Type: session.TurnRegular},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := child.appendModelResponse(validResponse("child-turn-2"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := child.appendToolCallStatus(
		validStatus("child-turn-1", "late-child-call", "child-operation"),
		[]operation.Operation{validOperation("child-operation", operation.StatusReady)},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}

	grandchild, err := forkStoredState(child, "grandchild", "child-turn-1", stateUpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	assertItemKinds(t, grandchild.Items,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemFork,
		sessionstore.ItemTurn,
		sessionstore.ItemModelResponse,
		sessionstore.ItemFork,
	)
	encoded, err := encodeInitialLog(grandchild.Snapshot.Session, grandchild.Items)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeLog(encoded); err != nil {
		t.Fatal(err)
	}
}

func assertItemKinds(t *testing.T, items []sessionstore.Item, want ...sessionstore.ItemKind) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("items = %#v, want kinds %v", items, want)
	}
	for index, kind := range want {
		if items[index].Kind != kind {
			t.Fatalf("item %d kind = %q, want %q", index, items[index].Kind, kind)
		}
	}
}

func emptyTestState(*testing.T) storedState {
	return newStoredState("session-1", stateCreatedAt)
}

func stateWithTurn(t *testing.T) storedState {
	t.Helper()
	state := emptyTestState(t)
	if err := state.appendTurn(session.Turn{ID: "turn-1", Type: session.TurnRegular}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	return state
}

func stateWithResponse(t *testing.T) storedState {
	t.Helper()
	state := stateWithTurn(t)
	if err := state.appendModelResponse(validResponse("turn-1"), stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	return state
}

func stateWithStatus(t *testing.T) storedState {
	t.Helper()
	state := stateWithTurn(t)
	if err := state.appendToolCallStatus(
		validStatus("turn-1", "call-1", "operation-1"),
		[]operation.Operation{validOperation("operation-1", operation.StatusReady)},
		stateUpdatedAt,
	); err != nil {
		t.Fatal(err)
	}
	return state
}

func validResponse(turnID session.TurnID, callIDs ...string) sessionstore.ModelResponse {
	output := make([]llm.Item, len(callIDs))
	for index, callID := range callIDs {
		output[index] = llm.Item{
			Type: llm.ItemToolCall,
			Data: llm.ToolCall{CallID: callID, Name: "test", Arguments: `{}`},
		}
	}
	return sessionstore.ModelResponse{TurnID: turnID, Response: llm.Response{Output: output}}
}

func validStatus(
	turnID session.TurnID,
	callID string,
	waitingFor ...operation.ID,
) sessionstore.ToolCallStatus {
	return sessionstore.ToolCallStatus{
		TurnID: turnID,
		CallID: callID,
		Status: tool.CallStatus{WaitingFor: waitingFor},
	}
}

func validOperation(id operation.ID, status operation.Status) operation.Operation {
	return operation.Operation{
		ID:      id,
		Type:    "test",
		Version: 1,
		Status:  status,
		State:   jsontext.Value(`{"step":1}`),
	}
}

func cloneStoredState(state storedState) storedState {
	state.Items = slices.Clone(state.Items)
	state.Operations = slices.Clone(state.Operations)
	state.turns = maps.Clone(state.turns)
	state.ownedTurns = maps.Clone(state.ownedTurns)
	state.respondedTurns = maps.Clone(state.respondedTurns)
	state.toolCallStatuses = maps.Clone(state.toolCallStatuses)
	state.operationPositions = maps.Clone(state.operationPositions)
	return state
}
