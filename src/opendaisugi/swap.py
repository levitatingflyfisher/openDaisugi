"""Stage knobs: what you can choose, what it does, and when it takes effect.

Each stage of the pipeline has an *effect*, how a choice there takes hold:

* ``live``: takes effect now, no restart. Shell, distill, backend, verifier,
  matcher, router. The verifier reads ``verifier_client`` on every verify
  call and dispatches to that compiled client beside the Python oracle. The
  matcher re-resolves ``matcher_model`` on every lookup, and ``tend``
  re-embeds the rows an earlier embedder stamped. The gateway re-reads its
  config on mtime change, SIGHUP, and a loopback ``POST /_reload``. See
  ADR-0018, 0019, and 0021.
* ``cfg``: a real choice, but it needs an install, restart, or relaunch.
  Gate, harness, envelope. ``CFG_REASON`` says why, per stage.
* ``planned``: you can record it, but nothing reads it yet. Stores.

A stage with a config field also has a :class:`SwapKnob` whose options carry a
one-line plain description and a ``cost`` flag (does picking it spend money).
``STAGE_EFFECT`` covers every stage, including the two with no config field
(harness, router), you change those by install/launch, so the map marks them
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
    "verifier": LIVE,  # dispatched per verify call by verifier_dispatch
    "shell": LIVE,
    "envelope": CFG,  # needs a configured backend
    "backend": LIVE,  # resolve_backend reads the config file per call
    "matcher": LIVE,  # _search.reload() and tend re-embed; find re-resolves per call
    "router": LIVE,  # the gateway re-reads config on mtime, SIGHUP, and POST /_reload
    "distill": LIVE,
    "stores": PLANNED,  # git store is a separate `daisugi registry init`
    # cfg, not live: the Stop/Notification hooks live in settings.json,
    # which the harness owns and reloads, not us, same reason gate mode
    # stays cfg (§5.9), spec-01.
    "floor_report": CFG,
    # live: the next pane pick reads config.floor.backend, no restart.
    "floor_backend": LIVE,
    # cfg: voice_engine picks the speech-to-text engine; a running
    # `daisugi voice serve` must be restarted to pick a different one.
    "voice_engine": CFG,
}

# One short line per effect, shown as the stage's tag.
EFFECT_TAG: dict[str, str] = {
    LIVE: "live: takes effect now",
    CFG: "cfg: needs a restart/reinstall",
    PLANNED: "planned: not wired yet",
}

# Why a cfg stage cannot be live, per stage. The GUI shows this so the operator
# reads a reason rather than a shrug. Every cfg stage must have one; the
# honesty test in tests/test_hot_swap.py enforces that.
CFG_REASON: dict[str, str] = {
    "gate": "the hook file belongs to the harness, not to us; "
    "run `daisugi install --enforce` to apply",
    "harness": "run `daisugi install` inside your agent host",
    "envelope": "pick a backend first, then this choice has something to run",
    "floor_report": "the Stop and Notification hooks live in the harness's "
    "settings.json; run `daisugi install --gate --report herdr` to apply",
    "voice_engine": "a running `daisugi voice serve` loads one engine; "
    "set voice_engine in config.yaml, then start it again",
}

# What a planned stage is waiting on, per stage. Same purpose as CFG_REASON:
# the wiring screen shows a reason, not a shrug.
PLANNED_REASON: dict[str, str] = {
    "stores": "git store: run `daisugi registry init`; nothing reads the choice yet",
}


def reason_for(stage_key: str) -> str:
    """Why a stage is not live, or an empty string for a live stage."""
    effect = STAGE_EFFECT.get(stage_key)
    if effect == CFG:
        return CFG_REASON.get(stage_key, "")
    if effect == PLANNED:
        return PLANNED_REASON.get(stage_key, "")
    return ""


def tag_for(stage_key: str) -> str:
    """The stage's tag line, with the reason appended for a stage that is not live."""
    effect = STAGE_EFFECT.get(stage_key)
    if effect is None:
        return ""
    tag = EFFECT_TAG[effect]
    reason = reason_for(stage_key)
    return f"{tag}: {reason}" if reason else tag


# matcher_model values that are placeholders, not shipped embedders. Selecting
# one is refused at the swap, never silently recorded. Empty today: lexical
# shipped in ADR-0019 and int8 in ADR-0021. The refusal path stays for the
# next placeholder.
_UNBUILT_MATCHERS: frozenset[str] = frozenset()


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
            SwapOption("int8 / fp16 onnx", "int8", "quantized MiniLM, ~23MB, no torch"),
            SwapOption("lexical / intent", "lexical", "keyword match, no model, offline"),
        ),
    ),
    "backend": SwapKnob(
        "backend",
        "llm_backend",
        (
            SwapOption("auto", None, "pick what runs on this box"),
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
    "floor_backend": SwapKnob(
        "floor_backend",
        "floor.backend",
        (
            SwapOption("auto", "auto", "first backend that answers"),
            SwapOption("coppice", "coppice", "our own server, frames and state"),
            SwapOption("herdr", "herdr", "drive herdr panes"),
            SwapOption("tmux", "tmux", "the multiplexer you already run"),
        ),
        kind="setting",
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


def get_field(config: Any, dotted: str) -> Any:
    """Read a field by a dotted path: ``gate_mode`` or ``floor.backend``.

    ``config`` is a ``Config`` at the top level and a nested ``BaseModel``
    below it, so the annotation stays loose on purpose.
    """
    value: Any = config
    for part in dotted.split("."):
        value = getattr(value, part)
    return value


def set_field(config: Any, dotted: str, value: Any) -> Any:
    """Return a copy of ``config`` with the dotted field set.

    A nested group is rebuilt one level at a time, not shadowed.
    ``Config.model_copy(update={"floor.backend": x})`` writes a key nothing
    reads and reports success. A live knob that silently does nothing is
    the exact failure the honesty tags exist to prevent.
    """
    head, _, rest = dotted.partition(".")
    if not rest:
        return config.model_copy(update={head: value})
    child = getattr(config, head)
    return config.model_copy(update={head: set_field(child, rest, value)})


def selected_label(config: Config, stage_key: str) -> str | None:
    """The option label whose value matches ``config`` now, or None."""
    knob = SWAP_KNOBS[stage_key]
    val = get_field(config, knob.field)
    return next((o.label for o in knob.options if o.value == val), None)


def resolve_command(text: str) -> tuple[str, str]:
    """Parse a ``:stage option`` command line into ``(stage_key, option_label)``.

    Forgiving: the option matches by its config value, its exact label, or a unique
    case-insensitive substring of the label, so ``gate enforce``, ``shell reject``,
    and ``verifier go`` all resolve. Raises ``ValueError`` with a usable message on a
    bad stage, no match, or an ambiguous one. The caller applies the result through
    :func:`apply_swap`, so the honesty (effect tag in the confirmation) is preserved.
    """
    parts = text.strip().lstrip(":").split()
    if len(parts) < 2:
        raise ValueError("usage: <stage> <option>, for example gate enforce")
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
    raise ValueError(f"ambiguous, matches: {', '.join(o.label for o in hits)}")


def apply_swap(stage_key: str, label: str, *, config_path: Path) -> Config:
    """Set the stage's field to the option named ``label``, persist, and return it.

    Raises ``KeyError`` for a stage with no knob and ``ValueError`` for a label
    that is not one of its options, a click can only land on a real choice.
    """
    knob = SWAP_KNOBS[stage_key]  # KeyError if not a knob, intentional
    opt = next((o for o in knob.options if o.label == label), None)
    if opt is None:
        raise ValueError(f"{label!r} is not a swap option for stage {stage_key!r}")
    if stage_key == "matcher" and opt.value in _UNBUILT_MATCHERS:
        # Refuse at the operator's action, not on next use: recording an unbuilt
        # embedder would write a config that then errors everywhere, a trap.
        from opendaisugi.exceptions import MatcherNotAvailable

        raise MatcherNotAvailable(
            f"the {label!r} matcher is not built yet. Choose 'all-MiniLM-L6-v2' "
            f"(torch), 'potion' (torch-free), or 'lexical' (no model)."
        )
    cfg = load_config(config_path)
    updated = set_field(cfg, knob.field, opt.value)
    save_config(updated, config_path)
    return updated
