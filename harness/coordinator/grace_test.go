package coordinator

import (
	"encoding/json/jsontext"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func TestCoordinatorToolGraceBatchesCompletionsUntilAllCallsFinish(t *testing.T) {
	for _, terminal := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		t.Run(string(terminal), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newToolGraceTestRun(t)
				run.start(t)
				run.input(t, externalEvent(t, 0, "input", "run both tools"))
				run.respond(t, 0, toolGraceResponse("A", "B"))
				deadline := time.Now().Add(time.Second)
				updateToolGraceCall(t, run, "A", terminal)
				if run.requestCount() != 1 || len(run.store.appendedStatuses) != 3 {
					t.Fatal("first completion was not persisted and deferred")
				}
				synctest.Sleep(100 * time.Millisecond)
				updateToolGraceCall(t, run, "B", terminal)
				if run.requestCount() != 2 || !time.Now().Before(deadline) {
					t.Fatal("last completion did not end the grace period early")
				}
				assertStopResult(t, run.calls[1].request, "A", string(terminal))
				assertStopResult(t, run.calls[1].request, "B", string(terminal))
				run.respond(t, 1, textResponse("Done."))
				synctest.Sleep(time.Until(deadline) + time.Second)
				if run.requestCount() != 2 {
					t.Fatal("expired grace period delivered results again")
				}
			})
		})
	}
}

func TestCoordinatorToolGraceWaitsOnlyForLatestTurn(t *testing.T) {
	for _, endGrace := range []string{"steering", "expiry"} {
		t.Run(endGrace, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newToolGraceTestRun(t)
				run.start(t)
				run.input(t, externalEvent(t, 0, "input", "run three tools"))
				run.respond(t, 0, toolGraceResponse("A", "B", "C"))
				updateToolGraceCall(t, run, "A", operation.StatusCompleted)
				if endGrace == "steering" {
					run.input(t, externalEvent(t, 1, "steering", "run two more tools"))
				} else {
					synctest.Sleep(toolCallRunGracePeriod)
				}
				run.respond(t, 1, toolGraceResponse("D", "E"))
				deadline := time.Now().Add(toolCallRunGracePeriod)
				updateToolGraceCall(t, run, "B", operation.StatusCompleted)
				if run.requestCount() != 2 {
					t.Fatal("older completion ended the new turn's grace period")
				}
				updateToolGraceCall(t, run, "D", operation.StatusCompleted)
				if run.requestCount() != 2 {
					t.Fatal("partial completion ended the new turn's grace period")
				}
				updateToolGraceCall(t, run, "E", operation.StatusCompleted)
				if run.requestCount() != 3 || !time.Now().Before(deadline) {
					t.Fatal("older running call prevented the new turn's grace period from ending early")
				}
				for _, callID := range []string{"A", "B", "D", "E"} {
					assertStopResult(t, run.calls[2].request, callID, string(operation.StatusCompleted))
				}
				assertStopResult(t, run.calls[2].request, "C", contextbuilder.ToolCallRunningPayload)
				run.respond(t, 2, textResponse("Waiting for C."))
				updateToolGraceCall(t, run, "C", operation.StatusCompleted)
				if run.requestCount() != 4 {
					t.Fatal("older completion was deferred after the new turn's grace period ended")
				}
				assertStopResult(t, run.calls[3].request, "C", string(operation.StatusCompleted))
			})
		})
	}
}

func TestCoordinatorToolGraceDeadlineDoesNotResetOnCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run three tools"))
		run.respond(t, 0, toolGraceResponse("A", "B", "C"))
		deadline := time.Now().Add(time.Second)
		synctest.Sleep(100 * time.Millisecond)
		updateToolGraceCall(t, run, "A", operation.StatusCompleted)
		synctest.Sleep(400 * time.Millisecond)
		updateToolGraceCall(t, run, "B", operation.StatusCompleted)
		synctest.Sleep(time.Until(deadline) - time.Nanosecond)
		if run.requestCount() != 1 {
			t.Fatal("partial completions started a turn before the grace deadline")
		}
		synctest.Sleep(time.Nanosecond)
		if run.requestCount() != 2 {
			t.Fatal("pending results were not delivered at the original grace deadline")
		}
		assertStopResult(t, run.calls[1].request, "A", string(operation.StatusCompleted))
		assertStopResult(t, run.calls[1].request, "B", string(operation.StatusCompleted))
		assertStopResult(t, run.calls[1].request, "C", contextbuilder.ToolCallRunningPayload)
		synctest.Sleep(time.Second)
		if run.requestCount() != 2 || run.calls[1].ctx.Err() != nil {
			t.Fatal("grace deadline repeated or interrupted the continuation")
		}
	})
}

func TestCoordinatorToolGraceExpiryWithoutResultsDoesNotStartTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run both tools"))
		run.respond(t, 0, toolGraceResponse("A", "B"))
		synctest.Sleep(time.Second)
		if run.requestCount() != 1 {
			t.Fatal("grace expiry started a turn without pending inputs")
		}
		updateToolGraceCall(t, run, "A", operation.StatusCompleted)
		if run.requestCount() != 2 {
			t.Fatal("completion after grace expiry was deferred")
		}
		assertStopResult(t, run.calls[1].request, "A", string(operation.StatusCompleted))
		assertStopResult(t, run.calls[1].request, "B", contextbuilder.ToolCallRunningPayload)
	})
}

func TestCoordinatorInboxEndsToolGracePeriod(t *testing.T) {
	for _, kind := range []string{"external", "heartbeat"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newToolGraceTestRun(t)
				run.start(t)
				run.input(t, externalEvent(t, 0, "input", "run both tools"))
				run.respond(t, 0, toolGraceResponse("A", "B"))
				updateToolGraceCall(t, run, "A", operation.StatusCompleted)
				input := externalEvent(t, 1, "steering", "check progress")
				if kind == "heartbeat" {
					input = heartbeatInput(t, "heartbeat")
				}
				run.input(t, input)
				if run.requestCount() != 2 {
					t.Fatal("inbox input did not end the tool grace period")
				}
				assertStopResult(t, run.calls[1].request, "A", string(operation.StatusCompleted))
				assertStopResult(t, run.calls[1].request, "B", contextbuilder.ToolCallRunningPayload)
				run.respond(t, 1, textResponse("Waiting."))
				updateToolGraceCall(t, run, "B", operation.StatusCompleted)
				if run.requestCount() != 3 {
					t.Fatal("completion was deferred after inbox input ended the grace period")
				}
				assertStopResult(t, run.calls[2].request, "B", string(operation.StatusCompleted))
			})
		})
	}
}

func TestCoordinatorToolGraceDiscardsPreviousDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run both tools"))
		run.respond(t, 0, toolGraceResponse("A", "B"))
		oldDeadline := time.Now().Add(time.Second)
		synctest.Sleep(250 * time.Millisecond)
		run.input(t, externalEvent(t, 1, "steering", "run another tool"))
		run.respond(t, 1, toolGraceResponse("C"))
		newDeadline := time.Now().Add(time.Second)
		updateToolGraceCall(t, run, "A", operation.StatusCompleted)
		synctest.Sleep(time.Until(oldDeadline) + time.Nanosecond)
		if run.requestCount() != 2 {
			t.Fatal("previous deadline ended the new grace period")
		}
		synctest.Sleep(time.Until(newDeadline))
		if run.requestCount() != 3 {
			t.Fatal("new grace deadline did not deliver pending results")
		}
		assertStopResult(t, run.calls[2].request, "A", string(operation.StatusCompleted))
		assertStopResult(t, run.calls[2].request, "B", contextbuilder.ToolCallRunningPayload)
		assertStopResult(t, run.calls[2].request, "C", contextbuilder.ToolCallRunningPayload)
	})
}

func TestCoordinatorImmediateToolStatusBypassesGrace(t *testing.T) {
	for _, name := range []string{tool.ViewImageName, "unavailable"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newToolGraceTestRun(t)
				run.start(t)
				run.input(t, externalEvent(t, 0, "input", "run both tools"))
				response := toolGraceResponse("A")
				response.Output = append(response.Output, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{
					CallID: "immediate", Name: name, Arguments: `{}`,
				}})
				run.respond(t, 0, response)
				if run.requestCount() != 2 || len(run.operations.adds) != 1 {
					t.Fatal("immediate tool status was deferred or prevented valid work from dispatching")
				}
				want := ""
				if name == "unavailable" {
					want = `tool "unavailable" is not available`
				}
				assertStopResult(t, run.calls[1].request, "immediate", want)
				assertStopResult(t, run.calls[1].request, "A", contextbuilder.ToolCallRunningPayload)
			})
		})
	}
}

func TestCoordinatorStopDuringToolGracePeriod(t *testing.T) {
	for _, mode := range []inbox.ControlMode{inbox.StopHard, inbox.StopWhenIdle} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newToolGraceTestRun(t)
				run.start(t)
				run.input(t, externalEvent(t, 0, "input", "run tool"))
				run.respond(t, 0, toolGraceResponse("A"))
				run.input(t, stopInput(t, "stop", mode))
				terminal := operation.StatusCanceled
				if mode == inbox.StopWhenIdle {
					terminal = operation.StatusCompleted
					if len(run.operations.cancels) != 0 {
						t.Fatal("when-idle stop canceled pending work")
					}
				} else if len(run.operations.cancels) != 1 {
					t.Fatal("stop waited for grace expiry before canceling work")
				}
				if run.requestCount() != 1 {
					t.Fatal("stop started a turn before the operation finished")
				}
				updateToolGraceCall(t, run, "A", terminal)
				if mode != inbox.StopHard {
					if run.requestCount() != 2 {
						t.Fatal("when_idle waited for grace expiry before delivering the result")
					}
					assertStopResult(t, run.calls[1].request, "A", string(terminal))
					run.respond(t, 1, textResponse("Done."))
				} else if run.requestCount() != 1 {
					t.Fatal("hard stop started a final turn")
				}
				run.assertStopped(t)
			})
		})
	}
}

func newToolGraceTestRun(t *testing.T) *stopTestRun {
	t.Helper()
	run := newStopTestRun(t, 0)
	spec, err := operation.NewValueSpec(jsontext.Value(`1`))
	if err != nil {
		t.Fatal(err)
	}
	translator := &submissionTranslator{}
	translator.specs = []operation.Spec{spec}
	run.current.dependencies.Tools = tool.NewRegistry(tool.StaticTranslators{Bash: translator, ViewImage: operationStatusTranslator{}}, tool.BashName, tool.ViewImageName)
	return run
}

func toolGraceResponse(callIDs ...string) llm.Response {
	var response llm.Response
	for _, id := range callIDs {
		response.Output = append(response.Output, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{
			CallID: id, Name: tool.BashName, Arguments: `{}`,
		}})
	}
	return response
}

func updateToolGraceCall(t *testing.T, run *stopTestRun, callID string, terminal operation.Status) {
	t.Helper()
	for _, status := range run.store.appendedStatuses {
		if status.CallID == callID {
			value := status.Operations[0]
			value.Status = terminal
			run.operations.updates <- value
			synctest.Wait()
			synctest.Sleep(2 * slurpIdleTimeout)
			return
		}
	}
	t.Fatalf("tool call %q was not scheduled", callID)
}
