package agentrunner

import (
	"flag"
	"fmt"
)

const requestHelp = `
Request schema (JSON object; unknown fields are rejected):
  messages: array of {role: "user", content: string, message_id?: UUID string}
    Non-empty array of user messages delivered in order. role defaults to "user";
    message_id defaults to a generated UUID.
  prompt: string
    Shorthand for one user message; used when messages is absent.
    Supply messages or prompt. messages takes precedence when both are present.
  model: string (optional)
    Provider model ID; defaults to UNREAL_HARNESS_LLM_MODEL or the provider default.
  max_attempts: positive integer (optional)
    Overrides UNREAL_HARNESS_LLM_MAX_ATTEMPTS (default 5); 1 disables retries.
  system_prompt: string (optional)
    Replaces the default system prompt.
  thinking_level: "low" | "medium" | "high" | "xhigh" | "max" (optional; default "high")
  session_id: non-empty string (optional)
    Creates or resumes a persisted session.
  disallowed_tools: array of non-empty strings (optional)
    Static tool names excluded from model context and execution.
  extra_allowed_tools: array of non-empty strings (optional; accepted but ignored)
  include_partial_messages: boolean (optional; accepted but ignored)
`

func writeUsage(flags *flag.FlagSet, interactive bool) error {
	if interactive {
		if _, err := fmt.Fprintf(flags.Output(), `Usage:
  %[1]s [options] [prompt...]

Interactive Heuristic agent; positional words form the first request,
then stdin is read line by line (/help lists slash commands, EOF exits).

Options:
`, flags.Name()); err != nil {
			return fmt.Errorf("write usage: %w", err)
		}
		flags.PrintDefaults()
		return nil
	}
	if _, err := fmt.Fprintf(flags.Output(), `Usage:
  %[1]s [options] < request.json
  %[1]s [options] 'JSON request'
  %[1]s [options] -p 'prompt'

Reads one JSON request from stdin unless a positional request or -p is supplied.
Place options before the positional request. -p and a positional request are mutually exclusive.

Options:
`, flags.Name()); err != nil {
		return fmt.Errorf("write usage: %w", err)
	}
	flags.PrintDefaults()
	if _, err := fmt.Fprint(flags.Output(), requestHelp); err != nil {
		return fmt.Errorf("write request schema: %w", err)
	}
	return nil
}
