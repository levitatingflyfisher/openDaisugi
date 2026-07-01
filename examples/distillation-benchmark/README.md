# Distillation-fidelity benchmark — live pilot (roadmap Stage 4)

The [Stage-4 ruler](../../src/opendaisugi/benchmark.py) wired to a local Ollama
model. `run_pilot.py` runs each task COLD (fresh decompose) vs WARM (a distilled
pathway is reused, skipping the decompose) and reports the two-denominator
summary: cost deltas over successful runs, outcome delta over all runs, and the
safety-direction check.

## Before you run it

Two gates the pilot checks (a full run is pointless unless both pass):
1. **Reuse fires** on warm runs (else warm == cold and the experiment measures
   nothing).
2. **Cold clears decompose+verify** a real fraction of the time (else there is
   an outcome story but no cost story).

## Resource warning

Small local models on a memory-constrained box are slow and swap-thrash. This
runner defaults to the model already **resident** in Ollama (avoiding an
evicting swap) and drops a run on timeout / server restart rather than
corrupting the numbers. **Do not run the full `--full` bar concurrently with
another benchmark** (e.g. deseretBench) on the same constrained box — they will
evict each other's models and can OOM. Run it when the box is free.

```bash
DAISUGI_CLAUDE_CODE_INTEGRATION=1 python run_pilot.py          # 3×3 pilot
DAISUGI_CLAUDE_CODE_INTEGRATION=1 python run_pilot.py --full   # 20×5 (heavy)
```

Status: the runner is written and the harness it drives is unit-tested, but the
end-to-end live numbers have **not** been produced yet — that awaits a free box.
No numbers are claimed until this pilot's two gates pass on a real run.
