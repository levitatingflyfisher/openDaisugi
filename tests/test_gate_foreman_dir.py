"""The floor's foreman works from its own directory, outside the floor.

coppice starts the foreman in $XDG_STATE_HOME/coppice/foreman, or
~/.local/state/coppice/foreman when that is unset. The gate refuses every
call made from inside a directory it guards, so that directory must be
outside all of them, or the foreman could run no command at all.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.floor_config import REFUSAL, text_names_floor
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
            shell_allowlist=["coppice"],
            shell_allow_decomposition=True,
        ),
    )


@pytest.fixture(params=["set", "unset"])
def foreman_dir(request, tmp_path, monkeypatch) -> str:
    """The foreman's directory, with XDG_STATE_HOME set and unset. The
    floor's own directories point into tmp_path, as coppice's do on a
    machine with XDG set."""
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))
    if request.param == "set":
        monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "state"))
        return str(tmp_path / "state" / "coppice" / "foreman")
    monkeypatch.delenv("XDG_STATE_HOME", raising=False)
    return str(Path.home() / ".local" / "state" / "coppice" / "foreman")


@pytest.mark.parametrize("command", ["coppice agent list", "coppice new x --no-attach"])
def test_the_foremans_commands_are_not_floor_hits(allow_all, foreman_dir, command):
    assert text_names_floor(command, foreman_dir) is False
    payload = {"tool_name": "Bash", "tool_input": {"command": command}, "session_id": "s1", "cwd": foreman_dir}
    d = evaluate_call(payload, allow_all, mode="enforce")
    assert d.reason != REFUSAL, d.reason


def test_a_relative_write_in_the_foremans_directory_is_not_a_floor_hit(allow_all, foreman_dir):
    payload = {
        "tool_name": "Write",
        "tool_input": {"file_path": "notes.md", "content": "x"},
        "session_id": "s1",
        "cwd": foreman_dir,
    }
    d = evaluate_call(payload, allow_all, mode="enforce")
    assert d.reason != REFUSAL, d.reason


def test_the_old_directory_inside_the_data_dir_was_a_floor_hit(foreman_dir):
    old = str(Path.home() / ".opendaisugi" / "coppice" / "foreman")
    assert text_names_floor("coppice agent list", old) is True
