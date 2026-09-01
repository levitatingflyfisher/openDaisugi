# ADR-0021: The `int8` matcher, MiniLM without torch, through onnxruntime

- **Status:** Accepted
- **Date:** 2026-09-08

## Context

ADR-0018 shipped `potion`, a model2vec embedder with no torch. ADR-0019
shipped `lexical`, a stdlib and numpy embedder with no model at all. Both are
alternatives to `all-MiniLM-L6-v2` for pathway-reuse embedding. `int8` was
the last placeholder in the swap menu. `_UNBUILT_MATCHERS` refused it, and
`daisugi modules` marked it `POSSIBLE`, planned and not wired.

Unlike potion and lexical, int8 is not a different model family. It is MiniLM
itself, quantized to 8-bit integers and exported to ONNX by the official
sentence-transformers repository, run through `onnxruntime` instead of
`sentence-transformers` and torch. That makes it the closest torch-free
approximation of the default backend's own embedding space. This line has
learned twice that close is not the same. A store whose rows say
`all-MiniLM-L6-v2` but hold int8 vectors would compare against the wrong
space.

## Decision

1. **A pinned manifest, not a live lookup.** `_INT8_FILES` in `_search.py`
   records four ONNX builds, `avx512_vnni`, `avx512`, `avx2`, and `arm64`,
   plus `tokenizer.json`, each with its exact size and sha256. The manifest
   was pinned on 2026-09-08 from
   `https://huggingface.co/api/models/sentence-transformers/all-MiniLM-L6-v2/tree/main/onnx`.
   The API reports the LFS sha256 for the onnx files directly.
   `tokenizer.json` sits below the LFS threshold, so its sha256 was computed
   from the downloaded file, not from the git-blob id the API reports for
   small files. Three of the four onnx builds, `vnni`, `avx512`, and `arm64`,
   were byte-identical on the day of the pin. The CPU-feature dispatch still
   names all four, because the upstream files can diverge later.

2. **CPU-feature dispatch. Refuse off the ladder.** `_detect_int8_variant`
   reads the `flags` line of `/proc/cpuinfo`, `avx512_vnni` selects vnni,
   `avx512f` selects avx512, `avx2` selects avx2, most capable first. On
   `platform.machine()` `aarch64` or `arm64` it selects the arm64 build. A CPU
   with none of these, an older x86 or a host with no `/proc/cpuinfo` such as
   macOS or Windows, raises `MatcherNotAvailable`. The message names potion
   and lexical as the alternatives that have no such floor.

3. **A generic, resumable, verified fetch.**
   `opendaisugi._model_fetch.fetch(url, sha256, dest)` is new general-purpose
   infrastructure. It short-circuits when `dest` already matches the hash,
   resumes a partial `.part` file with an HTTP Range request, verifies the
   sha256 of the complete file before it renames the file into place, and
   deletes and raises on a mismatch so the next call re-fetches clean. It is
   not specific to int8. Nothing else needs a second caller today, and
   nothing in it assumes one.

4. **The same pipeline sentence-transformers applies for this model.**
   `_Int8Embedder` tokenizes with `tokenizers.Tokenizer.from_file`, truncates
   at 256 and pads dynamically, runs the onnxruntime session with
   `input_ids`, `attention_mask`, and `token_type_ids`, mean-pools
   `last_hidden_state` over the attention mask, and L2-normalizes. Batch size
   is 32. At load, the embedder reads the session's first output by name
   and refuses unless it has rank 3, so a pooled export can never be
   mean-pooled twice. Verified against the real `model_quint8_avx2.onnx`
   build: output shape `(batch, 384)`, norm 1.0. The four tests marked
   `_NEEDS_REAL_INT8` in `tests/test_matcher_backend.py` re-verify that
   claim on any box whose int8 cache holds the pinned files; they skip
   elsewhere and never fetch.

5. **A threshold derived on the real journal, not guessed.** The command is:

   ```
   uv run --no-sync python scripts/matcher_fpr.py
   ```

   It ran on 2026-09-08 against `~/.opendaisugi/journal/index.db`,
   20,000 random distinct-task pairs, seed 42, the
   same corpus and method ADR-0018 and ADR-0019 used, at the 2.56% target
   FPR. It printed:

   ```
   === all-MiniLM-L6-v2-int8 ===
     thr=0.15  fpr=0.4223
     thr=0.20  fpr=0.2669
     thr=0.25  fpr=0.1618
     thr=0.30  fpr=0.0983
     thr=0.35  fpr=0.0619
     thr=0.40  fpr=0.0416
     thr=0.45  fpr=0.0278
     thr=0.50  fpr=0.0182
     thr=0.55  fpr=0.0114
     thr=0.59  fpr=0.0077
     thr=0.65  fpr=0.0044
     thr=0.70  fpr=0.0031
   -> closest to target FPR 0.0256: thr=0.45
   ```

   `_INT8_THRESHOLD = 0.45`, measured on the `avx2` build, the one this
   box's CPU selects. The three byte-identical builds measure the same. On
   the near-duplicate and unrelated pairs the lexical test uses: "clean up
   stale tmp files in the build directory" against "remove old temporary
   files from the build folder" scores 0.771, which clears 0.45. "rebuild
   the android apk" against "write the ADR for the gateway" scores 0.127,
   well under. An operator re-derives the table with the command above. A
   different journal prints a different table. Record the new
   `-> closest to target FPR` line here and in `_INT8_THRESHOLD` together.

6. **Package absence falls back to lexical. Everything else refuses.**
   `_FALLBACK_PACKAGE["int8"] = ("onnxruntime", "opendaisugi[int8]")`, the
   same rule ADR-0019 established for MiniLM and potion. A CPU with no
   build, a fetch failure, or a downloaded file that does not verify are not
   fallback cases. The operator chose int8, so `_Int8Embedder` raises
   `MatcherNotAvailable` with the fix, `OPENDAISUGI_INT8_MODEL` or a switch
   to lexical, instead of a silent downgrade that mislabels provenance.

7. **`_UNBUILT_MATCHERS` is now empty.** int8 was the only member. The
   identity-layer and swap-layer refusal tests that existed only to prove
   int8 refused, `test_unbuilt_backends_refuse_at_identity`,
   `test_swap_refuses_unbuilt_matcher_without_writing_config`, and
   `test_swap_confirmation_reports_refusal_without_crashing`, are deleted.
   No unbuilt backend is left to exercise them against. The refusal code
   path in `apply_swap` stays, dormant, for the next placeholder.

## Consequences

- `matcher_model: int8` now produces and reuses pathways on a machine that
  cannot run torch and does not want potion's static-embedding ceiling.
  int8 is a real neural embedding, quantized, under a lighter runtime.
- int8 is CPU-feature-gated in a way potion and lexical are not. A pre-2013
  x86 CPU with no AVX2, or a host with no `/proc/cpuinfo`, has no int8 build
  and must use potion or lexical instead.
- The 23MB fetch happens once and is cached under
  `~/.cache/opendaisugi/models/minilm-int8/`. `OPENDAISUGI_INT8_MODEL`
  points at a pre-fetched directory for air-gapped installs, the same as
  potion's `OPENDAISUGI_POTION_MODEL`.
- A switch to or from int8 needs a `daisugi tend` to re-embed, per the
  existing stale-embedding guard.

## Alternatives considered

- **fp16 instead of or beside int8.** Out of scope. fp16 needs a GPU
  execution provider to pay for itself. On CPU it is not smaller or faster
  than int8 in a way that earns a second identity yet.
- **GPU execution providers, CUDA or DirectML.** Out of scope. onnxruntime
  supports them, but a Pascal GPU (sm_61) is the case ADR-0018 named as
  unable to run the torch build either. CPU-only keeps int8's value
  proposition honest: it runs where torch cannot.
- **Bundling the model in the wheel.** Rejected for the reason ADR-0018
  rejected it for potion. It keeps the wheel small. `OPENDAISUGI_INT8_MODEL`
  is the air-gapped path.
