"""Stage knobs: what you can choose, what it does, and when it takes effect.

Each stage of the pipeline has an *effect* — how a choice there takes hold:

* ``live``    — takes effect now, no restart (shell, distill).
* ``cfg``     — a real choice, but it needs an install / restart / relaunch
                (gate, harness, router, backend, envelope).
* ``planned`` — you can record it, but nothing reads it yet (verifier, matcher,
                stores). The compiled clients and alt embedders are written and
                conformance-tested; the runtime dispatch is not built.

A stage with a config field also has a :class:`SwapKnob` whose options carry a
one-line plain description and a ``cost`` flag (does picking it spend money).
``STAGE_EFFECT`` covers every stage, including the two with no config field
(harness, router) — you change those by install/launch, so the map marks them
``cfg`` but the GUI shows no buttons for them.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from opendaisugi.config import Config, load_config, save_config

LIVE = "live"
CFG = "cfg"
PLANNED = "planned"

# How a choice at each stage takes effect. Single source of truth for the map
# markers and the GUI. Every stage appears here exactly once.
STAGE_EFFECT: dict[str, str] = {
    "harness": CFG,  # via `daisugi install` into your agent host
    "gate": CFG,  # via `daisugi install --enforce/--shadow`
    "verifier": PLANNED,  # python enforces; clients not dispatched at runtime
    "shell": LIVE,
    "envelope": CFG,  # needs a configured backend
    "backend": CFG,  # via OPENDAISUGI_LLM_BACKEND + restart
    "matcher": PLANNED,  # embedder is hard-wired to MiniLM today
    "router": CFG,  # via `daisugi gateway` (launch)
    "distill": LIVE,
    "stores": PLANNED,  # git store is a separate `daisugi registry init`
}

# One short line per effect, shown as the stage's tag.
EFFECT_TAG: dict[str, str] = {
    LIVE: "live — takes effect now",
    CFG: "cfg — needs a restart/reinstall",
    PLANNED: "planned — not wired yet",
}


@dataclass(frozen=True)
class SwapOption:
    """One choice. ``desc`` is a short plain line; ``cost`` flags spending money.
    For a ``module`` knob, ``label`` must match a module name in the map."""

    label: str
    value: Any
    desc: str = ""
    cost: bool = False


@dataclass(frozen=True)
class SwapKnob:
    stage_key: str
    field: str  # a real Config field name
    options: tuple[SwapOption, ...]
    kind: str = "module"  # "module" (option == a module) | "setting" (a mode)


SWAP_KNOBS: dict[str, SwapKnob] = {
    "shell": SwapKnob(
        "shell",
        "shell_allow_decomposition",
        (
            SwapOption("tree-sitter-bash", True, "parse a && b, check each part"),
            SwapOption("reject-compound", False, "refuse a && b outright (safest)"),
        ),
    ),
    "distill": SwapKnob(
        "distill",
        "auto_tend",
        (
            SwapOption("sentence-transformers", True, "cluster tasks into reusable pathways"),
            SwapOption("no-embedder", False, "log only, build no pathways"),
        ),
    ),
    "gate": SwapKnob(
        "gate",
        "gate_mode",
        (
            SwapOption("enforce", "enforce", "block unsafe actions"),
            SwapOption("shadow", "shadow", "watch only, block nothing"),
        ),
        kind="setting",
    ),
    "verifier": SwapKnob(
        "verifier",
        "verifier_client",
        (
            SwapOption("python (in-process)", "python", "built-in checker, always runs"),
            SwapOption("rust", "rust", "same checks in Rust"),
            SwapOption("go (mvdan)", "go", "same checks in Go"),
            SwapOption("typescript", "typescript", "same checks in TypeScript"),
            SwapOption("lean (proven core)", "lean", "machine-proved core"),
        ),
    ),
    "matcher": SwapKnob(
        "matcher",
        "matcher_model",
        (
            SwapOption("all-MiniLM-L6-v2", "all-MiniLM-L6-v2", "neural match, ~90MB"),
            SwapOption("potion static", "potion", "tiny lookup match, ~30MB"),
            SwapOption("int8 / fp16 onnx", "int8", "smaller neural match"),
            SwapOption("lexical / intent", "lexical", "keyword match"),
        ),
    ),
    "backend": SwapKnob(
        "backend",
        "llm_backend",
        (
            SwapOption("claude-code", "claude-code", "uses your Claude plan"),
            SwapOption("anthropic-api", "anthropic", "pay-per-token API key", cost=True),
            SwapOption("llamafile / local", "llamafile", "runs local, free"),
            SwapOption("ollama", "ollama", "runs local, free"),
        ),
    ),
    "envelope": SwapKnob(
        "envelope",
        "envelope_source",
        (
            SwapOption("evidence-inferred", "evidence-inferred", "built from what it saw, free"),
            SwapOption("llm-generated", "llm-generated", "a model writes it", cost=True),
        ),
    ),
    "stores": SwapKnob(
        "stores",
        "pathway_store_backend",
        (
            SwapOption("pathway store (sqlite)", "sqlite", "local file, default"),
            SwapOption("git-backed store", "git", "shared registry (registry init)"),
        ),
    ),
}


def is_swappable(stage_key: str) -> bool:
    """True iff the stage has a config knob (buttons in the GUI)."""
    return stage_key in SWAP_KNOBS


def effect_of(stage_key: str) -> str | None:
    """How a choice at this stage takes effect: live | cfg | planned, or None."""
    return STAGE_EFFECT.get(stage_key)


def is_live(stage_key: str) -> bool:
    """True iff the running code reads this stage's choice now (no restart)."""
    return STAGE_EFFECT.get(stage_key) == LIVE


def selected_label(config: Config, stage_key: str) -> str | None:
    """The option label whose value matches ``config`` now, or None."""
    knob = SWAP_KNOBS[stage_key]
    val = getattr(config, knob.field)
    return next((o.label for o in knob.options if o.value == val), None)


def resolve_command(text: str) -> tuple[str, str]:
    """Parse a ``:stage option`` command line into ``(stage_key, option_label)``.

    Forgiving: the option matches by its config value, its exact label, or a unique
    case-insensitive substring of the label — so ``gate enforce``, ``shell reject``,
    and ``verifier go`` all resolve. Raises ``ValueError`` with a usable message on a
    bad stage, no match, or an ambiguous one. The caller applies the result through
    :func:`apply_swap`, so the honesty (effect tag in the confirmation) is preserved.
    """
    parts = text.strip().lstrip(":").split()
    if len(parts) < 2:
        raise ValueError("usage: <stage> <option>  — e.g. gate enforce")
    stage = parts[0].lower()
    query = " ".join(parts[1:]).lower()
    if stage not in SWAP_KNOBS:
        raise ValueError(f"no swappable stage {stage!r}")
    options = SWAP_KNOBS[stage].options
    for o in options:  # exact value or exact label wins
        if str(o.value).lower() == query or o.label.lower() == query:
            return stage, o.label
    hits = [o for o in options if query in o.label.lower()]
    if len(hits) == 1:
        return stage, hits[0].label
    if not hits:
        raise ValueError(f"no option matching {query!r} in {stage}")
    raise ValueError(f"ambiguous — matches: {', '.join(o.label for o in hits)}")


def apply_swap(stage_key: str, label: str, *, config_path: Path) -> Config:
    """Set the stage's field to the option named ``label``, persist, and return it.

    Raises ``KeyError`` for a stage with no knob and ``ValueError`` for a label
    that is not one of its options — a click can only land on a real choice.
    """
    knob = SWAP_KNOBS[stage_key]  # KeyError if not a knob — intentional
    opt = next((o for o in knob.options if o.label == label), None)
    if opt is None:
        raise ValueError(f"{label!r} is not a swap option for stage {stage_key!r}")
    cfg = load_config(config_path)
    updated = cfg.model_copy(update={knob.field: opt.value})
    save_config(updated, config_path)
    return updated
