"""CLI wiring for `daisugi orchestrate` (v0.32)."""

from __future__ import annotations

import json

from typer.testing import CliRunner

from opendaisugi.budget import BudgetReport
from opendaisugi.cli import app
from opendaisugi.model_sizer import StepSizing
from opendaisugi.models import ActionPlan, ShellStep, VerificationResult
from opendaisugi.orchestrator import OrchestrationResult
from opendaisugi.run_session import RunSession, RunStatus

runner = CliRunner()


def _fake_result():
    plan = ActionPlan(source="decomposer", task="demo", steps=[ShellStep(id="s1", command="echo hi")])
    session = RunSession(
        id="run_1", envelope_id="e", plan_id=plan.id, status=RunStatus.SUCCEEDED,
        verification=VerificationResult(ok=True, envelope_id="e", plan_id=plan.id, duration_ms=1.0),
        steps=[],
    )
    return OrchestrationResult(
        prompt="demo", plan=plan, session=session,
        final_answer="THE ANSWER",
        sizings=[StepSizing(step_id="s1", difficulty=0.1, tier="local", model="local-model", est_tokens=1200)],
        budget=BudgetReport(total=5000, spent=1200, remaining=3800, step_count=1, by_model={"local-model": 1200}),
        reused_pathway=False, used_llm_synthesis=True,
    )


def test_orchestrate_help():
    res = runner.invoke(app, ["orchestrate", "--help"])
    assert res.exit_code == 0
    assert "orchestrate" in res.output.lower()


def test_orchestrate_renders_answer_and_summary(monkeypatch, tmp_path):
    async def fake_orchestrate(self, prompt, **kwargs):
        return _fake_result()

    import opendaisugi
    monkeypatch.setattr(opendaisugi.Daisugi, "orchestrate", fake_orchestrate, raising=False)
    res = runner.invoke(app, ["orchestrate", "demo", "--data-dir", str(tmp_path), "--budget", "5000"])
    assert res.exit_code == 0, res.output
    assert "THE ANSWER" in res.output
    assert "s1" in res.output  # step summary rendered


def test_orchestrate_json_output(monkeypatch, tmp_path):
    async def fake_orchestrate(self, prompt, **kwargs):
        return _fake_result()

    import opendaisugi
    monkeypatch.setattr(opendaisugi.Daisugi, "orchestrate", fake_orchestrate, raising=False)
    res = runner.invoke(app, ["orchestrate", "demo", "--data-dir", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    payload = json.loads(res.output)
    assert payload["final_answer"] == "THE ANSWER"
    assert payload["status"] == "succeeded"
    assert payload["budget"]["spent"] == 1200
    assert payload["reused_pathway"] is False


def test_orchestrate_rejects_bad_llm_flag(tmp_path):
    res = runner.invoke(app, ["orchestrate", "x", "--llm", "nonsense", "--data-dir", str(tmp_path)])
    assert res.exit_code == 2
    assert "Invalid --llm" in res.output


def test_orchestrate_llm_flag_sets_backend_env(monkeypatch, tmp_path):
    import opendaisugi
    seen = {}

    async def fake_orchestrate(self, prompt, **kwargs):
        import os
        seen["backend"] = os.environ.get("OPENDAISUGI_LLM_BACKEND")
        return _fake_result()

    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.setattr(opendaisugi.Daisugi, "orchestrate", fake_orchestrate, raising=False)
    res = runner.invoke(app, ["orchestrate", "demo", "--llm", "claude-code", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert seen["backend"] == "claude-code"


def test_orchestrate_accepts_budget_and_envelope_optionals(monkeypatch, tmp_path):
    # Optional-typed flags: omitting them must not error on annotation parsing.
    import opendaisugi
    captured = {}

    async def fake_orchestrate(self, prompt, **kwargs):
        captured.update(kwargs)
        return _fake_result()

    monkeypatch.setattr(opendaisugi.Daisugi, "orchestrate", fake_orchestrate, raising=False)
    res = runner.invoke(app, ["orchestrate", "demo", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert captured["budget_tokens"] is None      # omitted → None, no crash
    assert captured["envelope"] is None


def _result_with_measured_cost():
    r = _fake_result()
    from dataclasses import replace
    r.budget = replace(r.budget, measured_cost_usd=0.0207)
    return r


def test_orchestrate_cost_hidden_by_default(monkeypatch, tmp_path):
    import opendaisugi
    monkeypatch.setattr(opendaisugi.Daisugi, "orchestrate",
                        lambda self, prompt, **k: _async(_fake_result()), raising=False)
    res = runner.invoke(app, ["orchestrate", "demo", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "cost:" not in res.output  # costing removed from default output


def test_orchestrate_cost_flag_shows_exact_when_measured(monkeypatch, tmp_path):
    import opendaisugi
    monkeypatch.setattr(opendaisugi.Daisugi, "orchestrate",
                        lambda self, prompt, **k: _async(_result_with_measured_cost()), raising=False)
    res = runner.invoke(app, ["orchestrate", "demo", "--cost", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "cost:" in res.output and "exact" in res.output and "0.0207" in res.output


def test_orchestrate_cost_flag_shows_estimate_without_measured(monkeypatch, tmp_path):
    import opendaisugi
    monkeypatch.setattr(opendaisugi.Daisugi, "orchestrate",
                        lambda self, prompt, **k: _async(_fake_result()), raising=False)
    res = runner.invoke(app, ["orchestrate", "demo", "--cost", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "cost:" in res.output and "estimated" in res.output


async def _async(v):
    return v
