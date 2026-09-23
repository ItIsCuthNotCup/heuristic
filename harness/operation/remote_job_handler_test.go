package operation_test

import (
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
)

type testRemoteJobHandler struct {
	adds          chan operation.Operation
	cancellations chan testRemoteJobCancellation
	updates       chan operation.Operation
	planType      operation.RemoteJobPlanType
	planVersion   operation.RemoteJobPlanVersion
	addErr        error
	cancelErr     error
}

type testRemoteJobCancellation struct {
	id     operation.ID
	reason string
}

func (handler *testRemoteJobHandler) RemoteJobPlanType() operation.RemoteJobPlanType {
	return handler.planType
}

func (handler *testRemoteJobHandler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion {
	return handler.planVersion
}

func (handler *testRemoteJobHandler) AddRemoteJob(current operation.Operation) error {
	if handler.addErr != nil {
		return handler.addErr
	}
	handler.adds <- current
	return nil
}

func (handler *testRemoteJobHandler) CancelRemoteJob(id operation.ID, reason string) error {
	if handler.cancelErr != nil {
		return handler.cancelErr
	}
	handler.cancellations <- testRemoteJobCancellation{id: id, reason: reason}
	return nil
}

func (handler *testRemoteJobHandler) RemoteJobUpdates() <-chan operation.Operation {
	return handler.updates
}

func TestLocalOperationManagerRoutesRemoteJobs(t *testing.T) {
	handler := newTestRemoteJobHandler()
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	current := newTestRemoteJobOperation(t, "remote-job")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	if added := <-handler.adds; added.ID != current.ID {
		t.Fatalf("added operation ID = %q", added.ID)
	}

	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	state.TerminalResult = `{"content":[]}`
	completed, err := operation.UpdateRemoteJob(current, state, operation.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	handler.updates <- *completed.Operation
	if update := receiveRemoteJobUpdate(t, manager.Updates()); update.Status != operation.StatusCompleted {
		t.Fatalf("status = %q", update.Status)
	}

	second := newTestRemoteJobOperation(t, "remote-cancel")
	if err := manager.Add(second); err != nil {
		t.Fatal(err)
	}
	<-handler.adds
	if err := manager.Cancel(second.ID, "requested"); err != nil {
		t.Fatal(err)
	}
	if cancellation := <-handler.cancellations; cancellation.id != second.ID || cancellation.reason != "requested" {
		t.Fatalf("cancellation = %#v", cancellation)
	}
}

func TestLocalOperationManagerRejectsInvalidRemoteJobUpdates(t *testing.T) {
	handler := newTestRemoteJobHandler()
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	current := newTestRemoteJobOperation(t, "remote-invalid")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	<-handler.adds
	invalid := current
	invalid.Status = operation.StatusAwaiting
	invalid.State = jsontext.Value(`{`)
	handler.updates <- invalid

	failed := receiveRemoteJobUpdate(t, manager.Updates())
	state, err := operation.DecodeRemoteJobState(failed)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != operation.StatusFailed ||
		!strings.Contains(state.TerminalError, "validate remote job handler update") {
		t.Fatalf("operation = %#v, state = %#v", failed, state)
	}
	if cancellation := <-handler.cancellations; cancellation.id != current.ID {
		t.Fatalf("cancellation = %#v", cancellation)
	}
}

func TestLocalOperationManagerSelectsRemoteJobHandler(t *testing.T) {
	t.Run("unsupported plan", func(t *testing.T) {
		manager := operation.NewLocalOperationManager(t.Context())
		err := manager.Add(newTestRemoteJobOperation(t, "unsupported"))
		if !errors.Is(err, operation.ErrUnsupported) {
			t.Fatalf("Add() error = %v, want ErrUnsupported", err)
		}
	})

	t.Run("ambiguous plan", func(t *testing.T) {
		first := newTestRemoteJobHandler()
		second := newTestRemoteJobHandler()
		manager := operation.NewLocalOperationManager(t.Context(), first, second)
		err := manager.Add(newTestRemoteJobOperation(t, "ambiguous"))
		if err == nil || !strings.Contains(err.Error(), "multiple handlers") {
			t.Fatalf("Add() error = %v", err)
		}
	})

	t.Run("add error", func(t *testing.T) {
		handler := newTestRemoteJobHandler()
		handler.addErr = errors.New("cannot add")
		manager := operation.NewLocalOperationManager(t.Context(), handler)
		if err := manager.Add(newTestRemoteJobOperation(t, "add-error")); !errors.Is(err, handler.addErr) {
			t.Fatalf("Add() error = %v", err)
		}
	})
}

func TestLocalOperationManagerPropagatesRemoteCancellationError(t *testing.T) {
	handler := newTestRemoteJobHandler()
	handler.cancelErr = errors.New("cannot cancel")
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	current := newTestRemoteJobOperation(t, "cancel-error")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	<-handler.adds
	if err := manager.Cancel(current.ID, "requested"); !errors.Is(err, handler.cancelErr) {
		t.Fatalf("Cancel() error = %v", err)
	}
}

func TestLocalOperationManagerFailsJobsWhenRemoteHandlerStops(t *testing.T) {
	handler := newTestRemoteJobHandler()
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	current := newTestRemoteJobOperation(t, "handler-stopped")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	<-handler.adds
	close(handler.updates)

	failed := receiveRemoteJobUpdate(t, manager.Updates())
	state, err := operation.DecodeRemoteJobState(failed)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != operation.StatusFailed ||
		!strings.Contains(state.TerminalError, "remote job handler 0 stopped") {
		t.Fatalf("operation = %#v, state = %#v", failed, state)
	}
	later := newTestRemoteJobOperation(t, "handler-already-stopped")
	if err := manager.Add(later); err == nil || !strings.Contains(err.Error(), "remote job handler 0 stopped") {
		t.Fatalf("Add() error = %v", err)
	}
	select {
	case added := <-handler.adds:
		t.Fatalf("stopped handler accepted operation %#v", added)
	default:
	}
}

func TestLocalOperationManagerIsolatesStoppedRemoteHandler(t *testing.T) {
	first := newTestRemoteJobHandler()
	second := newTestRemoteJobHandler()
	second.planType = "other"
	manager := operation.NewLocalOperationManager(t.Context(), first, second)
	firstJob := newTestRemoteJobOperation(t, "first-handler")
	secondJob := newTestRemoteJobOperationForPlan(t, "second-handler", "other")
	if err := manager.Add(firstJob); err != nil {
		t.Fatal(err)
	}
	<-first.adds
	if err := manager.Add(secondJob); err != nil {
		t.Fatal(err)
	}
	<-second.adds
	close(first.updates)
	if failed := receiveRemoteJobUpdate(t, manager.Updates()); failed.ID != firstJob.ID || failed.Status != operation.StatusFailed {
		t.Fatalf("failed update = %#v", failed)
	}
	state, err := operation.DecodeRemoteJobState(secondJob)
	if err != nil {
		t.Fatal(err)
	}
	state.TerminalResult = `{}`
	completed, err := operation.UpdateRemoteJob(secondJob, state, operation.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	second.updates <- *completed.Operation
	if update := receiveRemoteJobUpdate(t, manager.Updates()); update.ID != secondJob.ID || update.Status != operation.StatusCompleted {
		t.Fatalf("completed update = %#v", update)
	}
}

func TestLocalOperationManagerRejectsUpdateFromWrongRemoteHandler(t *testing.T) {
	first := newTestRemoteJobHandler()
	second := newTestRemoteJobHandler()
	second.planType = "other"
	manager := operation.NewLocalOperationManager(t.Context(), first, second)
	current := newTestRemoteJobOperation(t, "wrong-handler")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	<-first.adds
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	awaiting, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
	if err != nil {
		t.Fatal(err)
	}
	second.updates <- *awaiting.Operation

	failed := receiveRemoteJobUpdate(t, manager.Updates())
	failedState, err := operation.DecodeRemoteJobState(failed)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != operation.StatusFailed ||
		!strings.Contains(failedState.TerminalError, "wrong handler") {
		t.Fatalf("operation = %#v, state = %#v", failed, failedState)
	}
	if cancellation := <-first.cancellations; cancellation.id != current.ID {
		t.Fatalf("cancellation = %#v", cancellation)
	}
}

func TestLocalOperationManagerValidatesRemoteJobUpdateIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*operation.Operation)
		want   string
	}{
		{
			name: "output limit", want: "changed output limit",
			mutate: func(update *operation.Operation) { update.MaxOutputLength++ },
		},
		{
			name: "type", want: "type or version",
			mutate: func(update *operation.Operation) { update.Type = operation.TypeShell },
		},
		{
			name: "version", want: "type or version",
			mutate: func(update *operation.Operation) { update.Version++ },
		},
		{
			name: "status", want: "invalid status",
			mutate: func(update *operation.Operation) { update.Status = "unknown" },
		},
		{
			name: "plan", want: "changed plan",
			mutate: func(update *operation.Operation) {
				state, err := operation.DecodeRemoteJobState(*update)
				if err != nil {
					t.Fatal(err)
				}
				state.Plan.Data = jsontext.Value(`{"changed":true}`)
				step, err := operation.UpdateRemoteJob(*update, state, operation.StatusAwaiting)
				if err != nil {
					t.Fatal(err)
				}
				*update = *step.Operation
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestRemoteJobHandler()
			manager := operation.NewLocalOperationManager(t.Context(), handler)
			current := newTestRemoteJobOperation(t, operation.ID("invalid-"+test.name))
			if err := manager.Add(current); err != nil {
				t.Fatal(err)
			}
			<-handler.adds
			update := current
			update.Status = operation.StatusAwaiting
			test.mutate(&update)
			handler.updates <- update

			failed := receiveRemoteJobUpdate(t, manager.Updates())
			state, err := operation.DecodeRemoteJobState(failed)
			if err != nil {
				t.Fatal(err)
			}
			if failed.Status != operation.StatusFailed || failed.MaxOutputLength != current.MaxOutputLength ||
				!strings.Contains(state.TerminalError, test.want) {
				t.Fatalf("operation = %#v, state = %#v", failed, state)
			}
			if cancellation := <-handler.cancellations; cancellation.id != current.ID {
				t.Fatalf("cancellation = %#v", cancellation)
			}
		})
	}
}

func TestLocalOperationManagerRejectsRemoteJobStatusRegression(t *testing.T) {
	handler := newTestRemoteJobHandler()
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	current := newTestRemoteJobOperation(t, "status-regression")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	<-handler.adds
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	awaiting, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
	if err != nil {
		t.Fatal(err)
	}
	handler.updates <- *awaiting.Operation
	if update := receiveRemoteJobUpdate(t, manager.Updates()); update.Status != operation.StatusAwaiting {
		t.Fatalf("status = %q", update.Status)
	}
	regressed, err := operation.UpdateRemoteJob(*awaiting.Operation, state, operation.StatusReady)
	if err != nil {
		t.Fatal(err)
	}
	handler.updates <- *regressed.Operation
	failed := receiveRemoteJobUpdate(t, manager.Updates())
	failedState, err := operation.DecodeRemoteJobState(failed)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != operation.StatusFailed ||
		!strings.Contains(failedState.TerminalError, "invalid status transition") {
		t.Fatalf("operation = %#v, state = %#v", failed, failedState)
	}
	if cancellation := <-handler.cancellations; cancellation.id != current.ID {
		t.Fatalf("cancellation = %#v", cancellation)
	}
}

func TestLocalOperationManagerReportsRemoteCancellationFailureAfterInvalidUpdate(t *testing.T) {
	handler := newTestRemoteJobHandler()
	handler.cancelErr = errors.New("cannot cancel")
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	current := newTestRemoteJobOperation(t, "invalid-cancel-error")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	<-handler.adds
	invalid := current
	invalid.Status = operation.StatusAwaiting
	invalid.State = jsontext.Value(`{`)
	handler.updates <- invalid

	failed := receiveRemoteJobUpdate(t, manager.Updates())
	state, err := operation.DecodeRemoteJobState(failed)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != operation.StatusFailed ||
		!strings.Contains(state.TerminalError, "validate remote job handler update") ||
		!strings.Contains(state.TerminalError, "cannot cancel") {
		t.Fatalf("operation = %#v, state = %#v", failed, state)
	}
}

func TestLocalOperationManagerAddsRemoteJobOnce(t *testing.T) {
	handler := newTestRemoteJobHandler()
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	current := newTestRemoteJobOperation(t, "idempotent")
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	<-handler.adds
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-handler.adds:
		t.Fatalf("duplicate add = %#v", duplicate)
	default:
	}
}

func newTestRemoteJobHandler() *testRemoteJobHandler {
	return &testRemoteJobHandler{
		adds:          make(chan operation.Operation, 1),
		cancellations: make(chan testRemoteJobCancellation, 1),
		updates:       make(chan operation.Operation, 1),
		planType:      "test",
		planVersion:   1,
	}
}

func newTestRemoteJobOperation(t *testing.T, id operation.ID) operation.Operation {
	t.Helper()
	return newTestRemoteJobOperationForPlan(t, id, "test")
}

func newTestRemoteJobOperationForPlan(
	t *testing.T,
	id operation.ID,
	planType operation.RemoteJobPlanType,
) operation.Operation {
	t.Helper()
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{
		Type: planType, Version: 1, Data: jsontext.Value(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{
		ID: id, Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, MaxOutputLength: spec.MaxOutputLength, State: spec.State,
	}
}

func receiveRemoteJobUpdate(t *testing.T, updates <-chan operation.Operation) operation.Operation {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case update := <-updates:
		return update
	case <-timer.C:
		t.Fatal("timed out waiting for remote job update")
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	return operation.Operation{}
}
