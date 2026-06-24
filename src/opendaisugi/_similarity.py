"""Cosine similarity helpers.

Extracted so the three near-identical blocks across ``_search``,
``pathway_store``, and ``gardener.merger`` share one source of truth.
The zero-norm guard mirrors the historical behavior at every callsite.
"""

from __future__ import annotations

from typing import Any


def cosine_similarity(a: Any, b: Any) -> float:
    """Scalar cosine similarity between two 1-D vectors (list or ndarray)."""
    import numpy as np

    va = np.asarray(a, dtype=float)
    vb = np.asarray(b, dtype=float)
    denom = (np.linalg.norm(va) * np.linalg.norm(vb)) or 1e-9
    return float(va @ vb / denom)


def cosine_similarity_batch(query: Any, candidates: Any) -> Any:
    """Cosine similarity of ``query`` (1-D) against a 2-D stack.

    Returns a 1-D ndarray of scores aligned to ``candidates`` rows.
    """
    import numpy as np

    q = np.asarray(query, dtype=float)
    c = np.asarray(candidates, dtype=float)
    q_norm = np.linalg.norm(q)
    c_norms = np.linalg.norm(c, axis=1)
    denom = c_norms * q_norm
    denom = np.where(denom == 0, 1e-9, denom)
    return (c @ q) / denom
