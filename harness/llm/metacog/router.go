package metacog

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

// NoulJudge answers one-shot scored questions about a piece of content.
// *Jev implements it; the adapter uses it for the cheap control-plane work
// around a turn (effort routing, context pruning) that would otherwise
// cost thinker tokens.
type NoulJudge interface {
	Noul(ctx context.Context, problem, subject, instructions string) (float64, error)
}

// EffortInstructions asks how much reasoning a request needs.
const EffortInstructions = `Score how much careful, step-by-step reasoning this request needs on a scale of 0 to 1. 0 means trivial: a greeting, a one-word question, a simple fact, a short command. 0.5 means moderate: summarize a file, write a small function, explain a concept. 1 means hard: subtle debugging, multi-step planning, math, proofs, or a change spanning many files.`

// PruneInstructions asks whether an old transcript chunk still matters.
const PruneInstructions = `Score how much this earlier piece of the conversation still matters for correctly completing the LATEST request, on a scale of 0 to 1. 1 means it is needed: a stated constraint, file content or prior findings the task depends on. 0 means safe to drop: unrelated output, stale logs, finished exploration.`

// effortFor maps a difficulty score to the most reasoning effort it needs.
func effortFor(score float64) llm.ReasoningEffort {
	switch {
	case score < 0.35:
		return llm.ReasoningEffortLow
	case score < 0.7:
		return llm.ReasoningEffortMedium
	default:
		return ""
	}
}

var effortRank = map[llm.ReasoningEffort]int{
	llm.ReasoningEffortLow:    1,
	llm.ReasoningEffortMedium: 2,
	llm.ReasoningEffortHigh:   3,
	llm.ReasoningEffortXHigh:  4,
	llm.ReasoningEffortMax:    5,
}

// latestUserText is the most recent user message, the request every
// control-plane question is about.
func latestUserText(req llm.Request) string {
	for i := len(req.Input) - 1; i >= 0; i-- {
		if msg, ok := req.Input[i].Data.(llm.Message); ok && msg.Role == llm.RoleUser {
			return msg.Text
		}
	}
	return ""
}

func textHash(text string) [32]byte { return sha256.Sum256([]byte(text)) }

// prepare runs the cheap Jev control plane before the thinker call: it
// lowers reasoning effort for easy requests and prunes transcript chunks
// the judge finds irrelevant. Both are cached so the agent loop's repeated
// Respond calls for one user turn only pay once.
func (a *Adapter) prepare(ctx context.Context, req llm.Request) llm.Request {
	router := a.cfg.Router
	if router == nil {
		return req
	}
	latest := latestUserText(req)
	if latest == "" {
		return req
	}
	req = a.routeEffort(ctx, router, latest, req)
	if a.cfg.PruneChars > 0 {
		req = a.pruneContext(ctx, router, latest, req)
	}
	return req
}

// routeEffort asks the judge how hard the latest request is once per user
// message and lowers the model's reasoning effort accordingly; it never
// raises the configured level.
func (a *Adapter) routeEffort(ctx context.Context, router NoulJudge, latest string, req llm.Request) llm.Request {
	hash := textHash(latest)
	a.mu.Lock()
	effort, ok := a.efforts[hash]
	a.mu.Unlock()
	if !ok {
		score, err := router.Noul(ctx, "The request below is the user's latest message.", truncatePath(latest, 8000), EffortInstructions)
		if err != nil {
			return req
		}
		effort = effortFor(score)
		a.mu.Lock()
		if len(a.efforts) > 512 {
			a.efforts = map[[32]byte]llm.ReasoningEffort{}
		}
		a.efforts[hash] = effort
		a.mu.Unlock()
	}
	if effort != "" && effortRank[effort] < effortRank[req.Model.ReasoningEffort] {
		req.Model.ReasoningEffort = effort
	}
	return req
}

// itemText measures roughly how much of the transcript an item takes.
func itemText(item llm.Item) string {
	switch data := item.Data.(type) {
	case llm.Message:
		return data.Text
	case llm.ToolResult:
		var text strings.Builder
		for _, out := range data.Output {
			text.WriteString(out.Value)
		}
		return text.String()
	default:
		return ""
	}
}

func requestChars(req llm.Request) int {
	total := 0
	for _, item := range req.Input {
		total += len(itemText(item))
	}
	return total
}

// pruneContext replaces old, bulky tool outputs the judge finds irrelevant
// with a short stub. The system item and the last KeepRecent items always
// stay verbatim, and keep/drop verdicts are cached per chunk so a chunk is
// judged once, not once per turn.
func (a *Adapter) pruneContext(ctx context.Context, router NoulJudge, latest string, req llm.Request) llm.Request {
	if requestChars(req) <= a.cfg.PruneChars {
		return req
	}
	const minChunkChars = 512
	type candidate struct {
		index int
		hash  [32]byte
		text  string
	}
	limit := len(req.Input) - a.cfg.KeepRecent
	if limit < 0 {
		limit = 0
	}
	var pending []candidate
	for i := 1; i < limit; i++ {
		item := req.Input[i]
		if item.Type != llm.ItemToolResult {
			continue
		}
		text := itemText(item)
		if len(text) < minChunkChars {
			continue
		}
		hash := textHash(text)
		a.mu.Lock()
		keep, seen := a.verdicts[hash]
		a.mu.Unlock()
		if !seen {
			pending = append(pending, candidate{index: i, hash: hash, text: text})
			continue
		}
		if !keep {
			req.Input[i] = stubbed(item, len(text))
		}
	}
	if len(pending) == 0 {
		return req
	}
	// Judge at most the 16 largest new chunks per call; the rest wait for
	// the next turn rather than holding the answer up.
	sort.Slice(pending, func(i, j int) bool { return len(pending[i].text) > len(pending[j].text) })
	if len(pending) > 16 {
		pending = pending[:16]
	}
	a.setStage("checking which earlier context still matters")
	defer a.setStage("")
	texts := make([]string, len(pending))
	for i := range pending {
		texts[i] = pending[i].text
	}
	scores, err := scoreEach(ctx, 8, texts, func(ctx context.Context, text string) (float64, error) {
		return router.Noul(ctx, "Latest request: "+truncatePath(latest, 8000), truncatePath(text, 16000), PruneInstructions)
	})
	if err != nil {
		// A judge outage must never drop context: keep everything verbatim.
		return req
	}
	a.mu.Lock()
	for i, cand := range pending {
		keep := scores[i] >= 0.5
		a.verdicts[cand.hash] = keep
		if !keep {
			req.Input[cand.index] = stubbed(req.Input[cand.index], len(cand.text))
		}
	}
	a.mu.Unlock()
	return req
}

func stubbed(item llm.Item, chars int) llm.Item {
	result := item.Data.(llm.ToolResult)
	result.Output = []llm.ToolResultOutput{{
		Kind:  llm.ToolResultText,
		Value: fmt.Sprintf("[earlier tool output omitted to save context — %d characters]", chars),
	}}
	item.Data = result
	return item
}
