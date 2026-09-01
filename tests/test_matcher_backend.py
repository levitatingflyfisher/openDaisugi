"""The configurable, torch-free pathway-reuse embedder. ADR-0018, 0019, 0021.

`matcher_model` in config selects the embedder. `all-MiniLM-L6-v2` needs
torch. `potion` is model2vec with no torch. `lexical` is stdlib and numpy
with no model and no download. `int8` is MiniLM quantized, run through
onnxruntime with no torch. All four are built. The identity a backend
reports must flow to pathway provenance, or cross-space vectors get
compared and surface wrong matches. A missing `[search]`, `[potion]`, or
`[int8]` package falls back to `lexical` per ADR-0019 rather than silently
distilling zero pathways.

Hermeticity: identity/threshold resolution reads config from DEFAULT_DATA_DIR
(which conftest isolates to a tmp dir), never the real ~/.opendaisugi — the real
config on a dev box may set matcher_model=potion, which would otherwise leak in.
"""

from __future__ import annotations

import numpy as np
import pytest

import opendaisugi
from opendaisugi.config import Config, save_config
from opendaisugi.exceptions import MatcherNotAvailable


def _set_matcher(model: str) -> None:
    """Write matcher_model into the test's isolated config dir."""
    save_config(Config(matcher_model=model), opendaisugi.DEFAULT_DATA_DIR / "config.yaml")


# --- the lexical embedder itself: pure stdlib + numpy, no model, no download,
# no config plumbing involved (ADR-0019) ------------------------------------


def test_lexical_embedder_shape_repeatable_and_l2_normalized():
    from opendaisugi._search import _LexicalEmbedder

    out = _LexicalEmbedder().encode(
        ["find stale tmp files", "find stale tmp files"], convert_to_numpy=True
    )
    assert isinstance(out, np.ndarray)
    assert out.shape == (2, 4096)
    assert np.array_equal(out[0], out[1])  # The same text gives the same vector.
    assert np.linalg.norm(out[0]) == pytest.approx(1.0)


def test_lexical_embedder_deterministic_across_processes():
    """blake2b, not Python's salted ``hash()`` — must agree across processes."""
    import subprocess
    import sys

    from opendaisugi._search import _LexicalEmbedder

    in_process = _LexicalEmbedder().encode(["find stale tmp files"])[0][:8]
    code = (
        "from opendaisugi._search import _LexicalEmbedder\n"
        "v = _LexicalEmbedder().encode(['find stale tmp files'])[0]\n"
        "print(','.join(repr(float(x)) for x in v[:8]))\n"
    )
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=True)
    from_subprocess = [float(x) for x in out.stdout.strip().split(",")]
    assert np.allclose(in_process, from_subprocess)


def test_lexical_embedder_stopwords_only_is_zero_vector_no_nan():
    from opendaisugi._search import _LexicalEmbedder

    out = _LexicalEmbedder().encode(["the a of"])
    assert not np.isnan(out).any()
    assert np.allclose(out[0], 0.0)


def test_lexical_embedder_paraphrase_clears_threshold_unrelated_pair_does_not():
    from opendaisugi._search import _LEXICAL_THRESHOLD, _LexicalEmbedder

    emb = _LexicalEmbedder()
    a, b = emb.encode(
        [
            "clean up stale tmp files in the build directory",
            "remove old temporary files from the build folder",
        ]
    )
    assert float(np.dot(a, b)) >= _LEXICAL_THRESHOLD

    c, d = emb.encode(["rebuild the android apk", "write the ADR for the gateway"])
    assert float(np.dot(c, d)) < _LEXICAL_THRESHOLD


# --- int8: the pinned manifest and CPU-variant detection, no model load ----


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


# --- the int8 adapter: encode() itself, no packages or network needed ------


class _FakeInt8Encoding:
    def __init__(self, ids, attention_mask):
        self.ids = ids
        self.attention_mask = attention_mask


class _FakeInt8Tokenizer:
    """Stand-in for a configured tokenizers.Tokenizer. Fixed batch width 5.
    The first N positions of each text are real, the rest are padding. This
    is enough to prove encode() batches and masks correctly without a real
    vocabulary."""

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


class _FakeInt8Session:
    """Stand-in for onnxruntime.InferenceSession. Every real, unmasked token
    position gets the constant 3.0. Every padded position gets 999.0. This
    proves encode() excludes padding from the mean-pool instead of averaging
    garbage into it."""

    OUTPUTS = ({"name": "last_hidden_state", "shape": ["batch", "sequence", 384]},)

    def get_outputs(self):
        return [type("_Out", (), spec)() for spec in self.OUTPUTS]

    def run(self, output_names, feed):
        assert output_names == ["last_hidden_state"]
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
    emb._output_name = "last_hidden_state"
    return emb


def test_int8_encode_masks_padding_before_pooling_and_l2_normalizes():
    emb = _bare_int8_embedder(_FakeInt8Tokenizer(), _FakeInt8Session())
    out = emb.encode(["a", "a much longer sentence with several more words in it"])
    assert out.shape == (2, 384)
    assert np.allclose(np.linalg.norm(out, axis=1), 1.0, atol=1e-5)
    # Every real position contributes the same constant 3.0 whatever the
    # length. If padding's 999.0 leaked into the mean, these rows would not
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


# --- the int8 adapter: __init__, package absent, offline, and online fetch --


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

    monkeypatch.delenv("OPENDAISUGI_INT8_MODEL", raising=False)
    monkeypatch.setattr(_search, "_detect_int8_variant", lambda: "avx2")

    def _raise(*a, **k):
        raise OSError("network unreachable")

    monkeypatch.setattr("opendaisugi._model_fetch.fetch", _raise)
    with pytest.raises(MatcherNotAvailable, match="OPENDAISUGI_INT8_MODEL"):
        _search._Int8Embedder()


def _online_int8_setup(monkeypatch, tmp_path, session_factory=None):
    """Point the embedder at tmp_path with a fetch that writes fake files.
    Returns the list of file names the fake fetch wrote."""
    from opendaisugi import _search

    monkeypatch.delenv("OPENDAISUGI_INT8_MODEL", raising=False)
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
    factory = session_factory or (lambda: _FakeInt8Session())
    monkeypatch.setattr("onnxruntime.InferenceSession", lambda path, providers=None: factory())
    return fetched


def test_int8_online_fetches_model_and_tokenizer_then_builds_session(monkeypatch, tmp_path):
    from opendaisugi import _search

    fetched = _online_int8_setup(monkeypatch, tmp_path)
    emb = _search._Int8Embedder()
    assert sorted(fetched) == ["model_quint8_avx2.onnx", "tokenizer.json"]
    assert isinstance(emb, _search._Int8Embedder)
    assert emb._output_name == "last_hidden_state"


def test_int8_refuses_a_model_whose_first_output_is_not_token_level(monkeypatch, tmp_path):
    """A pooled, rank-2 first output is the wrong export. Refuse at load,
    never mean-pool a vector that is already pooled."""
    from opendaisugi import _search

    class _PooledSession(_FakeInt8Session):
        OUTPUTS = ({"name": "sentence_embedding", "shape": ["batch", 384]},)

    _online_int8_setup(monkeypatch, tmp_path, _PooledSession)
    with pytest.raises(MatcherNotAvailable, match="token-level"):
        _search._Int8Embedder()


def test_int8_tokenizer_load_failure_becomes_matcher_not_available(monkeypatch, tmp_path):
    from opendaisugi import _search

    _online_int8_setup(monkeypatch, tmp_path)

    def _broken(p):
        raise RuntimeError("bad json")

    monkeypatch.setattr("tokenizers.Tokenizer.from_file", staticmethod(_broken))
    with pytest.raises(MatcherNotAvailable, match="OPENDAISUGI_INT8_MODEL") as info:
        _search._Int8Embedder()
    assert str(tmp_path) in str(info.value)


def test_int8_session_load_failure_becomes_matcher_not_available(monkeypatch, tmp_path):
    from opendaisugi import _search

    def _broken():
        raise RuntimeError("unsupported opset")

    _online_int8_setup(monkeypatch, tmp_path, _broken)
    with pytest.raises(MatcherNotAvailable, match="OPENDAISUGI_INT8_MODEL") as info:
        _search._Int8Embedder()
    assert str(tmp_path) in str(info.value)


def test_int8_threshold_is_derived_and_recorded():
    """Locks the value ADR-0021 records. Re-derive it with
    scripts/matcher_fpr.py when the journal or the model changes. Update
    the ADR together with this constant."""
    from opendaisugi._search import _INT8_THRESHOLD

    assert _INT8_THRESHOLD == pytest.approx(0.45)


# --- identity resolution (no model load) -----------------------------------


def test_default_backend_identity_is_minilm():
    from opendaisugi._search import active_model_name

    assert active_model_name() == "all-MiniLM-L6-v2"


def test_potion_identity_is_the_resolved_model_id():
    from opendaisugi._search import active_model_name

    _set_matcher("potion")
    # Identity is the concrete model id, not the bare key "potion" — so switching
    # potion models (or pointing at a local dir) invalidates stale-provenance rows.
    assert active_model_name() == "minishlab/potion-base-8M"


def test_potion_model_override_via_env(monkeypatch):
    from opendaisugi._search import active_model_name

    _set_matcher("potion")
    monkeypatch.setenv("OPENDAISUGI_POTION_MODEL", "minishlab/potion-retrieval-32M")
    assert active_model_name() == "minishlab/potion-retrieval-32M"


@pytest.mark.parametrize(
    "matcher_key, expected_identity, expected_threshold",
    [
        ("lexical", "lexical-hash-v1", 0.25),
        ("int8", "all-MiniLM-L6-v2-int8", 0.45),
    ],
)
def test_static_backend_identity_and_threshold(matcher_key, expected_identity, expected_threshold):
    """Both lexical and int8 resolve identity and threshold without a model
    load. active_model_name and active_threshold never load. ADR-0018, 0021."""
    from opendaisugi._search import active_model_name, active_threshold

    _set_matcher(matcher_key)
    assert active_model_name() == expected_identity
    assert active_threshold() == pytest.approx(expected_threshold)


# --- per-backend threshold (FPR-matched, ADR-0018) -------------------------


def test_threshold_is_per_backend():
    from opendaisugi._search import active_threshold

    assert active_threshold() == pytest.approx(0.55)  # MiniLM
    _set_matcher("potion")
    assert active_threshold() == pytest.approx(0.59)  # FPR-matched to MiniLM@0.55


# --- the adapter: uniform, L2-normalized encode ----------------------------


class _FakeStatic:
    """Stand-in for model2vec.StaticModel: returns UNnormalized vectors so the
    test proves the adapter normalizes (the concat invariant depends on it)."""

    def __init__(self, dim=256):
        self.dim = dim

    def encode(self, texts):
        # deterministic, norm != 1 on purpose
        return np.array([[float((i + 1) * 3)] * self.dim for i in range(len(texts))])


def test_potion_adapter_normalizes_and_returns_numpy(monkeypatch):
    _set_matcher("potion")
    import model2vec

    monkeypatch.setattr(
        model2vec.StaticModel, "from_pretrained", staticmethod(lambda mid: _FakeStatic())
    )
    from opendaisugi import _search

    _search._reset_embedder_cache()
    out = _search._get_model().encode(["a", "b"], convert_to_numpy=True)
    assert isinstance(out, np.ndarray)
    norms = np.linalg.norm(out, axis=1)
    assert np.allclose(norms, 1.0, atol=1e-5)  # adapter L2-normalized


def test_potion_missing_package_teaches_the_extra(monkeypatch):
    _set_matcher("potion")
    import builtins

    real_import = builtins.__import__

    def _blocked(name, *a, **k):
        if name == "model2vec" or name.startswith("model2vec."):
            raise ImportError("No module named 'model2vec'")
        return real_import(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", _blocked)
    from opendaisugi import _search

    _search._reset_embedder_cache()
    with pytest.raises(ImportError, match=r"opendaisugi\[potion\]"):
        _search._get_model()


# --- the package-absence fallback to lexical (ADR-0019) --------------------


def test_fallback_to_lexical_when_minilm_package_missing(monkeypatch, caplog):
    """A missing [search] degrades identity/threshold/model TOGETHER, and
    warns exactly once — not once per active_*()/get_model() call."""
    import importlib.util
    import logging

    from opendaisugi import _search

    _set_matcher("all-MiniLM-L6-v2")
    real_find_spec = importlib.util.find_spec

    def _blocked(name, *a, **k):
        if name == "sentence_transformers":
            return None
        return real_find_spec(name, *a, **k)

    monkeypatch.setattr(importlib.util, "find_spec", _blocked)
    _search._reset_embedder_cache()

    with caplog.at_level(logging.WARNING, logger="opendaisugi._search"):
        assert _search.active_model_name() == "lexical-hash-v1"
        assert _search.active_threshold() == pytest.approx(0.25)
        model = _search._get_model()

    assert isinstance(model, _search._LexicalEmbedder)
    hits = [r for r in caplog.records if "needs opendaisugi[search]" in r.message]
    assert len(hits) == 1, f"expected exactly one warning, got {len(hits)}: {hits}"


def test_potion_load_failure_does_not_fall_back_to_lexical(monkeypatch):
    """The operator chose potion; a model that fails to LOAD (not a missing
    package) must refuse honestly, never silently downgrade to lexical."""
    _set_matcher("potion")
    import model2vec

    def _raise(_mid):
        raise OSError("offline")

    monkeypatch.setattr(model2vec.StaticModel, "from_pretrained", staticmethod(_raise))
    from opendaisugi import _search

    _search._reset_embedder_cache()
    with pytest.raises(MatcherNotAvailable, match="OPENDAISUGI_POTION_MODEL") as exc_info:
        _search._get_model()
    assert "lexical" in str(exc_info.value)


# --- provenance identity flows to the store; cross-space vectors don't crash --


def _pathway(id_, *, embedding, model, version):
    from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
    from opendaisugi.pathway import CompiledPathway

    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo hi")])
    return CompiledPathway(
        id=id_,
        task_description="task",
        task_embedding=embedding,
        embedding_model=model,
        embedding_model_version=version,
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=1.0,
    )


def test_find_dimension_guard_excludes_cross_dim_legacy_row(tmp_path):
    # A legacy (pre-provenance) row is admitted as a wildcard regardless of
    # backend. Under potion (256-dim query) an old 384-dim MiniLM legacy row would
    # reach the cosine dot product and raise — the dimension guard must drop it.
    from opendaisugi.pathway_store import PathwayStore

    _set_matcher("potion")
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("legacy", embedding=[0.1] * 384, model="", version=""))
    store._embed_query = lambda _t: np.ones(256)  # potion query width
    assert store.find("anything", threshold=0.1) is None  # no crash, cleanly excluded


def test_find_uses_active_backend_identity(tmp_path):
    # Under potion, a potion-stamped row is compatible and matches; a MiniLM-stamped
    # row is stale (different embedding space) and must be excluded.
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION
    from opendaisugi.pathway_store import PathwayStore

    _set_matcher("potion")
    store = PathwayStore(tmp_path / "p.db")
    store.put(
        _pathway(
            "pot",
            embedding=[1.0] + [0.0] * 255,
            model="minishlab/potion-base-8M",
            version=_EMBEDDING_MODEL_VERSION,
        )
    )
    store.put(
        _pathway(
            "mini",
            embedding=[1.0] + [0.0] * 383,
            model="all-MiniLM-L6-v2",
            version=_EMBEDDING_MODEL_VERSION,
        )
    )
    store._embed_query = lambda _t: np.array([1.0] + [0.0] * 255)
    m = store.find("q", threshold=0.5)
    assert m is not None and m.pathway.id == "pot"


def _write_success_trace(journal, task):
    import sqlite3

    from opendaisugi.models import (
        ActionPlan,
        Envelope,
        Permission,
        ShellStep,
        VerificationResult,
    )

    env = Envelope(
        generated_by="test", task=task, permissions=Permission(shell=True, shell_allowlist=["find"])
    )
    plan = ActionPlan(
        source="t", task=task, steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")]
    )
    result = VerificationResult(
        ok=True, violations=[], warnings=[], envelope_id=env.id, plan_id=plan.id, duration_ms=0.1
    )
    tid = journal.log(task=task, envelope=env, plan=plan, result=result)
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_id = ?, run_status = 'succeeded' WHERE id = ?",
            (f"run_{tid}", tid),
        )
    return tid


@pytest.mark.asyncio
async def test_tend_stamps_active_backend_identity(tmp_path, monkeypatch):
    from opendaisugi import distiller as dist_mod
    from opendaisugi.distiller import Distiller, GeneralizedTemplate
    from opendaisugi.journal import Journal
    from opendaisugi.models import ActionPlan, ShellStep
    from opendaisugi.pathway_store import PathwayStore

    _set_matcher("potion")
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):
        _write_success_trace(journal, f"find stale tmp files run {i}")

    distiller = Distiller(journal=journal, pathway_store=store, model="test-model", min_traces=3)
    monkeypatch.setattr(distiller, "_embed_tasks", lambda tasks: np.ones((len(tasks), 4)))
    monkeypatch.setattr(distiller, "_embed_plan_structures", lambda sigs: np.ones((len(sigs), 4)))
    plan_tmpl = ActionPlan(
        source="template", task="T", steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")]
    )

    async def _fake_gen(**kwargs):
        return GeneralizedTemplate(
            task_description="find stale temp files", plan_template=plan_tmpl
        )

    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_gen)

    report = await distiller.tend()
    assert report.created == 1
    # Stamped with the ACTIVE backend, not a hard-wired MiniLM constant.
    assert store.list_all()[0].embedding_model == "minishlab/potion-base-8M"


@pytest.mark.asyncio
async def test_tend_distills_with_real_lexical_embedder_end_to_end(tmp_path, monkeypatch):
    """Unlike test_tend_stamps_active_backend_identity above, this does NOT
    monkeypatch _embed_tasks — the REAL _LexicalEmbedder does the work, all the
    way through clustering and back out through PathwayStore.find().

    structure_weight=0.0 keeps task_embedding at the lexical embedder's own
    width, so it matches the width _embed_query() produces for the find() call
    below (the concatenated task+structure embedding the 0.5 default would
    stamp is a different, wider space — a real dimension, not a test wrinkle).
    """
    from opendaisugi import distiller as dist_mod
    from opendaisugi.distiller import Distiller, GeneralizedTemplate
    from opendaisugi.journal import Journal
    from opendaisugi.models import ActionPlan, ShellStep
    from opendaisugi.pathway_store import PathwayStore

    _set_matcher("lexical")
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):
        _write_success_trace(journal, f"find stale tmp files run {i}")

    distiller = Distiller(
        journal=journal,
        pathway_store=store,
        model="test-model",
        min_traces=3,
        structure_weight=0.0,
    )
    plan_tmpl = ActionPlan(
        source="template", task="T", steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")]
    )

    async def _fake_gen(**kwargs):
        return GeneralizedTemplate(
            task_description="find stale temp files", plan_template=plan_tmpl
        )

    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_gen)

    report = await distiller.tend()
    assert report.created == 1
    pathway = store.list_all()[0]
    assert pathway.embedding_model == "lexical-hash-v1"

    match = store.find("find stale tmp files run 9")
    assert match is not None and match.pathway.id == pathway.id


@pytest.mark.asyncio
async def test_lexical_path_never_imports_heavy_ml_packages(tmp_path, monkeypatch):
    """Zero-network guard (ADR-0019): the lexical path — driven end to end
    through the real Distiller and PathwayStore, exactly as the test above —
    must never import model2vec, sentence-transformers, huggingface_hub, or
    transformers. Blocking at ``builtins.__import__`` proves it, not just the
    identity/threshold resolution (which never imports anything heavy anyway)."""
    import builtins

    from opendaisugi import _search
    from opendaisugi import distiller as dist_mod
    from opendaisugi.distiller import Distiller, GeneralizedTemplate
    from opendaisugi.journal import Journal
    from opendaisugi.models import ActionPlan, ShellStep
    from opendaisugi.pathway_store import PathwayStore

    _set_matcher("lexical")
    _search._reset_embedder_cache()

    blocked = ("model2vec", "sentence_transformers", "huggingface_hub", "transformers")
    real_import = builtins.__import__

    def _guarded(name, *a, **k):
        if name in blocked or any(name.startswith(b + ".") for b in blocked):
            raise ImportError(f"blocked by the zero-network guard test: {name}")
        return real_import(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", _guarded)

    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")
    for i in range(3):
        _write_success_trace(journal, f"find stale tmp files run {i}")

    distiller = Distiller(
        journal=journal,
        pathway_store=store,
        model="test-model",
        min_traces=3,
        structure_weight=0.0,
    )
    plan_tmpl = ActionPlan(
        source="template", task="T", steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")]
    )

    async def _fake_gen(**kwargs):
        return GeneralizedTemplate(
            task_description="find stale temp files", plan_template=plan_tmpl
        )

    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_gen)

    report = await distiller.tend()
    assert report.created == 1
    assert store.find("find stale tmp files run 9") is not None


# --- the per-backend threshold is actually WIRED into the reuse path --------


def test_active_threshold_is_wired_into_find_router_and_distiller(tmp_path):
    import math

    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION, Distiller
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.routing import RouteAdvisor

    _set_matcher("potion")
    # Holders resolve the per-backend threshold at construction, not the 0.55 default.
    assert RouteAdvisor(pathway_store=None).threshold == pytest.approx(0.59)
    assert Distiller(
        journal=None, pathway_store=None, model="m"
    ).similarity_threshold == pytest.approx(0.59)

    # find() with no explicit threshold applies 0.59: a 0.57 match is rejected —
    # under MiniLM's 0.55 the same match would have been accepted (the bug this
    # whole session was about: a documented threshold that wasn't in force).
    store = PathwayStore(tmp_path / "p.db")
    store.put(
        _pathway(
            "p",
            embedding=[1.0, 0.0],
            model="minishlab/potion-base-8M",
            version=_EMBEDDING_MODEL_VERSION,
        )
    )
    angle = math.acos(0.57)
    store._embed_query = lambda _t: np.array([math.cos(angle), math.sin(angle)])
    assert store.find("q") is None  # 0.57 < 0.59
    assert store.find("q", threshold=0.55) is not None  # explicit 0.55 still accepts it


def test_facade_resolves_active_threshold(tmp_path):
    # The Daisugi facade is the main entry; it must honor matcher_model, not pin 0.55.
    from opendaisugi.facade import Daisugi

    _set_matcher("potion")
    d = Daisugi(pathway_store=False, cache=False, answer_store=False, data_dir=str(tmp_path))
    assert d._pathway_threshold == pytest.approx(0.59)


# --- the swap menu records every built backend ----------------------------


def test_swap_allows_potion(tmp_path):
    from opendaisugi.swap import apply_swap

    cfg = apply_swap("matcher", "potion static", config_path=tmp_path / "config.yaml")
    assert cfg.matcher_model == "potion"


def test_swap_allows_lexical(tmp_path):
    from opendaisugi.swap import apply_swap

    cfg = apply_swap("matcher", "lexical / intent", config_path=tmp_path / "config.yaml")
    assert cfg.matcher_model == "lexical"


# --- the module map tells the truth about potion ---------------------------


def test_modules_map_marks_potion_available_when_installed(tmp_path):
    # model2vec is installed in the dev env, so potion is a real swap target,
    # not a placeholder. lexical has no package to be missing, so it is
    # AVAILABLE here: not selected, no fallback triggered.
    from opendaisugi.modules import ACTIVE, AVAILABLE, detect_stages

    stages = {s.key: s for s in detect_stages(tmp_path)}
    matcher = stages["matcher"]
    by_name = {m.name: m for m in matcher.modules}
    assert by_name["potion static"].state in (ACTIVE, AVAILABLE)
    assert by_name["lexical / intent"].state == AVAILABLE
    # Two real embedders installed -> a genuine swap point, not falsely single-impl.
    assert matcher.swappable


def test_modules_map_marks_lexical_active_when_selected(tmp_path):
    from opendaisugi.config import Config, save_config
    from opendaisugi.modules import ACTIVE, detect_stages

    save_config(Config(matcher_model="lexical"), tmp_path / "config.yaml")
    matcher = next(s for s in detect_stages(tmp_path) if s.key == "matcher")
    lex = {m.name: m for m in matcher.modules}["lexical / intent"]
    assert lex.state == ACTIVE
    assert lex.note == "keyword floor — no model, no download"


def test_modules_map_marks_lexical_active_via_fallback_and_names_the_cause(tmp_path, monkeypatch):
    import importlib.util

    from opendaisugi.config import Config, save_config
    from opendaisugi.modules import ACTIVE, detect_stages

    save_config(Config(matcher_model="all-MiniLM-L6-v2"), tmp_path / "config.yaml")
    real_find_spec = importlib.util.find_spec

    def _blocked(name, *a, **k):
        if name == "sentence_transformers":
            return None
        return real_find_spec(name, *a, **k)

    monkeypatch.setattr(importlib.util, "find_spec", _blocked)
    from opendaisugi import _search

    _search._reset_embedder_cache()

    matcher = next(s for s in detect_stages(tmp_path) if s.key == "matcher")
    lex = {m.name: m for m in matcher.modules}["lexical / intent"]
    assert lex.state == ACTIVE
    assert lex.note == "keyword floor — active: all-MiniLM-L6-v2's package is not installed"


# --- int8 wired into the dispatch: fallback, swap, modules -----------------


def _int8_reachable() -> bool:
    """True when onnxruntime is installed and the pinned model and tokenizer
    files for this CPU already sit in the int8 cache dir. Never touches the
    network. A test that needs the real model skips otherwise."""
    import importlib.util
    from pathlib import Path

    if importlib.util.find_spec("onnxruntime") is None:
        return False
    from opendaisugi._search import _INT8_FILES, _detect_int8_variant, _int8_cache_dir

    try:
        variant = _detect_int8_variant()
    except MatcherNotAvailable:
        return False
    from opendaisugi._model_fetch import sha256_file

    cache = _int8_cache_dir()
    for rel, want, _size in (_INT8_FILES[variant], _INT8_FILES["tokenizer"]):
        p = cache / Path(rel).name
        if not p.is_file() or sha256_file(p) != want:
            return False
    return True


def _pin_int8_offline(monkeypatch) -> str:
    """Point OPENDAISUGI_INT8_MODEL at the cache dir the gate checked, so the
    embedder takes the offline branch. A stale cache then refuses. It never
    fetches from inside the suite. Returns the dir for a child process."""
    from opendaisugi._search import _int8_cache_dir

    cache = str(_int8_cache_dir())
    monkeypatch.setenv("OPENDAISUGI_INT8_MODEL", cache)
    return cache


_NEEDS_REAL_INT8 = pytest.mark.skipif(
    not _int8_reachable(), reason="int8 model not in the local cache; no download in tests"
)


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


def test_int8_dispatch_builds_the_int8_embedder(monkeypatch):
    """_get_model constructs _Int8Embedder for matcher_model=int8 and caches
    it under the int8 identity. The constructor is stubbed, so this needs
    no model file."""
    from opendaisugi import _search

    _set_matcher("int8")
    _search._reset_embedder_cache()
    monkeypatch.setattr(_search._Int8Embedder, "__init__", lambda self: None)

    model = _search._get_model()
    assert isinstance(model, _search._Int8Embedder)
    assert _search._get_model() is model
    assert _search._embedder_cache == {"all-MiniLM-L6-v2-int8": model}


def _encode_int8_in_subprocess(text: str, cache_dir: str) -> tuple[list[float], bool]:
    """Encode ``text`` with a fresh _Int8Embedder in a child process that is
    pinned offline to ``cache_dir``. Returns the first 8 components and
    whether torch was imported there."""
    import os
    import subprocess
    import sys

    code = (
        "import sys\n"
        "from opendaisugi._search import _Int8Embedder\n"
        f"v = _Int8Embedder().encode([{text!r}])[0]\n"
        "print(','.join(repr(float(x)) for x in v[:8]))\n"
        "print('torch' in sys.modules)\n"
    )
    env = {**os.environ, "OPENDAISUGI_INT8_MODEL": cache_dir}
    out = subprocess.run(
        [sys.executable, "-c", code], capture_output=True, text=True, check=True, env=env
    )
    head, torch_line = out.stdout.strip().splitlines()[-2:]
    return [float(x) for x in head.split(",")], torch_line == "True"


@_NEEDS_REAL_INT8
def test_int8_never_imports_torch(monkeypatch):
    """A fresh process that runs the int8 embedder must not load torch. The
    check runs in a child process because other tests in this process may
    already have imported torch through sentence-transformers."""
    cache = _pin_int8_offline(monkeypatch)
    _, torch_imported = _encode_int8_in_subprocess("a quick smoke check", cache)
    assert not torch_imported


@_NEEDS_REAL_INT8
def test_int8_shape_repeatable_and_l2_normalized(monkeypatch):
    """The real model, not the fakes: the same hygiene checks the lexical
    embedder gets, shape, repeatability, L2 norm, on the same task string."""
    _pin_int8_offline(monkeypatch)
    _set_matcher("int8")
    from opendaisugi import _search

    _search._reset_embedder_cache()
    model = _search._get_model()
    out = model.encode(["find stale tmp files", "find stale tmp files"], convert_to_numpy=True)
    assert out.shape == (2, 384)
    assert np.allclose(out[0], out[1])  # The same text gives the same vector.
    assert np.linalg.norm(out[0]) == pytest.approx(1.0, abs=1e-4)


@_NEEDS_REAL_INT8
def test_int8_embedder_deterministic_across_processes(monkeypatch):
    """onnxruntime CPU inference must agree across processes."""
    from opendaisugi._search import _Int8Embedder

    cache = _pin_int8_offline(monkeypatch)
    in_process = _Int8Embedder().encode(["find stale tmp files"])[0][:8]
    from_subprocess, _ = _encode_int8_in_subprocess("find stale tmp files", cache)
    assert np.allclose(in_process, from_subprocess, atol=1e-5)


@_NEEDS_REAL_INT8
def test_int8_paraphrase_clears_threshold_unrelated_does_not(monkeypatch):
    _pin_int8_offline(monkeypatch)
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
    # onnxruntime is installed in the dev env through opendaisugi[int8], so
    # int8 is a real swap target on any CPU these tests run on, not the
    # POSSIBLE placeholder.
    from opendaisugi.modules import ACTIVE, AVAILABLE, detect_stages

    stages = {s.key: s for s in detect_stages(tmp_path)}
    matcher = {m.name: m for m in stages["matcher"].modules}
    assert matcher["int8 / fp16 onnx"].state in (ACTIVE, AVAILABLE)
    distill = {m.name: m for m in stages["distill"].modules}
    assert distill["int8 (onnx, no torch)"].state in (ACTIVE, AVAILABLE)


def test_modules_map_names_the_missing_int8_package(tmp_path, monkeypatch):
    import importlib.util

    from opendaisugi.modules import POSSIBLE, detect_stages

    real_find_spec = importlib.util.find_spec

    def _blocked(name, *a, **k):
        if name == "onnxruntime":
            return None
        return real_find_spec(name, *a, **k)

    monkeypatch.setattr(importlib.util, "find_spec", _blocked)
    stages = {s.key: s for s in detect_stages(tmp_path)}
    int8 = {m.name: m for m in stages["matcher"].modules}["int8 / fp16 onnx"]
    assert int8.state == POSSIBLE
    assert int8.note == "needs opendaisugi[int8]"


def test_modules_map_names_the_unsupported_cpu_for_int8(tmp_path, monkeypatch):
    """Package present, CPU without a build: the note says so. It must not
    tell the operator to install a package that is already installed."""
    from opendaisugi import _search
    from opendaisugi.modules import POSSIBLE, detect_stages

    def _no_build(**kw):
        raise MatcherNotAvailable("no int8 build for this CPU.")

    monkeypatch.setattr(_search, "_detect_int8_variant", _no_build)
    stages = {s.key: s for s in detect_stages(tmp_path)}
    matcher = {m.name: m for m in stages["matcher"].modules}["int8 / fp16 onnx"]
    distill = {m.name: m for m in stages["distill"].modules}["int8 (onnx, no torch)"]
    assert matcher.state == POSSIBLE and distill.state == POSSIBLE
    assert matcher.note == "no int8 build for this CPU"
    assert distill.note == "no int8 build for this CPU"
