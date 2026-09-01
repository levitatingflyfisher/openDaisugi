# ADR-0019 — The `lexical` matcher: a zero-model, zero-download floor

- **Status:** Accepted
- **Date:** 2026-09-07

## Context

ADR-0018 shipped `potion` (model2vec, torch-free) as an alternative to
`all-MiniLM-L6-v2` for pathway-reuse embedding. potion runs where torch can't,
but it still downloads a ~30 MB model from Hugging Face on first use. A
machine with no network on first run — the project's own stated goal, "a
fresh `pip install opendaisugi` on an offline machine must distill pathways"
— gets zero pathways from either backend, exactly the failure ADR-0018 named
under "Consequences" and left unclosed: `matcher_model: lexical` was a swap
menu entry that refused honestly rather than doing anything.

Separately, ADR-0018's own honesty gap persisted one level up: selecting
`potion`/`all-MiniLM-L6-v2` when the package is not installed still degrades
silently to a warned zero-pathway `tend` — the swap menu records a choice
nothing can act on, on a box that never installed the extra.

## Decision

1. **A signed-feature-hashing embedder, pure stdlib + numpy.** Tokenize
   lowercase `[a-z0-9_]+` runs, drop a small stopword list, hash each
   surviving token with `blake2b` (not Python's per-process-salted `hash()` —
   cross-process determinism is a contract) into one of 4096 dims with a sign
   from the hash's top bit, weight by `sqrt(term count)`, L2-normalize.
   `_LexicalEmbedder` never imports model2vec, sentence-transformers,
   huggingface_hub, or transformers, and never touches the network — proved
   by an end-to-end test with those imports blocked at `builtins.__import__`.

   Measured 2026-09-07 on a real local journal (`~/.opendaisugi/journal/index.db`),
   20,000 random distinct-task pairs, plus a 20-pair hand-labeled paraphrase set. FPR target
   = 2.56% (the MiniLM@0.55 rate ADR-0018 matched potion to):

   | featurization                     | thr @ 2.56% FPR | paraphrase recall |
   |-----------------------------------|------------------|--------------------|
   | unigram + bigram, no stopwords    | 0.194            | 0.60               |
   | unigram + bigram, stopwords       | 0.159            | 0.65               |
   | **unigram only, stopwords**       | **0.250**        | **0.70**           |
   | unigram only, no stopwords        | 0.307            | 0.70               |
   | potion-base-8M @ 0.59 (reference) | —                | 0.65               |

   Chosen: unigram only, stopwords removed, signed feature hashing into 4096
   dims, sqrt term-frequency, L2-normalized, threshold **0.25**.

2. **The package-absence fallback: `effective_matcher()`.** `selected_matcher`
   (what the operator recorded) and `effective_matcher` (what actually runs)
   are now two different questions. If `matcher_model` names `lexical`
   directly, or names `all-MiniLM-L6-v2`/`potion` and that package is not
   importable, `effective_matcher` returns `lexical` — logging the reason
   once per process, not once per call. `active_model_name`,
   `active_threshold`, and `_get_model` all resolve through
   `effective_matcher`, so identity/threshold/the loaded model can never
   disagree (an explicit invariant test proves this three-way agreement).
   `int8` still reaches `MatcherNotAvailable` unchanged — this is a
   package-absence fallback, not a "make everything work somehow" catch-all.

3. **No fallback on a potion model that fails to *load*.** Package absence and
   load failure are different failures with different honest responses. If
   `model2vec` is installed but `StaticModel.from_pretrained` raises (offline,
   not cached, bad id), `_PotionEmbedder` re-raises as `MatcherNotAvailable`
   naming `OPENDAISUGI_POTION_MODEL` and the lexical alternative — the
   operator chose potion, so this refuses rather than silently substituting a
   weaker embedder and mislabeling provenance.

4. **numpy becomes a hard dependency.** The clusterer and pathway store
   already required it at call time; a zero-extra install that distills needs
   it too.

## Consequences

- A fresh, extras-free `pip install opendaisugi` now distills and reuses
  pathways offline, on the first run, with no download — the gap ADR-0018
  left open is closed.
- lexical is measurably weaker than MiniLM on paraphrase (0.70 recall here
  vs ADR-0018's reported ~0.39 for MiniLM's labeled set, at each backend's
  own FPR-matched threshold — different labeled sets, not directly
  comparable, but lexical is a keyword floor, not a semantic matcher).
  Exact and near-duplicate recurring tasks — the dominant, safe reuse case —
  still match at cosine ≈ 1.0.
- Provenance is `lexical-hash-v1`; switching backends (including a fallback
  taking hold or clearing) invalidates old rows under the existing
  stale-embedding guard exactly as any other backend switch does — a
  `daisugi tend` re-embeds.
- Vectors are wider (4096 dims) than potion's (256) or MiniLM's (384) — about
  4x the per-row storage the pathway store already pays for embeddings
  (float64 JSON: roughly 4 KB/row vs potion's ~0.25 KB), a cost worth noting
  though not yet a problem at this project's row counts.
- The package-absence fallback means a typo'd or half-installed `potion`
  extra now silently becomes `lexical` (logged once) rather than a
  zero-pathway `tend` — a real behavior change for anyone who has been
  quietly relying on the old degrade-to-nothing default.

## Alternatives considered

- **Bigrams (with or without stopwords).** Lower recall at the FPR-matched
  threshold than unigram-only-with-stopwords in the measured table above —
  more features did not buy more signal here.
- **IDF weighting.** Needs a corpus to fit against, which would make
  provenance depend on the *store's* contents rather than the text alone —
  rejected for the same reason a stable, portable identity matters at all.
- **Pure Python, no numpy.** The clusterer (`_cluster_with_centroids`) and the
  pathway store's cosine similarity already require numpy at call time, so a
  numpy-free lexical embedder would not have made numpy avoidable anywhere
  that matters — better to make the real floor (numpy, not numpy+torch)
  the honest, hard dependency.
