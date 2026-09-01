"""The backend bench scores recorded envelopes on envelope self-consistency and
size. It never calls a model without --live, and a backend with no recording
prints absent with the command that records one."""

import json
import time

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers.backend import COLUMNS, clause_count, run, self_consistent
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json
from opendaisugi.models import Envelope, Invariant, Permission


def test_columns_name_what_they_measure():
    keys = [c.key for c in COLUMNS]
    assert keys == ["option", "envelopes", "self_consistent", "mean_clauses", "tokens", "source"]
    titles = {c.key: c.title for c in COLUMNS}
    assert titles["self_consistent"] == "self-consistent"


def test_clause_count_counts_permissions_and_invariants():
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo", "ls"], file_write=["build/**"]),
    )
    assert clause_count(env) == 4  # shell + two allowlist entries + one write scope


def test_a_satisfiable_envelope_is_self_consistent():
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
        invariants=[
            Invariant(
                type="shell_only",
                description="every step is a shell step",
                expr={
                    "op": "forall_steps",
                    "pred": {"op": "equals", "path": "type", "value": "shell"},
                },
            )
        ],
    )
    assert self_consistent(env) is True


def test_a_contradictory_invariant_is_not_self_consistent():
    """The column has to be able to read False, or it certifies nothing. A step
    cannot be both a shell step and a file_read step, so this admits no plan."""
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True),
        invariants=[
            Invariant(
                type="impossible",
                description="contradictory by construction",
                expr={
                    "op": "and",
                    "children": [
                        {"op": "equals", "path": "type", "value": "shell"},
                        {"op": "equals", "path": "type", "value": "file_read"},
                    ],
                },
            )
        ],
    )
    assert self_consistent(env) is False


def test_a_tautological_invariant_still_counts_as_self_consistent():
    """A tautology admits everything, which is the opposite failure. It is not a
    self-inconsistency and must not be reported under this column's name."""
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True),
        invariants=[
            Invariant(
                type="always",
                description="tautological by construction",
                expr={
                    "op": "or",
                    "children": [
                        {"op": "equals", "path": "type", "value": "shell"},
                        {"op": "not_equals", "path": "type", "value": "shell"},
                    ],
                },
            )
        ],
    )
    assert self_consistent(env) is True


def test_an_invariant_the_parser_rejects_is_not_self_consistent():
    """A check that cannot complete is not a pass."""
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True),
        invariants=[
            Invariant(type="bad", description="no such op", expr={"op": "teleport"}),
        ],
    )
    assert self_consistent(env) is False


def test_an_invariant_with_no_expression_is_skipped_not_failed():
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True),
        invariants=[Invariant(type="prose", description="no expr, prose only")],
    )
    assert self_consistent(env) is True


def test_every_configured_backend_gets_a_row():
    from opendaisugi.swap import SWAP_KNOBS

    names = {o.value for o in SWAP_KNOBS["backend"].options if o.value is not None}
    assert names, "the knob must still name real backends"
    assert {r.name for r in run(BenchOpts()).rows} == names


def test_the_recorded_backend_row_reads_its_recording():
    rows = {r.name: r for r in run(BenchOpts()).rows}
    recorded = load_jsonl(resolve_corpus("envelopes/claude-code.jsonl", None))
    tasks = load_jsonl(resolve_corpus("tasks.jsonl", None))
    assert rows["claude-code"].cells["envelopes"] == f"{len(recorded)}/{len(tasks)}"
    assert rows["claude-code"].cells["source"] == "recorded"
    assert rows["claude-code"].cells["self_consistent"] == len(recorded)


def test_an_unrecorded_backend_prints_absent_with_the_recording_command():
    rows = {r.name: r for r in run(BenchOpts()).rows}
    assert rows["ollama"].absent is not None
    assert "envelopes/ollama.jsonl" in rows["ollama"].absent
    assert "claude-code.jsonl" in rows["ollama"].absent


def test_recorded_rows_carry_their_own_provenance():
    task_ids = {t["id"] for t in load_jsonl(resolve_corpus("tasks.jsonl", None))}
    for row in load_jsonl(resolve_corpus("envelopes/claude-code.jsonl", None)):
        assert row["backend"] == "claude-code"
        assert row["model"]
        assert row["recorded_at"]
        assert row["task_id"] in task_ids


def test_a_recording_for_a_task_the_corpus_lacks_is_a_corpus_error(tmp_path):
    import pytest

    from opendaisugi.bench.corpus import CorpusMissing

    tasks = tmp_path / "tasks.jsonl"
    tasks.write_text('{"id":"t01","task":"one"}\n')
    (tmp_path / "envelopes").mkdir()
    src = resolve_corpus("envelopes/claude-code.jsonl", None).path
    (tmp_path / "envelopes" / "claude-code.jsonl").write_bytes(src.read_bytes())
    with pytest.raises(CorpusMissing, match="t02"):
        run(BenchOpts(corpus=tasks))


def test_the_table_pins_every_recording_it_read():
    table = run(BenchOpts())
    names = [c.rel.rsplit("/", 1)[-1] for c in table.companions]
    assert names == ["claude-code.jsonl"]


def test_the_bench_calls_no_model_without_live(monkeypatch):
    def _boom(*a, **k):
        raise AssertionError("the bench must not call a model without --live")

    monkeypatch.setattr("opendaisugi.llm.get_instructor_client", _boom)
    monkeypatch.setattr("opendaisugi.llm.generate_envelope", _boom, raising=False)
    run(BenchOpts())


def test_live_is_refused_with_the_reason_not_a_paid_call(monkeypatch):
    """Recording needs a backend call the bench cannot make offline. Until a
    recorder exists, --live says so instead of quietly running the offline path."""
    import pytest

    from opendaisugi.bench.registry import BenchRefused

    def _boom(*a, **k):
        raise AssertionError("the bench must not call a model")

    monkeypatch.setattr("opendaisugi.llm.get_instructor_client", _boom)
    with pytest.raises(BenchRefused, match="record"):
        run(BenchOpts(live=True))


def test_the_cli_prints_the_refusal_and_exits_one():
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    result = CliRunner().invoke(app, ["bench", "backend", "--live"])
    assert result.exit_code == 1
    assert "recorder is not built" in result.output


def test_a_malformed_recording_is_a_corpus_error_with_the_line(tmp_path):
    import pytest

    from opendaisugi.bench.corpus import CorpusMissing

    tasks = tmp_path / "tasks.jsonl"
    tasks.write_text('{"id":"t01","task":"one"}\n')
    (tmp_path / "envelopes").mkdir()
    rec = tmp_path / "envelopes" / "claude-code.jsonl"
    good = '{"task_id":"t01","backend":"claude-code","model":"m","recorded_at":"d","tokens_in":1,"tokens_out":1,"envelope":{"generated_by":"x","task":"one","permissions":{}}}\n'
    for bad in (
        '{"backend":"claude-code","tokens_in":1,"tokens_out":1,"envelope":{}}\n',
        '{"task_id":"t01","tokens_in":"many","tokens_out":1,"envelope":{"generated_by":"x","task":"one","permissions":{}}}\n',
        '{"task_id":"t01","tokens_in":1,"tokens_out":1,"envelope":{"task":"one"}}\n',
    ):
        rec.write_text(good + bad)
        with pytest.raises(CorpusMissing, match="line 2") as exc:
            run(BenchOpts(corpus=tasks))
        assert "claude-code.jsonl" in str(exc.value)
    rec.write_text(good)
    rows = {r.name: r for r in run(BenchOpts(corpus=tasks)).rows}
    assert rows["claude-code"].cells["envelopes"] == "1/1"


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "backend"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
