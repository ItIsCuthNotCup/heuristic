package metacog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const logprobSystemPrompt = "You are a strict grader. Answer with exactly one word: yes or no."

// yesTokenFamily reports whether a top-logprob token is a yes/no variant.
// Equivalent to the YES_TOKENS/NO_TOKENS membership test in local_judge.py:
// those sets cover every capitalisation/leading-space variant, which is what
// this normalisation (strip whitespace and sentencepiece "▁", lowercase)
// reduces to.
func tokenFamily(token string) string {
	norm := strings.ToLower(strings.Trim(token, " \t\r\n▁"))
	switch norm {
	case "yes":
		return "yes"
	case "no":
		return "no"
	}
	return ""
}

// yesProbability is the port of local_judge.py yes_probability:
// P(yes) / (P(yes) + P(no)) over the yes/no token variants in the top-k
// logprobs; 0.5 when neither family is present.
func yesProbability(logprobs map[string]float64) float64 {
	var pYes, pNo float64
	for token, lp := range logprobs {
		switch tokenFamily(token) {
		case "yes":
			pYes += math.Exp(lp)
		case "no":
			pNo += math.Exp(lp)
		}
	}
	if pYes+pNo == 0.0 {
		return 0.5
	}
	return pYes / (pYes + pNo)
}

// LogprobConfig configures a local/open judge served by any OpenAI-compatible
// endpoint that returns top_logprobs (vLLM, llama-server). Port of
// LogitJudge.openai_compat.
type LogprobConfig struct {
	BaseURL     string // required; a trailing /v1 is stripped
	APIKey      string // optional
	Model       string
	Concurrency int           // default 1 (local servers are usually serial)
	MaxChars    int           // default 24000
	TopLogprobs int           // default 20
	Timeout     time.Duration // default 60s
	HTTP        *http.Client
}

type Logprob struct {
	cfg LogprobConfig
}

func NewLogprob(cfg LogprobConfig) *Logprob {
	cfg.BaseURL = strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1")
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = 24000
	}
	if cfg.TopLogprobs <= 0 {
		cfg.TopLogprobs = 20
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: cfg.Timeout}
	}
	return &Logprob{cfg: cfg}
}

func (l *Logprob) Score(ctx context.Context, problem string, candidates []string) ([]float64, error) {
	return l.ScoreWithInstructions(ctx, problem, candidates, DefaultScoreInstructions)
}

func (l *Logprob) ScoreWithInstructions(ctx context.Context, problem string, candidates []string, instructions string) ([]float64, error) {
	if instructions == "" {
		instructions = DefaultScoreInstructions
	}
	return scoreEach(ctx, l.cfg.Concurrency, candidates, func(ctx context.Context, cand string) (float64, error) {
		path := truncatePath(cand, l.cfg.MaxChars)
		user := fmt.Sprintf("%s\n\nproblem:\n%s\n\npath:\n%s\n\nAnswer yes or no.", instructions, problem, path)
		logprobs, err := l.nextTokenLogprobs(ctx, logprobSystemPrompt, user)
		if err != nil {
			return 0, err
		}
		return yesProbability(logprobs), nil
	})
}

func (l *Logprob) nextTokenLogprobs(ctx context.Context, system, user string) (map[string]float64, error) {
	body := map[string]any{
		"model": l.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"max_tokens":   1,
		"temperature":  0,
		"logprobs":     true,
		"top_logprobs": l.cfg.TopLogprobs,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.cfg.APIKey)
	}
	resp, err := l.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &JudgeError{fmt.Sprintf("HTTP %d: %s", resp.StatusCode, data)}
	}
	var parsed struct {
		Choices []struct {
			Logprobs struct {
				Content []struct {
					TopLogprobs []struct {
						Token   string  `json:"token"`
						Logprob float64 `json:"logprob"`
					} `json:"top_logprobs"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, &JudgeError{fmt.Sprintf("decode chat completions response: %v", err)}
	}
	if len(parsed.Choices) == 0 || len(parsed.Choices[0].Logprobs.Content) == 0 {
		return nil, &JudgeError{"server did not return top_logprobs (logprobs unsupported?)"}
	}
	out := make(map[string]float64, len(parsed.Choices[0].Logprobs.Content[0].TopLogprobs))
	for _, t := range parsed.Choices[0].Logprobs.Content[0].TopLogprobs {
		out[t.Token] = t.Logprob
	}
	return out, nil
}
