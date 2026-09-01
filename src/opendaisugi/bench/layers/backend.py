"""`daisugi bench backend`: envelope quality per model backend.

Four numbers per backend: how many envelopes it produced for the corpus
tasks, how many of those are self-consistent, the mean number of clauses,
and the tokens it spent. Self-consistent means the envelope's own predicates
are satisfiable, checked with the same Z3 bridge the verifier uses. A
tighter envelope is a smaller one, not a better one by itself.

Self-consistency is not plan-versus-envelope. No plan is recorded beside
these envelopes, so the column is named for what it measures. The bench reads
recorded envelopes and never calls a model. A backend with no recording
prints absent and names the file that would hold one.
"""

from __future__ import annotations

import logging
from pathlib import Path

from opendaisugi.bench.corpus import (
    CorpusMissing,
    CorpusRef,
    corpus_ref,
    load_jsonl,
    resolve_corpus,
)
from opendaisugi.bench.options import repo_root
from opendaisugi.bench.registry import BenchOpts, BenchRefused, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.models import Envelope

_log = logging.getLogger("opendaisugi.bench.backend")

CORPUS = "tasks.jsonl"
RECORDINGS = "envelopes"

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("envelopes", "envelopes", align="right"),
    Column("self_consistent", "self-consistent", align="right"),
    Column("mean_clauses", "mean clauses", align="right"),
    Column("tokens", "tokens", align="right"),
    Column("source", "source"),
)


def clause_count(envelope: Envelope) -> int:
    """How many things this envelope says. Fewer is tighter, not better by itself."""
    p = envelope.permissions
    total = len(p.file_read) + len(p.file_write) + len(p.shell_allowlist)
    total += len(p.network_hosts) + len(p.mcp_allowlist) + len(p.custom_step_allowlist)
    total += int(p.shell) + int(p.network)
    total += len(envelope.invariants) + len(envelope.postconditions)
    return total


def self_consistent(envelope: Envelope) -> bool:
    """True when no invariant of this envelope is a contradiction.

    `check_vacuity` returns one of the three literals in `vacuity.Verdict`,
    a plain string, so the comparison is to the literal. Only "contradiction"
    counts here: a contradictory invariant admits nothing and denies every
    plan. A tautology is the opposite failure, it admits everything, and it
    is not a self-inconsistency, so it is not folded in under a column named
    self-consistent.

    A check that cannot complete is not a pass. An invariant the parser
    rejects, or the solver cannot decide, counts as not self-consistent. The
    except is narrow so a typo in this function raises instead of quietly
    zeroing the column.
    """
    from opendaisugi.predicate import parse_expression
    from opendaisugi.vacuity import check_vacuity

    for inv in envelope.invariants:
        if inv.expr is None:
            continue
        try:
            expr = parse_expression(inv.expr) if isinstance(inv.expr, dict) else inv.expr
            if check_vacuity(expr) == "contradiction":
                return False
        except (ValueError, TypeError, KeyError) as exc:
            _log.debug("vacuity check failed for %r: %s", inv.type, exc)
            return False
    return True


def recording_path(ref: CorpusRef, backend: str) -> Path:
    """Where this backend's recording lives: `envelopes/<backend>.jsonl` beside the task corpus."""
    return Path(ref.path).with_name(RECORDINGS) / f"{backend}.jsonl"


def _shown(path: Path) -> str:
    """The path as the reproduce line would print it: repo-relative when it can be."""
    root = repo_root()
    try:
        return str(path.relative_to(root)) if root else str(path)
    except ValueError:
        return str(path)


def absent_hint(ref: CorpusRef, backend: str) -> str:
    """The absent text for a backend with no recording: where it would be, and the shape."""
    return (
        f"no recording at {_shown(recording_path(ref, backend))}; "
        f"record one in the shape of {RECORDINGS}/claude-code.jsonl"
    )


def _validated(path: Path, rows: list[dict]) -> list[dict]:
    """Every row checked, with the file and line named on the first bad one.

    A row with no task id, non-integer token counts, or an envelope that does
    not validate is a corpus error with the fix in it, not a KeyError deep in
    the mean or a pydantic traceback with no line number.
    """
    from pydantic import ValidationError

    for line, row in enumerate(rows, start=1):
        problem: str | None = None
        if not isinstance(row, dict):
            problem = "the row is not a JSON object"
        elif not isinstance(row.get("task_id"), str) or not row["task_id"]:
            problem = "no task_id"
        elif not all(isinstance(row.get(k), int) for k in ("tokens_in", "tokens_out")):
            problem = "tokens_in and tokens_out must be integers"
        else:
            try:
                Envelope.model_validate(row.get("envelope"))
            except ValidationError as exc:
                problem = f"the envelope does not validate: {exc.errors()[0]['msg']}"
        if problem:
            raise CorpusMissing(
                f"{path} line {line}: {problem}.\n"
                f"Every recording row needs task_id, backend, model, recorded_at, "
                f"tokens_in, tokens_out and a valid envelope; see {RECORDINGS}/claude-code.jsonl."
            )
    return rows


def _recorded(
    ref: CorpusRef, backend: str, task_ids: set[str]
) -> tuple[CorpusRef, list[dict]] | None:
    """The recording for one backend, pinned, or None when there is none.

    Every recorded row must name a task the corpus holds. A foreign task id is
    a corpus error with the fix in it, not a number quietly folded into a mean.
    """
    path = recording_path(ref, backend)
    if not path.is_file():
        return None
    rec_ref = corpus_ref(path)
    rows = _validated(path, load_jsonl(rec_ref))
    unknown = sorted({r["task_id"] for r in rows if r["task_id"] not in task_ids})
    if unknown:
        raise CorpusMissing(
            f"{path} records tasks that {ref.path} does not hold: {', '.join(unknown)}.\n"
            f"Every task_id in a recording must be an id in the task corpus."
        )
    return rec_ref, rows


def run(opts: BenchOpts) -> Table:
    if opts.live:
        raise BenchRefused(
            "daisugi bench backend --live: the recorder is not built.\n"
            f"Record {RECORDINGS}/<backend>.jsonl beside the task corpus by hand, "
            f"in the shape of {RECORDINGS}/claude-code.jsonl, then run without --live."
        )
    ref = resolve_corpus(CORPUS, opts.corpus)
    tasks = load_jsonl(ref)
    task_ids = {t["id"] for t in tasks}
    from opendaisugi.swap import SWAP_KNOBS

    rows: list[Row] = []
    companions: list[CorpusRef] = []
    # The auto option names no backend of its own, so it has no row here.
    for option in SWAP_KNOBS["backend"].options:
        if option.value is None:
            continue
        backend = str(option.value)
        found = _recorded(ref, backend, task_ids)
        if found is None:
            rows.append(Row(name=backend, absent=absent_hint(ref, backend)))
            continue
        rec_ref, recorded = found
        companions.append(rec_ref)
        envelopes = [Envelope.model_validate(r["envelope"]) for r in recorded]
        clauses = [clause_count(e) for e in envelopes]
        rows.append(
            Row(
                name=backend,
                cells={
                    "option": backend,
                    "envelopes": f"{len(envelopes)}/{len(tasks)}",
                    "self_consistent": sum(self_consistent(e) for e in envelopes),
                    "mean_clauses": round(sum(clauses) / max(1, len(clauses)), 1),
                    "tokens": sum(int(r["tokens_in"]) + int(r["tokens_out"]) for r in recorded),
                    "source": "recorded",
                },
            )
        )
    return Table(
        layer="backend",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        companions=tuple(companions),
        reproduce=f"uv run --no-sync daisugi bench backend --corpus {ref.rel}",
        notes=(
            "self-consistent means the envelope's own predicates are satisfiable",
            "it does not mean a plan verified against it; no plan is recorded here",
            f"rows read {RECORDINGS}/<backend>.jsonl beside the task corpus; each file read is pinned",
            "the corpus path is repo-relative, so run the reproduce line from the checkout root",
            "the committed claude-code recording is a hand-written stand-in, not an API capture",
        ),
    )


register(BenchSpec(name="backend", layer="backend", corpus=CORPUS, columns=COLUMNS, run=run))
