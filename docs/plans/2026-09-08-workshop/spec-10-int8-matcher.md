# Spec 10 — The int8 matcher: MiniLM without torch

**Master:** §5.10 · **Size:** S · **Depends on:** nothing · **Lineage:** ADR-0018, ADR-0019

## Purpose

Fill the last placeholder in the matcher line. onnxruntime plus the `tokenizers` library, a
23 MB quantised MiniLM from the official sentence-transformers repository, no torch, one-time
download with a pinned hash. After this, `_UNBUILT_MATCHERS` is empty.

## The crux

Quantised embeddings are about 95% similar to the float ones. That is "close", and close is not
"same": a store whose rows say `all-MiniLM-L6-v2` but hold int8 vectors would match differently
from what its provenance claims. So int8 is its own identity with its own FPR-derived threshold,
and the fallback rule is the one ADR-0019 set: a *missing package* falls back to lexical with one
warning; a *missing or failed model* refuses and teaches the fix.

## Files

```
src/opendaisugi/_search.py           # _Int8Embedder; _INT8_IDENTITY, _INT8_DIM=384, _INT8_THRESHOLD; _FALLBACK_PACKAGE["int8"]
src/opendaisugi/_model_fetch.py      # fetch(url, sha256, dest) with resume + verify; env override; offline error
src/opendaisugi/swap.py              # _UNBUILT_MATCHERS = frozenset(); option desc "quantized MiniLM, ~23MB, no torch"
src/opendaisugi/modules.py           # int8 AVAILABLE/ACTIVE with the real reason
src/opendaisugi/config.py            # comment: matcher_model: all-MiniLM-L6-v2 | potion | lexical | int8
scripts/matcher_fpr.py               # add the int8 backend
pyproject.toml                       # [int8] = onnxruntime>=1.18, tokenizers>=0.19, numpy>=1.26
tests/test_matcher_backend.py        # the 28 tests parameterised over int8 where applicable + int8-specific
docs/adr/0021-int8-matcher.md        # short: identity, threshold, files, fallback
```

## Model files

From `https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/`:
`onnx/model_qint8_avx512_vnni.onnx`, `onnx/model_qint8_avx512.onnx`, `onnx/model_quint8_avx2.onnx`,
`onnx/model_qint8_arm64.onnx`, plus `tokenizer.json`. Plan task 1 records each file's sha256 in
`_search.py` as `_INT8_FILES`. Selection: read `/proc/cpuinfo` flags (`avx512_vnni` → vnni;
`avx512f` → avx512; `avx2` → quint8_avx2; `aarch64` → arm64; else refuse with "no int8 build for
this CPU; use potion or lexical"). Cache dir `~/.cache/opendaisugi/models/minilm-int8/`; env
`OPENDAISUGI_INT8_MODEL=<dir>` points at a pre-fetched directory (offline installs). Download
only on first use, with a one-line notice naming the URL and size; never at import.

## Embedder

`encode(texts, convert_to_numpy=True)`: tokenize with `tokenizers.Tokenizer.from_file`
(truncate 256, pad), run the session with `input_ids`, `attention_mask`, `token_type_ids`,
mean-pool over the mask, L2-normalise — the same pipeline sentence-transformers applies for this
model. Batch size 32. Identity `all-MiniLM-L6-v2-int8`, dim 384. Threshold `_INT8_THRESHOLD`
set by running `scripts/matcher_fpr.py` at the 2.56% FPR target the line uses and recorded in
ADR-0021 with the command that produced it.

## Tests

- Shape, repeatability, L2 norm, determinism across processes (the lexical test set, reused).
- `test_int8_identity_and_threshold`; `test_int8_paraphrase_clears_threshold_unrelated_does_not`
  (the 20-pair set from ADR-0019, skip if the model is not cached and no network).
- Fallback: package absent → lexical with one warning; model dir missing offline →
  `MatcherNotAvailable` naming `OPENDAISUGI_INT8_MODEL`; a corrupted file (bad sha) → refused,
  file deleted, re-fetch on next call.
- `test_int8_never_imports_torch` (sys.modules check after encode).
- swap allows int8; modules map marks it honestly; `_UNBUILT_MATCHERS` is empty and the
  refusal test is deleted with a commit message saying why.

## Out of scope

fp16; GPU execution providers; re-embedding existing stores (tend handles orphans per the ADR-0019 note).
