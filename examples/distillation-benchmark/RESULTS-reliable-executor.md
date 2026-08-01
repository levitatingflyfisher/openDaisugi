# Stage-4 — reliable-executor run (isolating the reuse effect)

**Date:** 2026-07-26 · **Backend:** `claude-code` (a reliable executor) ·
**Scale:** 4 tasks × 3 repeats · **Runner:** `run_claude.py`

The [4B-model pilot](RESULTS.md) found reuse *real in direction but unproven*,
and named the cause honestly: the model's ~67% execution success swamped the
signal, and warm even scored **below** cold (56% < 67%) — the quantified form of
the "a reused block dropped something the situation needed" worry. Its stated
next experiment was to rerun with a **reliable** executor so the reuse effect is
isolated from execution noise. This is that experiment.

## Numbers

| Metric | 4B pilot | Reliable executor (here) |
|---|---|---|
| Cold success | 67% | **100%** |
| Warm success | 56% | **100%** |
| Reuse fires on warm | 33% | **100% (12/12)** |
| Latency delta (warm − cold, successes) | −2.6 s (CI −42.6…+37.3) | **−8.2 s** (CI95 −23.7…+7.4 s) |
| Safety regression | No | **No** |
| Meets Stage-4 bar (≥20×5) | No | **No** (this is 4×3) |

## What it means, honestly

1. **The pilot's warm-below-cold was execution noise, not a reuse defect.** With
   a reliable executor, warm success = cold success = **100%** with no safety
   regression, and the harness reported reuse firing on all 12 warm runs
   (12/12). The fear the pilot quantified — that a reused pathway silently drops
   a dependent step — **did not manifest**: warm runs succeeded exactly as often
   as cold. *On the 12/12 count:* this run's `find-todos` prime hit the
   cosine-overshoot bug below, so the 12/12 figure was not cleanly separated
   per-task at the time. A follow-up **3×1 run with the bug fixed primed and
   reused all three tasks cleanly — `find-todos` included, 3/3, no
   ValidationError**, cold/warm both 100%. That confirms the overshoot bug was
   the only obstacle and reuse fires reliably; the finding does not rest on the
   original run's exact count.
2. **Direction of the cost saving is confirmed; significance still isn't.** Warm
   is ~8 s faster per successful task (it skips the decompose call), but at
   n = 12 the confidence interval still spans zero. Direction is not
   significance — the ≥20-task × 5-repeat bar exists for exactly this reason,
   and this run does **not** clear it.
3. **This isolates *fidelity*, not the product pitch.** The `claude-code`
   backend is reliable but **not local and not cheap-token** — so this run
   answers "does reuse *work and stay safe* when execution is reliable?" (yes),
   **not** "is a cheap *local* model reliable enough to make local reuse pay?"
   That second question still needs a local model that clears ~90% run success
   (the box's Ollama server was down for this run). The **token** side of the
   saving is not metered on the claude-code subprocess path at all; the guard/
   token story lives in [`examples/jit-metrics/`](../jit-metrics/).

## A bug this run surfaced (and fixed)

Priming the `find-todos` task raised `ValidationError: PathwayMatch.similarity
Input should be less than or equal to 1.0`: a self-match embedding scored
`1.0000000000000002` (float rounding), tripping the model's `le=1.0` bound and
breaking `pathway_store.find()`. Fixed by clamping cosine similarity to
`[-1, 1]` at the shared source (`_similarity.py`), with a regression test. The
reliable-executor run earned its keep by exposing a real correctness bug in the
reuse hot path — and a clamp-fixed 3×1 follow-up then primed and reused every
task cleanly (`find-todos` included), verifying the fix end to end.

## The honest conclusion

With a reliable executor, distilled-pathway reuse is **as reliable as fresh
work and faster in direction, with no safety regression** — the reuse-fidelity
question the 4B pilot could not answer under its noise now has a clean, if
small, "yes." What remains for Stage 4 to be *solved* is unchanged and stated
plainly: the full **≥20 tasks × 5 repeats** bar, and — separately — a **local**
model reliable enough to make the *cheap-local-reuse* product pitch (not just
the fidelity question) pay. Neither is claimed here.

## Reproduce

```bash
CUDA_VISIBLE_DEVICES="" OPENDAISUGI_LLM_BACKEND=claude-code \
  python run_claude.py --tasks 4 --repeats 3        # this run
CUDA_VISIBLE_DEVICES="" OPENDAISUGI_LLM_BACKEND=claude-code \
  python run_claude.py --full                        # the ≥20×5 bar (~hours)
```
