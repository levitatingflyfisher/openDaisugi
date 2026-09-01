"""`daisugi status` counts what the dialect's words would deny in audit.

Calls are counted in the gate's audit log (``gate.word_would_deny``);
plans in the journal (``journal.word_would_deny``): traces whose
verification result holds a ``dialect audit:`` warning. So the audit
measurement covers plans as well as calls.
"""

from __future__ import annotations

import json
from pathlib import Path

import yaml
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.journal import Journal, word_would_deny
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.onboarding import gather_status
from opendaisugi.verify import verify

runner = CliRunner()


def _env(*invariants) -> Envelope:
    return Envelope(
        task="t",
        generated_by="test",
        permissions={
            "shell": True,
            "shell_allowlist": ["echo"],
            "file_write": ["**"],
            "shell_allow_decomposition": True,
        },
        invariants=list(invariants),
    )


def _log(data_dir: Path, command: str, invariants) -> None:
    plan = ActionPlan(
        task="t", source="test", steps=[{"id": "s1", "type": "shell", "command": command}]
    )
    env = _env(*invariants)
    j = Journal(data_dir=data_dir)
    try:
        j.log(task="t", envelope=env, plan=plan, result=verify(plan, env))
    finally:
        j.close()


def _trace(data_dir: Path, name: str, text: str) -> None:
    d = data_dir / "journal" / "traces"
    d.mkdir(parents=True, exist_ok=True)
    (d / name).write_text(text, encoding="utf-8")


def test_a_missing_journal_counts_nothing(tmp_path: Path):
    assert word_would_deny(tmp_path) == 0
    assert not (tmp_path / "journal").exists()


def test_the_plans_a_word_would_deny_are_counted(tmp_path: Path):
    ro = {"type": "read_only", "description": "d", "target": "src/**"}
    _log(tmp_path, "echo x > src/a.py", [ro])
    _log(tmp_path, "echo x > out/a.py", [ro])
    _log(tmp_path, "echo x > src/b.py", [ro, {**ro, "type": "file_unchanged"}])
    _log(tmp_path, "echo x > src/c.py", [])
    assert word_would_deny(tmp_path) == 2


def test_a_trace_it_cannot_read_is_skipped(tmp_path: Path):
    ok = yaml.safe_dump({"result": {"warnings": ["dialect audit: invariant 'x' is y"]}})
    _trace(tmp_path, "a.yaml", ok)
    _trace(tmp_path, "b.yaml", "result: [dialect\n")
    _trace(tmp_path, "c.yaml", "result:\n  warnings: dialect audit: x\n")
    _trace(tmp_path, "d.yaml", "- dialect\n")
    (tmp_path / "journal" / "traces" / "e.yaml").write_bytes(b"\xff dialect")
    _trace(tmp_path, "f.txt", ok)
    assert word_would_deny(tmp_path) == 1


def test_status_shows_both_counts(tmp_path: Path):
    ro = {"type": "read_only", "description": "d", "target": "src/**"}
    _log(tmp_path, "echo x > src/a.py", [ro])
    audit = tmp_path / "gate" / "audit"
    audit.mkdir(parents=True)
    (audit / "s.jsonl").write_text(
        json.dumps({"word_audit": ["dialect audit: x"]}) + "\n" + json.dumps({}) + "\n"
    )
    rep = gather_status(tmp_path)
    assert (rep.word_would_deny_plans, rep.word_would_deny_calls) == (1, 1)
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert (
        "  • dialect would-denies: 1 plans in the journal, 1 calls in the gate's audit log\n"
        in res.output
    )
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path), "--json"])
    got = json.loads(res.output)
    assert (got["word_would_deny_plans"], got["word_would_deny_calls"]) == (1, 1)
