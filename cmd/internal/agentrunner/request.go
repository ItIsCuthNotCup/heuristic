package agentrunner

import (
	"context"
	"io"

	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

// DefaultParseRequest reads one JSON request and returns the standard tool
// factory (static tools filtered by disallowed_tools). Shared by the
// `heuristic` one-shot runner and the `heu` interactive runner.
func DefaultParseRequest(input io.Reader) (Request, ToolFactory, error) {
	var parsed Request
	if err := DecodeRequest(input, &parsed); err != nil {
		return Request{}, nil, err
	}
	return parsed, defaultToolFactory(parsed), nil
}

func defaultToolFactory(parsed Request) ToolFactory {
	return func(_ context.Context, config ToolConfig) (Tools, error) {
		return Tools{Registry: tool.NewRegistry(config.Translators, parsed.EnabledTools(config.Names...)...)}, nil
	}
}
