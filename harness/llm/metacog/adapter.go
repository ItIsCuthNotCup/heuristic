package metacog

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
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
	// Stage, if set, is told what MetaCog is doing ("" when it is done).
	Stage func(string)
	// Background returns the first answer at once and checks it
	// afterwards; a better path is applied to later turns with UsePath and
	// reported through Trace. A new Respond cancels a check still running.
	Background bool
	// Extra thought paths get BranchTimeFactor × the first answer's time,
	// but at least MinBranchTime; paths still running then are dropped.
	BranchTimeFactor float64       // default 1.5
	MinBranchTime    time.Duration // default 20s
	JudgeTimeout     time.Duration // per judging step, default 30s
	// Vote runs this many extra thought paths at once after a final
	// answer and stops as soon as two paths agree; the judge only picks
	// when none do. Default 3; < 0 uses the v0.3 judge-first loop.
	Vote int
	// TrustConfidence skips the vote when the judge scores the first
	// answer at least this high. Default 0.98.
	TrustConfidence float64
	// SameThreshold is the SameJudge score from which two paths without a
	// stated final answer count as agreeing. Default 0.7.
	SameThreshold float64
	// Router is the cheap judge used for control-plane work around a turn:
	// it lowers reasoning effort for easy requests, decides which old
	// transcript chunks may be stubbed, and may strip tool schemas on
	// pure-chat turns. Defaults to Judge when it implements NoulJudge;
	// RouterOff disables routing, pruning and the tool gate.
	Router    NoulJudge
	RouterOff bool
	// ToolGateOff keeps every request's tool schemas verbatim: when set,
	// the router no longer strips them on turns it judges pure chat.
	ToolGateOff bool
	// PruneChars replaces old tool outputs the router finds irrelevant
	// with stubs once the request transcript grows past this many
	// characters. Default 48000; <= 0 disables. The system message and the
	// last KeepRecent items always stay verbatim (default 12).
	PruneChars int
	KeepRecent int
	// TreeDepth switches the unsure-answer branch from racing full
	// thought paths (Vote) to a sketch tree: TreeWidth compact approach
	// sketches per level, the judge picks the most promising, and the
	// winning chain is expanded into the answer after at most TreeDepth
	// levels of derivation. Default 0 (off); 2-3 is the useful range.
	TreeDepth int
	// TreeWidth is how many sketches each level samples. Default 3.
	TreeWidth int
	// TreeConfidence expands the chain early once the level's best sketch
	// reaches this judge score. Default 0.9.
	TreeConfidence float64
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
	if c.BranchTimeFactor <= 0 {
		c.BranchTimeFactor = 1.5
	}
	if c.MinBranchTime <= 0 {
		c.MinBranchTime = 20 * time.Second
	}
	if c.JudgeTimeout <= 0 {
		c.JudgeTimeout = 30 * time.Second
	}
	if c.Vote == 0 {
		c.Vote = 3
	}
	if c.TrustConfidence <= 0 {
		c.TrustConfidence = 0.98
	}
	if c.SameThreshold <= 0 {
		c.SameThreshold = 0.7
	}
	if c.PruneChars == 0 {
		c.PruneChars = 48000
	}
	if c.KeepRecent <= 0 {
		c.KeepRecent = 12
	}
	if c.TreeWidth <= 0 {
		c.TreeWidth = 3
	}
	if c.TreeConfidence <= 0 {
		c.TreeConfidence = 0.9
	}
	if c.Router == nil && !c.RouterOff {
		if router, ok := c.Judge.(NoulJudge); ok {
			c.Router = router
		}
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
	// Skipped reports the router judged the request trivial, so the first
	// answer was returned without judging or extra thought paths.
	Skipped    bool  `json:"skipped,omitempty"`
	DurationMs int64 `json:"duration_ms"`
	// Paths holds the text of every thought path in pool order (the first
	// answer, then the branches), aligned with Scores.
	Paths []string `json:"-"`
	// Final reports that every path is a final answer (no tool calls), so
	// the conversation can continue from any of them.
	Final bool `json:"-"`
	// Agree marks, per path, the paths that reached the chosen answer when
	// a vote decided; Scores is empty then.
	Agree []bool `json:"agree,omitempty"`
	// Background reports that the first answer was already returned and
	// this event is the later check of it.
	Background bool `json:"background,omitempty"`
	// ExtraUsage is what the extra thought paths cost.
	ExtraUsage llm.Usage `json:"-"`
	// Tree reports the decision came from the sketch tree: Paths holds
	// the level-1 approach sketches and Chosen the branch that was
	// expanded. Tree paths are read-only — there is no finished alternate
	// answer to continue from.
	Tree bool `json:"tree,omitempty"`
	// Answer is the expanded final answer when a tree leaf beat the first
	// answer (background mode shows it).
	Answer string `json:"-"`

	switched bool
}

// Adapter wraps an inner llm.Adapter with the MetaCog adaptive loop: score
// the first ("greedy") answer, branch n extra paths scaled by the judge's
// uncertainty when the score is below StopConfidence, and return the best.
type Adapter struct {
	inner llm.Adapter
	cfg   Config

	mu       sync.Mutex
	swaps    map[string]string
	check    func()                    // cancels the running background check
	efforts  map[[32]byte]routedEffort // routed difficulty per user message
	verdicts map[[32]byte]pruneVerdict // prune keep/head/stub per chunk
	chats    map[[32]byte]bool         // needs-tools verdict per user message
}

func New(inner llm.Adapter, cfg Config) *Adapter {
	return &Adapter{inner: inner, cfg: cfg.withDefaults(), efforts: map[[32]byte]routedEffort{}, verdicts: map[[32]byte]pruneVerdict{}, chats: map[[32]byte]bool{}}
}

var _ llm.Adapter = (*Adapter)(nil)

// UsePath makes later turns continue from replacement instead of the answer
// MetaCog picked: every assistant message whose text is original is sent to
// the model as replacement.
func (a *Adapter) UsePath(original, replacement string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.swaps == nil {
		a.swaps = map[string]string{}
	}
	a.swaps[original] = replacement
}

func (a *Adapter) applySwaps(req llm.Request) llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.swaps) == 0 {
		return req
	}
	input := make([]llm.Item, len(req.Input))
	for i, item := range req.Input {
		if msg, ok := item.Data.(llm.Message); ok && msg.Role == llm.RoleAssistant {
			if replacement, ok := a.swaps[msg.Text]; ok {
				msg.Text = replacement
				item.Data = msg
			}
		}
		input[i] = item
	}
	req.Input = input
	return req
}

func (a *Adapter) Respond(ctx context.Context, req llm.Request, opts llm.RequestOptions) (llm.Response, error) {
	a.cancelCheck()
	req = a.applySwaps(req)
	req = a.prepare(ctx, req)
	start := time.Now()
	greedy, err := a.inner.Respond(ctx, req, opts)
	if err != nil {
		return greedy, err
	}
	if a.cfg.Mode == ModeOff || a.cfg.Judge == nil {
		return greedy, nil
	}
	if a.cfg.Mode == ModeFinal && !isFinalResponse(greedy) {
		// Routine agent tool step: pass through with zero judge overhead.
		return greedy, nil
	}
	if a.cfg.Background && isFinalResponse(greedy) {
		checkCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		done := make(chan struct{})
		a.mu.Lock()
		a.check = func() {
			cancel()
			<-done
		}
		a.mu.Unlock()
		go func() {
			defer close(done)
			defer cancel()
			a.think(checkCtx, req, opts, greedy, start, true)
		}()
		return greedy, nil
	}
	return a.think(ctx, req, opts, greedy, start, false), nil
}

// cancelCheck stops a background check that is still running, so a new turn
// never waits for it.
func (a *Adapter) cancelCheck() {
	a.mu.Lock()
	check := a.check
	a.check = nil
	a.mu.Unlock()
	if check != nil {
		check()
	}
}

func (a *Adapter) setStage(stage string) {
	if a.cfg.Stage != nil {
		a.cfg.Stage(stage)
	}
}

// think runs the MetaCog loop on greedy and returns the chosen response with
// the usage of every inner call. In background mode greedy has already been
// returned to the caller, so a better path is applied with UsePath.
func (a *Adapter) think(ctx context.Context, req llm.Request, opts llm.RequestOptions, greedy llm.Response, start time.Time, background bool) llm.Response {
	event := Event{Turn: opts.CacheKey, Chosen: 0, Background: background}
	judgeCalls := 0
	emit := func() {
		if a.cfg.Trace != nil && (ctx.Err() == nil || event.switched) {
			event.JudgeCalls = judgeCalls
			event.DurationMs = time.Since(start).Milliseconds()
			a.cfg.Trace(event)
		}
	}
	greedyTime := time.Since(start)
	defer a.setStage("")

	problem := BuildProblem(req, a.cfg.MaxProblemChars)
	if a.trivial(req) {
		// The router already judged this request easy: nothing to gain
		// from extra thought paths, so skip the judge and the vote.
		event.Stopped = true
		event.Skipped = true
		emit()
		return greedy
	}
	if a.cfg.TreeDepth > 0 && isFinalResponse(greedy) {
		return a.tree(ctx, req, opts, greedy, problem, greedyTime, &event, &judgeCalls, emit, background)
	}
	if a.cfg.Vote > 0 && isFinalResponse(greedy) {
		return a.vote(ctx, req, opts, greedy, problem, greedyTime, &event, &judgeCalls, emit, background)
	}
	pool := []llm.Response{greedy}

	a.setStage("checking the answer")
	s0, err := a.judgeWithin(ctx, problem, []string{responseText(greedy)}, nil)
	judgeCalls++
	if err != nil {
		return greedy // judge unavailable: degrade to the plain adapter
	}
	event.GreedyScore = s0[0]
	if s0[0] >= a.cfg.StopConfidence {
		event.Stopped = true
		emit()
		return greedy
	}

	// Branching scales with the judge's uncertainty u = 1 - s0, the same
	// formula as controller.py _adaptive.
	u := 1.0 - s0[0]
	n := int(math.Round(float64(a.cfg.NMin) + u*float64(a.cfg.NMax-a.cfg.NMin)))
	if n < 1 {
		n = 1
	}
	if extra := a.pathBudget(req); n > extra {
		n = extra
	}
	event.Branches = n

	a.setStage(fmt.Sprintf("trying %d more thought paths", n))
	budget := max(time.Duration(float64(greedyTime)*a.cfg.BranchTimeFactor), a.cfg.MinBranchTime)
	branches := a.branch(ctx, req, opts, n, budget)
	for _, resp := range branches {
		if a.cfg.Mode == ModeFinal && !isFinalResponse(resp) {
			continue
		}
		pool = append(pool, resp)
	}
	event.PoolSize = len(pool)
	event.ExtraUsage = sumUsage(llm.Usage{}, branches)
	// From here on every return must carry the summed usage of all inner
	// calls, not just greedy's — branch tokens were spent either way.
	usage := sumUsage(greedy.Usage, branches)
	usage.Raw = nil
	if len(pool) == 1 {
		emit()
		greedy.Usage = usage
		return greedy
	}

	chosen := a.judgePool(ctx, problem, pool, &event, &judgeCalls, background)
	emit()
	selected := pool[max(chosen, 0)]
	selected.Usage = usage
	return selected
}

// judgePool scores every path (text plus the answer prior), records the
// scores in event and returns the best path, or -1 when the judge fails. In
// background mode a better path is applied with UsePath.
func (a *Adapter) judgePool(ctx context.Context, problem string, pool []llm.Response, event *Event, judgeCalls *int, background bool) int {
	a.setStage(fmt.Sprintf("judging %d thought paths", len(pool)))
	texts := make([]string, len(pool))
	for i := range pool {
		texts[i] = responseText(pool[i])
	}
	textScores, err := a.judgeWithin(ctx, problem, texts, nil)
	*judgeCalls += len(texts)
	if err != nil || len(textScores) != len(pool) {
		return -1
	}
	event.Scores = textScores
	event.Paths = texts
	event.Final = true
	for _, resp := range pool {
		event.Final = event.Final && isFinalResponse(resp)
	}

	totals := textScores
	if a.cfg.AnswerPrior > 0 {
		prior := a.answerPriors(ctx, problem, texts, judgeCalls)
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
	a.applyChoice(ctx, texts, chosen, event, background)
	return chosen
}

func (a *Adapter) applyChoice(ctx context.Context, texts []string, chosen int, event *Event, background bool) {
	if background && ctx.Err() == nil && chosen > 0 && event.Final {
		a.UsePath(texts[0], texts[chosen])
		event.switched = true
	}
}

// judgeWithin scores candidates, giving up after JudgeTimeout. instructions
// selects ScoreWithInstructions when non-nil.
func (a *Adapter) judgeWithin(ctx context.Context, problem string, candidates []string, instructions *string) ([]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.JudgeTimeout)
	defer cancel()
	if instructions != nil {
		return scoreWithInstructions(ctx, a.cfg.Judge, problem, candidates, *instructions)
	}
	return a.judge(ctx, problem, candidates)
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
	instructions := AnswerPriorInstructions
	scores, err := a.judgeWithin(ctx, problem, paths, &instructions)
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
func (a *Adapter) branch(ctx context.Context, req llm.Request, opts llm.RequestOptions, n int, budget time.Duration) []llm.Response {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
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
