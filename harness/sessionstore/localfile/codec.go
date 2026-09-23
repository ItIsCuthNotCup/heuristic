package localfile

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
)

const formatVersion = 2

type recordType string

const (
	recordSession   recordType = "session"
	recordItem      recordType = "item"
	recordOperation recordType = "operation"
)

type logRecord struct {
	Type recordType     `json:"type"`
	Data jsontext.Value `json:"data"`
}

type sessionRecord struct {
	Version int
	Session session.Session
}

type itemRecord struct {
	Item       sessionstore.Item
	Operations []operation.Operation `json:",omitempty"`
}

type operationRecord struct {
	Operation operation.Operation
}

type decodedRecord struct {
	Type      recordType
	Item      itemRecord
	Operation operation.Operation
}

func encodeInitialLog(value session.Session, items []sessionstore.Item) ([]byte, error) {
	if err := validateSessionID(value.ID); err != nil {
		return nil, err
	}

	encoded, err := encodeRecord(recordSession, sessionRecord{
		Version: formatVersion,
		Session: value,
	})
	if err != nil {
		return nil, fmt.Errorf("encode session: %w", err)
	}

	for index, item := range items {
		if item.Sequence != sessionstore.Sequence(index+1) {
			return nil, fmt.Errorf("item %d has non-contiguous sequence %d", index, item.Sequence)
		}
		if item.RecordedAt.IsZero() {
			return nil, fmt.Errorf("item %d recorded time is zero", index)
		}

		record := itemRecord{Item: item}
		if item.Kind == sessionstore.ItemToolCallStatus {
			status := item.Data.(sessionstore.ToolCallStatus)
			record.Operations = status.Operations
			status.Operations = nil
			record.Item.Data = status
		}
		line, err := encodeRecord(recordItem, record)
		if err != nil {
			return nil, fmt.Errorf("encode item %d: %w", index, err)
		}
		encoded = append(encoded, line...)
	}
	return encoded, nil
}

func decodeLog(encoded []byte) (storedState, int64, error) {
	lastNewline := bytes.LastIndexByte(encoded, '\n')
	if lastNewline < 0 {
		return storedState{}, 0, fmt.Errorf("session log has no committed records")
	}
	committedSize := int64(lastNewline + 1)
	lines := bytes.Split(encoded[:committedSize], []byte{'\n'})
	lines = lines[:len(lines)-1]

	var value storedState
	records := make([]decodedRecord, 0, len(lines))
	itemCount := 0
	inheritedItemCount := 0
	for lineIndex, line := range lines {
		var record logRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return storedState{}, 0, fmt.Errorf("decode record %d: %w", lineIndex+1, err)
		}
		switch record.Type {
		case recordSession:
			if lineIndex != 0 {
				return storedState{}, 0, fmt.Errorf("session record must be first")
			}
			var header sessionRecord
			if err := json.Unmarshal(record.Data, &header); err != nil {
				return storedState{}, 0, fmt.Errorf("decode session record: %w", err)
			}
			if header.Version == 1 {
				return storedState{}, 0, fmt.Errorf(
					"legacy session format version 1 cannot be resumed",
				)
			}
			if header.Version != formatVersion {
				return storedState{}, 0, fmt.Errorf("unsupported session format version %d", header.Version)
			}
			if err := validateSessionID(header.Session.ID); err != nil {
				return storedState{}, 0, err
			}
			if header.Session.CreatedAt.IsZero() {
				return storedState{}, 0, fmt.Errorf("session %q creation time is zero", header.Session.ID)
			}
			value = newStoredState(header.Session.ID, header.Session.CreatedAt)
		case recordItem:
			if lineIndex == 0 {
				return storedState{}, 0, fmt.Errorf("session record must be first")
			}
			var itemValue itemRecord
			if err := json.Unmarshal(record.Data, &itemValue); err != nil {
				return storedState{}, 0, fmt.Errorf("decode item record: %w", err)
			}
			wantSequence := sessionstore.Sequence(itemCount + 1)
			if itemValue.Item.Sequence != wantSequence {
				return storedState{}, 0, fmt.Errorf(
					"item %d has sequence %d, want %d",
					itemCount,
					itemValue.Item.Sequence,
					wantSequence,
				)
			}
			if itemValue.Item.RecordedAt.IsZero() {
				return storedState{}, 0, fmt.Errorf("item %d recorded time is zero", itemCount)
			}
			if itemValue.Item.Kind != sessionstore.ItemToolCallStatus && len(itemValue.Operations) != 0 {
				return storedState{}, 0, fmt.Errorf(
					"item %d of kind %q initializes operations",
					itemCount,
					itemValue.Item.Kind,
				)
			}
			itemCount++
			if itemValue.Item.Kind == sessionstore.ItemFork {
				inheritedItemCount = itemCount
			}
			records = append(records, decodedRecord{Type: recordItem, Item: itemValue})
		case recordOperation:
			if lineIndex == 0 {
				return storedState{}, 0, fmt.Errorf("session record must be first")
			}
			var update operationRecord
			if err := json.Unmarshal(record.Data, &update); err != nil {
				return storedState{}, 0, fmt.Errorf("decode operation record: %w", err)
			}
			records = append(records, decodedRecord{
				Type: recordOperation, Operation: update.Operation,
			})
		default:
			return storedState{}, 0, fmt.Errorf("unsupported record type %q", record.Type)
		}
	}

	replayedItems := 0
	for recordIndex, record := range records {
		switch record.Type {
		case recordItem:
			itemIndex := replayedItems
			replayedItems++
			if itemIndex < inheritedItemCount {
				if len(record.Item.Operations) != 0 {
					return storedState{}, 0, fmt.Errorf("inherited item %d initializes operations", itemIndex)
				}
				value.inheritItem(record.Item.Item)
				continue
			}
			if err := replayItem(&value, record.Item); err != nil {
				return storedState{}, 0, fmt.Errorf("apply item %d: %w", itemIndex, err)
			}
		case recordOperation:
			if replayedItems < inheritedItemCount {
				return storedState{}, 0, fmt.Errorf(
					"operation record %d appears in inherited history",
					recordIndex+2,
				)
			}
			if err := value.saveOperation(record.Operation); err != nil {
				return storedState{}, 0, fmt.Errorf("apply operation record: %w", err)
			}
		}
	}
	return value, committedSize, nil
}

func replayItem(state *storedState, record itemRecord) error {
	switch record.Item.Kind {
	case sessionstore.ItemInput:
		return state.appendInput(record.Item.Data.(inbox.Input), record.Item.RecordedAt)
	case sessionstore.ItemTurn:
		return state.appendTurn(record.Item.Data.(session.Turn), record.Item.RecordedAt)
	case sessionstore.ItemModelResponse:
		return state.appendModelResponse(
			record.Item.Data.(sessionstore.ModelResponse),
			record.Item.RecordedAt,
		)
	case sessionstore.ItemToolCallStatus:
		return state.appendToolCallStatus(
			record.Item.Data.(sessionstore.ToolCallStatus),
			record.Operations,
			record.Item.RecordedAt,
		)
	default:
		return fmt.Errorf("unexpected owned item kind %q", record.Item.Kind)
	}
}

func encodeRecord(kind recordType, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(logRecord{Type: kind, Data: data})
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(encoded, '\n') >= 0 {
		return nil, fmt.Errorf("encoded record contains a newline")
	}
	return append(encoded, '\n'), nil
}
