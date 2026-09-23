package operation

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"

	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

const (
	TypeSkillUse    Type    = "skill_use"
	VersionSkillUse Version = 1

	skillUseReadCorrelation primitives.CorrelationID = "skill-use-read"
)

type SkillUseState struct {
	Path          string
	Content       []byte
	TerminalError string
}

func NewSkillUseSpec(path string) (Spec, error) {
	state := SkillUseState{Path: path}
	if err := validateSkillUseState(state); err != nil {
		return Spec{}, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return Spec{}, fmt.Errorf("encode skill-use operation state: %w", err)
	}
	return Spec{Type: TypeSkillUse, Version: VersionSkillUse, State: encoded}, nil
}

func DecodeSkillUse(current Operation) (SkillUseState, error) {
	return skillUseOperationState(current)
}

func AdvanceSkillUse(current Operation, event *primitives.PrimitiveEvent) (Step, error) {
	state, err := skillUseOperationState(current)
	if err != nil {
		return Step{}, err
	}

	switch current.Status {
	case StatusReady:
		if event != nil {
			return Step{}, fmt.Errorf("advance ready skill-use operation %q: unexpected primitive event", current.ID)
		}
		if len(state.Content) != 0 || state.TerminalError != "" {
			return Step{}, fmt.Errorf("advance ready skill-use operation %q: state is not initial", current.ID)
		}
		return dispatchSkillUseRead(current, state)

	case StatusAwaiting:
		if state.TerminalError != "" {
			return Step{}, fmt.Errorf("advance awaiting skill-use operation %q: state is terminal", current.ID)
		}
		if event == nil {
			return dispatchSkillUseRead(current, state)
		}
		return advanceAwaitingSkillUse(current, state, *event)

	case StatusCanceling:
		if event != nil {
			return Step{}, fmt.Errorf("advance canceling skill-use operation %q: unexpected primitive event", current.ID)
		}
		return cancelSkillUse(current, state)

	default:
		return Step{}, fmt.Errorf("advance skill-use operation %q: terminal status %q", current.ID, current.Status)
	}
}

func skillUseOperationState(current Operation) (SkillUseState, error) {
	if current.Type != TypeSkillUse {
		return SkillUseState{}, fmt.Errorf(
			"decode skill-use operation %q: type %q: %w",
			current.ID,
			current.Type,
			ErrUnsupported,
		)
	}
	if current.Version != VersionSkillUse {
		return SkillUseState{}, fmt.Errorf(
			"decode skill-use operation %q: version %d: %w",
			current.ID,
			current.Version,
			ErrUnsupported,
		)
	}
	var state SkillUseState
	if err := json.Unmarshal(current.State, &state); err != nil {
		return SkillUseState{}, fmt.Errorf("decode skill-use operation %q state: %w", current.ID, err)
	}
	if err := validateSkillUseState(state); err != nil {
		return SkillUseState{}, fmt.Errorf("validate skill-use operation %q state: %w", current.ID, err)
	}
	return state, nil
}

func validateSkillUseState(state SkillUseState) error {
	if state.Path == "" {
		return errors.New("skill path must be set")
	}
	return nil
}

func advanceAwaitingSkillUse(
	current Operation,
	state SkillUseState,
	event primitives.PrimitiveEvent,
) (Step, error) {
	if event.Source != primitives.SourceID(current.ID) {
		return failSkillUse(current, state, fmt.Errorf(
			"skill-use primitive event source is %q, want %q",
			event.Source,
			current.ID,
		))
	}
	if event.Type == primitives.PrimitiveEventCanceled {
		return cancelSkillUse(current, state)
	}
	if event.Type == primitives.PrimitiveEventFailed {
		failure, ok := event.Result.(primitives.PrimitiveFailureResult)
		if !ok {
			return failSkillUse(current, state, errors.New("skill-use primitive failed with an invalid result"))
		}
		return failSkillUse(current, state, errors.New(failure.Error))
	}
	if event.CorrelationID != skillUseReadCorrelation {
		return failSkillUse(current, state, fmt.Errorf(
			"skill-use primitive event correlation is %q, want %q",
			event.CorrelationID,
			skillUseReadCorrelation,
		))
	}

	switch event.Type {
	case primitives.PrimitiveEventIOReadOutput:
		output, ok := event.Result.(primitives.IOReadOutputResult)
		if !ok || output.Offset != int64(len(state.Content)) {
			return failSkillUse(current, state, errors.New("skill read returned invalid output"))
		}
		state.Content = append(state.Content, output.Data...)
		return awaitSkillUse(current, state)

	case primitives.PrimitiveEventIOReadCompleted:
		result, ok := event.Result.(primitives.IOReadCompletedResult)
		if !ok || result.Size != int64(len(state.Content)) {
			return failSkillUse(current, state, errors.New("skill read returned an invalid completion"))
		}
		current.Status = StatusCompleted
		return skillUseStep(current, state)

	default:
		return failSkillUse(current, state, fmt.Errorf("skill read returned unexpected event %q", event.Type))
	}
}

func dispatchSkillUseRead(current Operation, state SkillUseState) (Step, error) {
	step, err := awaitSkillUse(current, state)
	if err != nil {
		return Step{}, err
	}
	step.Dispatches = []PrimitiveDispatch{{
		Type: primitives.PrimitiveDispatchIORead,
		Data: primitives.IOReadRequest{
			Source:        primitives.SourceID(current.ID),
			CorrelationID: skillUseReadCorrelation,
			Path:          state.Path,
			Offset:        int64(len(state.Content)),
			Count:         math.MaxInt64,
		},
	}}
	return step, nil
}

func awaitSkillUse(current Operation, state SkillUseState) (Step, error) {
	current.Status = StatusAwaiting
	return skillUseStep(current, state)
}

func skillUseStep(current Operation, state SkillUseState) (Step, error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return Step{}, fmt.Errorf("encode skill-use operation %q state: %w", current.ID, err)
	}
	current.State = encoded
	return Step{Operation: &current}, nil
}

func failSkillUse(current Operation, state SkillUseState, err error) (Step, error) {
	state.TerminalError = err.Error()
	current.Status = StatusFailed
	return skillUseStep(current, state)
}

func cancelSkillUse(current Operation, state SkillUseState) (Step, error) {
	state.TerminalError = "skill-use operation canceled"
	current.Status = StatusCanceled
	return skillUseStep(current, state)
}
