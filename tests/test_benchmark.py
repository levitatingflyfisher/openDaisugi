"""Tests for the distillation-fidelity benchmark harness (roadmap Stage 4).

Stage 4 deliberately waits on real tool-using transcripts for its *numbers*,
and on at least one local model. This harness is the *ruler* — the seeded,
content-addressed paired-run machinery, its confidence-interval math, and the
safety-direction check — verified here with a deterministic fake runner so the
scaffold is trustworthy before any live model is wired in.
"""

from __future__ import annotations

from opendaisugi.benchmark import (
    RunMetric,
    meets_stage4_bar,
    run_paired_benchmark,
    summarize,
    tasks_hash,
)


def _fake_runner_warm_is_cheaper(task, *, warm, seed):
    """Deterministic: warm (pathway-reuse) runs spend fewer tokens and less
    time, same outcome, and never add denials/violations."""
    base_tokens = 1000 + (seed * 10)
    base_ms = 500.0 + seed
    if warm:
        return RunMetric(tokens=int(base_tokens * 0.6), latency_ms=base_ms * 0.7,
                         success=True, denials=0, violations=0)
    return RunMetric(tokens=base_tokens, latency_ms=base_ms, success=True,
                     denials=0, violations=0)


def _tasks(n):
    return [{"id": f"t{i}", "prompt": f"do thing {i}"} for i in range(n)]


def test_paired_benchmark_runs_cold_and_warm_repeats():
    results = run_paired_benchmark(_tasks(3), _fake_runner_warm_is_cheaper, repeats=5)
    assert len(results) == 3
    for r in results:
        assert len(r.cold) == 5
        assert len(r.warm) == 5


def test_seeds_are_deterministic_and_distinct_per_repeat():
    seen = {}

    def _spy(task, *, warm, seed):
        seen.setdefault(task["id"], []).append(seed)
        return RunMetric(tokens=1, latency_ms=1.0, success=True)

    run_paired_benchmark(_tasks(1), _spy, repeats=4, base_seed=100)
    # 4 cold + 4 warm, seeds deterministic and paired (cold/warm share a seed
    # so the only difference is pathway warmth, not the seed).
    assert seen["t0"] == [100, 101, 102, 103, 100, 101, 102, 103]


def test_summary_reports_token_and_latency_deltas_with_ci():
    results = run_paired_benchmark(_tasks(4), _fake_runner_warm_is_cheaper, repeats=5)
    s = summarize(results)
    # warm spends ~40% fewer tokens → negative delta (warm - cold).
    assert s["token_delta_mean"] < 0
    assert s["latency_delta_mean"] < 0
    # A confidence interval is present and brackets the mean.
    lo, hi = s["token_delta_ci95"]
    assert lo <= s["token_delta_mean"] <= hi


def test_summary_reports_outcome_rates():
    results = run_paired_benchmark(_tasks(3), _fake_runner_warm_is_cheaper, repeats=5)
    s = summarize(results)
    assert s["cold_success_rate"] == 1.0
    assert s["warm_success_rate"] == 1.0


def test_safety_direction_flags_warm_increasing_denials():
    def _unsafe_warm(task, *, warm, seed):
        # Warm runs attempt MORE denied actions — the regression Stage 4 must catch.
        return RunMetric(tokens=100, latency_ms=10.0, success=True,
                         denials=(2 if warm else 0))

    results = run_paired_benchmark(_tasks(3), _unsafe_warm, repeats=5)
    s = summarize(results)
    assert s["safety_regression"] is True
    assert s["warm_denials_total"] > s["cold_denials_total"]


def test_safety_direction_clean_when_warm_no_worse():
    results = run_paired_benchmark(_tasks(3), _fake_runner_warm_is_cheaper, repeats=5)
    s = summarize(results)
    assert s["safety_regression"] is False


def test_tasks_are_content_addressed():
    h1 = tasks_hash(_tasks(3))
    h2 = tasks_hash(_tasks(3))
    assert h1 == h2 and len(h1) == 16
    assert tasks_hash(_tasks(4)) != h1


def test_stage4_bar_requires_enough_tasks_and_repeats():
    small = run_paired_benchmark(_tasks(3), _fake_runner_warm_is_cheaper, repeats=5)
    assert meets_stage4_bar(small) is False  # < 20 tasks
    big = run_paired_benchmark(_tasks(20), _fake_runner_warm_is_cheaper, repeats=5)
    assert meets_stage4_bar(big) is True
    thin = run_paired_benchmark(_tasks(20), _fake_runner_warm_is_cheaper, repeats=3)
    assert meets_stage4_bar(thin) is False  # < 5 repeats


def test_runner_returning_none_is_dropped_not_crash():
    def _flaky(task, *, warm, seed):
        if seed % 2 == 0:
            return None  # a failed/aborted run
        return RunMetric(tokens=100, latency_ms=10.0, success=True)

    results = run_paired_benchmark(_tasks(1), _flaky, repeats=4)
    # None runs are dropped; the harness doesn't crash and records what ran.
    assert len(results[0].cold) + len(results[0].warm) < 8
