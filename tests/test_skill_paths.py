from __future__ import annotations

from pathlib import Path

from opendaisugi.skill_paths import SkillInstaller, resolve_skill_dir


def test_resolve_skill_dir_points_at_real_skill():
    d = resolve_skill_dir()
    assert (d / "SKILL.md").is_file()
    assert (d / "references").is_dir()


def test_installer_symlinks_when_source_is_real(tmp_path):
    target = tmp_path / "skills" / "opendaisugi-checklist"
    inst = SkillInstaller(resolve_skill_dir())
    result = inst.link(target)
    assert result == target
    assert target.is_symlink()
    assert (target / "SKILL.md").is_file()  # resolves through the link


def test_installer_is_idempotent(tmp_path):
    target = tmp_path / "skills" / "opendaisugi-checklist"
    inst = SkillInstaller(resolve_skill_dir())
    inst.link(target)
    again = inst.link(target)  # must not raise on existing correct link
    assert again == target
    assert target.is_symlink()


def test_installer_copies_when_source_not_a_real_path(tmp_path):
    # Simulate zipimport: source dir does not exist on disk as a real path.
    fake_src = tmp_path / "nonexistent-zip-src"
    target = tmp_path / "skills" / "opendaisugi-checklist"
    inst = SkillInstaller(fake_src, materialize=lambda dst: _fake_materialize(dst))
    result = inst.link(target)
    assert result == target
    assert not target.is_symlink()
    assert (target / "SKILL.md").read_text() == "stub"


def _fake_materialize(dst: Path) -> None:
    dst.mkdir(parents=True, exist_ok=True)
    (dst / "SKILL.md").write_text("stub")
