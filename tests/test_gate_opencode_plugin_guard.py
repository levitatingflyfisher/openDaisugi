"""The gate's rule that no agent edits OpenCode's gate plugin.

OpenCode loads every file in its global plugin directory at start, and the
gate plugin is one of them. An agent that could write there could delete
or rewrite its own gate. The gate denies every write it can place there,
and every shell line or tool argument that names that directory, before
any envelope check, in every mode and for every host, and no operator ask
can turn it.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.floor_config import OPENCODE_PLUGIN_REFUSAL
from opendaisugi.floor_config import REFUSAL as FLOOR_REFUSAL
from opendaisugi.gate import evaluate_call
from opendaisugi.models import Envelope, Permission


@pytest.fixture
def allow_all() -> Envelope:
    return Envelope(
        generated_by="test",
        task="allow everything",
        permissions=Permission(
            file_read=["/**"],
            file_write=["/**"],
            shell=True,
            shell_allowlist=["cp", "echo", "rm", "sed", "mv", "ls", "cat", "tee"],
            shell_allow_decomposition=True,
            mcp_allowlist=["opencode/apply_patch", "fs/write_file"],
        ),
    )


@pytest.fixture
def homes(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))
    return tmp_path


def _plugin(homes: Path) -> Path:
    return homes / "cfg" / "opencode" / "plugins" / "daisugi-gate.ts"


def _call(tool: str, tool_input: dict, cwd: str = "/work") -> dict:
    return {"tool_name": tool, "tool_input": tool_input, "session_id": "s1", "cwd": cwd}


def test_the_refusal_names_the_opencode_gate():
    assert OPENCODE_PLUGIN_REFUSAL == "this is OpenCode's config or gate plugin. Edit it yourself."


@pytest.mark.parametrize("mode", ["enforce", "shadow"])
@pytest.mark.parametrize("fmt", ["opencode", "claude", "pi"])
def test_a_write_to_the_installed_plugin_is_denied(allow_all, homes, mode, fmt):
    call = _call("Write", {"filePath": str(_plugin(homes)), "file_path": str(_plugin(homes))})
    d = evaluate_call(call, allow_all, mode=mode, fmt=fmt)
    assert d.allow is False
    assert d.reason == OPENCODE_PLUGIN_REFUSAL
    assert d.pane_rule is True


def test_writes_under_every_plugin_directory_spelling_are_denied(allow_all, homes):
    for path in [
        homes / "cfg" / "opencode" / "plugins" / "other.ts",
        homes / "cfg" / "opencode" / "plugin" / "old.js",
        Path.home() / ".config" / "opencode" / "plugins" / "daisugi-gate.ts",
        Path.home() / ".config" / "opencode" / "plugin" / "x.ts",
    ]:
        d = evaluate_call(_call("Edit", {"filePath": str(path)}), allow_all, mode="enforce")
        assert d.reason == OPENCODE_PLUGIN_REFUSAL, path


def test_a_relative_write_from_inside_the_plugin_directory_is_denied(allow_all, homes):
    cwd = str(homes / "cfg" / "opencode" / "plugins")
    d = evaluate_call(
        _call("Write", {"filePath": "daisugi-gate.ts"}, cwd), allow_all, mode="enforce"
    )
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


@pytest.mark.parametrize(
    "command",
    [
        "rm ~/.config/opencode/plugins/daisugi-gate.ts",
        "sed -i s/throw/return/ $HOME/.config/opencode/plugins/daisugi-gate.ts",
        "mv ${HOME}/.config/./opencode/plugins/daisugi-gate.ts /work/x",
        "cd ~/.config/opencode && rm plugins/daisugi-gate.ts",
        "echo x > $XDG_CONFIG_HOME/opencode/plugins/daisugi-gate.ts",
        "cp x ~/.config/opencode/plugin/evil.ts",
    ],
)
def test_a_shell_line_naming_the_plugin_directory_is_denied(allow_all, homes, command):
    d = evaluate_call(_call("Bash", {"command": command}), allow_all, mode="enforce")
    assert d.allow is False, command
    assert d.reason == OPENCODE_PLUGIN_REFUSAL, command


def test_a_bash_call_that_runs_inside_the_plugin_directory_is_denied(allow_all, homes):
    """The plugin sends a bash workdir as the call's cwd."""
    cwd = str(homes / "cfg" / "opencode" / "plugins")
    call = _call("Bash", {"command": "rm daisugi-gate.ts", "workdir": cwd}, cwd)
    d = evaluate_call(call, allow_all, mode="enforce", fmt="opencode")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


def test_an_apply_patch_that_names_the_plugin_is_denied(allow_all, homes):
    patch = f"*** Begin Patch\n*** Delete File: {_plugin(homes)}\n*** End Patch"
    call = _call("mcp__opencode__apply_patch", {"patchText": patch})
    d = evaluate_call(call, allow_all, mode="enforce", fmt="opencode")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


def test_an_mcp_call_from_inside_the_plugin_directory_is_denied(allow_all, homes):
    cwd = str(homes / "cfg" / "opencode" / "plugins")
    patch = "*** Begin Patch\n*** Delete File: daisugi-gate.ts\n*** End Patch"
    call = _call("mcp__opencode__apply_patch", {"patchText": patch}, cwd)
    d = evaluate_call(call, allow_all, mode="enforce", fmt="opencode")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


def test_reading_the_plugin_is_left_to_the_envelope(allow_all, homes):
    d = evaluate_call(_call("Read", {"filePath": str(_plugin(homes))}), allow_all, mode="enforce")
    assert d.reason != OPENCODE_PLUGIN_REFUSAL
    assert d.allow is True


def test_other_writes_are_left_to_the_envelope(allow_all, homes):
    for path in ["/work/src/app.ts", "/work/.opencode/agent/review.md", "/work/opencode-notes.md"]:
        d = evaluate_call(_call("Write", {"filePath": path}), allow_all, mode="enforce")
        assert d.reason != OPENCODE_PLUGIN_REFUSAL, path
        assert d.allow is True, path


@pytest.mark.parametrize(
    "command",
    [
        "rm -rf ~/.config/opencode",
        "mv ~/.config/opencode ~/.config/oc2",
        "rm -rf $XDG_CONFIG_HOME/opencode",
        "cd ~/.config && rm -rf opencode",
        "echo x > ~/.config/opencode/opencode.json",
    ],
)
def test_removing_or_moving_the_whole_config_directory_is_denied(allow_all, homes, command):
    d = evaluate_call(_call("Bash", {"command": command}), allow_all, mode="enforce")
    assert d.allow is False, command
    assert d.reason == OPENCODE_PLUGIN_REFUSAL, command


def test_a_write_to_the_global_opencode_json_is_denied(allow_all, homes):
    for path in [
        homes / "cfg" / "opencode" / "opencode.json",
        homes / "cfg" / "opencode" / "opencode.jsonc",
        Path.home() / ".config" / "opencode" / "opencode.json",
    ]:
        d = evaluate_call(_call("Write", {"filePath": str(path)}), allow_all, mode="enforce")
        assert d.reason == OPENCODE_PLUGIN_REFUSAL, path


@pytest.mark.parametrize(
    "path",
    [
        "/work/opencode.json",
        "/work/opencode.jsonc",
        "/work/.opencode/opencode.json",
        "/work/.opencode/plugins/evil.ts",
        "/work/.opencode/plugin/evil.js",
        "/work/sub/.opencode/plugins/evil.ts",
        ".opencode/plugins/evil.ts",
        "opencode.json",
    ],
)
def test_a_write_to_a_project_plugin_or_opencode_json_is_denied(allow_all, homes, path):
    d = evaluate_call(_call("Write", {"filePath": path}, "/work"), allow_all, mode="enforce")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL, path
    assert d.pane_rule is True


@pytest.mark.parametrize(
    "command",
    [
        "cp evil.ts .opencode/plugins/evil.ts",
        "rm -rf .opencode",
        "cd .opencode && echo x > plugins/evil.ts",
        "sed -i s/a/b/ opencode.json",
        "mv x ./sub/../.opencode/plugin/y.ts",
    ],
)
def test_a_shell_line_naming_a_project_plugin_path_is_denied(allow_all, homes, command):
    d = evaluate_call(_call("Bash", {"command": command}, "/work"), allow_all, mode="enforce")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL, command


def test_an_mcp_argument_naming_a_project_plugin_is_denied(allow_all, homes):
    call = _call("mcp__fs__write_file", {"path": "/work/.opencode/plugins/evil.ts", "content": "x"})
    d = evaluate_call(call, allow_all, mode="enforce")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


def test_the_floor_rule_still_names_the_floor(allow_all, homes):
    path = str(homes / "cfg" / "coppice" / "coppice.toml")
    d = evaluate_call(_call("Write", {"file_path": path}), allow_all, mode="enforce")
    assert d.reason == FLOOR_REFUSAL


@pytest.mark.parametrize(
    "path",
    [
        "/work/.opencode/tool/evil.ts",
        "/work/.opencode/tools/evil.ts",
        ".opencode/tools/evil.js",
    ],
)
def test_a_write_to_a_project_tool_directory_is_denied(allow_all, homes, path):
    for mode in ("enforce", "shadow"):
        d = evaluate_call(
            _call("Write", {"filePath": path}, "/work"), allow_all, mode=mode, fmt="opencode"
        )
        assert d.reason == OPENCODE_PLUGIN_REFUSAL, path
        assert d.pane_rule is True


def test_a_shell_line_writing_a_project_tool_is_denied(allow_all, homes):
    d = evaluate_call(
        _call("Bash", {"command": "cp x .opencode/tools/evil.ts"}, "/work"),
        allow_all,
        mode="enforce",
    )
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


@pytest.fixture
def reads_ok() -> Envelope:
    return Envelope(
        generated_by="test",
        task="reads",
        permissions=Permission(
            file_read=["/**"],
            file_write=["/work/**"],
            shell=True,
            shell_allowlist=["grep", "git", "rg"],
            shell_allow_decomposition=True,
            mcp_allowlist=["github/create_issue"],
        ),
    )


@pytest.mark.parametrize("mode", ["enforce", "shadow"])
@pytest.mark.parametrize(
    "command",
    [
        'git commit -m "see opencode.json docs"',
        "git commit -m 'move .opencode/plugins/x.ts out'",
        'git commit --message="drop opencode.json"',
        'git commit -am "opencode.json"',
    ],
)
def test_a_git_commit_message_naming_opencode_json_passes_the_hard_rule(
    reads_ok, homes, command, mode
):
    d = evaluate_call(_call("Bash", {"command": command}, "/work"), reads_ok, mode=mode)
    assert d.reason != OPENCODE_PLUGIN_REFUSAL, command
    assert d.pane_rule is False


@pytest.mark.parametrize(
    "command",
    [
        "grep -rn opencode.json src",
        "git log -- .opencode",
        "rg -n .opencode/plugins docs",
    ],
)
def test_a_read_that_names_the_paths_is_denied_and_fails_closed(reads_ok, homes, command):
    d = evaluate_call(_call("Bash", {"command": command}, "/work"), reads_ok, mode="enforce")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL, command


@pytest.mark.parametrize(
    ("command", "cwd"),
    [
        ("cd ~/.config && find opencode -name daisugi-gate.ts | xargs rm", "/work"),
        ("ls opencode/plugins/daisugi-gate.ts | xargs rm", "HOME_CONFIG"),
        ("echo .opencode/plugins/x.ts | xargs rm", "/work"),
        ("find .opencode -name '*.ts' | xargs rm", "/work"),
        ("grep -l x -r .opencode | xargs rm", "/work"),
        ("ls -d .opencode/plugins/x.ts | xargs -I{} cp evil.ts {}", "/work"),
        ("for f in .opencode/plugins/*; do rm $f; done", "/work"),
        ("git grep --no-index -Ocp -e '' -- evil.json opencode.json", "/work"),
        (
            'git commit -m ".opencode/plugins/*"; for f in .opencode/plugins/*; do rm $f; done',
            "/work",
        ),
    ],
)
def test_each_re_review_probe_line_is_denied(allow_all, homes, command, cwd):
    if cwd == "HOME_CONFIG":
        cwd = str(Path.home() / ".config")
    for mode in ("enforce", "shadow"):
        d = evaluate_call(
            _call("Bash", {"command": command}, cwd), allow_all, mode=mode, fmt="opencode"
        )
        assert d.allow is False, command
        assert d.reason == OPENCODE_PLUGIN_REFUSAL, command
        assert d.pane_rule is True


@pytest.mark.parametrize(
    "command",
    [
        "git grep -Ocp -e x -- a",
        "git grep --open-files-in-pager=vi x",
        "rg --pre sh x",
        "rg --pre=sh x",
    ],
)
def test_git_grep_o_and_rg_pre_are_never_placed_as_reads(command):
    from opendaisugi.effects import PERMANENT, effect_class, tier_for

    record = {"step_type": "shell", "command": command}
    assert tier_for(effect_class(record, "/work", "/work")) == PERMANENT, command


@pytest.mark.parametrize("mode", ["enforce", "shadow"])
def test_an_mcp_issue_body_naming_opencode_json_passes_the_hard_rule(reads_ok, homes, mode):
    call = _call(
        "mcp__github__create_issue",
        {"title": "docs", "body": "see opencode.json docs and .opencode/plugins for the gate"},
    )
    d = evaluate_call(call, reads_ok, mode=mode)
    assert d.reason != OPENCODE_PLUGIN_REFUSAL
    assert d.allow is True


def test_a_word_the_gate_cannot_place_still_meets_the_text_rule(allow_all, homes):
    command = "cd $(mktemp -d) && cp x .opencode/plugins/y.ts"
    d = evaluate_call(_call("Bash", {"command": command}, "/work"), allow_all, mode="enforce")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


@pytest.fixture
def shell_mcp() -> Envelope:
    return Envelope(
        generated_by="test",
        task="mcp shell",
        permissions=Permission(file_read=["/**"], mcp_allowlist=["shell/run"]),
    )


@pytest.mark.parametrize("fmt", ["opencode", "claude"])
@pytest.mark.parametrize(
    ("command", "refusal"),
    [
        ("rm -rf $HOME/.config/./coppice", FLOOR_REFUSAL),
        ("cat $HOME//.config/coppice/coppice.toml", FLOOR_REFUSAL),
        ("rm ~/.config/x/../coppice/coppice.toml", FLOOR_REFUSAL),
        ("rm $HOME/.config/./opencode/plugins/daisugi-gate.ts", OPENCODE_PLUGIN_REFUSAL),
    ],
)
def test_an_mcp_argument_with_an_odd_spelling_is_still_placed(
    shell_mcp, homes, command, refusal, fmt
):
    call = _call("mcp__shell__run", {"command": command})
    d = evaluate_call(call, shell_mcp, mode="enforce", fmt=fmt)
    assert d.allow is False, command
    assert d.reason == refusal, command
    assert d.pane_rule is True


@pytest.mark.parametrize(
    "command",
    [
        "git checkout -m .opencode/plugins/x.ts",
        "git restore -m opencode.json",
        "git -C /work checkout -m .opencode/plugins/x.ts",
    ],
)
def test_a_git_m_that_takes_no_message_is_not_exempt(allow_all, homes, command):
    d = evaluate_call(_call("Bash", {"command": command}, "/work"), allow_all, mode="enforce")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL, command


@pytest.mark.parametrize(
    "command",
    [
        'git -C /work commit -m "opencode.json"',
        'git tag -a v1 -m "opencode.json"',
        'git merge -m "fix .opencode/plugins/x.ts" topic',
        'git notes add -m "opencode.json"',
        'git stash push -m "opencode.json"',
        'git stash save -m "opencode.json"',
    ],
)
def test_a_git_message_on_a_message_verb_is_exempt(reads_ok, homes, command):
    d = evaluate_call(_call("Bash", {"command": command}, "/work"), reads_ok, mode="enforce")
    assert d.reason != OPENCODE_PLUGIN_REFUSAL, command
