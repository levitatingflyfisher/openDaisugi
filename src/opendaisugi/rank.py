"""rank: which of N attempts at one task is best, and how sure we are.

``daisugi rank fit ATTEMPTS.json`` is a pure function of the attempts, the
judge comparisons and the owner's answers. It calls no model and runs no
test. Hard evidence comes first and opinions last:

1. **Eliminate.** A failed or missing edge proof, a failed or missing
   verify, a required test that failed or did not run, a required feature
   that is not green. If every attempt is out, nothing is ranked: rank never
   returns the least bad attempt.
2. **Quality tiers.** The survivors grouped by (optional tests passed,
   optional features green), larger first.
3. **Preference.** Inside a tier, Bradley-Terry over the judges' votes,
   with a weak prior: each attempt plays one virtual tie (weight ``prior``)
   against a reference of strength 1. A judge's vote counts only when it
   holds with the pair shown in both orders; when the two orders disagree it
   is a tie. A seeded bootstrap over the votes gives each strength a 90%
   interval and the leader a confidence.
4. **Cost.** Attempts the fit cannot separate are ordered by tokens, then
   wall time, then diff lines, then id.

The owner's answers on cards are constraints, not votes: each fixes the
order of one pair, and the order is the fitted one with the least change
that keeps every constraint. Answers that form a cycle are all left out,
with a warning.

Three languages must agree, so the fit is spelled out: plain ``+ - * /``,
sums in id order, a fixed iteration rule, splitmix64 for the bootstrap
seeded from a hash of what the fit reads, and every float rounded to 9
decimals before a test or a print.

The choice record (``<data dir>/journal/rankings/choices.jsonl``) and the
review queue are below the fit: a runner proceeds on the leader and opens a
card; the owner answers later with ``daisugi rank record pick|drop``; an
unanswered card decays.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
import sqlite3
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

MAX_ATTEMPTS = 8
DEFAULT_THRESHOLD = 0.9
DEFAULT_PRIOR = 1.0
DEFAULT_BOOTSTRAP = 1000
MAX_BOOTSTRAP = 10000
MAX_ITER = 500
TOL = 1e-10
DECAY_SECONDS = 7 * 86400

_RANKING_ID = re.compile(r"[A-Za-z0-9._:#-]{1,96}")
_ATTEMPT_ID = re.compile(r"[A-Za-z0-9._#-]{1,32}")
_COST_KEYS = ("tokens", "wall_ms", "diff_lines")
_RESULTS = ("pass", "fail", "not_run")
_OUTCOMES = ("a", "b", "tie", "skip")
_MASK = (1 << 64) - 1


class RankError(ValueError):
    """An attempts file that does not read; nothing was ranked."""


def r9(x: float) -> float:
    return round(x, 9)


# ---------------------------------------------------------------------------
# Reading the input
# ---------------------------------------------------------------------------


@dataclass
class Attempt:
    id: str
    content_hash: str
    author: str = ""
    edge_proof: str | None = None
    started_by_parent: bool = False
    ran_plan: bool = False
    verify: dict[str, Any] | None = None
    gate: dict[str, int] | None = None
    tests: list[dict[str, Any]] = field(default_factory=list)
    features: list[dict[str, Any]] = field(default_factory=list)
    cost: dict[str, Any] = field(default_factory=dict)
    where: Any = None
    summary: str = ""


@dataclass
class Comparison:
    a: str
    b: str
    a_hash: str
    b_hash: str
    shown: str
    outcome: str
    judge: str
    pair_id: str

    def canon(self) -> dict[str, Any]:
        return {
            "a": self.a,
            "a_hash": self.a_hash,
            "b": self.b,
            "b_hash": self.b_hash,
            "judge": self.judge,
            "outcome": self.outcome,
            "pair_id": self.pair_id,
            "shown": self.shown,
        }


@dataclass
class Policy:
    threshold: float = DEFAULT_THRESHOLD
    prior: float = DEFAULT_PRIOR
    bootstrap: int = DEFAULT_BOOTSTRAP
    judges: list[str] = field(default_factory=list)


@dataclass
class Ranking:
    ranking_id: str
    task: str
    project: str
    attempts: list[Attempt]
    comparisons: list[Comparison] | None
    owner: list[tuple[str, str]] | None
    policy: Policy


def _is_int(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def _count(v: Any, what: str) -> int:
    if not _is_int(v) or not 0 <= v <= 2**53:
        raise RankError(f"{what} must be a whole number from 0 to 2**53")
    return v


def _text(v: Any, what: str) -> str:
    if not isinstance(v, str):
        raise RankError(f"{what} must be a string")
    return v


def _flag(v: Any, what: str) -> bool:
    if not isinstance(v, bool):
        raise RankError(f"{what} must be true or false")
    return v


def _obj(v: Any, what: str) -> dict[str, Any]:
    if not isinstance(v, dict):
        raise RankError(f"{what} must be an object")
    return v


def _list(v: Any, what: str) -> list[Any]:
    if not isinstance(v, list):
        raise RankError(f"{what} must be a list")
    return v


def _number(v: Any, what: str) -> float:
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        raise RankError(f"{what} must be a number")
    try:
        f = float(v)
    except OverflowError:
        raise RankError(f"{what} must be a finite number") from None
    if not math.isfinite(f):
        raise RankError(f"{what} must be a finite number")
    return f


def _attempt(raw: Any, i: int) -> Attempt:
    o = _obj(raw, f"attempts[{i}]")
    aid = o.get("id")
    if not isinstance(aid, str) or not _ATTEMPT_ID.fullmatch(aid):
        raise RankError(f"attempts[{i}].id must be 1 to 32 of A-Z a-z 0-9 . _ # -")
    w = f"attempt {aid}"
    h = o.get("content_hash")
    if not isinstance(h, str) or not h:
        raise RankError(f"{w}: content_hash must be a string that is not empty")
    a = Attempt(id=aid, content_hash=h)
    if o.get("author") is not None:
        a.author = _text(o["author"], f"{w}: author")
    ep = o.get("edge_proof")
    if ep is not None and ep not in ("ok", "failed"):
        raise RankError(f'{w}: edge_proof must be "ok", "failed" or absent')
    a.edge_proof = ep
    if o.get("started_by_parent") is not None:
        a.started_by_parent = _flag(o["started_by_parent"], f"{w}: started_by_parent")
    if o.get("ran_plan") is not None:
        a.ran_plan = _flag(o["ran_plan"], f"{w}: ran_plan")
    if o.get("verify") is not None:
        v = _obj(o["verify"], f"{w}: verify")
        a.verify = {
            "ok": _flag(v.get("ok"), f"{w}: verify.ok"),
            "violations": _count(v.get("violations", 0), f"{w}: verify.violations"),
        }
    if o.get("gate") is not None:
        g = _obj(o["gate"], f"{w}: gate")
        a.gate = {
            "denies": _count(g.get("denies", 0), f"{w}: gate.denies"),
            "override_allows": _count(g.get("override_allows", 0), f"{w}: gate.override_allows"),
        }
    for j, t in enumerate(_list(o.get("tests", []), f"{w}: tests")):
        t = _obj(t, f"{w}: tests[{j}]")
        res = t.get("result")
        if res not in _RESULTS:
            raise RankError(f'{w}: tests[{j}].result must be "pass", "fail" or "not_run"')
        a.tests.append(
            {
                "name": _text(t.get("name"), f"{w}: tests[{j}].name"),
                "required": _flag(t.get("required", False), f"{w}: tests[{j}].required"),
                "result": res,
            }
        )
    for j, f in enumerate(_list(o.get("features", []), f"{w}: features")):
        f = _obj(f, f"{w}: features[{j}]")
        a.features.append(
            {
                "name": _text(f.get("name"), f"{w}: features[{j}].name"),
                "required": _flag(f.get("required", False), f"{w}: features[{j}].required"),
                "green": _flag(f.get("green"), f"{w}: features[{j}].green"),
            }
        )
    cost = _obj(o.get("cost", {}), f"{w}: cost")
    for k in _COST_KEYS:
        if cost.get(k) is not None:
            a.cost[k] = _count(cost[k], f"{w}: cost.{k}")
    est = _list(cost.get("estimated", []), f"{w}: cost.estimated")
    for k in est:
        if k not in _COST_KEYS:
            raise RankError(f"{w}: cost.estimated names tokens, wall_ms or diff_lines only")
    if est:
        a.cost["estimated"] = sorted(set(est), key=_COST_KEYS.index)
    a.where = o.get("where")
    pv = o.get("previews")
    if pv is not None:
        pv = _obj(pv, f"{w}: previews")
        if pv.get("summary") is not None:
            a.summary = _text(pv["summary"], f"{w}: previews.summary")
    return a


def _comparison(raw: Any, i: int) -> Comparison:
    o = _obj(raw, f"comparisons[{i}]")
    w = f"comparisons[{i}]"
    shown = o.get("shown")
    if shown not in ("ab", "ba"):
        raise RankError(f'{w}.shown must be "ab" or "ba"')
    outcome = o.get("outcome")
    if outcome not in _OUTCOMES:
        raise RankError(f'{w}.outcome must be "a", "b", "tie" or "skip"')
    c = Comparison(
        a=_text(o.get("a"), f"{w}.a"),
        b=_text(o.get("b"), f"{w}.b"),
        a_hash=_text(o.get("a_hash"), f"{w}.a_hash"),
        b_hash=_text(o.get("b_hash"), f"{w}.b_hash"),
        shown=shown,
        outcome=outcome,
        judge=_text(o.get("judge"), f"{w}.judge"),
        pair_id=_text(o.get("pair_id"), f"{w}.pair_id"),
    )
    if c.a == c.b:
        raise RankError(f"{w} compares {c.a} with itself")
    return c


def _policy(raw: Any) -> Policy:
    p = Policy()
    if raw is None:
        return p
    o = _obj(raw, "policy")
    if o.get("threshold") is not None:
        p.threshold = _number(o["threshold"], "policy.threshold")
        if not 0.5 <= p.threshold <= 1.0:
            raise RankError("policy.threshold must be from 0.5 to 1")
    if o.get("prior") is not None:
        p.prior = _number(o["prior"], "policy.prior")
        if not 0.01 <= p.prior <= 100.0:
            raise RankError("policy.prior must be from 0.01 to 100")
    if o.get("bootstrap") is not None:
        b = o["bootstrap"]
        if not _is_int(b) or not 1 <= b <= MAX_BOOTSTRAP:
            raise RankError(f"policy.bootstrap must be a whole number from 1 to {MAX_BOOTSTRAP}")
        p.bootstrap = b
    if o.get("judges") is not None:
        for j, name in enumerate(_list(o["judges"], "policy.judges")):
            p.judges.append(_text(name, f"policy.judges[{j}]"))
    return p


def parse(doc: Any) -> Ranking:
    """The attempts document, checked. Raises RankError at the first problem."""
    o = _obj(doc, "the attempts file")
    rid = o.get("ranking_id")
    if not isinstance(rid, str) or not _RANKING_ID.fullmatch(rid):
        raise RankError("ranking_id must be 1 to 96 of A-Z a-z 0-9 . _ : # -")
    task = "" if o.get("task") is None else _text(o["task"], "task")
    project = "" if o.get("project") is None else _text(o["project"], "project")
    raw_attempts = _list(o.get("attempts"), "attempts")
    if not 1 <= len(raw_attempts) <= MAX_ATTEMPTS:
        raise RankError(f"attempts must hold 1 to {MAX_ATTEMPTS} attempts")
    attempts = [_attempt(a, i) for i, a in enumerate(raw_attempts)]
    seen: set[str] = set()
    for a in attempts:
        if a.id in seen:
            raise RankError(f"attempt id {a.id} is used twice")
        seen.add(a.id)
    comparisons = None
    if "comparisons" in o and o["comparisons"] is not None:
        comparisons = [
            _comparison(c, i) for i, c in enumerate(_list(o["comparisons"], "comparisons"))
        ]
    owner = None
    if "owner_constraints" in o and o["owner_constraints"] is not None:
        owner = []
        for i, pair in enumerate(_list(o["owner_constraints"], "owner_constraints")):
            if (
                not isinstance(pair, list)
                or len(pair) != 2
                or not all(isinstance(x, str) for x in pair)
            ):
                raise RankError(f"owner_constraints[{i}] must be a list of two attempt ids")
            for x in pair:
                if x not in seen:
                    raise RankError(f"owner_constraints[{i}] names no attempt {x}")
            if pair[0] == pair[1]:
                raise RankError(f"owner_constraints[{i}] puts {pair[0]} before itself")
            owner.append((pair[0], pair[1]))
    return Ranking(
        ranking_id=rid,
        task=task,
        project=project,
        attempts=attempts,
        comparisons=comparisons,
        owner=owner,
        policy=_policy(o.get("policy")),
    )


# ---------------------------------------------------------------------------
# The journal: comparisons and choices, read without making anything
# ---------------------------------------------------------------------------


def rankings_dir(data_dir: Path) -> Path:
    return Path(data_dir) / "journal" / "rankings"


def _read_rows(path: Path) -> tuple[list[dict[str, Any]], int]:
    """The JSON object rows of a JSONL file, and how many lines did not read."""
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return [], 0
    rows: list[dict[str, Any]] = []
    bad = 0
    for line in text.split("\n"):
        if not line.strip():
            continue
        try:
            obj = json.loads(line)
        except (ValueError, RecursionError):
            bad += 1
            continue
        if isinstance(obj, dict):
            rows.append(obj)
        else:
            bad += 1
    return rows, bad


def journal_comparisons(data_dir: Path, ranking_id: str) -> tuple[list[Comparison], int]:
    rows, bad = _read_rows(rankings_dir(data_dir) / "comparisons.jsonl")
    out: list[Comparison] = []
    for row in rows:
        if row.get("ranking_id") != ranking_id:
            continue
        try:
            out.append(_comparison(row, len(out)))
        except RankError:
            bad += 1
    return out, bad


# ---------------------------------------------------------------------------
# The fit
# ---------------------------------------------------------------------------


def eliminate(a: Attempt) -> list[str]:
    """Every reason the attempt is out, in a fixed order; empty when it stays."""
    why: list[str] = []
    if a.edge_proof == "failed":
        why.append("edge proof failed: never ran")
    elif a.started_by_parent and a.edge_proof is None:
        why.append("edge proof missing")
    if a.verify is not None and not a.verify["ok"]:
        why.append(f"verify failed: {a.verify['violations']} violations")
    elif a.verify is None and a.ran_plan:
        why.append("verify missing")
    for t in a.tests:
        if t["required"] and t["result"] == "fail":
            why.append(f"required test failed: {t['name']}")
        elif t["required"] and t["result"] == "not_run":
            why.append(f"required test not run: {t['name']}")
    for f in a.features:
        if f["required"] and not f["green"]:
            why.append(f"required feature not green: {f['name']}")
    return why


def quality_key(a: Attempt) -> tuple[int, int]:
    tests = 0
    for t in a.tests:
        if not t["required"] and t["result"] == "pass":
            tests += 1
    feats = 0
    for f in a.features:
        if not f["required"] and f["green"]:
            feats += 1
    return (tests, feats)


@dataclass
class Vote:
    i: str  # the lower id
    j: str  # the higher id
    win: float  # 1.0 when i won, 0.0 when j won, 0.5 for a tie
    judge: str


def _verdict(c: Comparison) -> str | None:
    """The winner's attempt id, "tie", or None for a skip."""
    if c.outcome == "skip":
        return None
    if c.outcome == "tie":
        return "tie"
    return c.a if c.outcome == "a" else c.b


def votes_from(
    comparisons: list[Comparison], by_id: dict[str, Attempt], warnings: list[str]
) -> list[Vote]:
    """The judges' votes: a judge's two calls on one pair (one per order)
    make a vote when neither skipped. One vote per judge per pair."""
    groups: dict[tuple[str, str], list[Comparison]] = {}
    order: list[tuple[str, str]] = []
    for c in comparisons:
        for x in (c.a, c.b):
            if x not in by_id:
                warnings.append(f"a comparison by {c.judge} names no attempt {x}; ignored")
                break
        else:
            if c.a_hash != by_id[c.a].content_hash or c.b_hash != by_id[c.b].content_hash:
                warnings.append(
                    f"a comparison by {c.judge} on {c.a} and {c.b} binds to a hash that "
                    "changed; ignored"
                )
                continue
            key = (c.judge, c.pair_id)
            if key not in groups:
                groups[key] = []
                order.append(key)
            groups[key].append(c)
    kept: dict[tuple[str, str, str], tuple[str, Vote]] = {}
    for key in order:
        rows = groups[key]
        judge, pair_id = key
        pair = sorted({rows[0].a, rows[0].b})
        if (
            len(rows) != 2
            or sorted({r.shown for r in rows}) != ["ab", "ba"]
            or any(sorted({r.a, r.b}) != pair for r in rows)
        ):
            warnings.append(
                f"judge {judge} call {pair_id} does not hold the pair once in each order; no vote"
            )
            continue
        v1, v2 = _verdict(rows[0]), _verdict(rows[1])
        if v1 is None or v2 is None:
            continue
        i, j = pair
        if v1 == v2 and v1 != "tie":
            win = 1.0 if v1 == i else 0.0
        else:
            win = 0.5
        vk = (judge, i, j)
        if vk in kept:
            warnings.append(f"judge {judge} voted more than once on {i} and {j}; one vote kept")
            if pair_id >= kept[vk][0]:
                continue
        kept[vk] = (pair_id, Vote(i=i, j=j, win=win, judge=judge))
    return [kept[k][1] for k in sorted(kept, key=lambda k: (k[1], k[2], k[0]))]


def bt_fit(members: list[str], votes: list[Vote], prior: float) -> dict[str, float]:
    """Bradley-Terry by the MM (Zermelo) iteration, all at once from the
    last round's strengths. Each member also plays a tie of weight ``prior``
    against a reference of strength 1. Members are in id order."""
    idx = {m: k for k, m in enumerate(members)}
    n = len(members)
    wins = [prior / 2.0] * n
    games = [[0.0] * n for _ in range(n)]
    for v in votes:
        a, b = idx[v.i], idx[v.j]
        wins[a] = wins[a] + v.win
        wins[b] = wins[b] + (1.0 - v.win)
        games[a][b] = games[a][b] + 1.0
        games[b][a] = games[b][a] + 1.0
    s = [1.0] * n
    for _ in range(MAX_ITER):
        new = [0.0] * n
        for a in range(n):
            denom = prior / (s[a] + 1.0)
            for b in range(n):
                if b != a and games[a][b] != 0.0:
                    denom = denom + games[a][b] / (s[a] + s[b])
            new[a] = wins[a] / denom
        diff = 0.0
        for a in range(n):
            d = abs(new[a] - s[a])
            if d > diff:
                diff = d
        s = new
        if diff < TOL:
            break
    return {m: s[idx[m]] for m in members}


def _p_ref(s: float) -> float:
    return s / (s + 1.0)


class SplitMix64:
    def __init__(self, seed: int) -> None:
        self.state = seed & _MASK

    def next(self) -> int:
        self.state = (self.state + 0x9E3779B97F4A7C15) & _MASK
        z = self.state
        z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & _MASK
        z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & _MASK
        return z ^ (z >> 31)


def canonical(obj: Any) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def seed_of(r: Ranking, comparisons: list[Comparison], owner: list[tuple[str, str]]) -> str:
    """The hex of the first 8 bytes of sha256 over what the fit reads."""
    doc = {
        "attempts": [
            {
                "content_hash": a.content_hash,
                "cost": a.cost,
                "edge_proof": a.edge_proof,
                "features": a.features,
                "id": a.id,
                "ran_plan": a.ran_plan,
                "started_by_parent": a.started_by_parent,
                "tests": a.tests,
                "verify": a.verify,
            }
            for a in r.attempts
        ],
        "comparisons": [c.canon() for c in comparisons],
        "owner_constraints": [list(p) for p in owner],
        "policy": {
            "bootstrap": r.policy.bootstrap,
            "judges": r.policy.judges,
            "prior": r.policy.prior,
            "threshold": r.policy.threshold,
        },
        "ranking_id": r.ranking_id,
    }
    return hashlib.sha256(canonical(doc).encode("utf-8")).hexdigest()[:16]


def _closure(edges: list[tuple[str, str]]) -> set[tuple[str, str]]:
    succ: dict[str, set[str]] = {}
    for w, lo in edges:
        succ.setdefault(w, set()).add(lo)
    out: set[tuple[str, str]] = set()
    for start in sorted(succ):
        todo = sorted(succ[start])
        seen: set[str] = set()
        while todo:
            x = todo.pop()
            if x in seen:
                continue
            seen.add(x)
            out.add((start, x))
            todo.extend(sorted(succ.get(x, ())))
    return out


def owner_edges(
    pairs: list[tuple[str, str]], survivors: set[str], warnings: list[str]
) -> list[tuple[str, str]]:
    """The owner's pairs that apply: among survivors, deduplicated, with
    every pair inside a cycle left out (and named)."""
    edges: list[tuple[str, str]] = []
    for p in pairs:
        if p[0] in survivors and p[1] in survivors and p not in edges:
            edges.append(p)
    clo = _closure(edges)
    cyclic = sorted({x for (x, y) in clo if (y, x) in clo})
    if cyclic:
        warnings.append(
            f"the owner's answers form a cycle among {', '.join(cyclic)}; none of them applied"
        )
        edges = [(w, lo) for (w, lo) in edges if not (w in cyclic and lo in cyclic)]
    return edges


def apply_owner(base: list[str], edges: list[tuple[str, str]]) -> list[str]:
    """The base order with the least change that keeps every edge: take the
    first item in base order whose predecessors are all placed."""
    preds: dict[str, set[str]] = {x: set() for x in base}
    for w, lo in edges:
        preds[lo].add(w)
    placed: list[str] = []
    left = list(base)
    while left:
        for x in left:
            if preds[x] <= set(placed):
                placed.append(x)
                left.remove(x)
                break
    return placed


def _cost_key(a: Attempt) -> tuple[int, int, int, int, int, int]:
    """Unknown costs sort last."""
    out: list[int] = []
    for k in _COST_KEYS:
        v = a.cost.get(k)
        out.extend((1, 0) if v is None else (0, v))
    return tuple(out)  # type: ignore[return-value]


def _join(items: list[str]) -> str:
    if len(items) <= 1:
        return "".join(items)
    return ", ".join(items[:-1]) + " and " + items[-1]


def fit(
    r: Ranking,
    *,
    comparisons: list[Comparison] | None = None,
    owner: list[tuple[str, str]] | None = None,
    warnings: list[str] | None = None,
) -> dict[str, Any]:
    """The ranking of ``r``. ``comparisons`` and ``owner`` stand in for the
    file's own when it has none."""
    warnings = list(warnings or [])
    comps = r.comparisons if r.comparisons is not None else (comparisons or [])
    owner_pairs = r.owner if r.owner is not None else (owner or [])
    by_id = {a.id: a for a in r.attempts}
    eliminated = []
    survivors: list[Attempt] = []
    for a in r.attempts:
        why = eliminate(a)
        if why:
            eliminated.append({"id": a.id, "reasons": why})
        else:
            survivors.append(a)
            if a.gate and a.gate["override_allows"]:
                warnings.append(
                    f"{a.id} was let out of its envelope {a.gate['override_allows']} times "
                    "by an operator allow; it is kept"
                )
    seed_hex = seed_of(r, comps, owner_pairs)
    out: dict[str, Any] = {
        "ranking_id": r.ranking_id,
        "status": "none_survived",
        "eliminated": eliminated,
        "quality_tiers": [],
        "order": [],
        "decided_by": [],
        "scores": {},
        "leader": None,
        "confidence": None,
        "label": "no_label",
        "next": None,
        "stop": "nothing_to_ask",
        "owner_constraints": [],
        "warnings": warnings,
        "seed": seed_hex,
    }
    if not survivors:
        return out
    surv_ids = {a.id for a in survivors}
    # Quality tiers, best first; members in id order.
    keys = sorted({quality_key(a) for a in survivors}, reverse=True)
    tiers = [sorted(a.id for a in survivors if quality_key(a) == k) for k in keys]
    tier_of = {m: t for t, members in enumerate(tiers) for m in members}
    if len(survivors) > 1 and len(tiers) == 1 and keys[0] == (0, 0):
        warnings.append(
            "no optional test or feature separates the attempts; only opinion ranks them"
        )
    survivor_by_id = {a.id: a for a in survivors}
    votes = [
        v
        for v in votes_from(comps, by_id, warnings)
        if v.i in surv_ids and v.j in surv_ids and tier_of[v.i] == tier_of[v.j]
    ]
    edges = owner_edges(owner_pairs, surv_ids, warnings)
    closure = _closure(edges)
    prior = r.policy.prior
    point: dict[str, float] = {}
    tier_votes: list[list[Vote]] = []
    for members in tiers:
        tv = [v for v in votes if v.i in members]
        tier_votes.append(tv)
        point.update(bt_fit(members, tv, prior))
    # The seeded bootstrap.
    rng = SplitMix64(int(seed_hex, 16))
    B = r.policy.bootstrap
    samples: dict[str, list[float]] = {m: [] for m in surv_ids}
    beats: dict[tuple[str, str], int] = {}
    boots: list[dict[str, float]] = []
    for _ in range(B):
        refit: dict[str, float] = {}
        for members, tv in zip(tiers, tier_votes, strict=True):
            if len(members) < 2:
                refit.update(bt_fit(members, [], prior))
                continue
            draw = [tv[rng.next() % len(tv)] for _ in range(len(tv))] if tv else []
            refit.update(bt_fit(members, draw, prior))
        boots.append(refit)
        for m in surv_ids:
            samples[m].append(_p_ref(refit[m]))
    for members in tiers:
        for x in members:
            for y in members:
                if x == y:
                    continue
                n = 0
                for refit in boots:
                    if r9(refit[x]) > r9(refit[y]):
                        n += 1
                beats[(x, y)] = n
    # The order inside each tier: strength, then the cost classes.
    base: list[str] = []
    klass: dict[str, int] = {}
    next_class = 0
    for members in tiers:
        pref = sorted(members, key=lambda m: (-r9(_p_ref(point[m])), m))
        groups: list[list[str]] = [[pref[0]]]
        for prev, cur in zip(pref, pref[1:], strict=False):
            share = r9(beats[(prev, cur)] / B)
            if share >= r.policy.threshold:
                groups.append([cur])
            else:
                groups[-1].append(cur)
        for g in groups:
            for m in sorted(g, key=lambda m: (_cost_key(survivor_by_id[m]), m)):
                base.append(m)
                klass[m] = next_class
            next_class += 1
    order = apply_owner(base, edges)
    pos = {m: k for k, m in enumerate(base)}
    decided: list[str] = []
    for x, y in zip(order, order[1:], strict=False):
        if (x, y) in edges or pos[x] > pos[y]:
            decided.append("owner")
        elif tier_of[x] != tier_of[y]:
            decided.append("quality")
        elif klass[x] != klass[y]:
            decided.append("preference")
        elif _cost_key(survivor_by_id[x]) != _cost_key(survivor_by_id[y]):
            decided.append("cost")
        else:
            decided.append("tie")
    lo_i = (5 * (B - 1)) // 100
    hi_i = (95 * (B - 1)) // 100
    scores: dict[str, Any] = {}
    for m in order:
        vals = sorted(samples[m])
        nv = 0
        for v in votes:
            if m in (v.i, v.j):
                nv += 1
        scores[m] = {
            "strength": r9(_p_ref(point[m])),
            "lo": r9(vals[lo_i]),
            "hi": r9(vals[hi_i]),
            "votes": nv,
        }
    leader = order[0]
    rivals = [m for m in tiers[tier_of[leader]] if m != leader]
    if rivals:
        n = 0
        for refit in boots:
            if all((leader, m) in closure or r9(refit[leader]) > r9(refit[m]) for m in rivals):
                n += 1
        confidence = r9(n / B)
    else:
        confidence = 1.0
    owner_pick = any(w == leader for (w, _) in edges)
    out.update(
        {
            "quality_tiers": tiers,
            "order": order,
            "decided_by": decided,
            "scores": scores,
            "leader": leader,
            "confidence": confidence,
            "owner_constraints": [[w, lo] for (w, lo) in edges],
        }
    )
    if len(survivors) == 1:
        out.update({"status": "single", "label": leader, "stop": "nothing_to_ask"})
        return out
    if owner_pick or confidence >= r.policy.threshold:
        out.update({"status": "ranked", "label": leader, "stop": "confident"})
        return out
    out.update({"status": "provisional", "label": "no_label"})
    # The next pair for a model judge: the challenger nearest to even.
    voted = {(v.judge, v.i, v.j) for v in votes}

    def nearness(m: str) -> tuple[float, int, str]:
        p = r9(point[leader] / (point[leader] + point[m]))
        nv = 0
        for v in votes:
            if {v.i, v.j} == {leader, m}:
                nv += 1
        return (r9(abs(p - 0.5)), nv, m)

    for m in sorted(rivals, key=nearness):
        i, j = sorted((leader, m))
        for judge in r.policy.judges:
            if (judge, i, j) not in voted:
                p = r9(point[leader] / (point[leader] + point[m]))
                out["next"] = {
                    "pair": [leader, m],
                    "judge": judge,
                    "why": f"P({leader} beats {m}) is {p!r}, the nearest to even",
                }
                out["stop"] = None
                return out
    out["stop"] = "judges_exhausted"
    return out


EXIT = {"ranked": 0, "single": 0, "provisional": 3, "none_survived": 1}


def fit_text(res: dict[str, Any]) -> str:
    """The ranking as ``daisugi rank fit`` prints it."""
    lines = [f"Ranking {res['ranking_id']}: {res['status']}"]
    for k, m in enumerate(res["order"]):
        sc = res["scores"][m]
        how = f" [{res['decided_by'][k - 1]}]" if k else ""
        lines.append(
            f"  {k + 1}. {m}: strength {sc['strength']!r} ({sc['lo']!r} to {sc['hi']!r}), "
            f"{sc['votes']} votes{how}"
        )
    for e in res["eliminated"]:
        lines.append(f"  out: {e['id']}: {'; '.join(e['reasons'])}")
    if res["leader"] is not None:
        lines.append(
            f"Leader: {res['leader']} (confidence {res['confidence']!r}); label: {res['label']}"
        )
    else:
        lines.append("No attempt survived; nothing is chosen.")
    for w, lo in res["owner_constraints"]:
        lines.append(f"Owner: {w} before {lo}")
    if res["next"] is not None:
        n = res["next"]
        lines.append(f"Next: judge {n['judge']} on {n['pair'][0]} and {n['pair'][1]}: {n['why']}")
    else:
        lines.append(f"Stop: {res['stop']}")
    for w in res["warnings"]:
        lines.append(f"Warning: {w}")
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# Choices and cards
# ---------------------------------------------------------------------------

CLOSES = ("confirmed", "overridden", "ignored")
OWNER_HOWS = ("answer", "drop", "permanent_ask")


def choice_id(ranking_id: str, survivors: list[tuple[str, str]]) -> str:
    text = canonical([ranking_id, sorted([list(s) for s in survivors])])
    return "ch_" + hashlib.sha256(text.encode("utf-8")).hexdigest()[:12]


@dataclass
class Card:
    """The fold of one choice's rows."""

    opened: dict[str, Any]
    close: dict[str, Any] | None = None
    answer: dict[str, Any] | None = None
    # The runs that resumed the plan while this choice was open, each once,
    # in file order. Their later steps count toward the switch cost.
    resumed: list[str] = field(default_factory=list)

    @property
    def id(self) -> str:
        return self.opened["choice_id"]


def _finite(v: Any) -> float | None:
    """A JSON number as a finite float, or None."""
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return None
    try:
        f = float(v)
    except OverflowError:
        return None
    return f if math.isfinite(f) else None


def _valid_open(row: dict[str, Any]) -> bool:
    opts = row.get("options")
    if not (
        isinstance(row.get("choice_id"), str)
        and isinstance(row.get("ranking_id"), str)
        and isinstance(row.get("chosen"), str)
        and _finite(row.get("ts")) is not None
        and isinstance(opts, dict)
        and isinstance(opts.get("survivors"), list)
    ):
        return False
    for s in opts["survivors"]:
        if not (
            isinstance(s, dict)
            and isinstance(s.get("id"), str)
            and isinstance(s.get("content_hash"), str)
        ):
            return False
    return True


def read_cards(data_dir: Path) -> list[Card]:
    """Every choice, folded from the rows in file order: the first
    ``opened`` row makes it, the first close closes it, and the first owner
    answer (before or after a decay) is its label."""
    rows, _ = _read_rows(rankings_dir(data_dir) / "choices.jsonl")
    cards: dict[str, Card] = {}
    for row in rows:
        cid = row.get("choice_id")
        if not isinstance(cid, str):
            continue
        ev = row.get("event")
        if ev == "opened":
            if cid not in cards and _valid_open(row):
                cards[cid] = Card(opened=row)
            continue
        card = cards.get(cid)
        if card is None:
            continue
        if ev == "resumed":
            run = row.get("run_id")
            if isinstance(run, str) and run not in card.resumed:
                card.resumed.append(run)
            continue
        if ev not in CLOSES:
            continue
        if ev in ("confirmed", "overridden"):
            if row.get("how") not in OWNER_HOWS:
                continue
            if ev == "overridden" and not isinstance(row.get("pick"), str):
                continue
            if card.answer is None:
                card.answer = row
        elif row.get("how") != "decay":
            continue
        if card.close is None:
            card.close = row
    return list(cards.values())


def owner_answers(
    data_dir: Path, ranking_id: str, by_id: dict[str, Attempt], warnings: list[str]
) -> list[tuple[str, str]]:
    """The pairs the owner's card answers fix, for one ranking. An answer
    whose attempts changed since is left out, with a warning."""
    out: list[tuple[str, str]] = []
    for card in read_cards(data_dir):
        if card.opened["ranking_id"] != ranking_id or card.answer is None:
            continue
        surv = {s["id"]: s["content_hash"] for s in card.opened["options"]["survivors"]}
        if any(x not in by_id or by_id[x].content_hash != h for x, h in surv.items()):
            warnings.append(
                f"the owner's answer on {card.id} binds to attempts that changed; ignored"
            )
            continue
        chosen = card.opened["chosen"]
        if card.answer["event"] == "confirmed":
            pairs = [(chosen, x) for x in surv if x != chosen]
        else:
            pick = card.answer["pick"]
            if pick not in surv or pick == chosen:
                continue
            pairs = [(pick, chosen)]
        for p in pairs:
            if p not in out:
                out.append(p)
    return out


# Switch cost: what a later pick would take.
CHEAP = "cheap"
COSTLY = "costly"
FOLLOW_UP = "follow_up_only"


def _receipts(data_dir: Path, run_id: str) -> list[tuple[str, str, float]]:
    """(step, reversibility, timestamp) of a run's receipts, read-only; none
    when there is no index or it does not read."""
    db = Path(data_dir) / "journal" / "index.db"
    if not db.exists():
        return []
    try:
        con = sqlite3.connect(db.absolute().as_uri() + "?mode=ro", uri=True)
    except sqlite3.Error:
        return []
    try:
        rows = con.execute(
            "SELECT step_id, reversibility, timestamp FROM receipts WHERE run_id = ? "
            "ORDER BY timestamp, step_id",
            (run_id,),
        ).fetchall()
    except sqlite3.Error:
        return []
    finally:
        con.close()
    return [
        (str(s), r if isinstance(r, str) else "", float(t))
        for s, r, t in rows
        if isinstance(t, (int, float))
    ]


def switch_cost(card: Card, data_dir: Path, to: str | None = None) -> dict[str, Any]:
    """The switch cost of a card, from the ledger and the places the
    alternatives live: to the cheapest alternative, or to ``to``."""
    facts = card.opened.get("facts") if isinstance(card.opened.get("facts"), dict) else {}
    chosen = card.opened["chosen"]
    alts = [s["id"] for s in card.opened["options"]["survivors"] if s["id"] != chosen]
    if to is not None:
        alts = [to]
    where = facts.get("where") if isinstance(facts.get("where"), dict) else {}
    gone = []
    for a in alts:
        w = where.get(a)
        if isinstance(w, dict) and isinstance(w.get("path"), str) and not os.path.exists(w["path"]):
            gone.append(a)
    run = facts.get("run") if isinstance(facts.get("run"), dict) else None
    later: list[tuple[str, str, float]] = []
    if run and isinstance(run.get("run_id"), str) and isinstance(run.get("downstream"), list):
        down = {d for d in run["downstream"] if isinstance(d, str)}
        runs = [run["run_id"], *(r for r in card.resumed if r != run["run_id"])]
        for rid in runs:
            later += [r for r in _receipts(data_dir, rid) if r[0] in down]
        later.sort(key=lambda r: (r[2], r[0]))
    hard = [r for r in later if r[1] not in ("none", "reversible")]
    undo = [r for r in later if r[1] == "reversible"]
    if hard:
        return {
            "cost": FOLLOW_UP,
            "undo_steps": 0,
            "fired_at": hard[0][2],
            "text": f"Cannot undo: step {hard[0][0]} ran on this choice and cannot be undone. "
            "Switching starts a follow-up task.",
        }
    if alts and len(gone) == len(alts):
        return {
            "cost": FOLLOW_UP,
            "undo_steps": 0,
            "fired_at": None,
            "text": f"Cannot undo: the place of {_join(gone)} is gone. "
            "Switching starts a follow-up task.",
        }
    if undo:
        return {
            "cost": COSTLY,
            "undo_steps": len(undo),
            "fired_at": None,
            "text": f"Switching undoes the later steps {_join(sorted({r[0] for r in undo}))} "
            "and applies the alternative.",
        }
    return {
        "cost": CHEAP,
        "undo_steps": 0,
        "fired_at": None,
        "text": "Switching applies the alternative; no later step that changes anything ran on this choice.",
    }


def decay(card: Card, data_dir: Path, now: float) -> dict[str, Any] | None:
    """The ``ignored`` row an open card's decay would append, or None."""
    sc = switch_cost(card, data_dir)
    fires: list[tuple[float, str]] = []
    if sc["cost"] == FOLLOW_UP:
        fires.append((now if sc["fired_at"] is None else sc["fired_at"], "follow_up_only"))
    due = _finite(card.opened["ts"]) + DECAY_SECONDS
    if now >= due:
        fires.append((due, "time"))
    if not fires:
        return None
    at, trigger = min(fires)
    return {
        "choice_id": card.id,
        "ranking_id": card.opened["ranking_id"],
        "event": "ignored",
        "how": "decay",
        "trigger": trigger,
        "fired_at": at,
        "ts": now,
    }


def append_rows(data_dir: Path, rows: list[dict[str, Any]]) -> None:
    if not rows:
        return
    d = rankings_dir(data_dir)
    d.mkdir(parents=True, exist_ok=True)
    with (d / "choices.jsonl").open("a", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row) + "\n")
        fh.flush()
        os.fsync(fh.fileno())


def sweep(data_dir: Path, now: float) -> list[dict[str, Any]]:
    """For a writer: the ``ignored`` rows of every open card that decayed."""
    rows = []
    for card in read_cards(data_dir):
        if card.close is None:
            row = decay(card, data_dir, now)
            if row is not None:
                rows.append(row)
    return rows


def opened_row(
    r: Ranking,
    res: dict[str, Any],
    *,
    now: float,
    run: dict[str, Any] | None = None,
) -> dict[str, Any]:
    by_id = {a.id: a for a in r.attempts}
    surv = [(m, by_id[m].content_hash) for m in res["order"]]
    return {
        "choice_id": choice_id(r.ranking_id, surv),
        "ranking_id": r.ranking_id,
        "project": r.project,
        "task": r.task,
        "event": "opened",
        "options": {
            "survivors": [{"id": m, "content_hash": h} for m, h in surv],
            "eliminated": res["eliminated"],
        },
        "chosen": res["leader"],
        "status": res["status"],
        "facts": {
            "order": res["order"],
            "decided_by": res["decided_by"],
            "quality_tiers": res["quality_tiers"],
            "confidence": res["confidence"],
            "cost": {m: by_id[m].cost for m in res["order"]},
            "summary": {m: by_id[m].summary for m in res["order"] if by_id[m].summary},
            "where": {m: by_id[m].where for m in res["order"] if by_id[m].where is not None},
            "run": run,
        },
        "ts": now,
    }


_WHY = {
    "quality": "{x} passed more optional tests or features than {y}.",
    "preference": "The judges preferred {x} to {y}.",
    "cost": "Nothing but cost separated {x} and {y}; {x} cost less.",
    "tie": "Nothing separated {x} and {y}; {x} came first by name.",
    "owner": "You placed {x} before {y}.",
}


def _named(card: Card, m: str) -> str:
    facts = card.opened.get("facts") if isinstance(card.opened.get("facts"), dict) else {}
    summ = facts.get("summary") if isinstance(facts.get("summary"), dict) else {}
    s = summ.get(m)
    return f"{m} ({s})" if isinstance(s, str) and s else m


def card_view(card: Card, data_dir: Path, now: float) -> dict[str, Any]:
    """One card as the queue shows it."""
    o = card.opened
    opened_at = _finite(o["ts"])
    facts = o.get("facts") if isinstance(o.get("facts"), dict) else {}
    chosen = o["chosen"]
    alts = [s["id"] for s in o["options"]["survivors"] if s["id"] != chosen]
    elim = o["options"].get("eliminated")
    elim = (
        [e for e in elim if isinstance(e, dict) and isinstance(e.get("id"), str)]
        if isinstance(elim, list)
        else []
    )
    head = f"Kept {_named(card, chosen)} over {_join([_named(card, a) for a in alts])}"
    if elim:
        head += f"; {_join([e['id'] for e in elim])} failed a required check"
    head += "."
    order = facts.get("order") if isinstance(facts.get("order"), list) else []
    decided = facts.get("decided_by") if isinstance(facts.get("decided_by"), list) else []
    why = ""
    if (
        len(order) > 1
        and decided
        and decided[0] in _WHY
        and isinstance(order[0], str)
        and isinstance(order[1], str)
    ):
        why = _WHY[decided[0]].format(x=order[0], y=order[1])
    conf = _finite(facts.get("confidence"))
    if conf is not None:
        why = (why + f" Confidence {r9(conf)!r}.").strip()
    sc = switch_cost(card, data_dir)
    costs = facts.get("cost") if isinstance(facts.get("cost"), dict) else {}

    def lines_of(m: str) -> int | None:
        c = costs.get(m)
        v = c.get("diff_lines") if isinstance(c, dict) else None
        return v if _is_int(v) and 0 <= v <= 2**53 else None

    base = lines_of(chosen)
    impact = 0
    for a in alts:
        la = lines_of(a)
        if base is not None and la is not None and abs(la - base) > impact:
            impact = abs(la - base)
    return {
        "choice_id": card.id,
        "ranking_id": o["ranking_id"],
        "project": o.get("project") if isinstance(o.get("project"), str) else "",
        "task": o.get("task") if isinstance(o.get("task"), str) else "",
        "chosen": chosen,
        "alternatives": alts,
        "eliminated": [e["id"] for e in elim],
        "status": o.get("status") if isinstance(o.get("status"), str) else None,
        "confidence": conf,
        "opened_at": opened_at,
        "decays_in_days": max(0, math.ceil((opened_at + DECAY_SECONDS - now) / 86400)),
        "headline": head,
        "why": why,
        "switch": {"cost": sc["cost"], "undo_steps": sc["undo_steps"], "text": sc["text"]},
        "impact_lines": impact,
    }


SORTS = ("reversibility", "impact", "date", "project")
_COST_RANK = {CHEAP: 0, COSTLY: 1, FOLLOW_UP: 2}


def sort_cards(views: list[dict[str, Any]], by: str) -> list[dict[str, Any]]:
    if by == "reversibility":
        return sorted(
            views, key=lambda v: (_COST_RANK[v["switch"]["cost"]], v["opened_at"], v["choice_id"])
        )
    if by == "impact":
        return sorted(
            views,
            key=lambda v: (
                -v["impact_lines"],
                1.0 if v["confidence"] is None else float(v["confidence"]),
                v["choice_id"],
            ),
        )
    if by == "date":
        return sorted(views, key=lambda v: (-v["opened_at"], v["choice_id"]))
    return sorted(views, key=lambda v: (v["project"], -v["opened_at"], v["choice_id"]))


def queue(data_dir: Path, now: float, by: str) -> dict[str, Any]:
    """The open cards; decay is computed, never written."""
    views = []
    decayed = 0
    for card in read_cards(data_dir):
        if card.close is not None:
            continue
        if decay(card, data_dir, now) is not None:
            decayed += 1
            continue
        views.append(card_view(card, data_dir, now))
    return {"cards": sort_cards(views, by), "decayed": decayed}


def _iso(t: float) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(t))


def queue_text(doc: dict[str, Any]) -> str:
    cards = doc["cards"]
    if not cards:
        lines = ["No open cards."]
    else:
        lines = [f"{len(cards)} open cards:"]
    for v in cards:
        where = f", {v['project']}" if v["project"] else ""
        lines.append(f"{v['choice_id']} ({v['ranking_id']}{where}), opened {_iso(v['opened_at'])}")
        lines.append(f"  {v['headline']}")
        if v["why"]:
            lines.append(f"  Why: {v['why']}")
        lines.append(f"  Switching ({v['switch']['cost']}): {v['switch']['text']}")
        lines.append(
            f"  Decays in {v['decays_in_days']} days unless switching gets expensive first."
        )
    if doc["decayed"]:
        lines.append(
            f"{doc['decayed']} cards decayed unread; the next writer records them as ignored."
        )
    return "\n".join(lines) + "\n"


def now() -> float:
    return time.time()


__all__ = [
    "Attempt",
    "Card",
    "Comparison",
    "EXIT",
    "Policy",
    "RankError",
    "Ranking",
    "SplitMix64",
    "append_rows",
    "bt_fit",
    "card_view",
    "choice_id",
    "decay",
    "fit",
    "fit_text",
    "journal_comparisons",
    "opened_row",
    "owner_answers",
    "parse",
    "queue",
    "queue_text",
    "read_cards",
    "seed_of",
    "sweep",
    "switch_cost",
    "votes_from",
]
