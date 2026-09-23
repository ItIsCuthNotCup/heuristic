package metacog

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

// fakeInner returns scripted responses/errors per call index (0 = greedy).
type fakeInner struct {
	responses []llm.Response
	errs      []error
	calls     atomic.Int32
}

func (f *fakeInner) Respond(ctx context.Context, req llm.Request, opts llm.RequestOptions) (llm.Response, error) {
	i := int(f.calls.Add(1)) - 1
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return llm.Response{}, f.errs[i]
	}
	return f.responses[i], nil
}

// fakeJudge returns scripted score batches; each Score call pops the next batch.
type fakeJudge struct {
	batches       [][]float64
	calls         atomic.Int32
	problems      []string
	candidateSets [][]string
}

func (f *fakeJudge) Score(ctx context.Context, problem string, candidates []string) ([]float64, error) {
	i := int(f.calls.Add(1)) - 1
	f.problems = append(f.problems, problem)
	f.candidateSets = append(f.candidateSets, candidates)
	if i >= len(f.batches) {
		i = len(f.batches) - 1
	}
	batch := f.batches[i]
	if len(batch) != len(candidates) {
		return nil, errors.New("scripted batch size mismatch")
	}
	return batch, nil
}

func messageResponse(id, text string) llm.Response {
	return llm.Response{
		ID:   id,
		Stop: llm.StopComplete,
		Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}},
		},
	}
}

func toolCallResponse(id string) llm.Response {
	return llm.Response{
		ID:   id,
		Stop: llm.StopComplete,
		Output: []llm.Item{
			{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "c1", Name: "Bash", Arguments: `{"cmd":"ls"}`}},
		},
	}
}

func testRequest() llm.Request {
	return llm.Request{
		Model: llm.Model{ID: "m"},
		Input: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "sys"}},
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "question"}},
		},
	}
}

func TestModeOffPassThrough(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{messageResponse("r0", "answer")}}
	judge := &fakeJudge{}
	a := New(inner, Config{Judge: judge, Mode: ModeOff})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want r0", resp.ID)
	}
	if inner.calls.Load() != 1 || judge.calls.Load() != 0 {
		t.Fatalf("inner=%d judge=%d, want 1/0", inner.calls.Load(), judge.calls.Load())
	}
}

func TestModeFinalSkipsToolCallTurns(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{toolCallResponse("r0")}}
	judge := &fakeJudge{}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want r0", resp.ID)
	}
	if inner.calls.Load() != 1 || judge.calls.Load() != 0 {
		t.Fatalf("inner=%d judge=%d, want 1/0", inner.calls.Load(), judge.calls.Load())
	}
}

func TestStopAtConfidence(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{messageResponse("r0", "answer")}}
	judge := &fakeJudge{batches: [][]float64{{0.97}}}
	var events []Event
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, Trace: func(e Event) { events = append(events, e) }})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want r0", resp.ID)
	}
	if inner.calls.Load() != 1 || judge.calls.Load() != 1 {
		t.Fatalf("inner=%d judge=%d, want 1/1", inner.calls.Load(), judge.calls.Load())
	}
	if len(events) != 1 || !events[0].Stopped {
		t.Fatalf("events=%+v, want one stopped event", events)
	}
}

func TestBranchingScalesWithUncertainty(t *testing.T) {
	cases := []struct {
		s0           float64
		wantBranches int
	}{
		{0.0, 6},
		{0.75, 3}, // round(2 + 0.25*4) = 3
		{0.9, 2},  // round(2.4) = 2
	}
	for _, tc := range cases {
		t.Run(strings.Repeat("x", int(tc.s0*10)+1), func(t *testing.T) {
			responses := []llm.Response{messageResponse("r0", "greedy")}
			for i := range 6 {
				responses = append(responses, messageResponse("b"+string(rune('a'+i)), "branch"))
			}
			inner := &fakeInner{responses: responses}
			// batch 1: greedy score; batch 2: pool text scores; batch 3: priors
			pool := make([]float64, tc.wantBranches+1)
			for i := range pool {
				pool[i] = 0.5
			}
			pool[len(pool)-1] = 0.9 // last branch wins
			judge := &fakeJudge{batches: [][]float64{{tc.s0}, pool, {0.5, 0.5}}}
			var events []Event
			a := New(inner, Config{Judge: judge, Mode: ModeFinal, AnswerPrior: 0.5, Trace: func(e Event) { events = append(events, e) }})
			resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if int(inner.calls.Load()) != tc.wantBranches+1 {
				t.Fatalf("inner calls = %d, want %d", inner.calls.Load(), tc.wantBranches+1)
			}
			// pool order is nondeterministic (concurrent branches); the highest
			// score sits at the last pool slot, so any branch may win — but never
			// the greedy r0.
			if resp.ID == "r0" {
				t.Fatalf("picked greedy despite a higher-scoring branch")
			}
			if len(events) != 1 || events[0].Branches != tc.wantBranches || events[0].PoolSize != tc.wantBranches+1 {
				t.Fatalf("event=%+v, want branches=%d pool=%d", events, tc.wantBranches, tc.wantBranches+1)
			}
			if events[0].Chosen != tc.wantBranches {
				t.Fatalf("chosen = %d, want %d (top score is last pool slot)", events[0].Chosen, tc.wantBranches)
			}
		})
	}
}

func TestAnswerPriorChangesWinner(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{
		messageResponse("r0", "reasoning\nFinal answer: A"),
		messageResponse("b1", "reasoning\nFinal answer: B"),
		messageResponse("b2", "reasoning\nFinal answer: B"),
	}}
	// text scores tie 0.75/0.75/0.75; priors: A=0.45, B=0.62 -> B wins.
	judge := &fakeJudge{batches: [][]float64{
		{0.5},
		{0.75, 0.75, 0.75},
		{0.45, 0.62},
	}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, AnswerPrior: 0.5, NMin: 2, NMax: 2})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// b1 and b2 both state "Final answer: B" and tie on the combined score;
	// concurrent branches make pool order nondeterministic, so either is a
	// valid winner — what matters is the prior-favoured answer beats greedy.
	if resp.ID == "r0" {
		t.Fatalf("picked greedy; want a Final-answer-B branch")
	}
}

func TestBranchFailuresIgnored(t *testing.T) {
	errBoom := errors.New("boom")
	inner := &fakeInner{
		responses: []llm.Response{
			messageResponse("r0", "greedy"),
			messageResponse("b1", "branch"),
			{}, {},
		},
		errs: []error{nil, nil, errBoom, errBoom},
	}
	pool := []float64{0.4, 0.8}
	judge := &fakeJudge{batches: [][]float64{{0.5}, pool, {0.5}}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, AnswerPrior: 0.5, NMin: 4, NMax: 4})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "b1" {
		t.Fatalf("picked %q, want b1", resp.ID)
	}
}

func TestAllBranchesFailReturnsGreedy(t *testing.T) {
	errBoom := errors.New("boom")
	inner := &fakeInner{
		responses: []llm.Response{messageResponse("r0", "greedy"), {}, {}},
		errs:      []error{nil, errBoom, errBoom},
	}
	judge := &fakeJudge{batches: [][]float64{{0.5}}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, NMin: 2, NMax: 2})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want r0", resp.ID)
	}
	if judge.calls.Load() != 1 {
		t.Fatalf("judge calls = %d, want 1 (no pool scoring when pool is 1)", judge.calls.Load())
	}
}

func TestUsageSummed(t *testing.T) {
	greedy := messageResponse("r0", "answer")
	greedy.Usage = llm.Usage{InputTokens: 10, OutputTokens: 5}
	branch := messageResponse("b1", "answer")
	branch.Usage = llm.Usage{InputTokens: 20, OutputTokens: 7, ReasoningTokens: 3}
	inner := &fakeInner{responses: []llm.Response{greedy, branch}}
	judge := &fakeJudge{batches: [][]float64{{0.5}, {0.9, 0.1}}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, AnswerPrior: 0, NMin: 1, NMax: 1})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 30 || resp.Usage.OutputTokens != 12 || resp.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage = %+v, want summed", resp.Usage)
	}
	if resp.Usage.Raw != nil {
		t.Fatal("usage Raw must be nilled")
	}
}

func TestUsageSummedWhenPoolCollapses(t *testing.T) {
	// All branches come back as tool calls (non-final in ModeFinal), so the
	// pool collapses to just greedy — but branch tokens were still spent.
	greedy := messageResponse("r0", "answer")
	greedy.Usage = llm.Usage{InputTokens: 10, OutputTokens: 5}
	b1 := toolCallResponse("b1")
	b1.Usage = llm.Usage{InputTokens: 20, OutputTokens: 8}
	inner := &fakeInner{responses: []llm.Response{greedy, b1}}
	judge := &fakeJudge{batches: [][]float64{{0.5}}}
	a := New(inner, Config{Judge: judge, Mode: ModeFinal, NMin: 1, NMax: 1})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want r0", resp.ID)
	}
	if resp.Usage.InputTokens != 30 || resp.Usage.OutputTokens != 13 {
		t.Fatalf("usage = %+v, want branch tokens summed", resp.Usage)
	}
}

type errJudge struct{ calls atomic.Int32 }

func (e *errJudge) Score(ctx context.Context, problem string, candidates []string) ([]float64, error) {
	if e.calls.Add(1) == 1 {
		return []float64{0.5}, nil // greedy score passes through
	}
	return nil, errors.New("judge down")
}

func TestUsageSummedOnJudgeError(t *testing.T) {
	greedy := messageResponse("r0", "answer")
	greedy.Usage = llm.Usage{InputTokens: 10, OutputTokens: 5}
	b1 := messageResponse("b1", "alt")
	b1.Usage = llm.Usage{InputTokens: 20, OutputTokens: 8}
	inner := &fakeInner{responses: []llm.Response{greedy, b1}}
	a := New(inner, Config{Judge: &errJudge{}, Mode: ModeFinal, NMin: 1, NMax: 1})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "r0" {
		t.Fatalf("got %q, want r0", resp.ID)
	}
	if resp.Usage.InputTokens != 30 || resp.Usage.OutputTokens != 13 {
		t.Fatalf("usage = %+v, want branch tokens summed on judge error", resp.Usage)
	}
}

func TestModeAllJudgesToolCallTurns(t *testing.T) {
	inner := &fakeInner{responses: []llm.Response{
		toolCallResponse("r0"),
		messageResponse("b1", "final"),
	}}
	judge := &fakeJudge{batches: [][]float64{{0.5}, {0.2, 0.9}}}
	a := New(inner, Config{Judge: judge, Mode: ModeAll, AnswerPrior: 0, NMin: 1, NMax: 1})
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "b1" {
		t.Fatalf("picked %q, want b1", resp.ID)
	}
	if judge.calls.Load() != 2 {
		t.Fatalf("judge calls = %d, want 2", judge.calls.Load())
	}
}

func TestExtractAnswer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"reasoning\nFinal answer: D", "D"},
		{"a\nFINAL ANSWER - 42\n", "42"},
		{"the answer is \\boxed{42} tail", "42"},
		{"first\n\nsecond line", "second line"},
		{"Final answer: x\nFinal answer: y", "y"},
	}
	for _, tc := range cases {
		if got := ExtractAnswer(tc.in); got != tc.want {
			t.Errorf("ExtractAnswer(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
