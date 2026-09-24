# heu

`heu` is the interactive Heuristic agent — a Claude-Code-style terminal loop
on the same runner as `heuristic`: you type a request, it renders the
assistant's replies and tool calls as readable text (no JSONL), and stays
alive for follow-ups.

Install with Go 1.27+:

```sh
go install github.com/ItIsCuthNotCup/heuristic/cmd/heu@latest
```

Configure the model via environment or a `.env` file in the workspace:

```sh
export OPENAI_API_KEY="..."                    # for the default openai provider
# or explicitly:
export HEURISTIC_LLM_PROVIDER=openai           # openai | openai-codex | openrouter | fireworks | ollama
export HEURISTIC_LLM_MODEL=gpt-6-astra
export HEURISTIC_LLM_API_KEY="..."             # generic key; provider-specific keys also work
export TYPESAFE_API_KEY="..."                  # optional: enables the Jev judge (metacog)
```

`UNREAL_HARNESS_*` names are accepted as fallbacks for all `HEURISTIC_*` vars.

## Usage

```sh
heu                              # banner + prompt, waits for input
heu "fix the failing test"       # initial request, then keeps running
heu -p "summarize this repo"     # one-shot: run and exit
heu -resume <session-id>         # resume a previous session
```

Slash commands: `/help`, `/session` (prints the id and the `-resume` line),
`/quit` or `/exit` (or EOF) to leave. Ctrl-C also exits. Extra input typed
while the agent works is delivered on its next turn (steering).

Options: `-workspace <dir>`, `-metacog off|final|all`,
`-session-directory <dir>`, `-log-directory <dir>`,
`-tool-heartbeat-interval <dur>`.

**Note:** the agent runs tools (Bash, ViewImage) without confirmation. Use it
in a repo you trust or inside a container.
