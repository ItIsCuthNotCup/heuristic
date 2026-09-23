package ollama

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

func TestLiveToolRoundTrip(t *testing.T) {
	model := os.Getenv("OLLAMA_TEST_MODEL")
	if model == "" {
		t.Skip("set OLLAMA_TEST_MODEL to an installed tool-capable model")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	client, err := NewClient(Config{BaseURL: os.Getenv("OLLAMA_TEST_BASE_URL"), MaxAttempts: new(1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := llm.Request{
		Model: llm.Model{ID: model, ReasoningEffort: llm.ReasoningEffortMedium},
		Input: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "Call read_secret exactly once to answer the user. Once you receive its result, reply with the secret and no other text."}},
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "What is the secret?"}},
		},
		Tools: []llm.Tool{{Type: llm.ToolFunction, Name: "read_secret", Description: "Read the secret.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}},
	}
	response, err := client.Respond(ctx, request, llm.RequestOptions{})
	if err != nil || response.Failure != nil {
		t.Fatalf("first response: %v, failure: %+v", err, response.Failure)
	}
	request.Input = append(request.Input, response.Output...)
	calls := 0
	const secret = "orchid-7319"
	for _, item := range response.Output {
		if call, ok := item.Data.(llm.ToolCall); ok {
			if call.Name != "read_secret" || call.CallID == "" {
				t.Fatalf("unexpected call: %+v", call)
			}
			calls++
			request.Input = append(request.Input, llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: secret}}}})
		}
	}
	if calls != 1 {
		t.Fatalf("got %d tool calls, want 1", calls)
	}
	response, err = client.Respond(ctx, request, llm.RequestOptions{})
	if err != nil || response.Failure != nil || response.Stop != llm.StopComplete {
		t.Fatalf("tool result response: %v, failure: %+v, stop: %s", err, response.Failure, response.Stop)
	}
	for _, item := range response.Output {
		if message, ok := item.Data.(llm.Message); ok && strings.Contains(message.Text, secret) {
			return
		}
	}
	t.Fatal("model did not return the tool result")
}
