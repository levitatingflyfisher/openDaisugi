"""Plug-and-play installation of openDaisugi into agent runtimes.

``daisugi install`` detects Claude Code, Codex, Hermes, and OpenClaw on the
local machine and installs five layers per harness by default:

  - Skill: symlink the bundled opendaisugi-checklist skill into the harness's
    discovery path (cross-vendor ~/.agents/skills first). Discovered on demand
    — no per-session token cost.
  - MCP: register the ``daisugi mcp serve`` tool server (per-harness syntax).
  - Capture: a pre-tool-call hook feeding distillation (per-harness surface).
  - Instructions: append pathway-routing guidance to the harness's always-on
    instructions file (CLAUDE.md / AGENTS.md).
  - Warden: the ADR-0007 fail-closed PreToolUse verify hook (Claude Code and
    Codex). It runs in audit mode: it evaluates every call, logs what enforce
    would deny, and always allows (ADR-0025). ``--enforce`` is the one opt-in
    to blocking.

One more layer is opt-in, since it is cost relevant and must never be
installed by surprise (ADR-0013, "one install that both saves and verifies"):

  - Base URL: points the harness at the local token-saving router
    (ADR-0012). Claude Code (env var) and OpenClaw (a registered provider,
    both speak Anthropic Messages) are wired; Codex and Hermes are not (their
    custom endpoints expect an OpenAI wire) — this is surfaced honestly via
    ``Runtime.unsupported_layers()``, never silently skipped.

All writes are idempotent, backed up before modification, and reversible via
``daisugi install --uninstall``.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import shutil
import sys
import time
import warnings
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Protocol

import yaml

from opendaisugi.config import (
    _segments,
    _skip_env,
    command_tokens,
    is_entry_name,
    is_record_hook,
    record_hook_programs,
    warden_hook_args,
    warden_hook_kind,
)

_CLAUDE_MD_MARKER = "<!-- opendaisugi-managed -->"

_CLAUDE_MD_BLOCK = f"""\
{_CLAUDE_MD_MARKER}
## openDaisugi — automatic pathway routing

Before planning any task with 3 or more steps, call the `find_pathway`
MCP tool first. If similarity ≥ 0.85, use the returned cached plan via
`run_plan` instead of re-planning from scratch. When a cached pathway is
used, say so: "Using cached opendaisugi pathway (similarity X.XX) —
skipping re-plan."

A job you will repeat goes through `verify_plan`, then `run_plan`. Make
one action in each tool call.

If no pathway matches, proceed normally.
{_CLAUDE_MD_MARKER}
"""

# ---------------------------------------------------------------------------
# The program the agent's hooks and MCP entries run
# ---------------------------------------------------------------------------


def daisugi_exe() -> str:
    """The absolute path of the installed ``daisugi`` entry point.

    An agent starts its hooks and MCP servers with its own PATH, which often
    lacks ``~/.local/bin``. A bare ``daisugi`` then fails with "command not
    found" and the hook fails open. So install writes this path instead. It
    is the path this process was started by when that is a daisugi entry
    point (symlinks kept, so an upgrade that swaps the file behind a stable
    link leaves the hook working), else what PATH finds. When neither exists
    it returns the bare name, and install names that in its check.
    """
    arg0 = sys.argv[0] if sys.argv else ""
    if arg0 and _is_entry_word(arg0) and Path(arg0).exists():
        return os.path.abspath(arg0)
    found = shutil.which("daisugi")
    if found:
        return os.path.abspath(found)
    return "daisugi"


def _is_entry_word(word: object) -> bool:
    """A program name that is a daisugi entry point: one of daisugi,
    daisugi-py, daisugi-go and daisugi-rs, by itself or as the last part of a
    path. A user's own ``daisugi-local`` is not ours."""
    return is_entry_name(word)


def _record_command(args: str) -> str:
    """``<daisugi> hook record <args>`` with the absolute entry point."""
    return f"{shlex.quote(daisugi_exe())} hook record {args}"


def _mcp_entry(**extra: object) -> dict:
    return {**extra, "command": daisugi_exe(), "args": ["mcp", "serve"]}


#: A note that says a hook was not rewritten contains this phrase.
LEFT_AS_IS = "was left as it is"

# One plain command: a program word (bare, or one single-quoted path), then
# plain words. No operator, redirection, quote, expansion, newline or
# NAME=val prefix can match, so the rest of the command is safe to keep as
# it is. The tail holds only the characters install writes in its own flags.
_PLAIN_RECORD = re.compile(
    r"""(?P<lead>[ \t]*)(?P<prog>'[^']*'|[^ \t\n\r\f\v'"\\;|&<>()$`]+)"""
    r"""(?P<tail>(?:[ \t]+[A-Za-z0-9_./:=@%+,-]+)*[ \t]*)"""
)


def _rebind_record_command(command: str) -> tuple[str, bool]:
    """A ``daisugi hook record ...`` command pointed at the absolute entry
    point, and whether it had to be left alone.

    Only one plain command is rewritten, and only its program word: every
    other byte stays. A command with an operator, a redirection, an ``env``
    or ``NAME=val`` prefix, a quote or an expansion (``... || true``,
    ``... 2>/dev/null``) comes back as it was with ``left`` True, unless its
    program is already the entry point. Any other command comes back as it
    was with ``left`` False.
    """
    programs = record_hook_programs(command)
    if not programs:
        return command, False
    exe = daisugi_exe()
    m = _PLAIN_RECORD.fullmatch(command)
    if m:
        prog = m.group("prog")
        word = prog[1:-1] if prog.startswith("'") else prog
        if _is_entry_word(word) and m.group("tail").split()[:2] == ["hook", "record"]:
            if word == exe:
                return command, False
            if "/" in word and "/" not in exe:
                # Install found no path to write. A bare word is less than
                # the path the hook has now: keep the path and say so.
                return command, True
            return m.group("lead") + shlex.quote(exe) + m.group("tail"), False
    return command, any(p != exe for p in programs)


def _keeps_path(old: object, want: object) -> bool:
    """True when install would write a bare word over a command that names a
    path. It never does: a bare word is less than the path."""
    return isinstance(old, str) and "/" in old and isinstance(want, str) and "/" not in want


def _left_mcp_note(path: Path, old: str, want: str) -> str:
    return (
        f"The MCP command `{old}` in {path} {LEFT_AS_IS}: install found no daisugi on PATH, "
        f"so the only word it could write is `{want}`, and the entry already names a path."
    )


def _left_hook_note(runtime: str, command: str) -> str:
    m = _PLAIN_RECORD.fullmatch(command)
    if m and "/" in m.group("prog").strip("'") and "/" not in daisugi_exe():
        return (
            f"[{runtime}] The hook `{command}` {LEFT_AS_IS}: install found no daisugi on PATH, "
            f"so the only word it could write is `{daisugi_exe()}`, and the hook already names "
            f"a path."
        )
    return (
        f"[{runtime}] The hook `{command}` {LEFT_AS_IS}: it is not one plain command, so "
        f"install did not rewrite it. If the agent cannot find daisugi, make its first "
        f"word {shlex.quote(daisugi_exe())}."
    )


def _rebind_hook_entries(entries: list, runtime: str) -> bool:
    """Point every record hook in a list of ``{"hooks": [...]}`` entries (or
    flat ``{"command": ...}`` entries) at the absolute entry point. True when
    one changed. A hook that is more than one plain command stays as it is,
    and a note names the runtime."""
    changed = False
    for entry in entries:
        if not isinstance(entry, dict):
            continue
        holders = [entry] if "command" in entry else entry.get("hooks", [])
        for h in holders:
            if not isinstance(h, dict):
                continue
            cmd = h.get("command")
            if isinstance(cmd, str) and is_record_hook(cmd):
                new, left = _rebind_record_command(cmd)
                if left:
                    _WARDEN_NOTES.append(_left_hook_note(runtime, cmd))
                if new != cmd:
                    h["command"] = new
                    changed = True
    return changed


def _pretooluse_hook() -> dict:
    return {
        "matcher": "Bash|Edit|Write|Read|Glob|Grep|WebFetch|WebSearch",
        "hooks": [{"type": "command", "command": _record_command("--format claude")}],
    }


# The skill directory name, shared across every harness target.
_SKILL_NAME = "opendaisugi-checklist"


def _agents_skill_target(home: Path, fallback_subdir: str) -> Path:
    """Prefer the cross-vendor ~/.agents/skills path; fall back per-vendor.

    Uses ~/.agents/skills when it already exists, or when the per-vendor
    fallback directory is absent (so a fresh install lands in the
    cross-vendor location by default).
    """
    agents = home / ".agents" / "skills"
    if agents.exists() or not (home / fallback_subdir).exists():
        return agents / _SKILL_NAME
    return home / fallback_subdir / _SKILL_NAME


def _link_skill(target: Path) -> Path:
    """Symlink (or copy on zipimport) the bundled skill into an explicit target.

    The single skill-install primitive — all four runtimes route through it. On
    zipimport the source has no real path; the sentinel is a guaranteed-absent
    sibling that forces SkillInstaller's copy branch (its location is never read).
    """
    from opendaisugi.skill_paths import SkillInstaller, resolve_skill_dir

    try:
        return SkillInstaller(resolve_skill_dir()).link(target)
    except FileNotFoundError:
        return SkillInstaller(target.with_name("_zipimport_sentinel")).link(target)


def _install_skill(home: Path, fallback_subdir: str) -> Path:
    """Install the skill into the cross-vendor (or per-vendor fallback) path."""
    return _link_skill(_agents_skill_target(home, fallback_subdir))


# ---------------------------------------------------------------------------
# Data types
# ---------------------------------------------------------------------------


class Layer(str, Enum):
    SKILL = "skill"
    MCP = "mcp"
    CAPTURE = "capture"
    INSTRUCTIONS = "instructions"
    WARDEN = "warden"
    BASE_URL = "base_url"
    PANDO = "pando"


# The six layers a plain install writes. The warden hook is one of them, in
# audit mode (ADR-0025). PANDO is the memory: its MCP server, chat hooks,
# Hermes plugin and follow service. BASE_URL is opt-in only: never add it here.
DEFAULT_LAYERS: frozenset[Layer] = frozenset(
    {Layer.SKILL, Layer.MCP, Layer.CAPTURE, Layer.INSTRUCTIONS, Layer.WARDEN, Layer.PANDO}
)

# The names a user gives `daisugi install --only`: the layers under their
# user-facing names, in the order the plan and the summary list them. Each row
# is (name, layer, what it writes, the harnesses that carry it).
COMPONENTS: tuple[tuple[str, Layer, str, str], ...] = (
    (
        "skill",
        Layer.SKILL,
        "the opendaisugi-checklist skill directory",
        "claude codex hermes openclaw",
    ),
    ("mcp", Layer.MCP, "the opendaisugi MCP server entry", "claude codex hermes openclaw"),
    (
        "capture",
        Layer.CAPTURE,
        "the passive capture hook (daisugi hook record)",
        "claude hermes openclaw",
    ),
    (
        "instructions",
        Layer.INSTRUCTIONS,
        "the pathway note in the harness instructions file",
        "claude codex hermes openclaw",
    ),
    (
        "warden",
        Layer.WARDEN,
        "the warden hook, audit mode; --enforce makes it deny",
        "claude codex",
    ),
    (
        "pando",
        Layer.PANDO,
        "the Pando memory: MCP server, chat hooks, Hermes plugin, follow service",
        "claude codex hermes",
    ),
    (
        "router",
        Layer.BASE_URL,
        "the harness base_url pointed at the router",
        "claude codex openclaw",
    ),
)
DEFAULT_COMPONENTS: tuple[str, ...] = (
    "skill",
    "mcp",
    "capture",
    "instructions",
    "warden",
    "pando",
)
_COMPONENT_NAMES = ", ".join(c[0] for c in COMPONENTS)


def component_layers(names: list[str]) -> "set[Layer]":
    """The layers the component names select. Names split on commas.

    Raises ``ValueError`` on a name that is not a component.
    """
    by_name = {c[0]: c[1] for c in COMPONENTS}
    out: set[Layer] = set()
    for raw in names:
        for token in raw.split(","):
            name = token.strip()
            if name not in by_name:
                raise ValueError(
                    f"unknown component '{name}'. Components: {_COMPONENT_NAMES}. "
                    "Run: daisugi install --list"
                )
            out.add(by_name[name])
    return out


def component_names(layers: "set[Layer]") -> list[str]:
    """The component names of a set of layers, in COMPONENTS order."""
    return [c[0] for c in COMPONENTS if c[1] in layers]


def list_components() -> str:
    """The table `daisugi install --list` prints."""
    rows = [f"{name:<13}{what} ({where})" for name, _layer, what, where in COMPONENTS]
    return "\n".join(
        [
            *rows,
            "",
            f"Default: {', '.join(DEFAULT_COMPONENTS)}. Add router with --router or --only router.",
            "The pi and OpenCode extensions are separate: daisugi install --harness pi|opencode.",
        ]
    )


# The router's default bind address (mirrors `daisugi router serve`'s --port 8787).
DEFAULT_ROUTER_BASE_URL = "http://127.0.0.1:8787"


def _resolve_layers(layers: "set[Layer] | None") -> "set[Layer]":
    """``None`` means "the default six" — never BASE_URL by accident."""
    return set(DEFAULT_LAYERS) if layers is None else set(layers)


@dataclass
class InstallStep:
    layer: Layer
    description: str
    target: Path | None = None
    # False marks a "capability gap" note (an unsupported (runtime, layer)
    # combo surfaced honestly rather than silently skipped) rather than a
    # real, applied change — _format_summary and the CLI must not count
    # these toward "N change(s)".
    supported: bool = True


@dataclass
class InstallResult:
    dry_run: bool
    planned: list[InstallStep]
    modified_files: list[Path]
    summary: str
    # "<runtime>: <why>" for each runtime whose apply raised; nothing was
    # written for it. The CLI names each one and exits 1.
    failures: list[str] = field(default_factory=list)
    # One line each for a warden hook left as it was on purpose or stepped up
    # from audit to enforce. The CLI prints them.
    notes: list[str] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Runtime protocol
# ---------------------------------------------------------------------------


class Runtime(Protocol):
    """An agent harness openDaisugi can install into.

    ``plan`` returns layer-aware :class:`InstallStep` rows for dry-run preview;
    ``apply`` performs the writes and returns the modified paths. ``layers``
    selects which layers to act on (``None`` = the default five); BASE_URL is
    opt-in only. ``enforce``/``ask``/``base_url``/``report`` only matter to runtimes that
    support those layers — everyone else ignores them (``ask`` and
    ``report`` are Claude Code's WARDEN layer only; Task 8 and spec-01).
    ``reverse`` (optional) always
    reverses everything it manages unconditionally (idempotent no-ops for
    anything absent) — it does not take a ``layers`` selection.

    ``unsupported_layers`` (optional; treat a missing one as ``{}``) maps each
    (runtime, layer) combination this runtime does NOT wire to a one-line,
    human-readable reason — ADR-0013's "encode the rest as an honest
    capability matrix rather than faking it."
    """

    name: str

    def detect(self, home: Path) -> bool: ...
    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[InstallStep]: ...
    def apply(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[Path]: ...
    def unsupported_layers(self) -> dict[Layer, str]: ...


def _append_unsupported_steps(steps: list[InstallStep], rt, sel: "set[Layer]") -> None:
    """Append an honest gap InstallStep for every selected, unsupported layer.

    Shared by every runtime's ``plan`` so a selected-but-unsupported (runtime,
    layer) combo always shows up in the plan/dry-run output — never a silent
    skip.
    """
    for layer, reason in rt.unsupported_layers().items():
        if layer in sel:
            steps.append(InstallStep(layer, f"Not wired: {reason}", None, supported=False))


# ---------------------------------------------------------------------------
# Claude Code
# ---------------------------------------------------------------------------


@dataclass
class ClaudeCodeRuntime:
    name: str = "Claude Code"

    def detect(self, home: Path) -> bool:
        return (home / ".claude").is_dir()

    def unsupported_layers(self) -> dict[Layer, str]:
        return {}  # fully wired — the reference harness for ADR-0013

    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        claude_dir = home / ".claude"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Copy opendaisugi-checklist skill",
                    _agents_skill_target(home, ".claude/skills"),
                )
            )
        if Layer.MCP in sel:
            steps.append(
                InstallStep(Layer.MCP, 'Register MCP server "opendaisugi"', home / ".claude.json")
            )
        if Layer.CAPTURE in sel:
            steps.append(
                InstallStep(
                    Layer.CAPTURE, "Add PreToolUse capture hook", claude_dir / "settings.json"
                )
            )
        if Layer.INSTRUCTIONS in sel:
            steps.append(
                InstallStep(
                    Layer.INSTRUCTIONS,
                    _pathway_guidance_title(claude_dir / "CLAUDE.md"),
                    claude_dir / "CLAUDE.md",
                )
            )
        if Layer.WARDEN in sel:
            mode = _planned_warden_mode(enforce, claude_dir / "settings.json")
            suffix = " + operator ask" if ask else ""
            steps.append(
                InstallStep(
                    Layer.WARDEN,
                    f"Install fail-closed warden PreToolUse hook ({mode}{suffix})",
                    claude_dir / "settings.json",
                )
            )
            if report == "herdr":
                steps.append(
                    InstallStep(
                        Layer.WARDEN,
                        "Add Stop + Notification floor-report hooks (Herdr)",
                        claude_dir / "settings.json",
                    )
                )
            if report in ("herdr", "coppice"):
                steps.append(
                    InstallStep(
                        Layer.WARDEN,
                        "Add SubagentStart + SubagentStop hooks (subagent rows)",
                        claude_dir / "settings.json",
                    )
                )
        if Layer.BASE_URL in sel:
            url = base_url or DEFAULT_ROUTER_BASE_URL
            steps.append(
                InstallStep(
                    Layer.BASE_URL,
                    f"Point ANTHROPIC_BASE_URL at {url}",
                    claude_dir / "settings.json",
                )
            )
        if Layer.PANDO in sel:
            steps += _pando_plan_steps("claude", home)
        _append_unsupported_steps(steps, self, sel)
        return steps

    def apply(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[Path]:
        sel = _resolve_layers(layers)
        claude_dir = home / ".claude"
        modified: list[Path] = []
        if Layer.SKILL in sel:
            modified.append(_install_skill(home, ".claude/skills"))
        if Layer.MCP in sel:
            modified += _patch_claude_mcp(home / ".claude.json")
        if Layer.CAPTURE in sel:
            modified += _patch_claude_settings(claude_dir / "settings.json")
        if Layer.INSTRUCTIONS in sel:
            modified += _patch_claude_md(claude_dir / "CLAUDE.md")
        if Layer.WARDEN in sel:
            modified += _patch_claude_warden(claude_dir / "settings.json", enforce=enforce, ask=ask)
            if report == "herdr":
                modified += _patch_claude_report_hooks(claude_dir / "settings.json")
            if report in ("herdr", "coppice"):
                modified += _patch_claude_subagent_hooks(claude_dir / "settings.json")
        if Layer.BASE_URL in sel:
            modified += _patch_claude_base_url(
                claude_dir / "settings.json", base_url or DEFAULT_ROUTER_BASE_URL
            )
        if Layer.PANDO in sel:
            modified += _apply_pando("claude", home)
        return modified

    def reverse(self, home: Path, layers: "set[Layer] | None" = None) -> list[Path]:
        """Undo the install. ``layers=None`` undoes every layer."""

        def has(layer: Layer) -> bool:
            return layers is None or layer in layers

        claude_dir = home / ".claude"
        if has(Layer.WARDEN):
            _refuse_unknown_warden_hooks(claude_dir / "settings.json")
        modified: list[Path] = []
        if has(Layer.SKILL):
            modified += _remove_skill_both(home, ".claude/skills")
        if has(Layer.MCP):
            modified += _pop_json_mcp(home / ".claude.json", mcp_key="mcpServers")
        if has(Layer.CAPTURE):
            modified += _pop_json_hook(claude_dir / "settings.json", match=is_record_hook)
        if has(Layer.WARDEN):
            modified += _pop_json_hook(claude_dir / "settings.json", match=_is_warden_hook)
            # The floor-report hooks belong to the warden layer.
            modified += _pop_json_hook(
                claude_dir / "settings.json",
                match=is_record_hook,
                events=("Stop", "Notification", "SubagentStart", "SubagentStop"),
            )
        if has(Layer.BASE_URL):
            modified += _pop_json_env_key(claude_dir / "settings.json", key="ANTHROPIC_BASE_URL")
        if has(Layer.INSTRUCTIONS):
            modified += _unpatch_instructions(claude_dir / "CLAUDE.md")
        if has(Layer.PANDO):
            modified += _reverse_pando("claude", home)
        return modified


@dataclass(frozen=True)
class _ConfigFormat:
    """A (parse, dump) pair for a structured config dialect."""

    parse: "Callable[[str], dict]"
    dump: "Callable[[dict], str]"


def _json_dump(cfg: dict) -> str:
    return json.dumps(cfg, indent=2) + "\n"


def _json5_parse(text: str) -> dict:
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return json.loads(_strip_json5_comments(text))


_JSON = _ConfigFormat(json.loads, _json_dump)
_JSON5 = _ConfigFormat(_json5_parse, _json_dump)


# v0.28.6: surface JSON5 → JSON comment loss explicitly. The writer round-
# trips through `json.dumps`, which has no concept of comments — any `//` or
# `/* … */` in the source file disappears on write. The backup
# (`.bak<ns>`) preserves the original text, so it's recoverable rather than
# silently destructive, but the install wizard's "idempotent, backed up,
# reversible" copy reads as if comments survive structurally. They don't.
# This warning makes the loss visible at exactly the moment it happens.
_JSON5_COMMENT_RE = re.compile(r"//|/\*")


def _patch_mcp(
    path: Path,
    fmt: _ConfigFormat,
    key_path: tuple[str, ...],
    entry: dict,
    *,
    what: str = "MCP registration",
    name: str = "opendaisugi",
) -> list[Path]:
    """Set ``leaf[name] = entry`` at ``key_path`` in a structured config.

    Generic over JSON / JSON5 via ``fmt``, and over what's being registered
    via ``what`` (the noun used in the skip-warning — "MCP registration" by
    default; callers register other opendaisugi-keyed leaves under this same
    idempotent/backed-up/skip-on-unparseable primitive, e.g. OpenClaw's
    router provider block). Never clobbers an unparseable file (these hold
    real user state — project history, auth): warns and skips. Idempotent,
    and backs up only when it actually writes.

    When writing a JSON5 file that had comments, emits a UserWarning before
    the write so the operator sees the comment loss in the CLI output
    (v0.28.6). The pre-write backup preserves the comments for recovery.
    """
    raw_text: str | None = None
    if path.exists():
        try:
            raw_text = path.read_text()
            cfg: dict = fmt.parse(raw_text)
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{path} is not valid; skipping {what} to avoid "
                "overwriting user state. Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        cfg = {}

    leaf = cfg
    for key in key_path:
        leaf = leaf.setdefault(key, {})
    if name in leaf:
        # An entry from an older install names a bare `daisugi`. Point it at
        # the absolute program; every other key of it stays as the user has it.
        existing = leaf[name]
        want = entry.get("command")
        if not (
            isinstance(existing, dict)
            and _is_entry_word(existing.get("command"))
            and existing.get("command") != want
        ):
            return []
        if _keeps_path(existing["command"], want):
            _WARDEN_NOTES.append(_left_mcp_note(path, existing["command"], want))
            return []
        existing["command"] = want
    else:
        leaf[name] = entry
    if path.exists():
        # v0.28.6 — warn before clobbering JSON5 comments on disk.
        if fmt is _JSON5 and raw_text is not None and _JSON5_COMMENT_RE.search(raw_text):
            warnings.warn(
                f"{path} contains JSON5 comments which will not survive the "
                f"rewrite (the writer emits plain JSON). The pre-write backup "
                f"at {path}.bak* preserves the original text — restore from it "
                f"if you need the comments back. Tracked as M7 in REVIEW_FINDINGS.md.",
                UserWarning,
                stacklevel=2,
            )
        _backup(path)
    path.write_text(fmt.dump(cfg))
    return [path]


def _patch_claude_mcp(claude_json: Path) -> list[Path]:
    """Register MCP where Claude Code reads user-scope servers: ``~/.claude.json``
    ``mcpServers`` (NOT settings.json, which only honors allow/deny flags)."""
    return _patch_mcp(
        claude_json,
        _JSON,
        ("mcpServers",),
        _mcp_entry(type="stdio"),
    )


def _patch_claude_settings(settings_path: Path) -> list[Path]:
    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            # Skip-and-warn — never clobber an unparseable settings.json. It holds
            # the user's permission deny-rules and env; resetting to {} and writing
            # back would silently destroy them (mirrors _patch_mcp's policy).
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping hook registration to "
                f"avoid overwriting your Claude Code settings (permissions/env). "
                f"Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        settings = {}

    changed = False

    hooks = settings.setdefault("hooks", {})

    # PreToolUse — check by command substring to stay idempotent across the
    # `--format claude` suffix and any future flags.
    pre = hooks.setdefault("PreToolUse", [])
    existing_pre_commands = {
        h["command"] for entry in pre for h in entry.get("hooks", []) if h.get("type") == "command"
    }
    if not any(is_record_hook(c) for c in existing_pre_commands):
        pre.append(_pretooluse_hook())
        changed = True
    elif _rebind_hook_entries(pre, "Claude Code"):
        changed = True

    # SessionStart migration: remove the v0.27.1 print-skill hook if present.
    # The skill is now discovered on demand, so SessionStart is no longer added.
    # Only mark changed when our hook was actually present (don't churn a user's
    # own SessionStart hook on every re-run).
    ss = hooks.get("SessionStart")
    if ss and any(
        h.get("command") == "daisugi install --print-skill"
        for entry in ss
        for h in entry.get("hooks", [])
    ):
        for entry in ss:
            entry["hooks"] = [
                h
                for h in entry.get("hooks", [])
                if h.get("command") != "daisugi install --print-skill"
            ]
        hooks["SessionStart"] = [e for e in ss if e.get("hooks")]
        if not hooks["SessionStart"]:
            del hooks["SessionStart"]
        changed = True

    if changed:
        if existed:
            _backup(settings_path)
        settings_path.write_text(json.dumps(settings, indent=2) + "\n")
        return [settings_path]
    return []


def _is_warden_hook(command: object) -> bool:
    return warden_hook_args(command) is not None


def _refuse_unknown_warden_hooks(settings_path: Path) -> None:
    """Raise before a reverse touches anything when a PreToolUse hook holds
    the warden module in a form config.warden_hook_args does not read: it may
    warden, and removing or keeping it silently would both be wrong."""
    try:
        data = json.loads(settings_path.read_text())
    except (OSError, ValueError):
        return
    hooks = data.get("hooks") if isinstance(data, dict) else None
    entries = hooks.get("PreToolUse") if isinstance(hooks, dict) else None
    for entry in entries if isinstance(entries, list) else []:
        inner = entry.get("hooks") if isinstance(entry, dict) else None
        for h in inner if isinstance(inner, list) else []:
            command = h.get("command") if isinstance(h, dict) else None
            if warden_hook_kind(command) == "unknown":
                raise ValueError(
                    f"{settings_path} holds a warden hook in a form this CLI does not read, "
                    f"so it is left in place: {command}. Remove it by hand, then run this again"
                )


# ADR-0013 WARDEN layer: the module name in the warden hook's command. A hook
# is found by its words (config.warden_hook_args), never by this substring.
# Distinct from the capture hook's "daisugi hook record" so both coexist as
# separate PreToolUse entries.
_WARDEN_HOOK_SUBSTR = "opendaisugi.warden"


def _upgraded_warden_command(old_command: str) -> str | None:
    """An older install's warden hook command, brought up to today's form,
    or None when it is not an older form of ours. Two upgrades: the slow
    `-m opendaisugi.warden` entry becomes `-m opendaisugi.warden_client`
    (ADR-0017), and `-I` is put before `-m` (in place of an earlier `-P`),
    so the hook never imports a module from the agent's working directory
    or PYTHONPATH (SX-R-9)."""
    if not isinstance(old_command, str) or warden_hook_args(old_command) is None:
        return None
    new = old_command
    if "opendaisugi.warden_client" not in new:
        new = new.replace(_WARDEN_HOOK_SUBSTR, "opendaisugi.warden_client", 1)
    if " -I -m opendaisugi.warden_client" not in new:
        new = new.replace(" -P -m opendaisugi.warden_client", " -m opendaisugi.warden_client", 1)
        new = new.replace(" -m opendaisugi.warden_client", " -I -m opendaisugi.warden_client", 1)
    return new if new != old_command else None


_WARDEN_MODE_RE = re.compile(r" --mode (audit|enforce)(?= )")
_WARDEN_ASK_RE = re.compile(r" --ask --ask-timeout \d+")

# Warden notes from the patchers, read by install() into InstallResult.notes.
_WARDEN_NOTES: list[str] = []


def _warden_shape(command: str) -> str:
    """A warden command with its mode, its ask words and its enforce tail
    removed: two commands with the same shape differ only in those."""
    shape = command.removesuffix(" || exit 2")
    shape = _WARDEN_ASK_RE.sub("", shape)
    return _WARDEN_MODE_RE.sub(" --mode _", shape, count=1)


def _warden_mode_of(command: str) -> str | None:
    m = _WARDEN_MODE_RE.search(command)
    return m.group(1) if m else None


def _warden_mode_relation(old_command: str, new_command: str) -> str | None:
    """How an installed warden command relates to the one a run asks for,
    when only the mode (and the ask words that go with enforce) differ.
    "up": audit installed, enforce asked. "keep": enforce installed, audit
    asked. None: any other difference."""
    old = _upgraded_warden_command(old_command) or old_command
    if not isinstance(old, str) or _warden_shape(old) != _warden_shape(new_command):
        return None
    pair = (_warden_mode_of(old), _warden_mode_of(new_command))
    if pair == ("audit", "enforce"):
        return "up"
    if pair == ("enforce", "audit"):
        return "keep"
    return None


def _planned_warden_mode(enforce: bool, hooks_file: Path) -> str:
    """The mode the plan line names: what the hook will have after this run.
    A plain install never lowers an installed enforce hook."""
    if enforce:
        return "ENFORCE"
    from opendaisugi.config import installed_hook_mode

    try:
        kept = installed_hook_mode(hooks_file) == "enforce"
    except Exception:  # a file this CLI cannot read is the patcher's to report
        kept = False
    return "ENFORCE, kept" if kept else "audit"


def _warden_step_up_note(runtime: str, path: Path) -> str:
    return f"Warden: the {runtime} hook in {path} now runs in enforce mode (it was audit)."


def _warden_keep_note(runtime: str, path: Path, upgraded: bool = False) -> str:
    note = (
        f"Warden: the {runtime} hook in {path} stays in enforce mode; a plain install never "
        "lowers it. To go back to audit, run daisugi install --uninstall, then daisugi install."
    )
    if upgraded:
        note += " Its command was brought up to today's form (python -I, warden_client)."
    return note


_NOT_ISOLATED_NOTE = (
    " That hook also runs Python without -I, so it can import a module from the"
    " agent's working directory: run `daisugi install --uninstall` and then"
    " `daisugi install` to replace it."
)


def _not_isolated_note(commands) -> str:
    """The extra words for the "different mode/config" warning when a warden
    hook there runs the Python module (`-m opendaisugi.warden...`) without
    -I: it can import a module from the agent's working directory or
    PYTHONPATH (SX-R-9). A plain text test, so the ports can say the same."""
    for c in commands:
        if (
            isinstance(c, str)
            and warden_hook_kind(c) is not None
            and ("-m opendaisugi.warden" in c or "-mopendaisugi.warden" in c)
            and " -I -m " not in c
        ):
            return _NOT_ISOLATED_NOTE
    return ""


class WardenRootError(ValueError):
    """The warden root is not an absolute path. The CLI turns it into one
    line, exit 2, before any write."""


def _absolute_warden_root(root: Path) -> Path:
    """The warden root an installed hook bakes. A relative one would resolve
    against the hook's cwd, which is the agent's workspace, so it is
    refused."""
    if not Path(root).is_absolute():
        raise WardenRootError(
            f"the warden root {str(root)!r} is not an absolute path; set HOME, "
            "OPENDAISUGI_HOME or XDG_DATA_HOME to an absolute path"
        )
    return Path(root)


def _patch_claude_warden(
    settings_path: Path,
    *,
    enforce: bool = False,
    ask: bool = False,
    root: Path | None = None,
    python: str | None = None,
    verify_timeout_s: float | None = None,
    session: str | None = None,
) -> list[Path]:
    """Merge the ADR-0007 fail-closed warden's PreToolUse hook into settings.json.

    Built from ``warden_settings_json`` — the one canonical source for the warden
    command — never hand-constructed. Merges exactly like
    ``_patch_claude_settings`` does for the capture hook: idempotent by the
    command substring ``"opendaisugi.warden"``, skip-and-warn on unparseable
    settings.json, backup only when actually writing. AUDIT unless
    ``enforce=True`` — the warden never enforces without an explicit opt-in.
    ``ask=True`` (Task 8) bakes ``--ask`` into the installed command at its
    default timeout — off by default, same posture as ``enforce``.
    ``session`` (plan 5's ``daisugi start``) pins the envelope lookup to one
    session id from OUTSIDE the agent, so a hook shared by more than one
    ``root`` (or a root shared by more than one project) can't fall back to
    another project's ``default`` envelope.

    Idempotency is by *presence*, not by content: re-running with the same
    flags is a true no-op, but asking for a different mode than what is
    already installed does NOT silently rewrite it (that could just as
    easily silently escalate an audit install into enforce as the reverse).
    Instead it warns — the honest signal is "run --uninstall first", not a
    config file that quietly disagrees with what the user just asked for.
    """
    from opendaisugi import warden as _warden
    from opendaisugi.warden import warden_settings_json

    kwargs: dict = {
        "mode": "enforce" if enforce else "audit",
        "fmt": "claude",
        "root": _absolute_warden_root(root if root is not None else _warden.DEFAULT_WARDEN_ROOT),
        "ask": ask,
    }
    if python is not None:
        kwargs["python"] = python
    if verify_timeout_s is not None:
        kwargs["verify_timeout_s"] = verify_timeout_s
    if session is not None:
        kwargs["session"] = session
    warden_entry = json.loads(warden_settings_json(**kwargs))["hooks"]["PreToolUse"][0]
    warden_command = warden_entry["hooks"][0]["command"]

    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping warden hook installation "
                f"to avoid overwriting your Claude Code settings (permissions/env). "
                f"Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        settings = {}

    hooks = settings.setdefault("hooks", {})
    pre = hooks.setdefault("PreToolUse", [])
    existing_commands = {
        h["command"] for entry in pre for h in entry.get("hooks", []) if h.get("type") == "command"
    }
    if any(c == warden_command for c in existing_commands):
        return []  # exact match already installed — idempotent no-op

    if any(warden_hook_kind(c) is not None for c in existing_commands):
        # Upgrade path (ADR-0017): an install from before the resident warden
        # still runs the slow `-m opendaisugi.warden` entry directly. Rewrite
        # it in place to `-m opendaisugi.warden_client` (the resident-server
        # client, same mode/root/config) instead of warning-and-skipping —
        # the one real user gets the fast path on a plain re-run of
        # `daisugi install`, no --uninstall dance required. Only a
        # command whose ONLY difference from warden_command is that one module
        # name is rewritten; a genuinely different mode/root/config still
        # warns, unchanged (the existing idempotent-by-presence contract).
        def _is_upgradeable(old_command: str) -> bool:
            return _upgraded_warden_command(old_command) == warden_command

        rewritten = False
        stepped_up = False
        kept = False
        kept_upgraded = False
        warden_hook = warden_entry["hooks"][0]
        for entry in pre:
            for h in entry.get("hooks", []):
                if h.get("type") != "command":
                    continue
                relation = _warden_mode_relation(h.get("command", ""), warden_command)
                if relation == "up":
                    # Audit to enforce, nothing else different: the one
                    # step a user asks for with `daisugi install --enforce`.
                    h["command"] = warden_command
                    if "timeout" in warden_hook:
                        h["timeout"] = warden_hook["timeout"]
                    rewritten = stepped_up = True
                elif relation == "keep":
                    kept = True
                    # An older form of the enforce hook is brought up to
                    # today's command with its mode unchanged (SX-R-9: -I).
                    old_command = h.get("command", "")
                    upgraded = _upgraded_warden_command(old_command)
                    if upgraded:
                        h["command"] = upgraded
                        rewritten = kept_upgraded = True
                elif _is_upgradeable(h.get("command", "")):
                    h["command"] = warden_command
                    rewritten = True
        if rewritten:
            _backup(settings_path)
            settings_path.write_text(json.dumps(settings, indent=2) + "\n")
            if stepped_up:
                _WARDEN_NOTES.append(_warden_step_up_note("Claude Code", settings_path))
            elif kept:
                _WARDEN_NOTES.append(_warden_keep_note("Claude Code", settings_path, kept_upgraded))
            return [settings_path]
        if kept:
            # A plain install never lowers enforce to audit.
            _WARDEN_NOTES.append(_warden_keep_note("Claude Code", settings_path))
            return []

        warnings.warn(
            f"a warden hook is already installed in {settings_path} with a "
            f"different mode/config; run `daisugi install --uninstall` first "
            f"if you want to change it (idempotent by presence, not content)."
            + _not_isolated_note(existing_commands),
            UserWarning,
            stacklevel=2,
        )
        return []  # already installed — idempotent no-op either way

    pre.append(warden_entry)
    if existed:
        _backup(settings_path)
    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
    return [settings_path]


def _report_hook(event_flag: str) -> dict:
    return {
        "hooks": [{"type": "command", "command": _record_command(f"--format claude {event_flag}")}],
    }


# SubagentStart and SubagentStop report each subagent to coppice as a read
# only child row under its pane.
_SUBAGENT_REPORT_HOOKS = {
    "SubagentStart": "--event subagent_start",
    "SubagentStop": "--event subagent_stop",
}


def _patch_claude_report_hooks(settings_path: Path) -> list[Path]:
    """Add Stop + Notification hooks so the floor gets exact idle/blocked
    signal between tool calls, not just around them (spec-01). Idempotent
    by command substring, same pattern as ``_patch_claude_settings``'s
    PreToolUse block; skip-and-warn on unparseable settings.json.
    """
    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping floor-report hook "
                f"registration to avoid overwriting your Claude Code settings. "
                f"Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        settings = {}

    changed = False
    hooks = settings.setdefault("hooks", {})

    stop = hooks.setdefault("Stop", [])
    stop_cmds = {
        h.get("command", "") for e in stop for h in e.get("hooks", []) if h.get("type") == "command"
    }
    if not any(is_record_hook(c, "stop") for c in stop_cmds):
        stop.append(_report_hook("--event stop"))
        changed = True
    elif _rebind_hook_entries(stop, "Claude Code"):
        changed = True

    notif = hooks.setdefault("Notification", [])
    notif_cmds = {
        h.get("command", "")
        for e in notif
        for h in e.get("hooks", [])
        if h.get("type") == "command"
    }
    if not any(is_record_hook(c, "notification") for c in notif_cmds):
        notif.append(_report_hook("--event notification"))
        changed = True
    elif _rebind_hook_entries(notif, "Claude Code"):
        changed = True

    if changed:
        if existed:
            _backup(settings_path)
        settings_path.write_text(json.dumps(settings, indent=2) + "\n")
        return [settings_path]
    return []


def _patch_claude_subagent_hooks(settings_path: Path) -> list[Path]:
    """Add SubagentStart + SubagentStop hooks so coppice shows each subagent
    as a read only child row under its pane. Idempotent by command
    substring; skip-and-warn on unparseable settings.json, the same as
    ``_patch_claude_report_hooks``.
    """
    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping subagent hook "
                f"registration to avoid overwriting your Claude Code settings. "
                f"Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        settings = {}

    changed = False
    hooks = settings.setdefault("hooks", {})
    for hook_event, flag in _SUBAGENT_REPORT_HOOKS.items():
        entries = hooks.setdefault(hook_event, [])
        cmds = {
            h.get("command", "")
            for e in entries
            for h in e.get("hooks", [])
            if h.get("type") == "command"
        }
        if not any(is_record_hook(c, flag.split()[-1]) for c in cmds):
            entries.append(
                {
                    "hooks": [
                        {
                            "type": "command",
                            "command": _record_command(f"--format claude {flag}"),
                        }
                    ]
                }
            )
            changed = True
        elif _rebind_hook_entries(entries, "Claude Code"):
            changed = True

    if changed:
        if existed:
            _backup(settings_path)
        settings_path.write_text(json.dumps(settings, indent=2) + "\n")
        return [settings_path]
    return []


def _patch_claude_base_url(settings_path: Path, url: str) -> list[Path]:
    """Merge ANTHROPIC_BASE_URL into settings.json's env, idempotently.

    Points Claude Code at the local token-saving router (ADR-0012's BASE_URL
    layer, ADR-0013). No-op if already set to ``url``; skip-and-warn on
    unparseable settings.json (mirrors ``_patch_claude_settings``); backs up
    before writing.
    """
    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping ANTHROPIC_BASE_URL "
                f"to avoid overwriting your Claude Code settings (permissions/env). "
                f"Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        settings = {}

    env = settings.setdefault("env", {})
    if env.get("ANTHROPIC_BASE_URL") == url:
        return []  # already pointed at the router — no-op

    env["ANTHROPIC_BASE_URL"] = url
    if existed:
        _backup(settings_path)
    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
    return [settings_path]


def _marker_starts(text: str) -> list[int]:
    """Where the marker stands alone on its line (CRLF or LF ends). A marker
    inside a line of prose is not one."""
    out: list[int] = []
    i = text.find(_CLAUDE_MD_MARKER)
    while i != -1:
        after = i + len(_CLAUDE_MD_MARKER)
        at_start = i == 0 or text[i - 1] == "\n"
        at_end = after == len(text) or text[after] == "\n" or text.startswith("\r\n", after)
        if at_start and at_end:
            out.append(i)
        i = text.find(_CLAUDE_MD_MARKER, after)
    return out


def _managed_span(text: str) -> tuple[int, int] | None:
    """The (start, end) of the marker-bounded block in text, or None.

    The first two markers that stand alone on a line bound it. A single one
    is not a block.
    """
    starts = _marker_starts(text)
    if len(starts) < 2:
        return None
    return starts[0], starts[1] + len(_CLAUDE_MD_MARKER)


def _read_raw(md_path: Path) -> str:
    """The file's text with its line ends as they are (no \\r\\n to \\n)."""
    return md_path.read_bytes().decode("utf-8")


def _write_raw(md_path: Path, text: str) -> None:
    md_path.write_bytes(text.encode("utf-8"))


def _block_for(span_text: str) -> str:
    """The current block, with the line ends of the block it replaces."""
    cur = _CLAUDE_MD_BLOCK.rstrip("\n")
    return cur.replace("\n", "\r\n") if "\r\n" in span_text else cur


def _pathway_guidance_title(md_path: Path) -> str:
    """Plan line for the instruction layer: what the install will do to the file.

    Update when an older block is there, no change when the current block is
    there already, else Append. A file that cannot be read reads as no block.
    """
    try:
        text = _read_raw(md_path)
    except (OSError, UnicodeDecodeError):
        return "Append pathway guidance"
    span = _managed_span(text)
    if span is None:
        return "Append pathway guidance"
    if text[span[0] : span[1]] != _block_for(text[span[0] : span[1]]):
        return "Update pathway guidance"
    return "Keep pathway guidance (already current)"


def _patch_instructions(md_path: Path) -> list[Path]:
    """Append the managed pathway block to an instruction file, idempotently.

    Works for any always-on instruction file: CLAUDE.md (Claude Code) or
    AGENTS.md (Codex, OpenClaw). When the marked block is already there, the
    same text is a no-op and older text is replaced in place; everything
    outside the markers stays byte for byte, line ends included. The block
    takes the line ends of the one it replaces.
    """
    existing = _read_raw(md_path) if md_path.exists() else ""
    span = _managed_span(existing)
    if span is not None:
        old = existing[span[0] : span[1]]
        cur = _block_for(old)
        if old == cur:
            return []  # already current
        _write_raw(md_path, existing[: span[0]] + cur + existing[span[1] :])
        return [md_path]
    if _marker_starts(existing):
        return []  # one marker line: not a block, leave it alone
    md_path.parent.mkdir(parents=True, exist_ok=True)
    updated = existing.rstrip("\n") + ("\n\n" if existing else "") + _CLAUDE_MD_BLOCK
    _write_raw(md_path, updated)
    return [md_path]


def _patch_claude_md(md_path: Path) -> list[Path]:
    return _patch_instructions(md_path)


# ---------------------------------------------------------------------------
# Hermes
# ---------------------------------------------------------------------------


@dataclass
class HermesRuntime:
    name: str = "Hermes"

    def detect(self, home: Path) -> bool:
        return (home / ".hermes").is_dir()

    def unsupported_layers(self) -> dict[Layer, str]:
        return {
            Layer.WARDEN: "external/JS-shim warden not yet wired — follow-up",
            Layer.BASE_URL: (
                "router speaks Anthropic Messages; this harness's custom endpoint "
                "expects OpenAI — not wired (needs an OpenAI-wire adapter)"
            ),
        }

    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        h = home / ".hermes"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Copy opendaisugi-checklist skill",
                    h / "skills" / "opendaisugi" / _SKILL_NAME,
                )
            )
        if Layer.MCP in sel:
            steps.append(
                InstallStep(Layer.MCP, "Register opendaisugi MCP server", h / "config.yaml")
            )
        if Layer.CAPTURE in sel:
            steps.append(
                InstallStep(Layer.CAPTURE, "Add pre_tool_call capture hook", h / "config.yaml")
            )
        if Layer.INSTRUCTIONS in sel:
            steps.append(
                InstallStep(
                    Layer.INSTRUCTIONS, _pathway_guidance_title(h / "SOUL.md"), h / "SOUL.md"
                )
            )
        if Layer.PANDO in sel:
            steps += _pando_plan_steps("hermes", home)
        _append_unsupported_steps(steps, self, sel)
        return steps

    def apply(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[Path]:
        sel = _resolve_layers(layers)
        h = home / ".hermes"
        h.mkdir(parents=True, exist_ok=True)
        modified: list[Path] = []
        if Layer.SKILL in sel:
            modified.append(_link_skill(h / "skills" / "opendaisugi" / _SKILL_NAME))
        if Layer.MCP in sel or Layer.CAPTURE in sel:
            # One writer covers both — Hermes registers the MCP server and the
            # capture hook in a single config.yaml rewrite. It writes only the
            # layers selected.
            modified += _patch_hermes_config(
                h / "config.yaml", mcp=Layer.MCP in sel, capture=Layer.CAPTURE in sel
            )
        if Layer.INSTRUCTIONS in sel:
            modified += _patch_hermes_soul(h / "SOUL.md")
        if Layer.PANDO in sel:
            modified += _apply_pando("hermes", home)
        return modified

    def reverse(self, home: Path, layers: "set[Layer] | None" = None) -> list[Path]:
        def has(layer: Layer) -> bool:
            return layers is None or layer in layers

        h = home / ".hermes"
        modified: list[Path] = []
        if has(Layer.SKILL):
            modified += _remove_skill(h / "skills" / "opendaisugi" / _SKILL_NAME)
        if has(Layer.INSTRUCTIONS):
            modified += _unpatch_instructions(h / "SOUL.md")
        modified += self._reverse_config(h, has)
        if has(Layer.PANDO):
            modified += _reverse_pando("hermes", home)
        return modified

    def _reverse_config(self, h: Path, has: "Callable[[Layer], bool]") -> list[Path]:
        modified: list[Path] = []
        cfg_path = h / "config.yaml"
        if not cfg_path.exists():
            return modified
        try:
            cfg = yaml.safe_load(cfg_path.read_text()) or {}
        except yaml.YAMLError:
            return modified

        changed = False
        # Only touch keys that exist; never inject empty scaffolding.
        servers = cfg.get("mcp_servers")
        if has(Layer.MCP) and isinstance(servers, dict) and "opendaisugi" in servers:
            del servers["opendaisugi"]
            if not servers:
                del cfg["mcp_servers"]
            changed = True
        hooks = cfg.get("hooks")
        if (
            has(Layer.CAPTURE)
            and isinstance(hooks, dict)
            and isinstance(hooks.get("pre_tool_call"), list)
        ):
            pre = hooks["pre_tool_call"]
            kept = [
                hk for hk in pre if not (isinstance(hk, dict) and is_record_hook(hk.get("command")))
            ]
            if len(kept) != len(pre):
                changed = True
                if kept:
                    hooks["pre_tool_call"] = kept
                else:
                    del hooks["pre_tool_call"]
                if not hooks:
                    del cfg["hooks"]

        if changed:
            _backup(cfg_path)
            cfg_path.write_text(yaml.dump(cfg, default_flow_style=False, sort_keys=False))
            modified.append(cfg_path)
        return modified


def _patch_hermes_soul(soul: Path) -> list[Path]:
    """Append the managed block to Hermes's SOUL.md, the one file Hermes
    always loads (from its home dir, whatever the working dir is).

    Hermes seeds a starter SOUL.md on its first run, and an empty or missing
    file means its built-in identity. A file made here would hold only this
    block and so replace that identity. So a missing SOUL.md is left alone,
    with a warning.
    """
    if not soul.exists():
        warnings.warn(
            f"{soul} does not exist yet; skipping the Hermes instructions. "
            f"Start Hermes once, then re-run `daisugi install`.",
            UserWarning,
            stacklevel=2,
        )
        return []
    return _patch_instructions(soul)


def _patch_hermes_config(
    config_path: Path, *, mcp: bool = True, capture: bool = True
) -> list[Path]:
    existed = config_path.exists()
    if existed:
        try:
            cfg: dict = yaml.safe_load(config_path.read_text()) or {}
        except yaml.YAMLError:
            # Skip-and-warn — never clobber an unparseable Hermes config (holds the
            # user's mcp_servers / hooks). Resetting to {} would silently wipe them.
            warnings.warn(
                f"{config_path} is not valid YAML; skipping to avoid overwriting your "
                f"Hermes config. Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        cfg = {}

    changed = False

    if mcp:
        servers = cfg.setdefault("mcp_servers", {})
        have = servers.get("opendaisugi")
        if "opendaisugi" not in servers:
            servers["opendaisugi"] = _mcp_entry()
            changed = True
        elif (
            isinstance(have, dict)
            and _is_entry_word(have.get("command"))
            and have["command"] != daisugi_exe()
        ):
            if _keeps_path(have["command"], daisugi_exe()):
                _WARDEN_NOTES.append(_left_mcp_note(config_path, have["command"], daisugi_exe()))
            else:
                have["command"] = daisugi_exe()
                changed = True

    if capture:
        hooks = cfg.setdefault("hooks", {})
        pre = hooks.setdefault("pre_tool_call", [])
        cmd = _record_command("--format hermes")
        if not any(isinstance(h, dict) and h.get("command") == cmd for h in pre):
            if any(isinstance(h, dict) and is_record_hook(h.get("command")) for h in pre):
                if _rebind_hook_entries(pre, "Hermes"):
                    changed = True
            else:
                pre.append({"matcher": ".*", "command": cmd, "timeout": 10})
                changed = True

    if changed:
        if existed:
            _backup(config_path)  # back up only when we actually rewrite
        config_path.write_text(yaml.dump(cfg, default_flow_style=False, sort_keys=False))
        return [config_path]
    return []


# ---------------------------------------------------------------------------
# The pando layer (round 7). Pando R2 is there in every harness with no setup
# step: `daisugi pando serve` is the MCP server "pando", a new chat gets the
# memory view in its context, a Claude Code chat is logged as it is said, and
# `daisugi pando follow` runs as a user service so Hermes chats and background
# tends keep going. It is on by default; `--only` leaves it out unless it
# names pando. The Go client's install/pandolayer.go is the twin.
# ---------------------------------------------------------------------------

PANDO_MCP_NAME = "pando"
PANDO_UNIT_NAME = "daisugi-pando.service"
_PANDO_HOOK_MARKER = " pando hook"
#: The Claude Code events the layer hooks. SessionEnd catches the last reply:
#: the transcript may lag the Stop hook.
PANDO_HOOK_EVENTS: tuple[str, ...] = ("SessionStart", "UserPromptSubmit", "Stop", "SessionEnd")
#: The first line of a unit this layer wrote. A unit file without it is the
#: owner's own.
_PANDO_UNIT_MARK = "# Managed by daisugi install."
PANDO_UNIT_NOTE = (
    "Pando follow service file written. If systemd does not run it for you, start it yourself: "
    f"systemctl --user enable --now {PANDO_UNIT_NAME}"
)

#: Runs systemctl for the service: ``hook(action, unit)`` where action is
#: "enable" (daemon-reload, then enable --now), "disable" (disable --now) or
#: "reload" (daemon-reload after the file is gone). None (the default, and in
#: every test) means install only writes the file. The install command sets it
#: for a real home and never for a dry run. A failure is the hook's to report;
#: it never fails the install.
UNIT_HOOK: "Callable[[str, str], object] | None" = None


def _run_unit_hook(action: str) -> None:
    hook = UNIT_HOOK
    if hook is not None:
        try:
            hook(action, PANDO_UNIT_NAME)
        except Exception:  # noqa: BLE001 - the files are written either way
            pass


def _pando_mcp_entry(**extra: object) -> dict:
    return {**extra, "command": daisugi_exe(), "args": ["pando", "serve"]}


def _pando_hook_command() -> str:
    return f"{shlex.quote(daisugi_exe())}{_PANDO_HOOK_MARKER}"


def is_pando_hook(command: object) -> bool:
    return isinstance(command, str) and command.strip().endswith(_PANDO_HOOK_MARKER)


def _pando_hook_async(event: str) -> bool:
    """Only SessionStart prints context, so only it must be waited for.

    SessionEnd is waited for too: it is the end of the chat and holds nothing
    up, and Claude Code kills a background hook at exit in -p mode, so the
    last reply would be lost. Its own timeout (10 s) is the wait.
    """
    return event not in ("SessionStart", "SessionEnd")


def _pando_hook_entry(event: str) -> dict:
    hook: dict = {"type": "command", "command": _pando_hook_command(), "timeout": 10}
    if _pando_hook_async(event):
        hook["async"] = True
    return {"hooks": [hook]}


def _pando_hook_fresh(event: str, hook: dict) -> bool:
    """Bring an existing pando hook to the shape the layer wants."""
    changed = False
    want = _pando_hook_command()
    if hook.get("command") != want and not _keeps_path(hook.get("command"), want):
        hook["command"] = want
        changed = True
    if (hook.get("async") is True) != _pando_hook_async(event):
        if _pando_hook_async(event):
            hook["async"] = True
        else:
            del hook["async"]
        changed = True
    return changed


def _patch_pando_claude_hooks(settings_path: Path) -> list[Path]:
    """Add the pando hook to SessionStart, UserPromptSubmit, Stop and
    SessionEnd of a Claude Code settings.json, unless it is there."""
    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping the pando hooks to avoid "
                f"overwriting your Claude Code settings. Fix the file and re-run "
                f"`daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        settings = {}
    changed = False
    hooks = settings.setdefault("hooks", {})
    for ev in PANDO_HOOK_EVENTS:
        entries = hooks.setdefault(ev, [])
        have = False
        for entry in entries:
            for h in entry.get("hooks", []):
                if isinstance(h, dict) and is_pando_hook(h.get("command")):
                    have = True
                    if _pando_hook_fresh(ev, h):
                        changed = True
        if not have:
            entries.append(_pando_hook_entry(ev))
            changed = True
    if not changed:
        return []
    if existed:
        _backup(settings_path)
    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
    return [settings_path]


_PANDO_TOML_HEADER = "[mcp_servers.pando]"


def _pando_block_lines(lines: list[str]) -> "tuple[int, int] | None":
    """The layer's block in a config.toml as ``(start, end)`` line numbers: the
    header line, and the line after the block. A header counts only when it is
    a whole line (a comment that holds the text does not). The block runs to
    the next table header that is not one of its own sub-tables."""
    start = -1
    for i, line in enumerate(lines):
        t = line.strip()
        if (
            t == _PANDO_TOML_HEADER
            or t.startswith(_PANDO_TOML_HEADER + " #")
            or t.startswith(_PANDO_TOML_HEADER + "#")
        ):
            start = i
            break
    if start < 0:
        return None
    end = len(lines)
    for i in range(start + 1, len(lines)):
        t = lines[i].strip()
        if t.startswith("[") and not t.startswith("[mcp_servers.pando."):
            end = i
            break
    return start, end


def _patch_pando_codex_config(config_path: Path) -> list[Path]:
    """Add ``[mcp_servers.pando]`` to a Codex config.toml."""
    existing = config_path.read_text() if config_path.exists() else ""
    if _pando_block_lines(existing.split("\n")) is not None:
        return []
    block = (
        f"\n{_PANDO_TOML_HEADER}\n"
        f"command = {json.dumps(daisugi_exe())}\n"
        'args = ["pando", "serve"]\n'
    )
    if config_path.exists():
        _backup(config_path)
    config_path.write_text(existing.rstrip("\n") + ("\n" if existing else "") + block)
    return [config_path]


def _unpatch_pando_codex_config(config_path: Path) -> list[Path]:
    """Take the block out again."""
    if not config_path.exists():
        return []
    lines = config_path.read_text().split("\n")
    span = _pando_block_lines(lines)
    if span is None:
        return []
    start, end = span
    cleaned = "\n".join(lines[:start] + lines[end:]).rstrip("\n")
    _backup(config_path)
    config_path.write_text(cleaned + "\n" if cleaned else "")
    return [config_path]


def _hermes_homes(home: Path) -> list[Path]:
    """The Hermes dirs the layer writes into: the root (HERMES_HOME, else
    ~/.hermes) and each profile dir that has a config.yaml or a state.db."""
    env = os.environ.get("HERMES_HOME", "").strip()
    if env:
        p = Path(env)
        root = p.parent.parent if p.parent.name == "profiles" else p
    else:
        root = home / ".hermes"
    out = [root]
    profiles = root / "profiles"
    if profiles.is_dir():
        for d in sorted(profiles.iterdir(), key=lambda x: x.name):
            if not d.is_dir():
                continue
            if any(os.path.lexists(d / f) for f in ("config.yaml", "state.db")):
                out.append(d)
    return out


def _patch_pando_hermes_config(config_path: Path) -> list[Path]:
    """Set the pando MCP server and enable the pando plugin in a Hermes
    config.yaml."""
    existed = config_path.exists()
    if existed:
        try:
            cfg: dict = yaml.safe_load(config_path.read_text()) or {}
        except yaml.YAMLError:
            warnings.warn(
                f"{config_path} is not valid YAML; skipping pando to avoid overwriting your "
                f"Hermes config. Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        cfg = {}
    changed = False
    servers = cfg.setdefault("mcp_servers", {})
    if PANDO_MCP_NAME not in servers:
        servers[PANDO_MCP_NAME] = _pando_mcp_entry()
        changed = True
    plugins = cfg.setdefault("plugins", {})
    enabled = plugins.get("enabled")
    if enabled is None:
        enabled = []
    if "pando" not in enabled:
        plugins["enabled"] = [*enabled, "pando"]
        changed = True
    if not changed:
        return []
    if existed:
        _backup(config_path)
    config_path.write_text(yaml.dump(cfg, default_flow_style=False, sort_keys=False))
    return [config_path]


def _unpatch_pando_hermes_config(config_path: Path) -> list[Path]:
    """Take the MCP server and the plugin entry out."""
    if not config_path.exists():
        return []
    try:
        cfg = yaml.safe_load(config_path.read_text()) or {}
    except yaml.YAMLError:
        return []
    changed = False
    servers = cfg.get("mcp_servers")
    if isinstance(servers, dict) and PANDO_MCP_NAME in servers:
        del servers[PANDO_MCP_NAME]
        if not servers:
            del cfg["mcp_servers"]
        changed = True
    plugins = cfg.get("plugins")
    if isinstance(plugins, dict) and isinstance(plugins.get("enabled"), list):
        kept = [x for x in plugins["enabled"] if x != "pando"]
        if len(kept) != len(plugins["enabled"]):
            changed = True
            if kept:
                plugins["enabled"] = kept
            else:
                del plugins["enabled"]
            if not plugins:
                del cfg["plugins"]
    if not changed:
        return []
    _backup(config_path)
    config_path.write_text(yaml.dump(cfg, default_flow_style=False, sort_keys=False))
    return [config_path]


_PANDO_PLUGIN_YAML = """\
name: pando
version: "1.0"
description: Puts the Pando memory view into the context of every turn. Managed by daisugi install.
provides_hooks:
  - pre_llm_call
"""

#: The plugin. The program path is written at install.
_PANDO_PLUGIN_PY = """\
# Managed by daisugi install. Puts the Pando memory view into the context of
# every turn: Hermes appends what pre_llm_call returns to the user message,
# and does not keep it, so a later turn needs it again. The text is the same
# bytes until the memory changes. It reads the memory through the daisugi
# program and never fails a turn. A process marked DAISUGI_PANDO_SKIP gets
# nothing (the program prints nothing then). The session id goes to the
# program too: a long-running agent's chat (DAISUGI_PANDO_BRANCH) is then
# logged into its branch of the memory, never into the store above it.
import subprocess

DAISUGI = %s


def _view(session_id=None, is_first_turn=False, **kwargs):
    argv = [DAISUGI, "pando", "context"]
    if session_id:
        argv += ["--session", str(session_id)]
    try:
        out = subprocess.run(argv, capture_output=True, text=True, timeout=10)
    except Exception:
        return None
    text = (out.stdout or "").strip()
    if out.returncode != 0 or not text:
        return None
    return {"context": text}


def register(ctx):
    ctx.register_hook("pre_llm_call", _view)
"""


def _pando_plugin_files() -> dict[str, str]:
    return {
        "plugin.yaml": _PANDO_PLUGIN_YAML,
        "__init__.py": _PANDO_PLUGIN_PY.replace("%s", json.dumps(daisugi_exe()), 1),
    }


def _install_pando_plugin(hermes_dir: Path) -> list[Path]:
    """Write the plugin into ``hermes_dir/plugins/pando``, unless it is there as wanted."""
    target = hermes_dir / "plugins" / "pando"
    files = _pando_plugin_files()
    if all(
        (target / rel).is_file() and (target / rel).read_text() == text
        for rel, text in files.items()
    ):
        return []
    target.mkdir(parents=True, exist_ok=True)
    for rel, text in files.items():
        (target / rel).write_text(text)
    return [target]


def pando_unit_path(home: Path) -> Path:
    return home / ".config" / "systemd" / "user" / PANDO_UNIT_NAME


def _pando_unit() -> str:
    return (
        f"{_PANDO_UNIT_MARK}\n[Unit]\nDescription=openDaisugi Pando memory follower\n\n"
        f"[Service]\nExecStart={shlex.quote(daisugi_exe())} pando follow\n"
        "Restart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n"
    )


def _is_our_unit(path: Path) -> bool:
    try:
        return path.read_text().startswith(_PANDO_UNIT_MARK)
    except OSError:
        return False


def _install_pando_unit(home: Path) -> list[Path]:
    """Write the user service file, and start the service."""
    path = pando_unit_path(home)
    want = _pando_unit()
    if path.exists() and path.read_text() == want:
        return []
    path.parent.mkdir(parents=True, exist_ok=True)
    # A unit the owner edited by hand is kept as a backup first.
    if os.path.lexists(path) and not _is_our_unit(path):
        _backup(path)
    path.write_text(want)
    _WARDEN_NOTES.append(PANDO_UNIT_NOTE)
    _run_unit_hook("enable")
    return [path]


def _remove_pando_unit(home: Path) -> list[Path]:
    """Stop the service and take the unit file away. A file that is not ours
    is left alone."""
    path = pando_unit_path(home)
    if not os.path.lexists(path) or not _is_our_unit(path):
        return []
    _run_unit_hook("disable")
    path.unlink()
    _run_unit_hook("reload")
    return [path]


def _pando_plan_steps(rt_key: str, home: Path) -> list[InstallStep]:
    steps: list[InstallStep] = []

    def add(desc: str, target: Path) -> None:
        steps.append(InstallStep(Layer.PANDO, desc, target))

    if rt_key == "claude":
        add(
            'Register MCP server "pando" (the memory: recall, zoom, date, view)',
            home / ".claude.json",
        )
        add(
            "Add pando memory hooks (SessionStart view; UserPromptSubmit, Stop and "
            "SessionEnd log the chat)",
            home / ".claude" / "settings.json",
        )
    elif rt_key == "codex":
        add('Register MCP server "pando"', home / ".codex" / "config.toml")
    elif rt_key == "hermes":
        for d in _hermes_homes(home):
            add(
                'Register MCP server "pando" and enable the pando plugin (memory view in every turn)',
                d / "config.yaml",
            )
            add("Install the pando plugin", d / "plugins" / "pando")
    if rt_key in ("claude", "hermes"):
        add(
            "Add the pando follow user service (live log of every chat, background "
            "summaries); enable and start it",
            pando_unit_path(home),
        )
    return steps


def _apply_pando(rt_key: str, home: Path) -> list[Path]:
    modified: list[Path] = []
    if rt_key == "claude":
        modified += _patch_mcp(
            home / ".claude.json",
            _JSON,
            ("mcpServers",),
            _pando_mcp_entry(type="stdio"),
            what="pando MCP registration",
            name=PANDO_MCP_NAME,
        )
        modified += _patch_pando_claude_hooks(home / ".claude" / "settings.json")
    elif rt_key == "codex":
        modified += _patch_pando_codex_config(home / ".codex" / "config.toml")
    elif rt_key == "hermes":
        for d in _hermes_homes(home):
            modified += _patch_pando_hermes_config(d / "config.yaml")
            modified += _install_pando_plugin(d)
    if rt_key in ("claude", "hermes"):
        modified += _install_pando_unit(home)
    return modified


def _reverse_pando(rt_key: str, home: Path) -> list[Path]:
    modified: list[Path] = []
    if rt_key == "claude":
        modified += _pop_json_mcp(home / ".claude.json", mcp_key="mcpServers", name=PANDO_MCP_NAME)
        modified += _pop_json_hook(
            home / ".claude" / "settings.json", match=is_pando_hook, events=PANDO_HOOK_EVENTS
        )
    elif rt_key == "codex":
        modified += _unpatch_pando_codex_config(home / ".codex" / "config.toml")
    elif rt_key == "hermes":
        for d in _hermes_homes(home):
            modified += _unpatch_pando_hermes_config(d / "config.yaml")
            modified += _remove_dir(d / "plugins" / "pando")
    if rt_key in ("claude", "hermes"):
        modified += _remove_pando_unit(home)
    return modified


# ---------------------------------------------------------------------------
# Codex (OpenAI CLI) — detected by binary presence
# ---------------------------------------------------------------------------


@dataclass
class CodexRuntime:
    name: str = "Codex"

    def detect(self, home: Path) -> bool:
        return shutil.which("codex") is not None or (home / ".codex").is_dir()

    def unsupported_layers(self) -> dict[Layer, str]:
        return {}  # warden wired via hooks.json (v0.114+); base_url via the OpenAI-wire adapter

    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        codex = home / ".codex"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Copy opendaisugi-checklist skill",
                    _agents_skill_target(home, ".codex/skills"),
                )
            )
        if Layer.MCP in sel:
            steps.append(
                InstallStep(Layer.MCP, "Register opendaisugi MCP server", codex / "config.toml")
            )
        if Layer.INSTRUCTIONS in sel:
            steps.append(
                InstallStep(
                    Layer.INSTRUCTIONS,
                    _pathway_guidance_title(codex / "AGENTS.md"),
                    codex / "AGENTS.md",
                )
            )
        if Layer.WARDEN in sel:
            mode = _planned_warden_mode(enforce, codex / "hooks.json")
            steps.append(
                InstallStep(
                    Layer.WARDEN,
                    f"Install warden PreToolUse hook ({mode}) — Codex hooks fail OPEN on "
                    "hook crash/timeout: deny works, a dead warden does not block",
                    codex / "hooks.json",
                )
            )
        if Layer.BASE_URL in sel:
            url = base_url or DEFAULT_ROUTER_BASE_URL
            steps.append(
                InstallStep(
                    Layer.BASE_URL,
                    f"Register router model provider ({url}/v1, wire_api=chat) and select it",
                    codex / "config.toml",
                )
            )
        if Layer.PANDO in sel:
            steps += _pando_plan_steps("codex", home)
        _append_unsupported_steps(steps, self, sel)
        return steps

    def apply(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[Path]:
        sel = _resolve_layers(layers)
        codex = home / ".codex"
        codex.mkdir(parents=True, exist_ok=True)
        modified: list[Path] = []
        if Layer.SKILL in sel:
            modified.append(_install_skill(home, ".codex/skills"))
        if Layer.MCP in sel:
            modified += _patch_codex_config(codex / "config.toml")
        if Layer.INSTRUCTIONS in sel:
            modified += _patch_instructions(codex / "AGENTS.md")
        if Layer.WARDEN in sel:
            modified += _patch_codex_warden(codex / "hooks.json", enforce=enforce)
        if Layer.BASE_URL in sel:
            modified += _patch_codex_base_url(
                codex / "config.toml", base_url or DEFAULT_ROUTER_BASE_URL
            )
        if Layer.PANDO in sel:
            modified += _apply_pando("codex", home)
        return modified

    def reverse(self, home: Path, layers: "set[Layer] | None" = None) -> list[Path]:
        def has(layer: Layer) -> bool:
            return layers is None or layer in layers

        codex = home / ".codex"
        if has(Layer.WARDEN):
            _refuse_unknown_warden_hooks(codex / "hooks.json")
        modified: list[Path] = []
        if has(Layer.SKILL):
            modified += _remove_skill_both(home, ".codex/skills")
        toml_path = codex / "config.toml"
        text = toml_path.read_text() if toml_path.exists() else ""
        if has(Layer.MCP) and _CODEX_MCP_RE.search(text):
            _backup(toml_path)
            cleaned = _CODEX_MCP_RE.sub("", text).rstrip("\n")
            toml_path.write_text(cleaned + "\n" if cleaned else "")
            modified.append(toml_path)
        if has(Layer.WARDEN):
            modified += _pop_json_hook(codex / "hooks.json", match=_is_warden_hook)
        if has(Layer.BASE_URL):
            modified += _unpatch_codex_base_url(codex / "config.toml")
        if has(Layer.INSTRUCTIONS):
            modified += _unpatch_instructions(codex / "AGENTS.md")
        if has(Layer.PANDO):
            modified += _reverse_pando("codex", home)
        return modified


_CODEX_MCP_RE = re.compile(
    r'\n\[mcp_servers\.opendaisugi\]\ncommand = "((?:[^"\\\n]|\\.)*)"\nargs = \["mcp", "serve"\]\n'
)


def _codex_mcp_block() -> str:
    return (
        "\n[mcp_servers.opendaisugi]\n"
        f"command = {json.dumps(daisugi_exe())}\n"
        'args = ["mcp", "serve"]\n'
    )


def _patch_codex_config(config_path: Path) -> list[Path]:
    existing = config_path.read_text() if config_path.exists() else ""
    block = _codex_mcp_block()
    if "[mcp_servers.opendaisugi]" in existing:
        # An older install wrote a bare `daisugi`. Replace that block with the
        # absolute one; a block the user edited is left as it is.
        m = _CODEX_MCP_RE.search(existing)
        if not m or m.group(0) == block:
            return []
        old_cmd = re.search(r'command = "((?:[^"\\\n]|\\.)*)"', m.group(0))
        try:
            old = json.loads('"' + old_cmd.group(1) + '"') if old_cmd else ""
        except ValueError:
            return []
        if not _is_entry_word(old):
            return []
        if _keeps_path(old, daisugi_exe()):
            _WARDEN_NOTES.append(_left_mcp_note(config_path, old, daisugi_exe()))
            return []
        _backup(config_path)
        config_path.write_text(existing[: m.start()] + block + existing[m.end() :])
        return [config_path]
    if config_path.exists():
        _backup(config_path)
    config_path.write_text(existing.rstrip("\n") + ("\n" if existing else "") + block)
    return [config_path]


def _patch_codex_warden(hooks_path: Path, *, enforce: bool = False) -> list[Path]:
    """Merge the fail-closed warden's PreToolUse hook into Codex ``hooks.json``.

    Codex (v0.114+) copied Claude Code's hook schema — same PreToolUse stdin
    shape (``tool_name`` + ``tool_input.command``), same exit-2 deny — so the
    canonical warden entry from ``warden_settings_json`` serves both, with one
    rewrite: Codex regex-matches matchers, and Claude's ``*`` glob is not a
    valid regex, so the matcher becomes ``.*``.

    Honesty note, mirrored in the plan step: Codex hooks fail OPEN — a hook
    that crashes, times out, or emits invalid JSON lets the tool run. The
    warden's inner verify timeout denies before the outer timeout can fail
    open, but a dead warden process does not block. Same idempotency/backup
    discipline as ``_patch_claude_warden``.
    """
    from opendaisugi import warden as _warden
    from opendaisugi.warden import warden_settings_json

    root = _absolute_warden_root(_warden.DEFAULT_WARDEN_ROOT)
    warden_entry = json.loads(
        warden_settings_json(mode="enforce" if enforce else "audit", root=root)
    )["hooks"]["PreToolUse"][0]
    warden_entry["matcher"] = ".*"

    existed = hooks_path.exists()
    if existed:
        try:
            hooks_cfg: dict = json.loads(hooks_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{hooks_path} is not valid JSON; skipping warden hook installation. "
                f"Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        hooks_cfg = {}
    pre = hooks_cfg.setdefault("hooks", {}).setdefault("PreToolUse", [])
    existing_commands = {
        h.get("command", "")
        for entry in pre
        for h in entry.get("hooks", [])
        if h.get("type") == "command"
    }
    if any(warden_hook_kind(c) is not None for c in existing_commands):
        # An older form of the same hook is brought up to today's command
        # in place (as for Claude Code); any other warden hook is left alone.
        rewritten = False
        stepped_up = False
        kept = False
        kept_upgraded = False
        new_command = warden_entry["hooks"][0]["command"]
        for entry in pre:
            for h in entry.get("hooks", []):
                if h.get("type") != "command":
                    continue
                relation = _warden_mode_relation(h.get("command", ""), new_command)
                if relation == "up":
                    h["command"] = new_command
                    rewritten = stepped_up = True
                elif relation == "keep":
                    kept = True
                    upgraded = _upgraded_warden_command(h.get("command", ""))
                    if upgraded:
                        h["command"] = upgraded
                        rewritten = kept_upgraded = True
                elif _upgraded_warden_command(h.get("command", "")) == new_command:
                    h["command"] = new_command
                    rewritten = True
        if not rewritten:
            if kept:
                _WARDEN_NOTES.append(_warden_keep_note("Codex", hooks_path))
            return []
        _backup(hooks_path)
        hooks_path.write_text(json.dumps(hooks_cfg, indent=2) + "\n")
        if stepped_up:
            _WARDEN_NOTES.append(_warden_step_up_note("Codex", hooks_path))
        elif kept:
            _WARDEN_NOTES.append(_warden_keep_note("Codex", hooks_path, kept_upgraded))
        return [hooks_path]
    if existed:
        _backup(hooks_path)
    pre.append(warden_entry)
    hooks_path.write_text(json.dumps(hooks_cfg, indent=2) + "\n")
    return [hooks_path]


_CODEX_PROVIDER_SELECT = 'model_provider = "opendaisugi"'


def _codex_provider_block(url: str) -> str:
    return (
        "\n[model_providers.opendaisugi]\n"
        'name = "openDaisugi router"\n'
        f'base_url = "{url}/v1"\n'
        'wire_api = "chat"\n'
    )


def _patch_codex_base_url(config_path: Path, url: str) -> list[Path]:
    """Register the router as a Codex model provider and select it.

    Two edits, both idempotent: the ``[model_providers.opendaisugi]`` table is
    appended, and the top-level ``model_provider`` selector is PREPENDED —
    top-level TOML keys must precede any table header or they silently nest
    under the last table.
    """
    existing = config_path.read_text() if config_path.exists() else ""
    if "[model_providers.opendaisugi]" in existing:
        return []
    if config_path.exists():
        _backup(config_path)
    text = existing
    if _CODEX_PROVIDER_SELECT not in text:
        text = _CODEX_PROVIDER_SELECT + "\n" + text
    config_path.write_text(text.rstrip("\n") + "\n" + _codex_provider_block(url))
    return [config_path]


def _unpatch_codex_base_url(config_path: Path) -> list[Path]:
    if not config_path.exists():
        return []
    text = config_path.read_text()
    if "[model_providers.opendaisugi]" not in text and _CODEX_PROVIDER_SELECT not in text:
        return []
    _backup(config_path)
    import re as _re

    text = _re.sub(r"\n?\[model_providers\.opendaisugi\][^\[]*", "\n", text, count=1)
    text = text.replace(_CODEX_PROVIDER_SELECT + "\n", "", 1)
    cleaned = text.strip("\n")
    config_path.write_text(cleaned + "\n" if cleaned else "")
    return [config_path]


# ---------------------------------------------------------------------------
# OpenClaw
# ---------------------------------------------------------------------------


@dataclass
class OpenClawRuntime:
    name: str = "OpenClaw"

    def detect(self, home: Path) -> bool:
        return (home / ".openclaw").is_dir()

    def _workspace(self, home: Path) -> Path:
        return home / ".openclaw" / "workspace"

    def unsupported_layers(self) -> dict[Layer, str]:
        # BASE_URL IS wired (below) — OpenClaw speaks Anthropic Messages.
        return {Layer.WARDEN: "external/JS-shim warden not yet wired — follow-up"}

    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        oc = home / ".openclaw"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Copy opendaisugi-checklist skill",
                    self._workspace(home) / "skills" / _SKILL_NAME,
                )
            )
        if Layer.MCP in sel:
            steps.append(
                InstallStep(Layer.MCP, "Register opendaisugi MCP server", oc / "openclaw.json")
            )
        if Layer.CAPTURE in sel:
            steps.append(
                InstallStep(
                    Layer.CAPTURE,
                    "Install before_tool_call capture plugin",
                    oc / "extensions" / "opendaisugi",
                )
            )
        if Layer.INSTRUCTIONS in sel:
            steps.append(
                InstallStep(
                    Layer.INSTRUCTIONS,
                    _pathway_guidance_title(self._workspace(home) / "AGENTS.md"),
                    self._workspace(home) / "AGENTS.md",
                )
            )
        if Layer.BASE_URL in sel:
            url = base_url or DEFAULT_ROUTER_BASE_URL
            steps.append(
                InstallStep(
                    Layer.BASE_URL,
                    f"Register opendaisugi router provider (anthropic-messages) at {url} "
                    f"— select it as the active provider manually",
                    oc / "openclaw.json",
                )
            )
        _append_unsupported_steps(steps, self, sel)
        return steps

    def apply(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
    ) -> list[Path]:
        sel = _resolve_layers(layers)
        oc = home / ".openclaw"
        ws = self._workspace(home)
        ws.mkdir(parents=True, exist_ok=True)
        modified: list[Path] = []
        if Layer.SKILL in sel:
            modified.append(_link_skill(ws / "skills" / _SKILL_NAME))
        if Layer.MCP in sel:
            modified += _patch_openclaw_config(oc / "openclaw.json")
        if Layer.INSTRUCTIONS in sel:
            modified += _patch_instructions(ws / "AGENTS.md")
        if Layer.CAPTURE in sel:
            modified.append(_install_openclaw_plugin(home))
        if Layer.BASE_URL in sel:
            modified += _patch_openclaw_base_url(
                oc / "openclaw.json", base_url or DEFAULT_ROUTER_BASE_URL
            )
        return modified

    def reverse(self, home: Path, layers: "set[Layer] | None" = None) -> list[Path]:
        def has(layer: Layer) -> bool:
            return layers is None or layer in layers

        oc = home / ".openclaw"
        ws = self._workspace(home)
        modified: list[Path] = []
        if has(Layer.SKILL):
            modified += _remove_skill(ws / "skills" / _SKILL_NAME)
        cfg_path = oc / "openclaw.json"
        if cfg_path.exists():
            try:
                cfg = json.loads(cfg_path.read_text())
            except json.JSONDecodeError:
                try:
                    cfg = json.loads(_strip_json5_comments(cfg_path.read_text()))
                except json.JSONDecodeError:
                    cfg = None
            if isinstance(cfg, dict):
                changed = False
                servers = cfg.get("mcp", {}).get("servers", {})
                if has(Layer.MCP) and "opendaisugi" in servers:  # only if ours is present
                    del servers["opendaisugi"]
                    if not servers:
                        cfg["mcp"].pop("servers", None)
                        if not cfg["mcp"]:
                            del cfg["mcp"]
                    changed = True
                providers = cfg.get("models", {}).get("providers", {})
                if has(Layer.BASE_URL) and "opendaisugi" in providers:
                    del providers["opendaisugi"]
                    if not providers:
                        cfg["models"].pop("providers", None)
                        if not cfg["models"]:
                            del cfg["models"]
                    changed = True
                if changed:
                    _backup(cfg_path)
                    cfg_path.write_text(json.dumps(cfg, indent=2) + "\n")
                    modified.append(cfg_path)
        if has(Layer.CAPTURE):
            modified += _remove_dir(oc / "extensions" / "opendaisugi")
        if has(Layer.INSTRUCTIONS):
            modified += _unpatch_instructions(ws / "AGENTS.md")
        return modified


def _install_openclaw_plugin(home: Path) -> Path:
    """Materialize the shipped before_tool_call plugin into ~/.openclaw/extensions."""
    import importlib.resources as _ir

    dest = home / ".openclaw" / "extensions" / "opendaisugi"
    dest.mkdir(parents=True, exist_ok=True)
    src = _ir.files("opendaisugi").joinpath("install_assets", "openclaw_plugin")
    for name in ("index.mjs", "package.json", "openclaw.plugin.json"):
        out = dest / name
        if out.is_symlink():
            out.unlink()  # never write THROUGH a pre-planted symlink (arbitrary file write)
        out.write_text(src.joinpath(name).read_text(encoding="utf-8"))
    return dest


SUPPORTED_HARNESSES: tuple[str, ...] = ("pi", "opencode")


def pi_extension_dir(home: Path) -> Path:
    """Where pi auto-discovers the daisugi-warden extension."""
    return home / ".pi" / "agent" / "extensions" / "daisugi-warden"


def _install_pi_extension(home: Path) -> list[Path]:
    """Materialize the daisugi-warden pi extension into
    ~/.pi/agent/extensions/daisugi-warden/.

    pi does not reload an extension on its own in --mode rpc. Restart the pi
    process, or run /reload in an interactive session, for a new or updated
    extension to take effect. Idempotent: a second run writes nothing.

    No per-install config. The extension carries no --mode of its own, so
    there is nothing here for an enforce flag to write. Its mode is
    config.yaml's warden_mode, shared with every other host.

    The cross-tenant uninstall limitation on ~/.agents/skills does not
    apply here: ~/.pi/agent/extensions/daisugi-warden/ belongs to pi alone.
    """
    import importlib.resources as _ir

    dest = pi_extension_dir(home)
    dest.mkdir(parents=True, exist_ok=True)
    src = _ir.files("opendaisugi").joinpath("harness_pi", "extension")
    out = dest / "index.ts"
    if out.is_symlink():
        out.unlink()  # never write THROUGH a pre-planted symlink
    new_text = src.joinpath("index.ts").read_text(encoding="utf-8")
    if not out.exists() or out.read_text(encoding="utf-8") != new_text:
        out.write_text(new_text, encoding="utf-8")
        return [out]
    return []


def _remove_pi_extension(home: Path) -> list[Path]:
    return _remove_dir(pi_extension_dir(home))


def opencode_plugin_path(home: Path, env: Mapping[str, str] | None = None) -> Path:
    """Where OpenCode loads the daisugi warden plugin from.

    OpenCode's global config directory is ``$XDG_CONFIG_HOME/opencode`` when
    that variable holds an absolute path, else ``~/.config/opencode``. It
    loads each file in its ``plugins`` directory at start.
    """
    env = os.environ if env is None else env
    xdg = env.get("XDG_CONFIG_HOME", "")
    base = Path(xdg) if xdg and os.path.isabs(xdg) else home / ".config"
    return base / "opencode" / "plugins" / "daisugi-warden.ts"


def _install_opencode_plugin(home: Path) -> list[Path]:
    """Copy the daisugi warden plugin into OpenCode's global plugin directory.

    The copy is byte for byte. The plugin carries no warden mode, so there is
    nothing to fill in: its mode is config.yaml's warden_mode, shared with
    every other host. OpenCode loads plugins at start, so a running
    OpenCode needs a restart. Idempotent: a second run writes nothing.

    This does not touch opencode.json. Its "plugin" list names npm packages
    that OpenCode installs at start. A local file needs no entry there,
    since OpenCode loads every file in the plugins directory.
    """
    import importlib.resources as _ir

    out = opencode_plugin_path(home)
    if out.parent.is_symlink():
        raise ValueError(
            f"{out.parent} is a symlink, so the plugin would land somewhere else. "
            "Replace it with a directory, then run this again."
        )
    out.parent.mkdir(parents=True, exist_ok=True)
    if out.is_symlink():
        out.unlink()  # never write THROUGH a pre-planted symlink
    src = _ir.files("opendaisugi").joinpath("harness_opencode", "plugin", "daisugi-warden.ts")
    new_text = src.read_text(encoding="utf-8")
    if not out.exists() or out.read_text(encoding="utf-8") != new_text:
        out.write_text(new_text, encoding="utf-8")
        return [out]
    return []


def _remove_opencode_plugin(home: Path) -> list[Path]:
    """Remove the warden plugin file. The plugins directory stays: it can hold
    the user's own plugins."""
    out = opencode_plugin_path(home)
    if out.is_symlink() or out.exists():
        out.unlink()
        return [out]
    return []


_HARNESS_INSTALLERS: dict[str, tuple] = {
    "pi": (_install_pi_extension, _remove_pi_extension),
    "opencode": (_install_opencode_plugin, _remove_opencode_plugin),
}


def harness_extension_target(name: str, *, home: Path) -> Path:
    """The path one loop harness's warden extension is written to."""
    if name == "opencode":
        return opencode_plugin_path(home)
    return pi_extension_dir(home) / "index.ts"


def install_harness_extension(name: str, *, home: Path) -> list[Path]:
    """Install one loop harness's warden extension. Returns the paths written."""
    installer, _ = _HARNESS_INSTALLERS[name]
    return installer(home)


def uninstall_harness_extension(name: str, *, home: Path) -> list[Path]:
    """Remove one loop harness's warden extension. Returns the paths removed."""
    _, remover = _HARNESS_INSTALLERS[name]
    return remover(home)


def _strip_json5_comments(text: str) -> str:
    """Best-effort JSON5 → JSON: drop // and /* */ comments and trailing commas.

    String-aware: a ``//`` or ``/*`` inside a JSON string value (e.g. a URL like
    ``https://…``) is NOT a comment and is preserved. The original file is always
    backed up before rewrite, so a dropped comment is recoverable.
    """
    out: list[str] = []
    i, n = 0, len(text)
    in_str = False
    while i < n:
        c = text[i]
        if in_str:
            out.append(c)
            if c == "\\" and i + 1 < n:  # keep escaped char verbatim
                out.append(text[i + 1])
                i += 2
                continue
            if c == '"':
                in_str = False
            i += 1
            continue
        if c == '"':
            in_str = True
            out.append(c)
            i += 1
            continue
        if c == "/" and i + 1 < n and text[i + 1] == "/":  # line comment
            j = text.find("\n", i)
            i = n if j == -1 else j
            continue
        if c == "/" and i + 1 < n and text[i + 1] == "*":  # block comment
            j = text.find("*/", i + 2)
            i = n if j == -1 else j + 2
            continue
        out.append(c)
        i += 1
    text = "".join(out)
    # Comments already stripped string-awarely above; only trailing commas remain.
    text = re.sub(r",(\s*[}\]])", r"\1", text)
    return text


def _patch_openclaw_config(config_path: Path) -> list[Path]:
    """Register MCP under ``mcp.servers`` in OpenClaw's JSON5 ``openclaw.json``."""
    return _patch_mcp(
        config_path,
        _JSON5,
        ("mcp", "servers"),
        _mcp_entry(),
    )


def _patch_openclaw_base_url(config_path: Path, url: str) -> list[Path]:
    """Register the router as an OpenClaw model provider (ADR-0013 BASE_URL).

    OpenClaw can speak Anthropic Messages, so the router is wire-compatible
    and this is genuinely wired (unlike Codex/Hermes). Registers the provider
    block under ``models.providers.opendaisugi`` but deliberately does NOT
    flip whatever key selects the *active* provider — that key's shape isn't
    verified here, and guessing wrong would silently redirect a live session.
    Register-and-note, not register-and-switch.
    """
    return _patch_mcp(
        config_path,
        _JSON5,
        ("models", "providers"),
        {"baseUrl": url, "api": "anthropic-messages"},
        what="router provider registration",
    )


# ---------------------------------------------------------------------------
# Uninstall helpers
# ---------------------------------------------------------------------------


def _remove_skill(target: Path) -> list[Path]:
    """Remove a symlinked (or copied) skill directory if present (symlink-safe)."""
    from opendaisugi.skill_paths import _clear

    return [target] if _clear(target) else []


def _remove_skill_both(home: Path, fallback_subdir: str) -> list[Path]:
    """Remove the skill from BOTH candidate paths.

    ``_agents_skill_target`` chooses cross-vendor vs per-vendor from mutable
    filesystem state, which can differ between apply and reverse (e.g. another
    runtime created ~/.agents/skills mid-install). Removing both candidates
    makes uninstall correct regardless of which path the symlink actually landed
    in — only one will exist.

    KNOWN LIMITATION (cross-tenant, deferred): the ~/.agents/skills skill is
    SHARED across harnesses (Claude + Codex both target it). Uninstalling one
    harness removes the shared skill out from under any other still-installed
    harness. Acceptable for now (the common case is one harness, or a full
    uninstall); a proper fix would ref-count installed harnesses before removing
    the cross-vendor skill. See project memory.
    """
    removed: list[Path] = []
    removed += _remove_skill(home / ".agents" / "skills" / _SKILL_NAME)
    removed += _remove_skill(home / fallback_subdir / _SKILL_NAME)
    return removed


def _remove_dir(target: Path) -> list[Path]:
    """Remove a materialized directory (e.g. an OpenClaw plugin), symlink-safe."""
    from opendaisugi.skill_paths import _clear

    if _clear(target):
        return [target]
    return []


def _unpatch_instructions(md_path: Path) -> list[Path]:
    """Remove the managed, marker-bounded block from an instruction file.

    Requires BOTH the opening and closing marker — a single stray marker in user
    prose is left alone (never truncate the file to EOF). No-op if no marker.
    """
    if not md_path.exists():
        return []
    text = _read_raw(md_path)
    span = _managed_span(text)
    if span is None:
        return []  # no block, or only one marker line: user prose
    start, end = span
    cleaned = (text[:start] + text[end:]).rstrip("\n")
    _backup(md_path)
    _write_raw(md_path, cleaned + "\n" if cleaned else "")
    return [md_path]


def _pop_json_mcp(json_path: Path, *, mcp_key: str, name: str = "opendaisugi") -> list[Path]:
    """Remove the MCP server called ``name`` from a JSON config; no-op if absent."""
    if not json_path.exists():
        return []
    try:
        cfg = json.loads(json_path.read_text())
    except (json.JSONDecodeError, OSError):
        return []
    if name not in cfg.get(mcp_key, {}):
        return []  # not managed by us — leave the file untouched
    _backup(json_path)
    cfg[mcp_key].pop(name, None)
    if not cfg[mcp_key]:
        del cfg[mcp_key]
    json_path.write_text(json.dumps(cfg, indent=2) + "\n")
    return [json_path]


def _pop_json_hook(
    settings_path: Path,
    *,
    match: "Callable[[object], bool]",
    events: tuple[str, ...] = ("PreToolUse",),
) -> list[Path]:
    """Remove the opendaisugi hook(s) whose command ``match`` accepts from the
    given settings.json hook event(s); no-op if absent from all of them.

    ``events`` defaults to ``("PreToolUse",)`` — every call site before the
    floor-report hooks (spec-01) removed exactly that one event, and this
    keeps them byte-identical. The report hooks live on ``Stop`` and
    ``Notification`` instead, so their reversal passes both.
    """
    if not settings_path.exists():
        return []
    try:
        s = json.loads(settings_path.read_text())
    except (json.JSONDecodeError, OSError):
        return []
    hooks = s.get("hooks", {})
    present = any(
        match(h.get("command"))
        for ev in events
        for e in (hooks.get(ev) or [])
        for h in e.get("hooks", [])
    )
    if not present:
        return []  # nothing of ours present
    _backup(settings_path)
    for ev in events:
        entries = hooks.get(ev)
        if not entries:
            continue
        for e in entries:
            e["hooks"] = [h for h in e.get("hooks", []) if not match(h.get("command"))]
        hooks[ev] = [e for e in entries if e.get("hooks")]
        if not hooks[ev]:
            del hooks[ev]
    if not hooks:
        del s["hooks"]
    settings_path.write_text(json.dumps(s, indent=2) + "\n")
    return [settings_path]


def _pop_json_env_key(settings_path: Path, *, key: str) -> list[Path]:
    """Remove one key from settings.json's ``env`` block; no-op if absent.

    Used to reverse the BASE_URL layer's ``ANTHROPIC_BASE_URL``. Leaves every
    other env key untouched, and drops ``env`` itself only once it's empty.
    """
    if not settings_path.exists():
        return []
    try:
        s = json.loads(settings_path.read_text())
    except (json.JSONDecodeError, OSError):
        return []
    env = s.get("env")
    if not isinstance(env, dict) or key not in env:
        return []  # nothing of ours present
    _backup(settings_path)
    del env[key]
    if not env:
        del s["env"]
    settings_path.write_text(json.dumps(s, indent=2) + "\n")
    return [settings_path]


# ---------------------------------------------------------------------------
# Core
# ---------------------------------------------------------------------------

_ALL_RUNTIMES: list = [
    ClaudeCodeRuntime(),
    CodexRuntime(),
    HermesRuntime(),
    OpenClawRuntime(),
]


_RUNTIME_KEYS: dict[str, str] = {
    "claude": "Claude Code",
    "codex": "Codex",
    "hermes": "Hermes",
    "openclaw": "OpenClaw",
}


def _select_runtimes(names: list[str]) -> list:
    """Resolve --runtime fragments to runtime instances by exact key or unique prefix.

    Matches against the canonical short keys (claude/codex/hermes/openclaw), not
    substrings of the display names — so ``code`` resolves to Codex only, never
    also Claude Code. Raises ValueError if a fragment matches zero or more than
    one key (e.g. ``c`` is ambiguous between claude and codex).
    """
    by_name = {r.name: r for r in _ALL_RUNTIMES}
    selected: dict[str, object] = {}
    for raw in names:
        frag = raw.lower().strip()
        matches = [k for k in _RUNTIME_KEYS if k == frag] or [
            k for k in _RUNTIME_KEYS if k.startswith(frag)
        ]
        if len(matches) != 1:
            valid = ", ".join(_RUNTIME_KEYS)
            raise ValueError(
                f"--runtime {raw!r} matched {len(matches)} runtimes; use one of: {valid}"
            )
        name = _RUNTIME_KEYS[matches[0]]
        selected[name] = by_name[name]
    return list(selected.values())


def uninstall(
    *,
    home: Path | None = None,
    runtimes: list | None = None,
    layers: "set[Layer] | None" = None,
) -> InstallResult:
    """Reverse the managed changes for the given (or detected) runtimes.

    ``layers=None`` reverses everything. A set reverses only those layers.
    """
    home = home or Path.home()
    active = runtimes if runtimes is not None else detect_runtimes(home=home)
    modified: list[Path] = []
    failures: list[str] = []
    for rt in active:
        reverse = getattr(rt, "reverse", None)
        if reverse is None:
            continue
        try:
            modified.extend(reverse(home) if layers is None else reverse(home, layers))
        except Exception as exc:  # one malformed config must not abort the rest
            failures.append(f"{rt.name}: {exc}")
    what = "" if layers is None else f"{', '.join(component_names(layers))} "
    summary = f"Uninstalled {what}from: {', '.join(r.name for r in active) or '(none)'}"
    if failures:
        summary += "\nFailures (left untouched): " + "; ".join(failures)
    return InstallResult(
        dry_run=False,
        planned=[],
        modified_files=modified,
        summary=summary,
        failures=failures,
    )


def runtime_key(rt) -> str:
    """The short key (claude, codex, hermes, openclaw) of a runtime."""
    return next(k for k, v in _RUNTIME_KEYS.items() if v == rt.name)


def layer_present(rt, home: Path, layer: Layer) -> bool:
    """Whether ``rt`` already holds the managed change of ``layer`` under ``home``.

    The test is what an uninstall of that one layer would do, run on a copy of
    the files the layer touches in a scratch home; nothing under ``home`` is
    written. A layer whose uninstall refuses counts as present, so a caller
    never claims to have added what it could not read.
    """
    import tempfile

    reverse = getattr(rt, "reverse", None)
    if reverse is None:
        return False
    with tempfile.TemporaryDirectory(prefix="daisugi-probe-") as td:
        try:
            scratch = Path(td)
            rels: list[Path] = []
            for step in rt.plan(home, set(Layer), base_url=DEFAULT_ROUTER_BASE_URL):
                if step.target is not None:
                    rels.append(Path(step.target))
            for sub in (".agents", ".claude", ".codex"):
                rels.append(home / sub / "skills" / _SKILL_NAME)
            seen: set[Path] = set()
            for src in rels:
                try:
                    rel = src.relative_to(home)
                except ValueError:
                    continue
                if rel in seen:
                    continue
                seen.add(rel)
                dst = scratch / rel
                if not (src.is_symlink() or src.exists()):
                    continue
                dst.parent.mkdir(parents=True, exist_ok=True)
                if src.is_symlink():
                    dst.symlink_to(os.readlink(src))
                elif src.is_dir():
                    shutil.copytree(src, dst, symlinks=True, dirs_exist_ok=True)
                else:
                    shutil.copy2(src, dst)
            return bool(reverse(scratch, {layer}))
        except Exception:  # noqa: BLE001 - a probe that cannot run says present, never "ours"
            return True


def detect_runtimes(*, home: Path | None = None) -> list:
    home = home or Path.home()
    return [r for r in _ALL_RUNTIMES if r.detect(home)]


RESTART_LINE = (
    "Restart your agent so the hook loads. To check it works, use one tool, "
    "then run: daisugi hook list"
)


def _hook_command_programs(command: object) -> list[str]:
    """The program of each record or warden hook segment in a hook command."""
    if not isinstance(command, str):
        return []
    found = record_hook_programs(command)
    if found:
        return found
    if warden_hook_args(command) is None:
        return []
    tokens = command_tokens(command)
    segs = _segments(tokens) if tokens else []
    words = _skip_env(segs[0]) if segs else []
    return words[:1]


def _walk_commands(node: object, out: list[str]) -> None:
    """Every hook command in a settings tree: a dict's ``command`` text."""
    if isinstance(node, dict):
        cmd = node.get("command")
        if isinstance(cmd, str):
            out.extend(_hook_command_programs(cmd))
        for v in node.values():
            _walk_commands(v, out)
    elif isinstance(node, list):
        for v in node:
            _walk_commands(v, out)


def _mcp_command(node: object, *path: str) -> list[str]:
    for key in path:
        node = node.get(key) if isinstance(node, dict) else None
    cmd = node.get("command") if isinstance(node, dict) else None
    return [cmd] if isinstance(cmd, str) and cmd else []


def _read_with(path: Path, parse) -> object:
    try:
        return parse(path.read_text(encoding="utf-8"))
    except (OSError, ValueError, yaml.YAMLError):
        return None


def written_programs(runtime: object, home: Path) -> list[str]:
    """The program word of each hook and MCP command a runtime's config holds
    on disk, in file order, without repeats. It reads the files, not what the
    install meant to write."""
    name = getattr(runtime, "name", "")
    words: list[str] = []
    if name == "Claude Code":
        settings = _read_with(home / ".claude" / "settings.json", json.loads)
        _walk_commands(settings, words)
        words += _mcp_command(
            _read_with(home / ".claude.json", json.loads), "mcpServers", "opendaisugi"
        )
    elif name == "Codex":
        text = _read_with(home / ".codex" / "config.toml", str)
        m = _CODEX_MCP_RE.search("\n" + text) if isinstance(text, str) else None
        if m:
            try:
                words.append(json.loads('"' + m.group(1) + '"'))
            except ValueError:
                pass
        _walk_commands(_read_with(home / ".codex" / "hooks.json", json.loads), words)
    elif name == "Hermes":
        cfg = _read_with(home / ".hermes" / "config.yaml", yaml.safe_load)
        words += _mcp_command(cfg, "mcp_servers", "opendaisugi")
        _walk_commands(cfg.get("hooks") if isinstance(cfg, dict) else None, words)
    elif name == "OpenClaw":
        cfg = _read_with(home / ".openclaw" / "openclaw.json", _json5_parse)
        words += _mcp_command(cfg, "mcp", "servers", "opendaisugi")
    return list(dict.fromkeys(words))


def _why_unrunnable(word: str) -> str | None:
    """Why the agent cannot start a program word, or None when it can."""
    if os.sep in word:
        if not os.path.isfile(word):
            return "is not there"
        if not os.access(word, os.X_OK):
            return "is not executable"
        return None
    return None if shutil.which(word) else "is not on the PATH"


def unrunnable_commands(runtimes: list, home: Path | None = None) -> list[str]:
    """One plain line for each runtime whose hook or MCP command, read back
    from the config on disk, names a program the agent cannot start: it is
    missing or is not executable. Empty when every one can run."""
    home = home or Path.home()
    out: list[str] = []
    for rt in runtimes:
        bad = []
        for word in written_programs(rt, home):
            why = _why_unrunnable(word)
            if why is not None:
                bad.append(f"The command {word} {why}")
        if bad:
            out.append(
                f"[{rt.name}] "
                + ", and ".join(bad)
                + ", so the agent cannot start its hook or MCP server. "
                "Install daisugi there, then run: daisugi install"
            )
    return out


def temporary_exe_note() -> str | None:
    """One line when the entry point lives in uv's cache, where a cache clean
    removes it and every hook then fails. None otherwise."""
    exe = daisugi_exe()
    for path in (exe, os.path.realpath(exe)):
        if "/.cache/uv/" in path:
            return (
                f"The path {exe} is temporary: it is in uv's cache, and a cache clean "
                "removes it. Install daisugi with: uv tool install opendaisugi, then run "
                "daisugi install again."
            )
    return None


def install(
    *,
    home: Path | None = None,
    dry_run: bool = False,
    yes: bool = False,
    runtimes: list | None = None,
    layers: "set[Layer] | None" = None,
    enforce: bool = False,
    ask: bool = False,
    base_url: str | None = None,
    report: str | None = None,
) -> InstallResult:
    """Install the selected layers into every active runtime.

    ``layers=None`` (the default) installs the five in ``DEFAULT_LAYERS``:
    SKILL, MCP, CAPTURE, INSTRUCTIONS and WARDEN (audit mode unless ``enforce``).
    BASE_URL is opt-in only: pass a set that includes it explicitly.
    ``enforce``, ``ask`` (Task 8 — Claude Code's WARDEN layer only), and
    ``base_url`` are only consumed by runtimes/layers that support them;
    everyone else ignores them.
    """
    home = home or Path.home()
    active = runtimes if runtimes is not None else detect_runtimes(home=home)

    if not active:
        return InstallResult(
            dry_run=dry_run,
            planned=[],
            modified_files=[],
            summary="No supported agent runtimes detected.",
        )

    # A relative warden root is refused before any runtime writes anything.
    if layers is None or Layer.WARDEN in layers:
        from opendaisugi import warden as _warden

        _absolute_warden_root(_warden.DEFAULT_WARDEN_ROOT)

    planned: list[InstallStep] = []
    for rt in active:
        planned.extend(
            rt.plan(home, layers, enforce=enforce, ask=ask, base_url=base_url, report=report)
        )

    if dry_run:
        return InstallResult(
            dry_run=True,
            planned=planned,
            modified_files=[],
            summary=_format_summary(active, planned, modified=[]),
        )

    modified: list[Path] = []
    failures: list[str] = []
    _WARDEN_NOTES.clear()
    for rt in active:
        try:
            modified.extend(
                rt.apply(home, layers, enforce=enforce, ask=ask, base_url=base_url, report=report)
            )
        except Exception as exc:  # one malformed config must not abort the rest
            failures.append(f"{rt.name}: {exc}")

    summary = _format_summary(active, planned, modified=modified)
    if failures:
        summary += "\nFailures (skipped): " + "; ".join(failures)

    return InstallResult(
        dry_run=False,
        planned=planned,
        modified_files=modified,
        summary=summary,
        failures=failures,
        notes=list(_WARDEN_NOTES),
    )


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _backup(path: Path) -> None:
    stamp = time.time_ns()
    dest = path.with_name(f"{path.name}.bak{stamp}")
    n = 0
    while dest.exists():  # never clobber an existing backup
        n += 1
        dest = path.with_name(f"{path.name}.bak{stamp}.{n}")
    shutil.copy2(path, dest)


def _format_summary(active: list, planned: list[InstallStep], modified: list[Path]) -> str:
    lines = [f"Runtimes: {', '.join(r.name for r in active)}"]
    by_layer: dict[str, int] = {}
    gaps = 0
    for s in planned:
        if not s.supported:
            gaps += 1  # a capability-gap note, not a real change — don't count it
            continue
        by_layer[s.layer.value] = by_layer.get(s.layer.value, 0) + 1
    for layer, count in by_layer.items():
        lines.append(f"  [{layer}] {count} change(s)")
    if gaps:
        lines.append(f"  ({gaps} capability gap note(s) — not applied)")
    if modified:
        lines.append(f"Files written: {len(modified)}")
    else:
        lines.append("No files modified.")
    return "\n".join(lines)


def print_skill() -> str:
    """Return the opendaisugi-checklist SKILL.md (back-compat for --print-skill).

    Reads from package data (importlib.resources) so it works after
    ``uv add opendaisugi`` — not just from a source-tree dev checkout.
    """
    import importlib.resources as _ir

    ref = _ir.files("opendaisugi").joinpath("skills", "opendaisugi-checklist", "SKILL.md")
    return ref.read_text(encoding="utf-8")


# ---------------------------------------------------------------------------
# The one-time move after the rename (gate -> warden, gateway -> router). names-ok
#
# The owner's own state carries the old names: two data directories, six
# config keys, a hook entry in two settings files, and two harness
# extensions. `daisugi install` moves each once and prints one line per move.
# Nothing is deleted. The old names below are read on purpose; each line
# carries a names-ok tag for the names check.
# ---------------------------------------------------------------------------

_OLD_HOOK_REWRITES: tuple[tuple[re.Pattern[str], str], ...] = (
    (re.compile(r"\bDAISUGI_GATE_"), "DAISUGI_WARDEN_"),  # names-ok: old env prefix
    (re.compile(r"\bOPENDAISUGI_GATE_"), "OPENDAISUGI_WARDEN_"),  # names-ok: old env prefix
    (re.compile(r"opendaisugi\.gate(?![A-Za-z])"), "opendaisugi.warden"),  # names-ok: old module
    (re.compile(r"opendaisugi\.gate_client"), "opendaisugi.warden_client"),  # names-ok: old module
    (re.compile(r"(?<=daisugi) gate (?=check)"), " warden "),  # names-ok: old verb
    (re.compile(r"(?<=\s)gate (?=check)"), "warden "),  # names-ok: old verb
    (re.compile(r"--gate-root"), "--warden-root"),  # names-ok: old flag
    (re.compile(r"\bgate\.sock\b"), "warden.sock"),  # names-ok: old socket name
)


def _new_hook_command(command: str, data_dir: Path | None = None) -> str | None:
    """An old-form warden hook command in today's words, or None when it is
    not one of ours. The hook keeps its mode and its other words; only the
    renamed words change, so an enforce hook stays an enforce hook."""
    new = command
    for rx, rep in _OLD_HOOK_REWRITES:
        new = rx.sub(rep, new)
    if data_dir is not None:
        # The hook reads the warden dir by path. install moves that dir, so
        # the path moves with it (plain or quoted).
        old_root = re.escape(str(data_dir / "gate"))  # names-ok: old dir
        new = re.sub(
            rf"(--root\s+)(['\"]?){old_root}\2(?=\s|$)",
            lambda m: f"{m.group(1)}{m.group(2)}{data_dir / 'warden'}{m.group(2)}",
            new,
        )
    if new == command or warden_hook_args(new) is None:
        return None
    return new


def _rewrite_old_hooks(path: Path, *, dry_run: bool, data_dir: Path | None = None) -> bool:
    """Rewrite each old-form warden hook command in ``path`` in place. True
    when a command changed. A file that is not valid JSON is left alone."""
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return False
    hooks = data.get("hooks") if isinstance(data, dict) else None
    entries = hooks.get("PreToolUse") if isinstance(hooks, dict) else None
    changed = False
    for entry in entries if isinstance(entries, list) else []:
        inner = entry.get("hooks") if isinstance(entry, dict) else None
        for h in inner if isinstance(inner, list) else []:
            command = h.get("command") if isinstance(h, dict) else None
            new = _new_hook_command(command, data_dir) if isinstance(command, str) else None
            if new is not None:
                h["command"] = new
                changed = True
    if changed and not dry_run:
        _backup(path)
        path.write_text(json.dumps(data, indent=2) + "\n")
    return changed


def _socket_listens(sock: Path) -> bool:
    import socket

    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(0.2)
    try:
        s.connect(str(sock))
        return True
    except OSError:
        return False
    finally:
        s.close()


def move_old_state(
    data_dir: Path,
    home: Path,
    now,
    out,
    *,
    dry_run: bool = False,
) -> list[str]:
    """Move the owner's state from the old names to the new ones, once.

    In order: the warden data directory (refused while a server still
    listens on its socket, or when the new one exists), the router data
    directory (old files join an existing ``router/``), the six renamed
    config keys, the old warden hook in the Claude Code and Codex hook
    files, and the old pi and OpenCode extensions (moved under
    ``<data>/moved-aside/<stamp>/``). One line per move goes to ``out`` and
    into the returned list. Nothing is deleted. With ``dry_run`` the lines
    say "would" and nothing changes.
    """
    lines: list[str] = []
    verb = "would move" if dry_run else "moved"

    def say(text: str) -> None:
        lines.append(text)
        print(text, file=out)

    def refuse(text: str) -> None:
        say(text)
        raise SystemExit(2)

    old_w, new_w = data_dir / "gate", data_dir / "warden"  # names-ok: old dir
    if old_w.is_dir():
        if new_w.exists():
            refuse(
                f"both {old_w} and {new_w} exist; move one aside by hand, then run install again"
            )
        sock = old_w / "gate.sock"  # names-ok: old socket
        if _socket_listens(sock):
            refuse(
                f"a gate server still listens on {sock}; stop it "  # names-ok: old words
                f"(the pid is in {old_w / 'gate.lock'}) and run install again"  # names-ok: old lock
            )
        say(f"{verb} {old_w} to {new_w}")
        if not dry_run:
            old_w.rename(new_w)

    old_r, new_r = data_dir / "gateway", data_dir / "router"  # names-ok: old dir
    if old_r.is_dir():
        if not new_r.exists():
            say(f"{verb} {old_r} to {new_r}")
            if not dry_run:
                old_r.rename(new_r)
        else:
            # `router/` already holds the delegation and label journals. The
            # old files join them, one by one; a name that is in both stops
            # the move.
            clash = sorted(c.name for c in old_r.iterdir() if (new_r / c.name).exists())
            if clash:
                refuse(
                    f"both {old_r} and {new_r} hold {', '.join(clash)}; "
                    "move one aside by hand, then run install again"
                )
            say(f"{verb} {old_r} into {new_r}")
            if not dry_run:
                for child in sorted(old_r.iterdir()):
                    child.rename(new_r / child.name)
                old_r.rmdir()

    from opendaisugi.config import RENAMED_CONFIG_KEYS

    cfg = data_dir / "config.yaml"
    if cfg.is_file():
        try:
            text = cfg.read_text()
        except OSError:
            text = ""
        new_text = text
        for old, new in RENAMED_CONFIG_KEYS:
            rx = re.compile(rf"^(\s*){re.escape(old)}(\s*:)", re.MULTILINE)
            if rx.search(new_text):
                new_text = rx.sub(rf"\1{new}\2", new_text)
                say(f"config: {old} is now {new}")
        if new_text != text and not dry_run:
            cfg.write_text(new_text)

    for hooks_file in (home / ".claude" / "settings.json", home / ".codex" / "hooks.json"):
        if hooks_file.is_file() and _rewrite_old_hooks(
            hooks_file, dry_run=dry_run, data_dir=data_dir
        ):
            say(f"replaced the old warden hook in {hooks_file}")

    stamp = now.strftime("%Y%m%dT%H%M%SZ")
    aside = data_dir / "moved-aside" / stamp
    pi_new = pi_extension_dir(home)
    pi_old = pi_new.with_name("daisugi-gate")  # names-ok: old extension dir
    oc_new = opencode_plugin_path(home)
    oc_old = oc_new.with_name("daisugi-gate.ts")  # names-ok: old plugin file
    for old, name in (
        (pi_old, "pi-daisugi-gate"),  # names-ok: old extension
        (oc_old, "opencode-daisugi-gate.ts"),  # names-ok: old plugin
    ):  # names-ok: old names
        if old.is_symlink() or old.exists():
            dest = aside / name
            say(f"{verb} aside {old} to {dest}")
            if not dry_run:
                dest.parent.mkdir(parents=True, exist_ok=True)
                old.rename(dest)

    # The delegate tool is now spawn. An envelope that allows the old tool
    # name must be edited by its owner; install edits no envelope.
    root = new_w if new_w.exists() else old_w
    for env_file in sorted((root / "envelopes").glob("*.json")) if root.is_dir() else []:
        try:
            allow = json.loads(env_file.read_text()).get("permissions", {}).get("mcp_allowlist")
        except (OSError, ValueError, AttributeError):
            continue
        if (
            isinstance(allow, list)
            and "opendaisugi/delegate" in allow  # names-ok: the old tool name
            and "opendaisugi/spawn" not in allow
        ):
            say(
                f"envelope {env_file.stem} allows mcp opendaisugi/delegate; "  # names-ok: old tool
                "add opendaisugi/spawn (the tool was renamed)"
            )
    return lines
