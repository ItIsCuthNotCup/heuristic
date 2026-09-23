// Package metacog wraps an llm.Adapter with MetaCog's inference-time
// metacognition loop: a judge scores the model's answer, and when it is not
// confident the adapter samples more reasoning paths in parallel and returns
// the best-scoring one. Ported from https://github.com/ItIsCuthNotCup/MetaCog.
package metacog

import (
	"context"
	"sync"
	"unicode/utf8"
)

// Judge scores candidate reasoning paths. Scores are P(correct) in [0,1].
type Judge interface {
	// Score returns one P(correct) in [0,1] per candidate, same order.
	// Must be safe for concurrent use.
	Score(ctx context.Context, problem string, candidates []string) ([]float64, error)
}

// InstructionJudge is an optional extension a Judge may implement to score
// under a different instruction; the adapter uses it for the answer-prior
// readout (a bare final answer must not be judged as if it were reasoning).
type InstructionJudge interface {
	Judge
	ScoreWithInstructions(ctx context.Context, problem string, candidates []string, instructions string) ([]float64, error)
}

// scoreWithInstructions calls ScoreWithInstructions when implemented and
// falls back to Score otherwise.
func scoreWithInstructions(ctx context.Context, judge Judge, problem string, candidates []string, instructions string) ([]float64, error) {
	if ij, ok := judge.(InstructionJudge); ok {
		return ij.ScoreWithInstructions(ctx, problem, candidates, instructions)
	}
	return judge.Score(ctx, problem, candidates)
}

// Copied verbatim from metacog/judge.py.
const DefaultScoreInstructions = "`path` is one candidate solution or reasoning path for `problem`. Is its final answer " +
	"correct (or, if it is unfinished, is it on track to reach a correct answer)? Judge the " +
	"substance, not the length or style."

const AnswerPriorInstructions = "`path` states only a proposed final answer to `problem`, with no reasoning. " +
	"Is that final answer correct?"

// truncatePath keeps the LAST maxChars runes; the most recent reasoning
// matters (same rule as truncate_path in judge.py).
func truncatePath(text string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	runes := []rune(text)
	return "…" + string(runes[len(runes)-maxChars:])
}

// scoreEach maps f over candidates preserving order with at most concurrency
// workers in flight; the first error cancels the rest and is returned.
func scoreEach(ctx context.Context, concurrency int, candidates []string, f func(context.Context, string) (float64, error)) ([]float64, error) {
	out := make([]float64, len(candidates))
	if len(candidates) <= 1 || concurrency <= 1 {
		for i, cand := range candidates {
			score, err := f(ctx, cand)
			if err != nil {
				return nil, err
			}
			out[i] = score
		}
		return out, nil
	}
	workers := min(concurrency, len(candidates))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	sem := make(chan struct{}, workers)
	for i, cand := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				once.Do(func() { firstErr = ctx.Err() })
				return
			}
			score, err := f(ctx, cand)
			if err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			out[i] = score
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
