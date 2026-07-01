# Stage-4 pilot — first real distillation-fidelity numbers

**Date:** 2026-07-25 · **Model:** `qwen3:4b-instruct` (local, Ollama, CPU
embedder) · **Scale:** 3 tasks × 3 repeats (a *pilot*, not the ≥20×5 bar).

This is the first time the distillation-fidelity question — *does reusing a
distilled pathway actually pay?* — has been run on real tool-using agentic
transcripts with a local model, rather than asserted. It is a pilot: the job is
to see whether the two gates are passable and what the effect looks like, not
to settle the question. It is reported here whether or not it flatters (it
doesn't, much).

## Numbers

| Metric | Value |
|---|---|
| Cold run success | 67% |
| Warm run success | 56% |
| Reuse fires (warm runs that reused a pathway) | 33% |
| Token delta (warm − cold, successful pairs) | **−632** (CI95 −8873…+7609) |
| Latency delta (successful pairs) | **−2.6 s** (CI95 −42.6…+37.3 s) |
| Safety regression (warm attempts more denials?) | No |
| Meets Stage-4 bar (≥20×5) | No (pilot) |

## What it means, honestly

1. **The pipeline works end to end.** A cold run decomposes → executes →
   synthesizes and succeeds ~2/3 of the time; distillation produces a pathway;
   a warm run reuses it. The machinery is real, not a mock.
2. **Direction favors distillation, but it is not proven.** Warm is cheaper in
   both tokens (−632) and latency (−2.6 s) — consistent with skipping the
   decompose call. But the confidence intervals span zero by a wide margin.
   At 3×3 this is underpowered by design; *direction is not significance.*
   This is exactly why the roadmap sets a ≥20-task ×5-repeat bar.
3. **The limiting factor is small-model execution reliability, not the reuse
   code.** Reuse fires only 33% because a pathway only exists once a run
   succeeds, and success is ~50–67% with a 4B model. Split by task: `list-py`
   runs and distills cleanly (its failures were stochastic); `find-todos`
   reliably *fails to execute* and so never distills. When a run succeeds,
   distill → reuse works and is cheaper; the model's unreliability swamps the
   signal.
4. **Reuse is slightly *less* reliable than fresh work** (warm 56% < cold 67%).
   A distilled pathway sometimes yields a plan that breaks. This is the
   quantified form of the "standard block looked reusable but dropped something
   the situation needed" failure — an aerospace-embedded-systems warning
   (thanks, Uncle Jim) reproduced empirically.

## The honest conclusion at this scale

With a 4B local model, distillation's savings are **real in direction but
unproven**, and the whole measurement is dominated by the model's execution
noise. The effort of distillation is *not clearly justified at this scale* —
which is a genuine result, not a null one, and it matches the prior skepticism.
The thesis most plausibly holds better with a **more reliable (larger) local
model** — less execution noise per run means higher distillation yield and a
cleaner cost signal — which is the obvious next experiment. Running the full
≥20×5 with *this* model would just be a bigger, equally noise-dominated version
of the same pilot; it is deliberately deferred until a model that clears ~90%
run success is wired in.

## Reproduce

```bash
CUDA_VISIBLE_DEVICES="" DAISUGI_CLAUDE_CODE_INTEGRATION=1 \
  python run_pilot.py --model ollama_chat/qwen3:4b-instruct        # 3×3 pilot
```

## Bugs found and fixed while building this

- **`executor._resolved_path_escape` root-base bug** — a `/**` (or `**`) grant
  passed plan-time `verify()` but the executor *always* refused it as a bogus
  "symlink escape" (base resolved to `/`, and `startswith("//")` matches
  nothing). A plan-time/run-time inconsistency; fixed with a regression test.
- **`benchmark.summarize` stratification** — cost deltas were pooled over all
  runs, so a cold run that fails cheaply at planning dragged the cold mean down
  and *understated* the saving. Split into two denominators: cost over
  successful runs, outcome over all.
