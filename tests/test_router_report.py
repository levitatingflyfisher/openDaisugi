"""The router's weekly measure and its place in `router status`."""

from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi import router_report
from opendaisugi.cli import app


def _turn(created_at: str, **kw) -> dict:
    row = {
        "created_at": created_at,
        "signature": "s",
        "task": "t",
        "tier": "tier1",
        "requested_model": "claude-sonnet-5",
        "model": "claude-haiku-4-5",
        "difficulty": 0.1,
        "downgraded": True,
        "estimated": True,
        "input_tokens": 100,
        "output_tokens": 10,
        "frontier_tokens_saved": 110,
        "actual_dollars": 0.001,
        "counterfactual_dollars": 0.004,
    }
    row.update(kw)
    return row


def _write(path: Path, rows: list) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        "".join((r if isinstance(r, str) else json.dumps(r)) + "\n" for r in rows),
        encoding="utf-8",
    )


def test_weeks_group_turns_delegations_and_grafts(tmp_path):
    _write(
        tmp_path / "gateway" / "turns.jsonl",
        [
            _turn("2026-09-28T10:00:00Z"),
            _turn("2026-09-21T10:00:00Z", downgraded=False, estimated=False),
            "not json",
            _turn("bad time"),
        ],
    )
    _write(
        tmp_path / "router" / "delegations.jsonl",
        [
            {
                "at": "2026-09-29T08:00:00Z",
                "ok": True,
                "quotes": 3,
                "dropped": 1,
                "worker_input_tokens": 900,
                "worker_output_tokens": 50,
                "worker_dollars": 0.0,
                "frontier_tokens_kept": 1000,
                "frontier_dollars_kept": 0.00375,
                "task_ok": None,
            },
            {"at": "2026-09-29T09:00:00Z", "ok": False, "reason": "no worker"},
            {"at": "2026-09-29T09:30:00Z", "ok": True, "worker_dollars": None, "task_ok": True},
        ],
    )
    # 2026-09-30T00:00:00Z
    _write(
        tmp_path / "gate" / "audit" / "s1.jsonl",
        [
            {"at": 1790726400.0, "graft": {"applied": True}},
            {"at": 1790726400.0, "graft": {"applied": False}},
            {"at": 1790726400.0, "allow": True},
        ],
    )
    weeks = router_report.weekly(tmp_path)
    assert [w["week"] for w in weeks] == ["2026-W40", "2026-W39"]
    w40 = weeks[0]
    assert w40["turns"] == 1 and w40["turns_downgraded"] == 1
    assert w40["delegations"] == 3 and w40["delegate_ok"] == 2
    assert w40["quotes"] == 3 and w40["dropped"] == 1
    assert w40["worker_tokens"] == 950
    assert w40["worker_unpriced"] == 1
    assert w40["redirects"] == 1 and w40["not_redirected"] == 1
    assert w40["task_pass"] == 1 and w40["task_unknown"] == 2
    assert w40["tokens_saved"] == 110 + 1000
    assert w40["estimated"] is True
    assert w40["escalations"] == 0
    assert weeks[1]["estimated"] is False
    line = router_report.week_line(w40)
    assert line.startswith("2026-W40: tokens saved 1,110 (estimated)")
    assert "1 unpriced" in line


def test_router_status_shows_weeks_and_delegate(tmp_path, monkeypatch):
    monkeypatch.setenv("PATH", "/nonexistent")
    runner = CliRunner()
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    assert "no turns and no delegations recorded" in result.output
    assert "rule: none" in result.output
    assert "worker: none. no worker: no local model is set up" in result.output
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    body = json.loads(result.output)
    assert body["weeks"] == []
    assert body["escalation_built"] is False
    assert body["delegate"]["rule"] is None
    assert body["delegate"]["worker"] is None


def test_router_status_names_the_rule_and_local_worker(tmp_path, monkeypatch):
    monkeypatch.setenv("PATH", "/nonexistent")
    (tmp_path / "gate" / "grafts").mkdir(parents=True)
    (tmp_path / "gate" / "grafts" / "big-read.json").write_text(
        json.dumps(
            {
                "id": "big-read",
                "version": 1,
                "shape": "deny_redirect",
                "state": "active",
                "match": {"tool": "Read", "file_lines_over": 400},
            }
        )
    )
    (tmp_path / "gate" / "grafts" / "bad.json").write_text("{")
    (tmp_path / "local_tier1.json").write_text(
        json.dumps({"model": "qwen", "base_url": "http://127.0.0.1:8080/v1"})
    )
    result = CliRunner().invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert "rule: big-read v1 (big-read.json), active: reads over 400 lines go to" in result.output
    assert "rule file bad.json is not used: not readable JSON" in result.output
    assert "worker: openai/qwen (local, 127.0.0.1)" in result.output


def test_a_gateway_turn_records_its_time(tmp_path):
    from opendaisugi.gateway_journal import GatewayJournal
    from opendaisugi.gateway_pipeline import Gateway

    gw = Gateway(journal=GatewayJournal(path=tmp_path / "turns.jsonl"))
    body = {"model": "claude-sonnet-5", "messages": [{"role": "user", "content": "hi"}]}
    prepared = gw.prepare(body)
    assert prepared.started is not None
    _, record = gw.finish(prepared, {"input_tokens": 1, "output_tokens": 1})
    assert isinstance(record.elapsed_ms, float) and record.elapsed_ms >= 0.0
    row = json.loads((tmp_path / "turns.jsonl").read_text())
    assert list(row)[-1] == "elapsed_ms"
