# Harbor evaluation adapter

Evaluates `heuristic` as `heuristic` with Harbor 0.22.0. Requires Go
from the root `go.mod`, Python 3.12+, uv, and Docker or Modal.

## Setup and build

From the repository root:

```sh
make -C benchmarks/harbor sync
make -C benchmarks/harbor build REVISION=HEAD
```

Builds committed source into `bin/harbor/<short-commit>/` with a revision and
checksum manifest. Defaults to Linux amd64. Existing bundles are not overwritten;
set `BUNDLE` to a new directory to rebuild.

The manifest records the selected runner along with the revision.

## Run

Use an absolute bundle path:

```sh
export OPENAI_API_KEY=...
uv run --project benchmarks/harbor --locked harbor run \
  -p /absolute/path/to/task \
  -a harness_harbor.agent:UnrealAgent \
  -m openai/gpt-5.4 \
  --ak bundle="$PWD/bin/harbor/<short-commit>" \
  --ak thinking_level=high
```

`thinking_level`: `low`, `medium`, `high` (default), `xhigh`, or `max`.
Also supports `openrouter/<model>` with `OPENROUTER_API_KEY` and
`fireworks_ai/<model>` with `FIREWORKS_AI_API_KEY`. Use Harbor's `--ae` option for
environment overrides.

OpenRouter requests opt into OpenRouter's automatic prompt caching with a one-hour
TTL and carry the session id, so Anthropic and other explicit-breakpoint upstreams
cache the growing conversation, keep it on one upstream, and keep it across long
tool calls and reasoning turns.

For Terminal-Bench 4.0 on Modal, configure Modal credentials and run:

```sh
uv run --project benchmarks/harbor --locked --extra modal harbor run \
  -d terminal-bench/terminal-bench@4.0.0 \
  -e modal -a harness_harbor.agent:UnrealAgent \
  -m openai/gpt-6-astra \
  --ak bundle="$PWD/bin/harbor/<short-commit>" --ak thinking_level=max \
  -k 5 -n 40 --job-name tb4-heuristic
```

Inspect results with `harbor view jobs/<job-name>`.

## Output

- Bash and ViewImage tools.
- ViewImage returns images within 2000×2000 pixels and 5 MB minus 1 KB of base64
  content. Trajectories reference the returned images under `agent/images/` and
  include original format and coordinate-scaling metadata when applicable.
- Logs and sessions are under `agent/`; `agent/trajectory.json` contains ATIF
  observations and token totals.
- Observations are grouped by tool call. Their `extra` sequence, timestamp, and
  `available_before_turn` fields preserve asynchronous timing.

## Validation

```sh
make -C benchmarks/harbor test check
make -C benchmarks/harbor smoke BUNDLE=../../bin/harbor/<short-commit>
```

The smoke test uses Docker and a local test model.

Reference: [Harbor agents](https://www.harborframework.com/docs/agents).
