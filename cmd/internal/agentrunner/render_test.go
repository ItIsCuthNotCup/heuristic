package agentrunner

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

func renderItems(r *renderer, items ...sessionstore.Item) {
	for _, item := range items {
		r.Observe("s", item)
	}
}

func testRenderer(showPrompt bool) (*renderer, *bytes.Buffer) {
	var out bytes.Buffer
	return &renderer{out: &out, showPrompt: showPrompt}, &out
}

func responseItem(resp llm.Response) sessionstore.Item {
	return sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: resp}}
}

func TestRendererMessageAndPrompt(t *testing.T) {
	r, out := testRenderer(true)
	renderItems(r, responseItem(llm.Response{Output: []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "hello there"}},
	}}))
	got := out.String()
	if !strings.Contains(got, "hello there\n") || !strings.HasSuffix(got, "\n› ") {
		t.Fatalf("output = %q, want text then prompt", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("no-color renderer emitted ANSI")
	}
}

func TestRendererNoPromptAfterToolCall(t *testing.T) {
	r, out := testRenderer(true)
	renderItems(r, responseItem(llm.Response{Output: []llm.Item{
		{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "c", Name: "Bash", Arguments: `{"command":"ls -la"}`}},
	}}))
	got := out.String()
	if got != "⚙ bash: ls -la\n" {
		t.Fatalf("output = %q, want tool line only, no prompt", got)
	}
}

func TestRendererToolCallFallbacks(t *testing.T) {
	r, out := testRenderer(false)
	renderItems(r, responseItem(llm.Response{Output: []llm.Item{
		{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "ViewImage", Arguments: `{"path":"img.png"}`}},
		{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "Mystery", Arguments: `{"x":1}`}},
	}}))
	got := out.String()
	if !strings.Contains(got, "⚙ view_image: img.png") || !strings.Contains(got, "⚙ mystery {\"x\":1}") {
		t.Fatalf("output = %q", got)
	}
}

func TestRendererToolStatus(t *testing.T) {
	r, out := testRenderer(false)
	renderItems(r, sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{
		Status: tool.CallStatus{Error: "boom"},
	}})
	if !strings.HasPrefix(out.String(), "✗ boom") {
		t.Fatalf("error output = %q", out.String())
	}

	out.Reset()
	state, _ := json.Marshal(operation.ShellState{Result: &operation.ShellResult{Out: "l1\nl2\n", ExitCode: 0}})
	renderItems(r, sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{
		Operations: []operation.Operation{{State: state}},
	}})
	if got := out.String(); got != "  l1\n  l2\n" {
		t.Fatalf("shell output = %q, want last lines indented", got)
	}

	out.Reset()
	renderItems(r, sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{}})
	if got := out.String(); got != "✓ done\n" {
		t.Fatalf("empty status = %q", got)
	}
}

func TestRendererMetaCogEvents(t *testing.T) {
	r, out := testRenderer(false)
	r.Event(metacog.Event{Stopped: true, GreedyScore: 0.97, JudgeCalls: 1, DurationMs: 320})
	r.Event(metacog.Event{GreedyScore: 0.4, Branches: 5, Chosen: 2, Scores: []float64{0.4, 0.5, 0.8}, JudgeCalls: 9, DurationMs: 2100})
	got := out.String()
	if !strings.Contains(got, "confident 0.97, no branching (1 judge call(s), 0.3s)") {
		t.Fatalf("stopped event = %q", got)
	}
	if !strings.Contains(got, "greedy 0.40 → 5 more thought paths → picked #3 (score 0.80), 9 judge calls, 2.1s") {
		t.Fatalf("branched event = %q", got)
	}

	// Unscored paths mean the judge never ranked them, so the first answer
	// stands; reporting "picked #1 (score 0.00)" would invent a verdict.
	out.Reset()
	r.Event(metacog.Event{GreedyScore: 0.84, Branches: 2, JudgeCalls: 1, DurationMs: 7400})
	if got := out.String(); !strings.Contains(got, "greedy 0.84 → 2 more thought paths → kept the first answer, 1 judge calls, 7.4s") {
		t.Fatalf("unscored event = %q", got)
	}
}

func TestRendererSkipsInputAndTurn(t *testing.T) {
	r, out := testRenderer(false)
	renderItems(r,
		sessionstore.Item{Kind: sessionstore.ItemInput},
		sessionstore.Item{Kind: sessionstore.ItemTurn},
		sessionstore.Item{Kind: sessionstore.ItemFork},
	)
	if out.Len() != 0 {
		t.Fatalf("output = %q, want silence", out.String())
	}
}

func TestRendererSkipsWaitingStatus(t *testing.T) {
	r, out := testRenderer(false)
	renderItems(r, sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{
		Status: tool.CallStatus{WaitingFor: []operation.ID{"op-1"}},
	}})
	if out.Len() != 0 {
		t.Fatalf("waiting status = %q, want silence", out.String())
	}
}
