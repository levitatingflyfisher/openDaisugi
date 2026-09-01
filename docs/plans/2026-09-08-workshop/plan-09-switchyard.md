# NeMo Switchyard Upstream Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `daisugi gateway --router switchyard` starts NVIDIA NeMo Switchyard's standalone
proxy as a managed child, forwards Claude Code's turns to it unchanged, and journals which
target Switchyard actually served — honestly, never inventing a saving it can't attribute —
so `daisugi router status` and the gateway report can show a real per-target breakdown.

**Architecture:** A new `router_switchyard.py` owns everything Switchyard-specific: locating
the `switchyard-server` binary, rendering its native TOML deployment file, and starting/
stopping it as a managed child (mirroring `opendaisugi.start`'s detached-spawn-then-poll
pattern, swapped for an HTTP health check). `Gateway` (gateway_pipeline.py) grows a third
`router_mode` (`rules` | `external` | `off`) alongside the existing rules router; in
`external` mode it never rewrites the model locally — it sends Switchyard's configured route
id and lets Switchyard choose. The ASGI layer (gateway_asgi.py) reads the served model back
off the response and, in `_record`, replaces the routing decision with one that compares the
*actual* served target against Switchyard's own configured targets (never against the
harness's literal request string) before handing it to the existing meter/journal — so
`downgraded` and every dollar/token figure it drives stay honest.

**Tech Stack:** Python 3.12 stdlib (`tomllib`, `urllib.request`, `subprocess`, `signal`);
`httpx`/`uvicorn` (already the `[gateway]` extra — no new dependency); the external
`switchyard-server` Rust binary, installed separately via `cargo install --locked
switchyard-server` (confirmed in Task 1 — `pip install nemo-switchyard` is a *different*
thing, the embeddable Python routing library, not the server; this plan adds no new Python
dependency to pyproject.toml).

**Spec:** `docs/plans/2026-09-08-workshop/spec-09-switchyard.md` (master spec §5.7, §3, §4).

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**All 8 tasks are ready to build now** — no fail-open, data-loss, or cross-plan contract-drift
defect was found; every fact Task 1 pins was checked against the live NVIDIA-NeMo/Switchyard
repo (README.md, docs/cli_reference.md, docs/reference/toml_schema.md,
crates/switchyard-server/README.md, and crates/protocol/src/metadata.rs's header constants)
and matches: `schema_version = 1`, the three `[llm_clients]`/`[targets]`/`[routes]` tables and
their exact keys, `stage_router`'s required `capable_target`/`efficient_target`/`picker`/
`confidence_threshold`, `forward_auth` XOR `api_key_env` with documented OAuth-forwarding
support, `/health`, `--dry-run`, `--routing-log-file`'s recorded fields, the `/v1/messages`
Anthropic Messages endpoint, and — checked by grepping every `x-switchyard-*` header constant
in the protocol crate, not just the docs — the absence of any response header naming the
served target. Fix the four items below; none blocks starting Task 1.

- **SHOULD-FIX — Task 2's renderer only gives the *capable* client the `forward_auth`
  fallback (`render_switchyard_toml`, plan-09-switchyard.md:527-533 vs. :535-539).** The
  capable branch checks `api_key_present(targets.capable_api_key_env)` before choosing
  `api_key_env` vs. `forward_auth = true` (line 529); the efficient branch only checks
  `if targets.efficient_api_key_env:` (line 537) — truthiness of the *configured name*, never
  whether that env var is actually set. `targets_from_config`'s own
  `test_targets_from_config_builds_a_cloud_efficient_target` (Task 3) sets
  `efficient_api_key_env = "ANTHROPIC_API_KEY"` whenever the efficient tier is a cloud model —
  exactly the case the how-to (line ~2500) documents as supported ("a cheap cloud model"). If
  `ANTHROPIC_API_KEY` is unset in that configuration, the rendered TOML emits
  `api_key_env = "ANTHROPIC_API_KEY"` with no `forward_auth` escape hatch, and
  `docs/reference/toml_schema.md`'s own `api_key_env` row says the named variable "must exist
  and be non-empty when the server loads" — so `switchyard-server` fails to start. No test in
  Task 2 or 3 exercises this combination. Fix: apply the same `api_key_present`-gated
  `forward_auth` fallback to the efficient client, and add a test for "efficient tier is a
  cloud model, no key configured."
- **SHOULD-FIX — `daisugi router status` always health-checks the compiled-in
  `DEFAULT_HOST`/`DEFAULT_PORT` (plan-09-switchyard.md:2192: `probe_health(DEFAULT_HOST,
  DEFAULT_PORT)`), never the port the child was actually started on.** `daisugi gateway
  --router switchyard` accepts `--switchyard-port` (line 2243) and passes it to
  `start_switchyard` (line 2292), but `router_status_cmd` has no matching option and no way to
  read back what port was used. After any non-default `--switchyard-port`, `daisugi router
  status` reports the server unreachable even when it is healthy. Fix: persist the actual
  host/port next to the pid file at start time (or in config) and have `router_status_cmd`
  read that instead of the hardcoded defaults.
- **SHOULD-FIX — the substitution of spec-09's `route_target` field is honest but
  undocumented.** spec-09-switchyard.md:24, :78, :87 specify a new persisted `route_target`
  field added to `src/opendaisugi/gateway.py`. Tasks 6-7 instead fold the served target into
  the *existing* `GatewayTurnRecord.model` field plus a new `tier="tier-switchyard"` marker
  (plan-09-switchyard.md:1600-1656, :1903-1929) and never touch `gateway.py` (confirmed by
  reading `src/opendaisugi/gateway.py`: `RouteDecision`, `route_turn`, `measure_turn`, and
  `_PRICES_PER_MTOK` already live there, unchanged by this plan). Tracing `_record`'s
  external-mode branch through `measure_turn` (`src/opendaisugi/gateway.py:398-432`) and
  `build_target_share_table` (Task 7) confirms the substitution **can still attribute savings
  honestly**: `.model` after `_record` always holds the true served target, or a
  known-non-downgraded fallback when the response can't be parsed — the same
  undersell-never-oversell property spec-09 asked of `route_target` — and the share table's
  `{model, turns, share, input_tokens, output_tokens}` columns satisfy spec-09's "target,
  turns, share, input tokens, output tokens" table one-for-one. This is a legitimate design
  choice, not a defect. But unlike this plan's *other* two corrections to spec-09
  (`forward_auth`, the nonexistent `escalation` route type — both called out explicitly in
  "Notes for whoever executes this plan"), this one is silent. Fix: add a fourth bullet to
  that Notes section naming the substitution and the honesty argument above, and update
  spec-09's Files list and "The gateway change" section to match what actually gets built.
- **SHOULD-FIX — `docs/how-to/router-switchyard.md` (Task 8, plan-09-switchyard.md:2463-2553)
  violates the STE100 constraint this very plan copies into its own Global Constraints below**
  ("no em-dashes, no parentheticals"). It is the plan's only wholly new user-facing prose
  (the CLI help strings mostly extend `modules.py`'s pre-existing dash-heavy convention, a
  separate, pre-existing gap this plan did not introduce) and uses em-dashes and parentheticals
  throughout — e.g. the header at line 2504 (`## What "downgraded" means here — and its one
  honest limitation`) and line 2544 (`(Ctrl+C), sending it \`SIGTERM\` — Switchyard drains
  active requests...`). Fix: rewrite the how-to in STE100 register before Task 8 ships it.

- **NOTE — `DEFAULT_HOST = "127.0.0.1"` (plan-09-switchyard.md:182) is a deliberate,
  safer choice, not an upstream fact.** `docs/cli_reference.md` lists `switchyard-server`'s own
  `--host` default as `0.0.0.0`. The module docstring's "pinned facts" block never claims
  `DEFAULT_HOST`/`DEFAULT_PORT` came from the docs, so this isn't a factual error, but a reader
  skimming that block could mistake it for one — worth one line in the docstring saying the
  host default is daisugi's own (bind the managed child to loopback only), not Switchyard's.
- **NOTE — the flagged "open question" (does the response `model` field name the target or
  echo the route id) may already be resolvable, though this is outside this review's
  README/docs scope.** `crates/switchyard-translation/src/codecs/anthropic/buffered.rs`
  (source, not docs) shows `decode_response` reading `model` straight off the *upstream's own*
  raw response before `encode_response` re-emits that same value to the client — suggesting
  the field does name the real served target, not the route id. Worth a source spike before
  assuming the `--routing-log-file` fallback described in the how-to is actually needed.
- **NOTE — Task 1's demotion of spec-09's "or `escalation` per --strategy" to "there is no
  escalation route type" is correct** (`docs/reference/toml_schema.md` confirms escalation is
  `type = "llm_classifier"` + `mode = "escalation"`) and evidenced in Task 1's own module
  docstring, just not surfaced in the top-level "Notes for whoever executes this plan" list
  alongside the plan's other two spec-09 corrections. No action required beyond awareness.

## Global Constraints

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

This plan's own scope is Python-only (`src/opendaisugi/`, `tests/`, `docs/how-to/`); the Go/
coppice constraints above bind other sub-projects, not this one, and are copied here only
because they are Global (§4) and every plan carries them verbatim.

## Notes for whoever executes this plan

- **This plan depends on spec-08 (`daisugi setup --remote`) landing first for one field only:**
  `Config.llm_base_url: str | None`. Task 3's `targets_from_config` reads it defensively via
  `getattr(cfg, "llm_base_url", None)` so this plan still runs standalone if plan-08 has not
  landed yet — it just falls back to an Ollama-shaped default (`http://127.0.0.1:11434/v1`).
- **The real switchyard-server binary is never required to make the test suite pass.** Task 1
  writes `check_prerequisite()`; every task after it fakes the binary (injected `spawn`/
  `health_probe`/`which` callables, or a real local `http.server` standing in for `/health`).
  The one test that needs a live binary + a real provider key (`test_response_model_names_the_
  served_target`) is skip-gated behind `OPENDAISUGI_TEST_SWITCHYARD` and documented as manual.
- **A genuinely open question this plan cannot resolve from documentation alone:** whether
  Switchyard's routed response echoes the *target's* model id or the *route's* id in its
  `model` field. Task 1 records this as an explicit unknown; Task 6's honesty logic is
  self-limiting either way — if the field turns out to echo the route id, every external-mode
  turn safely books zero saving (undersells) rather than a wrong one (never fabricates one).
- **Codex / the OpenAI wire is out of scope for `router_mode`/`external`.** Only the Anthropic
  Messages `Gateway` gets the new modes; `serve_gateway`'s `openai_gateway` keeps its existing
  unconditional rules-router behavior. Routing Codex traffic through Switchyard is a follow-on.
- **Correction to spec-09's own text, found in Task 1's discovery:** spec-09 says "with no API
  key, Switchyard can only route between local hosts; frontier turns still go through your
  Claude login" — implying a missing `ANTHROPIC_API_KEY` blocks the capable/frontier target.
  The pinned schema doc says otherwise: an `llm_client` can set `forward_auth = true` instead
  of `api_key_env`, and an Anthropic-format forwarding client sends the *caller's own*
  `authorization` (including `oauth-*` values, i.e. a Claude subscription login) upstream.
  Task 2's renderer uses `forward_auth = true` whenever no API key is set, so the capable
  target keeps working on a subscription login with no key configured — the opposite of
  spec-09's claim. This is unverified end-to-end against a live binary (see the how-to);
  it is what the schema documents, not something this plan has run and watched succeed.
- **The double-translation "wire-format conformance" test spec-09 describes does not exist in
  this plan, on purpose.** Switchyard translates between OpenAI/Anthropic/Responses wire
  formats *inside* `switchyard-server`, invisible to the daisugi gateway — the gateway's own
  `/v1/messages` request goes to Switchyard's `/v1/messages` route and gets back a proper
  Anthropic Messages response no matter which provider format the chosen target actually
  speaks. There is nothing for daisugi's own code to translate, so a test simulating a
  translation hop inside `tests/test_gateway_upstream.py` would mock a responsibility this
  codebase doesn't have. Task 6's real regression risk — does the new decision-replacement
  logic in `_record` disturb byte-for-byte passthrough of a response carrying a `tool_use`
  block and streaming deltas — is what its wire test actually proves instead.

---

### Task 1: Discovery — pin the Switchyard facts, the prerequisite check, the fixture

**Files:**
- Create: `src/opendaisugi/router_switchyard.py`
- Create: `tests/fixtures/switchyard/routes.example.toml`
- Create: `tests/test_router_switchyard.py`

**Interfaces:**
- Produces: `BINARY_NAME: str`, `INSTALL_CMD: str`, `HEALTH_PATH: str`, `DEFAULT_HOST: str`,
  `DEFAULT_PORT: int`, `locate_binary(*, which: Callable[[str], str | None] = shutil.which) ->
  str | None`, `check_prerequisite(*, which=shutil.which) -> str | None`,
  `binary_version(*, run: Callable[..., subprocess.CompletedProcess] = subprocess.run) -> str
  | None`.

Fetched 2026-09-08 from `github.com/NVIDIA-NeMo/Switchyard` @ `main` (README.md,
`docs/reference/toml_schema.md`, `docs/cli_reference.md`, `crates/switchyard-server/
README.md`) and pinned as facts every later task reads instead of re-deriving:

- The standalone proxy is the Rust binary `switchyard-server`, installed with `cargo install
  --locked switchyard-server` (needs a Rust toolchain). `pip install nemo-switchyard`
  installs a *different* thing — the embeddable Python library (`switchyard.libsy`), not the
  server. This plan needs neither package; pyproject.toml gets no new dependency.
- Liveness: `GET /health`. Version: `switchyard-server --version`. A config validates without
  binding a socket via `switchyard-server --config PATH --dry-run`.
- Config is native TOML, `schema_version = 1`, three tables: `[llm_clients.<name>]` (`format`
  = `openai_chat` | `openai_responses` | `anthropic_messages`; `base_url`; `api_key_env` XOR
  `forward_auth = true`), `[targets.<name>]` (`id` = the exact upstream model id;
  `llm_client`), `[routes.<name>]` (`id` = the model string *clients must send* to select
  this route — never a real model name; `type`). Table names under all three are arbitrary
  local references.
- The `stage_router` route type's keys are `capable_target`, `efficient_target`, `picker`
  (`efficient_first` | `capable_first`), `confidence_threshold`. There is no `escalation`
  route *type* — an escalation policy is `type = "llm_classifier"` with `mode =
  "escalation"`, `strong_target`, `weak_target`. This plan only ever renders `stage_router`.
- **No response header names the chosen target.** Checked every fetched doc (README, server
  README, CLI reference, TOML schema) for something like `x-switchyard-target`: none exists.
  `route_target` must come from the response body's own `model` field.
- **Open question, not resolved by documentation:** whether that `model` field carries the
  target's real id or echoes the route's id back. If it echoes the route id, `router
  status`/the share table will show zero saving for every external-mode turn — safe
  (undersells) but not useful; the documented, not-yet-wired fallback is `--routing-log-file`
  (a JSONL file the server can be told to append one record to per completed turn, naming
  `route_id`, `algorithm`, served `model`, `tier`, `usage`).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_router_switchyard.py
"""Task 1: discovery facts pinned from the Switchyard docs, plus the prerequisite check.

Nothing here needs a real switchyard-server binary — `locate_binary`/`check_prerequisite`
take an injectable `which`, so every test fakes PATH lookup rather than depending on the
host having Rust/cargo installed.
"""

from __future__ import annotations

import tomllib
from pathlib import Path

from opendaisugi.router_switchyard import (
    BINARY_NAME,
    DEFAULT_HOST,
    DEFAULT_PORT,
    HEALTH_PATH,
    INSTALL_CMD,
    binary_version,
    check_prerequisite,
    locate_binary,
)

FIXTURE = Path(__file__).parent / "fixtures" / "switchyard" / "routes.example.toml"


def test_binary_and_install_facts_are_pinned():
    assert BINARY_NAME == "switchyard-server"
    assert INSTALL_CMD == "cargo install --locked switchyard-server"
    assert HEALTH_PATH == "/health"
    assert DEFAULT_HOST == "127.0.0.1"
    assert DEFAULT_PORT == 4000


def test_fixture_is_valid_toml_with_the_real_schema_v1_keys():
    doc = tomllib.loads(FIXTURE.read_text())
    assert doc["schema_version"] == 1
    assert set(doc["llm_clients"]["capable"]) == {"format", "base_url", "forward_auth"}
    assert set(doc["llm_clients"]["efficient"]) == {"format", "base_url"}
    assert set(doc["targets"]["capable"]) == {"id", "llm_client"}
    assert set(doc["routes"]["daisugi"]) == {
        "id",
        "type",
        "capable_target",
        "efficient_target",
        "picker",
        "confidence_threshold",
    }
    assert doc["routes"]["daisugi"]["type"] == "stage_router"


def test_locate_binary_uses_the_injected_which():
    assert locate_binary(which=lambda name: None) is None
    assert (
        locate_binary(which=lambda name: "/usr/local/bin/switchyard-server")
        == "/usr/local/bin/switchyard-server"
    )


def test_check_prerequisite_teaches_the_cargo_install_when_absent():
    msg = check_prerequisite(which=lambda name: None)
    assert msg is not None
    assert "cargo install --locked switchyard-server" in msg
    assert "rust" in msg.lower()


def test_check_prerequisite_is_none_when_the_binary_is_present():
    assert check_prerequisite(which=lambda name: "/usr/bin/switchyard-server") is None


def test_binary_version_is_none_without_the_binary(monkeypatch):
    monkeypatch.setattr("opendaisugi.router_switchyard.locate_binary", lambda **_: None)
    assert binary_version() is None


def test_binary_version_reads_the_dash_dash_version_output(monkeypatch):
    monkeypatch.setattr(
        "opendaisugi.router_switchyard.locate_binary",
        lambda **_: "/usr/local/bin/switchyard-server",
    )

    class _Result:
        stdout = "switchyard-server 0.2.0\n"

    monkeypatch.setattr("opendaisugi.router_switchyard.subprocess.run", lambda *a, **k: _Result())
    assert binary_version() == "switchyard-server 0.2.0"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_router_switchyard.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi.router_switchyard'`
(and the fixture file does not exist yet).

- [ ] **Step 3: Write the fixture**

```toml
# tests/fixtures/switchyard/routes.example.toml
schema_version = 1

[llm_clients.capable]
format = "anthropic_messages"
base_url = "https://api.anthropic.com"
forward_auth = true

[llm_clients.efficient]
format = "openai_chat"
base_url = "http://127.0.0.1:11434/v1"

[targets.capable]
id = "claude-sonnet-5"
llm_client = "capable"

[targets.efficient]
id = "qwen3-coder:30b"
llm_client = "efficient"

[routes.daisugi]
id = "daisugi"
type = "stage_router"
capable_target = "capable"
efficient_target = "efficient"
picker = "efficient_first"
confidence_threshold = 0.5
```

- [ ] **Step 4: Write the implementation**

```python
# src/opendaisugi/router_switchyard.py
"""Manage NVIDIA NeMo Switchyard as the gateway's external model chooser (spec-09).

Switchyard picks which model serves each turn; the gateway stays the meter (master spec
§5.7). This module owns everything Switchyard-specific: locating the `switchyard-server`
binary, rendering its native TOML deployment file, starting/stopping it as a managed child
(mirroring `opendaisugi.start`'s detached-spawn-then-poll pattern), and probing its health.

Pinned facts (fetched 2026-09-08 from github.com/NVIDIA-NeMo/Switchyard @ main — README.md,
docs/reference/toml_schema.md, docs/cli_reference.md, crates/switchyard-server/README.md; see
tests/fixtures/switchyard/routes.example.toml for a checked-in, schema-valid example asserted
against this module's renderer in a later task):

- The standalone proxy is the Rust binary `switchyard-server`, installed with `cargo install
  --locked switchyard-server` (needs a Rust toolchain). `pip install nemo-switchyard`
  installs a DIFFERENT thing — the embeddable Python routing library (`switchyard.libsy`),
  not the server. Nothing here needs that package; pyproject.toml gets no new dependency.
- Liveness: `GET /health`. Version: `switchyard-server --version`. Validate a config without
  binding a socket: `switchyard-server --config PATH --dry-run`.
- Config file: native TOML, `schema_version = 1`, three tables — `[llm_clients.*]` (`format`,
  `base_url`, `api_key_env` XOR `forward_auth`), `[targets.*]` (`id`, `llm_client`),
  `[routes.*]` (`id`, `type`, plus per-type keys). Clients send the route's `id` as the
  request's `model` — NOT a real model name.
- `stage_router` route type keys: `capable_target`, `efficient_target`, `picker`
  (`efficient_first` | `capable_first`), `confidence_threshold`. There is no `escalation`
  route TYPE — an escalation policy is `type = "llm_classifier"` with `mode = "escalation"`.
- KNOWN UNKNOWN, not verified against a live binary: whether a routed response's `model`
  field names the TARGET actually served or echoes the ROUTE id back. No header names the
  chosen target (no `x-switchyard-target` exists in any fetched doc). If it turns out to echo
  the route id, every external-mode turn will book zero saving (`route_target` never matches
  a known target id) — safe (undersells) rather than wrong (never fabricates a saving), but
  not useful; the documented escape hatch is `--routing-log-file` (a JSONL file naming
  `route_id`, `algorithm`, served `model`, `tier`, `usage` per completed turn), wired into
  the launch argv in a later task but not parsed by this version. See
  docs/how-to/router-switchyard.md.
"""

from __future__ import annotations

import shutil
import subprocess
from collections.abc import Callable

BINARY_NAME = "switchyard-server"
INSTALL_CMD = "cargo install --locked switchyard-server"
HEALTH_PATH = "/health"
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 4000


def locate_binary(*, which: Callable[[str], str | None] = shutil.which) -> str | None:
    """Absolute path to `switchyard-server` on PATH, or None."""
    return which(BINARY_NAME)


def check_prerequisite(*, which: Callable[[str], str | None] = shutil.which) -> str | None:
    """None when the binary is present; else a teaching message naming the install command."""
    if locate_binary(which=which):
        return None
    return (
        f"`{BINARY_NAME}` was not found on PATH. Switchyard's standalone proxy is a Rust "
        f"binary, not a pip package. Install it with `{INSTALL_CMD}` (needs a Rust "
        f"toolchain: see https://rust-lang.org/tools/install), then run this again."
    )


def binary_version(
    *, run: Callable[..., subprocess.CompletedProcess] = subprocess.run
) -> str | None:
    """`switchyard-server --version` output, or None if the binary can't be found or run."""
    binary = locate_binary()
    if binary is None:
        return None
    try:
        result = run([binary, "--version"], capture_output=True, text=True, timeout=5.0)
    except (OSError, subprocess.SubprocessError):
        return None
    return result.stdout.strip() or None
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_router_switchyard.py -q`
Expected: PASS (7 tests)

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/router_switchyard.py tests/test_router_switchyard.py \
  tests/fixtures/switchyard/routes.example.toml
git commit -m "gateway: pin the Switchyard binary/config facts and the prerequisite check

Discovery task for spec-09: records what the upstream docs actually say (binary
name, install command, health endpoint, real TOML field names, and the fact
that no response header names the chosen target) so later tasks read a fixture
instead of guessing field names from memory."
```

---

### Task 2: Render the Switchyard TOML

**Files:**
- Modify: `src/opendaisugi/router_switchyard.py`
- Modify: `tests/test_router_switchyard.py`

**Interfaces:**
- Consumes: `tests/fixtures/switchyard/routes.example.toml` (Task 1).
- Produces: `SwitchyardTargets` (frozen dataclass: `capable_id: str`, `efficient_id: str`,
  `efficient_client_format: str`, `efficient_base_url: str`, `capable_client_format: str =
  "anthropic_messages"`, `capable_base_url: str = "https://api.anthropic.com"`,
  `capable_api_key_env: str | None = "ANTHROPIC_API_KEY"`, `efficient_api_key_env: str |
  None = None`), `render_switchyard_toml(targets: SwitchyardTargets, *, route_id: str =
  "daisugi", picker: str = "efficient_first", confidence_threshold: float = 0.5,
  api_key_present: Callable[[str], bool] = ...) -> str`, `write_switchyard_config(data_dir:
  Path, toml_text: str) -> Path`.

- [ ] **Step 1: Write the failing tests**

```python
# append to tests/test_router_switchyard.py
import os

from opendaisugi.router_switchyard import (
    SwitchyardTargets,
    render_switchyard_toml,
    write_switchyard_config,
)


def _targets(**overrides) -> SwitchyardTargets:
    base = dict(
        capable_id="claude-sonnet-5",
        efficient_id="qwen3-coder:30b",
        efficient_client_format="openai_chat",
        efficient_base_url="http://127.0.0.1:11434/v1",
    )
    base.update(overrides)
    return SwitchyardTargets(**base)


def test_render_matches_the_pinned_fixtures_key_shape():
    fixture = tomllib.loads(FIXTURE.read_text())
    rendered = tomllib.loads(render_switchyard_toml(_targets(), api_key_present=lambda name: False))
    assert set(rendered["routes"]["daisugi"]) == set(fixture["routes"]["daisugi"])
    assert set(rendered["targets"]["capable"]) == set(fixture["targets"]["capable"])
    assert set(rendered["llm_clients"]["efficient"]) == set(fixture["llm_clients"]["efficient"])


def test_render_uses_api_key_env_when_the_variable_is_set():
    toml_text = render_switchyard_toml(_targets(), api_key_present=lambda name: True)
    parsed = tomllib.loads(toml_text)
    assert parsed["llm_clients"]["capable"]["api_key_env"] == "ANTHROPIC_API_KEY"
    assert "forward_auth" not in parsed["llm_clients"]["capable"]


def test_render_falls_back_to_forward_auth_without_an_api_key():
    toml_text = render_switchyard_toml(_targets(), api_key_present=lambda name: False)
    parsed = tomllib.loads(toml_text)
    assert parsed["llm_clients"]["capable"]["forward_auth"] is True
    assert "api_key_env" not in parsed["llm_clients"]["capable"]


def test_render_stage_router_fields_come_from_the_targets():
    toml_text = render_switchyard_toml(
        _targets(capable_id="claude-opus-4-8", efficient_id="claude-haiku-4-5"),
        route_id="myroute",
        picker="capable_first",
        confidence_threshold=0.7,
    )
    parsed = tomllib.loads(toml_text)
    route = parsed["routes"]["myroute"]
    assert route["id"] == "myroute"
    assert route["type"] == "stage_router"
    assert route["picker"] == "capable_first"
    assert route["confidence_threshold"] == 0.7
    assert parsed["targets"]["capable"]["id"] == "claude-opus-4-8"
    assert parsed["targets"]["efficient"]["id"] == "claude-haiku-4-5"


def test_write_switchyard_config_writes_mode_0600(tmp_path):
    path = write_switchyard_config(tmp_path, "schema_version = 1\n")
    assert path == tmp_path / "switchyard.toml"
    assert path.read_text() == "schema_version = 1\n"
    assert oct(path.stat().st_mode & 0o777) == "0o600"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_router_switchyard.py -q`
Expected: FAIL — `ImportError: cannot import name 'SwitchyardTargets'`

- [ ] **Step 3: Write the implementation**

```python
# add to src/opendaisugi/router_switchyard.py, after the Task 1 functions
import os
from dataclasses import dataclass, field
from pathlib import Path


@dataclass(frozen=True)
class SwitchyardTargets:
    """The two tiers the gateway wants Switchyard's stage_router to choose between.

    ``capable_id`` should equal the literal string the harness itself sends as ``model``
    against a direct-format client (e.g. the real Anthropic model id) — that equality is
    what later lets the gateway tell "Switchyard served capable" from "Switchyard served
    efficient" apart cleanly, by comparing the response against THESE two strings rather
    than against the harness's own request (see gateway_pipeline.ExternalRouterConfig).
    """

    capable_id: str
    efficient_id: str
    efficient_client_format: str  # "openai_chat" | "anthropic_messages"
    efficient_base_url: str
    capable_client_format: str = "anthropic_messages"
    capable_base_url: str = "https://api.anthropic.com"
    capable_api_key_env: str | None = "ANTHROPIC_API_KEY"  # None -> forward_auth
    efficient_api_key_env: str | None = None


def render_switchyard_toml(
    targets: SwitchyardTargets,
    *,
    route_id: str = "daisugi",
    picker: str = "efficient_first",
    confidence_threshold: float = 0.5,
    api_key_present: Callable[[str], bool] = lambda name: bool(os.environ.get(name)),
) -> str:
    """Render a schema_version=1 Switchyard deployment TOML for a two-tier stage_router.

    The capable client uses `api_key_env` when that variable is actually set in the
    environment, else `forward_auth = true` (the schema forbids setting both) — so a
    subscription/OAuth Claude Code login still reaches the frontier target with no API
    key configured (per the schema's documented `forward_auth` behavior; see the how-to
    for what is and is not verified end-to-end on this box).
    """
    lines: list[str] = ["schema_version = 1", ""]

    lines += ["[llm_clients.capable]", f'format = "{targets.capable_client_format}"']
    lines += [f'base_url = "{targets.capable_base_url}"']
    if targets.capable_api_key_env and api_key_present(targets.capable_api_key_env):
        lines += [f'api_key_env = "{targets.capable_api_key_env}"']
    else:
        lines += ["forward_auth = true"]
    lines += [""]

    lines += ["[llm_clients.efficient]", f'format = "{targets.efficient_client_format}"']
    lines += [f'base_url = "{targets.efficient_base_url}"']
    if targets.efficient_api_key_env:
        lines += [f'api_key_env = "{targets.efficient_api_key_env}"']
    lines += [""]

    lines += ["[targets.capable]", f'id = "{targets.capable_id}"', 'llm_client = "capable"', ""]
    lines += [
        "[targets.efficient]",
        f'id = "{targets.efficient_id}"',
        'llm_client = "efficient"',
        "",
    ]

    lines += [f"[routes.{route_id}]", f'id = "{route_id}"', 'type = "stage_router"']
    lines += ['capable_target = "capable"', 'efficient_target = "efficient"']
    lines += [f'picker = "{picker}"', f"confidence_threshold = {confidence_threshold}"]
    return "\n".join(lines) + "\n"


def write_switchyard_config(data_dir: Path, toml_text: str) -> Path:
    """Write the rendered TOML to <data_dir>/switchyard.toml, mode 0600. Never committed."""
    path = Path(data_dir) / "switchyard.toml"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(toml_text)
    os.chmod(path, 0o600)
    return path
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_router_switchyard.py -q`
Expected: PASS (12 tests)

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/router_switchyard.py tests/test_router_switchyard.py
git commit -m "gateway: render Switchyard's native TOML from SwitchyardTargets

Field names and shape are asserted against the pinned fixture, not typed from
memory. forward_auth is used when no API key env var is set, matching the
schema's documented (if unverified end-to-end here) OAuth-forwarding path."
```

---

### Task 3: Config fields + a config-to-targets adapter

**Files:**
- Modify: `src/opendaisugi/config.py:96` (after `pathway_store_backend`)
- Modify: `src/opendaisugi/router_switchyard.py`
- Create: `tests/test_config_switchyard.py`

**Interfaces:**
- Consumes: `SwitchyardTargets` (Task 2). From spec-08/plan-08 (not yet landed — read
  defensively via `getattr`): `Config.llm_base_url: str | None`.
- Produces: `Config.gateway_router: str = "rules"`, `Config.switchyard_route_id: str =
  "daisugi"`, `Config.switchyard_capable_model: str = "claude-sonnet-5"`,
  `Config.switchyard_efficient_model: str | None = None`,
  `targets_from_config(cfg: Config) -> SwitchyardTargets | None`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_config_switchyard.py
"""spec-09: the config fields that persist a Switchyard router selection, and the
adapter that turns them into SwitchyardTargets for the renderer."""

from __future__ import annotations

from opendaisugi.config import default_config, load_config, save_config
from opendaisugi.router_switchyard import targets_from_config


def test_config_defaults_to_the_rules_router():
    cfg = default_config()
    assert cfg.gateway_router == "rules"
    assert cfg.switchyard_route_id == "daisugi"
    assert cfg.switchyard_capable_model == "claude-sonnet-5"
    assert cfg.switchyard_efficient_model is None


def test_config_round_trips_switchyard_fields(tmp_path):
    cfg = default_config().model_copy(
        update={
            "gateway_router": "switchyard",
            "switchyard_route_id": "daisugi",
            "switchyard_capable_model": "claude-sonnet-5",
            "switchyard_efficient_model": "qwen3-coder:30b",
        }
    )
    path = tmp_path / "config.yaml"
    save_config(cfg, path)
    loaded = load_config(path)
    assert loaded.gateway_router == "switchyard"
    assert loaded.switchyard_efficient_model == "qwen3-coder:30b"


def test_targets_from_config_is_none_without_a_qualified_efficient_model():
    assert targets_from_config(default_config()) is None


def test_targets_from_config_builds_a_local_efficient_target():
    cfg = default_config().model_copy(
        update={
            "switchyard_efficient_model": "qwen3-coder:30b",
            "gateway_local_model": "qwen3-coder:30b",
        }
    )
    targets = targets_from_config(cfg)
    assert targets is not None
    assert targets.efficient_client_format == "openai_chat"
    assert targets.efficient_base_url == "http://127.0.0.1:11434/v1"
    assert targets.efficient_api_key_env is None
    assert targets.capable_id == "claude-sonnet-5"


def test_targets_from_config_builds_a_cloud_efficient_target():
    cfg = default_config().model_copy(update={"switchyard_efficient_model": "claude-haiku-4-5"})
    targets = targets_from_config(cfg)
    assert targets is not None
    assert targets.efficient_client_format == "anthropic_messages"
    assert targets.efficient_api_key_env == "ANTHROPIC_API_KEY"


def test_targets_from_config_prefers_llm_base_url_when_present():
    cfg = default_config().model_copy(
        update={
            "switchyard_efficient_model": "qwen3-coder:30b",
            "gateway_local_model": "qwen3-coder:30b",
        }
    )
    # spec-08's field — may not exist on Config yet; the adapter reads it
    # defensively, so simulate it being present without requiring plan-08.
    object.__setattr__(cfg, "__dict__", cfg.__dict__)  # no-op, keeps pydantic happy
    cfg_dict = cfg.model_dump()
    cfg_dict["llm_base_url"] = "http://192.168.1.50:11434/v1"
    try:
        cfg_with_host = cfg.model_copy(update={"llm_base_url": "http://192.168.1.50:11434/v1"})
    except ValueError:
        import pytest

        pytest.skip("Config.llm_base_url not defined yet (spec-08 not landed)")
    targets = targets_from_config(cfg_with_host)
    assert targets.efficient_base_url == "http://192.168.1.50:11434/v1"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_config_switchyard.py -q`
Expected: FAIL — `AttributeError: 'Config' object has no attribute 'gateway_router'`

- [ ] **Step 3: Write the implementation**

```python
# insert into src/opendaisugi/config.py, in class Config, after line 96
# (after the pathway_store_backend field)

    # spec-09: which chooser assigns a model to each gateway turn. "rules" is the
    # no-dependency default (opendaisugi.gateway.route_turn); "switchyard" hands that
    # choice to NVIDIA NeMo Switchyard, running as a managed local proxy the gateway
    # forwards to unchanged; "off" disables local routing entirely (pure metered
    # passthrough — no downgrade, no external chooser either).
    gateway_router: str = "rules"  # rules | switchyard | off

    # The model string the harness must send as `model` to select Switchyard's route
    # (Switchyard requires clients to send the ROUTE id, never a real model name).
    # Only consulted when gateway_router == "switchyard".
    switchyard_route_id: str = "daisugi"

    # The two targets Switchyard's stage_router chooses between. capable_model SHOULD
    # equal the literal model id the harness sends today (e.g. "claude-sonnet-5") — the
    # gateway compares the response's served model against THIS string, not the
    # harness's request, to tell "served capable" apart from "served efficient" honestly.
    switchyard_capable_model: str = "claude-sonnet-5"

    # None until a local/cheap model has been qualified for the efficient tier.
    # `daisugi install --router switchyard` refuses to proceed with this unset.
    switchyard_efficient_model: str | None = None
```

```python
# add to src/opendaisugi/router_switchyard.py, after SwitchyardTargets
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from opendaisugi.config import Config

_DEFAULT_LOCAL_BASE_URL = "http://127.0.0.1:11434/v1"


def targets_from_config(cfg: "Config") -> SwitchyardTargets | None:
    """Build SwitchyardTargets from persisted config, or None if nothing is qualified
    for the efficient tier yet (nothing to route to).

    `llm_base_url` is spec-08's field (`daisugi setup --remote`); read defensively so
    this plan works whether or not that plan has landed yet.
    """
    if not cfg.switchyard_efficient_model:
        return None
    is_local = cfg.switchyard_efficient_model == cfg.gateway_local_model
    local_base_url = getattr(cfg, "llm_base_url", None) or _DEFAULT_LOCAL_BASE_URL
    return SwitchyardTargets(
        capable_id=cfg.switchyard_capable_model,
        efficient_id=cfg.switchyard_efficient_model,
        efficient_client_format="openai_chat" if is_local else "anthropic_messages",
        efficient_base_url=local_base_url if is_local else "https://api.anthropic.com",
        efficient_api_key_env=None if is_local else "ANTHROPIC_API_KEY",
    )
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_config_switchyard.py -q`
Expected: PASS (6 tests; the `llm_base_url` test skips cleanly if spec-08 hasn't landed)

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/config.py src/opendaisugi/router_switchyard.py \
  tests/test_config_switchyard.py
git commit -m "gateway: persist the router choice and adapt config into SwitchyardTargets

gateway_router/switchyard_route_id/switchyard_capable_model/
switchyard_efficient_model follow the gateway_local_model precedent (flat
pydantic fields, install-time writer). targets_from_config reads spec-08's
llm_base_url defensively so this works whether or not that plan has landed."
```

---

### Task 4: Start, health-check, and stop the managed child

**Files:**
- Modify: `src/opendaisugi/router_switchyard.py`
- Modify: `tests/test_router_switchyard.py`

**Interfaces:**
- Consumes: `BINARY_NAME`, `HEALTH_PATH`, `DEFAULT_HOST`, `DEFAULT_PORT` (Task 1).
- Produces: `SwitchyardHandle` (frozen dataclass: `pid: int`, `host: str`, `port: int`,
  `config_path: Path`, `argv: list[str]`, property `base_url: str`), `probe_health(host: str,
  port: int) -> bool` (public — Task 8's `router status` calls it directly, not just as
  `start_switchyard`'s default),
  `start_switchyard(config_path: Path, *, host: str = DEFAULT_HOST, port: int =
  DEFAULT_PORT, binary: str | None = None, routing_log_path: Path | None = None, pid_path:
  Path | None = None, spawn: Callable[[list[str]], int] = ..., health_probe: Callable[[str,
  int], bool] = ..., wait_s: float = 3.0, poll_interval_s: float = 0.05) ->
  tuple[SwitchyardHandle | None, str]`, `stop_switchyard(pid_path: Path, *, kill:
  Callable[[int, int], None] = os.kill) -> str`.

- [ ] **Step 1: Write the failing tests**

```python
# append to tests/test_router_switchyard.py
import signal
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

from opendaisugi.router_switchyard import (
    SwitchyardHandle,
    probe_health,
    start_switchyard,
    stop_switchyard,
)


def test_start_switchyard_reports_done_once_health_answers(tmp_path):
    calls = {"n": 0}

    def fake_spawn(argv: list[str]) -> int:
        return 4242

    def fake_health(host: str, port: int) -> bool:
        calls["n"] += 1
        return calls["n"] >= 2  # not healthy on the first poll, healthy on the second

    pid_path = tmp_path / "switchyard.pid"
    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        spawn=fake_spawn,
        health_probe=fake_health,
        pid_path=pid_path,
        wait_s=1.0,
        poll_interval_s=0.01,
    )
    assert isinstance(handle, SwitchyardHandle)
    assert handle.pid == 4242
    assert handle.base_url == "http://127.0.0.1:4000"
    assert "healthy" in msg
    assert pid_path.read_text() == "4242"


def test_start_switchyard_passes_config_host_port_and_routing_log_in_argv(tmp_path):
    seen = {}

    def fake_spawn(argv: list[str]) -> int:
        seen["argv"] = argv
        return 1

    start_switchyard(
        tmp_path / "routes.toml",
        host="0.0.0.0",
        port=5001,
        routing_log_path=tmp_path / "routing.jsonl",
        spawn=fake_spawn,
        health_probe=lambda h, p: True,
        wait_s=1.0,
        poll_interval_s=0.01,
    )
    assert seen["argv"] == [
        "switchyard-server",
        "--config",
        str(tmp_path / "routes.toml"),
        "--host",
        "0.0.0.0",
        "--port",
        "5001",
        "--routing-log-file",
        str(tmp_path / "routing.jsonl"),
    ]


def test_start_switchyard_times_out_with_a_dry_run_hint(tmp_path):
    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        spawn=lambda argv: 1,
        health_probe=lambda h, p: False,
        wait_s=0.05,
        poll_interval_s=0.01,
    )
    assert handle is None
    assert "--dry-run" in msg
    assert "switchyard-server" in msg


def test_probe_health_reads_a_real_http_server():
    class _Health(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            self.send_response(200)
            self.end_headers()

        def log_message(self, *a):  # silence the test output
            pass

    server = HTTPServer(("127.0.0.1", 0), _Health)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        assert probe_health("127.0.0.1", port) is True
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_probe_health_is_false_when_nothing_is_listening():
    assert probe_health("127.0.0.1", 1) is False


def test_stop_switchyard_sends_sigterm_and_removes_the_pid_file(tmp_path):
    pid_path = tmp_path / "switchyard.pid"
    pid_path.write_text("4242")
    sent = {}

    def fake_kill(pid: int, sig: int) -> None:
        sent["pid"], sent["sig"] = pid, sig

    msg = stop_switchyard(pid_path, kill=fake_kill)
    assert sent == {"pid": 4242, "sig": signal.SIGTERM}
    assert not pid_path.exists()
    assert "sent SIGTERM" in msg


def test_stop_switchyard_is_idempotent_with_no_pid_file(tmp_path):
    assert "nothing to stop" in stop_switchyard(tmp_path / "nope.pid")


def test_stop_switchyard_handles_an_already_dead_process(tmp_path):
    pid_path = tmp_path / "switchyard.pid"
    pid_path.write_text("999999")

    def fake_kill(pid: int, sig: int) -> None:
        raise ProcessLookupError()

    msg = stop_switchyard(pid_path, kill=fake_kill)
    assert "already stopped" in msg
    assert not pid_path.exists()
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_router_switchyard.py -q`
Expected: FAIL — `ImportError: cannot import name 'SwitchyardHandle'`

- [ ] **Step 3: Write the implementation**

```python
# add to src/opendaisugi/router_switchyard.py
import os
import signal
import subprocess
import time
import urllib.error
import urllib.request


@dataclass(frozen=True)
class SwitchyardHandle:
    pid: int
    host: str
    port: int
    config_path: Path
    argv: list[str]

    @property
    def base_url(self) -> str:
        return f"http://{self.host}:{self.port}"


def _default_spawn(argv: list[str]) -> int:
    """Detach a child the way opendaisugi.start._detach does — fire and forget, but
    return the pid so the caller can track it in a pid file (unlike the resident gate,
    which nobody currently stops programmatically, a Switchyard child must be stoppable)."""
    proc = subprocess.Popen(
        argv,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    return proc.pid


def probe_health(host: str, port: int) -> bool:
    try:
        with urllib.request.urlopen(f"http://{host}:{port}{HEALTH_PATH}", timeout=0.5) as resp:
            return resp.status == 200
    except (urllib.error.URLError, OSError, TimeoutError):
        return False


def start_switchyard(
    config_path: Path,
    *,
    host: str = DEFAULT_HOST,
    port: int = DEFAULT_PORT,
    binary: str | None = None,
    routing_log_path: Path | None = None,
    pid_path: Path | None = None,
    spawn: Callable[[list[str]], int] = _default_spawn,
    health_probe: Callable[[str, int], bool] = probe_health,
    wait_s: float = 3.0,
    poll_interval_s: float = 0.05,
) -> tuple[SwitchyardHandle | None, str]:
    """Start switchyard-server as a managed child; wait for GET /health.

    Mirrors opendaisugi.start's detach-then-poll pattern (start.py's `_detach` plus the
    gate-server socket wait in `_steps`), swapped for an HTTP health check since
    Switchyard's liveness signal is `/health`, not a filesystem socket. Returns
    (handle, message); handle is None on a health-wait timeout, and the message then
    names the exact `--dry-run` command to debug with — it never claims the daemon is
    up without having actually seen it answer.
    """
    resolved_binary = binary or BINARY_NAME
    argv = [resolved_binary, "--config", str(config_path), "--host", host, "--port", str(port)]
    if routing_log_path is not None:
        argv += ["--routing-log-file", str(routing_log_path)]
    pid = spawn(argv)
    t0 = time.monotonic()
    while time.monotonic() - t0 < wait_s:
        if health_probe(host, port):
            handle = SwitchyardHandle(
                pid=pid, host=host, port=port, config_path=Path(config_path), argv=argv
            )
            if pid_path is not None:
                pid_path.parent.mkdir(parents=True, exist_ok=True)
                pid_path.write_text(str(pid))
            return handle, f"started `{' '.join(argv)}` (pid {pid}), healthy on {handle.base_url}"
        time.sleep(poll_interval_s)
    return None, (
        f"started `{' '.join(argv)}` (pid {pid}) but http://{host}:{port}{HEALTH_PATH} never "
        f"answered after {wait_s:.0f}s. Run `{resolved_binary} --config {config_path} "
        f"--dry-run` to check the config, then run the command above directly to see why "
        f"it exited."
    )


def stop_switchyard(pid_path: Path, *, kill: Callable[[int, int], None] = os.kill) -> str:
    """Send SIGTERM to a switchyard-server started via start_switchyard's pid_path.

    Switchyard drains active requests for up to --shutdown-timeout (30s default) on
    SIGTERM before exiting (its own documented shutdown behavior). Idempotent: a
    missing or already-dead pid file is reported, never raised.
    """
    if not pid_path.exists():
        return "no switchyard-server pid file found; nothing to stop"
    try:
        pid = int(pid_path.read_text().strip())
    except ValueError:
        pid_path.unlink(missing_ok=True)
        return f"pid file {pid_path} was unreadable; removed it"
    try:
        kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pid_path.unlink(missing_ok=True)
        return f"switchyard-server (pid {pid}) was already stopped"
    pid_path.unlink(missing_ok=True)
    return f"sent SIGTERM to switchyard-server (pid {pid})"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_router_switchyard.py -q`
Expected: PASS (20 tests)

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/router_switchyard.py tests/test_router_switchyard.py
git commit -m "gateway: start/stop switchyard-server as a managed child, over /health

Mirrors start.py's detach-then-poll pattern with an HTTP health check instead
of a socket file. A timeout names the exact --dry-run command to debug with;
stop is a pid-file SIGTERM, idempotent against a missing or dead process."
```

---

### Task 5: `Gateway` grows a router mode — rules, external, off

**Files:**
- Modify: `src/opendaisugi/gateway_pipeline.py:53-102`
- Create: `tests/test_gateway_upstream.py`

**Interfaces:**
- Consumes: `RouteDecision`, `PreparedTurn`, `_latest_user_text`, `_new_user_text` (existing,
  `opendaisugi.gateway`/`opendaisugi.gateway_pipeline`).
- Produces: `ExternalRouterConfig` (frozen dataclass: `route_id: str`, `capable_target:
  str`, `efficient_target: str`, `prices: dict[str, tuple[float, float]] = {}`),
  `Gateway.router_mode: str = "rules"` (`"rules" | "external" | "off"`),
  `Gateway.external: ExternalRouterConfig | None = None`. `Gateway.prepare()` keeps its
  existing signature/behavior for `router_mode == "rules"` (the default — no caller is
  affected).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_gateway_upstream.py
"""spec-09: the gateway's external router mode — Switchyard (or any outside chooser)
owns the model choice; the gateway never rewrites/downgrades locally in this mode.
"""

from __future__ import annotations

import pytest

from opendaisugi.gateway_pipeline import ExternalRouterConfig, Gateway


def _external_gateway(**overrides) -> Gateway:
    ext = ExternalRouterConfig(
        route_id="daisugi",
        capable_target="claude-sonnet-5",
        efficient_target="qwen3-coder:30b",
    )
    kwargs = {"router_mode": "external", "external": ext}
    kwargs.update(overrides)
    return Gateway(**kwargs)


def test_rules_mode_is_unchanged_by_default():
    gw = Gateway()
    assert gw.router_mode == "rules"
    assert gw.external is None
    prepared = gw.prepare(
        {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}
    )
    assert prepared.outbound_body["model"] == "claude-haiku-4-5"  # existing downgrade behavior


def test_external_mode_requires_a_config():
    with pytest.raises(ValueError, match="ExternalRouterConfig"):
        Gateway(router_mode="external")


def test_external_prepare_sends_the_route_id_not_the_harness_model():
    gw = _external_gateway()
    body = {"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}
    prepared = gw.prepare(body)
    assert prepared.outbound_body["model"] == "daisugi"
    assert prepared.decision.tier == "tier-switchyard"
    assert prepared.decision.model == "daisugi"
    assert prepared.decision.requested_model == "claude-sonnet-5"
    assert prepared.decision.downgraded is False  # not known until the response comes back
    assert body["model"] == "claude-sonnet-5"  # the caller's body is never mutated in place


def test_external_mode_registers_target_prices():
    ext = ExternalRouterConfig(
        route_id="daisugi",
        capable_target="claude-sonnet-5",
        efficient_target="qwen3-coder:30b",
        prices={"qwen3-coder:30b": (0.0, 0.0)},
    )
    gw = Gateway(router_mode="external", external=ext)
    assert gw.prices["qwen3-coder:30b"] == (0.0, 0.0)


def test_off_mode_forwards_the_body_completely_unchanged():
    gw = Gateway(router_mode="off")
    body = {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}
    prepared = gw.prepare(body)
    assert prepared.outbound_body == body
    assert prepared.outbound_body is not body  # still a copy, never the caller's own dict
    assert prepared.decision.tier == "tier-off"
    assert prepared.decision.downgraded is False
    assert prepared.decision.model == "claude-opus-4-8"


def test_off_mode_needs_no_external_config():
    Gateway(router_mode="off")  # must not raise
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_gateway_upstream.py -q`
Expected: FAIL — `ImportError: cannot import name 'ExternalRouterConfig'`

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/gateway_pipeline.py:53 — insert ExternalRouterConfig before class Gateway,
# then modify Gateway's field list, __post_init__, and prepare()


@dataclass(frozen=True)
class ExternalRouterConfig:
    """What the gateway must know about an external chooser (Switchyard) to meter it
    honestly (spec-09, master spec §5.1 applied to the meter).

    ``route_id`` is the model string the harness must send to select Switchyard's route
    (Switchyard requires clients to send the ROUTE id, never a real model name — see
    router_switchyard.py's pinned facts). ``capable_target``/``efficient_target`` are the
    two real model ids Switchyard's stage_router chooses between; comparing the response's
    served model against THESE strings — never against the harness's own request — is what
    lets "served capable" (no saving) and "served efficient" (a real saving) be told apart
    honestly, instead of a string-diff against the harness's plain model name that would
    over-report every turn as a saving just because a provider-qualified target id never
    equals it.
    """

    route_id: str
    capable_target: str
    efficient_target: str
    prices: dict[str, tuple[float, float]] = field(default_factory=dict)


@dataclass
class Gateway:
    """Ties routing, the meter, and the turn journal into the proxy's two touchpoints."""

    cheap_model: str = _DEFAULT_CHEAP_MODEL
    journal: GatewayJournal | None = None
    prices: dict[str, tuple[float, float]] = field(default_factory=lambda: _PRICES_PER_MTOK)
    answer_store: "AnswerStore | None" = None
    capture_answers: bool = False
    local_model: str | None = None
    # spec-09: "rules" (default) uses route_turn below; "external" hands model choice to
    # `external` (Switchyard) and never rewrites/downgrades locally; "off" forwards every
    # turn completely unchanged (pure metered passthrough, no chooser at all).
    router_mode: str = "rules"  # rules | external | off
    external: ExternalRouterConfig | None = None
    _session_models: dict = field(default_factory=dict, repr=False)

    _MAX_SESSIONS = 4096

    def __post_init__(self) -> None:
        if self.local_model and self.local_model not in self.prices:
            self.prices = {**self.prices, self.local_model: (0.0, 0.0)}
        if self.router_mode == "external":
            if self.external is None:
                raise ValueError("router_mode='external' needs an ExternalRouterConfig")
            self.prices = {**self.prices, **self.external.prices}

    def prepare(self, body: dict) -> PreparedTurn:
        """Decide the model for one turn and produce the body to forward.

        In "external"/"off" mode this never consults route_turn or the sticky-session
        table below — an outside chooser (or nothing) owns the decision, so there is no
        local downgrade to record here (external mode's honest saving/no-saving call is
        made later, in the ASGI layer's `_record`, once the response names what actually
        served the turn)."""
        if self.router_mode == "external":
            return self._prepare_external(body)
        if self.router_mode == "off":
            return self._prepare_off(body)

        task = _latest_user_text(body)
        ask = _new_user_text(body)
        key = conversation_key(body)
        decision = route_turn(
            body,
            cheap_model=self.cheap_model,
            local_model=self.local_model,
            sticky_model=self._session_models.get(key),
        )
        if len(self._session_models) >= self._MAX_SESSIONS and key not in self._session_models:
            self._session_models.pop(next(iter(self._session_models)))
        self._session_models[key] = decision.model
        outbound_body = dict(body)
        if decision.downgraded:
            outbound_body["model"] = decision.model
        return PreparedTurn(decision=decision, outbound_body=outbound_body, task=task, ask=ask)

    def _prepare_external(self, body: dict) -> PreparedTurn:
        task = _latest_user_text(body)
        ask = _new_user_text(body)
        requested = body.get("model", "")
        decision = RouteDecision(
            tier="tier-switchyard",
            model=self.external.route_id,
            requested_model=requested,
            difficulty=0.0,
            downgraded=False,
            reason=(
                f"external router (switchyard) owns model choice; sent as route "
                f"'{self.external.route_id}'"
            ),
        )
        outbound_body = dict(body)
        outbound_body["model"] = self.external.route_id
        return PreparedTurn(decision=decision, outbound_body=outbound_body, task=task, ask=ask)

    def _prepare_off(self, body: dict) -> PreparedTurn:
        task = _latest_user_text(body)
        ask = _new_user_text(body)
        requested = body.get("model", "")
        decision = RouteDecision(
            tier="tier-off",
            model=requested,
            requested_model=requested,
            difficulty=0.0,
            downgraded=False,
            reason="routing disabled (--router off); pure metered passthrough",
        )
        return PreparedTurn(decision=decision, outbound_body=dict(body), task=task, ask=ask)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_gateway_upstream.py tests/test_gateway_pipeline.py -q`
Expected: PASS (all — the existing pipeline tests are untouched since `router_mode`
defaults to `"rules"`)

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gateway_pipeline.py tests/test_gateway_upstream.py
git commit -m "gateway: add router_mode (external/off) alongside the existing rules router

External mode sends Switchyard's configured route id instead of downgrading
locally; the honest saving/no-saving call is deferred to the ASGI layer once
the response names what actually served the turn (next task)."
```

---

### Task 6: Capture the served model, meter it honestly

**Files:**
- Modify: `src/opendaisugi/gateway_asgi.py:72-117` (`_UsageSniffer`), `:224-329` (`_record`,
  `_dispatch_streaming`, `_dispatch_buffered`, `_extract_buffered_text`), `:332-363`
  (`build_default_gateway`)
- Modify: `tests/test_gateway_upstream.py`

**Interfaces:**
- Consumes: `ExternalRouterConfig`, `Gateway.router_mode`, `Gateway.external` (Task 5).
- Produces: `_UsageSniffer.model: str | None` (new attribute), `_extract_buffered_model(data:
  dict) -> str | None`, `_record(gateway, prepared, usage, used_original, answer_text=None,
  route_target=None)` (new keyword), `build_default_gateway(..., router_mode: str = "rules",
  external: "ExternalRouterConfig | None" = None)` (new keywords, both optional — every
  existing caller is unaffected).

- [ ] **Step 1: Write the failing tests**

```python
# append to tests/test_gateway_upstream.py
import json

import httpx

from opendaisugi.gateway_asgi import make_gateway_app
from opendaisugi.gateway_journal import GatewayJournal


def _mock_upstream(handler):
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def _call(app, body):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        return await client.post(
            "/v1/messages",
            content=json.dumps(body).encode(),
            headers={"authorization": "Bearer sk-oauth-XYZ", "content-type": "application/json"},
        )


def _stream_body(model: str, text: str) -> dict:
    return {
        "model": model,
        "max_tokens": 1024,
        "stream": True,
        "messages": [{"role": "user", "content": text}],
    }


def _sse_served_by(model: str) -> bytes:
    return (
        b"event: message_start\n"
        b'data: {"type":"message_start","message":{"model":"' + model.encode() + b'",'
        b'"usage":{"input_tokens":1000,"cache_read_input_tokens":0,'
        b'"cache_creation_input_tokens":0,"output_tokens":1}}}\n\n'
        b"event: content_block_delta\n"
        b'data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}\n\n'
        b"event: message_delta\n"
        b'data: {"type":"message_delta","usage":{"output_tokens":250}}\n\n'
        b'event: message_stop\ndata: {"type":"message_stop"}\n\n'
    )


def _external_gateway_with_journal(journal) -> "Gateway":
    ext = ExternalRouterConfig(
        route_id="daisugi",
        capable_target="claude-sonnet-5",
        efficient_target="qwen3-coder:30b",
        prices={"qwen3-coder:30b": (0.0, 0.0)},
    )
    return Gateway(router_mode="external", external=ext, journal=journal)


async def test_external_mode_forwards_the_rest_of_the_body_byte_for_byte(tmp_path):
    # spec-09: the only thing external mode may change on the way out is `model`
    # (swapped to the route id) -- everything else, including a tool_result-shaped
    # message list, must reach Switchyard exactly as the harness sent it.
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        return httpx.Response(
            200,
            headers={"content-type": "text/event-stream"},
            content=_sse_served_by("claude-sonnet-5"),
        )

    body = _stream_body("claude-sonnet-5", "say hi")
    body["metadata"] = {"user_id": "abc"}
    app = make_gateway_app(
        _external_gateway_with_journal(None),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    await _call(app, body)
    expected = dict(body)
    expected["model"] = "daisugi"
    assert seen["body"] == expected


async def test_switchyard_turn_prices_the_served_model_not_the_requested_one(tmp_path):
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        assert json.loads(request.content)["model"] == "daisugi"  # sent as the route id
        return httpx.Response(
            200,
            headers={"content-type": "text/event-stream"},
            content=_sse_served_by("qwen3-coder:30b"),
        )

    app = make_gateway_app(
        _external_gateway_with_journal(journal),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    resp = await _call(app, _stream_body("claude-sonnet-5", "say hi"))
    assert resp.status_code == 200
    rec = journal.load()[0]
    assert rec.model == "qwen3-coder:30b"
    assert rec.downgraded is True
    assert rec.frontier_tokens_saved > 0


async def test_capable_tier_turn_books_no_saving(tmp_path):
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            headers={"content-type": "text/event-stream"},
            content=_sse_served_by("claude-sonnet-5"),
        )

    app = make_gateway_app(
        _external_gateway_with_journal(journal),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    await _call(app, _stream_body("claude-sonnet-5", "say hi"))
    rec = journal.load()[0]
    assert rec.model == "claude-sonnet-5"
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


async def test_unknown_served_model_books_no_saving(tmp_path):
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            headers={"content-type": "text/event-stream"},
            content=_sse_served_by("some-other-provider/model-x"),
        )

    app = make_gateway_app(
        _external_gateway_with_journal(journal),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    await _call(app, _stream_body("claude-sonnet-5", "say hi"))
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


async def test_route_target_equal_to_the_route_id_never_books_a_saving(tmp_path):
    # Guards the open question in router_switchyard.py: if a response ever echoes the
    # ROUTE id instead of the target id, it matches neither capable nor efficient — the
    # existing "unknown served model" path already makes this safe (undersells, never
    # fabricates), with no extra logic required.
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200, headers={"content-type": "text/event-stream"}, content=_sse_served_by("daisugi")
        )

    app = make_gateway_app(
        _external_gateway_with_journal(journal),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    await _call(app, _stream_body("claude-sonnet-5", "say hi"))
    rec = journal.load()[0]
    assert rec.downgraded is False
    assert rec.frontier_tokens_saved == 0


async def test_unknown_route_id_returns_switchyards_error_naming_the_route():
    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        return httpx.Response(404, json={"error": {"message": f"unknown route '{body['model']}'"}})

    # External mode never adds a fail-open retry attempt (that only happens on a local
    # downgrade, and external mode never marks one at prepare-time) — so a bad route id
    # reaches the client as Switchyard's own error, naming the route it didn't recognize.
    app = make_gateway_app(
        _external_gateway_with_journal(None),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    resp = await _call(app, _stream_body("claude-sonnet-5", "say hi"))
    assert resp.status_code == 404
    assert "daisugi" in resp.text


async def test_tool_use_block_and_streaming_deltas_survive_external_mode(tmp_path):
    sse = (
        b"event: message_start\n"
        b'data: {"type":"message_start","message":{"model":"qwen3-coder:30b","usage":{'
        b'"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,'
        b'"output_tokens":1}}}\n\n'
        b"event: content_block_start\n"
        b'data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use",'
        b'"id":"toolu_1","name":"Bash","input":{}}}\n\n'
        b"event: content_block_delta\n"
        b'data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"running"}}\n\n'
        b"event: message_delta\n"
        b'data: {"type":"message_delta","usage":{"output_tokens":5}}\n\n'
        b'event: message_stop\ndata: {"type":"message_stop"}\n\n'
    )
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=sse)

    app = make_gateway_app(
        _external_gateway_with_journal(journal),
        upstream_base_url="http://up",
        client=_mock_upstream(handler),
    )
    resp = await _call(app, _stream_body("claude-sonnet-5", "run ls"))
    assert resp.content == sse  # byte-for-byte, tool_use block intact
    rec = journal.load()[0]
    assert rec.model == "qwen3-coder:30b"
    assert rec.downgraded is True


import os

import pytest


@pytest.mark.skipif(
    not os.environ.get("OPENDAISUGI_TEST_SWITCHYARD"),
    reason=(
        "set OPENDAISUGI_TEST_SWITCHYARD=1, run a real switchyard-server against a "
        "config with a live provider key, and edit this test's host/port to verify "
        "the response's `model` field names the served target rather than echoing "
        "the route id back (router_switchyard.py's open question, Task 1)"
    ),
)
def test_response_model_names_the_served_target():
    pytest.skip("run manually against a real switchyard-server + provider key")
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_gateway_upstream.py -q`
Expected: FAIL — `rec.model == "daisugi"` (the response's real model is not yet captured;
`_record` has no `route_target` parameter)

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/gateway_asgi.py:83 — _UsageSniffer.__init__ gains .model
    def __init__(self) -> None:
        self.usage: dict[str, int] = {}
        self.text: str = ""
        self.model: str | None = None
        self._buf = ""
```

```python
# src/opendaisugi/gateway_asgi.py:102-106 — feed() also captures the served model
                u = None
                if obj.get("type") == "message_start":
                    message = obj.get("message", {})
                    u = message.get("usage")
                    model = message.get("model")
                    if isinstance(model, str):
                        self.model = model
                elif obj.get("type") == "message_delta":
```

```python
# src/opendaisugi/gateway_asgi.py:329 — new helper, next to _extract_buffered_text
def _extract_buffered_model(data: dict) -> str | None:
    """The served model off a non-streamed Messages response, or None if absent/wrong type."""
    model = data.get("model")
    return model if isinstance(model, str) else None
```

```python
# src/opendaisugi/gateway_asgi.py:224-252 — _record grows route_target and the
# external-mode honesty branch
def _record(
    gateway: "Gateway",
    prepared: "PreparedTurn | None",
    usage: dict,
    used_original: bool,
    answer_text: str | None = None,
    route_target: str | None = None,
):
    """Measure and journal the turn — best-effort; a meter failure never breaks the proxy.

    ``used_original`` means the fail-open retry ran (rules mode only — external/off modes
    never mark a downgrade at prepare-time, so there is never a retry attempt for them):
    the turn actually served on the requested model, booked as *not* downgraded.

    In external mode, ``route_target`` is the model the response actually named (or None
    if it couldn't be read). The decision is replaced so ``downgraded`` compares the
    *served* model against Switchyard's own configured targets — never against the
    harness's request string, which would over-report every turn as a saving. A served
    model matching neither known target (including one that turns out to equal the ROUTE
    id, if Switchyard ever echoes that instead) safely books no saving rather than an
    unattributable one."""
    if prepared is None:
        return
    try:
        eff = prepared
        if used_original:
            reverted = replace(
                prepared.decision,
                model=prepared.decision.requested_model,
                tier="tier2-frontier",
                downgraded=False,
                reason="downgrade rejected by upstream 4xx; served the original model",
            )
            eff = replace(prepared, decision=reverted)
        elif gateway is not None and gateway.router_mode == "external" and gateway.external:
            ext = gateway.external
            served = route_target or prepared.decision.requested_model
            downgraded = route_target is not None and route_target == ext.efficient_target
            actual = replace(
                prepared.decision,
                model=served,
                requested_model=ext.capable_target,
                tier="tier-switchyard",
                downgraded=downgraded,
                reason=(
                    f"switchyard served the efficient target ({served})"
                    if downgraded
                    else f"switchyard served {served}; booked no saving"
                ),
            )
            eff = replace(prepared, decision=actual)
        gateway.finish(eff, usage, answer_text=answer_text)
    except Exception as exc:  # pragma: no cover - defensive
        _log.warning("gateway meter/journal failed (turn still served): %s", exc)
```

```python
# src/opendaisugi/gateway_asgi.py:255-276 — _dispatch_streaming passes sniffer.model
async def _dispatch_streaming(
    cl, url, headers, attempts, send, gateway, prepared, wire="anthropic"
) -> None:
    sniffer = OpenAIUsageSniffer() if wire == "openai" else _UsageSniffer()
    for i, (content, used_original) in enumerate(attempts):
        is_last = i == len(attempts) - 1
        async with cl.stream("POST", url, content=content, headers=headers) as resp:
            if resp.status_code >= 400 and not is_last:
                continue
            await send(
                {
                    "type": "http.response.start",
                    "status": resp.status_code,
                    "headers": _response_headers(resp),
                }
            )
            async for chunk in resp.aiter_bytes():
                sniffer.feed(chunk)
                await send({"type": "http.response.body", "body": chunk, "more_body": True})
            await send({"type": "http.response.body", "body": b"", "more_body": False})
            _record(
                gateway,
                prepared,
                sniffer.usage,
                used_original,
                answer_text=sniffer.text,
                route_target=getattr(sniffer, "model", None),
            )
            return
```

```python
# src/opendaisugi/gateway_asgi.py:279-309 — _dispatch_buffered passes the extracted model
async def _dispatch_buffered(
    cl, url, headers, attempts, send, gateway, prepared, wire="anthropic"
) -> None:
    for i, (content, used_original) in enumerate(attempts):
        is_last = i == len(attempts) - 1
        resp = await cl.request("POST", url, content=content, headers=headers)
        if resp.status_code >= 400 and not is_last:
            continue
        await send(
            {
                "type": "http.response.start",
                "status": resp.status_code,
                "headers": _response_headers(resp),
            }
        )
        await send({"type": "http.response.body", "body": resp.content, "more_body": False})
        usage = {}
        answer_text = ""
        route_target = None
        try:
            data = resp.json()
        except (json.JSONDecodeError, ValueError):
            data = None
        if isinstance(data, dict):
            if wire == "openai":
                usage = normalize_openai_usage(data.get("usage"))
                answer_text = extract_openai_text(data)
            else:
                usage = data.get("usage", {}) or {}
                answer_text = _extract_buffered_text(data)
                route_target = _extract_buffered_model(data)
        _record(
            gateway,
            prepared,
            usage,
            used_original,
            answer_text=answer_text,
            route_target=route_target,
        )
        return
```

```python
# src/opendaisugi/gateway_asgi.py:332-363 — build_default_gateway forwards the new modes
def build_default_gateway(
    *,
    data_dir: Path,
    cheap_model: str | None = None,
    journalling: bool = True,
    capture_answers: bool = False,
    local_model: str | None = None,
    router_mode: str = "rules",
    external: "ExternalRouterConfig | None" = None,
):
    from opendaisugi.gateway_journal import GatewayJournal
    from opendaisugi.gateway_pipeline import Gateway

    journal = (
        GatewayJournal(path=Path(data_dir) / "gateway" / "turns.jsonl") if journalling else None
    )
    kwargs: dict = {"journal": journal, "router_mode": router_mode}
    if external is not None:
        kwargs["external"] = external
    if cheap_model:
        kwargs["cheap_model"] = cheap_model
    if local_model:
        kwargs["local_model"] = local_model
    if capture_answers:
        from opendaisugi.gateway_answers import AnswerStore

        kwargs["answer_store"] = AnswerStore(path=Path(data_dir) / "gateway" / "answers.jsonl")
        kwargs["capture_answers"] = True
    return Gateway(**kwargs)
```

Also add `ExternalRouterConfig` to the `TYPE_CHECKING` import block at the top of
gateway_asgi.py (next to the existing `Gateway`/`PreparedTurn` import):

```python
if TYPE_CHECKING:
    from opendaisugi.gateway_pipeline import ExternalRouterConfig, Gateway, PreparedTurn
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_gateway_upstream.py tests/test_gateway_asgi.py -q`
Expected: PASS (the pre-existing `test_gateway_asgi.py` suite is untouched — every new
parameter is optional with a backward-compatible default)

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gateway_asgi.py tests/test_gateway_upstream.py
git commit -m "gateway: meter external-mode turns by the served model, not the route id

route_target comes off the response body's model field (streaming and
buffered). downgraded compares it against Switchyard's own configured
capable/efficient targets, never against the harness's request string --
so a provider-qualified target id never over-reports a saving it can't
attribute. An unrecognized served model safely books zero saving."
```

---

### Task 7: Per-target share table in the report

**Files:**
- Modify: `src/opendaisugi/gateway_report.py`
- Create: `tests/test_gateway_report_switchyard.py`

**Interfaces:**
- Consumes: `GatewayTurnRecord` (existing, `opendaisugi.gateway_journal`) — specifically its
  `.tier`, `.model`, `.input_tokens`, `.output_tokens` fields.
- Produces: `TargetShare` (frozen dataclass: `model: str`, `turns: int`, `share: float`,
  `input_tokens: int`, `output_tokens: int`), `build_target_share_table(records:
  Iterable[GatewayTurnRecord]) -> list[TargetShare]`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_gateway_report_switchyard.py
"""spec-09: the per-target share table for external-mode (Switchyard) turns.

Deliberately has no pass-rate column: `grep -rn "rationale" src/opendaisugi/` finds only
prose fields on envelopes/plans, never a turn-level outcome, so there is no honest source
for one. The project's own rule ("no benchmark claims a number that its script cannot
reproduce") means the column is omitted, not filled with a fake 100%.
"""

from __future__ import annotations

import pytest

from opendaisugi.gateway_journal import GatewayTurnRecord
from opendaisugi.gateway_report import TargetShare, build_target_share_table


def _rec(
    model: str, tier: str = "tier-switchyard", input_tokens: int = 100, output_tokens: int = 50
):
    return GatewayTurnRecord(
        created_at="2026-09-08T00:00:00Z",
        signature="sig",
        task="t",
        tier=tier,
        requested_model="claude-sonnet-5",
        model=model,
        difficulty=0.0,
        downgraded=(tier == "tier-switchyard"),
        estimated=False,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        frontier_tokens_saved=0,
        actual_dollars=0.0,
        counterfactual_dollars=0.0,
    )


def test_share_table_sums_to_100_percent():
    records = [_rec("claude-sonnet-5"), _rec("qwen3-coder:30b"), _rec("qwen3-coder:30b")]
    table = build_target_share_table(records)
    assert sum(row.share for row in table) == pytest.approx(1.0)
    assert {row.model: row.turns for row in table} == {"claude-sonnet-5": 1, "qwen3-coder:30b": 2}


def test_share_table_ignores_rules_mode_turns():
    records = [_rec("claude-haiku-4-5", tier="tier1-cheap"), _rec("qwen3-coder:30b")]
    table = build_target_share_table(records)
    assert len(table) == 1
    assert table[0].model == "qwen3-coder:30b"


def test_share_table_is_empty_with_no_switchyard_turns():
    assert build_target_share_table([_rec("claude-haiku-4-5", tier="tier1-cheap")]) == []


def test_share_table_reports_token_totals_per_target():
    records = [
        _rec("qwen3-coder:30b", input_tokens=100, output_tokens=20),
        _rec("qwen3-coder:30b", input_tokens=200, output_tokens=30),
    ]
    table = build_target_share_table(records)
    assert table[0].input_tokens == 300
    assert table[0].output_tokens == 50


def test_share_table_omits_pass_rate_when_no_outcome_source_exists():
    table = build_target_share_table([_rec("qwen3-coder:30b")])
    assert set(TargetShare.__dataclass_fields__) == {
        "model",
        "turns",
        "share",
        "input_tokens",
        "output_tokens",
    }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_gateway_report_switchyard.py -q`
Expected: FAIL — `ImportError: cannot import name 'TargetShare'`

- [ ] **Step 3: Write the implementation**

```python
# add to src/opendaisugi/gateway_report.py, after CalibrationReport/build_report
@dataclass(frozen=True)
class TargetShare:
    """One Switchyard target's share of external-mode ("tier-switchyard") turns."""

    model: str
    turns: int
    share: float  # turns / total tier-switchyard turns, in [0, 1]
    input_tokens: int
    output_tokens: int


def build_target_share_table(records: Iterable[GatewayTurnRecord]) -> list["TargetShare"]:
    """Group external-mode (Switchyard) turns by served model and report each one's share.

    Empty when no tier-switchyard turns exist. Carries NO pass-rate column: that would
    need a per-turn outcome signal, and no such ledger exists in this codebase yet
    (`grep -rn "rationale" src/opendaisugi/` finds prose fields, never an outcome) — per
    the project's own rule against unreproducible numbers, the column is omitted rather
    than filled with a fabricated 100%.
    """
    switchyard = [r for r in records if r.tier == "tier-switchyard"]
    total = len(switchyard)
    if total == 0:
        return []
    by_model: dict[str, list[GatewayTurnRecord]] = {}
    for r in switchyard:
        by_model.setdefault(r.model, []).append(r)
    return [
        TargetShare(
            model=model,
            turns=len(rows),
            share=len(rows) / total,
            input_tokens=sum(r.input_tokens for r in rows),
            output_tokens=sum(r.output_tokens for r in rows),
        )
        for model, rows in sorted(by_model.items(), key=lambda kv: -len(kv[1]))
    ]
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_gateway_report_switchyard.py -q`
Expected: PASS (5 tests)

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gateway_report.py tests/test_gateway_report_switchyard.py
git commit -m "gateway report: add a per-target share table for external-mode turns

No pass-rate column -- there is no outcome ledger to source one from honestly,
and this project never fills a gap with a fabricated number."
```

---

### Task 8: CLI, install, the honesty tag, and the how-to

**Files:**
- Modify: `src/opendaisugi/cli.py:2311-2403` (`gateway_cmd`), new `router_app` +
  `router_status_cmd` (near the `gate_app` block, `:307-313`), `:3536-3575`
  (`install_cmd` options), `:3719-3736` (after the `allow_shell_decomposition` persistence
  block)
- Modify: `src/opendaisugi/modules.py:223-235`
- Create: `tests/test_cli_router_switchyard.py`
- Modify: `tests/test_modules.py`
- Create: `docs/how-to/router-switchyard.md`

**Interfaces:**
- Consumes: `check_prerequisite`, `locate_binary`, `binary_version`, `probe_health`,
  `start_switchyard`, `stop_switchyard`, `render_switchyard_toml`, `write_switchyard_config`,
  `targets_from_config`, `INSTALL_CMD` (Tasks 1-4); `ExternalRouterConfig`,
  `build_default_gateway(..., router_mode, external)` (Tasks 5-6); `GatewayJournal.load`
  (existing).
- Produces: `daisugi gateway --router {rules,switchyard,off}`, `daisugi router status
  [--json]`, `daisugi install --router {rules,switchyard,off} --efficient-model M
  --capable-model M`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_cli_router_switchyard.py
from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import load_config
from opendaisugi.modules import ACTIVE, AVAILABLE, POSSIBLE, detect_stages

runner = CliRunner()


def _cli_output(result) -> str:
    combined = result.output.lower()
    try:
        combined += (result.stderr or "").lower()
    except ValueError:
        pass
    return combined


def test_cli_install_router_switchyard_writes_config_and_toml(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(
        app,
        [
            "install",
            "--gateway",
            "--router",
            "switchyard",
            "--efficient-model",
            "qwen3-coder:30b",
            "--yes",
            "--runtime",
            "claude",
        ],
    )
    assert result.exit_code == 0, _cli_output(result)
    cfg_path = tmp_path / ".opendaisugi" / "config.yaml"
    toml_path = tmp_path / ".opendaisugi" / "switchyard.toml"
    assert cfg_path.exists()
    assert toml_path.exists()
    cfg = load_config(cfg_path)
    assert cfg.gateway_router == "switchyard"
    assert cfg.switchyard_efficient_model == "qwen3-coder:30b"


def test_cli_install_router_switchyard_without_efficient_model_errors(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(
        app,
        ["install", "--gateway", "--router", "switchyard", "--yes", "--runtime", "claude"],
    )
    assert result.exit_code == 1
    assert "--efficient-model" in _cli_output(result)


def test_cli_install_router_off_persists_the_choice(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(
        app,
        ["install", "--gateway", "--router", "off", "--yes", "--runtime", "claude"],
    )
    assert result.exit_code == 0
    cfg = load_config(tmp_path / ".opendaisugi" / "config.yaml")
    assert cfg.gateway_router == "off"


def test_cli_gateway_router_switchyard_reports_the_missing_binary(monkeypatch):
    monkeypatch.setattr("opendaisugi.router_switchyard.locate_binary", lambda **_: None)
    result = runner.invoke(app, ["gateway", "--router", "switchyard"])
    assert result.exit_code == 3
    assert "cargo install --locked switchyard-server" in _cli_output(result)


def test_cli_router_status_reports_missing_binary(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.router_switchyard.locate_binary", lambda **_: None)
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert "not found" in _cli_output(result)


def test_cli_router_status_json_shape(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.router_switchyard.locate_binary", lambda **_: None)
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    payload = json.loads(result.output)
    assert set(payload) == {"router", "binary", "version", "config_path", "healthy", "recent_turns"}


def test_router_stage_shows_switchyard_available_when_binary_present(tmp_path, monkeypatch):
    monkeypatch.setattr(
        "opendaisugi.modules.locate_binary", lambda **_: "/usr/local/bin/switchyard-server"
    )
    stages = detect_stages(tmp_path)
    router = next(s for s in stages if s.key == "router")
    sw = next(m for m in router.modules if m.name == "NeMo Switchyard")
    assert sw.state == AVAILABLE


def test_router_stage_shows_switchyard_possible_without_the_binary(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.modules.locate_binary", lambda **_: None)
    stages = detect_stages(tmp_path)
    router = next(s for s in stages if s.key == "router")
    sw = next(m for m in router.modules if m.name == "NeMo Switchyard")
    assert sw.state == POSSIBLE


def test_router_stage_shows_switchyard_active_when_selected_and_present(tmp_path, monkeypatch):
    from opendaisugi.config import load_config, save_config

    monkeypatch.setattr(
        "opendaisugi.modules.locate_binary", lambda **_: "/usr/local/bin/switchyard-server"
    )
    cfg_path = tmp_path / "config.yaml"
    save_config(load_config(cfg_path).model_copy(update={"gateway_router": "switchyard"}), cfg_path)
    stages = detect_stages(tmp_path)
    router = next(s for s in stages if s.key == "router")
    sw = next(m for m in router.modules if m.name == "NeMo Switchyard")
    assert sw.state == ACTIVE
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_cli_router_switchyard.py -q`
Expected: FAIL — `--router` is not a recognized option on `install`/`gateway`; `router` is
not a registered command; `modules.py` has no `locate_binary` name to patch.

- [ ] **Step 3: Write the implementation**

`modules.py:223-235` — flip the "NeMo Switchyard" row from a hardcoded `POSSIBLE`:

```python
# src/opendaisugi/modules.py:19 — a MODULE-LEVEL import, not lazy inside detect_stages.
# router_switchyard.py is stdlib-only (no heavy deps to defer), and detect_stages's
# tests patch "opendaisugi.modules.locate_binary" — that only rebinds a name that
# lives in this module's own namespace, which a function-local `from ... import`
# does not (it re-imports fresh from router_switchyard every call, ignoring a patch
# on modules.locate_binary). Add this next to "from dataclasses import ...":
from opendaisugi.router_switchyard import INSTALL_CMD, locate_binary
```

```python
# src/opendaisugi/modules.py — inside detect_stages, compute switchyard_present /
# switchyard_active alongside matcher_cfg (next to "matcher_sel = matcher_cfg.matcher_model")
    switchyard_present = locate_binary() is not None
    switchyard_active = matcher_cfg.gateway_router == "switchyard" and switchyard_present
```

```python
# src/opendaisugi/modules.py:229-234 — the router stage's module list
                Module(
                    "daisugi gateway",
                    ACTIVE if gateway_on else POSSIBLE,
                    "base_url wired" if gateway_on else "daisugi install --gateway",
                ),
                Module(
                    "NeMo Switchyard",
                    ACTIVE
                    if switchyard_active
                    else (AVAILABLE if switchyard_present else POSSIBLE),
                    (
                        "active — daisugi router status"
                        if switchyard_active
                        else "binary found — daisugi install --gateway --router switchyard "
                        "--efficient-model <id>"
                        if switchyard_present
                        else f"needs the binary — {INSTALL_CMD}"
                    ),
                ),
                Module("off (direct)", AVAILABLE, "default; start routing with `daisugi gateway`"),
```

`cli.py` — the `router` sub-app, next to `gate_app` (`:307-313`):

```python
router_app = typer.Typer(
    name="router",
    help="The gateway's model chooser (spec-09): NeMo Switchyard as a managed child, "
    "or the built-in rules router.",
    no_args_is_help=True,
)
app.add_typer(router_app, name="router", hidden=True)
```

`cli.py` — `router status`:

```python
@router_app.command("status")
def router_status_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show the Switchyard binary, config, health, and the last 10 external-mode turns.

    Reads the gateway's own turn journal for "recent turns" (master spec §5.1: the
    authority we produced ourselves beats an inference) rather than parsing Switchyard's
    --routing-log-file, which this version does not wire up.
    """
    from opendaisugi.config import load_config
    from opendaisugi.gateway_journal import GatewayJournal
    from opendaisugi.router_switchyard import (
        DEFAULT_HOST,
        DEFAULT_PORT,
        INSTALL_CMD,
        probe_health,
        binary_version,
        locate_binary,
    )

    cfg = load_config(data_dir / "config.yaml")
    binary = locate_binary()
    config_path = data_dir / "switchyard.toml"
    version = binary_version() if binary else None
    healthy = probe_health(DEFAULT_HOST, DEFAULT_PORT) if binary else False
    records = GatewayJournal(path=data_dir / "gateway" / "turns.jsonl").load()
    recent = [r for r in records if r.tier == "tier-switchyard"][-10:]

    if json_output:
        typer.echo(
            json.dumps(
                {
                    "router": cfg.gateway_router,
                    "binary": binary,
                    "version": version,
                    "config_path": str(config_path) if config_path.exists() else None,
                    "healthy": healthy,
                    "recent_turns": [
                        {"task": r.task, "model": r.model, "downgraded": r.downgraded}
                        for r in recent
                    ],
                }
            )
        )
        return

    typer.echo(f"router: {cfg.gateway_router}")
    typer.echo(f"  binary:  {binary or 'not found — ' + INSTALL_CMD}")
    if version:
        typer.echo(f"  version: {version}")
    typer.echo(f"  config:  {config_path if config_path.exists() else '(none written yet)'}")
    typer.echo(f"  health:  {'ok' if healthy else 'unreachable'}")
    typer.echo(f"  recent turns ({len(recent)}):")
    for r in recent:
        label = " ".join(r.task.split())[:60]
        typer.echo(f"    {label!r:62}  ->  {r.model}  (downgraded={r.downgraded})")
```

`cli.py:2311-2403` — extend `gateway_cmd` with `--router`:

```python
# add to gateway_cmd's parameter list, after openai_cheap_model
    router: str = typer.Option(
        "rules",
        "--router",
        help="Model chooser behind the gateway: rules (default, the no-dependency "
        "route_turn heuristic), switchyard (start/use NeMo Switchyard as a managed "
        "child; --upstream is overridden with its address), or off (pure metered "
        "passthrough, no local rewriting).",
    ),
    switchyard_config: Path = typer.Option(
        None,
        "--switchyard-config",
        help="Switchyard TOML path (default: <data-dir>/switchyard.toml).",
    ),
    switchyard_port: int = typer.Option(
        4000, "--switchyard-port", help="Port for the managed switchyard-server child."
    ),
```

```python
# gateway_cmd's body, replacing the block from
#     "if local_model is None: ..." through "serve_gateway(...)"
    from opendaisugi.config import load_config
    from opendaisugi.gateway_asgi import serve_gateway

    if local_model is None:
        local_model = load_config().gateway_local_model

    if router not in ("rules", "switchyard", "off"):
        typer.echo(f"--router must be one of: rules, switchyard, off (got {router!r})", err=True)
        raise typer.Exit(code=1)

    router_mode = "rules" if router == "rules" else ("off" if router == "off" else "external")
    external = None
    stop_switchyard_child = None

    if router == "switchyard":
        from opendaisugi.gateway_pipeline import ExternalRouterConfig
        from opendaisugi.router_switchyard import (
            check_prerequisite,
            render_switchyard_toml,
            start_switchyard,
            stop_switchyard,
            targets_from_config,
            write_switchyard_config,
        )

        problem = check_prerequisite()
        if problem:
            typer.echo(problem, err=True)
            raise typer.Exit(code=3)
        cfg = load_config(data_dir / "config.yaml")
        targets = targets_from_config(cfg)
        if targets is None:
            typer.echo(
                "no efficient model configured for Switchyard yet. Run `daisugi "
                "install --gateway --router switchyard --efficient-model <id>` first.",
                err=True,
            )
            raise typer.Exit(code=1)
        config_path = switchyard_config or (data_dir / "switchyard.toml")
        write_switchyard_config(data_dir, render_switchyard_toml(targets, route_id=cfg.switchyard_route_id))
        pid_path = data_dir / "gateway" / "switchyard.pid"
        handle, msg = start_switchyard(config_path, port=switchyard_port, pid_path=pid_path)
        typer.echo(f"  switchyard: {msg}")
        if handle is None:
            raise typer.Exit(code=3)
        upstream = handle.base_url
        external = ExternalRouterConfig(
            route_id=cfg.switchyard_route_id,
            capable_target=targets.capable_id,
            efficient_target=targets.efficient_id,
        )
        stop_switchyard_child = lambda: typer.echo(f"  switchyard: {stop_switchyard(pid_path)}")

    typer.echo(f"opendaisugi gateway  →  {upstream}")
    typer.echo(f"  router: {router}")
    if local_model:
        typer.echo(f"  local rung: easy turns → {local_model}")
    typer.echo(f"  listening on http://{host}:{port}  (journal: {data_dir}/gateway/turns.jsonl)")
    typer.echo(f"  point your harness at it:  ANTHROPIC_BASE_URL=http://{host}:{port}")
    if openai_cheap_model:
        typer.echo(
            f"  OpenAI wire: …/chat/completions → {openai_upstream} "
            f"(easy turns → {openai_cheap_model})"
        )
    if capture_answers:
        typer.echo(f"  capturing answers to:      {data_dir}/gateway/answers.jsonl")
    try:
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
            router_mode=router_mode,
            external=external,
        )
    finally:
        if stop_switchyard_child is not None:
            stop_switchyard_child()
```

Note: `serve_gateway` (gateway_asgi.py, modified in Task 6's `build_default_gateway`) also
needs `router_mode`/`external` keywords forwarded to its internal `build_default_gateway`
call when `gateway is None` — add them to `serve_gateway`'s own signature and pass through:

```python
# src/opendaisugi/gateway_asgi.py — serve_gateway gains two keywords
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
    router_mode: str = "rules",
    external: "ExternalRouterConfig | None" = None,
) -> None:
    ...
    resolved_dir = data_dir or (Path.home() / ".opendaisugi")
    if gateway is None:
        gateway = build_default_gateway(
            data_dir=resolved_dir,
            cheap_model=cheap_model,
            capture_answers=capture_answers,
            local_model=local_model,
            router_mode=router_mode,
            external=external,
        )
    # openai_gateway is intentionally unaffected -- Codex/OpenAI-wire routing through
    # Switchyard is out of scope for spec-09; it keeps the existing rules router.
    openai_gateway = (
        build_default_gateway(
            data_dir=resolved_dir,
            cheap_model=openai_cheap_model,
            capture_answers=capture_answers,
            local_model=local_model,
        )
        if openai_cheap_model
        else None
    )
    ...
```

`install_cmd` — add `--router`/`--efficient-model`/`--capable-model` to the parameter list
(`:3568`, after `gateway`):

```python
router: str = (
    typer.Option(
        "rules",
        "--router",
        help="rules (default) | switchyard | off — the gateway's model chooser "
        "(spec-09). Only meaningful together with --gateway.",
    ),
)
efficient_model: str = (
    typer.Option(
        None,
        "--efficient-model",
        help="Model id for Switchyard's efficient tier (a local model id, or a cheap "
        "cloud model). Required with --router switchyard.",
    ),
)
capable_model: str = (
    typer.Option(
        "claude-sonnet-5",
        "--capable-model",
        help="Model id for Switchyard's capable tier — should match what your "
        "harness sends as `model` today.",
    ),
)
```

And in the body, right after the existing `allow_shell_decomposition` persistence block
(`:3719-3736`, the block ending `_warn_if_decomposition_unusable(allow_shell_decomposition)`):

```python
    # spec-09: persist the router choice the same way — config is the only channel a
    # detached `daisugi gateway` invocation reads later.
    if router == "switchyard":
        if not gateway:
            typer.echo("Note: --router only applies with --gateway; no router config was written.")
        elif not efficient_model:
            typer.echo(
                "Error: --router switchyard needs --efficient-model <id> (a local model "
                "id, or a cheap cloud model).",
                err=True,
            )
            raise typer.Exit(code=1)
        else:
            from opendaisugi.config import load_config, save_config
            from opendaisugi.router_switchyard import (
                render_switchyard_toml,
                targets_from_config,
                write_switchyard_config,
            )

            cfg_path = home / ".opendaisugi" / "config.yaml"
            cfg = load_config(cfg_path).model_copy(
                update={
                    "gateway_router": "switchyard",
                    "switchyard_capable_model": capable_model,
                    "switchyard_efficient_model": efficient_model,
                }
            )
            save_config(cfg, cfg_path)
            targets = targets_from_config(cfg)
            toml_path = write_switchyard_config(
                home / ".opendaisugi", render_switchyard_toml(targets, route_id=cfg.switchyard_route_id)
            )
            typer.echo(
                f"Switchyard router: wrote {toml_path} and set gateway_router=switchyard in "
                f"{cfg_path}. Start it with `daisugi gateway --router switchyard`."
            )
    elif router == "off" and gateway:
        from opendaisugi.config import load_config, save_config

        cfg_path = home / ".opendaisugi" / "config.yaml"
        save_config(load_config(cfg_path).model_copy(update={"gateway_router": "off"}), cfg_path)
        typer.echo(f"Gateway router set to off in {cfg_path} (pure metered passthrough).")
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_cli_router_switchyard.py tests/test_modules.py -q`
Expected: PASS

- [ ] **Step 5: Write the how-to doc**

```markdown
<!-- docs/how-to/router-switchyard.md -->
# Hand model choice to NeMo Switchyard, keep the gateway as the meter

*How to run [NVIDIA NeMo Switchyard](https://github.com/NVIDIA-NeMo/Switchyard) behind the
token-saving gateway, so Switchyard's stage router picks the model and the gateway still
journals every turn honestly.*

## What class of thing this is, stated up front

- **Switchyard is a chooser. The gateway is the meter.** ([master spec §5.7](../plans/2026-09-08-workshop/00-master-spec.md))
  A chooser we do not own must sit where we can measure it — behind the gateway, not in
  front of it — or a saving can never be attributed to the choice that produced it.
- **This is opt-in.** A plain `daisugi gateway` keeps the built-in rules router (the
  no-dependency default). Nothing here changes that install.
- **The standalone proxy is a Rust binary, not a pip package.** `pip install
  nemo-switchyard` installs a *different* thing (the embeddable Python routing library).
  Install the server with cargo:

  ```bash
  cargo install --locked switchyard-server   # needs a Rust toolchain
  ```

## Turn it on

```bash
daisugi install --gateway --router switchyard --efficient-model qwen3-coder:30b
daisugi gateway --router switchyard
```

The first command writes `~/.opendaisugi/switchyard.toml` (a two-tier `stage_router`: your
capable model, default `claude-sonnet-5`, against the efficient model you named) and sets
`gateway_router: switchyard` in `~/.opendaisugi/config.yaml`. The second starts
`switchyard-server` as a managed child, waits for `GET /health`, and points the gateway's
upstream at it. `daisugi router status` shows the binary, its version, the config path,
health, and the last 10 external-mode turns.

`--efficient-model` can be a local model already qualified for `gateway_local_model` (then
Switchyard's efficient client points at your local server, no API key) or a cheap cloud
model (then it's an Anthropic client with `ANTHROPIC_API_KEY`, if set).

## What "downgraded" means here — and its one honest limitation

The gateway compares the model Switchyard actually served against the **capable/efficient
target ids you configured**, never against the harness's own request string. That is
deliberate: a provider-qualified id (`anthropic/claude-sonnet-4.5` via some router) almost
never equals the plain string Claude Code sent (`claude-sonnet-5`), so a naive string-diff
would call *every* turn a downgrade. Comparing against the configured targets instead means:

- Served the **capable** target → `downgraded: false`, no saving booked (correct — you
  paid the same as your requested model would have).
- Served the **efficient** target → `downgraded: true`, a real saving, priced from whatever
  you set as that target's price (default `$0/$0` for a local model).
- Served anything else (including — if this documentation's one unverified assumption turns
  out wrong — Switchyard echoing the *route* id back instead of the target it chose) →
  `downgraded: false`. **This never fabricates a saving it can't attribute.** It can
  undersell (report zero when a real saving happened), never oversell.

If `daisugi router status` shows zero downgraded turns despite Switchyard clearly picking
the efficient tier (check with `curl http://127.0.0.1:4000/v1/stats`), your Switchyard build
is echoing the route id, not the target id, in its response. That is a known, documented gap
in this version — the fix is reading Switchyard's `--routing-log-file` JSONL output instead,
which names the served model per completed turn; this version starts the child with that
flag wired into the launch argv but does not yet parse the file.

## Dollars are only as real as the prices you give it

Tokens saved are exact (they come straight from the provider's own usage report). Dollars
are only meaningful once you tell the gateway what your efficient target actually costs —
`$0/$0` for a model you're already running locally, or a real per-million-token rate for a
paid one. Without that, the fallback rate applies and the multiplier reads close to 1.0x:
safe, but not informative. Tokens are the headline metric for exactly this reason.

## Turn it off

```bash
daisugi install --gateway --router off     # pure metered passthrough, no chooser at all
daisugi install --gateway --router rules   # back to the built-in heuristic (the default)
```

`daisugi gateway --router switchyard` stops the managed `switchyard-server` child on exit
(Ctrl+C), sending it `SIGTERM` — Switchyard drains active requests for up to 30 seconds
before it exits.

## Out of scope in this version

Codex and every other OpenAI-wire harness keep the gateway's existing rules router
unconditionally — routing that wire through Switchyard is a follow-on. Training Switchyard's
own prefill/classifier routers, and running it as a NeMo Relay plugin, are also out of scope
here; see [Switchyard's own docs](https://github.com/NVIDIA-NeMo/Switchyard) for those paths.
```

- [ ] **Step 6: Run the full test suite and lint**

Run: `uv run --no-sync pytest -q`
Expected: PASS, no regressions

Run: `uv run --no-sync ruff check .`
Expected: clean

- [ ] **Step 7: Commit**

```bash
git add src/opendaisugi/cli.py src/opendaisugi/gateway_asgi.py src/opendaisugi/modules.py \
  tests/test_cli_router_switchyard.py tests/test_modules.py docs/how-to/router-switchyard.md
git commit -m "gateway: wire NeMo Switchyard into the CLI, install, and the module map

'daisugi gateway --router switchyard' starts/stops the managed child around
serve_gateway; 'daisugi router status' shows binary/version/health/recent
turns from the gateway's own journal; 'daisugi install --router switchyard
--efficient-model M' persists the choice and writes the TOML. The router
stage's NeMo Switchyard row now reports its real state instead of a
hardcoded POSSIBLE. Codex/OpenAI-wire routing through Switchyard stays out
of scope -- that gateway instance keeps the unconditional rules router."
```
