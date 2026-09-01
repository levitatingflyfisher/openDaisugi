"""Five pairs, no more. Each one crosses two layers where a real coupling exists,
and a pair whose layer bench has an absent option carries absent rows through."""

import json

import pytest

from opendaisugi.bench.pairs import (
    PAIRS,
    backend_x_envelope,
    index_of,
    loop_x_gate_path,
    matcher_x_distiller,
    router_x_models,
    run,
    run_all,
    verifier_x_shell,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json


@pytest.fixture(scope="module")
def tables():
    """One sweep for the module. The pairs are deterministic, so every test
    that only reads rows can share it instead of paying for the embedders again."""
    return run_all(BenchOpts())


def test_exactly_five_pairs():
    assert PAIRS == (
        "verifier-x-shell",
        "matcher-x-distiller",
        "router-x-models",
        "loop-x-gate-path",
        "backend-x-envelope",
    )


def test_the_index_does_not_re_run_the_pairs(monkeypatch):
    """Deriving the index from tables that already ran is the difference between
    `daisugi bench pairs` costing one sweep and costing two."""
    from opendaisugi.bench import pairs as pairs_mod

    calls: list[str] = []
    original = dict(pairs_mod._RUNNERS)
    try:
        for name, fn in original.items():

            def _counting(opts, _name=name, _fn=fn):
                calls.append(_name)
                return _fn(opts)

            pairs_mod._RUNNERS[name] = _counting
        run(BenchOpts())
    finally:
        pairs_mod._RUNNERS.clear()
        pairs_mod._RUNNERS.update(original)
    assert calls == list(PAIRS), "a pair ran more than once"


def test_run_all_returns_one_table_per_pair(tables):
    assert [t.layer for t in tables] == list(PAIRS)
    for table in tables:
        assert table.rows, table.layer
        assert table.reproduce.startswith("uv run --no-sync daisugi bench pairs")
        assert table.corpus is not None


def test_the_index_is_derived_from_the_tables_it_describes(tables):
    index = index_of(tables)
    assert [r.cells["rows"] for r in index.rows] == [len(t.rows) for t in tables]


def test_the_index_table_says_why_each_pair_earns_a_row(tables):
    table = index_of(tables)
    assert table.layer == "pairs"
    assert [r.name for r in table.rows] == list(PAIRS)
    for row in table.rows:
        assert row.cells["why"].strip()
        assert " x " in row.cells["layers"]


def test_verifier_x_shell_crosses_both_decomposition_settings(tables):
    table = tables[0]
    settings = {r.cells.get("decomposition") for r in table.rows if r.absent is None}
    assert settings == {"on", "off"}


def test_verifier_x_shell_sets_the_envelope_field():
    """The axis has to be the field, not a filter over the same corpus."""
    from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
    from opendaisugi.bench.layers.verifier import CORPUS
    from opendaisugi.bench.pairs import _decomposition_cases

    cases = load_jsonl(resolve_corpus(CORPUS, None))
    off = _decomposition_cases(cases, False)
    on = _decomposition_cases(cases, True)
    assert off and len(off) == len(on)
    assert all(c["kind"] == "verify" for c in off + on)
    assert all(c["envelope"]["permissions"]["shell_allow_decomposition"] is False for c in off)
    assert all(c["envelope"]["permissions"]["shell_allow_decomposition"] is True for c in on)
    # Different envelopes are different cases, so their content addresses differ.
    assert {c["id"] for c in off}.isdisjoint({c["id"] for c in on})


def test_verifier_x_shell_recomputes_the_expectation_with_the_oracle():
    from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
    from opendaisugi.bench.layers.verifier import CORPUS
    from opendaisugi.bench.pairs import _decomposition_cases
    from opendaisugi.conformance import case_id
    from opendaisugi.models import ActionPlan, Envelope
    from opendaisugi.verify import verify

    cases = load_jsonl(resolve_corpus(CORPUS, None))
    for case in _decomposition_cases(cases, True):
        body = {k: v for k, v in case.items() if k != "id"}
        assert case["id"] == case_id(body)
        result = verify(
            ActionPlan.model_validate(case["plan"]),
            Envelope.model_validate(case["envelope"]),
            strict=case["options"]["strict"],
            z3_timeout_ms=case["options"]["z3_timeout_ms"],
        )
        assert case["expect"]["ok"] is result.ok


def test_the_committed_corpus_separates_the_two_decomposition_settings():
    """At least one verify case must flip between off and on, or the pair
    exists to catch a coupling no case reaches. `echo ok && echo done` is
    refused outright with decomposition off and allowed with it on, because
    every head is in the allowlist."""
    from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
    from opendaisugi.bench.layers.verifier import CORPUS
    from opendaisugi.bench.pairs import _decomposition_cases
    from opendaisugi.shell_decompose import parser_available

    assert parser_available(), "the shell extra is needed to separate the axis"
    cases = load_jsonl(resolve_corpus(CORPUS, None))
    off = _decomposition_cases(cases, False)
    on = _decomposition_cases(cases, True)
    flips = [
        (a["plan"]["steps"][0]["command"], a["expect"]["ok"], b["expect"]["ok"])
        for a, b in zip(off, on, strict=True)
        if a["expect"]["ok"] != b["expect"]["ok"]
    ]
    assert flips, "no verify case changes verdict with decomposition on"
    assert all(ok_off is False and ok_on is True for _, ok_off, ok_on in flips)


def test_verifier_x_shell_shows_how_many_cases_the_oracle_allows_per_setting():
    table = verifier_x_shell(BenchOpts())
    python = {
        r.cells["decomposition"]: r.cells for r in table.rows if r.cells.get("option") == "python"
    }
    assert python["on"]["allows"] > python["off"]["allows"]
    assert not any("does not separate" in n for n in table.notes)


def test_verifier_x_shell_says_so_when_the_corpus_does_not_separate_the_axis(monkeypatch):
    from opendaisugi.bench import pairs as pairs_mod

    real = pairs_mod._decomposition_cases
    monkeypatch.setattr(
        pairs_mod, "_decomposition_cases", lambda cases, setting: real(cases, False)
    )
    table = verifier_x_shell(BenchOpts())
    assert any("does not separate" in n for n in table.notes)


def test_verifier_x_shell_names_a_broken_client_in_its_own_cell(monkeypatch):
    from opendaisugi.bench import pairs as pairs_mod

    monkeypatch.setattr(
        pairs_mod.verifier_bench, "client_verdicts", lambda *a, **k: ({}, 1.0, "exit 3")
    )
    table = verifier_x_shell(BenchOpts())
    present = [r for r in table.rows if r.absent is None]
    assert present
    assert all(r.cells["failure"] == "exit 3" for r in present)
    # Unknown, not zero: a fail-open cell of 0 would read as safe.
    assert all(r.cells["agree"] == "n/a" and r.cells["fail_open"] == "n/a" for r in present)
    assert all(r.cells["cases"] > 0 for r in present)


def test_verifier_x_shell_carries_absent_clients_through(monkeypatch):
    from opendaisugi.bench import pairs as pairs_mod

    monkeypatch.setattr(pairs_mod, "client_is_built", lambda spec: spec.name == "python")
    table = verifier_x_shell(BenchOpts())
    absent = {r.name: r.absent for r in table.rows if r.absent is not None}
    assert "rust" in absent and "cargo build --release" in absent["rust"]
    assert {r.name for r in table.rows if r.absent is None} == {"python/off", "python/on"}


def test_verifier_x_shell_runs_each_client_once_for_both_settings(monkeypatch):
    """One client invocation per client, so one hung client costs one
    timeout, not one per setting, and the pair stays inside the budget."""
    from opendaisugi.bench import pairs as pairs_mod

    calls: list[int] = []
    real = pairs_mod.verifier_bench.client_verdicts

    def _counting(argv, cases, **kw):
        calls.append(len(cases))
        return real(argv, cases, **kw)

    monkeypatch.setattr(pairs_mod, "client_is_built", lambda spec: spec.name == "python")
    monkeypatch.setattr(pairs_mod.verifier_bench, "client_verdicts", _counting)
    table = verifier_x_shell(BenchOpts())
    assert len(calls) == 1
    present = [r for r in table.rows if r.absent is None]
    assert [r.cells["decomposition"] for r in present] == ["off", "on"]
    assert calls[0] == sum(r.cells["cases"] for r in present)
    assert all(r.cells["agree"] == r.cells["cases"] for r in present)


def test_backend_x_envelope_absent_rows_carry_the_recording_hint():
    from opendaisugi.bench import pairs as pairs_mod

    table = backend_x_envelope(BenchOpts())
    absent = {r.name: r.absent for r in table.rows if r.absent is not None}
    assert absent
    for name, hint in absent.items():
        assert f"envelopes/{name}.jsonl" in hint
        assert "claude-code.jsonl" in hint
    layer = {r.name: r.absent for r in pairs_mod.backend_bench.run(BenchOpts()).rows if r.absent}
    assert absent == layer


def test_matcher_x_distiller_reports_clusters_and_paraphrase_purity(tables):
    table = tables[1]
    lex = next(r for r in table.rows if r.name.startswith("lexical"))
    assert lex.cells["clusters"] >= 1
    assert 0.0 <= lex.cells["purity"] <= 1.0
    assert [c.rel.rsplit("/", 1)[-1] for c in table.companions] == ["paraphrases.jsonl"]


def test_matcher_x_distiller_carries_absent_backends_through(monkeypatch):
    from opendaisugi.bench import pairs as pairs_mod

    real = pairs_mod.matcher_bench.build_embedder
    monkeypatch.setattr(
        pairs_mod.matcher_bench,
        "build_embedder",
        lambda name: real(name) if name == "lexical" else None,
    )
    table = matcher_x_distiller(BenchOpts())
    absent = {r.name: r.absent for r in table.rows if r.absent is not None}
    assert "potion" in absent and absent["potion"]
    assert [r.name for r in table.rows if r.absent is None] == ["lexical@0.25"]


def test_router_x_models_crosses_routers_with_cheap_model_choices():
    table = router_x_models(BenchOpts())
    assert len({r.cells["cheap_model"] for r in table.rows}) >= 2
    names = {c.rel.rsplit("/", 1)[-1] for c in table.companions}
    assert names == {"prices.json", "switchyard-targets.json"}


def test_loop_x_gate_path_carries_the_fail_open_class():
    table = loop_x_gate_path(BenchOpts())
    present = [r for r in table.rows if r.absent is None]
    assert present
    assert all(r.cells["fail_open"] for r in present)
    assert all(r.cells["contract"] == "4/4" for r in present)


def test_backend_x_envelope_crosses_both_envelope_sources():
    table = backend_x_envelope(BenchOpts())
    sources = {r.cells.get("envelope_source") for r in table.rows if r.absent is None}
    assert sources == {"evidence-inferred", "llm-generated"}


def test_the_evidence_inferred_row_claims_only_what_it_measured(tables):
    """It must not borrow the LLM row's clause count. Nothing evidence-inferred
    was recorded, so those cells are n/a and only tokens=0 is asserted."""
    rows = [
        r
        for r in tables[4].rows
        if r.absent is None and r.cells["envelope_source"] == "evidence-inferred"
    ]
    assert rows
    for row in rows:
        assert row.cells["mean_clauses"] == "n/a"
        assert row.cells["envelopes"] == "n/a"
        assert row.cells["tokens"] == 0


def test_a_pair_with_an_absent_option_prints_absent_rows(tables):
    for table in tables:
        for row in table.rows:
            assert row.absent is not None or row.cells


def test_pairs_json_and_determinism(tables):
    body = json.loads(to_json(index_of(tables)))
    assert body["layer"] == "pairs"
    again = run_all(BenchOpts())
    for first, second in zip(tables, again, strict=True):
        assert stable_rows(first) == stable_rows(second), first.layer


def test_cli_pairs_prints_five_tables_and_all_prints_the_index_only():
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    result = CliRunner().invoke(app, ["bench", "pairs", "--json"])
    assert result.exit_code == 0, result.output
    body = json.loads(result.output)
    assert isinstance(body, list) and [t["layer"] for t in body] == list(PAIRS)
    result = CliRunner().invoke(app, ["bench", "all", "--json"])
    assert result.exit_code == 0, result.output
    layers = [t["layer"] for t in json.loads(result.output)]
    assert "pairs" in layers and "verifier-x-shell" not in layers
