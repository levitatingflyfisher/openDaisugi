"""Every allow and deny an operator gives names who gave it.

The answer file carries ``by`` and ``whoFrom``. The gate journals
``allowed_by`` or ``denied_by`` with ``who_from``. An answer with no name
journals ``local`` from ``none``, never an empty name.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi import ask
from opendaisugi.gate import gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission


def _deny_all() -> Envelope:
    return Envelope(generated_by="test", task="who test", permissions=Permission())


def _payload(tid: str) -> bytes:
    return json.dumps(
        {
            "tool_name": "Bash",
            "tool_input": {"command": "git push -f"},
            "session_id": "s1",
            "cwd": "/work",
            "tool_use_id": tid,
            "hook_event_name": "PreToolUse",
        }
    ).encode()


def _run(tmp_path: Path, monkeypatch, tid: str, answer: dict | None, timeout: float = 5.0):
    """Run one call through the gate with ask on. The operator answers with
    ``answer`` through the real answer file, or never when it is None."""
    from opendaisugi import gate

    root = tmp_path / "gate"
    register_envelope(_deny_all(), root=root)
    monkeypatch.setattr(ask, "operator_present", lambda r: True)
    monkeypatch.setattr(gate, "_report_blocked", lambda *a, **k: None)
    real_wait = ask.wait_answer

    def answered_wait(r, **kw):
        if answer is not None:
            ask.answer(r, tool_use_id=kw["tool_use_id"], **answer)
        return real_wait(r, **kw)

    monkeypatch.setattr(ask, "wait_answer", answered_wait)
    out = gate_and_contract(
        _payload(tid), root=root, mode="enforce", ask=True, ask_timeout_s=timeout
    )
    rows = [json.loads(line) for line in (root / "shadow" / "s1.jsonl").read_text().splitlines()]
    return out, rows[-1]


def test_an_allow_from_alice_journals_allowed_by_alice(tmp_path, monkeypatch):
    out, row = _run(
        tmp_path,
        monkeypatch,
        "toolu_a",
        {"decision": "allow", "reason": "ok", "by": "alice", "who_from": "token"},
    )
    assert out.decision.allow is True
    assert row["allowed_by"] == "alice"
    assert row["who_from"] == "token"
    assert "denied_by" not in row


def test_a_deny_from_bob_journals_denied_by_bob(tmp_path, monkeypatch):
    out, row = _run(
        tmp_path,
        monkeypatch,
        "toolu_b",
        {"decision": "deny", "reason": "no", "by": "bob", "who_from": "socket"},
    )
    assert out.decision.allow is False
    assert row["denied_by"] == "bob"
    assert row["who_from"] == "socket"
    assert "allowed_by" not in row


def test_an_answer_with_no_name_journals_local(tmp_path, monkeypatch):
    _, row = _run(tmp_path, monkeypatch, "toolu_c", {"decision": "allow", "reason": "ok"})
    assert row["allowed_by"] == "local"
    assert row["who_from"] == "none"


def test_a_foreman_deny_journals_the_pane(tmp_path, monkeypatch):
    _, row = _run(
        tmp_path,
        monkeypatch,
        "toolu_d",
        {"decision": "deny", "reason": "no", "by": "pane:w1:p9", "who_from": "pane"},
    )
    assert row["denied_by"] == "pane:w1:p9"
    assert row["who_from"] == "pane"


@pytest.mark.parametrize(
    "by,who_from",
    [("", "token"), ("two words", "token"), ("x" * 200, "socket"), ("alice", "passport")],
)
def test_a_name_that_breaks_the_rule_journals_local(tmp_path, monkeypatch, by, who_from):
    _, row = _run(
        tmp_path,
        monkeypatch,
        "toolu_e",
        {"decision": "allow", "reason": "ok", "by": by, "who_from": who_from},
    )
    assert row["allowed_by"] == "local"
    assert row["who_from"] == "none"


def test_a_timeout_names_nobody(tmp_path, monkeypatch):
    out, row = _run(tmp_path, monkeypatch, "toolu_f", None, timeout=0.0)
    assert out.decision.allow is False
    assert "allowed_by" not in row
    assert "denied_by" not in row
    assert "who_from" not in row


def test_the_answer_file_carries_the_name(tmp_path):
    root = tmp_path / "gate"
    ask.post_ask(root, tool_use_id="t1", question={}, deadline=9e9)
    path = ask.answer(root, tool_use_id="t1", decision="allow", by="alice", who_from="token")
    body = json.loads(path.read_text())
    assert body["by"] == "alice"
    assert body["whoFrom"] == "token"
    path = ask.answer(root, tool_use_id="t1", decision="allow")
    body = json.loads(path.read_text())
    assert body["by"] == "local"
    assert body["whoFrom"] == "none"


@pytest.mark.parametrize(
    "by,who_from,want",
    [
        ("pane", "pane", ("pane", "pane")),
        ("pane:w1:p9", "pane", ("pane:w1:p9", "pane")),
        ("plugin:merge", "plugin", ("plugin:merge", "plugin")),
        ("plugin", "plugin", ("local", "none")),
        ("pane:w1:p9", "plugin", ("local", "none")),
        ("alice", "pane", ("local", "none")),
    ],
)
def test_who_of_reads_the_names_coppice_writes(by, who_from, want):
    # coppice names a pane it cannot name by id as a bare pane.
    assert ask.who_of(by, who_from) == want
