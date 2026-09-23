package primitives_test

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestComputeReturnsValueOrFailure(t *testing.T) {
	failure := errors.New("encoding failed")
	for _, test := range []struct {
		name string
		err  error
		kind primitives.PrimitiveEventType
	}{
		{name: "success", kind: primitives.PrimitiveEventComputeCompleted},
		{name: "partial failure", err: failure, kind: primitives.PrimitiveEventFailed},
		{name: "callback cancellation error", err: context.Canceled, kind: primitives.PrimitiveEventFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				value := []int{640, 480}
				request := primitives.ComputeRequest{
					Source: "image", CorrelationID: "resize",
					Run: func(ctx context.Context) (any, error) {
						if ctx != t.Context() {
							return nil, errors.New("callback did not receive the caller's context")
						}
						return value, test.err
					},
				}
				events := make(chan primitives.PrimitiveEvent, 2)
				primitives.Compute(t.Context(), request, events)
				event := <-events
				if event.Type != test.kind || event.Source != request.Source || event.CorrelationID != request.CorrelationID {
					t.Fatalf("event = %#v", event)
				}
				if test.err != nil {
					if result := eventResult[primitives.PrimitiveFailureResult](t, event); result.Error != test.err.Error() {
						t.Fatalf("failure = %#v", result)
					}
				} else if result := eventResult[primitives.ComputeResult](t, event); !reflect.DeepEqual(result.Value, value) {
					t.Fatalf("result = %#v", result)
				}
				synctest.Wait()
				assertNoComputeEvent(t, events)
			})
		})
	}
}

func TestComputeCanceledBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ran := false
		events := make(chan primitives.PrimitiveEvent, 2)
		request := primitives.ComputeRequest{
			Source: "image", CorrelationID: "resize",
			Run: func(context.Context) (any, error) { ran = true; return nil, nil },
		}
		primitives.Compute(ctx, request, events)
		event := <-events
		if event.Type != primitives.PrimitiveEventCanceled || event.Source != request.Source ||
			event.CorrelationID != request.CorrelationID || event.Result != nil {
			t.Fatalf("event = %#v", event)
		}
		synctest.Wait()
		if ran {
			t.Fatal("canceled callback ran")
		}
		assertNoComputeEvent(t, events)
	})
}

func TestComputeCancellationWaitsForCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		failure := errors.New("incomplete image")
		events := make(chan primitives.PrimitiveEvent, 2)
		request := primitives.ComputeRequest{
			Source: "image", CorrelationID: "resize",
			Run: func(context.Context) (any, error) {
				close(started)
				<-release
				return "dimensions", failure
			},
		}
		primitives.Compute(ctx, request, events)
		<-started
		cancel()
		synctest.Wait()
		assertNoComputeEvent(t, events)
		unblock()
		event := <-events
		if event.Type != primitives.PrimitiveEventCanceled || event.Source != request.Source ||
			event.CorrelationID != request.CorrelationID || event.Result != nil {
			t.Fatalf("event = %#v", event)
		}
		synctest.Wait()
		assertNoComputeEvent(t, events)
	})
}

func TestComputeCallbackObservesCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		events := make(chan primitives.PrimitiveEvent, 1)
		primitives.Compute(ctx, primitives.ComputeRequest{
			Run: func(ctx context.Context) (any, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}, events)
		<-started
		cancel()
		event := <-events
		if event.Type != primitives.PrimitiveEventCanceled || event.Result != nil {
			t.Fatalf("event = %#v", event)
		}
	})
}

func TestComputeReportsInvalidRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := make(chan primitives.PrimitiveEvent)
		request := primitives.ComputeRequest{Source: "image", CorrelationID: "resize"}
		primitives.Compute(t.Context(), request, events)
		event := <-events
		if event.Type != primitives.PrimitiveEventFailed || event.Source != request.Source || event.CorrelationID != request.CorrelationID {
			t.Fatalf("event = %#v", event)
		}
		if result := eventResult[primitives.PrimitiveFailureResult](t, event); result.Error != "compute: Run must be set" {
			t.Fatalf("failure = %#v", result)
		}
		synctest.Wait()
		assertNoComputeEvent(t, events)
	})
}

func TestComputeInvocationsRunIndependently(t *testing.T) {
	for _, blocked := range []string{"callback", "event delivery"} {
		t.Run(blocked, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				first := make(chan primitives.PrimitiveEvent)
				primitives.Compute(t.Context(), primitives.ComputeRequest{
					Run: func(context.Context) (any, error) {
						if blocked == "callback" {
							<-release
						}
						return "first", nil
					},
				}, first)
				synctest.Wait()
				second := make(chan primitives.PrimitiveEvent, 1)
				primitives.Compute(t.Context(), primitives.ComputeRequest{
					Run: func(context.Context) (any, error) { return "second", nil },
				}, second)
				if result := eventResult[primitives.ComputeResult](t, <-second); result.Value != "second" {
					t.Fatalf("independent invocation = %#v", result)
				}
				unblock()
				if result := eventResult[primitives.ComputeResult](t, <-first); result.Value != "first" {
					t.Fatalf("blocked invocation = %#v", result)
				}
			})
		})
	}
}

func TestComputeRecoversCallbackPanics(t *testing.T) {
	panicError := errors.New("broken codec")
	for _, test := range []struct {
		name    string
		value   any
		message string
		cancel  bool
	}{
		{name: "string", value: "broken codec", message: "broken codec"},
		{name: "error", value: panicError, message: panicError.Error()},
		{name: "nil", message: "runtime error: panic called with nil argument"},
		{name: "canceled", value: panicError, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				events := make(chan primitives.PrimitiveEvent, 2)
				primitives.Compute(ctx, primitives.ComputeRequest{
					Source: "image", CorrelationID: "decode",
					Run: func(context.Context) (any, error) {
						if test.cancel {
							cancel()
						}
						panic(test.value)
					},
				}, events)
				event := <-events
				wantType := primitives.PrimitiveEventFailed
				if test.cancel {
					wantType = primitives.PrimitiveEventCanceled
				}
				if event.Type != wantType || event.Source != "image" || event.CorrelationID != "decode" {
					t.Fatalf("panic event = %#v", event)
				}
				if test.cancel {
					if event.Result != nil {
						t.Fatalf("canceled event result = %#v", event.Result)
					}
				} else {
					result := eventResult[primitives.PrimitiveFailureResult](t, event)
					if result.Error != "compute callback panicked: "+test.message {
						t.Fatalf("panic diagnostics = %q", result.Error)
					}
				}
				synctest.Wait()
				assertNoComputeEvent(t, events)
			})
		})
	}
}

func TestComputeReportsGoexit(t *testing.T) {
	for _, test := range []struct {
		name     string
		canceled bool
	}{
		{name: "failed"},
		{name: "canceled", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cleanedUp := false
				events := make(chan primitives.PrimitiveEvent, 2)
				primitives.Compute(ctx, primitives.ComputeRequest{
					Source: "image", CorrelationID: "decode",
					Run: func(context.Context) (any, error) {
						defer func() { cleanedUp = true }()
						if test.canceled {
							cancel()
						}
						runtime.Goexit()
						return nil, nil
					},
				}, events)
				event := <-events
				wantType := primitives.PrimitiveEventFailed
				if test.canceled {
					wantType = primitives.PrimitiveEventCanceled
				}
				if event.Type != wantType || event.Source != "image" || event.CorrelationID != "decode" || !cleanedUp {
					t.Fatalf("Goexit event = %#v, cleaned up = %v", event, cleanedUp)
				}
				if test.canceled {
					if event.Result != nil {
						t.Fatalf("canceled event result = %#v", event.Result)
					}
				} else if result := eventResult[primitives.PrimitiveFailureResult](t, event); result.Error != "compute callback exited without returning" {
					t.Fatalf("Goexit failure = %#v", result)
				}
				synctest.Wait()
				assertNoComputeEvent(t, events)
			})
		})
	}
}

func assertNoComputeEvent(t *testing.T, events <-chan primitives.PrimitiveEvent) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected compute event = %#v", event)
	default:
	}
}
