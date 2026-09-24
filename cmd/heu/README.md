# heu

`heu` is the interactive Heuristic agent — a Claude-Code-style terminal loop
on the same runner as `heuristic`: you type a request, it renders the
assistant's replies and tool calls as readable text (no JSONL), and stays
alive for follow-ups.

Install with Go 1.27+:

```sh
go install github.com/ItIsCuthNotCup/heuristic/cmd/heu@latest
```

Configure the model by just running `heu`: on first run with no provider
configured it walks you through sign-in — OpenAI API key, ChatGPT/Codex
subscription (OAuth, browser or paste-the-redirect-URL for headless/SSH),
OpenRouter, Fireworks, Ollama, or any OpenAI-compatible endpoint — then the
MetaCog judge. Re-run it any time with `heu setup` (or `heu login`).

Settings are saved to `~/.config/heuristic/config.env` (respects
`XDG_CONFIG_HOME`). Precedence: process environment > `<workspace>/.env` >
the user config file. Codex OAuth credentials live in
`~/.config/heuristic/codex-auth.json`; an existing `~/.codex/auth.json`
login is detected and offered instead.

Advanced/manual path: set the variables yourself in the environment or a
workspace `.env` file — `HEURISTIC_LLM_PROVIDER` (`openai` |
`openai-codex` | `openrouter` | `fireworks` | `ollama`),
`HEURISTIC_LLM_MODEL`, `HEURISTIC_LLM_API_KEY`, `HEURISTIC_LLM_BASE_URL`,
`OPENAI_CODEX_AUTH_FILE`, `TYPESAFE_API_KEY` (Jev judge).
`UNREAL_HARNESS_*` names are accepted as fallbacks for all `HEURISTIC_*`
vars.

## Usage

```sh
heu                              # banner + prompt (runs setup on first use)
heu setup                        # re-run the sign-in/provider wizard
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
