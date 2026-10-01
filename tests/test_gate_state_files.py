"""The gate reads its own state through no link (GR-R-1, GR-R-2).

The gate root, ``DISARMED``, ``envelopes/`` and each envelope file are read
only when no link stands at or below the root, and only from a directory or
a regular file this user owns that group and other cannot write. A refusal
reads the safe way: DISARMED is not there, the envelope is none. It leaves
one stderr note naming the file and the reason.

Each test here fails when the check is taken out: it either plants the
state the gate must refuse and needs the refusal, or pairs the refusal with
the same state in its right place.
"""

from __future__ import annotations

import json
import os
import re
from pathlib import Path

import pytest

from opendaisugi import _state_files
from opendaisugi import delegate as delegate_mod
from opendaisugi import gate as gate_mod
from opendaisugi.gate import (
    disarm,
    is_disarmed,
    load_envelope,
    register_envelope,
    run_argv,
    starter_envelope,
)

READ = json.dumps(
    {
        "session_id": "s1",
        "tool_name": "Read",
        "tool_input": {"file_path": "/work/a.txt"},
        "cwd": "/work",
    }
).encode()
WIPE = json.dumps(
    {"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": "rm -rf /"}, "cwd": "/work"}
).encode()


@pytest.fixture(autouse=True)
def _fresh_notes():
    _state_files._NOTED.clear()
    yield
    _state_files._NOTED.clear()


@pytest.fixture
def root(tmp_path: Path) -> Path:
    r = tmp_path / "gate"
    register_envelope(starter_envelope(Path("/work")), root=r)
    return r


def _notes(capsys) -> list[str]:
    """The notes, each less its ``(fix: ...)`` tail: these tests are about
    the reason. The tail is held in test_gate_state_writers.py."""
    return [
        re.sub(r" \(fix: .*\)$", "", ln)
        for ln in capsys.readouterr().err.splitlines()
        if ln.startswith("daisugi gate: not reading ")
    ]


def _deny(root: Path, payload: bytes = READ):
    return run_argv(["--mode", "enforce", "--root", str(root), "--format", "claude"], payload)


# ---- DISARMED ---------------------------------------------------------------


def test_a_plain_disarmed_file_disarms(root, capsys):
    disarm(root)
    assert is_disarmed(root)
    assert _deny(root, WIPE).decision.allow is True
    assert _notes(capsys) == []


def test_a_missing_disarmed_file_is_armed_and_silent(root, capsys):
    assert not is_disarmed(root)
    assert _notes(capsys) == []


def test_a_symlinked_disarmed_is_armed(root, tmp_path, capsys):
    target = tmp_path / "elsewhere"
    target.write_text("x")
    target.chmod(0o600)
    (root / "DISARMED").symlink_to(target)
    assert not is_disarmed(root)
    assert _deny(root, WIPE).decision.allow is False
    assert _notes(capsys) == [f"daisugi gate: not reading {root}/DISARMED: is a symbolic link"]


def test_a_hard_linked_disarmed_is_armed(root, tmp_path, capsys):
    disarm(root)
    assert is_disarmed(root)
    os.link(root / "DISARMED", tmp_path / "second-name")
    assert not is_disarmed(root)
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/DISARMED: has more than one hard link"
    ]


@pytest.mark.parametrize("mode", [0o620, 0o602, 0o666])
def test_a_disarmed_others_can_write_is_armed(root, mode, capsys):
    disarm(root).chmod(mode)
    assert not is_disarmed(root)
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/DISARMED: is writable by group or others"
    ]


def test_a_disarmed_directory_is_armed(root, capsys):
    (root / "DISARMED").mkdir()
    assert not is_disarmed(root)
    assert _notes(capsys) == [f"daisugi gate: not reading {root}/DISARMED: is not a regular file"]


def test_a_disarmed_another_user_owns_is_armed(root, monkeypatch, capsys):
    disarm(root)
    assert is_disarmed(root)
    monkeypatch.setattr(gate_mod.os, "geteuid", lambda: os.getuid() + 1)
    assert not is_disarmed(root)
    # The root is the first thing refused, so it is the one named.
    assert _notes(capsys) == [f"daisugi gate: not reading {root}: is owned by another user"]


def test_a_note_is_given_once_per_file_and_reason(root, capsys):
    disarm(root).chmod(0o666)
    for _ in range(3):
        assert not is_disarmed(root)
    assert len(_notes(capsys)) == 1


# ---- the root ---------------------------------------------------------------


def test_a_symlinked_root_reads_nothing(root, tmp_path, capsys):
    disarm(root)
    link = tmp_path / "link-to-gate"
    link.symlink_to(root)
    assert is_disarmed(root)
    assert not is_disarmed(link)
    assert load_envelope(None, root=link) is None
    assert _deny(link, WIPE).decision.allow is False
    assert _notes(capsys) == [f"daisugi gate: not reading {link}: is a symbolic link"]


def test_a_root_behind_a_symlinked_parent_is_read(tmp_path, capsys):
    real = tmp_path / "real-home"
    real.mkdir()
    (tmp_path / "home").symlink_to(real)
    root = tmp_path / "home" / "gate"
    register_envelope(starter_envelope(Path("/work")), root=root)
    disarm(root)
    assert is_disarmed(root)
    assert load_envelope(None, root=root) is not None
    assert _notes(capsys) == []


@pytest.mark.parametrize("mode", [0o770, 0o707, 0o777])
def test_a_root_others_can_write_reads_nothing(root, mode, capsys):
    disarm(root)
    root.chmod(mode)
    assert not is_disarmed(root)
    assert load_envelope(None, root=root) is None
    assert _notes(capsys) == [f"daisugi gate: not reading {root}: is writable by group or others"]


def test_a_root_that_is_a_file_reads_nothing(tmp_path, capsys):
    f = tmp_path / "gate"
    f.write_text("x")
    assert not is_disarmed(f)
    assert _notes(capsys) == [f"daisugi gate: not reading {f}: is not a directory"]


def test_a_missing_root_is_silent(tmp_path, capsys):
    assert not is_disarmed(tmp_path / "nothing")
    assert load_envelope(None, root=tmp_path / "nothing") is None
    assert _notes(capsys) == []


# ---- envelopes --------------------------------------------------------------


def test_a_plain_envelope_is_loaded(root, capsys):
    assert load_envelope(None, root=root) is not None
    assert _deny(root).decision.allow is True
    assert _notes(capsys) == []


def test_a_symlinked_envelopes_dir_is_no_envelope(root, tmp_path, capsys):
    real = tmp_path / "real-envelopes"
    (root / "envelopes").rename(real)
    (root / "envelopes").symlink_to(real)
    assert load_envelope(None, root=root) is None
    assert _deny(root).decision.allow is False
    assert _notes(capsys) == [f"daisugi gate: not reading {root}/envelopes: is a symbolic link"]


def test_a_symlinked_envelope_file_is_no_envelope(root, tmp_path, capsys):
    real = tmp_path / "real.json"
    (root / "envelopes" / "default.json").rename(real)
    (root / "envelopes" / "default.json").symlink_to(real)
    assert load_envelope(None, root=root) is None
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/envelopes/default.json: is a symbolic link"
    ]


def test_a_hard_linked_envelope_is_no_envelope(root, tmp_path, capsys):
    assert load_envelope(None, root=root) is not None
    os.link(root / "envelopes" / "default.json", tmp_path / "second-name")
    assert load_envelope(None, root=root) is None
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/envelopes/default.json: has more than one hard link"
    ]


@pytest.mark.parametrize("mode", [0o620, 0o602, 0o666])
def test_an_envelope_others_can_write_is_no_envelope(root, mode, capsys):
    (root / "envelopes" / "default.json").chmod(mode)
    assert load_envelope(None, root=root) is None
    assert _deny(root).decision.allow is False
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/envelopes/default.json: is writable by group or others"
    ]


def test_an_envelopes_dir_others_can_write_is_no_envelope(root, capsys):
    (root / "envelopes").chmod(0o757)
    assert load_envelope(None, root=root) is None
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/envelopes: is writable by group or others"
    ]


def test_an_envelope_another_user_owns_is_no_envelope(root, monkeypatch, capsys):
    assert load_envelope(None, root=root) is not None
    monkeypatch.setattr(gate_mod.os, "geteuid", lambda: os.getuid() + 1)
    assert load_envelope(None, root=root) is None
    assert _notes(capsys) == [f"daisugi gate: not reading {root}: is owned by another user"]


def test_a_session_envelope_is_read_the_same_way(root, capsys):
    register_envelope(starter_envelope(Path("/work")), session_id="s1", root=root)
    assert load_envelope("s1", root=root) is not None
    (root / "envelopes" / "s1.json").chmod(0o666)
    assert load_envelope("s1", root=root) is None
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/envelopes/s1.json: is writable by group or others"
    ]


# ---- graft rules ------------------------------------------------------------

RULE = {
    "id": "big-reads",
    "version": 1,
    "shape": "deny_redirect",
    "state": "active",
    "match": {"tool": "Read", "file_lines_over": 100},
    "redirect": {"tool": "delegate"},
    "worker": {"choose": "router", "allow_remote": False},
}


def _rule(root: Path, mode: int = 0o600) -> Path:
    d = root / "grafts"
    d.mkdir(mode=0o700, exist_ok=True)
    p = d / "big-reads.json"
    p.write_text(json.dumps(RULE))
    p.chmod(mode)
    return p


def test_a_plain_graft_rule_is_loaded(root):
    _rule(root)
    rules, bad = delegate_mod.load_rules(root)
    assert [r.id for r in rules] == ["big-reads"] and bad == []


def test_a_graft_rule_others_can_write_is_left_out(root, capsys):
    _rule(root, 0o666)
    rules, bad = delegate_mod.load_rules(root)
    assert rules == [] and bad == [("big-reads.json", "not readable JSON")]
    assert _notes(capsys) == [
        f"daisugi gate: not reading {root}/grafts/big-reads.json: is writable by group or others"
    ]


def test_a_symlinked_grafts_dir_is_left_out(root, tmp_path, capsys):
    _rule(root)
    (root / "grafts").rename(tmp_path / "real-grafts")
    (root / "grafts").symlink_to(tmp_path / "real-grafts")
    assert delegate_mod.load_rules(root) == ([], [])
    assert _notes(capsys) == [f"daisugi gate: not reading {root}/grafts: is a symbolic link"]


# ---- writers pass the reader, whatever the umask ------------------------------


def test_the_writers_pass_the_reader_under_a_group_writable_umask(tmp_path):
    old = os.umask(0o002)
    try:
        root = tmp_path / "newhome" / "gate"
        register_envelope(starter_envelope(Path("/work")), root=root)
        disarm(root)
    finally:
        os.umask(old)
    assert is_disarmed(root)
    assert load_envelope(None, root=root) is not None


def test_enoent_from_the_open_stands_even_if_the_name_exists_a_moment_later(tmp_path):
    """The flake behind test_maybe_ask_operator_deny_is_a_deny: the open
    saw no file, a writer renamed one into place, and a second look at the
    name called the plain miss "cannot be opened". The errno of the open
    decides: ENOENT is a miss."""
    import errno

    from opendaisugi._state_files import _why_not_opened

    (tmp_path / "x.json").write_text("{}")
    assert _why_not_opened(str(tmp_path / "x.json"), None, errno.ENOENT) is None
    assert _why_not_opened(str(tmp_path / "x.json"), None, errno.EACCES) == "cannot be opened"
