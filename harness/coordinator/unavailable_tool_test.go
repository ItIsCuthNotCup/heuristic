package coordinator

import (
	"encoding/json/jsontext"
	"reflect"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func TestCoordinatorRejectsRestoreWhenRecordedCallRequiresUnavailableTool(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     tool.CallStatus
		operations []operation.Operation
	}{
		{name: "successful translation"},
		{name: "pending operation", status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}}},
		{name: "completed operation", status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}}, operations: []operation.Operation{
			{ID: "operation-1", Status: operation.StatusCompleted},
		}},
		{name: "error with operation", status: tool.CallStatus{Error: "error", WaitingFor: []operation.ID{"operation-1"}}},
		{name: "error with recorded operation but no waiting IDs", status: tool.CallStatus{Error: "error"}, operations: []operation.Operation{
			{ID: "operation-1", Status: operation.StatusCompleted},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := emptyFakeStore()
			store.items = []sessionstore.Item{
				storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
				storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
					TurnID: "turn-1",
					Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{
						CallID: "call-1", Name: tool.BashName, Arguments: `{}`,
					}}}},
				}),
				storedItem(3, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
					TurnID: "turn-1", CallID: "call-1", Status: test.status, Operations: test.operations,
				}),
			}
			current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(),
				tool.NewRegistry(tool.StaticTranslators{Bash: &submittingTranslator{}}))
			err := current.restore(t.Context())
			if err == nil || !strings.Contains(err.Error(), `tool "Bash" required by recorded call "call-1" is not available`) {
				t.Fatalf("restore error = %v", err)
			}
		})
	}
}

func TestCoordinatorSchedulesAvailableCallAlongsideUnavailableCall(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	translator := &submittingTranslator{specs: []operation.Spec{spec}}
	store := emptyFakeStore()
	current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName))
	valid := llm.ToolCall{CallID: "valid", Name: tool.BashName, Arguments: `{}`}
	statuses, err := current.handleModelResponse(t.Context(), sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{
			{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "unknown", Name: "unknown-tool"}},
			{Type: llm.ItemToolCall, Data: valid},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || len(store.appendedStatuses) != 2 || !reflect.DeepEqual(translator.calls, []llm.ToolCall{valid}) {
		t.Fatalf("statuses = %#v, recorded = %#v, translated = %#v", statuses, store.appendedStatuses, translator.calls)
	}
	for _, status := range store.appendedStatuses {
		switch status.CallID {
		case "unknown":
			if status.Status.Error != `tool "unknown-tool" is not available` || len(status.Operations) != 0 || len(status.Status.WaitingFor) != 0 {
				t.Fatalf("unavailable call status = %#v", status)
			}
		case "valid":
			if status.Status.Error != "" || len(status.Operations) != 1 || len(status.Status.WaitingFor) != 1 {
				t.Fatalf("available call status = %#v", status)
			}
		default:
			t.Fatalf("unexpected call status = %#v", status)
		}
	}
}

func TestCoordinatorRestoresUnavailableToolCall(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "untranslated", true: "rejected"}[recorded], func(t *testing.T) {
			call := llm.ToolCall{CallID: "call-1", Name: "unknown-tool", Arguments: `{}`}
			wantError := `tool "unknown-tool" is not available`
			store := emptyFakeStore()
			store.items = []sessionstore.Item{
				storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
				storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
					TurnID:   "turn-1",
					Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}},
				}),
			}
			if recorded {
				store.items = append(store.items, storedItem(3, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
					TurnID: "turn-1", CallID: call.CallID, Status: tool.CallStatus{Error: wantError},
				}))
			}
			builder := contextbuilder.NewBuilder()
			current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), builder, tool.NewRegistry(tool.StaticTranslators{}))
			if err := current.restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			wantNewStatuses := 1
			if recorded {
				wantNewStatuses = 0
			}
			if statuses, err := current.scheduleToolCalls(t.Context()); err != nil || len(statuses) != wantNewStatuses {
				t.Fatalf("scheduled statuses = %#v, error = %v", statuses, err)
			}
			built, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			want := withPreamble(t,
				llm.Item{Type: llm.ItemToolCall, Data: call},
				llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: wantError}}}},
			)
			if !reflect.DeepEqual(built.Request.Input, want) {
				t.Fatalf("restored input = %#v, want %#v", built.Request.Input, want)
			}
			if statuses, err := current.scheduleToolCalls(t.Context()); err != nil || len(statuses) != 0 {
				t.Fatalf("rejected call was rescheduled: statuses = %#v, error = %v", statuses, err)
			}
			if len(current.state.toolCalls) != 0 || len(store.appendedStatuses) != wantNewStatuses {
				t.Fatalf("rejected call remained pending: state = %#v, statuses = %#v", current.state, store.appendedStatuses)
			}
		})
	}
}
