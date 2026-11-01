"""rank pivot: switch from the chosen attempt to another, or say why not.

``plan`` reads the card and the ledger and writes nothing. ``apply`` undoes
the later steps the ledger marks reversible, newest first, then applies the
alternative's patch inside the chosen worktree. It never runs a permanent
step. When the undo would be dishonest (``follow_up_only``), it changes
nothing and returns the follow-up task text (W3K-4).
"""

from __future__ import annotations

import json
import os
import sqlite3
import subprocess
from collections.abc import Callable
from pathlib import Path
from typing import Any

from opendaisugi import branch_git, rank
from opendaisugi.models import ReversalHandle


def _rows(data_dir: Path, run_id: str) -> list[tuple[str, str, float, str | None]]:
    """(step, reversibility, timestamp, reversal_json) of a run, read only."""
    db = Path(data_dir) / "journal" / "index.db"
    if not db.exists():
        return []
    try:
        con = sqlite3.connect(db.absolute().as_uri() + "?mode=ro", uri=True)
    except sqlite3.Error:
        return []
    try:
        rows = con.execute(
            "SELECT step_id, reversibility, timestamp, reversal_json FROM receipts "
            "WHERE run_id = ? ORDER BY timestamp, step_id",
            (run_id,),
        ).fetchall()
    except sqlite3.Error:
        return []
    finally:
        con.close()
    return [(str(s), r or "", float(t), j) for s, r, t, j in rows if isinstance(t, (int, float))]


def _where(card: rank.Card, attempt: str) -> dict[str, Any] | None:
    facts = card.opened.get("facts")
    where = facts.get("where") if isinstance(facts, dict) else None
    w = where.get(attempt) if isinstance(where, dict) else None
    return w if isinstance(w, dict) else None


def plan(card: rank.Card, data_dir: Path, to: str) -> dict[str, Any]:
    surv = [s["id"] for s in card.opened["options"]["survivors"]]
    chosen = card.opened["chosen"]
    if to not in surv or to == chosen:
        raise rank.RankError(
            f"{to} is not another attempt that survived; the others are "
            f"{rank._join([s for s in surv if s != chosen])}."
        )
    sc = rank.switch_cost(card, data_dir, to=to)
    facts = card.opened.get("facts") if isinstance(card.opened.get("facts"), dict) else {}
    run = facts.get("run") if isinstance(facts.get("run"), dict) else None
    undo: list[dict[str, str]] = []
    skipped: list[dict[str, str]] = []
    if run and isinstance(run.get("run_id"), str) and isinstance(run.get("downstream"), list):
        down = {d for d in run["downstream"] if isinstance(d, str)}
        runs = [run["run_id"], *(r for r in card.resumed if r != run["run_id"])]
        later = []
        for rid in runs:
            for step, rev, ts, rjson in _rows(data_dir, rid):
                if step in down:
                    later.append((ts, rid, step, rev, rjson))
        later.sort(key=lambda r: (r[0], r[2]), reverse=True)
        for _, rid, step, rev, rjson in later:
            if rev == "reversible":
                path = ""
                if rjson:
                    try:
                        path = str(json.loads(rjson).get("path", ""))
                    except ValueError:
                        path = ""
                undo.append({"run_id": rid, "step_id": step, "path": path})
            elif rev == "irreversible":
                skipped.append({"run_id": rid, "step_id": step})
    apply_: dict[str, Any] | None = None
    alt, cur = _where(card, to), _where(card, chosen)
    if alt and alt.get("kind") == "output" and isinstance(alt.get("text"), str):
        apply_ = {"kind": "output", "text": alt["text"]}
    elif (
        alt
        and cur
        and alt.get("kind") == "worktree"
        and cur.get("kind") == "worktree"
        and isinstance(alt.get("path"), str)
        and isinstance(cur.get("path"), str)
        and branch_git.usable(alt.get("base"), alt.get("repo"))
        and branch_git.usable(cur.get("base"), cur.get("repo"))
    ):
        apply_ = {
            "kind": "worktree_patch",
            "from": alt["path"],
            "from_base": alt["base"],
            "from_repo": alt["repo"],
            "onto": cur["path"],
            "onto_base": cur["base"],
            "onto_repo": cur["repo"],
        }
    follow_up = None
    if sc["cost"] == rank.FOLLOW_UP or apply_ is None:
        follow_up = (
            f"Follow-up task: redo the work of choice {card.id} with {to} in place of {chosen}. "
            f"{sc['text']}"
            if sc["cost"] == rank.FOLLOW_UP
            else f"Follow-up task: apply {to} in place of {chosen} by hand; its place "
            "cannot be patched onto the chosen one."
        )
    return {
        "choice_id": card.id,
        "to": to,
        "chosen": chosen,
        "cost": sc["cost"],
        "text": sc["text"],
        "undo": undo,
        "skipped": skipped,
        "apply": apply_,
        "follow_up": follow_up,
    }


def apply(
    p: dict[str, Any],
    data_dir: Path,
    *,
    git: Callable[..., Any] = subprocess.run,
) -> dict[str, Any]:
    """Run a plan. Stops at the first failure and says so; never hides a
    half-applied patch."""
    if p["cost"] == rank.FOLLOW_UP or p["follow_up"] is not None:
        return {"applied": False, "follow_up": p["follow_up"], "why": p["text"]}
    from opendaisugi import deeds

    done: list[str] = []
    if p["undo"]:
        from opendaisugi.journal import Journal

        journal = Journal(data_dir=data_dir)
        by_step = {
            (r.run_id, r.step_id): r
            for rid in {u["run_id"] for u in p["undo"]}
            for r in journal.receipts_for_run(rid)
        }
        for u in p["undo"]:
            r = by_step.get((u["run_id"], u["step_id"]))
            if r is None or r.reversal is None or r.reversibility != "reversible":
                return {"applied": False, "why": f"step {u['step_id']} has no reversal; stopped"}
            deeds.apply_reversal(ReversalHandle.model_validate(r.reversal))
            done.append(u["step_id"])
    a = p["apply"]
    if a is not None and a["kind"] == "worktree_patch":
        return _patch(a, done, git)
    return {"applied": True, "undone": done, "applied_kind": a["kind"] if a else None}


def _names(path: str, base: str, repo: str, git: Callable[..., Any]) -> list[str] | None:
    out = branch_git.diff(
        path,
        base,
        repo,
        git,
        extra=("--name-only", "--no-renames", "-z", "--cached"),
        mark_new=False,
    )
    if out is None:
        return None
    return sorted(f for f in out.decode("utf-8", "replace").split("\0") if f)


def _patch(a: dict[str, Any], done: list[str], git: Callable[..., Any]) -> dict[str, Any]:
    """Take the chosen edits out of the chosen worktree and put the
    alternative's in. Both are read against their recorded base commits, so
    a child that committed changes nothing here. Each git apply is all or
    nothing; when the alternative does not apply, every touched path goes
    back to the base and the chosen edits are applied again, as ``adopt``
    puts a tree back."""
    onto, frm = a["onto"], a["from"]
    for w, repo in ((onto, a["onto_repo"]), (frm, a["from_repo"])):
        if not branch_git.owns(w, repo, git):
            return {
                "applied": False,
                "undone": done,
                "why": f"{w} no longer belongs to {repo}; nothing was changed",
            }
    # Stage everything so new files show in the diff and the index matches
    # the tree, which `git apply --index` needs.
    for w in (onto, frm):
        branch_git.stage(w, git)
    ob, fb = a["onto_base"], a["from_base"]
    mine = branch_git.diff(onto, ob, a["onto_repo"], git, extra=("--cached",), mark_new=False)
    theirs = branch_git.diff(frm, fb, a["from_repo"], git, extra=("--cached",), mark_new=False)
    if mine is None or theirs is None:
        return {"applied": False, "undone": done, "why": "git diff failed; nothing was changed"}
    mine_files = _names(onto, ob, a["onto_repo"], git) or []
    theirs_files = _names(frm, fb, a["from_repo"], git) or []
    if mine:
        res = git(
            ["git", *branch_git.GUARD, "-C", onto, "apply", "-R", "--index"],
            input=mine,
            capture_output=True,
        )
        if res.returncode != 0:
            return {
                "applied": False,
                "undone": done,
                "why": f"git apply -R failed, the chosen edits stay: {_err(res)}",
            }
    before = {f for f in theirs_files if os.path.lexists(Path(onto) / f)}
    res = git(
        ["git", *branch_git.GUARD, "-C", onto, "apply", "--3way", "--index"],
        input=theirs,
        capture_output=True,
    )
    if res.returncode == 0:
        return {"applied": True, "undone": done, "applied_kind": a["kind"]}
    why = f"git apply failed: {_err(res)}"
    touched = sorted(set(mine_files) | set(theirs_files))
    tracked = set(
        git(
            [
                "git",
                *branch_git.GUARD,
                "-C",
                onto,
                "ls-tree",
                "-r",
                "--name-only",
                "-z",
                ob,
                "--",
                *touched,
            ],
            capture_output=True,
        )
        .stdout.decode("utf-8", "replace")
        .split("\0")
    )
    git(["git", *branch_git.GUARD, "-C", onto, "reset", "-q", "--", *touched], capture_output=True)
    for f in touched:
        if f in tracked:
            git(
                ["git", *branch_git.GUARD, "-C", onto, "checkout", ob, "--", f], capture_output=True
            )
        elif f in mine_files or f not in before:
            # The chosen edit made this file, or the failed merge wrote it
            # (the alternative's new file). A file only the alternative adds
            # may already have been in the tree (an ignored one): it is not
            # ours to remove.
            try:
                (Path(onto) / f).unlink()
            except OSError:
                pass
    if mine:
        back = git(
            ["git", *branch_git.GUARD, "-C", onto, "apply", "--index"],
            input=mine,
            capture_output=True,
        )
        if back.returncode != 0:
            why += f"; the chosen edits could not be put back: {_err(back)}"
        else:
            why += "; the chosen edits are back"
    return {"applied": False, "undone": done, "why": why}


def _err(r: Any) -> str:
    err = getattr(r, "stderr", b"") or b""
    return (err.decode("utf-8", "replace") if isinstance(err, bytes) else str(err)).strip()


def plan_text(p: dict[str, Any]) -> str:
    lines = [f"Pivot {p['choice_id']}: {p['chosen']} to {p['to']} ({p['cost']})", p["text"]]
    for u in p["undo"]:
        lines.append(f"  undo step {u['step_id']} of {u['run_id']}: {u['path']}")
    for u in p["skipped"]:
        lines.append(f"  cannot undo step {u['step_id']} of {u['run_id']}")
    if p["apply"] is not None:
        a = p["apply"]
        lines.append(
            f"  then apply {a['from']} onto {a['onto']}"
            if a["kind"] == "worktree_patch"
            else "  then use the alternative's output"
        )
    if p["follow_up"]:
        lines.append(p["follow_up"])
    return "\n".join(lines) + "\n"
