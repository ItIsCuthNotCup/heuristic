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
`/quit` or `/exit` (or EOF) to leave. `/quit` while the agent is working
lets the current task finish before exiting; Ctrl-C interrupts immediately
(the session can be resumed with `heu -resume <id>`). Extra input typed
while the agent works is delivered on its next turn (steering).

Works with any OpenAI-compatible Responses endpoint:

```sh
HEURISTIC_LLM_PROVIDER=openai \
HEURISTIC_LLM_BASE_URL=https://api.commandcode.ai/provider/v1 \
HEURISTIC_LLM_API_KEY=... \
HEURISTIC_LLM_MODEL=moonshotai/Kimi-K2.5 \
heu
```

```
› What is in note.txt? Use bash to check, then answer in one sentence.
⚙ bash: cat note.txt
  hello world
◆ metacog: greedy 0.87 → 3 more thought paths → picked #1 (score 0.89), 7 judge calls, 4.9s
The note.txt file contains the text "hello world".
```

Options: `-workspace <dir>`, `-metacog off|final|all`,
`-session-directory <dir>`, `-log-directory <dir>`,
`-tool-heartbeat-interval <dur>`.

**Note:** the agent runs tools (Bash, ViewImage) without confirmation. Use it
in a repo you trust or inside a container.
