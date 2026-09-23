package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func TestCoordinatorRunReturnsDispatchErrorForNewOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		want := errors.New("dispatch failed")
		run.operations.addError = func(operation.Operation) error { return want }
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run tool"))
		run.respond(t, 0, toolGraceResponse("A"))
		select {
		case err := <-run.done:
			if !errors.Is(err, want) {
				t.Fatalf("Run error = %v, want %v", err, want)
			}
		default:
			t.Fatal("Run did not return the dispatch error")
		}
		if len(run.calls) != 1 || len(run.store.appendedStatuses) != 1 || len(run.operations.adds) != 1 {
			t.Fatal("dispatch failure started another turn or failed to preserve the committed operation")
		}
		if !reflect.DeepEqual(run.store.appendedStatuses[0].Operations, run.operations.adds) {
			t.Fatal("dispatched operation differs from its committed state")
		}
	})
}

func TestCoordinatorDispatchesOnlyNewToolOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "first", "run tool"))
		run.respond(t, 0, toolGraceResponse("A"))
		run.input(t, externalEvent(t, 1, "second", "run another tool"))
		run.respond(t, 1, toolGraceResponse("B"))
		updateToolGraceCall(t, run, "A", operation.StatusAwaiting)
		run.input(t, heartbeatInput(t, "heartbeat"))
		run.respond(t, 2, textResponse("Waiting."))
		if len(run.operations.adds) != 2 || run.operations.adds[0].ID == run.operations.adds[1].ID {
			t.Fatalf("operation dispatches = %#v, want each new operation once", run.operations.adds)
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		updateToolGraceCall(t, run, "A", operation.StatusCompleted)
		updateToolGraceCall(t, run, "B", operation.StatusCompleted)
		run.respond(t, 3, textResponse("A completed."))
		run.respond(t, 4, textResponse("B completed."))
		run.assertStopped(t)
		if len(run.operations.adds) != 2 {
			t.Fatal("result delivery redispatched operations")
		}
	})
}

func TestCoordinatorSchedulingResultFailurePreventsDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		spec, err := operation.NewValueSpec(jsontext.Value(`1`))
		if err != nil {
			t.Fatal(err)
		}
		want := errors.New("cannot translate result")
		translator := &submittingResultErrorTranslator{err: want}
		translator.specs = []operation.Spec{spec}
		run.current.dependencies.Tools = tool.NewRegistry(tool.StaticTranslators{Bash: translator}, tool.BashName)
		adapter := &fakeAdapter{respond: func(context.Context, llm.Request) (llm.Response, error) {
			return llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{
				CallID: "call-1", Name: tool.BashName, Arguments: `{}`,
			}}}}, nil
		}}
		run.current.dependencies.LLM = adapter
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run it"))
		if err := <-run.done; !errors.Is(err, want) {
			t.Fatalf("Run error = %v, want result translation error", err)
		}
		if len(translator.calls) != 1 || len(run.store.appendedResponses) != 1 {
			t.Fatal("model response was not persisted and translated")
		}
		if len(run.store.appendedStatuses) != 0 || len(run.operations.adds) != 0 || len(adapter.requestSnapshot()) != 1 {
			t.Fatal("failed result translation committed a status, dispatched work, or started another request")
		}
	})
}

func TestCoordinatorReconciliationRejectsUntranslatedCall(t *testing.T) {
	run := newStopTestRun(t, 1)
	run.store.items = run.store.items[:2]
	if err := run.current.loadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	statuses, err := run.current.reconcileToolCalls(t.Context())
	if err == nil || err.Error() != `reconcile untranslated tool call "call-0" in turn "turn-1"` {
		t.Fatalf("reconciliation error = %v, want untranslated-call error", err)
	}
	if len(statuses) != 0 || len(run.store.appendedStatuses) != 0 || len(run.current.state.toolCalls) != 1 || run.current.pendingInputs() != 0 {
		t.Fatal("reconciliation completed an untranslated call")
	}
}

type submittingResultErrorTranslator struct {
	submittingTranslator
	err error
}

func (translator *submittingResultErrorTranslator) TranslateResult(string, tool.CallStatus, []operation.Operation) (llm.ToolResult, error) {
	return llm.ToolResult{}, translator.err
}

func TestCoordinatorRunDefersCompletionsUntilModelFinishes(t *testing.T) {
	for _, startWithInput := range []bool{false, true} {
		for _, steer := range []bool{false, true} {
			t.Run(fmt.Sprintf("input=%t/steer=%t", startWithInput, steer), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store, registry := independentToolCalls(t, 3)
					inputs := newTestInbox(t)
					operations := newFakeOperationManager()
					type modelCall struct {
						ctx      context.Context
						request  llm.Request
						response chan llm.Response
					}
					var calls []modelCall
					adapter := &fakeAdapter{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
						call := modelCall{ctx: ctx, request: request, response: make(chan llm.Response)}
						calls = append(calls, call)
						select {
						case response := <-call.response:
							return response, nil
						case <-ctx.Done():
							return llm.Response{}, ctx.Err()
						}
					}}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					current := newTestCoordinatorWithAdapter(
						store, inputs, operations, contextbuilder.NewBuilder(), registry, adapter,
					)
					var runErr error
					go func() { runErr = current.Run(ctx) }()

					finishOperation := func(index int) {
						value := store.resume.Operations[index]
						value.Status = operation.StatusCompleted
						operations.updates <- value
						synctest.Wait()
						synctest.Sleep(2 * slurpIdleTimeout)
						if len(store.appendedStatuses) != index+1 {
							t.Fatalf("completed tool statuses = %d, want %d", len(store.appendedStatuses), index+1)
						}
						if status := store.appendedStatuses[index]; status.CallID != fmt.Sprintf("call-%d", index) {
							t.Fatalf("completed call = %q", status.CallID)
						}
					}
					firstPending := 0
					if startWithInput {
						submitTestInput(t, inputs, externalEvent(t, 1, "progress", "check progress"))
						synctest.Wait()
						synctest.Sleep(2 * slurpIdleTimeout)
					} else {
						finishOperation(0)
						firstPending = 1
					}
					if len(calls) != 1 {
						t.Fatalf("model requests = %d, want 1", len(calls))
					}
					first := calls[0]
					for index := firstPending; index < 3; index++ {
						finishOperation(index)
						if err := first.ctx.Err(); err != nil {
							t.Fatalf("operation %d canceled active model request: %v", index, err)
						}
						if len(calls) != 1 {
							t.Fatalf("model requests = %d, want 1", len(calls))
						}
					}
					response := llm.Response{ID: "progress-response", Output: []llm.Item{{
						Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "progress"},
					}}}
					if steer {
						submitTestInput(t, inputs, externalEvent(t, 2, "steering", "summarize results"))
						synctest.Wait()
						synctest.Sleep(2 * slurpIdleTimeout)
					} else {
						first.response <- response
						synctest.Wait()
						synctest.Sleep(2 * slurpIdleTimeout)
					}
					if len(calls) != 2 {
						t.Fatalf("model requests = %d, want 2", len(calls))
					}
					next := calls[1]
					assertCompletedResults(t, next.request, 3)
					if steer {
						if !errors.Is(first.ctx.Err(), context.Canceled) {
							t.Fatal("steering did not cancel active model request")
						}
						last := next.request.Input[len(next.request.Input)-1]
						if last.Type != llm.ItemMessage || last.Data.(llm.Message).Text != "summarize results" {
							t.Fatalf("steering missing from request: %#v", next.request)
						}
					}
					next.response <- llm.Response{ID: "final-response"}
					synctest.Wait()
					if len(calls) != 2 || len(store.appendedTurns) != 2 {
						t.Fatalf("model requests = %d, turns = %d, want 2 each", len(calls), len(store.appendedTurns))
					}
					if !steer && !reflect.DeepEqual(store.appendedResponses[0].Response, response) {
						t.Fatalf("active model response was not preserved: %#v", store.appendedResponses)
					}
					cancel()
					synctest.Wait()
					if !errors.Is(runErr, context.Canceled) {
						t.Fatalf("Run error = %v, want context cancellation", runErr)
					}
				})
			})
		}
	}
}

func TestCoordinatorRunSlurpsIndependentOperationCompletions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, registry := independentToolCalls(t, 3)
		operations := newFakeOperationManager()
		for _, value := range store.resume.Operations {
			value.Status = operation.StatusCompleted
			operations.updates <- value
		}
		adapter := &fakeAdapter{respond: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
			<-ctx.Done()
			return llm.Response{}, ctx.Err()
		}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		current := newTestCoordinatorWithAdapter(
			store, newTestInbox(t), operations, contextbuilder.NewBuilder(), registry, adapter,
		)
		var runErr error
		go func() { runErr = current.Run(ctx) }()
		synctest.Wait()
		synctest.Sleep(2 * slurpIdleTimeout)
		requests := adapter.requestSnapshot()
		if len(requests) != 1 {
			t.Fatalf("model requests = %d, want 1", len(requests))
		}
		assertCompletedResults(t, requests[0], 3)
		if len(store.savedOperations) != 3 || len(store.appendedStatuses) != 3 || len(store.appendedTurns) != 1 {
			t.Fatalf("completion effects: operations=%d statuses=%d turns=%d",
				len(store.savedOperations), len(store.appendedStatuses), len(store.appendedTurns))
		}
		cancel()
		synctest.Wait()
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("Run error = %v, want context cancellation", runErr)
		}
	})
}

func TestCoordinatorRunPersistsCompletedUpdatesBeforeClosure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, registry := independentToolCalls(t, 3)
		operations := newFakeOperationManager()
		var completed []operation.Operation
		for _, value := range store.resume.Operations {
			value.Status = operation.StatusCompleted
			completed = append(completed, value)
			operations.updates <- value
		}
		close(operations.updates)
		adapter := &fakeAdapter{respond: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
			<-ctx.Done()
			return llm.Response{}, ctx.Err()
		}}
		builder := contextbuilder.NewBuilder()
		current := newTestCoordinatorWithAdapter(store, newTestInbox(t), operations, builder, registry, adapter)
		err := current.Run(t.Context())
		if err == nil || err.Error() != "operation updates closed" {
			t.Fatalf("Run error = %v, want closed operation updates error", err)
		}
		if !reflect.DeepEqual(store.savedOperations, completed) || len(store.appendedStatuses) != 3 {
			t.Fatalf("completion effects: operations=%v statuses=%v", store.savedOperations, store.appendedStatuses)
		}
		built, err := builder.Build()
		if err != nil {
			t.Fatal(err)
		}
		assertCompletedResults(t, built.Request, 3)
	})
}

func independentToolCalls(t *testing.T, count int) (*fakeStore, tool.Registry) {
	t.Helper()
	registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: operationStatusTranslator{}}, tool.ViewImageName)
	store := emptyFakeStore()
	store.items = []sessionstore.Item{
		storedItem(1, sessionstore.ItemTurn, session.Turn{ID: "turn-1", Type: session.TurnRegular}),
		storedItem(2, sessionstore.ItemModelResponse, sessionstore.ModelResponse{}),
	}
	response := sessionstore.ModelResponse{TurnID: "turn-1"}
	for index := range count {
		value := operation.Operation{
			ID: operation.ID(fmt.Sprintf("operation-%d", index)), Type: operation.TypeShell,
			Version: 1, Status: operation.StatusAwaiting,
		}
		call := llm.ToolCall{CallID: fmt.Sprintf("call-%d", index), Name: tool.ViewImageName, Arguments: `{}`}
		response.Response.Output = append(response.Response.Output, llm.Item{Type: llm.ItemToolCall, Data: call})
		store.resume.Operations = append(store.resume.Operations, value)
		store.items = append(store.items,
			storedItem(sessionstore.Sequence(len(store.items)+1), sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{
				TurnID: "turn-1", CallID: call.CallID,
				Status: tool.CallStatus{WaitingFor: []operation.ID{value.ID}}, Operations: []operation.Operation{value},
			}),
		)
	}
	store.items[1].Data = response
	return store, registry
}

func assertCompletedResults(t *testing.T, request llm.Request, count int) {
	t.Helper()
	completed := make(map[string]int)
	for _, item := range request.Input {
		if item.Type == llm.ItemToolResult {
			result := item.Data.(llm.ToolResult)
			if result.Output[0].Value == string(operation.StatusCompleted) {
				completed[result.CallID]++
			}
		}
	}
	if len(completed) != count {
		t.Fatalf("completed results = %v, want %d", completed, count)
	}
	for index := range count {
		if completed[fmt.Sprintf("call-%d", index)] != 1 {
			t.Fatalf("completed results = %v, want each call once", completed)
		}
	}
}
