// Command heu is the interactive Heuristic agent: it runs the same runner as
// heuristic but renders a human-readable transcript and reads follow-up
// requests from stdin instead of JSONL.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/ItIsCuthNotCup/heuristic/cmd/internal/agentrunner"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(agentrunner.RunMain(
		ctx, os.Args[1:], os.Getenv, os.Environ,
		os.Stdin, os.Stdout, os.Stderr,
		agentrunner.Config{
			Name:         "heu",
			Interactive:  true,
			ParseRequest: agentrunner.DefaultParseRequest,
			Providers:    agentrunner.DefaultProviders(),
		},
	))
}
