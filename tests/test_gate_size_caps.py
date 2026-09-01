"""Size caps the gate applies before it reads a call.

Reading a payload or a shell command costs time that grows with its size
(shlex is quadratic in CPython), so a large enough input made the verdict
depend on the verify budget and the speed of the box. The gate now denies
a payload past MAX_PAYLOAD_BYTES and a shell command past
MAX_SHELL_COMMAND_CHARS before it reads them, the same cap in every client.
"""

import json

from opendaisugi.gate import MAX_PAYLOAD_BYTES, MAX_SHELL_COMMAND_CHARS, gate_and_contract

ENV = {"generated_by": "t", "task": "t",
       "permissions": {"shell": True, "shell_allowlist": ["echo"], "file_read": ["/work/**"]}}


def _root(tmp_path):
    root = tmp_path / "gate"
    (root / "envelopes").mkdir(parents=True)
    (root / "envelopes" / "default.json").write_text(json.dumps(ENV))
    return root


def test_the_caps():
    assert MAX_PAYLOAD_BYTES == 16 * 1024 * 1024
    assert MAX_SHELL_COMMAND_CHARS == 256 * 1024


def test_a_payload_past_the_cap_is_denied_unread(tmp_path):
    raw = json.dumps({"session_id": "s", "tool_name": "Read", "cwd": "/work",
                      "tool_input": {"file_path": "/work/a", "pad": "x" * MAX_PAYLOAD_BYTES}}).encode()
    out = gate_and_contract(raw, root=_root(tmp_path), mode="enforce")
    assert out.exit_code == 2
    assert out.decision.reason == f"hook payload is larger than {MAX_PAYLOAD_BYTES} bytes; the gate does not read it"


def test_a_shell_command_at_the_cap_is_read_and_past_it_is_denied(tmp_path):
    root = _root(tmp_path)
    for n, denied in ((MAX_SHELL_COMMAND_CHARS, False), (MAX_SHELL_COMMAND_CHARS + 1, True)):
        cmd = "echo " + "a" * (n - 5)
        raw = json.dumps({"session_id": "s", "tool_name": "Bash", "cwd": "/work",
                          "tool_input": {"command": cmd}}).encode()
        out = gate_and_contract(raw, root=root, mode="enforce")
        want = f"shell command is longer than {MAX_SHELL_COMMAND_CHARS} characters; the gate does not read it"
        assert (out.decision.reason == want) is denied
        assert out.exit_code == (2 if denied else 0)
