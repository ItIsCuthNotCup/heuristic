package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

func TestCoordinatorRestoresSession(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	input := externalEvent(t, 1, "input-1", "hello")
	turn := session.Turn{ID: "turn-1", Type: session.TurnRegular}
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	response := llm.Response{
		ID: "response-1",
		Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "working"}},
			{Type: llm.ItemToolCall, Data: call},
		},
	}
	status := sessionstore.ToolCallStatus{
		TurnID: turn.ID,
		CallID: call.CallID,
		Status: tool.CallStatus{Error: "invalid arguments"},
	}
	resumedOperation := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusAwaiting,
	}
	store := &fakeStore{
		resume: sessionstore.ResumeState{
			Snapshot: sessionstore.Snapshot{
				Session: session.Session{ID: "session-1"},
			},
			Operations: []operation.Operation{resumedOperation},
		},
		items: []sessionstore.Item{
			storedItem(1, sessionstore.ItemInput, input),
			storedItem(2, sessionstore.ItemTurn, turn),
			storedItem(3, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
				TurnID: turn.ID, Response: response,
			}),
			storedItem(4, sessionstore.ItemToolCallStatus, status),
		},
	}
	builder := contextbuilder.NewBuilder()
	inputs := newTestInbox(t)
	current := newTestCoordinator(store, inputs, newFakeOperationManager(), builder, registry)

	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}

	if current.state.currentTurnID != turn.ID ||
		current.state.currentTurnType != session.TurnRegular ||
		!reflect.DeepEqual(current.state.operations[resumedOperation.ID], resumedOperation) {
		t.Fatalf("restored state = %#v", current.state)
	}

	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	wantInput := withPreamble(t,
		llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hello"}},
		response.Output[0],
		response.Output[1],
		llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{
			CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "error:invalid arguments"}},
		}},
	)
	if !reflect.DeepEqual(built.Request.Input, wantInput) {
		t.Fatalf("built request = %#v, want input %#v", built.Request, wantInput)
	}
	if !reflect.DeepEqual(store.itemRequests, []itemRequest{{After: 0, Limit: historyPageSize}}) {
		t.Fatalf("item requests = %#v", store.itemRequests)
	}
}

func TestCoordinatorAppliesTurnTypes(t *testing.T) {
	current := newStopTestRun(t, 0).current
	for _, turnType := range []session.TurnType{session.TurnRegular, session.TurnCompaction, session.TurnRegular, session.TurnCompaction} {
		turn := session.Turn{ID: "turn", Type: turnType}
		if _, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: turn}); err != nil {
			t.Fatal(err)
		}
		if current.state.currentTurnType != turnType {
			t.Fatalf("current turn type = %q, want %q", current.state.currentTurnType, turnType)
		}
	}
}

func TestCoordinatorRestoresOrdinaryResponseDuringCompaction(t *testing.T) {
	run := newStopTestRun(t, 0)
	response := textResponse("Ordinary response")
	response.Output = append(response.Output, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "old-call", Name: "unknown"}})
	run.store.items = []sessionstore.Item{
		storedItem(1, sessionstore.ItemInput, externalEvent(t, 0, "input", "hello")),
		storedItem(2, sessionstore.ItemTurn, session.Turn{ID: "ordinary", Type: session.TurnRegular}),
		storedItem(3, sessionstore.ItemTurn, session.Turn{ID: "compact", PreviousTurnID: "ordinary", Type: session.TurnCompaction}),
		storedItem(4, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "ordinary", Response: response}),
	}
	if err := run.current.loadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	built, err := run.current.dependencies.ContextBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(t, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hello"}})
	want = append(want, response.Output...)
	if !reflect.DeepEqual(built.Request.Input, want) || run.current.state.currentTurnType != session.TurnCompaction || run.current.pendingInputs() != 1 || len(run.current.state.toolCalls) != 1 {
		t.Fatal("earlier ordinary response was lost or treated as a compaction response")
	}
}

func TestCoordinatorRestoresPaginatedForkHistory(t *testing.T) {
	parentInput := externalEvent(t, 1, "parent-input", "parent")
	items := []sessionstore.Item{storedItem(1, sessionstore.ItemInput, parentInput)}
	for sequence := sessionstore.Sequence(2); sequence <= historyPageSize; sequence++ {
		items = append(items, storedItem(
			sequence,
			sessionstore.ItemTurn,
			session.Turn{ID: session.TurnID(fmt.Sprintf("turn-%d", sequence)), Type: session.TurnRegular},
		))
	}
	items = append(items,
		storedItem(historyPageSize+1, sessionstore.ItemFork, sessionstore.Fork{
			ParentID: "parent", PreviousTurnID: "turn-256",
		}),
		storedItem(historyPageSize+2, sessionstore.ItemInput, externalEvent(
			t, 1, "child-input", "child",
		)),
	)
	store := &fakeStore{
		resume: sessionstore.ResumeState{Snapshot: sessionstore.Snapshot{
			Session: session.Session{ID: "session-1"},
		}},
		items: items,
	}
	builder := contextbuilder.NewBuilder()
	inputs := newTestInbox(t)
	current := newTestCoordinator(
		store,
		inputs,
		newFakeOperationManager(),
		builder,
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(store.itemRequests, []itemRequest{
		{After: 0, Limit: historyPageSize},
		{After: historyPageSize, Limit: historyPageSize},
	}) {
		t.Fatalf("item requests = %#v", store.itemRequests)
	}
	if current.state.currentTurnID != "turn-256" {
		t.Fatalf("current turn ID = %q, want %q", current.state.currentTurnID, "turn-256")
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(t,
		llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "parent"}},
		llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "child"}},
	)
	if !reflect.DeepEqual(built.Request.Input, want) {
		t.Fatalf("replayed input = %#v, want %#v", built.Request.Input, want)
	}
}

func TestCoordinatorReturnsSessionHistoryError(t *testing.T) {
	store := emptyFakeStore()
	store.itemsErr = errors.New("disk unavailable")
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	err := current.Run(t.Context())
	if err == nil || err.Error() != "load session history after 0: disk unavailable" {
		t.Fatalf("restore error = %v", err)
	}
}

func TestCoordinatorRejectsSessionHistoryWithoutProgress(t *testing.T) {
	store := emptyFakeStore()
	store.itemsPage = &sessionstore.Page{More: true, NextAfter: sessionstore.BeforeFirst}
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	err := current.restore(t.Context())
	if err == nil || err.Error() != "load session history did not advance after 0" {
		t.Fatalf("restore error = %v", err)
	}
}

func TestCoordinatorKeepsUnreplayableToolStatusInLocalState(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	turn := session.Turn{ID: "turn-1", Type: session.TurnRegular}
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	status := sessionstore.ToolCallStatus{
		TurnID: turn.ID,
		CallID: call.CallID,
		Status: tool.CallStatus{WaitingFor: []operation.ID{"terminal-operation"}},
	}
	store := &fakeStore{
		resume: sessionstore.ResumeState{Snapshot: sessionstore.Snapshot{
			Session: session.Session{ID: "session-1"},
		}},
		items: []sessionstore.Item{
			storedItem(1, sessionstore.ItemTurn, turn),
			storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
				TurnID:   turn.ID,
				Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}},
			}),
			storedItem(3, sessionstore.ItemToolCallStatus, status),
		},
	}
	builder := contextbuilder.NewBuilder()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		builder,
		registry,
	)

	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	callState, exists := current.state.toolCalls[toolCallKey{
		turnID: status.TurnID,
		callID: status.CallID,
	}]
	if !exists || callState.status == nil || !reflect.DeepEqual(*callState.status, status.Status) {
		t.Fatalf("tool call state = %#v", callState)
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(built.Request.Input, withPreamble(t, llm.Item{Type: llm.ItemToolCall, Data: call})) {
		t.Fatalf("replayed input = %#v", built.Request.Input)
	}
}

func TestCoordinatorRestoresCompletedToolCallFromStatusSnapshots(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: operationStatusTranslator{}}, tool.ViewImageName)
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	initial := operation.Operation{
		ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	completed := initial
	completed.Status = operation.StatusCompleted
	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: call.CallID,
		Status: tool.CallStatus{WaitingFor: []operation.ID{initial.ID}},
	}
	initialStatus := status
	initialStatus.Operations = []operation.Operation{initial}
	completedStatus := status
	completedStatus.Operations = []operation.Operation{completed}
	store := &fakeStore{
		resume: sessionstore.ResumeState{Snapshot: sessionstore.Snapshot{
			Session: session.Session{ID: "session-1"},
		}},
		items: []sessionstore.Item{
			storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
			storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
				TurnID:   "turn-1",
				Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}},
			}),
			storedItem(3, sessionstore.ItemToolCallStatus, initialStatus),
			storedItem(4, sessionstore.ItemToolCallStatus, completedStatus),
		},
	}
	builder := contextbuilder.NewBuilder()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		builder,
		registry,
	)

	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, exists := current.state.toolCalls[toolCallKey{turnID: "turn-1", callID: call.CallID}]; exists {
		t.Fatal("completed tool call remains in local state")
	}
	if !reflect.DeepEqual(current.state.operations[completed.ID], completed) {
		t.Fatalf("restored operation = %#v, want %#v", current.state.operations[completed.ID], completed)
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(t,
		llm.Item{Type: llm.ItemToolCall, Data: call},
		llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "completed"}}}},
	)
	if !reflect.DeepEqual(built.Request.Input, want) {
		t.Fatalf("replayed input = %#v, want %#v", built.Request.Input, want)
	}
}

func TestCoordinatorOverlaysResumedOperationsAfterHistorySnapshots(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: operationStatusTranslator{}}, tool.ViewImageName)
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	first := operation.Operation{
		ID: "operation-1", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	second := operation.Operation{
		ID: "operation-2", Type: "test", Version: 1, Status: operation.StatusReady,
	}
	resumedSecond := second
	resumedSecond.Status = operation.StatusAwaiting
	status := sessionstore.ToolCallStatus{
		TurnID:     "turn-1",
		CallID:     call.CallID,
		Status:     tool.CallStatus{WaitingFor: []operation.ID{first.ID, second.ID}},
		Operations: []operation.Operation{first, second},
	}
	store := &fakeStore{
		resume: sessionstore.ResumeState{
			Snapshot:   sessionstore.Snapshot{Session: session.Session{ID: "session-1"}},
			Operations: []operation.Operation{resumedSecond},
		},
		items: []sessionstore.Item{
			storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
			storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
				TurnID:   "turn-1",
				Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}},
			}),
			storedItem(3, sessionstore.ItemToolCallStatus, status),
		},
	}
	operations := newFakeOperationManager()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		operations,
		contextbuilder.NewBuilder(),
		registry,
	)

	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.state.operations[first.ID], first) ||
		!reflect.DeepEqual(current.state.operations[second.ID], resumedSecond) {
		t.Fatalf("restored operations = %#v", current.state.operations)
	}
	if err := current.dispatchOperationsToManager(); err != nil {
		t.Fatal(err)
	}
	want := map[operation.ID]operation.Operation{
		first.ID:  first,
		second.ID: resumedSecond,
	}
	got := make(map[operation.ID]operation.Operation, len(operations.adds))
	for _, value := range operations.adds {
		got[value.ID] = value
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatched operations = %#v, want %#v", got, want)
	}
}

func TestCoordinatorTracksToolCalls(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	current := newTestCoordinator(
		emptyFakeStore(),
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		registry,
	)
	first := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	second := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{"second":true}`}
	for _, response := range []sessionstore.ModelResponse{
		{
			TurnID: "turn-1",
			Response: llm.Response{Output: []llm.Item{
				{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "working"}},
				{Type: llm.ItemToolCall, Data: first},
			}},
		},
		{
			TurnID: "turn-2",
			Response: llm.Response{Output: []llm.Item{
				{Type: llm.ItemToolCall, Data: second},
			}},
		},
	} {
		if _, err := current.addItemToLocalState(sessionstore.Item{
			Kind: sessionstore.ItemModelResponse,
			Data: response,
		}); err != nil {
			t.Fatal(err)
		}
	}

	want := map[toolCallKey]toolCallState{
		{turnID: "turn-1", callID: first.CallID}: {
			toolCall: first, operations: map[operation.ID]struct{}{},
		},
		{turnID: "turn-2", callID: second.CallID}: {
			toolCall: second, operations: map[operation.ID]struct{}{},
		},
	}
	if !reflect.DeepEqual(current.state.toolCalls, want) {
		t.Fatalf("tool calls = %#v, want %#v", current.state.toolCalls, want)
	}

	if _, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemToolCallStatus,
		Data: sessionstore.ToolCallStatus{
			TurnID: "turn-1",
			CallID: first.CallID,
			Status: tool.CallStatus{Error: "invalid arguments"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	want = map[toolCallKey]toolCallState{
		{turnID: "turn-2", callID: second.CallID}: {
			toolCall: second, operations: map[operation.ID]struct{}{},
		},
	}
	if !reflect.DeepEqual(current.state.toolCalls, want) {
		t.Fatalf("tool calls = %#v, want %#v", current.state.toolCalls, want)
	}

	waitingFor := []operation.ID{"operation-1", "operation-2"}
	waitingStatus := tool.CallStatus{WaitingFor: waitingFor}
	if _, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemToolCallStatus,
		Data: sessionstore.ToolCallStatus{
			TurnID: "turn-2",
			CallID: second.CallID,
			Status: waitingStatus,
		},
	}); err != nil {
		t.Fatal(err)
	}
	want = map[toolCallKey]toolCallState{
		{turnID: "turn-2", callID: second.CallID}: {
			toolCall: second,
			status:   &waitingStatus,
			operations: map[operation.ID]struct{}{
				waitingFor[0]: {},
				waitingFor[1]: {},
			},
		},
	}
	if !reflect.DeepEqual(current.state.toolCalls, want) {
		t.Fatalf("tool calls = %#v, want %#v", current.state.toolCalls, want)
	}
}

func TestCoordinatorToolCallOperationsAreTerminal(t *testing.T) {
	current := &coordinator{state: newLoopState()}
	if !current.toolCallOperationsAreTerminal("missing-turn", "missing-call") {
		t.Fatal("tool call without operations was not recognized as terminal")
	}

	key := toolCallKey{turnID: "turn-1", callID: "call-1"}
	current.state.toolCalls[key] = toolCallState{
		operations: map[operation.ID]struct{}{
			"completed": {},
			"failed":    {},
			"canceled":  {},
		},
	}
	for id, status := range map[operation.ID]operation.Status{
		"completed": operation.StatusCompleted,
		"failed":    operation.StatusFailed,
		"canceled":  operation.StatusCanceled,
	} {
		current.addOperationToLocalState(operation.Operation{ID: id, Status: status})
	}

	if !current.toolCallOperationsAreTerminal(key.turnID, key.callID) {
		t.Fatal("terminal operations were not recognized")
	}

	current.addOperationToLocalState(operation.Operation{
		ID: "failed", Status: operation.StatusAwaiting,
	})
	if current.toolCallOperationsAreTerminal(key.turnID, key.callID) {
		t.Fatal("non-terminal operation was recognized as terminal")
	}
}

func TestCoordinatorAddsToolResultFromTrackedToolCall(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	builder := contextbuilder.NewBuilder()
	current := newTestCoordinator(
		emptyFakeStore(),
		newTestInbox(t),
		newFakeOperationManager(),
		builder,
		registry,
	)
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	current.addToolCallsToLocalState(sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: call,
		}}},
	})

	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: call.CallID,
		Status: tool.CallStatus{Error: "invalid arguments"},
	}
	if err := current.addToolResultToLocalState(status); err != nil {
		t.Fatal(err)
	}

	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(t, llm.Item{
		Type: llm.ItemToolResult,
		Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "error:invalid arguments"}}},
	})
	if !reflect.DeepEqual(built.Request.Input, want) {
		t.Fatalf("built input = %#v, want %#v", built.Request.Input, want)
	}
}

func TestCoordinatorAcceptsOrphanedToolStatus(t *testing.T) {
	current := newTestCoordinator(
		emptyFakeStore(),
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)
	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: "missing-call",
		Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
	}

	item, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemToolCallStatus,
		Data: status,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(item.Data, status) {
		t.Fatalf("returned item = %#v, want %#v", item.Data, status)
	}
}

func TestCoordinatorReturnsToolResultTranslationError(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: failingResultTranslator{err: errors.New("translation failed")}}, tool.ViewImageName)
	current := newTestCoordinator(
		emptyFakeStore(),
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		registry,
	)
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	current.addToolCallsToLocalState(sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: call,
		}}},
	})

	_, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemToolCallStatus,
		Data: sessionstore.ToolCallStatus{TurnID: "turn-1", CallID: call.CallID},
	})
	if err == nil || err.Error() != `add tool call "call-1" result to context: translation failed` {
		t.Fatalf("add status error = %v", err)
	}
}

func TestCoordinatorSkipsToolResultWithoutAvailableTranslator(t *testing.T) {
	current := newTestCoordinator(
		emptyFakeStore(),
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)
	call := llm.ToolCall{CallID: "call-1", Name: "unavailable-tool"}
	current.addToolCallsToLocalState(sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: call,
		}}},
	})

	if err := current.addToolResultToLocalState(sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: call.CallID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, exists := current.state.toolCalls[toolCallKey{turnID: "turn-1", callID: call.CallID}]; !exists {
		t.Fatal("tool call with unavailable translator was removed")
	}
}

func TestCoordinatorSkipsToolResultWithUntrackedOperation(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	current := newTestCoordinator(
		emptyFakeStore(),
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		registry,
	)
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName}
	current.addToolCallsToLocalState(sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: call,
		}}},
	})
	current.addOperationToLocalState(operation.Operation{
		ID: "operation-1", Status: operation.StatusCompleted,
	})

	if err := current.addToolResultToLocalState(sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: call.CallID,
		Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, exists := current.state.toolCalls[toolCallKey{turnID: "turn-1", callID: call.CallID}]; !exists {
		t.Fatal("tool call with an untracked operation was removed")
	}
}

func TestCoordinatorKeepsControlInputWithoutContextProjection(t *testing.T) {
	input := inbox.Input{
		ID: "control-input", Kind: inbox.InputControl, Payload: []byte(`{"Mode":"when_idle"}`),
	}
	store := &fakeStore{
		resume: sessionstore.ResumeState{Snapshot: sessionstore.Snapshot{
			Session: session.Session{ID: "session-1"},
		}},
		items: []sessionstore.Item{storedItem(1, sessionstore.ItemInput, input)},
	}
	builder := contextbuilder.NewBuilder()
	inputs := newTestInbox(t)
	current := newTestCoordinator(
		store,
		inputs,
		newFakeOperationManager(),
		builder,
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(built.Request.Input, withPreamble(t)) {
		t.Fatalf("replayed input = %#v", built.Request.Input)
	}
}

func TestCoordinatorRejectsInvalidSessionItemData(t *testing.T) {
	tests := []struct {
		name string
		item sessionstore.Item
		want string
	}{
		{name: "fork", item: storedItem(1, sessionstore.ItemFork, session.Turn{}), want: "want sessionstore.Fork"},
		{name: "input", item: storedItem(1, sessionstore.ItemInput, session.Turn{}), want: "want inbox.Input"},
		{
			name: "invalid input",
			item: storedItem(1, sessionstore.ItemInput, inbox.Input{
				ID: "input-1", Kind: "unknown",
			}),
			want: "invalid input",
		},
		{name: "turn", item: storedItem(1, sessionstore.ItemTurn, inbox.Input{}), want: "want session.Turn"},
		{name: "response", item: storedItem(1, sessionstore.ItemModelResponse, session.Turn{}), want: "want sessionstore.ModelResponse"},
		{name: "status", item: storedItem(1, sessionstore.ItemToolCallStatus, session.Turn{}), want: "want sessionstore.ToolCallStatus"},
		{name: "kind", item: storedItem(1, "unknown", nil), want: `unsupported item kind "unknown"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{
				resume: sessionstore.ResumeState{Snapshot: sessionstore.Snapshot{
					Session: session.Session{ID: "session-1"},
				}},
				items: []sessionstore.Item{test.item},
			}
			current := newTestCoordinator(
				store,
				newTestInbox(t),
				newFakeOperationManager(),
				contextbuilder.NewBuilder(),
				tool.NewRegistry(tool.StaticTranslators{}),
			)
			err := current.restore(t.Context())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestCoordinatorRunCallsModelAfterPersistedExternalInput(t *testing.T) {
	store := emptyFakeStore()
	store.items = []sessionstore.Item{storedItem(
		1,
		sessionstore.ItemTurn,
		session.Turn{ID: "previous-turn", Type: session.TurnRegular},
	)}
	inputs := newTestInbox(t)
	operations := newFakeOperationManager()
	started := make(chan llm.Request, 1)
	requestCanceled := make(chan error, 1)
	var orderMutex sync.Mutex
	order := make([]string, 0, 3)
	record := func(value string) {
		orderMutex.Lock()
		defer orderMutex.Unlock()
		order = append(order, value)
	}
	store.onAppendInput = func(inbox.Input) { record("input") }
	store.onAppendTurn = func(session.Turn) { record("turn") }
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		record("respond")
		started <- request
		<-ctx.Done()
		requestCanceled <- ctx.Err()
		return llm.Response{}, ctx.Err()
	}}
	builder := contextbuilder.NewBuilder()
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		builder,
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	event := externalEvent(t, 1, "input-1", "hello")
	submitTestInput(t, inputs, event)
	request := receiveTestValue(t, started)
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(t, llm.Item{
		Type: llm.ItemMessage,
		Data: llm.Message{Role: llm.RoleUser, Text: "hello"},
	})
	if !reflect.DeepEqual(built.Request.Input, want) {
		t.Fatalf("built input = %#v, want %#v", built.Request.Input, want)
	}
	if !reflect.DeepEqual(request, built.Request) {
		t.Fatalf("model request = %#v, want %#v", request, built.Request)
	}
	if !reflect.DeepEqual(store.appendedInputs, []inbox.Input{event}) {
		t.Fatalf("appended inputs = %#v, want %#v", store.appendedInputs, []inbox.Input{event})
	}
	if len(store.appendedTurns) != 1 || store.appendedTurns[0].ID == "" ||
		store.appendedTurns[0].PreviousTurnID != "previous-turn" {
		t.Fatalf("appended turns = %#v", store.appendedTurns)
	}
	orderMutex.Lock()
	gotOrder := append([]string(nil), order...)
	orderMutex.Unlock()
	if !reflect.DeepEqual(gotOrder, []string{"input", "turn", "respond"}) {
		t.Fatalf("effect order = %v", gotOrder)
	}
	if len(operations.adds) != 0 || len(operations.cancels) != 0 {
		t.Fatalf("unexpected operation effects: adds=%v cancels=%v", operations.adds, operations.cancels)
	}

	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if err := receiveTestValue(t, requestCanceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("model request error = %v, want context cancellation", err)
	}
}

func TestCoordinatorRunSlurpsQueuedInputsBeforeCallingModel(t *testing.T) {
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	first := externalEvent(t, 1, "input-1", "first")
	second := externalEvent(t, 2, "input-2", "second")
	submitTestInput(t, inputs, first)
	submitTestInput(t, inputs, second)
	started := make(chan llm.Request, 1)
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		started <- request
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	request := receiveTestValue(t, started)
	want := withPreamble(t,
		llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "first"}},
		llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "second"}},
	)
	if !reflect.DeepEqual(request.Input, want) {
		t.Fatalf("model input = %#v, want %#v", request.Input, want)
	}
	if !reflect.DeepEqual(store.appendedInputs, []inbox.Input{first, second}) {
		t.Fatalf("appended inputs = %#v", store.appendedInputs)
	}
	if got := len(adapter.requestSnapshot()); got != 1 {
		t.Fatalf("model requests = %d, want 1", got)
	}

	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
}

func TestCoordinatorRunDoesNotCallModelWhenRequestBuildFails(t *testing.T) {
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	adapter := &fakeAdapter{}
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		failingBuilder{
			Builder: contextbuilder.NewBuilder(),
			err:     errors.New("context unavailable"),
		},
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(t.Context())
	}()

	event := externalEvent(t, 1, "input-1", "hello")
	submitTestInput(t, inputs, event)
	err := receiveTestValue(t, done)
	if err == nil || err.Error() != "build model request: context unavailable" {
		t.Fatalf("Run error = %v", err)
	}
	if !reflect.DeepEqual(store.appendedInputs, []inbox.Input{event}) ||
		len(store.appendedTurns) != 0 || len(adapter.requestSnapshot()) != 0 {
		t.Fatalf(
			"effects after build failure: inputs=%v turns=%v requests=%v",
			store.appendedInputs,
			store.appendedTurns,
			adapter.requestSnapshot(),
		)
	}
}

func TestCoordinatorRunDoesNotCallModelWhenTurnStoreFails(t *testing.T) {
	store := emptyFakeStore()
	store.appendTurnErr = errors.New("disk unavailable")
	inputs := newTestInbox(t)
	adapter := &fakeAdapter{}
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(t.Context())
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "hello"))
	err := receiveTestValue(t, done)
	if err == nil || !strings.Contains(err.Error(), "disk unavailable") {
		t.Fatalf("Run error = %v", err)
	}
	if len(store.appendedTurns) != 1 || len(adapter.requestSnapshot()) != 0 {
		t.Fatalf(
			"effects after turn store failure: turns=%v requests=%v",
			store.appendedTurns,
			adapter.requestSnapshot(),
		)
	}
}

func TestCoordinatorRunPersistsModelResponseForOriginatingTurn(t *testing.T) {
	store := emptyFakeStore()
	responseStored := make(chan sessionstore.ModelResponse, 1)
	store.onAppendModelResponse = func(response sessionstore.ModelResponse) {
		responseStored <- response
	}
	response := llm.Response{ID: "response-1", Output: []llm.Item{{
		Type: llm.ItemMessage,
		Data: llm.Message{Role: llm.RoleAssistant, Text: "done"},
	}}}
	adapter := &fakeAdapter{respond: func(
		context.Context,
		llm.Request,
	) (llm.Response, error) {
		return response, nil
	}}
	inputs := newTestInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "hello"))
	stored := receiveTestValue(t, responseStored)
	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if len(store.appendedTurns) != 1 || stored.TurnID != store.appendedTurns[0].ID ||
		!reflect.DeepEqual(stored.Response, response) {
		t.Fatalf("stored response = %#v, turns = %#v", stored, store.appendedTurns)
	}
	if len(adapter.requestSnapshot()) != 1 || len(store.appendedResponses) != 1 ||
		len(store.appendedTurns) != 1 {
		t.Fatalf(
			"message response effects: requests=%v responses=%v turns=%v",
			adapter.requestSnapshot(),
			store.appendedResponses,
			store.appendedTurns,
		)
	}
}

func TestCoordinatorRunPersistsToolCallBeforeDispatch(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	translator := &submittingTranslator{specs: []operation.Spec{spec}}
	store := emptyFakeStore()
	dispatched := make(chan operation.Operation, 1)
	operations := newFakeOperationManager()
	operations.addError = func(value operation.Operation) error {
		if len(store.appendedResponses) != 1 || len(store.appendedStatuses) != 1 {
			return errors.New("operation dispatched before response and status were stored")
		}
		dispatched <- value
		return nil
	}
	response := llm.Response{ID: "response-1", Output: []llm.Item{{
		Type: llm.ItemToolCall,
		Data: llm.ToolCall{CallID: "call-1", Name: tool.BashName, Arguments: `{}`},
	}}}
	adapter := &fakeAdapter{respond: func(
		context.Context,
		llm.Request,
	) (llm.Response, error) {
		return response, nil
	}}
	inputs := newTestInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "run"))
	operationValue := receiveTestValue(t, dispatched)
	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if len(store.appendedStatuses) != 1 ||
		!reflect.DeepEqual(store.appendedStatuses[0].Operations, []operation.Operation{operationValue}) ||
		!reflect.DeepEqual(store.appendedStatuses[0].Status.WaitingFor, []operation.ID{operationValue.ID}) {
		t.Fatalf("appended statuses = %#v, operation = %#v", store.appendedStatuses, operationValue)
	}
	if len(adapter.requestSnapshot()) != 1 || len(store.appendedTurns) != 1 {
		t.Fatalf("requests = %v, turns = %v", adapter.requestSnapshot(), store.appendedTurns)
	}
}

func TestCoordinatorRunStartsCorrectiveTurnForValidationError(t *testing.T) {
	firstResponse := llm.Response{ID: "response-1", Output: []llm.Item{{
		Type: llm.ItemToolCall,
		Data: llm.ToolCall{CallID: "call-1", Name: tool.BashName, Arguments: `{}`},
	}}}
	started := make(chan llm.Request, 2)
	callCount := 0
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		callCount++
		started <- request
		if callCount == 1 {
			return firstResponse, nil
		}
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}, tool.BashName),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "run"))
	firstRequest := receiveTestValue(t, started)
	secondRequest := receiveTestValue(t, started)
	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if len(store.appendedStatuses) != 1 || store.appendedStatuses[0].Status.Error == "" {
		t.Fatalf("appended statuses = %#v", store.appendedStatuses)
	}
	if len(store.appendedTurns) != 2 ||
		store.appendedTurns[1].PreviousTurnID != store.appendedTurns[0].ID {
		t.Fatalf("appended turns = %#v", store.appendedTurns)
	}
	if len(firstRequest.Input) != 2 || len(secondRequest.Input) != 4 ||
		secondRequest.Input[3].Type != llm.ItemToolResult {
		t.Fatalf("model requests = %#v", []llm.Request{firstRequest, secondRequest})
	}
}

func TestCoordinatorRunStartsContinuationTurnForCompletedToolCall(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	translator := &submittingTranslator{specs: []operation.Spec{spec}}
	firstResponse := llm.Response{ID: "response-1", Output: []llm.Item{{
		Type: llm.ItemToolCall,
		Data: llm.ToolCall{CallID: "call-1", Name: tool.BashName, Arguments: `{}`},
	}}}
	started := make(chan llm.Request, 2)
	callCount := 0
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		callCount++
		started <- request
		if callCount == 1 {
			return firstResponse, nil
		}
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	store := emptyFakeStore()
	operations := newFakeOperationManager()
	dispatched := make(chan operation.Operation, 1)
	operations.addError = func(value operation.Operation) error {
		dispatched <- value
		return nil
	}
	inputs := newTestInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "run"))
	_ = receiveTestValue(t, started)
	operationValue := receiveTestValue(t, dispatched)
	operationValue.Status = operation.StatusCompleted
	operations.updates <- operationValue
	continuationRequest := receiveTestValue(t, started)
	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if !reflect.DeepEqual(store.savedOperations, []operation.Operation{operationValue}) ||
		len(store.appendedStatuses) != 2 ||
		!reflect.DeepEqual(store.appendedStatuses[1].Operations, []operation.Operation{operationValue}) {
		t.Fatalf(
			"completion effects: operations=%#v statuses=%#v",
			store.savedOperations,
			store.appendedStatuses,
		)
	}
	options := adapter.requestOptionsSnapshot()
	if len(options) != 2 || options[0].CacheKey != "session-1" || options[1].CacheKey != "session-1" {
		t.Fatalf("request options = %#v, want session-1 cache keys for both turns", options)
	}
	if len(continuationRequest.Input) != 4 ||
		continuationRequest.Input[3].Type != llm.ItemToolResult {
		t.Fatalf("continuation request = %#v", continuationRequest)
	}
	if len(adapter.requestSnapshot()) != 2 || len(store.appendedTurns) != 2 {
		t.Fatalf("requests = %v, turns = %v", adapter.requestSnapshot(), store.appendedTurns)
	}
}

func TestCoordinatorRunBatchesCompletedToolCallsIntoOneTurn(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	operationValue := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady,
	}
	calls := []llm.ToolCall{
		{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`},
		{CallID: "call-2", Name: tool.ViewImageName, Arguments: `{}`},
	}
	status := tool.CallStatus{WaitingFor: []operation.ID{operationValue.ID}}
	store := emptyFakeStore()
	store.resume.Operations = []operation.Operation{operationValue}
	store.items = []sessionstore.Item{
		storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
		storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
			TurnID: "turn-1",
			Response: llm.Response{Output: []llm.Item{
				{Type: llm.ItemToolCall, Data: calls[0]},
				{Type: llm.ItemToolCall, Data: calls[1]},
			}},
		}),
		storedItem(3, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
			TurnID: "turn-1", CallID: calls[0].CallID, Status: status,
			Operations: []operation.Operation{operationValue},
		}),
		storedItem(4, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
			TurnID: "turn-1", CallID: calls[1].CallID, Status: status,
			Operations: []operation.Operation{operationValue},
		}),
	}
	started := make(chan llm.Request, 1)
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		started <- request
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	inputs := newTestInbox(t)
	operations := newFakeOperationManager()
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		registry,
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	operationValue.Status = operation.StatusCompleted
	operations.updates <- operationValue
	request := receiveTestValue(t, started)
	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if len(store.appendedStatuses) != 2 || len(store.appendedTurns) != 1 ||
		store.appendedTurns[0].PreviousTurnID != "turn-1" {
		t.Fatalf("statuses = %#v, turns = %#v", store.appendedStatuses, store.appendedTurns)
	}
	if len(adapter.requestSnapshot()) != 1 || len(request.Input) != 5 {
		t.Fatalf("requests = %#v", adapter.requestSnapshot())
	}
}

func TestCoordinatorRunSteersActiveModelRequest(t *testing.T) {
	started := make(chan llm.Request, 2)
	firstCanceled := make(chan struct{}, 1)
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		started <- request
		<-ctx.Done()
		if len(request.Input) == 2 {
			firstCanceled <- struct{}{}
		}
		return llm.Response{}, ctx.Err()
	}}
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "first"))
	firstRequest := receiveTestValue(t, started)
	submitTestInput(t, inputs, externalEvent(t, 2, "input-2", "second"))
	secondRequest := receiveTestValue(t, started)
	_ = receiveTestValue(t, firstCanceled)
	select {
	case err := <-done:
		t.Fatalf("Run stopped after superseded cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if len(firstRequest.Input) != 2 || len(secondRequest.Input) != 3 {
		t.Fatalf("steered requests = %#v", []llm.Request{firstRequest, secondRequest})
	}
	if len(store.appendedTurns) != 2 ||
		store.appendedTurns[1].PreviousTurnID != store.appendedTurns[0].ID {
		t.Fatalf("appended turns = %#v", store.appendedTurns)
	}
	if len(store.appendedResponses) != 0 {
		t.Fatalf("appended responses = %#v", store.appendedResponses)
	}

	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
}

func TestCoordinatorRunHandlesOperationUpdateWhileModelIsRunning(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		_ llm.Request,
	) (llm.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	initial := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady,
	}
	store := emptyFakeStore()
	store.resume.Operations = []operation.Operation{initial}
	storedUpdate := make(chan operation.Operation, 1)
	store.onSaveOperation = func(value operation.Operation) {
		storedUpdate <- value
	}
	inputs := newTestInbox(t)
	operations := newFakeOperationManager()
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "hello"))
	_ = receiveTestValue(t, started)
	update := initial
	update.Status = operation.StatusAwaiting
	operations.updates <- update
	if stored := receiveTestValue(t, storedUpdate); !reflect.DeepEqual(stored, update) {
		t.Fatalf("stored operation = %#v, want %#v", stored, update)
	}

	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if len(adapter.requestSnapshot()) != 1 || len(store.appendedTurns) != 1 {
		t.Fatalf("requests = %v, turns = %v", adapter.requestSnapshot(), store.appendedTurns)
	}
}

func TestCoordinatorRunDropsSuccessfulResponseFromSupersededTurn(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	translator := &submittingTranslator{specs: []operation.Spec{spec}}
	firstResponse := llm.Response{ID: "response-1", Output: []llm.Item{{
		Type: llm.ItemToolCall,
		Data: llm.ToolCall{CallID: "stale-call", Name: tool.BashName, Arguments: `{}`},
	}}}
	secondResponse := llm.Response{ID: "response-2", Output: []llm.Item{{
		Type: llm.ItemMessage,
		Data: llm.Message{Role: llm.RoleAssistant, Text: "current answer"},
	}}}
	started := make(chan llm.Request, 2)
	firstReturned := make(chan struct{}, 1)
	releaseSecond := make(chan struct{})
	responseStored := make(chan sessionstore.ModelResponse, 2)
	store := emptyFakeStore()
	store.onAppendModelResponse = func(response sessionstore.ModelResponse) {
		responseStored <- response
	}
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		started <- request
		if len(request.Input) == 2 {
			<-ctx.Done()
			firstReturned <- struct{}{}
			return firstResponse, nil
		}
		<-releaseSecond
		return secondResponse, nil
	}}
	inputs := newTestInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "first"))
	_ = receiveTestValue(t, started)
	submitTestInput(t, inputs, externalEvent(t, 2, "input-2", "second"))
	_ = receiveTestValue(t, started)
	_ = receiveTestValue(t, firstReturned)
	time.Sleep(20 * time.Millisecond)
	close(releaseSecond)
	stored := receiveTestValue(t, responseStored)
	if len(store.appendedTurns) != 2 || stored.TurnID != store.appendedTurns[1].ID ||
		!reflect.DeepEqual(stored.Response, secondResponse) {
		t.Fatalf("stored response = %#v, turns = %#v", stored, store.appendedTurns)
	}
	select {
	case unexpected := <-responseStored:
		t.Fatalf("stored superseded response = %#v", unexpected)
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if !reflect.DeepEqual(store.appendedResponses, []sessionstore.ModelResponse{stored}) ||
		len(translator.calls) != 0 || len(store.appendedStatuses) != 0 {
		t.Fatalf(
			"superseded effects: responses=%#v translations=%#v statuses=%#v",
			store.appendedResponses,
			translator.calls,
			store.appendedStatuses,
		)
	}
}

func TestCoordinatorRunReturnsCurrentModelError(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	adapter := &fakeAdapter{respond: func(
		context.Context,
		llm.Request,
	) (llm.Response, error) {
		return llm.Response{}, providerErr
	}}
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(t.Context())
	}()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "hello"))
	err := receiveTestValue(t, done)
	if !errors.Is(err, providerErr) || !strings.Contains(err.Error(), "call model for turn") {
		t.Fatalf("Run error = %v", err)
	}
	if len(store.appendedTurns) != 1 || len(store.appendedResponses) != 0 {
		t.Fatalf("turns = %#v, responses = %#v", store.appendedTurns, store.appendedResponses)
	}
}

func TestCoordinatorHandlesModelResponseBeforeSchedulingToolCalls(t *testing.T) {
	spec, err := operation.NewValueSpec(jsontext.Value(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	response := sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{ID: "response-1", Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: llm.ToolCall{CallID: "call-1", Name: tool.BashName, Arguments: `{}`},
		}}},
	}
	store := emptyFakeStore()
	storedBeforeTranslation := false
	translator := &submittingTranslator{
		specs: []operation.Spec{spec},
		onTranslate: func() {
			storedBeforeTranslation = reflect.DeepEqual(
				store.appendedResponses,
				[]sessionstore.ModelResponse{response},
			)
		},
	}
	operations := newFakeOperationManager()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName),
	)

	if _, err := current.handleModelResponse(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	if !storedBeforeTranslation {
		t.Fatal("tool call was translated before its model response was stored")
	}
	if len(store.appendedStatuses) != 1 || len(store.appendedStatuses[0].Operations) != 1 {
		t.Fatalf("appended statuses = %#v", store.appendedStatuses)
	}
	if len(operations.adds) != 0 {
		t.Fatalf("handler dispatched operations = %#v", operations.adds)
	}
}

func TestCoordinatorDoesNotScheduleToolCallsWhenModelResponseStoreFails(t *testing.T) {
	store := emptyFakeStore()
	store.appendModelResponseErr = errors.New("disk unavailable")
	translator := &submittingTranslator{}
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName),
	)
	response := sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: llm.ToolCall{CallID: "call-1", Name: tool.BashName, Arguments: `{}`},
		}}},
	}

	_, err := current.handleModelResponse(t.Context(), response)
	if err == nil || err.Error() != `store turn "turn-1" response: disk unavailable` {
		t.Fatalf("handle model response error = %v", err)
	}
	if !reflect.DeepEqual(store.appendedResponses, []sessionstore.ModelResponse{response}) {
		t.Fatalf("appended responses = %#v", store.appendedResponses)
	}
	if len(translator.calls) != 0 || len(store.appendedStatuses) != 0 ||
		len(current.state.operations) != 0 {
		t.Fatalf(
			"scheduled after response store failure: calls=%#v statuses=%#v operations=%#v",
			translator.calls,
			store.appendedStatuses,
			current.state.operations,
		)
	}
}

func TestCoordinatorHandlesModelResponseWithoutToolCalls(t *testing.T) {
	store := emptyFakeStore()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)
	response := sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleAssistant, Text: "done"},
		}}},
	}

	if _, err := current.handleModelResponse(t.Context(), response); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.appendedResponses, []sessionstore.ModelResponse{response}) ||
		len(store.appendedStatuses) != 0 || len(current.state.operations) != 0 {
		t.Fatalf(
			"response effects: responses=%#v statuses=%#v operations=%#v",
			store.appendedResponses,
			store.appendedStatuses,
			current.state.operations,
		)
	}
}

func TestCoordinatorRunSchedulesToolCallsWithoutStatusBeforeDispatch(t *testing.T) {
	firstSpec, err := operation.NewValueSpec(jsontext.Value(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	secondSpec, err := operation.NewValueSpec(jsontext.Value(`{"value":2}`))
	if err != nil {
		t.Fatal(err)
	}
	firstSpec.MaxOutputLength = 3
	secondSpec.MaxOutputLength = 7
	translator := &submittingTranslator{specs: []operation.Spec{firstSpec, secondSpec}}
	registry := tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName)
	handled := llm.ToolCall{CallID: "call-handled", Name: tool.BashName, Arguments: `{}`}
	missing := llm.ToolCall{CallID: "call-missing", Name: tool.BashName, Arguments: `{}`}
	store := &fakeStore{
		resume: sessionstore.ResumeState{Snapshot: sessionstore.Snapshot{
			Session: session.Session{ID: "session-1"},
		}},
		items: []sessionstore.Item{
			storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
			storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
				TurnID: "turn-1",
				Response: llm.Response{Output: []llm.Item{
					{Type: llm.ItemToolCall, Data: handled},
					{Type: llm.ItemToolCall, Data: missing},
				}},
			}),
			storedItem(3, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
				TurnID: "turn-1",
				CallID: handled.CallID,
				Status: tool.CallStatus{Error: "already handled"},
			}),
		},
	}
	inboxContext, cancelInbox := context.WithCancel(t.Context())
	cancelInbox()
	inputs, inboxErr := inbox.New(inboxContext, nil)
	if inboxErr != nil {
		t.Fatal(inboxErr)
	}
	operations := newFakeOperationManager()
	operations.addError = func(operation.Operation) error {
		if len(store.appendedStatuses) != 1 || store.appendedStatuses[0].CallID != missing.CallID {
			return errors.New("operation dispatched before tool-call status was stored")
		}
		return nil
	}
	requests := make(chan llm.Request, 1)
	adapter := &fakeAdapter{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
		requests <- request
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		registry,
		adapter,
	)

	err = current.Run(t.Context())
	if err == nil || err.Error() != "inbox output closed" {
		t.Fatalf("Run error = %v, want closed inbox error", err)
	}
	if !reflect.DeepEqual(translator.calls, []llm.ToolCall{missing}) {
		t.Fatalf("translated calls = %#v, want %#v", translator.calls, []llm.ToolCall{missing})
	}
	if len(store.appendedStatuses) != 1 {
		t.Fatalf("appended statuses = %#v", store.appendedStatuses)
	}
	status := store.appendedStatuses[0]
	if status.TurnID != "turn-1" || status.CallID != missing.CallID ||
		len(status.Status.WaitingFor) != 2 || len(status.Operations) != 2 {
		t.Fatalf("appended status = %#v", status)
	}
	wantOperations := make(map[operation.ID]operation.Operation, len(status.Operations))
	for index, value := range status.Operations {
		if value.ID == "" || value.ID != status.Status.WaitingFor[index] ||
			value.Status != operation.StatusReady || value.MaxOutputLength != translator.specs[index].MaxOutputLength {
			t.Fatalf("scheduled operation %d = %#v", index, value)
		}
		wantOperations[value.ID] = value
		if !reflect.DeepEqual(current.state.operations[value.ID], value) {
			t.Fatalf("local operation %q = %#v, want %#v", value.ID, current.state.operations[value.ID], value)
		}
	}
	gotOperations := make(map[operation.ID]operation.Operation, len(operations.adds))
	for _, value := range operations.adds {
		gotOperations[value.ID] = value
	}
	if !reflect.DeepEqual(gotOperations, wantOperations) {
		t.Fatalf("dispatched operations = %#v, want %#v", gotOperations, wantOperations)
	}
	if _, err := current.scheduleToolCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(translator.calls) != 1 || len(store.appendedStatuses) != 1 {
		t.Fatalf("rescheduled call: calls=%#v statuses=%#v", translator.calls, store.appendedStatuses)
	}
	assertStopResult(t, receiveTestValue(t, requests), handled.CallID, "already handled")
}

func TestCoordinatorRunStartsCorrectiveTurnForRecoveredValidationError(t *testing.T) {
	call := llm.ToolCall{CallID: "call-1", Name: tool.BashName, Arguments: `{}`}
	store := emptyFakeStore()
	store.items = []sessionstore.Item{
		storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
		storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
			TurnID:   "turn-1",
			Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}},
		}),
	}
	started := make(chan llm.Request, 1)
	adapter := &fakeAdapter{respond: func(
		ctx context.Context,
		request llm.Request,
	) (llm.Response, error) {
		started <- request
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}}
	inputs := newTestInbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}, tool.BashName),
		adapter,
	)
	done := make(chan error, 1)
	go func() {
		done <- current.Run(ctx)
	}()

	request := receiveTestValue(t, started)
	cancel()
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if len(store.appendedStatuses) != 1 || store.appendedStatuses[0].Status.Error == "" ||
		len(store.appendedTurns) != 1 || store.appendedTurns[0].PreviousTurnID != "turn-1" {
		t.Fatalf("statuses = %#v, turns = %#v", store.appendedStatuses, store.appendedTurns)
	}
	if len(request.Input) != 3 || request.Input[2].Type != llm.ItemToolResult {
		t.Fatalf("corrective request = %#v", request)
	}
}

func TestCoordinatorRunRejectsExternalInputWithoutTextPayload(t *testing.T) {
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	submitTestInput(t, inputs, inbox.Input{
		ID: "input-1", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
	})
	current := newTestCoordinator(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	err := current.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), `add input "input-1" to context`) {
		t.Fatalf("Run error = %v", err)
	}
	if len(store.appendedInputs) != 0 {
		t.Fatalf("stored inputs = %#v", store.appendedInputs)
	}
}

func TestCoordinatorRunStoresOperationUpdatesWithoutRedispatch(t *testing.T) {
	store := emptyFakeStore()
	initial := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady,
	}
	store.resume.Operations = []operation.Operation{initial}
	inputs := newTestInbox(t)
	operations := newFakeOperationManager()
	updated := initial
	updated.Status = operation.StatusAwaiting
	operations.updates <- updated
	close(operations.updates)
	adapter := &fakeAdapter{}
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)

	err := current.Run(t.Context())
	if err == nil || err.Error() != "operation updates closed" {
		t.Fatalf("Run error = %v, want closed operation updates error", err)
	}
	if !reflect.DeepEqual(current.state.operations[initial.ID], updated) {
		t.Fatalf("operation = %#v, want %#v", current.state.operations[initial.ID], updated)
	}
	if !reflect.DeepEqual(store.savedOperations, []operation.Operation{updated}) {
		t.Fatalf("saved operations = %#v, want %#v", store.savedOperations, []operation.Operation{updated})
	}
	if !reflect.DeepEqual(operations.adds, []operation.Operation{initial}) {
		t.Fatalf("dispatched operations = %#v, want only the initial operation", operations.adds)
	}
	if len(operations.cancels) != 0 || len(adapter.requests) != 0 {
		t.Fatalf("unexpected effects: cancels=%v model=%v", operations.cancels, adapter.requests)
	}
}

func TestCoordinatorReconcilesToolCallsFromPersistedOperationUpdates(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	store := emptyFakeStore()
	builder := contextbuilder.NewBuilder()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		builder,
		registry,
	)
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	if _, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemModelResponse,
		Data: sessionstore.ModelResponse{
			TurnID: "turn-1",
			Response: llm.Response{Output: []llm.Item{{
				Type: llm.ItemToolCall,
				Data: call,
			}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	operations := []operation.Operation{
		{ID: "operation-1", Status: operation.StatusReady},
		{ID: "operation-2", Status: operation.StatusAwaiting},
	}
	for _, value := range operations {
		current.addOperationToLocalState(value)
	}
	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: call.CallID,
		Status: tool.CallStatus{WaitingFor: []operation.ID{
			operations[0].ID,
			operations[1].ID,
		}},
	}
	if _, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemToolCallStatus,
		Data: status,
	}); err != nil {
		t.Fatal(err)
	}

	first := operations[0]
	first.Status = operation.StatusCompleted
	if err := current.handleOperationUpdate(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := current.reconcileToolCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := toolCallKey{turnID: status.TurnID, callID: status.CallID}
	if _, exists := current.state.toolCalls[key]; !exists {
		t.Fatal("tool call was removed before every operation became terminal")
	}

	second := operations[1]
	second.Status = operation.StatusFailed
	if err := current.handleOperationUpdate(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := current.reconcileToolCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, exists := current.state.toolCalls[key]; exists {
		t.Fatal("completed tool call remains in local state")
	}
	if !reflect.DeepEqual(store.savedOperations, []operation.Operation{first, second}) {
		t.Fatalf("saved operations = %#v, want %#v", store.savedOperations, []operation.Operation{first, second})
	}
	if !reflect.DeepEqual(store.appendedStatuses, []sessionstore.ToolCallStatus{{
		TurnID: status.TurnID,
		CallID: status.CallID,
		Status: status.Status,
		Operations: []operation.Operation{
			first,
			second,
		},
	}}) {
		t.Fatalf("appended statuses = %#v", store.appendedStatuses)
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(t,
		llm.Item{Type: llm.ItemToolCall, Data: call},
		llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "error:"}}}},
	)
	if !reflect.DeepEqual(built.Request.Input, want) {
		t.Fatalf("built input = %#v, want %#v", built.Request.Input, want)
	}
}

func TestCoordinatorRunReturnsReconciliationError(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: terminalResultTranslator{}}, tool.ViewImageName)
	value := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady,
	}
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: call.CallID,
		Status: tool.CallStatus{WaitingFor: []operation.ID{value.ID}},
	}
	store := emptyFakeStore()
	store.resume.Operations = []operation.Operation{value}
	store.items = []sessionstore.Item{
		storedItem(1, sessionstore.ItemModelResponse, sessionstore.ModelResponse{
			TurnID: "turn-1",
			Response: llm.Response{Output: []llm.Item{{
				Type: llm.ItemToolCall,
				Data: call,
			}}},
		}),
		storedItem(2, sessionstore.ItemToolCallStatus, status),
		storedItem(3, sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
			TurnID: "another-turn",
			CallID: "another-call",
		}),
		storedItem(4, sessionstore.ItemTurn, session.Turn{ID: "turn-2", Type: session.TurnRegular}),
	}
	completed := value
	completed.Status = operation.StatusCompleted
	operations := newFakeOperationManager()
	operations.updates <- completed
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		operations,
		contextbuilder.NewBuilder(),
		registry,
	)

	err := current.Run(t.Context())
	if err == nil || err.Error() != `add tool call "call-1" result to context: terminal result failed` {
		t.Fatalf("Run error = %v", err)
	}
	if !reflect.DeepEqual(store.savedOperations, []operation.Operation{completed}) {
		t.Fatalf("saved operations = %#v, want %#v", store.savedOperations, []operation.Operation{completed})
	}
}

func TestCoordinatorDoesNotCompleteToolCallBeforeOperationIsStored(t *testing.T) {
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: testTranslator{}}, tool.ViewImageName)
	store := emptyFakeStore()
	store.saveOperationErr = errors.New("disk unavailable")
	builder := contextbuilder.NewBuilder()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		builder,
		registry,
	)
	call := llm.ToolCall{CallID: "call-1", Name: tool.ViewImageName, Arguments: `{}`}
	current.addToolCallsToLocalState(sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall,
			Data: call,
		}}},
	})
	value := operation.Operation{ID: "operation-1", Status: operation.StatusReady}
	current.addOperationToLocalState(value)
	status := sessionstore.ToolCallStatus{
		TurnID: "turn-1",
		CallID: call.CallID,
		Status: tool.CallStatus{WaitingFor: []operation.ID{value.ID}},
	}
	if _, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemToolCallStatus,
		Data: status,
	}); err != nil {
		t.Fatal(err)
	}

	value.Status = operation.StatusCompleted
	err := current.handleOperationUpdate(t.Context(), value)
	if err == nil || err.Error() != `store operation "operation-1": disk unavailable` {
		t.Fatalf("handle operation update error = %v", err)
	}
	if _, exists := current.state.toolCalls[toolCallKey{
		turnID: status.TurnID,
		callID: status.CallID,
	}]; !exists {
		t.Fatal("tool call was removed before its operation was stored")
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Request.Input) != 2 {
		t.Fatalf("tool results = %#v, want only the initial result", built.Request.Input)
	}
}

func TestCoordinatorRunDispatchesRestoredNonTerminalOperations(t *testing.T) {
	store := emptyFakeStore()
	statuses := []operation.Status{
		operation.StatusReady,
		operation.StatusAwaiting,
		operation.StatusCanceling,
		operation.StatusCompleted,
		operation.StatusFailed,
		operation.StatusCanceled,
	}
	for _, status := range statuses {
		store.resume.Operations = append(store.resume.Operations, operation.Operation{
			ID: operation.ID(status), Type: operation.TypeShell, Version: 1, Status: status,
		})
	}
	inboxContext, cancelInbox := context.WithCancel(t.Context())
	cancelInbox()
	inputs, inboxErr := inbox.New(inboxContext, nil)
	if inboxErr != nil {
		t.Fatal(inboxErr)
	}
	operations := newFakeOperationManager()
	adapter := &fakeAdapter{}
	current := newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
		adapter,
	)

	err := current.Run(t.Context())
	if err == nil || err.Error() != "inbox output closed" {
		t.Fatalf("Run error = %v, want closed inbox error", err)
	}
	sort.Slice(operations.adds, func(left, right int) bool {
		return operations.adds[left].ID < operations.adds[right].ID
	})
	want := []operation.Operation{
		{ID: "awaiting", Type: operation.TypeShell, Version: 1, Status: operation.StatusAwaiting},
		{ID: "canceling", Type: operation.TypeShell, Version: 1, Status: operation.StatusCanceling},
		{ID: "ready", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady},
	}
	if !reflect.DeepEqual(operations.adds, want) {
		t.Fatalf("dispatched operations = %#v, want %#v", operations.adds, want)
	}
	if len(adapter.requests) != 0 {
		t.Fatalf("model requests = %#v", adapter.requests)
	}
}

func TestCoordinatorRunReturnsOperationDispatchError(t *testing.T) {
	store := emptyFakeStore()
	value := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady,
	}
	store.resume.Operations = []operation.Operation{value}
	operations := newFakeOperationManager()
	operations.addError = func(operation.Operation) error {
		return errors.New("dispatch failed")
	}
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	err := current.Run(t.Context())
	if err == nil || err.Error() != `dispatch operation "operation-1": dispatch failed` {
		t.Fatalf("Run error = %v", err)
	}
}

func TestCoordinatorRunReturnsUnsupportedRecoveredOperation(t *testing.T) {
	store := emptyFakeStore()
	store.resume.Operations = []operation.Operation{
		{ID: "unsupported", Type: "remote", Version: 1, Status: operation.StatusReady},
		{ID: "supported", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady},
	}
	inboxContext, cancelInbox := context.WithCancel(t.Context())
	cancelInbox()
	inputs, inboxErr := inbox.New(inboxContext, nil)
	if inboxErr != nil {
		t.Fatal(inboxErr)
	}
	operations := newFakeOperationManager()
	operations.addError = func(value operation.Operation) error {
		if value.ID == "unsupported" {
			return fmt.Errorf("manager does not support %q: %w", value.Type, operation.ErrUnsupported)
		}
		return nil
	}
	current := newTestCoordinator(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	err := current.Run(t.Context())
	if !errors.Is(err, operation.ErrUnsupported) {
		t.Fatalf("Run error = %v, want unsupported operation", err)
	}
	if _, ok := current.state.operations["unsupported"]; !ok {
		t.Fatal("unsupported operation was not retained")
	}
}

func TestCoordinatorClonesOperationDataBeforeDispatch(t *testing.T) {
	operations := newFakeOperationManager()
	operations.mutateAdds = true
	current := newTestCoordinator(
		emptyFakeStore(),
		newTestInbox(t),
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)
	value := current.addOperationToLocalState(operation.Operation{
		ID:          "operation-1",
		Type:        operation.TypeShell,
		Version:     1,
		Status:      operation.StatusReady,
		State:       jsontext.Value(`{"state":"original"}`),
		Idempotency: jsontext.Value(`{"key":"original"}`),
	})

	if err := current.dispatchOperationsToManager(); err != nil {
		t.Fatal(err)
	}
	stored := current.state.operations[value.ID]
	if string(stored.State) != `{"state":"original"}` ||
		string(stored.Idempotency) != `{"key":"original"}` {
		t.Fatalf("stored operation was mutated: %#v", stored)
	}
}

func TestCoordinatorRunReturnsInputStoreErrorAfterUpdatingLocalState(t *testing.T) {
	store := emptyFakeStore()
	store.appendInputErr = errors.New("disk unavailable")
	inputs := newTestInbox(t)
	event := externalEvent(t, 1, "input-1", "hello")
	submitTestInput(t, inputs, event)
	current := newTestCoordinator(
		store,
		inputs,
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	err := current.Run(t.Context())
	if err == nil || err.Error() != `store input "input-1": disk unavailable` {
		t.Fatalf("Run error = %v", err)
	}
	built, buildErr := current.dependencies.ContextBuilder.Build()
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	want := withPreamble(t, llm.Item{
		Type: llm.ItemMessage,
		Data: llm.Message{Role: llm.RoleUser, Text: "hello"},
	})
	if !reflect.DeepEqual(built.Request.Input, want) {
		t.Fatalf("built input = %#v, want %#v", built.Request.Input, want)
	}
}

func TestCoordinatorRunReturnsOperationStoreErrorAfterUpdatingLocalState(t *testing.T) {
	store := emptyFakeStore()
	store.saveOperationErr = errors.New("disk unavailable")
	inputs := newTestInbox(t)
	operations := newFakeOperationManager()
	update := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusAwaiting,
	}
	operations.updates <- update
	current := newTestCoordinator(
		store,
		inputs,
		operations,
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)

	err := current.Run(t.Context())
	if err == nil || err.Error() != `store operation "operation-1": disk unavailable` {
		t.Fatalf("Run error = %v", err)
	}
	if !reflect.DeepEqual(current.state.operations[update.ID], update) {
		t.Fatalf("operation = %#v, want %#v", current.state.operations[update.ID], update)
	}
	if len(operations.adds) != 0 {
		t.Fatalf("operations dispatched before store commit = %#v", operations.adds)
	}
}

func TestCoordinatorStoresEverySessionItemKind(t *testing.T) {
	store := emptyFakeStore()
	current := newTestCoordinator(
		store,
		newTestInbox(t),
		newFakeOperationManager(),
		contextbuilder.NewBuilder(),
		tool.NewRegistry(tool.StaticTranslators{}),
	)
	event := externalEvent(t, 1, "input-1", "hello")
	turn := session.Turn{ID: "turn-1", Type: session.TurnRegular}
	response := sessionstore.ModelResponse{
		TurnID:   turn.ID,
		Response: llm.Response{ID: "response-1"},
	}
	value := operation.Operation{
		ID: "operation-1", Type: operation.TypeShell, Version: 1, Status: operation.StatusReady,
	}
	current.addOperationToLocalState(value)
	status := sessionstore.ToolCallStatus{
		TurnID:     turn.ID,
		CallID:     "call-1",
		Status:     tool.CallStatus{WaitingFor: []operation.ID{value.ID}},
		Operations: []operation.Operation{value},
	}
	items := []sessionstore.Item{
		{Kind: sessionstore.ItemInput, Data: event},
		{Kind: sessionstore.ItemTurn, Data: turn},
		{Kind: sessionstore.ItemModelResponse, Data: response},
		{Kind: sessionstore.ItemToolCallStatus, Data: status},
	}
	for _, item := range items {
		if err := current.storeItemInSessionStore(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}

	if !reflect.DeepEqual(store.appendedInputs, []inbox.Input{event}) ||
		!reflect.DeepEqual(store.appendedTurns, []session.Turn{turn}) ||
		!reflect.DeepEqual(store.appendedResponses, []sessionstore.ModelResponse{response}) ||
		!reflect.DeepEqual(store.appendedStatuses, []sessionstore.ToolCallStatus{status}) {
		t.Fatalf("stored items: inputs=%#v turns=%#v responses=%#v statuses=%#v",
			store.appendedInputs,
			store.appendedTurns,
			store.appendedResponses,
			store.appendedStatuses,
		)
	}
}

func TestCoordinatorReturnsSessionItemStoreErrors(t *testing.T) {
	turn := session.Turn{ID: "turn-1", Type: session.TurnRegular}
	response := sessionstore.ModelResponse{TurnID: turn.ID}
	status := sessionstore.ToolCallStatus{
		TurnID: turn.ID,
		CallID: "call-1",
		Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
	}
	tests := []struct {
		name      string
		item      sessionstore.Item
		configure func(*fakeStore, *coordinator)
		want      string
	}{
		{
			name: "turn",
			item: sessionstore.Item{Kind: sessionstore.ItemTurn, Data: turn},
			configure: func(store *fakeStore, _ *coordinator) {
				store.appendTurnErr = errors.New("disk unavailable")
			},
			want: `store turn "turn-1": disk unavailable`,
		},
		{
			name: "model response",
			item: sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: response},
			configure: func(store *fakeStore, _ *coordinator) {
				store.appendModelResponseErr = errors.New("disk unavailable")
			},
			want: `store turn "turn-1" response: disk unavailable`,
		},
		{
			name: "tool status",
			item: sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: status},
			configure: func(store *fakeStore, _ *coordinator) {
				store.appendStatusErr = errors.New("disk unavailable")
			},
			want: `store tool call "call-1" status: disk unavailable`,
		},
		{
			name: "unsupported item",
			item: sessionstore.Item{Kind: sessionstore.ItemFork, Data: sessionstore.Fork{}},
			configure: func(*fakeStore, *coordinator) {
			},
			want: `unsupported local item kind "fork"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := emptyFakeStore()
			current := newTestCoordinator(
				store,
				newTestInbox(t),
				newFakeOperationManager(),
				contextbuilder.NewBuilder(),
				tool.NewRegistry(tool.StaticTranslators{}),
			)
			test.configure(store, current)

			err := current.storeItemInSessionStore(t.Context(), test.item)
			if err == nil || err.Error() != test.want {
				t.Fatalf("store error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCoordinatorRunReturnsContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := emptyFakeStore()
		inputs := newTestInbox(t)
		operations := newFakeOperationManager()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		current := newTestCoordinator(
			store,
			inputs,
			operations,
			contextbuilder.NewBuilder(),
			tool.NewRegistry(tool.StaticTranslators{}),
		)
		done := make(chan error, 1)
		go func() {
			done <- current.Run(ctx)
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context cancellation", err)
		}
	})
}

func TestClosedInputErrorPrefersContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := closedInputError(ctx, "input"); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed input error = %v, want context cancellation", err)
	}
}

func newTestCoordinator(
	store *fakeStore,
	inputs *inbox.Inbox,
	operations *fakeOperationManager,
	builder contextbuilder.Builder,
	registry tool.Registry,
) *coordinator {
	return newTestCoordinatorWithAdapter(
		store,
		inputs,
		operations,
		builder,
		registry,
		&fakeAdapter{},
	)
}

func newTestCoordinatorWithAdapter(
	store *fakeStore,
	inputs *inbox.Inbox,
	operations *fakeOperationManager,
	builder contextbuilder.Builder,
	registry tool.Registry,
	adapter *fakeAdapter,
) *coordinator {
	return New(Dependencies{
		SessionID:      "session-1",
		Inbox:          inputs,
		Restored:       store.resume,
		Sessions:       store,
		ContextBuilder: builder,
		LLM:            adapter,
		Tools:          registry,
		Operations:     operations,
	}).(*coordinator)
}

// withPreamble expects the items after the preamble every context builder starts with.
func withPreamble(t *testing.T, items ...llm.Item) []llm.Item {
	t.Helper()
	initial, err := contextbuilder.NewBuilder().Build()
	if err != nil {
		t.Fatal(err)
	}
	return append(initial.Request.Input, items...)
}

func emptyFakeStore() *fakeStore {
	return &fakeStore{resume: sessionstore.ResumeState{Snapshot: sessionstore.Snapshot{
		Session: session.Session{ID: "session-1"},
	}}}
}

func externalEvent(t *testing.T, _ int, id inbox.ID, text string) inbox.Input {
	t.Helper()
	payload, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return inbox.Input{ID: id, Kind: inbox.InputExternal, Payload: payload}
}

func storedItem(sequence sessionstore.Sequence, kind sessionstore.ItemKind, data any) sessionstore.Item {
	return sessionstore.Item{Sequence: sequence, Kind: kind, Data: data}
}

type testTranslator struct{}

func (testTranslator) Translate(tool.Context, llm.ToolCall) tool.CallStatus {
	return tool.CallStatus{}
}

func (testTranslator) TranslateResult(
	_ string,
	status tool.CallStatus,
	_ []operation.Operation,
) (llm.ToolResult, error) {
	return llm.ToolResult{Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "error:" + status.Error}}}, nil
}

type submittingTranslator struct {
	specs       []operation.Spec
	calls       []llm.ToolCall
	onTranslate func()
}

func (translator *submittingTranslator) Translate(
	ctx tool.Context,
	call llm.ToolCall,
) tool.CallStatus {
	translator.calls = append(translator.calls, call)
	if translator.onTranslate != nil {
		translator.onTranslate()
	}
	status := tool.CallStatus{WaitingFor: make([]operation.ID, 0, len(translator.specs))}
	for _, spec := range translator.specs {
		status.WaitingFor = append(status.WaitingFor, ctx.Submit(spec))
	}
	return status
}

func (*submittingTranslator) TranslateResult(
	callID string,
	status tool.CallStatus,
	_ []operation.Operation,
) (llm.ToolResult, error) {
	return llm.ToolResult{CallID: callID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: status.Error}}}, nil
}

type operationStatusTranslator struct{}

func (operationStatusTranslator) Translate(tool.Context, llm.ToolCall) tool.CallStatus {
	return tool.CallStatus{}
}

func (operationStatusTranslator) TranslateResult(
	_ string,
	_ tool.CallStatus,
	operations []operation.Operation,
) (llm.ToolResult, error) {
	statuses := make([]string, 0, len(operations))
	for _, value := range operations {
		statuses = append(statuses, string(value.Status))
	}
	return llm.ToolResult{Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Join(statuses, ",")}}}, nil
}

type failingResultTranslator struct {
	err error
}

func (failingResultTranslator) Translate(tool.Context, llm.ToolCall) tool.CallStatus {
	return tool.CallStatus{}
}

func (translator failingResultTranslator) TranslateResult(
	string,
	tool.CallStatus,
	[]operation.Operation,
) (llm.ToolResult, error) {
	return llm.ToolResult{}, translator.err
}

type terminalResultTranslator struct{}

func (terminalResultTranslator) Translate(tool.Context, llm.ToolCall) tool.CallStatus {
	return tool.CallStatus{}
}

func (terminalResultTranslator) TranslateResult(
	_ string,
	_ tool.CallStatus,
	operations []operation.Operation,
) (llm.ToolResult, error) {
	for _, value := range operations {
		if operationIsTerminal(value.Status) {
			return llm.ToolResult{}, errors.New("terminal result failed")
		}
	}
	return llm.ToolResult{Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "pending"}}}, nil
}

type itemRequest struct {
	After sessionstore.Sequence
	Limit int
}

type fakeStore struct {
	resume                 sessionstore.ResumeState
	items                  []sessionstore.Item
	itemsErr               error
	itemsPage              *sessionstore.Page
	itemRequests           []itemRequest
	appendedInputs         []inbox.Input
	appendedTurns          []session.Turn
	appendedResponses      []sessionstore.ModelResponse
	appendedStatuses       []sessionstore.ToolCallStatus
	savedOperations        []operation.Operation
	appendInputErr         error
	appendTurnErr          error
	appendModelResponseErr error
	appendStatusErr        error
	saveOperationErr       error
	unexpectedMutations    []string
	onAppendInput          func(inbox.Input)
	onAppendTurn           func(session.Turn)
	onAppendModelResponse  func(sessionstore.ModelResponse)
	onAppendToolCallStatus func(sessionstore.ToolCallStatus)
	onSaveOperation        func(operation.Operation)
}

func (*fakeStore) AddObserver(sessionstore.Observer) sessionstore.ObserverID {
	return sessionstore.ObserverID{}
}

func (*fakeStore) RemoveObserver(sessionstore.ObserverID) {}

func (store *fakeStore) Create(context.Context, session.ID) (sessionstore.Snapshot, error) {
	store.unexpectedMutations = append(store.unexpectedMutations, "create")
	return sessionstore.Snapshot{}, errors.New("unexpected create")
}

func (*fakeStore) ListSessions(context.Context) ([]sessionstore.SessionInfo, error) {
	return nil, errors.New("unexpected list sessions")
}

func (store *fakeStore) Inspect(context.Context, session.ID) (sessionstore.Snapshot, error) {
	return store.resume.Snapshot, nil
}

func (store *fakeStore) Items(
	_ context.Context,
	_ session.ID,
	after sessionstore.Sequence,
	limit int,
) (sessionstore.Page, error) {
	store.itemRequests = append(store.itemRequests, itemRequest{After: after, Limit: limit})
	if store.itemsErr != nil {
		return sessionstore.Page{}, store.itemsErr
	}
	if store.itemsPage != nil {
		return *store.itemsPage, nil
	}
	start := sort.Search(len(store.items), func(index int) bool {
		return store.items[index].Sequence > after
	})
	end := min(start+limit, len(store.items))
	items := append([]sessionstore.Item(nil), store.items[start:end]...)
	next := after
	if len(items) != 0 {
		next = items[len(items)-1].Sequence
	}
	return sessionstore.Page{Items: items, NextAfter: next, More: end < len(store.items)}, nil
}

func (store *fakeStore) AppendInput(_ context.Context, _ session.ID, input inbox.Input) error {
	store.appendedInputs = append(store.appendedInputs, input)
	if store.onAppendInput != nil {
		store.onAppendInput(input)
	}
	return store.appendInputErr
}

func (store *fakeStore) AppendTurn(_ context.Context, _ session.ID, turn session.Turn) error {
	store.appendedTurns = append(store.appendedTurns, turn)
	if store.onAppendTurn != nil {
		store.onAppendTurn(turn)
	}
	return store.appendTurnErr
}

func (store *fakeStore) AppendModelResponse(
	_ context.Context,
	_ session.ID,
	response sessionstore.ModelResponse,
) error {
	store.appendedResponses = append(store.appendedResponses, response)
	if store.onAppendModelResponse != nil {
		store.onAppendModelResponse(response)
	}
	return store.appendModelResponseErr
}

func (store *fakeStore) AppendToolCallStatus(
	_ context.Context,
	_ session.ID,
	status sessionstore.ToolCallStatus,
) error {
	store.appendedStatuses = append(store.appendedStatuses, status)
	if store.onAppendToolCallStatus != nil {
		store.onAppendToolCallStatus(status)
	}
	return store.appendStatusErr
}

func (store *fakeStore) SaveOperation(
	_ context.Context,
	_ session.ID,
	value operation.Operation,
) error {
	store.savedOperations = append(store.savedOperations, value)
	if store.onSaveOperation != nil {
		store.onSaveOperation(value)
	}
	return store.saveOperationErr
}

func (store *fakeStore) Resume(context.Context, session.ID) (sessionstore.ResumeState, error) {
	return store.resume, nil
}

func (store *fakeStore) Fork(
	context.Context,
	session.ID,
	session.ID,
	session.TurnID,
) (sessionstore.Snapshot, error) {
	store.unexpectedMutations = append(store.unexpectedMutations, "fork")
	return sessionstore.Snapshot{}, errors.New("unexpected fork")
}

func newTestInbox(t *testing.T) *inbox.Inbox {
	t.Helper()
	inputs, err := inbox.New(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

func submitTestInput(t *testing.T, inputs *inbox.Inbox, input inbox.Input) {
	t.Helper()
	if err := inputs.Submit(t.Context(), input); err != nil {
		t.Fatal(err)
	}
}

type fakeOperationManager struct {
	updates       chan operation.Operation
	adds          []operation.Operation
	addError      func(operation.Operation) error
	mutateAdds    bool
	cancels       []operation.ID
	cancelReasons []string
	cancelErr     error
}

func newFakeOperationManager() *fakeOperationManager {
	return &fakeOperationManager{updates: make(chan operation.Operation, 8)}
}

func (manager *fakeOperationManager) Add(value operation.Operation) error {
	manager.adds = append(manager.adds, value)
	if manager.mutateAdds {
		value.State[0] = '!'
		value.Idempotency[0] = '!'
	}
	if manager.addError != nil {
		return manager.addError(value)
	}
	return nil
}

func (manager *fakeOperationManager) Cancel(id operation.ID, reason string) error {
	manager.cancels = append(manager.cancels, id)
	manager.cancelReasons = append(manager.cancelReasons, reason)
	return manager.cancelErr
}

func (manager *fakeOperationManager) Updates() <-chan operation.Operation {
	return manager.updates
}

type fakeAdapter struct {
	mutex          sync.Mutex
	requests       []llm.Request
	requestOptions []llm.RequestOptions
	respond        func(context.Context, llm.Request) (llm.Response, error)
}

func (adapter *fakeAdapter) Respond(
	ctx context.Context,
	request llm.Request,
	options llm.RequestOptions,
) (llm.Response, error) {
	adapter.mutex.Lock()
	adapter.requests = append(adapter.requests, request)
	adapter.requestOptions = append(adapter.requestOptions, options)
	respond := adapter.respond
	adapter.mutex.Unlock()
	if respond != nil {
		return respond(ctx, request)
	}
	return llm.Response{}, errors.New("unexpected respond")
}

func (adapter *fakeAdapter) requestOptionsSnapshot() []llm.RequestOptions {
	adapter.mutex.Lock()
	defer adapter.mutex.Unlock()
	return append([]llm.RequestOptions(nil), adapter.requestOptions...)
}

func (adapter *fakeAdapter) requestSnapshot() []llm.Request {
	adapter.mutex.Lock()
	defer adapter.mutex.Unlock()
	return append([]llm.Request(nil), adapter.requests...)
}

type failingBuilder struct {
	contextbuilder.Builder
	err error
}

func (builder failingBuilder) Build() (contextbuilder.Result, error) {
	return contextbuilder.Result{}, builder.err
}

func receiveTestValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for value")
		var zero T
		return zero
	}
}
