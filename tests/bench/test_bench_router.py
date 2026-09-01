"""The router bench replays a committed journal slice through each routing
option, prices the result from a committed price table, and never invents a
pass-rate it has no outcomes for."""

import json
import time

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.layers.router import (
    COLUMNS,
    ROUTERS,
    choose,
    escalation_router,
    run,
    stage_router,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json

TARGETS = {"local": "L", "cheap": "C", "capable": "K"}


def _turn(text: str, prefix_tokens: int = 0) -> dict:
    return {
        "body": {"model": "claude-opus-4-8", "messages": [{"role": "user", "content": text}]},
        "prefix_tokens": prefix_tokens,
    }


def test_columns_and_options():
    assert [c.key for c in COLUMNS] == [
        "option",
        "turns",
        "local_share",
        "cost",
        "pass_rate",
        "source",
    ]
    assert ROUTERS == ("rules", "switchyard:stage", "switchyard:escalation", "off")


def test_off_never_changes_the_requested_model():
    assert choose("off", _turn("list the files"), TARGETS) == "claude-opus-4-8"


def test_rules_takes_the_local_rung_on_an_easy_turn_and_keeps_a_hard_one():
    easy = _turn("list the files in src")
    hard = _turn("debug the race condition in the pane watcher")
    assert choose("rules", easy, TARGETS) == "L"
    assert choose("rules", hard, TARGETS) == "claude-opus-4-8"


def test_rules_reaches_the_cheap_rung_when_no_local_model_is_offered():
    no_local = dict(TARGETS, local="claude-haiku-4-5")
    assert choose("rules", _turn("list the files in src"), no_local) == "claude-haiku-4-5"


def test_rules_local_share_is_not_structurally_zero():
    """Without local_model the built-in router can never reach tier1-local, so
    its local share would always read 0.0 and the comparison against the
    Switchyard rows would be rigged."""
    share = {r.name: r.cells["local_share"] for r in run(BenchOpts()).rows}
    assert share["rules"] > 0.0


def test_rules_keeps_a_large_cached_prefix_on_the_requested_model_when_no_local_rung():
    """Stickiness never blocks the local rung, so the sticky path is only
    observable when no local target is offered at all."""
    no_local = dict(TARGETS, local=None)
    sticky = _turn("add a docstring here", prefix_tokens=6000)
    assert choose("rules", sticky, no_local) == "claude-opus-4-8"


def test_the_two_fake_switchyard_strategies_differ_on_a_middling_turn():
    assert stage_router(0.10, TARGETS) == "L"
    assert stage_router(0.35, TARGETS) == "C"
    assert stage_router(0.80, TARGETS) == "K"
    assert escalation_router(0.35, TARGETS) == "L"
    assert escalation_router(0.80, TARGETS) == "K"


def test_an_unknown_option_is_refused():
    import pytest

    with pytest.raises(KeyError):
        choose("coin-flip", _turn("list the files"), TARGETS)


def test_every_router_option_gets_a_row_with_the_corpus_turn_count():
    turns = len(load_jsonl(resolve_corpus("journal-slice.jsonl", None)))
    rows = run(BenchOpts()).rows
    assert [r.name for r in rows] == list(ROUTERS)
    for row in rows:
        assert row.cells["turns"] == turns


def test_off_costs_at_least_as_much_as_every_router():
    cells = {r.name: r.cells for r in run(BenchOpts()).rows}
    for name in ("rules", "switchyard:stage", "switchyard:escalation"):
        assert cells[name]["cost"] <= cells["off"]["cost"], name


def test_off_routes_nothing_local_and_the_fakes_say_they_are_fake():
    cells = {r.name: r.cells for r in run(BenchOpts()).rows}
    assert cells["off"]["local_share"] == 0.0
    assert cells["switchyard:stage"]["source"] == "fake"
    assert cells["switchyard:escalation"]["source"] == "fake"
    assert cells["rules"]["source"] == "built-in"
    assert cells["off"]["source"] == "built-in"


def test_pass_rate_is_absent_not_a_fake_hundred_percent():
    for row in run(BenchOpts()).rows:
        assert row.cells["pass_rate"] == "n/a, no outcomes"


def test_local_share_is_a_fraction_of_the_corpus():
    for row in run(BenchOpts()).rows:
        assert 0.0 <= row.cells["local_share"] <= 1.0


def test_the_table_pins_every_file_it_read():
    table = run(BenchOpts())
    assert table.corpus is not None and table.corpus.rel.endswith("journal-slice.jsonl")
    companions = {c.rel.rsplit("/", 1)[-1] for c in table.companions}
    assert companions == {"prices.json", "switchyard-targets.json"}


def test_corpus_override_reads_the_price_and_target_files_beside_it(tmp_path):
    src = resolve_corpus("journal-slice.jsonl", None).path.parent
    for name in ("prices.json", "switchyard-targets.json"):
        (tmp_path / name).write_bytes((src / name).read_bytes())
    slice_path = tmp_path / "mine.jsonl"
    slice_path.write_text(
        json.dumps(
            {
                "id": "x1",
                "prefix_tokens": 0,
                "body": {
                    "model": "claude-opus-4-8",
                    "messages": [{"role": "user", "content": "list the files"}],
                },
                "usage": {"input_tokens": 100, "output_tokens": 10},
            }
        )
        + "\n"
    )
    table = run(BenchOpts(corpus=slice_path))
    assert all(r.cells["turns"] == 1 for r in table.rows)
    assert {c.path.parent for c in table.companions} == {tmp_path}


def test_corpus_override_without_the_price_table_teaches_the_fix(tmp_path):
    import pytest

    from opendaisugi.bench.corpus import CorpusMissing

    slice_path = tmp_path / "mine.jsonl"
    slice_path.write_text("")
    with pytest.raises(CorpusMissing, match="prices.json"):
        run(BenchOpts(corpus=slice_path))


def test_a_model_the_price_table_lacks_is_a_corpus_error_not_a_guess(tmp_path):
    """price_turn would fall back to a constant the table does not name."""
    import pytest

    from opendaisugi.bench.corpus import CorpusMissing

    src = resolve_corpus("journal-slice.jsonl", None).path.parent
    for name in ("prices.json", "switchyard-targets.json"):
        (tmp_path / name).write_bytes((src / name).read_bytes())
    slice_path = tmp_path / "mine.jsonl"
    slice_path.write_text(
        json.dumps(
            {
                "id": "x1",
                "prefix_tokens": 0,
                "body": {
                    "model": "claude-mystery-9",
                    "messages": [{"role": "user", "content": "list the files"}],
                },
                "usage": {"input_tokens": 100, "output_tokens": 10},
            }
        )
        + "\n"
    )
    with pytest.raises(CorpusMissing, match="claude-mystery-9") as exc:
        run(BenchOpts(corpus=slice_path))
    assert "prices.json" in str(exc.value)


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "router"
    assert len(body["companions"]) == 2
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0


def test_prices_come_from_the_committed_table():
    prices = load_json(resolve_corpus("prices.json", None))
    assert prices["qwen2.5-coder-7b"] == [0.0, 0.0]
