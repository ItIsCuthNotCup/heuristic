// Package inbox owns input deduplication for one session.
package inbox

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

type ID string

type InputKind string

const (
	InputExternal InputKind = "external"
	InputControl  InputKind = "control"
	InputCrash    InputKind = "crash"
)

type Input struct {
	ID      ID
	Kind    InputKind
	Payload jsontext.Value `json:",omitzero"`
}

func (input Input) Validate() error {
	if input.ID == "" {
		return fmt.Errorf("input ID is empty")
	}
	switch input.Kind {
	case InputExternal, InputCrash:
	case InputControl:
		if _, err := input.DecodeControlMessage(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("input %q has unsupported kind %q", input.ID, input.Kind)
	}
	if input.Payload != nil && !input.Payload.IsValid() {
		return fmt.Errorf("input %q payload is not valid JSON", input.ID)
	}
	return nil
}

type ControlMode string

const (
	StopHard       ControlMode = "hard"
	StopWhenIdle   ControlMode = "when_idle"
	Heartbeat      ControlMode = "heartbeat"
	UpdateSettings ControlMode = "settings"
)

type Settings struct {
	ReasoningEffort llm.ReasoningEffort `json:",omitzero"`
}

type ControlMessage struct {
	Mode       ControlMode
	Reason     string
	Parameters any `json:",omitzero"`
}

func (input Input) DecodeControlMessage() (ControlMessage, error) {
	if input.Kind != InputControl {
		return ControlMessage{}, fmt.Errorf("control message input has kind %q", input.Kind)
	}
	var envelope struct {
		Mode       ControlMode
		Reason     string
		Parameters jsontext.Value
	}
	if err := json.Unmarshal(input.Payload, &envelope, json.RejectUnknownMembers(true)); err != nil {
		return ControlMessage{}, fmt.Errorf("decode control message: %w", err)
	}
	request := ControlMessage{Mode: envelope.Mode, Reason: envelope.Reason}
	if request.Mode != UpdateSettings && len(envelope.Parameters) != 0 {
		return ControlMessage{}, fmt.Errorf("control mode %q does not accept parameters", request.Mode)
	}
	switch request.Mode {
	case StopHard, StopWhenIdle:
	case Heartbeat:
		if request.Reason == "" {
			return ControlMessage{}, fmt.Errorf("heartbeat reason is empty")
		}
	case UpdateSettings:
		var settings Settings
		if err := json.Unmarshal(envelope.Parameters, &settings, json.RejectUnknownMembers(true)); err != nil {
			return ControlMessage{}, fmt.Errorf("decode settings parameters: %w", err)
		}
		if !settings.ReasoningEffort.Valid() {
			return ControlMessage{}, fmt.Errorf("unsupported reasoning effort %q", settings.ReasoningEffort)
		}
		request.Parameters = settings
	default:
		return ControlMessage{}, fmt.Errorf("unsupported control mode %q", request.Mode)
	}
	return request, nil
}

type Writer interface {
	Submit(context.Context, Input) error
}
