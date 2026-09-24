package metacog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// TypeSafeBaseURL is the default Jev endpoint (same as judge.py).
const TypeSafeBaseURL = "https://api.typesafe.ai"

// JudgeError is returned when a judge endpoint answers with HTTP >= 400 or a
// malformed body.
type JudgeError struct{ message string }

func (e *JudgeError) Error() string { return e.message }

// JevConfig configures the System-One (Jev) judge, a port of SystemOneJudge.
type JevConfig struct {
	BaseURL         string        // default https://api.typesafe.ai
	APIKey          string        // default $TYPESAFE_API_KEY for the TypeSafe URL
	Model           string        // default "jev-latest"
	Concurrency     int           // default 8
	MaxCharsPerPath int           // default 24000
	Timeout         time.Duration // default 60s
	HTTP            *http.Client
}

// Jev speaks the POST {BaseURL}/v1/systemone protocol.
type Jev struct {
	cfg JevConfig
}

func NewJev(cfg JevConfig) *Jev {
	if cfg.BaseURL == "" {
		cfg.BaseURL = TypeSafeBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.APIKey == "" && cfg.BaseURL == TypeSafeBaseURL {
		cfg.APIKey = os.Getenv("TYPESAFE_API_KEY")
	}
	if cfg.Model == "" {
		cfg.Model = "jev-latest"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.MaxCharsPerPath <= 0 {
		cfg.MaxCharsPerPath = 24000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: cfg.Timeout}
	}
	return &Jev{cfg: cfg}
}

func (j *Jev) Score(ctx context.Context, problem string, candidates []string) ([]float64, error) {
	return j.score(ctx, problem, candidates, DefaultScoreInstructions)
}

func (j *Jev) ScoreWithInstructions(ctx context.Context, problem string, candidates []string, instructions string) ([]float64, error) {
	if instructions == "" {
		instructions = DefaultScoreInstructions
	}
	return j.score(ctx, problem, candidates, instructions)
}

// score issues one isolated noul request per candidate, concurrently, with
// candidate order preserved (port of SystemOneJudge.score).
func (j *Jev) score(ctx context.Context, problem string, candidates []string, instructions string) ([]float64, error) {
	return scoreEach(ctx, j.cfg.Concurrency, candidates, func(ctx context.Context, cand string) (float64, error) {
		body := map[string]any{
			"model": j.cfg.Model,
			"state": map[string]any{
				"problem": problem,
				"path":    truncatePath(cand, j.cfg.MaxCharsPerPath),
			},
			"questions": map[string]any{
				"is_correct": map[string]any{
					"type":         "noul",
					"instructions": instructions,
				},
			},
		}
		data, err := j.post(ctx, body)
		if err != nil {
			return 0, err
		}
		var parsed struct {
			Answers struct {
				IsCorrect struct {
					Noul float64 `json:"noul"`
				} `json:"is_correct"`
			} `json:"answers"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return 0, &JudgeError{fmt.Sprintf("decode systemone response: %v", err)}
		}
		return parsed.Answers.IsCorrect.Noul, nil
	})
}

// Same asks whether paths a and b reach the same final answer.
func (j *Jev) Same(ctx context.Context, problem, a, b string) (float64, error) {
	half := j.cfg.MaxCharsPerPath / 2
	body := map[string]any{
		"model": j.cfg.Model,
		"state": map[string]any{
			"problem": problem,
			"a":       truncatePath(a, half),
			"b":       truncatePath(b, half),
		},
		"questions": map[string]any{
			"same": map[string]any{"type": "noul", "instructions": SameInstructions},
		},
	}
	data, err := j.post(ctx, body)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Answers struct {
			Same struct {
				Noul float64 `json:"noul"`
			} `json:"same"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return 0, &JudgeError{fmt.Sprintf("decode systemone response: %v", err)}
	}
	return parsed.Answers.Same.Noul, nil
}

// post mirrors judge.py _post: one retry on 5xx or transport errors, no retry
// on 4xx.
func (j *Jev) post(ctx context.Context, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.cfg.BaseURL+"/v1/systemone", bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if j.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+j.cfg.APIKey)
		}
		resp, err := j.cfg.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = &JudgeError{fmt.Sprintf("HTTP %d: %s", resp.StatusCode, data)}
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, &JudgeError{fmt.Sprintf("HTTP %d: %s", resp.StatusCode, data)}
		}
		return data, nil
	}
	var jerr *JudgeError
	if errors.As(lastErr, &jerr) {
		return nil, jerr
	}
	return nil, &JudgeError{fmt.Sprintf("request failed: %v", lastErr)}
}
