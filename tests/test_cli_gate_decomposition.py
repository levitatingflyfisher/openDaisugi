"""`daisugi gate init/quickstart --allow-shell-decomposition` (ADR-0010).

The flag is the only CLI route to the opt-in; without it the field was
reachable only by hand-editing the registered envelope JSON. These are
plumbing tests — they must run on a box WITHOUT ``opendaisugi[shell]``, so
the parser probe is monkeypatched rather than depended on.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.shell_decompose import parser_available

runner = CliRunner()

_FLAG = "--allow-shell-decomposition"


def _envelope(root: Path) -> dict:
    return json.loads((root / "envelopes" / "default.json").read_text())


def _init(root: Path, *extra: str):
    return runner.invoke(
        app, ["gate", "init", "--workspace", str(root), "--root", str(root), *extra]
    )


def _quickstart(root: Path, *extra: str):
    return runner.invoke(
        app, ["gate", "quickstart", "--workspace", str(root), "--root", str(root), *extra]
    )


# --- the default is provably unchanged -----------------------------------------


def test_gate_init_without_the_flag_still_writes_false(tmp_path):
    res = _init(tmp_path)
    assert res.exit_code == 0, res.output
    assert _envelope(tmp_path)["permissions"]["shell_allow_decomposition"] is False


def test_gate_quickstart_without_the_flag_still_writes_false(tmp_path):
    res = _quickstart(tmp_path)
    assert res.exit_code == 0, res.output
    assert _envelope(tmp_path)["permissions"]["shell_allow_decomposition"] is False


# --- the flag reaches the registered envelope ----------------------------------


def test_gate_init_flag_writes_true(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.shell_decompose.parser_available", lambda: True)
    res = _init(tmp_path, _FLAG)
    assert res.exit_code == 0, res.output
    assert _envelope(tmp_path)["permissions"]["shell_allow_decomposition"] is True


def test_gate_quickstart_flag_writes_true(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.shell_decompose.parser_available", lambda: True)
    res = _quickstart(tmp_path, _FLAG)
    assert res.exit_code == 0, res.output
    assert _envelope(tmp_path)["permissions"]["shell_allow_decomposition"] is True


# --- honest capability warning --------------------------------------------------


def test_flag_without_the_parser_warns_and_still_writes(tmp_path, monkeypatch):
    """Turning the field on without the extra makes compound commands DENY
    (verify.py fails closed on a missing capability). Say so — but still write
    it, because the envelope may travel to a box that has the parser."""
    monkeypatch.setattr("opendaisugi.shell_decompose.parser_available", lambda: False)
    res = _init(tmp_path, _FLAG)
    assert res.exit_code == 0, res.output
    assert "opendaisugi[shell]" in res.output
    assert "DENIED" in res.output
    assert _envelope(tmp_path)["permissions"]["shell_allow_decomposition"] is True


def test_no_warning_when_the_parser_is_present(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi.shell_decompose.parser_available", lambda: True)
    res = _init(tmp_path, _FLAG)
    assert "opendaisugi[shell]" not in res.output


def test_no_warning_when_the_flag_is_not_used(tmp_path, monkeypatch):
    """A missing parser is irrelevant to an envelope that never opts in."""
    monkeypatch.setattr("opendaisugi.shell_decompose.parser_available", lambda: False)
    res = _init(tmp_path)
    assert "opendaisugi[shell]" not in res.output


# --- the flag is discoverable ---------------------------------------------------


def test_gate_init_help_mentions_the_flag(monkeypatch):
    # Rich wraps the options table to the terminal width; an 80-column default
    # elides the long flag name, so pin a width the assertion can see.
    monkeypatch.setenv("COLUMNS", "200")
    res = runner.invoke(app, ["gate", "init", "--help"])
    assert res.exit_code == 0
    assert "--allow-shell-decomposition" in res.output


# --- end to end: the flag reaches the gate's verdict ----------------------------
#
# These need the real grammar, so they skip where the extra is absent (CI has it
# via `uv pip install -e ".[dev]"`). The plumbing tests above deliberately do not.

_needs_parser = pytest.mark.skipif(
    not parser_available(), reason="needs the opendaisugi shell extra (tree-sitter-bash)"
)


def _enforce(root: Path, command: str):
    from opendaisugi.gate import gate_and_contract

    payload = json.dumps(
        {"tool_name": "Bash", "tool_input": {"command": command}, "session_id": "s1"}
    ).encode()
    return gate_and_contract(payload, root=root, fmt="claude", mode="enforce")


@_needs_parser
def test_compound_of_allowlisted_heads_is_allowed_with_the_flag(tmp_path):
    _init(tmp_path, _FLAG)
    assert _enforce(tmp_path, "git status && ls -la").exit_code == 0


@_needs_parser
def test_the_same_call_is_denied_without_the_flag(tmp_path):
    """The regression that proves the flag is what changed the verdict."""
    _init(tmp_path)
    out = _enforce(tmp_path, "git status && ls -la")
    assert out.exit_code == 2
    assert "metacharacters" in out.stderr


@_needs_parser
@pytest.mark.parametrize(
    "command",
    [
        "git status && rm -rf /tmp/x",  # a head outside the allowlist
        "git status > /etc/passwd",  # redirection escapes the file scope
        "git status && $CMD",  # non-literal head
        "ls | xargs rm",  # command-taking wrapper
        "git status && (",  # malformed
    ],
)
def test_the_flag_does_not_admit_the_smuggle(tmp_path, command):
    """Opting in checks EVERY head; it never becomes 'allow compound shell'."""
    _init(tmp_path, _FLAG)
    assert _enforce(tmp_path, command).exit_code == 2
