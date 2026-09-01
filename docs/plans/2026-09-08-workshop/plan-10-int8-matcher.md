# The int8 matcher: MiniLM without torch — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `matcher_model: int8` — a quantized MiniLM run through `onnxruntime` (no torch),
fetched once and pinned by sha256 — so `_UNBUILT_MATCHERS` becomes empty and every entry in the
matcher swap menu is a real, working embedder.

**Architecture:** A pinned manifest of four CPU-specific ONNX builds plus the tokenizer (recorded
as data, not discovered at runtime); a small generic `fetch(url, sha256, dest)` utility with resume
and verification; a `_Int8Embedder` adapter that picks the right build for the host CPU, fetches it
on first use, and runs the same tokenize → session → mean-pool → L2-normalize pipeline
sentence-transformers applies for this model; a threshold derived by running the project's existing
FPR-measurement script against the real journal; then the dispatch wiring (`_search.py`, `swap.py`,
`modules.py`, `config.py`) that makes `int8` a first-class matcher choice alongside
`all-MiniLM-L6-v2`, `potion`, and `lexical`.

**Tech Stack:** Python 3.12, `onnxruntime` (CPU execution provider), `tokenizers`, `numpy`, stdlib
`urllib.request`/`hashlib` for the fetch utility. No new dependency in the layer's hard requirements
— `onnxruntime`/`tokenizers` live behind the new `[int8]` extra, same shape as `[search]`/`[potion]`.

**Spec:** `docs/plans/2026-09-08-workshop/spec-10-int8-matcher.md` (cites master spec §5.10).
Lineage: ADR-0018 (potion), ADR-0019 (lexical). This plan writes ADR-0021.

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 1–3 are ready to build now** — the pinned manifest, CPU-variant detection, the fetch
utility, and the `_Int8Embedder` adapter are verified sound: every sha256/size in `_INT8_FILES`
was independently re-checked against a live `GET .../tree/main/onnx` call and a fresh download
of `tokenizer.json` (byte-for-byte match, including the claim that `vnni`/`avx512`/`arm64` share
one sha256 and `avx2` alone differs — true), the fallback/refusal wiring never resolves a
missing package, an unsupported CPU, a fetch failure, or a hash mismatch to a working embedder
(each raises `MatcherNotAvailable` or falls back to `lexical` per ADR-0019's rule, never the
reverse), and the file:line citations for every "Modify" step match the current repo exactly.
The Task 4 threshold was independently re-derived end to end — real download, real tokenizer,
real onnxruntime session, real journal (258 distinct tasks) — and reproduces the plan's printed
table and `0.45` exactly, including the two labeled-pair scores (0.771 / 0.127) ADR-0021 quotes.
Task 5's dispatch wiring (`_search.py`'s `_FALLBACK_PACKAGE`/`active_model_name`/
`active_threshold`/`_get_model`, `swap.py`'s `_UNBUILT_MATCHERS`) is correct. The defects below
are a real, plan-blocking sequencing gap (Task 4 cannot run as written) and honesty-tag/doc
staleness gaps in Task 5.

- **BLOCKER — Task 4 Step 4 cannot run as written; Task 5's new package-dependent tests will
  fail in CI.** No task adds `onnxruntime`/`tokenizers` to any environment before Task 4 Step 4
  asks the implementer to run `uv run --no-sync python scripts/matcher_fpr.py` for real — the
  `[int8]` extra in `pyproject.toml` isn't even defined until Task 5 Step 7, and it is never
  added to the `dev` extras aggregate (`pyproject.toml:27-40`) the way `[search]`/`[mcp]`/etc.
  are. Verified empirically: `uv run --no-sync python -c "import onnxruntime"` on this box
  raises `ModuleNotFoundError` today, and a from-scratch `uv pip install -e ".[dev]"` into a
  clean venv has neither `onnxruntime` nor `model2vec`. The same gap means, once Task 5 lands,
  `test_int8_offline_missing_cached_file_teaches_the_env_var`,
  `test_int8_online_fetch_failure_becomes_matcher_not_available`,
  `test_int8_online_fetches_model_and_tokenizer_then_builds_session`, and
  `test_modules_map_marks_int8_state_honestly` will error on a clean CI checkout (they need a
  real `import onnxruntime`/`tokenizers` to reach the mocked code paths), and the two
  `@pytest.mark.skipif(not _int8_reachable(), ...)` real-model tests will not skip — the
  runner has network access, so `_int8_reachable()` returns `True` — they will instead try to
  run and error, since `_int8_reachable()` never checks package availability. This is the exact
  already-live failure mode for potion: a clean `.[dev]`-only install fails 11 tests in
  `tests/test_matcher_backend.py` right now (`test_potion_identity_is_the_resolved_model_id`,
  `test_potion_adapter_normalizes_and_returns_numpy`, `test_modules_map_marks_potion_available_
  when_installed`, and 8 more) because `model2vec` was never added to `dev` either (ADR-0018,
  commit `a7b37ae`). Fix: add `"opendaisugi[int8]"` to `pyproject.toml`'s `dev` list (and, while
  in the neighborhood, `"opendaisugi[potion]"` — a pre-existing, separate gap this plan
  otherwise leaves in place); move the `[int8]` extra's definition earlier than Task 4, or add
  an explicit `uv pip install -e ".[int8]"` sub-step before Task 4 Step 4; and make
  `_int8_reachable()` also require `importlib.util.find_spec("onnxruntime") is not None` before
  treating the backend as reachable.
- **SHOULD-FIX — Task 5's `modules.py` wiring reports the wrong reason on an unsupported CPU.**
  `_int8_supported()` (Step 5) collapses "package absent" and "CPU has no matching build" into
  one boolean; `_emb_state`'s note text then always prints `"needs opendaisugi[int8]"` when
  `int8_installed` is `False`, even when `onnxruntime` genuinely is installed and the real
  reason is `_detect_int8_variant()` raising for this CPU. spec-10's Files list explicitly asks
  for `modules.py`: "int8 AVAILABLE/ACTIVE with the real reason." Fix: have `_int8_supported()`
  (or `_emb_state`) distinguish the two cases and print `"no int8 build for this CPU"` for the
  second.
- **SHOULD-FIX — Task 5 leaves four stale, self-contradicting doc strings.** None of these
  files are in Task 5's edit list, so each still says int8 is unbuilt right next to code that
  now builds it: `swap.py`'s module docstring (lines 8–12, "only `int8` is still a
  recorded-not-wired slot") and the comment directly above `_UNBUILT_MATCHERS` (lines 56–57,
  "only int8 is still a design slot") — the second sits immediately above the very line the
  plan changes to `frozenset()`; `exceptions.py`'s `MatcherNotAvailable` docstring ("`int8` is
  the only placeholder left in the swap menu"); and `tests/test_matcher_backend.py`'s module
  docstring ("`int8` is a placeholder that must refuse honestly"). Fix: add these four edits to
  Task 5.
- **SHOULD-FIX — spec-10's "determinism across processes" test is missing for int8.** spec-10's
  Tests section asks for "Shape, repeatability, L2 norm, determinism across processes (the
  lexical test set, reused)." The plan's Task 5 adds shape/repeatability/L2-norm coverage
  (`test_int8_shape_repeatable_and_l2_normalized`) but no analogue of
  `test_lexical_embedder_deterministic_across_processes` (spawn a subprocess, compare vectors).
  Low practical risk — onnxruntime CPU inference has no per-process randomness — but it is an
  explicit, dropped spec requirement. Fix: add it, gated the same way as the other real-model
  int8 tests.
- **NOTE — `docs/adr/README.md`'s ADR index is never updated.** Every prior ADR (0018, 0019, …)
  has a row there; no task in this plan adds one for ADR-0021. Add it in Task 4 Step 7 alongside
  the ADR file itself.
- **NOTE — the manifest and the derived threshold are real, not fabricated.** Re-verified
  independently against the live Hugging Face tree API and by actually running the described
  tokenize→onnxruntime→mean-pool→L2-normalize pipeline against this box's real journal
  (`~/.opendaisugi/journal/index.db`, 258 distinct usable tasks, matching ADR-0019's own
  corpus count one day earlier) — the FPR table, the `0.45` threshold, and the two labeled-pair
  scores in ADR-0021 all reproduce exactly. No correction needed here; noted so the BLOCKER
  above isn't mistaken for a sign the underlying data is suspect too.

## Global Constraints

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty declares go 1.26.0; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. Zig 0.16 and CMake are *build-time* requirements for go-libghostty; the
  plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  Herdr's vendored commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` (their 1.3.2) is the
  reference for patch notes, not a requirement; the binding chooses the commit it fetches.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

---

## Background for the implementer

`src/opendaisugi/_search.py` holds every pathway-reuse embedder behind a uniform adapter:
`.encode(texts, convert_to_numpy=True) -> np.ndarray` (L2-normalized). Three dispatch functions —
`active_model_name(config=None)`, `active_threshold(config=None)`, and `_get_model(config=None)` —
all resolve through `effective_matcher(config)`, which is `selected_matcher(config)` (what the
config file says) after a package-absence fallback to `lexical` (ADR-0019). `_FALLBACK_PACKAGE`
maps a config key to `(package_name, extra_name)` for that fallback check. `swap.py`'s
`_UNBUILT_MATCHERS` is the set of config values the swap UI refuses to record at all (currently
`{"int8"}`) — refused *before* any config write, so choosing a placeholder never leaves a config
file that errors on every subsequent use. `modules.py`'s `detect_stages` renders the same set of
backends as an honest ACTIVE/AVAILABLE/POSSIBLE wiring map.

This plan adds a fourth embedder, `_Int8Embedder`, and wires it through all three places the same
way `potion` (ADR-0018) and `lexical` (ADR-0019) already are. The new pieces:

- `_INT8_FILES`: a pinned manifest (path, sha256, size) for four CPU-specific ONNX builds of
  `sentence-transformers/all-MiniLM-L6-v2` plus its tokenizer, fetched from the HF tree API on
  2026-09-08 (recorded below — this is real data, not a placeholder to fill in later).
- `_detect_int8_variant()`: picks the right build for the host CPU (or refuses).
- `opendaisugi/_model_fetch.py`: a new, generic module — `fetch(url, sha256, dest)` with resume
  and verification. Not int8-specific; nothing else needs it today, but nothing about it assumes
  a single caller either.
- `_Int8Embedder`: tokenize → onnxruntime session → mean-pool → L2-normalize, batch size 32.
- `_INT8_THRESHOLD`: derived by actually running `scripts/matcher_fpr.py` against this box's real
  journal (`~/.opendaisugi/journal/index.db`, 258 distinct usable tasks) — the exact same method
  and target FPR (2.56%) ADR-0018 and ADR-0019 used. The measured result is **0.45** (see Task 4);
  this plan records that command and result in ADR-0021, not a guess.

---

### Task 1: Pin the model manifest and detect the CPU build

**Files:**
- Modify: `src/opendaisugi/_search.py` (add near the top, after the existing `_LEXICAL_TOKEN_RE`
  definition at line 51 and before the `_embedder_cache` declaration at line 55)
- Test: `tests/test_matcher_backend.py` (new section, after the lexical-embedder tests ending at
  line 86 and before the "identity resolution" section starting at line 88)

**Interfaces:**
- Produces: `_INT8_BASE_URL: str`, `_INT8_IDENTITY: str = "all-MiniLM-L6-v2-int8"`,
  `_INT8_DIM: int = 384`, `_INT8_FILES: dict[str, tuple[str, str, int]]` (variant key → (relative
  path under `_INT8_BASE_URL`, sha256 hex, size in bytes)), `_cpuinfo_flags(path: Path | None =
  None) -> set[str]`, `_detect_int8_variant(*, machine: str | None = None, flags: set[str] | None
  = None) -> str` (returns a key into `_INT8_FILES`, or raises `MatcherNotAvailable`).
- Consumes: `opendaisugi.exceptions.MatcherNotAvailable` (existing).

This is a discovery task: the manifest below was fetched live from the Hugging Face tree API on
2026-09-08 (`GET https://huggingface.co/api/models/sentence-transformers/all-MiniLM-L6-v2/tree/main/onnx`,
which reports each LFS file's sha256 directly; `tokenizer.json` sits below the LFS threshold, so
its sha256 was computed from the downloaded file, not the git-blob id the API reports for small
files). Record it as data. Do not re-derive it in a later task.

- [ ] **Step 1: Write the failing test**

Add to `tests/test_matcher_backend.py`, after line 86 (`test_lexical_embedder_paraphrase_...`):

```python
# --- int8: pinned manifest + CPU-variant detection (no model load) --------


def test_int8_files_manifest_is_pinned_data():
    from opendaisugi._search import _INT8_DIM, _INT8_FILES, _INT8_IDENTITY

    assert _INT8_IDENTITY == "all-MiniLM-L6-v2-int8"
    assert _INT8_DIM == 384
    assert set(_INT8_FILES) == {"vnni", "avx512", "avx2", "arm64", "tokenizer"}
    expected = {
        "vnni": (
            "onnx/model_qint8_avx512_vnni.onnx",
            "4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474",
            23_026_053,
        ),
        "avx512": (
            "onnx/model_qint8_avx512.onnx",
            "4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474",
            23_026_053,
        ),
        "avx2": (
            "onnx/model_quint8_avx2.onnx",
            "b941bf19f1f1283680f449fa6a7336bb5600bdcd5f84d10ddc5cd72218a0fd21",
            23_046_789,
        ),
        "arm64": (
            "onnx/model_qint8_arm64.onnx",
            "4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474",
            23_026_053,
        ),
        "tokenizer": (
            "tokenizer.json",
            "be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037",
            466_247,
        ),
    }
    assert _INT8_FILES == expected


def test_detect_int8_variant_prefers_vnni_over_avx512_over_avx2():
    from opendaisugi._search import _detect_int8_variant

    assert (
        _detect_int8_variant(machine="x86_64", flags={"avx512_vnni", "avx512f", "avx2"}) == "vnni"
    )
    assert _detect_int8_variant(machine="x86_64", flags={"avx512f", "avx2"}) == "avx512"
    assert _detect_int8_variant(machine="x86_64", flags={"avx2"}) == "avx2"


def test_detect_int8_variant_aarch64_ignores_flags():
    from opendaisugi._search import _detect_int8_variant

    assert _detect_int8_variant(machine="aarch64", flags=set()) == "arm64"
    assert _detect_int8_variant(machine="arm64", flags=set()) == "arm64"


def test_detect_int8_variant_refuses_unsupported_cpu():
    from opendaisugi._search import _detect_int8_variant
    from opendaisugi.exceptions import MatcherNotAvailable

    with pytest.raises(MatcherNotAvailable, match="no int8 build for this CPU"):
        _detect_int8_variant(machine="x86_64", flags={"sse2", "mmx"})


def test_cpuinfo_flags_parses_the_real_proc_cpuinfo_format(tmp_path):
    from opendaisugi._search import _cpuinfo_flags

    p = tmp_path / "cpuinfo"
    p.write_text("processor\t: 0\nflags\t\t: fpu vme avx2 avx512f\nbogomips\t: 4000.00\n")
    assert _cpuinfo_flags(p) == {"fpu", "vme", "avx2", "avx512f"}


def test_cpuinfo_flags_empty_when_file_missing(tmp_path):
    from opendaisugi._search import _cpuinfo_flags

    assert _cpuinfo_flags(tmp_path / "does-not-exist") == set()
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k int8 -q`
Expected: FAIL — `ImportError: cannot import name '_INT8_FILES'` (and siblings).

- [ ] **Step 3: Write the implementation**

In `src/opendaisugi/_search.py`, add `from pathlib import Path` to the top-of-file imports (it
currently has `contextlib`, `hashlib`, `logging`, `math`, `re`, `warnings`, `Counter`,
`TYPE_CHECKING`, `Any` — add `Path` alongside them). Then, after `_LEXICAL_TOKEN_RE` (line 51),
insert:

```python
# The second torch-free backend (ADR-0021): MiniLM itself, quantized to int8
# and exported to ONNX by the official sentence-transformers repository, run
# through onnxruntime instead of sentence-transformers/torch. Unlike potion
# and lexical this is not a different model family — it is the closest
# torch-free approximation to the default backend's own embedding space,
# which is exactly why it needs its own identity and threshold rather than
# a claim of equivalence (the crux ADR-0018/0019 already learned twice).
#
# Pinned 2026-09-08 from the HF tree API (GET https://huggingface.co/api/
# models/sentence-transformers/all-MiniLM-L6-v2/tree/main/onnx), which
# reports each LFS file's sha256 directly. tokenizer.json sits below the LFS
# threshold, so its sha256 here was computed from the downloaded file, not
# the git-blob id the API reports for small files. Three of the four onnx
# builds (vnni, avx512, arm64) are byte-identical on the day this was
# pinned — the CPU-feature dispatch still names all four separately, since
# HF may diverge them later.
_INT8_BASE_URL = "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/"
_INT8_IDENTITY = "all-MiniLM-L6-v2-int8"
_INT8_DIM = 384
_INT8_FILES: dict[str, tuple[str, str, int]] = {
    # variant/file key -> (path under _INT8_BASE_URL, sha256, size in bytes)
    "vnni": (
        "onnx/model_qint8_avx512_vnni.onnx",
        "4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474",
        23_026_053,
    ),
    "avx512": (
        "onnx/model_qint8_avx512.onnx",
        "4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474",
        23_026_053,
    ),
    "avx2": (
        "onnx/model_quint8_avx2.onnx",
        "b941bf19f1f1283680f449fa6a7336bb5600bdcd5f84d10ddc5cd72218a0fd21",
        23_046_789,
    ),
    "arm64": (
        "onnx/model_qint8_arm64.onnx",
        "4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474",
        23_026_053,
    ),
    "tokenizer": (
        "tokenizer.json",
        "be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037",
        466_247,
    ),
}


def _cpuinfo_flags(path: Path | None = None) -> set[str]:
    """x86 CPU feature flags from /proc/cpuinfo's first ``flags`` line.

    Empty on any host with no such file (macOS, Windows, a container without
    /proc) or no matching line — never raises, so CPU detection can treat
    "no flags found" the same as "no matching flags".
    """
    try:
        text = (path or Path("/proc/cpuinfo")).read_text()
    except OSError:
        return set()
    for line in text.splitlines():
        if line.startswith("flags"):
            return set(line.split(":", 1)[1].split())
    return set()


def _detect_int8_variant(*, machine: str | None = None, flags: set[str] | None = None) -> str:
    """Which key into ``_INT8_FILES`` this CPU can run.

    aarch64 always gets the arm64 build. x86 is matched most-capable flag
    first (VNNI > AVX-512 > AVX2) so a VNNI CPU gets the VNNI build rather
    than falling through to a plain AVX-512 one. ``machine``/``flags`` are
    injectable for tests; real callers pass neither. Raises
    ``MatcherNotAvailable`` when nothing matches — an older or unusual CPU
    has no published int8 build for this model; potion and lexical have no
    such floor.
    """
    if machine is None:
        import platform

        machine = platform.machine()
    if machine in ("aarch64", "arm64"):
        return "arm64"
    if flags is None:
        flags = _cpuinfo_flags()
    if "avx512_vnni" in flags:
        return "vnni"
    if "avx512f" in flags:
        return "avx512"
    if "avx2" in flags:
        return "avx2"
    from opendaisugi.exceptions import MatcherNotAvailable

    raise MatcherNotAvailable(
        "no int8 build for this CPU. Use matcher_model: potion or lexical instead."
    )
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k int8 -q`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/_search.py tests/test_matcher_backend.py
git commit -m "$(cat <<'EOF'
int8 matcher: pin the model manifest and CPU-variant detection

Records the four ONNX builds + tokenizer for all-MiniLM-L6-v2-int8, fetched
live from the HF tree API on 2026-09-08, as data rather than a runtime
lookup. Detection picks the CPU-appropriate build (VNNI > AVX-512 > AVX2, or
arm64) and refuses honestly on older/unusual CPUs. First step of ADR-0021;
not yet wired into the matcher dispatch.
EOF
)"
```

---

### Task 2: `_model_fetch.py` — resumable, verified downloads

**Files:**
- Create: `src/opendaisugi/_model_fetch.py`
- Test: `tests/test_model_fetch.py`

**Interfaces:**
- Produces: `sha256_file(path: Path) -> str`, `fetch(url: str, sha256: str, dest: Path, *, size:
  int | None = None) -> Path` (returns `dest`), `FetchVerificationError(OSError)`.
- Consumes: nothing from earlier tasks — this is generic infrastructure.

This is new, general-purpose infrastructure, not int8-specific: nothing about it assumes a single
caller. `fetch()` short-circuits when `dest` already matches `sha256` (no network call at all),
resumes a partial `<dest>.part` file via an HTTP Range request, verifies the sha256 of the
*complete* file before renaming it into place, and — on a mismatch — deletes the file and raises,
so a caller never sees a corrupt file at `dest` and the next call re-fetches clean.

- [ ] **Step 1: Write the failing test**

Create `tests/test_model_fetch.py`:

```python
"""A generic, resumable, sha256-verified download utility (ADR-0021's int8
matcher is the first caller, but nothing here is int8-specific)."""

from __future__ import annotations

import hashlib
import http.server
import threading

import pytest

from opendaisugi._model_fetch import FetchVerificationError, fetch, sha256_file

BLOB = b"x" * 500_000 + b"y" * 12345
SHA = hashlib.sha256(BLOB).hexdigest()


class _RangeHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass  # keep test output clean

    def do_GET(self):
        rng = self.headers.get("Range")
        if rng:
            start = int(rng.split("=")[1].split("-")[0])
            body = BLOB[start:]
            self.send_response(206)
            self.send_header("Content-Range", f"bytes {start}-{len(BLOB) - 1}/{len(BLOB)}")
        else:
            body = BLOB
            self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


@pytest.fixture
def server():
    httpd = http.server.HTTPServer(("127.0.0.1", 0), _RangeHandler)
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    yield f"http://127.0.0.1:{httpd.server_port}/blob"
    httpd.shutdown()


def test_sha256_file_matches_hashlib(tmp_path):
    p = tmp_path / "f.bin"
    p.write_bytes(BLOB)
    assert sha256_file(p) == SHA


def test_fetch_downloads_and_verifies(tmp_path, server):
    dest = tmp_path / "model.bin"
    out = fetch(server, SHA, dest, size=len(BLOB))
    assert out == dest
    assert dest.read_bytes() == BLOB
    assert not (tmp_path / "model.bin.part").exists()


def test_fetch_short_circuits_when_already_valid(tmp_path, server, monkeypatch):
    dest = tmp_path / "model.bin"
    dest.write_bytes(BLOB)
    calls = []
    import urllib.request

    orig = urllib.request.urlopen

    def spy(*a, **k):
        calls.append(1)
        return orig(*a, **k)

    monkeypatch.setattr(urllib.request, "urlopen", spy)
    fetch(server, SHA, dest, size=len(BLOB))
    assert calls == []  # no network call at all


def test_fetch_resumes_partial_download(tmp_path, server):
    dest = tmp_path / "model.bin"
    part = tmp_path / "model.bin.part"
    part.write_bytes(BLOB[:500_000])
    fetch(server, SHA, dest, size=len(BLOB))
    assert dest.read_bytes() == BLOB
    assert not part.exists()


def test_fetch_deletes_and_raises_on_hash_mismatch(tmp_path, server):
    with pytest.raises(FetchVerificationError):
        fetch(server, "0" * 64, tmp_path / "model.bin", size=len(BLOB))
    assert not (tmp_path / "model.bin").exists()
    assert not (tmp_path / "model.bin.part").exists()  # next call re-fetches clean


def test_fetch_unreachable_host_raises_oserror(tmp_path):
    with pytest.raises(OSError):
        fetch("http://127.0.0.1:1/nope", SHA, tmp_path / "model.bin", size=1)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/test_model_fetch.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'opendaisugi._model_fetch'`.

- [ ] **Step 3: Write the implementation**

Create `src/opendaisugi/_model_fetch.py`:

```python
"""Resumable, sha256-verified downloads for on-demand model files.

Generic infrastructure — the int8 matcher (ADR-0021) is the first caller,
fetching a small quantized ONNX model and its tokenizer on first use, but
nothing here is int8-specific. A file at ``dest`` that already matches the
expected hash short-circuits to zero network calls; a hash mismatch after a
full download deletes the file and raises, so a caller never sees a corrupt
or truncated file at ``dest`` and the next call re-fetches clean.
"""

from __future__ import annotations

import hashlib
import logging
import urllib.error
import urllib.request
from pathlib import Path

_CHUNK = 1 << 20  # 1 MiB
_log = logging.getLogger("opendaisugi._model_fetch")


class FetchVerificationError(OSError):
    """Raised when a downloaded file's sha256 does not match after fetch.

    The file (and any partial ``.part``) is deleted before this is raised —
    the next ``fetch()`` call starts clean rather than reusing a corrupt file.
    """


def sha256_file(path: Path) -> str:
    """Streaming SHA-256 of a file, hex-encoded."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(_CHUNK), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch(url: str, sha256: str, dest: Path, *, size: int | None = None) -> Path:
    """Return ``dest``, downloading it first if absent or hash-mismatched.

    Resumes a partial download at ``<dest>.part`` via an HTTP Range request
    when one exists, verifies the sha256 of the *complete* file before
    renaming it into place, and prints one notice naming the file and size
    before the first network call this process makes for it (never at
    import — only when a caller actually needs the file).
    """
    dest = Path(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists() and sha256_file(dest) == sha256:
        return dest
    if dest.exists():
        dest.unlink()  # stale or corrupt — start clean, don't silently reuse

    part = dest.with_name(dest.name + ".part")
    resume_from = part.stat().st_size if part.exists() else 0
    size_note = f" (~{size // 1024 // 1024}MB)" if size else ""
    _log.warning("fetching %s%s from %s ...", dest.name, size_note, url)

    req = urllib.request.Request(url)
    if resume_from:
        req.add_header("Range", f"bytes={resume_from}-")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            mode = "ab" if resume_from and resp.status == 206 else "wb"
            if mode == "wb":
                resume_from = 0
            with open(part, mode) as f:
                while chunk := resp.read(_CHUNK):
                    f.write(chunk)
    except urllib.error.URLError as exc:
        # Deliberately generic: this module has no caller-specific env var to
        # name. A caller that has one (e.g. _Int8Embedder's OPENDAISUGI_INT8_
        # MODEL) catches this OSError and adds that guidance itself — see
        # Task 3's except block.
        raise OSError(f"could not download {url}: {exc}") from exc

    actual = sha256_file(part)
    if actual != sha256:
        part.unlink(missing_ok=True)
        raise FetchVerificationError(
            f"{url} downloaded but sha256 did not match "
            f"(expected {sha256}, got {actual}). Deleted; the next call re-fetches."
        )
    part.rename(dest)
    return dest
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/test_model_fetch.py -q`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/_model_fetch.py tests/test_model_fetch.py
git commit -m "$(cat <<'EOF'
Add a generic resumable, verified download utility

fetch(url, sha256, dest) short-circuits on an already-valid file, resumes a
partial download via HTTP Range, and deletes + raises on a hash mismatch so
callers never see a corrupt file. The int8 matcher (ADR-0021) is the first
caller; this module is not int8-specific.
EOF
)"
```

---

### Task 3: `_Int8Embedder` — tokenize, run, pool, normalize

**Files:**
- Modify: `src/opendaisugi/_search.py` (add after `_detect_int8_variant`, i.e. immediately before
  the `_embedder_cache: dict[str, Any] = {}` declaration at line 55 of the original file)
- Test: `tests/test_matcher_backend.py` (new section, immediately after Task 1's int8 tests)

**Interfaces:**
- Consumes: `_INT8_FILES`, `_INT8_IDENTITY`, `_INT8_DIM`, `_INT8_BASE_URL`, `_detect_int8_variant`
  (Task 1); `opendaisugi._model_fetch.fetch`, `sha256_file` (Task 2); `opendaisugi.exceptions.
  MatcherNotAvailable` (existing).
- Produces: `_int8_cache_dir() -> Path`; `_Int8Embedder` with `__init__(self)` and `encode(self,
  texts, convert_to_numpy=True) -> np.ndarray` (L2-normalized, shape `(len(texts), _INT8_DIM)`).
  **Not yet wired** into `effective_matcher`/`active_model_name`/`active_threshold`/`_get_model` —
  that is Task 5. Selecting `matcher_model: int8` still raises `MatcherNotAvailable` at
  `active_model_name()` until then; this task only builds and tests the class directly.

- [ ] **Step 1: Write the failing test — the encode() adapter, no packages needed**

`encode()`'s masking/pooling/batching logic is tested by constructing a bare instance and
injecting fake `_tok`/`_session` attributes directly (bypassing `__init__`, which is tested
separately in Step 1b) — this needs no onnxruntime, no tokenizers, no network. Add to
`tests/test_matcher_backend.py`, after Task 1's `test_cpuinfo_flags_empty_when_file_missing`:

```python
# --- the int8 adapter: encode() itself, no packages or network needed ------


class _FakeInt8Encoding:
    def __init__(self, ids, attention_mask):
        self.ids = ids
        self.attention_mask = attention_mask


class _FakeInt8Tokenizer:
    """Stand-in for a configured tokenizers.Tokenizer: fixed batch width 5,
    marking the first N positions "real" per text and the rest padding —
    enough to prove encode() batches and masks correctly without a real
    vocabulary."""

    WIDTH = 5

    def encode_batch(self, texts):
        out = []
        for t in texts:
            real = min(len(t.split()) + 2, self.WIDTH)
            ids = [101] * real + [0] * (self.WIDTH - real)
            mask = [1] * real + [0] * (self.WIDTH - real)
            out.append(_FakeInt8Encoding(ids, mask))
        return out


class _FakeInt8Session:
    """Stand-in for onnxruntime.InferenceSession: every real (unmasked)
    token position gets a constant embedding (3.0); every padded position
    gets a different constant (999.0) — proving encode() excludes padding
    from the mean-pool rather than averaging garbage into it."""

    def run(self, output_names, feed):
        mask = feed["attention_mask"].astype(bool)
        batch, seq = mask.shape
        vecs = np.full((batch, seq, 384), 999.0, dtype=np.float32)
        vecs[mask] = 3.0
        return [vecs]


def _bare_int8_embedder(tok, session):
    from opendaisugi._search import _Int8Embedder

    emb = object.__new__(_Int8Embedder)
    emb._tok = tok
    emb._session = session
    return emb


def test_int8_encode_masks_padding_before_pooling_and_l2_normalizes():
    emb = _bare_int8_embedder(_FakeInt8Tokenizer(), _FakeInt8Session())
    out = emb.encode(["a", "a much longer sentence with several more words in it"])
    assert out.shape == (2, 384)
    assert np.allclose(np.linalg.norm(out, axis=1), 1.0, atol=1e-5)
    # Every real position contributes the SAME constant (3.0) regardless of
    # length; if padding's 999.0 leaked into the mean, these rows would not
    # be uniform unit vectors. This is the assertion a masking bug breaks.
    expected = 1.0 / np.sqrt(384)
    assert np.allclose(out, expected, atol=1e-5)


def test_int8_encode_batches_in_groups_of_32():
    calls = []

    class _CountingSession(_FakeInt8Session):
        def run(self, output_names, feed):
            calls.append(feed["input_ids"].shape[0])
            return super().run(output_names, feed)

    emb = _bare_int8_embedder(_FakeInt8Tokenizer(), _CountingSession())
    emb.encode([f"task {i}" for i in range(70)])
    assert calls == [32, 32, 6]


def test_int8_encode_empty_list_returns_correct_shape():
    emb = _bare_int8_embedder(_FakeInt8Tokenizer(), _FakeInt8Session())
    out = emb.encode([])
    assert out.shape == (0, 384)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k int8_encode -q`
Expected: FAIL — `AttributeError`/`ImportError`: `_Int8Embedder` does not exist yet.

- [ ] **Step 3: Write the implementation**

In `src/opendaisugi/_search.py`, immediately after `_detect_int8_variant` (end of Task 1's
addition) and before the `_embedder_cache: dict[str, Any] = {}` line, add:

```python
def _int8_cache_dir() -> Path:
    """Where the pinned int8 files live: ``OPENDAISUGI_INT8_MODEL`` when set
    (a pre-fetched directory, for offline installs — mirrors potion's
    ``OPENDAISUGI_POTION_MODEL``), else the shared model cache."""
    import os

    override = os.environ.get("OPENDAISUGI_INT8_MODEL")
    if override:
        return Path(override)
    return Path.home() / ".cache" / "opendaisugi" / "models" / "minilm-int8"


class _Int8Embedder:
    """all-MiniLM-L6-v2, int8-quantized, via onnxruntime — no torch (ADR-0021).

    A missing onnxruntime/tokenizers package is a fallback case, handled by
    ``effective_matcher`` before this class is ever constructed (same as
    potion). A CPU with no matching build, a fetch failure, or a file that
    still fails its sha256 check are NOT fallback cases: the operator chose
    int8, so this raises ``MatcherNotAvailable`` with the fix instead of
    silently downgrading and mislabeling provenance.
    """

    def __init__(self):
        try:
            import onnxruntime as ort
        except ImportError as exc:
            raise ImportError(
                "the 'int8' matcher needs onnxruntime + tokenizers: pip install 'opendaisugi[int8]'"
            ) from exc
        try:
            from tokenizers import Tokenizer
        except ImportError as exc:
            raise ImportError(
                "the 'int8' matcher needs onnxruntime + tokenizers: pip install 'opendaisugi[int8]'"
            ) from exc

        import os

        from opendaisugi.exceptions import MatcherNotAvailable

        variant = _detect_int8_variant()
        cache_dir = _int8_cache_dir()
        model_rel, model_sha, model_size = _INT8_FILES[variant]
        tok_rel, tok_sha, tok_size = _INT8_FILES["tokenizer"]
        model_path = cache_dir / Path(model_rel).name
        tok_path = cache_dir / Path(tok_rel).name

        offline = "OPENDAISUGI_INT8_MODEL" in os.environ
        try:
            if offline:
                from opendaisugi._model_fetch import sha256_file

                for p, want in ((model_path, model_sha), (tok_path, tok_sha)):
                    if not p.exists() or sha256_file(p) != want:
                        raise FileNotFoundError(
                            f"{p} is missing or does not match the pinned sha256"
                        )
            else:
                from opendaisugi._model_fetch import fetch

                fetch(_INT8_BASE_URL + model_rel, model_sha, model_path, size=model_size)
                fetch(_INT8_BASE_URL + tok_rel, tok_sha, tok_path, size=tok_size)
        except OSError as exc:
            raise MatcherNotAvailable(
                f"the int8 model could not be fetched ({exc}). Pre-fetch it and "
                f"set OPENDAISUGI_INT8_MODEL=<local dir>, or set matcher_model: "
                f"lexical for a zero-download matcher."
            ) from exc

        self._tok = Tokenizer.from_file(str(tok_path))
        self._tok.enable_truncation(max_length=256)
        pad_id = self._tok.token_to_id("[PAD]")
        self._tok.enable_padding(pad_id=pad_id if pad_id is not None else 0, pad_token="[PAD]")
        self._session = ort.InferenceSession(str(model_path), providers=["CPUExecutionProvider"])

    def encode(self, texts, convert_to_numpy=True):
        import numpy as np

        texts = list(texts)
        if not texts:
            return np.zeros((0, _INT8_DIM))
        pooled = []
        for i in range(0, len(texts), 32):
            batch = texts[i : i + 32]
            encs = self._tok.encode_batch(batch)
            input_ids = np.array([e.ids for e in encs], dtype=np.int64)
            attention_mask = np.array([e.attention_mask for e in encs], dtype=np.int64)
            token_type_ids = np.zeros_like(input_ids)
            (token_embeddings,) = self._session.run(
                None,
                {
                    "input_ids": input_ids,
                    "attention_mask": attention_mask,
                    "token_type_ids": token_type_ids,
                },
            )
            mask = attention_mask[..., None].astype(np.float32)
            summed = (token_embeddings * mask).sum(axis=1)
            counts = np.clip(mask.sum(axis=1), 1e-9, None)
            pooled.append(summed / counts)
        return _l2(np.concatenate(pooled, axis=0))
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k int8_encode -q`
Expected: PASS (3 tests).

- [ ] **Step 5: Write the failing test — the constructor (package-absent, offline, online fetch)**

Add, right after the Step 1 tests:

```python
# --- the int8 adapter: __init__ (package-absent, offline, online fetch) ---


def test_int8_missing_package_teaches_the_extra(monkeypatch):
    import builtins

    real_import = builtins.__import__

    def _blocked(name, *a, **k):
        if name == "onnxruntime":
            raise ImportError("No module named 'onnxruntime'")
        return real_import(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", _blocked)
    from opendaisugi._search import _Int8Embedder

    with pytest.raises(ImportError, match=r"opendaisugi\[int8\]"):
        _Int8Embedder()


def test_int8_offline_missing_cached_file_teaches_the_env_var(monkeypatch, tmp_path):
    monkeypatch.setenv("OPENDAISUGI_INT8_MODEL", str(tmp_path / "does-not-exist"))
    from opendaisugi._search import _Int8Embedder

    with pytest.raises(MatcherNotAvailable, match="OPENDAISUGI_INT8_MODEL"):
        _Int8Embedder()


def test_int8_online_fetch_failure_becomes_matcher_not_available(monkeypatch):
    from opendaisugi import _search

    monkeypatch.setattr(_search, "_detect_int8_variant", lambda: "avx2")

    def _raise(*a, **k):
        raise OSError("network unreachable")

    monkeypatch.setattr("opendaisugi._model_fetch.fetch", _raise)
    with pytest.raises(MatcherNotAvailable, match="OPENDAISUGI_INT8_MODEL"):
        _search._Int8Embedder()


def test_int8_online_fetches_model_and_tokenizer_then_builds_session(monkeypatch, tmp_path):
    from opendaisugi import _search

    monkeypatch.setattr(_search, "_int8_cache_dir", lambda: tmp_path)
    monkeypatch.setattr(_search, "_detect_int8_variant", lambda: "avx2")

    fetched = []

    def _fake_fetch(url, sha256, dest, *, size=None):
        fetched.append(dest.name)
        dest.write_bytes(b"fake bytes")
        return dest

    monkeypatch.setattr("opendaisugi._model_fetch.fetch", _fake_fetch)
    monkeypatch.setattr(
        "tokenizers.Tokenizer.from_file", staticmethod(lambda p: _FakeInt8Tokenizer())
    )
    monkeypatch.setattr(
        "onnxruntime.InferenceSession", lambda path, providers=None: _FakeInt8Session()
    )

    emb = _search._Int8Embedder()
    assert sorted(fetched) == ["model_quint8_avx2.onnx", "tokenizer.json"]
    assert isinstance(emb, _search._Int8Embedder)
```

- [ ] **Step 6: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k "int8_missing_package or int8_offline or int8_online" -q`
Expected: the first two tests may already pass (they exercise real code paths that Step 3 already
built); `test_int8_online_fetches_model_and_tokenizer_then_builds_session` FAILS if
`_FakeInt8Tokenizer`/`_FakeInt8Session` lack the methods `_Int8Embedder.__init__` calls
(`enable_truncation`, `enable_padding`, `token_to_id`) — confirm the failure names one of those.

- [ ] **Step 7: Extend the fakes so `__init__` can run against them**

Add `enable_truncation`, `enable_padding`, and `token_to_id` to `_FakeInt8Tokenizer` (defined in
Step 1), so the same fake serves both the encode-only tests and the constructor test:

```python
class _FakeInt8Tokenizer:
    WIDTH = 5

    def enable_truncation(self, max_length):
        pass

    def enable_padding(self, pad_id=0, pad_token="[PAD]"):
        pass

    def token_to_id(self, token):
        return 0

    def encode_batch(self, texts):
        out = []
        for t in texts:
            real = min(len(t.split()) + 2, self.WIDTH)
            ids = [101] * real + [0] * (self.WIDTH - real)
            mask = [1] * real + [0] * (self.WIDTH - real)
            out.append(_FakeInt8Encoding(ids, mask))
        return out
```

(This replaces the Step 1 version of the class — same file, same name, now with three more
no-op methods so `__init__` can call them without touching a real tokenizer.)

- [ ] **Step 8: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k "int8" -q`
Expected: PASS (all int8 tests so far — manifest, detection, encode, constructor).

- [ ] **Step 9: Commit**

```bash
git add src/opendaisugi/_search.py tests/test_matcher_backend.py
git commit -m "$(cat <<'EOF'
Add _Int8Embedder: tokenize, run, mean-pool, L2-normalize

Fetches the CPU-appropriate pinned ONNX build on first use (via
_model_fetch.fetch), builds an onnxruntime CPU session, and follows the same
tokenize -> pool -> normalize pipeline sentence-transformers applies for this
model. Not yet reachable via matcher_model=int8 (Task 5 wires the dispatch);
this task builds and tests the adapter on its own.
EOF
)"
```

---

### Task 4: Derive the threshold and write ADR-0021

**Files:**
- Modify: `scripts/matcher_fpr.py` (insert after the `potion` try/except block, lines 92-99 of the
  original file, before the `all-MiniLM-L6-v2` try/except block)
- Modify: `src/opendaisugi/_search.py` (add `_INT8_THRESHOLD` after `_INT8_DIM = 384`)
- Test: `tests/test_matcher_backend.py`
- Create: `docs/adr/0021-int8-matcher.md`

**Interfaces:**
- Consumes: `_Int8Embedder` (Task 3).
- Produces: `_INT8_THRESHOLD: float = 0.45` in `_search.py` — a plain module constant, not yet
  consumed by `active_threshold` (Task 5 wires that).

`scripts/matcher_fpr.py` measures a backend's false-positive rate by constructing it directly
(exactly as it already does for `_PotionEmbedder`/`_STEmbedder`) — it does not go through
`effective_matcher`/`_get_model`, so this works even though int8 is not dispatch-wired yet.

- [ ] **Step 1: Write the failing test**

Add to `tests/test_matcher_backend.py`, in the int8 section:

```python
def test_int8_threshold_is_derived_and_recorded():
    """Locks the value ADR-0021 records — re-derive with scripts/matcher_fpr.py
    if the journal or the model changes; don't hand-edit this without also
    updating the ADR."""
    from opendaisugi._search import _INT8_THRESHOLD

    assert _INT8_THRESHOLD == pytest.approx(0.45)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k int8_threshold -q`
Expected: FAIL — `ImportError: cannot import name '_INT8_THRESHOLD'`.

- [ ] **Step 3: Add the int8 backend to the measurement script**

In `scripts/matcher_fpr.py`, insert after the potion block (originally lines 92-99, right before
`try: from opendaisugi._search import _STEmbedder`):

```python
    try:
        from opendaisugi._search import _Int8Embedder

        int8 = _Int8Embedder()
    except Exception as exc:  # noqa: BLE001 - a measurement tool reports, not raises
        print(f"\n=== all-MiniLM-L6-v2-int8 === skipped: {exc}")
    else:
        measure_backend("all-MiniLM-L6-v2-int8", int8.encode, tasks, pairs)
```

- [ ] **Step 4: Run the script for real, against the real journal**

Run: `uv run --no-sync python scripts/matcher_fpr.py`

This downloads the ~23MB pinned model on first run (one-time; cached under
`~/.cache/opendaisugi/models/minilm-int8/`) and prints, among the other backends:

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

(This is the actual output measured on the reference box's real journal — 258 distinct usable
tasks, 20,000 random distinct-task pairs, seed 42, target FPR 2.56% — the same corpus and method
ADR-0018/0019 used, run on the `avx2` build this box's CPU selects. If your journal differs, your
printed table will differ; use *your* printed `-> closest to target FPR` line in Step 5, not the
number above.)

- [ ] **Step 5: Record the derived threshold**

In `src/opendaisugi/_search.py`, immediately after `_INT8_DIM = 384`, add:

```python
# Per-backend reuse threshold (ADR-0021), derived the same way ADR-0018 and
# ADR-0019 derived potion's and lexical's: `uv run --no-sync python
# scripts/matcher_fpr.py` against the real journal (258 distinct usable
# tasks, 20,000 random distinct-task pairs, seed 42), closest to MiniLM@0.55's
# measured 2.56% false-match rate. Measured on the `avx2` build.
_INT8_THRESHOLD = 0.45
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -k int8_threshold -q`
Expected: PASS.

- [ ] **Step 7: Write ADR-0021**

Create `docs/adr/0021-int8-matcher.md`:

```markdown
# ADR-0021 — The `int8` matcher: MiniLM without torch, via onnxruntime

- **Status:** Accepted
- **Date:** 2026-09-08

## Context

ADR-0018 shipped `potion` (model2vec, torch-free) and ADR-0019 shipped `lexical`
(stdlib + numpy, zero model) as alternatives to `all-MiniLM-L6-v2` for
pathway-reuse embedding. `int8` was the last placeholder in the swap menu:
`_UNBUILT_MATCHERS` refused it, and `daisugi modules` marked it `POSSIBLE` —
"planned, not wired yet." Unlike potion and lexical, int8 is not a different
model family; it is MiniLM itself, quantized to 8-bit integers and exported
to ONNX by the official sentence-transformers repository, run through
`onnxruntime` instead of `sentence-transformers`/torch. That makes it the
closest torch-free approximation to the default backend's own embedding
space — and, per the crux this line has learned twice now, "close" is not
"same": a store whose rows say `all-MiniLM-L6-v2` but hold int8 vectors
would compare against the wrong space.

## Decision

1. **A pinned manifest, not a live lookup.** `_INT8_FILES` in `_search.py`
   records four ONNX builds (`avx512_vnni`, `avx512`, `avx2`, `arm64`) plus
   `tokenizer.json`, each with its exact size and sha256, pinned 2026-09-08
   from `https://huggingface.co/api/models/sentence-transformers/all-MiniLM-L6-v2/tree/main/onnx`.
   The API reports the LFS sha256 for the (LFS) onnx files directly;
   `tokenizer.json` sits below the LFS threshold, so its sha256 was computed
   from the downloaded file, not the git-blob id the API reports for small
   files. Three of the four onnx builds (`vnni`, `avx512`, `arm64`) are
   byte-identical on the day this was pinned — the CPU-feature dispatch
   still names all four separately, since HF may diverge them later.

2. **CPU-feature dispatch, refuse honestly off the ladder.**
   `_detect_int8_variant` reads `/proc/cpuinfo`'s `flags` line
   (`avx512_vnni` -> vnni, `avx512f` -> avx512, `avx2` -> avx2) or
   `platform.machine()` (`aarch64` -> arm64), most capable first. A CPU with
   none of these (older x86, or a host with no `/proc/cpuinfo` — macOS,
   Windows) raises `MatcherNotAvailable` naming potion and lexical as the
   alternatives that have no such floor.

3. **A generic, resumable, verified fetch.**
   `opendaisugi._model_fetch.fetch(url, sha256, dest)` is new,
   general-purpose infrastructure: short-circuits when `dest` already
   matches the hash, resumes a partial `.part` file via an HTTP Range
   request, verifies the complete file's sha256 before renaming it into
   place, and deletes + raises on a mismatch so the next call re-fetches
   clean rather than silently reusing a corrupt file. It is not
   int8-specific; nothing today needs a second caller, but nothing about it
   assumes one either.

4. **The same pipeline sentence-transformers applies for this model.**
   `_Int8Embedder` tokenizes with `tokenizers.Tokenizer.from_file` (truncate
   256, dynamic pad), runs the onnxruntime session with `input_ids`,
   `attention_mask`, `token_type_ids`, mean-pools `last_hidden_state` over
   the attention mask, and L2-normalizes — batch size 32. Verified against
   the real `model_quint8_avx2.onnx` build: output shape `(batch, 384)`,
   norm 1.0.

5. **A threshold derived on the real journal, not guessed.** Running
   `uv run --no-sync python scripts/matcher_fpr.py` against
   `~/.opendaisugi/journal/index.db` (258 distinct usable tasks, 20,000
   random distinct-task pairs, seed 42 — the same corpus and method
   ADR-0018 and ADR-0019 used) at the 2.56% target FPR:

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

   `_INT8_THRESHOLD = 0.45` (measured on the `avx2` build — this box's CPU;
   the three byte-identical builds would measure the same). On the same
   near-duplicate/unrelated pair the lexical test uses: "clean up stale tmp
   files in the build directory" / "remove old temporary files from the
   build folder" scores 0.771 (clears 0.45); "rebuild the android apk" /
   "write the ADR for the gateway" scores 0.127 (well under).

6. **Package-absence falls back to lexical; everything else refuses.**
   `_FALLBACK_PACKAGE["int8"] = ("onnxruntime", "opendaisugi[int8]")` — the
   same rule ADR-0019 established for MiniLM/potion. A CPU with no build, a
   fetch failure, or a downloaded file that will not verify are NOT fallback
   cases: the operator chose int8, so `_Int8Embedder` raises
   `MatcherNotAvailable` with the fix (`OPENDAISUGI_INT8_MODEL`, or switch
   to lexical) instead of silently downgrading and mislabeling provenance.

7. **`_UNBUILT_MATCHERS` is now empty.** int8 was the only member. The
   identity-layer and swap-layer refusal tests that existed only to prove
   int8 refused (`test_unbuilt_backends_refuse_at_identity`,
   `test_swap_refuses_unbuilt_matcher_without_writing_config`,
   `test_swap_confirmation_reports_refusal_without_crashing`) are deleted —
   there is no unbuilt backend left to exercise them against. The refusal
   code path in `apply_swap` stays, dormant, for the next placeholder.

## Consequences

- `matcher_model: int8` now produces and reuses pathways on a machine that
  cannot run torch and does not want potion's static-embedding ceiling —
  int8 is a real neural embedding, just quantized and running under a
  different (lighter) runtime.
- int8 is CPU-feature-gated in a way potion and lexical are not: a pre-2013
  x86 CPU (no AVX2), or a host with no `/proc/cpuinfo`, has no int8 build
  and must use potion or lexical instead.
- The 23MB fetch is one-time and cached under
  `~/.cache/opendaisugi/models/minilm-int8/`; `OPENDAISUGI_INT8_MODEL`
  points at a pre-fetched directory for air-gapped installs, exactly like
  potion's `OPENDAISUGI_POTION_MODEL`.
- Switching to or from int8 requires a `daisugi tend` to re-embed, per the
  existing stale-embedding guard.

## Alternatives considered

- **fp16 instead of / alongside int8.** Out of scope (per spec) — fp16 needs
  a GPU execution provider to pay for itself; on CPU it is not smaller or
  faster than int8 in a way that earns a second identity yet.
- **GPU execution providers (CUDA, DirectML).** Out of scope. onnxruntime
  supports them, but this box's Pascal GPU is exactly the case ADR-0018
  named as unable to run the torch build either — CPU-only keeps int8's
  value proposition (runs where torch can't) honest.
- **Bundling the model in the wheel.** Rejected for the same reason ADR-0018
  rejected it for potion — keeps the wheel small; `OPENDAISUGI_INT8_MODEL`
  is the air-gapped path.
```

- [ ] **Step 8: Commit**

```bash
git add scripts/matcher_fpr.py src/opendaisugi/_search.py tests/test_matcher_backend.py docs/adr/0021-int8-matcher.md
git commit -m "$(cat <<'EOF'
Derive the int8 matcher threshold and write ADR-0021

Measured against the real journal with scripts/matcher_fpr.py at the same
2.56% target FPR ADR-0018/0019 used: 0.45. Recorded as _INT8_THRESHOLD (not
yet wired into active_threshold — Task 5) and in ADR-0021 with the exact
command and printed table, so it is reproducible, not asserted.
EOF
)"
```

---

### Task 5: Wire int8 into the matcher, swap menu, and module map

**Files:**
- Modify: `src/opendaisugi/_search.py` (`_FALLBACK_PACKAGE` dict at original lines 162-165;
  `active_model_name` at lines 210-234; `active_threshold` at lines 237-253; `_get_model` at lines
  365-388)
- Modify: `src/opendaisugi/swap.py` (`_UNBUILT_MATCHERS` at line 58; the `int8` `SwapOption` at
  line 123)
- Modify: `src/opendaisugi/modules.py` (`_have`/potion detection block at lines 94-105; the
  matcher stage's int8 `Module` at line 219; the distill stage's module list at lines 237-255)
- Modify: `src/opendaisugi/config.py` (the `matcher_model` doc comment at lines 77-83)
- Modify: `tests/test_matcher_backend.py` (delete three stale tests; parametrize one; add the
  rest)

**Interfaces:**
- Consumes: `_INT8_IDENTITY`, `_INT8_DIM` (Task 1); `_Int8Embedder` (Task 3); `_INT8_THRESHOLD`
  (Task 4).
- Produces: `matcher_model: int8` is now a fully working, selectable backend everywhere
  (`active_model_name`, `active_threshold`, `_get_model`, `apply_swap`, `detect_stages`).

- [ ] **Step 1: Write the failing tests**

Add to `tests/test_matcher_backend.py`:

```python
# --- int8 wired into the dispatch (fallback, swap, modules) ----------------


def test_fallback_to_lexical_when_int8_package_missing(monkeypatch, caplog):
    import importlib.util
    import logging

    from opendaisugi import _search

    _set_matcher("int8")
    real_find_spec = importlib.util.find_spec

    def _blocked(name, *a, **k):
        if name == "onnxruntime":
            return None
        return real_find_spec(name, *a, **k)

    monkeypatch.setattr(importlib.util, "find_spec", _blocked)
    _search._reset_embedder_cache()

    with caplog.at_level(logging.WARNING, logger="opendaisugi._search"):
        assert _search.active_model_name() == "lexical-hash-v1"
        assert _search.active_threshold() == pytest.approx(0.25)
        model = _search._get_model()

    assert isinstance(model, _search._LexicalEmbedder)
    hits = [r for r in caplog.records if "needs opendaisugi[int8]" in r.message]
    assert len(hits) == 1, f"expected exactly one warning, got {len(hits)}: {hits}"


def test_int8_never_imports_torch():
    import sys

    if not _int8_reachable():
        pytest.skip("int8 model not cached and no network")
    _set_matcher("int8")
    from opendaisugi import _search

    _search._reset_embedder_cache()
    model = _search._get_model()
    model.encode(["a quick smoke check"])
    assert "torch" not in sys.modules


def _int8_reachable() -> bool:
    """True if the pinned int8 model is already cached, or the network is
    reachable to fetch it. Mirrors the skip condition these tests need
    without forcing a download just to decide whether to run."""
    import os
    import urllib.request
    from pathlib import Path

    if os.environ.get("OPENDAISUGI_INT8_MODEL"):
        return True
    cache = Path.home() / ".cache" / "opendaisugi" / "models" / "minilm-int8"
    if cache.exists() and any(cache.iterdir()):
        return True
    try:
        urllib.request.urlopen("https://huggingface.co", timeout=3)
        return True
    except OSError:
        return False


@pytest.mark.skipif(not _int8_reachable(), reason="int8 model not cached and no network")
def test_int8_shape_repeatable_and_l2_normalized():
    """The real model (not the Task 3 fakes): same hygiene checks the lexical
    embedder gets — shape, repeatability, L2 norm — reusing the same task
    string the lexical test uses."""
    _set_matcher("int8")
    from opendaisugi import _search

    _search._reset_embedder_cache()
    model = _search._get_model()
    out = model.encode(["find stale tmp files", "find stale tmp files"], convert_to_numpy=True)
    assert out.shape == (2, 384)
    assert np.allclose(out[0], out[1])  # same text -> identical vector
    assert np.linalg.norm(out[0]) == pytest.approx(1.0, abs=1e-4)


@pytest.mark.skipif(not _int8_reachable(), reason="int8 model not cached and no network")
def test_int8_paraphrase_clears_threshold_unrelated_does_not():
    _set_matcher("int8")
    from opendaisugi import _search

    _search._reset_embedder_cache()
    model = _search._get_model()

    a, b = model.encode(
        [
            "clean up stale tmp files in the build directory",
            "remove old temporary files from the build folder",
        ]
    )
    assert float(np.dot(a, b)) >= _search._INT8_THRESHOLD

    c, d = model.encode(["rebuild the android apk", "write the ADR for the gateway"])
    assert float(np.dot(c, d)) < _search._INT8_THRESHOLD


def test_swap_allows_int8(tmp_path):
    from opendaisugi.swap import apply_swap

    cfg = apply_swap("matcher", "int8 / fp16 onnx", config_path=tmp_path / "config.yaml")
    assert cfg.matcher_model == "int8"


def test_modules_map_marks_int8_state_honestly(tmp_path):
    # onnxruntime is installed in the dev/test env (opendaisugi[int8]), so
    # int8 is a real swap target on any CPU these tests run on — not the
    # POSSIBLE placeholder anymore.
    from opendaisugi.modules import ACTIVE, AVAILABLE, detect_stages

    stages = {s.key: s for s in detect_stages(tmp_path)}
    matcher = {m.name: m for m in stages["matcher"].modules}
    assert matcher["int8 / fp16 onnx"].state in (ACTIVE, AVAILABLE)
    distill = {m.name: m for m in stages["distill"].modules}
    assert distill["int8 (onnx, no torch)"].state in (ACTIVE, AVAILABLE)
```

Replace `test_lexical_identity_and_threshold` (original lines 122-127) with a parametrized version
covering both static (no-model-load) backends:

```python
@pytest.mark.parametrize(
    "matcher_key, expected_identity, expected_threshold",
    [
        ("lexical", "lexical-hash-v1", 0.25),
        ("int8", "all-MiniLM-L6-v2-int8", 0.45),
    ],
)
def test_static_backend_identity_and_threshold(matcher_key, expected_identity, expected_threshold):
    """Both lexical and int8 resolve identity/threshold WITHOUT loading a
    model — the invariant active_model_name/active_threshold never load
    (ADR-0018/0021)."""
    from opendaisugi._search import active_model_name, active_threshold

    _set_matcher(matcher_key)
    assert active_model_name() == expected_identity
    assert active_threshold() == pytest.approx(expected_threshold)
```

Delete these three tests entirely (there is no unbuilt matcher left to exercise their refusal
paths against; the refusal code stays, dormant, for the next placeholder):
`test_unbuilt_backends_refuse_at_identity` (lines 114-119), `test_swap_refuses_unbuilt_matcher_
without_writing_config` (lines 502-511), `test_swap_confirmation_reports_refusal_without_crashing`
(lines 514-518).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -q`
Expected: the new/parametrized tests FAIL (`int8` still raises `MatcherNotAvailable` at
`active_model_name`; the swap still refuses; the module map still says `POSSIBLE`). The three
deleted tests are gone from the run entirely — confirm they no longer appear in the output.

- [ ] **Step 3: Wire `_search.py`'s dispatch**

In `src/opendaisugi/_search.py`, update `_FALLBACK_PACKAGE` (original lines 162-165):

```python
_FALLBACK_PACKAGE: dict[str, tuple[str, str]] = {
    _MODEL_NAME: ("sentence_transformers", "opendaisugi[search]"),
    "potion": ("model2vec", "opendaisugi[potion]"),
    "int8": ("onnxruntime", "opendaisugi[int8]"),
}
```

Update the comment inside `effective_matcher` (originally `# unbuilt (int8): MatcherNotAvailable
below, not a fallback`) to `# an unbuilt future backend: MatcherNotAvailable below, not a
fallback` — no backend is unbuilt today.

Update `active_model_name`'s body (originally lines 224-234):

```python
def active_model_name(config=None) -> str:
    from opendaisugi.exceptions import MatcherNotAvailable

    key = effective_matcher(config)
    if key == _MODEL_NAME:
        return _MODEL_NAME
    if key == "potion":
        return resolve_potion_model()
    if key == "lexical":
        return _LEXICAL_IDENTITY
    if key == "int8":
        return _INT8_IDENTITY
    raise MatcherNotAvailable(
        f"matcher_model={key!r} is not a built embedder. "
        f"Built: 'all-MiniLM-L6-v2' (torch), 'potion' (torch-free), "
        f"'lexical' (no model), 'int8' (onnx, no torch)."
    )
```

Update `active_threshold`'s body (originally lines 244-253):

```python
def active_threshold(config=None) -> float:
    from opendaisugi.exceptions import MatcherNotAvailable

    key = effective_matcher(config)
    if key == "potion":
        return _POTION_THRESHOLD
    if key == _MODEL_NAME:
        from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

        return DEFAULT_PATHWAY_THRESHOLD
    if key == "lexical":
        return _LEXICAL_THRESHOLD
    if key == "int8":
        return _INT8_THRESHOLD
    raise MatcherNotAvailable(f"matcher_model={key!r} is not a built embedder.")
```

Update `_get_model`'s dispatch (originally lines 381-386):

```python
    if key == _MODEL_NAME:
        emb = _STEmbedder()
    elif key == "lexical":
        emb = _LexicalEmbedder()
    elif key == "int8":
        emb = _Int8Embedder()
    else:  # key == "potion" (active_model_name already rejected anything else)
        emb = _PotionEmbedder(ident)
```

- [ ] **Step 4: Update `swap.py`**

In `src/opendaisugi/swap.py`, change line 58:

```python
_UNBUILT_MATCHERS: frozenset[str] = frozenset()
```

And update the matcher `SwapKnob`'s int8 option (line 123):

```python
(SwapOption("int8 / fp16 onnx", "int8", "quantized MiniLM, ~23MB, no torch"),)
```

- [ ] **Step 5: Update `modules.py`**

Add a helper near `_have` (after line 52):

```python
def _int8_supported() -> bool:
    """Package present AND this CPU has a matching pinned build (ADR-0021)."""
    if not _have("onnxruntime"):
        return False
    from opendaisugi._search import _detect_int8_variant
    from opendaisugi.exceptions import MatcherNotAvailable

    try:
        _detect_int8_variant()
    except MatcherNotAvailable:
        return False
    return True
```

In `detect_stages`, add alongside `potion_installed = _have("model2vec")` (line 96):

```python
    int8_installed = _int8_supported()
```

Replace the matcher stage's int8 `Module` (line 219):

```python
(
    Module(
        "int8 / fp16 onnx",
        _emb_state("int8", int8_installed),
        "onnx, ~23MB, no torch" if int8_installed else "needs opendaisugi[int8]",
    ),
)
```

Add a matching entry to the distill stage's module list (after the `potion (torch-free)` `Module`,
before `lexical (no model)`):

```python
(
    Module(
        "int8 (onnx, no torch)",
        _emb_state("int8", int8_installed),
        "onnx clustering" if int8_installed else "needs opendaisugi[int8]",
    ),
)
```

- [ ] **Step 6: Update `config.py`'s doc comment**

Replace the `matcher_model` comment block (lines 77-83):

```python
    # Pathway-reuse embedder. all-MiniLM-L6-v2 (torch), potion (model2vec,
    # torch-free — runs where torch can't), lexical (stdlib + numpy, no
    # model, no download), and int8 (onnxruntime, quantized MiniLM, no
    # torch) are all built. A missing package for MiniLM/potion/int8 falls
    # back to lexical (warned once) rather than silently distilling zero
    # pathways. Switching backends changes the embedding space; re-run
    # `tend`. See ADR-0018, ADR-0019, ADR-0021.
    matcher_model: str = "all-MiniLM-L6-v2"  # | potion | lexical | int8
```

- [ ] **Step 7: Add the `[int8]` extra**

In `pyproject.toml`, after the `potion = [...]` block and before `mcp = [`, add:

```toml
# v0.46: the second torch-free pathway-reuse embedder — MiniLM itself,
# quantized to int8 and run through onnxruntime (no torch, no transformers).
# Needs a CPU with AVX2 or better, or aarch64; refuses honestly on an older
# CPU rather than silently falling back. Fetches ~23MB from the official
# sentence-transformers repo on first use, pinned by sha256. MIT. Absent =>
# selecting int8 degrades to a warned lexical fallback, exactly as a missing
# [search]/[potion] does. See ADR-0021.
int8 = [
    "onnxruntime>=1.18",
    "tokenizers>=0.19",
    "numpy>=1.26",
]
```

- [ ] **Step 8: Run the full test suite**

Run: `uv run --no-sync pytest tests/test_matcher_backend.py -q`
Expected: PASS — every int8 test from Tasks 1-5 green, the parametrized
`test_static_backend_identity_and_threshold` green for both `lexical` and `int8`, and the three
deleted tests absent from the run.

Run: `uv run --no-sync pytest -q`
Expected: full suite PASS (no regression in the other matcher/swap/modules tests, which still
exercise `all-MiniLM-L6-v2`/`potion`/`lexical` unchanged).

Run: `uv run --no-sync ruff check .`
Expected: clean.

- [ ] **Step 9: Commit**

```bash
git add src/opendaisugi/_search.py src/opendaisugi/swap.py src/opendaisugi/modules.py src/opendaisugi/config.py pyproject.toml tests/test_matcher_backend.py
git commit -m "$(cat <<'EOF'
Wire int8 into the matcher dispatch, swap menu, and module map

matcher_model=int8 now resolves through active_model_name/active_threshold/
_get_model exactly like all-MiniLM-L6-v2/potion/lexical. _UNBUILT_MATCHERS
is empty: the swap menu accepts int8, daisugi modules marks it honestly
(ACTIVE/AVAILABLE by package + CPU support, POSSIBLE otherwise), and the
refusal tests that existed only to prove int8 was unbuilt are deleted — the
refusal code stays for the next placeholder. Closes the last gap ADR-0018/
0019 named. See ADR-0021.
EOF
)"
```

---

## Post-plan state

`_UNBUILT_MATCHERS` is `frozenset()`. Every entry in the matcher swap menu
(`all-MiniLM-L6-v2`, `potion`, `lexical`, `int8`) is a real, working, independently-threshold-ed
embedder. `daisugi modules` reports int8 honestly for both the matcher and distill stages. Spec
`spec-11-layer-management.md` (which benches the matcher line) can now include int8 in its
comparison table without a caveat.
