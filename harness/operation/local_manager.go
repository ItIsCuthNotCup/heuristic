package operation

import (
	"context"
	"errors"
	"fmt"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

type LocalOperationManager struct {
	ctx             context.Context
	remoteJobs      *remoteJobHandlers
	adds            chan localAddRequest
	cancellations   chan localCancelRequest
	primitiveEvents chan primitives.PrimitiveEvent
	updates         chan Operation
	pendingUpdates  []Operation
}

type localAddRequest struct {
	operation Operation
	result    chan error
}

type localCancelRequest struct {
	id     ID
	reason string
	result chan error
}

type localRunningOperation struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	operation             Operation
	remoteJobHandlerIndex int
	handle                func(*primitives.PrimitiveEvent) (Step, error)
}

func (current *localRunningOperation) initialize() error {
	switch current.operation.Type {
	case TypeShell:
		shell, err := NewShell(current.operation)
		if err != nil {
			return err
		}
		current.handle = shell.Handle
	case TypeViewImage:
		view, err := NewViewImage(current.operation)
		if err != nil {
			return err
		}
		current.handle = view.Handle
	default:
		current.handle = func(event *primitives.PrimitiveEvent) (Step, error) {
			return advanceLocalOperation(current.operation, event)
		}
	}
	return nil
}

var _ Manager = (*LocalOperationManager)(nil)

func NewLocalOperationManager(ctx context.Context, remoteJobHandlers ...RemoteJobHandler) *LocalOperationManager {
	manager := &LocalOperationManager{
		ctx:             ctx,
		remoteJobs:      newRemoteJobHandlers(ctx, remoteJobHandlers),
		adds:            make(chan localAddRequest),
		cancellations:   make(chan localCancelRequest),
		primitiveEvents: make(chan primitives.PrimitiveEvent),
		updates:         make(chan Operation),
	}
	go manager.run()
	return manager
}

func (manager *LocalOperationManager) Add(operation Operation) error {
	result := make(chan error, 1)
	request := localAddRequest{operation: operation, result: result}
	select {
	case manager.adds <- request:
	case <-manager.ctx.Done():
		return manager.ctx.Err()
	}
	select {
	case err := <-result:
		return err
	case <-manager.ctx.Done():
		return manager.ctx.Err()
	}
}

func (manager *LocalOperationManager) Cancel(id ID, reason string) error {
	result := make(chan error, 1)
	request := localCancelRequest{id: id, reason: reason, result: result}
	select {
	case manager.cancellations <- request:
	case <-manager.ctx.Done():
		return manager.ctx.Err()
	}
	select {
	case err := <-result:
		return err
	case <-manager.ctx.Done():
		return manager.ctx.Err()
	}
}

func (manager *LocalOperationManager) Updates() <-chan Operation {
	return manager.updates
}

func (manager *LocalOperationManager) run() {
	defer close(manager.updates)
	defer manager.remoteJobs.workers.Wait()
	operations := make(map[ID]*localRunningOperation)
	accepted := make(map[ID]struct{})
	activePrimitives := 0

	for {
		var updates chan Operation
		var update Operation
		if len(manager.pendingUpdates) != 0 {
			updates = manager.updates
			update = manager.pendingUpdates[0]
		}

		select {
		case updates <- update:
			manager.pendingUpdates[0] = Operation{}
			manager.pendingUpdates = manager.pendingUpdates[1:]

		case request := <-manager.adds:
			if _, exists := accepted[request.operation.ID]; exists {
				request.result <- nil
				continue
			}
			ctx, cancel := context.WithCancel(manager.ctx)
			current := &localRunningOperation{
				ctx:                   ctx,
				cancel:                cancel,
				operation:             request.operation,
				remoteJobHandlerIndex: -1,
			}
			if request.operation.Type == TypeRemoteJob {
				handlerIndex, err := manager.remoteJobs.add(request.operation)
				if err != nil {
					cancel()
					request.result <- err
					continue
				}
				current.remoteJobHandlerIndex = handlerIndex
				accepted[request.operation.ID] = struct{}{}
				operations[request.operation.ID] = current
				request.result <- nil
				continue
			}
			if err := current.initialize(); err != nil {
				cancel()
				request.result <- err
				continue
			}
			step, err := current.handle(nil)
			if err != nil {
				cancel()
				request.result <- err
				continue
			}
			accepted[request.operation.ID] = struct{}{}
			operations[request.operation.ID] = current
			activePrimitives += manager.acceptLocalStep(operations, current, step)
			request.result <- nil

		case request := <-manager.cancellations:
			current, exists := operations[request.id]
			if !exists {
				request.result <- nil
				continue
			}
			if current.remoteJobHandlerIndex >= 0 {
				request.result <- manager.remoteJobs.cancel(
					current.remoteJobHandlerIndex,
					request.id,
					request.reason,
				)
				continue
			}
			current.cancel()
			request.result <- nil

		case update := <-manager.remoteJobs.updates:
			if update.closed {
				manager.remoteJobs.markClosed(update.handlerIndex)
				for _, current := range operations {
					if current.remoteJobHandlerIndex == update.handlerIndex {
						manager.failLocalOperation(
							operations,
							current,
							fmt.Errorf("remote job handler %d stopped", update.handlerIndex),
						)
					}
				}
				continue
			}
			current, exists := operations[update.operation.ID]
			if !exists {
				continue
			}
			if current.remoteJobHandlerIndex != update.handlerIndex {
				manager.failRemoteJobOperation(
					operations,
					current,
					fmt.Errorf("remote job operation %q update came from the wrong handler", update.operation.ID),
				)
				continue
			}
			if err := validateRemoteJobUpdate(current.operation, update.operation); err != nil {
				manager.failRemoteJobOperation(operations, current, err)
				continue
			}
			current.operation = update.operation
			manager.appendLocalUpdate(update.operation)
			if localOperationFinished(update.operation.Status) {
				current.cancel()
				delete(operations, update.operation.ID)
			}

		case event := <-manager.primitiveEvents:
			completed := localPrimitiveCompleted(event.Type)
			if completed {
				activePrimitives--
			}
			current, exists := operations[ID(event.Source)]
			if !exists {
				continue
			}
			if completed && current.ctx.Err() != nil {
				event.Type = primitives.PrimitiveEventCanceled
				event.Result = nil
			}
			step, err := current.handle(&event)
			if err != nil {
				manager.failLocalOperation(operations, current, err)
				continue
			}
			activePrimitives += manager.acceptLocalStep(operations, current, step)

		case <-manager.ctx.Done():
			for _, current := range operations {
				current.cancel()
			}
			manager.drainLocalPrimitives(activePrimitives)
			return
		}
	}
}

func (manager *LocalOperationManager) acceptLocalStep(
	operations map[ID]*localRunningOperation,
	current *localRunningOperation,
	step Step,
) int {
	if step.Operation != nil {
		current.operation = *step.Operation
		manager.appendLocalUpdate(*step.Operation)
		if localOperationFinished(step.Operation.Status) {
			current.cancel()
			delete(operations, step.Operation.ID)
			return 0
		}
	}
	started := 0
	for _, dispatch := range step.Dispatches {
		if err := startLocalPrimitive(current.ctx, dispatch, manager.primitiveEvents); err != nil {
			failed, advanceErr := current.handle(&primitives.PrimitiveEvent{
				Type:   primitives.PrimitiveEventFailed,
				Source: primitives.SourceID(current.operation.ID),
				Result: primitives.PrimitiveFailureResult{Error: err.Error()},
			})
			if advanceErr != nil {
				manager.failLocalOperation(operations, current, advanceErr)
				return started
			}
			return started + manager.acceptLocalStep(operations, current, failed)
		}
		started++
	}
	return started
}

func (manager *LocalOperationManager) failLocalOperation(
	operations map[ID]*localRunningOperation,
	current *localRunningOperation,
	err error,
) {
	failed := failLocalOperation(current.operation, err)
	manager.appendLocalUpdate(failed)
	current.cancel()
	delete(operations, current.operation.ID)
}

func (manager *LocalOperationManager) failRemoteJobOperation(
	operations map[ID]*localRunningOperation,
	current *localRunningOperation,
	cause error,
) {
	if err := manager.remoteJobs.cancel(
		current.remoteJobHandlerIndex,
		current.operation.ID,
		cause.Error(),
	); err != nil {
		cause = errors.Join(cause, fmt.Errorf("cancel rejected remote job: %w", err))
	}
	manager.failLocalOperation(operations, current, cause)
}

func (manager *LocalOperationManager) drainLocalPrimitives(active int) {
	for active != 0 {
		event := <-manager.primitiveEvents
		if localPrimitiveCompleted(event.Type) {
			active--
		}
	}
}

func (manager *LocalOperationManager) appendLocalUpdate(operation Operation) {
	operation.State = operation.State.Clone()
	operation.Idempotency = operation.Idempotency.Clone()
	manager.pendingUpdates = append(manager.pendingUpdates, operation)
}

func advanceLocalOperation(current Operation, event *primitives.PrimitiveEvent) (Step, error) {
	switch current.Type {
	case TypeValue:
		return AdvanceValue(current, event)
	case TypeSkillUse:
		return AdvanceSkillUse(current, event)
	default:
		return Step{}, fmt.Errorf(
			"local operation manager does not support type %q: %w",
			current.Type,
			ErrUnsupported,
		)
	}
}

func failLocalOperation(current Operation, err error) Operation {
	switch current.Type {
	case TypeSkillUse:
		state, stateErr := skillUseOperationState(current)
		if stateErr == nil {
			step, stepErr := failSkillUse(current, state, err)
			if stepErr == nil {
				return *step.Operation
			}
		}
	case TypeShell:
		shell, stateErr := NewShell(current)
		if stateErr == nil {
			step, stepErr := shell.fail(err)
			if stepErr == nil {
				return *step.Operation
			}
		}
	case TypeViewImage:
		step, stateErr := failViewImage(current, err)
		if stateErr != nil {
			panic(fmt.Errorf("fail validated view-image operation %q: %w", current.ID, stateErr))
		}
		return *step.Operation
	case TypeRemoteJob:
		step, stateErr := FailRemoteJob(current, err)
		if stateErr != nil {
			panic(fmt.Errorf("fail validated remote job operation %q: %w", current.ID, stateErr))
		}
		return *step.Operation
	}
	current.Status = StatusFailed
	return current
}

func startLocalPrimitive(
	ctx context.Context,
	dispatch PrimitiveDispatch,
	events chan primitives.PrimitiveEvent,
) error {
	switch dispatch.Type {
	case primitives.PrimitiveDispatchIOCreate:
		request, ok := dispatch.Data.(primitives.IOCreateRequest)
		if !ok {
			return fmt.Errorf("io.create dispatch data is %T, want primitives.IOCreateRequest", dispatch.Data)
		}
		primitives.Create(ctx, request, events)

	case primitives.PrimitiveDispatchIORead:
		request, ok := dispatch.Data.(primitives.IOReadRequest)
		if !ok {
			return fmt.Errorf("io.read dispatch data is %T, want primitives.IOReadRequest", dispatch.Data)
		}
		primitives.ReadFile(ctx, request, events)

	case primitives.PrimitiveDispatchProcessStart:
		request, ok := dispatch.Data.(primitives.ProcessStartRequest)
		if !ok {
			return fmt.Errorf("process.start dispatch data is %T, want primitives.ProcessStartRequest", dispatch.Data)
		}
		primitives.StartProcess(ctx, request, events)

	case primitives.PrimitiveDispatchCompute:
		request, ok := dispatch.Data.(primitives.ComputeRequest)
		if !ok {
			return fmt.Errorf("compute dispatch data is %T, want primitives.ComputeRequest", dispatch.Data)
		}
		primitives.Compute(ctx, request, events)

	default:
		return fmt.Errorf("local operation manager does not support dispatch %q", dispatch.Type)
	}

	return nil
}

func localOperationFinished(status Status) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

func localPrimitiveCompleted(eventType primitives.PrimitiveEventType) bool {
	switch eventType {
	case primitives.PrimitiveEventIOCreateCompleted,
		primitives.PrimitiveEventIOReadCompleted,
		primitives.PrimitiveEventProcessExited,
		primitives.PrimitiveEventFailed,
		primitives.PrimitiveEventCanceled,
		primitives.PrimitiveEventComputeCompleted:
		return true
	default:
		return false
	}
}
