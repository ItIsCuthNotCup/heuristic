package primitives_test

import (
	"context"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func collectEvents(events <-chan primitives.PrimitiveEvent) []primitives.PrimitiveEvent {
	var collected []primitives.PrimitiveEvent
	for {
		event := <-events
		collected = append(collected, event)
		if primitiveEventIsTerminal(event.Type) {
			return collected
		}
	}
}

func primitiveEventIsTerminal(eventType primitives.PrimitiveEventType) bool {
	switch eventType {
	case primitives.PrimitiveEventFailed,
		primitives.PrimitiveEventCanceled,
		primitives.PrimitiveEventIOCreateCompleted,
		primitives.PrimitiveEventIOReadCompleted,
		primitives.PrimitiveEventProcessExited,
		primitives.PrimitiveEventProcessInputWritten,
		primitives.PrimitiveEventProcessInputWriteFailed,
		primitives.PrimitiveEventProcessInputClosed,
		primitives.PrimitiveEventProcessSignaled,
		primitives.PrimitiveEventRemoteCompleted,
		primitives.PrimitiveEventTimerFired,
		primitives.PrimitiveEventComputeCompleted:
		return true
	default:
		return false
	}
}

func readFile(ctx context.Context, request primitives.IOReadRequest) <-chan primitives.PrimitiveEvent {
	events := make(chan primitives.PrimitiveEvent)
	primitives.ReadFile(ctx, request, events)
	return events
}

func create(ctx context.Context, request primitives.IOCreateRequest) <-chan primitives.PrimitiveEvent {
	events := make(chan primitives.PrimitiveEvent)
	primitives.Create(ctx, request, events)
	return events
}

func scheduleTimer(ctx context.Context, request primitives.TimerRequest) <-chan primitives.PrimitiveEvent {
	events := make(chan primitives.PrimitiveEvent)
	primitives.ScheduleTimer(ctx, request, events)
	return events
}

type processInvocation struct {
	*primitives.ProcessInvocation
	events <-chan primitives.PrimitiveEvent
}

func startProcess(ctx context.Context, request primitives.ProcessStartRequest) *processInvocation {
	events := make(chan primitives.PrimitiveEvent, 1)
	process := primitives.StartProcess(ctx, request, events)
	return &processInvocation{ProcessInvocation: process, events: events}
}

func (process *processInvocation) Events() <-chan primitives.PrimitiveEvent {
	return process.events
}

func (process *processInvocation) WriteInput(
	ctx context.Context,
	request primitives.ProcessWriteRequest,
) <-chan primitives.PrimitiveEvent {
	events := make(chan primitives.PrimitiveEvent, 1)
	process.ProcessInvocation.WriteInput(ctx, request, events)
	return events
}

func (process *processInvocation) CloseInput(
	ctx context.Context,
	request primitives.ProcessCloseInputRequest,
) <-chan primitives.PrimitiveEvent {
	events := make(chan primitives.PrimitiveEvent, 1)
	process.ProcessInvocation.CloseInput(ctx, request, events)
	return events
}

func (process *processInvocation) Signal(
	ctx context.Context,
	request primitives.ProcessSignalRequest,
) <-chan primitives.PrimitiveEvent {
	events := make(chan primitives.PrimitiveEvent, 1)
	process.ProcessInvocation.Signal(ctx, request, events)
	return events
}

func singleEvent(
	t *testing.T,
	events []primitives.PrimitiveEvent,
	eventType primitives.PrimitiveEventType,
) primitives.PrimitiveEvent {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Type != eventType {
		t.Fatalf("event type = %q, want %q", events[0].Type, eventType)
	}
	return events[0]
}

func eventResult[T any](t *testing.T, event primitives.PrimitiveEvent) T {
	t.Helper()
	result, ok := event.Result.(T)
	if !ok {
		t.Fatalf("result type = %T", event.Result)
	}
	return result
}
