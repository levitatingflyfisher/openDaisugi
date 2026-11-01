"""`daisugi hook auto-tend` honors the background-distillation consent check."""

from __future__ import annotations

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import Config, save_config

runner = CliRunner()


def test_auto_tend_skips_without_consent(tmp_path):
    # No config → unasked → the background tend must not run.
    res = runner.invoke(app, ["hook", "auto-tend", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "background distillation is off" in res.output.lower()
    assert "converted" not in res.output  # never reached the capture→trace loop


def test_auto_tend_skips_when_declined(tmp_path):
    save_config(Config(auto_tend=False), tmp_path / "config.yaml")
    res = runner.invoke(app, ["hook", "auto-tend", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "background distillation is off" in res.output.lower()


def test_auto_tend_runs_when_consented(tmp_path):
    # Consented → the warden lets it through; with no captured sessions it reports
    # a clean zero-conversion run rather than the "off" skip message.
    save_config(Config(auto_tend=True), tmp_path / "config.yaml")
    (tmp_path / "captures").mkdir()
    res = runner.invoke(
        app,
        [
            "hook",
            "auto-tend",
            "--data-dir",
            str(tmp_path),
            "--captures-root",
            str(tmp_path / "captures"),
        ],
    )
    assert res.exit_code == 0, res.output
    assert "background distillation is off" not in res.output.lower()
    assert "converted 0 sessions" in res.output.lower()


def test_auto_tend_force_bypasses_the_warden(tmp_path):
    # --force tends once even with no consent (a manual, deliberate run).
    (tmp_path / "captures").mkdir()
    res = runner.invoke(
        app,
        [
            "hook",
            "auto-tend",
            "--data-dir",
            str(tmp_path),
            "--captures-root",
            str(tmp_path / "captures"),
            "--force",
        ],
    )
    assert res.exit_code == 0, res.output
    assert "background distillation is off" not in res.output.lower()


def _cap(root, sid, n=1):
    import json

    root.mkdir(parents=True, exist_ok=True)
    (root / f"{sid}.jsonl").write_text(
        "".join(
            json.dumps(
                {
                    "captured_at": 1.0 + i,
                    "session_id": sid,
                    "tool_name": "Bash",
                    "step_type": "shell",
                    "command": "ls",
                }
            )
            + "\n"
            for i in range(n)
        )
    )


def test_auto_tend_converts_the_warden_root_and_skips_the_duplicate(tmp_path):
    data = tmp_path / "d"
    data.mkdir()
    (data / "config.yaml").write_text("auto_tend: true\n")
    _cap(data / "captures", "claude-s", 2)
    _cap(data / "warden" / "captures", "claude-s", 2)
    _cap(data / "warden" / "captures", "pi-s", 1)
    res = runner.invoke(
        app, ["hook", "auto-tend", "--data-dir", str(data), "--force", "--skip-distill"]
    )
    assert res.exit_code == 0, res.output
    assert res.output.count("converted claude-s") == 1
    assert "converted pi-s" in res.output
    assert "converted 2 sessions" in res.output


def test_auto_tend_explicit_captures_root_uses_only_that_root(tmp_path):
    data = tmp_path / "d"
    data.mkdir()
    _cap(data / "warden" / "captures", "pi-s", 1)
    _cap(tmp_path / "other", "o", 1)
    res = runner.invoke(
        app,
        [
            "hook",
            "auto-tend",
            "--data-dir",
            str(data),
            "--captures-root",
            str(tmp_path / "other"),
            "--force",
            "--skip-distill",
        ],
    )
    assert "converted o" in res.output and "pi-s" not in res.output
