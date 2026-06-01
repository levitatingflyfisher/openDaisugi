"""print_skill() must return full skill content from package data,
not a path relative to the source tree — so it works after uv add opendaisugi.
"""
from __future__ import annotations

from opendaisugi.install import print_skill


def test_print_skill_returns_substantial_content():
    content = print_skill()
    # The 2-line fallback stub is ~80 chars; real content is many hundreds.
    assert len(content) > 500, "should return full skill, not the 2-line fallback stub"


def test_print_skill_contains_key_workflow_terms():
    content = print_skill()
    assert "envelope" in content.lower()
    assert "verify" in content.lower()
    assert "journal" in content.lower()


def test_skill_dir_bundles_references():
    import importlib.resources as ir
    skill_dir = ir.files("opendaisugi").joinpath("skills", "opendaisugi-checklist")
    assert skill_dir.joinpath("SKILL.md").is_file()
    refs = skill_dir.joinpath("references")
    assert refs.is_dir()
    names = {p.name for p in refs.iterdir()}
    assert "postconditions.md" in names
    assert "mcp-usage.md" in names


def test_print_skill_does_not_depend_on_source_tree(tmp_path, monkeypatch):
    """Simulate a post-install environment by pointing __file__ somewhere
    where the relative ../../../skills path definitely doesn't exist."""
    import opendaisugi.install as mod
    fake_file = tmp_path / "lib" / "python3.12" / "site-packages" / "opendaisugi" / "install.py"
    monkeypatch.setattr(mod, "__file__", str(fake_file))
    content = print_skill()
    assert len(content) > 500, "should still return full skill when source tree is absent"
