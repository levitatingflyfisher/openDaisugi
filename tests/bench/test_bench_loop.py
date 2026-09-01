"""The loop bench replays nine gate cases in each loop's own tool vocabulary.
A loop whose vocabulary is not pinned prints absent; a deny case never allows."""

import json
import time

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.layers.gate_path import battery_envelope
from opendaisugi.bench.layers.loop import (
    COLUMNS,
    payload_for,
    payload_format,
    run,
    run_vocabulary_cases,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json
from opendaisugi.gate import evaluate_call


def _cases():
    return load_jsonl(resolve_corpus("loop-vocabulary-cases.jsonl", None))


def _vocab():
    return load_json(resolve_corpus("loop-vocabulary.json", None))


def test_columns():
    assert [c.key for c in COLUMNS] == [
        "option",
        "installed",
        "gate_cases",
        "gate_path",
        "fail_open",
        "source",
    ]


def test_there_are_nine_vocabulary_cases():
    assert len(_cases()) == 9


def test_payload_uses_the_loop_s_own_names_and_keys():
    case = next(c for c in _cases() if c["case"] == "write-allow")
    claude = payload_for("claude-code", case, _vocab())
    assert claude["tool_name"] == "Write"
    assert claude["tool_input"]["file_path"] == "build/out.txt"
    codex = payload_for("codex", case, _vocab())
    assert codex["tool_name"] == "apply_patch"
    pi = payload_for("pi", case, _vocab())
    assert pi["tool_name"] == "write"
    assert pi["tool_input"]["path"] == "build/out.txt"


def test_pi_is_judged_under_its_own_format_and_the_others_under_claude_s():
    vocab = _vocab()
    assert payload_format("pi", vocab) == "pi"
    for loop in ("claude-code", "sprig", "codex"):
        assert payload_format(loop, vocab) == "claude"


def test_a_loop_without_the_tool_yields_no_payload():
    read_case = next(c for c in _cases() if c["case"] == "read-allow")
    assert payload_for("codex", read_case, _vocab()) is None


def test_an_unpinned_vocabulary_yields_no_payload_at_all():
    for case in _cases():
        assert payload_for("opencode", case, _vocab()) is None


def test_claude_code_passes_every_vocabulary_case():
    passed, applicable, na = run_vocabulary_cases(
        "claude-code", _cases(), _vocab(), battery_envelope()
    )
    assert (passed, applicable, na) == (9, 9, 0)


def test_pi_passes_every_vocabulary_case_in_lowercase():
    """pi's names only classify under its own format. Judged as Claude payloads
    every one would deny, which is fail-closed and also not what pi delivers."""
    passed, applicable, na = run_vocabulary_cases("pi", _cases(), _vocab(), battery_envelope())
    assert (passed, applicable, na) == (9, 9, 0)


def test_codex_read_case_is_not_applicable_not_a_pass():
    passed, applicable, na = run_vocabulary_cases("codex", _cases(), _vocab(), battery_envelope())
    assert na == 1
    assert applicable == 8
    assert passed <= applicable


def test_codex_write_tool_is_refused_by_the_gate_and_that_is_counted():
    """apply_patch is not in the gate's classification map, so codex's write
    and edit allow cases deny. The bench reports that as a miss, not a pass."""
    passed, applicable, _ = run_vocabulary_cases("codex", _cases(), _vocab(), battery_envelope())
    assert passed < applicable


def test_no_loop_is_ever_allowed_an_out_of_scope_write():
    envelope = battery_envelope()
    vocab = _vocab()
    deny_case = next(c for c in _cases() if c["case"] == "write-deny-scope")
    for loop in vocab:
        payload = payload_for(loop, deny_case, vocab)
        if payload is None:
            continue
        fmt = payload_format(loop, vocab)
        assert evaluate_call(payload, envelope, mode="enforce", fmt=fmt).allow is False, loop


def test_every_loop_row_names_its_gate_path_and_class():
    from opendaisugi.bench.options import GATE_PATHS, LOOPS

    for row in run(BenchOpts()).rows:
        if row.absent is not None:
            continue
        spec = LOOPS[row.name]
        assert row.cells["gate_path"] == spec.gate_path
        assert row.cells["fail_open"] == GATE_PATHS[spec.gate_path].fail_open_class


def test_an_unpinned_loop_row_is_absent_with_its_install_command():
    rows = {r.name: r for r in run(BenchOpts()).rows}
    assert rows["opencode"].absent is not None
    assert "npm install -g opencode-ai" in rows["opencode"].absent
    assert rows["pi"].absent is None


def test_live_without_the_binary_prints_absent(monkeypatch):
    from opendaisugi.bench.layers import loop

    monkeypatch.setattr(loop, "loop_is_installed", lambda spec: False)
    rows = {r.name: r for r in run(BenchOpts(live=True)).rows}
    assert rows["claude-code"].absent == "npm install -g @anthropic-ai/claude-code"


def test_the_table_pins_the_vocabulary_and_the_envelope():
    table = run(BenchOpts())
    names = [c.rel.rsplit("/", 1)[-1] for c in table.companions]
    assert names == ["loop-vocabulary.json", "battery-envelope.json"]


def test_corpus_override_reads_the_vocabulary_beside_it(tmp_path):
    import pytest

    from opendaisugi.bench.corpus import CorpusMissing

    cases = tmp_path / "cases.jsonl"
    cases.write_text("")
    with pytest.raises(CorpusMissing, match="loop-vocabulary.json"):
        run(BenchOpts(corpus=cases))
    src = resolve_corpus("loop-vocabulary.json", None).path
    (tmp_path / "loop-vocabulary.json").write_bytes(src.read_bytes())
    table = run(BenchOpts(corpus=cases))
    present = [r for r in table.rows if r.absent is None]
    assert present and all(r.cells["gate_cases"] == "0/0" for r in present)


def test_the_gate_cases_cell_reads_as_a_sentence_not_a_parenthetical():
    rows = {r.name: r for r in run(BenchOpts()).rows}
    assert rows["codex"].cells["gate_cases"] == "6/8, 1 n/a"
    assert rows["claude-code"].cells["gate_cases"] == "9/9"


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "loop"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
