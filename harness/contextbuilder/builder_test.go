package contextbuilder

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

// withPreamble expects the items after the preamble every builder starts with.
func withPreamble(items ...llm.Item) []llm.Item {
	return append([]llm.Item{{
		Type: llm.ItemMessage,
		Data: llm.Message{Role: llm.RoleSystem, Text: preamble},
	}}, items...)
}

func TestBuilderAddsExternalInputAsUserMessage(t *testing.T) {
	payload, err := json.Marshal("Hello")
	if err != nil {
		t.Fatal(err)
	}
	current := NewBuilder()
	if err := current.AddExternalInput(inbox.Input{
		ID:      "input-1",
		Kind:    inbox.InputExternal,
		Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(llm.Item{
		Type: llm.ItemMessage,
		Data: llm.Message{Role: llm.RoleUser, Text: "Hello"},
	})
	if !reflect.DeepEqual(result.Request.Input, want) {
		t.Fatalf("input = %#v, want %#v", result.Request.Input, want)
	}
}

func TestBuilderRejectsInvalidExternalInput(t *testing.T) {
	tests := []struct {
		name  string
		input inbox.Input
		want  string
	}{
		{
			name:  "wrong kind",
			input: inbox.Input{ID: "input-1", Kind: inbox.InputControl},
			want:  `external input "input-1" has input kind "control"`,
		},
		{
			name: "invalid payload",
			input: inbox.Input{
				ID: "input-1", Kind: inbox.InputExternal, Payload: []byte(`{`),
			},
			want: `decode external input "input-1"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := NewBuilder()
			err := current.AddExternalInput(test.input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
			result, buildErr := current.Build()
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			if !reflect.DeepEqual(result.Request.Input, withPreamble()) {
				t.Fatalf("input = %#v", result.Request.Input)
			}
		})
	}
}

func TestBuilderAddsModelResponseOutput(t *testing.T) {
	output := []llm.Item{
		{
			ProviderID: "reasoning-1",
			Type:       llm.ItemReasoning,
			Data:       llm.Reasoning{Summary: []string{"Need current weather."}},
		},
		{
			ProviderID: "message-1",
			Type:       llm.ItemMessage,
			Data:       llm.Message{Role: llm.RoleAssistant, Text: "Checking."},
		},
		{
			ProviderID: "call-item-1",
			Type:       llm.ItemToolCall,
			Data: llm.ToolCall{
				CallID: "call-1", Name: "weather", Arguments: `{"city":"London"}`,
			},
		},
	}
	current := NewBuilder()
	current.AddModelResponse(llm.Response{
		ID: "response-1", Stop: llm.StopComplete, Output: output,
		Usage: llm.Usage{InputTokens: 12, OutputTokens: 8},
	})

	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := withPreamble(output...)
	if !reflect.DeepEqual(result.Request.Input, want) {
		t.Fatalf("input = %#v, want %#v", result.Request.Input, want)
	}
}

func TestBuilderBuildsRequestFromAddedValues(t *testing.T) {
	model := llm.Model{ID: "gpt-test"}
	weather := llm.Tool{
		Type: llm.ToolFunction, Name: "weather", Description: "Get weather",
	}
	reasoning := llm.Reasoning{Summary: []string{"Need current weather."}}
	call := llm.ToolCall{
		CallID: "call-1", Name: "weather", Arguments: `{"city":"London"}`,
	}

	current := NewBuilder()
	current.SetModel(model)
	current.AddTool(weather)
	current.AddReasoning(reasoning)
	current.Commit()
	current.AddModelResponse(llm.Response{Output: []llm.Item{{
		Type: llm.ItemToolCall,
		Data: call,
	}}})
	current.AddToolResult("call-1", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "completed:operation-1"}}, false)
	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}

	want := Result{Request: llm.Request{
		Model: model,
		Tools: []llm.Tool{weather},
		Input: withPreamble(
			llm.Item{Type: llm.ItemReasoning, Data: reasoning},
			llm.Item{Type: llm.ItemToolCall, Data: call},
			llm.Item{
				Type: llm.ItemToolResult,
				Data: llm.ToolResult{CallID: "call-1", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "completed:operation-1"}}},
			},
		),
	}}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("result = %#v\nwant %#v", result, want)
	}
}

func TestBuilderPreservesToolResultPayload(t *testing.T) {
	output := []llm.ToolResultOutput{
		{Kind: llm.ToolResultText, Value: strings.Repeat("界", 4_001) + string([]byte{0xff})},
		{Kind: llm.ToolResultImage, Value: "data:image/png;base64,aGVsbG8="},
		{Kind: llm.ToolResultText, Value: "Dimensions: 2000x1500"},
	}
	current := NewBuilder()
	current.AddToolResult("call-1", output, false)

	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	got := result.Request.Input[1].Data.(llm.ToolResult)
	if !reflect.DeepEqual(got.Output, output) {
		t.Fatalf("output = %#v, want unchanged output", got.Output)
	}
	if len(result.Report.Changes) != 0 {
		t.Fatalf("changes = %#v", result.Report.Changes)
	}
}

func TestBuilderAppendsValidationErrorToolResult(t *testing.T) {
	current := NewBuilder()
	current.AddToolResult("call-1", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "error:invalid arguments"}}, false)

	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := llm.ToolResult{CallID: "call-1", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "error:invalid arguments"}}}
	if got := result.Request.Input[1].Data.(llm.ToolResult); !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func TestBuilderRemovesOnlyStagedRunningResultsForUpdatedCall(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "completed"
		output := "done A"
		if running {
			name = "still running"
			output = ToolCallRunningPayload
		}
		t.Run(name, func(t *testing.T) {
			current := NewBuilder()
			current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ""}}, true)
			current.Commit()
			current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ""}}, true)
			current.AddToolResult("B", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ""}}, true)
			current.AddToolResult("C", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done C"}}, false)
			if err := current.AddExternalInput(inbox.Input{ID: "input", Kind: inbox.InputExternal, Payload: []byte(`"continue"`)}); err != nil {
				t.Fatal(err)
			}
			before, err := current.Build()
			if err != nil {
				t.Fatal(err)
			}
			original := append([]llm.Item(nil), before.Request.Input...)
			current.AddToolResult("A", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done A"}}, running)
			result, err := current.Build()
			if err != nil {
				t.Fatal(err)
			}
			want := withPreamble(
				llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "A", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ToolCallRunningPayload}}}},
				llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "B", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ToolCallRunningPayload}}}},
				llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "C", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done C"}}}},
				llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "continue"}},
				llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "A", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: output}}}},
			)
			if !reflect.DeepEqual(result.Request.Input, want) {
				t.Fatalf("input = %#v, want %#v", result.Request.Input, want)
			}
			if !reflect.DeepEqual(before.Request.Input, original) {
				t.Fatal("updating a staged result mutated a previously built request")
			}
		})
	}
}

func TestBuilderLeadsSystemPromptWithPreamble(t *testing.T) {
	payload, err := json.Marshal("hello")
	if err != nil {
		t.Fatal(err)
	}
	current := NewBuilder()
	current.SetSystemPrompt("Be concise.")
	if err := current.AddExternalInput(inbox.Input{
		ID: "input-1", Kind: inbox.InputExternal, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{
			Role: llm.RoleSystem, Text: preamble + "\n\nBe concise.",
		}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hello"}},
	}
	if !reflect.DeepEqual(result.Request.Input, want) {
		t.Fatalf("input = %#v, want %#v", result.Request.Input, want)
	}
}

func TestBuilderAppendsSkillsToPreamble(t *testing.T) {
	skills := []tool.Skill{
		{
			Name:        "go-review",
			Description: `Review <Go> & "tests"`,
			Path:        "/skills/reviewer's/SKILL.md",
		},
		{
			Name:        "documents",
			Description: "Edit documents",
			Path:        "/skills/documents/SKILL.md",
		},
	}
	current := NewBuilder(skills...)
	skills[0].Name = "changed"
	current.SetSystemPrompt("Be concise.")

	result, err := current.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := preamble + `

The following skills provide specialized instructions for specific tasks.
Use SkillUse to load a skill's file when the task matches its description.
When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool calls.

<available_skills><skill><name>go-review</name><description>Review &lt;Go&gt; &amp; &#34;tests&#34;</description><location>/skills/reviewer&#39;s/SKILL.md</location></skill><skill><name>documents</name><description>Edit documents</description><location>/skills/documents/SKILL.md</location></skill></available_skills>

Be concise.`
	if got := result.Request.Input[0].Data.(llm.Message).Text; got != want {
		t.Fatalf("system prompt = %q, want %q", got, want)
	}
}
