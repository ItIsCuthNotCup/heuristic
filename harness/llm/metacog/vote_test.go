package metacog

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

// scriptInner answers call i with texts[i] after delays[i]; a delay < 0
// blocks until the context is cancelled.
type scriptInner struct {
	texts     []string
	delays    []time.Duration
	calls     atomic.Int32
	cancelled atomic.Int32
}

func (s *scriptInner) Respond(ctx context.Context, req llm.Request, opts llm.RequestOptions) (llm.Response, error) {
	i := int(s.calls.Add(1)) - 1
	if s.delays[i] < 0 {
		<-ctx.Done()
		s.cancelled.Add(1)
		return llm.Response{}, ctx.Err()
	}
	select {
	case <-time.After(s.delays[i]):
	case <-ctx.Done():
		s.cancelled.Add(1)
		return llm.Response{}, ctx.Err()
	}
	resp := messageResponse("r", s.texts[i])
	resp.Usage = llm.Usage{OutputTokens: 10}
	return resp, nil
}

// constJudge scores every candidate the same, optionally per-text.
type constJudge struct {
	score  float64
	byText map[string]float64
	same   map[[2]string]float64
	mu     sync.Mutex
	sames  int
}

func (j *constJudge) Score(ctx context.Context, problem string, candidates []string) ([]float64, error) {
	out := make([]float64, len(candidates))
	for i, c := range candidates {
		out[i] = j.score
		if s, ok := j.byText[c]; ok {
			out[i] = s
		}
	}
	return out, nil
}

type sameJudge struct{ *constJudge }

func (j sameJudge) Same(ctx context.Context, problem, a, b string) (float64, error) {
	j.mu.Lock()
	j.sames++
	j.mu.Unlock()
	if s, ok := j.same[[2]string{a, b}]; ok {
		return s, nil
	}
	return j.same[[2]string{b, a}], nil
}

func voteAdapter(inner llm.Adapter, judge Judge, events *[]Event, extra func(*Config)) *Adapter {
	var mu sync.Mutex
	cfg := Config{Judge: judge, Mode: ModeFinal, Vote: 3, MinBranchTime: 2 * time.Second, Trace: func(e Event) {
		mu.Lock()
		*events = append(*events, e)
		mu.Unlock()
	}}
	if extra != nil {
		extra(&cfg)
	}
	return New(inner, cfg)
}

func TestVoteStopsWhenTwoPathsAgree(t *testing.T) {
	inner := &scriptInner{
		texts:  []string{"Answer: 42", "Answer: 42", "", ""},
		delays: []time.Duration{0, time.Millisecond, -1, -1},
	}
	var events []Event
	a := voteAdapter(inner, &constJudge{score: 0.5}, &events, nil)
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if responseText(resp) != "Answer: 42" {
		t.Fatalf("resp = %q", responseText(resp))
	}
	if resp.Usage.OutputTokens != 20 {
		t.Fatalf("usage = %+v, want first answer + one branch", resp.Usage)
	}
	waitFor(t, func() bool { return inner.cancelled.Load() == 2 })
	if len(events) != 1 || events[0].Chosen != 0 || agreeing(events[0].Agree) != 2 || len(events[0].Scores) != 0 {
		t.Fatalf("event = %+v", events)
	}
}

// TestVoteLazyNeverStartsPathsWhenConfident: with VoteLazy the judge
// answers first, so a confident first answer spends zero path calls.
func TestVoteLazyNeverStartsPathsWhenConfident(t *testing.T) {
	inner := &scriptInner{texts: []string{"Answer: 1", "", "", ""}, delays: []time.Duration{0, -1, -1, -1}}
	var events []Event
	a := voteAdapter(inner, &constJudge{score: 0.99}, &events, func(c *Config) { c.VoteLazy = true })
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID == "" {
		t.Fatal("no response")
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner calls = %d, want 1 (paths never started)", got)
	}
	if len(events) != 1 || !events[0].Stopped || events[0].GreedyScore != 0.99 {
		t.Fatalf("event = %+v", events)
	}
}

// TestVoteLazyStillRacesWhenUnsure: an unsure first answer falls through
// to the race even in lazy mode.
func TestVoteLazyStillRacesWhenUnsure(t *testing.T) {
	inner := &scriptInner{
		texts:  []string{"Answer: 42", "Answer: 42", "", ""},
		delays: []time.Duration{0, time.Millisecond, -1, -1},
	}
	var events []Event
	a := voteAdapter(inner, &constJudge{score: 0.5}, &events, func(c *Config) { c.VoteLazy = true })
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if responseText(resp) != "Answer: 42" {
		t.Fatalf("resp = %q", responseText(resp))
	}
	if got := inner.calls.Load(); got < 2 {
		t.Fatalf("inner calls = %d, want greedy + raced paths", got)
	}
}

func TestVoteTrustsAVeryConfidentFirstAnswer(t *testing.T) {
	inner := &scriptInner{texts: []string{"Answer: 1", "", "", ""}, delays: []time.Duration{0, -1, -1, -1}}
	var events []Event
	a := voteAdapter(inner, &constJudge{score: 0.99}, &events, nil)
	if _, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return inner.cancelled.Load() == 3 })
	if len(events) != 1 || !events[0].Stopped || events[0].GreedyScore != 0.99 {
		t.Fatalf("event = %+v", events)
	}
}

func TestVoteFallsBackToJudgeWhenNoPathsAgree(t *testing.T) {
	inner := &scriptInner{
		texts:  []string{"Answer: A", "Answer: B", "Answer: C", "Answer: D"},
		delays: []time.Duration{0, time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
	}
	var events []Event
	judge := &constJudge{score: 0.3, byText: map[string]float64{"Answer: C": 0.9}}
	a := voteAdapter(inner, judge, &events, func(c *Config) { c.AnswerPrior = 0 })
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if responseText(resp) != "Answer: C" {
		t.Fatalf("resp = %q, want the judge's pick", responseText(resp))
	}
	if resp.Usage.OutputTokens != 40 {
		t.Fatalf("usage = %+v, want all four paths", resp.Usage)
	}
	if len(events) != 1 || len(events[0].Scores) != 4 || len(events[0].Agree) != 0 {
		t.Fatalf("event = %+v", events)
	}
}

func TestVoteAsksSameJudgeWhenNoAnswerIsStated(t *testing.T) {
	inner := &scriptInner{
		texts:  []string{"prose one", "prose two", "prose three", ""},
		delays: []time.Duration{0, time.Millisecond, 20 * time.Millisecond, -1},
	}
	judge := sameJudge{&constJudge{score: 0.5, same: map[[2]string]float64{
		{"prose one", "prose two"}:   0.1,
		{"prose two", "prose three"}: 0.9,
	}}}
	var events []Event
	a := voteAdapter(inner, judge, &events, func(c *Config) { c.Background = true })
	resp, err := a.Respond(context.Background(), testRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if responseText(resp) != "prose one" {
		t.Fatalf("background mode must return the first answer, got %q", responseText(resp))
	}
	swap := func() string {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.swaps["prose one"]
	}
	waitFor(t, func() bool { return swap() != "" })
	if swap() != "prose two" {
		t.Fatalf("swap = %q, want the agreed path", swap())
	}
	judge.mu.Lock()
	defer judge.mu.Unlock()
	if judge.sames != 3 {
		t.Fatalf("same calls = %d, want 3", judge.sames)
	}
}

func TestStatedAnswer(t *testing.T) {
	for text, want := range map[string]string{
		"so \\boxed{C} it is":               "c",
		"work\n**Answer:** 42.":             "42",
		"Final answer - x = 3":              "x = 3",
		"The capital is Paris.":             "",
		"answer: $\\frac{1}{2}$":            "\\frac{1}{2}",
		"lines\nAnswer: B\nmore\nAnswer: D": "d",
	} {
		if got := statedAnswer(text); got != want {
			t.Errorf("statedAnswer(%q) = %q, want %q", text, got, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func agreeing(values []bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}
