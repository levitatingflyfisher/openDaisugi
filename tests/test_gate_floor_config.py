"""The gate's rule that no agent edits the floor's own config.

coppice.toml lists the plugins that run, and the plugin directory holds
their code. A pane that could write there could add a program the server
starts, outside every pane's gate. The gate denies every write under the
coppice config directory and the coppice data directories before any
envelope check, in every mode, and no operator ask can turn it.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.floor_config import REFUSAL
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
            shell_allowlist=["cp", "echo", "cat", "ls", "mkdir", "tee", "sh", "git"],
            shell_allow_decomposition=True,
        ),
    )


@pytest.fixture
def homes(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))
    return tmp_path


def _write(path: str, cwd: str = "/work") -> dict:
    return {
        "tool_name": "Write",
        "tool_input": {"file_path": path, "content": "x"},
        "session_id": "s1",
        "cwd": cwd,
    }


def _bash(command: str, cwd: str = "/work") -> dict:
    return {"tool_name": "Bash", "tool_input": {"command": command}, "session_id": "s1", "cwd": cwd}


def test_the_refusal_names_the_floor():
    assert REFUSAL == "this is the floor's own config. Edit it yourself."


@pytest.mark.parametrize("mode", ["enforce", "shadow"])
def test_a_write_to_the_plugin_directory_is_denied(allow_all, homes, mode):
    path = str(homes / "cfg" / "coppice" / "plugins" / "evil" / "manifest.json")
    d = evaluate_call(_write(path), allow_all, mode=mode)
    assert d.allow is False
    assert d.reason == REFUSAL
    assert d.pane_rule is True


def test_writes_under_every_floor_directory_are_denied(allow_all, homes):
    for path in [
        homes / "cfg" / "coppice" / "coppice.toml",
        homes / "share" / "coppice" / "plugins" / "_lib" / "floor_client.py",
        Path.home() / ".opendaisugi" / "coppice" / "web.json",
        Path.home() / ".config" / "coppice" / "coppice.toml",
    ]:
        d = evaluate_call(_write(str(path)), allow_all, mode="enforce")
        assert d.reason == REFUSAL, path


def test_a_relative_write_from_inside_the_config_is_denied(allow_all, homes):
    cwd = str(homes / "cfg" / "coppice")
    d = evaluate_call(_write("plugins/x/run.py", cwd=cwd), allow_all, mode="enforce")
    assert d.reason == REFUSAL


@pytest.mark.parametrize(
    "command",
    [
        "cp evil.py ~/.config/coppice/plugins/tree/tree.js",
        "echo 'plugins = []' > $HOME/.config/coppice/coppice.toml",
        "mkdir -p ${HOME}/.opendaisugi/coppice/x",
        "tee ~/.local/share/coppice/plugins/_lib/floor_client.py < x",
    ],
)
def test_a_shell_line_naming_the_floor_config_is_denied(allow_all, homes, command):
    d = evaluate_call(_bash(command), allow_all, mode="enforce")
    assert d.allow is False, command
    assert d.reason == REFUSAL


def test_a_shell_line_naming_the_xdg_config_is_denied(allow_all, homes):
    d = evaluate_call(
        _bash(f"cp x {homes}/cfg/coppice/plugins/a/run.py"), allow_all, mode="enforce"
    )
    assert d.reason == REFUSAL


def test_an_mcp_tool_writing_there_is_denied(allow_all, homes):
    payload = {
        "tool_name": "mcp__fs__write_file",
        "tool_input": {"path": str(homes / "cfg" / "coppice" / "coppice.toml"), "content": "x"},
        "session_id": "s1",
        "cwd": "/work",
    }
    d = evaluate_call(payload, allow_all, mode="enforce")
    assert d.reason == REFUSAL


def test_other_writes_are_left_to_the_envelope(allow_all, homes):
    for path in ["/work/src/app.py", str(homes / "cfg" / "other" / "x.toml")]:
        d = evaluate_call(_write(path), allow_all, mode="enforce")
        assert d.reason != REFUSAL, path
    d = evaluate_call(_bash("git status"), allow_all, mode="enforce")
    assert d.reason != REFUSAL


def test_the_web_token_read_stays_on_the_pane_rule(allow_all, homes):
    payload = {
        "tool_name": "Read",
        "tool_input": {"file_path": str(Path.home() / ".opendaisugi/coppice/web/token")},
        "session_id": "s1",
        "cwd": "/work",
    }
    d = evaluate_call(payload, allow_all, mode="enforce")
    assert d.allow is False and d.pane_rule is True


@pytest.mark.parametrize(
    "command",
    [
        "cp x ~/.config/./coppice/coppice.toml",
        "cp x $HOME/.config//coppice/coppice.toml",
        "cp x $HOME/.config/x/../coppice/coppice.toml",
        "cd $HOME/.config && cp x coppice/coppice.toml",
        "cd ~ && cp x .config/coppice/plugins/a/run.py",
        "cp x ${HOME}/.local/share/./coppice/plugins/_lib/floor_client.py",
        "cd /tmp && cp x ~/.opendaisugi//coppice/web.json",
        "echo x > ~/.config/./coppice/coppice.toml",
    ],
)
def test_a_path_spelled_another_way_is_still_denied(allow_all, homes, command):
    d = evaluate_call(_bash(command), allow_all, mode="enforce")
    assert d.allow is False, command
    assert d.reason == REFUSAL, command


def test_the_xdg_variable_spelling_is_denied(allow_all, homes):
    for command in [
        "cp x $XDG_CONFIG_HOME/./coppice/coppice.toml",
        "cd $XDG_DATA_HOME && cp x coppice/plugins/a.py",
    ]:
        d = evaluate_call(_bash(command), allow_all, mode="enforce")
        assert d.reason == REFUSAL, command


def test_a_write_path_spelled_another_way_is_denied(allow_all, homes):
    for path in [
        str(homes / "cfg" / "." / "coppice" / "coppice.toml"),
        str(homes / "cfg" / "x" / ".." / "coppice" / "coppice.toml"),
        "~/.config/./coppice/coppice.toml",
        "$HOME/.config//coppice/coppice.toml",
    ]:
        d = evaluate_call(_write(path), allow_all, mode="enforce")
        assert d.reason == REFUSAL, path


def test_a_relative_floor_word_after_a_lost_cd_is_refused(allow_all, homes):
    # After a cd the gate cannot follow, the cwd is unknown, so a relative
    # word could land anywhere. One that names a floor directory by its
    # last part is refused rather than left to its tier.
    for command in [
        "cd $(mktemp -d) && cp x coppice/coppice.toml",
        "cd .. && cp a .config/coppice/x",
        "cd ../.. && echo x > .local/share/coppice/y",
    ]:
        d = evaluate_call(_bash(command, cwd=str(homes / "work")), allow_all, mode="enforce")
        assert d.reason == REFUSAL, command


def test_a_relative_word_after_a_lost_cd_that_names_no_floor_part_is_not_refused(allow_all, homes):
    from opendaisugi.effects import PERMANENT, effect_class, tier_for

    command = "cd .. && cp a notes/x"
    d = evaluate_call(_bash(command, cwd=str(homes / "work")), allow_all, mode="enforce")
    assert d.reason != REFUSAL
    record = {"step_type": "shell", "command": command}
    assert tier_for(effect_class(record, "/work", "/work")) == PERMANENT


def test_a_long_line_of_redirects_is_checked_quickly(allow_all, homes):
    import time

    # One cwd for the whole line: each word is placed once, not once per
    # command already seen, so the check stays well inside a hook timeout.
    command = " ; ".join(f"ls > /work/a/b/c/o{i}" for i in range(400))
    t0 = time.monotonic()
    d = evaluate_call(_bash(command), allow_all, mode="enforce", verify_timeout_s=60)
    assert d.reason != REFUSAL
    assert time.monotonic() - t0 < 10


def test_a_line_with_many_distinct_cds_is_checked_quickly(allow_all, homes):
    import time

    # After many distinct cds the gate stops following them, as it does
    # for any cd it cannot follow, so the check stays inside a hook timeout.
    command = " ; ".join(f"cd /work/d{i} ; ls > /work/o{i}" for i in range(400))
    t0 = time.monotonic()
    evaluate_call(_bash(command), allow_all, mode="enforce", verify_timeout_s=60)
    assert time.monotonic() - t0 < 10


def test_after_many_cds_a_relative_floor_word_is_still_refused(allow_all, homes):
    command = " ; ".join(f"cd /work/d{i}" for i in range(100)) + " ; cp a coppice/coppice.toml"
    d = evaluate_call(_bash(command), allow_all, mode="enforce", verify_timeout_s=60)
    assert d.reason == REFUSAL


def test_a_line_the_floor_rule_cannot_split_is_a_hit(homes, monkeypatch):
    # A line deep enough to reach the recursion limit inside the floor rule
    # may still split in the verifier, which runs from a shallower stack.
    # The floor rule then cannot place the line's words, so it counts a hit.
    import opendaisugi.shell_decompose as sd
    from opendaisugi.floor_config import _shell_hit

    def deep(command):
        raise RecursionError("maximum recursion depth exceeded")

    monkeypatch.setattr(sd, "decompose_command", deep)
    hit, _ = _shell_hit(f"cd {homes}/cfg && ls && cd coppice && echo x > coppice.toml", "/work")
    assert hit


def test_a_word_split_error_does_not_end_the_floor_check(homes):
    # A command whose words cannot be split leaves the cwd unknown, and
    # the commands after it are still checked.
    from opendaisugi.floor_config import _shell_hit

    hit, _ = _shell_hit(f"cd {homes}/cfg ; echo $'a\\'b' ; cd coppice && echo x > coppice.toml", "/work")
    assert hit
