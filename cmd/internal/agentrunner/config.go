package agentrunner

import (
	"context"
	"io"
	"slices"

	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

type Config struct {
	Name         string
	Providers    []Provider
	ParseRequest func(io.Reader) (Request, ToolFactory, error)
	// Interactive renders a human-readable transcript instead of session
	// JSONL, joins positional args into a plain-text prompt, and keeps
	// reading requests from stdin instead of stopping when idle (unless -p).
	Interactive bool
}

type ToolConfig struct {
	Translators tool.StaticTranslators
	Names       []string
	SessionID   session.ID
	Getenv      func(string) string
}

type Tools struct {
	Registry   tool.Registry
	RemoteJobs []operation.RemoteJobHandler
	Close      func() error
}

type ToolFactory func(context.Context, ToolConfig) (Tools, error)

func (parsed Request) EnabledTools(names ...string) []string {
	enabled := make([]string, 0, len(names))
	for _, name := range names {
		if !slices.Contains(parsed.DisallowedTools, name) {
			enabled = append(enabled, name)
		}
	}
	return enabled
}
