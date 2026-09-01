# tests/test_cli_errors.py
"""Every error teaches: what was tried, why it failed, the next action. No tracebacks."""

from __future__ import annotations

import os
import subprocess
import sys

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import _fail, app

runner = CliRunner()


def test_fail_prints_three_lines_and_exits(capsys):
    import typer

    with pytest.raises(typer.Exit) as ei:
        _fail("Tried to read plan.yaml.", "The file does not exist.", "Check the path.", code=2)
    assert ei.value.exit_code == 2
    err = capsys.readouterr().err
    assert err.splitlines() == [
        "Tried to read plan.yaml.",
        "The file does not exist.",
        "Check the path.",
    ]


def test_main_renders_opendaisugi_errors_without_a_traceback(tmp_path):
    env = {
        k: v
        for k, v in os.environ.items()
        if k not in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
    }
    env["OPENDAISUGI_LLM_BACKEND"] = "litellm"
    env["HOME"] = str(tmp_path)
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "opendaisugi.cli",
            "orchestrate",
            "list three things",
            "--data-dir",
            str(tmp_path / "d"),
        ],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )
    assert proc.returncode == 1, proc.stderr
    assert "Traceback" not in proc.stderr
    assert "<failed_attempts>" not in proc.stderr
    assert "ANTHROPIC_API_KEY" in proc.stderr
    assert "--llm claude-code" in proc.stderr


def test_debug_env_shows_the_traceback(tmp_path):
    env = {
        k: v
        for k, v in os.environ.items()
        if k not in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
    }
    env.update({"OPENDAISUGI_LLM_BACKEND": "litellm", "HOME": str(tmp_path), "DAISUGI_DEBUG": "1"})
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "opendaisugi.cli",
            "orchestrate",
            "list three things",
            "--data-dir",
            str(tmp_path / "d"),
        ],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )
    assert proc.returncode != 0
    assert "Traceback" in proc.stderr


def _three_lines(text: str) -> bool:
    lines = [ln for ln in text.strip().splitlines() if ln.strip()]
    return len(lines) == 3


# --- Task 7: corrected from the plan's un-buildable versions (2026-08-27 review) ---
#
# `run`/`verify` take the envelope as --envelope, not a positional, and
# plan_path is `typer.Argument(exists=True)`, so a missing file is rejected by
# Typer before the body runs — a bare-missing-file test can never reach our
# own error text. These instead aim at a *parse* failure on an
# existing-but-invalid YAML file, which the command body does reach.


def _minimal_envelope_yaml(path):
    path.write_text("generated_by: t\ntask: t\npermissions: {}\n")


def test_verify_invalid_plan_yaml_teaches(tmp_path):
    bad_plan = tmp_path / "plan.yaml"
    bad_plan.write_text(": :\n  - [invalid")
    env_path = tmp_path / "env.yaml"
    _minimal_envelope_yaml(env_path)
    res = runner.invoke(app, ["verify", str(bad_plan), "--envelope", str(env_path)])
    assert res.exit_code == 2, res.output
    assert "plan.yaml" in res.output
    assert _three_lines(res.output), res.output


def test_run_invalid_plan_yaml_teaches(tmp_path):
    bad_plan = tmp_path / "plan.yaml"
    bad_plan.write_text(": :\n  - [invalid")
    env_path = tmp_path / "env.yaml"
    _minimal_envelope_yaml(env_path)
    res = runner.invoke(app, ["run", str(bad_plan), "--envelope", str(env_path)])
    assert res.exit_code == 2, res.output
    assert "plan.yaml" in res.output
    assert _three_lines(res.output), res.output


def test_orchestrate_malformed_envelope_teaches(tmp_path):
    """Fix round 1, item 2: the envelope load in orchestrate_cmd was outside the
    try/except, so a malformed --envelope printed a raw traceback with no
    DAISUGI_DEBUG. It must now teach three lines, like run/verify.
    """
    bad_env = tmp_path / "env.yaml"
    bad_env.write_text(": :\n  - [invalid")
    res = runner.invoke(
        app,
        [
            "orchestrate",
            "do something",
            "--envelope",
            str(bad_env),
            "--llm",
            "litellm",
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert res.exit_code == 2, res.output
    assert "Traceback" not in res.output
    # `_echo_resolved` prints one line before the three-part error.
    lines = [ln for ln in res.output.strip().splitlines() if ln.strip()]
    assert len(lines) == 4, res.output
    assert "env.yaml" in res.output


def test_generate_envelope_usage_error_has_no_resolved_echo():
    """Fix round 1, item 5: _echo_resolved ran before --stakes validation, so a
    plain usage error (invalid flag value) printed the resolved-state line too.
    Match orchestrate's order: validate first, echo only once past validation.
    """
    res = runner.invoke(app, ["generate-envelope", "task", "--stakes", "bogus"])
    assert res.exit_code == 2, res.output
    assert "backend:" not in res.output
