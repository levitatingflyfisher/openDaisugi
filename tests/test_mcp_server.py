"""Tests for the MCP server wrapper (v0.6.0)."""

from __future__ import annotations

import time

import pytest

from opendaisugi import Daisugi
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
)
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore

pytest.importorskip("mcp")

from opendaisugi.mcp_server import build_server  # noqa: E402


def _daisugi(tmp_path) -> Daisugi:
    """Build a Daisugi rooted in a temp dir — no network, no real LLM."""
    return Daisugi(data_dir=tmp_path, cache=False)


def _seed_pathway(store: PathwayStore, id_: str = "p1") -> CompiledPathway:
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    p = CompiledPathway(
        id=id_,
        task_description="generalized task",
        task_embedding=[0.1, 0.2, 0.3],
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
        hit_count=3,
    )
    store.put(p)
    return p


@pytest.mark.asyncio
async def test_tools_registered(tmp_path):
    server = build_server(_daisugi(tmp_path))
    names = {t.name for t in await server.list_tools()}
    # Existing v0.6 tools + v0.20 runtime tools
    assert {
        "envelope_for",
        "find_pathway",
        "verify_plan",
        "verify_completed_step",
        "list_pathways",
        "pathway_stats",
    }.issubset(names)


@pytest.mark.asyncio
async def test_list_pathways_empty(tmp_path):
    server = build_server(_daisugi(tmp_path))
    result = await server.call_tool("list_pathways", {})
    # FastMCP returns (content_blocks, structured_output) for tools with
    # structured return types; the second element is what tests care about.
    _, structured = result
    assert structured == {"result": []}


@pytest.mark.asyncio
async def test_list_pathways_with_seed(tmp_path):
    d = _daisugi(tmp_path)
    _seed_pathway(d.pathway_store, id_="p1")
    _seed_pathway(d.pathway_store, id_="p2")

    server = build_server(d)
    _, structured = await server.call_tool("list_pathways", {})
    rows = structured["result"]
    assert {r["id"] for r in rows} == {"p1", "p2"}
    assert all(r["hit_count"] == 3 for r in rows)


@pytest.mark.asyncio
async def test_pathway_stats(tmp_path):
    d = _daisugi(tmp_path)
    _seed_pathway(d.pathway_store, id_="p1")
    server = build_server(d)
    _, structured = await server.call_tool("pathway_stats", {})
    assert structured == {"count": 1, "total_hits": 3}


@pytest.mark.asyncio
async def test_verify_plan_roundtrip(tmp_path):
    d = _daisugi(tmp_path)
    env = Envelope(
        generated_by="test", task="T",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(source="t", task="T", steps=[ShellStep(id="s1", command="echo hi")])

    server = build_server(d)
    _, structured = await server.call_tool(
        "verify_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json")},
    )
    assert structured["ok"] is True
    assert structured["violations"] == []


@pytest.mark.asyncio
async def test_verify_completed_step_rejects_impersonation(tmp_path):
    from opendaisugi.models import Postcondition
    from opendaisugi.predicate import parse_expression

    d = _daisugi(tmp_path)
    env = Envelope(
        generated_by="test",
        task="send email",
        permissions=Permission(shell=True, shell_allowlist=["send_email"]),
        postconditions=[
            Postcondition(
                type="body_no_impersonation",
                description="body must not sign as Ada",
                expr=parse_expression({
                    "op": "forall_steps",
                    "pred": {
                        "op": "not_matches",
                        "path": "metadata.body",
                        "regex": r"(?i)\bada\b",
                    },
                }),
            )
        ],
    )
    step = ShellStep(
        id="s1",
        command="send_email",
        metadata={"type": "email_send", "body": "— Ada"},
    )

    server = build_server(d)
    _, structured = await server.call_tool(
        "verify_completed_step",
        {"step": step.model_dump(mode="json"), "envelope": env.model_dump(mode="json")},
    )
    assert len(structured["violations"]) == 1
    assert "body_no_impersonation" in structured["violations"][0]["message"]


@pytest.mark.asyncio
async def test_verify_completed_step_accepts_clean(tmp_path):
    from opendaisugi.models import Postcondition
    from opendaisugi.predicate import parse_expression

    d = _daisugi(tmp_path)
    env = Envelope(
        generated_by="test",
        task="send email",
        permissions=Permission(shell=True, shell_allowlist=["send_email"]),
        postconditions=[
            Postcondition(
                type="body_no_impersonation",
                description="body must not sign as Ada",
                expr=parse_expression({
                    "op": "forall_steps",
                    "pred": {
                        "op": "not_matches",
                        "path": "metadata.body",
                        "regex": r"(?i)\bada\b",
                    },
                }),
            )
        ],
    )
    step = ShellStep(
        id="s1",
        command="send_email",
        metadata={"type": "email_send", "body": "— Robin"},
    )

    server = build_server(d)
    _, structured = await server.call_tool(
        "verify_completed_step",
        {"step": step.model_dump(mode="json"), "envelope": env.model_dump(mode="json")},
    )
    assert structured["violations"] == []


@pytest.mark.asyncio
async def test_verify_completed_step_rejects_missing_permissions(tmp_path):
    """Envelope dict without 'permissions' key must error, not silently default."""
    d = _daisugi(tmp_path)
    step = ShellStep(id="s1", command="echo", metadata={"type": "shell_exec"})

    server = build_server(d)
    with pytest.raises(Exception) as exc:
        await server.call_tool(
            "verify_completed_step",
            {
                "step": step.model_dump(mode="json"),
                "envelope": {"generated_by": "x", "task": "y"},
            },
        )
    assert "permissions" in str(exc.value).lower()


@pytest.mark.asyncio
async def test_find_pathway_no_store(tmp_path):
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False)
    server = build_server(d)
    _, structured = await server.call_tool("find_pathway", {"task": "anything"})
    # FastMCP wraps Optional[dict] returning None into {"result": None}
    assert structured == {"result": None}


@pytest.mark.asyncio
async def test_envelope_for_rejects_invalid_stakes(tmp_path):
    server = build_server(_daisugi(tmp_path))
    with pytest.raises(Exception) as exc:
        await server.call_tool(
            "envelope_for", {"task": "ignored", "stakes": "extreme"}
        )
    assert "stakes" in str(exc.value).lower()


# ----- v0.20 new tools -----


@pytest.mark.asyncio
async def test_v020_tools_registered(tmp_path):
    """v0.20 adds run_plan, receipts_for_run, recent_runs to the tool surface."""
    server = build_server(_daisugi(tmp_path))
    names = {t.name for t in await server.list_tools()}
    assert "run_plan" in names
    assert "receipts_for_run" in names
    assert "recent_runs" in names


@pytest.mark.asyncio
async def test_run_plan_executes_and_returns_receipts(tmp_path):
    """run_plan with dry_run=False: validate, run supervisor live, return receipts."""
    d = _daisugi(tmp_path)
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])
    server = build_server(d)
    _, structured = await server.call_tool(
        "run_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json"),
         "dry_run": False},
    )
    assert structured["status"] == "succeeded"
    assert structured["integrity_passed"] is True
    assert len(structured["receipts"]) == 1
    assert structured["receipts"][0]["step_id"] == "s1"


@pytest.mark.asyncio
async def test_run_plan_dry_run_default_does_not_touch_disk(tmp_path):
    """v0.28.2: run_plan defaults to dry_run=True. Even if the LLM authors
    a write step pointing at a real path, nothing should land on disk
    until the caller explicitly flips dry_run=False.
    """
    from opendaisugi.models import FileWriteStep

    d = _daisugi(tmp_path)
    target = tmp_path / "would_have_been_written"
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(file_write=[str(tmp_path / "**")]),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        FileWriteStep(id="s1", path=str(target), content="HACKED"),
    ])
    server = build_server(d)
    _, structured = await server.call_tool(
        "run_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json")},
    )
    assert structured["status"] == "succeeded"
    assert not target.exists(), "dry_run=True should not have created the file"


@pytest.mark.asyncio
async def test_run_plan_dry_run_false_does_touch_disk(tmp_path, monkeypatch):
    """v0.28.2 — proves the dry_run flag actually does something.
    Counterpart to test_run_plan_dry_run_default_does_not_touch_disk.
    Without this, a future refactor that wires DryRunExecutor unconditionally
    would not be caught by the suite.

    SGCM M1: live MCP execution now requires the operator's approval opt-in
    (DAISUGI_APPROVE=always), so this test sets it explicitly.
    """
    from opendaisugi.models import FileWriteStep

    monkeypatch.setenv("DAISUGI_APPROVE", "always")  # operator opts into live exec
    d = _daisugi(tmp_path)
    target = tmp_path / "lived"
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(file_write=[str(tmp_path / "**")]),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        FileWriteStep(id="s1", path=str(target), content="real-write"),
    ])
    server = build_server(d)
    _, structured = await server.call_tool(
        "run_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json"),
         "dry_run": False},
    )
    assert structured["status"] == "succeeded"
    assert target.exists(), "dry_run=False must invoke the live FileWriteExecutor"
    assert target.read_text() == "real-write"


async def test_run_plan_live_denied_without_approval_optin(tmp_path, monkeypatch):
    """SGCM M1: a caller-supplied envelope makes verify() pass by construction, so
    live MCP execution must NOT auto-approve. Without DAISUGI_APPROVE (and no TTY),
    the step is denied and nothing touches disk — closing the confused-deputy bypass.
    """
    from opendaisugi.models import FileWriteStep

    monkeypatch.delenv("DAISUGI_APPROVE", raising=False)
    d = _daisugi(tmp_path)
    target = tmp_path / "should_not_exist"
    env = Envelope(generated_by="attacker", task="t",
                   permissions=Permission(file_write=[str(tmp_path / "**")]))  # self-authored, permissive
    plan = ActionPlan(source="t", task="t",
                      steps=[FileWriteStep(id="s1", path=str(target), content="pwned")])
    server = build_server(d)
    _, structured = await server.call_tool(
        "run_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json"),
         "dry_run": False},
    )
    assert structured["status"] != "succeeded"
    assert not target.exists()  # no arbitrary write via MCP


@pytest.mark.asyncio
async def test_run_plan_propagates_verify_rejection(tmp_path):
    """A plan that fails verify is rejected; receipts list is empty."""
    d = _daisugi(tmp_path)
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["cat"]),  # echo not allowed
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])
    server = build_server(d)
    _, structured = await server.call_tool(
        "run_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json")},
    )
    assert structured["status"] == "rejected"
    assert structured["receipts"] == []


@pytest.mark.asyncio
async def test_receipts_for_run_returns_empty_for_unknown_run(tmp_path):
    server = build_server(_daisugi(tmp_path))
    _, structured = await server.call_tool(
        "receipts_for_run", {"run_id": "nonexistent"},
    )
    # FastMCP wraps list returns as {"result": [...]} in the structured payload.
    rows = structured.get("result", structured)
    if isinstance(rows, dict):
        rows = rows.get("result", [])
    assert rows == []


@pytest.mark.asyncio
async def test_receipts_for_run_returns_real_receipts_after_run_plan(tmp_path):
    """Round-trip: run_plan writes receipts, receipts_for_run reads them back."""
    d = _daisugi(tmp_path)
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])
    server = build_server(d)
    _, run_result = await server.call_tool(
        "run_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json"),
         "dry_run": False},
    )
    run_id = run_result["run_id"]
    _, structured = await server.call_tool(
        "receipts_for_run", {"run_id": run_id},
    )
    rows = structured.get("result", structured)
    if isinstance(rows, dict):
        rows = rows.get("result", [])
    assert len(rows) == 1
    assert rows[0]["step_id"] == "s1"


@pytest.mark.asyncio
async def test_recent_runs_returns_journaled_runs(tmp_path):
    d = _daisugi(tmp_path)
    env = Envelope(
        generated_by="t", task="audit-test",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(source="t", task="audit-test", steps=[
        ShellStep(id="s1", command="echo hi"),
    ])
    server = build_server(d)
    await server.call_tool(
        "run_plan",
        {"plan": plan.model_dump(mode="json"), "envelope": env.model_dump(mode="json"),
         "dry_run": False},
    )
    _, structured = await server.call_tool("recent_runs", {"limit": 5})
    rows = structured.get("result", structured)
    if isinstance(rows, dict):
        rows = rows.get("result", [])
    assert len(rows) >= 1
    assert any("audit-test" in (r.get("task") or "") for r in rows)


def test_cli_mcp_serve_fails_gracefully_without_extra(monkeypatch):
    """If `mcp` isn't importable, `daisugi mcp serve` should exit 1 with a hint."""
    import builtins
    import sys

    from typer.testing import CliRunner

    from opendaisugi.cli import app

    # Poison the mcp_server import so the try/except path fires.
    sys.modules.pop("opendaisugi.mcp_server", None)
    real_import = builtins.__import__

    def _blocked_import(name, *args, **kwargs):
        if name == "opendaisugi.mcp_server" or name.startswith("mcp."):
            raise ImportError("simulated missing mcp extra")
        return real_import(name, *args, **kwargs)

    monkeypatch.setattr(builtins, "__import__", _blocked_import)
    result = CliRunner().invoke(app, ["mcp", "serve"])
    assert result.exit_code == 1
    assert "opendaisugi[mcp]" in result.output
