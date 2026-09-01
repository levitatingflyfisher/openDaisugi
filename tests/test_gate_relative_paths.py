"""A relative file path is placed from the call's working directory.

A loop may name a file as fizz.py. The gate checks the file the call will
touch, so it joins the path to the call's absolute cwd first. With no
absolute cwd the path stays as written, and a workspace pattern does not
match it, so the call is denied.
"""

from __future__ import annotations

import pytest

from opendaisugi.gate import evaluate_call
from opendaisugi.models import Envelope, Permission


@pytest.fixture(autouse=True)
def homes(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))


def _env() -> Envelope:
    return Envelope(
        generated_by="test",
        task="relative",
        permissions=Permission(file_read=["/work/**"], file_write=["/work/**"]),
    )


def _write(path: str, **extra) -> dict:
    return {"tool_name": "Write", "tool_input": {"path": path, "content": "x"}, **extra}


def test_a_relative_write_inside_the_workspace_is_allowed():
    d = evaluate_call(_write("fizz.py", cwd="/work"), _env(), mode="enforce")
    assert d.allow, d.reason


def test_a_relative_write_that_climbs_out_is_denied():
    d = evaluate_call(_write("../etc/passwd", cwd="/work"), _env(), mode="enforce")
    assert not d.allow


def test_a_relative_write_with_no_cwd_is_denied():
    d = evaluate_call(_write("fizz.py"), _env(), mode="enforce")
    assert not d.allow


def test_a_relative_cwd_places_nothing():
    d = evaluate_call(_write("fizz.py", cwd="work"), _env(), mode="enforce")
    assert not d.allow


def test_a_relative_read_inside_the_workspace_is_allowed():
    call = {"tool_name": "Read", "tool_input": {"file_path": "src/a.py"}, "cwd": "/work"}
    d = evaluate_call(call, _env(), mode="enforce")
    assert d.allow, d.reason


def test_a_home_path_is_not_joined_to_the_cwd():
    from opendaisugi.hook import _payload_to_record

    for path in ["~/.config/coppice/coppice.toml", "$HOME/.config/coppice/coppice.toml"]:
        record = _payload_to_record(_write(path, cwd="/work"))
        assert record["path"] == path
