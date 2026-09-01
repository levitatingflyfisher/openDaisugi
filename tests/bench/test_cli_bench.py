"""`daisugi bench` dispatches to a registered layer, honours --json and
--corpus, and teaches the next command when the layer or corpus is wrong."""

import json

import pytest
from typer.testing import CliRunner

from opendaisugi.bench.registry import BenchOpts, BenchSpec, layer_names, register, run_bench
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.cli import app

runner = CliRunner()


@pytest.fixture()
def fake_layer():
    columns = (Column("option", "option"), Column("n", "n", align="right"))

    def _run(opts: BenchOpts) -> Table:
        from opendaisugi.bench.corpus import resolve_corpus

        ref = resolve_corpus("tasks.jsonl", opts.corpus)
        return Table(
            layer="fake",
            columns=columns,
            rows=(Row(name="one", cells={"option": "one", "n": 1}),),
            corpus=ref,
            reproduce="uv run --no-sync daisugi bench fake",
        )

    register(BenchSpec(name="fake", layer="fake", corpus="tasks.jsonl", columns=columns, run=_run))
    yield
    from opendaisugi.bench import registry

    registry._SPECS.pop("fake", None)


def test_registered_layer_is_listed_and_runnable(fake_layer):
    assert "fake" in layer_names()
    table = run_bench("fake", BenchOpts())
    assert table.rows[0].cells["n"] == 1


def test_cli_prints_the_human_table(fake_layer):
    result = runner.invoke(app, ["bench", "fake"])
    assert result.exit_code == 0, result.output
    assert "reproduce:" in result.output and "corpus:" in result.output


def test_cli_json_is_the_documented_shape(fake_layer):
    result = runner.invoke(app, ["bench", "fake", "--json"])
    assert result.exit_code == 0, result.output
    body = json.loads(result.output)
    assert set(body) >= {"layer", "corpus", "rows", "columns", "reproduce"}


def test_cli_unknown_layer_lists_the_real_ones():
    result = runner.invoke(app, ["bench", "nope"])
    assert result.exit_code == 1
    assert "nope" in result.output
    assert "verifier" in result.output or "fake" in result.output


def test_cli_missing_corpus_names_bench_corpus(fake_layer, tmp_path):
    result = runner.invoke(app, ["bench", "fake", "--corpus", str(tmp_path / "gone.jsonl")])
    assert result.exit_code == 1
    assert "bench/corpus" in result.output


def test_cli_all_runs_every_registered_layer(fake_layer):
    result = runner.invoke(app, ["bench", "all"])
    assert result.exit_code == 0, result.output
    assert result.output.count("reproduce:") >= 1


def test_cli_all_json_is_one_document_a_list_of_tables(fake_layer):
    result = runner.invoke(app, ["bench", "all", "--json"])
    assert result.exit_code == 0, result.output
    body = json.loads(result.output)
    assert isinstance(body, list)
    assert any(t["layer"] == "fake" for t in body)
    assert all({"layer", "corpus", "rows", "columns", "reproduce"} <= set(t) for t in body)


def test_cli_single_layer_json_stays_one_object(fake_layer):
    result = runner.invoke(app, ["bench", "fake", "--json"])
    assert isinstance(json.loads(result.output), dict)
