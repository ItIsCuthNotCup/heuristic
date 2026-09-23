package contextbuilder

import (
	"reflect"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

func TestBuilderControlMessages(t *testing.T) {
	for _, test := range []struct {
		mode inbox.ControlMode
		role llm.Role
		text string
	}{
		{mode: inbox.Heartbeat, role: llm.RoleUser, text: "requested"},
		{mode: inbox.StopHard},
		{mode: inbox.StopWhenIdle},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			builder := NewBuilder()
			builder.AddControlMessage(inbox.ControlMessage{Mode: test.mode, Reason: "requested"})
			result, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			input := result.Request.Input[1:]
			if test.text == "" {
				if len(input) != 0 {
					t.Fatal("control added a model message")
				}
				return
			}
			if len(input) != 1 || input[0].Type != llm.ItemMessage {
				t.Fatalf("input = %#v", input)
			}
			message := input[0].Data.(llm.Message)
			if message.Role != test.role || message.Text != test.text {
				t.Fatalf("message = %#v", message)
			}
		})
	}
}

func TestBuilderSettingsOnlyChangeEffortInSubsequentRequests(t *testing.T) {
	limit := int64(123)
	builder := NewBuilder()
	builder.SetModel(llm.Model{ID: "model", MaxOutputTokens: &limit, ReasoningEffort: llm.ReasoningEffortHigh})
	builder.AddTool(llm.Tool{Name: "tool"})
	builder.AddReasoning(llm.Reasoning{Summary: []string{"thought"}})
	original, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	builder.AddControlMessage(inbox.ControlMessage{
		Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: llm.ReasoningEffortLow},
	})
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := original
	want.Request.Model.ReasoningEffort = llm.ReasoningEffortLow
	if !reflect.DeepEqual(built, want) {
		t.Fatalf("settings changed other request content: got %#v, want %#v", built, want)
	}
	if original.Request.Model.ReasoningEffort != llm.ReasoningEffortHigh {
		t.Fatal("settings mutated an already built request")
	}
}
