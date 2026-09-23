package operation

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

const (
	TypeValue    Type    = "value"
	VersionValue Version = 1
)

type ValueState struct {
	Value jsontext.Value
}

func NewValueSpec(value jsontext.Value) (Spec, error) {
	if len(value) == 0 || !value.IsValid() {
		return Spec{}, errors.New("value operation result must be valid JSON")
	}
	encoded, err := json.Marshal(ValueState{Value: value})
	if err != nil {
		return Spec{}, fmt.Errorf("encode value operation state: %w", err)
	}
	return Spec{Type: TypeValue, Version: VersionValue, State: encoded}, nil
}

func DecodeValue(current Operation) (jsontext.Value, error) {
	if current.Type != TypeValue {
		return nil, fmt.Errorf(
			"decode value operation %q: type %q: %w",
			current.ID,
			current.Type,
			ErrUnsupported,
		)
	}
	if current.Version != VersionValue {
		return nil, fmt.Errorf(
			"decode value operation %q: version %d: %w",
			current.ID,
			current.Version,
			ErrUnsupported,
		)
	}
	var state ValueState
	if err := json.Unmarshal(current.State, &state); err != nil {
		return nil, fmt.Errorf("decode value operation %q state: %w", current.ID, err)
	}
	if len(state.Value) == 0 || !state.Value.IsValid() {
		return nil, fmt.Errorf("decode value operation %q state: result must be valid JSON", current.ID)
	}
	return state.Value, nil
}

func AdvanceValue(current Operation, event *primitives.PrimitiveEvent) (Step, error) {
	if _, err := DecodeValue(current); err != nil {
		return Step{}, err
	}
	if event != nil {
		return Step{}, fmt.Errorf("advance value operation %q: unexpected primitive event", current.ID)
	}
	switch current.Status {
	case StatusReady:
		current.Status = StatusCompleted
	case StatusCanceling:
		current.Status = StatusCanceled
	default:
		return Step{}, fmt.Errorf(
			"advance value operation %q: status %q: %w",
			current.ID,
			current.Status,
			ErrUnsupported,
		)
	}
	return Step{Operation: &current}, nil
}
