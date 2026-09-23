package responsesapi

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

const fallbackMessage = `{"id":"msg-1","type":"message","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"fallback"}]}`
const fallbackReasoning = `{"id":"rs-1","type":"reasoning","summary":[],"encrypted_content":"opaque","vendor_field":"kept"}`
const fallbackCall = `{"id":"fc-1","type":"function_call","call_id":"call-1","name":"Bash","arguments":"{\"command\":\"pwd\"}","status":"completed"}`

func TestCollectedItemsFallback(t *testing.T) {
	for _, output := range []string{"", `,"output":[]`, `,"output":[ ]`, `,"output":null`, `,"output":[` + fallbackMessage + `]`} {
		t.Run(output, func(t *testing.T) {
			var state responseState
			for _, item := range []struct {
				index int
				body  string
			}{{2, fallbackCall}, {0, fallbackReasoning}, {1, fallbackMessage}, {1, fallbackMessage}} {
				event := fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, item.index, item.body)
				if err := state.observe([]byte(event)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := state.unwrap(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("items escaped without terminal response: %v", err)
			}
			const usage = `{"input_tokens":12,"output_tokens":8,"cost":0.25,"vendor_usage":{"tokens":20}}`
			terminal := `{"id":"r","status":"completed","vendor":"preserved","usage":` + usage + output + `}`
			if err := state.observe([]byte(`{"type":"response.completed","response":` + terminal + `}`)); err != nil {
				t.Fatal(err)
			}
			body, err := state.unwrap()
			if err != nil {
				t.Fatal(err)
			}
			if string(state.response) != terminal {
				t.Fatal("captured terminal response mutated")
			}
			var fields map[string]jsontext.Value
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["vendor"]) != `"preserved"` || string(fields["usage"]) != usage {
				t.Fatal("terminal metadata changed")
			}
			response, err := decodeResponse(body)
			if err != nil {
				t.Fatal(err)
			}
			if output == `,"output":[`+fallbackMessage+`]` {
				if string(body) != terminal || len(response.Output) != 1 {
					t.Fatal("non-empty terminal output was not authoritative")
				}
			} else {
				if len(response.Output) != 3 {
					t.Fatalf("output = %#v", response.Output)
				}
				if !strings.Contains(string(response.Output[0].Data.(llm.Reasoning).Raw), `"vendor_field":"kept"`) {
					t.Fatal("reasoning data lost")
				}
				if message := response.Output[1].Data.(llm.Message); message.Text != "fallback" || message.Phase != "final_answer" {
					t.Fatalf("message=%#v", message)
				}
				if call := response.Output[2].Data.(llm.ToolCall); call.CallID != "call-1" || call.Arguments != `{"command":"pwd"}` {
					t.Fatalf("call=%#v", call)
				}
			}
			if response.ID != "r" || response.Stop != llm.StopComplete || response.Usage.InputTokens != 12 {
				t.Fatal("terminal status/usage lost")
			}
			again, err := state.unwrap()
			if err != nil || string(again) != string(body) {
				t.Fatal("unwrap is not stable")
			}
		})
	}
}

func TestCollectedItemsPreserveTerminalStatus(t *testing.T) {
	for _, test := range []struct {
		status, extra string
		stop          llm.StopReason
		failure       bool
	}{
		{"incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`, llm.StopMaxOutputTokens, false},
		{"failed", `,"error":{"code":"server_error","message":"failed"}`, "", true},
	} {
		var state responseState
		if err := state.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":` + fallbackMessage + `}`)); err != nil {
			t.Fatal(err)
		}
		terminal := fmt.Sprintf(`{"type":"response.%s","response":{"id":"r","status":%q,"output":[]%s}}`, test.status, test.status, test.extra)
		if err := state.observe([]byte(terminal)); err != nil {
			t.Fatal(err)
		}
		body, err := state.unwrap()
		if err != nil {
			t.Fatal(err)
		}
		response, err := decodeResponse(body)
		if err != nil || response.Stop != test.stop || (response.Failure != nil) != test.failure || len(response.Output) != 1 {
			t.Fatalf("response=%#v err=%v", response, err)
		}
	}
}
