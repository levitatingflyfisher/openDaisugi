"""Contract test for the OpenClaw Node client (v0.10.0).

The Node client in examples/integrations/openclaw/ drives the MCP
server through three tools. This test exercises the same tools from
the Python side against the same envelope/step shapes the Node demo
uses, so we can verify the contract without requiring Node.js in CI.
"""

from __future__ import annotations

import pytest

pytest.importorskip("mcp")

from opendaisugi import Daisugi  # noqa: E402
from opendaisugi.mcp_server import build_server  # noqa: E402


ENVELOPE = {
    "generated_by": "openclaw-demo",
    "task": "Send email on Ada's behalf",
    "stakes": "medium",
    "permissions": {"shell": True, "shell_allowlist": ["send_email"]},
    "invariants": [],
    "postconditions": [
        {
            "type": "body_no_impersonation",
            "description": "body must not end with Ada's signature",
            "expr": {
                "op": "forall_steps",
                "pred": {
                    "op": "not_matches",
                    "path": "metadata.body",
                    "regex": r"(?i)(\u2014|-)\s*ada(\s+lin)?\s*$",
                },
            },
            "enforce": True,
        }
    ],
}


def _step(body: str) -> dict:
    return {
        "id": "s1",
        "type": "shell",
        "command": "send_email",
        "depends_on": [],
        "metadata": {"type": "email_send", "to": "editor@blog.com", "body": body},
    }


@pytest.mark.asyncio
async def test_openclaw_contract_rejects_impersonation(tmp_path):
    server = build_server(Daisugi(data_dir=tmp_path, cache=False))
    _, structured = await server.call_tool(
        "verify_completed_step",
        {
            "step": _step("Hi editor,\n\nDraft attached.\n\n\u2014 Ada"),
            "envelope": ENVELOPE,
        },
    )
    assert len(structured["violations"]) == 1
    assert "body_no_impersonation" in structured["violations"][0]["message"]


@pytest.mark.asyncio
async def test_openclaw_contract_accepts_clean(tmp_path):
    server = build_server(Daisugi(data_dir=tmp_path, cache=False))
    _, structured = await server.call_tool(
        "verify_completed_step",
        {
            "step": _step("Hi editor,\n\nDraft attached.\n\n\u2014 Robin"),
            "envelope": ENVELOPE,
        },
    )
    assert structured["violations"] == []
