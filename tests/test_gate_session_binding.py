"""Which envelope a call is checked against must not follow the call's own
claim.

The session id in a hook payload is input the checked agent can shape: it
can start a second harness with any session id it likes (for example
``claude --session-id <id>``), and in a shared gate root that harness's
calls reach the same gate. So:

- unpinned, the gate checks every call against the ``default`` envelope.
  The payload's id is recorded, but it never selects an envelope;
- pinned with ``--session``, the gate checks the call against that
  session's own envelope only. A pin with no envelope denies. It never
  falls back to ``default``.
"""

from __future__ import annotations

import json

from opendaisugi.gate import gate_and_contract, load_envelope, register_envelope
from opendaisugi.models import Envelope, Permission


def _env(allow: list[str]) -> Envelope:
    return Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=allow),
    )


def _call(cmd: str, sid: str | None) -> bytes:
    p = {"tool_name": "Bash", "tool_input": {"command": cmd}, "cwd": "/work"}
    if sid is not None:
        p["session_id"] = sid
    return json.dumps(p).encode()


def _root(tmp_path):
    root = tmp_path / "gate"
    register_envelope(_env(["ls"]), root=root)  # narrow default
    register_envelope(_env(["ls", "rm"]), session_id="victim", root=root)  # wide session
    return root


def _denied(out) -> bool:
    return out.exit_code == 2


# --- the attacks -----------------------------------------------------------


def test_a_call_naming_another_sessions_id_gets_no_wider_envelope(tmp_path):
    root = _root(tmp_path)
    out = gate_and_contract(_call("rm x", "victim"), root=root, mode="enforce")
    assert _denied(out), "a payload session id selected a wider envelope"


def test_a_pinned_call_naming_another_session_keeps_its_pin(tmp_path):
    root = _root(tmp_path)
    register_envelope(_env(["ls"]), session_id="mine", root=root)
    out = gate_and_contract(_call("rm x", "victim"), root=root, mode="enforce", pin_session="mine")
    assert _denied(out)


def test_a_pin_with_no_envelope_denies_and_never_uses_default(tmp_path):
    root = tmp_path / "gate"
    register_envelope(_env(["ls", "rm"]), root=root)  # a wide default
    out = gate_and_contract(_call("ls", "x"), root=root, mode="enforce", pin_session="child")
    assert _denied(out)
    assert "no envelope registered" in out.stderr


# --- what still works --------------------------------------------------------


def test_unpinned_calls_use_default(tmp_path):
    root = _root(tmp_path)
    assert not _denied(gate_and_contract(_call("ls", "victim"), root=root, mode="enforce"))
    assert not _denied(gate_and_contract(_call("ls", None), root=root, mode="enforce"))
    assert _denied(gate_and_contract(_call("rm x", None), root=root, mode="enforce"))


def test_a_pin_selects_its_own_envelope(tmp_path):
    root = _root(tmp_path)
    out = gate_and_contract(
        _call("rm x", "anything"), root=root, mode="enforce", pin_session="victim"
    )
    assert not _denied(out)


def test_unpinned_with_no_default_denies(tmp_path):
    root = tmp_path / "gate"
    register_envelope(_env(["ls"]), session_id="victim", root=root)
    assert _denied(gate_and_contract(_call("ls", "victim"), root=root, mode="enforce"))


def test_load_envelope_reads_the_exact_file_or_default_only(tmp_path):
    root = _root(tmp_path)
    assert load_envelope("victim", root=root).permissions.shell_allowlist == ["ls", "rm"]
    assert load_envelope(None, root=root).permissions.shell_allowlist == ["ls"]
    assert load_envelope("nobody", root=root) is None
