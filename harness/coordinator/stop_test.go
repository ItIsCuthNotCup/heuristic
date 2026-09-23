package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/ItIsCuthNotCup/heuristic/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

func TestCoordinatorRemainsAvailableUntilStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		run.assertRunning(t)
		for index := range 2 {
			run.input(t, externalEvent(t, index, inbox.ID(fmt.Sprintf("input-%d", index)), "hello"))
			run.respond(t, index, textResponse("hello"))
			run.assertRunning(t)
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
		if len(run.calls) != 2 {
			t.Fatalf("requests = %d, want 2", len(run.calls))
		}
	})
}

func TestCoordinatorWhenIdleDeliversPendingResults(t *testing.T) {
	for _, terminal := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		for _, duringRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/during_request=%t", terminal, duringRequest), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					run := newStopTestRun(t, 1)
					run.start(t)
					run.input(t, externalEvent(t, 1, "progress", "check progress"))
					run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
					if len(run.calls) != 1 || run.calls[0].ctx.Err() != nil {
						t.Fatal("when_idle interrupted the model request")
					}
					if duringRequest {
						run.update(t, 0, terminal)
					}
					run.respond(t, 0, textResponse("The check is still running."))
					run.assertRunning(t)
					if !duringRequest {
						if len(run.calls) != 1 {
							t.Fatal("requested model response before tool completed")
						}
						run.update(t, 0, terminal)
					}
					if len(run.calls) != 2 {
						t.Fatalf("requests = %d, want 2", len(run.calls))
					}
					assertStopResult(t, run.calls[1].request, "call-0", string(terminal))
					run.respond(t, 1, textResponse("All results received."))
					run.assertStopped(t)
					if len(run.operations.cancels) != 0 {
						t.Fatal("when_idle canceled an operation")
					}
				})
			})
		}
	}
}

func TestCoordinatorStopsAfterCancellationIsRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 2)
		run.start(t)
		run.input(t, externalEvent(t, 1, "progress", "check progress"))
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		if !errors.Is(run.calls[0].ctx.Err(), context.Canceled) {
			t.Fatal("stop did not interrupt active model request")
		}
		if len(run.operations.cancels) != 2 {
			t.Fatalf("cancellations = %v, want both operations", run.operations.cancels)
		}
		for _, reason := range run.operations.cancelReasons {
			if reason != "user requested stop" {
				t.Fatalf("cancellation reason = %q", reason)
			}
		}
		run.assertRunning(t)
		run.update(t, 0, operation.StatusCanceled)
		run.assertRunning(t)
		run.update(t, 1, operation.StatusCompleted)
		if len(run.store.savedOperations) != 2 || len(run.store.appendedStatuses) != 2 {
			t.Fatal("stop did not persist terminal updates and tool results")
		}
		if len(run.calls) != 1 {
			t.Fatal("hard stop started another request")
		}
		run.assertStopped(t)
		if len(run.operations.cancels) != 2 {
			t.Fatal("operation cancellation was requested more than once")
		}
	})
}

func TestCoordinatorCancelsCompactionWithoutRecordingAResponse(t *testing.T) {
	for _, mode := range []inbox.ControlMode{inbox.StopHard, "steer"} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				// Return a successful result even after cancellation.
				run.current.dependencies.LLM = &fakeAdapter{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
					call := stopTestCall{ctx: ctx, request: request, response: make(chan llm.Response)}
					run.calls = append(run.calls, call)
					return <-call.response, nil
				}}
				run.start(t)
				run.input(t, externalEvent(t, 0, "first", "hello"))
				run.current.state.currentTurnType = session.TurnCompaction
				if mode == "steer" {
					run.input(t, externalEvent(t, 0, "second", "change course"))
				} else {
					run.input(t, stopInput(t, "stop", mode))
				}
				wantType := session.TurnRegular
				if mode == inbox.StopHard {
					wantType = session.TurnCompaction
				}
				if run.calls[0].ctx.Err() == nil || run.current.state.currentTurnType != wantType {
					t.Fatal("cancellation did not preserve the recorded turn kind until the next turn")
				}
				run.respond(t, 0, textResponse("Late summary"))
				if len(run.store.appendedResponses) != 0 || run.current.state.deliveredInputs != 0 {
					t.Fatal("canceled compaction recorded a response or delivered input")
				}
				if mode != inbox.StopHard {
					if len(run.calls) != 2 || run.current.cancelModel == nil {
						t.Fatal("late response affected the replacement request")
					}
					run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
					run.respond(t, 1, textResponse("Done"))
				}
				run.assertStopped(t)
			})
		})
	}
}

func TestCoordinatorHardStopOverridesWhenIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		run.start(t)
		run.input(t, externalEvent(t, 0, "first", "hello"))
		run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		if run.calls[0].ctx.Err() != nil {
			t.Fatal("when_idle interrupted the model")
		}
		run.input(t, stopInput(t, "hard", inbox.StopHard))
		run.input(t, stopInput(t, "idle-again", inbox.StopWhenIdle), stopInput(t, "hard-again", inbox.StopHard), heartbeatInput(t, "heartbeat"), externalEvent(t, 1, "late", "more work"))
		run.assertRunning(t)
		if run.current.stop.request.Mode != inbox.StopHard || len(run.operations.cancels) != 1 {
			t.Fatal("later inputs changed the stop or repeated cancellation")
		}
		if len(run.calls) != 1 || !errors.Is(run.calls[0].ctx.Err(), context.Canceled) {
			t.Fatal("hard stop did not interrupt the model or later input started another request")
		}
		run.update(t, 0, operation.StatusCanceled)
		run.assertStopped(t)
		if len(run.operations.cancels) != 1 || len(run.calls) != 1 {
			t.Fatal("stop repeated cancellation or started another model request")
		}
	})
}

func TestCoordinatorHardStopDiscardsLateModelResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		run.current.dependencies.LLM = &fakeAdapter{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
			call := stopTestCall{ctx: ctx, request: request, response: make(chan llm.Response)}
			run.calls = append(run.calls, call)
			return <-call.response, nil
		}}
		run.start(t)
		run.input(t, externalEvent(t, 0, "first", "hello"))
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		run.assertRunning(t)
		run.respond(t, 0, llm.Response{Output: []llm.Item{{
			Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "late", Name: tool.ViewImageName, Arguments: `{}`},
		}}})
		if len(run.store.appendedResponses) != 0 || len(run.store.appendedStatuses) != 0 || len(run.calls) != 1 {
			t.Fatal("late response was recorded or translated during cancellation")
		}
		run.update(t, 0, operation.StatusCanceled)
		run.assertStopped(t)
	})
}

func TestCoordinatorStopPropagatesErrors(t *testing.T) {
	for _, failure := range []string{"input", "cancel", "operation", "tool-result"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 2)
				want := errors.New("test failure")
				switch failure {
				case "input":
					run.store.appendInputErr = want
				case "cancel":
					run.operations.cancelErr = want
				case "operation":
					run.store.saveOperationErr = want
				case "tool-result":
					run.store.appendStatusErr = want
				}
				run.start(t)
				run.input(t, stopInput(t, "stop", inbox.StopHard))
				if failure == "operation" || failure == "tool-result" {
					run.update(t, 0, operation.StatusCanceled)
				}
				select {
				case err := <-run.done:
					if !errors.Is(err, want) {
						t.Fatalf("Run error = %v, want %v", err, want)
					}
				default:
					t.Fatal("Run did not return the error")
				}
				if failure == "cancel" && len(run.operations.cancels) != 2 {
					t.Fatal("cancellation error prevented attempting the other operation")
				}
			})
		})
	}
}

func TestCoordinatorCancellationWhileCollectingUpdates(t *testing.T) {
	for _, source := range []string{"inbox", "operation updates"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				go func() { run.done <- run.current.Run(ctx) }()
				synctest.Wait()
				if source == "inbox" {
					submitTestInput(t, run.inputs, externalEvent(t, 0, "input", "hello"))
				} else {
					value := run.store.resume.Operations[0]
					value.Status = operation.StatusCompleted
					run.operations.updates <- value
				}
				synctest.Wait()
				want := errors.New("caller disconnected")
				cancel(want)
				synctest.Wait()
				if err := <-run.done; !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "slurp inbox:") {
					t.Fatalf("Run error = %v, want collection cancellation cause", err)
				}
				wantInputs, wantUpdates := 1, 0
				if source == "operation updates" {
					wantInputs, wantUpdates = 0, 1
				}
				if len(run.store.appendedInputs) != wantInputs || len(run.store.savedOperations) != wantUpdates || len(run.calls) != 0 {
					t.Fatal("selected event was not persisted before draining, or canceled draining started a model request")
				}
			})
		})
	}
}

func TestCoordinatorDrainsInboxAndOperationsBeforeCallingModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		run.start(t)
		input := externalEvent(t, 0, "input", "check completed work")
		submitTestInput(t, run.inputs, input)
		update := run.store.resume.Operations[0]
		update.Status = operation.StatusCompleted
		run.operations.updates <- update
		synctest.Wait()
		synctest.Sleep(2 * slurpIdleTimeout)
		synctest.Wait()

		if len(run.calls) != 1 {
			t.Fatalf("model requests = %d, want one request containing both events", len(run.calls))
		}
		assertCompletedResults(t, run.calls[0].request, 1)
		foundInput := false
		for _, item := range run.calls[0].request.Input {
			if item.Type == llm.ItemMessage && item.Data.(llm.Message).Text == "check completed work" {
				foundInput = true
			}
		}
		if !foundInput || len(run.store.appendedInputs) != 1 || len(run.store.savedOperations) != 1 {
			t.Fatal("request omitted the input, or selected and drained events were not persisted exactly once")
		}
	})
}

func TestCoordinatorCancellationTakesPrecedenceOverModelError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		results := make(chan error)
		run.current.dependencies.LLM = &fakeAdapter{respond: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
			select {
			case err := <-results:
				return llm.Response{}, err
			case <-ctx.Done():
				return llm.Response{}, ctx.Err()
			}
		}}
		go func() { run.done <- run.current.Run(ctx) }()
		synctest.Wait()
		run.input(t, externalEvent(t, 0, "input", "hello"))
		cancelModel := run.current.cancelModel
		if cancelModel == nil {
			t.Fatal("model request did not start")
		}
		// Cancel after the loop receives the model error, before it handles it.
		run.current.cancelModel = func() {
			cancel()
			cancelModel()
		}
		results <- errors.New("model failed")
		synctest.Wait()
		if err := <-run.done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context cancellation", err)
		}
		if len(run.store.appendedResponses) != 0 {
			t.Fatal("failed model response was persisted")
		}
	})
}

func TestCoordinatorStopControlsDoNotRepeatOnResume(t *testing.T) {
	for _, mode := range []inbox.ControlMode{inbox.StopHard, inbox.StopWhenIdle} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				run.store.items = append(run.store.items, storedItem(3, sessionstore.ItemInput, stopInput(t, "old-stop", mode)))
				run.start(t)
				run.assertRunning(t)
				run.input(t, externalEvent(t, 1, "new", "continue"))
				run.respond(t, 0, textResponse("Continuing."))
				run.assertRunning(t)
				run.input(t, stopInput(t, "new-stop", inbox.StopHard))
				run.assertStopped(t)
			})
		})
	}
}

func TestCoordinatorWhenIdleDeliversRestoredResults(t *testing.T) {
	for _, stage := range []string{"completed", "request-started", "response-recorded"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				status := run.store.items[2].Data.(sessionstore.ToolCallStatus)
				status.Operations[0].Status = operation.StatusCompleted
				run.store.items[2].Data = status
				run.store.resume.Operations = nil
				run.current.dependencies.Restored = run.store.resume
				if stage != "completed" {
					run.store.items = append(run.store.items,
						storedItem(4, sessionstore.ItemTurn, session.Turn{ID: "delivered", PreviousTurnID: "turn-1", Type: session.TurnRegular}),
					)
				}
				if stage == "response-recorded" {
					run.store.items = append(run.store.items,
						storedItem(5, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "delivered", Response: textResponse("Done.")}),
					)
				}
				run.start(t)
				run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				if stage != "response-recorded" {
					if len(run.calls) != 1 {
						t.Fatalf("requests = %d, want restored result delivery", len(run.calls))
					}
					assertStopResult(t, run.calls[0].request, "call-0", string(operation.StatusCompleted))
					run.respond(t, 0, textResponse("Done."))
				} else if len(run.calls) != 0 {
					t.Fatal("already delivered result caused another turn")
				}
				run.assertStopped(t)
			})
		})
	}
}

type stopTestCall struct {
	ctx      context.Context
	request  llm.Request
	response chan llm.Response
}

type stopTestRun struct {
	current    *coordinator
	store      *fakeStore
	inputs     *inbox.Inbox
	operations *fakeOperationManager
	calls      []stopTestCall
	done       chan error
}

func newStopTestRun(t *testing.T, pending int) *stopTestRun {
	t.Helper()
	store, registry := independentToolCalls(t, pending)
	run := &stopTestRun{store: store, inputs: newTestInbox(t), operations: newFakeOperationManager(), done: make(chan error, 1)}
	adapter := &fakeAdapter{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
		call := stopTestCall{ctx: ctx, request: request, response: make(chan llm.Response)}
		run.calls = append(run.calls, call)
		select {
		case response := <-call.response:
			return response, nil
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}}
	run.current = newTestCoordinatorWithAdapter(store, run.inputs, run.operations, contextbuilder.NewBuilder(), registry, adapter)
	return run
}

func (run *stopTestRun) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { run.done <- run.current.Run(ctx) }()
	synctest.Wait()
}

func (run *stopTestRun) input(t *testing.T, inputs ...inbox.Input) {
	t.Helper()
	for _, input := range inputs {
		submitTestInput(t, run.inputs, input)
	}
	synctest.Wait()
	synctest.Sleep(2 * slurpIdleTimeout)
	synctest.Wait()
}

func (run *stopTestRun) update(t *testing.T, index int, status operation.Status) {
	t.Helper()
	value := run.store.resume.Operations[index]
	value.Status = status
	run.operations.updates <- value
	synctest.Wait()
	synctest.Sleep(2 * slurpIdleTimeout)
	synctest.Wait()
}

func (run *stopTestRun) respond(t *testing.T, index int, response llm.Response) {
	t.Helper()
	if index >= len(run.calls) {
		t.Fatalf("request %d has not started", index)
	}
	run.calls[index].response <- response
	synctest.Wait()
	synctest.Sleep(2 * slurpIdleTimeout)
	synctest.Wait()
}

func (run *stopTestRun) assertRunning(t *testing.T) {
	t.Helper()
	select {
	case err := <-run.done:
		t.Fatalf("Run exited early: %v", err)
	default:
	}
}

func (run *stopTestRun) assertStopped(t *testing.T) {
	t.Helper()
	select {
	case err := <-run.done:
		if err != nil {
			t.Fatalf("Run error = %v", err)
		}
	default:
		t.Fatal("Run did not stop")
	}
}

func stopInput(t *testing.T, id inbox.ID, mode inbox.ControlMode) inbox.Input {
	t.Helper()
	payload, err := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: "user requested stop"})
	if err != nil {
		t.Fatal(err)
	}
	return inbox.Input{ID: id, Kind: inbox.InputControl, Payload: payload}
}

func textResponse(text string) llm.Response {
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}
}

func assertStopResult(t *testing.T, request llm.Request, callID, want string) {
	t.Helper()
	for _, item := range request.Input {
		if item.Type == llm.ItemToolResult {
			result := item.Data.(llm.ToolResult)
			if result.CallID == callID && result.Output[0].Value == want {
				return
			}
		}
	}
	t.Fatalf("request lacks result %q for %q", want, callID)
}
