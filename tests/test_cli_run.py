"""Tests for the `daisugi run` CLI subcommand."""

import json
import sys

import pytest
import yaml
from typer.testing import CliRunner

from opendaisugi.cli import app

pytestmark = pytest.mark.skipif(
    sys.platform == "win32", reason="daisugi run is POSIX-only in v0.1"
)


def _write_plan_and_envelope(tmp_path, command, allowlist):
    envelope = {
        "id": "env_cli_test",
        "generated_by": "test",
        "task": "t",
        "permissions": {
            "file_read": [], "file_write": [], "network": False,
            "shell": True, "shell_allowlist": allowlist,
            "max_execution_time_s": 30, "max_output_size_mb": 10,
        },
        "invariants": [], "postconditions": [],
        "fallback": {"strategy": "abort", "retry_count": 0},
        "parent_envelope": None, "tightening_only": False,
    }
    plan = {
        "id": "plan_cli_test",
        "source": "test",
        "task": "t",
        "steps": [{
            "id": "s1", "type": "shell", "command": command,
            "path": None, "content": None, "depends_on": [],
        }],
    }
    env_path = tmp_path / "env.yaml"
    plan_path = tmp_path / "plan.yaml"
    env_path.write_text(yaml.safe_dump(envelope))
    plan_path.write_text(yaml.safe_dump(plan))
    return plan_path, env_path


def test_run_dry_run_exits_zero(tmp_path):
    plan_path, env_path = _write_plan_and_envelope(tmp_path, "echo hi", ["echo"])
    runner = CliRunner()
    result = runner.invoke(app, [
        "run", str(plan_path), "--envelope", str(env_path),
        "--dry-run", "--data-dir", str(tmp_path),
    ])
    assert result.exit_code == 0, result.output
    assert "dry-run" in result.output.lower() or "would shell" in result.output.lower()


def test_run_yes_flag_sets_always_approve(tmp_path, monkeypatch):
    monkeypatch.delenv("DAISUGI_APPROVE", raising=False)
    # allowlist includes "echo" so verify() passes; --yes bypasses the approval prompt
    plan_path, env_path = _write_plan_and_envelope(
        tmp_path, "echo cli-test-ok", ["echo"]
    )
    runner = CliRunner()
    result = runner.invoke(app, [
        "run", str(plan_path), "--envelope", str(env_path),
        "--yes", "--data-dir", str(tmp_path),
    ])
    assert result.exit_code == 0, result.output


def test_run_failed_step_exits_one(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    plan_path, env_path = _write_plan_and_envelope(tmp_path, "false", ["false"])
    runner = CliRunner()
    result = runner.invoke(app, [
        "run", str(plan_path), "--envelope", str(env_path),
        "--data-dir", str(tmp_path),
    ])
    assert result.exit_code == 1, result.output


def test_run_rejected_verification_exits_two(tmp_path, monkeypatch):
    """Envelope forbids shell; verify() will reject the shell step."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    envelope = {
        "id": "env_r",
        "generated_by": "t", "task": "t",
        "permissions": {
            "file_read": [], "file_write": [], "network": False,
            "shell": False, "shell_allowlist": [],
            "max_execution_time_s": 30, "max_output_size_mb": 10,
        },
        "invariants": [], "postconditions": [],
        "fallback": {"strategy": "abort", "retry_count": 0},
        "parent_envelope": None, "tightening_only": False,
    }
    plan = {
        "id": "plan_r", "source": "t", "task": "t",
        "steps": [{"id": "s1", "type": "shell", "command": "echo hi",
                   "path": None, "content": None, "depends_on": []}],
    }
    env_path = tmp_path / "env.yaml"
    plan_path = tmp_path / "plan.yaml"
    env_path.write_text(yaml.safe_dump(envelope))
    plan_path.write_text(yaml.safe_dump(plan))
    runner = CliRunner()
    result = runner.invoke(app, [
        "run", str(plan_path), "--envelope", str(env_path),
        "--data-dir", str(tmp_path),
    ])
    assert result.exit_code == 2, result.output


def test_run_json_output(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    plan_path, env_path = _write_plan_and_envelope(tmp_path, "echo hi", ["echo"])
    runner = CliRunner()
    result = runner.invoke(app, [
        "run", str(plan_path), "--envelope", str(env_path),
        "--json", "--data-dir", str(tmp_path),
    ])
    assert result.exit_code == 0, result.output
    payload = json.loads(result.stdout)
    assert payload["status"] == "succeeded"
    assert payload["id"].startswith("run_")
    assert len(payload["steps"]) == 1
    assert payload["steps"][0]["status"] == "succeeded"
