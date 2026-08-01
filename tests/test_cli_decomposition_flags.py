"""One opt-in, one persisted default, one override — across every command.

ADR-0010's opt-in became reachable on `gate init`, but the bulk paths that
actually replay history (`onboard`, `journal ingest`, `hook to-trace`,
`hook auto-tend`) had no way to ask for it. `hook auto-tend` is the reason the
default has to live in config.yaml at all: it runs from cron and from a
detached spawn, where no flag can reach it and stdout goes to DEVNULL.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
import yaml
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import Config, load_config, save_config
from opendaisugi.journal import Journal
from opendaisugi.shell_decompose import parser_available

runner = CliRunner()

_FLAG = "--allow-shell-decomposition"
_NO_FLAG = "--no-allow-shell-decomposition"

_needs_parser = pytest.mark.skipif(
    not parser_available(), reason="needs the opendaisugi shell extra (tree-sitter-bash)"
)


def _capture(root: Path, sid: str = "sess", command: str = "cd /repo && pytest -q") -> Path:
    root.mkdir(parents=True, exist_ok=True)
    path = root / f"{sid}.jsonl"
    path.write_text(json.dumps({"step_type": "shell", "command": command}) + "\n")
    return path


def _to_trace(tmp_path: Path, *extra: str, sid: str = "sess"):
    captures = tmp_path / "captures"
    _capture(captures, sid)
    return runner.invoke(
        app,
        [
            "hook",
            "to-trace",
            sid,
            "--captures-root",
            str(captures),
            "--data-dir",
            str(tmp_path),
            *extra,
        ],
    )


def _trace_ok(tmp_path: Path, trace_id: str) -> bool:
    return Journal(data_dir=tmp_path).load_trace(trace_id).result.ok


# --- the persisted default ------------------------------------------------------


def test_config_defaults_the_opt_in_off():
    assert Config().shell_allow_decomposition is False


def test_config_round_trips_the_opt_in(tmp_path: Path):
    path = tmp_path / "config.yaml"
    save_config(Config(shell_allow_decomposition=True), path)
    assert yaml.safe_load(path.read_text())["shell_allow_decomposition"] is True
    assert load_config(path).shell_allow_decomposition is True


# --- hook to-trace: flag, default, override -------------------------------------


def test_to_trace_without_the_flag_still_rejects_the_compound(tmp_path: Path):
    res = _to_trace(tmp_path)
    assert res.exit_code == 0, res.output
    assert _trace_ok(tmp_path, res.output.strip()) is False


@_needs_parser
def test_to_trace_with_the_flag_verifies_the_compound(tmp_path: Path):
    res = _to_trace(tmp_path, _FLAG)
    assert res.exit_code == 0, res.output
    assert _trace_ok(tmp_path, res.output.strip()) is True


@_needs_parser
def test_to_trace_takes_its_default_from_config(tmp_path: Path):
    save_config(Config(shell_allow_decomposition=True), tmp_path / "config.yaml")
    res = _to_trace(tmp_path)
    assert res.exit_code == 0, res.output
    assert _trace_ok(tmp_path, res.output.strip()) is True


def test_an_explicit_no_overrides_a_true_config(tmp_path: Path):
    """The flag is a per-run override in BOTH directions, never a one-way ratchet."""
    save_config(Config(shell_allow_decomposition=True), tmp_path / "config.yaml")
    res = _to_trace(tmp_path, _NO_FLAG)
    assert res.exit_code == 0, res.output
    assert _trace_ok(tmp_path, res.output.strip()) is False


# --- auto-tend: the path a flag cannot reach ------------------------------------


@_needs_parser
def test_auto_tend_honours_the_config_default(tmp_path: Path):
    """The cron path reads the same one source of truth."""
    save_config(Config(auto_tend=True, shell_allow_decomposition=True), tmp_path / "config.yaml")
    captures = tmp_path / "captures"
    _capture(captures, "s1")
    res = runner.invoke(
        app,
        [
            "hook",
            "auto-tend",
            "--captures-root",
            str(captures),
            "--data-dir",
            str(tmp_path),
            "--skip-distill",
            "--force",
        ],
    )
    assert res.exit_code == 0, res.output
    traces = [line.split("→")[-1].strip() for line in res.output.splitlines() if "→" in line]
    assert traces, res.output
    assert _trace_ok(tmp_path, traces[0]) is True


# --- gate: same resolver, config beside the gate root ---------------------------


@_needs_parser
def test_gate_init_takes_its_default_from_config(tmp_path: Path):
    root = tmp_path / ".opendaisugi" / "gate"
    save_config(Config(shell_allow_decomposition=True), tmp_path / ".opendaisugi" / "config.yaml")
    res = runner.invoke(app, ["gate", "init", "--workspace", str(tmp_path), "--root", str(root)])
    assert res.exit_code == 0, res.output
    env = json.loads((root / "envelopes" / "default.json").read_text())
    assert env["permissions"]["shell_allow_decomposition"] is True


# --- the flag is discoverable wherever an envelope is produced ------------------


@pytest.mark.parametrize(
    "argv",
    [
        ["onboard", "--help"],
        ["journal", "ingest", "--help"],
        ["hook", "to-trace", "--help"],
        ["hook", "auto-tend", "--help"],
        ["generate-envelope", "--help"],
        ["gate", "init", "--help"],
        ["install", "--help"],
    ],
    ids=lambda a: "-".join(a[:-1]),
)
def test_the_flag_is_documented_on_every_envelope_producing_command(argv, monkeypatch):
    monkeypatch.setenv("COLUMNS", "200")  # rich wraps the options table to terminal width
    res = runner.invoke(app, argv)
    assert res.exit_code == 0, res.output
    assert _FLAG in res.output


# --- install writes the persisted default ---------------------------------------


def test_install_writes_the_opt_in_to_config(tmp_path: Path, monkeypatch):
    monkeypatch.setattr(Path, "home", classmethod(lambda cls: tmp_path))
    res = runner.invoke(app, ["install", "--yes", "--runtime", "claude", _FLAG])
    assert res.exit_code == 0, res.output
    assert load_config(tmp_path / ".opendaisugi" / "config.yaml").shell_allow_decomposition


def test_install_dry_run_writes_no_config(tmp_path: Path, monkeypatch):
    monkeypatch.setattr(Path, "home", classmethod(lambda cls: tmp_path))
    res = runner.invoke(app, ["install", "--dry-run", "--runtime", "claude", _FLAG])
    assert res.exit_code == 0, res.output
    assert not (tmp_path / ".opendaisugi" / "config.yaml").exists()


def test_generate_envelope_resolves_config_from_its_own_data_dir(tmp_path: Path, monkeypatch):
    """Resolution must never reach past --data-dir into the real user's config.

    A module-level default evaluated at import time would make this command's
    behaviour depend on the machine it runs on, and quietly make any test that
    asserts on generated permissions machine-state dependent.
    """
    from opendaisugi.models import Envelope, Permission

    seen: list[Path] = []

    def _record(flag, config_path):
        seen.append(config_path)
        return False

    async def _fake_generate(**kwargs):
        return Envelope(generated_by="test", task="a task", permissions=Permission())

    monkeypatch.setattr("opendaisugi.cli._resolve_decompose", _record)
    monkeypatch.setattr("opendaisugi.cli.generate_envelope", _fake_generate)
    res = runner.invoke(app, ["generate-envelope", "a task", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert seen == [tmp_path / "config.yaml"]
