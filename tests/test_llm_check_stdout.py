"""A failed llm_check call must not write to the hook's stdout.

On the hermes and openclaw formats stdout is the verdict body, so anything
the model client printed there would turn a block into text the host could
not parse. The client prints nothing. Never a real model: the URL is a port
nothing listens on, and no claude binary is on PATH.
"""

import json
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


@pytest.mark.parametrize("fmt", ["hermes", "openclaw"])
def test_a_failed_model_call_leaves_the_body_alone(tmp_path, fmt):
    root = tmp_path / "gate"
    (root / "envelopes").mkdir(parents=True)
    env_doc = {
        "generated_by": "t",
        "task": "t",
        "permissions": {"file_read": ["/work/**"]},
        "invariants": [{"type": "t", "description": "d", "expr": {"op": "llm_check", "rule": "r"}}],
    }
    (root / "envelopes" / "default.json").write_text(json.dumps(env_doc))
    env = {
        "HOME": str(tmp_path),
        "PATH": "/usr/bin:/bin",
        "PYTHONPATH": str(ROOT / "src"),
        "ANTHROPIC_API_KEY": "sk-test",
        "ANTHROPIC_API_BASE": "http://127.0.0.1:1",
        "OPENDAISUGI_LLM_BACKEND": "api",
    }
    payload = {
        "session_id": "s",
        "tool_name": "Read",
        "cwd": "/work",
        "tool_input": {"file_path": "/work/a"},
    }
    p = subprocess.run(
        [
            sys.executable,
            "-m",
            "opendaisugi.gate",
            "--mode",
            "enforce",
            "--root",
            str(root),
            "--format",
            fmt,
        ],
        input=json.dumps(payload).encode(),
        capture_output=True,
        env=env,
        timeout=120,
        check=False,
    )
    body = json.loads(p.stdout)  # the whole stdout is the verdict body
    assert "could not reach the model server at http://127.0.0.1:1/v1/messages" in json.dumps(body)
    assert p.stderr == b""
