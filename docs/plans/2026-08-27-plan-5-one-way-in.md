# Plan 5: One Way In (W4 + W3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A new operator types `daisugi start` and is watching gated work within a minute; the bare run says how; the top-level command list fits on one screen.

**Architecture:** `start.py` is a small planner that reports each step as done / skipped / failed / would (dry run). The CLI's top level is reduced with `hidden=True` and help panels; nothing is deleted except the three rival start commands. `help --all` prints everything.

**Tech Stack:** Python 3.12, Typer 0.23 (`rich_help_panel`, `hidden`), pytest `CliRunner`.

**Spec:** `docs/plans/2026-08-27-cockpit-spec.md` §8, §9, and §11 (cross-plan rules).

**Requires:** plans 1–3 shipped — `config.installed_hook_mode` + the `config` command
(plan 1), `gate_server.SOCK_NAME` + `gate serve` + the root output flags (plan 2),
`_patch_claude_gate(ask=)` + `gate_settings_json(ask=)` (plan 3). Do not build in isolation.

## Corrections from adversarial review (2026-08-27 — authoritative over the tasks below)

Dry-run safety is verified sound (one `act` flag gates all three writers; read helpers have
no side effects; `_envelopes_dir` is a pure path helper), the shadow default is honest on the
mode axis, hidden-but-reachable + did-you-mean-over-hidden are real (Typer suggests hidden
commands). Also: **route `start`'s step output through plan 2's `console.py`
(`say`/`note`), not raw `typer.echo`** (7 sites) — otherwise plan 5 re-breaks the pipe-safety
plan 2 just landed. Then:

- **BLOCKER B-1 (honesty + real breakage) — `start` claims per-directory scope but installs
  machine-global.** The hook installs into `~/.claude/settings.json` and the envelope registers
  with no `session_id` → `default.json`; but the docstring says "a gated session over *this
  directory*." So `daisugi start --enforce` in project A installs a global hook whose only
  envelope is A's, and every Claude Code session in project B then falls back to A's envelope and
  is denied. **Fix:** install the hook to `<cwd>/.claude/settings.json` and/or bind the envelope
  with the session id; at minimum make the step text honest ("every Claude Code session on this
  machine").
- **BLOCKER B-2 — wrong import.** `SOCK_NAME` lives in `gate_server`, not `gate_client`. Use
  `from opendaisugi.gate_server import SOCK_NAME`, or every `test_start.py` case ImportErrors.
- **BLOCKER B-3 — Tasks 1 and 3 commit a red suite.** `test_quickstart_is_gone_and_suggests_start`
  (green at Task 2) and `test_top_level_help_is_short` (green at Task 4) are committed before they
  pass. Mark each `@pytest.mark.xfail(strict=True, reason="green at Task N")` (§11.1 rule 2).
- **BLOCKER B-4 (honesty) — the gate-server step says "done" without confirming the daemon came
  up.** `_detach` spawns to `DEVNULL` and unconditionally reports "done"; `gate serve` is a plan-2
  deliverable, so a mis-ordered build spawns a process that dies and still says "done." Poll for
  `root / SOCK_NAME` for ~2 s; "done" only on appearance, else "failed" with a next action.
- **SHOULD-FIX — a failed harness step still writes state, then exits 1.** When `which("claude")`
  is `None`, the hook step is skipped but `run_start` keeps acting (writes `default.json`, spawns
  the daemon). Short-circuit the remaining acting steps once the harness step fails (§2 line 18).
- **SHOULD-FIX — Task 2 breaks 3 unlisted `gate quickstart` tests.** `test_cli_gate.py:295` and
  the two in `test_cli_gate_decomposition.py` invoke `gate quickstart`; repoint them to
  `gate init` (which carries the same `--allow-shell-decomposition` opt-in).
- **SHOULD-FIX — Task 5's reference grep misses `src/`.** After `setup`→`tiers setup`,
  `daisugi status` (cli.py:2819) and `tiers setup`'s own body (cli.py:2715) still print
  `daisugi setup …`; and `setup` gets *no* did-you-mean (difflib won't match `setup`→`tiers`).
  Extend the grep to `src/`, fix those strings, and either accept the clean break for `setup`
  explicitly or print guidance.
- **SHOULD-FIX SF-5 — reconcile the visible-command count to the spec's 10.** Spec §8.2 mandates
  exactly 10 visible (`start, status, dashboard, orchestrate, install, config, gate, pathways,
  journal, help`) with `onboard` hidden; Task 4 shows 14 (adds `run/verify/onboard/tend`) and
  Task 6 says 12. Hide `onboard`, `run`, `verify`, `tend` → 10.
- **NIT — the `_root` rewrite must preserve the eager `--version` callback (cli.py:59-68)** and
  merge plan 2's output-mode setup; don't paste the literal `...`. Gate `--ask` behind
  `--enforce` (it no-ops in shadow). Pass `--data-dir tmp` in `test_start_dry_run_*` (else it
  reads the real `~/.opendaisugi`). Keep the plan's `StartOptions`/single-`act` shape (spec §9
  corrected, §11.2).

## Global Constraints

- `uv run pytest -q` green and `uv run ruff check .` clean before each commit (line length 100).
- Commit messages: `type(scope): why`. No attribution trailers. Never push.
- Clean breaks, no alias period: the repo has one real user. Removed commands must produce did-you-mean suggestions pointing at the replacement.
- `start` changes only what it says it changed, prints one line per step, and never spends tokens.
- Did-you-mean for hidden commands must keep working (click suggests from `list_commands`, which includes hidden ones; verify with a test).

---

### Task 1: `daisugi start`

**Files:**
- Create: `src/opendaisugi/start.py`
- Modify: `src/opendaisugi/cli.py` (add `start` command)
- Test: `tests/test_start.py`, `tests/test_cli_start.py` (new)

**Interfaces:**
- Consumes: `install._patch_claude_gate(settings_path, *, enforce, ask)`, `gate.starter_envelope/register_envelope/_envelopes_dir`, `config.installed_hook_mode`, `gate_client.SOCK_NAME`, `shutil.which`.
- Produces: `StartStep(key, state, text)`, `plan_start(...)`, `run_start(...)`, `StartOptions`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_start.py
"""`daisugi start`: four steps, each reported, nothing hidden, nothing spent."""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi.start import StartOptions, plan_start, run_start


def _opts(tmp_path: Path, **kw) -> StartOptions:
    home = tmp_path / "home"
    (home / ".claude").mkdir(parents=True, exist_ok=True)
    return StartOptions(
        cwd=tmp_path / "proj",
        home=home,
        data_dir=home / ".opendaisugi",
        enforce=False,
        ask=False,
        which=kw.pop("which", lambda n: "/usr/bin/claude"),
        spawn=kw.pop("spawn", lambda argv: None),
        **kw,
    )


def test_plan_on_a_fresh_home_would_do_everything(tmp_path):
    (tmp_path / "proj").mkdir()
    steps = {s.key: s for s in plan_start(_opts(tmp_path))}
    assert steps["harness"].state == "done" and "claude" in steps["harness"].text
    assert steps["hook"].state == "would" and "shadow" in steps["hook"].text
    assert steps["envelope"].state == "would"
    assert steps["gate-server"].state == "would"
    assert steps["view"].state == "would"


def test_run_installs_hook_and_envelope_and_reports(tmp_path):
    (tmp_path / "proj").mkdir()
    spawned = []
    opts = _opts(tmp_path, spawn=spawned.append)
    steps = {s.key: s for s in run_start(opts)}
    settings = json.loads((opts.home / ".claude" / "settings.json").read_text())
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "opendaisugi.gate" in cmd and "--mode shadow" in cmd
    assert steps["hook"].state == "done"
    assert (opts.data_dir / "gate" / "envelopes" / "default.json").exists()
    assert steps["envelope"].state == "done" and str(tmp_path / "proj") in steps["envelope"].text
    assert steps["gate-server"].state == "done" and spawned and "serve" in " ".join(spawned[0])
    assert steps["view"].state in ("would", "done")


def test_second_run_skips_what_exists(tmp_path):
    (tmp_path / "proj").mkdir()
    opts = _opts(tmp_path)
    run_start(opts)
    steps = {s.key: s for s in run_start(opts)}
    assert steps["hook"].state == "skipped" and "already installed" in steps["hook"].text
    assert steps["envelope"].state == "skipped"


def test_no_claude_is_a_failed_step_with_a_fix(tmp_path):
    (tmp_path / "proj").mkdir()
    steps = {s.key: s for s in run_start(_opts(tmp_path, which=lambda n: None))}
    assert steps["harness"].state == "failed"
    assert "PATH" in steps["harness"].text and "install" in steps["harness"].text.lower()
    assert steps["hook"].state == "skipped"  # nothing to hook into


def test_enforce_flag_installs_enforce(tmp_path):
    (tmp_path / "proj").mkdir()
    opts = _opts(tmp_path)
    opts = StartOptions(**{**opts.__dict__, "enforce": True})
    run_start(opts)
    settings = json.loads((opts.home / ".claude" / "settings.json").read_text())
    assert "--mode enforce" in settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
```

```python
# tests/test_cli_start.py
from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_start_help():
    res = runner.invoke(app, ["start", "--help"])
    assert res.exit_code == 0 and "--dry-run" in res.output and "--enforce" in res.output


def test_start_dry_run_prints_the_steps(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    monkeypatch.setattr("shutil.which", lambda n: "/usr/bin/claude")
    res = runner.invoke(app, ["start", "--dry-run", "--no-ui"])
    assert res.exit_code == 0, res.output
    for key in ("harness", "hook", "envelope", "gate-server", "view"):
        assert key in res.output
    assert not (tmp_path / ".claude" / "settings.json").exists()


def test_quickstart_is_gone_and_suggests_start():
    res = runner.invoke(app, ["quickstart"])
    assert res.exit_code != 0
    assert "start" in res.output
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_start.py tests/test_cli_start.py -q`
Expected: FAIL with `ModuleNotFoundError: opendaisugi.start`.

- [ ] **Step 3: Write minimal implementation**

```python
# src/opendaisugi/start.py
"""`daisugi start`: the one way in.

Four steps, each reported as done / skipped / failed / would. Idempotent: run
it twice and the second run skips what the first did. Never spends tokens.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path


@dataclass(frozen=True)
class StartStep:
    key: str
    state: str  # done | skipped | failed | would
    text: str


@dataclass(frozen=True)
class StartOptions:
    cwd: Path
    home: Path
    data_dir: Path
    enforce: bool = False
    ask: bool = False
    no_ui: bool = False
    # Looked up at call time (not captured at import) so tests can patch shutil.which.
    which: Callable[[str], str | None] = field(default=lambda name: shutil.which(name))
    spawn: Callable[[list[str]], None] = field(default=lambda argv: _detach(argv))


def _detach(argv: list[str]) -> None:
    subprocess.Popen(
        argv,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )


def _steps(opts: StartOptions, *, act: bool) -> list[StartStep]:
    from opendaisugi.config import installed_hook_mode
    from opendaisugi.gate import _envelopes_dir, register_envelope, starter_envelope
    from opendaisugi.gate_client import SOCK_NAME
    from opendaisugi.install import _patch_claude_gate

    out: list[StartStep] = []
    root = opts.data_dir / "gate"
    mode = "enforce" if opts.enforce else "shadow"

    claude = opts.which("claude")
    if claude:
        out.append(StartStep("harness", "done", f"Claude Code found at {claude}"))
    else:
        out.append(
            StartStep(
                "harness",
                "failed",
                "Tried to find the agent harness. No `claude` on PATH. "
                "Install Claude Code, then run `daisugi start` again.",
            )
        )

    settings = opts.home / ".claude" / "settings.json"
    installed = installed_hook_mode(settings)
    if not claude:
        out.append(StartStep("hook", "skipped", "no harness to hook into"))
    elif installed:
        out.append(StartStep("hook", "skipped", f"gate hook already installed (mode: {installed})"))
    elif act:
        _patch_claude_gate(settings, enforce=opts.enforce, ask=opts.ask)
        out.append(
            StartStep("hook", "done", f"installed the gate hook in {mode} mode into {settings}")
        )
    else:
        out.append(
            StartStep("hook", "would", f"install the gate hook in {mode} mode into {settings}")
        )

    env_dir = _envelopes_dir(root)
    if (env_dir / "default.json").exists():
        out.append(StartStep("envelope", "skipped", "a default envelope is registered"))
    elif act:
        register_envelope(starter_envelope(opts.cwd), root=root)
        out.append(StartStep("envelope", "done", f"registered a starter envelope for {opts.cwd}"))
    else:
        out.append(StartStep("envelope", "would", f"register a starter envelope for {opts.cwd}"))

    if (root / SOCK_NAME).exists():
        out.append(StartStep("gate-server", "skipped", "the resident gate is running"))
    elif act:
        opts.spawn([sys.executable, "-m", "opendaisugi.cli", "gate", "serve", "--root", str(root)])
        out.append(
            StartStep("gate-server", "done", "started the resident gate (daisugi gate serve)")
        )
    else:
        out.append(
            StartStep("gate-server", "would", "start the resident gate (daisugi gate serve)")
        )

    try:
        import textual  # noqa: F401

        view = "open the multi-session view (daisugi dashboard --tui)"
    except ImportError:
        view = (
            "open the live view (daisugi dashboard); install the tui extra for the full instrument"
        )
    out.append(StartStep("view", "skipped" if opts.no_ui else "would", view))
    return out


def plan_start(opts: StartOptions) -> list[StartStep]:
    return _steps(opts, act=False)


def run_start(opts: StartOptions) -> list[StartStep]:
    return _steps(opts, act=True)
```

CLI command:

```python
@app.command("start", rich_help_panel="Start here")
def start_cmd(
    enforce: bool = typer.Option(
        False, "--enforce", help="Install the gate in enforce mode (default: shadow)."
    ),
    ask: bool = typer.Option(
        False, "--ask", help="Let the gate hand a would-deny to you in the view."
    ),
    no_ui: bool = typer.Option(False, "--no-ui", help="Do the setup, do not open the view."),
    dry_run: bool = typer.Option(
        False, "--dry-run", help="Show the steps and their state; change nothing."
    ),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Get a gated session over this directory and watch it. One command, four steps.

    1. find the harness (Claude Code)  2. install the gate hook (shadow by default)
    3. register a starter envelope for this directory  4. start the resident gate,
    then open the multi-session view. Run it again any time: it skips what is done.
    """
    from opendaisugi.start import StartOptions, plan_start, run_start

    opts = StartOptions(
        cwd=Path.cwd(), home=Path.home(), data_dir=data_dir, enforce=enforce, ask=ask, no_ui=no_ui
    )
    steps = plan_start(opts) if dry_run else run_start(opts)
    width = max(len(s.key) for s in steps)
    for s in steps:
        typer.echo(f"  {s.key:<{width}}  {s.state:<7}  {s.text}")
    if any(s.state == "failed" for s in steps):
        raise typer.Exit(code=1)
    if dry_run or no_ui:
        return
    view = next(s for s in steps if s.key == "view")
    if "--tui" in view.text:
        from opendaisugi.tui import run_tui

        run_tui(data_dir)
    else:
        from opendaisugi.dashboard import run_live

        run_live(data_dir)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_start.py tests/test_cli_start.py -q`
Expected: PASS except `test_quickstart_is_gone_and_suggests_start` (Task 2).

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/start.py src/opendaisugi/cli.py tests/test_start.py tests/test_cli_start.py
git commit -m "feat(cli): daisugi start, the one way in: hook, envelope, resident gate, view"
```

---

### Task 2: Remove `quickstart` and `gate quickstart`

**Files:**
- Modify: `src/opendaisugi/cli.py:2552-2599` (delete `quickstart_cmd`), `:718-774` (delete `gate quickstart`)
- Delete: `tests/test_cli_quickstart.py` (its transcript-discovery assertions move to `tests/test_onboarding_status.py` if not already covered)
- Modify: `README.md`, `docs/` (every `daisugi quickstart` mention → `daisugi start`)

- [ ] **Step 1: Delete the commands and fix references**

Run: `grep -rn "quickstart" src docs README.md AGENTS.md tests | grep -v "^docs/research\|^docs/plans"`.
Replace every user-facing mention with `daisugi start` (or `daisugi start --dry-run` where
the text meant "orient me without changing anything"). Delete the two functions.

- [ ] **Step 2: Run the tests**

Run: `uv run pytest tests/test_cli_start.py tests/test_cli.py tests/test_cli_gate.py -q`
Expected: PASS, including `test_quickstart_is_gone_and_suggests_start` (Typer prints
`No such command 'quickstart'` and a `Did you mean` line that includes `start`).

- [ ] **Step 3: Commit**

```bash
git add -A src/opendaisugi/cli.py tests README.md docs AGENTS.md
git commit -m "refactor(cli): remove quickstart and gate quickstart; start is the one way in"
```

---

### Task 3: The bare run says how to start; `help --all` lists everything

**Files:**
- Modify: `src/opendaisugi/cli.py:42-46` (Typer app), `:58-69` (root callback), add `help` command
- Test: `tests/test_cli_bare_run.py` (new)

**Interfaces:**
- Produces: bare `daisugi` → the start-here text (exit 0); `daisugi help` → same as bare; `daisugi help --all` → full command list grouped by panel.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_cli_bare_run.py
"""The bare run is short, shows examples, and points at help --all."""

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_bare_run_is_short_and_actionable():
    res = runner.invoke(app, [])
    assert res.exit_code == 0, res.output
    lines = [ln for ln in res.output.splitlines() if ln.strip()]
    assert len(lines) <= 25, res.output
    assert "daisugi start" in res.output
    assert "daisugi orchestrate" in res.output
    assert "daisugi help --all" in res.output
    assert "╭" not in res.output and "│" not in res.output


def test_help_all_lists_every_command():
    res = runner.invoke(app, ["help", "--all"])
    assert res.exit_code == 0, res.output
    for name in (
        "start",
        "status",
        "orchestrate",
        "install",
        "config",
        "gate",
        "pathways",
        "journal",
        "onboard",
        "tend",
        "route",
        "viz",
        "metrics",
        "lora",
        "release",
        "batch",
        "conformance",
        "registry",
        "hook",
        "mcp",
        "tiers",
        "gardener",
        "gateway",
        "models",
    ):
        assert name in res.output, name


def test_top_level_help_is_short():
    res = runner.invoke(app, ["--help"])
    assert res.exit_code == 0
    for hidden in ("lora", "release", "batch", "conformance", "viz", "metrics"):
        assert hidden not in res.output, f"{hidden} should be hidden from --help"
    assert "start" in res.output and "gate" in res.output
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_bare_run.py -q`
Expected: FAIL (the bare run prints the full help; `help` is unknown).

- [ ] **Step 3: Write minimal implementation**

```python
app = typer.Typer(
    name="daisugi",
    help="Runtime assurance for agent actions.",
    no_args_is_help=False,
    invoke_without_command=True,
    pretty_exceptions_enable=False,
)

_START_HERE = """\
daisugi: a gate that proves each agent action stays inside an envelope, and a garden
of reusable pathways distilled from the work.

Start here
  daisugi start                          gate this directory (shadow mode) and watch it
  daisugi start --enforce                the same, but out-of-envelope calls are denied
  daisugi orchestrate "list three risks" run a prompt end to end under a verified plan
  daisugi status                         is this machine ready; what is installed
  daisugi dashboard                      the live view of sessions, verdicts, and cost

More
  daisugi config                         every setting, with its source
  daisugi help --all                     every command, grouped
"""


@app.callback()
def _root(
    ctx: typer.Context,
    version: bool = ...,
    plain: bool = ...,
    quiet: bool = ...,
    verbose: bool = ...,
    no_color: bool = ...,
) -> None:
    """Runtime assurance for agent actions."""
    ...  # the plan 2 output-mode setup stays
    if ctx.invoked_subcommand is None:
        typer.echo(_START_HERE.rstrip())
        raise typer.Exit()


@app.command("help", rich_help_panel="Start here")
def help_cmd(
    ctx: typer.Context,
    show_all: bool = typer.Option(False, "--all", help="List every command, grouped."),
) -> None:
    """Show the short start-here text, or every command with --all."""
    if not show_all:
        typer.echo(_START_HERE.rstrip())
        return
    root = ctx.parent or ctx
    command = typer.main.get_command(app)
    typer.echo(command.get_help(root))
    hidden = [name for name, cmd in command.commands.items() if getattr(cmd, "hidden", False)]
    if hidden:
        typer.echo("\nMore commands (hidden from the short list):")
        for name in sorted(hidden):
            typer.echo(f"  {name:<20} {command.commands[name].get_short_help_str(60)}")
            sub = command.commands[name]
            for sub_name, sub_cmd in sorted(getattr(sub, "commands", {}).items()):
                typer.echo(f"    {name} {sub_name:<16} {sub_cmd.get_short_help_str(56)}")
```

If `get_help` renders rich panels with box drawing, pass the plain glyph mode from
`console` (plan 2) or call `command.format_help` with a plain `click.HelpFormatter`;
the bare run must contain no box drawing.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_bare_run.py tests/test_cli.py -q`
Expected: PASS except `test_top_level_help_is_short` (Task 4).

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_bare_run.py
git commit -m "feat(cli): the bare run says how to start; help --all lists everything"
```

---

### Task 4: A short top level: hidden groups and help panels

**Files:**
- Modify: `src/opendaisugi/cli.py` (every `typer.Typer(...)` at :71-235 and every top-level `@app.command`)
- Test: `tests/test_cli_bare_run.py` (from Task 3), `tests/test_cli_suggest.py` (new)

- [ ] **Step 1: Write the failing test**

```python
# tests/test_cli_suggest.py
"""Hidden commands still work and are still suggested on a typo."""

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_hidden_group_still_runs():
    assert runner.invoke(app, ["lora", "--help"]).exit_code == 0


def test_typo_of_a_hidden_command_is_suggested():
    res = runner.invoke(app, ["relaese"])
    assert res.exit_code != 0
    assert "release" in res.output
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run pytest tests/test_cli_suggest.py tests/test_cli_bare_run.py -q`
Expected: `test_top_level_help_is_short` FAILS (everything is visible); the suggest tests pass already.

- [ ] **Step 3: Write minimal implementation**

Visible top level, with panels:

| Panel | Commands |
|---|---|
| Start here | `start`, `help`, `status`, `dashboard` |
| Run | `orchestrate`, `run`, `verify` |
| Gate | `gate` (group), `install` |
| Garden | `pathways` (group), `journal` (group), `onboard`, `tend` |
| Settings | `config` |

Everything else gets `hidden=True`: groups `lora`, `release`, `batch`, `conformance`,
`registry`, `hook`, `mcp`, `tiers`, `gardener`; commands `generate-envelope`, `route`,
`gateway`, `distill-repeats`, `gateway-report`, `viz`, `models`, `metrics`, `modules`.
Apply `rich_help_panel="..."` on the visible ones (`add_typer(..., rich_help_panel=...)`
for groups). `hidden=True` on `add_typer` and `@app.command`. Leave the group-internal
commands as they are.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run pytest tests/test_cli_bare_run.py tests/test_cli_suggest.py tests/test_cli.py tests/test_cli_install_help.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_suggest.py
git commit -m "feat(cli): a one-screen top level; the rest is hidden but reachable and suggested"
```

---

### Task 5: `setup` becomes `tiers setup`; the truth in the docs

**Files:**
- Modify: `src/opendaisugi/cli.py:2602-2740` (`setup_cmd` re-registered under `tiers_app` as `setup`), `:1-15` (module docstring)
- Modify: `README.md`, `AGENTS.md`, `docs/README.md`, `docs/how-to/*` (every `daisugi setup` → `daisugi tiers setup`; the quick start → `daisugi start`)
- Test: `tests/test_cli_setup.py` (invocations become `["tiers", "setup", ...]`)

- [ ] **Step 1: Move the command and rewrite the docstring**

`@tiers_app.command("setup")` with the same body. The module docstring becomes:

```python
"""opendaisugi command-line interface.

One entry: ``daisugi start``. The short top level is Start here / Run / Gate /
Garden / Settings; ``daisugi help --all`` lists every command, including the
hidden operational groups. Read commands take ``--json``; the root takes
``--plain``, ``-q``, ``-v``, ``--no-color``. The gate group uses ``--root``
(``~/.opendaisugi/gate``); most others take ``--data-dir`` (``~/.opendaisugi``).
"""
```

- [ ] **Step 2: Fix the docs and tests**

Run: `grep -rn "daisugi setup\|daisugi quickstart\|daisugi onboard" README.md AGENTS.md docs --include=*.md | grep -v "docs/research\|docs/plans"`.
Update each. In `README.md` the quick start is three lines: install, `daisugi start`,
what you will see. In `AGENTS.md` the command map lists the five panels.

- [ ] **Step 3: Run the suite**

Run: `uv run pytest -q && uv run ruff check .`
Expected: green.

- [ ] **Step 4: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_setup.py README.md AGENTS.md docs
git commit -m "refactor(cli): setup lives under tiers; docs and the module docstring tell the truth"
```

---

### Task 6: Record W4 and W3 in the plan file

**Files:**
- Modify: `docs/plans/2026-08-26-daisugi-interface-two-lenses.md` (W3, W4)

- [ ] **Step 1: Add the shipped notes**

Under W4: "**Shipped (plan 5):** `daisugi start` (four reported steps, idempotent,
`--dry-run`); `quickstart` and `gate quickstart` removed; `setup` → `tiers setup`."
Under W3: "**Shipped (plan 5):** bare run is a 25-line start-here; 12 visible commands in
five panels; the rest hidden but reachable and suggested; `help --all`."
Then re-run the two lenses' probes from the Evidence table and record the after numbers
beside the before numbers.

- [ ] **Step 2: Commit**

```bash
git add docs/plans/2026-08-26-daisugi-interface-two-lenses.md
git commit -m "docs(plans): record plans 1 to 5 as shipped with the after numbers"
```

---

## Self-review

- Spec coverage (§8): `start` with four reported steps and dry run (T1), removal of the rivals (T2, T5), bare run and `help --all` (T3), hidden top level with panels and working suggestions (T4), docstring and docs (T5), after numbers (T6). `start` also launches the resident gate (plan 2) so the first gated call is fast.
- Placeholders: Task 3's `_root` signature elides the plan-2 options with `...` on purpose because plan 2 defines them; the new lines are given in full.
- Types: `StartOptions` fields match every test; `StartStep.state` values are exactly `done|skipped|failed|would` everywhere.
