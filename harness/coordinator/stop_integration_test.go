package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
)

func TestCoordinatorStopCancelsShellProcess(t *testing.T) {
	store, registry := independentToolCalls(t, 1)
	spec, err := operation.NewShellSpec(operation.ShellInput{Shell: "/bin/sh", Command: "exec sleep 30"}, t.TempDir(), 64)
	if err != nil {
		t.Fatal(err)
	}
	value := store.resume.Operations[0]
	value.Status, value.State = operation.StatusReady, spec.State
	value.Type, value.Version = spec.Type, spec.Version
	value.MaxOutputLength = spec.MaxOutputLength
	store.resume.Operations[0] = value
	status := store.items[2].Data.(sessionstore.ToolCallStatus)
	status.Operations[0] = value
	store.items[2].Data = status
	inputs := newTestInbox(t)
	stop := stopInput(t, "stop", inbox.StopHard)
	var processGroupID int
	store.onSaveOperation = func(update operation.Operation) {
		var state operation.ShellState
		if err := json.Unmarshal(update.State, &state); err != nil {
			store.saveOperationErr = err
			return
		}
		if processGroupID == 0 && state.ProcessGroupID > 1 {
			processGroupID = state.ProcessGroupID
			submitTestInput(t, inputs, stop)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	adapter := &fakeAdapter{respond: func(context.Context, llm.Request) (llm.Response, error) {
		return textResponse("Stopped."), nil
	}}
	current := New(Dependencies{
		SessionID: "session-1", Inbox: inputs, Restored: store.resume, Sessions: store,
		ContextBuilder: contextbuilder.NewBuilder(), LLM: adapter, Tools: registry,
		Operations: operation.NewLocalOperationManager(ctx),
	})
	if err := current.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if processGroupID <= 1 {
		t.Fatal("shell process never started")
	}
	if err := syscall.Kill(-processGroupID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group %d after stop: %v", processGroupID, err)
	}
	last := store.appendedStatuses[len(store.appendedStatuses)-1]
	if last.Operations[0].Status != operation.StatusCanceled {
		t.Fatalf("persisted operation status = %s", last.Operations[0].Status)
	}
	requests := adapter.requestSnapshot()
	if len(requests) != 0 {
		t.Fatal("hard stop requested a model response")
	}
}

func TestCoordinatorStopAfterToolCommitSettlesOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		store := persistTestRun(t, run)
		restoreTestRun(t, run, store)
		run.current.dependencies.Operations = operation.NewLocalOperationManager(t.Context())
		stopQueued := false
		observer := store.AddObserver(func(_ session.ID, item sessionstore.Item) {
			if item.Kind != sessionstore.ItemToolCallStatus || stopQueued {
				return
			}
			status := item.Data.(sessionstore.ToolCallStatus)
			if len(status.Operations) == 0 {
				return
			}
			stopQueued = true
			submitTestInput(t, run.inputs, stopInput(t, "stop", inbox.StopHard))
		})
		defer store.RemoveObserver(observer)
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run tool"))
		run.respond(t, 0, toolGraceResponse("A"))
		run.assertStopped(t)
		if !stopQueued || len(run.calls) != 1 {
			t.Fatal("stop was not queued at the commit boundary or started another turn")
		}
		page, err := store.Items(t.Context(), "session-1", sessionstore.BeforeFirst, 100)
		if err != nil {
			t.Fatal(err)
		}
		last := page.Items[len(page.Items)-1]
		if last.Kind != sessionstore.ItemToolCallStatus {
			t.Fatalf("last item = %s, want settled tool call", last.Kind)
		}
		status := last.Data.(sessionstore.ToolCallStatus)
		if status.CallID != "A" || len(status.Operations) != 1 || status.Operations[0].Status != operation.StatusCompleted {
			t.Fatalf("persisted tool result = %#v, want completed value operation", status)
		}
		restored, err := store.Resume(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(restored.Operations) != 0 {
			t.Fatal("stop left an operation unsettled in the session store")
		}
	})
}
