# jit-metrics — measuring the "JIT compiler for AI inference" claim

openDaisugi is, structurally, a JIT compiler for agent inference: an interpreter
(full decompose→execute→synthesize), a hotness gate (`min_traces`), a compile
step (the distiller), a code cache (the pathway store), a guard (Z3 `verify`),
and deopt (fall-through to a fresh decompose). This directory is the **ruler**
for the two claims that framing makes measurable — deterministic, no LLM, no
network:

1. **Is checking orders of magnitude cheaper than doing?** (Runtime assurance's
   founding assumption.) → `verify` latency percentiles + the fraction of
   envelopes whose guard is purely symbolic (needs no LLM).
2. **What fraction of interactions are compilable?** → over *all* interactions
   (the pessimistic denominator), how many land in a distillable cluster and
   still pass the guard — reported as an explicitly-labelled upper bound.

```bash
python examples/jit-metrics/measure_corpus.py                 # your ~/.opendaisugi journal
python examples/jit-metrics/measure_corpus.py --data-dir DIR  # a specific corpus
python examples/jit-metrics/measure_corpus.py -o RESULTS.md   # write the markdown
```

The measurement logic lives in `opendaisugi.jit_metrics` (unit-tested in
`tests/test_jit_metrics.py`); this script is the thin corpus-replay wrapper.
[`RESULTS.md`](RESULTS.md) is a committed run over the maintainer's benchmark
corpus — with its composition disclosed, because that corpus is test-generated
and not representative of organic agent diversity. Point the ruler at your own
journal for a number that means something for your workload.
