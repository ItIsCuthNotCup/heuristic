package responsesapi

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

func TestSubmittedPrefixAndLateResultOrderOnWire(t *testing.T) {
	const reasoning = `{"id":"reasoning","type":"reasoning","summary":[{"type":"summary_text","text":"Keep working."}],"encrypted_content":"opaque"}`
	for _, test := range []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "assistant message",
			output: `[{"id":"message","type":"message","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"Waiting.","annotations":[],"logprobs":[]}]}]`,
			want:   `[{"id":"message","type":"message","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"Waiting.","annotations":[],"logprobs":[]}]}]`,
		},
		{
			name:   "reasoning and tool call",
			output: `[` + reasoning + `,{"id":"item-C","type":"function_call","call_id":"C","name":"test","arguments":"{}","status":"completed"}]`,
			want:   `[` + reasoning + `,{"id":"item-C","type":"function_call","call_id":"C","name":"test","arguments":"{}"}]`,
		},
		{name: "empty output", output: `[]`, want: `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := contextbuilder.NewBuilder()
			builder.SetModel(llm.Model{ID: "test"})
			builder.AddModelResponse(llm.Response{Output: []llm.Item{
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "A", Name: "test", Arguments: `{}`}},
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "B", Name: "test", Arguments: `{}`}},
			}})
			builder.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ""}}, true)
			builder.AddToolResult("B", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done B"}}, false)
			before, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			beforeBody, err := requestBody(before.Request, "session", nil)
			if err != nil {
				t.Fatal(err)
			}
			builder.Commit()
			builder.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done A"}}, false)
			response, err := decodeResponse([]byte(`{"id":"response","object":"response","status":"completed","output":` + test.output + `}`))
			if err != nil {
				t.Fatal(err)
			}
			builder.AddModelResponse(response)
			after, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			afterBody, err := requestBody(after.Request, "session", nil)
			if err != nil {
				t.Fatal(err)
			}
			var sent, next struct {
				Input []jsontext.Value `json:"input"`
			}
			if err := json.Unmarshal(beforeBody, &sent); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(afterBody, &next); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(next.Input[:len(sent.Input)], sent.Input) {
				t.Fatal("serialized submitted prefix changed")
			}
			var wantResponse []map[string]any
			if err := json.Unmarshal([]byte(test.want), &wantResponse); err != nil {
				t.Fatal(err)
			}
			wantTail := append(wantResponse, map[string]any{"type": "function_call_output", "call_id": "A", "output": []any{map[string]any{"type": "input_text", "text": "done A"}}})
			tail := make([]map[string]any, len(next.Input)-len(sent.Input))
			for i, item := range next.Input[len(sent.Input):] {
				if err := json.Unmarshal(item, &tail[i]); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(tail, wantTail) {
				t.Fatalf("wire tail = %#v, want %#v", tail, wantTail)
			}
		})
	}
}
