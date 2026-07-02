"""MCP server exposing openDaisugi as tools for Claude Code, OpenClaw, etc.

A thin FastMCP wrapper over the :class:`Daisugi` facade. Consumers that
already speak MCP (Claude Code, OpenClaw, any MCP-compatible agent) can
call envelope generation, Z3 verification, and pathway lookup as tools
rather than shelling out or re-implementing the library in another
language.

Requires the ``[mcp]`` extra: ``uv add 'opendaisugi[mcp]'``.
"""

from __future__ import annotations

import asyncio
import os
from typing import Any

from opendaisugi import Daisugi
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.stage2 import verify_completed_step as _verify_completed_step

# Default ceiling for run_plan; overridable via OPENDAISUGI_MCP_RUN_TIMEOUT.
# Without this, a hung delegated step blocks the FastMCP stdio transport
# indefinitely — and stdio is single-threaded, so the entire agent session
# calling our server hangs with it.
_DEFAULT_RUN_PLAN_TIMEOUT = float(
    os.environ.get("OPENDAISUGI_MCP_RUN_TIMEOUT", "300")
)


def build_server(daisugi: Daisugi | None = None, *, name: str = "opendaisugi"):
    """Construct a FastMCP server bound to a Daisugi instance.

    Separated from ``serve()`` so tests can drive the server's tool
    handlers directly without spinning up stdio transport.
    """
    from mcp.server.fastmcp import FastMCP

    d = daisugi if daisugi is not None else Daisugi()
    mcp = FastMCP(
        name=name,
        instructions=(
            "openDaisugi provides runtime-assurance primitives for agent "
            "actions: envelope generation with Z3-verified plan constraints, "
            "static verification of plans against envelopes, and a pathway "
            "cache for reusing verified plans across similar tasks."
        ),
    )

    @mcp.tool()
    async def envelope_for(
        task: str,
        stakes: str = "medium",
        context: str | None = None,
    ) -> dict[str, Any]:
        """Generate a verified envelope for a task.

        Args:
            task: Natural-language description of what the agent will do.
            stakes: "low" | "medium" | "high". High stakes uses stricter
                model ladder + thinking budgets.
            context: Optional extra context threaded into the prompt.

        Returns the envelope as a JSON-serializable dict.
        """
        if stakes not in ("low", "medium", "high"):
            raise ValueError(f"stakes must be low|medium|high, got {stakes!r}")
        env = await d.generate_envelope(task, context=context, stakes=stakes)  # type: ignore[arg-type]
        return env.model_dump(mode="json")

    @mcp.tool()
    async def find_pathway(task: str) -> dict[str, Any] | None:
        """Look up a compiled pathway matching this task.

        Returns ``None`` if no pathway is above the similarity threshold
        or the pathway store is disabled. When a match is found, returns
        ``{"similarity": float, "pathway": {...}}`` — callers can use the
        embedded envelope + plan_template directly or call ``adapt_plan``
        on the facade separately.
        """
        match = await d.find_pathway(task)
        if match is None:
            return None
        return {
            "similarity": match.similarity,
            "pathway": match.pathway.model_dump(mode="json"),
        }

    @mcp.tool()
    def verify_plan(plan: dict[str, Any], envelope: dict[str, Any]) -> dict[str, Any]:
        """Statically verify a plan against an envelope.

        Plan and envelope are accepted as dicts (the same shape
        ``envelope_for`` returns) and validated by Pydantic. Returns the
        ``VerificationResult`` as a dict, including violations.
        """
        plan_obj = ActionPlan.model_validate(plan)
        env_obj = Envelope.model_validate(envelope)
        result = d.verify(plan_obj, env_obj)
        return result.model_dump(mode="json")

    @mcp.tool()
    def verify_completed_step(
        step: dict[str, Any], envelope: dict[str, Any]
    ) -> dict[str, Any]:
        """Stage 2: verify a post-execution step against envelope postconditions.

        Call immediately after the step runs and before its effect
        commits externally (SMTP send, HTTP write, file persist). An
        empty ``violations`` list means the step is safe to commit.

        ``step`` is the same discriminated-union shape produced by
        ``ActionStep.model_dump()`` — caller sets ``metadata`` with the
        LLM-generated fields that postconditions will check (``body``,
        ``signature``, etc.). For shell steps with an ``exit_code``
        postcondition (v0.28.3+), caller MUST set ``metadata.rc`` to the
        observed return code — Stage 2 cannot infer it from a step dict.

        ``envelope`` must include an explicit ``permissions`` key. An
        absent key would silently fall back to Permission()'s all-default
        (all-deny) shape, narrowing the policy below what the caller
        intended and passing steps that the real policy would catch.
        """
        if "permissions" not in envelope:
            raise ValueError(
                "envelope dict is missing 'permissions'; pass an explicit "
                "Permission block (all-default is not a safe fallback)"
            )
        # Route through ActionPlan so the discriminated union dispatches
        # the concrete step subclass (ShellStep, EmailStep, …).
        plan = ActionPlan.model_validate(
            {"source": "stage2-mcp", "task": envelope.get("task", ""), "steps": [step]}
        )
        env_obj = Envelope.model_validate(envelope)
        violations = _verify_completed_step(plan.steps[0], env_obj)
        return {"violations": [v.model_dump(mode="json") for v in violations]}

    @mcp.tool()
    def list_pathways() -> list[dict[str, Any]]:
        """Return a summary of all compiled pathways in the store.

        Shape per row: id, task_description, hit_count, distilled_at.
        Full pathway bodies are omitted — call ``find_pathway`` to fetch
        a specific one by task similarity.
        """
        store = d.pathway_store
        if store is None:
            return []
        return [
            {
                "id": p.id,
                "task_description": p.task_description,
                "hit_count": p.hit_count,
                "distilled_at": p.distilled_at,
            }
            for p in store.list_all()
        ]

    @mcp.tool()
    def pathway_stats() -> dict[str, Any]:
        """Return pathway-store counts: total pathways, total hits."""
        store = d.pathway_store
        if store is None:
            return {"count": 0, "total_hits": 0}
        return store.stats()

    @mcp.tool()
    async def run_plan(
        plan: dict[str, Any],
        envelope: dict[str, Any],
        dry_run: bool = True,
    ) -> dict[str, Any]:
        """Execute a plan under supervision and return session + receipts (v0.20).

        End-to-end: the plan and envelope are validated by Pydantic, the
        verifier runs (permissions / Z3 / DAG / predicate / delegation
        guard), and if it passes, the supervisor executes each step under
        the journal's per-step Receipt machinery. The run-end integrity
        check fires automatically; ``integrity_passed`` reflects whether
        every expected step produced a Receipt.

        Args:
            plan: ActionPlan dict.
            envelope: Envelope dict.
            dry_run: When True (default, v0.28.2+), every step routes
                through ``DryRunExecutor`` — nothing touches disk, the
                shell, or the network. When False, the call uses the live
                ``default_executors()`` (SubprocessExecutor, FileWrite,
                NetworkExecutor); approval is still hardcoded to true, so
                only flip this when you trust the upstream LLM and have
                hardened the envelope. Default flipped from live→dry-run
                in v0.28.2 to close the docstring/behavior gap.

        Returns:
            ``{run_id, status, integrity_passed, failed_step_id, receipts}``
            where ``receipts`` is a list of receipt dicts (step_id,
            timestamp, evidence_hash, verify_result, model_id).
        """
        from opendaisugi.approval import CallbackStrategy, default_strategy
        from opendaisugi.executor import dry_run_executor_map
        from opendaisugi.supervisor import Supervisor

        plan_obj = ActionPlan.model_validate(plan)
        env_obj = Envelope.model_validate(envelope)
        # When dry_run, route every step kind through DryRunExecutor —
        # supervisor still runs verify, journal, receipts, integrity check
        # against the dry-run results. This matches what the v0.20→v0.28.1
        # docstring claimed but the code did not deliver (see v0.28.2
        # CHANGELOG entry).
        executors = dry_run_executor_map(plan_obj) if dry_run else None
        # LIVE execution over an MCP surface must NOT auto-approve: the caller
        # supplies BOTH the plan and the envelope, so verify() passes by
        # construction (confused deputy). Use the real approval gate (allowlist /
        # DAISUGI_APPROVE) for live runs — the operator opts into live execution,
        # not a possibly-injected MCP client. Dry-run touches nothing → auto-ok.
        approval = (
            CallbackStrategy(lambda step, env: True) if dry_run else default_strategy()
        )
        sup = Supervisor(
            journal=d.journal,
            approval=approval,
            executors=executors,
        )
        try:
            session = await asyncio.wait_for(
                sup.run(plan_obj, env_obj),
                timeout=_DEFAULT_RUN_PLAN_TIMEOUT,
            )
        except asyncio.TimeoutError:
            return {
                "run_id": None,
                "status": "timeout",
                "integrity_passed": False,
                "failed_step_id": None,
                "receipts": [],
                "error": (
                    f"run_plan exceeded {_DEFAULT_RUN_PLAN_TIMEOUT}s timeout; "
                    f"set OPENDAISUGI_MCP_RUN_TIMEOUT to override."
                ),
            }
        receipts = []
        if d.journal is not None:
            for r in d.journal.receipts_for_run(session.id):
                receipts.append({
                    "step_id": r.step_id,
                    "timestamp": r.timestamp,
                    "evidence_hash": r.evidence_hash,
                    "verify_result": r.verify_result,
                    "model_id": r.model_id,
                })
        return {
            "run_id": session.id,
            "status": session.status.value,
            "integrity_passed": session.integrity_passed,
            "failed_step_id": session.failed_step_id,
            "receipts": receipts,
        }

    @mcp.tool()
    def receipts_for_run(run_id: str) -> list[dict[str, Any]]:
        """Return all per-step receipts for a previous run (v0.20).

        Audits whether a run actually executed every step it claimed to.
        ``model_id`` reflects which model produced the evidence for
        delegated steps (None for non-LLM executors).
        """
        if d.journal is None:
            return []
        return [
            {
                "step_id": r.step_id,
                "run_id": r.run_id,
                "timestamp": r.timestamp,
                "evidence_hash": r.evidence_hash,
                "verify_result": r.verify_result,
                "verify_details": r.verify_details,
                "model_id": r.model_id,
            }
            for r in d.journal.receipts_for_run(run_id)
        ]

    @mcp.tool()
    def recent_runs(limit: int = 20) -> list[dict[str, Any]]:
        """Return recent runs from the journal index (v0.20).

        The discovery surface: an agent (or a human auditor) can find
        what's been done before deciding what to do next, or before
        re-issuing a similar plan. Bounded by ``limit``.
        """
        if d.journal is None:
            return []
        rows = d.journal.list_recent(limit=limit)
        return [
            {
                "run_id": r.id,
                "task": r.task,
                "ok": r.ok,
                "duration_ms": r.duration_ms,
                "created_at": r.created_at,
            }
            for r in rows
        ]

    return mcp


def serve(daisugi: Daisugi | None = None, *, name: str = "opendaisugi") -> None:
    """Run the MCP server over stdio. Blocks until the client disconnects."""
    build_server(daisugi, name=name).run()
