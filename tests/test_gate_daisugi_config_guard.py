"""The gate's rule that no agent edits daisugi's own config.yaml.

config.yaml beside the gate root holds dialect_enforce, the pin that turns
the dialect's words from audit to enforce. An agent that could write it
could drop the pin. The gate denies every write it can place there, and
every shell line or tool argument that names it, before any envelope
check, in every mode.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.floor_config import DAISUGI_CONFIG_REFUSAL, daisugi_config_roots
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
            shell_allowlist=["cp", "echo", "rm", "sed", "mv", "ls", "cat", "tee", "cd"],
            shell_allow_decomposition=True,
            mcp_allowlist=["fs/write_file"],
        ),
    )


@pytest.fixture
def root(tmp_path, monkeypatch) -> Path:
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    (tmp_path / "data" / "gate").mkdir(parents=True)
    return tmp_path / "data" / "gate"


def _call(tool: str, tool_input: dict, cwd: str = "/work") -> dict:
    return {"tool_name": tool, "tool_input": tool_input, "session_id": "s1", "cwd": cwd}


def test_the_refusal_names_daisugis_config():
    assert DAISUGI_CONFIG_REFUSAL == "this is daisugi's own config. Edit it yourself."


def test_the_roots_are_the_config_beside_the_gate_root_and_the_default(root):
    roots = daisugi_config_roots(root)
    assert root.parent / "config.yaml" in roots
    assert Path.home() / ".opendaisugi" / "config.yaml" in roots


@pytest.mark.parametrize("mode", ["enforce", "audit"])
def test_a_write_to_the_config_is_denied(allow_all, root, mode):
    path = str(root.parent / "config.yaml")
    d = evaluate_call(_call("Write", {"file_path": path}), allow_all, mode=mode, root=root)
    assert d.allow is False
    assert d.reason == DAISUGI_CONFIG_REFUSAL
    assert d.pane_rule is True


def test_the_default_config_is_guarded_too(allow_all, root):
    path = str(Path.home() / ".opendaisugi" / "config.yaml")
    d = evaluate_call(_call("Edit", {"file_path": path}), allow_all, mode="enforce", root=root)
    assert d.reason == DAISUGI_CONFIG_REFUSAL


@pytest.mark.parametrize(
    "command",
    [
        "sed -i '/dialect_enforce/d' {cfg}",
        "echo 'gate_mode: audit' > {cfg}",
        "cp /tmp/x {cfg}",
        "cd {data} && rm config.yaml",
        "cat {cfg}",
        "rm -f ~/.opendaisugi/config.yaml",
        "tee $HOME/.opendaisugi/config.yaml < /dev/null",
    ],
)
def test_a_shell_line_that_names_the_config_is_denied(allow_all, root, command):
    cfg = root.parent / "config.yaml"
    line = command.format(cfg=cfg, data=root.parent)
    d = evaluate_call(_call("Bash", {"command": line}), allow_all, mode="enforce", root=root)
    assert d.reason == DAISUGI_CONFIG_REFUSAL, line


def test_a_relative_write_from_the_data_dir_is_denied(allow_all, root):
    call = _call("Bash", {"command": "echo x > config.yaml"}, cwd=str(root.parent))
    d = evaluate_call(call, allow_all, mode="enforce", root=root)
    assert d.reason == DAISUGI_CONFIG_REFUSAL


def test_an_mcp_argument_that_names_the_config_is_denied(allow_all, root):
    call = _call("mcp__fs__write_file", {"path": str(root.parent / "config.yaml")})
    d = evaluate_call(call, allow_all, mode="enforce", root=root)
    assert d.reason == DAISUGI_CONFIG_REFUSAL


def test_a_read_tool_call_is_not_denied_here(allow_all, root):
    call = _call("Read", {"file_path": str(root.parent / "config.yaml")})
    d = evaluate_call(call, allow_all, mode="enforce", root=root)
    assert d.reason != DAISUGI_CONFIG_REFUSAL


def test_another_config_yaml_is_left_to_the_envelope(allow_all, root):
    d = evaluate_call(
        _call("Write", {"file_path": "/work/config.yaml"}), allow_all, mode="enforce", root=root
    )
    assert d.allow is True


@pytest.mark.parametrize(
    ("command", "cwd"),
    [
        ("cd .. && cat config.yaml", "/work"),
        ("cat config.yaml", None),
        ("cd .. && cat src/config.yaml", "/work"),
    ],
)
def test_a_bare_config_yaml_after_an_unfollowed_cd_is_left_to_the_envelope(
    allow_all, root, command, cwd
):
    d = evaluate_call(
        _call("Bash", {"command": command}, cwd=cwd), allow_all, mode="audit", root=root
    )
    assert d.reason != DAISUGI_CONFIG_REFUSAL, command


def test_the_data_dir_and_file_after_an_unfollowed_cd_are_denied(allow_all, root):
    call = _call("Bash", {"command": "cd .. && rm .opendaisugi/config.yaml"})
    d = evaluate_call(call, allow_all, mode="audit", root=root)
    assert d.reason == DAISUGI_CONFIG_REFUSAL


def test_an_mcp_text_that_mentions_config_yaml_is_left_to_the_envelope(allow_all, root):
    call = _call("mcp__fs__write_file", {"body": "fix the config.yaml parser"})
    d = evaluate_call(call, allow_all, mode="audit", root=root)
    assert d.reason != DAISUGI_CONFIG_REFUSAL
