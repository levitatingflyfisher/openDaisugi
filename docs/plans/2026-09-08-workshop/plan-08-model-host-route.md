# The Model-Host Route Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `daisugi tiers setup --remote HOST[:PORT]` probes any self-hosted model
server (Ollama, an OpenAI-compatible `/v1`, or an Anthropic-compatible
`/v1/messages`) in one round trip per candidate wire, records what it honestly
found, and the gateway stops forwarding Claude Code's `count_tokens` preflight
to hosts that do not implement it.

**Architecture:** `src/opendaisugi/model_host.py` is the new pure module:
`HostInfo` + `probe()` (Task 2) identify the wire and read the context window;
`record()`/`harness_env()`/`describe_host()` (Task 3) turn a probe into
persisted config and the lines the CLI prints. `daisugi tiers setup --remote`
(Task 4) is the only caller; `daisugi modules`' backend stage grows one more
line when a host is recorded. The gateway's ASGI proxy (Task 5) gets an
`upstream_kind` fact and, when it is not `"anthropic"`, answers
`/v1/messages/count_tokens` itself with `gateway.estimate_prefix_tokens`
instead of forwarding a request most self-hosted servers do not implement.

**Tech Stack:** Python 3.12, httpx (the existing `[gateway]` extra, lazily
imported — no new dependency), pydantic (Config), typer (CLI), pytest +
`httpx.MockTransport` for every test.

**Spec:** `docs/plans/2026-09-08-workshop/spec-08-model-host-route.md`; cites
`docs/plans/2026-09-08-workshop/00-master-spec.md` §5.7, §5.12, §4 (global
constraints, copied below).

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 1–5 are ready to build now** — every cited file:line in the plan matches the current
repo byte-for-byte (`cli.py:2887-2935` `setup_cmd`, `cli.py:3028-3040` the `setup` stub,
`cli.py:2311-2390` `gateway_cmd`, `gateway_asgi.py:159-221/366-418`, `modules.py:67-199`,
`config.py:87`), the Task 1 fixtures (`ollama_tags.json`, `ollama_show.json`) are verified
byte-for-byte against the live `raw.githubusercontent.com/ollama/ollama/main/docs/api.md`
(digests, timestamps, the num_ctx-lives-in-modelfile-not-parameters quirk, and the
`{"model": m}` vs. the spec's stale `{"name": m}` all confirmed), the
`docs.ollama.com/api/anthropic-compatibility` facts the count-tokens shim is built on
(base_url without `/v1`, `ANTHROPIC_AUTH_TOKEN=ollama`, `/v1/messages/count_tokens` explicitly
listed as NOT supported) are confirmed against that page directly, `Config.model` really is
the pre-existing litellm envelope-generation id (`config.py:24`, `cli.py:1802`) so
`llm_host_model` cannot collide with it, and the `--upstream`-vs-`llm_base_url` join
(Task 5 Step 4) fails toward `upstream_kind="anthropic"` (shim inactive) in every case except
an exact string match against a host `record()` actually wrote — the shim cannot fire against
a real Anthropic upstream by accident.

- **SHOULD-FIX — Task 1 `SOURCES.txt`, `anthropic_message.json` entry (plan lines 333-341).**
  The line claims the fixture is a "verbatim example response body for POST /v1/messages" from
  `docs.ollama.com/api/anthropic-compatibility`. Fetched that page directly
  (`docs.ollama.com/api/anthropic-compatibility.md`): it contains curl/Python/JS *request*
  examples only — no example *response* body anywhere on the page. The fixture's shape (id,
  type, role, model, content, stop_reason, usage.input_tokens/output_tokens) does match that
  page's "Supported response fields" checklist, so the field names are sound, but "verbatim"
  overstates the provenance. Fix: reword to "constructed from the page's Supported response
  fields list (id / type / role / model / content / stop_reason / usage(input_tokens,
  output_tokens)) — the page publishes no full example response body."
- **SHOULD-FIX — Task 3 `_KIND_TO_BACKEND` (plan line 1123) cannot produce `llm_backend
  = "llamafile"`, contradicting spec-08's own enumeration.** Spec-08 says `record()` writes
  `llm_backend` as one of `ollama | llamafile | openai-compatible | anthropic-compatible`
  (spec-08-model-host-route.md:50-51). `_KIND_TO_BACKEND` has exactly three entries
  (`ollama`, `openai`→`openai-compatible`, `anthropic`→`anthropic-compatible`) because
  `HostInfo.kind` can only ever be `ollama | openai | anthropic | unknown` — spec-08's own
  Probe section groups llamafile with llama.cpp/vLLM/LM Studio behind the single
  `GET /v1/models` check (spec-08-model-host-route.md:42-43), so the wire shape cannot tell
  them apart. A llamafile host probed via `--remote` therefore always records as
  `llm_backend="openai-compatible"`, never `"llamafile"` — the spec's fourth enum value is
  unreachable from this code path. This is not a bug (nothing breaks; `"llamafile"` stays
  reachable as a `Config.llm_backend` value through the pre-existing, unrelated
  `daisugi swap backend` knob — `swap.py:127-135`), but it is a silent resolution of a spec
  contradiction that belongs beside the plan's other three in "Design rulings this plan makes"
  (plan lines 79-116), not left for a builder to notice on their own. Fix: add a fourth
  ruling there stating the above.
- **NOTE — `swap.py`'s backend-stage TUI panel shows no highlighted button for a recorded
  remote host.** `SWAP_KNOBS["backend"]` (`swap.py:127-135`) has exactly four fixed options
  (`claude-code`, `anthropic`, `llamafile`, `ollama`); the two new `llm_backend` values this
  plan writes (`openai-compatible`, `anthropic-compatible`) match none of them, so
  `selected_label()` (`swap.py:171-175`) returns `None` and the TUI (`tui_wiring.py:115`)
  highlights nothing for that stage. Verified this is display-only: `opendaisugi.llm
  .resolve_backend()` (the function that actually picks the running backend) never reads
  `Config.llm_backend` at all, so the plan's own claim that the field's overlap is "cosmetic,
  not a collision" holds for behavior. No fix required; noting it because it is the one
  concrete side effect of widening that field's vocabulary.
- **NOTE — `gateway_cmd`'s new `upstream_kind` auto-derivation reads config from a fixed
  path, not `--data-dir`.** Task 5 Step 4 calls bare `load_config()`
  (`~/.opendaisugi/config.yaml`), not `load_config(data_dir / "config.yaml")`, so a host
  recorded via `daisugi tiers setup --remote --data-dir /custom` is invisible to
  `daisugi gateway --data-dir /custom`'s automatic `upstream_kind` guess (it silently falls
  back to `"anthropic"`). This exactly mirrors the pre-existing `local_model =
  load_config().gateway_local_model` line right above it (already in the repo, untouched by
  this plan) and fails toward the safe direction (shim inactive), so it is not a regression
  this plan introduces and not a blocker — just worth knowing if `--data-dir` is ever used
  with `--remote`.

## Global Constraints

(Copied verbatim from `00-master-spec.md` §4 — every task's requirements
implicitly include this section.)

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty declares go 1.26.0; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. Zig 0.16 and CMake are *build-time* requirements for go-libghostty; the
  plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  Herdr's vendored commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` (their 1.3.2) is the
  reference for patch notes, not a requirement; the binding chooses the commit it fetches.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

This plan touches no Go code, no PTY/ghostty toolchain, and adds no new
dependency (httpx is already the `[gateway]` extra) — the Go/Zig/CMake/pin
constraints above bind later plans, not this one.

**The §6 prerequisite check, realized rather than skipped.** Master spec §6
says plans 06-09 each start with a check that "verifies the host and stops
with a teaching message if it is absent." Spec-08 itself declares
`Depends on: nothing`, and unlike 06/07/09 there is no ambient workshop VM
this plan assumes is already running — the only real prerequisite is the
`[gateway]` extra (httpx), and this plan realizes the check as working code
rather than a ceremonial standalone task, the same way plan-09 folds its
prerequisite check into Task 1 instead of a separate Task 0: `probe()`
(Task 2) raises a teaching `ImportError` naming `uv add 'opendaisugi[gateway]'`
when httpx is missing, `daisugi tiers setup --remote` (Task 4) catches it and
exits 2 with that exact message, and the one test that needs a real network
host (`test_probe_live_host_has_at_least_one_model`, Task 2 Step 8) is
skip-guarded behind `OPENDAISUGI_TEST_MODEL_HOST` with a reason string. Every
other test in this plan runs against `httpx.MockTransport` fakes — there is
no ambient-host prerequisite to gate the rest of the plan behind.

### Design rulings this plan makes (read before executing)

The spec's prose drifted from three things this plan verified against the
current repo and current upstream docs. Each is resolved below, not left for
an implementer to rediscover:

1. **`daisugi setup` is gone.** It is now `daisugi tiers setup` (a hidden stub
   at the top level redirects with "run: daisugi tiers setup" —
   `src/opendaisugi/cli.py:3028-3040`, tested by
   `tests/test_cli_setup.py::test_setup_is_gone_from_top_level_and_suggests_tiers_setup`).
   `--remote` is added to the real command, `tiers setup`.
2. **Ollama's `/api/show` request key is `"model"`, not `"name"`.** Confirmed
   against `docs.ollama.com/api` (fetched 2026-09-08; see
   `tests/fixtures/model_host/SOURCES.txt`, Task 1). The spec's own prose
   paraphrases it as `{"name": m}`; that is stale. `_probe_ollama` (Task 2)
   sends `{"model": m}`.
3. **`Config.model` already means something else.** It is opendaisugi's own
   envelope-generation model id (a litellm `provider/model` string, e.g.
   `"anthropic/claude-sonnet-4-20250514"` — `src/opendaisugi/config.py:23`).
   Writing a bare host-served model name like `"qwen3-coder"` into it would
   silently break envelope generation. This plan adds a fourth new field,
   `llm_host_model`, instead of reusing `model` (Task 3).

Two smaller decisions, made once here so every task agrees:

- `probe()`'s signature gets one addition beyond the spec's literal text: a
  keyword-only `client: httpx.Client | None = None`, mirroring
  `make_gateway_app`'s own `client` parameter (`gateway_asgi.py:165`). Without
  it nothing could inject an `httpx.MockTransport` and every test would hit
  the network.
- `record()` stays a pure function (writes config, returns it) instead of
  printing, matching every other business-logic module in this repo
  (`local_setup.write_tier1_config`, `config.save_config` — none of them
  print). A new pure function, `describe_host()`, returns the lines; the CLI
  echoes them. The spec's line "`record()` ... prints one line per fact" is
  read as "the feature prints", not "that specific function has a print
  statement in it".

---

### Task 1: Pin the upstream API shapes `probe()` must parse

**Files:**
- Create: `tests/fixtures/model_host/ollama_tags.json`
- Create: `tests/fixtures/model_host/ollama_show.json`
- Create: `tests/fixtures/model_host/openai_models.json`
- Create: `tests/fixtures/model_host/anthropic_message.json`
- Create: `tests/fixtures/model_host/SOURCES.txt`
- Test: `tests/test_model_host.py` (new)

**Interfaces:**
- Consumes: nothing.
- Produces: four fixture files under `tests/fixtures/model_host/`, loaded by
  every later task's tests as `Path(__file__).parent / "fixtures" / "model_host" / "<name>.json"`.

This is a discovery task: it records real upstream response shapes as
committed facts, so later tasks read them instead of guessing. Every field
name used here was fetched from the vendor's own docs on 2026-09-08 (URLs and
the "model" vs "name" correction are in `SOURCES.txt`) — the fixtures are not
constructed by hand.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_model_host.py
"""Probes a self-hosted model server (spec-08): identifies the wire it
speaks, lists its models, and reads its context window honestly — never
trusted, always checked in one round trip. See
docs/plans/2026-09-08-workshop/spec-08-model-host-route.md.

The fixtures under tests/fixtures/model_host/ are pinned, real upstream
response shapes (see SOURCES.txt for URLs and fetch date) — probe() must
parse exactly these, not an invented shape.
"""

from __future__ import annotations

import json
from pathlib import Path

FIXTURES = Path(__file__).parent / "fixtures" / "model_host"


def _load(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text())


def test_ollama_tags_fixture_has_a_named_model():
    tags = _load("ollama_tags.json")
    assert tags["models"][0]["name"] == "deepseek-r1:latest"
    assert tags["models"][1]["name"] == "llama3.2:latest"


def test_ollama_show_fixture_has_a_context_length_and_a_num_ctx_override():
    show = _load("ollama_show.json")
    assert show["model_info"]["llama.context_length"] == 8192
    assert "PARAMETER num_ctx 4096" in show["modelfile"]


def test_openai_models_fixture_has_an_id():
    models = _load("openai_models.json")
    assert models["object"] == "list"
    assert models["data"][0]["id"] == "qwen3-coder:latest"


def test_anthropic_message_fixture_has_usage():
    msg = _load("anthropic_message.json")
    assert msg["type"] == "message"
    assert msg["usage"] == {"input_tokens": 10, "output_tokens": 20}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/test_model_host.py -q`
Expected: FAIL — `FileNotFoundError` (the fixture files do not exist yet).

- [ ] **Step 3: Write the fixture files**

```json
// tests/fixtures/model_host/ollama_tags.json
{
  "models": [
    {
      "name": "deepseek-r1:latest",
      "model": "deepseek-r1:latest",
      "modified_at": "2025-05-10T08:06:48.639712648-07:00",
      "size": 4683075271,
      "digest": "0a8c266910232fd3291e71e5ba1e058cc5af9d411192cf88b6d30e92b6e73163",
      "details": {
        "parent_model": "",
        "format": "gguf",
        "family": "qwen2",
        "families": ["qwen2"],
        "parameter_size": "7.6B",
        "quantization_level": "Q4_K_M"
      }
    },
    {
      "name": "llama3.2:latest",
      "model": "llama3.2:latest",
      "modified_at": "2025-05-04T17:37:44.706015396-07:00",
      "size": 2019393189,
      "digest": "a80c4f17acd55265feec403c7aef86be0c25983ab279d83f3bcd3abbcb5b8b72",
      "details": {
        "parent_model": "",
        "format": "gguf",
        "family": "llama",
        "families": ["llama"],
        "parameter_size": "3.2B",
        "quantization_level": "Q4_K_M"
      }
    }
  ]
}
```

```json
// tests/fixtures/model_host/ollama_show.json
{
  "modelfile": "# Modelfile generated by \"ollama show\"\n# To build a new Modelfile based on this one, replace the FROM line with:\n# FROM llava:latest\n\nFROM /Users/matt/.ollama/models/blobs/sha256:200765e1283640ffbd013184bf496e261032fa75b99498a9613be4e94d63ad52\nTEMPLATE \"\"\"{{ .System }}\nUSER: {{ .Prompt }}\nASSISTANT: \"\"\"\nPARAMETER num_ctx 4096\nPARAMETER stop \"</s>\"\nPARAMETER stop \"USER:\"\nPARAMETER stop \"ASSISTANT:\"",
  "parameters": "num_keep                       24\nstop                           \"<|start_header_id|>\"\nstop                           \"<|end_header_id|>\"\nstop                           \"<|eot_id|>\"",
  "template": "{{ if .System }}<|start_header_id|>system<|end_header_id|>\n\n{{ .System }}<|eot_id|>{{ end }}{{ if .Prompt }}<|start_header_id|>user<|end_header_id|>\n\n{{ .Prompt }}<|eot_id|>{{ end }}<|start_header_id|>assistant<|end_header_id|>\n\n{{ .Response }}<|eot_id|>",
  "details": {
    "parent_model": "",
    "format": "gguf",
    "family": "llama",
    "families": ["llama"],
    "parameter_size": "8.0B",
    "quantization_level": "Q4_0"
  },
  "model_info": {
    "general.architecture": "llama",
    "general.file_type": 2,
    "general.parameter_count": 8030261248,
    "general.quantization_version": 2,
    "llama.attention.head_count": 32,
    "llama.attention.head_count_kv": 8,
    "llama.attention.layer_norm_rms_epsilon": 0.00001,
    "llama.block_count": 32,
    "llama.context_length": 8192,
    "llama.embedding_length": 4096,
    "llama.feed_forward_length": 14336,
    "llama.rope.dimension_count": 128,
    "llama.rope.freq_base": 500000,
    "llama.vocab_size": 128256,
    "tokenizer.ggml.bos_token_id": 128000,
    "tokenizer.ggml.eos_token_id": 128009,
    "tokenizer.ggml.merges": [],
    "tokenizer.ggml.model": "gpt2",
    "tokenizer.ggml.pre": "llama-bpe",
    "tokenizer.ggml.token_type": [],
    "tokenizer.ggml.tokens": []
  },
  "capabilities": ["completion", "vision"]
}
```

```json
// tests/fixtures/model_host/openai_models.json
{
  "object": "list",
  "data": [
    {
      "id": "qwen3-coder:latest",
      "object": "model",
      "created": 1715980800,
      "owned_by": "library"
    }
  ]
}
```

```json
// tests/fixtures/model_host/anthropic_message.json
{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "model": "qwen3-coder",
  "content": [{"type": "text", "text": "..."}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 10, "output_tokens": 20}
}
```

```text
# tests/fixtures/model_host/SOURCES.txt
Pinned upstream API shapes for opendaisugi.model_host (spec-08).
Fetched 2026-09-08.

ollama_tags.json        https://raw.githubusercontent.com/ollama/ollama/main/docs/api.md
                         ("List Local Models", GET /api/tags) — verbatim example response.

ollama_show.json         https://raw.githubusercontent.com/ollama/ollama/main/docs/api.md
                         ("Show Model Information", POST /api/show) — verbatim example
                         response for `curl http://localhost:11434/api/show -d
                         '{"model": "llava"}'`.
                         CORRECTION: the request body key is "model", not "name" — spec-08's
                         prose ("POST /api/show {\"name\": m}") is stale against the current
                         docs. model_host.py sends {"model": m}.
                         NOTE: num_ctx appears here inside "modelfile" (a
                         "PARAMETER num_ctx 4096" line), not inside "parameters" — Ollama's own
                         docs are inconsistent about which field carries it depending on the
                         model, so _ollama_num_ctx() checks both.

openai_models.json       Standard OpenAI /v1/models shape (object/data/id/object/created/
                         owned_by), partially confirmed for Ollama's OpenAI-compat surface at
                         https://docs.ollama.com/openai (created = last-modified time,
                         owned_by defaults to "library"). No full example body is published
                         there; this fixture is the well-known OpenAI schema, not a
                         byte-for-byte copy of a fetched example. Context-window extraction
                         from a /v1/models entry is best-effort (see model_host._probe_openai)
                         and unverified against a real llama.cpp/vLLM server.

anthropic_message.json   https://docs.ollama.com/api/anthropic-compatibility — verbatim
                         example response body for POST /v1/messages. Confirms: the endpoint
                         is /v1/messages; base_url is documented WITHOUT /v1 (the SDK appends
                         it), e.g. ANTHROPIC_BASE_URL=http://host:11434;
                         ANTHROPIC_AUTH_TOKEN=ollama is the documented literal token; the api
                         key is "accepted but not validated"; and
                         /v1/messages/count_tokens is explicitly listed as NOT supported —
                         the premise the count-tokens shim (Task 5) is built on.
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/test_model_host.py -q`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add tests/fixtures/model_host tests/test_model_host.py
git commit -m "$(cat <<'EOF'
test(model-host): pin the Ollama/OpenAI/Anthropic wire shapes probe() must parse

Fetched straight from vendor docs on 2026-09-08 rather than invented:
spec-08's own paraphrase of Ollama's /api/show request body ({"name": m})
turned out to be stale (the real key is "model"). Pinning these as
committed fixtures means every later test parses a real shape, and the
correction is recorded once instead of rediscovered per task.
EOF
)"
```

---

### Task 2: `HostInfo` and `probe()` — identify the wire, read the context window

**Files:**
- Create: `src/opendaisugi/model_host.py`
- Test: `tests/test_model_host.py` (created in Task 1; this task appends to it)

**Interfaces:**
- Consumes: the four fixtures from Task 1.
- Produces: `HostInfo` (`base_url: str`, `kind: str`, `models: list[str]`,
  `chosen: str | None`, `context_window: int | None`, `latency_ms: float`,
  `warnings: list[str]`), `CONTEXT_FLOOR: int`,
  `context_floor_warning(context_window: int | None) -> str | None`,
  `parse_remote(spec: str) -> tuple[str, int | None]`,
  `probe(host: str, port: int | None, *, kind: str = "auto", timeout_s: float = 3.0, client: "httpx.Client | None" = None) -> HostInfo`.
  Task 3 imports `HostInfo`, `context_floor_warning`, `CONTEXT_FLOOR` from
  this module. Task 4 imports `probe`, `parse_remote`. Task 5 does not use
  this module at all (it only needs `gateway.estimate_prefix_tokens`, which
  already exists).

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_model_host.py`:

```python
import httpx
import pytest

from opendaisugi.model_host import CONTEXT_FLOOR, HostInfo, probe


def _mock_client(handler) -> httpx.Client:
    return httpx.Client(transport=httpx.MockTransport(handler))


def test_probe_detects_ollama_and_the_num_ctx_override_beats_context_length():
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        if request.url.path == "/api/show":
            assert json.loads(request.content) == {"model": "deepseek-r1:latest"}
            return httpx.Response(200, json=show)
        return httpx.Response(404)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.kind == "ollama"
    assert info.chosen == "deepseek-r1:latest"
    assert info.models == ["deepseek-r1:latest", "llama3.2:latest"]
    assert info.context_window == 4096  # num_ctx override wins over context_length=8192
    assert any("32K floor" in w for w in info.warnings)
    assert info.latency_ms >= 0


def test_probe_ollama_falls_back_to_context_length_without_a_num_ctx_override():
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")
    show = dict(show, modelfile=show["modelfile"].replace("PARAMETER num_ctx 4096\n", ""))

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        return httpx.Response(200, json=show)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.context_window == 8192


def test_probe_no_floor_warning_when_context_is_wide_enough():
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")
    show = dict(show, modelfile=show["modelfile"].replace("PARAMETER num_ctx 4096\n", ""))
    show = {**show, "model_info": {**show["model_info"], "llama.context_length": 65536}}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        return httpx.Response(200, json=show)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.context_window == 65536
    assert info.warnings == []


def test_probe_ollama_with_no_models_pulled_yet_is_still_identified():
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json={"models": []})
        return httpx.Response(404)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.kind == "ollama"
    assert info.models == []
    assert info.chosen is None


def test_probe_falls_through_to_openai_compatible():
    models = _load("openai_models.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(404)
        if request.url.path == "/v1/models":
            return httpx.Response(200, json=models)
        return httpx.Response(404)

    info = probe("box", 8080, client=_mock_client(handler))
    assert info.kind == "openai"
    assert info.chosen == "qwen3-coder:latest"
    assert info.context_window is None
    assert any("context window unknown" in w for w in info.warnings)


def test_probe_falls_through_to_anthropic_compatible():
    msg = _load("anthropic_message.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path in ("/api/tags", "/v1/models"):
            return httpx.Response(404)
        if request.url.path == "/v1/messages":
            return httpx.Response(200, json=msg)
        return httpx.Response(404)

    info = probe("box", 4000, client=_mock_client(handler))
    assert info.kind == "anthropic"
    assert info.chosen == "qwen3-coder"


def test_probe_reports_unknown_with_a_teaching_warning_when_nothing_answers():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(404)

    info = probe("box", 9999, client=_mock_client(handler))
    assert info.kind == "unknown"
    assert info.models == []
    assert "box:9999" in info.warnings[0]


def test_probe_explicit_kind_skips_the_cascade():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.path)
        return httpx.Response(200, json=_load("openai_models.json"))

    probe("box", 8080, kind="openai", client=_mock_client(handler))
    assert calls == ["/v1/models"]  # never touched /api/tags


def test_probe_rejects_an_unsupported_kind():
    with pytest.raises(ValueError, match="unknown host kind"):
        probe("box", 11434, kind="bogus")


def test_context_floor_warning_thresholds():
    from opendaisugi.model_host import context_floor_warning

    assert context_floor_warning(None) is not None
    assert context_floor_warning(CONTEXT_FLOOR - 1) is not None
    assert context_floor_warning(CONTEXT_FLOOR) is None


def test_parse_remote_splits_host_and_port():
    from opendaisugi.model_host import parse_remote

    assert parse_remote("box:11434") == ("box", 11434)
    assert parse_remote("box") == ("box", None)
    assert parse_remote("my-3090.tailnet.ts.net:8080") == ("my-3090.tailnet.ts.net", 8080)


import os  # noqa: E402 — grouped with the live test below, which needs it


_LIVE_HOST = os.environ.get("OPENDAISUGI_TEST_MODEL_HOST")


@pytest.mark.skipif(
    not _LIVE_HOST, reason="requires OPENDAISUGI_TEST_MODEL_HOST=host[:port] (a real model host)"
)
def test_probe_live_host_has_at_least_one_model():
    from opendaisugi.model_host import parse_remote

    host, port = parse_remote(_LIVE_HOST)
    info = probe(host, port)
    assert len(info.models) >= 1
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_model_host.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.model_host'`.

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/model_host.py
"""Probe a self-hosted model server and report what is actually there.

Local-first means the zero-config path is the machine in front of you
(``daisugi tiers setup`` with no flags already right-sizes a model that runs
here). A remote host — any box on the tailnet or the LAN — is a swap, not a
requirement, and it is never trusted blind: :func:`probe` makes one round
trip per candidate wire (Ollama's native API, an OpenAI-compatible ``/v1``,
or the Anthropic Messages wire) and reports exactly what it found, including
when it found nothing. :func:`record` (see the bottom of this module, added
by spec-08 Task 3) persists the result; this module never guesses a fact it
did not check.

Requires the ``[gateway]`` extra (httpx) — the same one the token-saving
gateway already needs. httpx is imported lazily inside :func:`probe`, so a
bare ``pip install opendaisugi`` still imports this module fine; only calling
``probe()`` without the extra raises a teaching ``ImportError``.
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    import httpx

# Ollama's documented default port (docs.ollama.com) — the assumption a bare
# "host" with no port and no embedded ":" makes. Any other server needs an
# explicit port; there is no universal default across llamafile/vLLM/LM Studio.
_OLLAMA_DEFAULT_PORT = 11434

# The 32K floor agentic coding needs (spec-08). Below it, expect truncation on
# real repos and long tool outputs.
CONTEXT_FLOOR = 32768

_KIND_ORDER = ("ollama", "openai", "anthropic")

_NUM_CTX_RE = re.compile(r"\bnum_ctx\s+(\d+)")


@dataclass(frozen=True)
class HostInfo:
    """What one round trip to a candidate model host actually found."""

    base_url: str
    kind: str  # ollama | openai | anthropic | unknown
    models: list[str]
    chosen: str | None
    context_window: int | None
    latency_ms: float
    warnings: list[str]


@dataclass(frozen=True)
class _Probed:
    """One wire's positive identification: it IS this kind; here is what it has."""

    models: list[str]
    chosen: str | None
    context_window: int | None


def context_floor_warning(context_window: int | None) -> str | None:
    """The one honest warning about a context window, or None when it needs none.

    Shared by :func:`probe` (what it found) and, later, the CLI (what an
    explicit ``--context`` override actually means) so the two never drift.
    """
    if context_window is None:
        return "context window unknown; set it with `--context 32768` if you know it"
    if context_window < CONTEXT_FLOOR:
        return (
            f"{context_window} tokens is below the 32K floor agentic coding needs; "
            "expect truncation"
        )
    return None


def parse_remote(spec: str) -> tuple[str, int | None]:
    """Split a ``host[:port]`` CLI argument into ``(host, port)``.

    No port suffix means ``port=None`` — :func:`probe`/``_base_url`` then
    assumes Ollama's default (11434), the most common self-hosted default.
    A non-decimal suffix is treated as part of the host, not a port (an
    IPv6 literal is out of scope; name the port explicitly in that case).
    """
    if ":" in spec:
        host, _, port_s = spec.rpartition(":")
        if port_s.isdigit():
            return host, int(port_s)
    return spec, None


def _base_url(host: str, port: int | None) -> str:
    if port is not None:
        return f"http://{host}:{port}"
    if ":" in host:
        return f"http://{host}"
    return f"http://{host}:{_OLLAMA_DEFAULT_PORT}"


def _ollama_context_length(show: dict) -> int | None:
    """The model's architectural window: the ``*.context_length`` key in
    ``model_info`` (family-prefixed — ``llama.context_length``,
    ``qwen2.context_length``, ...; docs.ollama.com/api, POST /api/show)."""
    info = show.get("model_info")
    if not isinstance(info, dict):
        return None
    for key, value in info.items():
        if key.endswith(".context_length") and isinstance(value, int):
            return value
    return None


def _ollama_num_ctx(show: dict) -> int | None:
    """The model's DEPLOYED window, if the Modelfile pins ``num_ctx`` — this
    can be smaller than the architectural max, and it is what will actually
    run. Ollama's own docs show ``num_ctx`` inside either ``modelfile`` (a
    ``PARAMETER num_ctx N`` line) or ``parameters`` depending on the model
    (see tests/fixtures/model_host/SOURCES.txt); both are checked.
    """
    for field_name in ("parameters", "modelfile"):
        text = show.get(field_name)
        if isinstance(text, str):
            m = _NUM_CTX_RE.search(text)
            if m:
                return int(m.group(1))
    return None


def _probe_ollama(cl: "httpx.Client", base: str, timeout_s: float) -> "_Probed | None":
    try:
        resp = cl.get(f"{base}/api/tags", timeout=timeout_s)
    except Exception:
        return None
    if resp.status_code != 200:
        return None
    try:
        data = resp.json()
    except Exception:
        return None
    if not isinstance(data.get("models"), list):
        return None
    models = [m["name"] for m in data["models"] if isinstance(m, dict) and m.get("name")]
    chosen = models[0] if models else None
    context_window = None
    if chosen is not None:
        try:
            show_resp = cl.post(f"{base}/api/show", json={"model": chosen}, timeout=timeout_s)
            if show_resp.status_code == 200:
                show = show_resp.json()
                context_window = _ollama_num_ctx(show) or _ollama_context_length(show)
        except Exception:
            pass  # the host IS Ollama; a failed /api/show just leaves context unknown
    return _Probed(models=models, chosen=chosen, context_window=context_window)


# Non-standard extension keys some OpenAI-compatible servers attach to a
# /v1/models entry. No published standard covers this; best-effort only —
# see tests/fixtures/model_host/SOURCES.txt.
_OPENAI_CONTEXT_KEYS = ("context_length", "context_window", "max_model_len", "n_ctx_train")


def _probe_openai(cl: "httpx.Client", base: str, timeout_s: float) -> "_Probed | None":
    try:
        resp = cl.get(f"{base}/v1/models", timeout=timeout_s)
    except Exception:
        return None
    if resp.status_code != 200:
        return None
    try:
        data = resp.json()
    except Exception:
        return None
    entries = data.get("data")
    if not isinstance(entries, list):
        return None
    models = [e["id"] for e in entries if isinstance(e, dict) and e.get("id")]
    chosen = models[0] if models else None
    context_window = None
    for e in entries:
        if isinstance(e, dict) and e.get("id") == chosen:
            for key in _OPENAI_CONTEXT_KEYS:
                value = e.get(key)
                if isinstance(value, int):
                    context_window = value
                    break
            break
    return _Probed(models=models, chosen=chosen, context_window=context_window)


def _probe_anthropic(cl: "httpx.Client", base: str, timeout_s: float) -> "_Probed | None":
    body = {
        "model": "daisugi-probe",
        "max_tokens": 1,
        "messages": [{"role": "user", "content": "hi"}],
    }
    try:
        resp = cl.post(f"{base}/v1/messages", json=body, timeout=timeout_s)
    except Exception:
        return None
    if resp.status_code != 200:
        return None
    try:
        data = resp.json()
    except Exception:
        return None
    if data.get("type") != "message":
        return None
    model_name = data.get("model") or "unknown"
    return _Probed(models=[model_name], chosen=model_name, context_window=None)


_PROBERS = {"ollama": _probe_ollama, "openai": _probe_openai, "anthropic": _probe_anthropic}


def probe(
    host: str,
    port: int | None,
    *,
    kind: str = "auto",
    timeout_s: float = 3.0,
    client: "httpx.Client | None" = None,
) -> HostInfo:
    """Identify what is running at ``host[:port]`` in one round trip per candidate wire.

    ``kind="auto"`` tries, in order, Ollama's native API, an OpenAI-compatible
    ``/v1``, then the Anthropic Messages wire — the first one that answers
    wins. An explicit ``kind`` skips straight to that one probe; if it does
    not answer, the result is ``kind="unknown"`` (never a silent fallback to
    a wire the operator did not ask for). ``client`` is injectable so tests
    can supply an ``httpx.MockTransport``; production creates its own and
    closes it.
    """
    try:
        import httpx
    except ImportError as exc:
        raise ImportError(
            "the model-host probe needs the [gateway] extra: uv add 'opendaisugi[gateway]' "
            "(or: pip install 'opendaisugi[gateway]')"
        ) from exc

    if kind != "auto" and kind not in _PROBERS:
        raise ValueError(
            f"unknown host kind {kind!r}; choose one of: auto, {', '.join(_KIND_ORDER)}"
        )

    base = _base_url(host, port)
    order = list(_KIND_ORDER) if kind == "auto" else [kind]

    owns_client = client is None
    cl = client or httpx.Client()
    t0 = time.monotonic()
    try:
        for k in order:
            result = _PROBERS[k](cl, base, timeout_s)
            if result is None:
                continue
            latency_ms = (time.monotonic() - t0) * 1000
            warning = context_floor_warning(result.context_window)
            return HostInfo(
                base_url=base,
                kind=k,
                models=result.models,
                chosen=result.chosen,
                context_window=result.context_window,
                latency_ms=latency_ms,
                warnings=[warning] if warning else [],
            )
        latency_ms = (time.monotonic() - t0) * 1000
        return HostInfo(
            base_url=base,
            kind="unknown",
            models=[],
            chosen=None,
            context_window=None,
            latency_ms=latency_ms,
            warnings=[
                f"could not identify a model server at {base} (tried {', '.join(order)}); "
                "is it running and reachable?"
            ],
        )
    finally:
        if owns_client:
            cl.close()
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_model_host.py -q`
Expected: PASS (all tests except the live one, which SKIPS —
"requires OPENDAISUGI_TEST_MODEL_HOST=host[:port] (a real model host)").

- [ ] **Step 5: Lint**

Run: `uv run --no-sync ruff check src/opendaisugi/model_host.py tests/test_model_host.py`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/model_host.py tests/test_model_host.py
git commit -m "$(cat <<'EOF'
feat(model-host): probe a self-hosted model host over its own wire

daisugi's zero-config path stays this laptop; a remote box is a swap,
not a requirement, and it is never trusted blind. probe() makes one
round trip per candidate wire (Ollama, OpenAI-compatible, Anthropic-
compatible) and reports exactly what it found — including an honest
"unknown" instead of a guessed kind, and a floor warning when the
window is too small for agentic coding.
EOF
)"
```

---

### Task 3: `record()` — persist a probed host, honestly or not at all

**Files:**
- Modify: `src/opendaisugi/exceptions.py` (append at the end of the file,
  after `StakesInheritanceWarning`, line 108)
- Modify: `src/opendaisugi/config.py:87-88` (the `llm_backend` field — widen
  its comment and add four new fields directly below it)
- Modify: `src/opendaisugi/model_host.py` (add imports at the top; append
  `record()`, `harness_env()`, `describe_host()` at the bottom)
- Test: `tests/test_model_host.py` (created in Task 1/2; this task appends to it)

**Interfaces:**
- Consumes: `HostInfo`, `context_floor_warning` (Task 2); `Config`,
  `load_config`, `save_config` (existing, `src/opendaisugi/config.py`).
- Produces: `ModelHostUnknownError` (new, in `opendaisugi.exceptions`);
  `Config.llm_base_url: str | None`, `Config.llm_host_kind: str | None`,
  `Config.llm_host_model: str | None`, `Config.llm_context_window: int | None`;
  `record(info: HostInfo, *, model: str | None = None, context_window: int | None = None, config_path: Path | None = None) -> Config`;
  `harness_env(info: HostInfo) -> dict[str, str]`;
  `describe_host(info: HostInfo, config: Config) -> list[str]`. Task 4
  imports all three functions plus `ModelHostUnknownError`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_model_host.py`:

```python
from opendaisugi.config import Config, load_config, save_config
from opendaisugi.exceptions import ModelHostUnknownError
from opendaisugi.model_host import describe_host, harness_env, record


def test_record_writes_config_and_maps_kind_to_backend(tmp_path):
    info = HostInfo(
        base_url="http://box:11434",
        kind="ollama",
        models=["qwen3-coder:latest"],
        chosen="qwen3-coder:latest",
        context_window=4096,
        latency_ms=12.0,
        warnings=[],
    )
    path = tmp_path / "config.yaml"
    cfg = record(info, config_path=path)
    assert cfg.llm_base_url == "http://box:11434"
    assert cfg.llm_host_kind == "ollama"
    assert cfg.llm_host_model == "qwen3-coder:latest"
    assert cfg.llm_context_window == 4096
    assert cfg.llm_backend == "ollama"
    reloaded = load_config(path)
    assert reloaded.llm_base_url == "http://box:11434"


def test_record_model_and_context_overrides_beat_the_probe(tmp_path):
    info = HostInfo(
        base_url="http://box:8080",
        kind="openai",
        models=["a", "b"],
        chosen="a",
        context_window=None,
        latency_ms=5.0,
        warnings=["context window unknown; set it with `--context 32768` if you know it"],
    )
    cfg = record(info, model="b", context_window=32768, config_path=tmp_path / "config.yaml")
    assert cfg.llm_host_model == "b"
    assert cfg.llm_context_window == 32768
    assert cfg.llm_backend == "openai-compatible"


def test_record_refuses_an_unknown_host(tmp_path):
    path = tmp_path / "config.yaml"
    info = HostInfo(
        base_url="http://box:9999",
        kind="unknown",
        models=[],
        chosen=None,
        context_window=None,
        latency_ms=1.0,
        warnings=["could not identify a model server at http://box:9999"],
    )
    with pytest.raises(ModelHostUnknownError):
        record(info, config_path=path)
    assert not path.exists()


def test_harness_env_for_ollama_sets_the_documented_auth_token():
    info = HostInfo("http://box:11434", "ollama", ["m"], "m", 4096, 1.0, [])
    assert harness_env(info) == {
        "ANTHROPIC_BASE_URL": "http://box:11434",
        "ANTHROPIC_AUTH_TOKEN": "ollama",
    }


def test_harness_env_for_anthropic_compatible_has_no_extra_token():
    info = HostInfo("http://box:4000", "anthropic", ["m"], "m", None, 1.0, [])
    assert harness_env(info) == {"ANTHROPIC_BASE_URL": "http://box:4000"}


def test_harness_env_is_empty_for_openai_only_hosts():
    info = HostInfo("http://box:8080", "openai", ["m"], "m", None, 1.0, [])
    assert harness_env(info) == {}


def test_describe_host_lines_reflect_the_recorded_config(tmp_path):
    info = HostInfo(
        "http://box:11434", "ollama", ["qwen3-coder:latest"], "qwen3-coder:latest", 4096, 9.0, []
    )
    cfg = record(info, config_path=tmp_path / "config.yaml")
    joined = "\n".join(describe_host(info, cfg))
    assert "host: http://box:11434 (ollama)" in joined
    assert "model: qwen3-coder:latest" in joined
    assert "context window: 4096 tokens" in joined
    assert "warning:" in joined  # 4096 is below the 32K floor
    assert "ANTHROPIC_BASE_URL=http://box:11434" in joined
    assert "ANTHROPIC_AUTH_TOKEN=ollama" in joined


def test_describe_host_warning_is_fresh_after_a_context_override(tmp_path):
    info = HostInfo(
        "http://box:8080",
        "openai",
        ["m"],
        "m",
        None,
        3.0,
        warnings=["context window unknown; set it with `--context 32768` if you know it"],
    )
    cfg = record(info, context_window=65536, config_path=tmp_path / "config.yaml")
    joined = "\n".join(describe_host(info, cfg))
    assert "context window: 65536 tokens" in joined
    assert "warning:" not in joined  # the override cleared the stale probe-time warning


def test_describe_host_teaches_the_gateway_route_for_openai_only_hosts(tmp_path):
    info = HostInfo("http://box:8080", "openai", ["m"], "m", 65536, 3.0, [])
    cfg = record(info, config_path=tmp_path / "config.yaml")
    joined = "\n".join(describe_host(info, cfg))
    assert "OpenAI wire only" in joined
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_model_host.py -q`
Expected: FAIL — `ImportError: cannot import name 'record' from 'opendaisugi.model_host'`
(and `ModelHostUnknownError` does not exist yet either).

- [ ] **Step 3: Add `ModelHostUnknownError`**

Append to the end of `src/opendaisugi/exceptions.py`, after
`StakesInheritanceWarning` (line 108) — exact position in the file does not
matter, the class hierarchy (`OpenDaisugiError` as the base) does:

```python
class ModelHostUnknownError(OpenDaisugiError):
    """`daisugi tiers setup --remote` could not identify the wire a host speaks.

    Raised by ``opendaisugi.model_host.record`` — refuses to persist a host as
    ollama/openai/anthropic when the probe itself came back ``kind="unknown"``.
    A guessed kind would silently misroute every later reader of
    ``llm_host_kind`` (the gateway's count-tokens shim, `daisugi modules`, a
    harness env line) — the ADR-0018/0019 lesson about unmarked provenance,
    applied to the model host instead of the matcher.
    """
```

- [ ] **Step 4: Widen `llm_backend` and add the new Config fields**

In `src/opendaisugi/config.py`, replace:

```python
    # Preferred LLM backend for envelope generation / planning. The running
    # backend is set by OPENDAISUGI_LLM_BACKEND / model; this records intent.
    llm_backend: str = "claude-code"  # claude-code | anthropic | llamafile | ollama
```

with:

```python
    # Preferred LLM backend for envelope generation / planning. The running
    # backend is set by OPENDAISUGI_LLM_BACKEND / model; this records intent.
    # `daisugi tiers setup --remote` (spec-08) also writes this field, with a
    # second vocabulary for the SAME field: the wire a probed remote host
    # speaks. Both readings answer "what backend does this operator's
    # traffic actually run on" — the field is a free string with no runtime
    # dispatch on it, so the overlap is cosmetic, not a collision.
    llm_backend: str = "claude-code"
    # claude-code | anthropic | llamafile | ollama | openai-compatible | anthropic-compatible

    # spec-08: the self-hosted model host `daisugi tiers setup --remote`
    # probed and recorded (any box on the tailnet or LAN — the laptop stays
    # the zero-config default; a remote host is a swap, not a requirement).
    # None means no remote host is recorded. llm_host_kind is the WIRE the
    # probe found (ollama | openai | anthropic), never guessed — see
    # opendaisugi.model_host.probe(). llm_host_model is the model name that
    # host serves — kept separate from `model` above (opendaisugi's OWN
    # envelope-generation model id, a litellm "provider/model" string);
    # conflating the two would silently break envelope generation the day an
    # operator points Claude Code at a bare "qwen3-coder" model name.
    llm_base_url: str | None = None
    llm_host_kind: str | None = None  # ollama | openai | anthropic
    llm_host_model: str | None = None
    llm_context_window: int | None = None  # tokens; None = probe couldn't tell
```

- [ ] **Step 5: Add `record()`, `harness_env()`, `describe_host()`**

In `src/opendaisugi/model_host.py`, change the top of the file from:

```python
from __future__ import annotations

import re
import time
from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    import httpx
```

to:

```python
from __future__ import annotations

import re
import time
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING

from opendaisugi.config import Config, load_config, save_config
from opendaisugi.exceptions import ModelHostUnknownError

if TYPE_CHECKING:
    import httpx
```

Then append at the bottom of `src/opendaisugi/model_host.py`:

```python
_KIND_TO_BACKEND = {
    "ollama": "ollama",
    "openai": "openai-compatible",
    "anthropic": "anthropic-compatible",
}


def record(
    info: HostInfo,
    *,
    model: str | None = None,
    context_window: int | None = None,
    config_path: Path | None = None,
) -> Config:
    """Persist a probed host as the operator's recorded model host.

    Refuses an unidentified host (``ModelHostUnknownError``) — there is
    nothing honest to write for a wire we could not identify. ``model`` and
    ``context_window`` override the probe's own ``chosen``/``context_window``
    (an explicit ``--model``/``--context`` the operator supplied). Returns
    the updated, already-saved ``Config``.
    """
    if info.kind == "unknown":
        reason = info.warnings[-1] if info.warnings else "no server identified"
        raise ModelHostUnknownError(f"cannot record {info.base_url}: {reason}")

    cfg = load_config(config_path)
    cfg = cfg.model_copy(
        update={
            "llm_base_url": info.base_url,
            "llm_host_kind": info.kind,
            "llm_host_model": model or info.chosen,
            "llm_context_window": (
                context_window if context_window is not None else info.context_window
            ),
            "llm_backend": _KIND_TO_BACKEND[info.kind],
        }
    )
    save_config(cfg, config_path)
    return cfg


def harness_env(info: HostInfo) -> dict[str, str]:
    """The exact environment variables that point an Anthropic-wire harness
    (Claude Code) at this host. Empty when the host does not speak that wire
    (an ``openai`` host needs an OpenAI-wire harness instead, or the
    gateway's OpenAI-wire path — spec-09).
    """
    if info.kind == "ollama":
        # docs.ollama.com/api/anthropic-compatibility: base_url WITHOUT /v1
        # (the SDK appends it), and the api key is accepted but not
        # validated — "ollama" is the documented literal token.
        return {"ANTHROPIC_BASE_URL": info.base_url, "ANTHROPIC_AUTH_TOKEN": "ollama"}
    if info.kind == "anthropic":
        return {"ANTHROPIC_BASE_URL": info.base_url}
    return {}


def describe_host(info: HostInfo, config: Config) -> list[str]:
    """The lines `daisugi tiers setup --remote` prints: one per fact, then
    the exact env a harness needs. Reads the warning fresh off ``config``
    (not ``info.warnings``) so an explicit ``--context`` override is
    reflected honestly instead of repeating a now-stale probe-time warning.
    """
    lines = [
        f"host: {info.base_url} ({info.kind})",
        f"model: {config.llm_host_model or 'unset'}",
    ]
    if config.llm_context_window:
        lines.append(f"context window: {config.llm_context_window} tokens")
    else:
        lines.append("context window: unknown")
    warning = context_floor_warning(config.llm_context_window)
    if warning:
        lines.append(f"warning: {warning}")
    env = harness_env(info)
    if env:
        lines.append("point your harness at it:")
        for k, v in env.items():
            lines.append(f"  {k}={v}")
    else:
        lines.append(
            "this host speaks the OpenAI wire only; point an OpenAI-wire harness "
            "at it directly, or route it through `daisugi gateway` once switchyard "
            "lands (spec-09)."
        )
    return lines
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_model_host.py -q`
Expected: PASS (all non-live tests; the live test still SKIPS).

- [ ] **Step 7: Lint**

Run: `uv run --no-sync ruff check src/opendaisugi/model_host.py src/opendaisugi/config.py src/opendaisugi/exceptions.py tests/test_model_host.py`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add src/opendaisugi/model_host.py src/opendaisugi/config.py \
        src/opendaisugi/exceptions.py tests/test_model_host.py
git commit -m "$(cat <<'EOF'
feat(model-host): record a probed host into config, honestly or not at all

record() refuses to persist a host it could not identify — a guessed
kind would silently misroute the gateway's count-tokens shim and
daisugi modules the way an unmarked quantized embedder once poisoned
pathway provenance (ADR-0018/0019). llm_host_model is a new field, not
a reuse of Config.model — that one is opendaisugi's own litellm
envelope-generation model id, and writing a bare host model name into
it would break envelope generation silently.
EOF
)"
```

---

### Task 4: `daisugi tiers setup --remote` and the `daisugi modules` backend line

**Files:**
- Modify: `src/opendaisugi/cli.py:2887-2935` (`setup_cmd`, the `tiers setup` command)
- Modify: `src/opendaisugi/modules.py:110-199` (`detect_stages`'s backend stage)
- Test: `tests/test_cli_setup_remote.py` (new)
- Test: `tests/test_modules.py` (append one test)

**Interfaces:**
- Consumes: `HostInfo`, `probe`, `parse_remote` (Task 2); `record`,
  `describe_host`, `ModelHostUnknownError` (Task 3); the existing `_fail`
  helper (`src/opendaisugi/cli.py:61`); the existing `Stage`, `Module`,
  `ACTIVE` (`src/opendaisugi/modules.py`).
- Produces: no new public functions — `daisugi tiers setup --remote` is the
  end-user surface. Task 5 does not depend on anything from this task.

- [ ] **Step 1: Write the failing CLI tests**

```python
# tests/test_cli_setup_remote.py
"""CLI: `daisugi tiers setup --remote` (spec-08)."""

from __future__ import annotations

import json
from pathlib import Path

import httpx
from typer.testing import CliRunner

import opendaisugi.model_host as model_host
from opendaisugi.cli import app
from opendaisugi.config import load_config

runner = CliRunner()
FIXTURES = Path(__file__).parent / "fixtures" / "model_host"


def _load(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text())


def _patched_probe(handler):
    """A probe() that runs the real code against a mocked transport."""
    real_probe = model_host.probe

    def fake_probe(host, port, *, kind="auto", timeout_s=3.0, client=None):
        return real_probe(
            host,
            port,
            kind=kind,
            timeout_s=timeout_s,
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )

    return fake_probe


def test_setup_remote_ollama_end_to_end(tmp_path, monkeypatch):
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        if request.url.path == "/api/show":
            return httpx.Response(200, json=show)
        return httpx.Response(404)

    monkeypatch.setattr(model_host, "probe", _patched_probe(handler))

    res = runner.invoke(
        app, ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:11434"]
    )
    assert res.exit_code == 0, res.output
    assert "host: http://box:11434 (ollama)" in res.output
    assert "ANTHROPIC_AUTH_TOKEN=ollama" in res.output

    cfg = load_config(tmp_path / "config.yaml")
    assert cfg.llm_host_kind == "ollama"
    assert cfg.llm_host_model == "deepseek-r1:latest"


def test_setup_remote_model_and_context_flags_override_the_probe(tmp_path, monkeypatch):
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        if request.url.path == "/api/show":
            return httpx.Response(200, json=show)
        return httpx.Response(404)

    monkeypatch.setattr(model_host, "probe", _patched_probe(handler))

    res = runner.invoke(
        app,
        [
            "tiers",
            "setup",
            "--data-dir",
            str(tmp_path),
            "--remote",
            "box:11434",
            "--model",
            "llama3.2:latest",
            "--context",
            "65536",
        ],
    )
    assert res.exit_code == 0, res.output
    cfg = load_config(tmp_path / "config.yaml")
    assert cfg.llm_host_model == "llama3.2:latest"
    assert cfg.llm_context_window == 65536
    assert "warning:" not in res.output


def test_setup_remote_and_endpoint_are_mutually_exclusive(tmp_path):
    res = runner.invoke(
        app,
        [
            "tiers",
            "setup",
            "--data-dir",
            str(tmp_path),
            "--remote",
            "box:11434",
            "--endpoint",
            "http://localhost:8080/v1",
            "--model",
            "m",
        ],
    )
    assert res.exit_code == 2
    assert "mutually exclusive" in res.output


def test_setup_remote_refuses_an_unidentified_host(tmp_path, monkeypatch):
    def fake_probe(host, port, *, kind="auto", timeout_s=3.0, client=None):
        return model_host.HostInfo(
            base_url=f"http://{host}:{port}",
            kind="unknown",
            models=[],
            chosen=None,
            context_window=None,
            latency_ms=1.0,
            warnings=[f"could not identify a model server at http://{host}:{port}"],
        )

    monkeypatch.setattr(model_host, "probe", fake_probe)
    res = runner.invoke(
        app, ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:9999"]
    )
    assert res.exit_code == 2
    assert "nothing was written" in res.output
    assert not (tmp_path / "config.yaml").exists()


def test_setup_remote_rejects_a_bad_kind(tmp_path):
    res = runner.invoke(
        app,
        ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:11434", "--kind", "bogus"],
    )
    assert res.exit_code == 2
```

And append to `tests/test_modules.py` (add `AVAILABLE` to the existing
`from opendaisugi.modules import (...)` import at the top of the file — it
currently imports only `ACTIVE`, `detect_stages`, `render_wiring`,
`wiring_json`):

```python
def test_backend_stage_shows_the_recorded_remote_host(tmp_path):
    from opendaisugi.config import Config, save_config

    save_config(
        Config(
            llm_base_url="http://box:11434",
            llm_host_kind="ollama",
            llm_host_model="qwen3-coder:latest",
            llm_context_window=32768,
        ),
        tmp_path / "config.yaml",
    )
    backend = next(s for s in detect_stages(tmp_path) if s.key == "backend")
    host_module = next((m for m in backend.modules if "box:11434" in m.name), None)
    assert host_module is not None
    assert "ollama" in host_module.name
    assert "qwen3-coder:latest" in host_module.name
    assert "32k" in host_module.name
    # AVAILABLE, not ACTIVE: recording a host is not the same as a harness
    # actually routing through it right now (spec-08 honesty tags, §3.5).
    assert host_module.state == AVAILABLE


def test_backend_stage_has_no_host_line_when_none_is_recorded(tmp_path):
    # A regression guard, not a failing test: this already passes today and
    # must keep passing — the four fixed modules are untouched by this change.
    backend = next(s for s in detect_stages(tmp_path) if s.key == "backend")
    assert len(backend.modules) == 4  # the four fixed modules only
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_cli_setup_remote.py tests/test_modules.py -q`
Expected: `test_setup_remote_*` and `test_backend_stage_shows_the_recorded_remote_host`
FAIL — `--remote` is not a recognized option yet (a Click usage error), and
`host_module is not None` fails (`detect_stages` never adds a fifth module
today). `test_backend_stage_has_no_host_line_when_none_is_recorded` PASSES
already — it is a regression guard for the no-host case, not new behavior;
confirm it stays green through Step 5, not that it starts failing here.

- [ ] **Step 3: Wire `--remote` into `setup_cmd`**

In `src/opendaisugi/cli.py`, change:

```python
@tiers_app.command("setup")
def setup_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    endpoint: str = typer.Option(
        None,
        "--endpoint",
        help="OpenAI-compatible local /v1 URL to qualify (e.g. http://localhost:8080/v1).",
    ),
    model: str = typer.Option(
        None, "--model", help="Model name served by --endpoint (required with --endpoint)."
    ),
    threshold: float = typer.Option(
        0.8, "--threshold", help="Min valid-envelope pass rate to promote."
    ),
    repeats: int = typer.Option(1, "--repeats", help="Sample each probe task N times."),
    wire: bool = typer.Option(False, "--wire", help="Persist the model as Tier-1 if it qualifies."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Detect hardware, recommend a hardware-appropriate local model, and (optionally) qualify + wire it.

    With no --endpoint: prints the hardware profile, a size-appropriate llamafile
    recommendation, and the commands to get a local server running. With
    --endpoint + --model: runs the qualification gate against the live model and,
    with --wire, persists it as Tier-1 only if it clears the pass-rate threshold.
    """
    from opendaisugi.hardware import detect_hardware, recommend_model
    from opendaisugi.local_setup import qualify_local_model, write_tier1_config

    if endpoint and not model:
        typer.echo(
            "--model is required with --endpoint (the model name the local server serves).",
            err=True,
        )
        raise typer.Exit(code=2)
```

to:

```python
@tiers_app.command("setup")
def setup_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    endpoint: str = typer.Option(
        None,
        "--endpoint",
        help="OpenAI-compatible local /v1 URL to qualify (e.g. http://localhost:8080/v1).",
    ),
    remote: str = typer.Option(
        None,
        "--remote",
        help="host[:port] of a self-hosted model server to probe and record "
        "(Tailscale or LAN; e.g. --remote my-3090:11434).",
    ),
    kind: str = typer.Option(
        "auto",
        "--kind",
        help="auto | ollama | openai | anthropic — the wire --remote speaks. "
        "auto probes in order and stops at the first one that answers.",
    ),
    context: int = typer.Option(
        None,
        "--context",
        help="Override the probed context window in tokens, e.g. --context 32768.",
    ),
    model: str = typer.Option(None, "--model", help="Model name served by --endpoint or --remote."),
    threshold: float = typer.Option(
        0.8, "--threshold", help="Min valid-envelope pass rate to promote."
    ),
    repeats: int = typer.Option(1, "--repeats", help="Sample each probe task N times."),
    wire: bool = typer.Option(False, "--wire", help="Persist the model as Tier-1 if it qualifies."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Detect hardware, recommend a hardware-appropriate local model, and (optionally) qualify + wire it.

    With no --endpoint or --remote: prints the hardware profile, a
    size-appropriate llamafile recommendation, and the commands to get a
    local server running. With --endpoint + --model: runs the qualification
    gate against the live model and, with --wire, persists it as Tier-1 only
    if it clears the pass-rate threshold. With --remote: probes a self-hosted
    model server (spec-08) and records it as the operator's model host —
    unrelated to local hardware sizing, so it skips straight to the probe.
    """
    from opendaisugi.hardware import detect_hardware, recommend_model
    from opendaisugi.local_setup import qualify_local_model, write_tier1_config

    if endpoint and remote:
        _fail(
            "--endpoint and --remote are mutually exclusive.",
            "--endpoint qualifies a local /v1 server against the envelope-generation "
            "gate; --remote probes and records a model host for your harness to point at.",
            "run one at a time: --endpoint URL --model NAME, or --remote HOST[:PORT].",
            code=2,
        )

    if remote:
        from opendaisugi.exceptions import ModelHostUnknownError
        from opendaisugi.model_host import describe_host, parse_remote, probe, record

        host, port = parse_remote(remote)
        try:
            info = probe(host, port, kind=kind)
        except ImportError as exc:
            _fail(
                str(exc),
                "the remote probe needs an HTTP client.",
                "run: uv add 'opendaisugi[gateway]'",
                code=2,
            )
        except ValueError as exc:
            _fail(
                str(exc),
                "the --kind flag only accepts one of the listed wires.",
                "run: daisugi tiers setup --remote HOST --kind auto",
                code=2,
            )
        try:
            cfg = record(
                info,
                model=model,
                context_window=context,
                config_path=data_dir / "config.yaml",
            )
        except ModelHostUnknownError as exc:
            _fail(
                str(exc),
                "nothing was written — recording an unidentified host would poison the config.",
                "check the host is reachable and serving Ollama, an OpenAI-compatible /v1, "
                "or an Anthropic-compatible /v1/messages. Or pass --kind explicitly.",
                code=2,
            )
        for line in describe_host(info, cfg):
            typer.echo(line)
        return

    if endpoint and not model:
        typer.echo(
            "--model is required with --endpoint (the model name the local server serves).",
            err=True,
        )
        raise typer.Exit(code=2)
```

- [ ] **Step 4: Add the backend host line to `daisugi modules`**

In `src/opendaisugi/modules.py`, change:

```python
    def _lexical_desc() -> str:
        if not lexical_effective or matcher_sel == "lexical":
            return "keyword floor — no model, no download"
        # Active only because the configured backend's package is missing.
        return f"keyword floor — active: {matcher_sel}'s package is not installed"

    return [
```

to:

```python
    def _lexical_desc() -> str:
        if not lexical_effective or matcher_sel == "lexical":
            return "keyword floor — no model, no download"
        # Active only because the configured backend's package is missing.
        return f"keyword floor — active: {matcher_sel}'s package is not installed"

    # spec-08: a fifth line in the backend stage when `daisugi tiers setup
    # --remote` recorded a self-hosted model host. The four fixed modules
    # below never change; this is purely additive and reads config, never
    # guesses — an absent llm_base_url means no line, not a "possible" stub.
    backend_modules = [
        Module("claude-code", ACTIVE if backend == "claude-code" else AVAILABLE, "no API key"),
        Module("anthropic-api", ACTIVE if backend == "anthropic" else AVAILABLE, "BYOK"),
        Module("llamafile / local", AVAILABLE, "base_url, offline"),
        Module("ollama", AVAILABLE, "if running"),
    ]
    if matcher_cfg.llm_base_url:
        from urllib.parse import urlparse

        netloc = urlparse(matcher_cfg.llm_base_url).netloc or matcher_cfg.llm_base_url
        model_part = matcher_cfg.llm_host_model or "model unset"
        ctx_part = (
            f"{matcher_cfg.llm_context_window // 1024}k"
            if matcher_cfg.llm_context_window
            else "context unknown"
        )
        backend_modules.append(
            Module(
                f"{netloc} ({matcher_cfg.llm_host_kind}, {model_part}, {ctx_part})",
                # AVAILABLE, not ACTIVE: recording a host is not the same as a
                # harness actually routing through it (nothing here checks
                # whether ANTHROPIC_BASE_URL or `daisugi gateway --upstream`
                # point at it right now). Claiming ACTIVE would be exactly the
                # dressed-up control §3.5 forbids.
                AVAILABLE,
                "recorded — export the env lines from `tiers setup --remote` to use it",
            )
        )

    return [
```

Then, further down in the same function, change:

```python
(
    Stage(
        "backend",
        "model backend",
        "the LLM for envelope generation / planning (Tier-1)",
        [
            Module("claude-code", ACTIVE if backend == "claude-code" else AVAILABLE, "no API key"),
            Module("anthropic-api", ACTIVE if backend == "anthropic" else AVAILABLE, "BYOK"),
            Module("llamafile / local", AVAILABLE, "base_url, offline"),
            Module("ollama", AVAILABLE, "if running"),
        ],
    ),
)
```

to:

```python
(
    Stage(
        "backend",
        "model backend",
        "the LLM for envelope generation / planning (Tier-1)",
        backend_modules,
    ),
)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_cli_setup_remote.py tests/test_modules.py tests/test_cli_setup.py tests/test_swap.py -q`
Expected: PASS. (`tests/test_cli_setup.py` and `tests/test_swap.py` are the
existing tests for this command and this stage — re-run them to confirm the
edits did not regress the `--endpoint` path or the fixed backend modules.)

- [ ] **Step 6: Lint**

Run: `uv run --no-sync ruff check src/opendaisugi/cli.py src/opendaisugi/modules.py tests/test_cli_setup_remote.py tests/test_modules.py`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add src/opendaisugi/cli.py src/opendaisugi/modules.py \
        tests/test_cli_setup_remote.py tests/test_modules.py
git commit -m "$(cat <<'EOF'
feat(cli): daisugi tiers setup --remote — probe, record, and show the wiring

--remote skips the local-hardware preamble entirely: probing a box on
the tailnet has nothing to do with sizing a model for this laptop.
Mutually exclusive with --endpoint (they answer different questions),
and an unidentified host writes nothing rather than guessing a kind.
daisugi modules grows one more backend line, purely additive, only
when a host is actually recorded.
EOF
)"
```

---

### Task 5: The gateway answers `count_tokens` itself when the upstream can't

**Files:**
- Modify: `src/opendaisugi/gateway_asgi.py:33-38` (imports), `:159-221`
  (`make_gateway_app`), `:366-418` (`serve_gateway`)
- Modify: `src/opendaisugi/cli.py:2311-2390` (`gateway_cmd`)
- Test: `tests/test_gateway_count_tokens.py` (new)

**Interfaces:**
- Consumes: `estimate_prefix_tokens` (existing, `src/opendaisugi/gateway.py:63`);
  `Config.llm_host_kind` (Task 3).
- Produces: `make_gateway_app(..., upstream_kind: str = "anthropic")`,
  `serve_gateway(..., upstream_kind: str = "anthropic")`. No other task
  depends on these — this task is independent of Tasks 2-4.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_gateway_count_tokens.py
"""The count-tokens shim (spec-08): Claude Code calls POST
/v1/messages/count_tokens before every turn. Ollama (and most self-hosted
Anthropic-compatible servers) do not implement it — Ollama's own docs list
it as unsupported, and it has been observed to wedge the server afterwards.
When the configured upstream is not the real Anthropic API, the gateway
answers this one route itself, using the same chars/4 estimate the router
already trusts for cache-prefix sizing (gateway.estimate_prefix_tokens).
"""

from __future__ import annotations

import httpx
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.gateway import estimate_prefix_tokens
from opendaisugi.gateway_asgi import make_gateway_app
from opendaisugi.gateway_pipeline import Gateway

runner = CliRunner()

_BODY = {
    "model": "claude-opus-4-8",
    "max_tokens": 1024,
    "system": "you are a careful coding agent. " * 20,
    "messages": [{"role": "user", "content": "summarize the last tool result: " + ("x" * 380)}],
}


def _mock_upstream(handler):
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def _count_tokens(app):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        return await client.post(
            "/v1/messages/count_tokens",
            params={"beta": "true"},
            json=_BODY,
            headers={"authorization": "Bearer sk-oauth-XYZ", "content-type": "application/json"},
        )


async def test_count_tokens_is_answered_locally_when_upstream_is_not_anthropic():
    def handler(request: httpx.Request) -> httpx.Response:
        raise AssertionError("the shim must never reach the upstream")

    app_ = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        upstream_kind="ollama",
        client=_mock_upstream(handler),
    )
    resp = await _count_tokens(app_)
    assert resp.status_code == 200
    # 1052 chars (system * 20 + the user message) // 4 — see estimate_prefix_tokens.
    assert resp.json() == {"input_tokens": 263}
    assert resp.json()["input_tokens"] == estimate_prefix_tokens(_BODY)


async def test_count_tokens_passes_through_when_upstream_is_real_anthropic():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        return httpx.Response(200, json={"input_tokens": 245})

    app_ = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        upstream_kind="anthropic",
        client=_mock_upstream(handler),
    )
    resp = await _count_tokens(app_)
    assert resp.status_code == 200
    assert seen["path"] == "/v1/messages/count_tokens"  # reached the upstream fake
    upstream_answer = resp.json()["input_tokens"]
    assert upstream_answer == 245  # the upstream's own answer, untouched
    estimate = estimate_prefix_tokens(_BODY)
    assert abs(estimate - upstream_answer) / upstream_answer <= 0.10


async def test_make_gateway_app_defaults_upstream_kind_to_anthropic():
    # The LIBRARY default, not the CLI's — Task 5 Step 4 below adds a separate
    # CLI-level test for how `daisugi gateway` derives upstream_kind, because
    # that derivation is a join against the recorded host, not a bare default.
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"input_tokens": 42})

    app_ = make_gateway_app(
        Gateway(), upstream_base_url="http://up", client=_mock_upstream(handler)
    )
    resp = await _count_tokens(app_)
    assert resp.json() == {"input_tokens": 42}  # untouched — the upstream's own answer


async def test_shim_never_touches_the_openai_wire():
    # Chat Completions has no count_tokens route at all; a request to it must
    # route through the normal pipeline, not the shim's early return.
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"id": "x", "choices": [], "usage": {}})

    app_ = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        upstream_kind="ollama",
        openai_gateway=Gateway(),
        client=_mock_upstream(handler),
    )
    transport = httpx.ASGITransport(app=app_)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        resp = await client.post(
            "/v1/chat/completions",
            json={"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "hi"}]},
        )
    assert resp.status_code == 200
    assert resp.json()["id"] == "x"  # reached the mock upstream, not the shim


def test_gateway_help_mentions_upstream_kind():
    res = runner.invoke(app, ["gateway", "--help"])
    assert res.exit_code == 0
    assert "--upstream-kind" in res.output


def _fake_serve_gateway_capturing(captured):
    def fake(**kwargs):
        captured.update(kwargs)

    return fake


def test_gateway_upstream_kind_stays_anthropic_when_upstream_is_not_the_recorded_host(
    tmp_path, monkeypatch
):
    """A recorded ollama host must never leak its kind onto an unrelated
    --upstream — including the default, the real Anthropic API. Getting this
    join wrong would make the gateway shim count_tokens on a real account."""
    from opendaisugi.config import Config, save_config

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    save_config(
        Config(llm_base_url="http://box:11434", llm_host_kind="ollama"),
        tmp_path / ".opendaisugi" / "config.yaml",
    )
    captured: dict = {}
    monkeypatch.setattr(
        "opendaisugi.gateway_asgi.serve_gateway", _fake_serve_gateway_capturing(captured)
    )

    res = runner.invoke(app, ["gateway"])  # default --upstream: the real Anthropic API
    assert res.exit_code == 0, res.output
    assert captured["upstream_kind"] == "anthropic"


def test_gateway_upstream_kind_resolves_to_the_recorded_kind_when_upstream_matches(
    tmp_path, monkeypatch
):
    from opendaisugi.config import Config, save_config

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    save_config(
        Config(llm_base_url="http://box:11434", llm_host_kind="ollama"),
        tmp_path / ".opendaisugi" / "config.yaml",
    )
    captured: dict = {}
    monkeypatch.setattr(
        "opendaisugi.gateway_asgi.serve_gateway", _fake_serve_gateway_capturing(captured)
    )

    res = runner.invoke(app, ["gateway", "--upstream", "http://box:11434"])
    assert res.exit_code == 0, res.output
    assert captured["upstream_kind"] == "ollama"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_gateway_count_tokens.py -q`
Expected: FAIL — `TypeError: make_gateway_app() got an unexpected keyword
argument 'upstream_kind'` (and the two `test_gateway_upstream_kind_*` tests
fail with `KeyError: 'upstream_kind'` — `gateway_cmd` does not accept
`--upstream-kind` yet, so `serve_gateway` is never called with that key).

- [ ] **Step 3: Add the shim to `gateway_asgi.py`**

Change the imports:

```python
from opendaisugi.gateway_openai import (
    OpenAIUsageSniffer,
    extract_openai_text,
    is_openai_wire,
    normalize_openai_usage,
)
```

to:

```python
from opendaisugi.gateway import estimate_prefix_tokens
from opendaisugi.gateway_openai import (
    OpenAIUsageSniffer,
    extract_openai_text,
    is_openai_wire,
    normalize_openai_usage,
)
```

Add, right after `_prepare` and before `_forward_request_headers`:

```python
_COUNT_TOKENS_PATH = "/v1/messages/count_tokens"


async def _answer_count_tokens_locally(send, body_bytes: bytes, upstream_kind: str) -> None:
    """Answer Claude Code's count_tokens preflight ourselves.

    Ollama (and most self-hosted Anthropic-compatible servers) do not
    implement this route and have been observed to wedge afterwards, so a
    non-anthropic upstream never sees this request. The answer is an honest
    estimate — good enough for Claude Code's display and budgeting, never
    claimed as exact.
    """
    try:
        body = json.loads(body_bytes)
    except (json.JSONDecodeError, ValueError):
        body = {}
    estimate = estimate_prefix_tokens(body)
    _log.debug(
        "count_tokens shim: estimated %d input tokens (upstream_kind=%s)", estimate, upstream_kind
    )
    payload = json.dumps({"input_tokens": estimate}).encode()
    await send(
        {
            "type": "http.response.start",
            "status": 200,
            "headers": [(b"content-type", b"application/json")],
        }
    )
    await send({"type": "http.response.body", "body": payload, "more_body": False})
```

Change `make_gateway_app`'s signature and body from:

```python
def make_gateway_app(
    gateway: "Gateway",
    *,
    upstream_base_url: str = "https://api.anthropic.com",
    openai_gateway: "Gateway | None" = None,
    openai_upstream_base_url: str = "https://api.openai.com",
    client: httpx.AsyncClient | None = None,
):
    """Build the ASGI app. ``client`` is injectable so tests can supply a mock upstream;
    in production it is created per-process with no timeout (agent turns run long).

    Two wires share the proxy, selected per-request by path: Anthropic Messages
    (default) and OpenAI Chat Completions (``…/chat/completions`` — Codex and
    every OpenAI-wire agent). Each wire has its own upstream and its own
    Gateway instance, because a downgrade must land on a model the upstream
    serves — a Claude name would 404 on api.openai.com and vice versa. With no
    ``openai_gateway`` configured, OpenAI-wire requests pass through unrouted
    and unmetered."""
    base = upstream_base_url.rstrip("/")
    openai_base = openai_upstream_base_url.rstrip("/")

    async def app(scope, receive, send) -> None:
        if scope["type"] != "http":
            return

        wire = "openai" if is_openai_wire(scope["path"]) else "anthropic"
        active_gateway = openai_gateway if wire == "openai" else gateway
        body_bytes = await _read_request_body(receive)
        if active_gateway is not None:
```

to:

```python
def make_gateway_app(
    gateway: "Gateway",
    *,
    upstream_base_url: str = "https://api.anthropic.com",
    upstream_kind: str = "anthropic",
    openai_gateway: "Gateway | None" = None,
    openai_upstream_base_url: str = "https://api.openai.com",
    client: httpx.AsyncClient | None = None,
):
    """Build the ASGI app. ``client`` is injectable so tests can supply a mock upstream;
    in production it is created per-process with no timeout (agent turns run long).

    Two wires share the proxy, selected per-request by path: Anthropic Messages
    (default) and OpenAI Chat Completions (``…/chat/completions`` — Codex and
    every OpenAI-wire agent). Each wire has its own upstream and its own
    Gateway instance, because a downgrade must land on a model the upstream
    serves — a Claude name would 404 on api.openai.com and vice versa. With no
    ``openai_gateway`` configured, OpenAI-wire requests pass through unrouted
    and unmetered.

    ``upstream_kind`` (spec-08) names the wire the ANTHROPIC upstream actually
    speaks: ``"anthropic"`` (the real API — the default; nothing changes) or
    ``"ollama"`` / ``"openai-compatible"`` / ``"anthropic-compatible"`` (a
    self-hosted host recorded by `daisugi tiers setup --remote`). Any value
    other than ``"anthropic"`` makes this app answer
    ``/v1/messages/count_tokens`` itself instead of forwarding it — most
    self-hosted servers do not implement that route."""
    base = upstream_base_url.rstrip("/")
    openai_base = openai_upstream_base_url.rstrip("/")

    async def app(scope, receive, send) -> None:
        if scope["type"] != "http":
            return

        wire = "openai" if is_openai_wire(scope["path"]) else "anthropic"
        active_gateway = openai_gateway if wire == "openai" else gateway
        body_bytes = await _read_request_body(receive)

        if (
            wire == "anthropic"
            and upstream_kind != "anthropic"
            and scope["path"].rstrip("/") == _COUNT_TOKENS_PATH
        ):
            await _answer_count_tokens_locally(send, body_bytes, upstream_kind)
            return

        if active_gateway is not None:
```

Change `serve_gateway`'s signature and the `make_gateway_app` call from:

```python
def serve_gateway(
    *,
    host: str = "127.0.0.1",
    port: int = 8787,
    upstream_base_url: str = "https://api.anthropic.com",
    data_dir: Path | None = None,
    cheap_model: str | None = None,
    gateway: "Gateway | None" = None,
    capture_answers: bool = False,
    local_model: str | None = None,
    openai_upstream_base_url: str = "https://api.openai.com",
    openai_cheap_model: str | None = "gpt-5-mini",
) -> None:
```

to:

```python
def serve_gateway(
    *,
    host: str = "127.0.0.1",
    port: int = 8787,
    upstream_base_url: str = "https://api.anthropic.com",
    upstream_kind: str = "anthropic",
    data_dir: Path | None = None,
    cheap_model: str | None = None,
    gateway: "Gateway | None" = None,
    capture_answers: bool = False,
    local_model: str | None = None,
    openai_upstream_base_url: str = "https://api.openai.com",
    openai_cheap_model: str | None = "gpt-5-mini",
) -> None:
```

and, further down in the same function:

```python
    app = make_gateway_app(
        gateway,
        upstream_base_url=upstream_base_url,
        openai_gateway=openai_gateway,
        openai_upstream_base_url=openai_upstream_base_url,
    )
    uvicorn.run(app, host=host, port=port)
```

to:

```python
    app = make_gateway_app(
        gateway,
        upstream_base_url=upstream_base_url,
        upstream_kind=upstream_kind,
        openai_gateway=openai_gateway,
        openai_upstream_base_url=openai_upstream_base_url,
    )
    uvicorn.run(app, host=host, port=port)
```

- [ ] **Step 4: Wire `--upstream-kind` into `daisugi gateway`**

In `src/opendaisugi/cli.py`, change:

```python
    openai_cheap_model: str = typer.Option(
        "gpt-5-mini",
        "--openai-cheap-model",
        help="Model an easy OpenAI-wire turn is routed onto (must be a model the OpenAI "
        "upstream serves). Empty string disables routing on that wire (pure passthrough).",
    ),
) -> None:
    """Run the token-saving gateway: a local proxy any harness points at via base_url.
```

to:

```python
    openai_cheap_model: str = typer.Option(
        "gpt-5-mini",
        "--openai-cheap-model",
        help="Model an easy OpenAI-wire turn is routed onto (must be a model the OpenAI "
        "upstream serves). Empty string disables routing on that wire (pure passthrough).",
    ),
    upstream_kind: str = typer.Option(
        None,
        "--upstream-kind",
        help="ollama | openai | anthropic — the wire --upstream actually speaks. Defaults to "
        "the kind `daisugi tiers setup --remote` recorded, but ONLY when --upstream still "
        "points at that same recorded host; any other --upstream (including the default, "
        "the real Anthropic API) defaults to anthropic. A non-anthropic kind makes the "
        "gateway answer Claude Code's count_tokens preflight itself instead of forwarding "
        "it, since most self-hosted servers do not implement that route.",
    ),
) -> None:
    """Run the token-saving gateway: a local proxy any harness points at via base_url.
```

Then, inside the function body, change:

```python
    from opendaisugi.config import load_config
    from opendaisugi.gateway_asgi import serve_gateway

    if local_model is None:
        local_model = load_config().gateway_local_model
    typer.echo(f"opendaisugi gateway  →  {upstream}")
```

to:

```python
    from opendaisugi.config import load_config
    from opendaisugi.gateway_asgi import serve_gateway

    if local_model is None:
        local_model = load_config().gateway_local_model
    if upstream_kind is None:
        # Only trust the recorded kind when --upstream still names that SAME
        # host — a stale recording plus the default (the real Anthropic API)
        # must never make this gateway shim a real, working count_tokens call.
        cfg = load_config()
        upstream_kind = (
            cfg.llm_host_kind
            if cfg.llm_base_url and upstream.rstrip("/") == cfg.llm_base_url.rstrip("/")
            else "anthropic"
        )
    typer.echo(f"opendaisugi gateway  →  {upstream}")
```

and, further down, change:

```python
    serve_gateway(
        host=host,
        port=port,
        upstream_base_url=upstream,
        data_dir=data_dir,
        local_model=local_model,
        cheap_model=cheap_model,
        capture_answers=capture_answers,
        openai_upstream_base_url=openai_upstream,
        openai_cheap_model=openai_cheap_model or None,
    )
```

to:

```python
    serve_gateway(
        host=host,
        port=port,
        upstream_base_url=upstream,
        upstream_kind=upstream_kind,
        data_dir=data_dir,
        local_model=local_model,
        cheap_model=cheap_model,
        capture_answers=capture_answers,
        openai_upstream_base_url=openai_upstream,
        openai_cheap_model=openai_cheap_model or None,
    )
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_gateway_count_tokens.py tests/test_gateway_asgi.py -q`
Expected: PASS. (`tests/test_gateway_asgi.py` is the existing ASGI suite —
re-run it to confirm the new early-return branch does not change any
existing routing/streaming behavior.)

- [ ] **Step 6: Lint**

Run: `uv run --no-sync ruff check src/opendaisugi/gateway_asgi.py src/opendaisugi/cli.py tests/test_gateway_count_tokens.py`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add src/opendaisugi/gateway_asgi.py src/opendaisugi/cli.py \
        tests/test_gateway_count_tokens.py
git commit -m "$(cat <<'EOF'
feat(gateway): answer count_tokens locally when the upstream can't

Ollama's own docs list /v1/messages/count_tokens as unsupported, and
it has been observed to wedge the server afterwards. When
--upstream-kind names anything but the real Anthropic API, the
gateway answers this one route itself with the same chars/4 estimate
the router already trusts for cache-prefix sizing — an honest
estimate, never claimed exact, and Claude Code only uses it for
display and budgeting.
EOF
)"
```

---

## Final verification

Run the full suite once, from the repo root:

```bash
uv run --no-sync pytest -q
uv run --no-sync ruff check .
uv run --no-sync ruff format --check .
```

All three must be clean before this plan is considered done — CI runs
`ruff format --check` in addition to `ruff check` (`.github/workflows/ci.yml`),
and this repo's ruff config deliberately does not enforce E501 (line length),
so `ruff check .` alone would not catch a call expression `ruff format` wants
wrapped differently than this plan wrote it. The one test that requires a
real model host (`test_probe_live_host_has_at_least_one_model`, Task 2) will
show as `SKIPPED` unless `OPENDAISUGI_TEST_MODEL_HOST` is set — that is the
expected, honest result on a box with no such host reachable.
