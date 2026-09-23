// Package tool defines model-visible tool selection and pure translation
// between model tool calls and durable operations.
package tool

import (
	"encoding/json/jsontext"
	"uuid"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
)

type CallStatus struct {
	Error          string
	ErrorTruncated bool           `json:",omitzero"`
	WaitingFor     []operation.ID `json:",omitzero"`
}

// Context is turn-local and coordinator-owned. Submit allocates an ID and
// records inert data without performing I/O or handing work to another queue.
type Context interface {
	Submit(operation.Spec) operation.ID
}

type ResultTranslator interface {
	TranslateResult(string, CallStatus, []operation.Operation) (llm.ToolResult, error)
}

type Translator interface {
	ResultTranslator
	Translate(Context, llm.ToolCall) CallStatus
}

type Definition struct {
	Tool     llm.Tool
	Metadata jsontext.Value
}

type RegistrationID = uuid.UUID

type Registry interface {
	StaticDefinitions() []Definition
	Resolve(string) (Translator, bool)
	RegisterSkill(Skill) (RegistrationID, error)
	UnregisterSkill(RegistrationID)
	Skills() []Skill
}

type Skill struct {
	Name        string
	Description string
	Path        string
}
