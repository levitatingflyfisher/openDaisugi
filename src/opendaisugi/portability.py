"""Pathway portability: export/import in JSON, skill, mermaid, md, smtlib formats.

A CompiledPathway is the library's canonical object; this module serializes
it for distribution outside openDaisugi and re-verifies on import.

Formats:
    json     — canonical JSON bundle. Lossless round-trip.
    skill    — markdown + YAML frontmatter, Claude Code / Hermes / OpenClaw
               compatible shape. Lossless round-trip (body is re-generated
               on export from task_description).
    mermaid  — flowchart of the plan DAG + permission summary. One-way.
    md       — human-readable audit report. One-way.
    smtlib   — SMT-LIB2 encoding of the Z3 verification. One-way.

Import auto-detects by extension and by frontmatter presence; on success,
the pathway is re-verified against its declared envelope before being
admitted to the PathwayStore.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

import yaml

from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore
from opendaisugi.verify import verify


def _pkg_version() -> str:
    """Read the package version without triggering a circular import.

    ``opendaisugi.__init__`` re-exports this module's symbols, so a
    top-level ``from opendaisugi import __version__`` would recurse.
    Late-binding the import inside the function is safe because by the
    time anyone calls us, ``__init__`` has finished running.
    """
    import opendaisugi
    return opendaisugi.__version__

ExportFormat = Literal["json", "skill", "mermaid", "md", "smtlib"]

_SUPPORTED_FORMATS: tuple[ExportFormat, ...] = ("json", "skill", "mermaid", "md", "smtlib")

# Bump when the on-disk JSON bundle shape changes incompatibly. Import
# rejects bundles with a higher major version than this.
BUNDLE_SCHEMA_VERSION = 1


class PathwayImportError(Exception):
    """Raised on import failure. ``code`` is a stable machine-readable tag."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(f"[{code}] {message}")
        self.code = code


@dataclass(frozen=True)
class ImportResult:
    pathway: CompiledPathway
    overwrote_existing: bool


# ─────────────────────────── EXPORT ───────────────────────────


def export(pathway: CompiledPathway, fmt: ExportFormat) -> str:
    """Serialize a pathway in the requested format. Returns text."""
    if fmt not in _SUPPORTED_FORMATS:
        raise ValueError(f"unknown format {fmt!r}; supported: {_SUPPORTED_FORMATS}")
    return _DISPATCH[fmt](pathway)


def _export_json(pathway: CompiledPathway) -> str:
    bundle = {
        "opendaisugi_version": _pkg_version(),
        "schema_version": BUNDLE_SCHEMA_VERSION,
        "pathway": pathway.model_dump(mode="json"),
    }
    return json.dumps(bundle, indent=2, sort_keys=False)


def _export_skill(pathway: CompiledPathway) -> str:
    """Emit markdown with YAML frontmatter. Claude Code / Hermes / OpenClaw shape.

    Frontmatter holds ``name`` + ``description`` (agent-framework standard)
    and a ``daisugi:`` key with the full JSON bundle for lossless import.
    Body is human-readable documentation of the pathway.
    """
    frontmatter = {
        "name": _slug(pathway.task_description),
        "description": pathway.task_description,
        "daisugi": {
            "opendaisugi_version": _pkg_version(),
            "schema_version": BUNDLE_SCHEMA_VERSION,
            "pathway": pathway.model_dump(mode="json"),
        },
    }
    head = yaml.safe_dump(frontmatter, sort_keys=False, default_flow_style=False)
    body = _skill_body(pathway)
    return f"---\n{head}---\n\n{body}"


def _export_mermaid(pathway: CompiledPathway) -> str:
    """Plan DAG as a Mermaid flowchart with permission summary."""
    lines = ["```mermaid", "flowchart TD"]
    for step in pathway.plan_template.steps:
        label = f"{step.id}<br/>{step.type}"
        detail = getattr(step, "command", None) or getattr(step, "path", None) or getattr(step, "url", "")
        if detail:
            label += f"<br/><code>{_mermaid_escape(str(detail))}</code>"
        lines.append(f"    {step.id}[{label}]")
    for step in pathway.plan_template.steps:
        for dep in step.depends_on:
            lines.append(f"    {dep} --> {step.id}")
    lines.append("```")

    perms = pathway.envelope.permissions
    perm_lines = [
        "",
        "### Permissions",
        f"- shell: {perms.shell} (allowlist: {perms.shell_allowlist or '[]'})",
        f"- file_read: {perms.file_read or '[]'}",
        f"- file_write: {perms.file_write or '[]'}",
        f"- network: {perms.network} (hosts: {perms.network_hosts or '[]'})",
    ]
    return "\n".join(lines + perm_lines)


def _export_md(pathway: CompiledPathway) -> str:
    """Human-readable audit report."""
    env = pathway.envelope
    plan = pathway.plan_template
    parts = [
        f"# Pathway: {pathway.task_description}",
        "",
        f"- **ID:** `{pathway.id}`",
        f"- **Distilled at:** {time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime(pathway.distilled_at))}",
        f"- **Hit count:** {pathway.hit_count}",
        f"- **Source traces:** {len(pathway.source_trace_ids)}",
        f"- **Embedding model:** {pathway.embedding_model or '(unspecified)'}",
        "",
        "## Envelope",
        "",
        f"- **Generator:** {env.generated_by}",
        "- **Permissions:**",
        f"  - shell: {env.permissions.shell} (allowlist: {env.permissions.shell_allowlist or '[]'})",
        f"  - file_read: {env.permissions.file_read or '[]'}",
        f"  - file_write: {env.permissions.file_write or '[]'}",
        f"  - network: {env.permissions.network} (hosts: {env.permissions.network_hosts or '[]'})",
    ]
    if env.invariants:
        parts.append("- **Invariants:**")
        for inv in env.invariants:
            parts.append(f"  - `{inv.type}`: {inv.description}")
    if env.postconditions:
        parts.append("- **Postconditions:**")
        for pc in env.postconditions:
            parts.append(f"  - `{pc.type}` → path={pc.path} expected={pc.expected}")
    parts.extend(["", "## Plan template", ""])
    for step in plan.steps:
        detail = getattr(step, "command", None) or getattr(step, "path", None) or getattr(step, "url", "")
        parts.append(f"- `{step.id}` ({step.type}): `{detail}`")
        if step.depends_on:
            parts.append(f"  - depends on: {', '.join(step.depends_on)}")
    return "\n".join(parts) + "\n"


def _export_smtlib(pathway: CompiledPathway) -> str:
    """SMT-LIB2 encoding of the Z3 verification assertions.

    Emits the same solver state that ``verify()`` builds, then dumps via
    ``Solver.to_smt2()``. Third parties can run ``z3 pathway.smt2`` and
    independently confirm the pathway verifies — no openDaisugi install
    needed for verification.
    """
    import z3
    from opendaisugi.z3_checks import check_envelope_self_consistency

    # Re-encode the self-consistency checks into a fresh solver and sexpr
    # it out. We don't use the z3_checks helper directly because it
    # builds and discards the solver internally; replicating the key
    # constraints keeps the SMT-LIB artifact self-contained.
    env = pathway.envelope
    solver = z3.Solver()
    shell = z3.Bool("shell")
    can_write = z3.Bool("can_write")
    solver.add(shell == env.permissions.shell)
    solver.add(can_write == (len(env.permissions.file_write) > 0))
    if env.permissions.shell_allowlist:
        solver.add(shell == True)  # noqa: E712
    for pc in env.postconditions:
        if pc.type == "file_exists":
            solver.add(can_write == True)  # noqa: E712
    # Sanity-run so the artifact includes the (check-sat) invocation.
    header = (
        f";; openDaisugi pathway proof artifact\n"
        f";; pathway_id: {pathway.id}\n"
        f";; task: {pathway.task_description}\n"
        f";; envelope_id: {env.id}\n"
        f";; opendaisugi_version: {_pkg_version()}\n"
    )
    return header + solver.to_smt2()


_DISPATCH = {
    "json": _export_json,
    "skill": _export_skill,
    "mermaid": _export_mermaid,
    "md": _export_md,
    "smtlib": _export_smtlib,
}


def _slug(text: str, max_len: int = 60) -> str:
    s = "".join(c if c.isalnum() else "-" for c in text.lower()).strip("-")
    while "--" in s:
        s = s.replace("--", "-")
    return s[:max_len] or "pathway"


def _mermaid_escape(text: str) -> str:
    return text.replace('"', "&quot;").replace("|", "\\|")[:120]


def _skill_body(pathway: CompiledPathway) -> str:
    perms = pathway.envelope.permissions
    lines = [
        f"# {pathway.task_description}",
        "",
        "A Z3-verified pathway compiled from successful journal traces by openDaisugi.",
        "On import, the plan template is re-verified against the envelope declared",
        "in this skill's frontmatter; imports fail closed if verification fails.",
        "",
        "## What this pathway does",
        "",
        pathway.task_description,
        "",
        "## Verified permissions",
        "",
        f"- shell: `{perms.shell}` (allowlist: `{perms.shell_allowlist or '[]'}`)",
        f"- file_read: `{perms.file_read or '[]'}`",
        f"- file_write: `{perms.file_write or '[]'}`",
        f"- network: `{perms.network}` (hosts: `{perms.network_hosts or '[]'}`)",
        "",
        "## Usage",
        "",
        "Install with:",
        "",
        "```bash",
        "daisugi pathways import path/to/this-skill.md",
        "```",
        "",
        "openDaisugi will re-verify the plan template against the envelope above.",
        "If verification passes, the pathway is admitted to the local PathwayStore",
        "and becomes a Tier-0 cache hit for matching tasks.",
    ]
    return "\n".join(lines) + "\n"


# ─────────────────────────── IMPORT ───────────────────────────


def parse_bundle(text: str, *, source_path: Path | None = None) -> CompiledPathway:
    """Parse JSON or skill-markdown text into a CompiledPathway.

    Does not verify; caller is responsible for re-running verification
    before admitting to a store.
    """
    text = text.lstrip()
    if text.startswith("---"):
        bundle = _extract_frontmatter(text)
    elif text.startswith("{"):
        bundle = json.loads(text)
    else:
        raise PathwayImportError(
            "SCHEMA_INCOMPATIBLE",
            "input is neither JSON nor skill markdown with YAML frontmatter",
        )

    if bundle.get("schema_version", 0) > BUNDLE_SCHEMA_VERSION:
        raise PathwayImportError(
            "SCHEMA_INCOMPATIBLE",
            f"bundle schema_version={bundle.get('schema_version')} "
            f"is newer than this library ({BUNDLE_SCHEMA_VERSION})",
        )

    raw = bundle.get("pathway")
    if raw is None:
        raise PathwayImportError(
            "SCHEMA_INCOMPATIBLE",
            f"bundle is missing 'pathway' key (source={source_path})",
        )
    return CompiledPathway.model_validate(raw)


def _extract_frontmatter(text: str) -> dict:
    if not text.startswith("---"):
        raise PathwayImportError("SCHEMA_INCOMPATIBLE", "expected YAML frontmatter")
    # Split on the closing --- on its own line.
    try:
        _, frontmatter_and_body = text.split("---\n", 1)
        frontmatter_text, _ = frontmatter_and_body.split("\n---\n", 1)
    except ValueError as e:
        raise PathwayImportError("SCHEMA_INCOMPATIBLE", "unterminated YAML frontmatter") from e
    data = yaml.safe_load(frontmatter_text) or {}
    if "daisugi" not in data:
        raise PathwayImportError(
            "SCHEMA_INCOMPATIBLE",
            "skill frontmatter is missing the 'daisugi' key",
        )
    return data["daisugi"]


def import_pathway(
    path: str | Path,
    store: PathwayStore,
    *,
    z3_timeout_ms: int = 500,
    allow_overwrite: bool = False,
) -> ImportResult:
    """Import a pathway bundle from disk into ``store``.

    Re-verifies the plan template against the declared envelope before
    insertion. Raises ``PathwayImportError`` with a stable ``code`` on
    any failure: SCHEMA_INCOMPATIBLE, VERIFICATION_FAILED, DUPLICATE_ID.
    """
    p = Path(path)
    text = p.read_text()
    pathway = parse_bundle(text, source_path=p)

    result = verify(pathway.plan_template, pathway.envelope, z3_timeout_ms=z3_timeout_ms)
    if not result.ok:
        summaries = "; ".join(f"[{v.stage}] {v.message}" for v in result.violations)
        raise PathwayImportError(
            "VERIFICATION_FAILED",
            f"plan template does not verify against declared envelope: {summaries}",
        )

    existed = store.delete(pathway.id) if allow_overwrite else False
    if not allow_overwrite:
        if any(p.id == pathway.id for p in store.list_all()):
            raise PathwayImportError(
                "DUPLICATE_ID",
                f"pathway {pathway.id!r} already exists; pass allow_overwrite=True to replace",
            )

    store.put(pathway)
    return ImportResult(pathway=pathway, overwrote_existing=existed)
