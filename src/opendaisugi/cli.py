"""opendaisugi command-line interface.

Seven commands:

    daisugi generate-envelope "<task>"
    daisugi verify <plan.yaml> --envelope <envelope.yaml>
    daisugi journal stats
    daisugi journal search "<query>"
    daisugi journal replay <trace_id>
    daisugi journal parse <transcript.jsonl> -o <episodes.yaml>
    daisugi journal ingest <episodes.yaml>

Each command accepts ``--data-dir`` (defaults to ``~/.opendaisugi``) and,
where output shape is meaningful, ``--json`` for machine-readable output.
"""

from __future__ import annotations

import asyncio
import json
import os
from dataclasses import asdict
from pathlib import Path

import typer
import yaml

from opendaisugi.approval import default_strategy
from opendaisugi.defaults import DEFAULT_LOW_STAKES_ENVELOPE
from opendaisugi.envelope import generate_envelope
from opendaisugi.exceptions import EnvelopeGenerationError, TaskTooLongError
from opendaisugi.executor import DryRunExecutor, default_executors
from opendaisugi.ingest import ingest_episodes
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.parsers import ParseResult, get_parser
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor
from opendaisugi.verify import verify

app = typer.Typer(
    name="daisugi",
    help="Runtime assurance for agent actions.",
    no_args_is_help=True,
)

journal_app = typer.Typer(
    name="journal",
    help="Inspect and replay journal traces.",
    no_args_is_help=True,
)
app.add_typer(journal_app, name="journal")

pathways_app = typer.Typer(
    name="pathways",
    help="Manage compiled pathways.",
    no_args_is_help=True,
)
app.add_typer(pathways_app, name="pathways")

tiers_app = typer.Typer(
    name="tiers",
    help="Tier-0/1/2 routing stats derived from the journal.",
    no_args_is_help=True,
)
app.add_typer(tiers_app, name="tiers")

gardener_app = typer.Typer(
    name="gardener",
    help="Lifecycle management for compiled pathways (prune, merge, status).",
    no_args_is_help=True,
)
app.add_typer(gardener_app, name="gardener")

lora_app = typer.Typer(
    name="lora",
    help="LoRA training-data pipeline (v0.5.0). Emit JSONL from the journal.",
    no_args_is_help=True,
)
app.add_typer(lora_app, name="lora")

mcp_app = typer.Typer(
    name="mcp",
    help="Run openDaisugi as an MCP server for Claude Code / OpenClaw / any MCP client.",
    no_args_is_help=True,
)
app.add_typer(mcp_app, name="mcp")

hook_app = typer.Typer(
    name="hook",
    help="Passive hook surface — capture tool calls into local JSONL for distillation.",
    no_args_is_help=True,
)
app.add_typer(hook_app, name="hook")

registry_app = typer.Typer(
    name="registry",
    help="Git-backed shared pathway registry (v0.25+).",
    no_args_is_help=True,
)
app.add_typer(registry_app, name="registry")


def _resolve_registry_keys(
    private_key_path: Path | None, public_key_path: Path | None,
) -> tuple[str | None, str | None]:
    """Read base64 ed25519 keypair from disk paths, or return (None, None)."""
    priv = private_key_path.read_text(encoding="utf-8").strip() if private_key_path else None
    pub = public_key_path.read_text(encoding="utf-8").strip() if public_key_path else None
    return priv, pub


@registry_app.command("init")
def registry_init_cmd(
    git_url: str = typer.Argument(..., help="Git URL of the team registry repo."),
    clone_to: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry", "--clone-to",
        help="Local clone directory.",
    ),
) -> None:
    """Clone a registry repo to a local directory.

    Subsequent commands (``pull``, ``publish``, ``status``) operate on
    the local clone. Idempotent — if the clone already exists, this is
    a no-op.
    """
    import subprocess
    if clone_to.exists() and (clone_to / ".git").exists():
        typer.echo(f"already cloned at {clone_to}")
        return
    clone_to.parent.mkdir(parents=True, exist_ok=True)
    typer.echo(f"cloning {git_url} → {clone_to}")
    subprocess.run(["git", "clone", git_url, str(clone_to)], check=True)
    typer.echo(f"clone ready at {clone_to}")


@registry_app.command("pull")
def registry_pull_cmd(
    repo_path: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry", "--repo-path",
    ),
    require_signed: bool = typer.Option(
        True, "--require-signed/--allow-unsigned",
        help="Refuse bundles without a valid signature from a trusted signer.",
    ),
) -> None:
    """git pull and materialize new pathway bundles into the local cache."""
    from opendaisugi.git_pathway_store import GitPathwayStore
    store = GitPathwayStore(repo_path=repo_path, require_signed=require_signed)
    n = store.pull()
    typer.echo(f"pulled; {n} new pathway(s) cached")


@registry_app.command("publish")
def registry_publish_cmd(
    pathway_id: str = typer.Argument(..., help="Local pathway id to publish."),
    repo_path: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry", "--repo-path",
    ),
    private_key: Path = typer.Option(
        ..., "--private-key",
        help="Path to a base64 ed25519 private key file.",
    ),
    public_key: Path = typer.Option(
        ..., "--public-key",
        help="Path to a base64 ed25519 public key file.",
    ),
    publisher: str = typer.Option(
        "opendaisugi-instance", "--publisher",
        help="Human-readable publisher id stamped on the bundle.",
    ),
    push: bool = typer.Option(True, "--push/--no-push"),
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi", "--data-dir",
    ),
) -> None:
    """Sign + commit + push a local pathway as a bundle to the registry."""
    from opendaisugi.git_pathway_store import GitPathwayStore
    from opendaisugi.pathway_store import PathwayStore

    priv, pub = _resolve_registry_keys(private_key, public_key)
    local = PathwayStore(data_dir / "pathways.db")
    pathway = next((p for p in local.list_all() if p.id == pathway_id), None)
    if pathway is None:
        typer.echo(f"error: no pathway {pathway_id} in local store", err=True)
        raise typer.Exit(code=1)
    store = GitPathwayStore(
        repo_path=repo_path,
        private_key_b64=priv, public_key_b64=pub,
        publisher=publisher,
    )
    bundle_hash = store.publish(pathway, push=push)
    typer.echo(bundle_hash)


@registry_app.command("status")
def registry_status_cmd(
    repo_path: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry", "--repo-path",
    ),
) -> None:
    """Show the local clone's diagnostic info."""
    from opendaisugi.git_pathway_store import GitPathwayStore
    store = GitPathwayStore(repo_path=repo_path)
    s = store.status()
    for k, v in s.items():
        typer.echo(f"  {k}: {v}")


@registry_app.command("pull-and-tend")
def registry_pull_and_tend_cmd(
    repo_path: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry", "--repo-path",
    ),
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi", "--data-dir",
    ),
) -> None:
    """Cron-friendly: pull new bundles from the registry, then run tend.

    Pairs with ``daisugi hook auto-tend`` to fully close the
    captures → traces → distillation → publish-eligible-pathways loop
    on every team instance.
    """
    import asyncio
    from opendaisugi.git_pathway_store import GitPathwayStore

    store = GitPathwayStore(repo_path=repo_path)
    n_pulled = store.pull()
    typer.echo(f"pulled; {n_pulled} new pathway(s) cached")

    from opendaisugi import Daisugi
    d = Daisugi(data_dir=data_dir)
    try:
        report = asyncio.run(d.tend())
        typer.echo(
            f"tend: created={report.created} updated={report.updated} "
            f"skipped={report.skipped}"
        )
    except Exception as exc:
        typer.echo(f"tend failed: {type(exc).__name__}: {exc}", err=True)


@hook_app.command("record")
def hook_record_cmd(
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
    fmt: str = typer.Option(
        "claude",
        "--format",
        help="Host runtime stdout contract: claude | codex | hermes | openclaw.",
    ),
) -> None:
    """Read a hook payload from stdin, record it, return the host's continue contract.

    Designed to be wired into Claude Code's PreToolUse hook, Hermes'
    shell-hook surface, OpenClaw's before_tool_call plugin, or any other host
    that emits JSON to stdin and reads JSON from stdout. Never blocks — even
    malformed input results in the host's allow contract so the runtime is
    never disrupted. ``--format`` selects which allow/continue shape to emit.
    """
    import sys
    from opendaisugi.hook import record_and_contract

    try:
        raw = sys.stdin.buffer.read()
    except Exception:
        raw = b""
    # record_and_contract never raises and always returns the host allow contract.
    typer.echo(record_and_contract(raw, root=captures_root, fmt=fmt))


@hook_app.command("list")
def hook_list_cmd(
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
) -> None:
    """List captured sessions with call counts."""
    from opendaisugi.hook import list_sessions

    sessions = list_sessions(root=captures_root)
    if not sessions:
        typer.echo("(no captured sessions)")
        return
    typer.echo(f"{'session_id':40s}  {'calls':>6s}  last_at")
    for s in sessions:
        typer.echo(f"{s['session_id']:40s}  {s['calls']:>6d}  {s.get('last_at', '')}")


@hook_app.command("to-trace")
def hook_to_trace_cmd(
    session_id: str = typer.Argument(..., help="Captured session id (filename stem)."),
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi", "--data-dir",
    ),
    task: str = typer.Option(
        None, "--task",
        help="Override the task description; default uses the session id.",
    ),
) -> None:
    """Convert a captured session into a journal trace.

    Synthesizes a permissive envelope from observed tool heads/paths,
    builds an ActionPlan from the captured calls, runs verify(), and
    appends a Trace to the journal. The trace then feeds normal
    ``daisugi tend`` distillation.
    """
    from opendaisugi.hook import captures_to_trace
    from opendaisugi.journal import Journal

    session_jsonl = captures_root / f"{session_id}.jsonl"
    if not session_jsonl.exists():
        typer.echo(f"error: no capture at {session_jsonl}", err=True)
        raise typer.Exit(code=1)
    journal = Journal(data_dir=data_dir)
    trace_id = captures_to_trace(session_jsonl, journal, task=task)
    journal.mark_session_converted(session_id, trace_id)
    typer.echo(trace_id)


@hook_app.command("auto-tend")
def hook_auto_tend_cmd(
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi", "--data-dir",
    ),
    min_interval_s: int = typer.Option(
        3600, "--min-interval",
        help="Skip if last auto-tend was newer than this many seconds.",
    ),
    force: bool = typer.Option(
        False, "--force", help="Ignore the min-interval gate.",
    ),
    skip_distill: bool = typer.Option(
        False, "--skip-distill",
        help="Convert captures-to-traces but don't run tend afterwards.",
    ),
) -> None:
    """Close the captures→traces→distillation loop in one cron-friendly call.

    For every captured session not already converted, runs `to-trace`. If any
    new traces land, runs ``Daisugi.tend()``. A min-interval gate (default
    1h) prevents wasteful re-runs; --force overrides. Designed for cron /
    systemd timers / Claude `/loop` skill invocations:

        # cron — every 30 minutes; gate ensures real work only every 1h
        */30 * * * * /usr/local/bin/daisugi hook auto-tend

    The thesis-3 reproduction loop is closed by this command. Without it,
    captures accumulate but distillation never sees them.
    """
    import asyncio
    import time

    from opendaisugi.hook import captures_to_trace, list_sessions
    from opendaisugi.journal import Journal

    stamp_file = data_dir / ".hook-auto-tend-last-run"
    now = time.time()
    last_run = 0.0
    if stamp_file.exists():
        try:
            last_run = float(stamp_file.read_text().strip())
        except ValueError:
            last_run = 0.0
    if not force and (now - last_run) < min_interval_s:
        typer.echo(
            f"skipped: last run {int(now - last_run)}s ago "
            f"(< --min-interval={min_interval_s}); use --force to override"
        )
        return

    journal = Journal(data_dir=data_dir)
    sessions = list_sessions(root=captures_root)
    converted: list[str] = []
    for s in sessions:
        sid = s["session_id"]
        if journal.is_session_converted(sid):
            continue
        session_jsonl = captures_root / f"{sid}.jsonl"
        try:
            trace_id = captures_to_trace(session_jsonl, journal)
        except ValueError as exc:
            typer.echo(f"  skipped {sid}: {exc}", err=True)
            continue
        journal.mark_session_converted(sid, trace_id)
        converted.append(trace_id)
        typer.echo(f"  converted {sid} → {trace_id}")

    typer.echo(f"converted {len(converted)} sessions")

    if converted and not skip_distill:
        from opendaisugi import Daisugi
        daisugi = Daisugi(data_dir=data_dir)
        try:
            report = asyncio.run(daisugi.tend())
            typer.echo(
                f"tend: created={report.created} "
                f"updated={report.updated} skipped={report.skipped}"
            )
        except Exception as exc:
            typer.echo(f"tend failed: {type(exc).__name__}: {exc}", err=True)

    data_dir.mkdir(parents=True, exist_ok=True)
    stamp_file.write_text(f"{now}")


@mcp_app.command("serve")
def mcp_serve_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514", "--model",
        help="Model used for envelope generation.",
    ),
) -> None:
    """Serve openDaisugi tools over MCP stdio.

    Exposes envelope_for, find_pathway, verify_plan, list_pathways,
    and pathway_stats. Requires the [mcp] extra:
    ``uv add 'opendaisugi[mcp]'``.
    """
    try:
        from opendaisugi.mcp_server import serve
    except ImportError as e:
        typer.echo(
            "opendaisugi[mcp] is not installed. "
            "Install with: uv add 'opendaisugi[mcp]'",
            err=True,
        )
        raise typer.Exit(1) from e
    from opendaisugi import Daisugi
    serve(Daisugi(model=model, data_dir=data_dir))


@lora_app.command("export")
def lora_export_cmd(
    output: Path = typer.Argument(..., help="Output JSONL file."),
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    fmt: str = typer.Option(
        "alpaca", "--format",
        help="Output format: alpaca (instruction/input/output) or chat (messages).",
    ),
    days: int | None = typer.Option(
        None, "--days",
        help="Only include traces from the last N days. Omit for all time.",
    ),
    min_task_chars: int = typer.Option(
        10, "--min-task-chars",
        help="Skip traces with tasks shorter than this many characters.",
    ),
    system_prompt: str | None = typer.Option(
        None, "--system-prompt",
        help="System prompt injected into chat-format examples.",
    ),
) -> None:
    """Emit (task → envelope JSON) pairs from the journal as JSONL for fine-tuning."""
    import time
    from opendaisugi.lora.dataset import emit_jsonl

    if fmt not in ("alpaca", "chat"):
        typer.echo(f"Unknown format {fmt!r}; expected 'alpaca' or 'chat'.", err=True)
        raise typer.Exit(code=2)

    since = None if days is None else time.time() - days * 86_400
    journal = Journal(data_dir=data_dir)
    stats = emit_jsonl(
        journal, output,
        format=fmt,  # type: ignore[arg-type]
        since=since,
        min_task_chars=min_task_chars,
        system_prompt=system_prompt,
    )
    typer.echo(json.dumps({
        "total": stats.total,
        "written": stats.written,
        "skipped_empty_task": stats.skipped_empty_task,
        "skipped_load_error": stats.skipped_load_error,
        "output_path": stats.output_path,
        "format": fmt,
    }, indent=2))


@tiers_app.command("stats")
def tiers_stats_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    days: int = typer.Option(30, "--days", help="Rollup window (days)."),
    json_output: bool = typer.Option(False, "--json", help="Emit stats as JSON."),
) -> None:
    """Show per-tier call counts, estimated tokens, and pathway hit rate."""
    from opendaisugi.accounting import tier_stats
    journal = Journal(data_dir=data_dir)
    stats = tier_stats(journal, window_days=days)
    if json_output:
        from dataclasses import asdict
        typer.echo(json.dumps(asdict(stats), indent=2))
        return
    typer.echo(f"window: last {stats.window_days}d")
    typer.echo(f"total traces: {stats.total}")
    for tier in ("tier0", "tier1", "tier2"):
        count = stats.by_tier.get(tier, 0)
        tokens = stats.estimated_tokens.get(tier, 0)
        typer.echo(f"  {tier}: {count} call(s)  ~{tokens:,} est tokens")
    typer.echo(f"estimated tokens total: ~{stats.estimated_tokens_total:,}")
    typer.echo(f"pathway hit rate: {stats.pathway_hit_rate:.1%}")
    if stats.by_tier1_provider:
        typer.echo("tier1 breakdown:")
        for name, count in sorted(stats.by_tier1_provider.items(), key=lambda kv: -kv[1]):
            typer.echo(f"  {name}: {count}")


DEFAULT_DATA_DIR = Path.home() / ".opendaisugi"


@pathways_app.command("list")
def pathways_list_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
) -> None:
    """List all compiled pathways."""
    from opendaisugi.pathway_store import PathwayStore
    store = PathwayStore(data_dir / "pathways.db")
    pathways = store.list_all()
    if not pathways:
        typer.echo("No compiled pathways.")
        return
    for p in pathways:
        typer.echo(
            f"{p.id}  hits={p.hit_count}  v{p.version}  {p.task_description}"
        )


@pathways_app.command("show")
def pathways_show_cmd(
    pathway_id: str = typer.Argument(..., help="Pathway id to inspect."),
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
) -> None:
    """Show a compiled pathway in detail."""
    from opendaisugi.pathway_store import PathwayStore
    store = PathwayStore(data_dir / "pathways.db")
    for p in store.list_all():
        if p.id == pathway_id:
            typer.echo(p.model_dump_json(indent=2))
            return
    typer.echo(f"Pathway {pathway_id!r} not found.", err=True)
    raise typer.Exit(code=1)


@pathways_app.command("stats")
def pathways_stats_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    json_output: bool = typer.Option(
        False, "--json", help="Emit stats as JSON."
    ),
) -> None:
    """Summarize stored pathways (count, total hits)."""
    from opendaisugi.pathway_store import PathwayStore
    store = PathwayStore(data_dir / "pathways.db")
    stats = store.stats()
    if json_output:
        typer.echo(json.dumps(stats, indent=2))
        return
    typer.echo(f"count: {stats['count']}")
    typer.echo(f"total_hits: {stats['total_hits']}")


@pathways_app.command("delete")
def pathways_delete_cmd(
    pathway_id: str = typer.Argument(..., help="Pathway id to remove."),
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
) -> None:
    """Delete a compiled pathway."""
    from opendaisugi.pathway_store import PathwayStore
    store = PathwayStore(data_dir / "pathways.db")
    if store.delete(pathway_id):
        typer.echo(f"Deleted {pathway_id}.")
    else:
        typer.echo(f"Pathway {pathway_id!r} not found.", err=True)
        raise typer.Exit(code=1)


@pathways_app.command("export")
def pathways_export_cmd(
    pathway_id: str = typer.Argument(..., help="Pathway id to export."),
    output: Path = typer.Argument(..., help="Output file path."),
    fmt: str = typer.Option(
        "skill", "--format",
        help="Export format: json, skill, mermaid, md, smtlib.",
    ),
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
) -> None:
    """Export a compiled pathway for sharing or inspection."""
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.portability import export as _export, _SUPPORTED_FORMATS

    if fmt not in _SUPPORTED_FORMATS:
        typer.echo(
            f"Unknown format {fmt!r}. Supported: {', '.join(_SUPPORTED_FORMATS)}.",
            err=True,
        )
        raise typer.Exit(code=2)

    store = PathwayStore(data_dir / "pathways.db")
    match = next((p for p in store.list_all() if p.id == pathway_id), None)
    if match is None:
        typer.echo(f"Pathway {pathway_id!r} not found.", err=True)
        raise typer.Exit(code=1)

    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(_export(match, fmt))  # type: ignore[arg-type]
    typer.echo(f"Exported {pathway_id} → {output} ({fmt})")


@pathways_app.command("import")
def pathways_import_cmd(
    source: Path = typer.Argument(..., help="Path to pathway bundle (.json or .md)."),
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    overwrite: bool = typer.Option(
        False, "--overwrite",
        help="Replace an existing pathway with the same ID.",
    ),
    z3_timeout_ms: int = typer.Option(500, "--z3-timeout-ms"),
) -> None:
    """Import a pathway bundle, re-verify, and admit to the PathwayStore."""
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.portability import PathwayImportError, import_pathway

    store = PathwayStore(data_dir / "pathways.db")
    try:
        result = import_pathway(
            source, store,
            z3_timeout_ms=z3_timeout_ms,
            allow_overwrite=overwrite,
        )
    except PathwayImportError as e:
        typer.echo(str(e), err=True)
        raise typer.Exit(code=1) from e

    action = "replaced" if result.overwrote_existing else "imported"
    typer.echo(
        f"{action.capitalize()} pathway {result.pathway.id} "
        f"({result.pathway.task_description[:60]})"
    )


@gardener_app.command("prune")
def gardener_prune_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    max_idle_days: float = typer.Option(30.0, "--max-idle-days"),
    max_failure_ratio: float = typer.Option(0.5, "--max-failure-ratio"),
    min_activations: int = typer.Option(5, "--min-activations"),
    dry_run: bool = typer.Option(False, "--dry-run"),
    json_output: bool = typer.Option(False, "--json"),
) -> None:
    """Evict stale / failure-dominated pathways."""
    from opendaisugi.gardener import PruneConfig, prune
    from opendaisugi.pathway_store import PathwayStore

    store = PathwayStore(data_dir / "pathways.db")
    cfg = PruneConfig(
        max_idle_days=max_idle_days,
        max_failure_ratio=max_failure_ratio,
        min_activations_before_prune=min_activations,
    )
    report = prune(store, cfg, dry_run=dry_run)
    if json_output:
        typer.echo(json.dumps({
            "removed_ids": report.removed_ids,
            "kept_count": report.kept_count,
            "reasons": report.reasons,
            "dry_run": dry_run,
        }, indent=2))
        return
    verb = "would remove" if dry_run else "removed"
    typer.echo(f"{verb}: {report.removed_count} (kept: {report.kept_count})")
    for pid in report.removed_ids:
        typer.echo(f"  {pid} — {report.reasons.get(pid, '')}")


@gardener_app.command("merge")
def gardener_merge_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    similarity: float = typer.Option(0.92, "--similarity"),
    dry_run: bool = typer.Option(False, "--dry-run"),
    json_output: bool = typer.Option(False, "--json"),
) -> None:
    """Collapse near-duplicate pathways."""
    from opendaisugi.gardener import MergeConfig, merge
    from opendaisugi.pathway_store import PathwayStore

    store = PathwayStore(data_dir / "pathways.db")
    cfg = MergeConfig(similarity_threshold=similarity)
    report = merge(store, cfg, dry_run=dry_run)
    if json_output:
        typer.echo(json.dumps({
            "merged_pairs": report.merged_pairs,
            "kept_ids": report.kept_ids,
            "removed_ids": report.removed_ids,
            "dry_run": dry_run,
        }, indent=2))
        return
    verb = "would merge" if dry_run else "merged"
    typer.echo(f"{verb}: {report.merge_count} pair(s)")
    for winner, loser in report.merged_pairs:
        typer.echo(f"  {winner}  <-  {loser}")


@gardener_app.command("run")
def gardener_run_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    dry_run: bool = typer.Option(False, "--dry-run"),
    json_output: bool = typer.Option(False, "--json"),
) -> None:
    """Run the full gardener pipeline (prune + merge)."""
    from opendaisugi.gardener import run_gardener
    from opendaisugi.pathway_store import PathwayStore

    store = PathwayStore(data_dir / "pathways.db")
    report = run_gardener(store, dry_run=dry_run)
    if json_output:
        typer.echo(json.dumps({
            "prune": {
                "removed_ids": report.prune.removed_ids,
                "kept_count": report.prune.kept_count,
                "reasons": report.prune.reasons,
            },
            "merge": {
                "merged_pairs": report.merge.merged_pairs,
                "kept_ids": report.merge.kept_ids,
            },
            "dry_run": dry_run,
        }, indent=2))
        return
    verb = "would" if dry_run else ""
    typer.echo(f"prune: {verb} removed {report.prune.removed_count}, "
               f"kept {report.prune.kept_count}")
    typer.echo(f"merge: {verb} merged {report.merge.merge_count} pair(s)")


@gardener_app.command("watch")
def gardener_watch_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    min_interval_s: int = typer.Option(
        3600, "--min-interval",
        help="Skip if the last run is newer than this many seconds.",
    ),
    force: bool = typer.Option(
        False, "--force", help="Ignore the min-interval check."
    ),
    dry_run: bool = typer.Option(False, "--dry-run"),
) -> None:
    """Cron-friendly one-shot gardener. Skips if last run is within --min-interval.

    Designed to be invoked by cron/systemd-timer every few minutes — the
    --min-interval gate prevents running more often than desired regardless
    of scheduler granularity. Writes a timestamp file to --data-dir so the
    gate survives restarts.
    """
    import time
    from opendaisugi.gardener import run_gardener
    from opendaisugi.pathway_store import PathwayStore

    stamp_file = data_dir / ".gardener-last-run"
    now = time.time()
    last_run = 0.0
    if stamp_file.exists():
        try:
            last_run = float(stamp_file.read_text().strip())
        except ValueError:
            last_run = 0.0

    elapsed = now - last_run
    if not force and elapsed < min_interval_s:
        payload = {
            "skipped": True,
            "reason": "min_interval_not_elapsed",
            "elapsed_s": round(elapsed, 1),
            "min_interval_s": min_interval_s,
        }
        typer.echo(json.dumps(payload))
        return

    store = PathwayStore(data_dir / "pathways.db")
    report = run_gardener(store, dry_run=dry_run)

    if not dry_run:
        data_dir.mkdir(parents=True, exist_ok=True)
        stamp_file.write_text(f"{now:.3f}")

    typer.echo(json.dumps({
        "skipped": False,
        "ran_at": now,
        "dry_run": dry_run,
        "prune": {
            "removed": report.prune.removed_count,
            "kept": report.prune.kept_count,
        },
        "merge": {
            "merged": report.merge.merge_count,
        },
    }))


@gardener_app.command("status")
def gardener_status_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    json_output: bool = typer.Option(False, "--json"),
) -> None:
    """Report current store size, pathway activation stats, failure ratios."""
    from opendaisugi.pathway_store import PathwayStore

    store = PathwayStore(data_dir / "pathways.db")
    pathways = store.list_all()
    payload = {
        "count": len(pathways),
        "pathways": [
            {
                "id": p.id,
                "hit_count": p.hit_count,
                "failure_count": p.failure_count,
                "last_activation_at": p.last_activation_at,
            }
            for p in pathways
        ],
    }
    if json_output:
        typer.echo(json.dumps(payload, indent=2))
        return
    typer.echo(f"count: {payload['count']}")
    for p in pathways:
        total = p.hit_count + p.failure_count
        ratio = (p.failure_count / total) if total else 0.0
        typer.echo(
            f"  {p.id}  hits={p.hit_count}  fails={p.failure_count}  "
            f"fail_ratio={ratio:.2f}"
        )


def _serialize_session(session) -> dict:
    payload = asdict(session)
    payload["status"] = session.status.value
    payload["verification"] = session.verification.model_dump(mode="json")
    return payload


@app.command("run")
def run_cmd(
    plan_path: Path = typer.Argument(..., exists=True, readable=True,
                                     help="Path to plan YAML."),
    envelope_path: Path = typer.Option(..., "--envelope", "-e",
                                       exists=True, readable=True,
                                       help="Path to envelope YAML."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir",
                                  help="Root data directory for the journal."),
    dry_run: bool = typer.Option(False, "--dry-run",
                                 help="Use DryRunExecutor — no real subprocesses."),
    yes: bool = typer.Option(False, "--yes", "-y",
                             help="Auto-approve every step "
                                  "(sets DAISUGI_APPROVE=always for this run)."),
    json_output: bool = typer.Option(False, "--json",
                                     help="Emit the run session as JSON on stdout."),
) -> None:
    """Execute PLAN against ENVELOPE under runtime supervision.

    Exit codes: 0 succeeded, 1 failed, 2 verify rejected, 130 aborted.
    """
    envelope = Envelope(**yaml.safe_load(envelope_path.read_text()))
    plan = ActionPlan(**yaml.safe_load(plan_path.read_text()))

    if yes:
        os.environ["DAISUGI_APPROVE"] = "always"

    if dry_run:
        typer.echo("Dry run — no real subprocesses will be spawned")

    if dry_run:
        dry = DryRunExecutor()
        executors = {
            "shell": dry,
            "file_read": dry,
            "file_write": dry,
            "network": dry,
        }
    else:
        executors = default_executors()
    approval = default_strategy()
    journal = Journal(data_dir=data_dir)
    supervisor = Supervisor(
        executors=executors,
        approval=approval,
        journal=journal,
    )

    try:
        session = asyncio.run(supervisor.run(plan, envelope))
    except KeyboardInterrupt:
        typer.echo("Aborted.", err=True)
        raise typer.Exit(code=130)

    if json_output:
        typer.echo(json.dumps(_serialize_session(session), indent=2, default=str))
    else:
        typer.echo(f"Run {session.id} ({session.status.value})")
        for outcome in session.steps:
            line = (f"  {outcome.step_id}: {outcome.status} "
                    f"(rc={outcome.rc}, approved_by={outcome.approved_by}, "
                    f"{outcome.duration_ms:.1f} ms)")
            typer.echo(line)
            if outcome.stdout:
                for out_line in outcome.stdout.rstrip().splitlines()[:5]:
                    typer.echo(f"      {out_line}")
        if session.trace_id:
            typer.echo(f"Journal: {session.trace_id}")

    if session.status == RunStatus.SUCCEEDED:
        raise typer.Exit(code=0)
    if session.status == RunStatus.REJECTED:
        typer.echo("Plan rejected by runtime verify():", err=True)
        for v in session.verification.violations:
            typer.echo(f"  {v.stage}: {v.message}", err=True)
        raise typer.Exit(code=2)
    if session.status == RunStatus.ABORTED:
        raise typer.Exit(code=130)
    raise typer.Exit(code=1)


_VALID_STAKES = {"low", "medium", "high"}
_VALID_THINKING_BUDGETS = {"light", "standard", "deep"}


@app.command("generate-envelope")
def generate_envelope_cmd(
    task: str = typer.Argument(..., help="Task description to envelope."),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514", "--model",
        help="LLM model (litellm provider/model format).",
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir",
        help="Root data directory. Unused by this command but accepted for consistency.",
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Emit JSON instead of YAML."
    ),
    stakes: str = typer.Option(
        "medium", "--stakes",
        help="Stakes level: low (uses default), medium (cache), high (always fresh).",
    ),
    low_stakes_envelope: Path | None = typer.Option(
        None, "--low-stakes-envelope",
        help="Path to a JSON Envelope file; used when --stakes low is set.",
    ),
    thinking_budget: str = typer.Option(
        "standard", "--thinking-budget",
        help="Thinking budget: light, standard, deep (mapped per provider).",
    ),
    llm: str = typer.Option(
        "litellm", "--llm",
        help="LLM backend: 'litellm' (default) or 'claude-code' (uses claude -p subprocess).",
    ),
) -> None:
    """Generate a safety envelope for TASK via an LLM."""
    if llm not in {"litellm", "claude-code"}:
        typer.echo(
            f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.",
            err=True,
        )
        raise typer.Exit(code=2)
    if llm != "litellm":
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm
    if stakes not in _VALID_STAKES:
        typer.echo(
            f"Invalid --stakes value {stakes!r}. Must be one of: {', '.join(sorted(_VALID_STAKES))}",
            err=True,
        )
        raise typer.Exit(code=2)
    if thinking_budget not in _VALID_THINKING_BUDGETS:
        typer.echo(
            f"Invalid --thinking-budget value {thinking_budget!r}. "
            f"Must be one of: {', '.join(sorted(_VALID_THINKING_BUDGETS))}",
            err=True,
        )
        raise typer.Exit(code=2)

    if stakes == "low" and low_stakes_envelope is not None:
        low_env: Envelope | None = Envelope.model_validate_json(low_stakes_envelope.read_text())
    elif stakes == "low":
        low_env = DEFAULT_LOW_STAKES_ENVELOPE
    else:
        low_env = None

    try:
        envelope = asyncio.run(
            generate_envelope(
                task=task,
                model=model,
                stakes=stakes,
                low_stakes_envelope=low_env,
                thinking_budget=thinking_budget,
            )
        )
    except TaskTooLongError as e:
        typer.echo(str(e), err=True)
        raise typer.Exit(code=2) from e
    except EnvelopeGenerationError as e:
        typer.echo(f"Envelope generation failed: {e}", err=True)
        raise typer.Exit(code=2) from e
    payload = envelope.model_dump(mode="json")
    if json_output:
        typer.echo(json.dumps(payload, indent=2))
    else:
        typer.echo(yaml.safe_dump(payload, sort_keys=False).rstrip())


@app.command("verify")
def verify_cmd(
    plan_path: Path = typer.Argument(
        ..., exists=True, dir_okay=False, readable=True,
        help="Path to a YAML file containing a serialized ActionPlan.",
    ),
    envelope_path: Path = typer.Option(
        ..., "--envelope", exists=True, dir_okay=False, readable=True,
        help="Path to a YAML file containing a serialized Envelope.",
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Emit VerificationResult as JSON."
    ),
) -> None:
    """Verify an action plan against a safety envelope."""
    try:
        plan_raw = yaml.safe_load(plan_path.read_text())
        env_raw = yaml.safe_load(envelope_path.read_text())
    except yaml.YAMLError as e:
        typer.echo(f"Invalid YAML: {e}", err=True)
        raise typer.Exit(code=2) from e
    plan = ActionPlan(**plan_raw)
    envelope = Envelope(**env_raw)

    result = verify(plan, envelope)

    if json_output:
        typer.echo(json.dumps(result.model_dump(mode="json"), indent=2))
    else:
        status = "OK" if result.ok else "FAILED"
        typer.echo(f"Verification: {status}")
        typer.echo(f"  plan:     {result.plan_id}")
        typer.echo(f"  envelope: {result.envelope_id}")
        typer.echo(f"  duration: {result.duration_ms:.2f}ms")
        if result.violations:
            typer.echo("  violations:")
            for v in result.violations:
                typer.echo(f"    - [{v.stage}] {v.message}")
        if result.warnings:
            typer.echo("  warnings:")
            for w in result.warnings:
                typer.echo(f"    - {w}")

    raise typer.Exit(code=0 if result.ok else 1)


@app.command("tend")
def tend_cmd(
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi", "--data-dir",
        help="Daisugi data directory.",
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514", "--model",
        help="Model used for template generalization + improvement.",
    ),
    min_traces: int = typer.Option(3, "--min-traces", help="Minimum cluster size to distill."),
    lookback_days: int = typer.Option(30, "--lookback-days", help="How far back to scan the journal."),
    dry_run: bool = typer.Option(False, "--dry-run", help="Run the pipeline but do not store pathways."),
) -> None:
    """Run the distiller. Scans successful traces and produces compiled pathways."""
    import asyncio
    from opendaisugi import Daisugi
    from opendaisugi.local_setup import load_configured_tier1

    tier1 = load_configured_tier1(data_dir)  # defer distillation LLM calls to a wired local model

    # Dry-run uses an in-memory SQLite so no bytes land in the user's data
    # dir; the pathway rows are discarded at process exit.
    if dry_run:
        from opendaisugi.pathway_store import PathwayStore
        dry_store = PathwayStore(":memory:")
        d = Daisugi(data_dir=data_dir, model=model, pathway_store=dry_store, tier1=tier1)
    else:
        d = Daisugi(data_dir=data_dir, model=model, pathway_store=True, tier1=tier1)

    report = asyncio.run(d.tend(min_traces=min_traces, lookback_days=lookback_days))
    typer.echo(
        f"tend complete: created={report.created} updated={report.updated} "
        f"skipped={report.skipped} in {report.duration_s:.1f}s"
    )
    if report.pathways:
        typer.echo(f"  {len(report.pathways)} pathway(s): {', '.join(report.pathways)}")
    for w in report.warnings:
        typer.echo(f"  warning: {w}")


@app.command("onboard")
def onboard_cmd(
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514", "--model",
        help="Model for envelope generation + distillation.",
    ),
    llm: str = typer.Option(
        "litellm", "--llm",
        help="LLM backend: 'litellm' (default) or 'claude-code' (no API key — uses your Claude Code subscription).",
    ),
    limit: int = typer.Option(
        None, "--limit", help="Process only the N most recent transcripts."
    ),
    harness: list[str] = typer.Option(
        None, "--harness",
        help="Only process these harnesses (repeatable). Default: all discovered.",
    ),
    concurrency: int = typer.Option(
        5, "--concurrency", help="Max parallel envelope-generation calls."
    ),
    min_tools: int = typer.Option(3, "--min-tools", help="Merge episodes below this tool-call count."),
    max_tools: int = typer.Option(30, "--max-tools", help="LLM-split episodes above this tool-call count."),
    min_traces: int = typer.Option(3, "--min-traces", help="Minimum cluster size to distill a pathway."),
    lookback_days: int = typer.Option(
        3650, "--lookback-days",
        help="How far back to scan ingested traces when distilling (default: all history).",
    ),
    threshold: float = typer.Option(
        DEFAULT_PATHWAY_THRESHOLD, "--threshold",
        help="Pathway clustering/retrieval similarity threshold (0-1).",
    ),
    dry_run: bool = typer.Option(
        False, "--dry-run", help="Discover + report only; no LLM calls, no pathways written."
    ),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Turn existing conversations into token-saving pathways — the day-one flow.

    Discovers your existing agent transcripts (Claude Code, Codex, ...), replays
    them into the verified journal, and distills reusable pathways so that from
    today matching tasks skip envelope generation (token savings) and every
    replayed action is verified (trust).
    """
    if llm not in {"litellm", "claude-code"}:
        typer.echo(f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.", err=True)
        raise typer.Exit(code=2)
    if llm != "litellm":
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm

    from opendaisugi.onboarding import DiscoveredTranscript, onboard
    from opendaisugi.local_setup import load_configured_tier1

    journal = Journal(data_dir=data_dir)
    tier1 = load_configured_tier1(data_dir)  # defer bulk envelope-gen to a qualified local model if wired

    def parse_one(t: "DiscoveredTranscript"):
        # The harness id doubles as the parser format name (claude-code, ...).
        try:
            parser = get_parser(t.harness, min_tools=min_tools, max_tools=max_tools, model=model)
        except ValueError:
            return None  # no parser registered for this harness — onboard skips + warns
        return parser.parse(t.path)

    async def ingest_one(parse_result):
        return await ingest_episodes(
            parse_result, journal, concurrency=concurrency, model=model,
            dry_run=dry_run, tier1=tier1,
        )

    async def run_tend():
        from opendaisugi import Daisugi

        d = Daisugi(
            data_dir=data_dir, model=model, pathway_store=True,
            pathway_threshold=threshold, tier1=tier1,
        )
        return await d.tend(
            min_traces=min_traces,
            lookback_days=lookback_days,
            similarity_threshold=threshold,
        )

    report = asyncio.run(
        onboard(
            parse_one=parse_one,
            ingest_one=ingest_one,
            run_tend=run_tend,
            harnesses=list(harness) if harness else None,
            limit=limit,
            dry_run=dry_run,
            progress=None if json_output else (lambda m: typer.echo(m)),
        )
    )

    if json_output:
        typer.echo(json.dumps(report.__dict__, indent=2, default=str))
        return

    typer.echo("")
    by = ", ".join(f"{k}: {v}" for k, v in sorted(report.by_harness.items())) or "—"
    typer.echo(
        f"Discovered {report.transcripts_found} transcript(s); "
        f"processed {report.transcripts_processed} ({by})."
    )
    typer.echo(
        f"Journal: {report.traces_passed} verified, {report.traces_failed} failed, "
        f"{report.traces_skipped} already present."
    )
    if dry_run:
        typer.echo("Dry run — no pathways written. Re-run without --dry-run to distill.")
    else:
        typer.echo(
            f"Pathways: {report.pathways_created} new, {report.pathways_updated} updated."
        )
        if report.pathways_created or report.pathways_updated:
            typer.echo(
                "  → Token routing is live: matching tasks now skip envelope generation (Tier-0)."
            )
        typer.echo(
            f"  → Trust: replay any action with `daisugi journal replay <id>`; "
            f"journal at {data_dir / 'journal'}."
        )
    for w in report.warnings:
        typer.echo(f"  warning: {w}")


@app.command("route")
def route_cmd(
    task: str = typer.Argument(..., help="The task to get a routing recommendation for."),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory (pathway store)."
    ),
    cheap_model: str = typer.Option(
        "claude-haiku-4-5", "--cheap-model", help="Model recommended for easy tasks."
    ),
    frontier_model: str = typer.Option(
        "claude-opus-4-8", "--frontier-model", help="Model recommended for hard tasks."
    ),
    threshold: float = typer.Option(
        DEFAULT_PATHWAY_THRESHOLD, "--threshold", help="Pathway-match threshold (0-1)."
    ),
    harness: str = typer.Option(
        "claude-code", "--harness",
        help="Host harness: claude-code, codex, ollama/local, hermes, openclaw. "
             "The Anthropic advisor-tool pairing is only suggested on Claude/Anthropic harnesses.",
    ),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Recommend the cheapest viable model/tier for a task.

    A repeat task that matches a distilled pathway routes to Tier-0 (reuse —
    near-free and re-verified); an easy novel task to a cheap model; a hard novel
    task to the frontier (or, on a Claude/Anthropic harness, the advisor-tool pairing).
    """
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.routing import RouteAdvisor, advisor_tool_available_for_harness

    db = data_dir / "pathways.db"
    store = PathwayStore(db) if db.exists() else None
    advisor = RouteAdvisor(
        pathway_store=store,
        cheap_model=cheap_model,
        frontier_model=frontier_model,
        threshold=threshold,
        advisor_tool_available=advisor_tool_available_for_harness(harness),
    )
    advice = advisor.advise(task)

    if json_output:
        typer.echo(json.dumps(advice.__dict__, indent=2))
        return

    typer.echo(f"route: {advice.tier}" + (f"  →  {advice.model}" if advice.model else ""))
    typer.echo(f"  difficulty: {advice.difficulty:.2f}")
    if advice.pathway_id:
        typer.echo(f"  pathway:    {advice.pathway_id}")
    typer.echo(f"  why:        {advice.reason}")


@app.command("orchestrate")
def orchestrate_cmd(
    prompt: str = typer.Argument(..., help="The prompt to run end to end."),
    envelope_path: "Path | None" = typer.Option(
        None, "--envelope", "-e",
        help="Envelope YAML (authorization boundary). If omitted, one is generated for the prompt.",
    ),
    budget: "int | None" = typer.Option(
        None, "--budget", "-b",
        help="Approximate token budget for the run (gates routing during execution). Omit for unbudgeted.",
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514", "--model",
        help="Model used to decompose the prompt (and generate the envelope if none is given).",
    ),
    llm: str = typer.Option(
        "litellm", "--llm",
        help="LLM backend: 'litellm' (needs ANTHROPIC_API_KEY) or 'claude-code' "
             "(no API key — uses your Claude Code subscription via a claude -p subprocess).",
    ),
    stakes: str = typer.Option("medium", "--stakes", help="Stakes for a generated envelope: low|medium|high."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data dir (pathway store + journal)."),
    json_output: bool = typer.Option(False, "--json", help="Emit the orchestration result as JSON."),
) -> None:
    """Run PROMPT end to end: decompose → size → supervised execute → synthesize.

    The decomposed plan is verified against the envelope before it runs and each
    step is re-verified at execution time; each step routes to the cheapest capable
    model within the token budget. Repeat prompts may reuse a distilled pathway.
    """
    from opendaisugi import Daisugi

    if stakes not in _VALID_STAKES:
        typer.echo(f"Invalid --stakes {stakes!r}; choose from {sorted(_VALID_STAKES)}.", err=True)
        raise typer.Exit(code=2)
    if llm not in {"litellm", "claude-code"}:
        typer.echo(f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.", err=True)
        raise typer.Exit(code=2)
    if llm != "litellm":
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm

    envelope = None
    if envelope_path is not None:
        envelope = Envelope(**yaml.safe_load(envelope_path.read_text()))

    d = Daisugi(model=model, data_dir=data_dir)
    try:
        result = asyncio.run(d.orchestrate(
            prompt, envelope=envelope, budget_tokens=budget, stakes=stakes,
        ))
    except EnvelopeGenerationError as e:
        typer.echo(f"Envelope generation failed: {e}", err=True)
        raise typer.Exit(code=1)
    except Exception as e:  # decomposition/out-of-policy/other — surface cleanly
        typer.echo(f"Orchestration failed: {type(e).__name__}: {e}", err=True)
        raise typer.Exit(code=1)

    if json_output:
        payload = {
            "prompt": result.prompt,
            "status": result.status,
            "final_answer": result.final_answer,
            "reused_pathway": result.reused_pathway,
            "used_llm_synthesis": result.used_llm_synthesis,
            "budget": asdict(result.budget),
            "sizings": [asdict(s) for s in result.sizings],
            "plan": result.plan.model_dump(mode="json"),
        }
        typer.echo(json.dumps(payload, indent=2, default=str))
    else:
        typer.echo(result.final_answer)
        typer.echo("")
        typer.echo(f"— orchestration ({result.status}"
                   + (", reused pathway" if result.reused_pathway else "") + ") —")
        for s in result.sizings:
            typer.echo(f"  {s.step_id}: difficulty={s.difficulty:.2f} → {s.tier} ({s.model})"
                       + ("  [downgraded]" if s.downgraded else ""))
        b = result.budget
        spent = f"{b.spent}" + (f"/{b.total}" if b.total is not None else "")
        typer.echo(f"  budget: {spent} tokens spent across {b.step_count} model call(s)")

    if result.status != "succeeded":
        raise typer.Exit(code=1)


@app.command("models")
def models_cmd(
    repo: str = typer.Argument(
        None,
        help="Trusted HF repo to resolve+pin (e.g. mozilla-ai/Qwen2.5-0.5B-Instruct-llamafile). Omit to discover.",
    ),
    suffix: str = typer.Option(".llamafile", "--suffix", help="File suffix to resolve (.llamafile or .gguf)."),
    pull: bool = typer.Option(False, "--pull", help="Download the resolved file (pinned to its commit)."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Discover or resolve a trustworthy, commit-pinned local model from the Hub.

    No repo → list trusted llamafile repos. With a repo → resolve it to a pinned
    reference (trusted-org allowlist + list-first, never a guessed filename);
    --pull downloads it at the pinned commit.
    """
    from opendaisugi import model_registry as mr

    if repo is None:
        repos = mr.discover_llamafiles()
        if json_output:
            typer.echo(json.dumps({"trusted_repos": repos}, indent=2))
            return
        typer.echo(f"Trusted llamafile repos on the Hub ({len(repos)}):")
        for r in repos:
            typer.echo(f"  {r}")
        typer.echo("\nResolve one to a pinned ref:  daisugi models <repo-id>")
        return

    try:
        ref = mr.resolve_pinned(repo, suffix=suffix)
    except (mr.UntrustedSource, mr.NoMatchingFile) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=2)

    path = None
    if pull:
        path = mr.download_pinned(ref, allow_download=True)

    if json_output:
        typer.echo(json.dumps(
            {"repo_id": ref.repo_id, "filename": ref.filename, "revision": ref.revision,
             "downloaded_path": path}, indent=2))
        return
    typer.echo(f"repo:     {ref.repo_id}")
    typer.echo(f"file:     {ref.filename}")
    typer.echo(f"revision: {ref.revision}   (immutable commit — reproducible)")
    if path:
        typer.echo(f"pulled:   {path}")
    else:
        typer.echo("\nDownload it (pinned):  daisugi models {} --pull".format(repo))


@app.command("quickstart")
def quickstart_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """One-stop coworker orientation: your hardware → a local model → your day-one commands.

    Guided only — it detects your machine, counts the transcripts it would distill,
    and prints the exact command sequence. It does NOT spend LLM tokens or write
    anything; you run `daisugi onboard` when ready.
    """
    from collections import Counter

    from opendaisugi.hardware import detect_hardware, recommend_model
    from opendaisugi.local_setup import load_configured_tier1
    from opendaisugi.onboarding import discover_transcripts, gather_status

    profile = detect_hardware()
    rec = recommend_model(profile)
    transcripts = discover_transcripts()
    by_harness = Counter(t.harness for t in transcripts)
    rep = gather_status(data_dir)
    tier1 = load_configured_tier1(data_dir)

    typer.echo("openDaisugi quickstart — your day-one path\n")
    typer.echo(f"1. Hardware: {profile.system}/{profile.arch}, ~{profile.model_budget_gb}GB model budget")
    if tier1 is not None:
        typer.echo(f"   ✓ local model already wired: {getattr(tier1, 'model', '?')}")
    else:
        typer.echo(f"   → recommended local model: a {rec.size_class}-class model at {rec.quant} "
                   f"via {rec.runtime} (provisional — qualify on your box). Run `daisugi setup`.")
    by = ", ".join(f"{k}: {v}" for k, v in sorted(by_harness.items())) or "none found"
    typer.echo(f"\n2. Existing transcripts discovered: {len(transcripts)} ({by})")
    typer.echo(f"   journal so far: {rep.journal_total} traces, {rep.pathway_count} pathways")

    typer.echo("\n3. Run these, in order:")
    if not rep.search_extra_installed:
        typer.echo("   uv add 'opendaisugi[search]'  # pathways need this")
    if tier1 is None:
        typer.echo("   daisugi setup                 # then start the recommended llamafile, then:")
        typer.echo("   daisugi setup --endpoint http://localhost:8080/v1 --model <name> --wire")
    typer.echo("   daisugi onboard --dry-run     # preview what would be distilled")
    typer.echo("   daisugi onboard               # distill your existing convos into pathways")
    typer.echo("   daisugi status                # confirm token savings live + journal verified")
    typer.echo('   daisugi route "<a task>"      # cheapest viable model/tier for a task')


@app.command("setup")
def setup_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    endpoint: str = typer.Option(
        None, "--endpoint",
        help="OpenAI-compatible local /v1 URL to qualify (e.g. http://localhost:8080/v1).",
    ),
    model: str = typer.Option(
        None, "--model", help="Model name served by --endpoint (required with --endpoint)."
    ),
    threshold: float = typer.Option(0.8, "--threshold", help="Min valid-envelope pass rate to promote."),
    repeats: int = typer.Option(1, "--repeats", help="Sample each probe task N times."),
    wire: bool = typer.Option(False, "--wire", help="Persist the model as Tier-1 if it qualifies."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Detect hardware, recommend a hardware-appropriate local model, and (optionally) qualify + wire it.

    With no --endpoint: prints the hardware profile, a size-appropriate llamafile
    recommendation, and the commands to get a local server running. With
    --endpoint + --model: runs the qualification gate against the live model and,
    with --wire, persists it as Tier-1 only if it clears the pass-rate threshold.
    """
    from opendaisugi.hardware import detect_hardware, recommend_model
    from opendaisugi.local_setup import qualify_local_model, write_tier1_config

    if endpoint and not model:
        typer.echo("--model is required with --endpoint (the model name the local server serves).", err=True)
        raise typer.Exit(code=2)

    profile = detect_hardware()
    rec = recommend_model(profile)

    qual = None
    wired = False
    if endpoint:
        from opendaisugi.tier1 import LiteLLMTier1Provider

        provider = LiteLLMTier1Provider(model=model, base_url=endpoint)
        qual = asyncio.run(qualify_local_model(provider, threshold=threshold, repeats=repeats))
        if qual.passed and wire:
            write_tier1_config(data_dir, model=model, base_url=endpoint)
            wired = True

    if json_output:
        payload = {
            "hardware": {
                "system": profile.system, "arch": profile.arch, "cpu_count": profile.cpu_count,
                "ram_gb": profile.ram_gb, "vram_gb": profile.vram_gb, "gpu_name": profile.gpu_name,
                "unified_memory": profile.unified_memory, "model_budget_gb": profile.model_budget_gb,
            },
            "recommendation": {
                "size_class": rec.size_class, "params_b_max": rec.params_b_max, "quant": rec.quant,
                "runtime": rec.runtime, "est_download_gb": rec.est_download_gb,
                "candidate_families": rec.candidate_families, "provisional": rec.provisional,
                "rationale": rec.rationale,
            },
            "qualification": None if qual is None else {
                "attempts": qual.attempts, "valid": qual.valid, "pass_rate": qual.pass_rate,
                "passed": qual.passed, "threshold": qual.threshold, "wired": wired,
            },
        }
        typer.echo(json.dumps(payload, indent=2))
        return

    typer.echo(f"Hardware: {profile.system}/{profile.arch}, {profile.cpu_count} CPU"
               + (f", {profile.ram_gb}GB RAM" if profile.ram_gb else ", RAM undetected")
               + (f", {profile.vram_gb}GB VRAM ({profile.gpu_name})" if profile.has_discrete_gpu else ", no discrete GPU"))
    typer.echo(f"Model budget: ~{profile.model_budget_gb}GB")
    typer.echo("")
    typer.echo(f"Recommended: a {rec.size_class}-class instruct model at {rec.quant} "
               f"via {rec.runtime} (~{rec.est_download_gb}GB).")
    typer.echo(f"  candidate families (your pick, none verified-best): {', '.join(rec.candidate_families)}")
    typer.echo(f"  {rec.rationale}")
    typer.echo("")
    if qual is None:
        typer.echo("Get a local server running (one file, no install), then qualify + wire it:")
        typer.echo("  1. Find a trusted, commit-pinned model llamafile:  daisugi models")
        typer.echo("     (canonical engine repo: github.com/mozilla-ai/llamafile; model org: huggingface.co/mozilla-ai)")
        typer.echo("  2. Serve it:  ./<model>.llamafile --server --port 8080 --nobrowser")
        typer.echo("  3. Qualify:   daisugi setup --endpoint http://localhost:8080/v1 --model <name> --wire")
    else:
        verdict = "PASSED" if qual.passed else "FAILED"
        typer.echo(f"Qualification: {verdict} — {qual.valid}/{qual.attempts} valid envelopes "
                   f"(pass rate {qual.pass_rate:.0%}, threshold {qual.threshold:.0%}).")
        if wired:
            typer.echo(f"  → Wired as Tier-1 in {data_dir}; `daisugi onboard`/`tend` will now defer to it.")
        elif qual.passed and not wire:
            typer.echo("  → Passed. Re-run with --wire to persist it as Tier-1.")
        else:
            errored = sum(1 for _, kind in qual.outcomes if kind == "error")
            if errored == qual.attempts:
                typer.echo("  → ALL attempts errored — this is a wiring problem, not model capacity. "
                           "Check the server is reachable at --endpoint and serving /v1, and that "
                           "--model matches the name it serves.")
            else:
                typer.echo("  → Not promoted. Try a larger model, a higher quant, or lower --threshold deliberately.")


@app.command("status")
def status_cmd(
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."
    ),
    threshold: float = typer.Option(
        DEFAULT_PATHWAY_THRESHOLD, "--threshold",
        help="Pathway retrieval threshold to display.",
    ),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show day-one readiness: are token savings live and are actions verified?"""
    from opendaisugi.onboarding import gather_status

    rep = gather_status(data_dir, threshold=threshold)

    if json_output:
        payload = dict(rep.__dict__)
        payload["data_dir"] = str(rep.data_dir)
        payload["token_savings_ready"] = rep.token_savings_ready
        payload["trust_ready"] = rep.trust_ready
        typer.echo(json.dumps(payload, indent=2))
        return

    ok = "✓"
    no = "✗"
    typer.echo(f"opendaisugi status (data dir: {rep.data_dir})")
    typer.echo("")
    typer.echo("Token savings (pathway routing):")
    typer.echo(
        f"  {ok if rep.search_extra_installed else no} [search] extra "
        f"{'installed' if rep.search_extra_installed else 'MISSING — pathways disabled; install opendaisugi[search]'}"
    )
    typer.echo(f"  • compiled pathways: {rep.pathway_count} ({rep.pathway_hits} hits)")
    typer.echo(f"  • retrieval threshold: {rep.retrieval_threshold:.2f}")
    typer.echo(
        f"  → {ok + ' token savings are LIVE' if rep.token_savings_ready else no + ' not yet — run `daisugi onboard`'}"
    )
    typer.echo("")
    typer.echo("Trust (verified actions):")
    typer.echo(
        f"  • journal traces: {rep.journal_total} "
        f"({rep.journal_passed} verified, {rep.journal_failed} rejected)"
    )
    typer.echo("  • verification: strict at stakes high/physical (rejects unprovable invariants)")
    typer.echo(
        f"  → {ok + ' journal populated; replay any action with `daisugi journal replay <id>`' if rep.trust_ready else no + ' empty — run `daisugi onboard` or start capturing'}"
    )
    typer.echo("")
    typer.echo("Local model (Tier-1 — cheap envelope generation):")
    from opendaisugi.hardware import detect_hardware, recommend_model
    from opendaisugi.local_setup import load_configured_tier1

    configured = load_configured_tier1(data_dir)
    if configured is not None:
        typer.echo(f"  {ok} wired: {getattr(configured, 'model', '?')} @ {getattr(configured, 'base_url', 'default')}")
        typer.echo("  → onboard/tend defer bulk envelope generation to your local model")
    else:
        prof = detect_hardware()
        rec = recommend_model(prof)
        typer.echo(
            f"  {no} none wired — hardware budget ~{prof.model_budget_gb:.0f}GB "
            f"→ recommends a {rec.size_class}-class model"
        )
        typer.echo("  → run `daisugi setup` to pick, qualify, and wire a local model")


@journal_app.command("search")
def journal_search_cmd(
    query: str = typer.Argument(..., help="Free-text query to match against trace tasks."),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir",
        help="Root data directory containing journal/",
    ),
    limit: int = typer.Option(10, "--limit", help="Maximum number of results."),
    json_output: bool = typer.Option(
        False, "--json", help="Emit result rows as JSON array."
    ),
) -> None:
    """Semantic search over journal traces (requires [search] extra)."""
    journal = Journal(data_dir=data_dir)
    try:
        results = journal.search(query, limit=limit)
    except ImportError as e:
        typer.echo(str(e), err=True)
        raise typer.Exit(code=2)

    if json_output:
        payload = [t.model_dump(mode="json") for t in results]
        typer.echo(json.dumps(payload, indent=2))
        return

    if not results:
        typer.echo("(no matching traces)")
        return
    for t in results:
        status = "ok" if t.ok else "FAIL"
        typer.echo(f"{t.id}  [{status}]  {t.task}")


@journal_app.command("replay")
def journal_replay_cmd(
    trace_id: str = typer.Argument(..., help="Trace id (e.g. 2026-04-09-a1b2c3d4)."),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir",
        help="Root data directory containing journal/",
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Emit ReplayResult as JSON."
    ),
) -> None:
    """Re-run verify() on a stored trace and report drift."""
    journal = Journal(data_dir=data_dir)
    try:
        replay = journal.replay(trace_id)
    except FileNotFoundError as e:
        typer.echo(f"Trace not found: {trace_id}")
        raise typer.Exit(code=2) from e

    if json_output:
        payload = {
            "trace_id": replay.trace_id,
            "original_ok": replay.original_ok,
            "replayed_ok": replay.replayed_ok,
            "drift": replay.drift,
            "original_result": replay.original_result.model_dump(mode="json"),
            "replayed_result": replay.replayed_result.model_dump(mode="json"),
        }
        typer.echo(json.dumps(payload, indent=2))
    else:
        if replay.drift:
            typer.echo(f"{replay.trace_id}: DRIFT detected")
            typer.echo(f"  original: ok={replay.original_ok}")
            typer.echo(f"  replayed: ok={replay.replayed_ok}")
            if replay.replayed_result.violations:
                typer.echo("  new violations:")
                for v in replay.replayed_result.violations:
                    typer.echo(f"    - [{v.stage}] {v.message}")
        else:
            typer.echo(f"{replay.trace_id}: no drift (ok={replay.original_ok})")

    raise typer.Exit(code=1 if replay.drift else 0)


@journal_app.command("stats")
def journal_stats_cmd(
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir",
        help="Root data directory containing journal/",
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Emit JournalStats as JSON."
    ),
) -> None:
    """Print aggregate stats from the journal index."""
    journal = Journal(data_dir=data_dir)
    stats = journal.stats()
    if json_output:
        typer.echo(json.dumps(asdict(stats), indent=2))
    else:
        typer.echo(f"total: {stats.total}")
        typer.echo(f"passed: {stats.passed}")
        typer.echo(f"failed: {stats.failed}")
        typer.echo(f"avg duration (ms): {stats.avg_duration_ms:.2f}")


@journal_app.command("parse")
def journal_parse_cmd(
    transcript: Path = typer.Argument(
        ..., exists=True, dir_okay=False, readable=True,
        help="Path to a Claude Code .jsonl transcript.",
    ),
    output: Path = typer.Option(
        ..., "-o", "--output",
        help="Path to write the episodes YAML/JSON file.",
    ),
    format_name: str = typer.Option(
        "claude-code", "--format",
        help="Parser format (default: claude-code).",
    ),
    min_tools: int = typer.Option(
        3, "--min-tools",
        help="Merge episodes below this tool-call threshold.",
    ),
    max_tools: int = typer.Option(
        30, "--max-tools",
        help="LLM-split episodes above this tool-call threshold.",
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514", "--model",
        help="Model for LLM splitting (rarely needed).",
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Write JSON instead of YAML.",
    ),
    llm: str = typer.Option(
        "litellm", "--llm",
        help="LLM backend: 'litellm' (default) or 'claude-code' (uses claude -p subprocess).",
    ),
) -> None:
    """Parse an agent transcript into episodes."""
    if llm not in {"litellm", "claude-code"}:
        typer.echo(
            f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.",
            err=True,
        )
        raise typer.Exit(code=2)
    if llm != "litellm":
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm
    try:
        parser = get_parser(format_name, min_tools=min_tools, max_tools=max_tools, model=model)
    except ValueError as e:
        typer.echo(str(e), err=True)
        raise typer.Exit(code=2) from e

    try:
        result = parser.parse(transcript)
    except Exception as e:
        typer.echo(f"Parse error: {e}", err=True)
        raise typer.Exit(code=2) from e

    payload = result.model_dump(mode="json", exclude_none=True)
    if json_output:
        output.write_text(json.dumps(payload, indent=2))
    else:
        output.write_text(yaml.safe_dump(payload, sort_keys=False))

    typer.echo(f"Parsed {len(result.episodes)} episodes to {output}")


@journal_app.command("ingest")
def journal_ingest_cmd(
    episodes_file: Path = typer.Argument(
        ..., exists=True, dir_okay=False, readable=True,
        help="Path to an episodes YAML/JSON file from 'journal parse'.",
    ),
    concurrency: int = typer.Option(
        5, "--concurrency",
        help="Max parallel envelope generation calls.",
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514", "--model",
        help="Model for envelope generation.",
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir",
        help="Root data directory containing journal/",
    ),
    dry_run: bool = typer.Option(
        False, "--dry-run",
        help="Show what would be ingested without LLM calls.",
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Machine-readable JSON output.",
    ),
) -> None:
    """Ingest parsed episodes into the journal."""
    try:
        raw = yaml.safe_load(episodes_file.read_text())
    except yaml.YAMLError as e:
        typer.echo(f"Invalid YAML: {e}", err=True)
        raise typer.Exit(code=2) from e

    try:
        parse_result = ParseResult(**raw)
    except Exception as e:
        typer.echo(f"Invalid episodes file: {e}", err=True)
        raise typer.Exit(code=2) from e

    journal = Journal(data_dir=data_dir)
    summary = asyncio.run(
        ingest_episodes(
            parse_result,
            journal,
            concurrency=concurrency,
            model=model,
            dry_run=dry_run,
        )
    )

    if json_output:
        payload = {
            "total": summary.total,
            "passed": summary.passed,
            "failed": summary.failed,
            "skipped": summary.skipped,
            "errored": summary.errored,
            "episodes": [
                {
                    "episode_id": e.episode_id,
                    "task": e.task,
                    "status": e.status,
                    "steps": e.steps,
                    "violations": e.violations,
                    "error": e.error,
                }
                for e in summary.episodes
            ],
        }
        typer.echo(json.dumps(payload, indent=2))
    else:
        for ep in summary.episodes:
            status = ep.status.ljust(7)
            detail = f"{ep.steps} steps"
            if ep.violations:
                detail += f", {ep.violations} violations"
            if ep.error:
                detail = ep.error
            typer.echo(f'{ep.episode_id}  {status} "{ep.task}" ({detail})')
        typer.echo("")
        typer.echo(f"Ingested {summary.total} episodes from {episodes_file.name}")
        typer.echo(f"  {summary.passed} passed verification")
        typer.echo(f"  {summary.failed} failed verification")
        typer.echo(f"  {summary.skipped} skipped (already in journal)")
        if summary.errored:
            typer.echo(f"  {summary.errored} errored")

    if summary.errored > 0:
        raise typer.Exit(code=1)


@app.command("install")
def install_cmd(
    dry_run: bool = typer.Option(False, "--dry-run", help="Show what would change without writing anything."),
    yes: bool = typer.Option(False, "--yes", "-y", help="Skip confirmation prompt."),
    print_skill: bool = typer.Option(False, "--print-skill", help="Print the opendaisugi-checklist skill content to stdout."),
    do_uninstall: bool = typer.Option(False, "--uninstall", help="Reverse all managed changes."),
    runtime: list[str] = typer.Option(None, "--runtime", help="Target named runtime(s) only, e.g. --runtime claude."),
) -> None:
    """Wire openDaisugi into every detected agent harness.

    Detects Claude Code, Codex, Hermes, and OpenClaw and installs three layers:
    the opendaisugi-checklist skill (discovered on demand — no per-session cost),
    the MCP tool server, and a passive capture hook that feeds distillation.
    All changes are idempotent, backed up, and reversible with --uninstall.
    """
    if print_skill:
        from opendaisugi.install import print_skill as _print_skill
        typer.echo(_print_skill())
        return

    from opendaisugi.install import (
        detect_runtimes, install as _install, uninstall as _uninstall,
        _select_runtimes,
    )

    home = Path.home()
    try:
        runtimes = _select_runtimes(runtime) if runtime else detect_runtimes(home=home)
    except ValueError as exc:
        typer.echo(f"Error: {exc}", err=True)
        raise typer.Exit(code=2)

    if not runtimes:
        typer.echo("No supported agent runtimes detected (Claude Code, Codex, Hermes, OpenClaw).")
        typer.echo("Install one and re-run, or see docs/hook-integration.md for manual setup.")
        raise typer.Exit(code=0)

    if do_uninstall:
        result = _uninstall(home=home, runtimes=runtimes)
        typer.echo(result.summary)
        if result.modified_files:
            typer.echo("\nReverted:")
            for f in result.modified_files:
                typer.echo(f"  {f}")
        return

    typer.echo("\nDetected runtimes:")
    for rt in runtimes:
        typer.echo(f"  ✓ {rt.name}")

    typer.echo("\nopenDaisugi will make these changes:\n")
    for rt in runtimes:
        typer.echo(f"[{rt.name}]")
        for step in rt.plan(home):
            target_hint = f"  → {step.target}" if step.target else ""
            typer.echo(f"  + [{step.layer.value}] {step.description}{target_hint}")
        typer.echo("")

    typer.echo("Skill is discovered on demand — zero added tokens for simple sessions.")
    typer.echo("Tool calls are captured to ~/.opendaisugi/captures/ for distillation.\n")

    if dry_run:
        typer.echo("Dry run — no files written.")
        return

    if not yes:
        confirmed = typer.confirm("Proceed?", default=False)
        if not confirmed:
            typer.echo("Aborted.")
            raise typer.Exit(code=0)

    result = _install(home=home, yes=True, runtimes=runtimes)

    if result.modified_files:
        typer.echo("\nDone. Files modified:")
        for f in result.modified_files:
            typer.echo(f"  {f}")
        typer.echo("\nRestart your agent session to pick up the changes.")
    else:
        typer.echo("\nAll runtimes were already configured — nothing changed.")


if __name__ == "__main__":  # enable `python -m opendaisugi.cli ...`
    app()
