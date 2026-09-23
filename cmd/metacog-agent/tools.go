package main

import (
	"context"
	"io"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/cmd/internal/agentrunner"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func parseRequest(input io.Reader) (agentrunner.Request, agentrunner.ToolFactory, error) {
	var parsed agentrunner.Request
	if err := agentrunner.DecodeRequest(input, &parsed); err != nil {
		return agentrunner.Request{}, nil, err
	}
	return parsed, func(_ context.Context, config agentrunner.ToolConfig) (agentrunner.Tools, error) {
		return agentrunner.Tools{Registry: tool.NewRegistry(config.Translators, parsed.EnabledTools(config.Names...)...)}, nil
	}, nil
}
