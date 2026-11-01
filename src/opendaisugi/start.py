"""``daisugi start``: the one way in.

Four steps, each reported as done / skipped / failed / would. Idempotent: run
it twice and the second run skips what the first did. Never spends tokens.

The warden hook this installs is scoped to ``cwd`` — ``<cwd>/.claude/settings.json``,
never ``~/.claude/settings.json`` — and the envelope it registers is keyed to
``cwd`` too (not the shared ``default`` envelope). A machine-global hook whose
only envelope belonged to one project would deny every other project's
sessions the moment ``--enforce`` was flipped; scoping both the hook file and
the envelope key to the directory ``start`` was run in keeps two projects on
the same machine from ever seeing each other's envelope.
"""

from __future__ import annotations

import hashlib
import re
import shutil
import subprocess
import sys
import time
import warnings
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

_SAFE_KEY = re.compile(r"[^A-Za-z0-9._-]")


@dataclass(frozen=True)
class StartStep:
    key: str
    state: str  # done | skipped | failed | would
    text: str


@dataclass(frozen=True)
class StartOptions:
    cwd: Path
    data_dir: Path
    enforce: bool = False
    ask: bool = False
    no_ui: bool = False
    # How long to wait for the resident warden's socket to appear before
    # reporting the warden-server step "failed" instead of "done" (B-4: never
    # claim a daemon is up without seeing its socket).
    server_wait_s: float = 2.0
    # Only used to check for a pre-existing machine-global hook (`daisugi
    # install` writes here) so the hook step can say so honestly —
    # start itself never reads or writes under home.
    home: Path = field(default_factory=Path.home)
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


def _session_key(cwd: Path) -> str:
    """A filename-safe, human-greppable id for this directory's envelope/hook.

    Sanitized with the same charset ``warden._safe_session_id`` allows, so
    passing it through that sanitizer again downstream (which both
    ``register_envelope`` and the warden's own session lookup do) is a no-op —
    the file this writes and the file a live session looks up are guaranteed
    to be the same path.

    KNOWN LIMITATION (per-project pin). Under ``daisugi start`` the hook,
    envelope, AND session-tree are pinned to this per-directory key, not to the
    harness's own session id. So N concurrent Claude sessions in ONE project
    directory currently collapse to a single tree file and a single roster row,
    and their appends race the same file — a violation of
    ``SessionTree``'s single-writer-per-id assumption (which degrades to a fork,
    not corruption, but still merges distinct sessions on screen). The real
    Claude uuid is preserved on the tree header's ``harnessSessionId`` so the
    TUI's attach resumes the right session (``cockpit.SessionRow.harness_session_id``);
    a follow-on will give ``start`` per-session trees under one project envelope.
    ``daisugi install`` is unaffected — it passes no ``--session``, so its
    trees are keyed by the real uuid, one per session.
    """
    resolved = Path(cwd).resolve()
    name = _SAFE_KEY.sub("_", resolved.name).strip("._") or "start"
    digest = hashlib.sha256(str(resolved).encode()).hexdigest()[:8]
    return f"{name}-{digest}"


def _view_step(opts: StartOptions) -> StartStep:
    try:
        import textual  # noqa: F401

        text = "open the multi-session view (daisugi dashboard --tui)"
    except ImportError:
        text = (
            "open the live view (daisugi dashboard); install the tui extra for the full instrument"
        )
    return StartStep("view", "skipped" if opts.no_ui else "would", text)


def _step_up_note(opts: StartOptions, settings_path: Path, root: Path, session_key: str, act: bool):
    """The one-line note when `start --enforce` over an audit hook rewrites it
    to enforce in place (as `daisugi install --enforce` does), else None. A dry
    run tries the same on a copy and writes nothing. Never steps down."""
    import shutil
    import tempfile

    from opendaisugi import install as _inst

    def attempt(path: Path) -> str | None:
        _inst._WARDEN_NOTES.clear()
        with warnings.catch_warnings():
            warnings.simplefilter("ignore")
            _inst._patch_claude_warden(
                path, enforce=True, ask=opts.ask, root=root, session=session_key
            )
        notes = [n for n in _inst._WARDEN_NOTES if "now runs in enforce mode" in n]
        _inst._WARDEN_NOTES.clear()
        return notes[0] if notes else None

    if act:
        return attempt(settings_path)
    with tempfile.TemporaryDirectory() as tmp:
        copy = Path(tmp) / "settings.json"
        shutil.copy(settings_path, copy)
        note = attempt(copy)
    return note.replace(str(copy), str(settings_path)) if note else None


def _steps(opts: StartOptions, *, act: bool) -> list[StartStep]:
    from opendaisugi.config import installed_hook_mode
    from opendaisugi.install import _absolute_warden_root, _patch_claude_warden
    from opendaisugi.warden import _envelopes_dir, register_envelope, starter_envelope
    from opendaisugi.warden_server import SOCK_NAME, probe

    out: list[StartStep] = []
    root = opts.data_dir / "warden"
    mode = "enforce" if opts.enforce else "audit"
    settings_path = opts.cwd / ".claude" / "settings.json"
    session_key = _session_key(opts.cwd)

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
        # Nothing downstream has a session to hook, envelope, or serve — a
        # default envelope written now would just be an orphan the next
        # project's `start` might trip over. Report all four remaining keys
        # (the caller indexes by key) and stop acting.
        out.append(StartStep("hook", "skipped", "no harness to hook into"))
        out.append(
            StartStep("envelope", "skipped", "no harness session to register an envelope for")
        )
        out.append(StartStep("warden-server", "skipped", "no harness session needs it yet"))
        out.append(_view_step(opts))
        return out

    # The hook bakes the warden root; a relative one is refused before any
    # write, in a dry run too (a plan must not promise a hook it cannot write).
    _absolute_warden_root(root)

    global_settings_path = opts.home / ".claude" / "settings.json"
    global_note = ""
    if global_settings_path != settings_path:
        global_mode = installed_hook_mode(global_settings_path)
        if global_mode:
            global_note = (
                f" (note: a machine-global warden hook is ALSO installed at "
                f"{global_settings_path}, mode: {global_mode} — `daisugi install` "
                f"put it there; it fires on every directory's sessions, including this one)"
            )

    installed = installed_hook_mode(settings_path)
    stepped_note = (
        _step_up_note(opts, settings_path, root, session_key, act)
        if installed == "audit" and mode == "enforce"
        else None
    )
    if stepped_note:
        text = stepped_note if act else f"would change the warden hook: {stepped_note}"
        out.append(StartStep("hook", "done" if act else "would", f"{text}{global_note}"))
    elif installed:
        change_hint = (
            f"; it's already {installed} — to switch to {mode}, remove the warden hook line "
            f"from {settings_path} and run `daisugi start` again"
            if installed != mode
            else ""
        )
        out.append(
            StartStep(
                "hook",
                "skipped",
                f"a warden hook is already installed in {settings_path} "
                f"(mode: {installed}, this directory only){change_hint}{global_note}",
            )
        )
    elif act:
        settings_path.parent.mkdir(parents=True, exist_ok=True)
        _patch_claude_warden(
            settings_path, enforce=opts.enforce, ask=opts.ask, root=root, session=session_key
        )
        out.append(
            StartStep(
                "hook",
                "done",
                f"installed the warden hook in {mode} mode into {settings_path} "
                f"(this directory only){global_note} — a project-settings hook needs Claude "
                f"Code to trust this folder before it fires; confirm it did with "
                f"`daisugi warden report`",
            )
        )
    else:
        out.append(
            StartStep(
                "hook",
                "would",
                f"install the warden hook in {mode} mode into {settings_path} "
                f"(this directory only){global_note}",
            )
        )

    env_dir = _envelopes_dir(root)
    env_path = env_dir / f"{session_key}.json"
    if env_path.exists():
        out.append(
            StartStep(
                "envelope",
                "skipped",
                f"an envelope for this directory is already registered at {env_path}",
            )
        )
    elif act:
        register_envelope(starter_envelope(opts.cwd), session_id=session_key, root=root)
        out.append(
            StartStep(
                "envelope", "done", f"registered a starter envelope for {opts.cwd} at {env_path}"
            )
        )
    else:
        out.append(StartStep("envelope", "would", f"register a starter envelope for {opts.cwd}"))

    sock = root / SOCK_NAME
    # A socket file is not a running warden: a server stopped by SIGKILL (or
    # an older one stopped by Ctrl-C) leaves it behind. Only one that
    # answers is.
    state = probe(sock)
    if state == "live":
        out.append(StartStep("warden-server", "skipped", "the resident warden is already running"))
    elif act:
        if state == "stale":
            try:
                sock.unlink()
            except OSError:
                pass
        # The first server wins, so start fixes the same captures root the
        # fence gives its server, <warden root>/captures (FN-R-32, FN-R-35).
        opts.spawn(
            [
                sys.executable,
                "-m",
                "opendaisugi.cli",
                "warden",
                "serve",
                "--root",
                str(root),
                "--captures-root",
                str(root / "captures"),
            ]
        )
        t0 = time.monotonic()
        while not sock.exists() and time.monotonic() - t0 < opts.server_wait_s:
            time.sleep(0.05)
        if sock.exists():
            out.append(StartStep("warden-server", "done", f"started the resident warden on {sock}"))
        else:
            out.append(
                StartStep(
                    "warden-server",
                    "failed",
                    f"started `daisugi warden serve` but {sock} never appeared after "
                    f"{opts.server_wait_s:.0f}s. Run `daisugi warden serve --root {root}` "
                    f"in a terminal to see why it exited.",
                )
            )
    else:
        out.append(
            StartStep("warden-server", "would", "start the resident warden (daisugi warden serve)")
        )

    out.append(_view_step(opts))
    return out


def plan_start(opts: StartOptions) -> list[StartStep]:
    return _steps(opts, act=False)


def run_start(opts: StartOptions) -> list[StartStep]:
    return _steps(opts, act=True)
