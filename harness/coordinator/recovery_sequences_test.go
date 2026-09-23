package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore/localfile"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func TestCoordinatorResumesMixedDeliveryAfterSteering(t *testing.T) {
	for _, stage := range []string{"steered-request", "followup-request"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 2)
				store := persistTestRun(t, run)
				restoreTestRun(t, run, store)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				run.current.dependencies.LLM = &fakeAdapter{respond: func(requestContext context.Context, request llm.Request) (llm.Response, error) {
					call := stopTestCall{ctx: requestContext, request: request, response: make(chan llm.Response)}
					run.calls = append(run.calls, call)
					if len(run.calls) == 1 {
						select {
						case response := <-call.response:
							return response, nil
						case <-ctx.Done():
							return llm.Response{}, ctx.Err()
						}
					}
					select {
					case response := <-call.response:
						return response, nil
					case <-requestContext.Done():
						return llm.Response{}, requestContext.Err()
					}
				}}
				go func() { run.done <- run.current.Run(ctx) }()
				synctest.Wait()
				run.input(t, externalEvent(t, 0, "first", "first input"))
				run.update(t, 0, operation.StatusCompleted)
				run.input(t, externalEvent(t, 1, "steer", "steering input"))
				run.update(t, 1, operation.StatusCompleted)
				if len(run.calls) != 2 || run.calls[0].ctx.Err() == nil {
					t.Fatal("steering did not replace the first request")
				}
				run.respond(t, 0, textResponse("Stale response."))
				if run.current.state.deliveredInputs != 0 {
					t.Fatal("stale response consumed pending inputs")
				}
				pending := 4
				if stage == "followup-request" {
					run.respond(t, 1, textResponse("Current response."))
					pending = 1
					if len(run.calls) != 3 {
						t.Fatal("completion during the steered request did not trigger delivery")
					}
				}
				cancel()
				synctest.Wait()
				if err := <-run.done; !errors.Is(err, context.Canceled) {
					t.Fatalf("Run error = %v", err)
				}

				resumed := newStopTestRun(t, 0)
				restoreTestRun(t, resumed, store)
				resumed.start(t)
				if got := resumed.current.pendingInputs(); got != pending {
					t.Fatalf("replayed balance = %d, want %d", got, pending)
				}
				if len(resumed.calls) != 1 {
					t.Fatalf("resumed requests = %d, want 1", len(resumed.calls))
				}
				request := resumed.calls[0].request
				assertCompletedResults(t, request, 2)
				messages := make(map[string]int)
				for _, item := range request.Input {
					if item.Type == llm.ItemMessage {
						messages[item.Data.(llm.Message).Text]++
					}
				}
				if messages["first input"] != 1 || messages["steering input"] != 1 || messages["Stale response."] != 0 {
					t.Fatalf("replayed messages = %v", messages)
				}
				resumed.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				resumed.respond(t, 0, textResponse("Done."))
				resumed.assertStopped(t)
				if resumed.current.pendingInputs() != 0 || len(resumed.calls) != 1 {
					t.Fatal("resumed delivery did not settle exactly once")
				}
			})
		})
	}
}

func TestCoordinatorResumesPartiallyCompletedToolCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		second := run.store.resume.Operations[0]
		second.ID = "operation-1"
		run.store.resume.Operations = append(run.store.resume.Operations, second)
		status := run.store.items[2].Data.(sessionstore.ToolCallStatus)
		status.Status.WaitingFor = append(status.Status.WaitingFor, second.ID)
		status.Operations = append(status.Operations, second)
		run.store.items[2].Data = status
		store := persistTestRun(t, run)
		first := status.Operations[0]
		first.Status = operation.StatusCompleted
		if err := store.SaveOperation(t.Context(), "session-1", first); err != nil {
			t.Fatal(err)
		}
		restoreTestRun(t, run, store)
		run.start(t)
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertRunning(t)
		if len(run.calls) != 0 || run.current.pendingInputs() != 0 {
			t.Fatal("partial completion produced a finished tool result")
		}
		if len(run.operations.adds) == 0 {
			t.Fatal("unfinished operation was not dispatched")
		}
		for _, dispatched := range run.operations.adds {
			if !reflect.DeepEqual(dispatched, second) {
				t.Fatalf("redispatched operation = %#v, want %#v", dispatched, second)
			}
		}
		run.update(t, 1, operation.StatusCompleted)
		if len(run.calls) != 1 {
			t.Fatalf("requests = %d, want one completed tool result", len(run.calls))
		}
		assertStopResult(t, run.calls[0].request, "call-0", "completed,completed")
		completedResults := 0
		for _, item := range run.calls[0].request.Input {
			if item.Type == llm.ItemToolResult && item.Data.(llm.ToolResult).Output[0].Value == "completed,completed" {
				completedResults++
			}
		}
		if completedResults != 1 {
			t.Fatalf("completed tool results = %d, want 1", completedResults)
		}
		run.update(t, 1, operation.StatusCompleted)
		if run.current.pendingInputs() != 1 {
			t.Fatal("duplicate terminal update changed the delivery balance")
		}
		run.respond(t, 0, textResponse("Done."))
		run.assertStopped(t)
		resumed := newStopTestRun(t, 0)
		restoreTestRun(t, resumed, store)
		if len(resumed.current.dependencies.Restored.Operations) != 0 {
			t.Fatal("settled operations were retained for recovery")
		}
		resumed.start(t)
		resumed.input(t, stopInput(t, "stop-again", inbox.StopWhenIdle))
		resumed.assertStopped(t)
		if len(resumed.calls) != 0 || len(resumed.operations.adds) != 0 {
			t.Fatal("settled tool call ran again on resume")
		}
	})
}

func TestCoordinatorRetriesRecoveryPersistenceFailures(t *testing.T) {
	for _, failure := range []string{"reconciliation", "initial-build", "initial-turn"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				store := persistTestRun(t, run)
				want := errors.New("injected failure")
				faults := &recoveryFailureStore{Store: store}
				terminal := operation.StatusCompleted
				value := run.store.resume.Operations[0]
				value.Status = terminal
				if err := store.SaveOperation(t.Context(), "session-1", value); err != nil {
					t.Fatal(err)
				}
				switch failure {
				case "reconciliation":
					faults.statusErr = want
				case "initial-build":
					run.current.dependencies.ContextBuilder = failingBuilder{Builder: run.current.dependencies.ContextBuilder, err: want}
				case "initial-turn":
					faults.turnErr = want
				}
				restoreTestRun(t, run, faults)
				run.start(t)
				select {
				case err := <-run.done:
					if !errors.Is(err, want) {
						t.Fatalf("Run error = %v, want %v", err, want)
					}
				default:
					t.Fatal("Run did not return the persistence failure")
				}
				if len(run.calls) != 0 {
					t.Fatal("model request started before its prerequisites were committed")
				}
				page, err := store.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 100)
				if err != nil {
					t.Fatal(err)
				}
				turns := 0
				for _, item := range page.Items {
					if item.Kind == sessionstore.ItemTurn {
						turns++
					}
				}
				if turns != 1 {
					t.Fatal("failed recovery persisted an extra turn")
				}

				resumed := newStopTestRun(t, 0)
				restoreTestRun(t, resumed, store)
				resumed.start(t)
				resumed.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				if len(resumed.calls) != 1 || len(resumed.operations.adds) != 0 {
					t.Fatal("retry did not deliver the saved result without redispatch")
				}
				assertStopResult(t, resumed.calls[0].request, "call-0", string(terminal))
				resumed.respond(t, 0, textResponse("Done."))
				resumed.assertStopped(t)
				if resumed.current.pendingInputs() != 0 || len(resumed.calls) != 1 {
					t.Fatal("retry did not settle delivery exactly once")
				}
			})
		})
	}
}

func TestCoordinatorRetriesInitialToolStatusPersistenceFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		run.store.items = run.store.items[:2]
		store := persistTestRun(t, run)
		spec, err := operation.NewValueSpec(jsontext.Value(`1`))
		if err != nil {
			t.Fatal(err)
		}
		translator := &submittingTranslator{specs: []operation.Spec{spec}}
		registry := tool.NewRegistry(tool.StaticTranslators{ViewImage: translator}, tool.ViewImageName)
		run.current.dependencies.Tools = registry
		want := errors.New("cannot persist initial tool status")
		restoreTestRun(t, run, &recoveryFailureStore{Store: store, statusErr: want})
		run.start(t)
		select {
		case err := <-run.done:
			if !errors.Is(err, want) {
				t.Fatalf("Run error = %v, want %v", err, want)
			}
		default:
			t.Fatal("Run did not return the persistence failure")
		}
		if len(translator.calls) != 1 || len(run.operations.adds) != 0 || len(run.calls) != 0 {
			t.Fatal("failed status commit did not prevent dispatch and model requests")
		}
		page, err := store.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 || page.Items[1].Kind != sessionstore.ItemModelResponse {
			t.Fatalf("history after failed commit = %#v", page.Items)
		}

		resumed := newStopTestRun(t, 0)
		resumed.current.dependencies.Tools = registry
		restoreTestRun(t, resumed, store)
		if len(resumed.current.dependencies.Restored.Operations) != 0 {
			t.Fatal("failed status commit persisted operations")
		}
		resumed.start(t)
		if len(translator.calls) != 2 || len(resumed.operations.adds) != 1 {
			t.Fatal("retry did not translate and dispatch the unfinished call")
		}
		restored, err := store.Resume(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(restored.Operations, resumed.operations.adds) {
			t.Fatal("dispatched operation differs from committed operation")
		}
		completed := resumed.operations.adds[0]
		completed.Status = operation.StatusCompleted
		resumed.operations.updates <- completed
		synctest.Wait()
		synctest.Sleep(2 * slurpIdleTimeout)
		resumed.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		if len(resumed.calls) != 1 {
			t.Fatalf("requests = %d, want completed result delivery", len(resumed.calls))
		}
		assertStopResult(t, resumed.calls[0].request, "call-0", "")
		resumed.respond(t, 0, textResponse("Done."))
		resumed.assertStopped(t)
	})
}

func persistTestRun(t *testing.T, run *stopTestRun) *localfile.Store {
	t.Helper()
	store, err := localfile.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurn(t.Context(), "session-1", run.store.items[0].Data.(session.Turn)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModelResponse(t.Context(), "session-1", run.store.items[1].Data.(sessionstore.ModelResponse)); err != nil {
		t.Fatal(err)
	}
	for _, item := range run.store.items[2:] {
		if err := store.AppendToolCallStatus(t.Context(), "session-1", item.Data.(sessionstore.ToolCallStatus)); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func restoreTestRun(t *testing.T, run *stopTestRun, store sessionstore.Store) {
	t.Helper()
	restored, err := store.Resume(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	run.current.dependencies.Sessions, run.current.dependencies.Restored = store, restored
}

type recoveryFailureStore struct {
	sessionstore.Store
	statusErr error
	turnErr   error
}

func (store *recoveryFailureStore) AppendToolCallStatus(ctx context.Context, id session.ID, status sessionstore.ToolCallStatus) error {
	if store.statusErr != nil {
		return store.statusErr
	}
	return store.Store.AppendToolCallStatus(ctx, id, status)
}

func (store *recoveryFailureStore) AppendTurn(ctx context.Context, id session.ID, turn session.Turn) error {
	if store.turnErr != nil {
		return store.turnErr
	}
	return store.Store.AppendTurn(ctx, id, turn)
}
