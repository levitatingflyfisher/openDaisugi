import json
import subprocess
from pathlib import Path

import pytest

from tests.harness_pi._node_check import node_status

HERE = Path(__file__).parent
PLUGIN = Path("src/opendaisugi/install_assets/openclaw_plugin/index.mjs")

ok, why = node_status()
needs_node = pytest.mark.skipif(not ok, reason=why)


def run(event, ctx):
    res = subprocess.run(
        [
            "node",
            str(HERE / "run_before_tool_call.mjs"),
            str(PLUGIN.resolve()),
            json.dumps(event),
            json.dumps(ctx),
        ],
        capture_output=True,
        text=True,
        timeout=20,
        check=True,
    )
    return json.loads(res.stdout)


@needs_node
def test_session_id_comes_from_ctx():
    p = run(
        {"toolName": "exec", "params": {"command": "ls"}, "toolCallId": "tc-9"},
        {"sessionId": "oc-sess", "runId": "run-1", "agentId": "main"},
    )
    assert p == {
        "tool": "exec",
        "args": {"command": "ls"},
        "session_id": "oc-sess",
        "tool_use_id": "tc-9",
        "run_id": "run-1",
    }
    assert list(p) == ["tool", "args", "session_id", "tool_use_id", "run_id"]


@needs_node
def test_no_session_anywhere_is_no_session():
    p = run({"toolName": "read", "params": {"path": "a"}}, {})
    assert p["session_id"] == "no-session" and p["tool_use_id"] == ""


def test_go_copy_is_byte_identical():
    go = Path("clients/go/internal/install/assets/openclaw_plugin/index.mjs")
    assert go.read_bytes() == PLUGIN.read_bytes()
