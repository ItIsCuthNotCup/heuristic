---
name: testing-heu-terminal
description: Test heu onboarding and authentication recovery in a real desktop terminal with isolated credentials.
---

# Testing heu terminal flows

## Build and launch
- Use Go 1.27+: `export PATH="$HOME/go-sdk/go/bin:$HOME/go/bin:$PATH"` then `go build -o /tmp/heu ./cmd/heu`.
- Run in a real desktop Konsole, not pipe/line mode:
  `DISPLAY=:0 konsole --separate -p 'Font=DejaVu Sans Mono,12' -e python3 <sanitized-launcher.py> <fixture>`.
- Maximize before recording with `DISPLAY=:0 wmctrl -r :ACTIVE: -b add,maximized_vert,maximized_horz`.
- Use a scratch workspace: heu can run commands without confirmation.

## Credential isolation
- Give each fixture its own HOME. Remove XDG_CONFIG_HOME too, since it overrides HOME.
- Sanitize the child environment: remove HEURISTIC_*, UNREAL_*, TYPESAFE_*,
  OPENAI*, COMMANDCODE_API_KEY, OPENROUTER_API_KEY, FIREWORKS_API_KEY and other
  provider/judge keys unless deliberately testing environment reuse.
- An allowlist of desktop variables (DISPLAY, XAUTHORITY,
  DBUS_SESSION_BUS_ADDRESS, XDG_RUNTIME_DIR), PATH, TERM and locale is safer.
- Saved config lives at `$HOME/.config/heuristic/config.env`; workspace `.env`
  and process environment can override it.
- A generic saved HEURISTIC_LLM_API_KEY is not equivalent to a provider-specific
  environment key. Test these as separate fixtures when asserting defaults.
- Paste secret references only into the hidden key prompt; never echo keys.
  After testing, delete isolated app-generated credential files.

## UI assertions
- A rejected saved Command Code key can be tested with provider=openai,
  base URL=https://api.commandcode.ai/provider/v1, model=moonshotai/Kimi-K2.5,
  API key=bogus, judge=off in the HEURISTIC_* config fields.
- Send hello and check both friendly rejection text and automatic picker.
- A valid key should load a model list, pass the live connection check, and
  produce a real chat reply after setup. Choose Not now at the MetaCog step
  when testing provider authentication without judge dependencies.
- `/models` opens the model picker; `/provider` opens sign-in. Esc cancels.
- With no config/keys, first-run defaults to ChatGPT and Esc exits setup.
- Record and annotate terminal UI; screenshots must show the selected row,
  not merely a successful process exit.

## Devin Secrets Needed
- COMMANDCODE_API_KEY: live Command Code connection and post-recovery chat.
- No real secret is required to reproduce rejected-key recovery or inspect
  fresh onboarding. Never substitute mock responses for the live check.
