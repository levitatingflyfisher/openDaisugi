"""Every ADR gets a row in the index, and a superseded ADR says so in both places.

Spec: docs/plans/2026-09-08-workshop/spec-00-adr-0020-and-vision.md, "Testing".
"""

from __future__ import annotations

from pathlib import Path

ADR_DIR = Path("docs/adr")


def _adr_files() -> list[Path]:
    return sorted(
        p for p in ADR_DIR.glob("[0-9][0-9][0-9][0-9]-*.md") if p.name != "0000-template.md"
    )


def test_every_adr_has_a_readme_row():
    readme = (ADR_DIR / "README.md").read_text()
    missing = [p.name for p in _adr_files() if f"({p.name})" not in readme]
    assert missing == [], f"ADRs missing a docs/adr/README.md row: {missing}"


def test_adr_0004_is_marked_superseded_in_both_places():
    adr_text = (ADR_DIR / "0004-layer-not-harness.md").read_text()
    readme = (ADR_DIR / "README.md").read_text()
    assert "**Status:** Superseded in part by ADR-0020" in adr_text
    row = next(line for line in readme.splitlines() if "[0004]" in line)
    assert "Superseded in part by ADR-0020" in row
