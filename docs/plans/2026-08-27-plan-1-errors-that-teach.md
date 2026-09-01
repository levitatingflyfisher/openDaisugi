# Plan 1: Errors That Teach (W9 finish + W6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every failure daisugi shows a human states what was tried, why it failed, and the one next action; deterministic errors are never retried; the resolved state is visible.

**Architecture:** Pre-flight checks in `llm.py` stop dead calls before instructor runs. Exceptions join the `OpenDaisugiError` hierarchy so one wrapper in `cli.main()` renders them. A `config` command and a one-line state echo make hidden state visible.

**Tech Stack:** Python 3.12, Typer 0.23, pydantic, PyYAML, pytest with `CliRunner`.

**Spec:** `docs/plans/2026-08-27-cockpit-spec.md` §4, §9, and §11 (cross-plan rules).

**Requires:** nothing (this is the first plan). **Provides to later plans:**
`config.installed_hook_mode`, the `config` command, `_echo_resolved`, `_fail`,
`LLMNotConfigured`, `NoStepsError`, `cli.main()`.

## Corrections from adversarial review (2026-08-27 — authoritative over the tasks below)

Design and fail-closed behavior verified sound (`main()` catches only `OpenDaisugiError`,
so `run`'s deny/exit-2 and `verify`'s reject/exit-1 pass through; `NoStepsError→exit 0`
provably cannot mask an LLM failure — it fires only *after* a successful completion). Apply
these before marking a task done:

- **BLOCKER — Task 7 tests target the wrong command shape (un-buildable).** `run`/`verify`
  take the envelope as `--envelope`, not a positional, and `plan_path` is
  `typer.Argument(exists=True)`, so a missing file is rejected by Typer before the body runs
  — the `except FileNotFoundError: _fail(...)` is unreachable. Retarget both tests at a
  *parse* failure on an existing-but-invalid YAML file (reachable in the body; keeps
  `exists=True` and `--envelope`), invoked as `["verify", str(plan), "--envelope", str(env)]`.
- **BLOCKER — Task 7 `test_tend_without_embedder_teaches` is un-implementable.** `tend_cmd`
  has zero `echo(err=True)`-then-`raise Exit` patterns for the audit rule to match; an empty
  `tmp_path` has no traces so the embedder never loads and `tend` exits 0; the deep
  `ImportError` gives exit 1 + traceback, not exit 2 + three lines. Drop this test, or add a
  real exit-2 branch to `tend_cmd` and seed a trace so the embedder is reached.
- **BLOCKER — Task 6 `except OpenDaisugiError as e: raise` trips ruff F841 (unused `e`).**
  Write `except OpenDaisugiError:` (drop `as e`; keep the comment). The sibling
  `except Exception as e:` is fine — it uses `e`.
- **SHOULD-FIX — Task 2 `preflight` blast radius.** It installs in `get_instructor_client`,
  the choke point for nine source modules. Run the **full** suite at the end of Task 2 (per
  §11.1), and audit callers (`envelope`, `distiller`, `tier1`, `synthesizer`, `fallback`,
  `pathway_bind`, `delegating_executor`) that build a keyless Anthropic litellm client.
- **SHOULD-FIX — state the final except-branch order in Task 6 Step 3.** It must be
  `NoStepsError → EnvelopeGenerationError → OpenDaisugiError → Exception` (each specific
  subclass above its parent), or a branch goes silently unreachable.
- **SHOULD-FIX — `LowStakesNotConfigured` is a `ValueError`, not an `OpenDaisugiError`**, so
  `main()` won't render it and Task 6's broad `except` mislabels a user config error as a
  bug. Either reparent it under `OpenDaisugiError` or catch it explicitly with a config
  message.
- **SHOULD-FIX — route non-`NoSteps` `DecompositionError` (DAG/LLM-call failures) through
  `_fail`** in `orchestrate_cmd`, so it teaches three lines instead of one.
- **DECISION — keep the plan's richer `preflight` signature** (Anthropic-only narrowing);
  spec §9/§4.1 are corrected to match (§11.2). NIT: console-script is at
  `pyproject.toml:90-91`, not `:78-79`.

## Global Constraints

- `uv run pytest -q` green and `uv run ruff check .` clean before each commit (line length 100).
- Commit messages: `type(scope): why`. No attribution trailers. Never push.
- Error text: three lines, in order: what was attempted, why it failed, the next action. Plain words, short sentences.
- Do not add symbols named `Session`, `SessionRecord`, or `list_sessions`.
- Tests that run the CLI use `typer.testing.CliRunner` on `opendaisugi.cli.app`, except the one subprocess test in Task 6.
- `tests/conftest.py:36-61` pins `OPENDAISUGI_LLM_BACKEND=litellm` for every test; pass `backend=` explicitly when a test needs another.

---

### Task 1: Flatten instructor's retry XML into one line

**Files:**
- Modify: `src/opendaisugi/llm.py:37-51` (`translate_llm_error`)
- Test: `tests/test_llm_errors.py` (new)

**Interfaces:**
- Consumes: `opendaisugi.exceptions.EnvelopeGenerationError`, `llm._redact_keys`.
- Produces: `translate_llm_error(exc) -> EnvelopeGenerationError` whose message is one line for retry exceptions.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_llm_errors.py
"""LLM error translation: one plain line, never instructor's XML dump."""

from __future__ import annotations

from opendaisugi.exceptions import EnvelopeGenerationError
from opendaisugi.llm import translate_llm_error


class _Attempt:
    def __init__(self, n: int, exc: BaseException) -> None:
        self.attempt_number = n
        self.exception = exc
        self.completion = None


class _RetryExc(Exception):
    """Shape of instructor.core.exceptions.InstructorRetryException."""

    def __init__(self, attempts: list[_Attempt]) -> None:
        super().__init__('<failed_attempts>\n<generation number="1">...</failed_attempts>')
        self.failed_attempts = attempts


def test_retry_exception_becomes_one_line():
    exc = _RetryExc([_Attempt(1, ValueError("bad json")), _Attempt(2, ValueError("bad json"))])
    out = translate_llm_error(exc)
    assert isinstance(out, EnvelopeGenerationError)
    text = str(out)
    assert "<failed_attempts>" not in text
    assert "ValueError: bad json" in text
    assert "(2 attempts)" in text
    assert "\n" not in text


def test_plain_exception_keeps_its_message():
    assert str(translate_llm_error(RuntimeError("boom"))) == "boom"


def test_retry_exception_with_multiline_inner_keeps_first_line():
    exc = _RetryExc([_Attempt(1, ValueError("line one\nline two"))])
    assert str(translate_llm_error(exc)) == "ValueError: line one (1 attempts)"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_llm_errors.py -q`
Expected: FAIL on `test_retry_exception_becomes_one_line` (the XML is still in the message).

- [ ] **Step 3: Write minimal implementation**

In `src/opendaisugi/llm.py`, add above `translate_llm_error`:

```python
def _flatten_retry(exc: BaseException) -> str | None:
    """One line for an instructor retry exception, or None if ``exc`` is not one.

    instructor renders ``failed_attempts`` as a multi-line XML template. The
    operator needs the first attempt's cause and the count, nothing else.
    """
    attempts = getattr(exc, "failed_attempts", None)
    if not attempts:
        return None
    inner = getattr(attempts[0], "exception", None)
    if inner is None:
        return None
    lines = [ln for ln in str(inner).strip().splitlines() if ln.strip()]
    first = lines[0].strip() if lines else inner.__class__.__name__
    return f"{inner.__class__.__name__}: {first} ({len(attempts)} attempts)"
```

Replace the body of `translate_llm_error` so it reads:

```python
    if isinstance(exc, EnvelopeGenerationError):
        return exc
    flat = _flatten_retry(exc)
    message = flat or (str(exc) or exc.__class__.__name__)
    return EnvelopeGenerationError(_redact_keys(message))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_llm_errors.py tests/test_llm_backend.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/llm.py tests/test_llm_errors.py
git commit -m "fix(llm): show one line for a retried LLM call, not instructor's XML"
```

---

### Task 2: Pre-flight the backend so dead calls are never made

**Files:**
- Modify: `src/opendaisugi/exceptions.py` (add `LLMNotConfigured`)
- Modify: `src/opendaisugi/llm.py` (add `preflight`; call it in `get_instructor_client`)
- Test: `tests/test_llm_errors.py` (extend)

**Interfaces:**
- Consumes: `resolve_backend(backend)` (`llm.py:53`), `shutil.which`.
- Produces: `preflight(backend: str | None = None, *, model: str | None = None, env=None, which=shutil.which) -> str` returning the resolved backend, raising `LLMNotConfigured`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_llm_errors.py`:

```python
import pytest

from opendaisugi.exceptions import LLMNotConfigured, OpenDaisugiError
from opendaisugi.llm import preflight


def test_litellm_anthropic_model_without_key_fails_with_the_fix(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    with pytest.raises(LLMNotConfigured) as ei:
        preflight("litellm", model="anthropic/claude-sonnet-4-20250514")
    text = str(ei.value)
    assert text.count("\n") == 2, text  # what / why / next action
    assert "ANTHROPIC_API_KEY" in text
    assert "--llm claude-code" in text
    assert isinstance(ei.value, OpenDaisugiError)


def test_litellm_other_provider_is_not_checked_here(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    assert preflight("litellm", model="openai/gpt-4o") == "litellm"


def test_litellm_with_key_passes(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")
    assert preflight("litellm", model="anthropic/x") == "litellm"


def test_claude_code_without_binary_fails_with_the_fix():
    with pytest.raises(LLMNotConfigured) as ei:
        preflight("claude-code", which=lambda _name: None)
    text = str(ei.value)
    assert text.count("\n") == 2
    assert "claude" in text and "PATH" in text
    assert "--llm litellm" in text


def test_claude_code_with_binary_passes():
    assert preflight("claude-code", which=lambda _name: "/usr/bin/claude") == "claude-code"


def test_get_instructor_client_preflights_before_importing_instructor(monkeypatch):
    import sys

    from opendaisugi import llm

    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setitem(sys.modules, "instructor", None)  # import would now fail loudly
    with pytest.raises(LLMNotConfigured):
        llm.get_instructor_client(model="anthropic/x", backend="litellm")
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_llm_errors.py -q`
Expected: FAIL with `ImportError: cannot import name 'LLMNotConfigured'`.

- [ ] **Step 3: Write minimal implementation**

In `src/opendaisugi/exceptions.py`, after `EnvelopeGenerationError`:

```python
class LLMNotConfigured(OpenDaisugiError):
    """The selected LLM backend cannot run on this machine (no key, no binary).

    Raised before any network call. Deterministic: retrying cannot help.
    """
```

In `src/opendaisugi/llm.py`, add `from opendaisugi.exceptions import LLMNotConfigured` to the
existing exceptions import, then add after `_auto_backend`:

```python
_ANTHROPIC_PREFIXES = ("anthropic/", "claude")


def preflight(
    backend: str | None = None,
    *,
    model: str | None = None,
    env: "Mapping[str, str] | None" = None,
    which=shutil.which,
) -> str:
    """Check the resolved backend can run here; raise LLMNotConfigured if not.

    Runs before any client is built so a missing key or binary is one plain
    error, never a retried network call. Only Anthropic models are checked on
    the litellm path; other providers surface their own errors.
    """
    env = os.environ if env is None else env
    resolved = resolve_backend(backend)
    if resolved == "litellm":
        anthropic = model is None or model.startswith(_ANTHROPIC_PREFIXES)
        has_key = bool(env.get("ANTHROPIC_API_KEY") or env.get("ANTHROPIC_AUTH_TOKEN"))
        if anthropic and not has_key:
            raise LLMNotConfigured(
                "Tried to call the Anthropic API through litellm.\n"
                "No ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.\n"
                "Set a key, or run with --llm claude-code to use the local claude CLI."
            )
    elif resolved == "claude-code":
        if which("claude") is None:
            raise LLMNotConfigured(
                "Tried to use the claude-code backend.\n"
                "No `claude` command is on PATH.\n"
                "Install Claude Code, or set ANTHROPIC_API_KEY and run with --llm litellm."
            )
    return resolved
```

Add `from collections.abc import Mapping` under `if TYPE_CHECKING:` (or import it plainly).
In `get_instructor_client` (`llm.py:~95`), make the first statement
`resolved = preflight(backend, model=model)` and use `resolved` where the function
currently calls `resolve_backend(backend)` (`llm.py:102`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_llm_errors.py tests/test_llm_backend.py tests/test_cli_llm_flag.py -q`
Expected: PASS. If a test in `test_llm_backend.py` builds a litellm client with no key set,
add `monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")` to that test; the old behaviour
(a dead client that fails at call time) was the defect.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/exceptions.py src/opendaisugi/llm.py tests/test_llm_errors.py tests/test_llm_backend.py
git commit -m "fix(llm): pre-flight the backend so a missing key is one error, not three retries"
```

---

### Task 3: A prompt with no steps is an answer, not a crash

**Files:**
- Modify: `src/opendaisugi/exceptions.py` (add `DecompositionError`, `NoStepsError`)
- Modify: `src/opendaisugi/decomposer.py:65` (class becomes an import) and `:218` (raise `NoStepsError`)
- Modify: `src/opendaisugi/cli.py:2418-2432` (`orchestrate` except branches)
- Test: `tests/test_decomposer.py` (extend, or `tests/test_cli_orchestrate.py`)

**Interfaces:**
- Consumes: the existing `DecompositionError(message, plan=None)` at `decomposer.py:65` (read it first; keep its `plan` attribute).
- Produces: `opendaisugi.exceptions.DecompositionError(OpenDaisugiError)` with `.plan`, `NoStepsError(DecompositionError)`. `opendaisugi.decomposer.DecompositionError` stays importable (re-export).

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_cli_orchestrate.py`:

```python
from opendaisugi.exceptions import DecompositionError, NoStepsError, OpenDaisugiError


def test_decomposition_errors_are_opendaisugi_errors():
    assert issubclass(DecompositionError, OpenDaisugiError)
    assert issubclass(NoStepsError, DecompositionError)
    from opendaisugi.decomposer import DecompositionError as FromDecomposer

    assert FromDecomposer is DecompositionError


def test_no_steps_is_a_plain_answer_not_a_crash(monkeypatch):
    async def _no_steps(self, prompt, **_kw):
        raise NoStepsError("decomposition produced no steps")

    from opendaisugi import Daisugi

    monkeypatch.setattr(Daisugi, "orchestrate", _no_steps)
    res = runner.invoke(app, ["orchestrate", "Hello how are you doing?", "--llm", "litellm"])
    assert res.exit_code == 0, res.output
    assert "no steps" in res.output.lower()
    assert "NoStepsError" not in res.output
    assert "Traceback" not in res.output


def test_no_steps_json_shape(monkeypatch):
    async def _no_steps(self, prompt, **_kw):
        raise NoStepsError("decomposition produced no steps")

    from opendaisugi import Daisugi

    monkeypatch.setattr(Daisugi, "orchestrate", _no_steps)
    res = runner.invoke(app, ["orchestrate", "hi", "--llm", "litellm", "--json"])
    assert res.exit_code == 0, res.output
    import json

    body = json.loads(res.output)
    assert body["status"] == "no-steps"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_orchestrate.py -q`
Expected: FAIL with `ImportError: cannot import name 'NoStepsError'`.

- [ ] **Step 3: Write minimal implementation**

In `src/opendaisugi/exceptions.py`:

```python
class DecompositionError(OpenDaisugiError):
    """The decomposer could not produce a valid plan.

    ``plan`` carries the assembled plan when one was built (DAG or policy
    failure) so a caller can inspect or retry; ``None`` when the LLM call itself
    failed or returned nothing.
    """

    def __init__(self, message: str, *, plan=None) -> None:
        super().__init__(message)
        self.plan = plan


class NoStepsError(DecompositionError):
    """The prompt decomposed to zero steps: there is nothing to run."""
```

In `src/opendaisugi/decomposer.py`: delete the class at line 65 and add
`from opendaisugi.exceptions import DecompositionError, NoStepsError` to the imports
(keep the name in the module namespace; `__init__.py:60` imports it from here). If the
old class had extra attributes, move them into the new class body. At line 218:

```python
    if not decomposed.steps:
        raise NoStepsError("decomposition produced no steps")
```

In `src/opendaisugi/cli.py` `orchestrate_cmd`, add before `except EnvelopeGenerationError`
(import `NoStepsError` from `opendaisugi.exceptions` inside the function):

```python
    except NoStepsError:
        if json_output:
            typer.echo(json.dumps({"status": "no-steps", "prompt": prompt}))
        else:
            typer.echo("This prompt has no steps to run.")
            typer.echo(
                "daisugi orchestrate runs multi-step tasks under a verified envelope. "
                "For a plain question, ask your agent directly."
            )
        raise typer.Exit(code=0)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_orchestrate.py tests/test_decomposer.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/exceptions.py src/opendaisugi/decomposer.py src/opendaisugi/cli.py tests/test_cli_orchestrate.py
git commit -m "fix(orchestrate): a prompt with no steps gets a plain answer and exit 0"
```

---

### Task 4: `--llm` defaults to auto, and the resolved state is echoed once

**Files:**
- Modify: `src/opendaisugi/cli.py` (`orchestrate` :2372/:2409-2412, `onboard` :1793/:1849, `generate-envelope` :1628, `journal parse` :3070; add `_echo_resolved`)
- Test: `tests/test_cli_llm_flag.py` (extend)

**Interfaces:**
- Consumes: `llm.resolve_backend()`, `gate.resolve_gate_mode(None, root=)`, `gate.is_disarmed(root)`.
- Produces: `cli._echo_resolved(data_dir: Path) -> None` printing one stderr line: `backend: <b> · gate: <shadow|enforce|disarmed> · data: <~/path>`.

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_cli_llm_flag.py` (it already has `runner` and `app`; if not, add
`from typer.testing import CliRunner`, `from opendaisugi.cli import app`, `runner = CliRunner()`):

```python
import os


def _fake_orchestrate(monkeypatch):
    from opendaisugi import Daisugi
    from tests.test_cli_orchestrate import _fake_result

    async def _ok(self, prompt, **_kw):
        return _fake_result()

    monkeypatch.setattr(Daisugi, "orchestrate", _ok)


def test_llm_flag_absent_leaves_auto_detection(monkeypatch, tmp_path):
    _fake_orchestrate(monkeypatch)
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    res = runner.invoke(app, ["orchestrate", "t", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "OPENDAISUGI_LLM_BACKEND" not in os.environ


def test_explicit_litellm_is_honoured(monkeypatch, tmp_path):
    _fake_orchestrate(monkeypatch)
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    res = runner.invoke(app, ["orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert os.environ.get("OPENDAISUGI_LLM_BACKEND") == "litellm"


def test_orchestrate_echoes_resolved_state(monkeypatch, tmp_path):
    _fake_orchestrate(monkeypatch)
    res = runner.invoke(app, ["orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "backend: litellm" in res.output
    assert "gate: shadow" in res.output
    assert "data:" in res.output
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_llm_flag.py -q`
Expected: FAIL on `test_explicit_litellm_is_honoured` (env var not set) and on the echo test.

- [ ] **Step 3: Write minimal implementation**

Add to `src/opendaisugi/cli.py` near `DEFAULT_DATA_DIR` (line 1136):

```python
def _tilde(path: Path) -> str:
    """Show a path under the home directory as ``~/...``."""
    try:
        return "~/" + str(path.resolve().relative_to(Path.home()))
    except ValueError:
        return str(path)


def _echo_resolved(data_dir: Path) -> None:
    """One stderr line that says what daisugi resolved, so no state stays hidden."""
    from opendaisugi.gate import is_disarmed, resolve_gate_mode
    from opendaisugi.llm import resolve_backend

    root = data_dir / "gate"
    gate = "disarmed" if is_disarmed(root) else resolve_gate_mode(None, root=root)
    typer.echo(f"backend: {resolve_backend()} · gate: {gate} · data: {_tilde(data_dir)}", err=True)
```

In each of the four commands, change the option to
`llm: str | None = typer.Option(None, "--llm", help="LLM backend: litellm | claude-code. Default: auto-detect.")`
and the handling to:

```python
    if llm is not None and llm not in {"litellm", "claude-code"}:
        typer.echo(f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.", err=True)
        raise typer.Exit(code=2)
    if llm is not None:
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm
    _echo_resolved(data_dir)
```

For `journal parse` use its own data dir option; if it has none, pass `DEFAULT_DATA_DIR`.
Update the docstring of `get_instructor_client` (`llm.py:88-91`) so item 3 says
"auto-detect (`_auto_backend`)" instead of "default litellm".

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_llm_flag.py tests/test_cli_orchestrate.py tests/test_cli_onboard.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py src/opendaisugi/llm.py tests/test_cli_llm_flag.py
git commit -m "feat(cli): --llm defaults to auto-detect and the resolved state is echoed once"
```

---

### Task 5: `daisugi config` shows every setting with its source

**Files:**
- Modify: `src/opendaisugi/config.py` (add `ResolvedField`, `installed_hook_mode`, `resolved_config`)
- Modify: `src/opendaisugi/cli.py` (add `config` command)
- Test: `tests/test_config_resolved.py` (new), `tests/test_cli_config.py` (new)

**Interfaces:**
- Consumes: `load_config(path)`, `Config.model_fields`, `llm.resolve_backend()`.
- Produces: `ResolvedField(key, value, source)`; `installed_hook_mode(settings_path) -> str | None`; `resolved_config(path=None, *, home=None, env=None) -> list[ResolvedField]`; `unknown_config_keys(path) -> list[str]`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_config_resolved.py
"""`daisugi config`: every setting, where it came from, nothing hidden."""

from __future__ import annotations

import json

from opendaisugi.config import (
    installed_hook_mode,
    resolved_config,
    unknown_config_keys,
)


def _by_key(fields):
    return {f.key: f for f in fields}


def test_file_values_are_marked_file_and_others_default(tmp_path):
    (tmp_path / "config.yaml").write_text("gate_mode: enforce\n")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, env={}))
    assert got["gate_mode"].value == "enforce" and got["gate_mode"].source == "file"
    assert got["z3_timeout_ms"].source == "default"


def test_missing_file_is_all_defaults(tmp_path):
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, env={}))
    assert all(f.source == "default" for f in got.values() if "(resolved)" not in f.key)


def test_backend_source_env_vs_auto(tmp_path):
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, env={}))
    assert got["llm_backend (resolved)"].source == "auto"
    got = _by_key(
        resolved_config(
            tmp_path / "config.yaml", home=tmp_path, env={"OPENDAISUGI_LLM_BACKEND": "litellm"}
        )
    )
    assert got["llm_backend (resolved)"].source == "env"
    assert got["llm_backend (resolved)"].value == "litellm"


def test_installed_hook_mode_reads_claude_settings(tmp_path):
    settings = tmp_path / ".claude" / "settings.json"
    settings.parent.mkdir()
    settings.write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "matcher": "*",
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": "/py -m opendaisugi.gate --mode enforce --root /r",
                                }
                            ],
                        }
                    ]
                }
            }
        )
    )
    assert installed_hook_mode(settings) == "enforce"
    assert installed_hook_mode(tmp_path / "nope.json") is None
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, env={}))
    assert got["gate_mode (resolved)"].value == "enforce"
    assert got["gate_mode (resolved)"].source == "hook"


def test_gate_mode_falls_back_to_file_then_default(tmp_path):
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, env={}))
    assert got["gate_mode (resolved)"].source == "default"
    (tmp_path / "config.yaml").write_text("gate_mode: enforce\n")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, env={}))
    assert got["gate_mode (resolved)"].source == "file"


def test_unknown_keys_are_reported_not_hidden(tmp_path):
    (tmp_path / "config.yaml").write_text("gate_mode: shadow\nbanana: 1\n")
    assert unknown_config_keys(tmp_path / "config.yaml") == ["banana"]
```

```python
# tests/test_cli_config.py
"""The `config` command prints resolved settings; --json gives the same as an object."""

from __future__ import annotations

import json

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_config_help():
    assert runner.invoke(app, ["config", "--help"]).exit_code == 0


def test_config_prints_sources(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    (tmp_path / ".opendaisugi" / "config.yaml").write_text("gate_mode: enforce\nbanana: 1\n")
    res = runner.invoke(app, ["config"])
    assert res.exit_code == 0, res.output
    assert "gate_mode" in res.output and "(file)" in res.output
    assert "(default)" in res.output
    assert "banana" in res.output and "ignored" in res.output


def test_config_json(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    res = runner.invoke(app, ["config", "--json"])
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["gate_mode"]["source"] == "default"
    assert "path" in body["_meta"]
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_config_resolved.py tests/test_cli_config.py -q`
Expected: FAIL with `ImportError` on `installed_hook_mode`.

- [ ] **Step 3: Write minimal implementation**

Append to `src/opendaisugi/config.py`:

```python
import json
import re
from collections.abc import Mapping
from dataclasses import dataclass

_MODE_RE = re.compile(r"--mode\s+(shadow|enforce)\b")


@dataclass(frozen=True)
class ResolvedField:
    """One setting as the running code will see it, and where it came from."""

    key: str
    value: str
    source: str  # file | default | env | auto | hook


def _read_raw(path: Path) -> dict:
    if not path.exists():
        return {}
    try:
        raw = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError:
        return {}
    return raw if isinstance(raw, dict) else {}


def unknown_config_keys(path: Path) -> list[str]:
    """Keys in the file that no Config field reads (load_config drops them silently)."""
    return sorted(k for k in _read_raw(path) if k not in Config.model_fields)


def installed_hook_mode(settings_path: Path) -> str | None:
    """The ``--mode`` the installed Claude Code gate hook passes, or None if no hook.

    The hook command is the one thing the agent cannot rewrite, so this is the
    mode that actually governs verdicts on the Claude path (gate.resolve_gate_mode).
    """
    try:
        data = json.loads(settings_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    for entry in (data.get("hooks") or {}).get("PreToolUse") or []:
        for hook in entry.get("hooks") or []:
            cmd = str(hook.get("command", ""))
            if "opendaisugi.gate" in cmd:
                m = _MODE_RE.search(cmd)
                if m:
                    return m.group(1)
    return None


def resolved_config(
    path: Path | None = None,
    *,
    home: Path | None = None,
    env: Mapping[str, str] | None = None,
) -> list[ResolvedField]:
    """Every Config field with its source, plus the two derived, load-bearing values."""
    from opendaisugi.llm import resolve_backend

    home = home or Path.home()
    env = os.environ if env is None else env
    path = path or home / ".opendaisugi" / "config.yaml"
    cfg = load_config(path)
    raw = _read_raw(path)
    out = [
        ResolvedField(key, str(getattr(cfg, key)), "file" if key in raw else "default")
        for key in Config.model_fields
    ]
    backend_env = env.get("OPENDAISUGI_LLM_BACKEND")
    out.append(
        ResolvedField(
            "llm_backend (resolved)",
            backend_env or resolve_backend(),
            "env" if backend_env else "auto",
        )
    )
    hook_mode = installed_hook_mode(home / ".claude" / "settings.json")
    if hook_mode:
        out.append(ResolvedField("gate_mode (resolved)", hook_mode, "hook"))
    else:
        out.append(
            ResolvedField(
                "gate_mode (resolved)", cfg.gate_mode, "file" if "gate_mode" in raw else "default"
            )
        )
    return out
```

Add `import os` at the top of `config.py` if absent. Note `resolve_backend` reads
`os.environ`, not `env`; for the `env` argument only the source label is derived. Keep it
that way: the value shown is what the code will use.

Add to `src/opendaisugi/cli.py` (top level, after `status`):

```python
@app.command("config")
def config_cmd(
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show every setting as daisugi will use it, and where each one came from.

    Sources: file (~/.opendaisugi/config.yaml), default, env, auto (detected),
    hook (the installed Claude Code gate hook's --mode, which always wins).
    """
    from opendaisugi.config import resolved_config, unknown_config_keys

    path = Path.home() / ".opendaisugi" / "config.yaml"
    fields = resolved_config(path)
    unknown = unknown_config_keys(path)
    if json_output:
        body = {f.key: {"value": f.value, "source": f.source} for f in fields}
        body["_meta"] = {"path": str(path), "unknown_keys_ignored": unknown}
        typer.echo(json.dumps(body, indent=2))
        return
    width = max(len(f.key) for f in fields)
    typer.echo(f"config file: {path}{'' if path.exists() else '  (not present)'}")
    for f in fields:
        typer.echo(f"  {f.key:<{width}}  {f.value}  ({f.source})")
    if unknown:
        typer.echo(f"  unknown keys ignored: {', '.join(unknown)}")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_config_resolved.py tests/test_cli_config.py tests/test_gate_mode.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/config.py src/opendaisugi/cli.py tests/test_config_resolved.py tests/test_cli_config.py
git commit -m "feat(cli): daisugi config shows every setting with its source"
```

---

### Task 6: One error renderer for the whole CLI

**Files:**
- Modify: `src/opendaisugi/cli.py:42-46` (Typer app), `:3557-3558` (`__main__`), add `main()` and `_fail()`
- Modify: `pyproject.toml:78-79` (`daisugi = "opendaisugi.cli:main"`)
- Test: `tests/test_cli_errors.py` (new)

**Interfaces:**
- Produces: `cli.main() -> None` (console entry), `cli._fail(what: str, why: str, fix: str, *, code: int = 1) -> NoReturn`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_cli_errors.py
"""Every error teaches: what was tried, why it failed, the next action. No tracebacks."""

from __future__ import annotations

import os
import subprocess
import sys

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import _fail, app

runner = CliRunner()


def test_fail_prints_three_lines_and_exits(capsys):
    import typer

    with pytest.raises(typer.Exit) as ei:
        _fail("Tried to read plan.yaml.", "The file does not exist.", "Check the path.", code=2)
    assert ei.value.exit_code == 2
    err = capsys.readouterr().err
    assert err.splitlines() == [
        "Tried to read plan.yaml.",
        "The file does not exist.",
        "Check the path.",
    ]


def test_main_renders_opendaisugi_errors_without_a_traceback(tmp_path):
    env = {
        k: v
        for k, v in os.environ.items()
        if k not in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
    }
    env["OPENDAISUGI_LLM_BACKEND"] = "litellm"
    env["HOME"] = str(tmp_path)
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "opendaisugi.cli",
            "orchestrate",
            "list three things",
            "--data-dir",
            str(tmp_path / "d"),
        ],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )
    assert proc.returncode == 1, proc.stderr
    assert "Traceback" not in proc.stderr
    assert "<failed_attempts>" not in proc.stderr
    assert "ANTHROPIC_API_KEY" in proc.stderr
    assert "--llm claude-code" in proc.stderr


def test_debug_env_shows_the_traceback(tmp_path):
    env = {
        k: v
        for k, v in os.environ.items()
        if k not in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
    }
    env.update({"OPENDAISUGI_LLM_BACKEND": "litellm", "HOME": str(tmp_path), "DAISUGI_DEBUG": "1"})
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "opendaisugi.cli",
            "orchestrate",
            "list three things",
            "--data-dir",
            str(tmp_path / "d"),
        ],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )
    assert proc.returncode != 0
    assert "Traceback" in proc.stderr
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_errors.py -q`
Expected: FAIL with `ImportError: cannot import name '_fail'`.

- [ ] **Step 3: Write minimal implementation**

In `src/opendaisugi/cli.py`:

```python
app = typer.Typer(
    name="daisugi",
    help="Runtime assurance for agent actions.",
    no_args_is_help=True,
    pretty_exceptions_enable=False,  # a human never gets a rich traceback; see main()
)


def _fail(what: str, why: str, fix: str, *, code: int = 1) -> "NoReturn":
    """Print the three-part error and exit. Most important line (the fix) last."""
    typer.echo(what, err=True)
    typer.echo(why, err=True)
    typer.echo(fix, err=True)
    raise typer.Exit(code=code)


def main() -> None:
    """Console entry: one renderer for every OpenDaisugiError.

    ``DAISUGI_DEBUG=1`` re-raises so the traceback shows. Typer's own
    handling of usage errors and Exit is untouched.
    """
    from opendaisugi.exceptions import OpenDaisugiError

    try:
        app()
    except OpenDaisugiError as exc:
        if os.environ.get("DAISUGI_DEBUG") == "1":
            raise
        typer.echo(str(exc).strip() or exc.__class__.__name__, err=True)
        raise SystemExit(1) from exc
```

Add `from typing import NoReturn` (or keep the string annotation). Replace the
`__main__` guard with `main()`. In `pyproject.toml` set
`daisugi = "opendaisugi.cli:main"`.

In `orchestrate_cmd` replace the last two `except` branches with:

```python
    except OpenDaisugiError as e:  # already a three-part message
        raise
    except Exception as e:  # noqa: BLE001 — anything else is our bug
        _fail(
            "Tried to orchestrate the prompt.",
            f"{type(e).__name__}: {e}",
            "Run again with DAISUGI_DEBUG=1 and file the traceback as a bug.",
        )
```

Keep the `EnvelopeGenerationError` branch above them but route it through `_fail`:

```python
    except EnvelopeGenerationError as e:
        _fail("Tried to generate the envelope.", str(e), "Check the backend with `daisugi config`.")
```

`LLMNotConfigured` is an `OpenDaisugiError`, so it now reaches `main()` untouched and
prints its own three lines.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_errors.py tests/test_cli_orchestrate.py -q`
Expected: PASS. Then `uv run pytest -q` for the whole suite (the entry-point change touches
`test_cli_install_help.py` and any test that imports `__main__` behaviour).

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py pyproject.toml tests/test_cli_errors.py
git commit -m "feat(cli): one error renderer; no tracebacks for humans unless DAISUGI_DEBUG=1"
```

---

### Task 7: Audit the remaining failure branches in `run`, `verify`, `tend`

**Files:**
- Modify: `src/opendaisugi/cli.py:1490-1583` (`run`), `:1684-1734` (`verify`), `:1735-1783` (`tend`)
- Test: `tests/test_cli_errors.py` (extend)

**Interfaces:**
- Consumes: `_fail` from Task 6.

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_cli_errors.py`:

```python
def _three_lines(text: str) -> bool:
    lines = [ln for ln in text.strip().splitlines() if ln.strip()]
    return len(lines) == 3


def test_verify_missing_plan_file_teaches(tmp_path):
    res = runner.invoke(app, ["verify", str(tmp_path / "plan.yaml"), str(tmp_path / "env.json")])
    assert res.exit_code == 2, res.output
    err = res.output
    assert "plan.yaml" in err
    assert _three_lines(err), err


def test_run_missing_plan_file_teaches(tmp_path):
    res = runner.invoke(app, ["run", str(tmp_path / "plan.yaml"), str(tmp_path / "env.json")])
    assert res.exit_code == 2, res.output
    assert _three_lines(res.output), res.output


def test_tend_without_embedder_teaches(tmp_path, monkeypatch):
    import sys

    monkeypatch.setitem(sys.modules, "sentence_transformers", None)
    res = runner.invoke(app, ["tend", "--data-dir", str(tmp_path)])
    assert res.exit_code == 2, res.output
    assert "sentence_transformers" in res.output or "search" in res.output
    assert _three_lines(res.output), res.output
```

Adjust the positional argument names to what `run`/`verify` declare (read
`cli.py:1490-1510` and `:1684-1700` first). If `run` reads the plan through a different
loader that raises before the CLI sees it, the test still expects three lines and exit 2.

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_errors.py -q`
Expected: FAIL on the line-count assertions (today's messages are one line or a traceback).

- [ ] **Step 3: Write minimal implementation**

For each command, find every `typer.echo(<msg>, err=True)` followed by
`raise typer.Exit(code=N)` and every bare `raise typer.Exit(code=N)`. Replace each with
`_fail(what, why, fix, code=N)` using this rule:

- `what`: "Tried to <verb> <object>." using the command's own words (`verify the plan`, `run the plan`, `tend the journal`).
- `why`: the concrete cause, one sentence. File errors say the path. Parse errors say the file and the parser's first line. Verification rejections say `[stage] message` of the first violation.
- `fix`: the one next action. Missing file → "Check the path." Verify rejection → "Edit the plan or widen the envelope; see the violation above." Missing extra → "Install it: `uv pip install 'opendaisugi[search]'`."

Wrap the file loads in `run` and `verify`:

```python
try:
    plan_obj = ActionPlan(**yaml.safe_load(plan_path.read_text()))
except FileNotFoundError:
    _fail(
        f"Tried to read the plan {plan_path}.",
        "The file does not exist.",
        "Check the path.",
        code=2,
    )
except (yaml.YAMLError, ValueError) as e:
    _fail(
        f"Tried to read the plan {plan_path}.",
        f"It did not parse: {str(e).splitlines()[0]}",
        "Fix the file and run again.",
        code=2,
    )
```

(Same for the envelope path.) Do not change exit codes: 2 stays "bad input or rejected".

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_errors.py tests/test_cli_run.py tests/test_cli_tend.py tests/test_cli.py -q`
Expected: PASS. Existing tests that asserted the old one-line text need their assertion
moved to the new `why` line; the exit codes are unchanged.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_errors.py tests/test_cli_run.py tests/test_cli_tend.py
git commit -m "fix(cli): run, verify, and tend errors say what, why, and the next action"
```

---

### Task 8: Record the change in the plan file

**Files:**
- Modify: `docs/plans/2026-08-26-daisugi-interface-two-lenses.md:150-158` (the W9 "Still unbuilt" paragraph) and W6.

- [ ] **Step 1: Edit the W9 paragraph**

Replace the sentence starting "**Still unbuilt in W9:**" with:
"**Shipped (plan 1):** deterministic errors are never retried (`llm.preflight`), the
instructor XML is gone (`translate_llm_error`), a zero-step prompt is a plain answer with
exit 0, `daisugi config` shows every setting with its source, and every token-spending
command echoes `backend · gate · data` once."

Under W6 add: "**Shipped (plan 1):** `cli.main()` renders every `OpenDaisugiError` as
three lines; `run`, `verify`, `tend`, `orchestrate` use `_fail(what, why, fix)`."

- [ ] **Step 2: Commit**

```bash
git add docs/plans/2026-08-26-daisugi-interface-two-lenses.md
git commit -m "docs(plans): record plan 1 as shipped in the two-lens plan"
```

---

## Self-review

- Spec coverage (§4): flatten XML (T1), pre-flight (T2), NoStepsError (T3), `--llm None` + echo (T4), `config` (T5), `main()` + `_fail` (T6), audit (T7). Embedder device echo: `daisugi config` does not import torch; the device note is logged by `_search._get_model` only when the embedder loads, which is the honest place for it. Covered by not adding a torch import to a diagnostic command.
- Placeholders: Task 7 asks the implementer to read two ranges before editing; the rule and a concrete replacement are given. No "TBD".
- Types: `_fail(what, why, fix, *, code)` is used the same way in T6 and T7. `preflight(backend, *, model, env, which)` matches T2's tests. `ResolvedField(key, value, source)` matches T5's tests.
