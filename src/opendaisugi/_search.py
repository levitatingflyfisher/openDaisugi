"""Lazy-loaded semantic search for opendaisugi journal traces.

This module is ONLY imported inside ``Journal.search()`` — it must never
be reachable from ``import opendaisugi``. That way, users without the
``[search]`` extra installed never pay the import cost of
``sentence-transformers`` / ``torch``.
"""

from __future__ import annotations

import contextlib
import hashlib
import logging
import math
import re
import warnings
from collections import Counter
from pathlib import Path
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from opendaisugi.journal import Journal


_MODEL_NAME = "all-MiniLM-L6-v2"

# The torch-free alternative (ADR-0018). model2vec inference needs only numpy +
# tokenizers, so it runs where sentence-transformers' torch slice can't. Default
# is potion-base-8M (measured ~30MB on disk, dim 256): at a threshold FPR-matched
# to MiniLM's it gives ~2x the paraphrase recall of potion-retrieval-32M and is 4x
# smaller. Override the concrete model — a HF id, or a local dir for air-gapped
# use with HF_HUB_OFFLINE=1 — via OPENDAISUGI_POTION_MODEL.
_POTION_DEFAULT = "minishlab/potion-base-8M"

# Per-backend reuse threshold. potion's is FPR-matched to MiniLM@0.55 on the real
# journal corpus (both hit ~2.6% false-match on random task pairs); see ADR-0018.
_POTION_THRESHOLD = 0.59

# The zero-model, zero-download floor (ADR-0019): signed feature hashing over
# unigrams, no package beyond numpy, never touches the network. Measured on the
# real journal corpus (740 rows, 258 distinct tasks): unigram-only + stopwords
# gives the best FPR-matched paraphrase recall of the featurizations tried
# (0.70 vs 0.60-0.70 for bigram variants). Bump the identity suffix if the
# feature scheme ever changes — it is the provenance stamp.
_LEXICAL_IDENTITY = "lexical-hash-v1"
_LEXICAL_DIM = 4096
_LEXICAL_THRESHOLD = 0.25
_LEXICAL_STOPWORDS = frozenset(
    "a an the and or of to in on for with by from at as is are be this that "
    "it its into via using use run make add fix update".split()
)
_LEXICAL_TOKEN_RE = re.compile(r"[a-z0-9_]+")

# The second torch-free backend. See ADR-0021. This is MiniLM itself,
# quantized to int8 and exported to ONNX by the sentence-transformers
# repository, run through onnxruntime. It is the closest torch-free
# approximation of the default backend's own embedding space. It still gets
# its own identity and threshold. Close is not the same space.
#
# The manifest is pinned data from the Hugging Face tree API, which reports
# the sha256 of each LFS file. tokenizer.json is below the LFS threshold, so
# its sha256 comes from the downloaded file itself. Three of the four onnx
# builds, vnni, avx512, and arm64, share one sha256. The CPU dispatch still
# names all four, because the upstream files can diverge later.
_INT8_BASE_URL = "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/"
_INT8_IDENTITY = "all-MiniLM-L6-v2-int8"
_INT8_DIM = 384
# Per-backend reuse threshold. See ADR-0021. Derived the same way ADR-0018
# and ADR-0019 derived potion's and lexical's: `uv run --no-sync python
# scripts/matcher_fpr.py` against the real journal, 258 distinct usable
# tasks, 20,000 random distinct-task pairs, seed 42, closest to the 2.56%
# false-match rate of MiniLM at 0.55. Measured on the `avx2` build.
_INT8_THRESHOLD = 0.45
_INT8_FILES: dict[str, tuple[str, str, int]] = {
    # Key: a variant or the tokenizer. Value: path under _INT8_BASE_URL, sha256, size in bytes.
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
    """x86 CPU feature flags from the first ``flags`` line of /proc/cpuinfo.

    Returns an empty set on a host with no such file, for example macOS,
    Windows, or a container without /proc, and on a file with no ``flags``
    line. It never raises, so CPU detection treats "no flags found" the same
    as "no matching flags".
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
    """Return the key into ``_INT8_FILES`` that this CPU can run.

    aarch64 always gets the arm64 build. x86 matches the most capable flag
    first, VNNI, then AVX-512, then AVX2, so a VNNI CPU gets the VNNI build.
    Tests inject ``machine`` and ``flags``. Real callers pass neither. Raises
    ``MatcherNotAvailable`` when nothing matches: an older or unusual CPU has
    no published int8 build for this model. potion and lexical have no such
    floor.
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


def _int8_cache_dir() -> Path:
    """Where the pinned int8 files live.

    ``OPENDAISUGI_INT8_MODEL`` names a pre-fetched directory for offline
    installs, the same way potion's ``OPENDAISUGI_POTION_MODEL`` does. When
    it is unset, the files live in the shared model cache.
    """
    import os

    override = os.environ.get("OPENDAISUGI_INT8_MODEL")
    if override:
        return Path(override)
    return Path.home() / ".cache" / "opendaisugi" / "models" / "minilm-int8"


class _Int8Embedder:
    """all-MiniLM-L6-v2, quantized to int8, run through onnxruntime. No torch.

    See ADR-0021. A missing onnxruntime or tokenizers package is a fallback
    case. ``effective_matcher`` handles it before this class is constructed,
    the same as potion. A CPU with no matching build, a fetch failure, or a
    file that fails its sha256 check are not fallback cases. The operator
    chose int8, so this raises ``MatcherNotAvailable`` with the fix instead
    of a silent downgrade that mislabels provenance.
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
                f"the int8 model could not be fetched: {exc}. Pre-fetch it and "
                f"set OPENDAISUGI_INT8_MODEL=<local dir>, or set matcher_model: "
                f"lexical for a zero-download matcher."
            ) from exc

        try:
            self._tok = Tokenizer.from_file(str(tok_path))
            self._tok.enable_truncation(max_length=256)
            pad_id = self._tok.token_to_id("[PAD]")
            self._tok.enable_padding(pad_id=pad_id if pad_id is not None else 0, pad_token="[PAD]")
            self._session = ort.InferenceSession(
                str(model_path), providers=["CPUExecutionProvider"]
            )
        except Exception as exc:  # noqa: BLE001 - a verified file can still fail to load
            raise MatcherNotAvailable(
                f"the int8 files in {cache_dir} could not be loaded: {exc}. Delete "
                f"that directory to re-fetch, point OPENDAISUGI_INT8_MODEL at a "
                f"good copy, or set matcher_model: lexical."
            ) from exc
        outputs = self._session.get_outputs()
        if not outputs or len(outputs[0].shape) != 3:
            raise MatcherNotAvailable(
                f"{model_path.name} does not expose token-level states as its first "
                f"output. Re-fetch the pinned files or set matcher_model: lexical."
            )
        self._output_name = outputs[0].name

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
            token_embeddings = self._session.run(
                [self._output_name],
                {
                    "input_ids": input_ids,
                    "attention_mask": attention_mask,
                    "token_type_ids": token_type_ids,
                },
            )[0]
            mask = attention_mask[..., None].astype(np.float32)
            summed = (token_embeddings * mask).sum(axis=1)
            counts = np.clip(mask.sum(axis=1), 1e-9, None)
            pooled.append(summed / counts)
        return _l2(np.concatenate(pooled, axis=0))


# Cached embedders, keyed by their identity string (so switching backends — or
# potion models — in one process loads the right one, never a stale one).
_embedder_cache: dict[str, Any] = {}

# Third-party loggers that print load-time chatter the operator does not need:
# transformers' "BertModel LOAD REPORT" (a logger.warning), and huggingface_hub's
# "unauthenticated requests to the HF Hub" line.
_NOISY_LOGGERS = ("transformers", "huggingface_hub", "sentence_transformers")


@contextlib.contextmanager
def _quiet_model_load():
    """Silence third-party load-time chatter for the duration of a model load.

    The embedder load prints an HF-hub warning, transformers' multi-line LOAD REPORT,
    and torch's GPU-capability warning — all informational, none actionable by the
    operator (clig: route engine chatter to a log, not the user). This mutes those
    loggers and load-time warnings only for the load, then restores every level so
    the rest of the process is unaffected.
    """
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        # Import the noisy libs FIRST: transformers/huggingface_hub configure their own
        # logging at import time, which would clobber a setLevel done before the import.
        # Importing them here (they are cached for the load below) means our ERROR level
        # sticks through the actual model load, where the LOAD REPORT and HF line fire.
        for _mod in ("transformers", "huggingface_hub"):
            with contextlib.suppress(Exception):
                __import__(_mod)
        # The "Loading weights" bar prints even into a pipe (transformers' bar does not
        # honor TTY here). It is a cached, sub-second load with no actionable progress —
        # disable it so redirected/piped output stays clean.
        with contextlib.suppress(Exception):
            import transformers

            transformers.logging.disable_progress_bar()
        prev = {name: logging.getLogger(name).level for name in _NOISY_LOGGERS}
        for name in _NOISY_LOGGERS:
            logging.getLogger(name).setLevel(logging.ERROR)
        try:
            yield
        finally:
            for name, level in prev.items():
                logging.getLogger(name).setLevel(level)


def _embedder_device() -> str:
    """Return the device the embedder should load on: ``"cuda"`` only when this
    torch build can actually run this GPU, else ``"cpu"``.

    This is deliberately NOT a blanket force to CPU. Sentence-transformers with no
    device grabs any visible GPU; on a card whose compute capability is not in the
    installed torch build (e.g. a Pascal ``sm_61`` GTX 10-series against a build for
    ``sm_75+``), that later crashes with ``CUDA error: no kernel image is available``.
    We use the GPU where it genuinely works and fall back to CPU only where it cannot,
    so machines with a supported GPU keep the acceleration.
    """
    try:
        import torch
    except Exception:
        return "cpu"
    try:
        if not torch.cuda.is_available():
            return "cpu"
        major, minor = torch.cuda.get_device_capability()
        if f"sm_{major}{minor}" in torch.cuda.get_arch_list():
            return "cuda"
    except Exception:
        return "cpu"
    return "cpu"


def _active_config():
    """Load config from the isolated data dir, not a bare ``Path.home()``.

    Resolving through ``DEFAULT_DATA_DIR`` means the test suite's
    ``_isolate_default_data_dir`` fixture keeps embedder selection hermetic — a
    dev box whose real ``~/.opendaisugi/config.yaml`` sets ``matcher_model:
    potion`` must not leak that into unrelated tests.
    """
    from opendaisugi import DEFAULT_DATA_DIR
    from opendaisugi.config import load_config

    return load_config(DEFAULT_DATA_DIR / "config.yaml")


def selected_matcher(config=None) -> str:
    """The configured ``matcher_model`` key (e.g. ``all-MiniLM-L6-v2``, ``potion``).

    This is what the operator recorded, not necessarily what runs — see
    ``effective_matcher`` for the package-absence fallback.
    """
    cfg = config if config is not None else _active_config()
    return cfg.matcher_model


def resolve_potion_model() -> str:
    """The concrete potion model: an env override (HF id or local dir) or the default."""
    import os

    return os.environ.get("OPENDAISUGI_POTION_MODEL", _POTION_DEFAULT)


# Package required for each built backend that needs one, and the extra that
# provides it. Keyed on the config value, not the identity — potion's identity
# is the resolved model id, which varies. Used only by effective_matcher's
# fallback: absence of the PACKAGE degrades to lexical; a package that IS
# installed but whose model fails to load is a different failure (no fallback
# — see _PotionEmbedder).
_FALLBACK_PACKAGE: dict[str, tuple[str, str]] = {
    _MODEL_NAME: ("sentence_transformers", "opendaisugi[search]"),
    "potion": ("model2vec", "opendaisugi[potion]"),
    "int8": ("onnxruntime", "opendaisugi[int8]"),
}

# Backends warned about falling back to lexical, this process — so the operator
# sees the guidance once per key, not once per active_*()/_get_model() call.
_fallback_warned: set[str] = set()

_log = logging.getLogger("opendaisugi._search")


def effective_matcher(config=None) -> str:
    """The embedder that actually runs: ``selected_matcher`` after the
    package-absence fallback (ADR-0019).

    Without this, a missing ``[search]``/``[potion]`` package silently degrades
    every reuse call to zero pathways — the gap ADR-0018 named but didn't close.
    Falling back to ``lexical`` (stdlib + numpy, always importable) means a
    fresh, extras-free install still distills and reuses pathways. This is a
    PACKAGE-ABSENCE fallback only: a potion model that fails to *load* (offline,
    not cached) is a different failure and must not fall back — the operator
    chose potion; see ``_PotionEmbedder``. ``active_model_name``,
    ``active_threshold``, and ``_get_model`` all resolve through this, so
    identity/threshold/the loaded model can never disagree.
    """
    import importlib.util

    key = selected_matcher(config)
    if key == "lexical":
        return "lexical"
    pkg_extra = _FALLBACK_PACKAGE.get(key)
    if pkg_extra is None:
        return key  # an unbuilt future backend: MatcherNotAvailable below, not a fallback
    pkg, extra = pkg_extra
    if importlib.util.find_spec(pkg) is not None:
        return key
    if key not in _fallback_warned:
        _fallback_warned.add(key)
        _log.warning(
            "matcher_model=%r needs %s; using the lexical matcher instead. "
            "Set matcher_model: lexical to make this the recorded choice.",
            key,
            extra,
        )
    return "lexical"


def active_model_name(config=None) -> str:
    """Identity of the active embedder, for pathway provenance — no model load.

    Must equal the identity a distilled pathway is stamped with, so the
    stale-embedding guard in ``PathwayStore.find`` never compares vectors across
    incompatible embedding spaces. For potion this is the concrete model id, so
    switching potion models (or pointing at a local dir) invalidates old rows.
    Resolves through ``effective_matcher``, not ``selected_matcher`` — a
    package-absence fallback must stamp ``lexical-hash-v1``, not the name of a
    backend that never actually loaded.
    """
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
        f"Built: 'all-MiniLM-L6-v2' with torch, 'potion' with no torch, "
        f"'lexical' with no model, 'int8' with onnx and no torch."
    )


def active_threshold(config=None) -> float:
    """Per-backend cosine reuse threshold. ADR-0018, ADR-0019, ADR-0021.

    Resolves through ``effective_matcher`` — see ``active_model_name``.
    """
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


def _l2(arr):
    """Row-wise L2-normalize. The structure_weight concat in the distiller weights
    a sum of per-component distances *under normalized inputs* — normalizing here
    keeps that invariant true for every backend, not by accident for one."""
    import numpy as np

    a = np.asarray(arr, dtype=float)
    n = np.linalg.norm(a, axis=-1, keepdims=True)
    return a / np.clip(n, 1e-12, None)


class _STEmbedder:
    """all-MiniLM-L6-v2 via sentence-transformers (needs the [search] torch slice)."""

    def __init__(self):
        from sentence_transformers import SentenceTransformer

        with _quiet_model_load():
            self._m = SentenceTransformer(_MODEL_NAME, device=_embedder_device())

    def encode(self, texts, convert_to_numpy=True):
        return _l2(self._m.encode(list(texts), convert_to_numpy=True))


class _PotionEmbedder:
    """A model2vec static embedder — numpy + tokenizers, no torch (ADR-0018).

    A missing ``model2vec`` package is a fallback case (``effective_matcher``
    degrades to lexical before this class is ever constructed). A model that
    fails to *load* — offline, not cached, bad id — is NOT: the operator chose
    potion, so this re-raises as ``MatcherNotAvailable`` with deterministic
    guidance instead of silently falling back and stamping the wrong provenance.
    """

    def __init__(self, model_id):
        try:
            from model2vec import StaticModel
        except ImportError as exc:
            raise ImportError(
                "the 'potion' matcher needs model2vec (torch-free): "
                "pip install 'opendaisugi[potion]'"
            ) from exc

        try:
            self._m = StaticModel.from_pretrained(model_id)
        except Exception as exc:
            from opendaisugi.exceptions import MatcherNotAvailable

            raise MatcherNotAvailable(
                f"potion model {model_id!r} could not be loaded ({exc}). "
                f"Pre-fetch it and set OPENDAISUGI_POTION_MODEL=<local dir>, "
                f"or set matcher_model: lexical for a zero-download matcher."
            ) from exc

    def encode(self, texts, convert_to_numpy=True):
        return _l2(self._m.encode(list(texts)))


def _lexical_vector(text: str):
    """Signed feature-hashed unigram vector for one text (ADR-0019).

    Tokenize lowercase ``[a-z0-9_]+`` runs, drop stopwords, hash each surviving
    token with blake2b (not Python's per-process-salted ``hash()`` — determinism
    across processes is a contract), bucket into ``_LEXICAL_DIM`` dims with a
    sign from the hash's top bit, and weight by sqrt(term count) so a repeated
    word doesn't linearly dominate. Unnormalized — ``_l2`` normalizes the batch.
    """
    import numpy as np

    tokens = [t for t in _LEXICAL_TOKEN_RE.findall(text.lower()) if t not in _LEXICAL_STOPWORDS]
    vec = np.zeros(_LEXICAL_DIM, dtype=float)
    for token, count in Counter(tokens).items():
        h = int(hashlib.blake2b(token.encode(), digest_size=8).hexdigest(), 16)
        sign = 1.0 if h >> 63 else -1.0
        vec[h % _LEXICAL_DIM] += sign * math.sqrt(count)
    return vec


class _LexicalEmbedder:
    """Zero-model, zero-download embedder: signed feature hashing (ADR-0019).

    Pure stdlib + numpy — never imports model2vec / sentence-transformers /
    huggingface_hub / transformers, and never touches the network. This is the
    honest floor: weaker than a real embedding model on paraphrase (see the
    ADR's measured table), but a fresh install with none of the optional
    embedder extras can still distill and reuse pathways.
    """

    def encode(self, texts, convert_to_numpy=True):
        import numpy as np

        texts = list(texts)
        if not texts:
            return np.zeros((0, _LEXICAL_DIM))
        return _l2(np.array([_lexical_vector(t) for t in texts], dtype=float))


def reload() -> None:
    """Drop cached embedders and the fallback-warned set.

    The matcher choice is already read per call: ``active_model_name``,
    ``active_threshold`` and ``_get_model`` re-resolve the config every time,
    and the cache is keyed by identity. What this frees is the old embedder
    itself, so a long-lived process does not keep a model it no longer uses,
    and it re-arms the once-per-process fallback warning so the next call
    says again that a fallback applies. Stored pathways embedded under the
    old identity stay excluded from ``find`` until ``tend`` re-embeds them.
    """
    _embedder_cache.clear()
    _fallback_warned.clear()


# The old private name. Several test modules call it.
_reset_embedder_cache = reload


def _get_model(config=None):
    """Return the cached embedder for the configured (effective) backend.

    Uniform interface: ``.encode(texts, convert_to_numpy=True) -> np.ndarray``,
    L2-normalized. The heavy import, sentence-transformers, model2vec, or
    onnxruntime, is deferred into the adapter so modules that only need
    ``_MODEL_NAME`` or ``active_model_name`` can import this file with no
    extra installed. Raises ``ImportError`` that teaches the extra when the
    backend's package is absent. Raises ``MatcherNotAvailable`` when the
    backend is not built, when potion's package is present but its model
    failed to load, or when int8 has no build for this CPU or cannot fetch
    its files.
    """
    key = effective_matcher(config)
    ident = active_model_name(config)  # raises MatcherNotAvailable for unbuilt keys
    cached = _embedder_cache.get(ident)
    if cached is not None:
        return cached
    if key == _MODEL_NAME:
        emb = _STEmbedder()
    elif key == "lexical":
        emb = _LexicalEmbedder()
    elif key == "int8":
        emb = _Int8Embedder()
    else:  # key == "potion"; active_model_name already rejected anything else
        emb = _PotionEmbedder(ident)
    _embedder_cache[ident] = emb
    return emb


def semantic_search(journal: "Journal", query: str, *, limit: int) -> list:
    """Rank traces by cosine similarity between ``query`` and trace tasks.

    Returns a list of ``Trace`` metadata rows, highest-similarity first.
    """
    import numpy as np

    from opendaisugi._similarity import cosine_similarity_batch

    model = _get_model()
    traces = journal.list_recent(limit=10_000)
    if not traces:
        return []

    task_vecs = model.encode([t.task for t in traces], convert_to_numpy=True)
    query_vec = model.encode([query], convert_to_numpy=True)[0]

    scores = cosine_similarity_batch(query_vec, task_vecs)
    order = np.argsort(-scores)
    return [traces[i] for i in order[:limit]]
