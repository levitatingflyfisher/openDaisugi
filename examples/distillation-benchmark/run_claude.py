"""Stage-4 distillation-fidelity — reliable-executor variant (claude-code backend).

The Ollama pilot (`run_pilot.py`) is dominated by a 4B model's execution noise:
cold success ~67%, so reuse fires rarely and the cost signal is swamped
(`RESULTS.md`). RESULTS.md names the fix — *"the thesis most plausibly holds
better with a more reliable model … the obvious next experiment"* — and this is
that experiment. It drives orchestration through the **claude-code** backend
(`OPENDAISUGI_LLM_BACKEND=claude-code`), a reliable executor that clears the
~90% success bar the 4B model could not, so the reuse effect is isolated from
execution noise.

**What this measures — and what it does not.** The claude-code path shells out
to `claude -p`; it does *not* flow through litellm, so `run_pilot.py`'s
litellm-wrapping token tally reads zero on it. This runner therefore reports the
signal it *can* measure honestly on that path — **cold/warm success, reuse-fire
rate, latency delta, and the safety direction** — and leaves the *token* cost
story to `examples/jit-metrics/` (which measures the guard cost directly and
labels the token amortization an estimate). The point of this run is the
**fidelity** question the Ollama pilot could not answer under its noise: with a
reliable executor, does a reused pathway actually *succeed*, or does it drop a
step (the reuse-execution gap the pilot flagged)?

Usage (opt-in, like the other live scripts):

    OPENDAISUGI_LLM_BACKEND=claude-code python run_claude.py            # 4×3 bounded
    OPENDAISUGI_LLM_BACKEND=claude-code python run_claude.py --full     # 20×5 (~hours)
"""

from __future__ import annotations

import argparse
import asyncio
import os
import sys
import tempfile
import time

from opendaisugi import Daisugi
from opendaisugi.benchmark import RunMetric, meets_stage4_bar, run_paired_benchmark, summarize

# The task corpus + envelope are the pilot's — import them so the two runners
# measure the same tasks over the same authorization boundary.
from run_pilot import _TASKS, _envelope, _metric


def make_runner(*, cwd: str, verbose: bool = False):
    envelope = _envelope(cwd)
    stats = {"warm_reused": 0, "warm_total": 0}
    warm_dirs: dict[str, str] = {}

    def _prime(task, *, tries: int = 3) -> str:
        """Run + distill in an isolated store so a warm run has a pathway to reuse.
        A reliable executor should need few retries (unlike the 4B pilot)."""
        d = tempfile.mkdtemp(prefix="daisugi-cc-warm-")

        async def _attempt():
            dai = Daisugi(data_dir=d)
            r = await dai.orchestrate(task["prompt"], envelope=envelope, stakes="low")
            if r.status == "succeeded":
                await dai.tend(min_traces=1)
                return dai.pathway_store.find(task["prompt"], threshold=0.0) is not None
            return False

        for _ in range(tries):
            try:
                if asyncio.run(asyncio.wait_for(_attempt(), timeout=300)):
                    return d
            except Exception as exc:
                if verbose:
                    print(
                        f"  prime {task['id']}: {type(exc).__name__}: {str(exc)[:80]}",
                        file=sys.stderr,
                    )
        if verbose:
            print(f"  prime {task['id']}: no pathway after {tries} tries", file=sys.stderr)
        return d

    def runner(task, *, warm, seed):
        if warm:
            data_dir = warm_dirs.get(task["id"]) or _prime(task)
            warm_dirs[task["id"]] = data_dir
        else:
            data_dir = tempfile.mkdtemp(prefix="daisugi-cc-cold-")

        dai = Daisugi(data_dir=data_dir)

        async def _go():
            t0 = time.monotonic()
            res = await dai.orchestrate(task["prompt"], envelope=envelope, stakes="low")
            dt = (time.monotonic() - t0) * 1000
            if warm:
                stats["warm_total"] += 1
                if res.reused_pathway:
                    stats["warm_reused"] += 1
            # tokens: budget.spent counts task-step spend only (decompose/synth are
            # overhead and, on the claude-code path, unmetered). Recorded for
            # completeness; latency + success are the load-bearing signal here.
            m = _metric(res, int(getattr(res.budget, "spent", 0) or 0), dt)
            return m

        try:
            return asyncio.run(asyncio.wait_for(_go(), timeout=300))
        except Exception as exc:
            if verbose:
                print(
                    f"  drop {task['id']} warm={warm} seed={seed}: "
                    f"{type(exc).__name__}: {str(exc)[:100]}",
                    file=sys.stderr,
                )
            return None

    return runner, stats


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--full", action="store_true", help="Run the ≥20×5 bar (~hours).")
    ap.add_argument("--tasks", type=int, default=4, help="Task count for the bounded run.")
    ap.add_argument("--repeats", type=int, default=3)
    ap.add_argument("--cwd", default=os.getcwd())
    args = ap.parse_args()

    tasks = _TASKS if args.full else _TASKS[: args.tasks]
    repeats = 5 if args.full else args.repeats

    print(f"backend=claude-code  tasks={len(tasks)}  repeats={repeats}", flush=True)
    runner, stats = make_runner(cwd=args.cwd, verbose=True)
    results = run_paired_benchmark(tasks, runner, repeats=repeats)
    s = summarize(results)

    reuse_rate = (stats["warm_reused"] / stats["warm_total"]) if stats["warm_total"] else 0.0
    print("\n=== gates ===")
    print(
        f"gate 1 — reuse fires on warm: {stats['warm_reused']}/{stats['warm_total']} "
        f"({reuse_rate:.0%})  {'PASS' if reuse_rate > 0.5 else 'FAIL — warm≈cold, void'}"
    )
    print(
        f"gate 2 — cold success rate: {s['cold_success_rate']:.0%}  "
        f"{'PASS' if s['cold_success_rate'] > 0 else 'FAIL'}"
    )
    print("\n=== results (reliable executor) ===")
    print(f"success rate cold/warm:  {s['cold_success_rate']:.0%} / {s['warm_success_rate']:.0%}")
    print(f"latency delta ms (succ): {s['latency_delta_mean']}  ci95={s['latency_delta_ci95']}")
    print(f"safety regression:       {s['safety_regression']}")
    print(f"meets Stage-4 bar:       {meets_stage4_bar(results)}")
    print("(token delta not metered on the claude-code path — see examples/jit-metrics/)")
    return 0


if __name__ == "__main__":
    os.environ.setdefault("OPENDAISUGI_LLM_BACKEND", "claude-code")
    os.environ.setdefault("DAISUGI_CLAUDE_CODE_INTEGRATION", "1")
    sys.exit(main())
