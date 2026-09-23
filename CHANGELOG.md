# Changelog

## v0.1.0

- Forked Unreal Agent as MetaCog Agent; module `github.com/ItIsCuthNotCup/MetaCog-Agent`, binary `metacog-agent`. Upstream `UNREAL_HARNESS_*` env vars still work; `METACOG_AGENT_*` equivalents win.
- `harness/llm/metacog`: adaptive metacognition `llm.Adapter` wrapper — judges the greedy answer, branches 2–6 extra paths scaled by judge uncertainty (`stop_confidence`, `n_min`, `n_max`), picks the argmax of text score plus `answer_prior` times the bare-answer score. Judges: `Jev` (TypeSafe `/v1/systemone` noul protocol, concurrent scoring) and `Logprob` (any OpenAI-compatible endpoint with `top_logprobs`, P(yes) readout).
- `metacog-agent` runner: `METACOG_AGENT_*` env config, `"metacog"` request object overrides, `-metacog` flag, trace events as `metacog {...}` JSON lines on stderr.
