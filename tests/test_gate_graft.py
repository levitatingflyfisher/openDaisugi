"""The gate's deny_redirect graft for large reads, and its check of the
delegate MCP call."""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi import gate
from opendaisugi.models import Envelope

RULE = {
    "id": "big-read",
    "version": 1,
    "shape": "deny_redirect",
    "state": "active",
    "match": {"tool": "Read", "file_lines_over": 5},
}


def _setup(tmp_path: Path, *, rule=RULE, tier1=True, lines=10, **perms) -> tuple[Path, Path]:
    data = tmp_path / "data"
    root = data / "gate"
    work = tmp_path / "work"
    work.mkdir(parents=True)
    f = work / "big.py"
    f.write_text("".join(f"x{i}\n" for i in range(lines)))
    p = {
        "file_read": [f"{work}/**"],
        "mcp_allowlist": ["opendaisugi/delegate"],
        **perms,
    }
    stakes = p.pop("stakes", "medium")
    gate.register_envelope(
        Envelope(generated_by="t", task="t", stakes=stakes, permissions=p), root=root
    )
    if rule is not None:
        (root / "grafts").mkdir()
        (root / "grafts" / "big-read.json").write_text(json.dumps(rule))
    if tier1:
        (data / "local_tier1.json").write_text(
            json.dumps({"model": "qwen", "base_url": "http://127.0.0.1:9/v1"})
        )
    return root, f


def _read(path: str, **inp) -> bytes:
    return json.dumps(
        {"session_id": "s1", "tool_name": "Read", "tool_input": {"file_path": path, **inp}}
    ).encode()


def _delegate(path, cwd="/") -> bytes:
    return json.dumps(
        {
            "session_id": "s1",
            "tool_name": "mcp__opendaisugi__delegate",
            "tool_input": {"path": path, "question": "q"},
            "cwd": cwd,
        }
    ).encode()


def _last_audit(root: Path) -> dict:
    lines = (root / "audit" / "s1.jsonl").read_text().splitlines()
    return json.loads(lines[-1])


def test_active_rule_redirects_a_large_read_in_enforce_mode(tmp_path):
    root, f = _setup(tmp_path)
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="enforce")
    assert out.exit_code == 2
    assert "mcp__opendaisugi__delegate" in out.stderr
    assert "10 lines, over the 5-line threshold" in out.stderr
    rec = _last_audit(root)
    assert rec["allow"] is False and rec["would_deny"] is False
    assert rec["graft"]["applied"] is True and rec["graft"]["rule_id"] == "big-read"
    assert rec["graft"]["worker"] == {"model": "openai/qwen", "tier": "local", "host": "127.0.0.1"}


def test_audit_mode_gate_only_records_an_active_rule(tmp_path):
    # RP-4: a cost deny never surprises an audit-mode user.
    root, f = _setup(tmp_path)
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="audit")
    assert out.exit_code == 0 and out.stderr == ""
    rec = _last_audit(root)
    assert rec["allow"] is True and rec["would_deny"] is False
    g = rec["graft"]
    assert g["applied"] is False
    assert g["why"] == "the gate is in audit mode: the read is not denied"
    assert g["worker"] == {"model": "openai/qwen", "tier": "local", "host": "127.0.0.1"}


def test_small_file_and_small_limit_pass(tmp_path):
    root, f = _setup(tmp_path, lines=5)
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="enforce")
    assert out.exit_code == 0 and "graft" not in _last_audit(root)
    root, f = _setup(tmp_path / "b")
    out = gate.gate_and_contract(_read(str(f), limit=5), root=root, mode="enforce")
    assert out.exit_code == 0 and "graft" not in _last_audit(root)
    out = gate.gate_and_contract(_read(str(f), limit=6), root=root, mode="enforce")
    assert out.exit_code == 2


def test_no_rule_no_change(tmp_path):
    root, f = _setup(tmp_path, rule=None)
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="enforce")
    assert out.exit_code == 0 and "graft" not in _last_audit(root)


def test_no_worker_lets_the_read_through_and_records_why(tmp_path):
    root, f = _setup(tmp_path, tier1=False)
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="enforce")
    assert out.exit_code == 0 and out.stderr == ""
    g = _last_audit(root)["graft"]
    assert g["applied"] is False and g["why"].startswith("no worker")


def test_audit_rule_only_records(tmp_path):
    root, f = _setup(tmp_path, rule={**RULE, "state": "audit"})
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="enforce")
    assert out.exit_code == 0
    g = _last_audit(root)["graft"]
    assert g["applied"] is False and "audit" in g["why"]


def test_a_denied_read_is_never_grafted(tmp_path):
    root, _ = _setup(tmp_path)
    other = tmp_path / "other.py"
    other.write_text("a\n" * 50)
    out = gate.gate_and_contract(_read(str(other)), root=root, mode="enforce")
    assert out.exit_code == 2
    rec = _last_audit(root)
    assert rec["would_deny"] is True and "graft" not in rec


def test_pi_format_is_not_grafted(tmp_path):
    root, f = _setup(tmp_path)
    raw = json.dumps(
        {"session_id": "s1", "tool_name": "read", "tool_input": {"path": str(f)}}
    ).encode()
    out = gate.gate_and_contract(raw, root=root, mode="enforce", fmt="pi")
    assert out.exit_code == 0


def test_delegate_call_checks_the_read(tmp_path):
    root, f = _setup(tmp_path)
    out = gate.gate_and_contract(_delegate(str(f)), root=root, mode="enforce")
    assert out.exit_code == 0, out.stderr
    out = gate.gate_and_contract(_delegate("/etc/passwd"), root=root, mode="enforce")
    assert out.exit_code == 2 and "delegate reads /etc/passwd" in out.stderr
    out = gate.gate_and_contract(
        _delegate(f"{f.parent}/../../etc/passwd"), root=root, mode="enforce"
    )
    assert out.exit_code == 2
    out = gate.gate_and_contract(_delegate("big.py", cwd=str(f.parent)), root=root, mode="enforce")
    assert out.exit_code == 2 and "absolute path" in out.stderr
    out = gate.gate_and_contract(_delegate(None), root=root, mode="enforce")
    assert out.exit_code == 2


def test_delegate_call_to_a_remote_worker_needs_the_network(tmp_path):
    rule = {**RULE, "worker": {"allow_remote": True}}
    root, f = _setup(tmp_path, rule=rule)
    (root.parent / "local_tier1.json").write_text(
        json.dumps({"model": "qwen", "base_url": "http://worker.invalid:8080/v1"})
    )
    # No grant: the route has no worker, so the tool will refuse; the call
    # itself only reads, which the envelope allows.
    out = gate.gate_and_contract(_delegate(str(f)), root=root, mode="enforce")
    assert out.exit_code == 0
    root2, f2 = _setup(tmp_path / "g", rule=rule, network=True, network_hosts=["worker.invalid"])
    (root2.parent / "local_tier1.json").write_text(
        json.dumps({"model": "qwen", "base_url": "http://worker.invalid:8080/v1"})
    )
    out = gate.gate_and_contract(_delegate(str(f2)), root=root2, mode="enforce")
    assert out.exit_code == 0, out.stderr


def test_delegate_call_under_physical_stakes(tmp_path):
    root, f = _setup(tmp_path, stakes="physical")
    out = gate.gate_and_contract(_delegate(str(f)), root=root, mode="enforce")
    assert out.exit_code == 2


def test_no_redirect_when_the_envelope_would_deny_the_delegate(tmp_path):
    root, f = _setup(tmp_path, mcp_allowlist=[])
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="enforce")
    assert out.exit_code == 0
    g = _last_audit(root)["graft"]
    assert g["applied"] is False and "would not allow the delegate tool" in g["why"]


def test_only_the_claude_format_redirects(tmp_path):
    root, f = _setup(tmp_path)
    out = gate.gate_and_contract(_read(str(f)), root=root, mode="enforce", fmt="opencode")
    assert out.exit_code == 0
