"""Stage 8 — the deed ledger: reversibility read from the journal alone.

A wrong-but-allowed action (in-envelope, but wrong for the task) should cost a
rollback, not a token-burning recovery arc. The Supervisor records a reversal
handle for each reversible side-effecting step (see ``Receipt.reversal``); this
module turns those handles back into filesystem state — **with no model, no
executor, and no re-run**. That is the whole point of Stage 8: recovery is a
mechanical read of the ledger, not more inference.

openDaisugi stays a *layer* (ADR-0004): it records the deed; the harness decides
to invoke the rollback. A reversal only ever writes to a path the run itself
wrote, so it cannot widen the envelope (ADR-0011 admissibility-preservation) — it
is valid **only** against the run's own ledger, never receipts from another run
or envelope.
"""

from __future__ import annotations

import os
import secrets
from dataclasses import dataclass, field
from pathlib import Path

from opendaisugi.models import ReversalHandle


@dataclass(frozen=True)
class PathState:
    """The state of a touched path *before* the run first mutated it."""

    pre_existed: bool
    pre_content: str | None


@dataclass
class RollbackReport:
    """What a rollback did. ``undone`` lists the paths restored or deleted;
    ``skipped`` lists irreversible deeds that could not be undone (read-only
    ``none`` deeds are neither undone nor skipped — there was nothing to do)."""

    undone: list[str] = field(default_factory=list)
    skipped: list[dict] = field(default_factory=list)


def _atomic_write(path: str, content: str) -> None:
    p = Path(path)
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.parent / f".daisugi-undo-{secrets.token_hex(6)}-{p.name}"
    tmp.write_text(content)
    os.replace(tmp, p)


def apply_reversal(handle: ReversalHandle) -> None:
    """Undo a single reversible deed. For a ``file_write``: restore the prior
    content when the target existed, or delete the file (and any directories the
    write created, deepest-first when empty) when it did not."""
    if handle.kind != "file_write":  # pragma: no cover - only kind today
        raise ValueError(f"cannot reverse deed of kind {handle.kind!r}")
    if handle.prior_existed:
        _atomic_write(handle.path, handle.prior_content or "")
        return
    try:
        os.unlink(handle.path)
    except FileNotFoundError:
        pass
    for d in handle.created_dirs:  # deepest-first; only remove if now empty
        try:
            os.rmdir(d)
        except OSError:
            pass


def rollback_run(journal, run_id: str) -> RollbackReport:
    """Undo a run's reversible deeds from the ledger alone, newest first.

    Reads the run's receipts and applies each reversible reversal handle in
    reverse execution order (a later write may sit atop an earlier one). No
    model, no executor, no re-run. Irreversible deeds are reported in
    ``skipped``; read-only deeds are ignored. Valid only against the run's own
    ledger (see module docstring)."""
    report = RollbackReport()
    for r in reversed(journal.receipts_for_run(run_id)):
        if r.reversibility == "reversible" and r.reversal is not None:
            apply_reversal(r.reversal)
            report.undone.append(r.reversal.path)
        elif r.reversibility == "irreversible":
            report.skipped.append(
                {"step_id": r.step_id, "effect_class": r.effect_class, "reason": "irreversible"}
            )
    return report


def touched_files(journal, run_id: str) -> dict[str, PathState]:
    """Reconstruct, from the ledger alone, which files the run touched and what
    was there before it — a ground-truth view a compacted agent can read instead
    of re-deriving from a lost transcript. Covers file writes that carry a
    captured reversal handle; the first write to a path wins its pre-state (the
    state before the run's first mutation). Writes whose prior image was too
    large or binary to hold (irreversible, no handle) are not represented."""
    view: dict[str, PathState] = {}
    for r in journal.receipts_for_run(run_id):  # ascending: first write wins
        h = r.reversal
        if r.effect_class == "file_write" and h is not None and h.path not in view:
            view[h.path] = PathState(pre_existed=h.prior_existed, pre_content=h.prior_content)
    return view
