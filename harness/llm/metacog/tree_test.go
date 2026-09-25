package metacog

import (
	"context"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

// TestTreeFusesWhenNothingConfident runs the full loop: unsure first
// answer → concise paths, none at the bar → merge → fused answer beats
// greedy.
func TestTreeFusesWhenNothingConfident(t *testing.T) {
	inner := &recordingInner{fakeInner: fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy answer"),
		messageResponse("p1", "path one"),
		messageResponse("p2", "path two"),
		messageResponse("p3", "path three"),
		messageResponse("fused", "merged answer"),
	}}}
	judge := &fakeJudge{batches: [][]float64{
		{0.4},            // greedy: unsure
		{0.5, 0.6, 0.55}, // round 1: none reaches 0.8 → merge
		{0.9},            // fused beats 0.4 and the best path
	}}
	var events []Event
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 1,
		Trace: func(e Event) { events = append(events, e) }})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "fused" {
		t.Fatalf("got %q, want the merged answer", resp.ID)
	}
	// 1 greedy + 3 paths + 1 merge — one round is enough when the merge lands.
	if got := inner.calls.Load(); got != 5 {
		t.Fatalf("inner calls = %d, want 5", got)
	}
	if got := judge.calls.Load(); got != 3 {
		t.Fatalf("judge calls = %d, want 3", got)
	}
	if len(events) != 1 || !events[0].Tree || len(events[0].Paths) != 3 || events[0].Answer != "merged answer" {
		t.Fatalf("event = %+v", events[0])
	}
	// The merge request carries the candidate answers.
	last := inner.requests[len(inner.requests)-1]
	msg, ok := last.Input[len(last.Input)-1].Data.(llm.Message)
	if !ok || !strings.Contains(msg.Text, "path two") || !strings.Contains(msg.Text, "Candidate") {
		t.Fatalf("merge request = %+v", last.Input[len(last.Input)-1])
	}
	if len(last.Tools) != 0 {
		t.Fatalf("merge request kept %d tools", len(last.Tools))
	}
}

// TestTreePicksConfidentPath stops as soon as a path clears the bar — no
// merge call, no second round.
func TestTreePicksConfidentPath(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy"),
		messageResponse("p1", "p1"), messageResponse("p2", "p2"), messageResponse("p3", "p3"),
	}}
	judge := &fakeJudge{batches: [][]float64{
		{0.4}, {0.5, 0.85, 0.4},
	}}
	var events []Event
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 3,
		Trace: func(e Event) { events = append(events, e) }})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent path calls land in any slot, so compare the chosen text,
	// not a fake response ID: the winner is scores' argmax (index 1).
	if len(events) != 1 || events[0].Chosen != 1 {
		t.Fatalf("event = %+v", events[0])
	}
	if got := responseText(resp); got != events[0].Paths[1] {
		t.Fatalf("got %q, want the confident path %q", got, events[0].Paths[1])
	}
	if got := inner.calls.Load(); got != 4 {
		t.Fatalf("inner calls = %d, want 4 (no merge)", got)
	}
	if got := judge.calls.Load(); got != 2 {
		t.Fatalf("judge calls = %d, want 2", got)
	}
}

func TestTreeKeepsGreedyWhenConfident(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{messageResponse("r0", "sure")}}
	judge := &fakeJudge{batches: [][]float64{{0.85}}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 3})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" || inner.calls.Load() != 1 || judge.calls.Load() != 1 {
		t.Fatalf("resp=%q inner=%d judge=%d", resp.ID, inner.calls.Load(), judge.calls.Load())
	}
}

// TestTreeSecondRoundWhenAllWeak: round 1 all below the bar, round 2
// clears it → the round-2 winner is used with no merge call.
func TestTreeSecondRoundWhenAllWeak(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy"),
		messageResponse("a1", "a1"), messageResponse("a2", "a2"), messageResponse("a3", "a3"),
		messageResponse("b1", "b1"), messageResponse("b2", "b2"), messageResponse("b3", "b3"),
	}}
	judge := &fakeJudge{batches: [][]float64{
		{0.4}, {0.5, 0.4, 0.3}, {0.6, 0.82, 0.5},
	}}
	var events []Event
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 3,
		Trace: func(e Event) { events = append(events, e) }})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Chosen != 4 || len(events[0].Paths) != 6 {
		t.Fatalf("event = %+v", events[0])
	}
	if got := responseText(resp); got != events[0].Paths[4] {
		t.Fatalf("got %q, want the round-2 winner %q", got, events[0].Paths[4])
	}
	if got := inner.calls.Load(); got != 7 {
		t.Fatalf("inner calls = %d, want 7 (two rounds, no merge)", got)
	}
}

// TestTreeKeepsGreedyWhenFusedLoses: fusion is a last resort, not a free
// upgrade — a merge that scores below greedy must not replace it.
func TestTreeKeepsGreedyWhenFusedLoses(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy"),
		messageResponse("p1", "p1"), messageResponse("p2", "p2"), messageResponse("p3", "p3"),
		messageResponse("fused", "fused"),
	}}
	judge := &fakeJudge{batches: [][]float64{
		{0.7}, {0.4, 0.6, 0.5}, {0.5},
	}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 1})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want greedy", resp.ID)
	}
	if got := inner.calls.Load(); got != 5 {
		t.Fatalf("inner calls = %d, want 5", got)
	}
}

// TestTreePicksBestPathWhenMergeFails: even under the bar, a path that
// still outscores greedy is used when the merge produces nothing better.
func TestTreePicksBestPathWhenMergeFails(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy"),
		messageResponse("p1", "p1"), messageResponse("p2", "p2"), messageResponse("p3", "p3"),
		messageResponse("fused", "fused"),
	}}
	judge := &fakeJudge{batches: [][]float64{
		{0.3}, {0.4, 0.7, 0.5}, {0.4}, // fused loses to the best path (0.7)
	}}
	var events []Event
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 1,
		Trace: func(e Event) { events = append(events, e) }})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Chosen != 1 {
		t.Fatalf("event = %+v", events[0])
	}
	if got := responseText(resp); got != events[0].Paths[1] {
		t.Fatalf("got %q, want the best path %q", got, events[0].Paths[1])
	}
}

func TestTreeStripsToolsFromPathsAndMerge(t *testing.T) {
	inner := &recordingInner{fakeInner: fakeInner{responses: []llm.Response{
		messageResponse("r0", "greedy"),
		messageResponse("p1", "p1"), messageResponse("p2", "p2"), messageResponse("p3", "p3"),
		messageResponse("fused", "fused"),
	}}}
	judge := &fakeJudge{batches: [][]float64{
		{0.4}, {0.5, 0.6, 0.4}, {0.9},
	}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, TreeDepth: 1})
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
	// The path instruction asks for a concise answer.
	if msg, ok := inner.requests[1].Input[len(inner.requests[1].Input)-1].Data.(llm.Message); !ok ||
		!strings.Contains(msg.Text, "1000 tokens") {
		t.Fatalf("concise instruction missing: %+v", inner.requests[1].Input[len(inner.requests[1].Input)-1])
	}
}
