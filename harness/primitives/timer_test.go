package primitives_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

func TestScheduleTimerFiresAtAbsoluteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startedAt := time.Now()
		request := primitives.TimerRequest{
			Source:        "operation-1",
			CorrelationID: "timer-1",
			Deadline:      startedAt.Add(15 * time.Minute),
		}
		events := scheduleTimer(t.Context(), request)

		synctest.Wait()
		assertNoTimerEvent(t, events)
		time.Sleep(15*time.Minute - time.Nanosecond)
		synctest.Wait()
		assertNoTimerEvent(t, events)

		time.Sleep(time.Nanosecond)
		event := singleEvent(t, collectEvents(events), primitives.PrimitiveEventTimerFired)
		if event.Source != request.Source || event.CorrelationID != request.CorrelationID {
			t.Fatalf("event identity = (%q, %q)", event.Source, event.CorrelationID)
		}
		result := eventResult[primitives.TimerResult](t, event)
		if !result.Deadline.Equal(request.Deadline) {
			t.Fatalf("deadline = %s, want %s", result.Deadline, request.Deadline)
		}
		if !result.FiredAt.Equal(request.Deadline) {
			t.Fatalf("fired at = %s, want %s", result.FiredAt, request.Deadline)
		}
	})
}

func TestScheduleTimerFiresImmediatelyAtExpiredDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startedAt := time.Now()
		request := primitives.TimerRequest{
			Source:        "operation-1",
			CorrelationID: "timer-1",
			Deadline:      startedAt.Add(-time.Hour),
		}

		event := singleEvent(
			t,
			collectEvents(scheduleTimer(t.Context(), request)),
			primitives.PrimitiveEventTimerFired,
		)
		result := eventResult[primitives.TimerResult](t, event)
		if !result.Deadline.Equal(request.Deadline) {
			t.Fatalf("deadline = %s, want %s", result.Deadline, request.Deadline)
		}
		if !result.FiredAt.Equal(startedAt) {
			t.Fatalf("fired at = %s, want %s", result.FiredAt, startedAt)
		}
	})
}

func TestScheduleTimerCanceledBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		request := primitives.TimerRequest{
			Source:        "operation-1",
			CorrelationID: "timer-1",
			Deadline:      time.Now().Add(time.Hour),
		}

		event := singleEvent(
			t,
			collectEvents(scheduleTimer(ctx, request)),
			primitives.PrimitiveEventCanceled,
		)
		assertCanceledTimerIdentity(t, event, request)
	})
}

func TestScheduleTimerCancellationInterruptsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startedAt := time.Now()
		ctx, cancel := context.WithCancel(t.Context())
		request := primitives.TimerRequest{
			Source:        "operation-1",
			CorrelationID: "timer-1",
			Deadline:      startedAt.Add(time.Hour),
		}
		events := scheduleTimer(ctx, request)
		synctest.Wait()
		assertNoTimerEvent(t, events)

		cancel()
		event := singleEvent(t, collectEvents(events), primitives.PrimitiveEventCanceled)
		assertCanceledTimerIdentity(t, event, request)
		if !time.Now().Equal(startedAt) {
			t.Fatalf("cancellation advanced time to %s, want %s", time.Now(), startedAt)
		}
	})
}

func TestScheduleTimerRejectsUnsetDeadline(t *testing.T) {
	request := primitives.TimerRequest{
		Source:        "operation-1",
		CorrelationID: "timer-1",
	}

	event := singleEvent(
		t,
		collectEvents(scheduleTimer(t.Context(), request)),
		primitives.PrimitiveEventFailed,
	)
	if event.Source != request.Source || event.CorrelationID != request.CorrelationID {
		t.Fatalf("event identity = (%q, %q)", event.Source, event.CorrelationID)
	}
	result := eventResult[primitives.PrimitiveFailureResult](t, event)
	if !strings.Contains(result.Error, "deadline must be set") {
		t.Fatalf("error = %q", result.Error)
	}
}

func assertNoTimerEvent(t *testing.T, events <-chan primitives.PrimitiveEvent) {
	t.Helper()
	select {
	case event, open := <-events:
		t.Fatalf("timer event before deadline = (%#v, open %t)", event, open)
	default:
	}
}

func assertCanceledTimerIdentity(
	t *testing.T,
	event primitives.PrimitiveEvent,
	request primitives.TimerRequest,
) {
	t.Helper()
	if event.Source != request.Source || event.CorrelationID != request.CorrelationID {
		t.Fatalf("event identity = (%q, %q)", event.Source, event.CorrelationID)
	}
	if event.Result != nil {
		t.Fatalf("result = %#v, want nil", event.Result)
	}
}
