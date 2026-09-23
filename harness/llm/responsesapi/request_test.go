package responsesapi

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

func TestRequestInputItemReplaysToolCallArguments(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		valid     bool
	}{
		{name: "empty object", arguments: `{}`, valid: true},
		{name: "whitespace and escapes", arguments: " \n{\"command\": \"echo \\u0061\"}\t", valid: true},
		{name: "schema error", arguments: `{"command":42}`, valid: true},
		{name: "nested values", arguments: `{"array":[null,true,1e1000],"object":{}}`, valid: true},
		{name: "truncated string", arguments: `{"command":"apt-get install -y r-base`},
		{name: "truncated object", arguments: `{"command":"pwd"`},
		{name: "trailing data", arguments: `{} []`},
		{name: "duplicate names", arguments: `{"command":"pwd","command":"ls"}`},
		{name: "null", arguments: `null`},
		{name: "null with whitespace", arguments: " \nnull\t"},
		{name: "array", arguments: `[]`},
		{name: "string", arguments: `"pwd"`},
		{name: "number", arguments: `42`},
		{name: "boolean", arguments: `true`},
		{name: "empty", arguments: ""},
		{name: "whitespace", arguments: " \n\t"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := llm.ToolCall{CallID: "call-1", Name: "Bash", Arguments: test.arguments}
			source := llm.Item{ProviderID: "function-1", Type: llm.ItemToolCall, Data: call}
			item, err := requestInputItem(source)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Type != "function_call" || wire.ID != source.ProviderID || wire.CallID != call.CallID || wire.Name != call.Name {
				t.Fatalf("call identity changed: %+v", wire)
			}
			var object map[string]jsontext.Value
			if err := json.Unmarshal([]byte(wire.Arguments), &object); err != nil || object == nil {
				t.Fatalf("replayed arguments = %q, want a JSON object: %v", wire.Arguments, err)
			}
			if test.valid {
				if wire.Arguments != test.arguments {
					t.Fatalf("valid arguments changed: %q", wire.Arguments)
				}
			} else {
				var raw string
				if err := json.Unmarshal(object["invalid_arguments"], &raw); err != nil {
					t.Fatal(err)
				}
				if len(object) != 1 || raw != test.arguments {
					t.Fatalf("invalid arguments were not preserved: %q", wire.Arguments)
				}
			}
			if source.Data.(llm.ToolCall) != call {
				t.Fatal("encoding changed the original tool call")
			}
		})
	}
}

func TestRequestBodyReportsToolArgumentEncodingError(t *testing.T) {
	_, err := requestBody(llm.Request{Input: []llm.Item{{
		Type: llm.ItemToolCall,
		Data: llm.ToolCall{CallID: "call-1", Name: "Bash", Arguments: "\xff"},
	}}}, "", nil)
	if err == nil || !strings.Contains(err.Error(), `input item 0: encode tool call "call-1" arguments:`) {
		t.Fatalf("error = %v, want the argument encoding error with call context", err)
	}
}

func TestRequestBodyRejectsInvalidUTF8ToolResult(t *testing.T) {
	body, err := requestBody(llm.Request{Input: []llm.Item{{
		Type: llm.ItemToolResult,
		Data: llm.ToolResult{
			CallID: "call-1",
			Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "bad\xffbyte"}},
		},
	}}}, "", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "input item 0: ") ||
		!strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("error = %v, want an invalid UTF-8 error with input item context", err)
	}
	var syntaxError *jsontext.SyntacticError
	if !errors.As(err, &syntaxError) || syntaxError.JSONPointer != "/text" {
		t.Fatalf("error = %v, want a JSON encoding error at /text", err)
	}
	if body != nil {
		t.Fatalf("body = %q, want no request body for invalid UTF-8", body)
	}
}

func TestRequestBodyEncodesToolResultOutputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		output []llm.ToolResultOutput
		want   string
	}{
		{name: "text", output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done"}}, want: `[{"type":"input_text","text":"done"}]`},
		{name: "empty text", output: []llm.ToolResultOutput{{Kind: llm.ToolResultText}}, want: `[{"type":"input_text","text":""}]`},
		{name: "no outputs", want: `[]`},
		{name: "image URL", output: []llm.ToolResultOutput{{Kind: llm.ToolResultImage, Value: "https://example.com/image.png"}}, want: `[{"type":"input_image","image_url":"https://example.com/image.png"}]`},
		{name: "image data URL", output: []llm.ToolResultOutput{{Kind: llm.ToolResultImage, Value: "data:image/png;base64,aGVsbG8="}}, want: `[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]`},
		{
			name: "mixed content in order",
			output: []llm.ToolResultOutput{
				{Kind: llm.ToolResultText, Value: "Original dimensions: 4000x3000"},
				{Kind: llm.ToolResultImage, Value: "data:image/png;base64,Zmlyc3Q="},
				{Kind: llm.ToolResultText, Value: "Resized dimensions: 2000x1500"},
				{Kind: llm.ToolResultImage, Value: "https://example.com/second.png"},
			},
			want: `[{"type":"input_text","text":"Original dimensions: 4000x3000"},{"type":"input_image","image_url":"data:image/png;base64,Zmlyc3Q="},{"type":"input_text","text":"Resized dimensions: 2000x1500"},{"type":"input_image","image_url":"https://example.com/second.png"}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := requestBody(llm.Request{
				Model: llm.Model{ID: "gpt-test"},
				Input: []llm.Item{{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-1", Output: test.output}}},
			}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Input []map[string]any `json:"input"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			var output []any
			if err := json.Unmarshal([]byte(test.want), &output); err != nil {
				t.Fatal(err)
			}
			want := []map[string]any{{"type": "function_call_output", "call_id": "call-1", "output": output}}
			if !reflect.DeepEqual(request.Input, want) {
				t.Fatalf("input = %#v, want %#v", request.Input, want)
			}
		})
	}
}

func TestRequestBodyRejectsInvalidItemPayloads(t *testing.T) {
	tests := []struct {
		name string
		item llm.Item
		want string
	}{
		{
			name: "message",
			item: llm.Item{Type: llm.ItemMessage},
			want: "input item 0: message item data must be llm.Message, got <nil>",
		},
		{
			name: "tool call",
			item: llm.Item{Type: llm.ItemToolCall},
			want: "input item 0: tool_call item data must be llm.ToolCall, got <nil>",
		},
		{
			name: "tool result",
			item: llm.Item{Type: llm.ItemToolResult},
			want: "input item 0: tool_result item data must be llm.ToolResult, got <nil>",
		},
		{
			name: "unset tool result kind",
			item: llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{Output: []llm.ToolResultOutput{{Value: "done"}}}},
			want: `input item 0: unsupported tool result kind ""`,
		},
		{
			name: "unsupported tool result kind",
			item: llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{Output: []llm.ToolResultOutput{{Kind: "unknown"}}}},
			want: `input item 0: unsupported tool result kind "unknown"`,
		},
		{
			name: "reasoning",
			item: llm.Item{Type: llm.ItemReasoning},
			want: "input item 0: reasoning item data must be llm.Reasoning, got <nil>",
		},
		{
			name: "reasoning without raw",
			item: llm.Item{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"thought"}}},
			want: "input item 0: reasoning item must carry the provider item in Raw",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := requestBody(llm.Request{Input: []llm.Item{test.item}}, "", nil)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRequestInputItemPreservesIdentifiedInputMessageRoles(t *testing.T) {
	roles := []llm.Role{llm.RoleUser, llm.RoleSystem}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			item, err := requestInputItem(llm.Item{
				ProviderID: "message-1",
				Type:       llm.ItemMessage,
				Data:       llm.Message{Role: role, Text: "hello"},
			})
			if err != nil {
				t.Fatalf("convert item: %v", err)
			}
			body, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal item: %v", err)
			}
			var got struct {
				Content []struct {
					Text string `json:"text"`
					Type string `json:"type"`
				} `json:"content"`
				ID     *string `json:"id"`
				Role   string  `json:"role"`
				Status string  `json:"status"`
				Type   string  `json:"type"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal item: %v", err)
			}
			if got.ID != nil || got.Role != string(role) || got.Status != "" || got.Type != "message" {
				t.Fatalf("item = %#v", got)
			}
			if len(got.Content) != 1 || got.Content[0].Text != "hello" || got.Content[0].Type != "input_text" {
				t.Fatalf("content = %#v", got.Content)
			}
		})
	}
}

func TestRequestInputItemReplaysRawReasoningVerbatim(t *testing.T) {
	raw := `{"id":"reasoning-1","type":"reasoning","status":"completed","summary":[],` +
		`"content":[{"type":"reasoning_text","text":"verbatim"}],"encrypted_content":"opaque"}`
	item, err := requestInputItem(llm.Item{
		ProviderID: "ignored",
		Type:       llm.ItemReasoning,
		Data: llm.Reasoning{
			Summary: []string{"stale summary"},
			Raw:     jsontext.Value(raw),
		},
	})
	if err != nil {
		t.Fatalf("convert item: %v", err)
	}
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("item = %#v, want %#v", got, want)
	}
}

func TestRequestBodyOmitsUnsetMaxOutputTokens(t *testing.T) {
	body, err := requestBody(validRequest(), "", nil)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if _, exists := request["max_output_tokens"]; exists {
		t.Fatalf("request = %#v", request)
	}
}

func TestRequestBodyEncodesReasoningEffort(t *testing.T) {
	body, err := requestBody(llm.Request{
		Model: llm.Model{ID: "gpt-test", ReasoningEffort: llm.ReasoningEffortHigh},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Reasoning struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.Reasoning.Effort != "high" || request.Reasoning.Summary != "auto" {
		t.Fatalf("reasoning = %#v", request.Reasoning)
	}
}

func TestRequestBodyRejectsUnsupportedReasoningEffort(t *testing.T) {
	_, err := requestBody(llm.Request{
		Model: llm.Model{ID: "gpt-test", ReasoningEffort: "maximum"},
	}, "", nil)
	if err == nil || err.Error() != `unsupported reasoning effort "maximum"` {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestBodyEncodesHostedWebSearch(t *testing.T) {
	body, err := requestBody(llm.Request{
		Model: llm.Model{ID: "gpt-test"},
		Input: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleUser, Text: "latest news"},
		}},
		Tools: []llm.Tool{{Type: llm.ToolHosted, Name: "web_search"}},
	}, "", nil)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var request struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(request.Tools) != 1 || request.Tools[0].Type != "web_search" {
		t.Fatalf("tools = %#v", request.Tools)
	}
}

func TestRequestBodyRejectsUnsupportedHostedTool(t *testing.T) {
	_, err := requestBody(llm.Request{
		Tools: []llm.Tool{{Type: llm.ToolHosted, Name: "unknown"}},
	}, "", nil)
	if err == nil || err.Error() != `tool 0: unsupported hosted tool name "unknown"` {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestBodyRejectsUnsupportedToolType(t *testing.T) {
	_, err := requestBody(llm.Request{
		Tools: []llm.Tool{{Name: "weather"}},
	}, "", nil)
	if err == nil || err.Error() != `tool 0: unsupported tool type ""` {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestBodyIsByteStableAcrossEncodings(t *testing.T) {
	request := validRequest()
	request.Tools = []llm.Tool{{
		Type:        llm.ToolFunction,
		Name:        "Bash",
		Description: "Execute a shell command.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":          map[string]any{"type": "string", "description": "The shell command."},
				"max_output_chars": map[string]any{"type": "integer", "description": "Inline budget."},
				"timeout_seconds":  map[string]any{"type": "integer", "description": "Deadline."},
			},
			"required": []any{"command"},
		},
	}}
	first, err := requestBody(request, "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 64 {
		body, err := requestBody(request, "session-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(first) {
			t.Fatalf("encoding %d differs:\n%s\n%s", attempt, first, body)
		}
	}
}

func TestRequestBodyEncodesMaxReasoningEffort(t *testing.T) {
	body, err := requestBody(llm.Request{
		Model: llm.Model{ID: "gpt-test", ReasoningEffort: llm.ReasoningEffortMax},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.Reasoning.Effort != "max" {
		t.Fatalf("reasoning = %#v", request.Reasoning)
	}
}

func TestRequestBodyMergesExtensions(t *testing.T) {
	body, err := requestBody(validRequest(), "", map[string]jsontext.Value{
		"cache_control": jsontext.Value(`{"type":"ephemeral"}`),
		"provider":      jsontext.Value(`{"only":["anthropic"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Model        string `json:"model"`
		Stream       bool   `json:"stream"`
		CacheControl struct {
			Type string `json:"type"`
		} `json:"cache_control"`
		Provider struct {
			Only []string `json:"only"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.Model != "gpt-test" || !request.Stream || request.CacheControl.Type != "ephemeral" || !reflect.DeepEqual(request.Provider.Only, []string{"anthropic"}) {
		t.Fatalf("request = %s", body)
	}
}

func TestRequestBodyRejectsExtensionsOverridingStandardFields(t *testing.T) {
	for name, extensions := range map[string]map[string]jsontext.Value{
		"standard field": {"model": jsontext.Value(`"other"`)},
		"invalid JSON":   {"provider": jsontext.Value(`{"only":`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := requestBody(validRequest(), "", extensions); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
