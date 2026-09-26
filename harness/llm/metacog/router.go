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
// around a turn (effort routing, context pruning, tool gating) that would
// otherwise cost thinker tokens.
type NoulJudge interface {
	Noul(ctx context.Context, problem, subject, instructions string) (float64, error)
}

// TrivialInstructions asks whether a request needs no real reasoning at
// all; such requests skip the judge and every thought path. Binary noul
// questions calibrate far better at the extremes than a continuous
// difficulty score does.
const TrivialInstructions = `Is the latest request trivial — a greeting, small talk, thanks, a simple fact or arithmetic question that needs no extended reasoning and no tools? 1 means trivial, 0 means real work.`

// HardInstructions asks whether a request needs the heaviest reasoning the
// model can do.
const HardInstructions = `Does the latest request need heavy reasoning — multi-step debugging, math or proofs, planning, or a change spanning several files? 1 means hard, 0 means ordinary or light work.`

// PruneInstructions asks whether an old transcript chunk still matters.
const PruneInstructions = `Score how much this earlier piece of the conversation still matters for correctly completing the LATEST request, on a scale of 0 to 1. 1 means it is needed: a stated constraint, file content or prior findings the task depends on. 0 means safe to drop: unrelated output, stale logs, finished exploration.`

// NeedsToolsInstructions asks whether a request needs tool access at all.
const NeedsToolsInstructions = `Score whether the latest request requires the agent to use tools — read or write files, run shell commands, search, or inspect the project — rather than reply in prose alone. 1 means it does; 0 means a pure conversational answer suffices.`

// Routing thresholds on the binary nouls, calibrated live against Jev: a
// trivial score at or above this means skip extra paths entirely; a hard
// score at or above this keeps the configured effort and full path count.
const (
	trivialScore = 0.9
	hardScore    = 0.7
	mediumPaths  = 2 // extra paths for requests that are neither
)

// Prune score thresholds: at or above pruneKeepScore the chunk stays
// verbatim, at or above pruneHeadScore only its head survives, below both
// it is stubbed entirely.
const (
	pruneKeepScore = 0.5
	pruneHeadScore = 0.2
	pruneHeadChars = 400
)

// routeFor maps the trivial/hard scores to an effort and a path budget.
func routeFor(trivial, hard float64) routedEffort {
	switch {
	case trivial >= trivialScore:
		return routedEffort{effort: llm.ReasoningEffortLow, paths: 0}
	case hard >= hardScore:
		return routedEffort{effort: "", paths: -1}
	default:
		return routedEffort{effort: llm.ReasoningEffortMedium, paths: mediumPaths}
	}
}

var effortRank = map[llm.ReasoningEffort]int{
	llm.ReasoningEffortLow:    1,
	llm.ReasoningEffortMedium: 2,
	llm.ReasoningEffortHigh:   3,
	llm.ReasoningEffortXHigh:  4,
	llm.ReasoningEffortMax:    5,
}

// routedEffort is the cached control-plane answer for one user message:
// the most reasoning effort it needs ("" keeps the configured level) and
// the extra thought paths it may race (-1 uses the configured default).
type routedEffort struct {
	effort llm.ReasoningEffort
	paths  int
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

// recentUserTexts returns up to the last n user messages, oldest first; it
// is the "goal" the prune questions are asked against.
func recentUserTexts(req llm.Request, n int) []string {
	var texts []string
	for i := len(req.Input) - 1; i >= 0 && len(texts) < n; i-- {
		if msg, ok := req.Input[i].Data.(llm.Message); ok && msg.Role == llm.RoleUser && strings.TrimSpace(msg.Text) != "" {
			texts = append([]string{truncatePath(msg.Text, 2000)}, texts...)
		}
	}
	return texts
}

func textHash(text string) [32]byte { return sha256.Sum256([]byte(text)) }

// truncateHead keeps the first maxChars runes of text plus an ellipsis;
// truncatePath keeps the tail.
func truncateHead(text string, maxChars int) string {
	runes := []rune(text)
	if maxChars <= 0 || len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + "…"
}

// prepare runs the cheap Jev control plane before the thinker call: it
// lowers reasoning effort for easy requests, strips tool schemas on turns
// that need none, and prunes transcript chunks the judge finds irrelevant.
// All are cached so the agent loop's repeated Respond calls for one user
// turn only pay once.
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
	if !a.cfg.ToolGateOff && len(req.Tools) > 0 {
		req = a.chatGate(ctx, router, latest, req)
	}
	if a.cfg.PruneChars > 0 {
		req = a.pruneContext(ctx, router, req)
	}
	return req
}

// routedFor asks the judge its two routing questions once per user
// message and caches the answer for effort and path routing. ok is false
// when the router failed.
func (a *Adapter) routedFor(ctx context.Context, router NoulJudge, latest string) (routedEffort, bool) {
	hash := textHash(latest)
	a.mu.Lock()
	routed, ok := a.efforts[hash]
	a.mu.Unlock()
	if ok {
		return routed, true
	}
	scores, err := scoreEach(ctx, 2, []string{TrivialInstructions, HardInstructions}, func(ctx context.Context, instructions string) (float64, error) {
		return router.Noul(ctx, "The request below is the user's latest message.", truncatePath(latest, 8000), instructions)
	})
	if err != nil {
		return routedEffort{}, false
	}
	routed = routeFor(scores[0], scores[1])
	a.mu.Lock()
	if len(a.efforts) > 512 {
		a.efforts = map[[32]byte]routedEffort{}
	}
	a.efforts[hash] = routed
	a.mu.Unlock()
	return routed, true
}

// routed looks up the cached routing answer for the request's latest user
// message; ok is false when it was never scored.
func (a *Adapter) routed(req llm.Request) (routedEffort, bool) {
	latest := latestUserText(req)
	if latest == "" {
		return routedEffort{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	routed, ok := a.efforts[textHash(latest)]
	return routed, ok
}

// trivial reports whether the router already judged this request easy
// enough that extra thought paths are not worth their cost.
func (a *Adapter) trivial(req llm.Request) bool {
	routed, ok := a.routed(req)
	return ok && routed.paths == 0
}

// pathBudget is the most extra thought paths this request may race: the
// router's answers cap branching for easy and moderate requests.
func (a *Adapter) pathBudget(req llm.Request) int {
	if routed, ok := a.routed(req); ok && routed.paths >= 0 {
		return routed.paths
	}
	if a.cfg.Vote > 0 {
		return a.cfg.Vote
	}
	return a.cfg.NMax
}

// routeEffort lowers the model's reasoning effort according to the routed
// answer; it never raises the configured level.
func (a *Adapter) routeEffort(ctx context.Context, router NoulJudge, latest string, req llm.Request) llm.Request {
	routed, ok := a.routedFor(ctx, router, latest)
	if !ok {
		return req
	}
	if routed.effort != "" && effortRank[routed.effort] < effortRank[req.Model.ReasoningEffort] {
		req.Model.ReasoningEffort = routed.effort
	}
	return req
}

// chatGate strips tool schemas on turns the judge believes need no tools —
// schemas are a sizeable constant cost on every call and every path. The
// verdict is cached per user message, and the gate only ever acts on
// confident answers so a borderline call keeps its tools.
func (a *Adapter) chatGate(ctx context.Context, router NoulJudge, latest string, req llm.Request) llm.Request {
	hash := textHash(latest)
	a.mu.Lock()
	needs, ok := a.chats[hash]
	a.mu.Unlock()
	if !ok {
		score, err := router.Noul(ctx, "The request below is the user's latest message.", truncatePath(latest, 8000), NeedsToolsInstructions)
		if err != nil {
			return req
		}
		needs = score >= 0.2
		a.mu.Lock()
		if len(a.chats) > 512 {
			a.chats = map[[32]byte]bool{}
		}
		a.chats[hash] = needs
		a.mu.Unlock()
	}
	if !needs {
		req.Tools = nil
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

// contextSkeleton renders the conversation the way fast-jev-compaction
// shows it to the judge: one line per item, tool results collapsed to a
// size note. It lets keep/drop verdicts see the whole exchange, not just
// the latest message. Items at index >= upto are left out.
func contextSkeleton(req llm.Request, upto int) string {
	var lines []string
	for i := 0; i < upto && i < len(req.Input); i++ {
		item := req.Input[i]
		switch data := item.Data.(type) {
		case llm.Message:
			text := strings.Join(strings.Fields(data.Text), " ")
			lines = append(lines, fmt.Sprintf("%s: %s", data.Role, truncatePath(text, 300)))
		case llm.ToolCall:
			lines = append(lines, fmt.Sprintf("tool call: %s(%s)", data.Name, truncatePath(data.Arguments, 120)))
		case llm.ToolResult:
			lines = append(lines, fmt.Sprintf("tool result: ok, %d chars", len(itemText(item))))
		}
	}
	skeleton := strings.Join(lines, "\n")
	for len(skeleton) > 8000 && len(lines) > 2 {
		lines = lines[1:]
		skeleton = strings.Join(lines, "\n")
	}
	return skeleton
}

// pruneVerdict is the cached judge decision for one transcript chunk.
type pruneVerdict uint8

const (
	verdictKeep pruneVerdict = iota
	verdictHead              // keep only the first pruneHeadChars
	verdictStub              // replace with an omission note
)

// pruneContext replaces old, bulky tool outputs the judge finds irrelevant:
// borderline chunks keep their first few hundred characters, dead ones a
// one-line stub. The system item and the last KeepRecent items always stay
// verbatim, and verdicts are cached per chunk so a chunk is judged once,
// not once per turn or per thought path.
func (a *Adapter) pruneContext(ctx context.Context, router NoulJudge, req llm.Request) llm.Request {
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
		verdict, seen := a.verdicts[hash]
		a.mu.Unlock()
		if !seen {
			pending = append(pending, candidate{index: i, hash: hash, text: text})
			continue
		}
		req.Input[i] = pruned(req.Input[i], text, verdict)
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
	problem := "Earlier conversation (tool results abbreviated):\n" + contextSkeleton(req, limit)
	if goal := recentUserTexts(req, 3); len(goal) > 0 {
		problem += "\nLatest request: " + strings.Join(goal, " / ")
	}
	texts := make([]string, len(pending))
	for i := range pending {
		texts[i] = pending[i].text
	}
	scores, err := scoreEach(ctx, 8, texts, func(ctx context.Context, text string) (float64, error) {
		return router.Noul(ctx, problem, truncatePath(text, 16000), PruneInstructions)
	})
	if err != nil {
		// A judge outage must never drop context: keep everything verbatim.
		return req
	}
	a.mu.Lock()
	if len(a.verdicts) > 2048 {
		a.verdicts = map[[32]byte]pruneVerdict{}
	}
	for i, cand := range pending {
		verdict := verdictKeep
		switch {
		case scores[i] >= pruneKeepScore:
		case scores[i] >= pruneHeadScore:
			verdict = verdictHead
		default:
			verdict = verdictStub
		}
		a.verdicts[cand.hash] = verdict
		req.Input[cand.index] = pruned(req.Input[cand.index], cand.text, verdict)
	}
	a.mu.Unlock()
	return req
}

// pruned applies a cached verdict to a tool result item.
func pruned(item llm.Item, text string, verdict pruneVerdict) llm.Item {
	if verdict == verdictKeep {
		return item
	}
	result := item.Data.(llm.ToolResult)
	if verdict == verdictHead {
		result.Output = []llm.ToolResultOutput{{
			Kind:  llm.ToolResultText,
			Value: fmt.Sprintf("%s\n[... remainder of %d characters omitted to save context]", truncateHead(text, pruneHeadChars), len(text)),
		}}
	} else {
		result.Output = []llm.ToolResultOutput{{
			Kind:  llm.ToolResultText,
			Value: fmt.Sprintf("[earlier tool output omitted to save context — %d characters]", len(text)),
		}}
	}
	item.Data = result
	return item
}
