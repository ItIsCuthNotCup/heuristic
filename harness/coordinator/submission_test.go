package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"reflect"
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

func TestCoordinatorSubmissionBoundarySurvivesRecovery(t *testing.T) {
	for _, stage := range []string{"in flight", "response committed", "turn write failed", "response write failed"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 2)
				store := persistTestRun(t, run)
				faults := &submissionFailureStore{Store: store}
				restoreTestRun(t, run, faults)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() { run.done <- run.current.Run(ctx) }()
				synctest.Wait()
				run.update(t, 1, operation.StatusCompleted)
				if len(run.calls) != 1 || run.current.state.currentTurnInputs != 1 {
					t.Fatal("completion before submission was not included immediately")
				}
				sent := run.calls[0].request
				original := append([]llm.Item(nil), sent.Input...)
				assertStopResult(t, sent, "call-1", string(operation.StatusCompleted))
				assertStopResult(t, sent, "call-0", contextbuilder.ToolCallRunningPayload)
				for _, item := range sent.Input {
					if item.Type != llm.ItemToolResult {
						continue
					}
					result := item.Data.(llm.ToolResult)
					if result.CallID == "call-1" && result.Output[0].Value == contextbuilder.ToolCallRunningPayload {
						t.Fatal("completion before submission retained its running placeholder")
					}
				}
				run.update(t, 0, operation.StatusCompleted)
				run.input(t, stopInput(t, "heartbeat", inbox.Heartbeat))
				if len(run.calls) != 1 || run.calls[0].ctx.Err() != nil {
					t.Fatal("late inputs interrupted or replaced the request")
				}
				suffix := []llm.Item{
					{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-0", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCompleted)}}}},
					{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "user requested stop"}},
				}
				want := sent
				want.Input = append(append([]llm.Item(nil), original...), suffix...)
				response := textResponse("Waiting for A.")
				failure := errors.New("injected write failure")
				switch stage {
				case "turn write failed":
					faults.turnErr = failure
				case "response write failed":
					faults.responseErr = failure
				}
				if stage != "in flight" {
					run.respond(t, 0, response)
					if stage != "response write failed" {
						want.Input = append(append(append([]llm.Item(nil), original...), response.Output...), suffix...)
					}
				}
				if stage == "response committed" {
					if len(run.calls) != 2 || !reflect.DeepEqual(run.calls[1].request, want) {
						t.Fatal("next request did not place the response before late inputs")
					}
					if run.current.state.deliveredInputs != 1 || run.current.state.currentTurnInputs != 3 {
						t.Fatal("late inputs were counted as delivered by the earlier response")
					}
				}
				if !reflect.DeepEqual(sent.Input, original) {
					t.Fatal("in-flight request was mutated")
				}
				if stage == "turn write failed" || stage == "response write failed" {
					if err := <-run.done; !errors.Is(err, failure) {
						t.Fatalf("Run error = %v, want %v", err, failure)
					}
					if len(run.calls) != 1 {
						t.Fatal("failed persistence started another model request")
					}
				} else {
					cancel()
					synctest.Wait()
					if err := <-run.done; !errors.Is(err, context.Canceled) {
						t.Fatalf("Run error = %v", err)
					}
				}

				resumed := newStopTestRun(t, 0)
				restoreTestRun(t, resumed, store)
				resumed.start(t)
				if len(resumed.calls) != 1 || !reflect.DeepEqual(resumed.calls[0].request, want) {
					t.Fatal("resume did not reconstruct submitted history and include pending inputs")
				}
				assertCompletedResults(t, resumed.calls[0].request, 2)
				if len(resumed.operations.adds) != 0 {
					t.Fatal("resume redispatched completed operations")
				}
				resumed.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				resumed.respond(t, 0, textResponse("Done."))
				resumed.assertStopped(t)
				if resumed.current.pendingInputs() != 0 || len(resumed.calls) != 1 {
					t.Fatal("completion delivery did not settle in one response")
				}
			})
		})
	}
}

func TestCoordinatorSteeringCommitsPendingSuffixBeforeNewResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 2)
		store := persistTestRun(t, run)
		restoreTestRun(t, run, store)
		run.start(t)
		run.input(t, externalEvent(t, 0, "first", "first input"))
		initial := run.calls[0].request
		run.update(t, 0, operation.StatusCompleted)
		run.input(t, externalEvent(t, 1, "steer", "steering input"))
		if len(run.calls) != 2 || run.calls[0].ctx.Err() == nil {
			t.Fatal("steering did not replace the request")
		}
		want := initial
		want.Input = append(append([]llm.Item(nil), initial.Input...),
			llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-0", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCompleted)}}}},
			llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "steering input"}},
		)
		if !reflect.DeepEqual(run.calls[1].request, want) {
			t.Fatal("replacement request omitted or reordered pending inputs")
		}
		run.update(t, 1, operation.StatusCompleted)
		response := textResponse("Current response.")
		run.respond(t, 1, response)
		want.Input = append(want.Input, response.Output...)
		want.Input = append(want.Input, llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-1", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCompleted)}}}})
		if len(run.calls) != 3 || !reflect.DeepEqual(run.calls[2].request, want) {
			t.Fatal("new response did not land at the replacement request boundary")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.respond(t, 2, textResponse("Done."))
		run.assertStopped(t)
	})
}

type submissionFailureStore struct {
	sessionstore.Store
	inputErr    error
	turnErr     error
	responseErr error
}

func (store *submissionFailureStore) AppendInput(ctx context.Context, id session.ID, input inbox.Input) error {
	if store.inputErr != nil {
		return store.inputErr
	}
	return store.Store.AppendInput(ctx, id, input)
}

func (store *submissionFailureStore) AppendTurn(ctx context.Context, id session.ID, turn session.Turn) error {
	if store.turnErr != nil {
		return store.turnErr
	}
	return store.Store.AppendTurn(ctx, id, turn)
}

func (store *submissionFailureStore) AppendModelResponse(ctx context.Context, id session.ID, response sessionstore.ModelResponse) error {
	if store.responseErr != nil {
		return store.responseErr
	}
	return store.Store.AppendModelResponse(ctx, id, response)
}

func TestCoordinatorRecoversPendingResultsAfterInputWriteFailure(t *testing.T) {
	for _, kind := range []string{"external", "heartbeat", "hard stop"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 2)
				store := persistTestRun(t, run)
				faults := &submissionFailureStore{Store: store}
				restoreTestRun(t, run, faults)
				run.start(t)
				run.update(t, 1, operation.StatusCompleted)
				want := run.calls[0].request
				run.update(t, 0, operation.StatusCompleted)
				want.Input = append(append([]llm.Item(nil), want.Input...), llm.Item{
					Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-0", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCompleted)}}},
				})
				input := externalEvent(t, 0, "unpersisted", "unpersisted input")
				switch kind {
				case "heartbeat":
					input = stopInput(t, "unpersisted", inbox.Heartbeat)
				case "hard stop":
					input = stopInput(t, "unpersisted", inbox.StopHard)
				}
				failure := errors.New("input write failed")
				faults.inputErr = failure
				run.input(t, input)
				if err := <-run.done; !errors.Is(err, failure) {
					t.Fatalf("Run error = %v, want %v", err, failure)
				}
				if len(run.calls) != 1 || len(run.operations.cancels) != 0 {
					t.Fatal("unpersisted input triggered a request or cancellation")
				}
				resumed := newStopTestRun(t, 0)
				restoreTestRun(t, resumed, store)
				resumed.start(t)
				if len(resumed.calls) != 1 || !reflect.DeepEqual(resumed.calls[0].request, want) {
					t.Fatal("resume lost persisted results or retained the failed input")
				}
				if resumed.current.state.availableInputs != 2 || len(resumed.operations.adds) != 0 {
					t.Fatal("resume miscounted results or redispatched completed operations")
				}
				resumed.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
				resumed.respond(t, 0, textResponse("Done."))
				resumed.assertStopped(t)
				if resumed.current.pendingInputs() != 0 || len(resumed.calls) != 1 {
					t.Fatal("restored results were not delivered exactly once")
				}
			})
		})
	}
}

func TestCoordinatorStartsNewToolWhileDeliveringPreviousCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 2)
		store := persistTestRun(t, run)
		spec, err := operation.NewValueSpec(jsontext.Value(`1`))
		if err != nil {
			t.Fatal(err)
		}
		translator := &submissionTranslator{}
		translator.specs = []operation.Spec{spec}
		run.current.dependencies.Tools = tool.NewRegistry(tool.StaticTranslators{Bash: translator, ViewImage: operationStatusTranslator{}}, tool.BashName, tool.ViewImageName)
		restoreTestRun(t, run, store)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { run.done <- run.current.Run(ctx) }()
		synctest.Wait()
		run.update(t, 1, operation.StatusCompleted)
		first := run.calls[0].request
		run.update(t, 0, operation.StatusCompleted)
		dispatches := len(run.operations.adds)
		response := llm.Response{Output: []llm.Item{
			{ProviderID: "reasoning", Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: jsontext.Value(`{"id":"reasoning","type":"reasoning","summary":[],"encrypted_content":"opaque"}`)}},
			{ProviderID: "item-C", Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "C", Name: tool.BashName, Arguments: `{}`}},
		}}
		run.respond(t, 0, response)
		if run.requestCount() != 1 {
			t.Fatal("previous completion started a turn instead of waiting for the new tool")
		}
		synctest.Sleep(time.Second - time.Nanosecond)
		if run.requestCount() != 1 {
			t.Fatal("previous completion started a turn before the tool grace period elapsed")
		}
		synctest.Sleep(time.Nanosecond)
		want := first
		want.Input = append(append([]llm.Item(nil), first.Input...), response.Output...)
		want.Input = append(want.Input,
			llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-0", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCompleted)}}}},
			llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "C", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: contextbuilder.ToolCallRunningPayload}}}},
		)
		if len(run.calls) != 2 || !reflect.DeepEqual(run.calls[1].request, want) {
			t.Fatal("new tool response, old completion, and new running result were reordered")
		}
		if len(translator.calls) != 1 || len(run.operations.adds) <= dispatches || run.current.state.deliveredInputs != 1 || run.current.state.currentTurnInputs != 2 {
			t.Fatalf("new work: translations=%d, dispatches=%d, delivered=%d, submitted=%d", len(translator.calls), len(run.operations.adds), run.current.state.deliveredInputs, run.current.state.currentTurnInputs)
		}
		value := run.operations.adds[dispatches]
		value.Status = operation.StatusCompleted
		run.operations.updates <- value
		synctest.Wait()
		synctest.Sleep(2 * slurpIdleTimeout)
		synctest.Wait()
		secondResponse := textResponse("Waiting for C.")
		run.respond(t, 1, secondResponse)
		want.Input = append(want.Input, secondResponse.Output...)
		want.Input = append(want.Input, llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "C", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCompleted)}}}})
		if len(run.calls) != 3 || !reflect.DeepEqual(run.calls[2].request, want) || run.current.state.deliveredInputs != 2 {
			t.Fatal("new completion was not delivered after its in-flight response")
		}
		cancel()
		synctest.Wait()
		if err := <-run.done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		resumed := newStopTestRun(t, 0)
		resumed.current.dependencies.Tools = tool.NewRegistry(tool.StaticTranslators{Bash: translator, ViewImage: operationStatusTranslator{}}, tool.BashName, tool.ViewImageName)
		restoreTestRun(t, resumed, store)
		resumed.start(t)
		if len(resumed.calls) != 1 || !reflect.DeepEqual(resumed.calls[0].request, want) || len(resumed.operations.adds) != 0 || len(translator.calls) != 1 {
			t.Fatal("replay changed the request or repeated completed work")
		}
		resumed.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		resumed.respond(t, 0, textResponse("Done."))
		resumed.assertStopped(t)
		if resumed.current.pendingInputs() != 0 || resumed.current.state.availableInputs != 3 || len(resumed.calls) != 1 {
			t.Fatal("completed calls were not delivered exactly once")
		}
	})
}

func TestCoordinatorHardStopPreservesPendingInputsOnReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 2)
		store := persistTestRun(t, run)
		restoreTestRun(t, run, store)
		run.start(t)
		run.input(t, externalEvent(t, 0, "first", "check progress"))
		want := run.calls[0].request
		run.update(t, 0, operation.StatusCompleted)
		run.input(t, stopInput(t, "hard", inbox.StopHard))
		if len(run.calls) != 1 || run.calls[0].ctx.Err() == nil || len(run.operations.cancels) != 1 {
			t.Fatal("hard stop failed to interrupt and cancel only pending work")
		}
		run.input(t, externalEvent(t, 1, "late", "follow up after stop"))
		run.update(t, 1, operation.StatusCanceled)
		run.assertStopped(t)
		if len(run.calls) != 1 {
			t.Fatal("hard stop started another model request")
		}
		want.Input = append(append([]llm.Item(nil), want.Input...),
			llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-0", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCompleted)}}}},
			llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "follow up after stop"}},
			llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-1", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(operation.StatusCanceled)}}}},
		)
		resumed := newStopTestRun(t, 0)
		restoreTestRun(t, resumed, store)
		resumed.start(t)
		if len(resumed.calls) != 1 || !reflect.DeepEqual(resumed.calls[0].request, want) || len(resumed.operations.adds) != 0 || len(resumed.operations.cancels) != 0 {
			t.Fatal("resume changed stop history, lost late input, or repeated operations")
		}
		resumed.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		resumed.respond(t, 0, textResponse("Follow-up received."))
		resumed.assertStopped(t)
		if resumed.current.pendingInputs() != 0 || len(resumed.calls) != 1 {
			t.Fatal("resumed stop history did not settle in one response")
		}
	})
}

type submissionTranslator struct {
	submittingTranslator
}

func (*submissionTranslator) TranslateResult(callID string, status tool.CallStatus, operations []operation.Operation) (llm.ToolResult, error) {
	return (operationStatusTranslator{}).TranslateResult(callID, status, operations)
}
