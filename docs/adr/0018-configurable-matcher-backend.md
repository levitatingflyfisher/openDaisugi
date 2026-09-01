# ADR-0018 — A configurable, torch-free pathway-reuse embedder (potion)

- **Status:** Accepted. Superseded in part by ADR-0019 (lexical is now built).
- **Date:** 2026-08-28

## Context

The `matcher_model` config field existed, the swap menu let you pick between
`all-MiniLM-L6-v2`, `potion`, `int8`, and `lexical`, and `daisugi modules` drew all
four — but nothing read the field. The pathway embedder (used by both matcher reuse in
`PathwayStore.find` and cluster distillation in `Distiller.tend`) was hard-wired to
`all-MiniLM-L6-v2` via `_search._MODEL_NAME`. That single constant did three jobs at
once: it loaded the model, it stamped `embedding_model` on every distilled pathway, and
it was the value the stale-embedding guard compared against.

`all-MiniLM-L6-v2` comes through `sentence-transformers`, whose `[search]` extra pulls
an ~80 MB torch slice. On a machine that cannot run that torch build — a GPU torch
cannot target (this box's Pascal GTX 1060 crashes it), or a machine too small for
torch — distillation caught the ImportError and degraded to a warned zero-pathway
report. So the recorded preference `matcher_model: potion` did nothing, and the only
"no-torch" outcome was zero pathways. The swap menu recording a value nothing reads is
also a quiet violation of the tool's own honesty rule.

## Decision

1. **An embedder abstraction in `_search`.** `_get_model()` returns a cached adapter with
   a uniform `encode(texts) -> np.ndarray`, L2-normalized, keyed by backend identity.
   `active_model_name()` and `active_threshold()` resolve the backend from config
   **without loading a model**, so provenance and the stale guard can name the active
   embedder cheaply. Config resolves through `DEFAULT_DATA_DIR` (which the test suite
   already isolates), keeping selection hermetic.

2. **A torch-free `potion` backend via model2vec** (numpy + tokenizers; its base install
   pulls neither torch nor transformers — verified). Default model `potion-base-8M`
   (measured 30.7 MB on disk, dim 256). Override the concrete model — a HF id, or a local
   directory for air-gapped `HF_HUB_OFFLINE=1` use — with `OPENDAISUGI_POTION_MODEL`
   (the save→offline-load round-trip is verified, so "offline" is earned, not asserted).

3. **A per-backend threshold, derived not guessed.** On the real 740-trace journal
   corpus, `all-MiniLM-L6-v2 @ 0.55` has a 2.56 % false-match rate on random task pairs.
   The potion threshold that matches that precision is **0.59** (`_POTION_THRESHOLD`).

4. **Identity flows to provenance.** The distiller stamps `active_model_name()`, and
   `find` compares against it, so potion-embedded pathways are labeled
   `minishlab/potion-base-8M` and MiniLM-embedded rows go stale (excluded until re-`tend`)
   when the backend changes — the existing migration mechanism, now correct across
   backends. A **dimension guard** in `find` drops any admitted row whose stored vector
   width differs from the query's (a legacy 384-dim MiniLM row under a 256-dim potion
   query would otherwise crash the cosine dot product, and `find` catches only
   ImportError).

5. **Unbuilt backends refuse honestly.** `int8` and `lexical` are placeholders. Selecting
   one is refused at the swap (`MatcherNotAvailable`, before any config write — no trap),
   and `active_model_name()` raises the same rather than silently stamping MiniLM.

## Consequences

- `matcher_model: potion` now produces and reuses pathways on a machine that cannot run
  torch. potion covers three of the four ways the MiniLM path fails: `[search]` absent, a
  GPU torch cannot target, and too-small-for-torch. It does **not** cover a no-network
  machine on first use — only a pre-fetched `OPENDAISUGI_POTION_MODEL=<local dir>` does.
- potion is a **weaker matcher** than MiniLM — the honest cost of static token
  embeddings. At the FPR-matched thresholds (potion 0.59, MiniLM 0.55) its paraphrase
  recall is ~0.31 vs MiniLM's 0.39 on the same labeled set — modestly weaker, not a cliff.
  It reuses near-duplicate recurring tasks perfectly (cosine ≈ 1.0), the dominant and safe
  reuse case, and is precision-leaning by design because a false reuse runs the wrong plan
  (caught by re-verification, but it "looks like success").
- Switching backends requires a `daisugi tend` to re-embed; the store already warns when
  ≥10 % of rows are cross-model stale.

## Alternatives considered

- **`potion-retrieval-32M` as the default.** Purpose-built for retrieval and a cleaner
  real-world baseline, but at the FPR-matched threshold it gave **half** the recall of
  `potion-base-8M` (0.17 vs 0.31) for **4×** the download (130 MB vs 31 MB). Baseline
  inflation was a red herring once the threshold is FPR-matched. Kept as an override.
- **`lexical` / `int8` now.** Out of scope. `lexical` (keyword overlap, zero model, works
  fully offline) is the obvious next backend; left honestly marked `POSSIBLE`.
- **Bundling a potion model in the wheel.** Rejected for now — keeps the wheel small; the
  local-dir override is the air-gapped path.
