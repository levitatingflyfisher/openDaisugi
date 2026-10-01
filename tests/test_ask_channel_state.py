"""The ask channel is read and written through no link (GR-R-10).

``operator.json``, ``asks/`` and ``answers/`` are opened as the rest of the
gate's state is (GR-R-1): the root and each directory without following a
symlink, each file by its fd with the owner, link-count and mode checks, the
mtime from the same fd. A planted link at any of them used to let a
concurrent workspace write supply ``{"decision": "allow"}``. Each test here
plants one and needs the refusal.
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path

import pytest

from opendaisugi import _state_files, ask


@pytest.fixture(autouse=True)
def _fresh_notes():
    _state_files._NOTED.clear()
    yield
    _state_files._NOTED.clear()


@pytest.fixture
def root(tmp_path: Path) -> Path:
    r = tmp_path / "gate"
    r.mkdir(mode=0o700)
    r.chmod(0o700)
    return r


def _forged(nonce: object, decision: str = "allow") -> str:
    return json.dumps({"toolUseId": "t1", "decision": decision, "nonce": nonce})


def _present(root: Path) -> None:
    ask.write_presence(root)


# ---- operator.json ---------------------------------------------------------------


def test_a_plain_presence_file_counts(root):
    _present(root)
    assert ask.operator_present(root) is True


def test_a_symlinked_operator_file_is_no_operator(root, tmp_path):
    real = tmp_path / "elsewhere.json"
    real.write_text(json.dumps({"pid": os.getpid(), "at": 0}))
    real.chmod(0o600)
    (root / "operator.json").symlink_to(real)
    assert ask.operator_present(root) is False


@pytest.mark.parametrize("mode", [0o620, 0o602, 0o666])
def test_an_operator_file_others_can_write_is_no_operator(root, mode):
    _present(root)
    (root / "operator.json").chmod(mode)
    assert ask.operator_present(root) is False


def test_a_hard_linked_operator_file_is_no_operator(root, tmp_path):
    _present(root)
    os.link(root / "operator.json", tmp_path / "second-name")
    assert ask.operator_present(root) is False


def test_presence_is_written_over_a_planted_link_not_through_it(root, tmp_path):
    victim = tmp_path / "victim"
    victim.write_text("keep")
    (root / "operator.json").symlink_to(victim)
    ask.write_presence(root)
    assert victim.read_text() == "keep"
    assert ask.operator_present(root) is True


# ---- asks/ and answers/ ------------------------------------------------------------


def _post(root: Path, tier: str = "permanent", deadline: float | None = None) -> Path:
    return ask.post_ask(
        root,
        tool_use_id="t1",
        question={"tier": tier},
        deadline=time.time() + 60 if deadline is None else deadline,
    )


def test_a_symlinked_asks_dir_takes_no_ask(root, tmp_path):
    real = tmp_path / "real-asks"
    real.mkdir()
    (root / "asks").symlink_to(real)
    assert ask.channel_ok(root) is False
    with pytest.raises(_state_files.StateWriteRefused):
        _post(root)
    assert list(real.iterdir()) == []


def test_a_symlinked_answers_dir_is_not_honored(root, tmp_path):
    _post(root)
    nonce = json.loads((root / "asks" / "t1.json").read_text())["nonce"]
    real = tmp_path / "real-answers"
    real.mkdir()
    (real / "t1.json").write_text(_forged(nonce))
    (real / "t1.json").chmod(0o600)
    (root / "answers").symlink_to(real)
    assert ask.channel_ok(root) is False
    t0 = time.monotonic()
    assert ask.wait_answer(root, tool_use_id="t1", timeout_s=30) is None
    assert time.monotonic() - t0 < 5
    assert (real / "t1.json").exists()  # nothing was removed through the link


def test_a_symlinked_answer_file_is_not_honored_and_is_removed(root, tmp_path):
    _post(root)
    nonce = json.loads((root / "asks" / "t1.json").read_text())["nonce"]
    real = tmp_path / "real-answer.json"
    real.write_text(_forged(nonce))
    real.chmod(0o600)
    (root / "answers").mkdir(mode=0o700)
    (root / "answers" / "t1.json").symlink_to(real)
    assert ask.wait_answer(root, tool_use_id="t1", timeout_s=2) is None
    assert real.exists()
    assert not (root / "answers" / "t1.json").is_symlink()


def test_an_answer_others_can_write_is_not_honored(root):
    _post(root)
    nonce = json.loads((root / "asks" / "t1.json").read_text())["nonce"]
    (root / "answers").mkdir(mode=0o700)
    a = root / "answers" / "t1.json"
    a.write_text(_forged(nonce))
    a.chmod(0o666)
    assert ask.wait_answer(root, tool_use_id="t1", timeout_s=2) is None


def test_a_hard_linked_answer_is_not_honored(root, tmp_path):
    _post(root)
    nonce = json.loads((root / "asks" / "t1.json").read_text())["nonce"]
    (root / "answers").mkdir(mode=0o700)
    a = root / "answers" / "t1.json"
    a.write_text(_forged(nonce))
    a.chmod(0o600)
    os.link(a, tmp_path / "second-name")
    assert ask.wait_answer(root, tool_use_id="t1", timeout_s=2) is None


def test_a_real_answer_is_still_honored(root):
    _post(root)
    ask.answer(root, tool_use_id="t1", decision="allow", reason="ok")
    got = ask.wait_answer(root, tool_use_id="t1", timeout_s=2)
    assert got is not None and got["decision"] == "allow"
    assert not (root / "asks" / "t1.json").exists()
    assert not (root / "answers" / "t1.json").exists()


def test_the_channel_dirs_and_files_are_private_whatever_the_umask(tmp_path):
    old = os.umask(0o002)
    try:
        r = tmp_path / "home" / "gate"
        _post(r)
        ask.answer(r, tool_use_id="t1", decision="deny")
    finally:
        os.umask(old)
    for rel, want in [("", 0o700), ("asks", 0o700), ("answers", 0o700)]:
        assert (r / rel).stat().st_mode & 0o777 == want
    assert (r / "asks" / "t1.json").stat().st_mode & 0o777 == 0o600
    assert (r / "answers" / "t1.json").stat().st_mode & 0o777 == 0o600


def test_a_planted_temp_name_does_not_wedge_the_channel(root, tmp_path):
    victim = tmp_path / "victim"
    victim.write_text("keep")
    (root / "asks").mkdir(mode=0o700)
    for n in range(4):
        (root / "asks" / f".t1.json.{n}.tmp").symlink_to(victim)
    _post(root)
    assert victim.read_text() == "keep"
    assert (root / "asks" / "t1.json").exists()


def test_the_sweep_removes_a_planted_ask_link_and_reads_no_target(root, tmp_path):
    real = tmp_path / "real-ask.json"
    real.write_text(json.dumps({"deadline": time.time() + 600}))
    real.chmod(0o600)
    (root / "asks").mkdir(mode=0o700)
    (root / "asks" / "x.json").symlink_to(real)
    assert ask.pending_asks(root) == []
    assert real.exists()
    assert not (root / "asks" / "x.json").exists()


# ---- the permanent rule reads the ask file the same way ------------------------------


def test_check_allow_reads_no_linked_ask(root, tmp_path):
    real = tmp_path / "real-ask.json"
    real.write_text(json.dumps({"tier": "undoable", "nonce": "n"}))
    real.chmod(0o600)
    (root / "asks").mkdir(mode=0o700)
    (root / "asks" / "t1.json").symlink_to(real)
    assert ask.check_allow(root, tool_use_id="t1", name="s1", confirm=None) == "that ask is gone"


def test_check_allow_takes_the_worse_of_the_reported_and_the_file_tier(root):
    # The row the operator armed said permanent; the file now says undoable.
    _post(root, tier="undoable")
    want = "this cannot be undone. Type the session id to allow: s1"
    assert (
        ask.check_allow(root, tool_use_id="t1", name="s1", confirm=None, tier="permanent") == want
    )
    assert ask.check_allow(root, tool_use_id="t1", name="s1", confirm=None, tier=None) == want
    assert ask.check_allow(root, tool_use_id="t1", name="s1", confirm=None, tier="undoable") is None
    # Not reported at all: the file alone decides, as before.
    assert ask.check_allow(root, tool_use_id="t1", name="s1", confirm=None) is None


def test_check_allow_reads_an_ask_others_can_write_as_gone(root):
    p = _post(root, tier="undoable")
    p.chmod(0o666)
    assert ask.check_allow(root, tool_use_id="t1", name="s1", confirm=None) == "that ask is gone"


# ---- the gate: a refused channel leaves the deny standing ------------------------


def _would_deny():
    from opendaisugi.gate import GateDecision

    return GateDecision(
        allow=False, would_deny=True, mode="enforce", reason="no", tool_name="Bash", tier="undoable"
    )


def test_a_gate_with_a_symlinked_asks_dir_leaves_the_deny_and_posts_nothing(root, tmp_path):
    from opendaisugi.gate import _maybe_ask

    _present(root)
    real = tmp_path / "real-asks"
    real.mkdir()
    (root / "asks").symlink_to(real)
    d = _would_deny()
    got = _maybe_ask(root, {"tool_use_id": "t1"}, d, timeout_s=30)
    assert got is d
    assert list(real.iterdir()) == []


def test_a_gate_with_a_symlinked_answers_dir_does_not_wait_and_honors_nothing(root, tmp_path):
    from opendaisugi.gate import _maybe_ask

    _present(root)
    real = tmp_path / "real-answers"
    real.mkdir()
    (root / "answers").symlink_to(real)
    t0 = time.monotonic()
    got = _maybe_ask(root, {"tool_use_id": "t1"}, _would_deny(), timeout_s=30)
    assert time.monotonic() - t0 < 5
    assert got.allow is False
    assert (root / "asks").exists() is False


def test_a_gate_with_a_symlinked_operator_file_asks_nobody(root, tmp_path):
    from opendaisugi.gate import _maybe_ask

    real = tmp_path / "op.json"
    real.write_text(json.dumps({"pid": os.getpid(), "at": 0}))
    real.chmod(0o600)
    (root / "operator.json").symlink_to(real)
    d = _would_deny()
    assert _maybe_ask(root, {"tool_use_id": "t1"}, d, timeout_s=30) is d
    assert not (root / "asks").exists()


# ---- GR r1 M5: the tier and the nonce come from one read of the ask -----------------


def test_answer_allow_carries_the_nonce_of_the_ask_it_checked(root, monkeypatch):
    _post(root, tier="undoable")
    first = json.loads((root / "asks" / "t1.json").read_text())["nonce"]
    real_read = ask._read
    calls = {"n": 0}

    def swapping(*a, **k):
        # After the first read, the gate retires the ask and posts a permanent one.
        out = real_read(*a, **k)
        calls["n"] += 1
        if calls["n"] == 1:
            (root / "asks" / "t1.json").unlink()
            _post(root, tier="permanent")
        return out

    monkeypatch.setattr(ask, "_read", swapping)
    refusal, path = ask.answer_allow(
        root, tool_use_id="t1", name="sess", confirm=None, tier="undoable"
    )
    assert refusal is None and path is not None
    assert calls["n"] == 1  # one read serves the tier and the nonce
    assert json.loads(path.read_text())["nonce"] == first


def test_answer_allow_refuses_a_permanent_ask_without_the_typed_name(root):
    _post(root, tier="permanent")
    refusal, path = ask.answer_allow(root, tool_use_id="t1", name="sess", confirm=None)
    assert path is None and "Type the session id to allow: sess" in str(refusal)
    assert not (root / "answers" / "t1.json").exists()
