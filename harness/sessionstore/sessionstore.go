// Package sessionstore defines append-only session history and operation state.
package sessionstore

import (
	"context"
	"time"
	"uuid"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

type Sequence uint64

// BeforeFirst is the cursor before the first assigned sequence.
const BeforeFirst Sequence = 0

type ItemKind string

const (
	ItemFork           ItemKind = "fork"
	ItemInput          ItemKind = "input"
	ItemTurn           ItemKind = "turn"
	ItemModelResponse  ItemKind = "model_response"
	ItemToolCallStatus ItemKind = "tool_call_status"
)

// Item Data is Fork, inbox.Input, session.Turn, ModelResponse, or
// ToolCallStatus according to Kind.
type Item struct {
	Sequence   Sequence
	RecordedAt time.Time
	Kind       ItemKind
	Data       any
}

type ObserverID = uuid.UUID

// Observer is called synchronously after an item is persisted. The session ID
// identifies the history containing the item.
type Observer func(session.ID, Item)

type Fork struct {
	ParentID       session.ID
	PreviousTurnID session.TurnID
}

type ModelResponse struct {
	TurnID   session.TurnID
	Response llm.Response
}

type ToolCallStatus struct {
	TurnID     session.TurnID
	CallID     string
	Status     tool.CallStatus
	Operations []operation.Operation `json:",omitempty"`
}

type Snapshot struct {
	Session session.Session
}

type SessionInfo struct {
	ID            session.ID
	LastUpdatedAt time.Time
}

type Page struct {
	Items     []Item
	NextAfter Sequence
	More      bool
}

type ResumeState struct {
	Snapshot         Snapshot
	Operations       []operation.Operation // Unfinished operations and terminal states missing from tool-call history.
	ExternalInputIDs []inbox.ID
}

// Store does not serialize methods for the same session ID.
type Store interface {
	// AddObserver and RemoveObserver are not safe for concurrent use with each
	// other or with methods that persist items.
	AddObserver(Observer) ObserverID
	RemoveObserver(ObserverID)
	Create(context.Context, session.ID) (Snapshot, error)
	ListSessions(context.Context) ([]SessionInfo, error)
	Inspect(context.Context, session.ID) (Snapshot, error)
	Items(context.Context, session.ID, Sequence, int) (Page, error)
	AppendInput(context.Context, session.ID, inbox.Input) error
	AppendTurn(context.Context, session.ID, session.Turn) error
	AppendModelResponse(context.Context, session.ID, ModelResponse) error
	// AppendToolCallStatus appends the status and its operation snapshots.
	// The first append also initializes those operations.
	AppendToolCallStatus(context.Context, session.ID, ToolCallStatus) error
	// SaveOperation stores a complete state; the latest state for its ID wins.
	SaveOperation(context.Context, session.ID, operation.Operation) error
	Resume(context.Context, session.ID) (ResumeState, error)
	Fork(
		ctx context.Context,
		id session.ID,
		parentID session.ID,
		previousTurnID session.TurnID,
	) (Snapshot, error)
}
