package metacog

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

// Config controls the metacognition wrapper around an llm.Adapter.
type Config struct {
	Judge           Judge
	Mode            Mode        // ModeOff | ModeFinal (default) | ModeAll
	StopConfidence  float64     // default 0.95
	NMin, NMax      int         // default 2, 6
	AnswerPrior     float64     // default 0.5; <= 0 disables
	MaxProblemChars int         // default 12000
	Trace           func(Event) // optional sink for trace events
}

func (c Config) withDefaults() Config {
	if c.StopConfidence <= 0 {
		c.StopConfidence = 0.95
	}
	if c.NMin <= 0 {
		c.NMin = 2
	}
	if c.NMax <= 0 {
		c.NMax = 6
	}
	if c.MaxProblemChars <= 0 {
		c.MaxProblemChars = 12000
	}
	return c
}

// Event is emitted once per Respond call (also for pass-throughs that decided
// not to branch) when Config.Trace is set.
type Event struct {
	Turn        string    `json:"turn"`
	GreedyScore float64   `json:"greedy_score"`
	Branches    int       `json:"branches"`
	PoolSize    int       `json:"pool_size"`
	Scores      []float64 `json:"scores,omitempty"`
	Prior       []float64 `json:"prior,omitempty"`
	Chosen      int       `json:"chosen"`
	JudgeCalls  int       `json:"judge_calls"`
	Stopped     bool      `json:"stopped"`
	DurationMs  int64     `json:"duration_ms"`
}

// Adapter wraps an inner llm.Adapter with the MetaCog adaptive loop: score
// the first ("greedy") answer, branch n extra paths scaled by the judge's
// uncertainty when the score is below StopConfidence, and return the best.
type Adapter struct {
	inner llm.Adapter
	cfg   Config
}

func New(inner llm.Adapter, cfg Config) *Adapter {
	return &Adapter{inner: inner, cfg: cfg.withDefaults()}
}

var _ llm.Adapter = (*Adapter)(nil)

func (a *Adapter) Respond(ctx context.Context, req llm.Request, opts llm.RequestOptions) (llm.Response, error) {
	start := time.Now()
	event := Event{Turn: opts.CacheKey, Chosen: 0}
	judgeCalls := 0
	emit := func() {
		if a.cfg.Trace != nil {
			event.JudgeCalls = judgeCalls
			event.DurationMs = time.Since(start).Milliseconds()
			a.cfg.Trace(event)
		}
	}

	greedy, err := a.inner.Respond(ctx, req, opts)
	if err != nil {
		return greedy, err
	}
	if a.cfg.Mode == ModeOff || a.cfg.Judge == nil {
		return greedy, nil
	}

	isFinal := isFinalResponse(greedy)
	if a.cfg.Mode == ModeFinal && !isFinal {
		// Routine agent tool step: pass through with zero judge overhead.
		return greedy, nil
	}

	problem := BuildProblem(req, a.cfg.MaxProblemChars)
	pool := []llm.Response{greedy}

	s0, err := a.judge(ctx, problem, []string{responseText(greedy)})
	judgeCalls++
	if err != nil {
		return greedy, nil // judge unavailable: degrade to the plain adapter
	}
	event.GreedyScore = s0[0]
	if s0[0] >= a.cfg.StopConfidence {
		event.Stopped = true
		emit()
		return greedy, nil
	}

	// Branching scales with the judge's uncertainty u = 1 - s0, the same
	// formula as controller.py _adaptive.
	u := 1.0 - s0[0]
	n := int(math.Round(float64(a.cfg.NMin) + u*float64(a.cfg.NMax-a.cfg.NMin)))
	if n < 1 {
		n = 1
	}
	event.Branches = n

	branches := a.branch(ctx, req, opts, n)
	for _, resp := range branches {
		if a.cfg.Mode == ModeFinal && !isFinalResponse(resp) {
			continue
		}
		pool = append(pool, resp)
	}
	event.PoolSize = len(pool)
	if len(pool) == 1 {
		emit()
		return greedy, nil
	}

	texts := make([]string, len(pool))
	for i := range pool {
		texts[i] = responseText(pool[i])
	}
	textScores, err := a.judge(ctx, problem, texts)
	judgeCalls += len(texts)
	if err != nil || len(textScores) != len(pool) {
		emit()
		return greedy, nil
	}
	event.Scores = textScores

	totals := textScores
	if a.cfg.AnswerPrior > 0 {
		prior := a.answerPriors(ctx, problem, texts, &judgeCalls)
		if prior != nil {
			totals = make([]float64, len(pool))
			priorPerPool := make([]float64, len(pool))
			for i, text := range texts {
				answer := ExtractAnswer(text)
				p := 0.5
				if answer != "" {
					p = priorAnswerScore(prior, answer)
				}
				priorPerPool[i] = p
				totals[i] = textScores[i] + a.cfg.AnswerPrior*p
			}
			event.Prior = priorPerPool
		}
	}

	chosen := argmax(totals)
	event.Chosen = chosen
	emit()

	selected := pool[chosen]
	selected.Usage = sumUsage(greedy.Usage, branches)
	selected.Usage.Raw = nil
	return selected, nil
}

// judge is Score or ScoreWithInstructions on the configured judge.
func (a *Adapter) judge(ctx context.Context, problem string, candidates []string) ([]float64, error) {
	return a.cfg.Judge.Score(ctx, problem, candidates)
}

// answerPriors scores each distinct bare answer (the path sent to the judge is
// just "Final answer: <a>", mirroring controller.py) and returns a map from
// answer text to its score; nil on any judge error.
func (a *Adapter) answerPriors(ctx context.Context, problem string, texts []string, judgeCalls *int) map[string]float64 {
	seen := map[string]bool{}
	var distinct []string
	for _, text := range texts {
		answer := ExtractAnswer(text)
		if answer != "" && !seen[answer] {
			seen[answer] = true
			distinct = append(distinct, answer)
		}
	}
	if len(distinct) == 0 {
		return nil
	}
	paths := make([]string, len(distinct))
	for i, answer := range distinct {
		paths[i] = "Final answer: " + answer
	}
	scores, err := scoreWithInstructions(ctx, a.cfg.Judge, problem, paths, AnswerPriorInstructions)
	*judgeCalls += len(distinct)
	if err != nil || len(scores) != len(distinct) {
		return nil
	}
	prior := make(map[string]float64, len(distinct))
	for i, answer := range distinct {
		prior[answer] = scores[i]
	}
	return prior
}

func priorAnswerScore(prior map[string]float64, answer string) float64 {
	if score, ok := prior[answer]; ok {
		return score
	}
	return 0.5
}

// branch fires n extra Respond calls on the inner adapter concurrently.
// Failures are ignored; an empty result means every branch failed.
func (a *Adapter) branch(ctx context.Context, req llm.Request, opts llm.RequestOptions, n int) []llm.Response {
	results := make([]llm.Response, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := a.inner.Respond(ctx, req, opts)
			if err == nil {
				results[i] = resp
			}
		}()
	}
	wg.Wait()
	out := results[:0]
	for _, resp := range results {
		if resp.ID != "" || len(resp.Output) > 0 {
			out = append(out, resp)
		}
	}
	return out
}

// isFinalResponse reports whether a response is a finished answer: no tool
// calls and at least one non-empty assistant message.
func isFinalResponse(resp llm.Response) bool {
	hasText := false
	for _, item := range resp.Output {
		switch item.Type {
		case llm.ItemToolCall:
			return false
		case llm.ItemMessage:
			if msg, ok := item.Data.(llm.Message); ok && strings.TrimSpace(msg.Text) != "" {
				hasText = true
			}
		}
	}
	return hasText
}

// responseText renders a response as judge-visible text: assistant message
// text plus `name(arguments)` lines for tool calls (ModeAll).
func responseText(resp llm.Response) string {
	var b strings.Builder
	for _, item := range resp.Output {
		switch data := item.Data.(type) {
		case llm.Message:
			if data.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(data.Text)
			}
		case llm.ToolCall:
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s(%s)", data.Name, data.Arguments)
		}
	}
	return b.String()
}

func argmax(values []float64) int {
	best := 0
	for i := 1; i < len(values); i++ {
		if values[i] > values[best] {
			best = i
		}
	}
	return best
}

func sumUsage(greedy llm.Usage, branches []llm.Response) llm.Usage {
	total := greedy
	for _, resp := range branches {
		u := resp.Usage
		total.InputTokens += u.InputTokens
		total.CachedInputTokens += u.CachedInputTokens
		total.CacheWriteInputTokens += u.CacheWriteInputTokens
		total.OutputTokens += u.OutputTokens
		total.ReasoningTokens += u.ReasoningTokens
	}
	return total
}
