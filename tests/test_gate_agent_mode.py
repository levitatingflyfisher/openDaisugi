"""The per-agent mode marker (F11): ``<gate root>/agents/<floor>/<pane>.mode``.

coppice writes the marker when the owner lowers one agent from enforcing to
watching. The gate reads it for the calling pane of the calling floor only
(COPPICE_PANE and COPPICE_SOCK). The marker can only lower an enforce hook
to audit for that one pane. Every other state of the file (missing,
garbage, ``enforcing``, a symlink, a FIFO, too big, a pane id of the wrong
shape, another floor's directory) leaves the hook's mode as it is.

Each test here fails when the marker code is taken out: it either needs
the marker to lower a call, or pairs a refusal with the same marker
lowering the call when it is in its right place.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

import pytest

from opendaisugi import gate as gate_mod
from opendaisugi.gate import (
    agent_mode_floor,
    agent_mode_lowered,
    register_envelope,
    run_argv,
    starter_envelope,
)

DENY = json.dumps(
    {"session_id": "s1", "tool_name": "Read", "tool_input": {"file_path": "/etc/passwd"}}
).encode()
SOCK = "/run/coppice/floor-a.sock"
OTHER_SOCK = "/run/coppice/floor-b.sock"


@pytest.fixture
def root(tmp_path: Path) -> Path:
    r = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path / "work"), root=r)
    return r


@pytest.fixture
def floor_env(monkeypatch):
    monkeypatch.setenv("COPPICE_SOCK", SOCK)
    monkeypatch.setenv("COPPICE_PANE", "w1:p1")


def _floor_dir(root: Path, sock: str = SOCK) -> Path:
    d = root / "agents" / agent_mode_floor(sock)
    d.mkdir(parents=True, exist_ok=True)
    return d


def _marker(root: Path, pane: str, body: bytes, sock: str = SOCK) -> Path:
    p = _floor_dir(root, sock) / f"{pane}.mode"
    p.write_bytes(body)
    p.chmod(0o600)
    return p


def _run(root: Path, mode: str = "enforce"):
    return run_argv(["--mode", mode, "--root", str(root), "--format", "claude"], DENY)


def _lowers(root: Path) -> bool:
    out = _run(root)
    return out.exit_code == 0 and out.decision.mode == "audit"


def test_the_floor_key_is_the_socket_paths_sha256():
    assert agent_mode_floor(SOCK) == hashlib.sha256(SOCK.encode()).hexdigest()[:16]
    assert len(agent_mode_floor("/x")) == 16


@pytest.mark.parametrize("body", [b"watching", b"watching\n"])
def test_watching_lowers_this_pane_to_audit(root, floor_env, body):
    assert not _lowers(root)
    _marker(root, "w1:p1", body)
    out = _run(root)
    assert out.exit_code == 0
    assert out.decision.mode == "audit"
    assert out.decision.would_deny is True


def test_watching_lowers_this_pane_only(root, floor_env, monkeypatch):
    _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    monkeypatch.setenv("COPPICE_PANE", "w1:p2")
    assert not _lowers(root)


def test_a_marker_in_another_floors_dir_is_ignored(root, floor_env, monkeypatch):
    """B2: two floors that share a gate root both have a w1:p1."""
    _marker(root, "w1:p1", b"watching\n", sock=OTHER_SOCK)
    assert not _lowers(root)
    monkeypatch.setenv("COPPICE_SOCK", OTHER_SOCK)
    assert _lowers(root)


def test_a_marker_with_no_floor_dir_is_ignored(root, floor_env):
    (root / "agents").mkdir()
    (root / "agents" / "w1:p1.mode").write_bytes(b"watching\n")
    assert not _lowers(root)
    _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)


def test_no_socket_reads_no_marker(root, floor_env, monkeypatch):
    _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    monkeypatch.delenv("COPPICE_SOCK")
    assert not _lowers(root)
    monkeypatch.setenv("COPPICE_SOCK", "")
    assert not _lowers(root)


@pytest.mark.parametrize(
    "body",
    [
        b"enforcing\n",
        b"off\n",
        b"audit\n",
        b"WATCHING\n",
        b" watching\n",
        b"watching\n\n",
        b"watching\r\n",
        b"watching\x00",
        b"\xffwatching",
        b"",
        b"watching\n" + b"x" * 100,
    ],
)
def test_any_other_content_keeps_enforce(root, floor_env, body):
    p = _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    p.write_bytes(body)
    assert not _lowers(root)


def test_a_symlinked_marker_is_not_followed(root, floor_env, tmp_path):
    target = tmp_path / "elsewhere"
    target.write_bytes(b"watching\n")
    (_floor_dir(root) / "w1:p1.mode").symlink_to(target)
    assert not _lowers(root)


def test_a_fifo_marker_does_not_hang_and_keeps_enforce(root, floor_env):
    os.mkfifo(_floor_dir(root) / "w1:p1.mode")
    assert not _lowers(root)


@pytest.mark.parametrize(
    "pane",
    ["../w1:p1", "w1:p1/../w1:p1", "w1:p1\n", "%3", "", "w1:p1.mode", "W1:P1", "w1:p1234567890"],
)
def test_a_pane_id_of_the_wrong_shape_reads_no_marker(root, floor_env, monkeypatch, pane):
    _marker(root, "w1:p1", b"watching\n")
    (_floor_dir(root).parent / "w1:p1.mode").write_bytes(b"watching\n")
    assert _lowers(root)
    monkeypatch.setenv("COPPICE_PANE", pane)
    assert not _lowers(root)


def test_only_coppice_pane_names_the_pane(root, floor_env, monkeypatch):
    _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    monkeypatch.delenv("COPPICE_PANE")
    monkeypatch.setenv("TMUX_PANE", "w1:p1")
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")
    assert not _lowers(root)


def test_the_marker_never_raises_an_audit_hook(root, floor_env):
    _marker(root, "w1:p1", b"enforcing\n")
    out = _run(root, mode="audit")
    assert out.exit_code == 0
    assert out.decision.mode == "audit"


def test_a_read_error_is_no_lowering(root, floor_env, monkeypatch):
    _marker(root, "w1:p1", b"watching\n")
    assert agent_mode_lowered(root) is True

    def boom(*a, **k):
        raise OSError("no")

    monkeypatch.setattr(gate_mod.os, "open", boom)
    assert agent_mode_lowered(root) is False


def test_the_resident_call_reads_the_callers_pane_and_floor(root, monkeypatch):
    monkeypatch.setenv("COPPICE_PANE", "w1:p9")
    monkeypatch.setenv("COPPICE_SOCK", SOCK)
    _marker(root, "w1:p1", b"watching\n")
    _marker(root, "w1:p9", b"watching\n")
    with gate_mod.resident_call(sock=SOCK, pane="w1:p1"):
        assert agent_mode_lowered(root) is True
    with gate_mod.resident_call(sock=OTHER_SOCK, pane="w1:p1"):
        assert agent_mode_lowered(root) is False
    with gate_mod.resident_call(sock=SOCK, pane="../w1:p1"):
        assert agent_mode_lowered(root) is False
    # A resident call never reads this process's own environment.
    with gate_mod.resident_call(sock=None, pane=None):
        assert agent_mode_lowered(root) is False


# F11-R-7 (B3): the directories are opened without following a symlink,
# and the marker must be this user's, with one link, and writable by no
# one else. Each test first shows the same marker lowering the call.


def test_a_symlinked_agents_dir_is_not_followed(root, floor_env, tmp_path):
    _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    real = tmp_path / "real-agents"
    (root / "agents").rename(real)
    (root / "agents").symlink_to(real)
    assert not _lowers(root)


def test_a_symlinked_floor_dir_is_not_followed(root, floor_env, tmp_path):
    d = _marker(root, "w1:p1", b"watching\n").parent
    assert _lowers(root)
    real = tmp_path / "real-floor"
    d.rename(real)
    d.symlink_to(real)
    assert not _lowers(root)


def test_a_hard_linked_marker_is_not_read(root, floor_env, tmp_path):
    p = _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    os.link(p, tmp_path / "second-name")
    assert not _lowers(root)


@pytest.mark.parametrize("mode", [0o620, 0o602, 0o666])
def test_a_marker_others_can_write_is_not_read(root, floor_env, mode):
    p = _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    p.chmod(mode)
    assert not _lowers(root)


def test_a_marker_another_user_owns_is_not_read(root, floor_env, monkeypatch):
    _marker(root, "w1:p1", b"watching\n")
    assert _lowers(root)
    monkeypatch.setattr(gate_mod.os, "geteuid", lambda: os.getuid() + 1)
    assert not _lowers(root)


# F11-R-11 (B1): a pane may not set a mode, so a typed wire call of
# agent.set_mode is the pane rule's deny in every mode, like agent.allow.
@pytest.mark.parametrize("mode", ["enforce", "audit"])
@pytest.mark.parametrize(
    "command",
    [
        """printf '{"cmd":"agent.set_mode","pane":"w1:p1","mode":"watching"}\\n' | nc -U "$COPPICE_SOCK\"""",
        """setsid python3 -c 'send({"cmd": "agent.set_mode", "pane": "w1:p2"})'""",
        """echo "{\\"method\\":\\"agent.set_mode\\"}" > /tmp/x""",
    ],
)
def test_a_typed_set_mode_is_denied_in_every_mode(root, mode, command):
    payload = json.dumps(
        {
            "session_id": "s1",
            "tool_name": "Bash",
            "tool_input": {"command": command},
            "cwd": "/work",
        }
    ).encode()
    out = run_argv(["--mode", mode, "--root", str(root)], payload)
    assert out.decision.pane_rule is True, out.decision.reason
    assert out.exit_code == 2 if mode == "enforce" else out.decision.allow is False
