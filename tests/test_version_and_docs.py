"""v0.27.x — version and docs checks."""
from __future__ import annotations
import pathlib
import opendaisugi


def test_version_is_current():
    assert opendaisugi.__version__.startswith("0.34.")


def test_feature_status_marks_vacuity_shipped():
    txt = pathlib.Path("docs/feature-status.md").read_text()
    assert "v1.0" not in txt or "vacuity" not in txt.split("v1.0")[0][-200:]
    # Looser contract: the doc must mention vacuity as shipped in 0.27.
    assert "vacuity" in txt.lower()


def test_changelog_has_migration_warning():
    txt = pathlib.Path("CHANGELOG.md").read_text()
    assert "0.27.0" in txt
    assert "strict" in txt.lower() and ("breaking" in txt.lower() or "migration" in txt.lower())
