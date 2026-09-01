"""Install, show and remove a graft rule file (GR-3, GR-5).

A graft rule is data in ``<gate root>/grafts/<id>.json`` (RP-1). This module
writes the one shape built, ``deny_redirect`` for large reads, and removes it.

Before it writes, it reads the PreToolUse hooks of every harness settings
file it knows: Claude Code's ``~/.claude/settings.json``, the project's
``.claude/settings.json`` and ``.claude/settings.local.json``, and Codex's
``~/.codex/hooks.json``. A hook that is not the gate and not the capture hook
may rewrite a tool's input after the gate saw it, so the gate could not vouch
for the input that runs. The installer then refuses and names the hook in one
line (GR-3). A settings file that is not readable JSON also refuses, since its
hooks cannot be checked. The refusal never touches the gate itself.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from opendaisugi import delegate
from opendaisugi.config import gate_hook_kind, is_record_hook

DEFAULT_RULE_ID = "big-read"
STATES = ("audit", "active")


def settings_files(home: Path, cwd: Path) -> list[Path]:
    """The harness settings files whose PreToolUse hooks are checked, in
    order. A path named twice (the project is the home) is read once."""
    out: list[Path] = []
    for p in (
        home / ".claude" / "settings.json",
        cwd / ".claude" / "settings.json",
        cwd / ".claude" / "settings.local.json",
        home / ".codex" / "hooks.json",
    ):
        if str(p) not in {str(q) for q in out}:
            out.append(p)
    return out


def _known(h: Any) -> bool:
    """A hook known not to rewrite input: the gate, or the capture hook."""
    if not isinstance(h, dict):
        return False
    command = h.get("command")
    return gate_hook_kind(command) == "gate" or is_record_hook(command)


def rival_hooks(home: Path, cwd: Path) -> list[tuple[str, str]]:
    """Every PreToolUse hook that may rewrite input, as (file, what), in order.

    ``what`` is the hook's command as a JSON string, the hook itself as JSON
    when it has no string command, or "not readable JSON" for a file whose
    hooks cannot be read. A missing file has no hooks.
    """
    out: list[tuple[str, str]] = []
    for path in settings_files(home, cwd):
        try:
            raw = path.read_bytes()
        except FileNotFoundError:
            continue
        except OSError:
            out.append((str(path), "not readable JSON"))
            continue
        try:
            data = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, ValueError, RecursionError):
            out.append((str(path), "not readable JSON"))
            continue
        hooks = data.get("hooks") if isinstance(data, dict) else None
        entries = hooks.get("PreToolUse") if isinstance(hooks, dict) else None
        for entry in entries if isinstance(entries, list) else []:
            inner = entry.get("hooks") if isinstance(entry, dict) else None
            for h in inner if isinstance(inner, list) else []:
                if _known(h):
                    continue
                command = h.get("command") if isinstance(h, dict) else None
                what = json.dumps(command if isinstance(command, str) else h)
                out.append((str(path), what))
    return out


def refusal_line(file: str, what: str) -> str:
    if what == "not readable JSON":
        return (
            f"Refused: {file} is not readable JSON, so its PreToolUse hooks cannot be "
            "checked. No graft was installed; the gate is not changed."
        )
    return (
        f"Refused: {file} has a PreToolUse hook that may rewrite a tool's input: {what}. "
        "No graft was installed; the gate is not changed."
    )


@dataclass(frozen=True)
class Installed:
    path: Path
    rule_id: str
    version: int
    state: str
    lines: int


@dataclass(frozen=True)
class Refused:
    line: str


def rule_path(root: Path, rule_id: str) -> Path:
    return delegate.grafts_dir(root) / f"{rule_id}.json"


def valid_id(rule_id: str) -> bool:
    return bool(delegate._RULE_ID.fullmatch(rule_id))


def _prior_version(path: Path, rule_id: str) -> int:
    """The version of the rule already at ``path`` with this id, or 0."""
    try:
        obj = json.loads(path.read_bytes().decode("utf-8"))
    except (OSError, UnicodeDecodeError, ValueError, RecursionError):
        return 0
    got = delegate.parse_rule(obj, path.name)
    if isinstance(got, str) or got.id != rule_id:
        return 0
    return got.version


def install(
    root: Path,
    *,
    home: Path,
    cwd: Path,
    rule_id: str = DEFAULT_RULE_ID,
    state: str = "audit",
    lines: int = delegate.DEFAULT_MIN_LINES,
    allow_remote: bool = False,
) -> Installed | Refused:
    """Write the rule file, or refuse beside a rival hook (nothing written)."""
    rivals = rival_hooks(home, cwd)
    if rivals:
        return Refused(refusal_line(*rivals[0]))
    path = rule_path(root, rule_id)
    version = _prior_version(path, rule_id) + 1
    rule = {
        "id": rule_id,
        "version": version,
        "shape": "deny_redirect",
        "state": state,
        "match": {"tool": "Read", "file_lines_over": lines},
        "redirect": {"tool": delegate.DELEGATE_NAME},
        "worker": {"choose": "router", "allow_remote": allow_remote},
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp")
    tmp.write_text(json.dumps(rule, indent=2) + "\n", encoding="utf-8")
    os.replace(tmp, path)
    return Installed(path, rule_id, version, state, lines)


def status(root: Path, *, home: Path, cwd: Path) -> dict[str, Any]:
    """The rule files, which one is in force, and the rival hooks."""
    rules, bad = delegate.load_rules(root)
    acting = next((r for r in rules if r.state in delegate.RULE_STATES_ACTING), None)
    out_rules = []
    for r in rules:
        if r is acting:
            why = None
        elif r.state not in delegate.RULE_STATES_ACTING:
            why = f"state {r.state} does not act"
        else:
            why = f"{acting.file} acts first"
        out_rules.append(
            {
                "file": r.file,
                "id": r.id,
                "version": r.version,
                "state": r.state,
                "file_lines_over": r.min_lines,
                "allow_remote": r.allow_remote,
                "in_force": r is acting,
                "why": why,
            }
        )
    return {
        "dir": str(delegate.grafts_dir(root)),
        "rules": out_rules,
        "unused": [{"file": f, "why": w} for f, w in bad],
        "rival_hooks": [{"file": f, "hook": w} for f, w in rival_hooks(home, cwd)],
    }


def status_text(doc: dict[str, Any]) -> str:
    lines: list[str] = []
    if doc["rules"] or doc["unused"]:
        lines.append(f"Graft rules in {doc['dir']}:")
    else:
        lines.append(f"No graft rules in {doc['dir']}.")
    for r in doc["rules"]:
        tail = "in force" if r["in_force"] else f"not in force: {r['why']}"
        lines.append(
            f"  {r['file']}: {r['id']} v{r['version']}, state {r['state']}, "
            f"reads over {r['file_lines_over']} lines, {tail}"
        )
    for u in doc["unused"]:
        lines.append(f"  {u['file']}: not used: {u['why']}")
    if doc["rival_hooks"]:
        lines.append("PreToolUse hooks that may rewrite input (install refuses):")
        for h in doc["rival_hooks"]:
            lines.append(f"  {h['file']}: {h['hook']}")
    else:
        lines.append("PreToolUse hooks that may rewrite input: none")
    return "\n".join(lines) + "\n"


def remove(root: Path, rule_id: str = DEFAULT_RULE_ID) -> tuple[bool, Path]:
    """Remove the rule file; (whether one was there, its path)."""
    path = rule_path(root, rule_id)
    try:
        path.unlink()
    except FileNotFoundError:
        return False, path
    return True, path


__all__ = [
    "DEFAULT_RULE_ID",
    "Installed",
    "Refused",
    "install",
    "remove",
    "rival_hooks",
    "settings_files",
    "status",
    "status_text",
]
