"""`daisugi install --only` and `--list`: install (and uninstall) only the
components a user names. Every test runs under a tmp HOME."""

import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.install import (
    COMPONENTS,
    DEFAULT_LAYERS,
    Layer,
    component_layers,
    list_components,
)

runner = CliRunner()


def test_component_names_and_order():
    assert [c[0] for c in COMPONENTS] == [
        "skill",
        "mcp",
        "capture",
        "instructions",
        "warden",
        "pando",
        "router",
    ]


def test_component_layers_splits_commas_and_folds():
    assert component_layers(["warden,capture", "warden"]) == {Layer.WARDEN, Layer.CAPTURE}


def test_component_layers_unknown_names_the_list():
    with pytest.raises(
        ValueError,
        match="unknown component 'journal'. Components: skill, mcp, capture, instructions, warden, pando, router",
    ):
        component_layers(["warden,journal"])


def test_list_prints_seven_rows_and_the_default_line():
    text = list_components()
    assert text.count("\n") >= 8
    assert "Default: skill, mcp, capture, instructions, warden, pando." in text
    assert "--harness pi|opencode" in text


def test_install_list_exits_before_detection(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)  # no runtimes at all
    res = runner.invoke(app, ["install", "--list"])
    assert res.exit_code == 0 and "warden " in res.output and "No supported" not in res.output


def test_only_warden_writes_the_hook_and_nothing_else(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "warden", "--yes", "--runtime", "claude"])
    assert res.exit_code == 0, res.output
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    cmds = [h["command"] for e in settings["hooks"]["PreToolUse"] for h in e["hooks"]]
    assert any("warden" in c for c in cmds) and not any("hook record" in c for c in cmds)
    assert not (tmp_path / ".claude.json").exists()
    assert not (tmp_path / ".claude" / "CLAUDE.md").exists()
    assert "Tool calls are captured" not in res.output


def test_only_unknown_is_exit_2(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "warden,journal", "--yes"])
    assert res.exit_code == 2 and "unknown component 'journal'" in res.output
    assert not (tmp_path / ".claude" / "settings.json").exists()


def test_enforce_needs_warden_in_only(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "skill", "--enforce", "--yes"])
    assert res.exit_code == 2 and "--enforce needs the warden component" in res.output


def test_router_flag_needs_router_in_only(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "skill", "--router", "--yes"])
    assert res.exit_code == 2 and "--router is a component" in res.output


def test_uninstall_only_removes_the_named_components(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    assert runner.invoke(app, ["install", "--yes", "--runtime", "claude"]).exit_code == 0
    res = runner.invoke(
        app, ["install", "--uninstall", "--only", "capture,instructions", "--runtime", "claude"]
    )
    assert res.exit_code == 0, res.output
    assert "Uninstalled capture, instructions from: Claude Code" in res.output
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    cmds = [h["command"] for e in settings["hooks"].get("PreToolUse", []) for h in e["hooks"]]
    assert any("warden" in c for c in cmds) and not any("hook record" in c for c in cmds)
    assert "opendaisugi" in (tmp_path / ".claude.json").read_text()
    md = tmp_path / ".claude" / "CLAUDE.md"
    assert not md.exists() or "opendaisugi-managed" not in md.read_text()


def test_default_without_only_is_the_six():
    assert DEFAULT_LAYERS == {
        Layer.SKILL,
        Layer.MCP,
        Layer.CAPTURE,
        Layer.INSTRUCTIONS,
        Layer.WARDEN,
        Layer.PANDO,
    }


def test_only_with_harness_prints_the_note(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    res = runner.invoke(app, ["install", "--only", "skill", "--harness", "pi", "--dry-run"])
    assert res.exit_code == 0 and "Note: --only does not apply to --harness." in res.output


def test_only_router_points_the_harness_at_the_router(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "router", "--yes", "--runtime", "claude"])
    assert res.exit_code == 0, res.output
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    assert settings["env"]["ANTHROPIC_BASE_URL"] == "http://127.0.0.1:8787"
    assert "hooks" not in settings


def test_hermes_one_writer_writes_only_the_layers_named(tmp_path, monkeypatch):
    import yaml

    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".hermes").mkdir()
    cfg = tmp_path / ".hermes" / "config.yaml"
    assert runner.invoke(app, ["install", "--only", "mcp", "--yes"]).exit_code == 0
    assert set(yaml.safe_load(cfg.read_text())) == {"mcp_servers"}
    cfg.unlink()
    assert runner.invoke(app, ["install", "--only", "capture", "--yes"]).exit_code == 0
    assert set(yaml.safe_load(cfg.read_text())) == {"hooks"}


def test_hermes_gets_the_pathway_note_in_soul_md(tmp_path, monkeypatch):
    """SOUL.md is the one file Hermes always loads; the note goes there, and
    comes out again on uninstall. A missing SOUL.md is not created: Hermes
    would then drop its built-in identity."""
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".hermes").mkdir()
    soul = tmp_path / ".hermes" / "SOUL.md"
    with pytest.warns(UserWarning, match="SOUL.md does not exist yet"):
        res = runner.invoke(app, ["install", "--only", "instructions", "--yes"])
    assert res.exit_code == 0, res.output
    assert not soul.exists()

    soul.write_text("You are Hermes.\n")
    assert runner.invoke(app, ["install", "--only", "instructions", "--yes"]).exit_code == 0
    text = soul.read_text()
    assert text.startswith("You are Hermes.\n")
    assert "find_pathway" in text and "verify_plan" in text and "one action" in text
    assert "journaled automatically" not in text
    assert runner.invoke(app, ["install", "--only", "instructions", "--yes"]).exit_code == 0
    assert soul.read_text() == text  # idempotent

    res = runner.invoke(
        app, ["install", "--uninstall", "--only", "instructions", "--runtime", "hermes"]
    )
    assert res.exit_code == 0, res.output
    assert soul.read_text() == "You are Hermes.\n"


def test_an_older_pathway_block_is_replaced_in_place(tmp_path):
    from opendaisugi import install

    m = install._CLAUDE_MD_MARKER
    md = tmp_path / "CLAUDE.md"
    head = "# mine\n\nkeep  this \t exactly\n\n"
    tail = "\n\nafter the block, no newline"
    md.write_text(f"{head}{m}\n## old text, journaled automatically\n{m}{tail}")
    assert install._pathway_guidance_title(md) == "Update pathway guidance"
    assert install._patch_instructions(md) == [md]
    assert md.read_text() == head + install._CLAUDE_MD_BLOCK.rstrip("\n") + tail
    # The same text a second time changes nothing, and the plan says so.
    assert install._pathway_guidance_title(md) == "Keep pathway guidance (already current)"
    before = md.read_text()
    assert install._patch_instructions(md) == []
    assert md.read_text() == before
    # One marker alone on its line is not a block: left alone.
    stray = tmp_path / "S.md"
    stray.write_text(f"prose\n{m}\nprose\n")
    assert install._patch_instructions(stray) == []


def test_crlf_files_keep_their_line_ends(tmp_path):
    from opendaisugi import install

    m = install._CLAUDE_MD_MARKER
    md = tmp_path / "CLAUDE.md"
    md.write_bytes(f"# mine\r\n\r\n{m}\r\nold\r\n{m}\r\n\r\nafter\r\n".encode())
    assert install._pathway_guidance_title(md) == "Update pathway guidance"
    install._patch_instructions(md)
    want = install._CLAUDE_MD_BLOCK.rstrip("\n").replace("\n", "\r\n")
    assert md.read_bytes() == f"# mine\r\n\r\n{want}\r\n\r\nafter\r\n".encode()
    assert install._pathway_guidance_title(md) == "Keep pathway guidance (already current)"
    assert install._patch_instructions(md) == []
    # An append to a CRLF file keeps the lines already there.
    other = tmp_path / "A.md"
    other.write_bytes(b"one\r\ntwo\r\n")
    install._patch_instructions(other)
    assert other.read_bytes().startswith(b"one\r\ntwo\r\n\n")


def test_a_marker_inside_prose_is_not_a_marker(tmp_path):
    from opendaisugi import install

    m = install._CLAUDE_MD_MARKER
    md = tmp_path / "CLAUDE.md"
    head = f"I wrote {m} in a sentence.\nkeep this line\n\n"
    md.write_text(f"{head}{m}\nold\n{m}\n")
    install._patch_instructions(md)
    assert md.read_text() == head + install._CLAUDE_MD_BLOCK.rstrip("\n") + "\n"
    # Uninstall takes the block and nothing between the prose marker and it.
    install._unpatch_instructions(md)
    assert md.read_text() == head.rstrip("\n") + "\n"
    # A marker only in prose: the block is appended and is then removable.
    only = tmp_path / "B.md"
    only.write_text(f"say {m} here\n")
    install._patch_instructions(only)
    assert only.read_text().startswith(f"say {m} here\n\n{m}\n")


def test_the_plan_title_survives_a_file_it_cannot_read(tmp_path):
    from opendaisugi import install

    d = tmp_path / "dir.md"
    d.mkdir()
    assert install._pathway_guidance_title(d) == "Append pathway guidance"
    bad = tmp_path / "bad.md"
    bad.write_bytes(b"\xff\xfe\x00")
    assert install._pathway_guidance_title(bad) == "Append pathway guidance"
