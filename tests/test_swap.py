"""Tests for stage knobs: real config writes, honest effect, plain descriptions.

A click must write config that maps to a real choice, and the GUI must be able
to tell the user what the choice does, whether it costs money, and when it takes
effect (now / after a restart / not yet).
"""

import pytest

from opendaisugi.config import load_config
from opendaisugi.modules import detect_stages
from opendaisugi.swap import (
    STAGE_EFFECT,
    SWAP_KNOBS,
    apply_swap,
    effect_of,
    is_live,
    is_swappable,
    selected_label,
)


def _cfg_path(tmp_path):
    return tmp_path / "config.yaml"


def test_shell_swap_writes_the_real_config_field(tmp_path):
    cfg = apply_swap("shell", "reject-compound", config_path=_cfg_path(tmp_path))
    assert cfg.shell_allow_decomposition is False
    assert load_config(_cfg_path(tmp_path)).shell_allow_decomposition is False
    cfg = apply_swap("shell", "tree-sitter-bash", config_path=_cfg_path(tmp_path))
    assert cfg.shell_allow_decomposition is True


def test_unknown_option_label_raises_not_writes(tmp_path):
    with pytest.raises(ValueError):
        apply_swap("shell", "nonexistent-module", config_path=_cfg_path(tmp_path))
    assert not _cfg_path(tmp_path).exists()


def test_unknown_stage_raises(tmp_path):
    # harness has no knob (it is an install fact, not a config choice).
    assert not is_swappable("harness")
    with pytest.raises(KeyError):
        apply_swap("harness", "claude-code", config_path=_cfg_path(tmp_path))


def test_selected_label_reflects_current_config(tmp_path):
    apply_swap("shell", "reject-compound", config_path=_cfg_path(tmp_path))
    cfg = load_config(_cfg_path(tmp_path))
    assert selected_label(cfg, "shell") == "reject-compound"


def test_effect_is_three_valued_and_covers_every_stage(tmp_path):
    stage_keys = {s.key for s in detect_stages(tmp_path)}
    assert set(STAGE_EFFECT) == stage_keys, "STAGE_EFFECT must cover every stage exactly"
    assert set(STAGE_EFFECT.values()) <= {"live", "cfg", "planned"}
    # shell/distill are live; verifier/matcher/stores are planned; gate is cfg.
    assert is_live("shell") and is_live("distill")
    assert effect_of("verifier") == "planned"
    assert effect_of("gate") == "cfg"


def test_verifier_is_a_knob_but_not_live(tmp_path):
    # You can record a verifier preference, but nothing runs it — python enforces.
    assert is_swappable("verifier")
    assert not is_live("verifier")
    cfg = apply_swap("verifier", "go (mvdan)", config_path=_cfg_path(tmp_path))
    assert cfg.verifier_client == "go"


def test_every_option_has_a_short_plain_description():
    for knob in SWAP_KNOBS.values():
        for opt in knob.options:
            assert opt.desc.strip(), f"{knob.stage_key}:{opt.label} has no description"
            assert len(opt.desc) <= 40, f"{knob.stage_key}:{opt.label} description is too long"


def test_paid_options_are_flagged_and_free_ones_are_not():
    backend = {o.label: o.cost for o in SWAP_KNOBS["backend"].options}
    assert backend["anthropic-api"] is True
    assert backend["llamafile / local"] is False
    # llm-generated spends on a model; evidence-inferred does not.
    envelope = {o.label: o.cost for o in SWAP_KNOBS["envelope"].options}
    assert envelope["llm-generated"] is True
    assert envelope["evidence-inferred"] is False


def test_every_module_knob_option_names_a_real_module_in_the_map(tmp_path):
    stages = {s.key: s for s in detect_stages(tmp_path)}
    for knob in SWAP_KNOBS.values():
        if knob.kind != "module":
            continue
        module_names = {m.name for m in stages[knob.stage_key].modules}
        for opt in knob.options:
            assert opt.label in module_names, (
                f"swap option {opt.label!r} is not a module in stage {knob.stage_key!r}"
            )


def test_resolve_command_parses_forgiving_matches():
    from opendaisugi.swap import resolve_command

    assert resolve_command(":gate enforce") == ("gate", "enforce")
    assert resolve_command("shell reject") == ("shell", "reject-compound")  # substring
    assert resolve_command("verifier go") == ("verifier", "go (mvdan)")  # value match


def test_resolve_command_rejects_bad_input():
    import pytest as _pytest

    from opendaisugi.swap import resolve_command

    for bad in ("gate", "nope enforce", "gate zzz"):
        with _pytest.raises(ValueError):
            resolve_command(bad)
