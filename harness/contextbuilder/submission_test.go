package contextbuilder

import (
	"reflect"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

func TestBuilderPlacesResponseBeforeUnsubmittedInputs(t *testing.T) {
	for _, emptyResponse := range []bool{false, true} {
		name := "full response"
		if emptyResponse {
			name = "empty response"
		}
		t.Run(name, func(t *testing.T) {
			current := NewBuilder()
			if err := current.AddExternalInput(inbox.Input{ID: "first", Kind: inbox.InputExternal, Payload: []byte(`"start"`)}); err != nil {
				t.Fatal(err)
			}
			current.Commit()
			current.AddModelResponse(llm.Response{Output: []llm.Item{
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "A", Name: "test", Arguments: `{}`}},
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "B", Name: "test", Arguments: `{}`}},
			}})
			current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ""}}, true)
			current.AddToolResult("B", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done B"}}, false)
			sent, err := current.Build()
			if err != nil {
				t.Fatal(err)
			}
			original := append([]llm.Item(nil), sent.Request.Input...)
			current.Commit()
			current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ""}}, true)
			current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done A"}}, false)
			if err := current.AddExternalInput(inbox.Input{ID: "second", Kind: inbox.InputExternal, Payload: []byte(`"continue"`)}); err != nil {
				t.Fatal(err)
			}
			current.AddControlMessage(inbox.ControlMessage{Mode: inbox.Heartbeat, Reason: "heartbeat"})
			current.AddReasoning(llm.Reasoning{Summary: []string{"added reasoning"}})
			preview, err := current.Build()
			if err != nil {
				t.Fatal(err)
			}
			suffix := []llm.Item{
				{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "A", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done A"}}}},
				{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "continue"}},
				{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "heartbeat"}},
				{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"added reasoning"}}},
			}
			wantPreview := append(append([]llm.Item(nil), original...), suffix...)
			if !reflect.DeepEqual(preview.Request.Input, wantPreview) {
				t.Fatalf("preview input = %#v, want %#v", preview.Request.Input, wantPreview)
			}
			output := []llm.Item{
				{ProviderID: "reasoning", Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"response reasoning"}}},
				{ProviderID: "message", Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "working"}},
				{ProviderID: "call", Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "C", Name: "test", Arguments: `{}`}},
			}
			if emptyResponse {
				output = nil
			}
			current.AddModelResponse(llm.Response{Output: output})
			after, err := current.Build()
			if err != nil {
				t.Fatal(err)
			}
			want := append(append(append([]llm.Item(nil), original...), output...), suffix...)
			if !reflect.DeepEqual(after.Request.Input, want) {
				t.Fatalf("response order = %#v, want %#v", after.Request.Input, want)
			}
			if !reflect.DeepEqual(sent.Request.Input, original) || !reflect.DeepEqual(preview.Request.Input, wantPreview) {
				t.Fatal("previously built requests were mutated")
			}
			current.Commit()
			current.Commit()
			current.AddToolResult("C", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done C"}}, false)
			nextOutput := llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "next response"}}
			current.AddModelResponse(llm.Response{Output: []llm.Item{nextOutput}})
			next, err := current.Build()
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, nextOutput, llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "C", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done C"}}}})
			if !reflect.DeepEqual(next.Request.Input, want) {
				t.Fatalf("next submission order = %#v, want %#v", next.Request.Input, want)
			}
			next.Request.Input[1] = llm.Item{}
			unchanged, err := current.Build()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(unchanged.Request.Input, want) {
				t.Fatal("mutating a built request changed builder state")
			}
		})
	}
}

func TestBuilderSubmitsAlreadyCompletedResultsWithoutDelay(t *testing.T) {
	current := NewBuilder()
	current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ""}}, true)
	current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done A"}}, false)
	before, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(
		llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "A", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done A"}}}},
	)
	if !reflect.DeepEqual(before.Request.Input, want) {
		t.Fatalf("completion not included before submission: %#v", before.Request.Input)
	}
	current.Commit()
	response := llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "done"}}
	current.AddModelResponse(llm.Response{Output: []llm.Item{response}})
	after, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, response)
	if !reflect.DeepEqual(after.Request.Input, want) {
		t.Fatalf("submitted completion moved behind response: %#v", after.Request.Input)
	}
}

func TestBuilderUpdatesSystemPromptWithoutChangingConversation(t *testing.T) {
	current := NewBuilder()
	current.SetSystemPrompt("First instructions.")
	if err := current.AddExternalInput(inbox.Input{ID: "input", Kind: inbox.InputExternal, Payload: []byte(`"start"`)}); err != nil {
		t.Fatal(err)
	}
	current.Commit()
	current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done A"}}, false)
	before, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"Updated instructions.", ""} {
		current.SetSystemPrompt(prompt)
		after, err := current.Build()
		if err != nil {
			t.Fatal(err)
		}
		want := preamble
		if prompt != "" {
			want += "\n\n" + prompt
		}
		if got := after.Request.Input[0].Data.(llm.Message); got.Role != llm.RoleSystem || got.Text != want {
			t.Fatalf("system message = %#v, want %q", got, want)
		}
		if !reflect.DeepEqual(after.Request.Input[1:], before.Request.Input[1:]) {
			t.Fatal("updating the system prompt changed the conversation")
		}
		if got := before.Request.Input[0].Data.(llm.Message).Text; got != preamble+"\n\nFirst instructions." {
			t.Fatal("updating the system prompt mutated a previously built request")
		}
	}
}
