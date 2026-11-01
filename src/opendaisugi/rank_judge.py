"""rank judge: one model comparison of two attempts, in both orders.

``judge_once`` takes the fit's next pair, asks one judge twice (the pair
shown A-first, then B-first, the author never shown), and records both
comparison rows or neither. A reply that is not one line of ``A``, ``B`` or
``tie``, a transport error and a non-2xx status are no vote: none of them is
ever a win for either side.

A judge must come from another model family than every author in the pair
(W3K-1). An author with no model name cannot be checked, so that pair is
not judged. A judge whose agreement with the owner fell under the floor is
dropped (RK-5) and is not asked.
"""

from __future__ import annotations

import hashlib
import os
import subprocess
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any

from opendaisugi import agent_profiles, branch_git, llm_client, rank

DIFF_CAP = 256 * 1024
_VERDICTS = {"a": "A", "b": "B", "tie": "tie"}

JUDGE_SYSTEM_TEXT = (
    "You compare two answers to one task. "
    "Decide which answer does the task better. "
    "Length, polish and confidence do not count. "
    "Reply with one line: A, B or tie."
)
JUDGE_SYSTEM: bytes = JUDGE_SYSTEM_TEXT.encode("utf-8")

_USER = (
    "Task:\n{task}\n\nAnswer A:\n{first}\n\nAnswer B:\n{second}\n\n"
    "Which answer does the task better? Reply with one line: A, B or tie."
)


def prompt(task: str, first: str, second: str) -> list[dict[str, str]]:
    return [
        {"role": "system", "content": JUDGE_SYSTEM_TEXT},
        {"role": "user", "content": _USER.format(task=task, first=first, second=second)},
    ]


def load_profiles() -> agent_profiles.Profiles:
    return agent_profiles.resolve(agent_profiles.configured_table(), os.environ)


def artifact(attempt: rank.Attempt, warnings: list[str] | None = None) -> str:
    """The text a judge reads for one attempt."""
    w = attempt.where
    if isinstance(w, dict) and w.get("kind") == "output" and isinstance(w.get("text"), str):
        return w["text"]
    if isinstance(w, dict) and w.get("kind") == "worktree" and isinstance(w.get("path"), str):
        # The base and the repository come from the attempt the parent wrote,
        # never from the worktree, which its child can rewrite.
        raw = None
        if branch_git.usable(w.get("base"), w.get("repo")):
            try:
                raw = branch_git.diff(w["path"], w["base"], w["repo"], timeout=60)
            except (OSError, subprocess.SubprocessError):
                raw = None
        if raw is not None:
            text = raw[:DIFF_CAP].decode("utf-8", "replace")
            if len(raw) > DIFF_CAP:
                text += "\n[cut at 256 KiB]"
            return text
    if warnings is not None:
        warnings.append(f"attempt {attempt.id} has no artifact a judge can read")
    return ""


def parse_verdict(text: str) -> str | None:
    """``a``, ``b`` or ``tie`` from the first non-empty line, else None."""
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        line = line.rstrip(".").strip().lower()
        return line if line in _VERDICTS else None
    return None


def _split_entry(entry: str) -> tuple[str, str | None]:
    name, sep, model = entry.partition("/")
    return name, (model if sep else None)


def _fit_for(r: rank.Ranking, data_dir: Path, warnings: list[str]) -> dict[str, Any]:
    comps = list(r.comparisons or [])
    journal, bad = rank.journal_comparisons(data_dir, r.ranking_id)
    comps += journal
    if bad:
        warnings.append(f"{bad} rows of the comparison journal did not read; skipped")
    owner = r.owner
    if owner is None:
        owner = rank.owner_answers(data_dir, r.ranking_id, {a.id: a for a in r.attempts}, warnings)
    # The file's comparisons are held inline; pass the merged list as the file's.
    r2 = rank.Ranking(
        ranking_id=r.ranking_id,
        task=r.task,
        project=r.project,
        attempts=r.attempts,
        comparisons=comps,
        owner=owner,
        policy=r.policy,
    )
    weights = None
    if not r.comparisons:
        # The journal supplies the votes (none in the file, or an empty list),
        # so judges weigh by their record. A file that holds comparisons inline
        # is explicit, and unweighted.
        judges = sorted({*r.policy.judges, *(c.judge for c in comps)})
        if judges:
            weights, _ = rank.weights_for(data_dir, judges, rank.seed_of(r2, comps, owner))
    res = rank.fit(r2, warnings=warnings, weights=weights)
    res["_n"] = len(comps)
    return res


def fit_doc(doc: dict[str, Any], data_dir: Path) -> tuple[rank.Ranking, dict[str, Any]]:
    """The ranking of an attempts document with the journal's votes and the
    owner's answers, as ``rank fit`` reads them. Reads only."""
    r = rank.parse(doc)
    res = _fit_for(r, data_dir, [])
    res.pop("_n")
    return r, res


def _nothing(why: str, fit: dict[str, Any] | None, reason: str = "nothing") -> dict[str, Any]:
    return {
        "recorded": False,
        "pair": None,
        "judge": None,
        "outcomes": None,
        "why": why,
        "reason": reason,
        "fit": fit,
    }


def judge_once(
    doc: dict[str, Any],
    data_dir: Path,
    *,
    profiles: agent_profiles.Profiles,
    env: Mapping[str, str],
    judge: str | None,
    now: float,
    call: Callable[..., Any] | None = None,
) -> dict[str, Any]:
    call = call or llm_client.complete
    r = rank.parse(doc)
    if judge is not None:
        r.policy.judges = [judge]
    if not r.policy.judges:
        return _nothing("policy.judges names no judge.", None)
    # A dropped judge is never asked, whatever shape the file has (RK-5).
    trust = rank.judge_trust(data_dir)
    dropped = [j for j in r.policy.judges if trust.get(j, {}).get("dropped")]
    r.policy.judges = [j for j in r.policy.judges if j not in dropped]
    warnings: list[str] = []
    res = _fit_for(r, data_dir, warnings)
    n_so_far = res.pop("_n")
    nxt = res["next"]
    if nxt is None:
        if dropped and res["stop"] == "judges_exhausted":
            j = dropped[0]
            return _nothing(f"judge {j} is dropped: {trust[j]['why']}", res)
        return _nothing(f"nothing to judge: {res['stop']}", res)
    entry = nxt["judge"]
    first_id, second_id = nxt["pair"]
    by_id = {a.id: a for a in r.attempts}
    fa, fb = by_id[first_id], by_id[second_id]
    fam_j_name, model = _split_entry(entry)
    profile, why = profiles.lookup(fam_j_name)
    if profile is None:
        return _nothing(f"judge {entry}: {why}", res)
    if profile.wire == "claude-code":
        return _nothing(
            f"judge {entry}: profile {fam_j_name} uses the claude-code wire; "
            "a judge needs the anthropic or openai wire.",
            res,
        )
    model = model or profile.model
    if not model:
        return _nothing(f"judge {entry}: no model; name PROFILE/MODEL.", res)
    fam = rank.family_of(model)
    for a in (fa, fb):
        if not a.author.strip():
            return _nothing(
                f"attempt {a.id} has no author, so the other-family rule cannot hold.", res
            )
        if rank.family_of(a.author) == fam:
            return _nothing(
                f"judge {entry} is of the same family ({fam}) as the author of {a.id}.", res
            )
    texts = {fa.id: artifact(fa, warnings), fb.id: artifact(fb, warnings)}
    if not texts[fa.id] and not texts[fb.id]:
        return _nothing("no attempt has an artifact a judge can read.", res)
    if not texts[fa.id] or not texts[fb.id]:
        return _nothing("an attempt has no artifact a judge can read.", res)
    wire_model = ("anthropic/" if profile.wire == "anthropic" else "openai/") + model
    key = profile.key or (env.get(profile.key_env) if profile.key_env else None)
    verdicts: list[str] = []
    for one, two in ((fa, fb), (fb, fa)):
        try:
            reply = call(
                wire_model,
                prompt(r.task, texts[one.id], texts[two.id]),
                base_url=profile.base_url,
                api_key=key,
                env=env,
            )
            text = reply.text
        except Exception as exc:  # a failed call is no vote, whatever it raised
            return _nothing(
                f"the judge call failed ({type(exc).__name__}: {exc}); no vote recorded.",
                res,
                "failed",
            )
        v = parse_verdict(text)
        if v is None:
            return _nothing(
                "the judge reply was not one line of A, B or tie; no vote recorded.",
                res,
                "unparsable",
            )
        verdicts.append(v)
    pair_id = (
        "pj_"
        + hashlib.sha256(
            rank.canonical([r.ranking_id, first_id, second_id, entry, n_so_far]).encode("utf-8")
        ).hexdigest()[:12]
    )
    rows = []
    outcomes = []
    for k, ((one, two), v) in enumerate(zip(((fa, fb), (fb, fa)), verdicts, strict=True)):
        rows.append(
            {
                "ranking_id": r.ranking_id,
                "a": one.id,
                "b": two.id,
                "a_hash": one.content_hash,
                "b_hash": two.content_hash,
                "shown": "ab" if k == 0 else "ba",
                "outcome": v,
                "judge": entry,
                "pair_id": pair_id,
                "lengths": [len(texts[one.id]), len(texts[two.id])],
                "ts": now,
            }
        )
        outcomes.append(one.id if v == "a" else two.id if v == "b" else "tie")
    rank.append_comparisons(data_dir, rows)
    after = _fit_for(r, data_dir, [])
    after.pop("_n")
    return {
        "recorded": True,
        "pair": [first_id, second_id],
        "judge": entry,
        "outcomes": outcomes,
        "why": f"recorded two calls of {entry} on {first_id} and {second_id}.",
        "reason": "recorded",
        "fit": after,
    }
