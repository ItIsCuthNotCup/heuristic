<p align="center"><img src="docs/logo.svg" alt="Heuristic" width="200"></p>

# Heuristic

*An agent that doesn't overthink.*

Heuristic is an async-first agent harness — a fork of [Unreal Agent](https://github.com/unreallabsai/unreal-agent)
by Unreal Labs (MIT) — with MetaCog inference-time metacognition built in. On answer
turns, a judge (TypeSafe's Jev/System-One, or any local open model via
logprobs) checks the model's answer while 3 more thought paths race; the
first answer two paths agree on wins, and the judge picks only when none do. Routine tool-call turns are untouched, so Unreal
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
| `HEURISTIC_STOP_CONFIDENCE` | `0.95` | v0.3 loop: judge score above which the first answer is kept |
| `HEURISTIC_N_MIN` / `N_MAX` | `2` / `6` | v0.3 loop: branch count bounds, scaled by judge uncertainty |
| `HEURISTIC_VOTE` | `3` (`off` for the v0.3 loop) | extra paths raced after an answer; the first answer two paths agree on wins |
| `HEURISTIC_TREE` | `off` (`on` = 3 rounds, or a round count) | experimental concise-path fusion: short candidate answers instead of full-length branches |
| `HEURISTIC_TREE_WIDTH` | `3` | candidate paths sampled per round |
| `HEURISTIC_TREE_EFFORT` | unset | cap candidate/merge reasoning effort (`low`, `medium`, `high`) |
| `HEURISTIC_TRUST_CONFIDENCE` | `0.98` | judge score that trusts the first answer outright |
| `HEURISTIC_VOTE_LAZY` | `off` | judge the first answer before racing paths, so a confident turn spends no path tokens |
| `HEURISTIC_ANSWER_PRIOR` | `0.5` (`none` disables) | weight of the bare-answer score |
| `HEURISTIC_JEV_API_KEY` | falls back to `TYPESAFE_API_KEY` | Jev key |
| `HEURISTIC_EFFORT` | `on` (`off` disables) | Jev scores how hard each new message looks and lowers reasoning effort for easy ones; it never raises your setting |
| `HEURISTIC_TOOLS` | `on` (`off` disables) | Jev judges whether a turn needs tools; pure-chat turns are sent without tool schemas |
| `HEURISTIC_PRUNE_CHARS` | `48000` (`off` disables) | once the conversation is bigger than this, Jev scores each old tool output and stubs the irrelevant ones |
| `UNREAL_HARNESS_*` | — | upstream names still work as fallbacks |

**The benchmark is the model itself** — the same Command Code model with
no MetaCog at all. On GPQA-Diamond-30 (deepseek-v4-flash, Jev control
plane on, agentic harness):

| arm | input tokens | accuracy |
|---|---:|---|
| raw model, no MetaCog | 5.8k | 24/25 answered |
| heu, this branch | 25.6k | 25/30 |
| heu, MetaCog off | 1,127k | 27/30 |

MetaCog v0.3 on single-answer benchmarks: 81.7% → 86.7%, +11/−2, p=0.02
on 180 paired GPQA/AIME rows; agentic benchmarks not yet measured. See
the [MetaCog](https://github.com/ItIsCuthNotCup/MetaCog) repo for the
method and evidence.

## Thought paths

Heuristic only thinks harder when it is unsure. Each answer turn works like
this:

1. **Answer once.** The model replies as normal.
2. **Race 3 more paths, check the first answer meanwhile.** Three more
   answers start at once while the judge scores the first one. If it scores
   0.98 or above the extra paths are cancelled and the first answer stands.
3. **Stop when two paths agree.** As each path finishes it is compared with
   the ones before it. Paths that state a final answer (`Answer: …`,
   `\boxed{…}`) are compared directly; otherwise the judge is asked whether
   the two reach the same answer (0.7 or above counts). The first answer two
   paths agree on wins and any path still running is cancelled.
4. **Otherwise the judge picks.** If no two paths agree, every path and its
   bare final answer are scored and the highest
   `path score + 0.5 × bare-answer score` wins.

How many paths actually race depends on how hard Jev judges the request:
trivial asks run no extra paths (the first answer is returned directly),
moderate ones up to 2, hard ones the full 3 — MoE-style sparse activation
over thought paths. `HEURISTIC_EFFORT=off` disables that routing too.

`HEURISTIC_VOTE=off` restores the MetaCog v0.3 loop: judge the first answer,
keep it at 0.95, else write `round(2 + (1 − score) × 4)` more paths and let
the judge pick.

On 140 GPQA/AIME problems with GPT-6 Luna on Command Code (every path
generated once, then each policy replayed on the same paths), the vote got
127 right vs 125 for v0.3 and 119 for a single answer, in about 0.85× v0.3's
time and 0.9× its tokens; in `heu` you see the first answer after a single
answer's time either way.

#### The concise-path fusion tree (experimental, `HEURISTIC_TREE=on`)

Instead of racing whole answers at full length, the tree races *concise*
candidates — each path is told to answer in at most ~1000 tokens — and
lets Jev decide which ideas are worth keeping:

1. Judge the first answer. `≥ 0.8` → Jev is sure; use it and stop.
2. Otherwise race `HEURISTIC_TREE_WIDTH` (3) concise candidate answers in
   parallel and Jev scores each. The best one at `≥ 0.8` is used outright.
3. Still unsure → try a fresh round of concise paths, up to
   `HEURISTIC_TREE` rounds (default 3).
4. If no candidate ever clears the bar, one merge call takes the strongest
   parts of the top-scoring paths and writes a single concise answer —
   Jev-verified against the first answer, which wins on a tie or loss.

The paths show up as the thought paths (with their Jev score) you can open
with Ctrl+T — they are read-only. Paths and the merge call are sent
without tool schemas; the tree only runs on turns the model answered in
prose. `HEURISTIC_TREE=off` (default) keeps the vote behaviour.

Only turns that end with an answer are judged; turns that call tools pass
straight through, so a normal agent loop costs the same as without MetaCog.

#### The Jev control plane

Before every model call, Jev (the cheap judge, ~70–500 ms per question)
makes three decisions, each cached so one user message is judged once:

- **How hard is this request?** Easy requests run at lower reasoning effort
  and race fewer thought paths; hard ones keep your configured effort.
- **Does it need tools?** Pure-chat turns are sent without tool schemas,
  which otherwise ride along on every call and every path.
- **Which old context still matters?** Once the transcript passes
  `HEURISTIC_PRUNE_CHARS`, each old tool output is scored against the
  conversation skeleton and the recent requests: relevant chunks stay
  verbatim, borderline ones keep their first 400 characters, dead ones
  become a one-line stub. Subsequent turns and thought paths reuse the
  compacted transcript, so history stops being re-read in full (the
  fast-jev-compaction pattern: extractive keep/drop, never a lossy
  summary). A judge outage keeps everything verbatim.

### Staying fast

- **You never wait for MetaCog in `heu`.** The first answer is shown as soon
  as the model finishes, and steps 2–5 run in the background while the
  footer shows what MetaCog is doing ("MetaCog is checking with 3 more
  thought paths…"). If a different path wins, `heu` prints it as *found a better
  answer* and your next message continues from it. Sending a new message
  cancels a check that hasn't finished. One-shot runs (`heuristic -p`) still
  wait and return the winner directly.
- **Slow paths are dropped.** The extra paths get 1.5× as long as the first
  answer took (at least 20 s); any path still running then is cancelled and
  the rest are compared. Each judging step gives up after 30 s and keeps the
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
judge score, so the list costs no extra model or judge calls. When a vote
decided, each path shows `agrees` or `differs` instead of a score. `❯` marks
the path the conversation is using.

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
