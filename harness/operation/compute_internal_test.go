package operation

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestLocalOperationManagerDispatchesComputeAndDrainsAfterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		manager := &LocalOperationManager{
			primitiveEvents: make(chan primitives.PrimitiveEvent),
		}
		current := &localRunningOperation{
			ctx: ctx, cancel: cancel,
			operation: Operation{ID: "compute", Status: StatusAwaiting},
		}
		operations := map[ID]*localRunningOperation{current.operation.ID: current}
		started := make(chan struct{})
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		step := Step{Dispatches: []PrimitiveDispatch{{
			Type: primitives.PrimitiveDispatchCompute,
			Data: primitives.ComputeRequest{
				Source: primitives.SourceID(current.operation.ID), CorrelationID: "prepare",
				Run: func(context.Context) (any, error) {
					close(started)
					<-release
					return "finished", nil
				},
			},
		}}}
		active := manager.acceptLocalStep(operations, current, step)
		if active != 1 {
			t.Fatalf("active primitives = %d, want 1", active)
		}
		<-started
		cancel()
		drained := make(chan struct{})
		go func() {
			manager.drainLocalPrimitives(active)
			close(drained)
		}()
		synctest.Wait()
		select {
		case <-drained:
			t.Fatal("shutdown finished before canceled callback returned")
		default:
		}
		if len(manager.pendingUpdates) != 0 || current.operation.Status != StatusAwaiting {
			t.Fatal("dispatch advanced the operation before completion")
		}
		unblock()
		<-drained
	})
}

func TestLocalOperationManagerDrainsEveryComputeOutcome(t *testing.T) {
	for _, outcome := range []string{"completed", "failed", "canceled", "panicked", "Goexit", "missing callback"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if outcome == "canceled" {
					cancel()
				}
				manager := &LocalOperationManager{
					primitiveEvents: make(chan primitives.PrimitiveEvent),
				}
				request := primitives.ComputeRequest{Run: func(context.Context) (any, error) {
					switch outcome {
					case "failed":
						return nil, errors.New("failed work")
					case "panicked":
						panic("failed callback")
					case "Goexit":
						runtime.Goexit()
					}
					return 42, nil
				}}
				if outcome == "missing callback" {
					request.Run = nil
				}
				if err := startLocalPrimitive(ctx, PrimitiveDispatch{
					Type: primitives.PrimitiveDispatchCompute,
					Data: request,
				}, manager.primitiveEvents); err != nil {
					t.Fatal(err)
				}
				manager.drainLocalPrimitives(1)
			})
		})
	}
}

func TestLocalOperationManagerRejectsInvalidComputePayload(t *testing.T) {
	manager := &LocalOperationManager{primitiveEvents: make(chan primitives.PrimitiveEvent)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	current := &localRunningOperation{
		ctx: ctx, cancel: cancel,
		operation: Operation{ID: "rejected", Status: StatusAwaiting},
	}
	var failure string
	current.handle = func(event *primitives.PrimitiveEvent) (Step, error) {
		if event == nil || event.Type != primitives.PrimitiveEventFailed || event.Source != primitives.SourceID(current.operation.ID) {
			return Step{}, errors.New("unexpected rejection event")
		}
		result, ok := event.Result.(primitives.PrimitiveFailureResult)
		if !ok {
			return Step{}, errors.New("invalid rejection result")
		}
		failure = result.Error
		failed := current.operation
		failed.Status = StatusFailed
		return Step{Operation: &failed}, nil
	}
	operations := map[ID]*localRunningOperation{current.operation.ID: current}
	dispatch := PrimitiveDispatch{Type: primitives.PrimitiveDispatchCompute}
	if active := manager.acceptLocalStep(operations, current, Step{Dispatches: []PrimitiveDispatch{dispatch}}); active != 0 {
		t.Fatalf("rejected dispatch counted %d active primitives", active)
	}
	if !strings.Contains(failure, "compute dispatch data") || len(operations) != 0 || len(manager.pendingUpdates) != 1 ||
		manager.pendingUpdates[0].Status != StatusFailed || ctx.Err() == nil {
		t.Fatalf("rejection: error %q, operations %#v, updates %#v", failure, operations, manager.pendingUpdates)
	}
}
