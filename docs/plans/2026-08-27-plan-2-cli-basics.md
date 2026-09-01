# Plan 2: CLI Basics (W1 + W2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** daisugi behaves under a pipe, is never silent, starts in well under a second, and answers gate calls in milliseconds through a resident process that can only ever fall back to the slow correct path, never to allow.

**Architecture:** One output module (`console.py`) that every command reads. A PEP 562 lazy package init that stops `import opendaisugi` from loading 54 submodules. A unix-socket gate server plus a stdlib-only client with an in-process fallback.

**Tech Stack:** Python 3.12, Typer 0.23, `socketserver`, pytest.

**Spec:** `docs/plans/2026-08-27-cockpit-spec.md` §5, §9, and §11 (cross-plan rules).

**Requires:** plan 1 shipped — Task 2 edits `_echo_resolved` and Task 3 imports
`config.installed_hook_mode` and lists the `config` command, all plan-1 deliverables. Do
not build in isolation. **Provides:** `console.py`, `facade.py`, `gate_server.SOCK_NAME`,
the resident gate + `gate_client`, the root output flags.

## Corrections from adversarial review (2026-08-27 — authoritative over the tasks below)

The resident-gate *fallback* is verified fail-closed for every broken-server case
(garbage / partial / timeout / wrong-version / non-int exit → `None` → in-process gate). The
`Daisugi`→`facade` PEP-562 move keeps `from opendaisugi import Daisugi` working (6 cli
functions + 36 test files). Apply these:

- **BLOCKER — Task 1 `step()` test can never pass.** The test sleeps `0.01 s` but the impl
  suppresses the "done (…)" tail unless `dt >= min_s` (`0.1`). Sleep `>= 0.11` in the test
  (or lower `min_s`).
- **BLOCKER — Task 2/3 help-text assertions fail on this box (`FORCE_COLOR=3`).** See §11.1
  rule 5: assert `--json` presence via Click param introspection, not `"--json" in
  res.output`; strip `FORCE_COLOR` / set `NO_COLOR=1` in the subprocess help tests.
- **SHOULD-FIX (fail-closed honesty) — enforce the socket invariant.** The client trusts any
  well-formed reply, and `serve()`'s `root.mkdir(exist_ok=True, mode=0o700)` is a no-op on a
  pre-existing `gate/`. Add server `os.chmod(root, 0o700)` after mkdir, and a client
  `os.lstat(sock_path)` guard that falls back unless it is a socket owned by the current uid,
  mode `0o600`, not a symlink. Add the rogue-well-formed-`exit_code:0`-listener test.
- **SHOULD-FIX — Task 5 `check_dag` import.** It is imported at `verify.py:24` (not 29-31)
  and *called at two sites* — `_verify` (≈830) and `verify_step` (≈910). Add the lazy import
  inside **both** functions, or `verify_step` NameErrors.
- **SHOULD-FIX — Task 4 name the real heavy imports.** `import opendaisugi.cli` pulls z3 and
  networkx via top-level `from opendaisugi.verify import verify` (cli.py:33) and
  `from opendaisugi.supervisor import Supervisor` (cli.py:38). Move those two explicitly;
  relocating `verify` touches several command bodies. Guard the `_LAZY` scraper against the
  self-referential `("opendaisugi","integrations")` (would infinite-loop `__getattr__`).
- **SHOULD-FIX — Task 7 upgrade path.** `_patch_claude_gate` dedupes on the substring
  `opendaisugi.gate`, so an existing `-m opendaisugi.gate` hook is *not* rewritten to
  `-m opendaisugi.gate_client` (warn-and-return branch). Add a rewrite branch so the one real
  user actually gets the resident client on upgrade.
- **SHOULD-FIX — Task 3 naming.** The code sample uses `as_json`, the prose says
  `json_output`. Pick one (`json_output`).
- **NIT — Task 7 escape-path behavior drift.** `parse_args` is now inside the `try`, so
  `python -m opendaisugi.gate --help` (argparse `SystemExit(0)`) is caught by
  `except BaseException` and returns exit 2. Let `SystemExit` from arg-parsing pass through;
  the "exit codes don't change" claim depends on it. Also: `_FLOW = "▸"` (dashboard.py:33)
  survives `--plain` — swap it with `_GLYPH`.

## Global Constraints

- `uv run pytest -q` green and `uv run ruff check .` clean before each commit (line length 100).
- Commit messages: `type(scope): why`. No attribution trailers. Never push.
- Human output on stdout; progress, warnings, errors on stderr. Color and box drawing only on a TTY, never under `--plain`, `NO_COLOR`, or `TERM=dumb`.
- Fail-closed: no code path in Task 7 may turn a server failure into an allow. Every failure returns to the in-process gate.
- `opendaisugi.cli:app` must stay importable (tests use it); `opendaisugi.cli:main` is the console entry (plan 1).
- Do not add symbols named `Session`, `SessionRecord`, or `list_sessions`.

---

### Task 1: `console.py`, the one output layer

**Files:**
- Create: `src/opendaisugi/console.py`
- Test: `tests/test_console.py` (new)

**Interfaces:**
- Produces: `OutputMode`, `resolve_output(...)`, `set_mode()`, `current()`, `say()`, `note()`, `warn()`, `style()`, `step()`, `BOX` / `ASCII_BOX` glyph tables and `glyphs()`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_console.py
"""The output layer: TTY detection, NO_COLOR, --plain, -q, and progress notes."""

from __future__ import annotations

import io
import time

from opendaisugi import console


class _Stream(io.StringIO):
    def __init__(self, tty: bool) -> None:
        super().__init__()
        self._tty = tty

    def isatty(self) -> bool:
        return self._tty


def test_color_only_on_a_tty():
    assert console.resolve_output(stream=_Stream(True), env={}).color is True
    assert console.resolve_output(stream=_Stream(False), env={}).color is False


def test_no_color_and_dumb_term_disable_color():
    assert console.resolve_output(stream=_Stream(True), env={"NO_COLOR": "1"}).color is False
    assert console.resolve_output(stream=_Stream(True), env={"TERM": "dumb"}).color is False


def test_plain_disables_color_and_boxes():
    mode = console.resolve_output(plain=True, stream=_Stream(True), env={})
    assert mode.color is False and mode.plain is True
    assert console.glyphs(mode) is console.ASCII_BOX


def test_style_is_identity_when_color_off():
    console.set_mode(console.resolve_output(stream=_Stream(False), env={}))
    assert console.style("x", "green") == "x"
    console.set_mode(console.resolve_output(stream=_Stream(True), env={}))
    assert console.style("x", "green") != "x" and "x" in console.style("x", "green")


def test_note_is_silent_under_quiet_but_say_is_not(capsys):
    console.set_mode(console.resolve_output(quiet=True, stream=_Stream(False), env={}))
    console.say("result")
    console.note("progress")
    out = capsys.readouterr()
    assert out.out == "result\n"
    assert out.err == ""


def test_note_goes_to_stderr(capsys):
    console.set_mode(console.resolve_output(stream=_Stream(False), env={}))
    console.note("progress")
    assert capsys.readouterr().err == "progress\n"


def test_step_prints_on_a_tty_and_not_when_piped(capsys):
    tty = _Stream(True)
    console.set_mode(console.resolve_output(stream=tty, env={}), err_stream=tty)
    with console.step("verifying"):
        time.sleep(0.01)
    text = tty.getvalue()
    assert text.startswith("verifying…")
    assert "done (" in text and "s)" in text

    piped = _Stream(False)
    console.set_mode(console.resolve_output(stream=piped, env={}), err_stream=piped)
    with console.step("verifying"):
        pass
    assert piped.getvalue() == ""
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_console.py -q`
Expected: FAIL with `ModuleNotFoundError: opendaisugi.console`.

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/console.py
"""The one output layer: results on stdout, everything else on stderr.

Rules (clig.dev, applied): color and box drawing only on a real terminal, off
under --plain, NO_COLOR, or TERM=dumb; -q silences progress notes but never
results; a task longer than a blink says so on a TTY and stays silent when piped.
"""

from __future__ import annotations

import os
import sys
import time
from collections.abc import Iterator, Mapping
from contextlib import contextmanager
from dataclasses import dataclass
from typing import TextIO

_COLORS = {
    "red": "31",
    "green": "32",
    "yellow": "33",
    "blue": "34",
    "magenta": "35",
    "cyan": "36",
    "dim": "2",
    "bold": "1",
}


@dataclass(frozen=True)
class OutputMode:
    color: bool
    plain: bool
    quiet: bool
    verbose: bool
    json: bool = False


BOX = {
    "h": "─",
    "v": "│",
    "tl": "┌",
    "tr": "┐",
    "bl": "└",
    "br": "┘",
    "t": "┬",
    "b": "┴",
    "l": "├",
    "r": "┤",
    "x": "┼",
    "on": "●",
    "avail": "○",
    "off": "·",
    "ok": "✓",
    "no": "✗",
}
ASCII_BOX = {
    "h": "-",
    "v": "|",
    "tl": "+",
    "tr": "+",
    "bl": "+",
    "br": "+",
    "t": "+",
    "b": "+",
    "l": "+",
    "r": "+",
    "x": "+",
    "on": "*",
    "avail": "o",
    "off": ".",
    "ok": "ok",
    "no": "X",
}


def resolve_output(
    *,
    plain: bool = False,
    quiet: bool = False,
    verbose: bool = False,
    no_color: bool = False,
    json: bool = False,
    stream: TextIO | None = None,
    env: Mapping[str, str] | None = None,
) -> OutputMode:
    stream = sys.stdout if stream is None else stream
    env = os.environ if env is None else env
    try:
        tty = bool(stream.isatty())
    except (AttributeError, ValueError):
        tty = False
    color = tty and not plain and not no_color and not json
    if env.get("NO_COLOR") or env.get("TERM") == "dumb":
        color = False
    return OutputMode(color=color, plain=plain or not tty, quiet=quiet, verbose=verbose, json=json)


_MODE = resolve_output()
_ERR: TextIO | None = None


def set_mode(mode: OutputMode, *, err_stream: TextIO | None = None) -> None:
    global _MODE, _ERR
    _MODE = mode
    _ERR = err_stream


def current() -> OutputMode:
    return _MODE


def glyphs(mode: OutputMode | None = None) -> dict[str, str]:
    mode = _MODE if mode is None else mode
    return ASCII_BOX if mode.plain else BOX


def _err() -> TextIO:
    return _ERR if _ERR is not None else sys.stderr


def say(text: str) -> None:
    """A result. Always printed, always stdout."""
    sys.stdout.write(text + "\n")
    sys.stdout.flush()


def note(text: str) -> None:
    """Progress or context. stderr; silent under -q."""
    if _MODE.quiet:
        return
    e = _err()
    e.write(text + "\n")
    e.flush()


def warn(text: str) -> None:
    e = _err()
    e.write(style(text, "yellow") + "\n")
    e.flush()


def style(text: str, color: str) -> str:
    if not _MODE.color:
        return text
    code = _COLORS.get(color)
    return f"\x1b[{code}m{text}\x1b[0m" if code else text


@contextmanager
def step(label: str, *, min_s: float = 0.1) -> Iterator[None]:
    """`label…` on a TTY, then ` done (1.2 s)`. Nothing when piped or quiet.

    The label prints immediately so the user is never looking at a silent
    prompt; the tail prints only when the step took longer than ``min_s``.
    """
    e = _err()
    live = not _MODE.quiet and not _MODE.plain
    t0 = time.monotonic()
    if live:
        e.write(f"{label}…")
        e.flush()
    try:
        yield
    finally:
        if live:
            dt = time.monotonic() - t0
            e.write(f" done ({dt:.1f} s)\n" if dt >= min_s else "\n")
            e.flush()
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_console.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/console.py tests/test_console.py
git commit -m "feat(cli): one output layer that knows TTYs, NO_COLOR, --plain, and -q"
```

---

### Task 2: Root flags, and no escapes or boxes under a pipe

**Files:**
- Modify: `src/opendaisugi/cli.py:58-69` (`_root` callback)
- Modify: `src/opendaisugi/modules.py:231-285` (`render_wiring`), `src/opendaisugi/dashboard.py:184-246` (`render_dashboard`), `:264-299` (`run_live`)
- Test: `tests/test_cli_pipe.py` (new)

**Interfaces:**
- Consumes: `console.resolve_output`, `console.set_mode`, `console.glyphs`.
- Produces: root options `--plain`, `-q/--quiet`, `-v/--verbose`, `--no-color`; `render_wiring(data_dir, *, width=66, plain: bool | None = None)`, `render_dashboard(..., plain: bool | None = None)`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_cli_pipe.py
"""Behave under a pipe: no escapes, no box drawing, quiet means quiet."""

from __future__ import annotations

import os
import subprocess
import sys

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()
_BOX = "─│┌┐└┘├┤┬┴┼╭╮╰╯━┃"


def _run(*args: str, env_extra: dict | None = None) -> subprocess.CompletedProcess:
    env = dict(os.environ, **(env_extra or {}))
    return subprocess.run(
        [sys.executable, "-m", "opendaisugi.cli", *args],
        capture_output=True,
        text=True,
        env=env,
        timeout=60,
    )


def test_help_piped_has_no_escapes():
    proc = _run("--help")
    assert proc.returncode == 0
    assert "\x1b[" not in proc.stdout


def test_modules_piped_has_no_box_drawing(tmp_path):
    proc = _run("modules", "--data-dir", str(tmp_path))
    assert proc.returncode == 0, proc.stderr
    assert not any(ch in proc.stdout for ch in _BOX), proc.stdout[:200]


def test_no_color_env_has_no_escapes(tmp_path):
    proc = _run("status", "--data-dir", str(tmp_path), env_extra={"NO_COLOR": "1"})
    assert "\x1b[" not in proc.stdout + proc.stderr


def test_plain_flag_before_command(tmp_path):
    res = runner.invoke(app, ["--plain", "modules", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert not any(ch in res.output for ch in _BOX)


def test_dashboard_once_piped_never_clears_the_screen(tmp_path):
    proc = _run("dashboard", "--data-dir", str(tmp_path))  # not a TTY: one frame
    assert proc.returncode == 0, proc.stderr
    assert "\x1b[H\x1b[2J" not in proc.stdout


def test_quiet_silences_the_state_echo(tmp_path, monkeypatch):
    from opendaisugi import Daisugi
    from tests.test_cli_orchestrate import _fake_result

    async def _ok(self, prompt, **_kw):
        return _fake_result()

    monkeypatch.setattr(Daisugi, "orchestrate", _ok)
    res = runner.invoke(
        app, ["-q", "orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)]
    )
    assert res.exit_code == 0, res.output
    assert "backend:" not in res.output
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_pipe.py -q`
Expected: FAIL on the box-drawing and `--plain` tests (Typer rejects the unknown option).

- [ ] **Step 3: Write minimal implementation**

In `cli.py` `_root`:

```python
@app.callback()
def _root(
    version: bool = typer.Option(
        False,
        "--version",
        callback=_version_callback,
        is_eager=True,
        help="Show the opendaisugi version and exit.",
    ),
    plain: bool = typer.Option(
        False, "--plain", help="No color, no box drawing; greppable output."
    ),
    quiet: bool = typer.Option(False, "-q", "--quiet", help="Results only; no progress notes."),
    verbose: bool = typer.Option(False, "-v", "--verbose", help="Show tracebacks and detail."),
    no_color: bool = typer.Option(False, "--no-color", help="Disable color (same as NO_COLOR=1)."),
) -> None:
    """Runtime assurance for agent actions."""
    from opendaisugi import console

    console.set_mode(
        console.resolve_output(plain=plain, quiet=quiet, verbose=verbose, no_color=no_color)
    )
    if verbose:
        os.environ["DAISUGI_DEBUG"] = "1"
```

Change `_echo_resolved` (plan 1) to call `console.note(...)` instead of `typer.echo(..., err=True)`.

In `modules.render_wiring` and `dashboard.render_dashboard`, add `plain: bool | None = None`;
resolve `g = console.glyphs() if plain is None else (console.ASCII_BOX if plain else console.BOX)`
and use `g["h"]`, `g["v"]`, `g["tl"]`... in place of the literal box characters, and
`g["on"]/g["avail"]/g["off"]` in place of `_GLYPH` values (keep `_GLYPH` as the
Unicode table; `dashboard.py:30` imports it). In `run_live`, keep the existing non-TTY
branch (one frame) and also skip the clear escape when `console.current().plain`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_pipe.py tests/test_dashboard.py tests/test_modules.py -q`
Expected: PASS. Tests that assert the Unicode glyphs must pass `plain=False` explicitly or
set the mode to a TTY.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py src/opendaisugi/modules.py src/opendaisugi/dashboard.py tests/test_cli_pipe.py
git commit -m "feat(cli): --plain/-q/-v/--no-color and no escapes or boxes under a pipe"
```

---

### Task 3: `--json` on every read command, with a conformance test

**Files:**
- Modify: `src/opendaisugi/cli.py` (`gate status` :866, `hook list` :492, `pathways list` :1139, `pathways show` :1155, `registry status` :403)
- Test: `tests/test_cli_json_conformance.py` (new)

**Interfaces:**
- Produces: JSON shapes: `gate status` → `{"armed": bool, "mode": str, "mode_source": str, "envelopes": [str]}`; `hook list` → `[{"session_id", "calls", "first_at", "last_at"}]`; `pathways list` → `[{"id", "task_description", "hit_count", "version", "distilled_at"}]`; `pathways show` → `CompiledPathway.model_dump(mode="json")` minus `task_embedding`; `registry status` → the dict the command already assembles.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_cli_json_conformance.py
"""Every read command accepts --json. Drop one and this fails."""

from __future__ import annotations

import json

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()

READ_COMMANDS = [
    ["status"],
    ["config"],
    ["modules"],
    ["dashboard"],
    ["models"],
    ["gate", "status"],
    ["gate", "report"],
    ["gate", "audit"],
    ["hook", "list"],
    ["pathways", "list"],
    ["pathways", "show"],
    ["pathways", "stats"],
    ["journal", "stats"],
    ["journal", "search"],
    ["tiers", "stats"],
    ["gardener", "status"],
    ["registry", "status"],
]


@pytest.mark.parametrize("cmd", READ_COMMANDS, ids=[" ".join(c) for c in READ_COMMANDS])
def test_read_command_accepts_json(cmd):
    res = runner.invoke(app, [*cmd, "--help"])
    assert res.exit_code == 0, res.output
    assert "--json" in res.output, f"{' '.join(cmd)} has no --json"


def test_gate_status_json_has_mode(tmp_path):
    res = runner.invoke(app, ["gate", "status", "--root", str(tmp_path / "gate"), "--json"])
    assert res.exit_code == 0, res.output
    body = json.loads(res.output)
    assert body["armed"] is True
    assert body["mode"] in ("shadow", "enforce")
    assert body["envelopes"] == []


def test_gate_status_human_shows_mode(tmp_path):
    res = runner.invoke(app, ["gate", "status", "--root", str(tmp_path / "gate")])
    assert res.exit_code == 0
    assert "mode: shadow" in res.output


def test_hook_list_json_empty(tmp_path):
    res = runner.invoke(app, ["hook", "list", "--captures-root", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    assert json.loads(res.output) == []


def test_pathways_list_json_empty(tmp_path):
    res = runner.invoke(app, ["pathways", "list", "--data-dir", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    assert json.loads(res.output) == []
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_json_conformance.py -q`
Expected: FAIL for `gate status`, `hook list`, `pathways list`, `pathways show`, `registry status`.

- [ ] **Step 3: Write minimal implementation**

Read each command first. `gate status` becomes:

```python
@gate_app.command("status")
def gate_status_cmd(
    root: Path = _GATE_ROOT_OPT,
    as_json: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show armed/disarmed state, the verdict mode, and the registered envelopes."""
    from opendaisugi.config import installed_hook_mode
    from opendaisugi.gate import _envelopes_dir, is_disarmed, resolve_gate_mode

    armed = not is_disarmed(root)
    hook_mode = installed_hook_mode(Path.home() / ".claude" / "settings.json")
    mode = hook_mode or resolve_gate_mode(None, root=root)
    source = "hook" if hook_mode else "config"
    d = _envelopes_dir(root)
    envelopes = sorted(e.stem for e in d.glob("*.json")) if d.exists() else []
    if as_json:
        typer.echo(
            json.dumps(
                {"armed": armed, "mode": mode, "mode_source": source, "envelopes": envelopes}
            )
        )
        return
    typer.echo(f"gate: {'armed' if armed else 'DISARMED'} · mode: {mode} ({source})")
    if not envelopes:
        typer.echo("no envelopes registered — enforce mode would deny everything")
    for e in envelopes:
        typer.echo(f"  envelope: {e}")
```

`hook list`: add `as_json` and `typer.echo(json.dumps(sessions))` before the human table.
`pathways list`: add `json_output`; emit
`[{"id": p.id, "task_description": p.task_description, "hit_count": p.hit_count, "version": p.version, "distilled_at": p.distilled_at} for p in store.list_all()]`.
`pathways show`: emit `p.model_dump(mode="json", exclude={"task_embedding"})`.
`registry status`: emit the dict it already builds. Name every new parameter `json_output`
(the three `as_json` in `gate` stay; do not rename working code).

Also give `gate check --mode` its missing validation (W5): change the option to
`typer.Option("shadow", "--mode", click_type=click.Choice(["shadow", "enforce"]))` or check
in the body and `_fail` on anything else with code 2.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_json_conformance.py tests/test_cli_gate.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_json_conformance.py
git commit -m "feat(cli): --json on every read command, gate status shows the mode"
```

---

### Task 4: Lazy package init: `import opendaisugi` loads nothing heavy

**Files:**
- Create: `src/opendaisugi/facade.py` (the `Daisugi` class, moved)
- Modify: `src/opendaisugi/__init__.py` (imports → `_LAZY` map + `__getattr__`)
- Modify: `src/opendaisugi/cli.py` (any top-level import that pulls z3/networkx moves into the function)
- Test: `tests/test_lazy_init.py` (new)

**Interfaces:**
- Consumes: the 54 `from opendaisugi.X import ...` lines at `__init__.py:22-272`.
- Produces: `opendaisugi.__getattr__`, `opendaisugi.__dir__`, `opendaisugi.facade.Daisugi`; every name in `__all__` still resolves as `opendaisugi.<name>`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_lazy_init.py
"""`import opendaisugi` must not load the verifier, the executors, or networkx."""

from __future__ import annotations

import subprocess
import sys

import opendaisugi

HEAVY = ("z3", "networkx", "opendaisugi.agentic_executor", "opendaisugi.verify", "opendaisugi.gate")


def _modules_after(stmt: str) -> set[str]:
    code = f"import sys; {stmt}; print('\\n'.join(sorted(sys.modules)))"
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=True)
    return set(out.stdout.split())


def test_import_package_is_light():
    loaded = _modules_after("import opendaisugi")
    assert not (loaded & set(HEAVY)), loaded & set(HEAVY)


def test_import_cli_is_light():
    loaded = _modules_after("import opendaisugi.cli")
    assert not (loaded & set(HEAVY)), loaded & set(HEAVY)


def test_every_public_name_still_resolves():
    missing = []
    for name in opendaisugi.__all__:
        try:
            getattr(opendaisugi, name)
        except ImportError:  # an optional extra (signing) is allowed to be absent
            continue
        except AttributeError:
            missing.append(name)
    assert not missing, missing


def test_facade_and_constants_are_reachable():
    from opendaisugi import DEFAULT_DATA_DIR, Daisugi, verify  # noqa: F401

    assert callable(verify)
    assert Daisugi.__module__ == "opendaisugi.facade"
    assert DEFAULT_DATA_DIR.name == ".opendaisugi"


def test_dir_lists_lazy_names():
    assert "Daisugi" in dir(opendaisugi) and "verify" in dir(opendaisugi)


def test_unknown_attribute_raises_attribute_error():
    import pytest

    with pytest.raises(AttributeError):
        opendaisugi.no_such_thing  # noqa: B018
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_lazy_init.py -q`
Expected: FAIL on `test_import_package_is_light` (z3 and networkx are loaded).

- [ ] **Step 3: Write minimal implementation**

3a. Move the class. Cut `class Daisugi` (from `__init__.py:297` to the line before
`def __getattr__`) into `src/opendaisugi/facade.py`. At its top:

```python
"""The Daisugi facade: the composition root, imported on first use only.

Lives outside ``__init__`` so ``import opendaisugi`` stays light (PEP 562 lazy
exports); the package attribute ``DEFAULT_DATA_DIR`` is read at call time so the
test-suite fixture that redirects it keeps working.
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Literal

_log = logging.getLogger("opendaisugi.facade")
```

Then run `uv run ruff check src/opendaisugi/facade.py` and add one import per F821
(undefined name) from the module that owns it. Inside `Daisugi.__init__`, where the class
reads `DEFAULT_DATA_DIR`, use:

```python
        import opendaisugi

        self.data_dir = Path(data_dir) if data_dir is not None else opendaisugi.DEFAULT_DATA_DIR
```

3b. Build the lazy map. Run once, from the repo root, and paste the output into
`__init__.py` in place of lines 22-272:

```python
# scratch: build _LAZY from the current import block
import ast, pathlib

src = pathlib.Path("src/opendaisugi/__init__.py").read_text()
tree = ast.parse(src)
lazy = {}
for node in tree.body:
    if isinstance(node, ast.ImportFrom) and node.module and node.module.startswith("opendaisugi"):
        for a in node.names:
            lazy[a.asname or a.name] = (node.module, a.name)
    elif isinstance(node, ast.Try):  # the optional signing block
        for inner in node.body:
            if isinstance(inner, ast.ImportFrom):
                for a in inner.names:
                    lazy[a.asname or a.name] = (inner.module, a.name)
print("_LAZY: dict[str, tuple[str, str]] = {")
for k in sorted(lazy):
    print(f"    {k!r}: {lazy[k]!r},")
print("}")
```

`integrations` is a subpackage: map it as `"integrations": ("opendaisugi.integrations", None)`.
Add `"Daisugi": ("opendaisugi.facade", "Daisugi")`.

3c. Replace the module-level `__getattr__` (the `MuJoCoExecutor` one) with:

```python
import importlib

_LAZY: dict[str, tuple[str, str | None]] = { ... pasted ... }
_LAZY["MuJoCoExecutor"] = ("opendaisugi.executor_mujoco", "MuJoCoExecutor")


def __getattr__(name: str):
    """PEP 562: import a public name on first use and cache it.

    ``import opendaisugi`` used to pull 54 submodules (~530 ms, incl. Z3 and
    networkx) before any command ran. Now the package is a directory of names.
    """
    target = _LAZY.get(name)
    if target is None:
        raise AttributeError(f"module 'opendaisugi' has no attribute {name!r}")
    module_name, attr = target
    module = importlib.import_module(module_name)
    value = module if attr is None else getattr(module, attr)
    globals()[name] = value
    return value


def __dir__() -> list[str]:
    return sorted(set(globals()) | set(_LAZY))
```

Keep in `__init__.py`, eagerly: the docstring, `logging` setup, `DEFAULT_DATA_DIR`,
`__version__`, `__all__`, and `os`/`Path`/`Literal` imports only if still used.

3d. `cli.py`: run `python -X importtime -c "import opendaisugi.cli" 2>&1 | grep -E "z3|networkx|agentic|gate|verify"`.
For every top-level `from opendaisugi.<mod> import ...` in `cli.py` that appears in that
list, move the import into the command function(s) that use it. `opendaisugi.models`
(pydantic, ~50 ms) may stay.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_lazy_init.py -q`, then `uv run pytest -q` (the full suite:
35 files import from the package root).
Expected: PASS. Record `time daisugi --help` (3 runs) in the commit message body.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/__init__.py src/opendaisugi/facade.py src/opendaisugi/cli.py tests/test_lazy_init.py
git commit -m "perf(init): lazy package exports; the CLI no longer imports Z3 to print help

Before: import opendaisugi ~530 ms (54 eager submodules). After: <measured> ms.
daisugi --help: <before> s -> <after> s (3 warm runs)."
```

---

### Task 5: The gate does not pay for networkx

**Files:**
- Modify: `src/opendaisugi/verify.py:29-31` (imports)
- Test: `tests/test_lazy_init.py` (extend)

- [ ] **Step 1: Write the failing test**

```python
def test_gate_import_does_not_load_networkx():
    loaded = _modules_after("import opendaisugi.gate")
    assert "networkx" not in loaded
    assert "z3" in loaded  # the verifier itself is expected here
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_lazy_init.py::test_gate_import_does_not_load_networkx -q`
Expected: FAIL (networkx loaded via `opendaisugi.dag`).

- [ ] **Step 3: Write minimal implementation**

In `verify.py`, remove the top-level `from opendaisugi.dag import check_dag` and inside
`verify()` at the DAG stage write:

```python
    from opendaisugi.dag import check_dag  # networkx: only when a plan reaches this stage
```

Apply the same to any other top-level `opendaisugi.dag` import on the gate's path
(`grep -rn "from opendaisugi.dag" src/opendaisugi/gate.py src/opendaisugi/verify.py src/opendaisugi/hook.py`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_lazy_init.py tests/test_verify.py tests/test_gate.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/verify.py tests/test_lazy_init.py
git commit -m "perf(gate): import networkx only when a plan reaches the DAG stage"
```

---

### Task 6: Never silent: long commands say what they are doing

**Files:**
- Modify: `src/opendaisugi/cli.py` (`orchestrate` :2418-2430, `onboard` :1784-2014, `tend` :1735-1783)
- Test: `tests/test_cli_progress.py` (new)

**Interfaces:**
- Consumes: `console.step(label)`.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_cli_progress.py
"""A command that takes longer than a blink says so on a TTY."""

from __future__ import annotations

import io

from typer.testing import CliRunner

from opendaisugi import console
from opendaisugi.cli import app

runner = CliRunner()


class _Tty(io.StringIO):
    def isatty(self) -> bool:
        return True


def test_orchestrate_prints_a_progress_note_on_a_tty(monkeypatch, tmp_path):
    from opendaisugi import Daisugi
    from tests.test_cli_orchestrate import _fake_result

    async def _slow(self, prompt, **_kw):
        return _fake_result()

    monkeypatch.setattr(Daisugi, "orchestrate", _slow)
    err = _Tty()
    monkeypatch.setattr(
        console, "set_mode", lambda mode, err_stream=None: None
    )  # keep our TTY mode
    console._MODE = console.resolve_output(stream=_Tty(), env={})
    console._ERR = err
    res = runner.invoke(app, ["orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert err.getvalue().startswith("orchestrating…")
    console._ERR = None
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_cli_progress.py -q`
Expected: FAIL (no progress note).

- [ ] **Step 3: Write minimal implementation**

In `orchestrate_cmd` wrap the call:

```python
    from opendaisugi import console

    try:
        with console.step("orchestrating"):
            result = asyncio.run(d.orchestrate(...))
```

In `onboard_cmd` wrap discovery, replay, and distillation in
`console.step("discovering transcripts")`, `console.step("replaying into the journal")`,
`console.step("distilling pathways")`. In `tend_cmd` wrap the tend call in
`console.step("tending the garden")`. Read each function first; wrap the existing calls,
change nothing else.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_progress.py tests/test_cli_onboard.py tests/test_cli_tend.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_progress.py
git commit -m "feat(cli): long commands say what they are doing on a TTY"
```

---

### Task 7: The resident gate: a socket server and a stdlib client with a fallback

**Files:**
- Modify: `src/opendaisugi/gate.py:736-798` (extract `run_argv`)
- Create: `src/opendaisugi/gate_server.py`, `src/opendaisugi/gate_client.py`
- Modify: `src/opendaisugi/gate.py:656-733` (`gate_settings_json` emits the client entry)
- Modify: `src/opendaisugi/cli.py` (add `gate serve`)
- Test: `tests/test_gate_resident.py` (new)

**Interfaces:**
- Produces: `gate.run_argv(argv: list[str], raw: bytes) -> GateOutcome`; `gate_server.serve(root: Path, *, ready=None, stop=None) -> None`; `gate_server.SOCK_NAME = "gate.sock"`; `gate_client.main(argv=None) -> int`; `gate_client.ask_server(sock: Path, argv: list[str], raw: bytes, *, timeout_s: float) -> dict | None`.
- Wire format: one JSON line each way. Request `{"v": 1, "argv": [...], "stdin_b64": "..."}`. Reply `{"v": 1, "stdout": str, "stderr": str, "exit_code": int}`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_gate_resident.py
"""A resident gate answers in milliseconds and can only fall back, never allow."""

from __future__ import annotations

import base64
import json
import os
import socket
import threading
from pathlib import Path

import pytest

from opendaisugi import gate as gate_mod
from opendaisugi.gate import register_envelope, run_argv, starter_envelope
from opendaisugi.gate_client import ask_server, main as client_main
from opendaisugi.gate_server import SOCK_NAME, serve

PAYLOAD = json.dumps(
    {"session_id": "s1", "tool_name": "Read", "tool_input": {"file_path": "README.md"}}
).encode()


@pytest.fixture
def root(tmp_path: Path) -> Path:
    r = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=r)
    return r


@pytest.fixture
def server(root: Path):
    ready = threading.Event()
    stop = threading.Event()
    t = threading.Thread(
        target=serve, args=(root,), kwargs={"ready": ready, "stop": stop}, daemon=True
    )
    t.start()
    assert ready.wait(5), "server did not start"
    yield root / SOCK_NAME
    stop.set()
    t.join(5)


def _argv(root: Path, mode: str = "enforce") -> list[str]:
    return ["--mode", mode, "--root", str(root), "--format", "claude"]


def test_run_argv_matches_main(root: Path):
    out = run_argv(_argv(root), PAYLOAD)
    assert out.exit_code in (0, 2)
    assert out.stdout or out.stderr


def test_server_reply_matches_in_process(root: Path, server: Path):
    direct = run_argv(_argv(root), PAYLOAD)
    reply = ask_server(server, _argv(root), PAYLOAD, timeout_s=5)
    assert reply is not None
    assert reply["exit_code"] == direct.exit_code
    assert reply["stdout"] == direct.stdout


def test_socket_is_private(root: Path, server: Path):
    assert oct(server.stat().st_mode & 0o777) == "0o600"


def test_client_falls_back_when_no_server(root: Path, capsys, monkeypatch):
    monkeypatch.setattr("sys.stdin", type("S", (), {"buffer": __import__("io").BytesIO(PAYLOAD)})())
    code = client_main(_argv(root))
    direct = run_argv(_argv(root), PAYLOAD)
    assert code == direct.exit_code


def test_client_falls_back_on_garbage_reply(root: Path, tmp_path: Path, monkeypatch):
    sock_path = root / SOCK_NAME
    root.mkdir(parents=True, exist_ok=True)
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    srv.listen(1)

    def _garbage():
        conn, _ = srv.accept()
        conn.recv(65536)
        conn.sendall(b"not json\n")
        conn.close()

    threading.Thread(target=_garbage, daemon=True).start()
    assert ask_server(sock_path, _argv(root), PAYLOAD, timeout_s=2) is None
    # the fallback is the real gate: a Read inside the workspace is allowed in enforce
    direct = run_argv(_argv(root), PAYLOAD)
    monkeypatch.setattr("sys.stdin", type("S", (), {"buffer": __import__("io").BytesIO(PAYLOAD)})())
    assert client_main(_argv(root)) == direct.exit_code
    srv.close()


def test_client_falls_back_on_partial_reply(root: Path, monkeypatch):
    sock_path = root / SOCK_NAME
    root.mkdir(parents=True, exist_ok=True)
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    srv.listen(1)

    def _partial():
        conn, _ = srv.accept()
        conn.recv(65536)
        conn.sendall(json.dumps({"v": 1, "exit_code": 0}).encode() + b"\n")  # no stdout key
        conn.close()

    threading.Thread(target=_partial, daemon=True).start()
    assert ask_server(sock_path, _argv(root), PAYLOAD, timeout_s=2) is None
    srv.close()


def test_client_never_allows_on_server_timeout(root: Path):
    sock_path = root / SOCK_NAME
    root.mkdir(parents=True, exist_ok=True)
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    srv.listen(1)  # accept nothing: the client must time out and fall back
    assert ask_server(sock_path, _argv(root), PAYLOAD, timeout_s=0.3) is None
    srv.close()


def test_settings_json_uses_the_client_entry(root: Path):
    body = json.loads(gate_mod.gate_settings_json(mode="enforce", root=root))
    cmd = body["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "-m opendaisugi.gate_client" in cmd
    assert "opendaisugi.gate" in cmd  # install's idempotency substring still matches
    assert cmd.endswith("|| exit 2")
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_gate_resident.py -q`
Expected: FAIL with `ImportError` (`run_argv`, `gate_server`, `gate_client`).

- [ ] **Step 3: Write minimal implementation**

3a. In `gate.py`, split `main`:

```python
def _build_parser() -> "argparse.ArgumentParser":
    import argparse

    parser = argparse.ArgumentParser(prog="opendaisugi.gate", add_help=True)
    parser.add_argument("--mode", choices=("shadow", "enforce"), default=None)
    parser.add_argument("--root", type=Path, default=DEFAULT_GATE_ROOT)
    parser.add_argument("--format", dest="fmt", default="claude")
    parser.add_argument("--verify-timeout", type=float, default=_DEFAULT_VERIFY_TIMEOUT_S)
    parser.add_argument("--captures-root", type=Path, default=None)
    parser.add_argument(
        "--session", default=None, help="Pin the envelope to this registered session."
    )
    return parser


def run_argv(argv: list[str], raw: bytes) -> GateOutcome:
    """The whole gate for one call, as an outcome: argv + stdin bytes in, verdict out.

    Fail-closed wrapper: ANY escape (a BaseException out of the verify thread,
    a bad argv) denies in enforce mode. Shared by the process entry, the
    resident server, and the client's fallback so the three cannot drift.
    """
    mode = "enforce"  # until argv proves otherwise, an escape must deny
    try:
        args = _build_parser().parse_args(argv)
        mode = resolve_gate_mode(args.mode, root=args.root)
        return gate_and_contract(
            raw,
            root=args.root,
            fmt=args.fmt,
            mode=mode,
            verify_timeout_s=args.verify_timeout,
            captures_root=args.captures_root,
            pin_session=args.session,
        )
    except BaseException as exc:  # noqa: BLE001 — deny-by-default on any escape
        if mode == "enforce":
            return GateOutcome(
                stdout="",
                stderr=f"openDaisugi gate: DENIED (fail-closed on error): {exc}",
                exit_code=2,
                decision=_deny("enforce", f"gate escape: {exc}", t0=time.monotonic()),
            )
        return GateOutcome(
            stdout=stdout_for_format("claude", block=False),
            stderr="",
            exit_code=0,
            decision=_deny("shadow", f"gate escape: {exc}", t0=time.monotonic()),
        )


def main(argv: list[str] | None = None) -> int:
    import sys as _sys

    try:
        raw = _sys.stdin.buffer.read()
    except Exception:  # noqa: BLE001 — a broken stdin still gets a verdict
        raw = b""
    out = run_argv(list(_sys.argv[1:] if argv is None else argv), raw)
    try:
        if out.stdout:
            print(out.stdout)
        if out.stderr:
            print(out.stderr, file=_sys.stderr)
    except Exception:  # noqa: BLE001 — a broken stdout must not un-deny
        pass
    return out.exit_code
```

Note: `argparse` exits with `SystemExit(2)` on a bad argv; `BaseException` catches it and
the enforce branch denies. Keep `tests/test_gate_main.py` (or wherever `main` is tested)
green: the observable exit codes do not change.

3b. `gate_server.py`:

```python
"""The resident gate: one process holds the verifier warm; the hook talks to it.

One cold gate call is ~0.7 s and ~98% of it is import. Verification itself is
0.6 to 4 ms. The server amortizes the import; the client (gate_client.py) can
only fall back to the in-process gate, never to allow.
"""

from __future__ import annotations

import base64
import json
import os
import socketserver
import threading
from pathlib import Path

SOCK_NAME = "gate.sock"
_MAX_REQUEST = 4 * 1024 * 1024


class _Handler(socketserver.StreamRequestHandler):
    def handle(self) -> None:
        from opendaisugi.gate import run_argv

        line = self.rfile.readline(_MAX_REQUEST)
        try:
            req = json.loads(line)
            argv = [str(a) for a in req["argv"]]
            raw = base64.b64decode(req.get("stdin_b64", ""))
        except Exception:  # noqa: BLE001 — a bad request gets a deny, not a crash
            reply = {
                "v": 1,
                "stdout": "",
                "stderr": "openDaisugi gate: DENIED — bad request",
                "exit_code": 2,
            }
        else:
            out = run_argv(argv, raw)
            reply = {"v": 1, "stdout": out.stdout, "stderr": out.stderr, "exit_code": out.exit_code}
        self.wfile.write(json.dumps(reply).encode() + b"\n")


class _Server(socketserver.UnixStreamServer):
    allow_reuse_address = True


def serve(
    root: Path, *, ready: threading.Event | None = None, stop: threading.Event | None = None
) -> None:
    """Serve gate verdicts on ``<root>/gate.sock`` until ``stop`` is set (or forever).

    Sequential on purpose: verdicts take milliseconds and Z3 is not safe to
    share across threads. A stale socket file from a dead server is removed.
    """
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    sock = root / SOCK_NAME
    if sock.exists():
        sock.unlink()
    with _Server(str(sock), _Handler) as srv:
        os.chmod(sock, 0o600)
        srv.timeout = 0.2
        if ready is not None:
            ready.set()
        while stop is None or not stop.is_set():
            srv.handle_request()
    try:
        sock.unlink()
    except OSError:
        pass
```

3c. `gate_client.py` (stdlib only; it must not import `opendaisugi.gate` unless it falls back):

```python
"""The hook's entry: ask the resident gate, else run the gate in-process.

Stdlib only until the fallback, so the hot path is ~40 ms of Python startup
plus one socket round trip. Every failure of the server path (no socket,
refused, timeout, malformed reply) falls back to the real gate. There is no
path from a broken server to an allow.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import sys
from pathlib import Path

SOCK_NAME = "gate.sock"
_DEFAULT_ROOT = Path.home() / ".opendaisugi" / "gate"
_SERVER_TIMEOUT_S = 5.0  # well under any host hook timeout, so the fallback still fits


def _root_from_argv(argv: list[str]) -> Path:
    for i, a in enumerate(argv):
        if a == "--root" and i + 1 < len(argv):
            return Path(argv[i + 1])
        if a.startswith("--root="):
            return Path(a.split("=", 1)[1])
    return _DEFAULT_ROOT


def ask_server(
    sock_path: Path, argv: list[str], raw: bytes, *, timeout_s: float = _SERVER_TIMEOUT_S
) -> dict | None:
    """One round trip. None on any failure; the caller then runs the gate itself."""
    if not sock_path.exists():
        return None
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
            s.settimeout(timeout_s)
            s.connect(str(sock_path))
            req = {"v": 1, "argv": list(argv), "stdin_b64": base64.b64encode(raw).decode()}
            s.sendall(json.dumps(req).encode() + b"\n")
            buf = b""
            while not buf.endswith(b"\n"):
                chunk = s.recv(65536)
                if not chunk:
                    return None
                buf += chunk
        reply = json.loads(buf)
        if not isinstance(reply, dict) or reply.get("v") != 1:
            return None
        if not all(k in reply for k in ("stdout", "stderr", "exit_code")):
            return None
        if not isinstance(reply["exit_code"], int):
            return None
        return reply
    except (OSError, ValueError):
        return None


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    try:
        raw = sys.stdin.buffer.read()
    except Exception:  # noqa: BLE001
        raw = b""
    reply = ask_server(_root_from_argv(argv) / SOCK_NAME, argv, raw)
    if reply is None:
        from opendaisugi.gate import run_argv  # the slow, correct path

        out = run_argv(argv, raw)
        reply = {"stdout": out.stdout, "stderr": out.stderr, "exit_code": out.exit_code}
    try:
        if reply["stdout"]:
            print(reply["stdout"])
        if reply["stderr"]:
            print(reply["stderr"], file=sys.stderr)
    except Exception:  # noqa: BLE001 — a broken stdout must not un-deny
        pass
    return int(reply["exit_code"])


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
```

3d. In `gate_settings_json` (`gate.py:685`) change `-m opendaisugi.gate` to
`-m opendaisugi.gate_client`. Update the tests that pin the old string
(`grep -rn "m opendaisugi.gate " tests`). The `|| exit 2` logic is unchanged.

3e. `cli.py`: add under `gate_app`:

```python
@gate_app.command("serve")
def gate_serve_cmd(root: Path = _GATE_ROOT_OPT) -> None:
    """Run the resident gate in the foreground (Ctrl-C to stop).

    Hooks installed by `daisugi install --gate` try this server first and fall
    back to a cold in-process gate when it is not running. Verdicts are the
    same either way; only latency differs (~0.7 s cold, milliseconds warm).
    """
    from opendaisugi.gate_server import SOCK_NAME, serve

    typer.echo(f"gate: serving on {root / SOCK_NAME} (Ctrl-C to stop)", err=True)
    try:
        serve(root)
    except KeyboardInterrupt:
        typer.echo("", err=True)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_gate_resident.py tests/test_gate.py tests/test_hook_gate_contract.py tests/test_install_gate_baseurl.py -q`
Expected: PASS. Then measure and write down: `python -m opendaisugi.gate_client --mode enforce --root <r> < payload.json` with the server running, 20 calls, median wall clock.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gate.py src/opendaisugi/gate_server.py src/opendaisugi/gate_client.py src/opendaisugi/cli.py tests/test_gate_resident.py tests/
git commit -m "feat(gate): a resident gate server and a stdlib client that can only fall back, never allow"
```

---

### Task 8: ADR-0017 with the measured numbers

**Files:**
- Create: `docs/adr/0017-lazy-init-and-resident-gate.md`
- Modify: `docs/adr/README.md` (index line), `docs/harness/harness-comparison.md` (the "import-dominated (Z3)" note)

- [ ] **Step 1: Write the ADR** (follow `docs/adr/0016-*.md`: Context, Decision, Measured, Consequences)

```markdown
# ADR-0017 — Lazy package exports and a resident gate: the gate's cost was import, not Z3

- **Status:** Accepted
- **Date:** 2026-08-27

## Context

`import opendaisugi` ran 54 eager submodule imports (~530 ms). Z3 was 24 to 36 ms of it;
`agentic_executor` (342 ms cumulative), networkx (106 ms) and pydantic model building
(44 ms) were the rest. Every gate call is a fresh process, so every tool call paid it.
Verification itself is 0.6 to 4 ms. The harness docs and ADR-0007 attributed the cost to Z3.

## Decision

1. PEP 562 lazy exports in `opendaisugi/__init__.py`; the `Daisugi` facade moves to
   `facade.py`. Every public name still resolves.
2. `verify.py` imports networkx only at the DAG stage.
3. A resident gate (`daisugi gate serve`, `gate_server.py`) on `<root>/gate.sock` and a
   stdlib-only client (`gate_client.py`) that the installed hook runs. The client falls
   back to the in-process gate on any server failure. The fallback is the same
   `run_argv` the server uses, so the two cannot drift. Fail-closed is unchanged.

## Measured

| Probe | Before | After |
|---|---|---|
| `import opendaisugi` | ~530 ms | <fill> |
| `daisugi --help` (warm, median of 3) | 0.82 s | <fill> |
| `python -m opendaisugi.gate` cold call | ~0.70 s | <fill> (client, no server) |
| gate call with server (median of 20) | n/a | <fill> ms |

## Consequences

- The 30 s hook timeout in `gate_settings_json` can stay; a cold fallback still fits.
- sprig's hard-coded 10 s in `cli.go:76` is now comfortable; the 30 s in `sprig-hook`
  and `sprig-mcp` are conservative. Not changed here.
- A tool that imports `opendaisugi` and reads names via `getattr` at import time now
  pays on first access instead of at import. No known caller does.
```

- [ ] **Step 2: Fill the numbers, fix the harness note**

In `docs/harness/harness-comparison.md`, replace "the gate is import-dominated (Z3)" with
"the gate was import-dominated by the package init, not Z3 (ADR-0017); with the resident
gate a call is milliseconds".

- [ ] **Step 3: Commit**

```bash
git add docs/adr/0017-lazy-init-and-resident-gate.md docs/adr/README.md docs/harness/harness-comparison.md
git commit -m "docs(adr): ADR-0017 lazy exports and the resident gate, with the numbers"
```

---

## Self-review

- Spec coverage (§5): console (T1), root flags + boxes + clear-screen (T2), `--json`
  conformance (T3), lazy init (T4), networkx (T5), never silent (T6), resident gate (T7),
  ADR (T8). W5's `gate check --mode` validation and the mode on `gate status` are in T3.
- Placeholders: ADR numbers are marked `<fill>` on purpose; the task says to measure and
  fill them before committing. Task 4 gives the exact script that generates `_LAZY`.
- Types: `run_argv(argv, raw) -> GateOutcome` is used identically in server, client, and
  tests. `ask_server(sock_path, argv, raw, *, timeout_s)` matches all calls. `serve(root,
  *, ready, stop)` matches the fixture.
