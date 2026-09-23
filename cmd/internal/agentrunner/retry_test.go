package agentrunner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

func TestResolveMaxAttempts(t *testing.T) {
	for _, test := range []struct {
		name        string
		requested   *int
		environment string
		want        int
		wantError   bool
	}{
		{name: "default", want: 5},
		{name: "environment", environment: " 3 ", want: 3},
		{name: "environment disables retries", environment: "1", want: 1},
		{name: "request override", requested: new(2), environment: "3", want: 2},
		{name: "request disables retries", requested: new(1), environment: "3", want: 1},
		{name: "unused invalid environment", requested: new(1), environment: "bad", want: 1},
		{name: "negative request", requested: new(-1), wantError: true},
		{name: "negative environment", environment: "-1", wantError: true},
		{name: "invalid environment", environment: "1.5", wantError: true},
		{name: "zero request", requested: new(0), wantError: true},
		{name: "zero environment", environment: "0", wantError: true},
		{name: "environment overflow", environment: "999999999999999999999999999", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveMaxAttempts(test.requested, func(string) string { return test.environment })
			if (err != nil) != test.wantError || got != test.want {
				t.Fatalf("max attempts, error = (%d, %v), want (%d, error=%v)", got, err, test.want, test.wantError)
			}
		})
	}
}

func TestRunMainRequestDisablesRetries(t *testing.T) {
	client := &fakeClient{respond: func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{ID: "response-1", Stop: llm.StopComplete}, nil
	}}
	config := testConfig(client)
	config.Providers[0].NewClient = func(_, _ string, maxAttempts int, _ func(string) string) (Client, error) {
		if maxAttempts != 1 {
			return nil, errors.New("request did not disable retries")
		}
		return client, nil
	}
	code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()},
		func(name string) string {
			switch name {
			case llmAPIKeyEnvironment:
				return "secret"
			case llmMaxAttemptsEnvironment:
				return "3"
			default:
				return ""
			}
		}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","max_attempts":1}`),
		io.Discard, io.Discard, config)
	if code != 0 || !client.closed {
		t.Fatalf("exit = %d, client closed = %v", code, client.closed)
	}
}
