package agentrunner

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

func TestRunRejectsIncompleteConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{
			name:   "missing name",
			config: Config{ParseRequest: parseTestRequest},
			want:   "runner name must be set",
		},
		{
			name:   "blank name",
			config: Config{Name: " \t", ParseRequest: parseTestRequest},
			want:   "runner name must be set",
		},
		{
			name:   "missing parser",
			config: Config{Name: "test-runner"},
			want:   "request parser must be set",
		},
		{
			name: "missing tool factory",
			config: Config{Name: "test-runner", ParseRequest: func(io.Reader) (Request, ToolFactory, error) {
				return Request{Prompt: new("hello")}, nil, nil
			}},
			want: "request parser returned no tool factory",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Run(t.Context(), nil, func(string) string {
				t.Fatal("invalid runner configuration reached environment setup")
				return ""
			}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello"}`), io.Discard, io.Discard, test.config)
			if err == nil || err.Error() != test.want {
				t.Fatalf("Run error = %v, want %q", err, test.want)
			}
		})
	}
}

func parseTestRequest(input io.Reader) (Request, ToolFactory, error) {
	var parsed Request
	err := DecodeRequest(input, &parsed)
	return parsed, func(_ context.Context, config ToolConfig) (Tools, error) {
		return Tools{Registry: tool.NewRegistry(config.Translators, parsed.EnabledTools(config.Names...)...)}, nil
	}, err
}
