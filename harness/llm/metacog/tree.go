package metacog

// Concise-path fusion mode (HEURISTIC_TREE): instead of racing full-length
// extra answers, MetaCog races TreeWidth concise candidate answers per round
// (each told to stay under ~1000 tokens). The judge's pick is used outright
// once it scores at or above TreeConfidence; when no candidate clears the
// bar after TreeDepth rounds, one merge call steals the strongest parts of
// the best candidates into a single answer, verified against the first
// answer by the judge. Paths and the merge run without tool schemas — the
// tree only runs on turns answered in prose.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

const (
	// concisePrompt bounds the answer text of every candidate path.
	concisePrompt = `Answer the task now — directly and concisely, in at most about 1000 tokens. No preamble, no restating the question.`

	// fusePrompt merges the top candidates into one answer.
	fusePrompt = `Here are %d candidate answers to the same task. None is clearly the best. Take the strongest parts of each and write one concise final answer — at most about 1000 tokens. Only the answer, no commentary on the merge.

%s`
)

// fuseTop caps how many candidates feed the merge call.
const fuseTop = 3

// tree replaces the full-answer vote when Config.TreeDepth is set. The
// judge's choice is trusted whenever a candidate — the first answer or a
// path — reaches TreeConfidence; below that, each round tries a fresh set
// of concise paths, and a merge call fuses the best of them as the last
// resort. A fused answer replaces the first answer only when the judge
// scores it higher.
func (a *Adapter) tree(ctx context.Context, req llm.Request, opts llm.RequestOptions, greedy llm.Response, problem string, greedyTime time.Duration, event *Event, judgeCalls *int, emit func(), background bool) llm.Response {
	event.Tree = true
	event.Chosen = -1 // a path index only when one is actually used

	a.setStage("checking the answer")
	s0, err := a.judgeWithin(ctx, problem, []string{responseText(greedy)}, nil)
	*judgeCalls++
	if err != nil || len(s0) != 1 {
		return greedy // judge unavailable: degrade to the plain adapter
	}
	event.GreedyScore = s0[0]
	if s0[0] >= a.cfg.TreeConfidence {
		event.Stopped = true
		emit()
		return greedy
	}

	budget := max(time.Duration(float64(greedyTime)*a.cfg.BranchTimeFactor), a.cfg.MinBranchTime)
	width := min(a.cfg.TreeWidth, a.pathBudget(req))
	var spent []llm.Response // path and merge calls already paid for
	var texts []string       // every candidate path, all rounds
	var scores []float64     // each candidate's judge score

	finish := func(resp llm.Response) llm.Response {
		usage := sumUsage(greedy.Usage, spent)
		usage.Raw = nil
		emit()
		resp.Usage = usage
		return resp
	}

	for round := 0; round < a.cfg.TreeDepth && ctx.Err() == nil; round++ {
		a.setStage(fmt.Sprintf("trying %d concise thought paths", width))
		fresh, resps := a.candidates(ctx, req, opts, concisePrompt, width, budget)
		spent = append(spent, resps...)
		event.Branches += len(resps)
		if len(fresh) == 0 {
			break
		}
		roundScores, serr := a.judgeWithin(ctx, problem, fresh, nil)
		*judgeCalls += len(fresh)
		if serr != nil || len(roundScores) != len(fresh) {
			break
		}
		texts = append(texts, fresh...)
		scores = append(scores, roundScores...)
		event.Paths = texts
		event.Scores = scores
		if best := argmax(scores); scores[best] >= a.cfg.TreeConfidence {
			event.Chosen = best
			event.PoolSize = len(texts)
			if background && ctx.Err() == nil {
				a.UsePath(responseText(greedy), texts[best])
				event.switched = true
			}
			return finish(candidateResponse(spent, texts[best]))
		}
	}
	event.PoolSize = len(event.Paths)

	best := -1
	bestScore := 0.0
	if len(scores) > 0 {
		best = argmax(scores)
		bestScore = scores[best]
	}

	// No candidate cleared the bar: fuse the strongest parts of the top
	// candidates into one concise answer and let the judge verify it.
	if len(texts) > 0 {
		a.setStage("merging the best parts of the thought paths")
		fused, ferr := a.inner.Respond(ctx, fuseRequest(req, texts, scores), opts)
		if ferr == nil && isFinalResponse(fused) {
			spent = append(spent, fused)
			fusedText := responseText(fused)
			sFused, jerr := a.judgeWithin(ctx, problem, []string{fusedText}, nil)
			*judgeCalls++
			if jerr == nil && len(sFused) == 1 && sFused[0] > s0[0] && sFused[0] >= bestScore {
				event.Answer = fusedText
				event.Chosen = best
				if background && ctx.Err() == nil {
					a.UsePath(responseText(greedy), fusedText)
					event.switched = true
				}
				return finish(fused)
			}
		} else if ferr == nil {
			spent = append(spent, fused)
		}
	}

	// A path still beat the first answer even without clearing the bar.
	if bestScore > s0[0] {
		event.Chosen = best
		if background && ctx.Err() == nil {
			a.UsePath(responseText(greedy), texts[best])
			event.switched = true
		}
		return finish(candidateResponse(spent, texts[best]))
	}
	return finish(greedy)
}

// candidateResponse finds the response carrying text among the spent calls.
func candidateResponse(spent []llm.Response, text string) llm.Response {
	for _, resp := range spent {
		if responseText(resp) == text {
			return resp
		}
	}
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}
}

// candidates samples n concise answers for prompt at once. Texts are the
// judgeable (final) ones; resps carries every call for usage. Tool schemas
// are stripped: a path under judgment never needs to act.
func (a *Adapter) candidates(ctx context.Context, req llm.Request, opts llm.RequestOptions, prompt string, n int, budget time.Duration) (texts []string, resps []llm.Response) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	pathReq := appendUser(req, prompt)
	pathReq.Tools = nil
	results := make([]llm.Response, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := a.inner.Respond(ctx, pathReq, opts)
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

// fuseRequest asks the model to merge the top-scoring candidates into one
// concise final answer.
func fuseRequest(req llm.Request, texts []string, scores []float64) llm.Request {
	top := fuseTop
	if len(texts) < top {
		top = len(texts)
	}
	// top indices by score, descending
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	for i := 0; i < len(idx); i++ {
		for j := i + 1; j < len(idx); j++ {
			if scores[idx[j]] > scores[idx[i]] {
				idx[i], idx[j] = idx[j], idx[i]
			}
		}
	}
	var b strings.Builder
	for i := 0; i < top; i++ {
		fmt.Fprintf(&b, "Candidate %d:\n%s\n\n", i+1, texts[idx[i]])
	}
	req = appendUser(req, fmt.Sprintf(fusePrompt, top, strings.TrimSpace(b.String())))
	req.Tools = nil // the tree only runs on turns answered without tools
	return req
}

// appendUser returns req with one extra trailing user message; the concise
// and merge instructions ride on it.
func appendUser(req llm.Request, text string) llm.Request {
	input := make([]llm.Item, len(req.Input), len(req.Input)+1)
	copy(input, req.Input)
	req.Input = append(input, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}})
	return req
}
