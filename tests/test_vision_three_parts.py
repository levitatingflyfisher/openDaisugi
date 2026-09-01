"""VISION.md names the three parts and points invariant 5 at ADR-0020."""

from __future__ import annotations

from pathlib import Path

VISION = Path("VISION.md")


def test_invariant_5_points_at_adr_0020():
    text = VISION.read_text()
    assert "5. **Layer, not harness.**" not in text
    assert "5. **daisugi stays importable alone.**" in text
    assert "docs/adr/0020-layer-floor-loop.md" in text


def test_three_parts_section_names_all_three():
    text = VISION.read_text()
    assert "## Three parts" in text
    section = text.split("## Three parts", 1)[1][:2000]
    assert "**daisugi**" in section
    assert "**coppice**" in section
    assert "the loop" in section
    assert "sprig" in section


def test_three_parts_sits_between_the_idea_and_the_invariants():
    text = VISION.read_text()
    assert (
        text.index("## The one idea")
        < text.index("## Three parts")
        < text.index("## The invariants")
    )


def test_vision_never_says_the_layer_for_daisugi():
    # The part is named daisugi; "the layer" survives only in the ADR file name.
    text = VISION.read_text().replace("0020-layer-floor-loop", "")
    assert "the layer" not in text.lower()


def test_scorecard_is_dated():
    assert "## Honest scorecard (" in VISION.read_text()
