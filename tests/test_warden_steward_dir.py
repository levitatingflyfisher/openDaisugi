"""The floor's steward works from its own directory, outside the floor.

coppice starts the steward in $XDG_STATE_HOME/coppice/steward, or
~/.local/state/coppice/steward when that is unset. The warden refuses every
call made from inside a directory it guards, so that directory must be
outside all of them, or the steward could run no command at all.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.floor_config import REFUSAL, text_names_floor
from opendaisugi.models import Envelope, Permission
from opendaisugi.warden import evaluate_call


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
def steward_dir(request, tmp_path, monkeypatch) -> str:
    """The steward's directory, with XDG_STATE_HOME set and unset. The
    floor's own directories point into tmp_path, as coppice's do on a
    machine with XDG set."""
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))
    if request.param == "set":
        monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "state"))
        return str(tmp_path / "state" / "coppice" / "steward")
    monkeypatch.delenv("XDG_STATE_HOME", raising=False)
    return str(Path.home() / ".local" / "state" / "coppice" / "steward")


@pytest.mark.parametrize("command", ["coppice agent list", "coppice new x --no-attach"])
def test_the_foremans_commands_are_not_floor_hits(allow_all, steward_dir, command):
    assert text_names_floor(command, steward_dir) is False
    payload = {
        "tool_name": "Bash",
        "tool_input": {"command": command},
        "session_id": "s1",
        "cwd": steward_dir,
    }
    d = evaluate_call(payload, allow_all, mode="enforce")
    assert d.reason != REFUSAL, d.reason


def test_a_relative_write_in_the_foremans_directory_is_not_a_floor_hit(allow_all, steward_dir):
    payload = {
        "tool_name": "Write",
        "tool_input": {"file_path": "notes.md", "content": "x"},
        "session_id": "s1",
        "cwd": steward_dir,
    }
    d = evaluate_call(payload, allow_all, mode="enforce")
    assert d.reason != REFUSAL, d.reason


def test_the_old_directory_inside_the_data_dir_was_a_floor_hit(steward_dir):
    old = str(Path.home() / ".opendaisugi" / "coppice" / "steward")
    assert text_names_floor("coppice agent list", old) is True
