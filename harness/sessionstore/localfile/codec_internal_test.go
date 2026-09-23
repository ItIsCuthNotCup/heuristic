package localfile

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func TestEncodeInitialLogRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name  string
		state storedState
		want  string
	}{
		{
			name:  "invalid session ID",
			state: newStoredState("invalid/session", stateCreatedAt),
			want:  "must contain only ASCII letters, digits, and dashes",
		},
		{
			name: "unencodable creation time",
			state: newStoredState(
				"session-1",
				time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
			),
			want: "encode session",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := encodeInitialLog(test.state.Snapshot.Session, test.state.Items); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEncodeInitialLogRejectsInvalidItems(t *testing.T) {
	tests := []struct {
		name string
		item sessionstore.Item
		want string
	}{
		{
			name: "non-contiguous sequence",
			item: sessionstore.Item{
				Sequence: 2, RecordedAt: stateUpdatedAt,
				Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular},
			},
			want: "non-contiguous sequence",
		},
		{
			name: "zero recorded time",
			item: sessionstore.Item{
				Sequence: 1, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular},
			},
			want: "recorded time is zero",
		},
		{
			name: "wrong data type",
			item: sessionstore.Item{
				Sequence: 1, RecordedAt: stateUpdatedAt,
				Kind: sessionstore.ItemTurn, Data: inbox.Input{},
			},
			want: "turn data must be session.Turn",
		},
		{
			name: "invalid model response",
			item: sessionstore.Item{
				Sequence: 1, RecordedAt: stateUpdatedAt,
				Kind: sessionstore.ItemModelResponse,
				Data: sessionstore.ModelResponse{
					TurnID:   "turn-1",
					Response: llm.Response{Usage: llm.Usage{Raw: jsontext.Value(`{`)}},
				},
			},
			want: "unexpected EOF",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newStoredState("session-1", stateCreatedAt)
			state.Items = []sessionstore.Item{test.item}
			if _, err := encodeInitialLog(state.Snapshot.Session, state.Items); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEncodeInitialLogEmbedsJSONValues(t *testing.T) {
	state := newStoredState("session-1", stateCreatedAt)
	if err := state.appendInput(inbox.Input{
		ID: "input-1", Kind: inbox.InputExternal,
		Payload: jsontext.Value(`{"input":true}`),
	}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := state.appendTurn(session.Turn{ID: "turn-1", Type: session.TurnRegular}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := state.appendModelResponse(sessionstore.ModelResponse{
		TurnID: "turn-1",
		Response: llm.Response{
			Output: []llm.Item{{
				Type: llm.ItemReasoning,
				Data: llm.Reasoning{Raw: jsontext.Value(`{"reasoning":true}`)},
			}},
			Usage: llm.Usage{Raw: jsontext.Value(`{"usage":true}`)},
		},
	}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeInitialLog(state.Snapshot.Session, state.Items)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"Payload":{"input":true}`,
		`"Raw":{"reasoning":true}`,
		`"Raw":{"usage":true}`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("encoded state does not contain %s: %s", want, encoded)
		}
	}
}

func TestEncodeRecordKeepsEmbeddedJSONOnOneLine(t *testing.T) {
	encoded, err := encodeRecord(recordOperation, jsontext.Value("{\n  \"step\": 1\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(encoded, []byte{'\n'}) != 1 || encoded[len(encoded)-1] != '\n' {
		t.Fatalf("encoded record is not one JSONL line: %q", encoded)
	}
}

func TestDecodeLogFoldsOperationUpdates(t *testing.T) {
	encoded := emptyLog(t)
	turn := sessionstore.Item{
		Sequence: 1, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular},
	}
	encoded = append(encoded, mustRecord(t, recordItem, itemRecord{Item: turn})...)
	initial := validOperation("operation-1", operation.StatusReady)
	status := sessionstore.Item{
		Sequence: 2, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemToolCallStatus,
		Data: sessionstore.ToolCallStatus{
			TurnID: "turn-1", CallID: "call-1",
			Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
		},
	}
	encoded = append(encoded, mustRecord(t, recordItem, itemRecord{
		Item: status, Operations: []operation.Operation{initial},
	})...)
	updated := initial
	updated.Status = operation.StatusAwaiting
	updated.State = jsontext.Value(`{"step":2}`)
	encoded = append(encoded, mustRecord(t, recordOperation, operationRecord{Operation: updated})...)

	state, committedSize, err := decodeLog(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if committedSize != int64(len(encoded)) {
		t.Fatalf("committed size = %d, want %d", committedSize, len(encoded))
	}
	if len(state.Operations) != 1 || state.Operations[0].Status != operation.StatusAwaiting ||
		string(state.Operations[0].State) != `{"step":2}` {
		t.Fatalf("operations = %#v", state.Operations)
	}
}

func TestDecodeLogIgnoresIncompleteTail(t *testing.T) {
	committed := emptyLog(t)
	encoded := append(append([]byte(nil), committed...), []byte(`{"type":"item"`)...)
	state, committedSize, err := decodeLog(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if state.Snapshot.Session.ID != "session-1" || committedSize != int64(len(committed)) {
		t.Fatalf("state = %#v, committed size = %d", state, committedSize)
	}

	completedInvalid := append(append([]byte(nil), committed...), []byte("{\n")...)
	if _, _, err := decodeLog(completedInvalid); err == nil {
		t.Fatal("invalid completed record decoded")
	}
}

func TestDecodeLogPreservesInheritedInput(t *testing.T) {
	encoded := emptyLog(t)
	encoded = append(encoded, mustRecord(t, recordItem, itemRecord{Item: sessionstore.Item{
		Sequence: 1, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "parent-turn", Type: session.TurnRegular},
	}})...)
	encoded = append(encoded, mustRecord(t, recordItem, itemRecord{Item: sessionstore.Item{
		Sequence: 2, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemInput,
		Data: inbox.Input{
			ID: "parent-input", Kind: inbox.InputExternal, Payload: jsontext.Value(`{}`),
		},
	}})...)
	encoded = append(encoded, mustRecord(t, recordItem, itemRecord{Item: sessionstore.Item{
		Sequence: 3, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemFork,
		Data: sessionstore.Fork{ParentID: "parent", PreviousTurnID: "parent-turn"},
	}})...)

	state, _, err := decodeLog(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Items[1].Data.(inbox.Input).ID; got != "parent-input" {
		t.Fatalf("inherited input ID = %q, want parent-input", got)
	}
}

func TestDecodeLogRejectsInvalidRecords(t *testing.T) {
	header := emptyLog(t)
	turn := sessionstore.Item{
		Sequence: 1, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular},
	}
	status := sessionstore.Item{
		Sequence: 2, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemToolCallStatus,
		Data: sessionstore.ToolCallStatus{
			TurnID: "turn-1", CallID: "call-1",
			Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
		},
	}
	response := sessionstore.Item{
		Sequence: 2, RecordedAt: stateUpdatedAt,
		Kind: sessionstore.ItemModelResponse,
		Data: sessionstore.ModelResponse{
			TurnID: "turn-1", Response: llm.Response{Output: []llm.Item{}},
		},
	}
	withTurn := append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{Item: turn})...)
	withResponse := append(append([]byte(nil), withTurn...), mustRecord(t, recordItem, itemRecord{Item: response})...)
	initial := validOperation("operation-1", operation.StatusReady)
	withStatus := append(append([]byte(nil), withTurn...), mustRecord(t, recordItem, itemRecord{
		Item: status, Operations: []operation.Operation{initial},
	})...)

	tests := []struct {
		name    string
		encoded []byte
		want    string
	}{
		{name: "no committed record", encoded: []byte(`{}`), want: "no committed records"},
		{name: "unsupported record", encoded: mustRecord(t, "unknown", struct{}{}), want: "unsupported record type"},
		{name: "item first", encoded: mustRecord(t, recordItem, itemRecord{Item: turn}), want: "session record must be first"},
		{
			name: "operation first",
			encoded: mustRecord(t, recordOperation, operationRecord{
				Operation: initial,
			}),
			want: "session record must be first",
		},
		{
			name: "session not first",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordSession, sessionRecord{
				Version: formatVersion, Session: session.Session{ID: "other", CreatedAt: stateCreatedAt},
			})...),
			want: "session record must be first",
		},
		{
			name:    "malformed session data",
			encoded: mustRecord(t, recordSession, jsontext.Value(`[]`)),
			want:    "decode session record",
		},
		{
			name: "legacy version",
			encoded: mustRecord(t, recordSession, sessionRecord{
				Version: 1, Session: session.Session{ID: "session-1", CreatedAt: stateCreatedAt},
			}),
			want: "legacy session format version 1 cannot be resumed",
		},
		{
			name: "unsupported version",
			encoded: mustRecord(t, recordSession, sessionRecord{
				Version: formatVersion + 1, Session: session.Session{ID: "session-1", CreatedAt: stateCreatedAt},
			}),
			want: "unsupported session format version",
		},
		{
			name: "invalid session ID",
			encoded: mustRecord(t, recordSession, sessionRecord{
				Version: formatVersion,
				Session: session.Session{ID: "invalid/id", CreatedAt: stateCreatedAt},
			}),
			want: "must contain only ASCII letters, digits, and dashes",
		},
		{
			name: "zero creation time",
			encoded: mustRecord(t, recordSession, sessionRecord{
				Version: formatVersion, Session: session.Session{ID: "session-1"},
			}),
			want: "creation time is zero",
		},
		{
			name:    "malformed item data",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordItem, jsontext.Value(`[]`))...),
			want:    "decode item record",
		},
		{
			name: "non-contiguous item",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{Item: sessionstore.Item{
				Sequence: 2, RecordedAt: stateUpdatedAt,
				Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular},
			}})...),
			want: "has sequence 2, want 1",
		},
		{
			name: "zero item time",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{Item: sessionstore.Item{
				Sequence: 1, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular},
			}})...),
			want: "recorded time is zero",
		},
		{
			name: "empty turn ID",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 1, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemTurn, Data: session.Turn{},
				},
			})...),
			want: "turn ID is empty",
		},
		{
			name: "wrong previous turn",
			encoded: append(append([]byte(nil), withTurn...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 2, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemTurn,
					Data: session.Turn{ID: "turn-2", PreviousTurnID: "other", Type: session.TurnRegular},
				},
			})...),
			want: `previous turn is "other", want "turn-1"`,
		},
		{
			name: "duplicate turn",
			encoded: append(append([]byte(nil), withTurn...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 2, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemTurn,
					Data: session.Turn{ID: "turn-1", PreviousTurnID: "turn-1", Type: session.TurnRegular},
				},
			})...),
			want: `append turn "turn-1": file already exists`,
		},
		{
			name: "response for missing turn",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 1, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemModelResponse,
					Data: sessionstore.ModelResponse{
						TurnID: "missing", Response: llm.Response{Output: []llm.Item{}},
					},
				},
			})...),
			want: "file does not exist",
		},
		{
			name: "duplicate response",
			encoded: append(append([]byte(nil), withResponse...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 3, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemModelResponse,
					Data: sessionstore.ModelResponse{
						TurnID: "turn-1", Response: llm.Response{Output: []llm.Item{}},
					},
				},
			})...),
			want: `append model response for turn "turn-1": file already exists`,
		},
		{
			name: "status for missing turn",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 1, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemToolCallStatus,
					Data: sessionstore.ToolCallStatus{
						TurnID: "missing", CallID: "call-1",
						Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
					},
				},
				Operations: []operation.Operation{initial},
			})...),
			want: "file does not exist",
		},
		{
			name: "operations on turn",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{
				Item: turn, Operations: []operation.Operation{validOperation("operation-1", operation.StatusReady)},
			})...),
			want: "initializes operations",
		},
		{
			name:    "status without operation",
			encoded: append(append([]byte(nil), withTurn...), mustRecord(t, recordItem, itemRecord{Item: status})...),
			want:    "must initialize at least one operation",
		},
		{
			name: "invalid initialized operation",
			encoded: append(append([]byte(nil), withTurn...), mustRecord(t, recordItem, itemRecord{
				Item: status,
				Operations: []operation.Operation{{
					Type: "test", Version: 1, Status: operation.StatusReady,
				}},
			})...),
			want: "operation ID is empty",
		},
		{
			name: "duplicate initialized operation",
			encoded: append(append([]byte(nil), withStatus...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 3, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemToolCallStatus,
					Data: sessionstore.ToolCallStatus{
						TurnID: "turn-1", CallID: "call-2",
						Status: tool.CallStatus{WaitingFor: []operation.ID{"operation-1"}},
					},
				},
				Operations: []operation.Operation{initial},
			})...),
			want: `initialize operation "operation-1": file already exists`,
		},
		{
			name: "operation inherited across fork",
			encoded: append(append([]byte(nil), withStatus...), mustRecord(t, recordItem, itemRecord{
				Item: sessionstore.Item{
					Sequence: 3, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemFork,
					Data: sessionstore.Fork{ParentID: "parent", PreviousTurnID: "turn-1"},
				},
			})...),
			want: "inherited item 1 initializes operations",
		},
		{
			name: "operation update inside inherited prefix",
			encoded: append(
				append(
					append(append([]byte(nil), header...), mustRecord(t, recordItem, itemRecord{Item: turn})...),
					mustRecord(t, recordOperation, operationRecord{Operation: initial})...,
				),
				mustRecord(t, recordItem, itemRecord{Item: sessionstore.Item{
					Sequence: 2, RecordedAt: stateUpdatedAt,
					Kind: sessionstore.ItemFork,
					Data: sessionstore.Fork{ParentID: "parent", PreviousTurnID: "turn-1"},
				}})...,
			),
			want: "appears in inherited history",
		},
		{
			name:    "malformed operation update",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordOperation, jsontext.Value(`[]`))...),
			want:    "decode operation record",
		},
		{
			name: "missing operation update",
			encoded: append(append([]byte(nil), header...), mustRecord(t, recordOperation, operationRecord{
				Operation: validOperation("missing", operation.StatusReady),
			})...),
			want: "file does not exist",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := decodeLog(test.encoded); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestTurnTypesSurviveReopen(t *testing.T) {
	for _, turnType := range []session.TurnType{session.TurnRegular, session.TurnCompaction} {
		for _, outcome := range []string{"incomplete", "response"} {
			t.Run(string(turnType)+"/"+outcome, func(t *testing.T) {
				state := newStoredState("session-1", stateCreatedAt)
				if err := state.appendTurn(session.Turn{ID: "turn-1", Type: turnType}, stateUpdatedAt); err != nil {
					t.Fatal(err)
				}
				response := validResponse("turn-1")
				if outcome == "response" {
					if err := state.appendModelResponse(response, stateUpdatedAt); err != nil {
						t.Fatal(err)
					}
				}
				encoded, err := encodeInitialLog(state.Snapshot.Session, state.Items)
				if err != nil {
					t.Fatal(err)
				}
				reopened, _, err := decodeLog(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(state, reopened) {
					t.Fatal("reopen changed state")
				}
				err = reopened.appendModelResponse(response, stateUpdatedAt)
				if outcome == "response" {
					if !errors.Is(err, fs.ErrExist) {
						t.Fatalf("duplicate response accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func emptyLog(t *testing.T) []byte {
	t.Helper()
	return mustRecord(t, recordSession, sessionRecord{
		Version: formatVersion,
		Session: session.Session{ID: "session-1", CreatedAt: stateCreatedAt},
	})
}

func mustRecord(t *testing.T, kind recordType, value any) []byte {
	t.Helper()
	encoded, err := encodeRecord(kind, value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
