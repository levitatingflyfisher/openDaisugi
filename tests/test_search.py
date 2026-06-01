"""Tests for lazy-loaded semantic search (opendaisugi[search] extra)."""

import sys

import pytest

from opendaisugi.journal import Journal


def test_search_raises_helpful_error_when_extra_not_installed(tmp_path, monkeypatch):
    # Simulate missing extra by stubbing opendaisugi._search to None — any
    # import of it raises ImportError, which Journal.search() catches.
    monkeypatch.setitem(sys.modules, "opendaisugi._search", None)

    j = Journal(data_dir=tmp_path)
    with pytest.raises(ImportError, match=r"opendaisugi\[search\]"):
        j.search("any query")


def test_search_dispatches_to_semantic_search_when_available(tmp_path, monkeypatch):
    # Install a fake _search module in sys.modules so Journal.search()
    # finds semantic_search without triggering the real top-level import.
    import types
    fake = types.ModuleType("opendaisugi._search")
    calls = {}
    def fake_semantic_search(journal, query, *, limit):
        calls["journal"] = journal
        calls["query"] = query
        calls["limit"] = limit
        return ["fake-result"]
    fake.semantic_search = fake_semantic_search
    monkeypatch.setitem(sys.modules, "opendaisugi._search", fake)

    j = Journal(data_dir=tmp_path)
    result = j.search("csv processing", limit=5)
    assert result == ["fake-result"]
    assert calls["query"] == "csv processing"
    assert calls["limit"] == 5
    assert calls["journal"] is j


def test_semantic_search_orders_by_cosine_similarity(tmp_path, monkeypatch):
    """Positive path — mocks SentenceTransformer to avoid model download."""
    pytest.importorskip("numpy")
    from opendaisugi.models import (
        ActionPlan, ShellStep, Envelope, Permission, VerificationResult,
    )

    j = Journal(data_dir=tmp_path)
    # Log 3 traces with different tasks.
    for i, task in enumerate(["read csv data", "delete tmp files", "parse xml feed"]):
        env = Envelope(id=f"env_{i:02d}", generated_by="t", task=task, permissions=Permission())
        plan = ActionPlan(id=f"plan_{i:02d}", source="t", task=task, steps=[
            ShellStep(id="s1", command="echo hi"),
        ])
        result = VerificationResult(
            ok=True, violations=[], warnings=[],
            envelope_id=env.id, plan_id=plan.id, duration_ms=1.0,
        )
        j.log(
            task=task, envelope=env, plan=plan, result=result,
            trace_id=f"2026-04-09-{i:08d}",
            created_at=f"2026-04-09T10:0{i}:00Z",
        )

    # Mock SentenceTransformer.encode() to return controlled vectors.
    # Query "csv" encodes to [1,0,0]; tasks encode based on their first word.
    import opendaisugi._search as search_module

    class FakeEncoder:
        def __init__(self, model_name):
            pass
        def encode(self, texts, convert_to_numpy=True):
            import numpy as np
            vectors = []
            for t in texts:
                if "csv" in t or t == "csv":
                    vectors.append([1.0, 0.0, 0.0])
                elif "tmp" in t:
                    vectors.append([0.0, 1.0, 0.0])
                elif "xml" in t:
                    vectors.append([0.0, 0.0, 1.0])
                else:
                    vectors.append([0.33, 0.33, 0.33])
            return np.array(vectors)

    # Bypass the lazy sentence_transformers import by preloading the model
    # cache with a fake encoder; _get_model() short-circuits on non-None.
    monkeypatch.setattr(search_module, "_model", FakeEncoder(None))
    results = search_module.semantic_search(j, "csv", limit=2)
    assert len(results) == 2
    # First result should be the "read csv data" trace.
    assert "csv" in results[0].task
