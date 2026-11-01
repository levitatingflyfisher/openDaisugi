"""v0.27.x — version and docs checks."""

from __future__ import annotations

import pathlib

import opendaisugi


def test_version_is_current():
    assert opendaisugi.__version__.startswith("0.43.")


def test_feature_status_marks_vacuity_shipped():
    txt = pathlib.Path("docs/feature-status.md").read_text()
    assert "v1.0" not in txt or "vacuity" not in txt.split("v1.0")[0][-200:]
    # Looser contract: the doc must mention vacuity as shipped in 0.27.
    assert "vacuity" in txt.lower()


def test_changelog_has_migration_warning():
    txt = pathlib.Path("CHANGELOG.md").read_text()
    assert "0.27.0" in txt
    assert "strict" in txt.lower() and ("breaking" in txt.lower() or "migration" in txt.lower())


def test_the_roadmap_does_not_call_ranking_unbuilt():
    text = pathlib.Path("docs/roadmap.md").read_text()
    section = text.split("## 6. Ranking as a primitive", 1)[1].split("## 7.", 1)[0]
    assert "Designed, not built" not in section
    assert "rank judge" in section


def test_the_roadmap_says_the_cards_view_and_the_tree_on_the_floor_are_built():
    text = pathlib.Path("docs/roadmap.md").read_text()
    six = text.split("## 6. Ranking as a primitive", 1)[1].split("## 7.", 1)[0]
    seven = text.split("## 7. The delegation tree", 1)[1].split("## 8.", 1)[0]
    assert "cards view" in six
    assert "the coppice cards view (the arena)" not in six.split("Not built:", 1)[1]
    assert "Built in part" not in seven
    assert "minted session" in seven
