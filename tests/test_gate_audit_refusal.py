"""FN-R-17: an enforce-mode allow whose audit line cannot land is denied.

Landlock cannot stop chmod, so an agent inside the fence can make its own
audit log read only. The gate must not then allow on with no record.
"""

from __future__ import annotations

import json
import os

import pytest

from opendaisugi.gate import AUDIT_REFUSAL, run_argv

ENVELOPE = {
    "generated_by": "t",
    "task": "t",
    "permissions": {"file_read": ["/**"], "file_write": []},
}
CALL = {"session_id": "s1", "tool_name": "Read", "tool_input": {"file_path": "/w/a"}, "cwd": "/w"}


def _run(root, mode="enforce"):
    out = run_argv(
        ["--mode", mode, "--root", str(root), "--format", "claude"], json.dumps(CALL).encode()
    )
    return out.exit_code, out.stderr


@pytest.fixture
def root(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    r = tmp_path / "gate"
    (r / "envelopes").mkdir(parents=True)
    (r / "envelopes" / "default.json").write_text(json.dumps(ENVELOPE))
    return r


def test_a_writable_audit_log_allows(root):
    assert _run(root) == (0, "")
    assert (root / "audit" / "s1.jsonl").exists()


@pytest.mark.skipif(os.geteuid() == 0, reason="root ignores the file mode")
def test_a_read_only_audit_log_denies_in_enforce_and_allows_in_audit(root):
    assert _run(root)[0] == 0
    (root / "audit" / "s1.jsonl").chmod(0o444)
    code, err = _run(root)
    assert code == 2
    assert AUDIT_REFUSAL in err
    assert _run(root, mode="audit")[0] == 0
