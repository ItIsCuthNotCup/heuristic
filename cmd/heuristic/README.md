# heuristic

Run an AI agent from a prompt or JSON request. It writes events to stdout as
JSONL and exits when the task finishes.

Install with Go 1.27+:

```sh
go install github.com/ItIsCuthNotCup/heuristic/cmd/heuristic@latest
```

Set an OpenAI API key and run a prompt in the current directory:

```sh
export OPENAI_API_KEY="..."
heuristic -p 'Inspect this project and explain how to run its tests.'
```

Or run from source at the repository root:

```sh
go run ./cmd/heuristic -p 'Inspect this project and explain how to run its tests.'
```

Choose a workspace and save the output:

```sh
heuristic -workspace ./my-project -p 'Summarize this project.' > run.jsonl
```

You can also pass a JSON request as an argument or through stdin:

```sh
heuristic '{"prompt":"Summarize this project."}'
heuristic < request.json
```

OpenAI is the default provider. Set `UNREAL_HARNESS_LLM_PROVIDER` to `openai`,
`openai-codex`, `openrouter`, `fireworks`, or `ollama`, and
`UNREAL_HARNESS_LLM_MODEL` to choose a model.

Run `heuristic -h` for options and the JSON request fields.

## Docker

The `ghcr.io/itiscuthnotcup/heuristic` image supports Linux on AMD64 and ARM64. Run it
with a project mounted as the workspace:

```sh
docker run --rm -i --user "$(id -u):$(id -g)" \
  -e OPENAI_API_KEY -v "$PWD:/workspace" \
  ghcr.io/itiscuthnotcup/heuristic:latest -p 'Summarize this project.'
```

Each release also publishes its Git tag (for example, `v0.1.0`) for version pinning.
