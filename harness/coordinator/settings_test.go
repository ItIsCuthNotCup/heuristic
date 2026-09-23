package coordinator

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
)

func TestCoordinatorSettingsDoNotWakeOrInterruptModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.start(t)
		run.input(t, settingsInput(t, "initial", inbox.Settings{ReasoningEffort: llm.ReasoningEffortLow}))
		if run.requestCount() != 0 || run.current.pendingInputs() != 0 {
			t.Fatal("settings woke an idle model")
		}
		run.assertRunning(t)
		run.input(t, externalEvent(t, 0, "prompt", "hello"))
		if run.requestCount() != 1 || run.calls[0].request.Model.ReasoningEffort != llm.ReasoningEffortLow {
			t.Fatal("next turn did not use settings")
		}
		run.input(t, settingsInput(t, "next", inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh}))
		if run.requestCount() != 1 || run.calls[0].ctx.Err() != nil || run.calls[0].request.Model.ReasoningEffort != llm.ReasoningEffortLow {
			t.Fatal("settings interrupted or altered the active request")
		}
		run.respond(t, 0, textResponse("Hello."))
		if run.requestCount() != 1 || run.current.pendingInputs() != 0 {
			t.Fatal("settings caused an extra turn after the response")
		}
		run.input(t, externalEvent(t, 1, "follow-up", "continue"))
		if run.requestCount() != 2 || run.calls[1].request.Model.ReasoningEffort != llm.ReasoningEffortHigh {
			t.Fatal("following turn did not use updated settings")
		}
		run.respond(t, 1, textResponse("Done."))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestCoordinatorSettingsPreserveToolGraceAndApplyToContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "prompt", "run both tools"))
		run.respond(t, 0, toolGraceResponse("A", "B"))
		updateToolGraceCall(t, run, "A", operation.StatusCompleted)
		grace := run.current.state.grace
		run.input(t, settingsInput(t, "settings", inbox.Settings{ReasoningEffort: llm.ReasoningEffortMax}))
		if run.requestCount() != 1 || len(run.current.state.graceToolCalls) != 1 || run.current.state.grace != grace || len(run.operations.cancels) != 0 {
			t.Fatal("settings disturbed pending operations or ended their grace period")
		}
		updateToolGraceCall(t, run, "B", operation.StatusCompleted)
		if run.requestCount() != 2 || run.calls[1].request.Model.ReasoningEffort != llm.ReasoningEffortMax {
			t.Fatal("automatic continuation did not use updated settings")
		}
		assertStopResult(t, run.calls[1].request, "A", string(operation.StatusCompleted))
		assertStopResult(t, run.calls[1].request, "B", string(operation.StatusCompleted))
	})
}

func TestCoordinatorSettingsFollowInboxOrderAndDeduplicate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newHeartbeatTestRun(t, 0)
		run.start(t)
		first := settingsInput(t, "first", inbox.Settings{ReasoningEffort: llm.ReasoningEffortLow})
		last := settingsInput(t, "last", inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh})
		run.input(t, first, last, first)
		if !reflect.DeepEqual(run.recordedInputs, []inbox.Input{first, last}) {
			t.Fatalf("recorded settings = %#v", run.recordedInputs)
		}
		run.input(t, externalEvent(t, 0, "prompt", "hello"))
		if run.calls[0].request.Model.ReasoningEffort != llm.ReasoningEffortHigh {
			t.Fatal("duplicate rolled back the latest settings")
		}
	})
}

func TestCoordinatorSettingsPreservePendingStop(t *testing.T) {
	for _, mode := range []inbox.ControlMode{inbox.StopHard, inbox.StopWhenIdle} {
		t.Run(string(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				run.start(t)
				run.input(t, stopInput(t, "stop", mode), settingsInput(t, "settings", inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh}))
				run.assertRunning(t)
				if run.current.stop.request.Mode != mode || run.requestCount() != 0 {
					t.Fatal("settings changed the pending stop or started a turn")
				}
				if mode == inbox.StopHard {
					if len(run.operations.cancels) != 1 {
						t.Fatal("settings prevented operation cancellation")
					}
					run.update(t, 0, operation.StatusCanceled)
					if run.requestCount() != 0 {
						t.Fatal("hard stop started a final turn")
					}
				} else {
					if len(run.operations.cancels) != 0 {
						t.Fatal("settings canceled pending work")
					}
					run.update(t, 0, operation.StatusCompleted)
					if run.requestCount() != 1 || run.calls[0].request.Model.ReasoningEffort != llm.ReasoningEffortHigh {
						t.Fatal("final turn did not use updated settings")
					}
					run.respond(t, 0, textResponse("Done."))
				}
				run.assertStopped(t)
			})
		})
	}
}

func TestCoordinatorSettingsRequirePersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		want := errors.New("settings storage failed")
		run.store.appendInputErr = want
		run.start(t)
		run.input(t, settingsInput(t, "settings", inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh}))
		if err := <-run.done; !errors.Is(err, want) {
			t.Fatalf("Run error = %v, want %v", err, want)
		}
		if run.requestCount() != 0 {
			t.Fatal("model ran after settings persistence failed")
		}
	})
}

func TestCoordinatorSettingsReplayOnResumeAndFork(t *testing.T) {
	for _, mode := range []string{"resume", "fork"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent := newStopTestRun(t, 0)
				store := persistTestRun(t, parent)
				for _, input := range []inbox.Input{settingsInput(t, "first", inbox.Settings{ReasoningEffort: llm.ReasoningEffortLow}), settingsInput(t, "last", inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh})} {
					if err := store.AppendInput(t.Context(), "session-1", input); err != nil {
						t.Fatal(err)
					}
				}
				turn := session.Turn{ID: "parent-turn", PreviousTurnID: "turn-1", Type: session.TurnRegular}
				if err := store.AppendTurn(t.Context(), "session-1", turn); err != nil {
					t.Fatal(err)
				}
				if err := store.AppendModelResponse(t.Context(), "session-1", sessionstore.ModelResponse{TurnID: turn.ID, Response: textResponse("Done.")}); err != nil {
					t.Fatal(err)
				}
				id := session.ID("session-1")
				if mode == "fork" {
					id = "child"
					if _, err := store.Fork(t.Context(), id, "session-1", turn.ID); err != nil {
						t.Fatal(err)
					}
				}
				restored, err := store.Resume(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				run := newStopTestRun(t, 0)
				run.current.dependencies.SessionID = id
				run.current.dependencies.Sessions = store
				run.current.dependencies.Restored = restored
				run.current.dependencies.ContextBuilder.SetModel(llm.Model{ID: "model", ReasoningEffort: llm.ReasoningEffortMedium})
				run.start(t)
				if run.requestCount() != 0 {
					t.Fatal("replayed settings started a turn")
				}
				run.input(t, externalEvent(t, 0, "prompt", "continue"))
				if run.calls[0].request.Model.ID != "model" || run.calls[0].request.Model.ReasoningEffort != llm.ReasoningEffortHigh {
					t.Fatal("latest recorded settings did not override initial configuration")
				}
			})
		})
	}
}

func settingsInput(t *testing.T, id inbox.ID, settings inbox.Settings) inbox.Input {
	t.Helper()
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: settings})
	if err != nil {
		t.Fatal(err)
	}
	return inbox.Input{ID: id, Kind: inbox.InputControl, Payload: payload}
}
