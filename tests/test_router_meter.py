"""The promotion meter (GR-6, GR-7): a graft trial split per session, the
operator's labels, each session's billed cost from its transcript, and what
promotion would do. Nothing is promoted automatically."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi import delegate, owner_rule, router_report
from opendaisugi.cli import app
from opendaisugi.gate import GateDecision, _maybe_graft, register_envelope
from opendaisugi.models import Envelope

RULE = {
    "id": "big-read",
    "version": 1,
    "shape": "deny_redirect",
    "state": "trial",
    "match": {"tool": "Read", "file_lines_over": 3},
    "trial": {"seed": 7},
}


# --- labels ----------------------------------------------------------------


def test_label_writes_a_row_and_the_last_label_wins(tmp_path):
    r = CliRunner().invoke(
        app, ["router", "label", "s1", "pass", "--data-dir", str(tmp_path), "--note", "tests green"]
    )
    assert r.exit_code == 0, r.output
    assert r.output == "labeled s1: pass\n"
    r = CliRunner().invoke(
        app, ["router", "label", "s1", "fail", "--data-dir", str(tmp_path), "--json"]
    )
    row = json.loads(r.output)
    assert row["session"] == "s1" and row["outcome"] == "fail" and row["note"] is None
    path = tmp_path / "router" / "labels.jsonl"
    assert oct(os.stat(path).st_mode & 0o777) == "0o600"
    assert router_report.read_labels(tmp_path) == {"s1": "fail"}


@pytest.mark.parametrize(
    "args,err",
    [
        (["s1", "maybe"], "Error: OUTCOME must be pass or fail."),
        (["a/b", "pass"], "Error: SESSION must be a session id as the gate names it"),
        ([".s", "pass"], "Error: SESSION must be a session id as the gate names it"),
        (["s1", "pass", "--note", "x" * 501], "Error: --note must be at most 500 characters."),
    ],
)
def test_label_refuses_what_it_cannot_record(tmp_path, args, err):
    r = CliRunner().invoke(app, ["router", "label", *args, "--data-dir", str(tmp_path)])
    assert r.exit_code == 2
    assert err in r.output
    assert not (tmp_path / "router" / "labels.jsonl").exists()


def test_read_labels_skips_bad_rows(tmp_path):
    p = tmp_path / "router" / "labels.jsonl"
    p.parent.mkdir()
    p.write_text(
        "\n".join(
            [
                json.dumps({"session": "a", "outcome": "pass"}),
                "not json",
                json.dumps({"session": "b/c", "outcome": "pass"}),
                json.dumps({"session": "d", "outcome": "maybe"}),
                json.dumps(["x"]),
                json.dumps({"session": "a", "outcome": "fail"}),
                json.dumps({"session": "e", "outcome": "pass"}),
            ]
        )
        + "\n"
    )
    assert router_report.read_labels(tmp_path) == {"a": "fail", "e": "pass"}


def test_the_gate_denies_an_agent_running_router_label():
    assert owner_rule.runs_verb("daisugi router label s1 pass", owner_rule.LABEL_VERBS)
    assert not owner_rule.runs_verb("daisugi router status", owner_rule.LABEL_VERBS)


# --- arms -------------------------------------------------------------------


def test_the_arm_is_a_seeded_hash_of_the_session():
    rule = delegate.parse_rule(RULE, "r.json")
    for s in ("s1", "s2", "abc", "no-session"):
        h = int(hashlib.sha256(f"7:big-read:1:{s}".encode()).hexdigest()[:8], 16)
        assert delegate.arm_of(rule, s) == ("graft" if h % 2 == 0 else "control")


@pytest.mark.parametrize("trial", [[], {"seed": -1}, {"seed": True}, {"seed": 2**53 + 1}, {"x": 1}])
def test_a_bad_trial_block_makes_a_bad_rule(trial):
    got = delegate.parse_rule({**RULE, "trial": trial}, "r.json")
    assert got == "trial must be an object with an integer seed from 0 to 2**53"


def test_trial_acts_and_the_seed_defaults_to_zero():
    rule = delegate.parse_rule({k: v for k, v in RULE.items() if k != "trial"}, "r.json")
    assert rule.seed == 0 and rule.state in delegate.RULE_STATES_ACTING


def _graft_root(tmp_path: Path) -> Path:
    root = tmp_path / "gate"
    (root / "grafts").mkdir(parents=True)
    (root / "grafts" / "r.json").write_text(json.dumps(RULE))
    (tmp_path / "local_tier1.json").write_text(
        json.dumps({"model": "qwen", "base_url": "http://127.0.0.1:9/v1"})
    )
    register_envelope(
        Envelope(
            generated_by="t",
            task="t",
            permissions={"file_read": ["/**"], "mcp_allowlist": ["opendaisugi/*"]},
        ),
        root=root,
    )
    return root


def _session_in(rule, arm: str) -> str:
    for i in range(100):
        if delegate.arm_of(rule, f"s{i}") == arm:
            return f"s{i}"
    raise AssertionError


@pytest.mark.parametrize("arm", ["graft", "control"])
def test_a_trial_redirects_only_the_graft_arm(tmp_path, arm):
    root = _graft_root(tmp_path)
    big = tmp_path / "big.py"
    big.write_text("a\nb\nc\nd\ne\n")
    rule = delegate.acting_rule(root)
    session = _session_in(rule, arm)
    env = Envelope.model_validate_json((root / "envelopes" / "default.json").read_text())
    allowed = GateDecision(
        allow=True, would_deny=False, reason="ok", mode="enforce", step_type="file_read"
    )
    payload = {"tool_name": "Read", "tool_input": {"file_path": str(big)}, "session_id": session}
    got = _maybe_graft(root, payload, allowed, env, "claude", session_id=session)
    assert got.graft["arm"] == arm
    if arm == "graft":
        assert got.graft["applied"] is True and not got.allow
    else:
        assert got.graft["applied"] is False and got.allow
        assert (
            got.graft["why"] == "the session is in the trial's control arm: the read is not denied"
        )


# --- the cost of a session --------------------------------------------------


def _assistant(mid, model, i=0, o=0, cr=0, cw=0) -> str:
    msg = {
        "model": model,
        "usage": {
            "input_tokens": i,
            "output_tokens": o,
            "cache_read_input_tokens": cr,
            "cache_creation_input_tokens": cw,
        },
    }
    if mid is not None:
        msg["id"] = mid
    return json.dumps({"type": "assistant", "message": msg})


def test_a_transcript_is_priced_once_per_message(tmp_path):
    t = tmp_path / "t.jsonl"
    t.write_text(
        "\n".join(
            [
                json.dumps({"type": "user", "message": {"content": "hi"}}),
                _assistant("m1", "claude-sonnet-5", i=10, o=1),
                _assistant("m1", "claude-sonnet-5", i=10, o=50, cr=1000, cw=100),
                _assistant(None, "claude-haiku-4-5", i=20, o=20),
                "not json",
                _assistant("m2", "claude-haiku-4-5", i=True, o=2.5),
            ]
        )
        + "\n"
    )
    got = router_report.session_cost(str(t))
    want = (10 * 3 + 1000 * 3 * 0.1 + 100 * 3 * 1.25 + 50 * 15) / 1e6 + (20 * 1 + 20 * 5) / 1e6
    assert got == {"dollars": want, "quota_tokens": 10 + 50 + 1000 + 100 + 40, "estimated": False}


def test_an_unknown_model_is_priced_at_the_fallback_and_marked(tmp_path):
    t = tmp_path / "t.jsonl"
    t.write_text(_assistant("m1", "claude-new-9", i=1000) + "\n")
    got = router_report.session_cost(str(t))
    assert got["estimated"] is True and got["dollars"] == 1000 * 3.0 / 1e6


@pytest.mark.parametrize("bad", ["relative.jsonl", None, 5])
def test_a_path_that_is_not_absolute_has_no_cost(bad):
    assert router_report.session_cost(bad) is None


def test_a_fifo_or_a_directory_has_no_cost(tmp_path):
    fifo = tmp_path / "f"
    os.mkfifo(fifo)
    assert router_report.session_cost(str(fifo)) is None
    assert router_report.session_cost(str(tmp_path)) is None
    assert router_report.session_cost(str(tmp_path / "missing")) is None


# --- the trial table ----------------------------------------------------------


def _audit(tmp_path: Path, session: str, arm: str, transcript: str | None) -> None:
    rec = {
        "at": 1.0,
        "session_id": session,
        "tool_name": "Read",
        "graft": {
            "rule_id": "big-read",
            "version": 1,
            "shape": "deny_redirect",
            "state": "trial",
            "arm": arm,
            "applied": arm == "graft",
        },
    }
    if transcript:
        rec["transcript_path"] = transcript
    p = tmp_path / "gate" / "audit" / f"{session}.jsonl"
    p.parent.mkdir(parents=True, exist_ok=True)
    with p.open("a") as f:
        f.write(json.dumps(rec) + "\n")


def _label(tmp_path: Path, session: str, outcome: str) -> None:
    p = tmp_path / "router" / "labels.jsonl"
    p.parent.mkdir(parents=True, exist_ok=True)
    with p.open("a") as f:
        f.write(json.dumps({"session": session, "outcome": outcome}) + "\n")


def _trial_dir(tmp_path: Path, rule: dict | None = None) -> None:
    (tmp_path / "gate" / "grafts").mkdir(parents=True, exist_ok=True)
    (tmp_path / "gate" / "grafts" / "r.json").write_text(json.dumps(rule or RULE))


def _session(tmp_path, name, arm, outcome, dollars_in):
    t = tmp_path / "tx" / f"{name}.jsonl"
    t.parent.mkdir(exist_ok=True)
    t.write_text(_assistant("m", "claude-sonnet-5", i=dollars_in) + "\n")
    _audit(tmp_path, name, arm, str(t))
    if outcome:
        _label(tmp_path, name, outcome)


def test_promote_when_the_graft_arm_costs_less_per_success(tmp_path):
    _trial_dir(tmp_path)
    for i in range(3):
        _session(tmp_path, f"g{i}", "graft", "pass", 1000)
        _session(tmp_path, f"c{i}", "control", "pass", 2000)
    _session(tmp_path, "g9", "graft", None, 5)
    t = router_report.trial_state(tmp_path)
    g, c = t["arms"]["graft"], t["arms"]["control"]
    assert g["sessions"] == 4 and g["labeled"] == 3 and g["unlabeled"] == 1 and g["passed"] == 3
    assert g["dollars_per_success"] == pytest.approx(0.003) and c[
        "dollars_per_success"
    ] == pytest.approx(0.006)
    assert t["verdict"] == "promote"
    assert "set rule big-read to active" in t["verdict_text"]
    lines = router_report.trial_lines(t)
    assert lines[0].startswith("trial (rule big-read v1, trial, seed 7)")
    assert any(ln.startswith("  promotion would: promote") for ln in lines)


def test_retire_when_success_falls_and_wait_when_too_few(tmp_path):
    _trial_dir(tmp_path)
    for i in range(3):
        _session(tmp_path, f"g{i}", "graft", "fail" if i else "pass", 10)
        _session(tmp_path, f"c{i}", "control", "pass", 2000)
    assert router_report.trial_state(tmp_path)["verdict"] == "retire"
    _label(tmp_path, "g1", "pass")
    _label(tmp_path, "g2", "pass")
    assert router_report.trial_state(tmp_path)["verdict"] == "promote"
    (tmp_path / "router" / "labels.jsonl").unlink()
    t = router_report.trial_state(tmp_path)
    assert t["verdict"] == "wait" and t["arms"]["graft"]["success_rate"] is None


def test_a_session_in_both_arms_is_left_out(tmp_path):
    _trial_dir(tmp_path)
    _audit(tmp_path, "x", "graft", None)
    _audit(tmp_path, "x", "control", None)
    t = router_report.trial_state(tmp_path)
    assert t["conflicting"] == 1 and t["arms"]["graft"]["sessions"] == 0


def test_an_unknown_cost_blocks_the_cost_verdict(tmp_path):
    _trial_dir(tmp_path)
    for i in range(3):
        _session(tmp_path, f"g{i}", "graft", "pass", 10)
        _session(tmp_path, f"c{i}", "control", "pass", 10)
    _audit(tmp_path, "g3", "graft", None)
    _label(tmp_path, "g3", "pass")
    t = router_report.trial_state(tmp_path)
    assert t["arms"]["graft"]["cost_unknown"] == 1
    assert t["arms"]["graft"]["dollars_per_success"] is None
    assert t["verdict"] == "wait"


def test_no_trial_for_an_active_rule_and_none_for_audit(tmp_path):
    _trial_dir(tmp_path, {**RULE, "state": "active"})
    assert router_report.trial_state(tmp_path) is None
    _trial_dir(tmp_path, {**RULE, "state": "audit"})
    assert router_report.trial_state(tmp_path)["verdict"] == "none"


def test_a_remote_worker_marks_the_graft_arm_estimated(tmp_path):
    _trial_dir(tmp_path, {**RULE, "worker": {"allow_remote": True}})
    _session(tmp_path, "g0", "graft", "pass", 10)
    t = router_report.trial_state(tmp_path)
    assert t["arms"]["graft"]["estimated"] is True and t["arms"]["control"]["estimated"] is False


def test_router_status_shows_the_trial(tmp_path):
    _trial_dir(tmp_path)
    r = CliRunner().invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    assert r.exit_code == 0, r.output
    assert json.loads(r.output)["trial"]["verdict"] == "wait"
    r = CliRunner().invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert "promotion would: wait" in r.output


def test_a_code_write_row_does_not_mark_the_week_estimated(tmp_path):
    p = tmp_path / "router" / "delegations.jsonl"
    p.parent.mkdir()
    p.write_text(json.dumps({"at": "2026-09-29T08:00:00Z", "ok": True, "estimated": False}) + "\n")
    assert router_report.weekly(tmp_path)[0]["estimated"] is False
