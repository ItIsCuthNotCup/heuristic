package sessionstore

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
)

type itemJSON Item

func (item Item) MarshalJSON() ([]byte, error) {
	if err := item.validateData(); err != nil {
		return nil, err
	}
	return json.Marshal(itemJSON(item))
}

func (item *Item) UnmarshalJSON(encoded []byte) error {
	var decoded struct {
		itemJSON
		Data jsontext.Value
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return err
	}

	data, err := decodeItemData(decoded.Kind, decoded.Data)
	if err != nil {
		return err
	}
	decoded.itemJSON.Data = data
	*item = Item(decoded.itemJSON)
	return nil
}

func (item Item) validateData() error {
	switch item.Kind {
	case ItemFork:
		if _, ok := item.Data.(Fork); !ok {
			return fmt.Errorf("fork data must be sessionstore.Fork, got %T", item.Data)
		}
	case ItemInput:
		input, ok := item.Data.(inbox.Input)
		if !ok {
			return fmt.Errorf("input data must be inbox.Input, got %T", item.Data)
		}
		if err := input.Validate(); err != nil {
			return fmt.Errorf("invalid input data: %w", err)
		}
	case ItemTurn:
		if _, ok := item.Data.(session.Turn); !ok {
			return fmt.Errorf("turn data must be session.Turn, got %T", item.Data)
		}
	case ItemModelResponse:
		if _, ok := item.Data.(ModelResponse); !ok {
			return fmt.Errorf("model response data must be sessionstore.ModelResponse, got %T", item.Data)
		}
	case ItemToolCallStatus:
		if _, ok := item.Data.(ToolCallStatus); !ok {
			return fmt.Errorf("tool-call status data must be sessionstore.ToolCallStatus, got %T", item.Data)
		}
	default:
		return fmt.Errorf("unsupported item kind %q", item.Kind)
	}
	return nil
}

func decodeItemData(kind ItemKind, encoded jsontext.Value) (any, error) {
	if encoded.Kind() == jsontext.KindNull {
		return nil, fmt.Errorf("%s data must not be null", kind)
	}
	switch kind {
	case ItemFork:
		var value Fork
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode fork data: %w", err)
		}
		return value, nil
	case ItemInput:
		var value inbox.Input
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode input data: %w", err)
		}
		if err := value.Validate(); err != nil {
			return nil, fmt.Errorf("decode input data: %w", err)
		}
		return value, nil
	case ItemTurn:
		var value session.Turn
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode turn data: %w", err)
		}
		return value, nil
	case ItemModelResponse:
		var value ModelResponse
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode model response data: %w", err)
		}
		return value, nil
	case ItemToolCallStatus:
		var value ToolCallStatus
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode tool-call status data: %w", err)
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported item kind %q", kind)
	}
}
