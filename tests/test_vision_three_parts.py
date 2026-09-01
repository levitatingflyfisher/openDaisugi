"""VISION.md names the three parts and points invariant 5 at ADR-0020.

Spec: docs/plans/2026-09-08-workshop/spec-00-adr-0020-and-vision.md, deliverable D2.
"""

from __future__ import annotations

from pathlib import Path

VISION = Path("VISION.md")


def test_invariant_5_points_at_adr_0020():
    text = VISION.read_text()
    assert "5. **Layer, not harness.**" not in text
    assert "5. **The layer stays importable alone.**" in text
    assert "([ADR-0020](docs/adr/0020-layer-floor-loop.md))" in text


def test_three_parts_section_names_all_three():
    text = VISION.read_text()
    assert "## Three parts" in text
    section = text.split("## Three parts", 1)[1][:2000]
    assert "**The layer**" in section
    assert "**The floor**" in section
    assert "**The loop**" in section
    assert "coppice" in section.lower()


def test_three_parts_sits_between_what_this_is_and_the_invariants():
    text = VISION.read_text()
    assert (
        text.index("## What this is")
        < text.index("## Three parts")
        < text.index("## The invariants")
    )


def test_scorecard_untouched():
    # Out of scope per spec-00 — the v0.44.0 doc-refresh release owns the numbers.
    assert "As of v0.39.x:" in VISION.read_text()
