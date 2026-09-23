package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
)

var errCoordinatorFuzzFault = errors.New("injected coordinator dependency failure")

type coordinatorFaultEvent struct {
	kind  string
	value any
}

type coordinatorFaultTrace struct {
	mu        sync.Mutex
	events    []coordinatorFaultEvent
	site      string
	remaining int
	failed    bool
}

func (trace *coordinatorFaultTrace) record(kind string, value any) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.events = append(trace.events, coordinatorFaultEvent{kind: kind, value: value})
}

func (trace *coordinatorFaultTrace) fail(site string) error {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.failed || site != trace.site {
		return nil
	}
	if trace.remaining > 0 {
		trace.remaining--
		return nil
	}
	trace.failed = true
	trace.events = append(trace.events, coordinatorFaultEvent{kind: "failure", value: site})
	return errCoordinatorFuzzFault
}

// This single-session store commits whole values or returns an error without
// changing history. It has no coordinator lifecycle or scheduling logic.
type faultMemoryStore struct {
	trace      *coordinatorFaultTrace
	snapshot   sessionstore.Snapshot
	items      []sessionstore.Item
	operations map[operation.ID]operation.Operation
	observers  map[sessionstore.ObserverID]sessionstore.Observer
}

func (store *faultMemoryStore) AddObserver(observer sessionstore.Observer) sessionstore.ObserverID {
	id := uuid.New()
	if store.observers == nil {
		store.observers = make(map[sessionstore.ObserverID]sessionstore.Observer)
	}
	store.observers[id] = observer
	return id
}

func (store *faultMemoryStore) RemoveObserver(id sessionstore.ObserverID) {
	delete(store.observers, id)
}

func (store *faultMemoryStore) Create(ctx context.Context, id session.ID) (sessionstore.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return sessionstore.Snapshot{}, err
	}
	if id == "" || store.snapshot.Session.ID != "" {
		return sessionstore.Snapshot{}, errors.New("fuzz store needs one fresh session")
	}
	store.snapshot.Session = session.Session{ID: id, CreatedAt: time.Now()}
	store.operations = make(map[operation.ID]operation.Operation)
	return store.snapshot, nil
}

func (store *faultMemoryStore) Inspect(ctx context.Context, id session.ID) (sessionstore.Snapshot, error) {
	if err := store.check(ctx, id); err != nil {
		return sessionstore.Snapshot{}, err
	}
	return store.snapshot, nil
}

func (store *faultMemoryStore) ListSessions(ctx context.Context) ([]sessionstore.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store.snapshot.Session.ID == "" {
		return nil, nil
	}
	updated := store.snapshot.Session.CreatedAt
	if len(store.items) > 0 {
		updated = store.items[len(store.items)-1].RecordedAt
	}
	return []sessionstore.SessionInfo{{ID: store.snapshot.Session.ID, LastUpdatedAt: updated}}, nil
}

func (store *faultMemoryStore) Items(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	if err := store.check(ctx, id); err != nil {
		return sessionstore.Page{}, err
	}
	if err := store.trace.fail("history"); err != nil {
		return sessionstore.Page{}, err
	}
	if limit <= 0 {
		return sessionstore.Page{}, errors.New("invalid page size")
	}
	start := min(uint64(after), uint64(len(store.items)))
	end := min(start+uint64(limit), uint64(len(store.items)))
	page := sessionstore.Page{NextAfter: after, More: end < uint64(len(store.items))}
	for _, item := range store.items[start:end] {
		page.Items = append(page.Items, cloneFaultItem(item))
		page.NextAfter = item.Sequence
	}
	return page, nil
}

func (store *faultMemoryStore) AppendInput(ctx context.Context, id session.ID, value inbox.Input) error {
	return store.append(ctx, id, "input", sessionstore.ItemInput, value)
}

func (store *faultMemoryStore) AppendTurn(ctx context.Context, id session.ID, value session.Turn) error {
	return store.append(ctx, id, "turn", sessionstore.ItemTurn, value)
}

func (store *faultMemoryStore) AppendModelResponse(ctx context.Context, id session.ID, value sessionstore.ModelResponse) error {
	return store.append(ctx, id, "response", sessionstore.ItemModelResponse, value)
}

func (store *faultMemoryStore) AppendToolCallStatus(ctx context.Context, id session.ID, value sessionstore.ToolCallStatus) error {
	return store.append(ctx, id, "status", sessionstore.ItemToolCallStatus, value)
}

func (store *faultMemoryStore) append(ctx context.Context, id session.ID, site string, kind sessionstore.ItemKind, value any) error {
	if err := store.check(ctx, id); err != nil {
		return err
	}
	if err := store.trace.fail(site); err != nil {
		return err
	}
	item := cloneFaultItem(sessionstore.Item{Sequence: sessionstore.Sequence(len(store.items) + 1), RecordedAt: time.Now(), Kind: kind, Data: value})
	if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
		for _, value := range status.Operations {
			store.operations[value.ID] = cloneFaultOperation(value)
		}
	}
	store.items = append(store.items, item)
	store.trace.record("commit", cloneFaultItem(item))
	for _, observe := range store.observers {
		observe(id, cloneFaultItem(item))
	}
	return nil
}

func (store *faultMemoryStore) SaveOperation(ctx context.Context, id session.ID, value operation.Operation) error {
	if err := store.check(ctx, id); err != nil {
		return err
	}
	if _, exists := store.operations[value.ID]; !exists {
		return fmt.Errorf("operation %q was never committed", value.ID)
	}
	if err := store.trace.fail("save"); err != nil {
		return err
	}
	store.operations[value.ID] = cloneFaultOperation(value)
	store.trace.record("save", cloneFaultOperation(value))
	return nil
}

func (*faultMemoryStore) Resume(context.Context, session.ID) (sessionstore.ResumeState, error) {
	return sessionstore.ResumeState{}, errors.New("fuzz store does not support resume")
}

func (*faultMemoryStore) Fork(context.Context, session.ID, session.ID, session.TurnID) (sessionstore.Snapshot, error) {
	return sessionstore.Snapshot{}, errors.New("fuzz store does not support forks")
}

func (store *faultMemoryStore) check(ctx context.Context, id session.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" || id != store.snapshot.Session.ID {
		return errors.New("unknown fuzz session")
	}
	return nil
}

func cloneFaultOperation(value operation.Operation) operation.Operation {
	value.State = value.State.Clone()
	value.Idempotency = value.Idempotency.Clone()
	return value
}

func cloneFaultItem(item sessionstore.Item) sessionstore.Item {
	switch value := item.Data.(type) {
	case inbox.Input:
		value.Payload = value.Payload.Clone()
		item.Data = value
	case sessionstore.ModelResponse:
		value.Response.Usage.Raw = value.Response.Usage.Raw.Clone()
		value.Response.Output = slices.Clone(value.Response.Output)
		item.Data = value
	case sessionstore.ToolCallStatus:
		value.Status.WaitingFor = slices.Clone(value.Status.WaitingFor)
		value.Operations = slices.Clone(value.Operations)
		for index := range value.Operations {
			value.Operations[index] = cloneFaultOperation(value.Operations[index])
		}
		item.Data = value
	}
	return item
}

type faultOperationPlan struct {
	path   string
	result string
	delay  time.Duration
	fail   bool
	repeat bool
}

type controlledFaultOperations struct {
	ctx     context.Context
	trace   *coordinatorFaultTrace
	plans   map[string]faultOperationPlan
	updates chan operation.Operation
	started chan struct{}
	once    sync.Once

	mu      sync.Mutex
	cancels map[operation.ID]context.CancelFunc
}

func (manager *controlledFaultOperations) Add(value operation.Operation) error {
	manager.trace.record("add", cloneFaultOperation(value))
	if err := manager.trace.fail("add"); err != nil {
		return err
	}
	state, err := operation.DecodeSkillUse(value)
	if err != nil {
		return err
	}
	plan, exists := manager.plans[state.Path]
	if !exists {
		return errors.New("operation has no execution plan")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if _, exists := manager.cancels[value.ID]; exists {
		return nil
	}
	ctx, cancel := context.WithCancel(manager.ctx)
	manager.cancels[value.ID] = cancel
	manager.trace.record("start", cloneFaultOperation(value))
	manager.once.Do(func() { close(manager.started) })
	go func() {
		defer cancel()
		value.Status = operation.StatusAwaiting
		manager.emit(value)
		select {
		case <-ctx.Done():
			if manager.ctx.Err() != nil {
				return
			}
			value.Status = operation.StatusCanceled
			state.TerminalError = "operation canceled"
		case <-time.After(plan.delay):
			if plan.fail {
				value.Status = operation.StatusFailed
				state.TerminalError = plan.result
			} else {
				value.Status = operation.StatusCompleted
				state.Content = []byte(plan.result)
			}
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			panic(err)
		}
		value.State = encoded
		manager.emit(value)
		if plan.repeat {
			manager.emit(value)
		}
	}()
	return nil
}

func (manager *controlledFaultOperations) Cancel(id operation.ID, _ string) error {
	manager.trace.record("cancel", id)
	if err := manager.trace.fail("cancel"); err != nil {
		return err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	cancel, exists := manager.cancels[id]
	if !exists {
		return errors.New("operation canceled before dispatch")
	}
	cancel()
	return nil
}

func (manager *controlledFaultOperations) emit(value operation.Operation) {
	manager.trace.record("update", cloneFaultOperation(value))
	select {
	case manager.updates <- cloneFaultOperation(value):
	case <-manager.ctx.Done():
	}
}

func (manager *controlledFaultOperations) Updates() <-chan operation.Operation {
	return manager.updates
}
