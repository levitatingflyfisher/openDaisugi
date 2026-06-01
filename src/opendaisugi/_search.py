"""Lazy-loaded semantic search for opendaisugi journal traces.

This module is ONLY imported inside ``Journal.search()`` — it must never
be reachable from ``import opendaisugi``. That way, users without the
``[search]`` extra installed never pay the import cost of
``sentence-transformers`` / ``torch``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from opendaisugi.journal import Journal


_MODEL_NAME = "all-MiniLM-L6-v2"
_model: Any | None = None


def _get_model():
    """Return a cached SentenceTransformer instance.

    First call downloads ~80MB from HuggingFace — subsequent calls are free.
    The sentence_transformers import is deferred so callers that never
    invoke search-path code don't pay the ~torch import cost, and modules
    that only need ``_MODEL_NAME`` can import this file without the
    ``[search]`` extra installed.
    """
    global _model
    if _model is None:
        from sentence_transformers import SentenceTransformer
        _model = SentenceTransformer(_MODEL_NAME)
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
