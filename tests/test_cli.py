"""Tests for the opendaisugi Typer CLI."""

from pathlib import Path
from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_cli_root_help_runs_and_lists_journal_subapp():
    result = runner.invoke(app, ["--help"])
    assert result.exit_code == 0
    # The journal sub-app is the only thing wired at this stage.
    assert "journal" in result.stdout


import json
from unittest.mock import AsyncMock, patch

import yaml

from opendaisugi.models import Envelope, Permission


def _fake_envelope() -> Envelope:
    return Envelope(
        id="env_cli0001",
        generated_by="test",
        task="cli demo",
        permissions=Permission(shell=False, file_read=["/tmp/demo.csv"]),
    )


def test_generate_envelope_prints_yaml_by_default(tmp_path):
    fake = _fake_envelope()
    with patch(
        "opendaisugi.cli.generate_envelope",
        new=AsyncMock(return_value=fake),
    ):
        result = runner.invoke(
            app, ["generate-envelope", "Read /tmp/demo.csv", "--data-dir", str(tmp_path)]
        )
    assert result.exit_code == 0
    loaded = yaml.safe_load(result.stdout)
    assert loaded["id"] == "env_cli0001"
    assert loaded["permissions"]["file_read"] == ["/tmp/demo.csv"]


def test_generate_envelope_prints_json_with_flag(tmp_path):
    fake = _fake_envelope()
    with patch(
        "opendaisugi.cli.generate_envelope",
        new=AsyncMock(return_value=fake),
    ):
        result = runner.invoke(
            app,
            [
                "generate-envelope", "Read /tmp/demo.csv",
                "--data-dir", str(tmp_path),
                "--json",
            ],
        )
    assert result.exit_code == 0
    loaded = json.loads(result.stdout)
    assert loaded["id"] == "env_cli0001"


def test_generate_envelope_passes_model_through(tmp_path):
    captured = {}
    async def fake(task, *, model, **kwargs):
        captured["model"] = model
        captured["task"] = task
        return _fake_envelope()
    with patch("opendaisugi.cli.generate_envelope", side_effect=fake):
        result = runner.invoke(
            app,
            [
                "generate-envelope", "t",
                "--model", "openai/gpt-4o-mini",
                "--data-dir", str(tmp_path),
            ],
        )
    assert result.exit_code == 0
    assert captured["model"] == "openai/gpt-4o-mini"
    assert captured["task"] == "t"


def _write_yaml(path: Path, obj) -> None:
    path.write_text(yaml.safe_dump(obj.model_dump(mode="json"), sort_keys=False))


def test_verify_ok_plan_exits_zero(tmp_path):
    from opendaisugi.models import ActionPlan, ShellStep, Envelope, Permission

    env = Envelope(
        id="env_ok", generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(
        id="plan_ok", source="t", task="t",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    env_path = tmp_path / "env.yaml"
    plan_path = tmp_path / "plan.yaml"
    _write_yaml(env_path, env)
    _write_yaml(plan_path, plan)

    result = runner.invoke(
        app, ["verify", str(plan_path), "--envelope", str(env_path)]
    )
    assert result.exit_code == 0
    assert "OK" in result.stdout.upper()


def test_verify_failing_plan_exits_one(tmp_path):
    from opendaisugi.models import ActionPlan, ShellStep, Envelope, Permission

    env = Envelope(
        id="env_f", generated_by="t", task="t",
        permissions=Permission(shell=False),  # no shell
    )
    plan = ActionPlan(
        id="plan_f", source="t", task="t",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    env_path = tmp_path / "env.yaml"
    plan_path = tmp_path / "plan.yaml"
    _write_yaml(env_path, env)
    _write_yaml(plan_path, plan)

    result = runner.invoke(
        app, ["verify", str(plan_path), "--envelope", str(env_path)]
    )
    assert result.exit_code == 1
    assert "violation" in result.stdout.lower()


def test_verify_json_output(tmp_path):
    from opendaisugi.models import ActionPlan, ShellStep, Envelope, Permission

    env = Envelope(
        id="env_j", generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(
        id="plan_j", source="t", task="t",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    env_path = tmp_path / "env.yaml"
    plan_path = tmp_path / "plan.yaml"
    _write_yaml(env_path, env)
    _write_yaml(plan_path, plan)

    result = runner.invoke(
        app, ["verify", str(plan_path), "--envelope", str(env_path), "--json"]
    )
    assert result.exit_code == 0
    payload = json.loads(result.stdout)
    assert payload["ok"] is True
    assert payload["plan_id"] == "plan_j"
    assert payload["envelope_id"] == "env_j"


import sys

from opendaisugi.journal import Journal


def test_journal_stats_empty(tmp_path):
    # Fresh data dir, no traces.
    result = runner.invoke(app, ["journal", "stats", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert "total: 0" in result.stdout.lower()


def test_journal_stats_with_traces(tmp_path):
    from opendaisugi.models import (
        ActionPlan, ShellStep, Envelope, Permission, VerificationResult,
    )
    j = Journal(data_dir=tmp_path)
    env = Envelope(id="e", generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(id="p", source="t", task="t",
                      steps=[ShellStep(id="s1", command="echo hi")])
    for ok in [True, True, False]:
        j.log(
            task="t", envelope=env, plan=plan,
            result=VerificationResult(
                ok=ok, violations=[], warnings=[],
                envelope_id="e", plan_id="p", duration_ms=2.0,
            ),
        )

    result = runner.invoke(app, ["journal", "stats", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    text = result.stdout.lower()
    assert "total: 3" in text
    assert "passed: 2" in text
    assert "failed: 1" in text


def test_journal_stats_json_output(tmp_path):
    result = runner.invoke(
        app, ["journal", "stats", "--data-dir", str(tmp_path), "--json"]
    )
    assert result.exit_code == 0
    payload = json.loads(result.stdout)
    assert payload == {
        "total": 0, "passed": 0, "failed": 0, "avg_duration_ms": 0.0,
    }


def test_journal_search_error_when_extra_missing(tmp_path, monkeypatch):
    # Simulate missing [search] extra.
    monkeypatch.setitem(sys.modules, "opendaisugi._search", None)
    result = runner.invoke(
        app, ["journal", "search", "csv", "--data-dir", str(tmp_path)]
    )
    # Helpful error message, nonzero exit
    assert result.exit_code != 0
    assert "uv add" in result.stdout.lower() or "uv add" in (result.stderr or "").lower() or "pip install" in result.stdout.lower() or "pip install" in (result.stderr or "").lower()


def test_journal_search_dispatches_to_semantic_search(tmp_path, monkeypatch):
    import types
    from opendaisugi.models import Trace
    fake_module = types.ModuleType("opendaisugi._search")
    def fake_semantic_search(journal, query, *, limit):
        return [
            Trace(
                id="2026-04-09-aaaaaaa0", created_at="2026-04-09T10:00:00Z",
                task="read csv data", plan_id="p", envelope_id="e",
                ok=True, duration_ms=1.0, violations=[],
            ),
        ]
    fake_module.semantic_search = fake_semantic_search
    monkeypatch.setitem(sys.modules, "opendaisugi._search", fake_module)

    result = runner.invoke(
        app, ["journal", "search", "csv", "--data-dir", str(tmp_path), "--limit", "3"]
    )
    assert result.exit_code == 0
    assert "read csv data" in result.stdout
    assert "2026-04-09-aaaaaaa0" in result.stdout


def test_journal_replay_no_drift(tmp_path):
    from opendaisugi.models import (
        ActionPlan, ShellStep, Envelope, Permission, VerificationResult,
    )
    j = Journal(data_dir=tmp_path)
    env = Envelope(id="e", generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(id="p", source="t", task="t",
                      steps=[ShellStep(id="s1", command="echo hi")])
    j.log(
        task="t", envelope=env, plan=plan,
        result=VerificationResult(
            ok=True, violations=[], warnings=[],
            envelope_id="e", plan_id="p", duration_ms=1.0,
        ),
        trace_id="2026-04-09-replay00",
        created_at="2026-04-09T10:00:00Z",
    )
    result = runner.invoke(
        app,
        ["journal", "replay", "2026-04-09-replay00", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0
    assert "no drift" in result.stdout.lower()


def test_journal_replay_drift_exits_one(tmp_path, monkeypatch):
    from opendaisugi.models import (
        ActionPlan, ShellStep, Envelope, Permission, VerificationResult,
    )
    j = Journal(data_dir=tmp_path)
    env = Envelope(id="e", generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(id="p", source="t", task="t",
                      steps=[ShellStep(id="s1", command="echo hi")])
    j.log(
        task="t", envelope=env, plan=plan,
        result=VerificationResult(
            ok=True, violations=[], warnings=[],
            envelope_id="e", plan_id="p", duration_ms=1.0,
        ),
        trace_id="2026-04-09-driftcli",
        created_at="2026-04-09T10:00:00Z",
    )
    # Simulate verification drift
    from opendaisugi import journal as journal_module
    def fake_verify(plan, envelope, *, z3_timeout_ms=500):
        return VerificationResult(
            ok=False, violations=[], warnings=["drift"],
            envelope_id=envelope.id, plan_id=plan.id, duration_ms=0.1,
        )
    monkeypatch.setattr(journal_module, "verify", fake_verify)

    result = runner.invoke(
        app,
        ["journal", "replay", "2026-04-09-driftcli", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 1
    assert "drift" in result.stdout.lower()


def test_journal_replay_missing_trace_exits_two(tmp_path):
    result = runner.invoke(
        app,
        ["journal", "replay", "2026-04-09-nosuch0", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 2
    assert "not found" in result.stdout.lower() or "no trace" in result.stdout.lower()


# --- Additional branch-coverage tests ---


def test_verify_prints_warnings(tmp_path, monkeypatch):
    """Cover the result.warnings branch in verify_cmd (lines 106-108)."""
    from opendaisugi.models import ActionPlan, ShellStep, Envelope, Permission
    from opendaisugi.exceptions import VerificationTimeout

    env = Envelope(
        id="env_w", generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(id="plan_w", source="t", task="t",
                      steps=[ShellStep(id="s1", command="echo hi")])
    env_path = tmp_path / "env.yaml"
    plan_path = tmp_path / "plan.yaml"
    _write_yaml(env_path, env)
    _write_yaml(plan_path, plan)

    # Force a Z3 timeout so verify() adds a warning
    import sys as _sys
    import opendaisugi.verify
    verify_mod = _sys.modules["opendaisugi.verify"]

    def _raise_timeout(*a, **kw):
        raise VerificationTimeout("z3 timed out")

    monkeypatch.setattr(verify_mod, "check_envelope_self_consistency", _raise_timeout)

    result = runner.invoke(
        app, ["verify", str(plan_path), "--envelope", str(env_path)]
    )
    assert result.exit_code == 0
    assert "warning" in result.stdout.lower()


def test_journal_search_json_output(tmp_path, monkeypatch):
    """Cover the --json branch of journal_search_cmd (lines 134-136)."""
    import types
    from opendaisugi.models import Trace

    fake_module = types.ModuleType("opendaisugi._search")

    def fake_semantic_search(journal, query, *, limit):
        return [
            Trace(
                id="2026-04-09-searchjsn", created_at="2026-04-09T10:00:00Z",
                task="json task", plan_id="p", envelope_id="e",
                ok=True, duration_ms=1.0, violations=[],
            ),
        ]

    fake_module.semantic_search = fake_semantic_search
    monkeypatch.setitem(sys.modules, "opendaisugi._search", fake_module)

    result = runner.invoke(
        app,
        ["journal", "search", "json task", "--data-dir", str(tmp_path), "--json"],
    )
    assert result.exit_code == 0
    payload = json.loads(result.stdout)
    assert isinstance(payload, list)
    assert payload[0]["id"] == "2026-04-09-searchjsn"


def test_journal_search_no_results(tmp_path, monkeypatch):
    """Cover the empty-results branch of journal_search_cmd (lines 139-140)."""
    import types

    fake_module = types.ModuleType("opendaisugi._search")
    fake_module.semantic_search = lambda journal, query, *, limit: []
    monkeypatch.setitem(sys.modules, "opendaisugi._search", fake_module)

    result = runner.invoke(
        app, ["journal", "search", "nothing", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert "no matching" in result.stdout.lower()


def test_journal_replay_json_output(tmp_path):
    """Cover the --json branch of journal_replay_cmd (lines 166-174)."""
    from opendaisugi.models import (
        ActionPlan, ShellStep, Envelope, Permission, VerificationResult,
    )

    j = Journal(data_dir=tmp_path)
    env = Envelope(id="e", generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(id="p", source="t", task="t",
                      steps=[ShellStep(id="s1", command="echo hi")])
    j.log(
        task="t", envelope=env, plan=plan,
        result=VerificationResult(
            ok=True, violations=[], warnings=[],
            envelope_id="e", plan_id="p", duration_ms=1.0,
        ),
        trace_id="2026-04-09-rplayjsn",
        created_at="2026-04-09T10:00:00Z",
    )
    result = runner.invoke(
        app,
        ["journal", "replay", "2026-04-09-rplayjsn", "--data-dir", str(tmp_path), "--json"],
    )
    assert result.exit_code == 0
    payload = json.loads(result.stdout)
    assert payload["trace_id"] == "2026-04-09-rplayjsn"
    assert payload["drift"] is False


def test_journal_replay_drift_with_violations(tmp_path, monkeypatch):
    """Cover the violations branch in drift text output (lines 181-183)."""
    from opendaisugi.models import (
        ActionPlan, ShellStep, Envelope, Permission, VerificationResult, Violation,
    )

    j = Journal(data_dir=tmp_path)
    env = Envelope(id="e", generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(id="p", source="t", task="t",
                      steps=[ShellStep(id="s1", command="echo hi")])
    j.log(
        task="t", envelope=env, plan=plan,
        result=VerificationResult(
            ok=True, violations=[], warnings=[],
            envelope_id="e", plan_id="p", duration_ms=1.0,
        ),
        trace_id="2026-04-09-driftviol",
        created_at="2026-04-09T10:00:00Z",
    )

    from opendaisugi import journal as journal_module

    def fake_verify(plan, envelope, *, z3_timeout_ms=500):
        return VerificationResult(
            ok=False,
            violations=[Violation(stage="permission", message="shell not allowed")],
            warnings=[],
            envelope_id=envelope.id, plan_id=plan.id, duration_ms=0.1,
        )

    monkeypatch.setattr(journal_module, "verify", fake_verify)

    result = runner.invoke(
        app,
        ["journal", "replay", "2026-04-09-driftviol", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 1
    assert "drift" in result.stdout.lower()
    assert "shell not allowed" in result.stdout.lower()


# ----- CLI error handling -----


def test_generate_envelope_task_too_long_exits_two(tmp_path):
    from opendaisugi.exceptions import TaskTooLongError

    async def raise_too_long(**kwargs):
        raise TaskTooLongError("Task exceeds 4000 chars")

    with patch("opendaisugi.cli.generate_envelope", side_effect=raise_too_long):
        result = runner.invoke(
            app, ["generate-envelope", "x" * 5000, "--data-dir", str(tmp_path)]
        )
    assert result.exit_code == 2


def test_generate_envelope_llm_error_exits_two(tmp_path):
    from opendaisugi.exceptions import EnvelopeGenerationError

    async def raise_llm_error(**kwargs):
        raise EnvelopeGenerationError("LLM failed")

    with patch("opendaisugi.cli.generate_envelope", side_effect=raise_llm_error):
        result = runner.invoke(
            app, ["generate-envelope", "some task", "--data-dir", str(tmp_path)]
        )
    assert result.exit_code == 2


def test_verify_invalid_yaml_exits_two(tmp_path):
    bad_yaml = tmp_path / "bad.yaml"
    bad_yaml.write_text(": :\n  - [invalid")
    env_path = tmp_path / "env.yaml"
    env_path.write_text("generated_by: t\ntask: t\npermissions: {}\n")
    result = runner.invoke(
        app, ["verify", str(bad_yaml), "--envelope", str(env_path)]
    )
    assert result.exit_code == 2


def test_journal_search_import_error_goes_to_stderr(tmp_path):
    journal = Journal(data_dir=tmp_path)

    def raise_import(*a, **kw):
        raise ImportError("pip install 'opendaisugi[search]'")

    with patch.object(journal, "search", side_effect=raise_import):
        with patch("opendaisugi.cli.Journal", return_value=journal):
            result = runner.invoke(
                app, ["journal", "search", "test query", "--data-dir", str(tmp_path)]
            )
    assert result.exit_code == 2


# ----- journal parse -----

FIXTURE = Path(__file__).parent / "fixtures" / "sample_transcript.jsonl"


def test_journal_parse_writes_yaml(tmp_path):
    output = tmp_path / "episodes.yaml"
    result = runner.invoke(app, ["journal", "parse", str(FIXTURE), "-o", str(output)])
    assert result.exit_code == 0, result.output
    assert output.exists()
    data = yaml.safe_load(output.read_text())
    assert data["source"] == "claude-code"
    assert len(data["episodes"]) == 3


def test_journal_parse_writes_json(tmp_path):
    output = tmp_path / "episodes.json"
    result = runner.invoke(app, ["journal", "parse", str(FIXTURE), "-o", str(output), "--json"])
    assert result.exit_code == 0, result.output
    data = json.loads(output.read_text())
    assert data["source"] == "claude-code"


def test_journal_parse_bad_format_exits_two(tmp_path):
    output = tmp_path / "out.yaml"
    result = runner.invoke(app, ["journal", "parse", str(FIXTURE), "-o", str(output), "--format", "unknown"])
    assert result.exit_code == 2


def test_journal_parse_missing_file_exits_two(tmp_path):
    output = tmp_path / "out.yaml"
    result = runner.invoke(app, ["journal", "parse", "/nonexistent.jsonl", "-o", str(output)])
    assert result.exit_code == 2


# ----- journal ingest -----


def _write_episodes_file(tmp_path, n_episodes=2):
    """Write a minimal episodes YAML for CLI testing."""
    episodes = []
    for i in range(n_episodes):
        episodes.append({
            "id": f"ep_{i:02d}",
            "task": f"Test task {i}",
            "steps": [{"id": f"s{i}", "type": "shell", "command": f"echo {i}"}],
            "source_range": {"first_message": 0, "last_message": 1},
        })
    data = {
        "source": "claude-code",
        "source_file": "/tmp/test.jsonl",
        "parsed_at": "2026-04-10T12:00:00Z",
        "episodes": episodes,
    }
    path = tmp_path / "episodes.yaml"
    path.write_text(yaml.safe_dump(data, sort_keys=False))
    return path


def _fake_ingest_envelope(task, **kwargs):
    return Envelope(
        generated_by="test",
        task=task,
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


def test_journal_ingest_processes_episodes(tmp_path):
    episodes_path = _write_episodes_file(tmp_path)
    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock) as mock_gen:
        mock_gen.side_effect = lambda task, **kw: _fake_ingest_envelope(task)
        result = runner.invoke(app, [
            "journal", "ingest", str(episodes_path),
            "--data-dir", str(tmp_path),
        ])
    assert result.exit_code == 0, result.output
    assert "2 episodes" in result.output or "ep_00" in result.output


def test_journal_ingest_dry_run(tmp_path):
    episodes_path = _write_episodes_file(tmp_path)
    result = runner.invoke(app, [
        "journal", "ingest", str(episodes_path),
        "--data-dir", str(tmp_path),
        "--dry-run",
    ])
    assert result.exit_code == 0, result.output
    assert "DRY" in result.output


def test_journal_ingest_invalid_yaml_exits_two(tmp_path):
    bad = tmp_path / "bad.yaml"
    bad.write_text(": : : not valid yaml [")
    result = runner.invoke(app, ["journal", "ingest", str(bad), "--data-dir", str(tmp_path)])
    assert result.exit_code == 2


def test_journal_ingest_json_output(tmp_path):
    episodes_path = _write_episodes_file(tmp_path, n_episodes=1)
    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock) as mock_gen:
        mock_gen.side_effect = lambda task, **kw: _fake_ingest_envelope(task)
        result = runner.invoke(app, [
            "journal", "ingest", str(episodes_path),
            "--data-dir", str(tmp_path),
            "--json",
        ])
    assert result.exit_code == 0, result.output
    data = json.loads(result.output)
    assert "total" in data


def test_journal_ingest_errored_episode_exits_one(tmp_path):
    """If any episode errors (e.g. LLM exception), the command exits 1."""
    episodes_path = _write_episodes_file(tmp_path, n_episodes=2)

    async def raising_gen(task, **kw):
        raise RuntimeError("simulated LLM failure")

    with patch("opendaisugi.ingest.generate_envelope", new_callable=AsyncMock, side_effect=raising_gen):
        result = runner.invoke(app, [
            "journal", "ingest", str(episodes_path),
            "--data-dir", str(tmp_path),
        ])

    assert result.exit_code == 1, result.output
    assert "errored" in result.output


# ----- stakes / thinking-budget CLI flags -----


def test_cli_generate_envelope_stakes_low_uses_default(tmp_path, capsys):
    """--stakes low without --low-stakes-envelope falls back to DEFAULT_LOW_STAKES_ENVELOPE."""
    result = runner.invoke(app, ["generate-envelope", "any task", "--stakes", "low", "--json"])
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["id"] == "env_default_low_stakes"


def test_cli_generate_envelope_stakes_low_custom_file(tmp_path):
    from opendaisugi.models import FallbackStrategy

    custom = Envelope(
        id="env_custom", generated_by="test", task="custom",
        permissions=Permission(
            file_read=[], file_write=[], network=False, network_hosts=[],
            shell=False, shell_allowlist=[],
            max_execution_time_s=5, max_output_size_mb=1,
        ),
        invariants=[], postconditions=[], fallback=FallbackStrategy(),
    )
    env_path = tmp_path / "e.json"
    env_path.write_text(custom.model_dump_json())

    result = runner.invoke(app, [
        "generate-envelope", "any task",
        "--stakes", "low",
        "--low-stakes-envelope", str(env_path),
        "--json",
    ])
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["id"] == "env_custom"
