package metacog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

// noulStub answers Noul from a fixed score or a per-subject map.
type noulStub struct {
	score  float64
	byText map[string]float64
	err    error
	mu     sync.Mutex
	calls  int
}

func (n *noulStub) Noul(ctx context.Context, problem, subject, instructions string) (float64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	if n.err != nil {
		return 0, n.err
	}
	if score, ok := n.byText[subject]; ok {
		return score, nil
	}
	return n.score, nil
}

func (n *noulStub) Score(ctx context.Context, problem string, candidates []string) ([]float64, error) {
	out := make([]float64, len(candidates))
	for i := range out {
		out[i] = 0.99 // confident: no branching in these tests
	}
	return out, nil
}

// captureInner records the request it was called with.
type captureInner struct {
	effort llm.ReasoningEffort
	input  []llm.Item
}

func (c *captureInner) Respond(ctx context.Context, req llm.Request, opts llm.RequestOptions) (llm.Response, error) {
	c.effort = req.Model.ReasoningEffort
	c.input = req.Input
	return messageResponse("r", "Answer: 4"), nil
}

func userRequest(text string, effort llm.ReasoningEffort) llm.Request {
	return llm.Request{
		Model: llm.Model{ID: "m", ReasoningEffort: effort},
		Input: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "sys"}},
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}},
		},
	}
}

func TestRouterLowersEffortForEasyRequests(t *testing.T) {
	inner := &captureInner{}
	adapter := New(inner, Config{Mode: ModeOff, Router: &noulStub{score: 0.1}})
	if _, err := adapter.Respond(context.Background(), userRequest("what is 2+2?", llm.ReasoningEffortHigh), llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if inner.effort != llm.ReasoningEffortLow {
		t.Fatalf("effort = %q, want low", inner.effort)
	}
}

func TestRouterKeepsConfiguredEffortForHardRequests(t *testing.T) {
	inner := &captureInner{}
	adapter := New(inner, Config{Mode: ModeOff, Router: &noulStub{score: 0.9}})
	if _, err := adapter.Respond(context.Background(), userRequest("prove the invariant", llm.ReasoningEffortHigh), llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if inner.effort != llm.ReasoningEffortHigh {
		t.Fatalf("effort = %q, want high", inner.effort)
	}
}

func TestRouterJudgesOncePerUserMessage(t *testing.T) {
	router := &noulStub{score: 0.1}
	adapter := New(&captureInner{}, Config{Mode: ModeOff, Router: router})
	for range 3 {
		if _, err := adapter.Respond(context.Background(), userRequest("same question", llm.ReasoningEffortHigh), llm.RequestOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if router.calls != 1 {
		t.Fatalf("router calls = %d, want 1", router.calls)
	}
}

func bigToolResult(size int) llm.Item {
	return llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{
		CallID: "c", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("x", size)}},
	}}
}

func prunedRequest(chunkScore float64) (llm.Request, *noulStub) {
	router := &noulStub{byText: map[string]float64{}}
	items := []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "sys"}},
		bigToolResult(60000),
	}
	for i := 0; i < 12; i++ {
		items = append(items, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "question"}})
	}
	req := llm.Request{Model: llm.Model{ID: "m", ReasoningEffort: llm.ReasoningEffortHigh}, Input: items}
	// Route all subjects at the same score so effort and pruning share it.
	router.score = chunkScore
	return req, router
}

func TestPruneStubsIrrelevantOldOutputs(t *testing.T) {
	inner := &captureInner{}
	req, router := prunedRequest(0.1)
	adapter := New(inner, Config{Mode: ModeOff, Router: router})
	if _, err := adapter.Respond(context.Background(), req, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	result, ok := inner.input[1].Data.(llm.ToolResult)
	if !ok {
		t.Fatalf("item 1 type changed: %T", inner.input[1].Data)
	}
	if !strings.Contains(result.Output[0].Value, "omitted") {
		t.Fatalf("old tool output was not stubbed: %.80s", result.Output[0].Value)
	}
}

func TestPruneKeepsRelevantOutputs(t *testing.T) {
	inner := &captureInner{}
	req, router := prunedRequest(0.9)
	adapter := New(inner, Config{Mode: ModeOff, Router: router})
	if _, err := adapter.Respond(context.Background(), req, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	result := inner.input[1].Data.(llm.ToolResult)
	if len(result.Output[0].Value) != 60000 {
		t.Fatalf("relevant output was stubbed (len %d)", len(result.Output[0].Value))
	}
}

func TestPruneKeepsEverythingWhenJudgeFails(t *testing.T) {
	inner := &captureInner{}
	req, router := prunedRequest(0)
	router.err = errors.New("down")
	adapter := New(inner, Config{Mode: ModeOff, Router: router})
	if _, err := adapter.Respond(context.Background(), req, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	result := inner.input[1].Data.(llm.ToolResult)
	if len(result.Output[0].Value) != 60000 {
		t.Fatal("output was stubbed despite judge failure")
	}
}
