// Command heuristic executes one JSON request and writes persisted session items as JSONL.
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
			Name:         "heuristic",
			ParseRequest: parseRequest,
			Providers:    agentrunner.DefaultProviders(),
		},
	))
}
