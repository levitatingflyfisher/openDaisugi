"""Run the pathway cases through a client's binaries (Go or Rust) and
compare them to the oracle.

    uv run --no-sync python clients/pathway_compare.py \\
        --binary clients/go/daisugi --probe PATH [--oracle] [--fuzz N --seed S] [--speed]

--probe is the pathway-probe binary, the test instrument that runs find
on many queries in one process: for Go, build it with
`go build -o PATH ./cmd/pathway-probe` in clients/go; for Rust, it is
clients/rust/target/release/pathway-probe, beside
clients/rust/target/release/daisugi.

It checks, from clients/fixtures/pathways (clients/pathway_cases.py writes
them from the oracle):

- every CLI case: exit code, stdout, the tree after (each database dumped
  as schema and rows) and, when Python exited 0, stderr. A case marked
  go_refuses, or a command the binary names as not in it yet, must exit 2
  with one line and leave the tree as it was (not-ported). A refusal to
  read an input ("Nothing was changed.") with the tree unchanged is
  refused. Anything else is a disagreement.
- interop: on every tree the binary wrote a database into, Python's
  `pathways list --json` must print what the binary's prints.
- every find case: the matched id, the score within 1e-6 (lexical) or
  1e-5 (potion), and the stale warning. go_only cases expect the
  binary's refusal.
- with the real potion model in the Hugging Face cache: the real
  tokenizer's ids on thousands of texts, and the real-model find cases.

--oracle also reruns the Python side, so a stale fixture shows up.
--fuzz N runs N seeded random queries against random stores, for the
lexical backend, the tiny potion model and (when cached) the real one.
--speed times `daisugi pathways list` and a find on a 1,000-pathway
lexical store, a cold process each time.
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import random
import shutil
import statistics
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from pathway_cases import (  # noqa: E402 - sibling module, run as a script
    _FIND_ORACLE,
    FIXTURE_DIR,
    PY_CLI,
    REAL_POTION,
    SCRATCH,
    TINY_WORDS,
    base_env,
    hf_cache,
    lay_out,
    lay_out_db,
    lexical,
    pathway,
    potion_vec,
    put_row,
    random_texts,
    read_tree,
    real_potion_dir,
    real_token_texts,
    row_spec,
    run_cli_case,
    run_find_oracle,
)
from fixture_paths import leaks  # noqa: E402 - sibling module, run as a script

NOT_YET = "is not in this binary yet."
TIES: list[str] = []


def diff(a: Any, b: Any, path: str = "", out: list[str] | None = None) -> list[str]:
    out = [] if out is None else out
    if len(out) >= 12:
        return out
    if isinstance(a, dict) and isinstance(b, dict):
        for k in list(a) + [k for k in b if k not in a]:
            if k not in a or k not in b:
                out.append(f"{path}/{k}: {'missing' if k not in a else repr(a[k])[:120]} vs "
                           f"{'missing' if k not in b else repr(b[k])[:120]}")
            else:
                diff(a[k], b[k], f"{path}/{k}", out)
    elif isinstance(a, list) and isinstance(b, list) and len(a) == len(b):
        for i, (x, y) in enumerate(zip(a, b, strict=True)):
            diff(x, y, f"{path}[{i}]", out)
    elif a != b:
        out.append(f"{path}: {a!r:.400} vs {b!r:.400}")
    return out


def before_tree(case: dict[str, Any], work: Path) -> dict[str, Any]:
    if work.exists():
        shutil.rmtree(work)
    lay_out(case.get("before") or {}, work / "home")
    tree = read_tree(work / "home")
    shutil.rmtree(work)
    return tree


def compare_cli(case: dict[str, Any], got: dict[str, Any], before: dict[str, Any]) -> tuple[str, list[str]]:
    expect = case["expect"]
    err = "\n".join(got["stderr"])
    if case.get("go_refuses") or (got["exit"] == 2 and NOT_YET in err):
        ok = got["exit"] == 2 and NOT_YET in err and len(got["stderr"]) == 1 and got["tree"] == before
        return ("not-ported", []) if ok else ("disagree", ["not refused cleanly"] + diff(before, got["tree"]))
    if got["exit"] == 2 and expect["exit"] != 2 and "Nothing was changed." in err:
        return ("refused", [err]) if got["tree"] == before else ("disagree", ["refused but changed the tree"])
    problems = []
    if got["exit"] != expect["exit"]:
        problems.append(f"exit {expect['exit']} vs {got['exit']}")
    if got["stdout"] != expect["stdout"]:
        problems += ["stdout: " + d for d in diff(expect["stdout"], got["stdout"])]
    problems += stderr_problems(expect, got)
    if got["tree"] != expect["tree"]:
        problems += ["tree: " + d for d in diff(expect["tree"], got["tree"])]
    return ("agree", []) if not problems else ("disagree", problems)


STDERR_CLASSES: collections.Counter[str] = collections.Counter()


def stderr_problems(expect: dict[str, Any], got: dict[str, Any]) -> list[str]:
    """stderr agrees when it is the same text. Two Python forms are read
    for what they say: a traceback (the fixture keeps its exception's
    type, which the binary's line must name) and click's usage box (the
    binary must say the boxed message; the usage line names the program)."""
    want, have = expect["stderr"], got["stderr"]
    text = "\n".join(have)
    if "Traceback (most recent call last): ..." in want:
        STDERR_CLASSES["traceback: exception type named"] += 1
        exc = want[-1]
        return [] if exc and exc in text else [f"stderr: {exc!r} not named in {have!r}"]
    boxed = [ln.strip("│ ").strip() for ln in want if ln.startswith("│")]
    if boxed:
        STDERR_CLASSES["usage error: boxed message"] += 1
        missing = [b for b in boxed if b not in text]
        return [f"stderr: usage message {m!r} not in {have!r}" for m in missing]
    STDERR_CLASSES["exact"] += 1
    return [] if want == have else ["stderr: " + d for d in diff(want, have)]


def interop(binary: str, work: Path, wrote: bool) -> list[str]:
    """Python reads the database the binary left as the binary reads it."""
    home = work / "home"
    dbs = [p for p in home.rglob("*.db")] if home.exists() else []
    out = []
    for db in dbs:
        env = base_env(home)
        args = ["pathways", "list", "--json", "--data-dir", str(db.parent)]
        py = subprocess.run(PY_CLI + args, env=env, cwd=home, capture_output=True, timeout=300, check=False)
        go = subprocess.run([binary] + args, env=env, cwd=home, capture_output=True, timeout=60, check=False)
        if py.returncode == 0 and (py.returncode, py.stdout) != (go.returncode, go.stdout):
            out.append(f"interop {db.name}: python {py.returncode} {py.stdout[:200]!r} vs "
                       f"binary {go.returncode} {go.stdout[:200]!r}")
        if go.returncode == 0 and py.returncode != 0:
            out.append(f"interop {db.name}: the binary lists a store Python cannot: {py.stderr[-300:]!r}")
        # Every row the binary wrote must leave Python's store usable: a
        # find reads every compatible row.
        if not wrote:
            continue
        fhome = work / "findhome"
        (fhome / ".opendaisugi").mkdir(parents=True, exist_ok=True)
        (fhome / ".opendaisugi" / "config.yaml").write_text("matcher_model: lexical\n")
        fenv = base_env(fhome)
        f = subprocess.run([sys.executable, "-c", _FIND_AFTER, str(db)], env=fenv, capture_output=True,
                           timeout=300, check=False)
        if f.returncode != 0:
            out.append(f"interop {db.name}: Python's find fails on the binary's store: {f.stderr[-300:]!r}")
    return out


_FIND_AFTER = """
import sys
from opendaisugi.pathway_store import PathwayStore
s = PathwayStore(sys.argv[1])
s.find("build the release")
s.list_all()
"""


def run_cli(args, binary: str) -> tuple[collections.Counter, int]:
    cases = [json.loads(ln) for ln in (FIXTURE_DIR / "cases.jsonl").read_text().splitlines() if ln.strip()]
    classes: collections.Counter[str] = collections.Counter()
    stale = 0
    for i, case in enumerate(cases):
        if args.only and args.only not in case["name"]:
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        before = before_tree(case, work)
        got = run_cli_case(case, [binary], work)
        cls, problems = compare_cli(case, got, before)
        if cls == "agree":
            problems = interop(binary, work, case["argv"][:2] == ["pathways", "import"])
            if problems:
                cls = "disagree"
        if args.oracle:
            live = run_cli_case(case, PY_CLI, SCRATCH / "cmp" / f"{i:04d}py")
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
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    return classes, stale


# ---------------------------------------------------------------------------
# find
# ---------------------------------------------------------------------------


def xdg_with_real_model(root: Path) -> Path:
    """An XDG_CACHE_HOME holding the pinned potion files, copied from the
    Hugging Face cache, so the binary loads them without a download."""
    src = real_potion_dir()
    dest = root / "xdg" / "opendaisugi" / "models" / "potion-base-8M"
    dest.mkdir(parents=True, exist_ok=True)
    for f in ("config.json", "tokenizer.json", "model.safetensors"):
        if not (dest / f).exists():
            shutil.copyfile(src / f, dest / f)
    return root / "xdg"


def probe_find(probe: str, case: dict[str, Any], home: Path, queries: list[dict[str, Any]]) -> list[dict[str, Any]]:
    env = base_env(home)
    env.update({k: v.replace("{HOME}", str(home)) for k, v in (case.get("env") or {}).items()})
    env["PATHWAY_PROBE_NO_FETCH"] = "1"
    if case.get("real_potion"):
        env["XDG_CACHE_HOME"] = str(xdg_with_real_model(SCRATCH))
    qs = "".join(json.dumps({k: v for k, v in q.items() if k != "expect"}) + "\n" for q in queries)
    proc = subprocess.run([probe, "find", str(home / "store.db")], input=qs.encode(), env=env,
                          capture_output=True, timeout=600, check=False)
    if proc.returncode != 0:
        raise SystemExit(f"probe failed on {case['name']}: {proc.stderr.decode()[-2000:]}")
    return [json.loads(ln) for ln in proc.stdout.decode().splitlines()]


def lay_out_find(case: dict[str, Any], work: Path) -> Path:
    if work.exists():
        shutil.rmtree(work)
    home = work / "home"
    lay_out({".opendaisugi/config.yaml": {"text": f"matcher_model: {case['matcher']}\n"}}, home)
    lay_out_db(home / "store.db", {"rows": case["rows"]}, str(home))
    shutil.copytree(FIXTURE_DIR / "potion-tiny", home / "potion-tiny")
    return home


def tol(case: dict[str, Any]) -> float:
    return 1e-5 if case["matcher"] == "potion" else 1e-6


def compare_find(case, queries, got) -> list[str]:
    bad = []
    for q, g in zip(queries, got, strict=True):
        e = q["expect"]
        if e.get("refused"):
            if not g.get("refused"):
                bad.append(f"{q['task']!r}: expected a refusal, got {g}")
            continue
        if g.get("error") or g.get("refused"):
            bad.append(f"{q['task']!r}: binary {g}")
            continue
        # An exact tie: which row wins is BLAS rounding, which differs
        # between CPUs, so any of the tied rows agrees.
        tie = e["id"] != g.get("id") and g.get("id") in e.get("tie_ids", [])
        if tie:
            TIES.append(q["task"])
        if e["id"] != g.get("id") and not tie:
            bad.append(f"{q['task']!r}: id {e['id']} vs {g.get('id')} (scores {e['score']} vs {g.get('score')})")
        elif e["id"] is not None and abs(e["score"] - g["score"]) > tol(case):
            bad.append(f"{q['task']!r}: score {e['score']} vs {g['score']}")
        if e.get("warning", "") != g.get("warning", ""):
            bad.append(f"{q['task']!r}: warning {e.get('warning')!r} vs {g.get('warning')!r}")
    return bad


def run_find(args, probe: str) -> tuple[int, int, int]:
    cases = [json.loads(ln) for ln in (FIXTURE_DIR / "find.jsonl").read_text().splitlines() if ln.strip()]
    have_real = real_potion_dir() is not None
    n_q = n_bad = skipped = 0
    for i, case in enumerate(cases):
        if case.get("real_potion") and not have_real:
            skipped += 1
            continue
        home = lay_out_find(case, SCRATCH / "find" / f"{i:04d}")
        got = probe_find(probe, case, home, case["queries"])
        bad = compare_find(case, case["queries"], got)
        if args.oracle and not case.get("go_only"):
            live = run_find_oracle(case, SCRATCH / "find" / f"{i:04d}py")
            if live != [q["expect"] for q in case["queries"]]:
                print(f"STALE find fixture: {case['name']}")
                bad.append("stale")
        n_q += len(case["queries"])
        n_bad += len(bad)
        if bad:
            print(f"FIND DISAGREE: {case['name']}")
            for b in bad[:10]:
                print(f"    {b}")
    shutil.rmtree(SCRATCH / "find", ignore_errors=True)
    return n_q, n_bad, skipped


def run_tokens(probe: str) -> tuple[int, int]:
    real = real_potion_dir()
    if real is None:
        return 0, 0
    from tokenizers import Tokenizer

    texts = real_token_texts()
    tok = Tokenizer.from_file(str(real / "tokenizer.json"))
    want = [e.ids for e in tok.encode_batch_fast(texts, add_special_tokens=False)]
    proc = subprocess.run([probe, "tokens", str(real / "tokenizer.json")],
                          input="".join(json.dumps(t) + "\n" for t in texts).encode(),
                          capture_output=True, timeout=600, check=True)
    got = [json.loads(ln) for ln in proc.stdout.decode().splitlines()]
    bad = [(t, w, g) for t, w, g in zip(texts, want, got, strict=True) if w != g]
    for t, w, g in bad[:10]:
        print(f"TOKENS: {t!r}: {w} vs {g}")
    return len(texts), len(bad)


def run_fuzz(args, probe: str) -> int:
    rng = random.Random(args.seed)
    total_bad = 0
    tiny = FIXTURE_DIR / "potion-tiny"
    real = real_potion_dir()
    backends = [("lexical", None), ("tiny", tiny)] + ([("real", real)] if real else [])
    for name, model in backends:
        n_rows = 150
        texts = random_texts(n_rows // 2, rng.randrange(1 << 30)) + [
            " ".join(rng.choice(TINY_WORDS) for _ in range(rng.randint(1, 8))) for _ in range(n_rows // 2)]
        if name == "lexical":
            rows = [row_spec(put_row(pathway(i, t, lexical(t)))) for i, t in enumerate(texts)]
            case = {"name": f"fuzz {name}", "matcher": "lexical"}
        elif name == "tiny":
            rows = [row_spec(put_row(pathway(i, t, potion_vec(model, t), model="{HOME}/potion-tiny")))
                    for i, t in enumerate(texts)]
            case = {"name": f"fuzz {name}", "matcher": "potion", "env": {"OPENDAISUGI_POTION_MODEL": "{HOME}/potion-tiny"}}
        else:
            rows = [row_spec(put_row(pathway(i, t, potion_vec(model, t), model=REAL_POTION)))
                    for i, t in enumerate(texts)]
            case = {"name": f"fuzz {name}", "matcher": "potion", "real_potion": True}
        case["rows"] = rows
        # Half the queries are near a stored task, so matches happen.
        qtexts = random_texts(args.fuzz // 2, rng.randrange(1 << 30))
        for _ in range(args.fuzz - len(qtexts)):
            words = rng.choice(texts).split()
            rng.shuffle(words)
            qtexts.append(" ".join(words[: rng.randint(1, max(1, len(words)))]))
        queries = [{"task": q} for q in qtexts]
        case["queries"] = queries
        t0 = time.perf_counter()
        expect = run_find_oracle(case, SCRATCH / "fuzz" / name)
        for q, e in zip(queries, expect, strict=True):
            q["expect"] = e
        home = lay_out_find(case, SCRATCH / "fuzz" / (name + "go"))
        got = probe_find(probe, case, home, queries)
        bad = compare_find(case, queries, got)
        matched = sum(1 for e in expect if e["id"])
        print(f"fuzz {name}: {len(queries)} queries, {matched} matched, {len(bad)} disagreements "
              f"({time.perf_counter() - t0:.0f}s)")
        for b in bad[:10]:
            print(f"    {b}")
        total_bad += len(bad)
    shutil.rmtree(SCRATCH / "fuzz", ignore_errors=True)
    return total_bad


def run_speed(binary: str, probe: str) -> None:
    work = SCRATCH / "speed"
    home = work / "home"
    db = home / ".opendaisugi" / "pathways.db"
    if not db.exists():
        if work.exists():
            shutil.rmtree(work)
        lay_out({".opendaisugi/config.yaml": {"text": "matcher_model: lexical\n"}}, home)
        from opendaisugi.pathway_store import PathwayStore

        rng = random.Random(1)
        store = PathwayStore(db)
        for i in range(1000):
            t = " ".join(rng.choice(TINY_WORDS) for _ in range(rng.randint(3, 8))) + f" item{i}"
            store.put(pathway(i, t, lexical(t)))
    env = base_env(home)

    def timed(cmd, stdin=None, n=31):
        ts = []
        for _ in range(n):
            t0 = time.perf_counter()
            p = subprocess.run(cmd, env=env, cwd=home, input=stdin, capture_output=True, check=False)
            ts.append((time.perf_counter() - t0) * 1000)
            if p.returncode != 0:
                raise SystemExit(f"{cmd}: exit {p.returncode}: {p.stderr[:300]!r}")
        ts.sort()
        return statistics.median(ts), ts[int(len(ts) * 0.9)]

    q = b'{"task": "build test deploy item5"}\n'
    for label, cmd, stdin in [
        ("daisugi pathways list", [binary, "pathways", "list"], None),
        ("daisugi pathways list --json", [binary, "pathways", "list", "--json"], None),
        ("find (pathway-probe, one query)", [probe, "find", str(db)], q),
        ("python pathways list", PY_CLI + ["pathways", "list"], None),
    ]:
        p50, p90 = timed(cmd, stdin, n=11 if cmd[0] == sys.executable else 31)
        print(f"speed: {label}: p50 {p50:.1f} ms, p90 {p90:.1f} ms (1,000 lexical pathways)")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--probe", required=True)
    ap.add_argument("--oracle", action="store_true", help="also rerun the Python side")
    ap.add_argument("--only", help="run only CLI cases whose name holds this text")
    ap.add_argument("--fuzz", type=int, default=0)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--speed", action="store_true")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()
    binary = str(Path(args.binary).resolve())
    probe = str(Path(args.probe).resolve())
    SCRATCH.mkdir(parents=True, exist_ok=True)
    failed = False
    classes, stale = run_cli(args, binary)
    print(f"\nCLI: {sum(classes.values())} cases: " + ", ".join(f"{k}={v}" for k, v in sorted(classes.items())))
    print("CLI stderr compared: " + ", ".join(f"{k}={v}" for k, v in sorted(STDERR_CLASSES.items())))
    failed |= bool(classes.get("disagree") or stale)
    if not args.only:
        n_q, n_bad, skipped = run_find(args, probe)
        print(f"find: {n_q} queries, {n_bad} disagreements, {skipped} real-potion cases skipped (model not cached)")
        n_t, t_bad = run_tokens(probe)
        print(f"real tokenizer ids: {n_t} texts, {t_bad} disagreements")
        failed |= bool(n_bad or t_bad)
    if args.fuzz:
        failed |= bool(run_fuzz(args, probe))
    if args.speed:
        run_speed(binary, probe)
    print(f"exact ties decided the other way (accepted, PW-4): {len(TIES)}")
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    print(f"machine paths in clients/fixtures: {len(leaked)}")
    return 1 if failed or leaked else 0


if __name__ == "__main__":
    raise SystemExit(main())
