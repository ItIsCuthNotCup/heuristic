package coordinator

import (
	"reflect"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore/localfile"
)

func TestCoordinatorReplaysToolResultBalance(t *testing.T) {
	store, registry := independentToolCalls(t, 2)
	store.resume.Operations = nil
	completion := func(index int) sessionstore.ToolCallStatus {
		status := store.items[index+2].Data.(sessionstore.ToolCallStatus)
		value := status.Operations[0]
		value.Status = operation.StatusCompleted
		status.Operations = []operation.Operation{value}
		return status
	}
	first, second := completion(0), completion(1)
	for _, step := range []struct {
		name                                               string
		kind                                               sessionstore.ItemKind
		data                                               any
		pendingCalls, completed, delivered, pendingResults int
	}{
		{"first completion", sessionstore.ItemToolCallStatus, first, 1, 1, 0, 1},
		{"request starts", sessionstore.ItemTurn, session.Turn{ID: "delivery-1", PreviousTurnID: "turn-1", Type: session.TurnRegular}, 1, 1, 0, 1},
		{"second completes during request", sessionstore.ItemToolCallStatus, second, 0, 2, 0, 2},
		{"duplicate completion", sessionstore.ItemToolCallStatus, second, 0, 2, 0, 2},
		{"request completes", sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "delivery-1", Response: textResponse("Checking.")}, 0, 2, 1, 1},
		{"next request starts", sessionstore.ItemTurn, session.Turn{ID: "delivery-2", PreviousTurnID: "delivery-1", Type: session.TurnRegular}, 0, 2, 1, 1},
		{"next request completes", sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "delivery-2", Response: textResponse("Done.")}, 0, 2, 2, 0},
	} {
		store.items = append(store.items, storedItem(sessionstore.Sequence(len(store.items)+1), step.kind, step.data))
		t.Run(step.name, func(t *testing.T) {
			current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), registry)
			if err := current.restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := len(current.state.toolCalls); got != step.pendingCalls {
				t.Fatalf("pending calls = %d, want %d", got, step.pendingCalls)
			}
			if got := current.state.availableInputs; got != step.completed {
				t.Fatalf("completed results = %d, want %d", got, step.completed)
			}
			built, err := current.dependencies.ContextBuilder.Build()
			if err != nil {
				t.Fatal(err)
			}
			appended := 0
			for _, item := range built.Request.Input {
				if item.Type == llm.ItemToolResult && item.Data.(llm.ToolResult).Output[0].Value == string(operation.StatusCompleted) {
					appended++
				}
			}
			if appended != current.state.availableInputs {
				t.Fatalf("appended completions = %d, available inputs = %d", appended, current.state.availableInputs)
			}
			if got := current.state.deliveredInputs; got != step.delivered {
				t.Fatalf("delivered results = %d, want %d", got, step.delivered)
			}
			if got := current.pendingInputs(); got != step.pendingResults {
				t.Fatalf("pending results = %d, want %d", got, step.pendingResults)
			}
		})
	}
}

func TestCoordinatorCompactionPreservesPendingInputsOnReplay(t *testing.T) {
	run := newStopTestRun(t, 2)
	directory := t.TempDir()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	current := run.current
	current.dependencies.Sessions = store
	status := run.store.items[2].Data.(sessionstore.ToolCallStatus)
	completed := status.Operations[0]
	completed.Status = operation.StatusCompleted
	status.Operations = []operation.Operation{completed}
	items := append(run.store.items,
		sessionstore.Item{Kind: sessionstore.ItemInput, Data: externalEvent(t, 0, "before", "Before compaction")},
		sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "compact", PreviousTurnID: "turn-1", Type: session.TurnCompaction}},
		sessionstore.Item{Kind: sessionstore.ItemInput, Data: externalEvent(t, 0, "during", "During compaction")},
		sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: status},
	)
	for _, item := range items {
		if _, err := current.addItemToLocalState(item); err != nil {
			t.Fatal(err)
		}
		if err := current.storeItemInSessionStore(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	before, err := current.dependencies.ContextBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	response := sessionstore.ModelResponse{TurnID: "compact", Response: textResponse("Summary")}
	response.Response.Output = append(response.Response.Output, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "summary-call", Name: "unknown"}})
	current.state.callModel = true
	statuses, err := current.handleModelResponse(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 || !current.state.callModel {
		t.Fatal("compaction response changed ordinary scheduling")
	}
	if _, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: response}); err != nil {
		t.Fatal(err)
	}
	if current.state.currentTurnType != session.TurnCompaction || current.state.deliveredInputs != 0 || current.pendingInputs() != 3 || len(current.state.toolCalls) != 1 || len(current.state.operations) != 2 {
		t.Fatalf("compaction changed pending work: %+v", current.state)
	}
	reopened, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reopened.Items(t.Context(), "session-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Items[len(page.Items)-1].Data, response) {
		t.Fatal("compaction response was not persisted")
	}
	replayed := newStopTestRun(t, 0).current
	replayed.dependencies.Sessions = reopened
	if err := replayed.loadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	wantState := current.state
	wantState.callModel = false
	if !reflect.DeepEqual(replayed.state, wantState) {
		t.Fatalf("replayed state = %+v, want %+v", replayed.state, wantState)
	}
	for _, builder := range []contextbuilder.Builder{current.dependencies.ContextBuilder, replayed.dependencies.ContextBuilder} {
		built, err := builder.Build()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(built.Request, before.Request) {
			t.Fatal("compaction changed active context")
		}
	}
}
