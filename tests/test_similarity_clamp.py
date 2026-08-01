"""Cosine similarity must stay in [-1, 1] despite float rounding.

Regression: an identical/near-identical embedding pair can score
1.0000000000000002 due to floating-point error, which then fails
``PathwayMatch.similarity`` (bounded ``le=1.0``) with a ValidationError and
breaks ``pathway_store.find()`` on a self-match. Seed 8 at 384 dims (an
embedding-sized vector) reproduces the overshoot deterministically.
"""

from __future__ import annotations

import numpy as np

from opendaisugi._similarity import cosine_similarity, cosine_similarity_batch


def _overshooting_vector() -> np.ndarray:
    return np.random.default_rng(8).random(384)


def test_scalar_cosine_never_exceeds_one():
    v = _overshooting_vector()
    assert cosine_similarity(v, v) <= 1.0
    assert cosine_similarity(v, v) == 1.0  # clamped exactly, not 1.0000000000000002


def test_batch_cosine_never_exceeds_one():
    v = _overshooting_vector()
    score = float(cosine_similarity_batch(v, np.array([v]))[0])
    assert score <= 1.0


def test_cosine_stays_at_or_above_minus_one():
    v = _overshooting_vector()
    assert cosine_similarity(v, -v) >= -1.0


def test_clamped_self_similarity_passes_pathway_match_bound():
    # The root fix: a self-match score must satisfy PathwayMatch's ``le=1.0``
    # bound (it used to raise ValidationError at 1.0000000000000002). The
    # find()→PathwayMatch integration itself is covered by test_pathway_store.
    from opendaisugi.pathway import PathwayMatch

    v = _overshooting_vector()
    score = cosine_similarity(v, v)
    assert score <= 1.0
    # Pydantic accepts the clamped ceiling for the field in isolation.
    field = PathwayMatch.model_fields["similarity"]
    le = next(m.le for m in field.metadata if hasattr(m, "le"))
    assert score <= le
