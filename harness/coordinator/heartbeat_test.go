package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
)

func TestCoordinatorHeartbeatsWhileWaitingForTools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 2)
		run.current.dependencies.ToolHeartbeatInterval = time.Minute
		run.start(t)
		advanceHeartbeatTime(30 * time.Second)
		run.update(t, 0, operation.StatusAwaiting)
		advanceHeartbeatTime(30*time.Second - (2 * slurpIdleTimeout) - time.Nanosecond)
		if run.requestCount() != 0 {
			t.Fatal("heartbeat arrived before the interval elapsed")
		}
		advanceHeartbeatTime(time.Nanosecond + (2 * slurpIdleTimeout))
		assertHeartbeatCount(t, run, 1)
		wantReason := "Heartbeat: waited 60 seconds for tool calls.\nRunning: " +
			`[{"CallID":"call-0","Name":"ViewImage","Arguments":"{}"},{"CallID":"call-1","Name":"ViewImage","Arguments":"{}"}]`
		if reason := latestHeartbeatReason(t, run); reason != wantReason {
			t.Fatalf("heartbeat reason = %q, want %q", reason, wantReason)
		}
		if run.requestCount() != 1 || countHeartbeatMessages(run.calls[0].request) != 1 {
			t.Fatal("heartbeat did not produce one model request with a check-in message")
		}
		advanceHeartbeatTime(3 * time.Minute)
		assertHeartbeatCount(t, run, 1)
		if run.requestCount() != 1 || run.calls[0].ctx.Err() != nil {
			t.Fatal("heartbeat interrupted or overlapped an active model request")
		}
		run.respond(t, 0, textResponse("Still waiting."))
		advanceHeartbeatTime(time.Minute - time.Nanosecond)
		assertHeartbeatCount(t, run, 1)
		advanceHeartbeatTime(time.Nanosecond + (2 * slurpIdleTimeout))
		assertHeartbeatCount(t, run, 2)
		if run.requestCount() != 2 || countHeartbeatMessages(run.calls[1].request) != 2 {
			t.Fatal("next heartbeat did not append exactly one check-in")
		}
		run.update(t, 0, operation.StatusCompleted)
		run.update(t, 1, operation.StatusCompleted)
		if run.requestCount() != 2 || run.calls[1].ctx.Err() != nil {
			t.Fatal("tool completion interrupted a heartbeat response")
		}
		run.respond(t, 1, textResponse("Waiting for results."))
		if run.requestCount() != 3 {
			t.Fatal("completed results were not delivered after the model response")
		}
		assertCompletedResults(t, run.calls[2].request, 2)
		run.respond(t, 2, textResponse("Done."))
		advanceHeartbeatTime(3 * time.Minute)
		assertHeartbeatCount(t, run, 2)
		if run.requestCount() != 3 || len(run.operations.cancels) != 0 {
			t.Fatal("heartbeats continued without pending calls or canceled operations")
		}
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestCoordinatorHeartbeatDisabledOrIdle(t *testing.T) {
	for _, test := range []struct {
		name     string
		pending  int
		interval time.Duration
	}{
		{name: "disabled", pending: 1},
		{name: "idle", interval: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newHeartbeatTestRun(t, test.pending)
				run.current.dependencies.ToolHeartbeatInterval = test.interval
				run.start(t)
				advanceHeartbeatTime(time.Hour)
				assertHeartbeatCount(t, run, 0)
				if run.requestCount() != 0 {
					t.Fatal("unexpected model request")
				}
			})
		})
	}
}

func TestCoordinatorHeartbeatYieldsToSteeringAndResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 2)
		run.current.dependencies.ToolHeartbeatInterval = time.Minute
		run.start(t)
		advanceHeartbeatTime(30 * time.Second)
		run.input(t, externalEvent(t, 1, "steering", "check progress"))
		advanceHeartbeatTime(time.Minute)
		assertHeartbeatCount(t, run, 0)
		run.respond(t, 0, textResponse("Waiting."))
		advanceHeartbeatTime(30 * time.Second)
		run.update(t, 0, operation.StatusCompleted)
		if run.requestCount() != 2 {
			t.Fatal("result waited for a heartbeat")
		}
		run.respond(t, 1, textResponse("One remains."))
		advanceHeartbeatTime(time.Minute - time.Nanosecond)
		assertHeartbeatCount(t, run, 0)
		advanceHeartbeatTime(time.Nanosecond + (2 * slurpIdleTimeout))
		assertHeartbeatCount(t, run, 1)
		wantReason := "Heartbeat: waited 60 seconds for tool calls.\nRunning: " +
			`[{"CallID":"call-1","Name":"ViewImage","Arguments":"{}"}]`
		if reason := latestHeartbeatReason(t, run); reason != wantReason {
			t.Fatalf("heartbeat reason = %q, want only the remaining call: %q", reason, wantReason)
		}
	})
}

func TestHeartbeatDiscardsPreviousDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 1)
		run.current.dependencies.ToolHeartbeatInterval = time.Minute
		run.start(t)
		oldDeadline := time.Now().Add(time.Minute)
		advanceHeartbeatTime(20 * time.Second)
		run.input(t, externalEvent(t, 1, "steering", "continue"))
		run.respond(t, 0, textResponse("Waiting."))
		newDeadline := time.Now().Add(time.Minute)
		advanceHeartbeatTime(time.Until(oldDeadline) + (2 * slurpIdleTimeout))
		assertHeartbeatCount(t, run, 0)
		if run.requestCount() != 1 {
			t.Fatal("discarded deadline started a turn")
		}
		advanceHeartbeatTime(time.Until(newDeadline) + (2 * slurpIdleTimeout))
		assertHeartbeatCount(t, run, 1)
	})
}

func TestHeartbeatIntervalShorterThanInboxBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 1)
		run.current.dependencies.ToolHeartbeatInterval = time.Nanosecond
		run.start(t)
		for index := range 3 {
			advanceHeartbeatTime(time.Nanosecond + (2 * slurpIdleTimeout))
			assertHeartbeatCount(t, run, index+1)
			if run.requestCount() != index+1 {
				t.Fatal("heartbeat prevented inbox delivery")
			}
			advanceHeartbeatTime(time.Second)
			assertHeartbeatCount(t, run, index+1)
			run.respond(t, index, textResponse("Waiting."))
		}
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		run.update(t, 0, operation.StatusCanceled)
		run.assertStopped(t)
	})
}

func TestQueuedHeartbeatPreservesActiveModelResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 1)
		run.start(t)
		run.input(t, externalEvent(t, 1, "steering", "check progress"))
		run.input(t, heartbeatInput(t, "queued-heartbeat"))
		if run.requestCount() != 1 || run.calls[0].ctx.Err() != nil {
			t.Fatal("queued heartbeat interrupted the model")
		}
		run.respond(t, 0, textResponse("Working."))
		if run.requestCount() != 2 || countHeartbeatMessages(run.calls[1].request) != 1 {
			t.Fatal("queued heartbeat was not delivered in the next turn")
		}
		run.respond(t, 1, textResponse("Waiting."))
		advanceHeartbeatTime(time.Hour)
		if run.requestCount() != 2 {
			t.Fatal("queued heartbeat was delivered twice")
		}
	})
}

func TestHeartbeatWaitsForAllOperationsOfCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 2)
		response := run.store.items[1].Data.(sessionstore.ModelResponse)
		response.Response.Output = response.Response.Output[:1]
		run.store.items[1].Data = response
		status := run.store.items[2].Data.(sessionstore.ToolCallStatus)
		status.Operations = run.store.resume.Operations
		status.Status.WaitingFor = []operation.ID{status.Operations[0].ID, status.Operations[1].ID}
		run.store.items[2].Data = status
		run.store.items = run.store.items[:3]
		run.current.dependencies.ToolHeartbeatInterval = time.Minute
		run.start(t)
		deadline := time.Now().Add(time.Minute)
		advanceHeartbeatTime(30 * time.Second)
		run.update(t, 0, operation.StatusCompleted)
		if run.requestCount() != 0 {
			t.Fatal("partial completion started a turn")
		}
		advanceHeartbeatTime(time.Until(deadline) + (2 * slurpIdleTimeout))
		assertHeartbeatCount(t, run, 1)
		run.respond(t, 0, textResponse("Waiting."))
		run.update(t, 1, operation.StatusCompleted)
		if run.requestCount() != 2 {
			t.Fatal("last operation did not complete the call")
		}
		assertStopResult(t, run.calls[1].request, "call-0", "completed,completed")
		run.respond(t, 1, textResponse("Done."))
		advanceHeartbeatTime(time.Minute)
		assertHeartbeatCount(t, run, 1)
	})
}

func TestCoordinatorHeartbeatStopsDuringCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 1)
		run.current.dependencies.ToolHeartbeatInterval = time.Minute
		run.start(t)
		advanceHeartbeatTime(30 * time.Second)
		run.input(t, stopInput(t, "stop", inbox.StopHard))
		advanceHeartbeatTime(3 * time.Minute)
		assertHeartbeatCount(t, run, 0)
		if run.requestCount() != 0 {
			t.Fatal("heartbeat ran during cancellation")
		}
		run.update(t, 0, operation.StatusCanceled)
		run.assertStopped(t)
	})
}

func TestHeartbeatControlPreservesWhenIdleStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 1)
		run.start(t)
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle), heartbeatInput(t, "heartbeat"))
		if run.current.stop.request.Mode != inbox.StopWhenIdle || run.requestCount() != 1 {
			t.Fatal("heartbeat changed the stop or failed to wake the model")
		}
		run.respond(t, 0, textResponse("Waiting."))
		run.update(t, 0, operation.StatusCompleted)
		run.respond(t, 1, textResponse("Done."))
		run.assertStopped(t)
	})
}

func TestCoordinatorHeartbeatPropagatesSubmissionFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 1)
		run.current.dependencies.ToolHeartbeatInterval = time.Second

		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		stoppedInbox, err := inbox.New(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := errors.New("heartbeat inbox stopped")
		cancel(want)
		run.store.onSaveOperation = func(operation.Operation) {
			// Swap on the coordinator goroutine while retaining its live inbox output.
			run.current.dependencies.Inbox = stoppedInbox
		}
		run.start(t)
		run.update(t, 0, operation.StatusAwaiting)

		advanceHeartbeatTime(time.Second)
		select {
		case err := <-run.done:
			if !errors.Is(err, want) {
				t.Fatalf("Run error = %v, want %v", err, want)
			}
		default:
			t.Fatal("Run did not return the heartbeat submission error")
		}
		assertHeartbeatCount(t, run, 0)
		if run.requestCount() != 0 {
			t.Fatal("model ran after heartbeat submission failed")
		}
	})
}

func TestCoordinatorHeartbeatRequiresPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 1)
		run.current.dependencies.ToolHeartbeatInterval = time.Second
		want := errors.New("heartbeat storage failed")
		run.store.appendInputErr = want
		run.start(t)
		advanceHeartbeatTime(time.Second)
		if err := <-run.done; !errors.Is(err, want) {
			t.Fatalf("Run error = %v, want %v", err, want)
		}
		if run.requestCount() != 0 {
			t.Fatal("model ran before heartbeat was persisted")
		}
	})
}

func TestCoordinatorReplaysHeartbeat(t *testing.T) {
	for _, stage := range []string{"input", "turn", "response"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newHeartbeatTestRun(t, 1)
				run.store.items = append(run.store.items, storedItem(4, sessionstore.ItemInput, heartbeatInput(t, "heartbeat-1")))
				if stage != "input" {
					run.store.items = append(run.store.items,
						storedItem(5, sessionstore.ItemTurn, session.Turn{ID: "heartbeat-turn", Type: session.TurnRegular}),
					)
				}
				if stage == "response" {
					run.store.items = append(run.store.items,
						storedItem(6, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "heartbeat-turn", Response: textResponse("Waiting.")}),
					)
				}
				run.start(t)
				built, err := run.current.dependencies.ContextBuilder.Build()
				if err != nil {
					t.Fatal(err)
				}
				if countHeartbeatMessages(built.Request) != 1 {
					t.Fatal("heartbeat was not replayed exactly once")
				}
				if stage == "response" {
					if run.requestCount() != 0 {
						t.Fatal("delivered heartbeat woke the model again on resume")
					}
				} else if run.requestCount() != 1 || !reflect.DeepEqual(built.Request, run.calls[0].request) {
					t.Fatal("undelivered heartbeat was not resumed")
				}
				assertHeartbeatCount(t, run, 0)
			})
		})
	}
}

func TestCoordinatorRejectsNegativeHeartbeatInterval(t *testing.T) {
	run := newHeartbeatTestRun(t, 0)
	run.current.dependencies.ToolHeartbeatInterval = -time.Second
	if err := run.current.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "heartbeat interval") {
		t.Fatalf("Run error = %v, want invalid interval", err)
	}
}

func assertHeartbeatCount(t *testing.T, run *heartbeatTestRun, want int) {
	t.Helper()
	count := 0
	ids := make(map[inbox.ID]bool)
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, input := range run.recordedInputs {
		if input.Kind != inbox.InputControl {
			continue
		}
		control, err := input.DecodeControlMessage()
		if err != nil {
			t.Fatal(err)
		}
		if control.Mode == inbox.Heartbeat {
			count++
			if input.ID == "" || ids[input.ID] {
				t.Fatal("heartbeat IDs must be nonempty and unique")
			}
			ids[input.ID] = true
		}
	}
	if count != want {
		t.Fatalf("heartbeats = %d, want %d", count, want)
	}
}

func latestHeartbeatReason(t *testing.T, run *heartbeatTestRun) string {
	t.Helper()
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, input := range slices.Backward(run.recordedInputs) {
		if input.Kind != inbox.InputControl {
			continue
		}
		control, err := input.DecodeControlMessage()
		if err != nil {
			t.Fatal(err)
		}
		if control.Mode == inbox.Heartbeat {
			return control.Reason
		}
	}
	t.Fatal("no recorded heartbeat")
	return ""
}

func heartbeatInput(t *testing.T, id inbox.ID) inbox.Input {
	t.Helper()
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.Heartbeat, Reason: "Heartbeat: waiting for tools."})
	if err != nil {
		t.Fatal(err)
	}
	return inbox.Input{ID: id, Kind: inbox.InputControl, Payload: payload}
}

func countHeartbeatMessages(request llm.Request) int {
	count := 0
	for _, item := range request.Input {
		if item.Type == llm.ItemMessage {
			message := item.Data.(llm.Message)
			if message.Role == llm.RoleUser && strings.HasPrefix(message.Text, "Heartbeat:") {
				count++
			}
		}
	}
	return count
}

func advanceHeartbeatTime(duration time.Duration) {
	synctest.Wait()
	synctest.Sleep(duration)
	synctest.Wait()
}

type heartbeatTestRun struct {
	*stopTestRun
	mu             sync.Mutex
	recordedInputs []inbox.Input
}

func newHeartbeatTestRun(t *testing.T, pending int) *heartbeatTestRun {
	t.Helper()
	run := &heartbeatTestRun{stopTestRun: newStopTestRun(t, pending)}
	run.store.onAppendInput = func(input inbox.Input) {
		run.mu.Lock()
		defer run.mu.Unlock()
		run.recordedInputs = append(run.recordedInputs, input)
	}
	return run
}

func (run *stopTestRun) requestCount() int {
	return len(run.current.dependencies.LLM.(*fakeAdapter).requestSnapshot())
}
