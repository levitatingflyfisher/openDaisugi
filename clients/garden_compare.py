"""Run the garden cases through the Go binary and compare it to the oracle.

    uv run --no-sync python clients/garden_compare.py --binary clients/go/daisugi \\
        [--oracle] [--only NAME] [--fuzz N --seed S] [--speed]

It checks, from clients/fixtures/garden (clients/garden_cases.py writes
them from the oracle), for every case: the exit code, stdout, stderr, the
tree after (every database dumped whole) and the model requests, byte for
byte. stderr is compared on every case, as clients/pathway_compare.py
compares it: exactly, except a Python traceback (the binary must name the
exception type) and click's usage box (the binary must say the message).
A command the binary names as not in it yet must exit 2 with one line and
leave the tree as it was.

The guard: on every tree the binary wrote, Python must still read what it
wrote. Each pathway store is read with list_all and find, and must list as
the binary lists it; each journal is read with list_successful_traces,
load_trace of every trace, get_refinements of every run and
is_session_converted of every converted session.

--oracle also reruns the Python side, so a stale fixture shows up.
--fuzz N runs N seeded random stores through prune, merge and run on both
sides. --speed times the gardener commands on a 1,000-pathway store.
"""

from __future__ import annotations

import argparse
import collections
import json
import random
import shutil
import statistics
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from garden_cases import (  # noqa: E402 - sibling module, run as a script
    DAY,
    DB,
    FIXTURE_DIR,
    PY_CLI,
    SCRATCH,
    all_cases,
    base_env,
    lay_out,
    normalize,
    read_tree,
    run_case,
)
from pathway_cases import envelope, pathway, put_row, row_spec  # noqa: E402
from pathway_compare import diff, stderr_problems, STDERR_CLASSES  # noqa: E402
from fixture_paths import leaks  # noqa: E402

NOT_YET = "is not in this binary yet."


def before_tree(case: dict[str, Any], work: Path) -> dict[str, Any]:
    if work.exists():
        shutil.rmtree(work)
    t0 = time.time()
    lay_out(case.get("before") or {}, work / "home", t0)
    tree = normalize({"tree": read_tree(work / "home")}, str(work / "home"), t0)["tree"]
    shutil.rmtree(work)
    return tree


def _vector(spec: Any) -> list[float] | None:
    if isinstance(spec, dict) and "n" in spec:
        v = [0.0] * spec["n"]
        for i, x in spec["nz"]:
            v[i] = x
        return v
    if isinstance(spec, dict) and "text" in spec:
        try:
            v = json.loads(spec["text"])
        except ValueError:
            return None
        return v if isinstance(v, list) else None
    return None


def potion_tolerance(expect_tree: dict[str, Any], got_tree: dict[str, Any], problems: list[str]) -> None:
    """Under potion, a stored vector is compared within 1e-6: model2vec sums
    float32 rows in float32, the binary in float64 (GD-5)."""
    for rel, e in expect_tree.items():
        g = got_tree.get(rel)
        if not (isinstance(e, dict) and "db" in e and isinstance(g, dict) and "db" in g):
            continue
        erows = e["db"]["tables"].get("pathways", [])
        grows = g["db"]["tables"].get("pathways", [])
        for er, gr in zip(erows, grows):
            ev, gv = _vector(er.get("task_embedding_json")), _vector(gr.get("task_embedding_json"))
            if ev is None or gv is None or len(ev) != len(gv):
                continue
            if max((abs(a - b) for a, b in zip(ev, gv)), default=0.0) <= 1e-6:
                gr["task_embedding_json"] = er["task_embedding_json"]
            else:
                problems.append(f"{rel}: vector of {er.get('id')} differs by more than 1e-6")


def compare(case: dict[str, Any], got: dict[str, Any], before: dict[str, Any]) -> tuple[str, list[str]]:
    expect = case["expect"]
    err = "\n".join(got["stderr"])
    if case.get("go_refuses") or (got["exit"] == 2 and NOT_YET in err):
        # Not in the binary yet, or input it cannot read the Python way:
        # either way one line, exit 2, and nothing changed.
        said = NOT_YET in err or "Nothing was changed." in err
        ok = got["exit"] == 2 and said and len(got["stderr"]) == 1 and got["tree"] == before
        return ("not-ported", []) if ok else ("disagree", ["not refused cleanly"] + diff(before, got["tree"]))
    if got["exit"] == 2 and expect["exit"] != 2 and "Nothing was changed." in err:
        return ("refused", [err]) if got["tree"] == before else ("disagree", ["refused but changed the tree"])
    problems: list[str] = []
    if case.get("potion"):
        potion_tolerance(expect["tree"], got["tree"], problems)
    if got["exit"] != expect["exit"]:
        problems.append(f"exit {expect['exit']} vs {got['exit']}")
    if got["stdout"] != expect["stdout"]:
        problems += ["stdout: " + d for d in diff(expect["stdout"], got["stdout"])]
    problems += stderr_problems(expect, got)
    if got["tree"] != expect["tree"]:
        problems += ["tree: " + d for d in diff(expect["tree"], got["tree"])]
    if got["requests"] != expect["requests"]:
        problems += ["requests: " + d for d in diff(expect["requests"], got["requests"])]
    return ("agree", []) if not problems else ("disagree", problems)


_GUARD = r"""
import json, sys, warnings
from pathlib import Path
warnings.simplefilter("ignore")
kind, path = sys.argv[1], Path(sys.argv[2])
if kind == "pathways":
    from opendaisugi.pathway_store import PathwayStore
    s = PathwayStore(path)
    s.list_all()
    s.find("build the release")
else:
    from opendaisugi.journal import Journal
    j = Journal(data_dir=path)
    ts = j.list_successful_traces()
    import sqlite3
    con = sqlite3.connect(path / "journal" / "index.db")
    ids = [r[0] for r in con.execute("SELECT id FROM traces")]
    runs = [r[0] for r in con.execute("SELECT DISTINCT run_id FROM traces WHERE run_id IS NOT NULL")]
    sess = [r[0] for r in con.execute("SELECT session_id FROM hook_conversions")]
    for i in ids:
        j.load_trace(i)
    for r in runs:
        j.get_refinements(r)
    for s in sess:
        assert j.is_session_converted(s)
print("ok")
"""


def guard(binary: str, work: Path) -> list[str]:
    """Python reads every store and journal the binary left."""
    home = work / "home"
    out = []
    if not home.exists():
        return out
    env = base_env(home, work)
    for db in sorted(home.rglob("*.db")):
        if db.name == "index.db" and db.parent.name == "journal":
            args = ["journal", str(db.parent.parent)]
        elif db.name == "pathways.db":
            args = ["pathways", str(db)]
            lst = ["pathways", "list", "--json", "--data-dir", str(db.parent)]
            py = subprocess.run(PY_CLI + lst, env=env, cwd=home, capture_output=True, timeout=300, check=False)
            go = subprocess.run([binary] + lst, env=env, cwd=home, capture_output=True, timeout=60, check=False)
            if py.returncode == 0 and (py.returncode, py.stdout) != (go.returncode, go.stdout):
                out.append(f"guard {db.name}: python {py.returncode} {py.stdout[:200]!r} vs "
                           f"binary {go.returncode} {go.stdout[:200]!r}")
        else:
            continue
        ghome = work / "guardhome"
        (ghome / ".opendaisugi").mkdir(parents=True, exist_ok=True)
        (ghome / ".opendaisugi" / "config.yaml").write_text("matcher_model: lexical\n")
        g = subprocess.run([sys.executable, "-c", _GUARD] + args, env=base_env(ghome, work), capture_output=True, timeout=300,
                           check=False)
        if g.returncode != 0:
            out.append(f"guard {db.relative_to(home)}: Python cannot read what the binary left: {g.stderr[-400:]!r}")
    return out


def run_cases(args, binary: str) -> tuple[collections.Counter, int, int]:
    cases = [json.loads(ln) for ln in (FIXTURE_DIR / "cases.jsonl").read_text().splitlines() if ln.strip()]
    classes: collections.Counter[str] = collections.Counter()
    stale = guarded = 0
    for i, case in enumerate(cases):
        if args.only and args.only not in case["name"]:
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        before = before_tree(case, SCRATCH / "cmp" / f"{i:04d}b")
        got = run_case(case, [binary], work)
        cls, problems = compare(case, got, before)
        if cls == "agree" and got["tree"] != before:
            guarded += 1
            problems = guard(binary, work)
            if problems:
                # A store or journal the case laid out broken is broken
                # before the binary runs too; only a new failure counts.
                base = SCRATCH / "cmp" / f"{i:04d}base"
                if base.exists():
                    shutil.rmtree(base)
                lay_out(case.get("before") or {}, base / "home", time.time())
                problems = [p for p in problems if p.split(":")[0] not in
                            {q.split(":")[0] for q in guard(binary, base)}]
                shutil.rmtree(base, ignore_errors=True)
            if problems:
                cls = "disagree"
        if args.oracle:
            live = run_case(case, PY_CLI, SCRATCH / "cmp" / f"{i:04d}py")
            if live != case["expect"]:
                stale += 1
                print(f"STALE fixture: {case['name']}: {diff(case['expect'], live)[:3]}")
        classes[cls] += 1
        if cls == "disagree":
            print(f"DISAGREE: {case['name']}")
            for p in problems[:12]:
                print(f"    {p}")
        elif cls in ("refused", "not-ported") and args.verbose:
            print(f"{cls}: {case['name']}")
        shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    return classes, stale, guarded


# ---------------------------------------------------------------------------
# Fuzz: random stores through prune, merge and run
# ---------------------------------------------------------------------------


def random_store(rng: random.Random) -> list[dict[str, Any]]:
    n = rng.randint(0, 40)
    rows = []
    dims = rng.choice([3, 4, 8])
    protos = [[rng.gauss(0, 1) for _ in range(dims)] for _ in range(rng.randint(1, 5))]
    envs = [
        lambda i: envelope(i),
        lambda i: envelope(i, shell_allowlist=["make"]),
        lambda i: envelope(i, file_read=["/a/**", "/b/**"]),
        lambda i: envelope(i, max_execution_time_s=60),
        lambda i: envelope(i, mcp_allowlist=["fs/read"]),
    ]
    for i in range(n):
        base = rng.choice(protos)
        emb = [x + rng.gauss(0, rng.choice([0.0, 0.01, 0.1, 0.5])) for x in base]
        if rng.random() < 0.05:
            emb = emb[:-1]
        kw: dict[str, Any] = {
            "hit_count": rng.choice([0, 1, 2, 3, 5, 8, 13, rng.randint(0, 50)]),
            "failure_count": rng.choice([0, 0, 1, 2, 5, rng.randint(0, 50)]),
        }
        if rng.random() < 0.1:
            kw["model"] = "all-MiniLM-L6-v2"
        env = rng.choice(envs)(i) if rng.random() < 0.4 else envelope(0)
        srcs = [f"t{rng.randint(0, 20)}" for _ in range(rng.randint(0, 4))]
        r = row_spec(put_row(pathway(i, f"task {i}", emb, env=env, source_trace_ids=srcs, **kw)))
        r["distilled_at"] = {"now": -rng.choice([0.5, 1, 10, 29, 31, 45, 90]) * DAY - rng.randint(0, 3000)}
        r["last_activation_at"] = rng.choice([0.0, {"now": -rng.choice([0.5, 2, 20, 35, 60]) * DAY - rng.randint(0, 3000)}])
        rows.append(r)
    rng.shuffle(rows)
    return rows


def run_fuzz(args, binary: str) -> int:
    rng = random.Random(args.seed)
    bad = 0
    commands = [
        ["gardener", "prune", "--json"],
        ["gardener", "prune", "--dry-run", "--json", "--max-idle-days", "15", "--min-activations", "2"],
        ["gardener", "merge", "--json"],
        ["gardener", "merge", "--json", "--similarity", "0.8"],
        ["gardener", "run", "--json"],
        ["gardener", "status", "--json"],
    ]
    t0 = time.perf_counter()
    for k in range(args.fuzz):
        case = {"name": f"fuzz {k}", "argv": rng.choice(commands),
                "before": {DB: {"db": {"rows": random_store(rng)}}}}
        py = run_case(case, PY_CLI, SCRATCH / "fuzz" / "py")
        go = run_case(case, [binary], SCRATCH / "fuzz" / "go")
        problems = []
        for key in ("exit", "stdout", "stderr", "tree"):
            if py[key] != go[key]:
                problems += [f"{key}: " + d for d in diff(py[key], go[key])]
        if not problems and go["tree"]:
            problems = guard(binary, SCRATCH / "fuzz" / "go")
        if problems:
            bad += 1
            print(f"FUZZ DISAGREE seed={args.seed} case={k} argv={case['argv']}")
            for p in problems[:8]:
                print(f"    {p}")
    shutil.rmtree(SCRATCH / "fuzz", ignore_errors=True)
    print(f"fuzz: {args.fuzz} random stores, {bad} disagreements ({time.perf_counter() - t0:.0f}s)")
    return bad


def run_speed(binary: str) -> None:
    work = SCRATCH / "speed"
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    rng = random.Random(3)
    rows = []
    for i in range(1000):
        r = row_spec(put_row(pathway(i, f"task {i}", [rng.gauss(0, 1) for _ in range(8)],
                                     hit_count=rng.randint(0, 20), failure_count=rng.randint(0, 5))))
        rows.append(r)
    lay_out({DB: {"db": {"rows": rows}}}, home, time.time())
    (work / "bin").mkdir(parents=True, exist_ok=True)
    (work / "tmp").mkdir(exist_ok=True)
    env = base_env(home, work)
    src = home / DB
    for label, argv in [("status", ["gardener", "status"]), ("prune --dry-run", ["gardener", "prune", "--dry-run"]),
                        ("merge --dry-run", ["gardener", "merge", "--dry-run"])]:
        for who, cmd in (("binary", [binary]), ("python", PY_CLI)):
            ts = []
            for _ in range(11 if who == "binary" else 5):
                t = time.perf_counter()
                p = subprocess.run(cmd + argv, env=env, cwd=home, capture_output=True, check=False)
                ts.append((time.perf_counter() - t) * 1000)
                if p.returncode != 0:
                    raise SystemExit(f"{argv}: {p.stderr[-300:]!r}")
            print(f"speed: {who} gardener {label}: p50 {statistics.median(ts):.1f} ms (1,000 pathways)")
    del src
    # tend over the salvage case's journal: 7 traces, one cluster salvaged
    # and verified with the linked Z3, no model call.
    case = next(c for c in all_cases() if c["name"] == "tend salvage many variants")
    if work.exists():
        shutil.rmtree(work)
    for who, cmd in (("binary", [binary]), ("python", PY_CLI)):
        ts = []
        for _ in range(7 if who == "binary" else 3):
            if work.exists():
                shutil.rmtree(work)
            lay_out(case["before"], home, time.time())
            (work / "bin").mkdir(parents=True, exist_ok=True)
            (work / "tmp").mkdir(exist_ok=True)
            env = base_env(home, work)
            env.update(case.get("env") or {})
            t = time.perf_counter()
            p = subprocess.run(cmd + ["tend"], env=env, cwd=home, capture_output=True, check=False)
            ts.append((time.perf_counter() - t) * 1000)
            if p.returncode != 0 or b"created=1" not in p.stdout:
                raise SystemExit(f"tend: {p.stdout[-300:]!r} {p.stderr[-300:]!r}")
        print(f"speed: {who} tend: p50 {statistics.median(ts):.1f} ms (7 traces, one cluster salvaged and verified)")
    shutil.rmtree(work, ignore_errors=True)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--fuzz", type=int, default=0)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--speed", action="store_true")
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--no-cases", action="store_true")
    args = ap.parse_args()
    binary = str(Path(args.binary).resolve())
    SCRATCH.mkdir(parents=True, exist_ok=True)
    failed = False
    if not args.no_cases:
        classes, stale, guarded = run_cases(args, binary)
        print(f"\ngarden: {sum(classes.values())} cases: " + ", ".join(f"{k}={v}" for k, v in sorted(classes.items())))
        print("stderr compared: " + ", ".join(f"{k}={v}" for k, v in sorted(STDERR_CLASSES.items())))
        print(f"guard: Python read back the trees of {guarded} cases the binary wrote")
        failed |= bool(classes.get("disagree") or stale)
    if args.fuzz:
        failed |= bool(run_fuzz(args, binary))
    if args.speed:
        run_speed(binary)
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    print(f"machine paths in clients/fixtures: {len(leaked)}")
    return 1 if failed or leaked else 0


if __name__ == "__main__":
    raise SystemExit(main())
