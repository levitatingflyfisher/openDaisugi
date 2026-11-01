"""Synthetic rank cases: `daisugi rank fit|choose|queue|record`, run through
the Python oracle.

    uv run --no-sync python clients/rank_cases.py [--out clients/fixtures/rank] [--only NAME]

A case runs as a garden case does (clients/garden_cases.py): a scratch
HOME, the tree laid out, one command, and the exit code, stdout, stderr and
the tree after it recorded. No case calls a model: the judges' votes are
written into the attempts file or the comparison journal. One kind of tree
entry is this file's own: ``receipts``, a journal index holding receipt
rows with a fixed run id, so a card's switch cost reads a ledger.

Times are laid out relative to the run ({NOWF:-691200} in text), so a card
is 8 days old on every run. Every path, id and task is synthetic.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
import gitfix  # noqa: E402
from garden_cases import PY_CLI, body_id, run_case, write_jsonl  # noqa: E402
from pathway_cases import REPO  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "rank"
SCRATCH = Path(
    os.environ.get("DAISUGI_RANK_SCRATCH") or Path.home() / "opendaisugi-scratch" / "rank" / "runs"
)
CASE_VERSION = 1
DAY = 86400
CHOICES = ".opendaisugi/journal/rankings/choices.jsonl"
COMPARISONS = ".opendaisugi/journal/rankings/comparisons.jsonl"
INDEX = ".opendaisugi/journal"

# ---------------------------------------------------------------------------
# A journal index with receipts
# ---------------------------------------------------------------------------

_generic_lay_out = garden_cases.lay_out


def lay_out(tree: dict[str, Any], home: Path, t0: float) -> None:
    """garden_cases.lay_out, and each ``receipts`` entry: a journal index
    made by the oracle's Journal, with the rows inserted as written."""
    rec = {k: v for k, v in tree.items() if "receipts" in v}
    _generic_lay_out({k: v for k, v in tree.items() if k not in rec}, home, t0)
    from opendaisugi.journal import Journal

    for rel, spec in sorted(rec.items()):
        j = Journal(data_dir=(home / rel).parent.parent)
        for r in spec["receipts"]:
            j._con.execute(
                "INSERT INTO receipts (run_id, step_id, timestamp, evidence_hash, verify_result, "
                "verify_details, evidence_json, model_id, effect_class, reversibility, "
                "reversal_json) VALUES (?, ?, ?, ?, 1, '', '{}', NULL, ?, ?, NULL)",
                (
                    r["run"],
                    r["step"],
                    t0 + r["at"],
                    "0" * 64,
                    r.get("kind", "shell"),
                    r["reversibility"],
                ),
            )
        j._con.commit()
        j.close()


garden_cases.lay_out = lay_out


def _git(cwd: Path, *args: str) -> None:
    gitfix.run(cwd, *args)


def _lay_out_git(tree: dict[str, Any], home: Path, t0: float) -> None:
    """Each ``git`` entry: a repository at the entry's path holding ``files``
    in one commit, and a detached worktree beside it for each name in
    ``worktrees``, with that name's files written (uncommitted) on top."""
    for rel, spec in sorted(tree.items()):
        repo = home / rel
        repo.mkdir(parents=True, exist_ok=True)
        _git(repo, "init", "-q", "-b", "main")
        for name, body in spec["git"]["files"].items():
            (repo / name).parent.mkdir(parents=True, exist_ok=True)
            (repo / name).write_text(body, encoding="utf-8")
        _git(repo, "add", "-A")
        _git(repo, "commit", "-qm", "base")
        for wt, files in spec["git"].get("worktrees", {}).items():
            _git(repo, "worktree", "add", "-q", "--detach", str(home / wt), "HEAD")
            for name, body in files.items():
                (home / wt / name).parent.mkdir(parents=True, exist_ok=True)
                (home / wt / name).write_text(body, encoding="utf-8")
            if wt in spec["git"].get("committed", []):
                _git(home / wt, "add", "-A")
                _git(home / wt, "commit", "-qm", "the child committed")


_receipts_lay_out = garden_cases.lay_out


def lay_out_all(tree: dict[str, Any], home: Path, t0: float) -> None:
    git = {k: v for k, v in tree.items() if "git" in v}
    _receipts_lay_out({k: v for k, v in tree.items() if k not in git}, home, t0)
    _lay_out_git(git, home, t0)


garden_cases.lay_out = lay_out_all

_generic_read_tree = garden_cases.read_tree


def read_tree(home: Path) -> dict[str, Any]:
    """garden_cases.read_tree without git's own files: their bytes carry
    object ids and times that differ on every run."""
    out = _generic_read_tree(home)
    return {k: v for k, v in out.items() if ".git" not in Path(k).parts and not k.endswith("/.git")}


garden_cases.read_tree = read_tree


def cmd_for(case: dict[str, Any], binary: str | None) -> list[str]:
    return PY_CLI if binary is None else [binary]


# ---------------------------------------------------------------------------
# Building blocks
# ---------------------------------------------------------------------------


def att(aid: str, **kw: Any) -> dict[str, Any]:
    return {"id": aid, "content_hash": f"sha256:{aid * 4}", **kw}


def cmp2(judge: str, pair: str, a: str, b: str, ab: str, ba: str) -> list[dict[str, Any]]:
    """A judge's two calls on one pair, shown a-first then b-first. ``ab``
    and ``ba`` name the winner by attempt id, or "tie" or "skip"."""

    def out(w: str, first: str) -> str:
        return w if w in ("tie", "skip") else ("a" if w == first else "b")

    rows = []
    for shown, (x, y), w in (("ab", (a, b), ab), ("ba", (b, a), ba)):
        rows.append(
            {
                "a": x,
                "b": y,
                "a_hash": f"sha256:{x * 4}",
                "b_hash": f"sha256:{y * 4}",
                "shown": shown,
                "outcome": out(w, x),
                "judge": judge,
                "pair_id": pair,
            }
        )
    return rows


def doc(
    attempts: list[dict[str, Any]],
    comps: list[dict[str, Any]] | None = None,
    owner: list[list[str]] | None = None,
    rid: str = "r-0001",
    **policy: Any,
) -> dict[str, Any]:
    d: dict[str, Any] = {"ranking_id": rid, "task": "fix the login bug", "project": "/w/app"}
    d["attempts"] = attempts
    if comps is not None:
        d["comparisons"] = comps
    if owner is not None:
        d["owner_constraints"] = owner
    d["policy"] = {"bootstrap": 200, **policy}
    return d


def text(obj: Any) -> dict[str, str]:
    return {"text": json.dumps(obj, indent=1) + "\n"}


def jsonl(rows: list[Any]) -> dict[str, str]:
    return {"text": "".join((r if isinstance(r, str) else json.dumps(r)) + "\n" for r in rows)}


def case(name: str, argv: list[str], before: dict[str, Any] | None = None) -> dict[str, Any]:
    return {"kind": "cli", "name": f"rank {name}", "argv": argv, "before": before or {}}


def fit_case(name: str, d: Any, *, json_out: bool = True, extra=None) -> dict[str, Any]:
    raw = isinstance(d, dict) and ("text" in d or "hex" in d)
    tree = {"a.json": d if raw else text(d)}
    tree.update(extra or {})
    argv = ["rank", "fit", "a.json"] + (["--json"] if json_out else [])
    return case(name, argv, tree)


def opened(
    cid: str,
    *,
    chosen: str = "a",
    survivors: tuple[str, ...] = ("a", "b"),
    age_days: float = 1,
    rid: str = "r-0001",
    project: str = "/w/app",
    facts: dict[str, Any] | None = None,
) -> str:
    """An opened choice row as text, its ts laid out relative to the run."""
    row = {
        "choice_id": cid,
        "ranking_id": rid,
        "project": project,
        "task": "fix the login bug",
        "event": "opened",
        "options": {
            "survivors": [{"id": s, "content_hash": f"sha256:{s * 4}"} for s in survivors],
            "eliminated": [],
        },
        "chosen": chosen,
        "status": "provisional",
        "facts": facts
        if facts is not None
        else {
            "order": list(survivors),
            "decided_by": ["cost"] * (len(survivors) - 1),
            "quality_tiers": [list(survivors)],
            "confidence": 0.5,
            "cost": {s: {} for s in survivors},
            "summary": {},
            "where": {},
            "run": None,
        },
        "ts": "TS",
    }
    return json.dumps(row).replace('"TS"', "{NOWF:%d}" % int(-age_days * DAY))


def closing(cid: str, event: str, how: str, *, pick: str | None = None, rid="r-0001") -> str:
    row: dict[str, Any] = {"choice_id": cid, "ranking_id": rid, "event": event, "how": how}
    if pick is not None:
        row["pick"] = pick
    row["ts"] = "TS"
    return json.dumps(row).replace('"TS"', "{NOWF:-3600}")


def choices(*rows: str) -> dict[str, Any]:
    return {CHOICES: {"text": "".join(r + "\n" for r in rows)}}


# ---------------------------------------------------------------------------
# The cases
# ---------------------------------------------------------------------------

Q = [{"name": "test_extra", "required": False, "result": "pass"}]
REQ_FAIL = [{"name": "test_login", "required": True, "result": "fail"}]


def fit_cases(add: Any) -> None:
    two = [att("a", cost={"tokens": 900}), att("b", cost={"tokens": 100})]
    add(fit_case("fit two no votes", doc(two, judges=["judge-x"]), json_out=False))
    add(fit_case("fit two no votes json", doc(two, judges=["judge-x"])))
    agree = []
    for k in range(4):
        agree += cmp2(f"judge-{k}", f"call-{k}", "a", "b", "a", "a")
    add(fit_case("fit votes ranked", doc(two, agree), json_out=False))
    add(fit_case("fit votes ranked json", doc(two, agree)))
    add(fit_case("fit votes ranked bootstrap 1000", doc(two, agree, bootstrap=1000)))
    add(fit_case("fit single", doc([att("a"), att("b", tests=REQ_FAIL)]), json_out=False))
    add(fit_case("fit single json", doc([att("a"), att("b", tests=REQ_FAIL)])))
    out_all = [
        att(
            "a",
            started_by_parent=True,
            ran_plan=True,
            tests=REQ_FAIL + [{"name": "test_slow", "required": True, "result": "not_run"}],
            features=[{"name": "dark mode", "required": True, "green": False}],
        ),
        att("b", edge_proof="failed", verify={"ok": False, "violations": 3}),
    ]
    add(fit_case("fit none survived", doc(out_all), json_out=False))
    add(fit_case("fit none survived json", doc(out_all)))
    add(
        fit_case(
            "fit kept attempts",
            doc(
                [
                    att(
                        "a",
                        edge_proof="ok",
                        started_by_parent=True,
                        ran_plan=True,
                        verify={"ok": True, "violations": 0},
                    ),
                    att("b", warden={"denies": 4, "override_allows": 2}),
                    att(
                        "c",
                        tests=[{"name": "t", "required": False, "result": "fail"}],
                        features=[{"name": "f", "required": False, "green": False}],
                    ),
                ]
            ),
        )
    )
    tiers = [
        att("a", tests=Q, cost={"tokens": 5}),
        att("b", tests=Q * 2),
        att("c", features=[{"name": "f", "required": False, "green": True}]),
        att("d"),
    ]
    add(
        fit_case(
            "fit quality tiers", doc(tiers, cmp2("j", "p", "c", "d", "d", "d")), json_out=False
        )
    )
    add(fit_case("fit quality tiers json", doc(tiers, cmp2("j", "p", "c", "d", "d", "d"))))
    add(fit_case("fit orders disagree is a tie", doc(two, cmp2("j", "p", "a", "b", "a", "b"))))
    add(fit_case("fit both tie", doc(two, cmp2("j", "p", "a", "b", "tie", "tie"))))
    add(fit_case("fit a skip is no vote", doc(two, cmp2("j", "p", "a", "b", "a", "skip"))))
    stale = cmp2("j", "p", "a", "b", "b", "b")
    stale[1]["a_hash"] = "sha256:old"
    add(fit_case("fit stale hash", doc(two, stale)))
    dup = cmp2("j", "p2", "a", "b", "b", "b") + cmp2("j", "p1", "a", "b", "a", "a")
    add(fit_case("fit one vote per judge", doc(two, dup)))
    half = cmp2("j", "p", "a", "b", "b", "b")[:1]
    add(fit_case("fit half a vote", doc(two, half)))
    three = cmp2("j", "p", "a", "b", "b", "b")
    three.append(dict(three[0]))
    add(fit_case("fit three calls in one pair", doc(two, three)))
    mixed = cmp2("j", "p", "a", "b", "b", "b")
    mixed[1]["a"], mixed[1]["b"] = "a", "zz"
    mixed[1]["a_hash"], mixed[1]["b_hash"] = "sha256:aaaa", "sha256:zz"
    add(fit_case("fit names no attempt", doc(two, mixed)))
    elim_vote = cmp2("j", "p", "a", "b", "b", "b")
    add(
        fit_case(
            "fit vote on an eliminated attempt",
            doc([att("a"), att("b", tests=REQ_FAIL), att("c")], elim_vote),
        )
    )
    many = [
        att(c, cost={"tokens": 10 * i, "wall_ms": 7, "diff_lines": 3}) for i, c in enumerate("abcd")
    ]
    add(fit_case("fit owner inline", doc(many[:2], agree, owner=[["b", "a"]]), json_out=False))
    add(fit_case("fit owner inline json", doc(many[:2], agree, owner=[["b", "a"]])))
    add(fit_case("fit owner cycle", doc(many[:3], owner=[["a", "b"], ["b", "c"], ["c", "a"]])))
    add(
        fit_case(
            "fit owner on eliminated", doc([att("a"), att("b", tests=REQ_FAIL)], owner=[["b", "a"]])
        )
    )
    add(
        fit_case(
            "fit owner across tiers",
            doc([att("a", tests=Q), att("b"), att("c")], owner=[["c", "a"]]),
        )
    )
    add(fit_case("fit owner chain", doc(many, owner=[["d", "b"], ["c", "a"]])))
    discon = cmp2("j", "p1", "a", "b", "a", "a") + cmp2("j", "p2", "c", "d", "c", "c")
    add(fit_case("fit disconnected", doc(many, discon)))
    eight = [att(c, cost={"tokens": 10 * i}) for i, c in enumerate("abcdefgh")]
    big: list[dict[str, Any]] = []
    k = 0
    for x in range(8):
        for y in range(x + 1, 8):
            for j in range(3):
                w = "abcdefgh"[x] if (x + y + j) % 3 else "abcdefgh"[y]
                w2 = w if (x * y + j) % 4 else "abcdefgh"[x]
                big += cmp2(f"judge-{j}", f"call-{k:03d}", "abcdefgh"[x], "abcdefgh"[y], w, w2)
                k += 1
    add(
        fit_case(
            "fit eight attempts", doc(eight, big, judges=["judge-0", "judge-9"], bootstrap=1000)
        )
    )
    strong = []
    for j in range(6):
        strong += cmp2(f"j{j}", f"p{j}", "a", "b", "a", "a")
        strong += cmp2(f"j{j}", f"q{j}", "b", "c", "b", "b")
    add(fit_case("fit preference separates", doc(many[:3], strong, bootstrap=500)))
    add(
        fit_case(
            "fit next pair", doc(many[:3], cmp2("j1", "p", "a", "b", "a", "b"), judges=["j1", "j2"])
        )
    )
    add(
        fit_case(
            "fit judges exhausted", doc(two, cmp2("j1", "p", "a", "b", "a", "b"), judges=["j1"])
        )
    )
    add(fit_case("fit no judges", doc(two)))
    add(
        fit_case(
            "fit cost unknown last",
            doc([att("a"), att("b", cost={"diff_lines": 9}), att("c", cost={"wall_ms": 1})]),
        )
    )
    add(
        fit_case(
            "fit cost estimated",
            doc([att("a", cost={"tokens": 5, "estimated": ["tokens", "tokens"]}), att("b")]),
        )
    )
    add(fit_case("fit tie by name", doc([att("b"), att("a")])))
    add(fit_case("fit threshold half", doc(two, cmp2("j", "p", "a", "b", "a", "a"), threshold=0.5)))
    add(fit_case("fit prior small", doc(two, agree, prior=0.01)))
    add(fit_case("fit prior large", doc(two, agree, prior=100)))
    add(fit_case("fit bootstrap one", doc(two, agree, bootstrap=1)))
    add(fit_case("fit policy defaults", {"text": json.dumps({"ranking_id": "r", "attempts": two})}))
    add(
        fit_case(
            "fit unicode",
            doc([att("a", previews={"summary": "café ☃"}), att("b")], rid="r:unicode"),
        )
    )
    add(fit_case("fit extra keys ignored", doc([{**att("a"), "zzz": [1, {"x": None}]}, att("b")])))
    add(fit_case("fit one attempt", doc([att("a")]), json_out=False))
    # Bad input.
    bad: list[tuple[str, Any]] = [
        ("not json", {"text": "{"}),
        ("a list", {"text": "[]"}),
        ("bad ranking id", {"ranking_id": "r 1", "attempts": [att("a")]}),
        ("no attempts", {"ranking_id": "r", "attempts": []}),
        ("attempts not a list", {"ranking_id": "r", "attempts": {}}),
        ("nine attempts", {"ranking_id": "r", "attempts": [att(c) for c in "abcdefghi"]}),
        ("duplicate id", {"ranking_id": "r", "attempts": [att("a"), att("a")]}),
        ("bad attempt id", {"ranking_id": "r", "attempts": [{"id": "a/b", "content_hash": "x"}]}),
        ("attempt not an object", {"ranking_id": "r", "attempts": [1]}),
        ("no content hash", {"ranking_id": "r", "attempts": [{"id": "a"}]}),
        ("empty content hash", {"ranking_id": "r", "attempts": [{"id": "a", "content_hash": ""}]}),
        ("bad edge proof", {"ranking_id": "r", "attempts": [att("a", edge_proof="maybe")]}),
        ("bad started", {"ranking_id": "r", "attempts": [att("a", started_by_parent=1)]}),
        ("verify no ok", {"ranking_id": "r", "attempts": [att("a", verify={})]}),
        (
            "verify bad count",
            {"ranking_id": "r", "attempts": [att("a", verify={"ok": True, "violations": 1.5})]},
        ),
        ("warden not object", {"ranking_id": "r", "attempts": [att("a", warden=[])]}),
        ("tests not a list", {"ranking_id": "r", "attempts": [att("a", tests={})]}),
        (
            "test bad result",
            {"ranking_id": "r", "attempts": [att("a", tests=[{"name": "t", "result": "ok"}])]},
        ),
        ("test no name", {"ranking_id": "r", "attempts": [att("a", tests=[{"result": "pass"}])]}),
        (
            "test required not bool",
            {
                "ranking_id": "r",
                "attempts": [att("a", tests=[{"name": "t", "required": "yes", "result": "pass"}])],
            },
        ),
        ("feature no green", {"ranking_id": "r", "attempts": [att("a", features=[{"name": "f"}])]}),
        ("cost negative", {"ranking_id": "r", "attempts": [att("a", cost={"tokens": -1})]}),
        ("cost bool", {"ranking_id": "r", "attempts": [att("a", cost={"wall_ms": True})]}),
        (
            "cost too big",
            {"ranking_id": "r", "attempts": [att("a", cost={"diff_lines": 2**53 + 1})]},
        ),
        ("cost float", {"ranking_id": "r", "attempts": [att("a", cost={"tokens": 1.0})]}),
        (
            "cost estimated bad",
            {"ranking_id": "r", "attempts": [att("a", cost={"estimated": ["money"]})]},
        ),
        ("summary not text", {"ranking_id": "r", "attempts": [att("a", previews={"summary": 1})]}),
        (
            "threshold low",
            {"ranking_id": "r", "attempts": [att("a")], "policy": {"threshold": 0.4}},
        ),
        (
            "threshold bool",
            {"ranking_id": "r", "attempts": [att("a")], "policy": {"threshold": True}},
        ),
        ("prior zero", {"ranking_id": "r", "attempts": [att("a")], "policy": {"prior": 0}}),
        (
            "prior nan",
            {
                "text": '{"ranking_id": "r", "attempts": [{"id": "a", "content_hash": "x"}], "policy": {"prior": NaN}}'
            },
        ),
        (
            "prior huge int",
            {
                "text": '{"ranking_id": "r", "attempts": [{"id": "a", "content_hash": "x"}], "policy": {"prior": '
                + "9" * 400
                + "}}"
            },
        ),
        ("bootstrap zero", {"ranking_id": "r", "attempts": [att("a")], "policy": {"bootstrap": 0}}),
        (
            "bootstrap big",
            {"ranking_id": "r", "attempts": [att("a")], "policy": {"bootstrap": 10001}},
        ),
        ("judges not list", {"ranking_id": "r", "attempts": [att("a")], "policy": {"judges": "j"}}),
        ("policy not object", {"ranking_id": "r", "attempts": [att("a")], "policy": 1}),
        (
            "comparison bad shown",
            {
                "ranking_id": "r",
                "attempts": [att("a")],
                "comparisons": [{**cmp2("j", "p", "a", "b", "a", "a")[0], "shown": "x"}],
            },
        ),
        (
            "comparison bad outcome",
            {
                "ranking_id": "r",
                "attempts": [att("a")],
                "comparisons": [{**cmp2("j", "p", "a", "b", "a", "a")[0], "outcome": "c"}],
            },
        ),
        (
            "comparison self",
            {
                "ranking_id": "r",
                "attempts": [att("a")],
                "comparisons": [{**cmp2("j", "p", "a", "b", "a", "a")[0], "b": "a"}],
            },
        ),
        (
            "comparison no judge",
            {
                "ranking_id": "r",
                "attempts": [att("a")],
                "comparisons": [
                    {k: v for k, v in cmp2("j", "p", "a", "b", "a", "a")[0].items() if k != "judge"}
                ],
            },
        ),
        ("comparisons not a list", {"ranking_id": "r", "attempts": [att("a")], "comparisons": {}}),
        (
            "owner not a pair",
            {"ranking_id": "r", "attempts": [att("a")], "owner_constraints": [["a"]]},
        ),
        (
            "owner self",
            {"ranking_id": "r", "attempts": [att("a")], "owner_constraints": [["a", "a"]]},
        ),
        (
            "owner unknown",
            {"ranking_id": "r", "attempts": [att("a")], "owner_constraints": [["a", "z"]]},
        ),
        ("not utf-8", {"hex": "7b22ff227d"}),
        ("deep", {"text": "[" * 800 + "]" * 800}),
    ]
    for label, d in bad:
        add(fit_case(f"fit bad {label}", d, json_out=False))
    # The journal stands in for what the file does not carry.
    no_comps = doc(two)
    jrows = (
        [{"ranking_id": "r-0001", **c} for c in agree]
        + [{"ranking_id": "r-other", **c} for c in cmp2("j", "p", "a", "b", "b", "b")]
        + ["not json", "[1]", {"ranking_id": "r-0001", "shown": "zz"}]
    )
    add(fit_case("fit comparisons from the journal", no_comps, extra={COMPARISONS: jsonl(jrows)}))
    # An empty inline list means the journal supplies the votes, as no key does.
    add(
        fit_case(
            "fit empty comparisons use the journal", doc(two, []), extra={COMPARISONS: jsonl(jrows)}
        )
    )
    add(
        fit_case(
            "fit empty comparisons use the journal text",
            doc(two, []),
            json_out=False,
            extra={COMPARISONS: jsonl(jrows)},
        )
    )
    # A file that holds votes inline keeps them, and the journal stays out.
    other = cmp2("j", "p", "a", "b", "b", "b")
    add(fit_case("fit inline comparisons win", doc(two, other), extra={COMPARISONS: jsonl(jrows)}))
    answers = choices(
        opened("ch_000000000001", chosen="a"),
        closing("ch_000000000001", "overridden", "answer", pick="b"),
        opened("ch_000000000002", chosen="a", rid="r-other"),
        closing("ch_000000000002", "confirmed", "drop", rid="r-other"),
    )
    add(fit_case("fit owner from the journal", doc(two, agree), extra=answers))
    add(fit_case("fit owner from the journal text", doc(two, agree), json_out=False, extra=answers))
    changed = doc([att("a"), {**att("b"), "content_hash": "sha256:new"}], agree)
    add(fit_case("fit owner answer on changed attempts", changed, extra=answers))
    add(fit_case("fit inline owner wins", doc(two, agree, owner=[]), extra=answers))
    add(
        case(
            "fit data dir",
            ["rank", "fit", "a.json", "--json", "--data-dir", "dd"],
            {"a.json": text(doc(two)), "dd/journal/rankings/choices.jsonl": answers[CHOICES]},
        )
    )


def choose_cases(add: Any) -> None:
    two = [
        att("a", cost={"diff_lines": 4}),
        att("b", cost={"diff_lines": 40}, previews={"summary": "a rewrite"}),
    ]
    add(case("choose opens a card", ["rank", "choose", "a.json"], {"a.json": text(doc(two))}))
    add(
        case(
            "choose opens a card json",
            ["rank", "choose", "a.json", "--json"],
            {"a.json": text(doc(two))},
        )
    )
    add(
        case(
            "choose already recorded",
            ["rank", "choose", "a.json"],
            {
                "a.json": text(doc(two)),
                CHOICES: {"text": "PLACEHOLDER"},
            },
        )
    )
    add(
        case(
            "choose single",
            ["rank", "choose", "a.json"],
            {"a.json": text(doc([att("a"), att("b", tests=REQ_FAIL)]))},
        )
    )
    add(
        case(
            "choose none survived",
            ["rank", "choose", "a.json", "--json"],
            {"a.json": text(doc([att("a", tests=REQ_FAIL)]))},
        )
    )
    add(case("choose bad file", ["rank", "choose", "a.json"], {"a.json": {"text": "{"}}))
    add(
        case(
            "choose sweeps a decayed card",
            ["rank", "choose", "a.json", "--json"],
            {
                "a.json": text(doc(two, rid="r-new")),
                **choices(opened("ch_000000000009", age_days=15)),
            },
        )
    )
    add(
        case(
            "choose data dir",
            ["rank", "choose", "a.json", "--data-dir", "dd"],
            {"a.json": text(doc(two))},
        )
    )


def queue_cases(add: Any) -> None:
    add(case("queue empty", ["rank", "queue"]))
    add(case("queue empty json", ["rank", "queue", "--json"]))
    facts = {
        "order": ["a", "b", "c"],
        "decided_by": ["preference", "quality"],
        "quality_tiers": [["a", "b"], ["c"]],
        "confidence": 0.75,
        "cost": {"a": {"diff_lines": 12}, "b": {"diff_lines": 140}, "c": {}},
        "summary": {"a": "the small fix", "b": "a rewrite"},
        "where": {"b": {"kind": "worktree", "path": "{HOME}/wt/b"}},
        "run": None,
    }
    three = choices(
        opened(
            "ch_00000000000a", survivors=("a", "b", "c"), facts=facts, age_days=2, project="/w/zeta"
        ),
        opened("ch_00000000000b", age_days=1, project="/w/alpha", rid="r-0002"),
        opened(
            "ch_00000000000c",
            age_days=3,
            project="/w/alpha",
            rid="r-0003",
            facts={
                **facts,
                "order": ["a", "b"],
                "decided_by": ["cost"],
                "confidence": 0.2,
                "where": {},
                "run": {"run_id": "run_00000001", "step": "t", "downstream": ["w", "x"]},
            },
            survivors=("a", "b"),
        ),
        opened("ch_00000000000d", age_days=1, rid="r-0004"),
        closing("ch_00000000000d", "confirmed", "drop", rid="r-0004"),
    )
    tree = {**three, "wt/b": {"dir": True}}
    receipts = {
        f"{INDEX}/index.db": {
            "receipts": [
                {
                    "run": "run_00000001",
                    "step": "w",
                    "at": -3000,
                    "reversibility": "reversible",
                    "kind": "file_write",
                },
                {
                    "run": "run_00000001",
                    "step": "x",
                    "at": -2000,
                    "reversibility": "none",
                    "kind": "file_read",
                },
                {"run": "run_00000002", "step": "w", "at": -1000, "reversibility": "irreversible"},
            ]
        }
    }
    tree.update(receipts)
    add(case("queue three cards", ["rank", "queue"], tree))
    for by in ("reversibility", "impact", "date", "project"):
        add(case(f"queue sort {by} json", ["rank", "queue", "--sort", by, "--json"], tree))
    add(case("queue bad sort", ["rank", "queue", "--sort", "size"], tree))
    add(
        case(
            "queue decayed by time",
            ["rank", "queue", "--json"],
            choices(opened("ch_000000000001", age_days=15)),
        )
    )
    add(
        case(
            "queue thirteen days old",
            ["rank", "queue"],
            choices(opened("ch_000000000001", age_days=13)),
        )
    )
    gone = {
        "order": ["a", "b"],
        "decided_by": ["cost"],
        "quality_tiers": [["a", "b"]],
        "confidence": 0.5,
        "cost": {},
        "summary": {},
        "where": {"b": {"kind": "worktree", "path": "{HOME}/wt/gone"}},
        "run": None,
    }
    add(
        case(
            "queue alternative gone",
            ["rank", "queue", "--json"],
            choices(opened("ch_000000000001", facts=gone)),
        )
    )
    irr = {**gone, "where": {}, "run": {"run_id": "run_00000002", "step": "t", "downstream": ["w"]}}
    add(
        case(
            "queue irreversible later step",
            ["rank", "queue", "--json"],
            {**choices(opened("ch_000000000001", facts=irr)), **receipts},
        )
    )
    add(
        case(
            "queue bad rows",
            ["rank", "queue", "--json"],
            choices(
                "not json",
                "[]",
                json.dumps({"choice_id": "ch_000000000005", "event": "opened"}),
                json.dumps({"choice_id": "ch_000000000006", "event": "confirmed", "how": "drop"}),
                opened("ch_000000000001"),
                opened("ch_000000000001", chosen="b"),
                closing("ch_000000000001", "confirmed", "decay"),
                closing("ch_000000000001", "overridden", "answer"),
                closing("ch_000000000001", "maybe", "answer"),
                opened("ch_000000000002"),
                json.dumps({"choice_id": 7, "event": "opened"}),
            ),
        )
    )
    odd = json.loads(opened("ch_000000000003").replace("{NOWF:-86400}", "0"))
    odd_rows = [
        opened("ch_000000000001").replace("{NOWF:-86400}", "{NOWF:-86400}.0") + "\r",
        json.dumps({**odd, "ts": 10**400}),
        json.dumps({**odd, "choice_id": "ch_000000000004", "ts": True}),
        json.dumps(
            {
                **odd,
                "choice_id": "ch_000000000005",
                "status": 7,
                "facts": {
                    "order": [1, "b"],
                    "decided_by": ["cost"],
                    "confidence": 1,
                    "cost": {"a": {"diff_lines": 2**60}, "b": {"diff_lines": 3}},
                },
            }
        ).replace('"ts": 0', '"ts": {NOWF:-7200}'),
    ]
    add(
        case(
            "queue odd rows",
            ["rank", "queue", "--json", "--sort", "impact"],
            {CHOICES: {"text": "\r\n".join(odd_rows) + "\n"}},
        )
    )
    add(
        case(
            "queue not utf-8",
            ["rank", "queue"],
            {CHOICES: {"hex": (opened("ch_000000000001") + "\n").encode().hex() + "ff0a"}},
        )
    )
    add(
        case(
            "queue data dir",
            ["rank", "queue", "--data-dir", "dd"],
            {"dd/journal/rankings/choices.jsonl": choices(opened("ch_000000000001"))[CHOICES]},
        )
    )


def record_cases(add: Any) -> None:
    one = choices(opened("ch_000000000001", age_days=1))
    add(case("record drop", ["rank", "record", "drop", "ch_000000000001"], one))
    add(case("record drop json", ["rank", "record", "drop", "ch_000000000001", "--json"], one))
    add(case("record pick chosen", ["rank", "record", "pick", "ch_000000000001", "a"], one))
    add(case("record pick other", ["rank", "record", "pick", "ch_000000000001", "b"], one))
    add(
        case(
            "record pick other json",
            ["rank", "record", "pick", "ch_000000000001", "b", "--json"],
            one,
        )
    )
    add(case("record pick not a survivor", ["rank", "record", "pick", "ch_000000000001", "z"], one))
    add(case("record pick no choice", ["rank", "record", "pick", "ch_00000000000f", "a"], one))
    add(
        case(
            "record pick answered",
            ["rank", "record", "pick", "ch_000000000001", "b"],
            choices(opened("ch_000000000001"), closing("ch_000000000001", "confirmed", "drop")),
        )
    )
    old = choices(opened("ch_000000000001", age_days=15))
    add(case("record pick decayed", ["rank", "record", "pick", "ch_000000000001", "b"], old))
    add(case("record drop decayed", ["rank", "record", "drop", "ch_000000000001"], old))
    facts = {
        "order": ["a", "b"],
        "decided_by": ["cost"],
        "quality_tiers": [["a", "b"]],
        "confidence": 0.5,
        "cost": {},
        "summary": {},
        "where": {},
        "run": {"run_id": "run_00000001", "step": "t", "downstream": ["w"]},
    }
    costly = {
        **choices(opened("ch_000000000001", facts=facts)),
        f"{INDEX}/index.db": {
            "receipts": [
                {"run": "run_00000001", "step": "w", "at": -60, "reversibility": "reversible"}
            ]
        },
    }
    add(case("record pick costly", ["rank", "record", "pick", "ch_000000000001", "b"], costly))
    gone = {**facts, "run": None, "where": {"b": {"kind": "worktree", "path": "{HOME}/gone"}}}
    add(
        case(
            "record pick gone json",
            ["rank", "record", "pick", "ch_000000000001", "b", "--json"],
            choices(opened("ch_000000000001", facts=gone)),
        )
    )
    add(case("record no choice file", ["rank", "record", "drop", "ch_000000000001"]))
    add(
        case(
            "record data dir",
            ["rank", "record", "drop", "ch_000000000001", "--data-dir", "dd"],
            {"dd/journal/rankings/choices.jsonl": one[CHOICES]},
        )
    )


# ---------------------------------------------------------------------------
# show, judge, judges, pivot
# ---------------------------------------------------------------------------

PROFILE = (
    "agent_profiles:\n"
    "  local:\n    wire: openai\n    base_url: http://127.0.0.1:{PORT}/v1\n    model: judge-9b\n"
)
CONFIG = ".opendaisugi/config.yaml"


def out_att(aid: str, author: str, **kw: Any) -> dict[str, Any]:
    return att(aid, author=author, where={"kind": "output", "text": f"answer {aid}"}, **kw)


def judge_doc(
    a_author: str = "claude-x", b_author: str = "claude-y", **policy: Any
) -> dict[str, Any]:
    return doc(
        [out_att("a", a_author), out_att("b", b_author)], judges=["local/judge-9b"], **policy
    )


def judge_case(
    name: str,
    d: dict[str, Any],
    replies: list[dict[str, Any]],
    *,
    extra: dict[str, Any] | None = None,
    flags: tuple[str, ...] = ("--json",),
) -> dict[str, Any]:
    tree = {"a.json": text(d), CONFIG: {"text": PROFILE}}
    tree.update(extra or {})
    c = case(name, ["rank", "judge", "a.json", *flags], tree)
    c["replies"] = replies
    return c


def judge_cases(add: Any) -> None:
    d = judge_doc()
    add(judge_case("judge records a pair", d, [{"chat": "A"}, {"chat": "B"}]))
    add(judge_case("judge records a pair text", d, [{"chat": "tie."}, {"chat": "Tie"}], flags=()))
    add(
        judge_case(
            "judge second call errors", d, [{"chat": "A"}, {"http_status": 500, "body": "boom"}]
        )
    )
    add(judge_case("judge unparsable", d, [{"chat": "A"}, {"chat": "A or B"}]))
    add(judge_case("judge same family", judge_doc("judge-9b"), []))
    claude_judge = PROFILE.replace("judge-9b", "claude-judge-9")
    add(
        judge_case(
            "judge bare alias is claude",
            {
                **judge_doc("haiku", "sonnet"),
                "policy": {"bootstrap": 200, "judges": ["local/claude-judge-9"]},
            },
            [],
            extra={CONFIG: {"text": claude_judge}},
        )
    )
    add(judge_case("judge no author", judge_doc(""), []))
    add(
        judge_case(
            "judge loop exhausts", d, [{"chat": "A"}, {"chat": "B"}], flags=("--loop", "--json")
        )
    )
    add(
        case(
            "judge no judges",
            ["rank", "judge", "a.json", "--json"],
            {"a.json": text(doc([out_att("a", "claude-x"), out_att("b", "claude-y")]))},
        )
    )
    add(
        judge_case(
            "judge named judge",
            doc([out_att("a", "claude-x"), out_att("b", "claude-y")]),
            [{"chat": "B"}, {"chat": "A"}],
            flags=("--judge", "local/judge-9b", "--json"),
        )
    )
    add(
        case(
            "judge profile missing",
            ["rank", "judge", "a.json", "--json"],
            {"a.json": text(d)},
        )
    )
    # Five answered cards against j-bad: it is dropped and not asked.
    rows: list[Any] = []
    cards: list[str] = []
    for n in range(1, 6):
        rid = f"r-000{n}"
        rows += [
            {"ranking_id": rid, **c} for c in cmp2("local/judge-9b", f"p{n}", "a", "b", "a", "a")
        ]
        cid = f"ch_00000000000{n}"
        cards += [
            opened(cid, rid=rid),
            closing(cid, "overridden", "answer", pick="b", rid=rid),
        ]
    add(
        judge_case(
            "judge dropped judge",
            {**d, "ranking_id": "r-9999"},
            [],
            extra={COMPARISONS: jsonl(rows), **choices(*cards)},
        )
    )
    add(case("judges empty", ["rank", "judges", "--json"]))
    add(case("judges empty text", ["rank", "judges"]))
    add(
        case(
            "judges five cards",
            ["rank", "judges", "--json"],
            {COMPARISONS: jsonl(rows), **choices(*cards)},
        )
    )
    add(
        case(
            "judges five cards text",
            ["rank", "judges"],
            {COMPARISONS: jsonl(rows), **choices(*cards)},
        )
    )
    # The weights are a function of the journal and the ranking's seed.
    two = [att("a", cost={"tokens": 100}), att("b", cost={"tokens": 100})]
    jr = [{"ranking_id": "r-0001", **c} for c in cmp2("j-good", "p", "a", "b", "a", "a")]
    for rid in ("r-0001", "r-0002"):
        add(
            case(
                f"judges weights {rid}",
                ["rank", "fit", "a.json", "--json"],
                {
                    "a.json": text(doc(two, rid=rid, judges=["j-good"])),
                    COMPARISONS: jsonl([{**r, "ranking_id": rid} for r in jr]),
                },
            )
        )
    add(
        case(
            "judges weights replay",
            ["rank", "choose", "a.json", "--json"],
            {
                "a.json": text(doc(two, rid="r-0001", judges=["j-good"])),
                COMPARISONS: jsonl(jr),
            },
        )
    )


def show_cases(add: Any) -> None:
    rows = [{"ranking_id": "r-0001", **c} for c in cmp2("j1", "p1", "a", "b", "a", "a")]
    both = {
        **choices(
            opened("ch_000000000001"),
            closing("ch_000000000001", "overridden", "answer", pick="b"),
        ),
        COMPARISONS: jsonl(rows + ["not json"]),
    }
    add(case("show by ranking", ["rank", "show", "--ranking", "r-0001", "--json"], both))
    add(case("show by choice", ["rank", "show", "--choice", "ch_000000000001", "--json"], both))
    add(case("show by ranking text", ["rank", "show", "--ranking", "r-0001"], both))
    add(case("show two selectors", ["rank", "show", "--ranking", "r-0001", "--choice", "c"], both))
    add(case("show no selector", ["rank", "show"], both))
    add(case("show missing ranking", ["rank", "show", "--ranking", "nope"], both))
    add(case("show missing choice", ["rank", "show", "--choice", "ch_00000000000f"], both))
    add(case("show nothing laid out", ["rank", "show", "--ranking", "r-0001"]))


GIT = {
    "files": {"a.txt": "one\n", "b.txt": "base\n"},
    "worktrees": {
        "wt-a": {"a.txt": "two\n"},
        "wt-b": {"a.txt": "three\n", "new.txt": "fresh\n"},
    },
}


BASE = gitfix.commit_id(GIT["files"])


def wt(name: str) -> dict[str, Any]:
    """A worktree attempt as a branch card records it: the path, the base
    commit and the repository, all written by the parent."""
    return {"kind": "worktree", "path": "{HOME}/" + name, "base": BASE, "repo": "{HOME}/repo/.git"}


def pivot_facts(**kw: Any) -> dict[str, Any]:
    f = {
        "order": ["a", "b"],
        "decided_by": ["cost"],
        "quality_tiers": [["a", "b"]],
        "confidence": 0.5,
        "cost": {},
        "summary": {},
        "where": {
            "a": wt("wt-a"),
            "b": wt("wt-b"),
        },
        "run": None,
    }
    f.update(kw)
    return f


def pivot_cases(add: Any) -> None:
    tree = {"repo": {"git": GIT}, **choices(opened("ch_000000000001", facts=pivot_facts()))}
    base = ["rank", "pivot", "--choice", "ch_000000000001"]
    # Children that committed in their worktrees: HEAD moved off the base,
    # and the pivot still reads both edits against the recorded base.
    moved = {
        "repo": {"git": {**GIT, "committed": ["wt-a", "wt-b"]}},
        **choices(opened("ch_000000000001", facts=pivot_facts())),
    }
    add(case("pivot apply patch after commits", [*base, "--to", "b", "--apply", "--json"], moved))
    add(case("pivot plan worktrees", [*base, "--to", "b", "--json"], tree))
    add(case("pivot plan worktrees text", [*base, "--to", "b"], tree))
    add(case("pivot apply patch", [*base, "--to", "b", "--apply"], tree))
    add(case("pivot apply patch json", [*base, "--to", "b", "--apply", "--json"], tree))
    add(case("pick apply", ["rank", "record", "pick", "ch_000000000001", "b", "--apply"], tree))
    add(
        case(
            "pick apply json",
            ["rank", "record", "pick", "ch_000000000001", "b", "--apply", "--json"],
            tree,
        )
    )
    add(case("pivot to the chosen", [*base, "--to", "a"], tree))
    add(case("pivot to a stranger", [*base, "--to", "z"], tree))
    add(
        case("pivot no choice", ["rank", "pivot", "--choice", "ch_00000000000f", "--to", "b"], tree)
    )
    add(case("pivot missing option", ["rank", "pivot", "--to", "b"], tree))
    run = {"run_id": "run_00000001", "step": "t", "downstream": ["w", "x", "y"]}
    receipts = {
        f"{INDEX}/index.db": {
            "receipts": [
                {"run": "run_00000001", "step": "w", "at": -90, "reversibility": "reversible"},
                {"run": "run_00000001", "step": "x", "at": -60, "reversibility": "reversible"},
                {"run": "run_00000001", "step": "y", "at": -30, "reversibility": "none"},
            ]
        }
    }
    costly = {
        "repo": {"git": GIT},
        **choices(opened("ch_000000000001", facts=pivot_facts(run=run))),
        **receipts,
    }
    add(case("pivot plan costly", [*base, "--to", "b", "--json"], costly))
    add(case("pivot plan costly text", [*base, "--to", "b"], costly))
    hard = {
        **costly,
        f"{INDEX}/index.db": {
            "receipts": [
                {"run": "run_00000001", "step": "w", "at": -90, "reversibility": "reversible"},
                {"run": "run_00000001", "step": "x", "at": -60, "reversibility": "irreversible"},
            ]
        },
    }
    add(case("pivot follow up only", [*base, "--to", "b", "--apply"], hard))
    add(case("pivot follow up only json", [*base, "--to", "b", "--apply", "--json"], hard))
    gone = {
        "repo": {"git": GIT},
        **choices(
            opened(
                "ch_000000000001",
                facts=pivot_facts(
                    where={
                        "a": wt("wt-a"),
                        "b": wt("wt-gone"),
                    }
                ),
            )
        ),
    }
    add(case("pivot place gone", [*base, "--to", "b", "--apply"], gone))
    out = pivot_facts(
        where={
            "a": wt("wt-a"),
            "b": {"kind": "output", "text": "the other answer"},
        }
    )
    add(
        case(
            "pivot output kind",
            [*base, "--to", "b", "--apply", "--json"],
            {"repo": {"git": GIT}, **choices(opened("ch_000000000001", facts=out))},
        )
    )
    nobase = pivot_facts(
        where={
            "a": {"kind": "worktree", "path": "{HOME}/wt-a"},
            "b": {"kind": "worktree", "path": "{HOME}/wt-b"},
        }
    )
    add(
        case(
            "pivot worktrees with no recorded base",
            [*base, "--to", "b", "--json"],
            {"repo": {"git": GIT}, **choices(opened("ch_000000000001", facts=nobase))},
        )
    )
    nowhere = pivot_facts(where={})
    add(
        case(
            "pivot nowhere to patch",
            [*base, "--to", "b"],
            choices(opened("ch_000000000001", facts=nowhere)),
        )
    )
    # A branch card names the command that takes the kept edits.
    adopt = pivot_facts(adopt="daisugi tree adopt tabc")
    add(
        case(
            "queue adopt line",
            ["rank", "queue"],
            choices(opened("ch_000000000001", facts=adopt)),
        )
    )
    add(
        case(
            "queue adopt line json",
            ["rank", "queue", "--json"],
            choices(opened("ch_000000000001", facts=adopt)),
        )
    )


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    fit_cases(C.append)
    choose_cases(C.append)
    queue_cases(C.append)
    record_cases(C.append)
    show_cases(C.append)
    judge_cases(C.append)
    pivot_cases(C.append)
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = build_cases()
    # "choose already recorded" holds the row choose itself writes.
    for c in cases:
        if c["name"] == "rank choose already recorded":
            from opendaisugi import rank

            r = rank.parse(json.loads(c["before"]["a.json"]["text"]))
            res = rank.fit(r)
            row = rank.opened_row(r, res, now=0.0)
            c["before"][CHOICES] = {
                "text": json.dumps(row).replace('"ts": 0.0', '"ts": {NOWF:-60}') + "\n"
            }
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    return cases


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument("--fresh", action="store_true", help="rerun every case")
    args = ap.parse_args()
    out_dir: Path = args.out
    out_dir.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out_dir / "cases.jsonl").exists() and not args.fresh:
        for ln in (out_dir / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        if prev is not None and not args.only:
            c["expect"] = prev["expect"]
            continue
        if "replies" in c:
            garden_cases.record_replies(c, SCRATCH / "gen" / f"{i:04d}r")
        c["expect"] = run_case(c, cmd_for(c, None), SCRATCH / "gen" / f"{i:04d}")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:8000]
            )
        else:
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out_dir / "cases.jsonl", cases)
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
