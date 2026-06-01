"""Plug-and-play installation of openDaisugi into agent runtimes.

``daisugi install`` detects Claude Code, Codex, Hermes, and OpenClaw on the
local machine and installs three layers per harness:

  - Skill: symlink the bundled opendaisugi-checklist skill into the harness's
    discovery path (cross-vendor ~/.agents/skills first). Discovered on demand
    — no per-session token cost.
  - MCP: register the ``daisugi mcp serve`` tool server (per-harness syntax).
  - Capture: a pre-tool-call hook feeding distillation (per-harness surface).

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
    from opendaisugi.skill_paths import resolve_skill_dir, SkillInstaller

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


@dataclass
class InstallStep:
    layer: Layer
    description: str
    target: Path | None = None


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
    ``apply`` performs the writes and returns the modified paths. Each runtime
    composes three layers internally: skill symlink, MCP registration, and a
    pre-tool-call capture hook.
    """

    name: str

    def detect(self, home: Path) -> bool: ...
    def plan(self, home: Path) -> list[InstallStep]: ...
    def apply(self, home: Path) -> list[Path]: ...


# ---------------------------------------------------------------------------
# Claude Code
# ---------------------------------------------------------------------------

@dataclass
class ClaudeCodeRuntime:
    name: str = "Claude Code"

    def detect(self, home: Path) -> bool:
        return (home / ".claude").is_dir()

    def plan(self, home: Path) -> list[InstallStep]:
        claude_dir = home / ".claude"
        return [
            InstallStep(Layer.SKILL, "Symlink opendaisugi-checklist skill",
                        _agents_skill_target(home, ".claude/skills")),
            InstallStep(Layer.MCP, 'Register MCP server "opendaisugi"',
                        home / ".claude.json"),
            InstallStep(Layer.CAPTURE, "Add PreToolUse capture hook",
                        claude_dir / "settings.json"),
            InstallStep(Layer.INSTRUCTIONS, "Append pathway guidance",
                        claude_dir / "CLAUDE.md"),
        ]

    def apply(self, home: Path) -> list[Path]:
        claude_dir = home / ".claude"
        modified: list[Path] = [_install_skill(home, ".claude/skills")]
        modified += _patch_claude_mcp(home / ".claude.json")
        modified += _patch_claude_settings(claude_dir / "settings.json")
        modified += _patch_claude_md(claude_dir / "CLAUDE.md")
        return modified

    def reverse(self, home: Path) -> list[Path]:
        claude_dir = home / ".claude"
        modified: list[Path] = []
        modified += _remove_skill_both(home, ".claude/skills")
        modified += _pop_json_mcp(home / ".claude.json", mcp_key="mcpServers")
        modified += _pop_json_hook(
            claude_dir / "settings.json", hook_substr="daisugi hook record",
        )
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


def _patch_mcp(path: Path, fmt: _ConfigFormat, key_path: tuple[str, ...], entry: dict) -> list[Path]:
    """Register the opendaisugi MCP entry at ``key_path`` in a structured config.

    Generic over JSON / JSON5 via ``fmt``. Never clobbers an unparseable file
    (these hold real user state — project history, auth): warns and skips.
    Idempotent, and backs up only when it actually writes.

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
                f"{path} is not valid; skipping MCP registration to avoid "
                "overwriting user state. Fix the file and re-run `daisugi install`.",
                UserWarning, stacklevel=2,
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
        if (
            fmt is _JSON5
            and raw_text is not None
            and _JSON5_COMMENT_RE.search(raw_text)
        ):
            warnings.warn(
                f"{path} contains JSON5 comments which will not survive the "
                f"rewrite (the writer emits plain JSON). The pre-write backup "
                f"at {path}.bak* preserves the original text — restore from it "
                f"if you need the comments back. Tracked as M7 in REVIEW_FINDINGS.md.",
                UserWarning, stacklevel=2,
            )
        _backup(path)
    leaf["opendaisugi"] = entry
    path.write_text(fmt.dump(cfg))
    return [path]


def _patch_claude_mcp(claude_json: Path) -> list[Path]:
    """Register MCP where Claude Code reads user-scope servers: ``~/.claude.json``
    ``mcpServers`` (NOT settings.json, which only honors allow/deny flags)."""
    return _patch_mcp(
        claude_json, _JSON, ("mcpServers",),
        {"type": "stdio", "command": "daisugi", "args": ["mcp", "serve"]},
    )


def _patch_claude_settings(settings_path: Path) -> list[Path]:
    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            settings = {}
    else:
        settings = {}

    changed = False

    hooks = settings.setdefault("hooks", {})

    # PreToolUse — check by command substring to stay idempotent across the
    # `--format claude` suffix and any future flags.
    pre = hooks.setdefault("PreToolUse", [])
    existing_pre_commands = {
        h["command"]
        for entry in pre
        for h in entry.get("hooks", [])
        if h.get("type") == "command"
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
        for entry in ss for h in entry.get("hooks", [])
    ):
        for entry in ss:
            entry["hooks"] = [
                h for h in entry.get("hooks", [])
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

    def plan(self, home: Path) -> list[InstallStep]:
        h = home / ".hermes"
        return [
            InstallStep(Layer.SKILL, "Symlink opendaisugi-checklist skill",
                        h / "skills" / "opendaisugi" / _SKILL_NAME),
            InstallStep(Layer.MCP, "Register opendaisugi MCP server",
                        h / "config.yaml"),
            InstallStep(Layer.CAPTURE, "Add pre_tool_call capture hook",
                        h / "config.yaml"),
        ]

    def apply(self, home: Path) -> list[Path]:
        h = home / ".hermes"
        h.mkdir(parents=True, exist_ok=True)
        modified: list[Path] = [_link_skill(h / "skills" / "opendaisugi" / _SKILL_NAME)]
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
            kept = [hk for hk in pre if not (isinstance(hk, dict)
                    and "daisugi hook record" in hk.get("command", ""))]
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
            cfg = {}
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

    def plan(self, home: Path) -> list[InstallStep]:
        codex = home / ".codex"
        return [
            InstallStep(Layer.SKILL, "Symlink opendaisugi-checklist skill",
                        _agents_skill_target(home, ".codex/skills")),
            InstallStep(Layer.MCP, "Register opendaisugi MCP server",
                        codex / "config.toml"),
            InstallStep(Layer.INSTRUCTIONS, "Append pathway guidance",
                        codex / "AGENTS.md"),
        ]

    def apply(self, home: Path) -> list[Path]:
        codex = home / ".codex"
        codex.mkdir(parents=True, exist_ok=True)
        modified: list[Path] = [_install_skill(home, ".codex/skills")]
        modified += _patch_codex_config(codex / "config.toml")
        modified += _patch_instructions(codex / "AGENTS.md")
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
        modified += _unpatch_instructions(codex / "AGENTS.md")
        return modified


_CODEX_MCP_BLOCK = (
    "\n[mcp_servers.opendaisugi]\n"
    'command = "daisugi"\n'
    'args = ["mcp", "serve"]\n'
)


def _patch_codex_config(config_path: Path) -> list[Path]:
    existing = config_path.read_text() if config_path.exists() else ""
    if "[mcp_servers.opendaisugi]" in existing:
        return []
    if config_path.exists():
        _backup(config_path)
    config_path.write_text(
        existing.rstrip("\n") + ("\n" if existing else "") + _CODEX_MCP_BLOCK
    )
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

    def plan(self, home: Path) -> list[InstallStep]:
        oc = home / ".openclaw"
        return [
            InstallStep(Layer.SKILL, "Symlink opendaisugi-checklist skill",
                        self._workspace(home) / "skills" / _SKILL_NAME),
            InstallStep(Layer.MCP, "Register opendaisugi MCP server",
                        oc / "openclaw.json"),
            InstallStep(Layer.CAPTURE, "Install before_tool_call capture plugin",
                        oc / "extensions" / "opendaisugi"),
            InstallStep(Layer.INSTRUCTIONS, "Append pathway guidance",
                        self._workspace(home) / "AGENTS.md"),
        ]

    def apply(self, home: Path) -> list[Path]:
        oc = home / ".openclaw"
        ws = self._workspace(home)
        ws.mkdir(parents=True, exist_ok=True)
        modified: list[Path] = [_link_skill(ws / "skills" / _SKILL_NAME)]
        modified += _patch_openclaw_config(oc / "openclaw.json")
        modified += _patch_instructions(ws / "AGENTS.md")
        modified.append(_install_openclaw_plugin(home))
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
            servers = cfg.get("mcp", {}).get("servers", {}) if isinstance(cfg, dict) else {}
            if "opendaisugi" in servers:  # only rewrite if ours is actually present
                _backup(cfg_path)
                del servers["opendaisugi"]
                if not servers:
                    cfg["mcp"].pop("servers", None)
                    if not cfg["mcp"]:
                        del cfg["mcp"]
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
        config_path, _JSON5, ("mcp", "servers"),
        {"command": "daisugi", "args": ["mcp", "serve"]},
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
        hook_substr in h.get("command", "")
        for e in pre for h in e.get("hooks", [])
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


# ---------------------------------------------------------------------------
# Core
# ---------------------------------------------------------------------------

_ALL_RUNTIMES: list = [
    ClaudeCodeRuntime(), CodexRuntime(), HermesRuntime(), OpenClawRuntime(),
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
                f"--runtime {raw!r} matched {len(matches)} runtimes; "
                f"use one of: {valid}"
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
) -> InstallResult:
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
        planned.extend(rt.plan(home))

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
            modified.extend(rt.apply(home))
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
    for s in planned:
        by_layer[s.layer.value] = by_layer.get(s.layer.value, 0) + 1
    for layer, count in by_layer.items():
        lines.append(f"  [{layer}] {count} change(s)")
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
