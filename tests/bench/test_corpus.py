"""The committed corpora must parse, must be pinned, and must regenerate
byte-for-byte. A corpus whose ids drift silently makes every bench read zero."""

import json
import subprocess
import sys

import pytest

from opendaisugi.bench.corpus import (
    CorpusMissing,
    corpus_dir,
    corpus_ref,
    load_json,
    load_jsonl,
    resolve_corpus,
)


def test_every_committed_corpus_parses_and_is_non_empty():
    for rel in (
        "tasks.jsonl",
        "paraphrases.jsonl",
        "journal-slice.jsonl",
        "loop-vocabulary-cases.jsonl",
        "gate-paths.jsonl",
        "verifier.jsonl",
    ):
        rows = load_jsonl(resolve_corpus(rel, None))
        assert rows, f"{rel} is empty"
        assert all(isinstance(r, dict) and r.get("id") for r in rows), rel


def test_task_and_paraphrase_ids_line_up():
    tasks = {r["id"] for r in load_jsonl(resolve_corpus("tasks.jsonl", None))}
    paras = load_jsonl(resolve_corpus("paraphrases.jsonl", None))
    assert len(tasks) == 12
    assert len(paras) == 12
    assert {p["of"] for p in paras} == tasks


def test_corpus_ref_digest_is_the_file_bytes(tmp_path):
    import hashlib

    f = tmp_path / "x.jsonl"
    f.write_bytes(b'{"id":"a"}\n')
    ref = corpus_ref(f)
    assert ref.sha256 == hashlib.sha256(b'{"id":"a"}\n').hexdigest()
    assert ref.short == ref.sha256[:8]


def test_missing_corpus_teaches_the_next_command(tmp_path):
    with pytest.raises(CorpusMissing) as exc:
        resolve_corpus("nope.jsonl", None)
    assert "bench/corpus" in str(exc.value)


def test_override_path_wins(tmp_path):
    f = tmp_path / "other.jsonl"
    f.write_text('{"id":"z"}\n')
    ref = resolve_corpus("tasks.jsonl", f)
    assert ref.path == f


def test_verifier_corpus_manifest_pins_the_file():
    ref = resolve_corpus("verifier.jsonl", None)
    manifest = json.loads((ref.path.parent / "verifier.jsonl.manifest.json").read_text())
    assert manifest["sha256"] == ref.sha256
    assert manifest["count"] == len(load_jsonl(ref))
    assert manifest["v"] == 1


def test_verifier_corpus_regenerates_byte_identical(tmp_path):
    """The ids are content addresses. If the generator and the committed file
    disagree, `run_corpus` silently reports 'no verdict' for every case."""
    script = corpus_dir() / "make_verifier_corpus.py"
    out = tmp_path / "verifier.jsonl"
    subprocess.run(
        [sys.executable, str(script), "--out", str(out)], check=True, capture_output=True
    )
    assert out.read_bytes() == (corpus_dir() / "verifier.jsonl").read_bytes()


def test_the_corpus_has_a_case_only_a_full_profile_client_can_deny():
    """Without this case the fail-open column reads 0 for a Core client and
    certifies the hole instead of finding it."""
    cases = load_jsonl(resolve_corpus("verifier.jsonl", None))
    predicate_denials = [
        c
        for c in cases
        if c["kind"] == "verify"
        and c["expect"]["ok"] is False
        and any(v["stage"] == "predicate" for v in c["expect"]["violations"])
    ]
    assert predicate_denials, "no predicate-stage denial in the corpus"


def test_verifier_corpus_cases_carry_computed_ids():
    from opendaisugi.conformance import case_id

    for case in load_jsonl(resolve_corpus("verifier.jsonl", None)):
        body = {k: v for k, v in case.items() if k != "id"}
        assert case["id"] == case_id(body), case["id"]


def test_battery_envelope_and_vocabulary_load():
    env = load_json(resolve_corpus("battery-envelope.json", None))
    assert env["permissions"]["file_write"] == ["build/**"]
    vocab = load_json(resolve_corpus("loop-vocabulary.json", None))
    assert set(vocab) == {"sprig", "claude-code", "codex", "pi", "opencode"}
    assert vocab["opencode"]["names"] is None, "opencode's vocabulary is not pinned yet"


def test_pi_vocabulary_is_the_map_the_gate_consults():
    """pi's names come from hook.py's own pi map, so the corpus cannot drift
    from what the extension delivers under --format pi."""
    from opendaisugi.hook import _PI_TOOL_TYPE_MAP

    pi = load_json(resolve_corpus("loop-vocabulary.json", None))["pi"]
    assert pi["fmt"] == "pi"
    for tool in ("write", "edit", "bash", "read"):
        assert pi["names"][tool] in _PI_TOOL_TYPE_MAP, tool
    assert pi["names"]["unknown"] not in _PI_TOOL_TYPE_MAP
    assert pi["keys"]["path"] == "path"


def test_prices_cover_every_switchyard_target():
    prices = load_json(resolve_corpus("prices.json", None))
    targets = load_json(resolve_corpus("switchyard-targets.json", None))
    for model in targets.values():
        assert model in prices, model
    assert prices[targets["local"]] == [0.0, 0.0], "a local model costs no money"
