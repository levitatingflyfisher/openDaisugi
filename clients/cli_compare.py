"""Run every CLI case through the Go `daisugi` binary and compare it to the
oracle.

    uv run --no-sync python clients/cli_compare.py \\
        --binary clients/go/daisugi [--oracle] [--json out.json] [--only NAME] [--skip A,B]

Cases come from clients/fixtures/cli/cases.jsonl (clients/cli_cases.py
writes them from the Python CLI). Each case runs in a fresh scratch HOME;
the binary's exit code, stdout, stderr and the tree it leaves are
normalized the way the oracle's were and compared structurally: file set,
modes, JSON files parsed, gate hook commands compared on their flags.

Each case lands in one class:

- agree: same exit code, output and tree.
- refused: the binary said it cannot yet read this input, exited 2 and
  left the tree as it was. Not a disagreement, but not an answer either.
- not-ported: a case marked go_refuses, where the binary printed its one
  line and exited 2 without changing anything, as it must.
- disagree: anything else. A disagreement where the binary's tree or
  status reports a stronger gate than Python's is flagged fail-open.

Output compared: stdout exactly, except --version, whose number comes
from the binary's build (only one version line is asked for). In an
install's text a runtime the binary will fail on reads as its name; the
oracle's skill link reads as the copy a port writes (IN-1). stderr is compared only when Python exited 0, since a Python
error is a traceback. Warnings compare as "warning: <text>"; the
binary's own note that it wrote through a symlink is dropped.

With --oracle the Python CLI is also run live, so a stale fixture shows
up. Every install case the binary wrote is also checked for interop:
Python's `gate status --json` on the binary's tree must equal the
binary's own.
"""

from __future__ import annotations

import argparse
import collections
import json
import re
import shlex
import shutil
import statistics
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from cli_cases import (  # noqa: E402 - sibling module, run as a script
    FIXTURE_DIR,
    SCRATCH,
    as_daisugi,
    go_expect_for,
    help_for_port,
    lay_out,
    python_cmd,
    read_tree,
    rich_before,
    rich_expect,
    rich_lay_out,
    run_rich,
    run_twice_aware,
    start_without_view,
)
from fixture_paths import leaks  # noqa: E402 - sibling module, run as a script

# The oracle's skill layer links to its package directory; a port copies
# the files from the binary (IN-1). The link is read as that copy.
_SKILL_SRC = "{SRC}/opendaisugi/skills/opendaisugi-checklist"


def skill_as_copy(tree: dict[str, Any]) -> dict[str, Any]:
    """The oracle's tree with each link to the packaged skill read as the
    directory of files a port writes there (dirs 0755, files 0644)."""
    src = (
        Path(__file__).resolve().parent.parent
        / "src"
        / "opendaisugi"
        / "skills"
        / "opendaisugi-checklist"
    )
    out: dict[str, Any] = {}
    for rel, entry in tree.items():
        if entry.get("link") != _SKILL_SRC:
            out[rel] = entry
            continue
        out[rel] = {"dir": True, "mode": 0o755}
        for p in sorted(src.rglob("*")):
            if "__pycache__" in p.parts:
                continue
            sub = rel + "/" + str(p.relative_to(src))
            if p.is_dir():
                out[sub] = {"dir": True, "mode": 0o755}
            else:
                out[sub] = {"mode": 0o644, "text": p.read_text(encoding="utf-8")}
    return out


_DETECTED = re.compile(r"^  (?:✓ (.+)|! ([^:]+): .*)$")


def install_lines(text: str) -> list[str]:
    out = []
    for line in text.splitlines():
        if not line.strip():
            continue
        # The binary marks a detected runtime it will fail on ("  ! Name:
        # why"); Python can only tick it. Both read as the runtime's name.
        m = _DETECTED.match(line)
        if m:
            line = "  runtime " + (m.group(1) or m.group(2))
        out.append(line)
    return out


def without_notes(lines: list[str]) -> list[str]:
    """The binary says when it wrote through a symlink; Python is silent."""
    return [ln for ln in lines if not (ln.startswith("note: ") and " is a symlink; wrote " in ln)]


def warnings_as_go(lines: list[str]) -> list[str]:
    return [
        ("warning: " + ln.split(": ", 1)[1]) if ln.startswith("UserWarning: ") else ln
        for ln in lines
    ]


def diff(a: Any, b: Any, path: str = "", out: list[str] | None = None) -> list[str]:
    out = [] if out is None else out
    if len(out) >= 12:
        return out
    if isinstance(a, dict) and isinstance(b, dict):
        for k in list(a) + [k for k in b if k not in a]:
            if k not in a or k not in b:
                out.append(
                    f"{path}/{k}: {'missing' if k not in a else repr(a[k])[:100]} vs "
                    f"{'missing' if k not in b else repr(b[k])[:100]}"
                )
            else:
                diff(a[k], b[k], f"{path}/{k}", out)
    elif isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            out.append(f"{path}: {len(a)} items vs {len(b)}: {a!r:.300} vs {b!r:.300}")
        else:
            for i, (x, y) in enumerate(zip(a, b, strict=True)):
                diff(x, y, f"{path}[{i}]", out)
    elif a != b:
        out.append(f"{path}: {a!r:.300} vs {b!r:.300}")
    return out


def before_tree(case: dict[str, Any], work: Path) -> dict[str, Any]:
    import shutil

    if work.exists():
        shutil.rmtree(work)
    lay_out(case.get("before") or {}, work / "home")
    tree = read_tree(work / "home")
    shutil.rmtree(work)
    return tree


def compare(
    case: dict[str, Any], expect: dict[str, Any], got: dict[str, Any], before: dict[str, Any]
) -> tuple[str, list[str]]:
    err_text = "\n".join(got["stderr"])
    if case.get("go_refuses"):
        # A ruled refusal is worded either way: a flag or command not in
        # the binary, or an input it does not read ("Nothing was changed.").
        ok = (
            got["exit"] == 2
            and ("is not in this binary yet." in err_text or "Nothing was changed." in err_text)
            and got["tree"] == before
            and len(got["stderr"]) == 1
        )
        return (
            ("not-ported", [])
            if ok
            else (
                "disagree",
                ["a flag this binary does not carry was not refused cleanly"]
                + diff(before, got["tree"]),
            )
        )
    if got["exit"] == 2 and expect["exit"] != 2 and "Nothing was changed." in err_text:
        if got["tree"] == before:
            return "refused", [err_text]
        return "disagree", ["refused but changed the tree"] + diff(before, got["tree"])
    problems: list[str] = []
    if got["exit"] != expect["exit"]:
        problems.append(f"exit {expect['exit']} vs {got['exit']}")
    is_install = case["argv"][:1] == ["install"]
    if is_install:
        a, b = install_lines(expect["stdout"]), install_lines(got["stdout"])
        if a != b:
            problems += ["stdout: " + d for d in diff(a, b)]
    elif case["argv"][-2:] == ["help", "--all"]:
        # HP-1: a port leaves out the lines of the commands it does not carry.
        want = help_for_port(expect["stdout"])
        if want != got["stdout"]:
            problems += [
                "stdout: " + d for d in diff(want.splitlines(), got["stdout"].splitlines())
            ]
    elif case["argv"] == ["--version"]:
        # The binary's version comes from its build (a release number, or
        # the commit it was built from), not from pyproject.toml, so only
        # the shape is compared: one non-empty line.
        if not re.fullmatch(r"\S+\n", got["stdout"]):
            problems.append(f"stdout: --version printed {got['stdout']!r}, not one version line")
    elif expect["stdout"] != got["stdout"]:
        problems += ["stdout: " + d for d in diff(expect["stdout"], got["stdout"])]
    if expect["exit"] == 0:
        a, b = warnings_as_go(expect["stderr"]), without_notes(got["stderr"])
        if a != b:
            problems += ["stderr: " + d for d in diff(a, b)]
    want_tree = skill_as_copy(expect["tree"])
    if want_tree != got["tree"]:
        problems += ["tree: " + d for d in diff(want_tree, got["tree"])]
    return ("agree", []) if not problems else ("disagree", problems)


def fail_open(case: dict[str, Any], expect: dict[str, Any], got: dict[str, Any]) -> bool:
    """The binary reported or wrote a stronger gate than Python did."""
    text = json.dumps(got["tree"]) + json.dumps(got["stdout"])
    want = json.dumps(expect["tree"]) + json.dumps(expect["stdout"])
    return ("enforce" in text and "enforce" not in want) or (
        '"armed": true' in want and '"armed": true' not in text and '"armed": false' in text
    )


def interop(case: dict[str, Any], binary: str, work: Path) -> list[str]:
    """Python's gate status on the tree the binary left must equal the
    binary's own status there."""
    import os
    import subprocess

    home = work / "home"
    if not home.exists():
        return []
    env = {
        "HOME": str(home),
        "PATH": "/usr/bin:/bin",
        "PYTHONPATH": str(Path(__file__).resolve().parent.parent / "src"),
        "LANG": "C.UTF-8",
        "NO_COLOR": "1",
        "CUDA_VISIBLE_DEVICES": "",
    }
    cwd = home
    py = subprocess.run(
        [sys.executable, "-m", "opendaisugi.cli", "gate", "status", "--json"],
        env=env,
        cwd=cwd,
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    go = subprocess.run(
        [binary, "gate", "status", "--json"],
        env=env,
        cwd=cwd,
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )
    if (py.returncode, py.stdout) != (go.returncode, go.stdout):
        return [
            f"interop: python status {py.returncode} {py.stdout.strip()} vs binary {go.returncode} {go.stdout.strip()}"
        ]
    del os
    return []


_CARRIES_VIEW: dict[str, bool] = {}


def carries_view(binary: str) -> bool:
    """Whether the binary carries `daisugi dashboard`, the view `start`
    opens: its --help answers instead of a refusal."""
    if binary not in _CARRIES_VIEW:
        import subprocess

        r = subprocess.run(
            [binary, "dashboard", "--help"],
            capture_output=True,
            env={"HOME": str(SCRATCH), "PATH": "/usr/bin:/bin"},
            timeout=60,
            check=False,
        )
        _CARRIES_VIEW[binary] = r.returncode == 0
    return _CARRIES_VIEW[binary]


def start_wrapper(binary: str) -> str:
    """The binary behind a wrapper that stands in for its spawned `gate
    serve` the way the oracle's fake spawn does: it logs the argv and makes
    the socket file. Everything else runs the binary, with the wrapper's
    path as argv[0], so `start` names the wrapper as itself and spawns it."""
    d = SCRATCH / "start-wrapper"
    d.mkdir(parents=True, exist_ok=True)
    w = d / "daisugi"
    w.write_text(
        "#!/bin/bash\n"
        'if [ "$1" = gate ] && [ "$2" = serve ] && [ "$3" = --root ] && [ $# -eq 4 ]; then\n'
        '  printf \'["{DAISUGI}", "gate", "serve", "--root", "%s"]\\n\' "$4" >> "$FAKE_SPAWN_LOG"\n'
        '  : > "$4/gate.sock"\n'
        "  exit 0\n"
        "fi\n"
        f'exec -a "$0" {shlex.quote(binary)} "$@"\n',
        encoding="utf-8",
    )
    w.chmod(0o755)
    return str(w)


def compare_rich(
    case: dict[str, Any], binary: str, i: int, oracle: bool, times: dict[str, list[float]]
) -> tuple[str, list[str], int]:
    """A case of the everyday commands: the garden compare (stderr on every
    case, databases whole, model requests) against the oracle's answer with
    its ruled fields replaced, then the garden guard on what the binary
    wrote."""
    import time as _time

    from garden_compare import compare as garden_compare
    from garden_compare import guard

    work = SCRATCH / "cmp" / f"{i:04d}"
    before = rich_before(case, SCRATCH / "cmp" / f"{i:04d}b")
    t0 = _time.perf_counter()
    runs = start_wrapper(binary) if case.get("oracle") == "start-shim" else binary
    got = run_rich(case, [runs], work)
    times["go"].append(_time.perf_counter() - t0)
    stale = 0
    live = None
    if oracle or case.get("live"):
        t0 = _time.perf_counter()
        live = run_rich(case, python_cmd(case), SCRATCH / "cmp" / f"{i:04d}py")
        times["python"].append(_time.perf_counter() - t0)
        if oracle and not case.get("live") and live != case["expect"]:
            stale = 1
            print(
                f"STALE fixture: {case['name']}: {diff(case['expect'], live)[:3]}", file=sys.stderr
            )
    expect = rich_expect(case)
    if case.get("oracle") == "start-shim" and not carries_view(binary):
        expect = start_without_view(case, expect)
        if case.get("view"):
            case = {**case, "go_refuses": True}
    if case.get("live"):
        # The answer depends on the box: the oracle answers again here.
        fresh = {**case, "expect": live}
        expect = {**live, **(go_expect_for(fresh) or {})}
    cls, problems = garden_compare({**case, "expect": expect}, got, before)
    if cls == "agree" and got.get("spawned", []) != expect.get("spawned", []):
        cls, problems = "disagree", [f"spawned {expect.get('spawned')} vs {got.get('spawned')}"]
    if cls == "agree" and got["tree"] != before:
        problems = guard(binary, work)
        if problems:
            # A store or journal the case laid out broken is broken before
            # the binary runs too; only a new failure counts.
            base = SCRATCH / "cmp" / f"{i:04d}base"
            rich_lay_out(case, base)
            known = {q.split(":")[0] for q in guard(binary, base)}
            problems = [p for p in problems if p.split(":")[0] not in known]
            shutil.rmtree(base, ignore_errors=True)
        if problems:
            cls = "disagree"
    return cls, problems, stale


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--cases", type=Path, default=FIXTURE_DIR / "cases.jsonl")
    ap.add_argument("--oracle", action="store_true", help="also run the Python CLI live")
    ap.add_argument("--json", type=Path, help="write every result here")
    ap.add_argument("--only", help="run only cases whose name contains this text")
    ap.add_argument(
        "--skip", help="leave out cases whose name contains any of these comma-separated texts"
    )
    args = ap.parse_args()
    binary = as_daisugi(args.binary)
    cases = [
        json.loads(line)
        for line in args.cases.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]
    classes: collections.Counter[str] = collections.Counter()
    results = []
    times = {"go": [], "python": []}
    stale = 0
    for i, case in enumerate(cases):
        if args.only and args.only not in case["name"]:
            continue
        if args.skip and any(t in case["name"] for t in args.skip.split(",")):
            continue
        if case.get("runner") == "rich":
            cls, problems, st = compare_rich(case, binary, i, args.oracle, times)
            stale += st
            classes[cls] += 1
            if cls == "disagree":
                print(f"DISAGREE: {case['name']}")
                for p in problems[:12]:
                    print(f"    {p}")
            elif cls == "refused":
                print(f"refused: {case['name']}: {problems[0].strip()[:160]}")
            results.append({"name": case["name"], "class": cls, "problems": problems})
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        before = before_tree(case, work)
        prog = as_daisugi(args.binary, name=case["prog"]) if case.get("prog") else binary
        got, t = run_twice_aware(case, [prog], work)
        times["go"].append(t)
        cls, problems = compare(case, case["expect"], got, before)
        if (
            cls == "agree"
            and case["argv"][:1] == ["install"]
            and got["exit"] == 0
            and "--dry-run" not in case["argv"]
        ):
            problems = interop(case, prog, work)
            if problems:
                cls = "disagree"
        if cls == "disagree" and fail_open(case, case["expect"], got):
            cls = "disagree fail-open"
        if args.oracle:
            live, tp = run_twice_aware(case, python_cmd(case), SCRATCH / "cmp" / f"{i:04d}py")
            times["python"].append(tp)
            if live != case["expect"]:
                stale += 1
                print(f"STALE fixture: {case['name']}", file=sys.stderr)
        classes[cls.split()[0] if cls.startswith("disagree") else cls] += 1
        if cls.startswith("disagree"):
            print(f"{cls.upper()}: {case['name']}")
            for p in problems[:12]:
                print(f"    {p}")
        elif cls == "refused":
            print(f"refused: {case['name']}: {problems[0].strip()[:160]}")
        results.append({"name": case["name"], "class": cls, "problems": problems})
    import shutil

    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    total = sum(classes.values())
    print(f"\n{total} cases: " + ", ".join(f"{k}={v}" for k, v in sorted(classes.items())))
    for side, ts in times.items():
        if ts:
            ts_ms = sorted(x * 1000 for x in ts)
            print(
                f"{side}: p50 {statistics.median(ts_ms):.1f} ms, p90 {ts_ms[int(len(ts_ms) * 0.9)]:.1f} ms per case"
            )
    if args.oracle:
        print(f"stale fixture cases: {stale}")
    if args.json:
        args.json.write_text(json.dumps(results, indent=1) + "\n", encoding="utf-8")
    # The fixtures are published: no path from a real machine may be in one.
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    print(f"machine paths in clients/fixtures: {len(leaked)}")
    return 1 if classes.get("disagree") or stale or leaked else 0


if __name__ == "__main__":
    raise SystemExit(main())
