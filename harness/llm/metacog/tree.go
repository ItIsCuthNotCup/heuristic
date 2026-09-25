package metacog

// Sketch-tree mode: instead of racing full extra answers, MetaCog samples
// TreeWidth compact approach sketches per level, the judge picks the most
// promising idea, the next level derives sharper continuations of it, and
// only the winning chain is expanded into the complete answer. Sketches
// are a few sentences each, so the explored space costs a fraction of the
// tokens a full-answer branch would; the judge calls are cheap
// control-plane reads.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

const (
	// SketchInstructions scores an approach sketch, not a finished path.
	SketchInstructions = `Score how promising this approach sketch is for correctly and completely handling the task, on a scale of 0 to 1. 1 means clearly the right plan; 0 means wrong, vague or likely to fail. Judge the idea, not its brevity.`

	// sketchPrompt asks the model for a compact approach outline.
	sketchPrompt = `Before answering fully, outline your approach to the task in 1-3 short sentences. No working, no details, no final answer — just the idea you would pursue.`

	// derivePrompt asks for the next derivation level of a chosen sketch.
	derivePrompt = `Here is a partial approach for the task:

%s

Give one concrete next step that sharpens or continues this approach, in 1-3 short sentences. Do not give the final answer.`

	// expandPrompt turns the winning sketch chain into the full answer.
	expandPrompt = `Work the following plan out fully, step by step, and give the complete final answer:

%s`
)

// tree replaces the full-answer vote when Config.TreeDepth is set: it
// checks the first answer, sketches TreeWidth ideas per level, follows
// the judge's pick deeper for up to TreeDepth levels, and expands the
// winning chain. The expanded leaf replaces the first answer only when
// the judge scores it higher.
func (a *Adapter) tree(ctx context.Context, req llm.Request, opts llm.RequestOptions, greedy llm.Response, problem string, greedyTime time.Duration, event *Event, judgeCalls *int, emit func(), background bool) llm.Response {
	event.Tree = true

	a.setStage("checking the answer")
	s0, err := a.judgeWithin(ctx, problem, []string{responseText(greedy)}, nil)
	*judgeCalls++
	if err != nil || len(s0) != 1 {
		return greedy // judge unavailable: degrade to the plain adapter
	}
	event.GreedyScore = s0[0]
	if s0[0] >= a.cfg.TrustConfidence {
		event.Stopped = true
		emit()
		return greedy
	}

	budget := max(time.Duration(float64(greedyTime)*a.cfg.BranchTimeFactor), a.cfg.MinBranchTime)
	width := min(a.cfg.TreeWidth, a.pathBudget(req))
	var spent []llm.Response // sketch and expansion calls already paid for
	var chain []string       // the judge's winning sketch per level

	for level := 0; level < a.cfg.TreeDepth && ctx.Err() == nil; level++ {
		prompt := sketchPrompt
		if level > 0 {
			prompt = fmt.Sprintf(derivePrompt, chain[level-1])
		}
		if level == 0 {
			a.setStage(fmt.Sprintf("sketching %d thought paths", width))
		} else {
			a.setStage(fmt.Sprintf("sketching %d continuations of the best idea", width))
		}
		texts, resps := a.sketches(ctx, req, opts, prompt, width, budget)
		spent = append(spent, resps...)
		event.Branches += len(resps)
		if len(texts) == 0 {
			break
		}
		instructions := SketchInstructions
		scores, serr := a.judgeWithin(ctx, problem, texts, &instructions)
		*judgeCalls += len(texts)
		if serr != nil || len(scores) != len(texts) {
			break
		}
		pick := argmax(scores)
		if level == 0 {
			event.Paths = texts
			event.Scores = scores
			event.Chosen = pick
		}
		chain = append(chain, texts[pick])
		if scores[pick] >= a.cfg.TreeConfidence {
			break // confident in the plan: expand it
		}
	}
	event.PoolSize = len(event.Paths)
	event.ExtraUsage = sumUsage(llm.Usage{}, spent)

	finish := func(resp llm.Response) llm.Response {
		usage := sumUsage(greedy.Usage, spent)
		usage.Raw = nil
		emit()
		resp.Usage = usage
		return resp
	}

	if len(chain) == 0 {
		return finish(greedy)
	}

	a.setStage("expanding the best thought path")
	expanded, err := a.inner.Respond(ctx, expandRequest(req, chain), opts)
	if err == nil {
		spent = append(spent, expanded)
	}
	if err != nil || !isFinalResponse(expanded) {
		return finish(greedy)
	}
	expandedText := responseText(expanded)

	// The expanded leaf replaces the first answer only when the judge
	// scores it higher; expanding a mediocre plan must not beat it.
	sExp, err := a.judgeWithin(ctx, problem, []string{expandedText}, nil)
	*judgeCalls++
	if err != nil || len(sExp) != 1 || sExp[0] <= s0[0] {
		return finish(greedy)
	}
	event.Answer = expandedText
	if background && ctx.Err() == nil {
		a.UsePath(responseText(greedy), expandedText)
		event.switched = true
	}
	return finish(expanded)
}

// sketches samples n compact approach outlines for prompt at once. Texts
// are the judgeable (final) ones; resps carries every call for usage.
// Tool schemas are stripped: an outline never needs to act.
func (a *Adapter) sketches(ctx context.Context, req llm.Request, opts llm.RequestOptions, prompt string, n int, budget time.Duration) (texts []string, resps []llm.Response) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	sketchReq := appendUser(req, prompt)
	sketchReq.Tools = nil
	// Outlines must stay cheap: a sketch at the routed effort still does a
	// full internal reasoning pass, which is the cost the tree exists to
	// avoid. Only the final expansion earns the model's full effort.
	sketchReq.Model.ReasoningEffort = llm.ReasoningEffortLow
	results := make([]llm.Response, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := a.inner.Respond(ctx, sketchReq, opts)
			if err == nil {
				results[i] = resp
			}
		}()
	}
	wg.Wait()
	for _, resp := range results {
		if resp.ID == "" && len(resp.Output) == 0 {
			continue
		}
		resps = append(resps, resp)
		if text := responseText(resp); isFinalResponse(resp) && strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return texts, resps
}

// appendUser returns req with one extra trailing user message; the sketch,
// derive and expand instructions ride on it.
func appendUser(req llm.Request, text string) llm.Request {
	input := make([]llm.Item, len(req.Input), len(req.Input)+1)
	copy(input, req.Input)
	req.Input = append(input, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}})
	return req
}

// expandRequest asks the model to work the chosen sketch chain into the
// full answer.
func expandRequest(req llm.Request, chain []string) llm.Request {
	var plan strings.Builder
	for i, sketch := range chain {
		fmt.Fprintf(&plan, "%d. %s\n", i+1, sketch)
	}
	req = appendUser(req, fmt.Sprintf(expandPrompt, strings.TrimSpace(plan.String())))
	req.Tools = nil // the tree only runs on turns answered without tools
	return req
}
