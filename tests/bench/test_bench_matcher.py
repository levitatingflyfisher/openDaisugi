"""The matcher bench compares the built embedders at their own thresholds on a
small committed corpus, never downloads, and reports the lexical floor always."""

import json
import os
import time

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers.matcher import (
    COLUMNS,
    TARGET_FPR,
    build_embedder,
    fpr_table,
    measure,
    run,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json


def test_cache_mb_measures_one_backend_not_the_whole_hub(tmp_path, monkeypatch):
    """Summing the whole HF cache would report the operator's entire model
    collection as each backend's size. And the HF layout keeps bytes under
    blobs/ with symlinks under snapshots/, so a symlink must not count twice."""
    from opendaisugi.bench.layers.matcher import _cache_mb, _hf_snapshot_dir

    assert _cache_mb("lexical") == 0.0
    fake = tmp_path / "models--minishlab--potion-base-8M"
    (fake / "blobs").mkdir(parents=True)
    (fake / "snapshots" / "abc").mkdir(parents=True)
    (fake / "blobs" / "b1").write_bytes(b"x" * (1024 * 1024))
    (fake / "snapshots" / "abc" / "model.safetensors").symlink_to(fake / "blobs" / "b1")
    monkeypatch.setattr("opendaisugi.bench.layers.matcher._hf_snapshot_dir", lambda model_id: fake)
    assert _cache_mb("potion") == 1.0
    assert _hf_snapshot_dir("minishlab/potion-base-8M").name == (
        "models--minishlab--potion-base-8M"
    )
    assert _hf_snapshot_dir("sentence-transformers/all-MiniLM-L6-v2").name == (
        "models--sentence-transformers--all-MiniLM-L6-v2"
    )


def test_columns_and_the_matched_target():
    assert [c.key for c in COLUMNS] == ["option", "threshold", "fpr", "recall", "ms_embed", "mb"]
    assert TARGET_FPR == 0.0256
    assert {c.key for c in COLUMNS if c.volatile} == {"ms_embed"}


def test_lexical_always_has_a_real_row_because_it_needs_no_package():
    lex = next(r for r in run(BenchOpts()).rows if r.name == "lexical")
    assert lex.absent is None
    assert lex.cells["threshold"] == 0.25
    assert 0.0 <= lex.cells["fpr"] <= 1.0
    assert 0.0 <= lex.cells["recall"] <= 1.0


def test_every_backend_appears_present_or_absent():
    from opendaisugi.bench.options import MATCHER_BACKENDS

    rows = run(BenchOpts()).rows
    assert {r.name for r in rows} == set(MATCHER_BACKENDS)
    for row in rows:
        assert row.absent is not None or "fpr" in row.cells


def test_an_absent_backend_teaches_the_install_or_fetch():
    for row in run(BenchOpts()).rows:
        if row.absent is not None:
            assert "opendaisugi[" in row.absent or "OPENDAISUGI_" in row.absent


def test_a_missing_package_is_reported_as_a_pip_install(monkeypatch):
    """A package that is not installed and a model that is not cached are two
    different absences, and the row must teach the right next command."""
    import importlib.util

    from opendaisugi.bench.layers.matcher import absent_hint

    real = importlib.util.find_spec

    def _no_model2vec(name, *args, **kwargs):
        return None if name == "model2vec" else real(name, *args, **kwargs)

    monkeypatch.setattr(importlib.util, "find_spec", _no_model2vec)
    assert "pip install 'opendaisugi[potion]'" in absent_hint("potion")
    assert "OPENDAISUGI_INT8_MODEL" in absent_hint("int8")


def test_recall_counts_paraphrase_pairs_over_the_threshold():
    tasks = [r["task"] for r in load_jsonl(resolve_corpus("tasks.jsonl", None))]
    paras = load_jsonl(resolve_corpus("paraphrases.jsonl", None))
    lexical = build_embedder("lexical")
    assert lexical is not None
    stats = measure(lexical.encode, tasks, paras)
    assert stats["pairs_same"] == 12
    assert stats["pairs_diff"] == 66  # C(12, 2)
    assert 0.0 <= stats["recall"] <= 1.0


def test_fpr_is_monotone_non_increasing_in_the_threshold():
    lexical = build_embedder("lexical")
    tasks = [r["task"] for r in load_jsonl(resolve_corpus("tasks.jsonl", None))]
    rates = [f for _, f in fpr_table(lexical.encode, tasks, (0.1, 0.3, 0.5, 0.7))]
    assert rates == sorted(rates, reverse=True)


def test_the_bench_never_downloads_a_model(monkeypatch):
    """A download inside a 30 s budget is a 90 MB surprise. Make every fetch
    path raise: each backend must either load from cache or report absent."""
    calls: list[str] = []

    def _boom(*args, **kwargs):
        calls.append("download")
        raise AssertionError("the bench tried to download a model")

    # snapshot_download is not on this list on purpose. In offline mode it is
    # the cache lookup, and model2vec calls it to find a cached folder. The
    # guards below are the paths bytes move through, plus the socket itself.
    for target in (
        "huggingface_hub.file_download.http_get",
        "urllib.request.urlopen",
        "socket.socket.connect",
        "opendaisugi._model_fetch.fetch",
    ):
        monkeypatch.setattr(target, _boom, raising=False)

    before = os.environ.get("HF_HUB_OFFLINE")
    table = run(BenchOpts())
    assert calls == []
    assert table.rows
    assert os.environ.get("HF_HUB_OFFLINE") == before


def test_an_uncached_int8_model_is_absent_and_never_fetched(monkeypatch, tmp_path):
    """int8 fetches its pinned files on first use when no local dir is named.
    The bench must never be that first use."""
    calls: list[str] = []

    def _boom(*args, **kwargs):
        calls.append("fetch")
        raise AssertionError("the bench tried to fetch the int8 files")

    monkeypatch.setattr("opendaisugi._model_fetch.fetch", _boom)
    monkeypatch.delenv("OPENDAISUGI_INT8_MODEL", raising=False)
    monkeypatch.setattr("opendaisugi._search._int8_cache_dir", lambda: tmp_path / "none")
    assert build_embedder("int8") is None
    assert calls == []
    assert "OPENDAISUGI_INT8_MODEL" not in os.environ


def test_offline_is_set_during_construction_and_restored_after(monkeypatch):
    from opendaisugi.bench.layers.matcher import _offline

    monkeypatch.delenv("HF_HUB_OFFLINE", raising=False)
    with _offline():
        assert os.environ["HF_HUB_OFFLINE"] == "1"
        import huggingface_hub.constants as hf_constants

        assert hf_constants.is_offline_mode() is True
    assert "HF_HUB_OFFLINE" not in os.environ


def test_json_and_determinism():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "matcher"
    assert body["reproduce"].startswith("uv run --no-sync daisugi bench matcher")
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))


def test_the_bench_finishes_well_under_thirty_seconds():
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0


# --- --corpus, the companion file, and the offline guard -----------------------

import subprocess
import sys

import pytest

from opendaisugi.bench.corpus import CorpusMissing


def _two_file_corpus(tmp_path):
    tasks = tmp_path / "tasks.jsonl"
    tasks.write_text(
        '{"id":"a1","task":"add a pytest test for the shell decomposer"}\n'
        '{"id":"a2","task":"bump the docker base image to bookworm"}\n'
        '{"id":"a3","task":"summarise the last ten journal traces"}\n'
    )
    (tmp_path / "paraphrases.jsonl").write_text(
        '{"id":"q1","of":"a1","task":"write a unit test that covers shell decomposition"}\n'
        '{"id":"q2","of":"a3","task":"give me a summary of the ten most recent journal traces"}\n'
    )
    return tasks


def test_corpus_override_reads_the_paraphrases_beside_it_and_pins_both(tmp_path):
    tasks = _two_file_corpus(tmp_path)
    table = run(BenchOpts(corpus=tasks))
    assert table.corpus.path == tasks
    assert [c.path for c in table.companions] == [tmp_path / "paraphrases.jsonl"]
    lex = next(r for r in table.rows if r.name == "lexical")
    assert lex.absent is None
    assert 0.0 <= lex.cells["recall"] <= 1.0
    assert str(tasks) in table.reproduce


def test_the_default_table_pins_the_paraphrase_file_too():
    table = run(BenchOpts())
    assert [c.rel for c in table.companions] == ["bench/corpus/paraphrases.jsonl"]


def test_a_paraphrase_of_an_unknown_task_is_a_corpus_error_not_a_key_error(tmp_path):
    tasks = _two_file_corpus(tmp_path)
    (tmp_path / "paraphrases.jsonl").write_text('{"id":"q1","of":"t01","task":"x"}\n')
    with pytest.raises(CorpusMissing) as exc:
        run(BenchOpts(corpus=tasks))
    assert "t01" in str(exc.value) and "paraphrases.jsonl" in str(exc.value)


def test_a_task_corpus_with_no_paraphrases_beside_it_teaches_the_layout(tmp_path):
    tasks = _two_file_corpus(tmp_path)
    (tmp_path / "paraphrases.jsonl").unlink()
    with pytest.raises(CorpusMissing) as exc:
        run(BenchOpts(corpus=tasks))
    assert "paraphrases.jsonl" in str(exc.value)


def test_offline_guard_restores_the_hub_flag_in_a_fresh_process():
    """In a process that has not imported the hub yet, the hub reads the env
    var the guard set and the guard must not then 'restore' it to offline."""
    code = (
        "from opendaisugi.bench.layers.matcher import _offline\n"
        "import os\n"
        "with _offline():\n"
        "    pass\n"
        "import huggingface_hub.constants as c\n"
        "print(c.is_offline_mode(), os.environ.get('HF_HUB_OFFLINE'))\n"
    )
    env = {k: v for k, v in os.environ.items() if k != "HF_HUB_OFFLINE"}
    proc = subprocess.run(
        [sys.executable, "-c", code], capture_output=True, text=True, env=env, check=False
    )
    assert proc.returncode == 0, proc.stderr[-500:]
    assert proc.stdout.strip() == "False None"


def test_an_installed_but_uncached_minilm_is_told_how_to_cache_it():
    from opendaisugi.bench.layers.matcher import absent_hint

    hint = absent_hint("all-MiniLM-L6-v2")
    assert "pip install" not in hint
    assert "matcher_model: all-MiniLM-L6-v2" in hint
    assert "HF_HOME" in hint


def test_a_backend_that_cannot_build_prints_absent_with_its_hint(monkeypatch):
    """int8 happens to be uncached on some boxes and cached on others, so the
    absent branch needs a test that forces it."""
    from opendaisugi.bench.layers import matcher as mod

    real = mod.build_embedder
    monkeypatch.setattr(
        mod, "build_embedder", lambda name: None if name == "potion" else real(name)
    )
    potion = next(r for r in run(BenchOpts()).rows if r.name == "potion")
    assert potion.absent is not None
    assert "OPENDAISUGI_POTION_MODEL" in potion.absent


def test_cache_mb_measures_a_potion_local_dir_named_by_the_env(tmp_path, monkeypatch):
    from opendaisugi.bench.layers.matcher import _cache_mb

    local = tmp_path / "potion-local"
    local.mkdir()
    (local / "model.safetensors").write_bytes(b"x" * (2 * 1024 * 1024))
    monkeypatch.setenv("OPENDAISUGI_POTION_MODEL", str(local))
    assert _cache_mb("potion") == 2.0
