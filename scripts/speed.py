"""Cold-start speed of every daisugi, coppice and sprig command, in Python, Go and Rust.

    uv run --no-sync python scripts/speed.py                 # table
    uv run --no-sync python scripts/speed.py --json          # the same, as JSON
    uv run --no-sync python scripts/speed.py --check         # fail over budget
    uv run --no-sync python scripts/speed.py --suggest       # budgets from medians

What it measures: one process start, to exit, for each surface below. A
surface is a tool, an implementation and a command:

  daisugi        py go rs   --version, --help, gate check, gate status
  daisugi-gate   py go rs   the hook check the agent pays on every tool call
                            (py is the in-process gate; py-client is the
                            installed hook entry with no resident server)
  coppice        go rs      --version, --help
  sprig          go rs      --version, --help

Every run is a fresh process. The first run of each surface is a warm-up and
is dropped (it fills the page cache). Fakes only: HOME and every XDG
directory sit in a scratch directory on real disk (SPEED_SCRATCH, default
.speed/scratch in this checkout), the gate reads one fake envelope and one fixed
fake hook payload, and nothing here opens a network connection, a live floor
or a model.

Binaries: the Go and Rust ones come from SPEED_BIN_DIR (default
.speed/bin in this checkout), named <tool>-go and <tool>-rs, which
scripts/speed-build.sh builds. A missing binary is skipped and said so;
--require-all makes it an error. Python runs from this checkout's src/.

Budgets live in docs/reference/speed-budgets.json (see speed-budgets.md).
--check compares each median with budget x factor. The factor is 1.0 by
default (the strict local mode). A slow shared runner sets
SPEED_FACTOR=3 (or --factor 3). The median of N runs, never one run, is
what is compared.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
from collections.abc import Callable, Iterable
from dataclasses import dataclass, field
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
BUDGETS_PATH = REPO / "docs" / "reference" / "speed-budgets.json"
DEFAULT_BIN_DIR = REPO / ".speed" / "bin"
DEFAULT_SCRATCH = REPO / ".speed" / "scratch"

# The one fixed fake hook payload: a Claude Code PreToolUse call for a plain
# shell command. The envelope below allows it, so the gate runs its whole
# path (parse, decompose, verify, audit write) and says yes.
HOOK_PAYLOAD = (
    '{"session_id": "s1", "tool_name": "Bash", '
    '"tool_input": {"command": "ls -la src"}, "cwd": "/work"}'
)
ENVELOPE = {
    "generated_by": "speed",
    "task": "synthetic speed envelope",
    "permissions": {
        "file_read": ["/**", "./**"],
        "file_write": ["/**", "./**"],
        "shell": True,
        "shell_allowlist": ["ls", "cat", "echo"],
        "network": False,
        "mcp_allowlist": ["github/*"],
        "max_execution_time_s": 60,
        "max_output_size_mb": 20,
        "shell_allow_decomposition": True,
    },
    "id": "env_base",
    "shell_interpreter_policy": "allow",
}


ALLOW = re.compile(r'"continue"\s*:\s*true')
# A call the envelope above must deny: rm is not in its shell allowlist.
DENY_PAYLOAD = (
    '{"session_id": "s1", "tool_name": "Bash", '
    '"tool_input": {"command": "rm -rf /work/x"}, "cwd": "/work"}'
)
DENIED = re.compile(r"DENIED")
# What sprig --help prints (Go's flag package writes it to stderr).
HELP_LOOK = re.compile(r"Usage|^\s+-version\b", re.MULTILINE)


@dataclass(frozen=True)
class Surface:
    tool: str
    impl: str  # py, py-client, go, rs
    command: str
    stdin: bool = False
    # sprig --help exits 2 in Go (its flag package) and in Rust (the port
    # copies Go's exit codes). Exit 2 counts only when the output looks like
    # help (HELP_LOOK).
    ok_codes: tuple[int, ...] = (0,)

    @property
    def key(self) -> str:
        return f"{self.tool}/{self.impl}/{self.command}"


def _surfaces() -> list[Surface]:
    out: list[Surface] = []
    for impl in ("py", "go", "rs"):
        for cmd in ("version", "help", "gate-check", "gate-status"):
            out.append(Surface("daisugi", impl, cmd, stdin=cmd == "gate-check"))
    for impl in ("py", "py-client", "go", "rs"):
        out.append(Surface("daisugi-gate", impl, "gate-hook", stdin=True))
    for tool in ("coppice", "sprig"):
        for impl in ("go", "rs"):
            for cmd in ("version", "help"):
                ok = (0, 2) if tool == "sprig" and cmd == "help" else (0,)
                out.append(Surface(tool, impl, cmd, ok_codes=ok))
    return out


SURFACES = _surfaces()


# --- argv for one surface ---------------------------------------------------


def gate_args(root: Path) -> list[str]:
    return [
        "--mode",
        "enforce",
        "--root",
        str(root),
        "--format",
        "claude",
        "--verify-timeout",
        "5.0",
    ]


def is_built(path: Path) -> bool:
    """The one test for "this binary exists": a file we may run."""
    return path.is_file() and os.access(path, os.X_OK)


def binary_for(s: Surface, bin_dir: Path) -> Path | None:
    """The executable for a Go or Rust surface, or None when it is not built."""
    p = bin_dir / f"{s.tool}-{s.impl}"
    return p if is_built(p) else None


def argv_for(s: Surface, bin_dir: Path, root: Path) -> list[str] | None:
    """The command line for a surface, or None when its binary is missing."""
    if s.impl in ("py", "py-client"):
        py = [sys.executable]
        if s.tool == "daisugi-gate":
            mod = "opendaisugi.gate" if s.impl == "py" else "opendaisugi.gate_client"
            return [*py, "-m", mod, *gate_args(root)]
        head = [*py, "-m", "opendaisugi.cli_entry"]
    else:
        b = binary_for(s, bin_dir)
        if b is None:
            return None
        if s.tool == "daisugi-gate":
            return [str(b), *gate_args(root)]
        head = [str(b)]
    if s.command == "version":
        return [*head, "--version"]
    if s.command == "help":
        return [*head, "--help"]
    if s.command == "gate-check":
        return [*head, "gate", "check", *gate_args(root)]
    if s.command == "gate-status":
        return [*head, "gate", "status", "--root", str(root)]
    raise ValueError(f"unknown command {s.command}")


# --- the fake world ---------------------------------------------------------


def make_world(scratch_parent: Path) -> tuple[Path, dict[str, str], Path]:
    """A scratch HOME and XDG tree on real disk, with one envelope registered.

    Returns (world dir, environment, gate root)."""
    scratch_parent.mkdir(parents=True, exist_ok=True)
    world = Path(tempfile.mkdtemp(prefix="speed-", dir=scratch_parent))
    home = world / "home"
    data = world / "data"
    gate_root = data / "gate"
    (gate_root / "envelopes").mkdir(parents=True)
    (gate_root / "envelopes" / "default.json").write_text(json.dumps(ENVELOPE), encoding="utf-8")
    for d in ("home", "config", "state", "cache", "tmp"):
        (world / d).mkdir(exist_ok=True)
    env = {
        "HOME": str(home),
        "PATH": "/usr/bin:/bin",
        "XDG_DATA_HOME": str(data),
        "XDG_CONFIG_HOME": str(world / "config"),
        "XDG_STATE_HOME": str(world / "state"),
        "XDG_CACHE_HOME": str(world / "cache"),
        "OPENDAISUGI_HOME": str(data),
        "TMPDIR": str(world / "tmp"),
        "PYTHONPATH": str(REPO / "src"),
        "CUDA_VISIBLE_DEVICES": "",
        "NO_COLOR": "1",
        "LC_ALL": "C.UTF-8",
    }
    return world, env, gate_root


# --- measuring --------------------------------------------------------------


@dataclass
class Result:
    key: str
    runs_ms: list[float] = field(default_factory=list)
    skipped: str = ""
    error: str = ""

    @property
    def median_ms(self) -> float:
        return statistics.median(self.runs_ms)

    @property
    def p95_ms(self) -> float:
        return p95(self.runs_ms)


def p95(values: Iterable[float]) -> float:
    """Nearest-rank 95th percentile: the smallest value with at least 95% of
    the runs at or below it."""
    v = sorted(values)
    return v[max(0, math.ceil(0.95 * len(v)) - 1)]


def time_once(
    argv: list[str], env: dict[str, str], stdin: bytes | None, cwd: Path
) -> tuple[float, int, str, str]:
    """Run once. Returns (ms, exit code, stdout, stderr)."""
    t0 = time.perf_counter()
    p = subprocess.run(
        argv,
        input=stdin if stdin is not None else b"",
        env=env,
        cwd=cwd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    ms = (time.perf_counter() - t0) * 1000.0
    return (
        ms,
        p.returncode,
        p.stdout.decode("utf-8", "replace"),
        p.stderr.decode("utf-8", "replace"),
    )


def is_gate(s: Surface) -> bool:
    return s.command in ("gate-check", "gate-hook")


def run_problem(s: Surface, argv: list[str], code: int, out: str, err: str) -> str:
    """Why one timed run does not count, or "" when it does."""
    where = f"{Path(argv[0]).name} {' '.join(argv[1:4])}"
    if code not in s.ok_codes:
        return f"exit {code} from {where}: {err.strip()[-300:]}"
    if code != 0 and not HELP_LOOK.search(out + err):
        return f"exit {code} from {where} without help text: {err.strip()[-200:]}"
    if is_gate(s) and not ALLOW.search(out):
        # A deny or a crash is a different (often faster) path: timing it
        # would flatter the gate.
        return f"the gate did not allow the fixed payload: {out.strip()[:120]}"
    return ""


def probe_problem(argv: list[str], env: dict[str, str], cwd: Path) -> str:
    """Run the gate once with a payload the envelope must deny. The real
    deny is exit 2 and a DENIED line on stderr. Anything else means the
    envelope did not load (a gate that fails open allows everything), so
    the timings after it would mean nothing."""
    _, code, _, err = time_once(argv, env, DENY_PAYLOAD.encode(), cwd)
    if code == 2 and DENIED.search(err):
        return ""
    return f"the deny probe was not denied (exit {code}): {err.strip()[-200:]}"


def measure(
    surfaces: list[Surface],
    runs: int,
    bin_dir: Path,
    scratch_parent: Path,
    require_all: bool = False,
    on_progress: Callable[[str], None] | None = None,
) -> list[Result]:
    """Time every surface `runs` times after one warm-up round.

    The runs are interleaved: round 1 starts each surface once, then round 2,
    and so on. One burst of noise on the box then lands on all surfaces
    alike, so a ratio between two of them stays fair."""
    world, env, gate_root = make_world(scratch_parent)
    results: list[Result] = []
    live: list[tuple[Surface, Result, list[str], bytes | None]] = []
    try:
        for s in surfaces:
            r = Result(s.key)
            results.append(r)
            argv = argv_for(s, bin_dir, gate_root)
            if argv is None:
                r.skipped = f"no {s.tool}-{s.impl} in {bin_dir}"
                if require_all:
                    r.error = r.skipped
                continue
            if is_gate(s):
                r.error = probe_problem(argv, env, world)
                if r.error:
                    continue
            if on_progress:
                on_progress(s.key)
            live.append((s, r, argv, HOOK_PAYLOAD.encode() if s.stdin else None))
        for i in range(runs + 1):  # round 0 is the warm-up
            for item in list(live):
                s, r, argv, stdin = item
                ms, code, out, err = time_once(argv, env, stdin, world)
                r.error = run_problem(s, argv, code, out, err)
                if r.error:
                    live.remove(item)
                elif i > 0:
                    r.runs_ms.append(ms)
    finally:
        shutil.rmtree(world, ignore_errors=True)
    return results


# --- budgets ----------------------------------------------------------------


def load_budgets(path: Path = BUDGETS_PATH) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def tidy_up(ms: float) -> int:
    """Round a budget up to a tidy number: 1 ms steps under 10, 5 ms under
    100, 25 ms under 1000, 100 ms above."""
    step = 1 if ms < 10 else 5 if ms < 100 else 25 if ms < 1000 else 100
    return int(math.ceil(ms / step) * step)


def suggest_budget(median_ms: float) -> int:
    """The budget rule: twice the median, or the median plus 5 ms, whichever
    is larger, rounded up to a tidy number."""
    return tidy_up(max(2.0 * median_ms, median_ms + 5.0))


def check(results: list[Result], budgets: dict, factor: float) -> list[str]:
    """Problems found, as text lines. Empty means every measured surface is
    within budget x factor and every relative rule holds. A surface with no
    measurement (binary not built) is not a problem here."""
    problems: list[str] = []
    by_key = {r.key: r for r in results}
    for key, limit in sorted(budgets["budgets_ms"].items()):
        r = by_key.get(key)
        if r is None or r.skipped:
            continue
        if r.error:
            problems.append(f"{key}: {r.error}")
        elif r.median_ms > limit * factor:
            problems.append(
                f"{key}: median {r.median_ms:.1f} ms over budget {limit} ms x {factor:g}"
            )
    for rule in budgets.get("relative", []):
        fast, slow = by_key.get(rule["fast"]), by_key.get(rule["slow"])
        if not fast or not slow or fast.skipped or slow.skipped or fast.error or slow.error:
            continue
        ratio = fast.median_ms / slow.median_ms
        if ratio > rule["max_ratio"]:
            problems.append(
                f"{rule['fast']} is {ratio:.2f}x of {rule['slow']}, over {rule['max_ratio']}x"
            )
    return problems


def unbudgeted(budgets: dict) -> list[str]:
    """Surfaces with no budget: every surface must have one."""
    return [s.key for s in SURFACES if s.key not in budgets["budgets_ms"]]


# --- output -----------------------------------------------------------------


def table(results: list[Result]) -> str:
    rows = [("surface", "median ms", "p95 ms", "min ms", "runs")]
    for r in results:
        if r.skipped:
            rows.append((r.key, "n/a", "n/a", "n/a", f"skipped: {r.skipped}"))
        elif r.error:
            rows.append((r.key, "ERR", "", "", r.error))
        else:
            rows.append(
                (
                    r.key,
                    f"{r.median_ms:.1f}",
                    f"{r.p95_ms:.1f}",
                    f"{min(r.runs_ms):.1f}",
                    str(len(r.runs_ms)),
                )
            )
    w = [max(len(row[i]) for row in rows) for i in range(5)]
    return "\n".join(
        "  ".join(c.ljust(w[i]) if i in (0, 4) else c.rjust(w[i]) for i, c in enumerate(row))
        for row in rows
    )


def as_json(results: list[Result], runs: int) -> dict:
    return {
        "runs": runs,
        "results": {
            r.key: (
                {"skipped": r.skipped}
                if r.skipped
                else {"error": r.error}
                if r.error
                else {
                    "median_ms": round(r.median_ms, 2),
                    "p95_ms": round(r.p95_ms, 2),
                    "min_ms": round(min(r.runs_ms), 2),
                    "max_ms": round(max(r.runs_ms), 2),
                    "runs_ms": [round(x, 2) for x in r.runs_ms],
                }
            )
            for r in results
        },
    }


def suggestion(results: list[Result], base: dict | None) -> dict:
    out = dict(base or {})
    out["budgets_ms"] = {
        r.key: suggest_budget(r.median_ms) for r in results if not r.skipped and not r.error
    }
    return out


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("-n", "--runs", type=int, default=int(os.environ.get("SPEED_RUNS", "15")))
    ap.add_argument(
        "--only", action="append", default=[], help="keep surfaces whose key contains this text"
    )
    ap.add_argument("--json", action="store_true", help="print JSON instead of the table")
    ap.add_argument(
        "--check", action="store_true", help="exit 1 when a median is over budget x factor"
    )
    ap.add_argument("--suggest", action="store_true", help="print budgets set from these medians")
    ap.add_argument("--factor", type=float, default=float(os.environ.get("SPEED_FACTOR", "1")))
    ap.add_argument("--budgets", type=Path, default=BUDGETS_PATH)
    ap.add_argument(
        "--bin-dir", type=Path, default=Path(os.environ.get("SPEED_BIN_DIR", DEFAULT_BIN_DIR))
    )
    ap.add_argument(
        "--scratch", type=Path, default=Path(os.environ.get("SPEED_SCRATCH", DEFAULT_SCRATCH))
    )
    ap.add_argument("--require-all", action="store_true", help="a missing binary is an error")
    a = ap.parse_args(argv)

    surfaces = [s for s in SURFACES if not a.only or any(t in s.key for t in a.only)]
    quiet = a.json or a.suggest
    results = measure(
        surfaces,
        a.runs,
        a.bin_dir,
        a.scratch,
        require_all=a.require_all,
        on_progress=None if quiet else lambda k: print(f"measuring {k}", file=sys.stderr),
    )
    if a.suggest:
        base = load_budgets(a.budgets) if a.budgets.exists() else None
        print(json.dumps(suggestion(results, base), indent=2, sort_keys=True))
    elif a.json:
        print(json.dumps(as_json(results, a.runs), indent=2, sort_keys=True))
    else:
        print(table(results))
    status = 0
    if any(r.error for r in results):
        status = 1
    if a.check:
        budgets = load_budgets(a.budgets)
        problems = check(results, budgets, a.factor)
        missing = unbudgeted(budgets)
        problems += [f"{k}: no budget" for k in missing]
        for p in problems:
            print(f"OVER: {p}", file=sys.stderr)
        if problems:
            status = 1
    return status


if __name__ == "__main__":
    raise SystemExit(main())
