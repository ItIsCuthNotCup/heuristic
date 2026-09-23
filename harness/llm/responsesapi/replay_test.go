package responsesapi

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool/bash"
)

func TestRequestBodyReplaysRejectedToolCallFromHistory(t *testing.T) {
	response, err := decodeResponse([]byte(`{
		"id":"response-1","status":"completed","output":[{
			"type":"function_call","id":"function-1","call_id":"call-1",
			"name":"Bash","arguments":"{\"command\":\"apt-get install -y r-base"
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	history, err := json.Marshal(sessionstore.Item{
		Kind: sessionstore.ItemModelResponse,
		Data: sessionstore.ModelResponse{TurnID: "turn-1", Response: response},
	})
	if err != nil {
		t.Fatal(err)
	}
	var restored sessionstore.Item
	if err := json.Unmarshal(history, &restored); err != nil {
		t.Fatal(err)
	}
	response = restored.Data.(sessionstore.ModelResponse).Response
	call := response.Output[0].Data.(llm.ToolCall)
	translator := bash.New(bash.Config{})
	toolContext := &replayToolContext{}
	status := translator.Translate(toolContext, call)
	if !strings.Contains(status.Error, "decode Bash arguments:") || toolContext.submissions != 0 {
		t.Fatalf("malformed call was not rejected before submission: %+v, submissions = %d", status, toolContext.submissions)
	}
	result, err := translator.TranslateResult(call.CallID, status, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder := contextbuilder.NewBuilder()
	builder.AddModelResponse(response)
	builder.AddToolResult(result.CallID, result.Output, false)
	builder.Commit()
	validCall := llm.ToolCall{CallID: "call-2", Name: "Search", Arguments: ` {"query":"weather"} `}
	builder.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: validCall}}})
	builder.AddToolResult(validCall.CallID, []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "found"}}, false)
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	body, err := requestBody(built.Request, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	type wireContent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type wireItem struct {
		Type      string        `json:"type"`
		CallID    string        `json:"call_id"`
		Name      string        `json:"name"`
		Arguments string        `json:"arguments"`
		Output    []wireContent `json:"output"`
	}
	var wire struct {
		Input []wireItem `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 5 {
		t.Fatalf("input = %+v, want system message and two call/result pairs", wire.Input)
	}
	var wrapped map[string]string
	if err := json.Unmarshal([]byte(wire.Input[1].Arguments), &wrapped); err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 1 || wrapped["invalid_arguments"] != call.Arguments {
		t.Fatalf("replayed arguments = %q", wire.Input[1].Arguments)
	}
	want := []wireItem{
		{Type: "function_call", CallID: call.CallID, Name: call.Name, Arguments: wire.Input[1].Arguments},
		{Type: "function_call_output", CallID: call.CallID, Output: []wireContent{{Type: "input_text", Text: result.Output[0].Value}}},
		{Type: "function_call", CallID: validCall.CallID, Name: validCall.Name, Arguments: validCall.Arguments},
		{Type: "function_call_output", CallID: validCall.CallID, Output: []wireContent{{Type: "input_text", Text: "found"}}},
	}
	if !reflect.DeepEqual(wire.Input[1:], want) {
		t.Fatalf("replayed calls and results = %+v, want %+v", wire.Input[1:], want)
	}
	unchanged, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(history) {
		t.Fatal("replay changed persisted history")
	}
}

type replayToolContext struct {
	submissions int
}

func (ctx *replayToolContext) Submit(operation.Spec) operation.ID {
	ctx.submissions++
	return "unexpected-operation"
}
