"""Distillation-fidelity benchmark — live pilot runner (roadmap Stage 4).

Wires a local Ollama model into `opendaisugi.benchmark`: the same task run
COLD (empty pathway store → the orchestrator decomposes fresh via the model)
and WARM (a pathway distilled from a prior success is in the store → reuse
short-circuits the decompose). The saving distillation buys is the skipped
planning call; the harness measures it with the two-denominator stratification
(cost over successful runs, outcome over all).

**Resource note.** Small local models on a memory-constrained box are slow and
swap-thrash. This runner defaults to whatever model is already RESIDENT in
Ollama (avoiding an evicting model swap) and treats a timeout / server restart
as a dropped run, not a crash — so a flaky box degrades the sample size
honestly rather than corrupting numbers. Do not run the full ≥20×≥5 bar
concurrently with another benchmark on the same constrained box.

Usage (opt-in, like the other live scripts). Force the embedder onto CPU if
the box's torch/CUDA build mismatches its GPU (a common local-box state):

    CUDA_VISIBLE_DEVICES="" DAISUGI_CLAUDE_CODE_INTEGRATION=1 python run_pilot.py          # 3×3 pilot
    CUDA_VISIBLE_DEVICES="" DAISUGI_CLAUDE_CODE_INTEGRATION=1 python run_pilot.py --full   # 20×5

**Known open finding (2026-07-25 first real run).** The mechanism validates end
to end — a cold run succeeds, `tend` distills a pathway, and a warm run *reuses*
it (`reused_pathway=True`, gate 1 fires). But warm reuse executes the template
*directly* (no `adapt_plan`, so 0 LLM tokens) and, on a plan with a dependent
`task` step, runs the leading `file_read` yet **silently skips the dependent
task step** (no receipt → integrity fail → run fails). Reuse fires, costs zero,
and produces a broken result — the literal "the standard block looked reusable
but dropped something essential" failure. So the full ≥20×5 run is not worth
launching until that reuse-execution gap (a dependency/id-preservation issue in
generalized template reuse) is understood: warm would score 0% success and the
cost delta would be undefined. This is itself a Stage-4 finding, not a null one.

The pilot's job is to clear two gates before any full run is worth it:
  (1) reuse actually fires on warm runs (else warm == cold, measuring nothing);
  (2) cold clears decompose+verify a real fraction of the time (else there is
      an outcome story but no cost story).
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
import urllib.request

from opendaisugi import Daisugi, Envelope, Permission
from opendaisugi.benchmark import RunMetric, meets_stage4_bar, run_paired_benchmark, summarize

OLLAMA = "http://localhost:11434"


# --- token accounting: tally every litellm call in one orchestrate ----------

class _Tally:
    """Tokens for every litellm call in one orchestrate, captured by WRAPPING
    acompletion/completion — synchronous with the await, so it can't be lost to
    an orphaned async logging worker (litellm's callback path races with our
    per-call asyncio.run()). Instructor retries flow through the same wrapped
    functions, so a small model's schema retries are counted as the real cost
    they are. openDaisugi imports these at each call site, so patching the
    module attribute is enough."""

    def __init__(self):
        self.total = 0
        self.calls = 0

    def reset(self):
        self.total = 0
        self.calls = 0

    def _add(self, resp):
        u = getattr(resp, "usage", None)
        if u is not None:
            self.total += (getattr(u, "total_tokens", 0) or 0)
            self.calls += 1


def _install_tally() -> _Tally:
    import litellm
    litellm.drop_params = True
    tally = _Tally()
    orig_a, orig_s = litellm.acompletion, litellm.completion

    async def _wrapped_a(*a, **k):
        r = await orig_a(*a, **k)
        tally._add(r)
        return r

    def _wrapped_s(*a, **k):
        r = orig_s(*a, **k)
        tally._add(r)
        return r

    litellm.acompletion = _wrapped_a
    litellm.completion = _wrapped_s
    return tally


def _resident_model() -> str | None:
    """The model already loaded in Ollama — use it to avoid an evicting swap."""
    try:
        with urllib.request.urlopen(f"{OLLAMA}/api/ps", timeout=5) as r:
            data = json.loads(r.read())
        loaded = data.get("models") or []
        if loaded:
            return f"ollama_chat/{loaded[0]['name']}"
    except Exception:
        pass
    return None


# --- the task corpus + a permissive-but-real envelope -----------------------
# Phrased to avoid compound shell commands (the metachar gate would reject a
# fresh `a && b`), and paired with an envelope broad enough that a competent
# small model's natural plan verifies a real fraction of the time.

def _envelope(cwd: str) -> Envelope:
    real = os.path.realpath(cwd)
    return Envelope(
        generated_by="stage4-benchmark",
        task="local analysis tasks",
        permissions=Permission(
            # Two globs, each for a different check:
            #  - "**" matches the RELATIVE paths a small model naturally emits
            #    ("README.md") at plan-time verify (lexical, I/O-free);
            #  - "<cwd>/**" (concrete, absolute) satisfies the EXECUTOR's
            #    run-time symlink guard, which resolves the path and needs a
            #    concrete base to anchor to. A bare "/**" passes verify but the
            #    executor always refuses it (its glob-base resolves to "/",
            #    which the guard can't anchor) — a real plan-time/run-time
            #    inconsistency; scoping to the workspace is the right envelope
            #    anyway (you'd never grant an agent all of /).
            file_read=["**", f"{real}/**"],
            file_write=["**", f"{real}/**"],
            shell=True,
            shell_allowlist=["ls", "cat", "grep", "find", "wc", "head", "tail",
                             "git", "echo", "sort", "uniq"],
            network=False,
        ),
        stakes="low",
    )


_TASKS = [
    {"id": "list-py", "prompt": "List the Python files in the current directory."},
    {"id": "count-lines", "prompt": "Count the total number of lines in README.md."},
    {"id": "find-todos", "prompt": "Find lines containing the word TODO in the source files."},
    {"id": "largest-file", "prompt": "Report which file in the current directory is the largest."},
    {"id": "git-status", "prompt": "Show the current git status of the repository."},
    {"id": "head-changelog", "prompt": "Show the first 20 lines of CHANGELOG.md."},
    {"id": "grep-import", "prompt": "List the files that import the json module."},
    {"id": "count-tests", "prompt": "Count how many test files are in the tests directory."},
    {"id": "list-docs", "prompt": "List the markdown files under the docs directory."},
    {"id": "word-count", "prompt": "Report the word count of the VISION document."},
    {"id": "recent-commit", "prompt": "Show the most recent git commit message."},
    {"id": "find-configs", "prompt": "Find the configuration files (toml, yaml) in the project."},
    {"id": "list-examples", "prompt": "List the subdirectories of the examples directory."},
    {"id": "cat-license", "prompt": "Show the first lines of the LICENSE file."},
    {"id": "grep-def", "prompt": "List the files that define a function named verify."},
    {"id": "count-py", "prompt": "Count how many Python files are in the src tree."},
    {"id": "head-readme", "prompt": "Show the first 10 lines of README.md."},
    {"id": "find-md", "prompt": "Find all markdown files in the repository."},
    {"id": "sort-sizes", "prompt": "List the files in the current directory sorted by size."},
    {"id": "grep-class", "prompt": "List the files that define a class named Envelope."},
]


def _metric(res, tokens: int, dt_ms: float) -> RunMetric:
    sess = res.session
    plan_rejected = 1 if res.status == "rejected" else 0
    step_denials = sum(
        1 for s in getattr(sess, "steps", [])
        if getattr(s, "approved_by", None) == "denied"
        or str(getattr(s, "status", "")).startswith("rejected")
    )
    return RunMetric(
        tokens=tokens, latency_ms=dt_ms,
        success=(res.status == "succeeded"),
        denials=plan_rejected + step_denials, violations=0,
    )


def make_runner(model: str, *, cwd: str, verbose: bool = False):
    import tempfile

    from opendaisugi.tier1 import LiteLLMTier1Provider

    tally = _install_tally()
    envelope = _envelope(cwd)
    # The local model as Tier-1 too, so the plan's `task` (LLM-reasoning) steps
    # route to it instead of falling through to a cloud model with no API key.
    tier1 = LiteLLMTier1Provider(model=model, base_url=OLLAMA, api_key=None)
    stats = {"warm_reused": 0, "warm_total": 0}
    warm_dirs: dict[str, str] = {}   # task_id -> primed, isolated data_dir

    def _prime(task, *, tries: int = 4) -> str:
        """Run the task in a fresh isolated store and distill, so a matching
        pathway exists for the warm measured runs to reuse. Retries until a
        pathway is actually present: priming only distills when the run
        succeeds (~cold success rate), so without retry ~1/3 of tasks would
        never get a pathway and 'reuse fires' would conflate 'reuse doesn't
        help' with 'we never managed to distill'. Retry isolates the former."""
        d = tempfile.mkdtemp(prefix="daisugi-warm-")

        async def _attempt():
            dai = Daisugi(model=model, data_dir=d, tier1=tier1)
            r = await dai.orchestrate(task["prompt"], envelope=envelope, stakes="low")
            if r.status == "succeeded":
                await dai.tend(min_traces=1)
                return dai.pathway_store.find(task["prompt"], threshold=0.0) is not None
            return False

        for _ in range(tries):
            try:
                if asyncio.run(asyncio.wait_for(_attempt(), timeout=180)):
                    return d
            except Exception as exc:
                if verbose:
                    print(f"  prime {task['id']} attempt: {type(exc).__name__}: "
                          f"{str(exc)[:80]}", file=sys.stderr)
        if verbose:
            print(f"  prime {task['id']}: no pathway after {tries} tries",
                  file=sys.stderr)
        return d

    def runner(task, *, warm, seed):
        # Isolation is load-bearing: every Daisugi() otherwise shares the
        # DEFAULT on-disk pathway store, so cold runs would see warm's
        # distilled pathways and the comparison would be meaningless. Cold gets
        # a fresh empty store (never tended → nothing to reuse); warm reuses a
        # store primed once per task.
        if warm:
            data_dir = warm_dirs.get(task["id"])
            if data_dir is None:
                data_dir = _prime(task)
                warm_dirs[task["id"]] = data_dir
        else:
            data_dir = tempfile.mkdtemp(prefix="daisugi-cold-")

        dai = Daisugi(model=model, data_dir=data_dir, tier1=tier1)

        async def _go():
            tally.reset()
            t0 = time.monotonic()
            res = await dai.orchestrate(task["prompt"], envelope=envelope, stakes="low")
            dt = (time.monotonic() - t0) * 1000
            if warm:
                stats["warm_total"] += 1
                if res.reused_pathway:
                    stats["warm_reused"] += 1
            return _metric(res, tally.total, dt)

        try:
            return asyncio.run(asyncio.wait_for(_go(), timeout=180))
        except Exception as exc:  # timeout, ollama restart, decompose blowup
            if verbose:
                print(f"  drop {task['id']} warm={warm} seed={seed}: "
                      f"{type(exc).__name__}: {str(exc)[:100]}", file=sys.stderr)
            return None

    return runner, stats


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--full", action="store_true", help="Run the ≥20×5 bar (heavy).")
    ap.add_argument("--model", default=None, help="Override model (default: resident).")
    ap.add_argument("--repeats", type=int, default=None)
    ap.add_argument("--cwd", default=os.getcwd())
    args = ap.parse_args()

    model = args.model or _resident_model() or "ollama_chat/qwen3:4b-instruct"
    tasks = _TASKS if args.full else _TASKS[:3]
    repeats = args.repeats or (5 if args.full else 3)

    print(f"model={model}  tasks={len(tasks)}  repeats={repeats}")
    runner, stats = make_runner(model, cwd=args.cwd, verbose=True)
    results = run_paired_benchmark(tasks, runner, repeats=repeats)
    s = summarize(results)

    print("\n=== gates ===")
    reuse_rate = (stats["warm_reused"] / stats["warm_total"]) if stats["warm_total"] else 0.0
    print(f"gate 1 — reuse fires on warm: {stats['warm_reused']}/{stats['warm_total']} "
          f"({reuse_rate:.0%})  {'PASS' if reuse_rate > 0.5 else 'FAIL — warm≈cold, void'}")
    print(f"gate 2 — cold success rate: {s['cold_success_rate']:.0%}  "
          f"{'PASS' if s['cold_success_rate'] > 0 else 'FAIL — no cost story, outcome only'}")

    print("\n=== results ===")
    print(f"cost_delta_tasks (paired successes): {s['cost_delta_tasks']}")
    print(f"token delta (warm−cold, successes):  {s['token_delta_mean']}  ci95={s['token_delta_ci95']}")
    print(f"latency delta ms (successes):        {s['latency_delta_mean']}  ci95={s['latency_delta_ci95']}")
    print(f"success rate cold/warm:              {s['cold_success_rate']:.0%} / {s['warm_success_rate']:.0%}")
    print(f"safety regression:                   {s['safety_regression']}")
    print(f"meets Stage-4 bar (≥20×≥5):          {meets_stage4_bar(results)}")
    return 0


if __name__ == "__main__":
    if os.environ.get("DAISUGI_CLAUDE_CODE_INTEGRATION") != "1":
        print("Set DAISUGI_CLAUDE_CODE_INTEGRATION=1 to run the live pilot "
              "(it drives a local Ollama model). Needs Ollama up on :11434.")
        sys.exit(1)
    sys.exit(main())
