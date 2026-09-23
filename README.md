# MetaCog Agent

An async-first agent harness — a fork of [Unreal Agent](https://github.com/unreallabsai/unreal-agent)
by Unreal Labs (MIT) — with built-in inference-time metacognition. On answer
turns, a judge (TypeSafe's Jev/System-One, or any local open model via
logprobs) scores the model's answer; if it is not confident the agent writes
2–6 more thought paths in parallel, scores them and their bare final answers,
and returns the best. Routine tool-call turns are untouched, so Unreal
Agent's cost profile is preserved.

Install:

```sh
go install github.com/ItIsCuthNotCup/MetaCog-Agent/cmd/metacog-agent@latest
```

Quick start:

```sh
export OPENAI_API_KEY="..."       # the agent model
export TYPESAFE_API_KEY="..."     # the Jev judge (omit for judge=off)
metacog-agent -p 'Solve this task.'
```

Environment:

| Variable | Default | Purpose |
| --- | --- | --- |
| `METACOG_AGENT_JUDGE` | `jev` if `TYPESAFE_API_KEY` is set, else `off` | `jev` \| `local` \| `off` |
| `METACOG_AGENT_JUDGE_URL` | `https://api.typesafe.ai` (jev); required for `local` | judge endpoint |
| `METACOG_AGENT_JUDGE_MODEL` | `jev-latest` | judge model |
| `METACOG_AGENT_JUDGE_CONCURRENCY` | `8` (jev), `1` (local) | parallel judge requests per pool |
| `METACOG_AGENT_MODE` | `final` | `final` judges answers only; `all` also judges tool-call turns; `off` disables |
| `METACOG_AGENT_STOP_CONFIDENCE` | `0.95` | judge score above which the first answer is kept |
| `METACOG_AGENT_N_MIN` / `N_MAX` | `2` / `6` | branch count bounds, scaled by judge uncertainty |
| `METACOG_AGENT_ANSWER_PRIOR` | `0.5` (`none` disables) | weight of the bare-answer score |
| `METACOG_AGENT_JEV_API_KEY` | falls back to `TYPESAFE_API_KEY` | Jev key |
| `UNREAL_HARNESS_*` | — | upstream names still work as fallbacks |

Measured (from [MetaCog](https://github.com/ItIsCuthNotCup/MetaCog) v0.3 on
single-answer benchmarks): 81.7% → 86.7%, +11/−2, p=0.02 on 180 paired
GPQA/AIME rows; agentic benchmarks not yet measured. See that repo for the
method and evidence.

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
