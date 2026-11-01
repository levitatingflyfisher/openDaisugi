# W1: Ollama qualification and record-by-default Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `daisugi tiers setup --endpoint` qualifies a thinking model on an Ollama host (no thinking, truncation named as truncation, no llamafile advice), and a plain `daisugi install` records every tool call in audit mode with `--enforce` the one opt-in to blocking, in all three ports.

**Architecture:** Python is the oracle; Go and Rust mirror it byte for byte and the golden cases prove it (`clients/k3_cases.py` for `tiers setup`, `clients/cli_cases.py` for `install`). W1-a adds one host-kind question to the endpoint path (`GET {root}/api/tags`, the same request `--remote` already sends), threads a `reasoning_effort` value through the existing Tier-1 adapter into the existing chat-body field, and renames one probe outcome. W1-b moves `Layer.GATE` into the default layer set, drops `--gate`, and rewrites every string that told the user to pass it.

**Tech Stack:** Python 3.12 (typer, httpx, pytest), Go (cgo, `net/http/httptest`), Rust (cargo), the k3 and cli golden-case writers and compare scripts.

**Spec:** `docs/plans/2026-10-30-backlog/INDEX.md` (W1-a, W1-b) and the ledger `.superpowers/sdd/backlog/progress.md`. Owner ruling 2026-10-28: a plain install records every call; blocking is opt-in via `--enforce`. Observed on the GPU box: Gemma 4 on Ollama thinks by default and the three probes end in "max_tokens length limit"; Ollama's `/v1` honours `"reasoning_effort":"none"` and ignores `"think":false`.

## Global Constraints

- Laptop: 4 cores, 15 GB. ONE heavy job at a time (a Go or cargo build, a cargo test, the full pytest suite, a full compare), each under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G <command>`, with `-j2` (`go test -p 1`, `CARGO_BUILD_JOBS=2`).
- `/tmp` is RAM. No scratch there. Scratch goes under `/mnt/tera/.tt/w1/` (set `DAISUGI_K3_SCRATCH=/mnt/tera/.tt/w1/k3` for the k3 writer and compare).
- No network in tests. Fake HTTP servers on loopback only. No real agents (`claude`, `codex`, `pi`, `opencode`, `sprig`) and no real models in tests.
- Never `pkill -f`. Never touch `~/.opendaisugi/gate` or the owner's `~/.claude`. Never run `daisugi install` outside a test HOME.
- Every coppice run passes its own `--socket` and `--data-dir` (this plan runs coppice tests only, no server).
- Commits: atomic, one concern each, the message states the why. No AI attribution lines. Never commit `CLAUDE.md` or `docs/superpowers/`. Commit by explicit path.
- Writing: ASD-STE100. Short sentences, plain words, active voice. No em dashes in new text. Never the words "seam" or "the layer". Docs state the current behaviour once; no changelog voice.
- Names stay as they are today: gate, gateway, foreman. The rename is the next wave.
- Never rebuild Z3; never `cargo clean` in `clients/rust`; never change `clients/go/scripts/native.sh`.
- Full parity: a behaviour the oracle gains, Go and Rust gain in the same wave, and the golden cases cover it. A port that cannot do a case refuses it (exit 2, one line, nothing written) and that refusal gets a ruling in `clients/ADJUDICATIONS.md`.
- JSON key order and file bytes matter: the compare scripts read trees whole. Insert new keys at the positions this plan names.

## Review Focus

1. An Ollama host serving a model that cannot think (llama3.2) gets `"reasoning_effort":"none"` too. The fake cannot prove Ollama accepts it. Task 4 adds the `--kind openai` escape and its test; the box check is in Task 7.
2. An `--endpoint` with no `/v1` suffix, or with a trailing slash, must send the kind question to `{root}/api/tags` with `root` cut the way `endpoint_context` cuts it. Task 2 pins both forms.
3. `/api/tags` that answers 200 with text that is not JSON, or JSON with no `models` list, means "not Ollama", never a crash and never a refusal. Task 2 pins both.
4. `--kind anthropic --endpoint URL` is refused before any request and before the hardware probe. Task 4 pins it.
5. After W1-b, a plain `daisugi install` on a box with only Hermes or OpenClaw prints the gate as an honest gap and exits 0; `daisugi install --enforce` with no envelope refuses before any write. Task 9 pins both.

---

## Part A: W1-a, `tiers setup` against an Ollama host

### Task 1: The oracle names truncation and carries `reasoning_effort` through Tier-1

**Files:**
- Modify: `src/opendaisugi/tier1.py` (`classify_failure`, `HTTPTier1Provider.__init__`, `HTTPTier1Provider.probe_envelope`, `OllamaTier1Provider.__init__`)
- Modify: `src/opendaisugi/local_setup.py` (`QualificationResult`, `write_tier1_config`, `load_configured_tier1`)
- Test: `tests/test_tier1_http.py`, `tests/test_local_setup.py`

**Interfaces:**
- Consumes: `llm_client._Completions.create(..., reasoning_effort: str | None)` and `llm_client.request_body(..., reasoning_effort=...)`, which already write `"reasoning_effort"` as the last key of a chat body. Nothing in `llm_client.py` changes.
- Produces:
  - `tier1.OLLAMA_NO_THINK_EFFORT: str = "none"`
  - `tier1.TRUNCATED_ADVICE: str = " A model that thinks first spends the limit on thinking: turn thinking off at the server, or raise the limit."`
  - `tier1.classify_failure(exc) -> tuple[str, str]` returns `("truncated", _clean_reason(msg + TRUNCATED_ADVICE))` when `msg` starts with `_CUT_PREFIX`. The outcome vocabulary is now `valid | declined | truncated | error | timeout`.
  - `HTTPTier1Provider(model, *, base_url=None, api_key=None, timeout_s=30.0, name=None, backend=None, reasoning_effort: str | None = None)`; the value is stored as `self.reasoning_effort` and passed as `reasoning_effort=self.reasoning_effort` to `client.chat.completions.create(...)` in `probe_envelope`.
  - `OllamaTier1Provider(..., reasoning_effort: str | None = OLLAMA_NO_THINK_EFFORT)` forwards it.
  - `local_setup.write_tier1_config(data_dir, *, model: str, base_url: str | None, host_kind: str) -> Path` writes `{"model": ..., "base_url": ..., "host_kind": ...}` with `json.dumps(indent=2)` plus a newline, keys in that order.
  - `local_setup.load_configured_tier1(data_dir)` builds `HTTPTier1Provider(model=model, base_url=cfg.get("base_url"), reasoning_effort=OLLAMA_NO_THINK_EFFORT if cfg.get("host_kind") == "ollama" else None)`. A missing or different `host_kind` means `None`.
  - `QualificationResult.all_failures_truncated() -> bool`: True when `valid < attempts` and every outcome that is not `valid` is `truncated`.

- [ ] **Step 1: Write the failing tests**

In `tests/test_tier1_http.py`:

```python
def test_cut_reply_is_truncated_not_declined() -> None:
    from opendaisugi.llm_client import CUT_TEXT, ModelCallError
    from opendaisugi.tier1 import TRUNCATED_ADVICE, classify_failure

    outcome, reason = classify_failure(ModelCallError(CUT_TEXT))
    assert outcome == "truncated"
    assert reason == CUT_TEXT + TRUNCATED_ADVICE
    assert len(reason) <= 200


@pytest.mark.asyncio
async def test_reasoning_effort_reaches_the_client_call() -> None:
    provider = HTTPTier1Provider("m", base_url="http://localhost:11434/v1", reasoning_effort="none")
    with patch("opendaisugi.llm.get_model_client") as factory:
        create = AsyncMock(return_value=_mk_envelope())
        factory.return_value.chat.completions.create = create
        await provider.generate_envelope("t")
        assert create.await_args.kwargs["reasoning_effort"] == "none"


@pytest.mark.asyncio
async def test_no_reasoning_effort_by_default() -> None:
    provider = HTTPTier1Provider("m", base_url="http://localhost:8080/v1")
    with patch("opendaisugi.llm.get_model_client") as factory:
        create = AsyncMock(return_value=_mk_envelope())
        factory.return_value.chat.completions.create = create
        await provider.generate_envelope("t")
        assert create.await_args.kwargs["reasoning_effort"] is None


def test_ollama_provider_turns_thinking_off() -> None:
    from opendaisugi.tier1 import OllamaTier1Provider

    assert OllamaTier1Provider("llama3.2:3b").reasoning_effort == "none"
```

In `tests/test_local_setup.py`:

```python
def test_write_then_load_tier1_config_keeps_host_kind(tmp_path):
    from opendaisugi.local_setup import load_configured_tier1, write_tier1_config

    p = write_tier1_config(tmp_path, model="m", base_url="http://h:11434/v1", host_kind="ollama")
    assert json.loads(p.read_text()) == {
        "model": "m",
        "base_url": "http://h:11434/v1",
        "host_kind": "ollama",
    }
    assert list(json.loads(p.read_text())) == ["model", "base_url", "host_kind"]
    assert load_configured_tier1(tmp_path).reasoning_effort == "none"
    write_tier1_config(tmp_path, model="m", base_url="http://h:8080/v1", host_kind="openai")
    assert load_configured_tier1(tmp_path).reasoning_effort is None


def test_load_tier1_config_without_host_kind_sends_no_effort(tmp_path):
    from opendaisugi.local_setup import load_configured_tier1

    (tmp_path / "local_tier1.json").write_text('{"model": "m", "base_url": "http://h/v1"}')
    assert load_configured_tier1(tmp_path).reasoning_effort is None


def test_all_failures_truncated():
    from opendaisugi.local_setup import QualificationResult

    def r(kinds):
        return QualificationResult(
            attempts=len(kinds),
            valid=kinds.count("valid"),
            pass_rate=0.0,
            passed=False,
            threshold=0.8,
            outcomes=[("t", k) for k in kinds],
            reasons=[""] * len(kinds),
        )

    assert r(["truncated", "truncated"]).all_failures_truncated()
    assert r(["valid", "truncated"]).all_failures_truncated()
    assert not r(["truncated", "declined"]).all_failures_truncated()
    assert not r(["valid", "valid"]).all_failures_truncated()
```

Also update the existing `test_write_then_load_tier1_config` to pass `host_kind="openai"`.

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_tier1_http.py tests/test_local_setup.py`
Expected: FAIL (`TypeError: unexpected keyword 'reasoning_effort'`, `assert 'declined' == 'truncated'`, `AttributeError: all_failures_truncated`).

- [ ] **Step 3: Implement**

In `tier1.py`: the two constants, the `classify_failure` branch, the constructor field, the `create(...)` kwarg, the `OllamaTier1Provider` default. Update the docstrings that list outcomes (`classify_failure`, `probe_envelope`). In `local_setup.py`: the `host_kind` keyword (required, keyword-only), the loader mapping, the method, and the comment on `QualificationResult.outcomes` that lists the outcomes.

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_tier1_http.py tests/test_local_setup.py tests/test_tier1_ollama.py tests/test_envelope_tier1_routing.py`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/tier1.py src/opendaisugi/local_setup.py tests/test_tier1_http.py tests/test_local_setup.py
git commit -m "tier1: a reply cut at the token limit is truncated, not declined; carry reasoning_effort and host_kind

A Gemma 4 probe on Ollama ended 0/3 with 'declined' and advice to raise
max_tokens. The model had spent the limit on thinking. The outcome is now
'truncated', the Tier-1 adapter can send reasoning_effort, and the wired
config remembers the host kind so later Tier-1 calls send it too."
```

### Task 2: `model_host.endpoint_kind`: one question to the server

**Files:**
- Modify: `src/opendaisugi/model_host.py` (after `endpoint_context`)
- Test: `tests/test_model_host.py`

**Interfaces:**
- Consumes: `model_host._send`, `model_host._Reach`, the `root` rule from `endpoint_context` (`endpoint.rstrip("/").removesuffix("/v1")`).
- Produces: `endpoint_kind(endpoint: str, *, timeout_s: float = 3.0, client: "httpx.Client | None" = None) -> str`, returning `"ollama"` when `GET {root}/api/tags` answers 200 with a JSON object whose `models` is a list, else `"openai"`. Any transport error, any non-200, any body that is not that shape: `"openai"`. It never raises. `client` is injectable for `httpx.MockTransport`, as `probe` does it.

- [ ] **Step 1: Write the failing tests**

Follow the `MockTransport` style already in `tests/test_model_host.py`:

```python
def test_endpoint_kind_ollama_from_api_tags():
    seen = []

    def handler(req):
        seen.append(str(req.url))
        if req.url.path == "/api/tags":
            return httpx.Response(200, json={"models": [{"name": "gemma4:latest"}]})
        return httpx.Response(404)

    cl = httpx.Client(transport=httpx.MockTransport(handler))
    assert endpoint_kind("http://box:11434/v1", client=cl) == "ollama"
    assert seen == ["http://box:11434/api/tags"]


@pytest.mark.parametrize("endpoint", ["http://box:8080", "http://box:8080/", "http://box:8080/v1/"])
def test_endpoint_kind_cuts_the_root_like_endpoint_context(endpoint):
    seen = []

    def handler(req):
        seen.append(str(req.url))
        return httpx.Response(404)

    cl = httpx.Client(transport=httpx.MockTransport(handler))
    assert endpoint_kind(endpoint, client=cl) == "openai"
    assert seen == ["http://box:8080/api/tags"]


@pytest.mark.parametrize(
    "resp",
    [
        httpx.Response(200, text="nope"),
        httpx.Response(200, json={"data": []}),
        httpx.Response(200, json={"models": "x"}),
        httpx.Response(500, text="{}"),
    ],
)
def test_endpoint_kind_anything_else_is_openai(resp):
    cl = httpx.Client(transport=httpx.MockTransport(lambda req: resp))
    assert endpoint_kind("http://box:8080/v1", client=cl) == "openai"


def test_endpoint_kind_unreachable_is_openai():
    def handler(req):
        raise httpx.ConnectError("refused")

    cl = httpx.Client(transport=httpx.MockTransport(handler))
    assert endpoint_kind("http://box:1/v1", client=cl) == "openai"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_model_host.py -k endpoint_kind`
Expected: FAIL with `ImportError: cannot import name 'endpoint_kind'`.

- [ ] **Step 3: Implement `endpoint_kind` in `model_host.py`**

One GET through `_send`. Own the client only when none is injected (`with httpx.Client() as cl`). Use `timeout_s` on the request, as `_probe_ollama` does. The shape test is exactly `isinstance(data, dict) and isinstance(data.get("models"), list)`.

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_model_host.py`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/model_host.py tests/test_model_host.py
git commit -m "model_host: endpoint_kind asks a /v1 endpoint once whether it is Ollama

tiers setup --endpoint needs the host kind to decide whether to send
reasoning_effort none. The question is the first request --remote already
sends, GET /api/tags, and anything but an Ollama answer means openai."
```

### Task 3: `tiers setup --endpoint` in the oracle: kind, no thinking, truncation, no llamafile

**Files:**
- Modify: `src/opendaisugi/cli.py` (`setup_cmd`, lines 5231 to 5500)
- Test: `tests/test_setup_endpoint_qualify.py`

**Interfaces:**
- Consumes: Task 1 (`OLLAMA_NO_THINK_EFFORT`, `HTTPTier1Provider(reasoning_effort=...)`, `write_tier1_config(host_kind=...)`, `all_failures_truncated()`), Task 2 (`endpoint_kind`).
- Produces the oracle's exact behaviour the ports mirror in Tasks 5 and 6:
  1. `--kind` help text becomes: `"auto | ollama | openai | anthropic. The wire the host speaks. With --remote, auto probes in order and stops at the first that answers. With --endpoint, auto asks the server once whether it is Ollama; ollama and openai skip the question. An Ollama host gets reasoning_effort none, so a thinking model answers without thinking."`
  2. With `--endpoint` and `--kind` not in `("auto", "ollama", "openai")`: stderr `--kind 'anthropic' does not apply to --endpoint: an endpoint speaks the OpenAI wire. Use --kind auto, ollama or openai.` and exit 2. This check runs right after the `--model is required with --endpoint` check, before `detect_hardware()` and before any request. (`{kind!r}` formatting, as the existing messages do.)
  3. After the claude-code refusal and before the provider is built: `host_kind = endpoint_kind(endpoint) if kind == "auto" else kind`.
  4. `provider = HTTPTier1Provider(model=model, base_url=endpoint, backend="api", timeout_s=QUALIFY_TIMEOUT_S, reasoning_effort=OLLAMA_NO_THINK_EFFORT if host_kind == "ollama" else None)`.
  5. Right after the existing `qualifying ... probes` note, and only when `host_kind == "ollama"`: `console.note("  host: ollama, thinking off (reasoning_effort none); pass --kind openai to send the request as is")`.
  6. `--wire` writes `write_tier1_config(data_dir, model=model, base_url=endpoint, host_kind=host_kind)`.
  7. `runtime = "ollama" if host_kind == "ollama" else rec.runtime` (default `rec.runtime` when there is no endpoint). The JSON `recommendation.runtime` and the text `Recommended: a {size}-class instruct model at {quant} via {runtime} (~{gb}GB).` both use it.
  8. JSON: `qualification["host_kind"] = host_kind`, inserted between `"wired"` and `"outcomes"`.
  9. Text: after the `Qualification:` line and the optional `Context window:` line, one line `  Host kind: {host_kind}`, with ` (thinking off: reasoning_effort none)` appended when ollama. Then the existing `First failure` line.
  10. The verdict arrows, in order: wired; passed and not `--wire`; all errored (unchanged text); new: `qual.all_failures_truncated()` prints `  → Every failed probe was truncated: the model stops at the token limit before the envelope is complete. Turn thinking off at the server, or raise its output limit, then re-run.`; else the unchanged `Not promoted` line.
  11. The no-endpoint path (hardware advice, "Serve it with llamafile") does not change.

- [ ] **Step 1: Write the failing tests**

Add to `tests/test_setup_endpoint_qualify.py` (the `box` fixture and `chat` helper are there; the `Server` class answers by path, so `/api/tags` is one more route; it records request bodies nowhere, so add a `bodies: list[bytes]` list to `Server` filled in `_answer` from the bytes it reads):

```python
TAGS = (200, json.dumps({"models": [{"name": "gemma4:latest"}]}))


def test_ollama_endpoint_gets_reasoning_effort_none(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "gemma4", "--json")
    assert res.exit_code == 0, res.output
    assert srv.hits["/api/tags"] == 1
    posted = [json.loads(b) for b in srv.bodies if b]
    assert posted and all(b.get("reasoning_effort") == "none" for b in posted)
    assert list(posted[0])[-1] == "reasoning_effort"
    doc = json.loads(res.stdout)
    assert doc["qualification"]["host_kind"] == "ollama"
    assert doc["recommendation"]["runtime"] == "ollama"
    keys = list(doc["qualification"])
    assert keys.index("wired") + 1 == keys.index("host_kind") < keys.index("outcomes")
    assert "host: ollama, thinking off (reasoning_effort none)" in res.stderr


def test_openai_endpoint_sends_no_reasoning_effort(box, tmp_path):
    srv = box({"/v1/chat/completions": chat({"role": "assistant", "content": VALID})})
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m")
    assert res.exit_code == 0, res.output
    assert srv.hits["/api/tags"] == 1
    assert all("reasoning_effort" not in json.loads(b) for b in srv.bodies if b)
    assert "via llamafile" in res.output and "Host kind: openai" in res.output
    assert "thinking off" not in res.stderr


def test_ollama_endpoint_text_says_ollama_not_llamafile(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "gemma4")
    assert "via ollama" in res.output and "llamafile" not in res.output
    assert "Host kind: ollama (thinking off: reasoning_effort none)" in res.output


def test_kind_flag_overrides_the_question(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m", "--kind", "openai")
    assert "/api/tags" not in srv.hits
    assert all("reasoning_effort" not in json.loads(b) for b in srv.bodies if b)
    srv2 = box({"/v1/chat/completions": chat({"role": "assistant", "content": VALID})})
    setup(tmp_path, "--endpoint", srv2.url + "/v1", "--model", "m", "--kind", "ollama")
    assert "/api/tags" not in srv2.hits
    assert all(json.loads(b)["reasoning_effort"] == "none" for b in srv2.bodies if b)


def test_kind_anthropic_does_not_apply_to_endpoint(box, tmp_path):
    srv = box({})
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "m", "--kind", "anthropic")
    assert res.exit_code == 2
    assert "--kind 'anthropic' does not apply to --endpoint" in res.output
    assert srv.hits == {}


def test_truncated_probe_is_named_and_advised(box, tmp_path):
    cut = box({"/v1/chat/completions": chat({"role": "assistant", "content": ""}, "length")})
    js = setup(tmp_path, "--endpoint", cut.url + "/v1", "--model", "m", "--json")
    outcomes = json.loads(js.stdout)["qualification"]["outcomes"]
    assert {o["outcome"] for o in outcomes} == {"truncated"}
    assert "spends the limit on thinking" in outcomes[0]["reason"]
    res = setup(tmp_path, "--endpoint", cut.url + "/v1", "--model", "m")
    assert "First failure (truncated)" in res.output
    assert "Every failed probe was truncated" in res.output
    assert "Not promoted" not in res.output and "ALL attempts errored" not in res.output


def test_wire_records_host_kind(box, tmp_path):
    srv = box(
        {"/api/tags": TAGS, "/v1/chat/completions": chat({"role": "assistant", "content": VALID})}
    )
    res = setup(tmp_path, "--endpoint", srv.url + "/v1", "--model", "gemma4", "--wire")
    assert res.exit_code == 0, res.output
    cfg = json.loads((tmp_path / "d" / "local_tier1.json").read_text())
    assert cfg == {"model": "gemma4", "base_url": srv.url + "/v1", "host_kind": "ollama"}
```

Update `test_declined_and_cut_reasons`: the second half now asserts `"First failure (truncated)" in res.output` instead of `"token budget"`. Update `test_endpoint_ignores_claude_on_path`: `srv.hits["/v1/chat/completions"] == 3` still holds; add `assert srv.hits["/api/tags"] == 1`.

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_setup_endpoint_qualify.py`
Expected: the new tests FAIL (no `/api/tags` hit, no `host_kind`, `declined` where `truncated` is expected).

- [ ] **Step 3: Implement the eleven points above in `setup_cmd`**

Keep every existing string that this task does not name. The `--kind` check for `--endpoint` uses `typer.echo(..., err=True)` and `raise typer.Exit(code=2)`, the same shape as the `--model is required` check beside it.

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_setup_endpoint_qualify.py tests/test_cli_setup.py tests/test_cli_setup_remote.py tests/test_local_rung_wiring.py`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_setup_endpoint_qualify.py
git commit -m "tiers setup --endpoint: ask the host its kind, turn thinking off on Ollama, name truncation

On the GPU box Gemma 4 on Ollama failed 0/3: it thought until the token
limit and the run called that 'declined' and recommended llamafile. The
endpoint path now asks GET /api/tags once (or takes --kind), sends
reasoning_effort none to an Ollama host, reports the host kind, labels a
cut reply 'truncated' with the right advice, and says 'via ollama'."
```

### Task 4: Golden cases for the endpoint path (oracle side)

**Files:**
- Modify: `clients/k3_cases.py` (`build_setup_cases`, the `--endpoint` block from `good = [...]` to the `--matcher` cases)
- Regenerate: `clients/fixtures/k3/` (the writer pins the manifest)

**Interfaces:**
- Consumes: the fake model server in `clients/garden_cases.py`: it answers requests in order, a GET by its path and a POST by its body, and records every request (method, path, body) into `expect.requests`; `{"chat": text, "finish": "length"}` makes a cut reply.
- Produces: the fixture the port compares in Tasks 5 and 6 read.

- [ ] **Step 1: Update the existing endpoint cases**

The kind question is now the first request of every `--endpoint` run that reaches the probes. Prepend `nf` (the 404 reply already defined in `remote_cases`; lift it to module level or define `NF = {"http_status": 404, "body": '{"error": "not found"}'}` beside `good`) to the `replies` of: `setup endpoint passes`, `passes wire`, `passes wire json`, `fails`, `low threshold`, `down`, `prefixed model`, `context`, `context json`, `context of the named model`, `context from props`. `setup endpoint claude code` and `setup endpoint without model` make no request and do not change.

- [ ] **Step 2: Add the new cases**

With `tags = {"http_body": {"models": [{"name": "gemma4:latest"}]}}` and `cut = {"chat": "", "finish": "length"}`:

| name | flags beyond `*ep` | replies |
|---|---|---|
| `setup endpoint ollama` | | `[tags, *good, NF]` |
| `setup endpoint ollama json` | `--json` | `[tags, *good, NF]` |
| `setup endpoint ollama wire` | `--wire --data-dir {HOME}/d` | `[tags, *good, NF]` |
| `setup endpoint ollama truncated` | | `[tags, cut, cut, cut, NF]` |
| `setup endpoint ollama truncated json` | `--json` | `[tags, cut, cut, cut, NF]` |
| `setup endpoint openai truncated` | | `[NF, cut, cut, cut]` |
| `setup endpoint truncated and declined` | | `[NF, {"chat": "no"}, cut, {"chat": reply_env("t")}]` |
| `setup endpoint kind ollama` | `--kind ollama` | `[*good, NF]` |
| `setup endpoint kind openai` | `--kind openai` | `[*good]` |
| `setup endpoint kind anthropic` | `--kind anthropic` | none |
| `setup endpoint kind bogus` | `--kind grpc` | none |
| `setup endpoint tags not ollama shape` | | `[{"http_body": {"data": []}}, *good]` |
| `setup endpoint tags not json` | | `[{"http_body": "nope"}, *good]` |
| `setup endpoint root without v1` | `--endpoint http://127.0.0.1:{PORT}` (replace the `ep` endpoint) | `[tags, *good, NF]` |

Every case uses `smi=smi("8192")` and `env=oai`, as the existing ones do. The `--model` for the ollama cases is `gemma4`.

- [ ] **Step 3: Regenerate and read the fixture**

Run: `DAISUGI_K3_SCRATCH=/mnt/tera/.tt/w1/k3 uv run --no-sync python clients/k3_cases.py --only "setup endpoint"`
Then: `grep -c '"reasoning_effort":"none"' clients/fixtures/k3/cases.jsonl`
Expected: a count of at least 15 (five ollama cases times three probes); `grep -c '"outcome": "truncated"' clients/fixtures/k3/cases.jsonl` at least 9. Open the `setup endpoint ollama` line and confirm the first recorded request is `GET /api/tags`, each chat body ends in `"reasoning_effort":"none"`, and stdout holds `via ollama` and `Host kind: ollama (thinking off: reasoning_effort none)`. Confirm `setup endpoint kind anthropic` records no request and exits 2.

- [ ] **Step 4: Run the path-leak guard and the oracle rerun**

Run: `DAISUGI_K3_SCRATCH=/mnt/tera/.tt/w1/k3 uv run --no-sync python clients/k3_compare.py --binary clients/go/daisugi --oracle --only "setup endpoint" 2>&1 | grep -c STALE`
Expected: `0` (the Go binary is still the old one here, so DISAGREE lines are expected and are Task 5's work; this step checks only that the fixture is fresh).

- [ ] **Step 5: Commit**

```bash
git add clients/k3_cases.py clients/fixtures/k3/
git commit -m "k3 cases: tiers setup --endpoint on an Ollama host, truncation, --kind on an endpoint"
```

### Task 5: The Go port

**Files:**
- Modify: `clients/go/internal/envgen/tier1.go` (`Tier1`, `NewTier1` unchanged, `GenerateErr`, `LoadConfiguredTier1`, outcomes, `Classify`)
- Modify: `clients/go/internal/cli/setupremote.go` (add `(h *hostClient) endpointKind(root string) string` beside `probeOllama`)
- Modify: `clients/go/internal/cli/setupcmd.go` (`tiersSetup`: the `--kind` help and check, the question, the provider, the note, `local_tier1.json`, JSON and text output, the hint)
- Test: `clients/go/internal/envgen/envgen_test.go`, `clients/go/internal/cli/setupqualify_test.go`

**Interfaces:**
- Consumes: `llm.Call.ReasoningEffort` and `llm.BodyOpts.ReasoningEffort`, which already write the field last in a chat body. `hostClient.send` already sends httpx's headers (ruling SR-1).
- Produces:
  - `envgen.OutcomeTruncated = "truncated"`, `envgen.OllamaNoThinkEffort = "none"`, `envgen.truncatedAdvice` with the exact text of Task 1's `TRUNCATED_ADVICE`.
  - `Tier1.ReasoningEffort string`; `GenerateErr` sets `Call.ReasoningEffort = t.ReasoningEffort`.
  - `LoadConfiguredTier1`: `host_kind` read with `cfg.Value("host_kind")`; the string `"ollama"` sets `ReasoningEffort = OllamaNoThinkEffort`; anything else (missing, another string, a non-string) leaves it empty.
  - `endpointKind(root)`: `GET root+"/api/tags"` through `h.send`; status 200 and `jsonBody` an object whose `models` is a list means `"ollama"`, else `"openai"`.
  - `tiersSetup` mirrors Task 3's points 1 to 11, with `recommendation.runtime` and the `via %s` line driven by the same `runtime` variable, and the `local_tier1.json` body `pyjson.NewObject().Set("model", model).Set("base_url", endpoint).Set("host_kind", hostKind)`.

- [ ] **Step 1: Write the failing tests**

In `envgen_test.go`:

```go
func TestClassifyNamesTruncation(t *testing.T) {
	outcome, reason := Classify(&llm.Error{Msg: "The output is incomplete due to a max_tokens length limit."})
	if outcome != OutcomeTruncated {
		t.Fatalf("outcome %q", outcome)
	}
	want := "The output is incomplete due to a max_tokens length limit." + truncatedAdvice
	if reason != want || len([]rune(reason)) > 200 {
		t.Fatalf("reason %q", reason)
	}
}

func TestLoadConfiguredTier1ReadsHostKind(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) *Tier1 {
		os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0o644)
		tier, err := LoadConfiguredTier1(dir)
		if err != nil {
			t.Fatal(err)
		}
		return tier
	}
	if got := write(`{"model": "m", "base_url": "http://h/v1", "host_kind": "ollama"}`).ReasoningEffort; got != "none" {
		t.Fatalf("ollama: %q", got)
	}
	for _, body := range []string{
		`{"model": "m", "base_url": "http://h/v1", "host_kind": "openai"}`,
		`{"model": "m", "base_url": "http://h/v1"}`,
		`{"model": "m", "base_url": "http://h/v1", "host_kind": 7}`,
	} {
		if got := write(body).ReasoningEffort; got != "" {
			t.Fatalf("%s: %q", body, got)
		}
	}
}
```

In `setupqualify_test.go`, extend `chatServer` so the handler also answers `/api/tags` when a `tags` body is given, and records each `/v1/chat/completions` request body into a slice:

```go
func TestOllamaEndpointGetsReasoningEffortNone(t *testing.T) {
	var hits atomic.Int32
	var bodies []string
	srv := tagsAndChatServer(t, &hits, `{"models":[{"name":"gemma4:latest"}]}`, &bodies, func(w http.ResponseWriter) {
		chatReply(w, map[string]any{"role": "assistant", "content": validEnvelopeReply}, "stop")
	})
	code, out, errb := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "gemma4", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, b := range bodies {
		if !strings.HasSuffix(b, `,"reasoning_effort":"none"}`) {
			t.Fatalf("body %s", b)
		}
	}
	if !strings.Contains(out, `"host_kind": "ollama"`) || !strings.Contains(out, `"runtime": "ollama"`) {
		t.Fatalf("out %s", out)
	}
	if !strings.Contains(errb, "host: ollama, thinking off (reasoning_effort none)") {
		t.Fatalf("stderr %s", errb)
	}
}

func TestOpenAIEndpointSendsNoReasoningEffort(t *testing.T) {
	// no /api/tags route: the question gets 404
	var hits atomic.Int32
	var bodies []string
	srv := tagsAndChatServer(t, &hits, "", &bodies, func(w http.ResponseWriter) {
		chatReply(w, map[string]any{"role": "assistant", "content": validEnvelopeReply}, "stop")
	})
	_, out, _ := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m")
	for _, b := range bodies {
		if strings.Contains(b, "reasoning_effort") {
			t.Fatalf("body %s", b)
		}
	}
	if !strings.Contains(out, "via llamafile") || !strings.Contains(out, "Host kind: openai\n") {
		t.Fatalf("out %s", out)
	}
}

func TestTruncatedProbeIsNamed(t *testing.T) {
	var hits atomic.Int32
	var bodies []string
	srv := tagsAndChatServer(t, &hits, "", &bodies, func(w http.ResponseWriter) {
		chatReply(w, map[string]any{"role": "assistant", "content": ""}, "length")
	})
	_, out, _ := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m")
	for _, want := range []string{"First failure (truncated)", "Every failed probe was truncated"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "Not promoted") {
		t.Fatalf("out %s", out)
	}
}

func TestKindAnthropicDoesNotApplyToEndpoint(t *testing.T) {
	var hits atomic.Int32
	srv := chatServer(t, &hits, func(w http.ResponseWriter) {})
	code, _, errb := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m", "--kind", "anthropic")
	if code != 2 || hits.Load() != 0 || !strings.Contains(errb, "--kind 'anthropic' does not apply to --endpoint") {
		t.Fatalf("exit %d hits %d: %s", code, hits.Load(), errb)
	}
}
```

`setupEnv(t, nil, flags...)` prepends `tiers setup` and runs in a scratch HOME, as the existing tests in that file do; `--data-dir` is not needed.

- [ ] **Step 2: Run them to verify they fail**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd clients/go && go test -p 1 ./internal/envgen/ ./internal/cli/ -run "Classify|HostKind|Ollama|OpenAIEndpoint|Truncated|KindAnthropic" 2>&1 | tail -20'`
Expected: compile errors (`OutcomeTruncated` undefined) or FAIL.

- [ ] **Step 3: Implement**

Mirror the oracle line for line. The host-kind question runs after the claude-code refusal and `c.Pin("api")`, before `envgen.NewTier1`; set `t.ReasoningEffort = envgen.OllamaNoThinkEffort` when `hostKind == "ollama"`. The `--kind` check for an endpoint runs right after the `--model is required` check. The `truncated` count for the hint: `allTruncated := qual.valid < qual.attempts && every non-valid outcome == OutcomeTruncated`.

- [ ] **Step 4: Run the unit tests, then build**

Run: the Step 2 command. Expected: PASS.
Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh`
Expected: `clients/go/daisugi` rebuilt, `check-paths.sh` clean.

- [ ] **Step 5: Compare**

Run: `DAISUGI_K3_SCRATCH=/mnt/tera/.tt/w1/k3 systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/k3_compare.py --binary clients/go/daisugi --only "setup endpoint"`
Expected: no `DISAGREE:` line; the class summary shows every `setup endpoint` case as `agree`. A disagreement is a bug in this task, not a ruling, unless the oracle is wrong (then fix the oracle in Task 3, regenerate in Task 4, and come back).

- [ ] **Step 6: Commit**

```bash
git add clients/go/internal/envgen/tier1.go clients/go/internal/envgen/envgen_test.go clients/go/internal/cli/setupcmd.go clients/go/internal/cli/setupremote.go clients/go/internal/cli/setupqualify_test.go
git commit -m "go: tiers setup --endpoint asks the host kind, turns thinking off on Ollama, names truncation

Mirrors the oracle: GET /api/tags once (or --kind), reasoning_effort none
on an Ollama host, host_kind in the JSON, the text and local_tier1.json,
'truncated' for a cut reply, 'via ollama' in the advice."
```

### Task 6: The Rust port

**Files:**
- Modify: `clients/rust/src/envgen/tier1.rs` (`Tier1.reasoning_effort: String`, `generate_err`, `load_configured`, `OUTCOME_TRUNCATED`, `TRUNCATED_ADVICE`, `OLLAMA_NO_THINK_EFFORT`, `classify`)
- Modify: `clients/rust/src/cli/setupremote.rs` (`HostClient::endpoint_kind(&mut self, root: &str) -> &'static str`; `Env::endpoint_kind(&self, root: &str) -> &'static str` beside `Env::endpoint_context`)
- Modify: `clients/rust/src/cli/setupcmd.rs` (`tiers_setup`, as Task 5 did for Go)
- Test: inline `#[cfg(test)]` modules in `tier1.rs` and `setupcmd.rs`

**Interfaces:**
- Consumes: `Call.reasoning_effort: &str` (empty means left out) and `wire.rs`'s body writer, both present.
- Produces: the same constants and behaviour as Task 5, in Rust names: `OUTCOME_TRUNCATED`, `OLLAMA_NO_THINK_EFFORT`, `TRUNCATED_ADVICE`, `Tier1::new` leaves `reasoning_effort` empty, `generate_err` sets `call.reasoning_effort = &self.reasoning_effort`, `load_configured` sets it from `cfg.value("host_kind")` being `Value::Str("ollama")`.

- [ ] **Step 1: Write the failing tests**

In `tier1.rs`:

```rust
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_cut_reply_is_truncated() {
        let (o, r) = classify("The output is incomplete due to a max_tokens length limit.");
        assert_eq!(o, OUTCOME_TRUNCATED);
        assert_eq!(r, format!("The output is incomplete due to a max_tokens length limit.{TRUNCATED_ADVICE}"));
        assert!(r.chars().count() <= 200);
    }

    #[test]
    fn load_configured_reads_host_kind() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join(CONFIG_FILE);
        let load = |body: &str| {
            std::fs::write(&p, body).unwrap();
            load_configured(dir.path().to_str().unwrap()).unwrap().unwrap().reasoning_effort
        };
        assert_eq!(load(r#"{"model": "m", "base_url": "http://h/v1", "host_kind": "ollama"}"#), "none");
        assert_eq!(load(r#"{"model": "m", "base_url": "http://h/v1", "host_kind": "openai"}"#), "");
        assert_eq!(load(r#"{"model": "m", "base_url": "http://h/v1"}"#), "");
        assert_eq!(load(r#"{"model": "m", "base_url": "http://h/v1", "host_kind": 7}"#), "");
    }
}
```

If `tempfile` is not a dev-dependency of `clients/rust`, use `std::env::temp_dir()` joined with a unique name under `/mnt/tera/.tt/w1/rs-<pid>` and remove it at the end; do not add a dependency.

In `setupcmd.rs` tests, add one pure test on the hint rule: factor the "all failures truncated" decision into `fn all_failures_truncated(outcomes: &[ProbeOutcome], valid: i64, attempts: i64) -> bool` and test the four cases of Task 1's `test_all_failures_truncated`.

- [ ] **Step 2: Run them to verify they fail**

Run: `. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path clients/rust/Cargo.toml tier1 2>&1 | tail -20`
Expected: compile error (`OUTCOME_TRUNCATED` not found).

- [ ] **Step 3: Implement**

Mirror Task 5. `HostClient::endpoint_kind` is `self.send(&format!("{root}/api/tags"), None)` then the shape test through `json_body`; `Env::endpoint_kind` builds a `HostClient` the way `Env::endpoint_context` does.

- [ ] **Step 4: Run the unit tests, then build the binary**

Run: the Step 2 command. Expected: PASS.
Run: `. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml`
Expected: `clients/rust/target/release/daisugi` rebuilt.

- [ ] **Step 5: Compare**

Run: `DAISUGI_K3_SCRATCH=/mnt/tera/.tt/w1/k3 systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/k3_compare.py --binary clients/rust/target/release/daisugi --only "setup endpoint"`
Expected: no `DISAGREE:` line.

- [ ] **Step 6: Commit**

```bash
git add clients/rust/src/envgen/tier1.rs clients/rust/src/cli/setupcmd.rs clients/rust/src/cli/setupremote.rs
git commit -m "rust: tiers setup --endpoint asks the host kind, turns thinking off on Ollama, names truncation"
```

### Task 7: W1-a docs and the box check note

**Files:**
- Modify: `docs/how-to/add-a-gpu-box.md` (one paragraph under the section that qualifies a model; if the page has no such section, add `## Qualify a model on the box` after the record step)
- Modify: `docs/feature-status.md` (the `tiers setup` row, if one exists; otherwise nothing)
- Modify: `.superpowers/sdd/backlog/progress.md` (one line under a `Box checks` heading; this file is git-ignored and is not committed)

**Interfaces:** none.

- [ ] **Step 1: Write the paragraph**

State the current behaviour once, in STE: `daisugi tiers setup --endpoint http://box:11434/v1 --model gemma4` asks the server once whether it is Ollama (`GET /api/tags`). On an Ollama host it sends `reasoning_effort: none`, so a thinking model answers without thinking. `--kind openai` sends the request as is; `--kind ollama` skips the question. A probe that stops at the token limit is reported as `truncated`, with the advice to turn thinking off at the server or raise its limit. The wired `local_tier1.json` keeps `host_kind`, so later Tier-1 calls send the same field.

- [ ] **Step 2: Note the box check in the ledger**

Append under `Box checks (W1-a)`: "Gemma 4 on the GPU box: `tiers setup --endpoint ... --model gemma4` must pass 3/3 and `local_tier1.json` must hold `host_kind: ollama`. Also run it once with a model that cannot think (llama3.2:3b) to confirm Ollama accepts `reasoning_effort none` for it; if it refuses, `--kind openai` is the documented escape and the ADJUDICATIONS entry is `K3-OL-1`."

- [ ] **Step 3: Run the style checks and commit**

Run: `grep -nE " — |seam|the layer" docs/how-to/add-a-gpu-box.md` Expected: no new hits in the added paragraph.

```bash
git add docs/how-to/add-a-gpu-box.md docs/feature-status.md
git commit -m "docs: tiers setup on an Ollama host: the kind question, thinking off, truncated"
```

---

## Part B: W1-b, record by default

Design, in one place, so each task can refer to it:

- A plain `daisugi install` writes five layers: skill, MCP server, capture hook, pathway instructions, and the gate hook in audit mode (Claude Code and Codex; Hermes and OpenClaw print the gate as an honest gap, as they do today for `--gate`). Audit mode evaluates every call, logs what enforce would deny, and always allows.
- `--enforce` installs the gate hook in enforce mode. It still refuses, before any write, when no envelope is registered (`ENFORCE_NEEDS_POLICY`, exit 1). `--audit` stays as the negation and the default. `--ask` applies with `--enforce`; without it the note reads `Note: --ask only applies with --enforce; no operator ask will be wired.` `--report` applies on every install; its "needs --gate" note goes away.
- `--gate` is gone. A clean break with one signpost, the same device as `--shadow`: the flag stays hidden and is refused in one usage line. `daisugi install --gate` prints to stderr `Usage: daisugi install [OPTIONS]`, `Try 'daisugi install --help' for help.`, a blank line, then `Error: Invalid value for '--gate': the gate hook is part of every install: drop --gate (add --enforce to make it deny)` and exits 2. `daisugi install --no-gate` prints the parser's own `Error: No such option: --no-gate` and exits 2. The old notes `--enforce only applies with --gate` and `--report needs --gate` are gone.
- `--uninstall` is already symmetric: `reverse()` removes the gate hook whether or not it was asked for. Nothing changes in it.
- Status: `daisugi gate status` after a plain install shows `gate: armed · mode: audit (global)`; the Python `status` command's `gate_mode (resolved)` row reads the same hook. No code changes; a golden case pins it.
- `install.install(layers=None)` now means the five. `DEFAULT_LAYERS` gains `Layer.GATE`.
- `ENFORCE_NEEDS_POLICY` becomes: `Enforce needs a policy first. Run: daisugi gate init --workspace DIR for a starter envelope, then this command again. Or run: daisugi install (audit mode records every call and blocks none)`.
- The stale-hook warning in `config.py` (and its Go and Rust copies) ends `Run: daisugi install --uninstall, then daisugi install`.

### Task 8: The oracle: default layers, the `--enforce` flag, the `--gate` signpost

**Files:**
- Modify: `src/opendaisugi/install.py` (module docstring; `DEFAULT_LAYERS`; the comment above it; `_resolve_layers` docstring; `install()` docstring)
- Modify: `src/opendaisugi/cli.py` (`ENFORCE_NEEDS_POLICY` at line 905; `install_cmd` at line 6675: the options, the docstring, the body)
- Modify: `src/opendaisugi/config.py:565` (the stale-mode warning text), `src/opendaisugi/swap.py:75` (`daisugi install --report herdr`)
- Test: `tests/test_install_gate_baseurl.py`, `tests/test_cli_install_help.py`, `tests/test_install.py`

**Interfaces:**
- Produces:
  - `install.DEFAULT_LAYERS = frozenset({Layer.SKILL, Layer.MCP, Layer.CAPTURE, Layer.INSTRUCTIONS, Layer.GATE})`.
  - `cli._gate_flag_moved(value: bool) -> bool`: raises `typer.BadParameter("the gate hook is part of every install: drop --gate (add --enforce to make it deny)")` when `value` is true. Wired as `_gate: bool = typer.Option(False, "--gate", hidden=True, callback=lambda v: _gate_flag_moved(v))`, the same shape as `_shadow`.
  - `install_cmd` option texts: `--enforce/--audit`: `"Gate mode: --enforce denies out-of-envelope calls; --audit (default) records them and allows."`; `--ask/--no-ask`: `"Hand an enforce-mode would-deny to a present operator for a bounded time before letting the deny stand (Claude Code only). Off by default; only meaningful together with --enforce."`; `--report`: drop the final sentence `Only meaningful together with --gate.`
  - `install_cmd` docstring: `"Wire openDaisugi into every detected agent harness.\n\nDetects Claude Code, Codex, Hermes, and OpenClaw and installs five layers: the opendaisugi-checklist skill (discovered on demand), the MCP tool server, a passive capture hook that feeds distillation, pathway-routing instructions, and the gate hook in audit mode, which records every tool call and blocks none. All changes are idempotent, backed up, and reversible with --uninstall.\n\n--enforce makes the gate deny out-of-envelope calls; it needs a registered envelope (daisugi gate init). --gateway points the harness at the local token-saving gateway (Claude Code, OpenClaw and Codex; Hermes is reported as an honest gap)."`
  - Body: `selected_layers = set(DEFAULT_LAYERS)`; `if enforce:` guards the envelope check; `effective_ask = ask and enforce`; `effective_report = report`; the two old notes removed; the `--ask` note reworded as above; the "The gate checks each call against a registered envelope" block prints unconditionally (it was under `Layer.GATE in selected_layers`, which is now always true; drop the `if`).

- [ ] **Step 1: Write the failing tests**

In `tests/test_install_gate_baseurl.py`, replace `test_default_install_writes_no_gate_and_no_base_url` and `test_install_layers_none_defaults_to_current_four` with:

The file's own idiom is `(tmp_path / ".claude").mkdir()`, `install(home=tmp_path, yes=True)`, `_settings(tmp_path)`, `_pre_commands(settings)`, `monkeypatch.setattr(Path, "home", lambda: tmp_path)` for CLI tests, and `_register_default_envelope(tmp_path)`. Use those.

```python
def test_default_install_writes_the_audit_gate_and_no_base_url(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    settings = _settings(tmp_path)
    commands = _pre_commands(settings)
    assert any("opendaisugi.gate" in c and "--mode audit" in c for c in commands)
    assert not any("--mode enforce" in c for c in commands)
    assert "ANTHROPIC_BASE_URL" not in settings.get("env", {})


def test_install_layers_none_defaults_to_the_five():
    assert DEFAULT_LAYERS == {Layer.SKILL, Layer.MCP, Layer.CAPTURE, Layer.INSTRUCTIONS, Layer.GATE}
    assert _resolve_layers(None) == set(DEFAULT_LAYERS)
```

Replace `test_cli_install_enforce_without_gate_is_inert_and_noted` with:

```python
def test_cli_install_enforce_alone_arms_the_gate(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    _register_default_envelope(tmp_path)
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(app, ["install", "--enforce", "--yes", "--runtime", "claude"])
    assert result.exit_code == 0, result.output
    assert "Note: --enforce only applies" not in result.output
    commands = _pre_commands(_settings(tmp_path))
    assert any("--mode enforce" in c and c.endswith("|| exit 2") for c in commands)


def test_cli_install_enforce_with_no_envelope_names_the_plain_install(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(app, ["install", "--enforce", "--yes", "--runtime", "claude"])
    assert result.exit_code == 1
    assert (
        "Or run: daisugi install (audit mode records every call and blocks none)" in result.output
    )
    assert not (tmp_path / ".claude" / "settings.json").exists()


def test_cli_install_old_gate_flag_is_refused_in_one_line(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(app, ["install", "--gate", "--yes"])
    assert result.exit_code == 2
    assert (
        "Error: Invalid value for '--gate': the gate hook is part of every install: "
        "drop --gate (add --enforce to make it deny)"
    ) in _cli_output(result)
    assert not (tmp_path / ".claude" / "settings.json").exists()
    result = runner.invoke(app, ["install", "--no-gate"])
    assert result.exit_code == 2 and "no such option: --no-gate" in _cli_output(result)


def test_cli_install_ask_without_enforce_is_inert_and_noted(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(app, ["install", "--ask", "--yes", "--runtime", "claude"])
    assert (
        "Note: --ask only applies with --enforce; no operator ask will be wired." in result.output
    )
    assert not any("--ask" in c for c in _pre_commands(_settings(tmp_path)))


def test_plain_install_reports_hermes_gate_as_a_gap(tmp_path, monkeypatch):
    (tmp_path / ".hermes").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(app, ["install", "--dry-run", "--runtime", "hermes"])
    assert result.exit_code == 0, result.output
    out = _cli_output(result)
    assert "honest gaps" in out and "[hermes] gate: not wired" in out
```

Update the other CLI tests in the file that pass `--gate` (`test_cli_install_dry_run_gate_gateway_lists_both_steps`, `test_cli_install_gate_yes_writes_hook`, `test_cli_install_enforce_with_no_envelope_refuses_and_writes_nothing`, `test_cli_install_enforce_with_an_envelope_installs`, `test_cli_install_audit_with_no_envelope_still_installs`, `test_cli_install_ask_flag_reaches_the_installed_hook`, `test_cli_install_codex_gate_plans_the_hook_with_the_fail_open_caveat`): drop `--gate` from their argv; their assertions stand. In `tests/test_cli_install_help.py` add:

```python
def test_install_help_has_no_gate_flag_and_names_enforce():
    text = runner.invoke(app, ["install", "--help"]).stdout
    assert "--gate" not in text
    assert "--enforce" in text and "records them and allows" in text
```

and drop `--gate` from `test_install_gate_points_at_the_envelope_it_does_not_write`'s argv. In `tests/test_install.py`, `test_dry_run_reports_all_layers_no_writes` and `test_install_step_carries_layer_and_target`: if they count four layers, make it five and include `Layer.GATE`.

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_install_gate_baseurl.py tests/test_cli_install_help.py tests/test_install.py`
Expected: FAIL (`DEFAULT_LAYERS` has four; `--gate` still accepted; old note text).

- [ ] **Step 3: Implement**

The interfaces above. In `config.py:565` the warning's last sentence becomes `Run: daisugi install --uninstall, then daisugi install`. In `swap.py:75` the hint becomes `` run `daisugi install --report herdr` to apply ``.

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_install_gate_baseurl.py tests/test_cli_install_help.py tests/test_install.py tests/test_install_gate_report.py tests/test_cli_install_report.py tests/test_install_gate_root_absolute.py tests/test_status_decomposition.py`
Expected: PASS. Then: `uv run ruff check . && uv run ruff format --check .` Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/install.py src/opendaisugi/cli.py src/opendaisugi/config.py src/opendaisugi/swap.py tests/test_install_gate_baseurl.py tests/test_cli_install_help.py tests/test_install.py
git commit -m "install: record every call by default; --enforce is the one opt-in to blocking

Owner ruling 2026-10-28. A plain install writes the gate hook in audit
mode beside the other four layers: it evaluates each call, logs what
enforce would deny, and allows. --gate is gone; the old flag is refused in
one line, as --shadow was. --enforce keeps its envelope check."
```

### Task 9: Golden cases for install (oracle side)

**Files:**
- Modify: `clients/cli_cases.py` (the `install --gate` block from line 933, `install plain` at 1618, the `layers` block at 2555)
- Regenerate: `clients/fixtures/cli/`

**Interfaces:**
- Consumes: `mk(name, argv, before=..., **kw)` with `pre=[argv, ...]` for commands run first in the same HOME, `stdin=`, `env=`.

- [ ] **Step 1: Edit the cases**

1. In every install case whose argv holds `"--gate"`, remove that element. Case names stay (they are ids in the fixture).
2. Delete `install enforce no gate`. Add in its place:
   - `mk("install old gate flag", ["install", "--gate", "--yes"], before=claude_dir)`
   - `mk("install no gate flag", ["install", "--no-gate"], before=claude_dir)`
   - `mk("install ask without enforce", ["install", "--ask", "--yes"], before=claude_dir)`
   - `mk("install report without enforce", ["install", "--report", "coppice", "--yes"], before=claude_dir)`
   - `mk("install plain then gate status", ["gate", "status"], before=claude_dir, pre=[["install", "--yes"]])`
   - `mk("install plain hermes only", ["install", "--yes"], before={".hermes": d()})`
3. `layers every harness gate gateway` becomes argv `[*y, "--gateway"]`; rename it `layers every harness gateway`.

- [ ] **Step 2: Regenerate**

Run: `uv run --no-sync python clients/cli_cases.py`
Expected: `clients/fixtures/cli/cases.jsonl` and its manifest rewritten. Check: `grep -c '"--gate"' clients/fixtures/cli/cases.jsonl` prints `1` (the `install old gate flag` case only). Open `install plain`: its tree holds `.claude/settings.json` with a `gate_client --mode audit` command. Open `install plain then gate status`: stdout is `gate: armed · mode: audit (global)` followed by the `no envelopes registered` line and the dialect line.

- [ ] **Step 3: Oracle freshness**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/cli_compare.py --binary clients/go/daisugi --oracle --only install 2>&1 | grep -c STALE`
Expected: `0`. (DISAGREE lines are Task 10's work.)

- [ ] **Step 4: Commit**

```bash
git add clients/cli_cases.py clients/fixtures/cli/
git commit -m "cli cases: a plain install writes the audit gate; --gate is refused; --enforce alone arms it"
```

### Task 10: The Go port of install

**Files:**
- Modify: `clients/go/internal/cli/installcmd.go` (`installOpts`, `EnforceNeedsPolicy`, `install()`)
- Modify: `clients/go/internal/config/gone.go:140`
- Test: golden cases (Task 9); `go vet`

**Interfaces:**
- Produces, in `installOpts`: the `--gate` entry becomes `{names: []string{"--gate"}, hidden: true}` (no `neg`); `--enforce`'s help: `"Gate mode: --enforce denies out-of-envelope calls; --audit records them and allows."`; `--ask`'s help gains nothing; `--report`'s help unchanged. The `cmdHelp` one-liner becomes: `"Wire openDaisugi into every detected agent harness: the skill, the MCP server, the capture hook, the pathway instructions and the gate hook in audit mode; --enforce makes the gate deny, --gateway points the harness at the token-saving gateway, --harness writes the pi or OpenCode extension."`
- In `install()`: after the `--shadow` check, `if p.flag("--gate") { return e.usage("install", &usageError{"Invalid value for '--gate': the gate hook is part of every install: drop --gate (add --enforce to make it deny)"}) }`; `gate := true` (one line with a comment: the gate layer is part of every install); the two notes removed; the `--ask` note reworded; `report` no longer cleared; `EnforceNeedsPolicy` text as the design says; the "The gate checks each call" block printed unconditionally. `install.Opts.Gate` stays a field and is always `true` from here.

- [ ] **Step 1: Implement**

Mirror Task 8. In `gone.go:140` the warning ends `Run: daisugi install --uninstall, then daisugi install`.

- [ ] **Step 2: Vet, build, help check**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd clients/go && go vet ./internal/cli/ ./internal/config/ && go test -p 1 ./internal/install/ ./internal/config/'`
Expected: clean, PASS.
Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh`
Run: `uv run --no-sync python clients/gen_help.py --check` Expected: exit 0 (the install one-liner in `help --all` did not change).

- [ ] **Step 3: Compare**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/cli_compare.py --binary clients/go/daisugi --only install`
Then the same with `--only layers`.
Expected: no `DISAGREE` line in either run. Compare `clients/fixtures/cli` whole afterwards (a full compare is one heavy job): `... cli_compare.py --binary clients/go/daisugi` Expected: the agree count equals the old one plus the new cases, 0 disagreements, the refused set unchanged.

- [ ] **Step 4: Commit**

```bash
git add clients/go/internal/cli/installcmd.go clients/go/internal/config/gone.go
git commit -m "go: install writes the audit gate by default; --gate refused in one line; --enforce alone arms it"
```

### Task 11: The Rust port of install

**Files:**
- Modify: `clients/rust/src/cli/installcmd.rs` (`INSTALL_OPTS`, `ENFORCE_NEEDS_POLICY`, `install()`)
- Modify: `clients/rust/src/cli/config.rs:633`

**Interfaces:** the same texts as Task 10; `Opt::hidden(&["--gate"])`; `layers::Opts { gate: true, ... }`.

- [ ] **Step 1: Implement**

Mirror Task 10.

- [ ] **Step 2: Build and test**

Run: `. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path clients/rust/Cargo.toml layers 2>&1 | tail -5`
Expected: PASS. Then the release build, as in Task 6 Step 4.

- [ ] **Step 3: Compare**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/cli_compare.py --binary clients/rust/target/release/daisugi --only install`, then `--only layers`.
Expected: no `DISAGREE` line. (`status`, `config`, `start` and `journal` are not in Rust; `install plain then gate status` runs `gate status`, which Rust carries.)

- [ ] **Step 4: Commit**

```bash
git add clients/rust/src/cli/installcmd.rs clients/rust/src/cli/config.rs
git commit -m "rust: install writes the audit gate by default; --gate refused in one line; --enforce alone arms it"
```

### Task 12: Every other string that says `install --gate`

**Files:**
- Modify: `scripts/install.sh:50,236,269`, `scripts/release.sh:235,241,273`, `scripts/smoke-tarball.sh:10,44,45`
- Modify: `harness/coppice/internal/tui/facts.go:40`, `harness/coppice-rs/src/tui/facts.rs:32`, `harness/coppice/internal/datahome/datahome.go:13`, `harness/coppice-rs/src/datahome.rs:14`, `harness/coppice/README.md:30`
- Modify: `src/opendaisugi/tui_sessions.py:353`, `src/opendaisugi/config.py:181` (comments)
- Test: `tests/test_install_sh.py`, `tests/test_install_sh_stock.py` (they may pin the printed hints); coppice's tests for `facts.go`

**Interfaces:**
- Produces: `gateHint`/`GATE_HINT` = `"daisugi is off: no agent is guarded. Run: daisugi install"`; `install.sh:269` = `daisugi install          # watch every agent tool call (add --enforce to deny)`; `release.sh:235` = `daisugi install           # audit: records every call, blocks none`, `:241` = `daisugi install --enforce`; `smoke-tarball.sh:44` runs `daisugi install --enforce --runtime claude --yes` and `:45` names `daisugi install --enforce`.

- [ ] **Step 1: Find every hit**

Run: `grep -rn "install --gate" --include='*.sh' --include='*.go' --include='*.rs' --include='*.py' --include='*.toml' --include='*.md' . | grep -v "docs/plans/\|docs/superpowers/\|ADJUDICATIONS\|CHANGELOG\|helpall_gen\|fixtures/\|\.git/\|target/"`
Expected: the files listed above plus the docs Task 13 owns. Fix every non-doc hit here.

- [ ] **Step 2: Run the pinned tests**

Run: `uv run pytest -q tests/test_install_sh.py tests/test_install_sh_stock.py`
Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd harness/coppice && go vet ./internal/tui/ ./internal/datahome/ && go test -race ./internal/tui/ ./internal/datahome/'`
Expected: PASS. (coppice-rs is not built here; its string is edited for parity and read by eye.)

- [ ] **Step 3: Commit**

```bash
git add scripts/install.sh scripts/release.sh scripts/smoke-tarball.sh harness/coppice/internal/tui/facts.go harness/coppice-rs/src/tui/facts.rs harness/coppice/internal/datahome/datahome.go harness/coppice-rs/src/datahome.rs harness/coppice/README.md src/opendaisugi/tui_sessions.py src/opendaisugi/config.py
git commit -m "hints and scripts: the gate hook comes with a plain daisugi install"
```

### Task 13: Docs state the current install once

**Files:**
- Modify: `README.md:67-69`, `docs/start-here.md:19,27-37,45`, `docs/how-to/gate.md` (the `Just want it working` section, §2, §3, line 103), `AGENTS.md` (the `## Install` block, lines 58-72)
- Modify: `docs/recipes/gate-only.md:21`, `docs/recipes/coppice-claude-gate.md:13-18`, `docs/how-to/token-saving-gateway.md:52-65`, `docs/feature-status.md:53`, `docs/integrations.md:23`
- Create: `docs/adr/0025-record-by-default.md`; Modify: `docs/adr/README.md` (index row), `docs/adr/0013-unified-save-and-verify-install.md` (status line: `Accepted. Superseded in part by ADR-0025: the gate hook is part of every install`)

**Interfaces:** none. Site pages are out of scope.

- [ ] **Step 1: Rewrite each place to the current behaviour**

- README.md:67: `` `daisugi install` makes the gate permanent. It writes five things for Claude Code: the gate hook in audit mode, a capture hook, the MCP server, the `opendaisugi-checklist` skill and a note in `~/.claude/CLAUDE.md`. Audit mode records every tool call and blocks none; `daisugi install --enforce` makes the gate deny calls outside your envelope. `daisugi install --uninstall` reverses all five. ``
- start-here.md:19: `cd openDaisugi && scripts/install.sh && daisugi install`. Line 27: `` `daisugi install` asks before it writes. It writes five things for Claude Code: ``. After the "With no envelope" bullet add: `` To make the warden block, run `daisugi install --enforce`. It needs an envelope first. `` (this page already uses the word warden; keep its voice). Line 45: `` `daisugi install --uninstall` reverses all five writes. `` stays.
- gate.md: §2 gains, after the `claude --settings` line, `` The permanent form is `daisugi install`: it writes the audit hook into `~/.claude/settings.json`. `` §3 gains `` The permanent form is `daisugi install --enforce`. It refuses when no envelope is registered. `` Line 103: `` Run `daisugi install --uninstall`, then `daisugi install`. ``
- AGENTS.md Install block:
  ```bash
  daisugi install                      # the skill, the MCP server, the capture hook, the pathway instructions, and the gate hook in audit mode (Claude Code and Codex)
  daisugi install --enforce            # the gate denies; needs a registered envelope: run `daisugi gate init --workspace ~/Work` first
  daisugi install --harness pi         # or: opencode
  daisugi install --gateway            # also point the harness at the gateway
  ```
  and replace the paragraph `The Go daisugi installs only the gate and gateway layers ...` with: `All three ports write the same five layers; the CLI cases prove it. Hermes and OpenClaw get no gate hook yet and the plan says so as an honest gap. Hermes gets no \`install --gateway\` yet (it expects an OpenAI wire that \`install\` does not write).`
- recipes, token-saving-gateway, feature-status, integrations: drop `--gate` from each command; `install --gate [--enforce]` in feature-status becomes `install [--enforce]`; integrations: `installed by \`daisugi install --runtime codex\``.
- ADR-0025, from the template, Accepted, dated 2026-10-30. Context: the owner ruling and the observation that an opt-in hook was never installed on the box that needed the record. Decision: the gate hook is part of every install, in audit mode; `--enforce` is the one opt-in to blocking; `--gate` is removed. Consequences: every install records; Hermes and OpenClaw show a gap on every plain install; enforce keeps fail-closed semantics and its envelope check. Alternatives: keep `--gate` as an alias (rejected: one real user, no aliases), install enforce by default (rejected: with no envelope it denies everything).

- [ ] **Step 2: Style check**

Run: `grep -nE " — |seam|the layer|now |no longer|used to" README.md docs/start-here.md docs/how-to/gate.md AGENTS.md docs/adr/0025-record-by-default.md docs/recipes/gate-only.md docs/recipes/coppice-claude-gate.md | grep -v "^docs/how-to/gate.md.*—"`
Expected: no hit in the lines you wrote (gate.md has older em dashes; leave them).

- [ ] **Step 3: Commit**

```bash
git add README.md docs/start-here.md docs/how-to/gate.md AGENTS.md docs/recipes/gate-only.md docs/recipes/coppice-claude-gate.md docs/how-to/token-saving-gateway.md docs/feature-status.md docs/integrations.md docs/adr/0025-record-by-default.md docs/adr/README.md docs/adr/0013-unified-save-and-verify-install.md
git commit -m "docs: a plain daisugi install records every call; --enforce blocks (ADR-0025)"
```

### Task 14: Whole-suite green and the ledger

**Files:**
- Modify: `.superpowers/sdd/backlog/progress.md` (not committed)

- [ ] **Step 1: The full Python suite, one heavy job**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run pytest -q -x -p no:cacheprovider`
Expected: green. A test that still passes `--gate` to `install` or expects `declined` for a cut reply is updated in this step and folded into the task's commit by `git commit --amend` only if that commit is the last one; otherwise a small fix commit named `tests: ...`.

- [ ] **Step 2: Ruff and the help generator**

Run: `uv run ruff check . && uv run ruff format --check . && uv run --no-sync python clients/gen_help.py --check`
Expected: clean.

- [ ] **Step 3: Ledger**

Append to `.superpowers/sdd/backlog/progress.md`: the commit range, the compare counts (k3 `setup endpoint`: agree N / Go, Rust; cli `install` and `layers`: agree N / Go, Rust), the two box checks (Task 7), and this line: `W1 done; the live install in ~/.local/bin is a copied binary, so the owner re-runs scripts/install.sh --replace-daisugi and then daisugi install once.`

---

## Self-review notes

- Spec coverage: W1-a (1) send `reasoning_effort none` decided by host kind with `--kind` as the override: Tasks 2, 3, 5, 6. (2) truncated, never declined: Tasks 1, 3, 5, 6. (3) no "via llamafile" on an Ollama host: Tasks 3, 5, 6. Every port, and the k3 compare covers it: Tasks 4 to 6. Fake HTTP only: every test. W1-b: plain install records: Task 8; `--enforce`: Task 8; `--gate` clean break with the exact text: Task 8, cases in Task 9; uninstall symmetry: stated in Part B, no change; status output: pinned by `install plain then gate status`; fail-closed under enforce: the envelope check and `|| exit 2` are untouched and asserted in Task 8; docs state the current behaviour once: Task 13; ports: Tasks 10, 11.
- Type consistency: `host_kind` (Python), `hostKind` (Go), `host_kind` (Rust); the JSON key is `host_kind` everywhere; `reasoning_effort` is the Python keyword and the Rust field, `ReasoningEffort` the Go field.
- Review Focus items 1 to 5 each have a test or a box check in the task named beside them.
