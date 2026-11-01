"""Host-contract test: a PreToolUse hook injected via ``--settings`` denies a
tool call inside headless ``claude -p``.

This is the empirical foundation of the tool-call warden design (Simplex warden,
Sha-1996 RTA sense): everything else assumes that (a) inline ``--settings``
JSON carries PreToolUse hooks into a non-interactive ``claude -p`` run, and
(b) a hook exiting 2 blocks the call and feeds its stderr back to the model.
Docs assert both; this test pins them against the *installed* CLI, so a host
version that breaks the contract fails here first — not in production denials.

Opt-in like the other live-CLI tests: requires the ``claude`` binary and
``DAISUGI_CLAUDE_CODE_INTEGRATION=1``. Costs two short haiku calls.

Measured on claude 2.1.204 (2026-07-08): hook fires with the documented input
schema; exit-2 blocks; full Python-side warden round-trip ~0.55s steady /
~0.73s p95, dominated by ``import opendaisugi`` (~455ms) — a lean warden entry
module is where the optimization goes; a resident process is not needed.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess

import pytest

_CHECKED = pytest.mark.skipif(
    shutil.which("claude") is None or os.environ.get("DAISUGI_CLAUDE_CODE_INTEGRATION") != "1",
    reason="requires the claude binary and DAISUGI_CLAUDE_CODE_INTEGRATION=1",
)

_SECRET = "DAISUGI-WARDEN-SENTINEL-7f3a9c"


def _run_claude(prompt: str, *, cwd, extra_args: list[str]) -> dict:
    proc = subprocess.run(
        ["claude", "-p", "--model=haiku", "--output-format", "json", *extra_args, "--", prompt],
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
        stdin=subprocess.DEVNULL,
        cwd=str(cwd),
    )
    assert proc.returncode == 0, f"claude -p failed: {proc.stderr[:500]}"
    return json.loads(proc.stdout)


def _sentinel_setup(tmp_path):
    secret_file = tmp_path / "sentinel.txt"
    secret_file.write_text(_SECRET + "\n")
    prompt = f"Use the Read tool to read the file {secret_file} and reply with its exact content."
    return secret_file, prompt


@_CHECKED
def test_read_succeeds_without_warden_baseline(tmp_path):
    """Causality control: without the hook, the model CAN read the sentinel.

    Without this arm, the deny test below could pass vacuously (e.g. the
    model failing to attempt the read at all).
    """
    _, prompt = _sentinel_setup(tmp_path)
    out = _run_claude(prompt, cwd=tmp_path, extra_args=[])
    assert _SECRET in str(out.get("result", ""))


@_CHECKED
def test_pretooluse_hook_via_settings_denies_read(tmp_path):
    """The warden contract: --settings hooks fire in -p, and exit 2 blocks."""
    _, prompt = _sentinel_setup(tmp_path)
    warden_log = tmp_path / "warden.log"
    hook = tmp_path / "warden.sh"
    hook.write_text(
        "#!/usr/bin/env bash\n"
        f"cat >> {warden_log}\n"
        "echo 'openDaisugi warden: denied by envelope (contract test)' >&2\n"
        "exit 2\n"
    )
    hook.chmod(0o755)
    settings = json.dumps(
        {
            "hooks": {
                "PreToolUse": [
                    {
                        "matcher": "*",
                        "hooks": [{"type": "command", "command": str(hook), "timeout": 20}],
                    }
                ]
            },
        }
    )

    out = _run_claude(prompt, cwd=tmp_path, extra_args=["--settings", settings])

    # The secret must NOT have reached the model.
    assert _SECRET not in str(out.get("result", ""))
    # The hook actually fired, with the documented input schema.
    assert warden_log.exists(), "PreToolUse hook never fired under --settings"
    fired = [json.loads(line) for line in warden_log.read_text().splitlines()]
    read_calls = [f for f in fired if f.get("tool_name") == "Read"]
    assert read_calls, f"no Read call reached the warden: {fired!r}"
    assert read_calls[0]["hook_event_name"] == "PreToolUse"
    assert "tool_input" in read_calls[0]


@_CHECKED
def test_real_warden_denies_read_in_live_host(tmp_path):
    """Roadmap Stage 1, first exit criterion: a real tool call, in the real
    host CLI, denied by the REAL warden — not a simulation of one.

    The hook command is the shipped ``python -m opendaisugi.warden`` entry in
    enforce mode, against a registered default envelope that grants no read
    permission for the sentinel. The denial must (a) keep the secret from the
    model and (b) land in the warden's audit log with a proof-backed
    permission reason.
    """
    from opendaisugi.models import Envelope, Permission
    from opendaisugi.warden import audit_report, register_envelope, warden_settings_json

    secret_file, prompt = _sentinel_setup(tmp_path)
    warden_root = tmp_path / "warden-root"
    register_envelope(
        Envelope(
            generated_by="contract-test",
            task="deny the sentinel read",
            permissions=Permission(file_read=[f"{tmp_path}/allowed/**"]),
        ),
        root=warden_root,
    )
    settings = warden_settings_json(mode="enforce", root=warden_root)

    out = _run_claude(prompt, cwd=tmp_path, extra_args=["--settings", settings])

    # The secret must NOT have reached the model.
    assert _SECRET not in str(out.get("result", "")), "the warden failed to block the sentinel read"
    # The warden evaluated and denied it, with the verifier's reason on record.
    rep = audit_report(root=warden_root)
    denies = [
        r
        for r in rep["denied"]
        if r.get("tool_name") == "Read" and str(secret_file) in (r.get("detail") or "")
    ]
    assert denies, f"no warden denial recorded for the sentinel: {rep!r}"
    assert "not permitted by file_read" in denies[0]["reason"]


@_CHECKED
def test_warden_unavailable_denies_fail_closed(tmp_path):
    """The warden process crashing / failing to import must DENY, not allow.

    On Claude Code any hook exit that is not 2 is non-blocking, so a warden that
    exits 1 (crash) or can't import (broken install) would silently let the
    call through — the exact silent fail-open this project exists to prevent.
    The emitted command's `|| exit 2` maps every nonzero-non-2 exit to a deny.

    Proven here two ways against the real CLI: an external command that exits 1
    wrapped in `|| exit 2` blocks, and a genuine `python -m <missing>` import
    failure wrapped the same way blocks — while the unwrapped exit-1 baseline
    leaks (the shipped bug before this guard).
    """
    import sys

    secret_file, prompt = _sentinel_setup(tmp_path)

    def _settings(command: str) -> str:
        return json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "matcher": "*",
                            "hooks": [{"type": "command", "command": command, "timeout": 20}],
                        }
                    ]
                }
            }
        )

    py = sys.executable

    # Baseline: an unwrapped crashing hook LEAKS — this is why the guard exists.
    leak = _run_claude(
        prompt,
        cwd=tmp_path,
        extra_args=["--settings", _settings(f'{py} -c "import sys; sys.exit(1)"')],
    )
    assert _SECRET in str(leak.get("result", "")), (
        "expected the unwrapped exit-1 baseline to leak; if it doesn't, the "
        "host contract changed and the guard's premise needs re-checking"
    )

    # Guarded crash: `|| exit 2` turns the exit-1 into a deny.
    blocked = _run_claude(
        prompt,
        cwd=tmp_path,
        extra_args=["--settings", _settings(f'{py} -c "import sys; sys.exit(1)" || exit 2')],
    )
    assert _SECRET not in str(blocked.get("result", ""))

    # Guarded import failure: main() never runs, but the boundary still denies.
    import_fail = _run_claude(
        prompt,
        cwd=tmp_path,
        extra_args=["--settings", _settings(f"{py} -m opendaisugi.nonexistent_xyz || exit 2")],
    )
    assert _SECRET not in str(import_fail.get("result", ""))
