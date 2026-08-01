"""Lazy-loaded semantic search for opendaisugi journal traces.

This module is ONLY imported inside ``Journal.search()`` — it must never
be reachable from ``import opendaisugi``. That way, users without the
``[search]`` extra installed never pay the import cost of
``sentence-transformers`` / ``torch``.
"""

from __future__ import annotations

import contextlib
import logging
import warnings
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from opendaisugi.journal import Journal


_MODEL_NAME = "all-MiniLM-L6-v2"
_model: Any | None = None

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


def _get_model():
    """Return a cached SentenceTransformer instance.

    First call downloads ~80MB from HuggingFace — subsequent calls are free.
    The sentence_transformers import is deferred so callers that never
    invoke search-path code don't pay the ~torch import cost, and modules
    that only need ``_MODEL_NAME`` can import this file without the
    ``[search]`` extra installed. The device is chosen by ``_embedder_device``
    so an unsupported GPU can't crash the load.
    """
    global _model
    if _model is None:
        with _quiet_model_load():
            from sentence_transformers import SentenceTransformer

            _model = SentenceTransformer(_MODEL_NAME, device=_embedder_device())
    return _model


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
