package responsesapi

import (
	"encoding/json/v2"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

func TestResponseConvertsWebSearchResponse(t *testing.T) {
	response, err := decodeResponse([]byte(`{
		"id":"response-1",
		"status":"completed",
		"output":[
			{
				"id":"reasoning-1",
				"type":"reasoning",
				"summary":[],
				"encrypted_content":"opaque"
			},
			{
				"id":"search-1",
				"type":"web_search_call",
				"status":"completed",
				"action":{"type":"search","query":"latest news"}
			},
			{
				"id":"message-1",
				"type":"message",
				"role":"assistant",
				"status":"completed",
				"content":[{
					"type":"output_text",
					"text":"Current answer.",
					"annotations":[],
					"logprobs":[]
				}]
			}
		]
	}`))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Output) != 2 {
		t.Fatalf("output = %#v", response.Output)
	}
	reasoning, ok := response.Output[0].Data.(llm.Reasoning)
	if !ok {
		t.Fatalf("reasoning = %#v", response.Output[0])
	}
	var raw map[string]any
	if err := json.Unmarshal(reasoning.Raw, &raw); err != nil {
		t.Fatalf("decode raw reasoning: %v", err)
	}
	if raw["encrypted_content"] != "opaque" || raw["id"] != "reasoning-1" {
		t.Fatalf("raw reasoning = %#v", raw)
	}
	message, ok := response.Output[1].Data.(llm.Message)
	if !ok || message.Text != "Current answer." {
		t.Fatalf("message = %#v", response.Output[1].Data)
	}
}

func TestResponseAllowsToolCallWithoutStatus(t *testing.T) {
	response, err := decodeResponse([]byte(`{
		"id":"response-1",
		"status":"completed",
		"output":[{
			"id":"function-1",
			"type":"function_call",
			"call_id":"call-1",
			"name":"weather",
			"arguments":"{}"
		}]
	}`))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Output) != 1 {
		t.Fatalf("output = %#v", response.Output)
	}
}

func TestResponseIgnoresUnfinishedToolCall(t *testing.T) {
	response, err := decodeResponse([]byte(`{
		"id":"response-1",
		"status":"completed",
		"output":[{
			"id":"function-1",
			"type":"function_call",
			"call_id":"call-1",
			"name":"weather",
			"arguments":"{}",
			"status":"incomplete"
		}]
	}`))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Output) != 0 {
		t.Fatalf("output = %#v", response.Output)
	}
}

func TestResponsePreservesRawUsage(t *testing.T) {
	raw := `{"input_tokens":7,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":12,"cost":0.0042,"provider_usage":{"prompt_tokens":7}}`
	response, err := decodeResponse([]byte(`{"id":"response-1","status":"completed","output":[],"usage":` + raw + `}`))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Usage.InputTokens != 7 || response.Usage.CachedInputTokens != 3 ||
		response.Usage.CacheWriteInputTokens != 2 || response.Usage.OutputTokens != 5 ||
		response.Usage.ReasoningTokens != 4 {
		t.Fatalf("usage = %#v", response.Usage)
	}
	if string(response.Usage.Raw) != raw {
		t.Fatalf("raw usage = %s, want %s", response.Usage.Raw, raw)
	}
}
