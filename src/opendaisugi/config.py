"""User configuration for opendaisugi.

Loads from and saves to ``~/.opendaisugi/config.yaml``. The Daisugi facade
constructor kwargs override whatever is loaded from disk — config.yaml is
a default source, not an authoritative one.
"""

from __future__ import annotations

import json
import os
import re
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path

import yaml
from pydantic import BaseModel, Field


class Config(BaseModel):
    """Typed config with sensible defaults for every field."""

    model: str = "anthropic/claude-sonnet-4-20250514"
    max_task_chars: int = 4000
    z3_timeout_ms: int = 500
    data_dir: Path = Field(default_factory=lambda: Path.home() / ".opendaisugi")
    # Background distillation consent (Phase A). None = never asked (distinct
    # from an explicit no); True = distil repeated tasks in the background;
    # False = declined. Distillation only ever affects *efficiency* (the guard
    # enforces safety regardless), so it is safe to automate once consented.
    auto_tend: bool | None = None
    # ADR-0015: a qualified local model for the gateway's local rung — easy
    # turns route here ahead of any cloud downgrade (zero quota; cache
    # stickiness never blocks the local rung). None = no local rung. Set it to
    # the model id `daisugi tiers setup` qualified, and start the proxy with
    # `daisugi gateway` (the flag --local-model overrides per run).
    gateway_local_model: str | None = None
    # ADR-0010 compound-shell decomposition, persisted so it also reaches the
    # paths no CLI flag can: `hook auto-tend` runs from cron and from a detached
    # spawn. Off by default — with it on, an envelope's allowlist admits
    # `a && b` (every head checked) instead of the blanket metachar rejection.
    shell_allow_decomposition: bool = False

    # --- module-selection knobs (see opendaisugi.swap / `daisugi modules`) -----
    # These make the pipeline's stages configurable. Two of them the running
    # code actually reads (shell_allow_decomposition above; gate_mode below when
    # the hook does not force --mode); the rest are RECORDED PREFERENCES the hot
    # path does not consume yet — the GUI marks them "not yet enforced". They
    # declare intent and are the wiring seam for the alternative; the current
    # implementation (python / MiniLM / evidence-inferred) runs regardless.

    # Gate verdict mode used ONLY as the fallback when the installed hook command
    # does not pass --mode. The hook DOES pass --mode explicitly today (the one
    # thing the agent cannot rewrite), so the flag always wins and this never
    # weakens the authorization boundary; it drives the verdict only for entries
    # invoked without the flag. The import-failure `|| exit 2` backstop is
    # install-time — full enforce still needs `daisugi install --enforce`.
    gate_mode: str = "shadow"  # shadow | enforce

    # Whether an installed gate hook hands a would-deny to a present operator
    # (Task 8, `_maybe_ask`) before letting it stand. Declared here for
    # completeness, not consulted at hook-evaluation time the way `gate_mode`
    # is: `--ask` is baked into the installed command at `daisugi install
    # --gate --ask`, a static argv the agent cannot rewrite, and it must stay
    # that way — unlike `gate_mode`'s narrow, explicitly-documented fallback,
    # letting a writable config.yaml silently turn on an operator hand-off
    # for an already-installed hook would be the exact authority leak
    # `gate_mode`'s own precedence rule exists to prevent.
    gate_ask: bool = False

    # Preferred verifier client. python is the only runtime checker and ALWAYS
    # enforces; the compiled clients are a conformance harness, so this records
    # the client you would run for shadow/experimental differential checking.
    verifier_client: str = "python"  # python | rust | go | typescript | lean

    # Preferred pathway-reuse embedder. MiniLM runs today; the alternatives are
    # under evaluation and change the embedding dimension / threshold calibration.
    matcher_model: str = "all-MiniLM-L6-v2"  # | potion | int8 | lexical

    # Preferred LLM backend for envelope generation / planning. The running
    # backend is set by OPENDAISUGI_LLM_BACKEND / model; this records intent.
    llm_backend: str = "claude-code"  # claude-code | anthropic | llamafile | ollama

    # Preferred envelope source. evidence-inferred runs zero-LLM; llm-generated
    # needs a configured backend, so the choice is partly derived, not free.
    envelope_source: str = "evidence-inferred"  # | llm-generated

    # Preferred pathway-store backend. sqlite is the local default; the
    # git-backed shared registry is a separate opt-in set up with
    # `daisugi registry init` (a repo + signing keys), not a config toggle.
    pathway_store_backend: str = "sqlite"  # sqlite | git


def default_config() -> Config:
    """Return a Config populated entirely from field defaults."""
    return Config()


def load_config(path: Path | None = None) -> Config:
    """Load config from ``path`` (default: ``~/.opendaisugi/config.yaml``).

    Returns ``default_config()`` when the file does not exist. Unknown keys
    in the YAML file are silently ignored so that a config written by a
    newer version of opendaisugi still loads on an older version.
    """
    if path is None:
        path = Path.home() / ".opendaisugi" / "config.yaml"
    if not path.exists():
        return default_config()

    raw = yaml.safe_load(path.read_text()) or {}
    known = {f for f in Config.model_fields}
    filtered = {k: v for k, v in raw.items() if k in known}
    return Config(**filtered)


def save_config(config: Config, path: Path | None = None) -> None:
    """Write ``config`` to ``path`` as YAML, creating parent dirs if needed.

    ``Path`` values are serialized as strings. No atomic-write ceremony —
    config.yaml is user-editable and written rarely.
    """
    if path is None:
        path = Path.home() / ".opendaisugi" / "config.yaml"
    path.parent.mkdir(parents=True, exist_ok=True)
    data = config.model_dump(mode="json")
    # Pydantic serializes Path to str in mode="json"; yaml.safe_dump is fine with it.
    path.write_text(yaml.safe_dump(data, sort_keys=True))


def auto_tend_enabled(config: Config) -> bool:
    """True only when the user has explicitly consented to background distillation.

    Unasked (None) and declined (False) both mean "do not auto-tend" — consent
    is opt-in, never assumed.
    """
    return config.auto_tend is True


def ensure_auto_tend_consent(
    config: Config,
    ask: Callable[[], bool],
    *,
    path: Path | None = None,
) -> Config:
    """Ask once whether to distil in the background, persist the answer, return it.

    If ``config.auto_tend`` is already set (True or False), returns ``config``
    unchanged without calling ``ask`` — the question is asked exactly once, ever.
    Otherwise ``ask()`` is invoked (a ``() -> bool`` the caller wires to a prompt),
    the choice is written to ``config`` on disk, and the updated Config returned.
    """
    if config.auto_tend is not None:
        return config
    decided = config.model_copy(update={"auto_tend": bool(ask())})
    save_config(decided, path)
    return decided


_MODE_RE = re.compile(r"--mode\s+(shadow|enforce)\b")


@dataclass(frozen=True)
class ResolvedField:
    """One setting as the running code will see it, and where it came from."""

    key: str
    value: str
    source: str  # file | default | env | auto | global | project | project+global | info


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


_STRICTNESS = {"enforce": 2, "shadow": 1}


@dataclass(frozen=True)
class EffectiveHook:
    """The gate hook mode across the two files a hook can live in.

    ``daisugi install --gate`` writes a machine-global hook to
    ``~/.claude/settings.json`` (no ``--session``, so it resolves to the shared
    ``default`` envelope). ``daisugi start`` (plan 5) writes a hook scoped to
    ``<cwd>/.claude/settings.json``. Claude Code fires PreToolUse hooks from
    every settings source that has one — a call is allowed only if EVERY firing
    hook allows it — so the mode that actually governs verdicts, when both
    exist, is the STRICTER of the two (enforce beats shadow), never just
    whichever one a truth surface happened to read.
    """

    mode: str | None  # enforce | shadow | None (no hook anywhere)
    cwd_mode: str | None
    global_mode: str | None


def effective_hook_mode(*, home: Path, cwd: Path) -> EffectiveHook:
    """Read both hook files and report the mode that actually governs verdicts."""
    global_path = home / ".claude" / "settings.json"
    cwd_path = cwd / ".claude" / "settings.json"
    global_mode = installed_hook_mode(global_path)
    # Same file (cwd IS home, or an explicit alias in a test) — one hook, not two.
    cwd_mode = installed_hook_mode(cwd_path) if cwd_path != global_path else None

    candidates = [m for m in (cwd_mode, global_mode) if m]
    mode = max(candidates, key=lambda m: _STRICTNESS.get(m, 0)) if candidates else None
    return EffectiveHook(mode=mode, cwd_mode=cwd_mode, global_mode=global_mode)


def hook_source_label(eff: EffectiveHook) -> str:
    """The ONE vocabulary every truth surface (`config`, `gate status`, the TUI
    header) uses for where the effective hook mode came from: ``global`` (only
    ``~/.claude/settings.json``), ``project`` (only ``<cwd>/.claude/settings.json``
    — what `daisugi start` writes), ``project+global`` (both — the verdict is
    their intersection), or ``""`` when neither hook exists (the caller falls
    back to its own config/default source in that case).

    A single function, not three copies of the same if/elif chain, so the
    three surfaces can't drift the way `config` (``hook``) and the TUI header
    (``global``) already had before this was factored out.
    """
    if eff.cwd_mode and eff.global_mode:
        return "project+global"
    if eff.cwd_mode:
        return "project"
    if eff.global_mode:
        return "global"
    return ""


def resolved_config(
    path: Path | None = None,
    *,
    home: Path | None = None,
    cwd: Path | None = None,
    env: Mapping[str, str] | None = None,
) -> list[ResolvedField]:
    """Every Config field with its source, plus the two derived, load-bearing values.

    ``gate_mode (resolved)`` is the EFFECTIVE mode across both a machine-global
    hook (``~/.claude/settings.json``) and a directory-scoped one
    (``<cwd>/.claude/settings.json``, what ``daisugi start`` writes) — never
    just the global one, or the source would silently under-report an active
    ``daisugi start --enforce`` hook as ``default``. When only the
    directory-scoped hook exists, the source says so explicitly (``project``,
    not a bare ``global``-flavored label that reads as "the usual global one").
    When both exist, three extra rows spell out what each one is doing and how
    they combine — the honest fact that verdicts are the intersection.
    """
    from opendaisugi.llm import resolve_backend

    home = home or Path.home()
    if cwd is None:
        try:
            cwd = Path.cwd()
        except OSError:
            # The working directory was deleted out from under this process —
            # fall back to reporting the global-only view (== home, so the
            # cwd-hook check is a no-op) rather than crash `daisugi config`.
            cwd = home
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
    eff = effective_hook_mode(home=home, cwd=cwd)
    if eff.mode:
        out.append(ResolvedField("gate_mode (resolved)", eff.mode, hook_source_label(eff)))
        if eff.cwd_mode and eff.global_mode:
            out.append(ResolvedField("gate_mode (project)", eff.cwd_mode, "project"))
            out.append(ResolvedField("gate_mode (global)", eff.global_mode, "global"))
            out.append(
                ResolvedField(
                    "gate_mode (coexistence)",
                    "both fire; the verdict is the intersection — either one enforcing denies",
                    "info",
                )
            )
    else:
        out.append(
            ResolvedField(
                "gate_mode (resolved)", cfg.gate_mode, "file" if "gate_mode" in raw else "default"
            )
        )
    return out
