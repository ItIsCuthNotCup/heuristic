package agentrunner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/metacog"
)

// MetaCogRequest is the optional "metacog" request field overriding the
// METACOG_AGENT_* environment configuration.
type MetaCogRequest struct {
	Judge          string   `json:"judge"`
	Mode           string   `json:"mode"`
	StopConfidence *float64 `json:"stop_confidence"`
	NMin           *int     `json:"n_min"`
	NMax           *int     `json:"n_max"`
	AnswerPrior    *float64 `json:"answer_prior"`
}

// resolveMetaCog builds the metacog adapter Config from the environment,
// the request's "metacog" object (which wins), and the -metacog flag.
// judgeName reports which judge was selected for the stderr notice line.
func resolveMetaCog(getenv func(string) string, req *MetaCogRequest, flagValue string) (cfg metacog.Config, judgeName string, err error) {
	judge := lookupEnv(getenv, "JUDGE")
	mode := lookupEnv(getenv, "MODE")
	concurrency := lookupEnv(getenv, "JUDGE_CONCURRENCY")
	stop := lookupEnv(getenv, "STOP_CONFIDENCE")
	nMin := lookupEnv(getenv, "N_MIN")
	nMax := lookupEnv(getenv, "N_MAX")
	prior := lookupEnv(getenv, "ANSWER_PRIOR")
	if req != nil {
		if req.Judge != "" {
			judge = req.Judge
		}
		if req.Mode != "" {
			mode = req.Mode
		}
		if req.StopConfidence != nil {
			stop = strconv.FormatFloat(*req.StopConfidence, 'g', -1, 64)
		}
		if req.NMin != nil {
			nMin = strconv.Itoa(*req.NMin)
		}
		if req.NMax != nil {
			nMax = strconv.Itoa(*req.NMax)
		}
		if req.AnswerPrior != nil {
			prior = strconv.FormatFloat(*req.AnswerPrior, 'g', -1, 64)
		}
	}
	if flagValue != "" {
		mode = flagValue
	}

	parsedMode, err := metacog.ParseMode(mode)
	if err != nil {
		return cfg, "", err
	}
	cfg.Mode = parsedMode

	if judge == "" {
		// Default: Jev when a key is available, otherwise disabled.
		if jevAPIKey(getenv) != "" {
			judge = "jev"
		} else {
			judge = "off"
		}
	}
	switch strings.ToLower(strings.TrimSpace(judge)) {
	case "off":
		cfg.Mode = metacog.ModeOff
		return cfg, "off", nil
	case "jev":
		jc, err := parseConcurrency(concurrency, 8)
		if err != nil {
			return cfg, "", err
		}
		cfg.Judge = metacog.NewJev(metacog.JevConfig{
			BaseURL:     lookupEnv(getenv, "JUDGE_URL"),
			APIKey:      jevAPIKey(getenv),
			Model:       lookupEnv(getenv, "JUDGE_MODEL"),
			Concurrency: jc,
		})
		judgeName = "jev"
	case "local":
		url := lookupEnv(getenv, "JUDGE_URL")
		model := lookupEnv(getenv, "JUDGE_MODEL")
		if url == "" || model == "" {
			return cfg, "", fmt.Errorf("METACOG_AGENT_JUDGE=local requires METACOG_AGENT_JUDGE_URL and METACOG_AGENT_JUDGE_MODEL")
		}
		jc, err := parseConcurrency(concurrency, 1)
		if err != nil {
			return cfg, "", err
		}
		cfg.Judge = metacog.NewLogprob(metacog.LogprobConfig{BaseURL: url, Model: model, Concurrency: jc})
		judgeName = "local"
	default:
		return cfg, "", fmt.Errorf("METACOG_AGENT_JUDGE must be jev, local or off, got %q", judge)
	}

	if stop != "" {
		cfg.StopConfidence, err = strconv.ParseFloat(stop, 64)
		if err != nil {
			return cfg, "", fmt.Errorf("parse metacog stop_confidence %q: %w", stop, err)
		}
	}
	if nMin != "" {
		cfg.NMin, err = strconv.Atoi(nMin)
		if err != nil {
			return cfg, "", fmt.Errorf("parse metacog n_min %q: %w", nMin, err)
		}
	}
	if nMax != "" {
		cfg.NMax, err = strconv.Atoi(nMax)
		if err != nil {
			return cfg, "", fmt.Errorf("parse metacog n_max %q: %w", nMax, err)
		}
	}
	cfg.AnswerPrior = 0.5 // the MetaCog v0.3 default
	if strings.EqualFold(prior, "none") {
		cfg.AnswerPrior = 0
	} else if prior != "" {
		cfg.AnswerPrior, err = strconv.ParseFloat(prior, 64)
		if err != nil {
			return cfg, "", fmt.Errorf("parse metacog answer_prior %q: %w", prior, err)
		}
	}
	return cfg, judgeName, nil
}

func parseConcurrency(value string, fallback int) (int, error) {
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("METACOG_AGENT_JUDGE_CONCURRENCY must be a positive integer, got %q", value)
	}
	return n, nil
}

// jevAPIKey reads METACOG_AGENT_JEV_API_KEY, then TYPESAFE_API_KEY.
func jevAPIKey(getenv func(string) string) string {
	if key := strings.TrimSpace(getenv("METACOG_AGENT_JEV_API_KEY")); key != "" {
		return key
	}
	return strings.TrimSpace(getenv("TYPESAFE_API_KEY"))
}
