"""tui_wiring._effect_hint: every non-live stage needs a "how to make it
take effect" line, or the wiring screen falls back to the generic "not
live yet" even for a stage that IS wired, just not live. The lines live
in swap.py beside the tags, so the map and the GUI cannot disagree."""

from __future__ import annotations

from opendaisugi.swap import CFG, LIVE, PLANNED, STAGE_EFFECT
from opendaisugi.tui_wiring import _effect_hint


def test_every_cfg_or_planned_stage_has_its_own_hint():
    for stage_key, effect in STAGE_EFFECT.items():
        if effect in (CFG, PLANNED):
            assert _effect_hint(stage_key) != "not live yet", f"{stage_key} has no reason"


def test_a_live_stage_has_no_hint_to_show():
    for stage_key, effect in STAGE_EFFECT.items():
        if effect == LIVE:
            assert _effect_hint(stage_key) == "not live yet", (
                f"{stage_key} is live but carries a not-live reason"
            )


def test_floor_hint_names_the_install_command():
    """A wired floor report must point at the command that applies it, not
    at the generic line."""
    assert "daisugi install --gate --report herdr" in _effect_hint("floor_report")


def test_the_gate_hint_and_the_map_tag_come_from_one_place():
    from opendaisugi.swap import tag_for

    assert _effect_hint("gate") in tag_for("gate")
