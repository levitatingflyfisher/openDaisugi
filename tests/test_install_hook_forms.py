"""Install rebinds a record hook only when it is one plain command.

A rebind that splits and re-quotes every word turns `daisugi hook record ||
true` into `... '||' true`, which exits 2 and denies every tool call. These
tests pin the rule: swap the program word and keep every other byte, or leave
the hook and say so.
"""

from __future__ import annotations

import json
import os
import shlex

import pytest
import yaml
from typer.testing import CliRunner

from opendaisugi import install as inst
from opendaisugi.cli import app
from opendaisugi.config import is_record_hook
from opendaisugi.install import ClaudeCodeRuntime, CodexRuntime, HermesRuntime, uninstall

_HERMES = {inst.Layer.MCP, inst.Layer.CAPTURE}


def _write_claude(home, command):
    claude = home / ".claude"
    claude.mkdir(exist_ok=True)
    (claude / "settings.json").write_text(
        json.dumps(
            {"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": command}]}]}}
        )
    )


def _pre_commands(home):
    cfg = json.loads((home / ".claude" / "settings.json").read_text())
    return [h["command"] for e in cfg["hooks"]["PreToolUse"] for h in e["hooks"]]


# -- A1: only a plain command is rewritten --------------------------------


@pytest.mark.parametrize(
    "old, tail",
    [
        ("daisugi hook record --format claude", " hook record --format claude"),
        ("/old/bin/daisugi-rs hook record --format claude", " hook record --format claude"),
        ("'/old path/daisugi' hook record --format claude", " hook record --format claude"),
        ("daisugi  hook record  --format   claude  ", "  hook record  --format   claude  "),
    ],
)
def test_a_plain_command_swaps_only_its_program_word(old, tail, tmp_path, fake_daisugi_exe):
    new = inst._rebind_record_command(old)
    assert new == (shlex.quote(fake_daisugi_exe) + tail, False)


@pytest.mark.parametrize(
    "old",
    [
        "daisugi hook record --format claude || true",
        "daisugi hook record --format claude 2>/dev/null",
        "daisugi hook record --format claude >/dev/null",
        "daisugi hook record --format claude; exit 0",
        "daisugi hook record --format claude && true",
        "daisugi hook record --format claude | cat",
        "daisugi hook record --format claude &",
        "daisugi hook record --format claude $(true)",
        "daisugi hook record --format 'claude'",
        "env X=1 daisugi hook record --format claude",
        "X=1 daisugi hook record --format claude",
        "daisugi hook record --format claude\ntrue",
    ],
)
def test_anything_more_is_left_byte_for_byte_and_flagged(old, fake_daisugi_exe):
    assert inst._rebind_record_command(old) == (old, True)


def test_a_command_that_already_runs_the_entry_point_needs_no_note(fake_daisugi_exe):
    for cmd in (
        f"{shlex.quote(fake_daisugi_exe)} hook record --format claude",
        f"{shlex.quote(fake_daisugi_exe)} hook record --format claude || true",
        f"env X=1 {shlex.quote(fake_daisugi_exe)} hook record --format claude",
    ):
        assert inst._rebind_record_command(cmd) == (cmd, False)


def test_a_command_that_is_not_ours_is_not_touched(fake_daisugi_exe):
    for cmd in ("echo hi", "daisugi-local hook record --format claude", "true || daisugi x"):
        assert inst._rebind_record_command(cmd) == (cmd, False)


def test_install_leaves_a_compound_hook_and_says_which_runtime(tmp_path, fake_daisugi_exe):
    old = "daisugi hook record --format claude || true"
    _write_claude(tmp_path, old)
    inst._WARDEN_NOTES.clear()
    ClaudeCodeRuntime().apply(tmp_path, layers={inst.Layer.CAPTURE})
    assert _pre_commands(tmp_path) == [old]
    (note,) = [n for n in inst._WARDEN_NOTES if inst.LEFT_AS_IS in n]
    assert note.startswith("[Claude Code] ") and old in note


def test_the_cli_does_not_call_a_left_hook_configured(tmp_path, monkeypatch, fake_daisugi_exe):
    monkeypatch.setenv("HOME", str(tmp_path))
    _write_claude(tmp_path, "env X=1 daisugi hook record --format claude")
    args = ["install", "--yes", "--runtime", "claude", "--only", "capture"]
    r = CliRunner().invoke(app, args)
    assert r.exit_code == 0, r.output
    assert "already configured" not in r.output
    assert "was left as it is" in r.output
    assert _pre_commands(tmp_path) == ["env X=1 daisugi hook record --format claude"]


def test_the_exit_code_of_a_rebound_hook_is_still_zero(tmp_path, fake_daisugi_exe):
    _write_claude(tmp_path, "daisugi hook record --format claude")
    ClaudeCodeRuntime().apply(tmp_path, layers={inst.Layer.CAPTURE})
    (cmd,) = _pre_commands(tmp_path)
    assert shlex.split(cmd) == [fake_daisugi_exe, "hook", "record", "--format", "claude"]


# -- A1b: a bare program word never replaces a path --------------------------


@pytest.fixture
def bare_exe(monkeypatch):
    """Install can find no path for daisugi: the only word it can write is
    the bare name."""
    monkeypatch.setattr(inst, "daisugi_exe", lambda: "daisugi")


@pytest.mark.parametrize(
    "old",
    [
        "/old/bin/daisugi hook record --format claude",
        "'/old path/daisugi-go' hook record --format claude",
    ],
)
def test_a_path_is_never_rewritten_to_a_bare_word(old, bare_exe):
    assert inst._rebind_record_command(old) == (old, True)


def test_a_bare_word_still_rewrites_a_bare_word(bare_exe):
    old = "daisugi-go hook record --format claude"
    assert inst._rebind_record_command(old) == ("daisugi hook record --format claude", False)
    same = "daisugi hook record --format claude"
    assert inst._rebind_record_command(same) == (same, False)


def test_install_keeps_the_path_hook_and_says_why(tmp_path, bare_exe):
    old = "/old/bin/daisugi hook record --format claude"
    _write_claude(tmp_path, old)
    inst._WARDEN_NOTES.clear()
    ClaudeCodeRuntime().apply(tmp_path, layers={inst.Layer.CAPTURE})
    assert _pre_commands(tmp_path) == [old]
    (note,) = [n for n in inst._WARDEN_NOTES if inst.LEFT_AS_IS in n]
    assert note.startswith("[Claude Code] ") and old in note
    assert "not one plain command" not in note and "PATH" in note


def test_the_path_note_needs_a_program_word_that_names_a_path(bare_exe):
    """A plain record hook with a bare word that is not self is left for another
    reason, so the note must not say it already names a path."""
    note = inst._left_hook_note("Claude Code", "daisugi-go hook record --format claude")
    assert "already names a path" not in note and "not one plain command" in note
    path_note = inst._left_hook_note("Claude Code", "/old/bin/daisugi hook record --format claude")
    assert "already names a path" in path_note


def test_install_keeps_a_path_in_the_claude_mcp_entry_and_says_why(tmp_path, bare_exe):
    cfg = tmp_path / ".claude.json"
    old = {"mcpServers": {"opendaisugi": {"command": "/old/bin/daisugi", "args": ["mcp", "serve"]}}}
    cfg.write_text(json.dumps(old))
    inst._WARDEN_NOTES.clear()
    assert inst._patch_claude_mcp(cfg) == []
    assert json.loads(cfg.read_text()) == old
    (note,) = [n for n in inst._WARDEN_NOTES if inst.LEFT_AS_IS in n]
    assert "/old/bin/daisugi" in note and "PATH" in note and "MCP" in note


def test_a_bare_mcp_entry_still_takes_the_bare_word(tmp_path, bare_exe):
    cfg = tmp_path / ".claude.json"
    cfg.write_text(
        json.dumps(
            {"mcpServers": {"opendaisugi": {"command": "daisugi-go", "args": ["mcp", "serve"]}}}
        )
    )
    inst._WARDEN_NOTES.clear()
    inst._patch_claude_mcp(cfg)
    assert json.loads(cfg.read_text())["mcpServers"]["opendaisugi"]["command"] == "daisugi"
    assert inst._WARDEN_NOTES == []


def test_install_keeps_a_path_in_the_codex_mcp_block(tmp_path, bare_exe):
    cfg = tmp_path / "config.toml"
    text = '\n[mcp_servers.opendaisugi]\ncommand = "/old/bin/daisugi"\nargs = ["mcp", "serve"]\n'
    cfg.write_text(text)
    inst._WARDEN_NOTES.clear()
    assert inst._patch_codex_config(cfg) == []
    assert cfg.read_text() == text
    assert len([n for n in inst._WARDEN_NOTES if inst.LEFT_AS_IS in n]) == 1


def test_install_keeps_a_path_in_the_hermes_mcp_entry(tmp_path, bare_exe):
    cfg = tmp_path / "config.yaml"
    cfg.write_text(
        yaml.safe_dump({"mcp_servers": {"opendaisugi": {"command": "/old/bin/daisugi"}}})
    )
    inst._WARDEN_NOTES.clear()
    inst._patch_hermes_config(cfg, mcp=True, capture=False)
    assert (
        yaml.safe_load(cfg.read_text())["mcp_servers"]["opendaisugi"]["command"]
        == "/old/bin/daisugi"
    )
    assert len([n for n in inst._WARDEN_NOTES if inst.LEFT_AS_IS in n]) == 1


# -- A2: only our own names are ours --------------------------------------


@pytest.mark.parametrize(
    "word, ours",
    [
        ("daisugi", True),
        ("daisugi-py", True),
        ("daisugi-go", True),
        ("daisugi-rs", True),
        ("/x/y/daisugi-rs", True),
        ("daisugi-local", False),
        ("/home/u/bin/daisugi-local", False),
        ("daisugi-", False),
        ("daisugix", False),
        ("mydaisugi", False),
        (None, False),
    ],
)
def test_entry_words_are_an_exact_set(word, ours):
    assert inst._is_entry_word(word) is ours


def test_a_record_hook_by_another_name_is_not_a_record_hook():
    assert is_record_hook("daisugi-rs hook record --format claude")
    assert not is_record_hook("daisugi-local hook record --format claude")


def test_a_user_wrapper_is_not_replaced_or_removed(tmp_path, fake_daisugi_exe):
    wrapper = "/home/u/bin/daisugi-local"
    old = f"{wrapper} hook record --format claude"
    _write_claude(tmp_path, old)
    (tmp_path / ".claude.json").write_text(
        json.dumps({"mcpServers": {"opendaisugi": {"command": wrapper, "args": ["mcp", "serve"]}}})
    )
    ClaudeCodeRuntime().apply(tmp_path, layers={inst.Layer.MCP})
    mcp = json.loads((tmp_path / ".claude.json").read_text())["mcpServers"]["opendaisugi"]
    assert mcp["command"] == wrapper
    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    assert old in (tmp_path / ".claude" / "settings.json").read_text()


def test_codex_wrapper_block_is_left(tmp_path, fake_daisugi_exe):
    codex = tmp_path / ".codex"
    codex.mkdir()
    block = (
        'model = "x"\n\n[mcp_servers.opendaisugi]\ncommand = "/h/daisugi-local"\n'
        'args = ["mcp", "serve"]\n'
    )
    (codex / "config.toml").write_text(block)
    CodexRuntime().apply(tmp_path, layers={inst.Layer.MCP})
    assert (codex / "config.toml").read_text() == block


# -- A3: nothing rebound, nothing rewritten -------------------------------


def test_hermes_rewrites_nothing_when_nothing_was_rebound(tmp_path, fake_daisugi_exe):
    h = tmp_path / ".hermes"
    h.mkdir()
    cmd = f"{shlex.quote(fake_daisugi_exe)} hook record --format hermes || true"
    cfg = {
        "mcp_servers": {"opendaisugi": {"command": fake_daisugi_exe, "args": ["mcp", "serve"]}},
        "hooks": {"pre_tool_call": [{"matcher": ".*", "command": cmd, "timeout": 10}]},
    }
    path = h / "config.yaml"
    path.write_text(yaml.dump(cfg, default_flow_style=False, sort_keys=False))
    before = path.read_text()
    assert HermesRuntime().apply(tmp_path, layers=_HERMES) == []
    assert path.read_text() == before
    assert not list(h.glob("config.yaml.bak*"))


def test_hermes_compound_hook_is_left_and_noted_without_a_backup(tmp_path, fake_daisugi_exe):
    h = tmp_path / ".hermes"
    h.mkdir()
    old = "daisugi hook record --format hermes || true"
    cfg = {
        "mcp_servers": {"opendaisugi": {"command": fake_daisugi_exe, "args": ["mcp", "serve"]}},
        "hooks": {"pre_tool_call": [{"matcher": ".*", "command": old, "timeout": 10}]},
    }
    path = h / "config.yaml"
    path.write_text(yaml.dump(cfg, default_flow_style=False, sort_keys=False))
    inst._WARDEN_NOTES.clear()
    assert HermesRuntime().apply(tmp_path, layers=_HERMES) == []
    assert yaml.safe_load(path.read_text())["hooks"]["pre_tool_call"][0]["command"] == old
    assert not list(h.glob("config.yaml.bak*"))
    assert any(n.startswith("[Hermes] ") and inst.LEFT_AS_IS in n for n in inst._WARDEN_NOTES)


def test_hermes_old_bare_hook_is_rebound_once(tmp_path, fake_daisugi_exe):
    h = tmp_path / ".hermes"
    h.mkdir()
    cfg = {
        "hooks": {
            "pre_tool_call": [
                {"matcher": ".*", "command": "daisugi hook record --format hermes", "timeout": 10}
            ]
        }
    }
    (h / "config.yaml").write_text(yaml.dump(cfg))
    assert HermesRuntime().apply(tmp_path, layers=_HERMES)
    cmd = yaml.safe_load((h / "config.yaml").read_text())["hooks"]["pre_tool_call"][0]["command"]
    assert shlex.split(cmd)[0] == fake_daisugi_exe


# -- A4: the check reads back what was written ----------------------------


def _executable(path):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("#!/bin/sh\nexit 0\n")
    path.chmod(0o755)
    return str(path)


def test_the_check_reads_the_hook_that_is_on_disk(tmp_path, fake_daisugi_exe):
    gone = tmp_path / "gone" / "daisugi"
    _write_claude(tmp_path, f"{gone} hook record --format claude")
    (line,) = inst.unrunnable_commands([ClaudeCodeRuntime()], home=tmp_path)
    assert line.startswith("[Claude Code] The command ") and str(gone) in line
    assert "is not there" in line


def test_the_check_reads_a_compound_hook_by_every_segment(tmp_path, fake_daisugi_exe):
    gone = tmp_path / "gone" / "daisugi"
    _write_claude(
        tmp_path, f"{shlex.quote(fake_daisugi_exe)} hook record --format claude || {gone}"
    )
    # only the record segment's program matters: the user's own tail is theirs
    assert inst.unrunnable_commands([ClaudeCodeRuntime()], home=tmp_path) == []
    _write_claude(tmp_path, f"env X=1 {gone} hook record --format claude")
    (line,) = inst.unrunnable_commands([ClaudeCodeRuntime()], home=tmp_path)
    assert str(gone) in line


def test_the_check_reads_the_mcp_entry_on_disk(tmp_path, fake_daisugi_exe):
    gone = str(tmp_path / "gone" / "daisugi")
    (tmp_path / ".claude.json").write_text(
        json.dumps({"mcpServers": {"opendaisugi": {"command": gone, "args": ["mcp", "serve"]}}})
    )
    (line,) = inst.unrunnable_commands([ClaudeCodeRuntime()], home=tmp_path)
    assert gone in line


def test_the_check_reads_hermes_and_codex_and_openclaw(tmp_path, fake_daisugi_exe):
    gone = str(tmp_path / "gone" / "daisugi")
    h = tmp_path / ".hermes"
    h.mkdir()
    (h / "config.yaml").write_text(
        yaml.dump({"mcp_servers": {"opendaisugi": {"command": gone, "args": ["mcp", "serve"]}}})
    )
    c = tmp_path / ".codex"
    c.mkdir()
    (c / "config.toml").write_text(
        f'[mcp_servers.opendaisugi]\ncommand = "{gone}"\nargs = ["mcp", "serve"]\n'
    )
    oc = tmp_path / ".openclaw"
    oc.mkdir()
    (oc / "openclaw.json").write_text(
        json.dumps(
            {"mcp": {"servers": {"opendaisugi": {"command": gone, "args": ["mcp", "serve"]}}}}
        )
    )
    lines = inst.unrunnable_commands(
        [HermesRuntime(), CodexRuntime(), inst.OpenClawRuntime()], home=tmp_path
    )
    assert [ln.split("]")[0] for ln in lines] == ["[Hermes", "[Codex", "[OpenClaw"]


def test_the_check_names_a_file_that_is_not_executable(tmp_path, fake_daisugi_exe):
    f = tmp_path / "bin" / "daisugi"
    f.parent.mkdir()
    f.write_text("x")
    f.chmod(0o644)
    _write_claude(tmp_path, f"{f} hook record --format claude")
    (line,) = inst.unrunnable_commands([ClaudeCodeRuntime()], home=tmp_path)
    assert "is not executable" in line


def test_the_check_is_quiet_after_a_good_install(tmp_path, fake_daisugi_exe):
    for d in (".claude", ".hermes", ".codex"):
        (tmp_path / d).mkdir()
    for rt in (ClaudeCodeRuntime(), HermesRuntime(), CodexRuntime()):
        rt.apply(tmp_path)
    assert (
        inst.unrunnable_commands(
            [ClaudeCodeRuntime(), HermesRuntime(), CodexRuntime()], home=tmp_path
        )
        == []
    )


def test_cli_install_exits_1_when_the_written_hook_cannot_run(
    tmp_path, monkeypatch, fake_daisugi_exe
):
    monkeypatch.setenv("HOME", str(tmp_path))
    _write_claude(tmp_path, f"{tmp_path}/gone/daisugi hook record --format claude || true")
    # a compound hook is left as it is, and its program is gone
    r = CliRunner().invoke(app, ["install", "--yes", "--runtime", "claude", "--only", "capture"])
    assert r.exit_code == 1
    assert "[Claude Code] The command" in r.output


def test_a_uv_cache_path_is_called_temporary(monkeypatch, tmp_path):
    exe = _executable(tmp_path / ".cache" / "uv" / "archive-v0" / "abc" / "bin" / "daisugi")
    monkeypatch.setattr(inst, "daisugi_exe", lambda: exe)
    line = inst.temporary_exe_note()
    assert line is not None and "uv tool install" in line and "temporary" in line


def test_a_linked_uv_cache_path_is_called_temporary(monkeypatch, tmp_path):
    real = _executable(tmp_path / ".cache" / "uv" / "archive-v0" / "abc" / "bin" / "daisugi")
    link = tmp_path / "bin" / "daisugi"
    link.parent.mkdir()
    os.symlink(real, link)
    monkeypatch.setattr(inst, "daisugi_exe", lambda: str(link))
    assert inst.temporary_exe_note() is not None


def test_an_installed_path_is_not_temporary(fake_daisugi_exe):
    assert inst.temporary_exe_note() is None


def test_cli_install_prints_the_uv_line_once_and_still_exits_0(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    (tmp_path / ".claude").mkdir()
    exe = _executable(tmp_path / ".cache" / "uv" / "archive-v0" / "abc" / "bin" / "daisugi")
    monkeypatch.setattr(inst, "daisugi_exe", lambda: exe)
    r = CliRunner().invoke(app, ["install", "--yes", "--runtime", "claude"])
    assert r.exit_code == 0, r.output
    assert r.output.count("uv tool install") == 1
