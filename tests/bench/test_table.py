"""A table must print what it read and how to re-read it, must say `absent`
rather than drop a row, and must compare equal across runs once the timing
columns are set aside."""

import json
from pathlib import Path

from opendaisugi.bench.corpus import CorpusRef
from opendaisugi.bench.table import Column, Row, Table, render, stable_rows, to_json

COLUMNS = (
    Column("option", "option"),
    Column("cases", "cases", align="right"),
    Column("ms", "ms/case", align="right", volatile=True),
)


def _table() -> Table:
    return Table(
        layer="verifier",
        columns=COLUMNS,
        rows=(
            Row(name="python", cells={"option": "python", "cases": 46, "ms": 0.14}),
            Row(name="lean", absent="cd clients/lean && lake build"),
        ),
        corpus=CorpusRef(
            path=Path("/x/verifier.jsonl"),
            rel="bench/corpus/verifier.jsonl",
            sha256="ab" * 32,
        ),
        reproduce="uv run --no-sync daisugi bench verifier",
        notes=("python is the oracle",),
    )


def test_render_shows_every_column_and_the_absent_row():
    art = render(_table())
    assert "option" in art and "ms/case" in art
    assert "python" in art and "46" in art
    assert "lean" in art and "absent" in art
    assert "cd clients/lean && lake build" in art


def test_render_ends_with_the_corpus_and_reproduce_lines():
    lines = [ln for ln in render(_table()).splitlines() if ln.strip()]
    assert lines[-2].startswith("corpus: bench/corpus/verifier.jsonl (abababab)")
    assert lines[-1] == "reproduce: uv run --no-sync daisugi bench verifier"


def test_json_carries_layer_corpus_rows_columns_and_reproduce():
    body = json.loads(to_json(_table()))
    assert body["layer"] == "verifier"
    assert body["corpus"] == {"path": "bench/corpus/verifier.jsonl", "sha256": "ab" * 32}
    assert body["reproduce"] == "uv run --no-sync daisugi bench verifier"
    assert [c["key"] for c in body["columns"]] == ["option", "cases", "ms"]
    assert body["columns"][2]["volatile"] is True
    assert body["rows"][1] == {"name": "lean", "absent": "cd clients/lean && lake build"}


def test_stable_rows_drop_the_volatile_columns():
    assert stable_rows(_table()) == [
        {"name": "python", "option": "python", "cases": 46},
        {"name": "lean", "absent": "cd clients/lean && lake build"},
    ]


def test_a_table_with_no_corpus_says_so_rather_than_printing_a_fake_digest():
    t = Table(layer="loop", columns=COLUMNS, rows=(), corpus=None, reproduce="daisugi bench loop")
    art = render(t)
    assert "corpus: none" in art


def test_floats_keep_four_decimals_so_a_small_rate_is_not_rounded_to_zero():
    from opendaisugi.bench.table import _cell_text

    assert _cell_text(0.0152) == "0.0152"
    assert _cell_text(3.2) == "3.2"
    assert _cell_text(262.0) == "262.0"
    assert _cell_text(0.0) == "0.0"
    assert _cell_text(46) == "46"
    assert _cell_text(None) == ""


def test_a_companion_corpus_is_pinned_in_both_the_text_and_the_json():
    """A bench that reads two files must print both digests, or an edit to the
    second file changes the numbers under an unchanged corpus line."""
    base = _table()
    paras = CorpusRef(
        path=Path("/x/paraphrases.jsonl"), rel="bench/corpus/paraphrases.jsonl", sha256="cd" * 32
    )
    t = Table(
        layer=base.layer,
        columns=base.columns,
        rows=base.rows,
        corpus=base.corpus,
        reproduce=base.reproduce,
        companions=(paras,),
    )
    lines = [ln for ln in render(t).splitlines() if ln.strip()]
    assert lines[-3].startswith("corpus: bench/corpus/verifier.jsonl (abababab)")
    assert lines[-2] == "corpus: bench/corpus/paraphrases.jsonl (cdcdcdcd)"
    assert lines[-1].startswith("reproduce:")
    body = json.loads(to_json(t))
    assert body["companions"] == [{"path": "bench/corpus/paraphrases.jsonl", "sha256": "cd" * 32}]
    assert json.loads(to_json(base))["companions"] == []
