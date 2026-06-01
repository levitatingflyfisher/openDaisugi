"""EnvelopeCache — SQLite-backed content-addressable cache (v0.1.2)."""

from __future__ import annotations

from opendaisugi.envelope_cache import EnvelopeCache, make_cache_key as _make_cache_key
from opendaisugi.models import Envelope, Permission


def _envelope(task: str = "demo task", summary: str | None = None) -> Envelope:
    return Envelope(
        generated_by="test-model",
        task=task,
        permissions=Permission(),
        summary=summary,
    )


def _key_args(**overrides):
    base = dict(
        task="demo task",
        context=None,
        model="anthropic/claude-sonnet-4-20250514",
        parent_envelope_id=None,
        summarize=False,
    )
    base.update(overrides)
    return base


def test_get_returns_none_on_miss(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    assert cache.get(**_key_args()) is None


def test_put_then_get_round_trip(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env = _envelope()
    cache.put(env, **_key_args())
    got = cache.get(**_key_args())
    assert got is not None
    assert got.task == env.task
    assert got.id == env.id


def test_get_returns_distinct_envelope_for_distinct_task(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env_a = _envelope(task="task A")
    env_b = _envelope(task="task B")
    cache.put(env_a, **_key_args(task="task A"))
    cache.put(env_b, **_key_args(task="task B"))
    assert cache.get(**_key_args(task="task A")).id == env_a.id
    assert cache.get(**_key_args(task="task B")).id == env_b.id


def test_summarize_flag_is_part_of_key(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env = _envelope()
    cache.put(env, **_key_args(summarize=False))
    assert cache.get(**_key_args(summarize=True)) is None  # different key
    assert cache.get(**_key_args(summarize=False)).id == env.id


def test_parent_id_is_part_of_key(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env = _envelope()
    cache.put(env, **_key_args(parent_envelope_id=None))
    assert cache.get(**_key_args(parent_envelope_id="env_abcd1234")) is None
    assert cache.get(**_key_args(parent_envelope_id=None)).id == env.id


def test_put_upsert_is_idempotent(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    cache.put(_envelope(), **_key_args())
    cache.put(_envelope(), **_key_args())  # replaces, does not raise
    assert cache.stats()["entries"] == 1


def test_prompt_version_mismatch_evicts_on_init(tmp_path):
    db = tmp_path / "cache.db"
    cache_v1 = EnvelopeCache(db, prompt_version="v1")
    cache_v1.put(_envelope(), **_key_args())
    assert cache_v1.stats()["entries"] == 1

    cache_v2 = EnvelopeCache(db, prompt_version="v2")
    assert cache_v2.stats()["evicted_on_init"] == 1
    assert cache_v2.stats()["entries"] == 0
    assert cache_v2.get(**_key_args()) is None


def test_clear_returns_count_and_empties_table(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    cache.put(_envelope(task="a"), **_key_args(task="a"))
    cache.put(_envelope(task="b"), **_key_args(task="b"))
    removed = cache.clear()
    assert removed == 2
    assert cache.stats()["entries"] == 0
    assert cache.get(**_key_args(task="a")) is None


def test_stats_reports_entries(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    assert cache.stats() == {"entries": 0, "evicted_on_init": 0}
    cache.put(_envelope(), **_key_args())
    assert cache.stats()["entries"] == 1


def test_summary_field_survives_round_trip(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env = _envelope(task="t", summary="short summary")
    cache.put(env, **_key_args(summarize=True))
    got = cache.get(**_key_args(summarize=True))
    assert got.summary == "short summary"


def test_make_cache_key_is_deterministic():
    k1 = _make_cache_key(**_key_args())
    k2 = _make_cache_key(**_key_args())
    assert k1 == k2
    assert len(k1) == 64  # sha256 hex


def test_cache_key_differs_across_thinking_budgets(tmp_path):
    from opendaisugi.envelope_cache import make_cache_key as _make_cache_key

    k_light = _make_cache_key(
        task="t", context=None, model="m", parent_envelope_id=None,
        summarize=False, thinking_budget="light",
    )
    k_standard = _make_cache_key(
        task="t", context=None, model="m", parent_envelope_id=None,
        summarize=False, thinking_budget="standard",
    )
    k_deep = _make_cache_key(
        task="t", context=None, model="m", parent_envelope_id=None,
        summarize=False, thinking_budget="deep",
    )
    assert k_light != k_standard != k_deep
    assert len({k_light, k_standard, k_deep}) == 3


def test_cache_get_put_round_trip_with_thinking_budget(tmp_path, sample_envelope):
    from opendaisugi.envelope_cache import EnvelopeCache

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="v1")
    cache.put(
        sample_envelope,
        task="t", context=None, model="m", parent_envelope_id=None,
        summarize=False, thinking_budget="deep",
    )
    assert cache.get(
        task="t", context=None, model="m", parent_envelope_id=None,
        summarize=False, thinking_budget="light",
    ) is None
    got = cache.get(
        task="t", context=None, model="m", parent_envelope_id=None,
        summarize=False, thinking_budget="deep",
    )
    assert got is not None
    assert got.id == sample_envelope.id


def test_cache_init_handles_parent_in_cwd(tmp_path, monkeypatch):
    """Regression: bare filename (parent == '.') must not error on mkdir."""
    monkeypatch.chdir(tmp_path)
    from opendaisugi.envelope_cache import EnvelopeCache
    cache = EnvelopeCache("bare.db", prompt_version="v1")
    assert cache.stats()["entries"] == 0


def test_clear_returns_int_rowcount(tmp_path, sample_envelope):
    from opendaisugi.envelope_cache import EnvelopeCache
    cache = EnvelopeCache(tmp_path / "c.db", prompt_version="v1")
    cache.put(
        sample_envelope,
        task="t", context=None, model="m", parent_envelope_id=None,
        summarize=False, thinking_budget="standard",
    )
    assert cache.clear() == 1
    assert isinstance(cache.clear(), int)
    assert cache.clear() == 0


def test_make_cache_key_is_public_and_deterministic():
    from opendaisugi.envelope_cache import make_cache_key
    k1 = make_cache_key(
        task="demo", context=None, model="m",
        parent_envelope_id=None, summarize=False,
        thinking_budget="standard",
    )
    k2 = make_cache_key(
        task="demo", context=None, model="m",
        parent_envelope_id=None, summarize=False,
        thinking_budget="standard",
    )
    assert k1 == k2
    assert len(k1) == 64  # sha256 hex


def test_make_cache_key_distinct_for_distinct_inputs():
    from opendaisugi.envelope_cache import make_cache_key
    k1 = make_cache_key(task="a", context=None, model="m",
                        parent_envelope_id=None, summarize=False)
    k2 = make_cache_key(task="b", context=None, model="m",
                        parent_envelope_id=None, summarize=False)
    assert k1 != k2


def test_get_inserted_at_returns_timestamp_for_cached_entry(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env = _envelope()
    from opendaisugi.envelope_cache import make_cache_key
    cache.put(env, **_key_args())
    key = make_cache_key(**_key_args())
    ts = cache.get_inserted_at(key)
    assert ts is not None
    assert isinstance(ts, float)
    assert ts > 0


def test_get_inserted_at_returns_none_for_missing_key(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    assert cache.get_inserted_at("nonexistent_key") is None


def test_invalidate_removes_entry_and_returns_true(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    env = _envelope()
    from opendaisugi.envelope_cache import make_cache_key
    cache.put(env, **_key_args())
    key = make_cache_key(**_key_args())
    assert cache.invalidate(key) is True
    assert cache.get(**_key_args()) is None


def test_invalidate_returns_false_for_missing_key(tmp_path):
    cache = EnvelopeCache(tmp_path / "cache.db", prompt_version="v1")
    assert cache.invalidate("nonexistent_key") is False
