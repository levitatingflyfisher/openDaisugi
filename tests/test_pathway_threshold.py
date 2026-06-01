"""v0.29: pathway retrieval threshold is calibrated and plumbed through the public API.

Calibration (scratchpad/threshold-calibration.md): on the shipped all-MiniLM-L6-v2 embedder,
same-task paraphrases score cosine ~0.5 (mean) while *different* tasks max out ~0.29. The old
0.85 default therefore retrieved nothing but near-verbatim restatements — the documented
"value-killer" for the token-savings path. The default is now DEFAULT_PATHWAY_THRESHOLD and is
overridable per-Daisugi and per-call.
"""

import asyncio
import time

import numpy as np

from opendaisugi import Daisugi
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD, PathwayStore


def _pathway(embedding) -> CompiledPathway:
    from opendaisugi._search import _MODEL_NAME
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    return CompiledPathway(
        id="pathway_para01",
        task_description="run the tests",
        task_embedding=embedding,
        embedding_model=_MODEL_NAME,
        embedding_model_version=_EMBEDDING_MODEL_VERSION,
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
    )


def _store_with_paraphrase_band_match(tmp_path) -> PathwayStore:
    """Store one pathway and stub the embedder so a query has cosine 0.6 to it.

    cosine([0.6, 0.8, 0], [1, 0, 0]) = 0.6 — inside the real paraphrase band,
    above the new default (~0.55) but below the old 0.85 default.
    """
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway([1.0, 0.0, 0.0]))
    store._embed_query = lambda _: np.array([0.6, 0.8, 0.0])  # type: ignore[attr-defined]
    return store


def test_default_threshold_constant_is_calibrated():
    # Low enough to catch the ~0.5-0.6 paraphrase band...
    assert DEFAULT_PATHWAY_THRESHOLD <= 0.6
    # ...but above the ~0.29 different-task ceiling, so unrelated tasks never false-merge.
    assert DEFAULT_PATHWAY_THRESHOLD >= 0.4


def test_default_threshold_retrieves_paraphrase_band_match(tmp_path):
    store = _store_with_paraphrase_band_match(tmp_path)
    match = store.find("a paraphrase of the stored task")  # no explicit threshold
    assert match is not None
    assert 0.59 < match.similarity < 0.61


def test_old_default_would_have_missed_it(tmp_path):
    # Regression guard: prove the band match really is below the old 0.85 default.
    store = _store_with_paraphrase_band_match(tmp_path)
    assert store.find("q", threshold=0.85) is None


def test_daisugi_find_pathway_uses_constructor_threshold(tmp_path):
    store = _store_with_paraphrase_band_match(tmp_path)
    # Default-threshold Daisugi retrieves the band match.
    dai = Daisugi(pathway_store=store)
    assert asyncio.run(dai.find_pathway("q")) is not None
    # A strict constructor threshold rejects it.
    dai_strict = Daisugi(pathway_store=store, pathway_threshold=0.85)
    assert asyncio.run(dai_strict.find_pathway("q")) is None


def test_daisugi_find_pathway_explicit_override_wins(tmp_path):
    store = _store_with_paraphrase_band_match(tmp_path)
    dai = Daisugi(pathway_store=store, pathway_threshold=0.85)  # would reject
    # Explicit per-call override admits it.
    assert asyncio.run(dai.find_pathway("q", threshold=0.5)) is not None
