"""Host-contract test: a PreToolUse hook injected via ``--settings`` denies a
tool call inside headless ``claude -p``.

This is the empirical foundation of the tool-call gate design (Simplex gate,
Sha-1996 RTA sense): everything else assumes that (a) inline ``--settings``
JSON carries PreToolUse hooks into a non-interactive ``claude -p`` run, and
(b) a hook exiting 2 blocks the call and feeds its stderr back to the model.
Docs assert both; this test pins them against the *installed* CLI, so a host
version that breaks the contract fails here first — not in production denials.

Opt-in like the other live-CLI tests: requires the ``claude`` binary and
``DAISUGI_CLAUDE_CODE_INTEGRATION=1``. Costs two short haiku calls.

Measured on claude 2.1.204 (2026-07-08): hook fires with the documented input
schema; exit-2 blocks; full Python-side gate round-trip ~0.55s steady /
~0.73s p95, dominated by ``import opendaisugi`` (~455ms) — a lean gate entry
module is the optimization seam; a resident process is not needed.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess

import pytest

_GATED = pytest.mark.skipif(
    shutil.which("claude") is None
    or os.environ.get("DAISUGI_CLAUDE_CODE_INTEGRATION") != "1",
    reason="requires the claude binary and DAISUGI_CLAUDE_CODE_INTEGRATION=1",
)

_SECRET = "DAISUGI-GATE-SENTINEL-7f3a9c"


def _run_claude(prompt: str, *, cwd, extra_args: list[str]) -> dict:
    proc = subprocess.run(
        ["claude", "-p", "--model=haiku", "--output-format", "json",
         *extra_args, "--", prompt],
        capture_output=True, text=True, timeout=180, check=False,
        stdin=subprocess.DEVNULL, cwd=str(cwd),
    )
    assert proc.returncode == 0, f"claude -p failed: {proc.stderr[:500]}"
    return json.loads(proc.stdout)


def _sentinel_setup(tmp_path):
    secret_file = tmp_path / "sentinel.txt"
    secret_file.write_text(_SECRET + "\n")
    prompt = (
        f"Use the Read tool to read the file {secret_file} and reply with "
        "its exact content."
    )
    return secret_file, prompt


@_GATED
def test_read_succeeds_without_gate_baseline(tmp_path):
    """Causality control: without the hook, the model CAN read the sentinel.

    Without this arm, the deny test below could pass vacuously (e.g. the
    model failing to attempt the read at all).
    """
    _, prompt = _sentinel_setup(tmp_path)
    out = _run_claude(prompt, cwd=tmp_path, extra_args=[])
    assert _SECRET in str(out.get("result", ""))


@_GATED
def test_pretooluse_hook_via_settings_denies_read(tmp_path):
    """The gate contract: --settings hooks fire in -p, and exit 2 blocks."""
    _, prompt = _sentinel_setup(tmp_path)
    gate_log = tmp_path / "gate.log"
    hook = tmp_path / "gate.sh"
    hook.write_text(
        "#!/usr/bin/env bash\n"
        f"cat >> {gate_log}\n"
        "echo 'openDaisugi gate: denied by envelope (contract test)' >&2\n"
        "exit 2\n"
    )
    hook.chmod(0o755)
    settings = json.dumps({
        "hooks": {"PreToolUse": [{
            "matcher": "*",
            "hooks": [{"type": "command", "command": str(hook), "timeout": 20}],
        }]},
    })

    out = _run_claude(prompt, cwd=tmp_path, extra_args=["--settings", settings])

    # The secret must NOT have reached the model.
    assert _SECRET not in str(out.get("result", ""))
    # The hook actually fired, with the documented input schema.
    assert gate_log.exists(), "PreToolUse hook never fired under --settings"
    fired = [json.loads(line) for line in gate_log.read_text().splitlines()]
    read_calls = [f for f in fired if f.get("tool_name") == "Read"]
    assert read_calls, f"no Read call reached the gate: {fired!r}"
    assert read_calls[0]["hook_event_name"] == "PreToolUse"
    assert "tool_input" in read_calls[0]
