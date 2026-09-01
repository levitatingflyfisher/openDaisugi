"""Run every CLI case through the Go `daisugi` binary and compare it to the
oracle.

    uv run --no-sync python clients/cli_compare.py \\
        --binary clients/go/daisugi [--oracle] [--json out.json]

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
from the binary's build (only one version line is asked for), and
install, whose Python text
describes layers this binary does not write (those lines are dropped on
both sides). stderr is compared only when Python exited 0, since a Python
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
import statistics
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from cli_cases import (  # noqa: E402 - sibling module, run as a script
    FIXTURE_DIR,
    SCRATCH,
    lay_out,
    python_cmd,
    read_tree,
    run_twice_aware,
)
from fixture_paths import leaks  # noqa: E402 - sibling module, run as a script

_PY_ONLY = (
    "Skill is discovered on demand",
    "Tool calls are captured to",
    "pi asks the gate in-process.",
    "OpenCode asks the gate in-process.",
)
_GO_ONLY = (
    "This binary installs the gate layer only.",
    "layers come from the Python CLI",
    "pi asks the resident gate",
    "OpenCode asks the resident gate",
)


_DETECTED = re.compile(r"^  (?:✓ (.+)|! ([^:]+): .*)$")


def install_lines(text: str) -> list[str]:
    out = []
    for line in text.splitlines():
        if not line.strip() or line.startswith(_PY_ONLY) or line.startswith(_GO_ONLY):
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
    return [("warning: " + ln.split(": ", 1)[1]) if ln.startswith("UserWarning: ") else ln for ln in lines]


def diff(a: Any, b: Any, path: str = "", out: list[str] | None = None) -> list[str]:
    out = [] if out is None else out
    if len(out) >= 12:
        return out
    if isinstance(a, dict) and isinstance(b, dict):
        for k in list(a) + [k for k in b if k not in a]:
            if k not in a or k not in b:
                out.append(f"{path}/{k}: {'missing' if k not in a else repr(a[k])[:100]} vs "
                           f"{'missing' if k not in b else repr(b[k])[:100]}")
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


def compare(case: dict[str, Any], expect: dict[str, Any], got: dict[str, Any], before: dict[str, Any]) -> tuple[str, list[str]]:
    err_text = "\n".join(got["stderr"])
    if case.get("go_refuses"):
        ok = got["exit"] == 2 and "is not in this binary yet." in err_text and got["tree"] == before \
            and len(got["stderr"]) == 1
        return ("not-ported", []) if ok else ("disagree", ["a flag this binary does not carry was not refused cleanly"]
                                              + diff(before, got["tree"]))
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
    if expect["tree"] != got["tree"]:
        problems += ["tree: " + d for d in diff(expect["tree"], got["tree"])]
    return ("agree", []) if not problems else ("disagree", problems)


def fail_open(case: dict[str, Any], expect: dict[str, Any], got: dict[str, Any]) -> bool:
    """The binary reported or wrote a stronger gate than Python did."""
    text = json.dumps(got["tree"]) + json.dumps(got["stdout"])
    want = json.dumps(expect["tree"]) + json.dumps(expect["stdout"])
    return ("enforce" in text and "enforce" not in want) or (
        '"armed": true' in want and '"armed": true' not in text and '"armed": false' in text)


def interop(case: dict[str, Any], binary: str, work: Path) -> list[str]:
    """Python's gate status on the tree the binary left must equal the
    binary's own status there."""
    import os
    import subprocess

    home = work / "home"
    if not home.exists():
        return []
    env = {"HOME": str(home), "PATH": "/usr/bin:/bin", "PYTHONPATH": str(Path(__file__).resolve().parent.parent / "src"),
           "LANG": "C.UTF-8", "NO_COLOR": "1", "CUDA_VISIBLE_DEVICES": ""}
    cwd = home
    py = subprocess.run([sys.executable, "-m", "opendaisugi.cli", "gate", "status", "--json"], env=env, cwd=cwd,
                        capture_output=True, text=True, timeout=120, check=False)
    go = subprocess.run([binary, "gate", "status", "--json"], env=env, cwd=cwd, capture_output=True, text=True,
                        timeout=60, check=False)
    if (py.returncode, py.stdout) != (go.returncode, go.stdout):
        return [f"interop: python status {py.returncode} {py.stdout.strip()} vs binary {go.returncode} {go.stdout.strip()}"]
    del os
    return []


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--cases", type=Path, default=FIXTURE_DIR / "cases.jsonl")
    ap.add_argument("--oracle", action="store_true", help="also run the Python CLI live")
    ap.add_argument("--json", type=Path, help="write every result here")
    ap.add_argument("--only", help="run only cases whose name contains this text")
    args = ap.parse_args()
    binary = str(Path(args.binary).resolve())
    cases = [json.loads(line) for line in args.cases.read_text(encoding="utf-8").splitlines() if line.strip()]
    classes: collections.Counter[str] = collections.Counter()
    results = []
    times = {"go": [], "python": []}
    stale = 0
    for i, case in enumerate(cases):
        if args.only and args.only not in case["name"]:
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        before = before_tree(case, work)
        got, t = run_twice_aware(case, [binary], work)
        times["go"].append(t)
        cls, problems = compare(case, case["expect"], got, before)
        if cls == "agree" and case["argv"][:1] == ["install"] and got["exit"] == 0 and "--dry-run" not in case["argv"]:
            problems = interop(case, binary, work)
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
            print(f"{side}: p50 {statistics.median(ts_ms):.1f} ms, p90 {ts_ms[int(len(ts_ms) * 0.9)]:.1f} ms per case")
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
