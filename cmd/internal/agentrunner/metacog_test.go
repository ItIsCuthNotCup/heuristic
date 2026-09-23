package agentrunner

import (
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
)

func envFunc(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestResolveMetaCogDefaults(t *testing.T) {
	cfg, judge, err := resolveMetaCog(envFunc(map[string]string{"TYPESAFE_API_KEY": "k"}), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if judge != "jev" || cfg.Mode != metacog.ModeFinal || cfg.Judge == nil {
		t.Fatalf("judge=%q mode=%v cfg=%+v, want jev/final", judge, cfg.Mode, cfg)
	}
	if cfg.StopConfidence != 0 || cfg.AnswerPrior != 0.5 {
		t.Fatalf("stop=%v prior=%v, want 0 (adapter default) / 0.5", cfg.StopConfidence, cfg.AnswerPrior)
	}
}

func TestResolveMetaCogDefaultsOffWithoutKey(t *testing.T) {
	cfg, judge, err := resolveMetaCog(envFunc(nil), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if judge != "off" || cfg.Mode != metacog.ModeOff {
		t.Fatalf("judge=%q mode=%v, want off", judge, cfg.Mode)
	}
}

func TestResolveMetaCogRequestOverridesEnv(t *testing.T) {
	env := envFunc(map[string]string{
		"HEURISTIC_JUDGE":           "off",
		"HEURISTIC_STOP_CONFIDENCE": "0.5",
	})
	req := &MetaCogRequest{Judge: "local", Mode: "all"}
	env = envFunc(map[string]string{
		"HEURISTIC_JUDGE":           "local",
		"HEURISTIC_JUDGE_URL":       "http://localhost:8080",
		"HEURISTIC_JUDGE_MODEL":     "m",
		"HEURISTIC_STOP_CONFIDENCE": "0.5",
	})
	stop := 0.8
	req.StopConfidence = &stop
	cfg, judge, err := resolveMetaCog(env, req, "")
	if err != nil {
		t.Fatal(err)
	}
	if judge != "local" || cfg.Mode != metacog.ModeAll || cfg.StopConfidence != 0.8 {
		t.Fatalf("judge=%q cfg=%+v", judge, cfg)
	}
}

func TestResolveMetaCogFlagBeatsAll(t *testing.T) {
	env := envFunc(map[string]string{
		"HEURISTIC_JUDGE":  "jev",
		"TYPESAFE_API_KEY": "k",
	})
	cfg, _, err := resolveMetaCog(env, &MetaCogRequest{Mode: "all"}, "off")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != metacog.ModeOff {
		t.Fatalf("mode = %v, want off", cfg.Mode)
	}
}

func TestResolveMetaCogInvalidValues(t *testing.T) {
	cases := []map[string]string{
		{"HEURISTIC_JUDGE": "wat"},
		{"HEURISTIC_JUDGE": "jev", "TYPESAFE_API_KEY": "k", "HEURISTIC_MODE": "bogus"},
		{"HEURISTIC_JUDGE": "jev", "TYPESAFE_API_KEY": "k", "HEURISTIC_STOP_CONFIDENCE": "high"},
		{"HEURISTIC_JUDGE": "jev", "TYPESAFE_API_KEY": "k", "HEURISTIC_N_MIN": "two"},
		{"HEURISTIC_JUDGE": "local"},
		{"HEURISTIC_JUDGE": "jev", "TYPESAFE_API_KEY": "k", "HEURISTIC_JUDGE_CONCURRENCY": "zero"},
		{"HEURISTIC_JUDGE": "local", "HEURISTIC_JUDGE_URL": "u", "HEURISTIC_JUDGE_MODEL": "m", "HEURISTIC_JUDGE_CONCURRENCY": "0"},
	}
	for i, env := range cases {
		if _, _, err := resolveMetaCog(envFunc(env), nil, ""); err == nil {
			t.Errorf("case %d: want error for %v", i, env)
		}
	}
}

func TestResolveMetaCogAnswerPriorNone(t *testing.T) {
	env := envFunc(map[string]string{
		"TYPESAFE_API_KEY":       "k",
		"HEURISTIC_ANSWER_PRIOR": "none",
	})
	cfg, _, err := resolveMetaCog(env, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AnswerPrior != 0 {
		t.Fatalf("answer prior = %v, want 0 (disabled)", cfg.AnswerPrior)
	}
}

func TestLookupEnvFallback(t *testing.T) {
	env := envFunc(map[string]string{
		"HEURISTIC_LLM_MODEL":        "new",
		"UNREAL_HARNESS_LLM_API_KEY": "old-key",
	})
	if got := lookupEnv(env, llmModelEnvironment); got != "new" {
		t.Errorf("metacog env should win, got %q", got)
	}
	if got := lookupEnv(env, llmAPIKeyEnvironment); got != "old-key" {
		t.Errorf("unreal fallback broken, got %q", got)
	}
}
