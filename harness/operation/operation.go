// Package operation defines durable operation values and their actor runtime.
package operation

import (
	"encoding/json/jsontext"
	"errors"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

var ErrUnsupported = errors.New("unsupported operation")

type Type string

type Version uint32

type ID string

type Status string

const (
	StatusReady     Status = "ready"
	StatusAwaiting  Status = "awaiting"
	StatusCanceling Status = "canceling"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

type Spec struct {
	MaxOutputLength int `json:",omitzero"`
	Type            Type
	Version         Version
	State           jsontext.Value `json:",omitzero"`
	Idempotency     jsontext.Value `json:",omitzero"`
}

type Operation struct {
	MaxOutputLength int `json:",omitzero"`
	ID              ID
	Type            Type
	Version         Version
	Status          Status
	State           jsontext.Value `json:",omitzero"`
	Idempotency     jsontext.Value `json:",omitzero"`
}

type PrimitiveDispatch struct {
	Type primitives.PrimitiveDispatchType
	Data any
}

type Step struct {
	// Operation is the next durable checkpoint; nil leaves the checkpoint unchanged.
	Operation  *Operation
	Dispatches []PrimitiveDispatch
}

type Manager interface {
	// Add starts an operation at most once for each ID during the manager's lifetime.
	// It returns ErrUnsupported when the operation type or version cannot be handled.
	Add(Operation) error
	Cancel(ID, string) error
	Updates() <-chan Operation
}
