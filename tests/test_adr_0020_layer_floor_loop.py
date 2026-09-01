"""ADR-0020 exists, is Accepted, and states the layer-floor-loop split verbatim.

Spec: docs/plans/2026-09-08-workshop/spec-00-adr-0020-and-vision.md, deliverable D1.
"""

from __future__ import annotations

import re
from pathlib import Path

ADR_PATH = Path("docs/adr/0020-layer-floor-loop.md")

REQUIRED_DECISION_TEXT = (
    "openDaisugi is three parts. The **layer** (gate, verifier, journal, garden) is a "
    "library that imports nothing above it and runs alone in any harness. The **floor** "
    "(coppice) supervises panes and sources every state it shows. The **loop** is "
    "whatever harness runs in a pane; sprig is ours, the rest are rented. The invariant "
    "that ADR-0004 protected is restated: **the layer stays importable alone.** "
    "`tests/test_layer_boundary.py` enforces it."
)


def _collapsed(text: str) -> str:
    return re.sub(r"\s+", " ", text).strip()


def test_adr_0020_exists_and_is_accepted():
    text = ADR_PATH.read_text()
    assert "# ADR-0020" in text
    assert "**Status:** Accepted" in text
    assert "**Date:** 2026-09-08" in text


def test_adr_0020_decision_section_is_verbatim():
    text = ADR_PATH.read_text()
    assert REQUIRED_DECISION_TEXT in _collapsed(text)


def test_adr_0020_has_the_required_sections():
    text = ADR_PATH.read_text()
    for heading in ("## Context", "## Decision", "## Consequences", "## Alternatives considered"):
        assert heading in text
