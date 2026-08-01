"""Plug-and-play installation of openDaisugi into agent runtimes.

``daisugi install`` detects Claude Code, Codex, Hermes, and OpenClaw on the
local machine and installs four layers per harness by default:

  - Skill: symlink the bundled opendaisugi-checklist skill into the harness's
    discovery path (cross-vendor ~/.agents/skills first). Discovered on demand
    — no per-session token cost.
  - MCP: register the ``daisugi mcp serve`` tool server (per-harness syntax).
  - Capture: a pre-tool-call hook feeding distillation (per-harness surface).
  - Instructions: append pathway-routing guidance to the harness's always-on
    instructions file (CLAUDE.md / AGENTS.md).

Two more layers exist but are opt-in (ADR-0013, "one install that both saves
and verifies"), since they are safety/cost relevant and must never be
installed by surprise:

  - Gate: the ADR-0007 fail-closed PreToolUse verify hook. Claude Code only;
    shadow-by-default, ``--enforce`` an explicit opt-in.
  - Base URL: points the harness at the local token-saving gateway
    (ADR-0012). Claude Code (env var) and OpenClaw (a registered provider,
    both speak Anthropic Messages) are wired; Codex and Hermes are not (their
    custom endpoints expect an OpenAI wire) — this is surfaced honestly via
    ``Runtime.unsupported_layers()``, never silently skipped.

All writes are idempotent, backed up before modification, and reversible via
``daisugi install --uninstall``.
"""

from __future__ import annotations

import json
import re
import shutil
import time
import warnings
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import Protocol

import yaml

_CLAUDE_MD_MARKER = "<!-- opendaisugi-managed -->"

_CLAUDE_MD_BLOCK = f"""\
{_CLAUDE_MD_MARKER}
## openDaisugi — automatic pathway routing

Before planning any task with 3 or more steps, call the `find_pathway`
MCP tool. If similarity ≥ 0.85, use the returned cached plan via
`run_plan` instead of re-planning from scratch. When a cached pathway is
used, note it explicitly: "Using cached opendaisugi pathway (similarity
X.XX) — skipping re-plan."

If no pathway matches, proceed normally. After execution, the run is
journaled automatically and will feed distillation on the next
`daisugi tend` cycle.
{_CLAUDE_MD_MARKER}
"""

_PRETOOLUSE_HOOK = {
    "matcher": "Bash|Edit|Write|Read|Glob|Grep|WebFetch|WebSearch",
    "hooks": [{"type": "command", "command": "daisugi hook record --format claude"}],
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
    GATE = "gate"
    BASE_URL = "base_url"


# The four layers installed by default, unchanged since before ADR-0013.
# GATE and BASE_URL are opt-in only — never added here.
DEFAULT_LAYERS: frozenset[Layer] = frozenset(
    {Layer.SKILL, Layer.MCP, Layer.CAPTURE, Layer.INSTRUCTIONS}
)

# The gateway's default bind address (mirrors `daisugi gateway`'s --port 8787).
DEFAULT_GATEWAY_BASE_URL = "http://127.0.0.1:8787"


def _resolve_layers(layers: "set[Layer] | None") -> "set[Layer]":
    """``None`` means "the current default four" — never GATE/BASE_URL by accident."""
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


# ---------------------------------------------------------------------------
# Runtime protocol
# ---------------------------------------------------------------------------


class Runtime(Protocol):
    """An agent harness openDaisugi can install into.

    ``plan`` returns layer-aware :class:`InstallStep` rows for dry-run preview;
    ``apply`` performs the writes and returns the modified paths. ``layers``
    selects which layers to act on (``None`` = the current default four —
    back-compat for every existing call site); GATE and BASE_URL are opt-in
    only. ``enforce``/``base_url`` only matter to runtimes that support those
    two layers — everyone else ignores them. ``reverse`` (optional) always
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
        base_url: str | None = None,
    ) -> list[InstallStep]: ...
    def apply(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        base_url: str | None = None,
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
        base_url: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        claude_dir = home / ".claude"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Symlink opendaisugi-checklist skill",
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
                InstallStep(Layer.INSTRUCTIONS, "Append pathway guidance", claude_dir / "CLAUDE.md")
            )
        if Layer.GATE in sel:
            mode = "ENFORCE" if enforce else "shadow"
            steps.append(
                InstallStep(
                    Layer.GATE,
                    f"Install fail-closed gate PreToolUse hook ({mode})",
                    claude_dir / "settings.json",
                )
            )
        if Layer.BASE_URL in sel:
            url = base_url or DEFAULT_GATEWAY_BASE_URL
            steps.append(
                InstallStep(
                    Layer.BASE_URL,
                    f"Point ANTHROPIC_BASE_URL at {url}",
                    claude_dir / "settings.json",
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
        base_url: str | None = None,
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
        if Layer.GATE in sel:
            modified += _patch_claude_gate(claude_dir / "settings.json", enforce=enforce)
        if Layer.BASE_URL in sel:
            modified += _patch_claude_base_url(
                claude_dir / "settings.json", base_url or DEFAULT_GATEWAY_BASE_URL
            )
        return modified

    def reverse(self, home: Path) -> list[Path]:
        claude_dir = home / ".claude"
        modified: list[Path] = []
        modified += _remove_skill_both(home, ".claude/skills")
        modified += _pop_json_mcp(home / ".claude.json", mcp_key="mcpServers")
        modified += _pop_json_hook(
            claude_dir / "settings.json",
            hook_substr="daisugi hook record",
        )
        modified += _pop_json_hook(
            claude_dir / "settings.json",
            hook_substr=_GATE_HOOK_SUBSTR,
        )
        modified += _pop_json_env_key(claude_dir / "settings.json", key="ANTHROPIC_BASE_URL")
        modified += _unpatch_instructions(claude_dir / "CLAUDE.md")
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
) -> list[Path]:
    """Set ``leaf["opendaisugi"] = entry`` at ``key_path`` in a structured config.

    Generic over JSON / JSON5 via ``fmt``, and over what's being registered
    via ``what`` (the noun used in the skip-warning — "MCP registration" by
    default; callers register other opendaisugi-keyed leaves under this same
    idempotent/backed-up/skip-on-unparseable primitive, e.g. OpenClaw's
    gateway provider block). Never clobbers an unparseable file (these hold
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
    if "opendaisugi" in leaf:
        return []
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
    leaf["opendaisugi"] = entry
    path.write_text(fmt.dump(cfg))
    return [path]


def _patch_claude_mcp(claude_json: Path) -> list[Path]:
    """Register MCP where Claude Code reads user-scope servers: ``~/.claude.json``
    ``mcpServers`` (NOT settings.json, which only honors allow/deny flags)."""
    return _patch_mcp(
        claude_json,
        _JSON,
        ("mcpServers",),
        {"type": "stdio", "command": "daisugi", "args": ["mcp", "serve"]},
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
    if not any("daisugi hook record" in c for c in existing_pre_commands):
        pre.append(_PRETOOLUSE_HOOK)
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


# ADR-0013 GATE layer — the substring the gate hook's command is deduped and
# reversed by. Distinct from the capture hook's "daisugi hook record" so both
# coexist as separate PreToolUse entries.
_GATE_HOOK_SUBSTR = "opendaisugi.gate"


def _patch_claude_gate(
    settings_path: Path,
    *,
    enforce: bool = False,
    root: Path | None = None,
    python: str | None = None,
    verify_timeout_s: float | None = None,
) -> list[Path]:
    """Merge the ADR-0007 fail-closed gate's PreToolUse hook into settings.json.

    Built from ``gate_settings_json`` — the one canonical source for the gate
    command — never hand-constructed. Merges exactly like
    ``_patch_claude_settings`` does for the capture hook: idempotent by the
    command substring ``"opendaisugi.gate"``, skip-and-warn on unparseable
    settings.json, backup only when actually writing. SHADOW unless
    ``enforce=True`` — the gate never enforces without an explicit opt-in.

    Idempotency is by *presence*, not by content: re-running with the same
    flags is a true no-op, but asking for a different mode than what is
    already installed does NOT silently rewrite it (that could just as
    easily silently escalate a shadow install into enforce as the reverse).
    Instead it warns — the honest signal is "run --uninstall first", not a
    config file that quietly disagrees with what the user just asked for.
    """
    from opendaisugi.gate import DEFAULT_GATE_ROOT, gate_settings_json

    kwargs: dict = {
        "mode": "enforce" if enforce else "shadow",
        "fmt": "claude",
        "root": root if root is not None else DEFAULT_GATE_ROOT,
    }
    if python is not None:
        kwargs["python"] = python
    if verify_timeout_s is not None:
        kwargs["verify_timeout_s"] = verify_timeout_s
    gate_entry = json.loads(gate_settings_json(**kwargs))["hooks"]["PreToolUse"][0]
    gate_command = gate_entry["hooks"][0]["command"]

    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping gate hook installation "
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
    if any(_GATE_HOOK_SUBSTR in c for c in existing_commands):
        if not any(c == gate_command for c in existing_commands):
            warnings.warn(
                f"a gate hook is already installed in {settings_path} with a "
                f"different mode/config; run `daisugi install --uninstall` first "
                f"if you want to change it (idempotent by presence, not content).",
                UserWarning,
                stacklevel=2,
            )
        return []  # already installed — idempotent no-op either way

    pre.append(gate_entry)
    if existed:
        _backup(settings_path)
    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
    return [settings_path]


def _patch_claude_base_url(settings_path: Path, url: str) -> list[Path]:
    """Merge ANTHROPIC_BASE_URL into settings.json's env, idempotently.

    Points Claude Code at the local token-saving gateway (ADR-0012's BASE_URL
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
        return []  # already pointed at the gateway — no-op

    env["ANTHROPIC_BASE_URL"] = url
    if existed:
        _backup(settings_path)
    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
    return [settings_path]


def _patch_instructions(md_path: Path) -> list[Path]:
    """Append the managed pathway block to an instruction file, idempotently.

    Works for any always-on instruction file — CLAUDE.md (Claude Code) or
    AGENTS.md (Codex, OpenClaw). The marker guard makes re-runs no-ops.
    """
    existing = md_path.read_text() if md_path.exists() else ""
    if _CLAUDE_MD_MARKER in existing:
        return []  # already present
    md_path.parent.mkdir(parents=True, exist_ok=True)
    updated = existing.rstrip("\n") + ("\n\n" if existing else "") + _CLAUDE_MD_BLOCK
    md_path.write_text(updated)
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
            Layer.GATE: "external/JS-shim gate not yet wired — follow-up",
            Layer.BASE_URL: (
                "gateway speaks Anthropic Messages; this harness's custom endpoint "
                "expects OpenAI — not wired (needs an OpenAI-wire adapter)"
            ),
        }

    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        base_url: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        h = home / ".hermes"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Symlink opendaisugi-checklist skill",
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
        _append_unsupported_steps(steps, self, sel)
        return steps

    def apply(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        base_url: str | None = None,
    ) -> list[Path]:
        sel = _resolve_layers(layers)
        h = home / ".hermes"
        h.mkdir(parents=True, exist_ok=True)
        modified: list[Path] = []
        if Layer.SKILL in sel:
            modified.append(_link_skill(h / "skills" / "opendaisugi" / _SKILL_NAME))
        if Layer.MCP in sel or Layer.CAPTURE in sel:
            # One writer covers both — Hermes registers the MCP server and the
            # capture hook in a single config.yaml rewrite.
            modified += _patch_hermes_config(h / "config.yaml")
        return modified

    def reverse(self, home: Path) -> list[Path]:
        h = home / ".hermes"
        modified: list[Path] = []
        modified += _remove_skill(h / "skills" / "opendaisugi" / _SKILL_NAME)
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
        if isinstance(servers, dict) and "opendaisugi" in servers:
            del servers["opendaisugi"]
            if not servers:
                del cfg["mcp_servers"]
            changed = True
        hooks = cfg.get("hooks")
        if isinstance(hooks, dict) and isinstance(hooks.get("pre_tool_call"), list):
            pre = hooks["pre_tool_call"]
            kept = [
                hk
                for hk in pre
                if not (isinstance(hk, dict) and "daisugi hook record" in hk.get("command", ""))
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


def _patch_hermes_config(config_path: Path) -> list[Path]:
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

    mcp = cfg.setdefault("mcp_servers", {})
    if "opendaisugi" not in mcp:
        mcp["opendaisugi"] = {"command": "daisugi", "args": ["mcp", "serve"]}
        changed = True

    hooks = cfg.setdefault("hooks", {})
    pre = hooks.setdefault("pre_tool_call", [])
    cmd = "daisugi hook record --format hermes"
    if not any(isinstance(h, dict) and h.get("command") == cmd for h in pre):
        pre.append({"matcher": ".*", "command": cmd, "timeout": 10})
        changed = True

    if changed:
        if existed:
            _backup(config_path)  # back up only when we actually rewrite
        config_path.write_text(yaml.dump(cfg, default_flow_style=False, sort_keys=False))
        return [config_path]
    return []


# ---------------------------------------------------------------------------
# Codex (OpenAI CLI) — detected by binary presence
# ---------------------------------------------------------------------------


@dataclass
class CodexRuntime:
    name: str = "Codex"

    def detect(self, home: Path) -> bool:
        return shutil.which("codex") is not None or (home / ".codex").is_dir()

    def unsupported_layers(self) -> dict[Layer, str]:
        return {}  # gate wired via hooks.json (v0.114+); base_url via the OpenAI-wire adapter

    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        base_url: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        codex = home / ".codex"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Symlink opendaisugi-checklist skill",
                    _agents_skill_target(home, ".codex/skills"),
                )
            )
        if Layer.MCP in sel:
            steps.append(
                InstallStep(Layer.MCP, "Register opendaisugi MCP server", codex / "config.toml")
            )
        if Layer.INSTRUCTIONS in sel:
            steps.append(
                InstallStep(Layer.INSTRUCTIONS, "Append pathway guidance", codex / "AGENTS.md")
            )
        if Layer.GATE in sel:
            mode = "ENFORCE" if enforce else "shadow"
            steps.append(
                InstallStep(
                    Layer.GATE,
                    f"Install gate PreToolUse hook ({mode}) — Codex hooks fail OPEN on "
                    "hook crash/timeout: deny works, a dead gate does not block",
                    codex / "hooks.json",
                )
            )
        if Layer.BASE_URL in sel:
            url = base_url or DEFAULT_GATEWAY_BASE_URL
            steps.append(
                InstallStep(
                    Layer.BASE_URL,
                    f"Register gateway model provider ({url}/v1, wire_api=chat) and select it",
                    codex / "config.toml",
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
        base_url: str | None = None,
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
        if Layer.GATE in sel:
            modified += _patch_codex_gate(codex / "hooks.json", enforce=enforce)
        if Layer.BASE_URL in sel:
            modified += _patch_codex_base_url(
                codex / "config.toml", base_url or DEFAULT_GATEWAY_BASE_URL
            )
        return modified

    def reverse(self, home: Path) -> list[Path]:
        codex = home / ".codex"
        modified: list[Path] = []
        modified += _remove_skill_both(home, ".codex/skills")
        toml_path = codex / "config.toml"
        text = toml_path.read_text() if toml_path.exists() else ""
        if _CODEX_MCP_BLOCK.strip() in text:
            _backup(toml_path)
            cleaned = text.replace(_CODEX_MCP_BLOCK, "").rstrip("\n")
            toml_path.write_text(cleaned + "\n" if cleaned else "")
            modified.append(toml_path)
        modified += _pop_json_hook(codex / "hooks.json", hook_substr=_GATE_HOOK_SUBSTR)
        modified += _unpatch_codex_base_url(codex / "config.toml")
        modified += _unpatch_instructions(codex / "AGENTS.md")
        return modified


_CODEX_MCP_BLOCK = '\n[mcp_servers.opendaisugi]\ncommand = "daisugi"\nargs = ["mcp", "serve"]\n'


def _patch_codex_config(config_path: Path) -> list[Path]:
    existing = config_path.read_text() if config_path.exists() else ""
    if "[mcp_servers.opendaisugi]" in existing:
        return []
    if config_path.exists():
        _backup(config_path)
    config_path.write_text(existing.rstrip("\n") + ("\n" if existing else "") + _CODEX_MCP_BLOCK)
    return [config_path]


def _patch_codex_gate(hooks_path: Path, *, enforce: bool = False) -> list[Path]:
    """Merge the fail-closed gate's PreToolUse hook into Codex ``hooks.json``.

    Codex (v0.114+) copied Claude Code's hook schema — same PreToolUse stdin
    shape (``tool_name`` + ``tool_input.command``), same exit-2 deny — so the
    canonical gate entry from ``gate_settings_json`` serves both, with one
    rewrite: Codex regex-matches matchers, and Claude's ``*`` glob is not a
    valid regex, so the matcher becomes ``.*``.

    Honesty note, mirrored in the plan step: Codex hooks fail OPEN — a hook
    that crashes, times out, or emits invalid JSON lets the tool run. The
    gate's inner verify timeout denies before the outer timeout can fail
    open, but a dead gate process does not block. Same idempotency/backup
    discipline as ``_patch_claude_gate``.
    """
    from opendaisugi.gate import gate_settings_json

    gate_entry = json.loads(gate_settings_json(mode="enforce" if enforce else "shadow"))["hooks"][
        "PreToolUse"
    ][0]
    gate_entry["matcher"] = ".*"

    existed = hooks_path.exists()
    if existed:
        try:
            hooks_cfg: dict = json.loads(hooks_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{hooks_path} is not valid JSON; skipping gate hook installation. "
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
    if any(_GATE_HOOK_SUBSTR in c for c in existing_commands):
        return []
    if existed:
        _backup(hooks_path)
    pre.append(gate_entry)
    hooks_path.write_text(json.dumps(hooks_cfg, indent=2) + "\n")
    return [hooks_path]


_CODEX_PROVIDER_SELECT = 'model_provider = "opendaisugi"'


def _codex_provider_block(url: str) -> str:
    return (
        "\n[model_providers.opendaisugi]\n"
        'name = "openDaisugi gateway"\n'
        f'base_url = "{url}/v1"\n'
        'wire_api = "chat"\n'
    )


def _patch_codex_base_url(config_path: Path, url: str) -> list[Path]:
    """Register the gateway as a Codex model provider and select it.

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
        return {Layer.GATE: "external/JS-shim gate not yet wired — follow-up"}

    def plan(
        self,
        home: Path,
        layers: "set[Layer] | None" = None,
        *,
        enforce: bool = False,
        base_url: str | None = None,
    ) -> list[InstallStep]:
        sel = _resolve_layers(layers)
        oc = home / ".openclaw"
        steps: list[InstallStep] = []
        if Layer.SKILL in sel:
            steps.append(
                InstallStep(
                    Layer.SKILL,
                    "Symlink opendaisugi-checklist skill",
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
                    "Append pathway guidance",
                    self._workspace(home) / "AGENTS.md",
                )
            )
        if Layer.BASE_URL in sel:
            url = base_url or DEFAULT_GATEWAY_BASE_URL
            steps.append(
                InstallStep(
                    Layer.BASE_URL,
                    f"Register opendaisugi gateway provider (anthropic-messages) at {url} "
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
        base_url: str | None = None,
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
                oc / "openclaw.json", base_url or DEFAULT_GATEWAY_BASE_URL
            )
        return modified

    def reverse(self, home: Path) -> list[Path]:
        oc = home / ".openclaw"
        ws = self._workspace(home)
        modified: list[Path] = []
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
                if "opendaisugi" in servers:  # only rewrite if ours is actually present
                    del servers["opendaisugi"]
                    if not servers:
                        cfg["mcp"].pop("servers", None)
                        if not cfg["mcp"]:
                            del cfg["mcp"]
                    changed = True
                providers = cfg.get("models", {}).get("providers", {})
                if "opendaisugi" in providers:
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
        modified += _remove_dir(oc / "extensions" / "opendaisugi")
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
        {"command": "daisugi", "args": ["mcp", "serve"]},
    )


def _patch_openclaw_base_url(config_path: Path, url: str) -> list[Path]:
    """Register the gateway as an OpenClaw model provider (ADR-0013 BASE_URL).

    OpenClaw can speak Anthropic Messages, so the gateway is wire-compatible
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
        what="gateway provider registration",
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
    text = md_path.read_text()
    start = text.find(_CLAUDE_MD_MARKER)
    if start == -1:
        return []
    second = text.find(_CLAUDE_MD_MARKER, start + len(_CLAUDE_MD_MARKER))
    if second == -1:
        return []  # only one marker — likely user prose, not our managed block
    end = second + len(_CLAUDE_MD_MARKER)
    cleaned = (text[:start] + text[end:]).rstrip("\n")
    _backup(md_path)
    md_path.write_text(cleaned + "\n" if cleaned else "")
    return [md_path]


def _pop_json_mcp(json_path: Path, *, mcp_key: str) -> list[Path]:
    """Remove the opendaisugi MCP server from a JSON config; no-op if absent."""
    if not json_path.exists():
        return []
    try:
        cfg = json.loads(json_path.read_text())
    except (json.JSONDecodeError, OSError):
        return []
    if "opendaisugi" not in cfg.get(mcp_key, {}):
        return []  # not managed by us — leave the file untouched
    _backup(json_path)
    cfg[mcp_key].pop("opendaisugi", None)
    if not cfg[mcp_key]:
        del cfg[mcp_key]
    json_path.write_text(json.dumps(cfg, indent=2) + "\n")
    return [json_path]


def _pop_json_hook(settings_path: Path, *, hook_substr: str) -> list[Path]:
    """Remove the opendaisugi PreToolUse hook from settings.json; no-op if absent."""
    if not settings_path.exists():
        return []
    try:
        s = json.loads(settings_path.read_text())
    except (json.JSONDecodeError, OSError):
        return []
    pre = s.get("hooks", {}).get("PreToolUse")
    if not pre or not any(
        hook_substr in h.get("command", "") for e in pre for h in e.get("hooks", [])
    ):
        return []  # nothing of ours present
    _backup(settings_path)
    for e in pre:
        e["hooks"] = [h for h in e.get("hooks", []) if hook_substr not in h.get("command", "")]
    s["hooks"]["PreToolUse"] = [e for e in pre if e.get("hooks")]
    if not s["hooks"]["PreToolUse"]:
        del s["hooks"]["PreToolUse"]
    if not s["hooks"]:
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


def uninstall(*, home: Path | None = None, runtimes: list | None = None) -> InstallResult:
    """Reverse every managed change for the given (or detected) runtimes."""
    home = home or Path.home()
    active = runtimes if runtimes is not None else detect_runtimes(home=home)
    modified: list[Path] = []
    failures: list[str] = []
    for rt in active:
        reverse = getattr(rt, "reverse", None)
        if reverse is None:
            continue
        try:
            modified.extend(reverse(home))
        except Exception as exc:  # one malformed config must not abort the rest
            failures.append(f"{rt.name}: {exc}")
    summary = f"Uninstalled from: {', '.join(r.name for r in active) or '(none)'}"
    if failures:
        summary += "\nFailures (left untouched): " + "; ".join(failures)
    return InstallResult(
        dry_run=False,
        planned=[],
        modified_files=modified,
        summary=summary,
    )


def detect_runtimes(*, home: Path | None = None) -> list:
    home = home or Path.home()
    return [r for r in _ALL_RUNTIMES if r.detect(home)]


def install(
    *,
    home: Path | None = None,
    dry_run: bool = False,
    yes: bool = False,
    runtimes: list | None = None,
    layers: "set[Layer] | None" = None,
    enforce: bool = False,
    base_url: str | None = None,
) -> InstallResult:
    """Install the selected layers into every active runtime.

    ``layers=None`` (the default) installs the current four — SKILL, MCP,
    CAPTURE, INSTRUCTIONS — byte-identical to pre-ADR-0013 behavior. GATE and
    BASE_URL are opt-in only: pass a set that includes them explicitly.
    ``enforce`` and ``base_url`` are only consumed by runtimes/layers that
    support them; everyone else ignores them.
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

    planned: list[InstallStep] = []
    for rt in active:
        planned.extend(rt.plan(home, layers, enforce=enforce, base_url=base_url))

    if dry_run:
        return InstallResult(
            dry_run=True,
            planned=planned,
            modified_files=[],
            summary=_format_summary(active, planned, modified=[]),
        )

    modified: list[Path] = []
    failures: list[str] = []
    for rt in active:
        try:
            modified.extend(rt.apply(home, layers, enforce=enforce, base_url=base_url))
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
