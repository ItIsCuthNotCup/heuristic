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
heu                              # sign in on first run, then chat
heu setup                        # re-run sign-in (same as /login)
heu "fix the failing test"       # start with a request
heu -c                           # continue the last conversation here
heu -resume <session-id>         # resume a specific conversation
heu -p "summarize this repo"     # one-shot: run and exit
```

Commands (type `/` for a menu, Tab completes):

| Command | |
| --- | --- |
| `/model` | switch model (live list from the provider) |
| `/login` · `/logout` | sign in / change provider · forget saved sign-in and keys |
| `/metacog on\|off` | turn MetaCog on or off |
| `/new` · `/resume` | fresh conversation · pick an earlier one |
| `/session` · `/clear` · `/help` · `/quit` | |
| `!<command>` | run a shell command yourself |

Keys: Enter sends, Shift+Enter / Ctrl+J / trailing `\` for a new line,
↑↓ history, Esc interrupts the agent (or clears the input), Ctrl+O shows the
full output of the last command, Ctrl+L clears, Ctrl+C twice exits. Set
`NO_COLOR` for plain output. Non-terminal stdin/stdout keeps the simple line
mode.

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
● Bash(cat note.txt)
  ⎿ hello world
◆ MetaCog unsure (0.87) → tried 3 more thought paths → picked #1 (0.89) · 7 judge calls · 4.9s
● The note.txt file contains the text "hello world".
```

Options: `-workspace <dir>`, `-metacog off|final|all`,
`-session-directory <dir>`, `-log-directory <dir>`,
`-tool-heartbeat-interval <dur>`.

**Note:** the agent runs tools (Bash, ViewImage) without confirmation. Use it
in a repo you trust or inside a container.
