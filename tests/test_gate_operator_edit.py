"""An operator's edited call passes the whole gate again.

The operator allows the call they were shown, and may send an
``updatedInput`` the host runs instead. That edit is a new call: it must
pass the pane rule, the hard-deny rules and the envelope, as any call
must. An edit that fails, or that is not a JSON object, is denied, with
no second ask. An edit the envelope does not admit is denied too, even
though the operator allowed the original call.
"""

from __future__ import annotations

import json
import os
import threading
import time

import pytest

from opendaisugi import ask
from opendaisugi.gate import EDIT_REFUSAL, FLOOR_REFUSAL, gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission


@pytest.fixture
def root(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    monkeypatch.delenv("XDG_CONFIG_HOME", raising=False)
    r = tmp_path / "gate"
    register_envelope(
        Envelope(
            generated_by="t",
            task="t",
            permissions=Permission(shell=True, shell_allowlist=["ls", "echo"]),
        ),
        root=r,
    )
    ask.write_presence(r, pid=os.getpid())
    return r


def _answer(root, updated_input, decision="allow"):
    def run():
        for _ in range(300):
            if (root / "asks" / "tu1.json").exists():
                ask.answer(
                    root,
                    tool_use_id="tu1",
                    decision=decision,
                    reason="ok",
                    updated_input=updated_input,
                    by="ana",
                    who_from="token",
                )
                return
            time.sleep(0.01)

    threading.Thread(target=run, daemon=True).start()


def _run(root, cmd="rm -rf /work/x"):
    raw = json.dumps(
        {
            "session_id": "s1",
            "tool_use_id": "tu1",
            "tool_name": "Bash",
            "tool_input": {"command": cmd},
            "cwd": "/work",
        }
    ).encode()
    return gate_and_contract(
        raw, root=root, fmt="claude", mode="enforce", ask=True, ask_timeout_s=5
    )


def test_an_edit_inside_the_envelope_is_carried(root):
    _answer(root, {"command": "ls"})
    out = _run(root)
    assert out.exit_code == 0
    body = json.loads(out.stdout)
    assert body["hookSpecificOutput"]["updatedInput"] == {"command": "ls"}
    assert out.decision.reason == "allowed by operator: ok"


def test_an_edit_into_the_floor_config_is_denied(root, tmp_path):
    _answer(root, {"command": f"rm -rf {tmp_path}/home/.config/coppice"})
    out = _run(root)
    assert out.exit_code == 2
    d = out.decision
    assert not d.allow and d.ask and d.pane_rule
    assert d.reason == f"{EDIT_REFUSAL}: {FLOOR_REFUSAL}"
    assert d.updated_input is None
    assert (d.answered_by, d.who_from) == ("ana", "token")


def test_an_edit_outside_the_envelope_is_denied(root):
    _answer(root, {"command": "rm -rf /tmp/x"})
    out = _run(root)
    assert out.exit_code == 2
    assert out.decision.reason.startswith(f"{EDIT_REFUSAL}: ")
    assert out.decision.updated_input is None


def test_an_edit_that_is_not_an_object_is_denied(root):
    _answer(root, "ls")
    out = _run(root)
    assert out.exit_code == 2
    assert out.decision.reason == f"{EDIT_REFUSAL}: the edit is not a JSON object"


def test_an_allow_with_no_edit_still_allows(root):
    _answer(root, None)
    out = _run(root)
    assert out.exit_code == 0
    assert out.decision.reason == "allowed by operator: ok"


def test_the_denied_edit_is_what_the_audit_log_records(root):
    _answer(root, {"command": "rm -rf /tmp/x"})
    _run(root)
    lines = (root / "audit" / "s1.jsonl").read_text(encoding="utf-8").splitlines()
    last = json.loads(lines[-1])
    assert last["allow"] is False and last["ask"] is True
    assert last["reason"].startswith(EDIT_REFUSAL)
