"""Every gate path's contract cases run through the real gate in enforce mode.
A deny case that comes back allow is a fail-open, and the test says so by name."""

import json
import subprocess
import time

import pytest

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers import gate_path
from opendaisugi.bench.layers.gate_path import (
    COLUMNS,
    battery_envelope,
    measure_roundtrip_ms,
    run,
    run_contract,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json
from opendaisugi.gate import evaluate_call


@pytest.fixture()
def no_subprocess(monkeypatch):
    """Skip the cold-start measurement so a test that reads rows stays fast."""
    monkeypatch.setattr(gate_path, "measure_roundtrip_ms", lambda: 0.0)


def test_columns():
    assert [c.key for c in COLUMNS] == [
        "option",
        "contract",
        "fail_open",
        "evidence",
        "roundtrip_ms",
        "source",
    ]
    assert {c.key for c in COLUMNS if c.volatile} == {"roundtrip_ms"}


def test_every_recorded_gate_path_gets_a_row(no_subprocess):
    from opendaisugi.bench.options import GATE_PATHS

    assert {r.name for r in run(BenchOpts()).rows} == set(GATE_PATHS)


def test_the_fail_open_class_column_is_the_pinned_fact(no_subprocess):
    from opendaisugi.bench.options import GATE_PATHS

    for row in run(BenchOpts()).rows:
        assert row.cells["fail_open"] == GATE_PATHS[row.name].fail_open_class
        assert row.cells["evidence"] == GATE_PATHS[row.name].evidence


def test_deny_and_malformed_cases_never_resolve_to_allow_in_enforce_mode():
    envelope = battery_envelope()
    for case in load_jsonl(resolve_corpus("gate-paths.jsonl", None)):
        if case["expect"] != "deny":
            continue
        decision = evaluate_call(case["payload"], envelope, mode="enforce")
        assert decision.allow is False, f"{case['id']} ({case['case']}) resolved to allow"


def test_a_malformed_payload_denies_fail_closed():
    envelope = battery_envelope()
    for payload in (None, "not-an-object", {}, {"tool_name": ""}, {"tool_name": "Nope"}):
        assert evaluate_call(payload, envelope, mode="enforce").allow is False


def test_contract_counts_are_out_of_four_per_path():
    cases = load_jsonl(resolve_corpus("gate-paths.jsonl", None))
    envelope = battery_envelope()
    passed, total = run_contract("claude-hook", cases, envelope)
    assert total == 4
    assert 0 <= passed <= 4


def test_every_path_passes_its_whole_contract():
    cases = load_jsonl(resolve_corpus("gate-paths.jsonl", None))
    envelope = battery_envelope()
    for path in {c["path"] for c in cases}:
        assert run_contract(path, cases, envelope) == (4, 4), path


def test_pi_cases_are_in_pi_s_own_vocabulary_and_judged_under_its_format():
    """The extension sends lowercase names with --format pi. Judged as Claude
    payloads the allow cases deny, so a 4/4 under the wrong format would be
    the Claude path scored a second time."""
    from opendaisugi.bench.options import GATE_PATHS
    from opendaisugi.hook import _PI_TOOL_TYPE_MAP

    assert GATE_PATHS["pi-ext"].fmt == "pi"
    assert all(spec.fmt == "claude" for name, spec in GATE_PATHS.items() if name != "pi-ext")
    cases = [
        c for c in load_jsonl(resolve_corpus("gate-paths.jsonl", None)) if c["path"] == "pi-ext"
    ]
    envelope = battery_envelope()
    assert len(cases) == 4
    for case in cases:
        name = case["payload"].get("tool_name", "")
        assert name == name.lower()
        if case["case"] != "malformed":
            assert name in _PI_TOOL_TYPE_MAP, case["id"]
        allow_as_claude = evaluate_call(case["payload"], envelope, mode="enforce").allow
        assert allow_as_claude is False, f"{case['id']} allows under the Claude format"
    assert run_contract("pi-ext", cases, envelope) == (4, 4)


def test_a_path_with_no_cases_scores_zero_of_zero_not_a_pass():
    assert run_contract("no-such-path", [], battery_envelope()) == (0, 0)


def test_a_case_the_gate_gets_wrong_is_counted_against_the_path():
    """A corpus that expects allow on an out-of-envelope write must fail the
    contract, not pass it. The gate is the judge, the corpus is the claim."""
    wrong = [
        {
            "id": "w1",
            "path": "p",
            "case": "deny-mislabelled",
            "payload": {
                "session_id": "s",
                "tool_name": "Write",
                "tool_input": {"file_path": "/etc/passwd", "content": "x"},
            },
            "expect": "allow",
        }
    ]
    assert run_contract("p", wrong, battery_envelope()) == (0, 1)


def test_the_cold_start_is_a_real_number_here():
    got = measure_roundtrip_ms()
    assert isinstance(got, float) and got > 0.0


def test_a_broken_round_trip_is_named_not_a_number(monkeypatch):
    def _hang(*a, **k):
        raise subprocess.TimeoutExpired(cmd="gate", timeout=k.get("timeout", 0))

    monkeypatch.setattr(subprocess, "run", _hang)
    got = measure_roundtrip_ms()
    assert isinstance(got, str) and "timed out" in got

    def _missing(*a, **k):
        raise OSError(2, "No such file or directory")

    monkeypatch.setattr(subprocess, "run", _missing)
    got = measure_roundtrip_ms()
    assert isinstance(got, str) and "cannot execute" in got


def test_a_gate_that_cannot_import_is_named_not_timed(monkeypatch):
    """A cold start that exits with a traceback ran no gate. Its exit code
    and first error line go in the cell, never a fast round trip."""

    def _broken(*a, **k):
        return subprocess.CompletedProcess(
            a[0],
            1,
            stdout=b"",
            stderr=b"Traceback (most recent call last):\n  x\nImportError: no z3\n",
        )

    monkeypatch.setattr(subprocess, "run", _broken)
    got = measure_roundtrip_ms()
    assert got == "exit 1: ImportError: no z3"


def test_a_gate_that_allows_an_empty_payload_is_named(monkeypatch):
    """The empty payload must deny with exit 2. Any other exit is not the gate."""

    def _permissive(*a, **k):
        return subprocess.CompletedProcess(a[0], 0, stdout=b"{}", stderr=b"")

    monkeypatch.setattr(subprocess, "run", _permissive)
    got = measure_roundtrip_ms()
    assert isinstance(got, str) and got.startswith("exit 0")


def test_the_round_trip_is_one_shared_measurement(monkeypatch):
    calls: list[int] = []

    def _once() -> float:
        calls.append(1)
        return 12.5

    monkeypatch.setattr(gate_path, "measure_roundtrip_ms", _once)
    rows = run(BenchOpts()).rows
    assert len(calls) == 1
    assert {r.cells["roundtrip_ms"] for r in rows} == {12.5}


def test_the_table_pins_the_envelope_it_checked_against(no_subprocess):
    table = run(BenchOpts())
    assert [c.rel.rsplit("/", 1)[-1] for c in table.companions] == ["battery-envelope.json"]


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "gate-path"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
