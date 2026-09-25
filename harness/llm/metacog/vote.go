package metacog

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

// SameJudge is an optional Judge extension that scores whether two paths
// reach the same final answer, so paths can vote even when neither states
// its answer in a parseable form.
type SameJudge interface {
	Same(ctx context.Context, problem, a, b string) (float64, error)
}

// SameInstructions is the question SameJudge implementations ask.
const SameInstructions = "Do `a` and `b` reach the same final answer to `problem`? Ignore wording and method."

var statedPattern = regexp.MustCompile(`(?im)^\W*(?:final\s+)?answer\W*[:\-]\s*(.+)$`)

// statedAnswer is a path's explicitly stated final answer (the last
// \boxed{...} or "Answer: ..." line), normalised for comparison; "" when the
// path states none.
func statedAnswer(text string) string {
	var answer string
	if m := boxedPattern.FindAllStringSubmatch(text, -1); len(m) > 0 {
		answer = m[len(m)-1][1]
	} else if m := statedPattern.FindAllStringSubmatch(text, -1); len(m) > 0 {
		answer = m[len(m)-1][1]
	}
	answer = strings.Trim(strings.TrimSpace(answer), "$*`. ")
	return strings.ToLower(strings.Join(strings.Fields(answer), " "))
}

// vote starts Config.Vote extra paths at once and returns the first answer
// two paths agree on, cancelling the rest. While they run the judge scores
// the first answer; at TrustConfidence or above the extra paths are
// cancelled. When no two paths agree the judge picks, as in the v0.3 loop.
func (a *Adapter) vote(ctx context.Context, req llm.Request, opts llm.RequestOptions, greedy llm.Response, problem string, greedyTime time.Duration, event *Event, judgeCalls *int, emit func(), background bool) llm.Response {
	k := min(a.cfg.Vote, a.pathBudget(req))
	event.Branches = k
	budget := max(time.Duration(float64(greedyTime)*a.cfg.BranchTimeFactor), a.cfg.MinBranchTime)
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	a.setStage(fmt.Sprintf("checking with %d more thought paths", k))
	results := make(chan llm.Response, k)
	for range k {
		go func() {
			resp, err := a.inner.Respond(runCtx, req, opts)
			if err != nil {
				resp = llm.Response{}
			}
			results <- resp
		}()
	}
	first := responseText(greedy)
	gate := make(chan float64, 1)
	go func() {
		scores, err := a.judgeWithin(runCtx, problem, []string{first}, nil)
		if err != nil || len(scores) != 1 {
			gate <- -1
			return
		}
		gate <- scores[0]
	}()

	pool := []llm.Response{greedy}
	texts := []string{first}
	answers := []string{statedAnswer(first)}
	var branches []llm.Response
	finish := func(chosen int) llm.Response {
		cancel()
		event.PoolSize = len(pool)
		event.ExtraUsage = sumUsage(llm.Usage{}, branches)
		usage := sumUsage(greedy.Usage, branches)
		usage.Raw = nil
		emit()
		selected := pool[max(chosen, 0)]
		selected.Usage = usage
		return selected
	}

	gateC := gate
	for pending := k; pending > 0; {
		select {
		case score := <-gateC:
			gateC = nil
			*judgeCalls++
			event.GreedyScore = score
			if score >= a.cfg.TrustConfidence {
				event.Stopped = true
				return finish(0)
			}
		case resp := <-results:
			pending--
			if resp.ID == "" && len(resp.Output) == 0 {
				continue
			}
			branches = append(branches, resp)
			if !isFinalResponse(resp) {
				continue
			}
			pool = append(pool, resp)
			texts = append(texts, responseText(resp))
			answers = append(answers, statedAnswer(texts[len(texts)-1]))
			if match := a.agreesWith(runCtx, problem, texts, answers, judgeCalls); match >= 0 {
				event.Paths = texts
				event.Final = true
				event.Agree = make([]bool, len(texts))
				event.Agree[match] = true
				event.Agree[len(texts)-1] = true
				for i := range texts {
					if answers[i] != "" && answers[i] == answers[match] {
						event.Agree[i] = true
					}
				}
				event.Chosen = match
				a.applyChoice(ctx, texts, match, event, background)
				return finish(match)
			}
		}
	}
	if len(pool) == 1 {
		return finish(0)
	}
	return finish(a.judgePool(ctx, problem, pool, event, judgeCalls, background))
}

// agreesWith returns the earliest path that reaches the same answer as the
// newest one, or -1. Stated answers are compared directly; a pair where
// either path states none is asked of the judge when it is a SameJudge.
func (a *Adapter) agreesWith(ctx context.Context, problem string, texts, answers []string, judgeCalls *int) int {
	last := len(texts) - 1
	same, _ := a.cfg.Judge.(SameJudge)
	matches := make([]bool, last)
	var wg sync.WaitGroup
	for i := range last {
		switch {
		case answers[last] != "" && answers[i] != "":
			matches[i] = answers[last] == answers[i]
		case same != nil:
			*judgeCalls++
			wg.Add(1)
			go func() {
				defer wg.Done()
				jctx, cancel := context.WithTimeout(ctx, a.cfg.JudgeTimeout)
				defer cancel()
				score, err := same.Same(jctx, problem, texts[i], texts[last])
				matches[i] = err == nil && score >= a.cfg.SameThreshold
			}()
		}
	}
	wg.Wait()
	for i, ok := range matches {
		if ok {
			return i
		}
	}
	return -1
}
