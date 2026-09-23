package localfile

import (
	"fmt"
	"io/fs"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

type toolCallStatusKey struct {
	turnID session.TurnID
	callID string
}

type sessionHead struct {
	Snapshot           sessionstore.Snapshot
	Operations         []operation.Operation
	itemSequence       sessionstore.Sequence
	latestTurnID       session.TurnID
	turns              map[session.TurnID]struct{}
	ownedTurns         map[session.TurnID]struct{}
	respondedTurns     map[session.TurnID]struct{}
	toolCallStatuses   map[toolCallStatusKey]struct{}
	operationPositions map[operation.ID]int
}

type storedState struct {
	sessionHead
	Items []sessionstore.Item
}

func newStoredState(id session.ID, createdAt time.Time) storedState {
	return storedState{
		sessionHead: newSessionHead(id, createdAt),
	}
}

func newSessionHead(id session.ID, createdAt time.Time) sessionHead {
	return sessionHead{
		Snapshot: sessionstore.Snapshot{
			Session: session.Session{ID: id, CreatedAt: createdAt},
		},
		turns:              make(map[session.TurnID]struct{}),
		ownedTurns:         make(map[session.TurnID]struct{}),
		respondedTurns:     make(map[session.TurnID]struct{}),
		toolCallStatuses:   make(map[toolCallStatusKey]struct{}),
		operationPositions: make(map[operation.ID]int),
	}
}

func (head *sessionHead) appendInput(
	input inbox.Input,
	recordedAt time.Time,
) (sessionstore.Item, error) {
	id := head.Snapshot.Session.ID
	if err := input.Validate(); err != nil {
		return sessionstore.Item{}, fmt.Errorf("append input to session %q: %w", id, err)
	}

	return head.appendItem(sessionstore.ItemInput, input, recordedAt), nil
}

func (head *sessionHead) appendTurn(
	turn session.Turn,
	recordedAt time.Time,
) (sessionstore.Item, error) {
	id := head.Snapshot.Session.ID
	if turn.ID == "" {
		return sessionstore.Item{}, fmt.Errorf("append turn to session %q: turn ID is empty", id)
	}
	if turn.PreviousTurnID != head.latestTurnID {
		return sessionstore.Item{}, fmt.Errorf(
			"append turn %q to session %q: previous turn is %q, want %q",
			turn.ID,
			id,
			turn.PreviousTurnID,
			head.latestTurnID,
		)
	}
	if _, exists := head.turns[turn.ID]; exists {
		return sessionstore.Item{}, fmt.Errorf("append turn %q: %w", turn.ID, fs.ErrExist)
	}

	head.turns[turn.ID] = struct{}{}
	head.ownedTurns[turn.ID] = struct{}{}
	head.latestTurnID = turn.ID
	return head.appendItem(sessionstore.ItemTurn, turn, recordedAt), nil
}

func (head *sessionHead) appendModelResponse(
	response sessionstore.ModelResponse,
	recordedAt time.Time,
) (sessionstore.Item, error) {
	if _, exists := head.ownedTurns[response.TurnID]; !exists {
		return sessionstore.Item{}, fmt.Errorf(
			"append model response to session %q for turn %q: %w",
			head.Snapshot.Session.ID,
			response.TurnID,
			fs.ErrNotExist,
		)
	}
	if _, exists := head.respondedTurns[response.TurnID]; exists {
		return sessionstore.Item{}, fmt.Errorf(
			"append model response for turn %q: %w",
			response.TurnID,
			fs.ErrExist,
		)
	}
	head.respondedTurns[response.TurnID] = struct{}{}
	return head.appendItem(sessionstore.ItemModelResponse, response, recordedAt), nil
}

func (head *sessionHead) appendToolCallStatus(
	status sessionstore.ToolCallStatus,
	operations []operation.Operation,
	recordedAt time.Time,
) (sessionstore.Item, error) {
	if _, exists := head.ownedTurns[status.TurnID]; !exists {
		return sessionstore.Item{}, fmt.Errorf(
			"append tool-call status to session %q for turn %q: %w",
			head.Snapshot.Session.ID,
			status.TurnID,
			fs.ErrNotExist,
		)
	}
	if status.CallID == "" {
		return sessionstore.Item{}, fmt.Errorf(
			"append tool-call status for turn %q: call ID is empty",
			status.TurnID,
		)
	}
	key := toolCallStatusKey{turnID: status.TurnID, callID: status.CallID}
	if _, exists := head.toolCallStatuses[key]; exists {
		return head.appendItem(sessionstore.ItemToolCallStatus, status, recordedAt), nil
	}
	seen := make(map[operation.ID]struct{}, len(operations))
	for index, value := range operations {
		if err := validateOperation(value); err != nil {
			return sessionstore.Item{}, fmt.Errorf("initialize operation %d: %w", index, err)
		}
		if _, exists := seen[value.ID]; exists {
			return sessionstore.Item{}, fmt.Errorf(
				"initialize operation %q: %w",
				value.ID,
				fs.ErrExist,
			)
		}
		seen[value.ID] = struct{}{}
		if _, exists := head.operationPositions[value.ID]; exists {
			return sessionstore.Item{}, fmt.Errorf(
				"initialize operation %q: %w",
				value.ID,
				fs.ErrExist,
			)
		}
	}
	if err := validateStatusOperationReferences(status.Status, operations); err != nil {
		return sessionstore.Item{}, fmt.Errorf(
			"append tool-call status for turn %q and call %q: %w",
			status.TurnID,
			status.CallID,
			err,
		)
	}

	head.toolCallStatuses[key] = struct{}{}
	for _, value := range operations {
		head.operationPositions[value.ID] = len(head.Operations)
		head.Operations = append(head.Operations, value)
	}
	return head.appendItem(sessionstore.ItemToolCallStatus, status, recordedAt), nil
}

func (state *storedState) appendInput(input inbox.Input, recordedAt time.Time) error {
	item, err := state.sessionHead.appendInput(input, recordedAt)
	if err != nil {
		return err
	}
	state.Items = append(state.Items, item)
	return nil
}

func (state *storedState) appendTurn(turn session.Turn, recordedAt time.Time) error {
	item, err := state.sessionHead.appendTurn(turn, recordedAt)
	if err != nil {
		return err
	}
	state.Items = append(state.Items, item)
	return nil
}

func (state *storedState) appendModelResponse(
	response sessionstore.ModelResponse,
	recordedAt time.Time,
) error {
	item, err := state.sessionHead.appendModelResponse(response, recordedAt)
	if err != nil {
		return err
	}
	state.Items = append(state.Items, item)
	return nil
}

func (state *storedState) appendToolCallStatus(
	status sessionstore.ToolCallStatus,
	operations []operation.Operation,
	recordedAt time.Time,
) error {
	item, err := state.sessionHead.appendToolCallStatus(status, operations, recordedAt)
	if err != nil {
		return err
	}
	status.Operations = operations
	item.Data = status
	state.Items = append(state.Items, item)
	return nil
}

func validateStatusOperationReferences(
	status tool.CallStatus,
	operations []operation.Operation,
) error {
	if status.Error != "" {
		if len(status.WaitingFor) != 0 {
			return fmt.Errorf("error status cannot wait for operations")
		}
		if len(operations) != 0 {
			return fmt.Errorf("error status cannot initialize operations")
		}
		return nil
	}
	if len(operations) == 0 {
		return fmt.Errorf("successful status must initialize at least one operation")
	}
	if len(status.WaitingFor) != len(operations) {
		return fmt.Errorf(
			"status waits for %d operations, initialized %d",
			len(status.WaitingFor),
			len(operations),
		)
	}

	initialized := make(map[operation.ID]struct{}, len(operations))
	for _, value := range operations {
		initialized[value.ID] = struct{}{}
	}
	waiting := make(map[operation.ID]struct{}, len(status.WaitingFor))
	for _, id := range status.WaitingFor {
		if _, exists := waiting[id]; exists {
			return fmt.Errorf("status repeats operation %q", id)
		}
		waiting[id] = struct{}{}
		if _, exists := initialized[id]; !exists {
			return fmt.Errorf("status waits for operation %q that was not initialized", id)
		}
	}
	return nil
}

func (head *sessionHead) saveOperation(value operation.Operation) error {
	if err := validateOperation(value); err != nil {
		return err
	}
	index, exists := head.operationPositions[value.ID]
	if !exists {
		return fmt.Errorf(
			"save operation %q in session %q: %w",
			value.ID,
			head.Snapshot.Session.ID,
			fs.ErrNotExist,
		)
	}
	existing := head.Operations[index]
	if existing.Type != value.Type || existing.Version != value.Version {
		return fmt.Errorf("save operation %q: type and version cannot change", value.ID)
	}
	head.Operations[index] = value
	return nil
}

func (state storedState) resume() sessionstore.ResumeState {
	externalInputIDs := make([]inbox.ID, 0)
	pending := make(map[operation.ID]struct{})
	for _, item := range state.Items {
		switch item.Kind {
		case sessionstore.ItemInput:
			input := item.Data.(inbox.Input)
			if input.Kind == inbox.InputExternal {
				externalInputIDs = append(externalInputIDs, input.ID)
			}
		case sessionstore.ItemToolCallStatus:
			for _, value := range item.Data.(sessionstore.ToolCallStatus).Operations {
				if terminalOperationStatus(value.Status) {
					delete(pending, value.ID)
				} else {
					pending[value.ID] = struct{}{}
				}
			}
		}
	}
	var operations []operation.Operation
	for _, value := range state.Operations {
		_, unrecorded := pending[value.ID]
		if !terminalOperationStatus(value.Status) || unrecorded {
			operations = append(operations, value)
		}
	}
	return sessionstore.ResumeState{
		Snapshot:         state.Snapshot,
		Operations:       operations,
		ExternalInputIDs: externalInputIDs,
	}
}

func forkStoredState(
	parent storedState,
	id session.ID,
	previousTurnID session.TurnID,
	createdAt time.Time,
) (storedState, error) {
	boundary := -1
	for index, item := range parent.Items {
		if boundary >= 0 && item.Kind == sessionstore.ItemTurn {
			break
		}
		switch item.Kind {
		case sessionstore.ItemTurn:
			if item.Data.(session.Turn).ID == previousTurnID {
				boundary = index
			}
		case sessionstore.ItemModelResponse:
			if item.Data.(sessionstore.ModelResponse).TurnID == previousTurnID {
				boundary = index
			}
		case sessionstore.ItemToolCallStatus:
			if item.Data.(sessionstore.ToolCallStatus).TurnID == previousTurnID {
				boundary = index
			}
		}
	}
	if boundary < 0 {
		return storedState{}, fmt.Errorf(
			"fork session %q at turn %q: %w",
			parent.Snapshot.Session.ID,
			previousTurnID,
			fs.ErrNotExist,
		)
	}

	state := newStoredState(id, createdAt)
	for _, item := range parent.Items[:boundary+1] {
		state.inheritItem(item)
	}
	state.appendFork(sessionstore.Fork{
		ParentID:       parent.Snapshot.Session.ID,
		PreviousTurnID: previousTurnID,
	}, createdAt)
	return state, nil
}

func (head *sessionHead) appendItem(
	kind sessionstore.ItemKind,
	data any,
	recordedAt time.Time,
) sessionstore.Item {
	head.itemSequence++
	return sessionstore.Item{
		Sequence:   head.itemSequence,
		RecordedAt: recordedAt,
		Kind:       kind,
		Data:       data,
	}
}

func (state *storedState) appendFork(value sessionstore.Fork, recordedAt time.Time) {
	state.Items = append(
		state.Items,
		state.appendItem(sessionstore.ItemFork, value, recordedAt),
	)
	state.resetOwnedState()
}

func (state *storedState) inheritItem(item sessionstore.Item) {
	state.itemSequence = item.Sequence
	switch item.Kind {
	case sessionstore.ItemTurn:
		turn := item.Data.(session.Turn)
		state.turns[turn.ID] = struct{}{}
		state.ownedTurns[turn.ID] = struct{}{}
		state.latestTurnID = turn.ID
	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse)
		state.respondedTurns[response.TurnID] = struct{}{}
	case sessionstore.ItemToolCallStatus:
		status := item.Data.(sessionstore.ToolCallStatus)
		// TODO: Preserve status snapshots in forked history without making inherited operations dispatchable.
		status.Operations = nil
		item.Data = status
		state.toolCallStatuses[toolCallStatusKey{
			turnID: status.TurnID,
			callID: status.CallID,
		}] = struct{}{}
	case sessionstore.ItemFork:
		state.resetOwnedState()
	}
	state.Items = append(state.Items, item)
}

func (head *sessionHead) resetOwnedState() {
	head.ownedTurns = make(map[session.TurnID]struct{})
	head.respondedTurns = make(map[session.TurnID]struct{})
	head.toolCallStatuses = make(map[toolCallStatusKey]struct{})
	head.Operations = nil
	head.operationPositions = make(map[operation.ID]int)
}

func validateOperation(value operation.Operation) error {
	if value.ID == "" {
		return fmt.Errorf("operation ID is empty")
	}
	if value.Type == "" {
		return fmt.Errorf("operation %q type is empty", value.ID)
	}
	if value.Version == 0 {
		return fmt.Errorf("operation %q version is zero", value.ID)
	}
	switch value.Status {
	case operation.StatusReady,
		operation.StatusAwaiting,
		operation.StatusCanceling,
		operation.StatusCompleted,
		operation.StatusFailed,
		operation.StatusCanceled:
	default:
		return fmt.Errorf("operation %q has unsupported status %q", value.ID, value.Status)
	}
	if value.State != nil && !value.State.IsValid() {
		return fmt.Errorf("operation %q state is not valid JSON", value.ID)
	}
	if value.Idempotency != nil && !value.Idempotency.IsValid() {
		return fmt.Errorf("operation %q idempotency data is not valid JSON", value.ID)
	}
	return nil
}

func terminalOperationStatus(status operation.Status) bool {
	switch status {
	case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
		return true
	default:
		return false
	}
}
