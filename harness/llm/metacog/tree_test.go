package metacog

import (
	"context"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

// TestTreeExpandsWinningSketch runs the full loop: unsure first answer →
// level-1 sketches → level-2 derivation (best l1 score below confidence)
// → expand → expanded beats greedy.
func TestTreeExpandsWinningSketch(t *testing.T) {
	inner := &recordingInner{fakeInner: fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy answer"),
		messageResponse("s1", "idea one"),
		messageResponse("s2", "idea two"),
		messageResponse("s3", "idea three"),
		messageResponse("d1", "refine"),
		messageResponse("d2", "refine more"),
		messageResponse("d3", "refine least"),
		messageResponse("exp", "expanded answer"),
	}}}
	judge := &fakeJudge{batches: [][]float64{
		{0.4},            // greedy: unsure
		{0.3, 0.85, 0.5}, // level 1: winner < 0.9 → derive
		{0.95, 0.4, 0.6}, // level 2: winner ≥ 0.9 → expand
		{0.9},            // expanded beats 0.4
	}}
	var events []Event
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 3,
		Trace: func(e Event) { events = append(events, e) }})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "exp" {
		t.Fatalf("got %q, want the expanded answer", resp.ID)
	}
	// 1 greedy + 3 + 3 sketches + 1 expansion.
	if got := inner.calls.Load(); got != 8 {
		t.Fatalf("inner calls = %d, want 8", got)
	}
	if got := judge.calls.Load(); got != 4 {
		t.Fatalf("judge calls = %d, want 4", got)
	}
	if len(events) != 1 || !events[0].Tree || len(events[0].Paths) != 3 || events[0].Answer != "expanded answer" {
		t.Fatalf("event = %+v", events[0])
	}
	// The level-2 derive requests must quote the level-1 winning sketch;
	// concurrent sketch calls make which text landed at index 1 unknown,
	// so any sketch text satisfies the check.
	found := false
	for _, req := range inner.requests[4:7] {
		if msg, ok := req.Input[len(req.Input)-1].Data.(llm.Message); ok && strings.Contains(msg.Text, "idea") {
			found = true
		}
	}
	if !found {
		t.Fatal("no level-2 request quoted a level-1 sketch")
	}
	// The expansion request carries the numbered chain.
	last := inner.requests[len(inner.requests)-1]
	if msg, ok := last.Input[len(last.Input)-1].Data.(llm.Message); !ok || !strings.Contains(msg.Text, "refine") {
		t.Fatalf("expand request = %+v", last.Input[len(last.Input)-1])
	}
}

func TestTreeKeepsGreedyWhenConfident(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{messageResponse("r0", "sure")}}
	judge := &fakeJudge{batches: [][]float64{{0.99}}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 3})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" || inner.calls.Load() != 1 || judge.calls.Load() != 1 {
		t.Fatalf("resp=%q inner=%d judge=%d", resp.ID, inner.calls.Load(), judge.calls.Load())
	}
}

func TestTreeKeepsGreedyWhenExpandedLoses(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy"),
		messageResponse("s1", "i1"), messageResponse("s2", "i2"), messageResponse("s3", "i3"),
		messageResponse("exp", "expanded"),
	}}
	judge := &fakeJudge{batches: [][]float64{
		{0.6},            // greedy: unsure
		{0.95, 0.4, 0.3}, // level 1 winner ≥ 0.9 → expand at once
		{0.5},            // expanded scores worse → keep greedy
	}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 3})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want greedy", resp.ID)
	}
	if got := inner.calls.Load(); got != 5 {
		t.Fatalf("inner calls = %d, want 5 (no level 2)", got)
	}
}

func TestTreeStripsToolsFromSketchAndExpand(t *testing.T) {
	inner := &recordingInner{fakeInner: fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy"),
		messageResponse("s1", "i1"), messageResponse("s2", "i2"), messageResponse("s3", "i3"),
		messageResponse("exp", "expanded"),
	}}}
	judge := &fakeJudge{batches: [][]float64{
		{0.4}, {0.95, 0.4, 0.3}, {0.9},
	}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 3})
	req := testRequest()
	req.Tools = []llm.Tool{{Type: llm.ToolFunction, Name: "Bash"}}
	if _, err := a.Respond(context.Background(), req, llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(inner.requests[0].Tools) != 1 {
		t.Fatal("the first answer must keep its tools")
	}
	for i, req := range inner.requests[1:] {
		if len(req.Tools) != 0 {
			t.Fatalf("call %d kept %d tools", i+1, len(req.Tools))
		}
	}
	// Sketches run at low effort so the outline stays cheap; the expansion
	// keeps whatever effort was routed for the turn.
	for i, req := range inner.requests[1:4] {
		if req.Model.ReasoningEffort != llm.ReasoningEffortLow {
			t.Fatalf("sketch call %d effort = %q, want low", i+1, req.Model.ReasoningEffort)
		}
	}
	if got := inner.requests[len(inner.requests)-1].Model.ReasoningEffort; got != "" {
		t.Fatalf("expand effort = %q, want the routed (unchanged) effort", got)
	}
}
