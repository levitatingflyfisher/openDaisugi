"""Run every gate case through another gate binary and compare it to the oracle.

    uv run --no-sync python clients/gate_compare.py \\
        --binary clients/go/daisugi-gate [--oracle] [--json out.json]

Cases come from clients/fixtures/gate/cases.jsonl (clients/gate_cases.py
writes them from the Python gate). Each case runs in a fresh scratch gate
root; the binary's exit code, stdout, stderr, shadow log, session tree,
captures mirror and coppice report are normalized the same way the oracle's
were and compared field by field, never as raw text.

A disagreement is classed by what it would do to a host:

- fail-open: the binary allowed a call the oracle denied. A blocker.
- stricter: the binary denied a call the oracle allowed. Safe, but a bug.
- record: same host verdict, but a logged field differs (reason, tier,
  shadow record, tree entry, report).

With --oracle the Python gate is run live as well, so a stale fixture shows
up as an oracle-vs-fixture difference, and both gates' timings are
reported side by side. The binary's DAISUGI_GATE_TRACE lines say which
calls it answered itself (native), which it could not decide and so
denied (unported), and which ended abnormally and were denied (crash).
Only native counts toward the native share.
"""

from __future__ import annotations

import argparse
import collections
import json
import re
import statistics
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from gate_cases import (  # noqa: E402 - sibling module, run as a script
    FIXTURE_DIR,
    REPO,
    SCRATCH,
    host_verdict,
    python_gate_cmd,
    run_case,
    summary,
)
from gate_fuzz import (  # noqa: E402 - sibling module, run as a script
    VARIANTS,
    fuzz_envelopes,
    fuzz_heredocs,
    fuzz_multiline,
    fuzz_unicode,
    over_variants,
)

FIELDS = ("exit", "stdout", "stderr", "shadow", "tree", "captures", "coppice")

GENERATORS = {
    "shell": None,
    "heredoc": fuzz_heredocs,
    "multiline": fuzz_multiline,
    "unicode": fuzz_unicode,
    "envelope": fuzz_envelopes,
}


def _parsed(text: str) -> Any:
    try:
        return json.loads(text) if text.strip() else text
    except json.JSONDecodeError:
        return text


def diff(a: Any, b: Any, path: str = "", out: list[str] | None = None) -> list[str]:
    """Paths where two JSON values differ (at most 12)."""
    out = [] if out is None else out
    if len(out) >= 12:
        return out
    if isinstance(a, dict) and isinstance(b, dict):
        for k in list(a) + [k for k in b if k not in a]:
            if k not in a or k not in b:
                out.append(f"{path}.{k}: {'missing' if k not in a else repr(a[k])[:120]} vs "
                           f"{'missing' if k not in b else repr(b[k])[:120]}")
            else:
                diff(a[k], b[k], f"{path}.{k}", out)
    elif isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            out.append(f"{path}: {len(a)} items vs {len(b)}")
        for i, (x, y) in enumerate(zip(a, b)):
            diff(x, y, f"{path}[{i}]", out)
    elif a != b:
        out.append(f"{path}: {repr(a)[:160]} vs {repr(b)[:160]}")
    return out


def compare(expect: dict[str, Any], got: dict[str, Any]) -> dict[str, Any] | None:
    """None when the two observations agree; else the class and the diffs."""
    lines: list[str] = []
    for f in FIELDS:
        e, g = expect.get(f), got.get(f)
        if f in ("stdout",):
            e, g = _parsed(e or ""), _parsed(g or "")
        diff(e, g, f, lines)
    if not lines:
        return None
    ev, gv = host_verdict(expect), host_verdict(got)
    if ev == "deny" and gv == "allow":
        kind = "fail-open"
    elif ev == "allow" and gv == "deny":
        kind = "stricter"
    else:
        kind = "record"
    return {"kind": kind, "diffs": lines}


def pct(xs: list[float], q: float) -> float:
    if not xs:
        return float("nan")
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))]


def timing(xs: list[float]) -> str:
    if not xs:
        return "n=0"
    return f"n={len(xs)} p50={pct(xs, 0.5) * 1000:.1f}ms p90={pct(xs, 0.9) * 1000:.1f}ms mean={statistics.mean(xs) * 1000:.1f}ms"


# ---------------------------------------------------------------------------
# Seeded differential fuzz over single-line compound shell commands
# ---------------------------------------------------------------------------

FUZZ_ATOMS = [
    "ls", "echo", "cat", "git", "a", "x.txt", "-la", "'<'", '"|"', "\\<", "\\>", "\\;", "\\&", "\\|",
    "\\(", "\\)", "[", "]", "[[", "]]", "@(a|b)", "!(x)", "+(y)", "*(z)", "?(w)", "@($(rm x))", "$x",
    "${x}", "${x:-a}", "$(ls)", "$(rm x)", "`ls`", "`rm x`", "<(ls)", ">(ls)", "{", "}", "(", ")", "((",
    "))", "=", "-f", "--", "#", "x#y", "'a b'", '"a b"', "$'\\t'", "a=b", "2>&1", ">/work/o", "</work/i",
    ">>/work/o", ">/etc/o", "&>", "|&", "!", "time", "case", "in", "esac", "if", "then", "fi", "do", "done",
    "for", "while", ";;", "\\", "~", "*", "*.py", "$((1+2))", "\\\"", "\\'", "a\\ b", "{a,b}", "$@", "$1",
    "$#", "%", "^", "-", "\\$(ls)", "'$(ls)'", "rm", "-rf", "/work/x", "coppice", "agent", "allow",
]
# Atoms mostly inside the gate's vetted shell grammar, so that half the
# commands exercise the paths the binary answers itself: the hard-deny
# rules, redirect scopes, interpreters and the tier.
FUZZ_GRAMMAR_ATOMS = [
    "ls", "echo", "cat", "git", "a", "wc", "rm", "x.txt", "-la", "'a b'", '"a b"', '"a|b"', "'x;y'", "*.py",
    "a=b", "FOO=1", "2>&1", "2>/dev/null", ">/work/o", ">>/work/o", "</work/i", ">/etc/o", "</etc/passwd",
    "-", "--", "~/x", "/work/x", "~", "status", "log", "@x", "a+b", "%", "coppice", "agent", "allow", "deny",
    "web", "token", "gate/asks/1", ".opencode/plugin/x", "opencode.json", "~/.config/coppice/x", "sh", "-c",
    "'ls; rm x'", "xargs", "timeout", "5", "env", "find", ".", "-exec", "python3", "if", "time", "done",
    "/home/user/.ssh/id_rsa", "cd", "/tmp", "..", "push", "--force", "curl", "https://x.test", "-o", "f",
    "let", "unset", "nameref", "declare", "export", "local", "readonly", "typeset", "eval", "exec", "source",
    "test", "alias", "builtin", "command", "shift", "return", "set", "read", "printf", "pushd", "popd", "trap",
]
FUZZ_SEPS = [" ; ", " && ", " || ", " | ", " & ", " "]


def fuzz_commands(n: int, seed: int) -> list[str]:
    """n distinct single-line commands, each holding a shell operator,
    built from a fixed seed so a run can be repeated exactly."""
    import random

    rng = random.Random(seed)
    out: set[str] = set()
    safe = "abcxyzABZ019_./:=,+@%^-~*?"
    printable = [chr(c) for c in range(0x20, 0x7F)]

    def word() -> str:
        # A random word of the gate's vetted grammar: bare safe
        # characters, '...' and "..." segments.
        segs = []
        for _ in range(rng.randint(1, 3)):
            kind = rng.random()
            if kind < 0.6:
                segs.append("".join(rng.choice(safe) for _ in range(rng.randint(1, 6))))
            elif kind < 0.8:
                segs.append("'" + "".join(rng.choice([c for c in printable if c != "'"])
                                          for _ in range(rng.randint(0, 5))) + "'")
            else:
                segs.append('"' + "".join(rng.choice([c for c in printable if c not in '$`\\"'])
                                          for _ in range(rng.randint(0, 5))) + '"')
        return "".join(segs)

    def grammar_command() -> str:
        simples = []
        for _ in range(rng.randint(1, 4)):
            parts = [f"{rng.choice(['A', 'FOO', '_x'])}={word()}" for _ in range(rng.choice([0, 0, 1, 2]))]
            parts.append(rng.choice(["ls", "echo", "cat", "git", "a", word()]))
            parts += [word() for _ in range(rng.randint(0, 3))]
            for _ in range(rng.choice([0, 0, 1, 2])):
                parts.append(rng.choice([">", ">>", "<", "2>", "1>"]) + rng.choice(["", " "]) + word())
            if rng.random() < 0.2:
                parts.append("2>&1")
            simples.append(" ".join(parts))
        out_cmd = simples[0]
        for s in simples[1:]:
            out_cmd += rng.choice([" && ", " || ", " ; ", " | ", ";", "|"]) + s
        return out_cmd

    while len(out) < n:
        if rng.random() < 0.34:
            out.add(grammar_command())
            continue
        parts = []
        k = rng.randint(2, 7)
        atoms = FUZZ_ATOMS if rng.random() < 0.5 else FUZZ_GRAMMAR_ATOMS
        for i in range(k):
            parts.append(rng.choice(atoms))
            if i < k - 1:
                parts.append(rng.choice([" ", " ", " "] + FUZZ_SEPS))
        c = "".join(parts)
        if any(ch in c for ch in ";|&`<>") or "$(" in c:
            out.add(c)
    return sorted(out)


_ORACLE_HELPER = r"""
import json, sys
from pathlib import Path
from opendaisugi.gate import run_argv
for line in sys.stdin:
    req = json.loads(line)
    out = run_argv(req["argv"], req["stdin"].encode())
    f = Path(req["shadow"])
    last = f.read_text(encoding="utf-8").splitlines()[-1] if f.exists() else ""
    print(json.dumps({"exit": out.exit_code, "stdout": out.stdout, "stderr": out.stderr, "shadow": last}),
          flush=True)
"""

_MASKS = [
    (re.compile(r'"(at|elapsed_ms)": [0-9.e+-]+'), r'"\1": N'),
    (re.compile(r"plan_[0-9a-f]{8}"), "plan_X"),
]


def _mask(line: str) -> str:
    for rx, rep in _MASKS:
        line = rx.sub(rep, line)
    return line


def _as_written(text: str, errors: str) -> bytes:
    """What main() writes for one stream of an outcome: print()'s newline,
    and UTF-8 with the stream's error handler (stderr escapes a lone
    surrogate)."""
    if not text:
        return b""
    try:
        return (text + "\n").encode("utf-8", errors)
    except UnicodeEncodeError:
        return b"<unwritable>"


def run_fuzz(binary: Path, items: list[dict[str, Any]], seed: int, kind: str) -> dict[str, Any]:
    """Every fuzz item through the in-process oracle and the binary. Each
    item names its envelope, registered as the default on both sides just
    before the call. Every call is compared, whatever path the binary took;
    a call it did not answer natively also counts as not native."""
    import shutil
    import subprocess

    from gate_cases import base_env

    work = SCRATCH / "fuzz"
    shutil.rmtree(work, ignore_errors=True)
    roots = {}
    for side in ("py", "go"):
        r = work / side / "gate"
        (r / "envelopes").mkdir(parents=True)
        roots[side] = r
    env = base_env()
    helper = subprocess.Popen(  # noqa: S603 - the oracle
        [sys.executable, "-c", _ORACLE_HELPER], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        env=env, text=True, cwd=work,
    )
    # Warm the oracle first. Its first verified call imports z3 and
    # networkx inside the verifier's time budget, and on a loaded box that
    # can run past --verify-timeout and deny: a timing artifact that reads
    # as a fail-open of the binary.
    warm = work / "warm" / "gate"
    (warm / "envelopes").mkdir(parents=True)
    (warm / "envelopes" / "default.json").write_text(json.dumps(VARIANTS["permissive"]), encoding="utf-8")
    helper.stdin.write(json.dumps({
        "argv": ["--mode", "enforce", "--format", "claude", "--verify-timeout", "60", "--root", str(warm)],
        "stdin": json.dumps({"session_id": "warm", "tool_name": "Bash", "cwd": "/work",
                             "tool_input": {"command": "ls && ls"}}),
        "shadow": str(warm / "shadow" / "warm.jsonl")}) + "\n")
    helper.stdin.flush()
    helper.stdout.readline()
    trace = work / "trace.jsonl"
    genv = dict(env, DAISUGI_GATE_TRACE=str(trace))
    stats: collections.Counter = collections.Counter()
    found: list[dict[str, Any]] = []
    for item in items:
        stdin = item["stdin"]
        sid = json.loads(stdin)["session_id"]
        text = item["envelope"] if isinstance(item["envelope"], str) else json.dumps(item["envelope"], indent=2)
        for side in ("py", "go"):
            (roots[side] / "envelopes" / "default.json").write_text(text, encoding="utf-8")
        stats["calls"] += 1
        argv = ["--mode", "enforce", "--format", "claude", "--verify-timeout", "5.0", "--root"]
        groot = roots["go"]
        if trace.exists():
            trace.unlink()
        g = subprocess.run(  # noqa: S603 - the binary under test
            [str(binary.resolve()), *argv, str(groot)], input=stdin.encode(), capture_output=True,
            env=genv, cwd=work, check=False,
        )
        rows = trace.read_text().splitlines() if trace.exists() else []
        path = json.loads(rows[-1])["path"] if rows else "no trace"
        stats["native" if path == "native" else f"not native ({path})"] += 1
        proot = roots["py"]
        helper.stdin.write(json.dumps({"argv": [*argv, str(proot)], "stdin": stdin,
                                       "shadow": str(proot / "shadow" / f"{sid}.jsonl")}) + "\n")
        helper.stdin.flush()
        py = json.loads(helper.stdout.readline())
        gf = groot / "shadow" / f"{sid}.jsonl"
        go_shadow = gf.read_text(encoding="utf-8").splitlines()[-1] if gf.exists() else ""
        same = (
            py["exit"] == g.returncode
            and _as_written(py["stdout"], "strict") == g.stdout
            and _as_written(py["stderr"], "backslashreplace") == g.stderr
            and _mask(py["shadow"]).replace(str(proot), "R") == _mask(go_shadow).replace(str(groot), "R")
        )
        if same:
            stats["same"] += 1
            continue
        if py["exit"] == 2 and g.returncode == 0:
            verdict = "fail-open"
        elif py["exit"] == 0 and g.returncode == 2:
            verdict = "stricter"
        else:
            verdict = "record"
        stats[verdict] += 1
        found.append({"kind": verdict, "command": item["show"], "envelope": item["label"], "path": path,
                      "stdin": stdin, "envelope_text": text, "oracle": py["shadow"], "binary": go_shadow,
                      "oracle_stderr": py["stderr"],
                      "binary_stderr": g.stderr.decode("utf-8", "backslashreplace")})
        why = ""
        try:
            why = json.loads(py["shadow"]).get("reason", "")[:160]
        except (json.JSONDecodeError, AttributeError):
            pass
        print(f"FUZZ [{verdict}] envelope={item['label']} {item['show']!r} | oracle: {why}", flush=True)
    helper.stdin.close()
    helper.wait(timeout=30)
    return {"kind": kind, "items": len(items), "seed": seed, "stats": dict(stats), "found": found}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", type=Path, default=REPO / "clients" / "go" / "daisugi-gate")
    ap.add_argument("--cases", type=Path, default=FIXTURE_DIR / "cases.jsonl")
    ap.add_argument("--oracle", action="store_true", help="also run the Python gate live")
    ap.add_argument("--filter", default="", help="only cases whose name contains this")
    ap.add_argument("--json", type=Path, help="write the full report here")
    ap.add_argument("--fuzz", type=int, default=0, metavar="N",
                    help="instead of the cases, run N seeded fuzz items; fail on any disagreement")
    ap.add_argument("--kind", choices=sorted(GENERATORS), default="shell",
                    help="the fuzz generator (gate_fuzz.py); shell runs each command under three envelopes")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--commands", type=Path, help="run the shell fuzz over this file's lines instead")
    args = ap.parse_args()

    if args.fuzz or args.commands:
        listed = None
        if args.commands:
            listed = [x for x in args.commands.read_text(encoding="utf-8").splitlines() if x.strip()]
        if listed is not None:
            items = over_variants(listed)
        elif args.kind == "shell":
            items = over_variants(fuzz_commands(args.fuzz, args.seed))
        else:
            items = GENERATORS[args.kind](args.fuzz, args.seed)
        rep = run_fuzz(args.binary, items, args.seed, args.kind)
        print(f"fuzz {args.kind}: {rep['items']} calls, seed {rep['seed']}: {rep['stats']}")
        if args.json:
            args.json.write_text(json.dumps(rep, indent=1) + "\n", encoding="utf-8")
        bad = sum(rep["stats"].get(k, 0) for k in ("fail-open", "stricter", "record"))
        return 1 if bad else 0

    cases = [json.loads(x) for x in args.cases.read_text(encoding="utf-8").splitlines() if x.strip()]
    cases = [c for c in cases if args.filter in c["name"]]
    work = SCRATCH / "compare"
    work.mkdir(parents=True, exist_ok=True)
    trace = work / "trace.jsonl"
    py_env: dict[str, str] = {}

    report: dict[str, Any] = {"cases": len(cases), "disagreements": [], "stale_fixture": []}
    go_t: dict[str, list[float]] = collections.defaultdict(list)
    py_t: list[float] = []
    native_by_tag: dict[str, collections.Counter] = collections.defaultdict(collections.Counter)
    why_counts: collections.Counter = collections.Counter()
    for i, c in enumerate(cases):
        if trace.exists():
            trace.unlink()
        env = dict(py_env, DAISUGI_GATE_TRACE=str(trace))
        got, secs = run_case(c, [str(args.binary.resolve())], work / "go", env)
        rows = [json.loads(x) for x in trace.read_text().splitlines()] if trace.exists() else []
        path = rows[-1]["path"] if rows else "?"
        why = rows[-1].get("why", "") if rows else "no trace line"
        go_t[path].append(secs)
        for tag in c["tags"] or ["untagged"]:
            native_by_tag[tag][path] += 1
        if path != "native":
            why_counts[why.split(":")[0]] += 1
        d = compare(c["expect"], got)
        if d:
            d.update({"id": c["id"], "name": c["name"], "path": path, "why": why,
                      "oracle": c["expect"]["summary"], "binary": summary(got)})
            report["disagreements"].append(d)
            print(f"DISAGREE [{d['kind']}] {c['name']} ({path})", flush=True)
            for line in d["diffs"][:6]:
                print(f"    {line}")
        if args.oracle:
            live, psecs = run_case(c, python_gate_cmd(), work / "py")
            py_t.append(psecs)
            stale = compare(c["expect"], live)
            if stale:
                report["stale_fixture"].append({"name": c["name"], "diffs": stale["diffs"]})
                print(f"STALE FIXTURE {c['name']}: {stale['diffs'][:3]}", flush=True)

    kinds = collections.Counter(d["kind"] for d in report["disagreements"])
    total_native = sum(len(go_t[k]) for k in go_t if k == "native")
    print()
    print(f"cases: {len(cases)}   native: {total_native}   not native: {len(cases) - total_native}")
    print(f"disagreements: {len(report['disagreements'])}  {dict(kinds)}")
    if args.oracle:
        print(f"stale fixture cases: {len(report['stale_fixture'])}")
    print("native share by tag:")
    for tag in sorted(native_by_tag):
        cnt = native_by_tag[tag]
        n = sum(cnt.values())
        print(f"    {tag:14s} {cnt['native']:3d}/{n:<3d} native")
    print("why not native (denied):")
    for why, n in why_counts.most_common():
        print(f"    {n:3d}  {why}")
    print("speed (wall time per call, cold process):")
    print(f"    go native     {timing(go_t['native'])}")
    print(f"    go not native {timing([x for k, v in go_t.items() if k != 'native' for x in v])}")
    print(f"    go all        {timing([x for v in go_t.values() for x in v])}")
    if args.oracle:
        print(f"    python        {timing(py_t)}")
    report["speed_ms"] = {
        k: {"p50": pct(v, 0.5) * 1000, "p90": pct(v, 0.9) * 1000, "n": len(v)} for k, v in go_t.items()
    }
    if py_t:
        report["speed_ms"]["python"] = {"p50": pct(py_t, 0.5) * 1000, "p90": pct(py_t, 0.9) * 1000, "n": len(py_t)}
    report["native_by_tag"] = {k: dict(v) for k, v in native_by_tag.items()}
    report["why"] = dict(why_counts)
    if args.json:
        args.json.write_text(json.dumps(report, indent=1) + "\n", encoding="utf-8")
    return 1 if report["disagreements"] or report["stale_fixture"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
