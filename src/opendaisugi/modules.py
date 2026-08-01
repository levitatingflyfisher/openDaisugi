"""The module map: what's wired, what could be swapped in, at every stage.

openDaisugi is a pipeline of swappable stages — the harness it plugs into, the
verifier that checks each action, the matcher that finds a reusable pathway, the
model backend, the token router. This renders that pipeline as an honest ASCII
wiring diagram: for each stage, which module is *active*, which are *available*
to swap in, and which are *possible* (a designed slot with no shipped
implementation yet). It is a status tool and a roadmap at once — a blank slot is
an invitation, not a bug.

Detection is best-effort and read-only. Nothing here changes wiring; it reports
it. ``daisugi modules`` prints this; ``--json`` emits the same as data.
"""

from __future__ import annotations

import importlib.util
import json
import os
from dataclasses import asdict, dataclass, field
from pathlib import Path

# State of one candidate module within a stage.
ACTIVE = "active"  # wired and in use right now
AVAILABLE = "available"  # installed / present — could be swapped in today
POSSIBLE = "possible"  # a designed slot; no shipped implementation yet


@dataclass
class Module:
    name: str
    state: str
    note: str = ""


@dataclass
class Stage:
    key: str
    title: str
    role: str  # one line: what this stage does
    modules: list[Module] = field(default_factory=list)

    @property
    def swappable(self) -> bool:
        # A stage is a real swap-point when more than one module is actually
        # runnable today (active or available). One-impl stages are honest
        # about being single-impl — the slot exists, the alternatives don't yet.
        return sum(m.state in (ACTIVE, AVAILABLE) for m in self.modules) > 1


def _have(mod: str) -> bool:
    return importlib.util.find_spec(mod) is not None


def _claude_hook_installed(kind_substr: str) -> bool:
    """Is a PreToolUse hook containing ``kind_substr`` registered for Claude Code?"""
    settings = Path.home() / ".claude" / "settings.json"
    try:
        data = json.loads(settings.read_text())
    except (OSError, ValueError):
        return False
    pre = data.get("hooks", {}).get("PreToolUse", [])
    cmds = [h.get("command", "") for e in pre for h in e.get("hooks", [])]
    return any(kind_substr in c for c in cmds)


def detect_stages(data_dir: Path) -> list[Stage]:
    """Build the wiring snapshot for ``data_dir`` (read-only, best-effort)."""
    from opendaisugi.onboarding import default_transcript_roots, gather_status

    status = gather_status(data_dir)
    roots = default_transcript_roots()
    present = {h for h, r in roots.items() if r.exists()}

    def harness(name: str, hid: str) -> Module:
        return Module(
            name,
            ACTIVE if hid in present else POSSIBLE,
            "transcripts found" if hid in present else "not detected on this box",
        )

    backend = os.environ.get("OPENDAISUGI_LLM_BACKEND", "").strip()
    gateway_on = bool(
        os.environ.get("OPENDAISUGI_GATEWAY_BASE_URL") or _claude_hook_installed("gateway")
    )

    return [
        Stage(
            "harness",
            "harness (agent host)",
            "the agent whose tool-calls we sit in front of",
            [
                harness("claude-code", "claude-code"),
                harness("codex", "codex"),
                Module("hermes", POSSIBLE, "adapter designed, not built"),
                Module("openclaw", POSSIBLE, "adapter designed, not built"),
                Module("<any via AGENTS.md>", POSSIBLE, "vendor-neutral hook contract"),
            ],
        ),
        Stage(
            "gate",
            "gate (call-time)",
            "checks each proposed action BEFORE it runs; fail-closed",
            [
                Module(
                    "claude PreToolUse",
                    ACTIVE if _claude_hook_installed("daisugi hook") else POSSIBLE,
                    "installed"
                    if _claude_hook_installed("daisugi hook")
                    else "run `daisugi install`",
                ),
                Module("codex hooks.json", AVAILABLE, "fail-open class — soft gate"),
                Module("shadow / off", AVAILABLE, "config: gate mode"),
            ],
        ),
        Stage(
            "verifier",
            "verifier (the checker)",
            "proves the action stays inside the envelope (SMT-backed)",
            [
                Module("python (in-process)", ACTIVE, "the runtime checker"),
                Module("rust", POSSIBLE, "conformance client; runtime swap = roadmap"),
                Module("go (mvdan)", POSSIBLE, "conformance client; runtime swap = roadmap"),
                Module("typescript", POSSIBLE, "conformance client; runtime swap = roadmap"),
                Module(
                    "lean (proven core)", POSSIBLE, "conformance client; runtime swap = roadmap"
                ),
            ],
        ),
        Stage(
            "shell",
            "shell decomposition",
            "splits `a && b` into heads so each is checked (ADR-0010/14)",
            [
                Module(
                    "tree-sitter-bash",
                    ACTIVE if status.shell_grammar_installed else POSSIBLE,
                    "installed"
                    if status.shell_grammar_installed
                    else "uv add 'opendaisugi[shell]'",
                ),
                Module("reject-compound", AVAILABLE, "the fail-closed default when off"),
            ],
        ),
        Stage(
            "envelope",
            "envelope source",
            "where the 'what is allowed' spec comes from",
            [
                Module("evidence-inferred", ACTIVE, "ADR-0016 — from observed steps, zero-LLM"),
                Module(
                    "llm-generated",
                    AVAILABLE if backend else POSSIBLE,
                    f"backend={backend}" if backend else "set OPENDAISUGI_LLM_BACKEND",
                ),
            ],
        ),
        Stage(
            "backend",
            "model backend",
            "the LLM for envelope generation / planning (Tier-1)",
            [
                Module(
                    "claude-code", ACTIVE if backend == "claude-code" else AVAILABLE, "no API key"
                ),
                Module("anthropic-api", ACTIVE if backend == "anthropic" else AVAILABLE, "BYOK"),
                Module("llamafile / local", AVAILABLE, "base_url, offline"),
                Module("ollama", AVAILABLE, "if running"),
            ],
        ),
        Stage(
            "matcher",
            "matcher / embedder (pathway reuse)",
            "finds a stored pathway to reuse instead of re-planning",
            [
                Module(
                    "all-MiniLM-L6-v2",
                    ACTIVE if status.search_extra_installed else POSSIBLE,
                    "torch, ~90MB"
                    if status.search_extra_installed
                    else "opendaisugi[search] missing",
                ),
                Module("potion static", POSSIBLE, "numpy-only, ~30MB, offline — under evaluation"),
                Module("int8 / fp16 onnx", POSSIBLE, "smaller MiniLM — under evaluation"),
                Module("lexical / intent", POSSIBLE, "anchor signal — under evaluation"),
            ],
        ),
        Stage(
            "router",
            "token router / gateway",
            "routes turns to cheap vs frontier models to save tokens",
            [
                Module(
                    "daisugi gateway",
                    ACTIVE if gateway_on else POSSIBLE,
                    "base_url wired" if gateway_on else "daisugi install --gateway",
                ),
                Module("NeMo Switchyard", POSSIBLE, "composes as a layer"),
                Module("off (direct)", AVAILABLE, "default; start routing with `daisugi gateway`"),
            ],
        ),
        Stage(
            "distill",
            "distillation (the gardener)",
            "clusters verified traces into reusable pathways (`tend`)",
            [
                Module(
                    "sentence-transformers",
                    ACTIVE if status.search_extra_installed else POSSIBLE,
                    "clustering embedder" if status.search_extra_installed else "needs [search]",
                ),
                Module("no-embedder", AVAILABLE, "journal only, 0 pathways (graceful)"),
            ],
        ),
        Stage(
            "stores",
            "stores (local-first)",
            "where trust and pathways live — on your disk",
            [
                Module("journal (sqlite)", ACTIVE, f"{status.journal_total} traces"),
                Module("pathway store (sqlite)", ACTIVE, f"{status.pathway_count} pathways"),
                Module("git-backed store", AVAILABLE, "shareable pathway registry"),
            ],
        ),
    ]


_GLYPH = {ACTIVE: "●", AVAILABLE: "○", POSSIBLE: "·"}
_STATE_KEY = {ACTIVE: "on", AVAILABLE: "avail", POSSIBLE: "off"}


def render_wiring(data_dir: Path, *, width: int = 66, plain: bool | None = None) -> str:
    """Render the wiring snapshot as an ASCII pipeline."""
    from opendaisugi import console

    g = console.glyphs() if plain is None else (console.ASCII_BOX if plain else console.BOX)
    stages = detect_stages(data_dir)
    out: list[str] = []
    out.append(f"openDaisugi — module wiring   (data dir: {data_dir})")
    out.append("")
    out.append("  a task from your agent")
    inner = width - 4
    for i, st in enumerate(stages):
        out.append(f"        {g['v']}")
        out.append(f"        {g['down']}")
        # Tag by how a choice here takes effect (see opendaisugi.swap):
        # [live] = the running code reads it now; [cfg] = needs a restart/
        # reinstall; [planned] = you can record it but nothing reads it yet.
        from opendaisugi.swap import effect_of

        eff = effect_of(st.key)
        tag = f" [{eff}] " if eff else ""
        head = f"{g['tl']}{g['h']} {st.title} "
        head = head + g["h"] * max(0, width - len(head) - len(tag) - 1) + tag + g["tr"]
        out.append(head)
        out.append(f"{g['v']} {st.role[:inner].ljust(inner)} {g['v']}")
        # modules: active first, wrapped
        line = "  "
        for m in st.modules:
            chunk = f"{g[_STATE_KEY[m.state]]} {m.name}   "
            if len(line) + len(chunk) > inner:
                out.append(f"{g['v']} {line[:inner].ljust(inner)} {g['v']}")
                line = "  "
            line += chunk
        if line.strip():
            out.append(f"{g['v']} {line[:inner].ljust(inner)} {g['v']}")
        out.append(g["bl"] + g["h"] * (width - 2) + g["br"])
    out.append(f"        {g['v']}")
    out.append(f"        {g['down']}")
    out.append("  verified action runs  (or falls back / is refused)")
    out.append("")
    from opendaisugi.swap import effect_of

    out.append(
        f"legend:  {g['on']} active   {g['avail']} available (swap in)   "
        f"{g['off']} possible / planned"
    )
    out.append("         [live]    = takes effect now")
    out.append("         [cfg]     = a real choice, but needs a restart/reinstall")
    out.append("         [planned] = you can record it, but nothing reads it yet")
    n_live = sum(effect_of(s.key) == "live" for s in stages)
    n_cfg = sum(effect_of(s.key) == "cfg" for s in stages)
    n_planned = sum(effect_of(s.key) == "planned" for s in stages)
    out.append(
        f"         {n_live} live · {n_cfg} need a restart/reinstall · "
        f"{n_planned} planned (not wired yet)."
    )
    return "\n".join(out)


def wiring_json(data_dir: Path) -> str:
    return json.dumps([asdict(s) for s in detect_stages(data_dir)], indent=2)
