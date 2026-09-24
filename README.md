<p align="center"><img src="docs/logo.svg" alt="Heuristic" width="200"></p>

# Heuristic

*An agent that doesn't overthink.*

Heuristic is an async-first agent harness — a fork of [Unreal Agent](https://github.com/unreallabsai/unreal-agent)
by Unreal Labs (MIT) — with MetaCog inference-time metacognition built in. On answer
turns, a judge (TypeSafe's Jev/System-One, or any local open model via
logprobs) scores the model's answer; if it is not confident the agent writes
2–6 more thought paths in parallel, scores them and their bare final answers,
and returns the best. Routine tool-call turns are untouched, so Unreal
Agent's cost profile is preserved.

Install:

```sh
go install github.com/ItIsCuthNotCup/heuristic/cmd/heuristic@latest
```

Quick start:

```sh
export OPENAI_API_KEY="..."       # the agent model
export TYPESAFE_API_KEY="..."     # the Jev judge (omit for judge=off)
heuristic -p 'Solve this task.'
```

## Interactive: `heu`

```sh
go install github.com/ItIsCuthNotCup/heuristic/cmd/heu@latest
heu                     # sign in on first run, then chat
heu "fix the failing test"
heu -c                  # continue the last conversation in this folder
heu -resume <session-id>
heu -p "summarize this repo"     # one-shot
```

`heu` is a full terminal coding agent on the same loop as `heuristic`:
arrow-key sign-in (ChatGPT, Command Code, OpenAI, OpenRouter, Fireworks,
Ollama, or any server) with a live connection check, Markdown answers,
compact tool cards with collapsed output (Ctrl-O expands), a `/` command
menu, history, multi-line paste, Esc to interrupt, and a footer with model,
tokens and MetaCog state. It runs tools without asking (yolo). See
[cmd/heu/README.md](cmd/heu/README.md).

Environment:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HEURISTIC_JUDGE` | `jev` if `TYPESAFE_API_KEY` is set, else `off` | `jev` \| `local` \| `off` |
| `HEURISTIC_JUDGE_URL` | `https://api.typesafe.ai` (jev); required for `local` | judge endpoint |
| `HEURISTIC_JUDGE_MODEL` | `jev-latest` | judge model |
| `HEURISTIC_JUDGE_CONCURRENCY` | `8` (jev), `1` (local) | parallel judge requests per pool |
| `HEURISTIC_MODE` | `final` | `final` judges answers only; `all` also judges tool-call turns; `off` disables |
| `HEURISTIC_STOP_CONFIDENCE` | `0.95` | judge score above which the first answer is kept |
| `HEURISTIC_N_MIN` / `N_MAX` | `2` / `6` | branch count bounds, scaled by judge uncertainty |
| `HEURISTIC_ANSWER_PRIOR` | `0.5` (`none` disables) | weight of the bare-answer score |
| `HEURISTIC_JEV_API_KEY` | falls back to `TYPESAFE_API_KEY` | Jev key |
| `UNREAL_HARNESS_*` | — | upstream names still work as fallbacks |

Measured (from [MetaCog](https://github.com/ItIsCuthNotCup/MetaCog) v0.3 on
single-answer benchmarks): 81.7% → 86.7%, +11/−2, p=0.02 on 180 paired
GPQA/AIME rows; agentic benchmarks not yet measured. See that repo for the
method and evidence.

## Thought paths

Heuristic only thinks harder when it is unsure. Each answer turn works like
this:

1. **Answer once.** The model replies as normal.
2. **Check confidence.** The judge scores that answer from 0 to 1 (how likely
   it is to be right). At 0.95 or above it is kept as is: one answer, no
   extra cost.
3. **Branch by uncertainty.** Otherwise the agent writes
   `round(2 + (1 − score) × 4)` more answers in parallel, i.e. 2 paths when
   it is nearly sure and up to 6 when it is lost. Together with the first
   answer these are the *thought paths*.
4. **Score every path.** The judge scores each full path, and separately each
   path's bare final answer (the answer prior).
5. **Pick the best.** The winner has the highest
   `path score + 0.5 × bare-answer score`.

Only turns that end with an answer are judged; turns that call tools pass
straight through, so a normal agent loop costs the same as without MetaCog.

### Staying fast

- **You never wait for MetaCog in `heu`.** The first answer is shown as soon
  as the model finishes, and steps 2–5 run in the background while the
  footer shows what MetaCog is doing ("MetaCog is trying 3 more thought
  paths…"). If a different path wins, `heu` prints it as *found a better
  answer* and your next message continues from it. Sending a new message
  cancels a check that hasn't finished. One-shot runs (`heuristic -p`) still
  wait and return the winner directly.
- **Slow paths are dropped.** The extra paths get 1.5× as long as the first
  answer took (at least 20 s); any path still running then is cancelled and
  the rest are judged. Each judging step gives up after 30 s and keeps the
  first answer.

When MetaCog branches, `heu` lists every path under its note:

```
◆ MetaCog found a better answer: Thought Path 2 (0.81 vs 0.38) · 6.1s
  Thought Path 1  0.38  Cache the parsed config in a package variable.
❯ Thought Path 2  0.81  Parse the config once in main and pass it down.
  Thought Path 3  0.44  Use sync.Once around the parser.
  Thought Path 4  0.52  Re-read the file only when its mtime changes.
  ctrl+t to open a path or continue from a different one
```

Each summary is the first line of that path's own text and the number is its
judge score, so the list costs no extra model or judge calls. `❯` marks the
path the conversation is using.

Press **Ctrl+T** (or type `/paths`) to open the list. Pick a path with ↑↓
and Enter to read it in full. On a path that isn't in use, choose
**Continue from this path**: your next message is answered as if the agent
had given that answer, because for the rest of this `heu` run it is sent to
the model in place of the judge's pick. The saved session log keeps what was
actually shown.

## Harness (from Unreal Agent)

- [harness/](harness/) — the library.
- [cmd/](cmd/) — executables that use the library.
- [benchmarks/](benchmarks/) — benchmark runners.

## Glossary

- **Input**: an event with a caller-supplied globally unique ID that remains
  stable across redeliveries.
- **Inbox**: session-scoped, in-memory deduplication of external, control, and
  crash inputs.
- **Session**: append-only persisted history that can be forked.
- **LLM turn**: the coordinator-managed sequence around one logical LLM request.
- **Tool**: a capability described by a schema and bound to a translator.
- **Tool call**: a model-produced request to use a tool.
- **Tool translator**: validates a tool call and translates it into one or more
  operations. It runs synchronously on the coordinator's event loop and must not
  perform I/O or suspend the loop.
- **Tool call status**: the translation outcome: a validation error or references
  to submitted operations. Operation execution state is tracked separately;
  the translator formats these into a model-facing result.
- **Operation**: a serializable description of work produced by a tool translator
  for asynchronous execution. Implementations are encouraged to use the available
  [primitives](harness/primitives/).

## Components

| Component | Responsibility |
| --- | --- |
| Session inbox | Volatile, session-scoped input idempotency. |
| Coordinator | Persist accepted inputs, run LLM turns, resolve tool translators through the registry, and dispatch committed operations. |
| Session store | Persist canonical session history and operation state; support recovery and forks; atomically record tool-call status with operations. |
| Context builder | Statefully assemble model input in memory. Return the model input together with a record of anything omitted, truncated, or compacted. Perform no I/O and accept no persistence dependencies. |
| LLM Adapter | Send prepared model input to a provider and return a normalized completed response. Own authentication, cancellation, and provider errors. |
| Tool registry | Own the fixed Bash, ViewImage, and skill-use definitions and their translators; expose the host-selected set. |
| Tool translator | Validate a tool call and produce its status and operations. Format a recorded call status and prepared operation output into model results. Perform no I/O. |
| Operation manager | Actor runtime for durable operations. The local implementation is swappable. |

## Extending the harness

Harness components are composable, and alternative implementations of their interfaces are encouraged.

We intend to preserve these invariants:

- Session-store items are serializable, and the storage format is versioned.
- We'll do our best to maintain backwards compatibility for sessions.
  An unsupported session version will always cause an explicit error on resume.
- Operations are versioned and always serializable.

For example, a proxy operations manager can send serialized operations to a
local operations manager running in a process inside a remote sandbox, allowing
tools to execute there.
