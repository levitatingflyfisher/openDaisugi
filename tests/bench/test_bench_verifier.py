"""The verifier bench must score the oracle against itself perfectly, must count
a client that accepts what the oracle rejects as fail-open, and must print an
absent row rather than skip a client that is not built."""

import json

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers.verifier import (
    COLUMNS,
    client_verdicts,
    fail_open_ids,
    run,
    score,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json


def test_columns_are_the_spec_columns():
    assert [c.key for c in COLUMNS] == [
        "option",
        "cases",
        "agree",
        "disagree",
        "fail_open",
        "ms",
        "failure",
    ]
    assert [c.key for c in COLUMNS if c.volatile] == ["ms"]


def test_python_oracle_agrees_with_itself_on_every_case():
    table = run(BenchOpts())
    python = next(r for r in table.rows if r.name == "python")
    cases = load_jsonl(resolve_corpus("verifier.jsonl", None))
    assert python.cells["cases"] == len(cases)
    assert python.cells["agree"] == len(cases)
    assert python.cells["disagree"] == 0
    assert python.cells["fail_open"] == 0


def test_every_client_row_is_present_or_absent_never_missing():
    from opendaisugi.bench.options import VERIFIER_CLIENTS

    table = run(BenchOpts())
    assert {r.name for r in table.rows} == set(VERIFIER_CLIENTS)
    for row in table.rows:
        assert row.absent is not None or "cases" in row.cells


def test_every_absent_row_carries_a_build_command():
    table = run(BenchOpts())
    for row in table.rows:
        if row.absent is not None:
            assert row.absent.startswith("cd clients/")


def test_no_full_profile_client_is_allowed_to_fail_open_on_the_committed_corpus():
    from opendaisugi.bench.options import VERIFIER_CLIENTS

    for row in run(BenchOpts()).rows:
        if row.absent is None and VERIFIER_CLIENTS[row.name].profile == "full":
            assert row.cells["fail_open"] == 0, f"{row.name} accepted what the oracle rejects"


def test_a_core_profile_client_fails_open_only_where_its_profile_ends():
    """The corpus carries one denial that only a Full-profile stage can see.
    A Core client accepts it, and the bench must show that as fail-open rather
    than read 0 and certify the hole. It must not fail open anywhere else."""
    from opendaisugi.bench.options import VERIFIER_CLIENTS, client_argv, client_is_built

    cases = load_jsonl(resolve_corpus("verifier.jsonl", None))
    full_stage_only = {
        c["id"]
        for c in cases
        if c["kind"] == "verify"
        and c["expect"]["ok"] is False
        and all(v["stage"] == "predicate" for v in c["expect"]["violations"])
    }
    assert full_stage_only, "the corpus lost its predicate-stage denial"
    for name, spec in VERIFIER_CLIENTS.items():
        if spec.profile != "core" or not client_is_built(spec):
            continue
        verdicts, _, _ = client_verdicts(client_argv(spec), cases)
        ids = fail_open_ids(cases, verdicts)
        assert ids, f"{name} read 0 fail-open on a corpus built to expose its profile gap"
        assert set(ids) <= full_stage_only, f"{name} fails open outside its profile: {ids}"
        assert score(cases, verdicts)["fail_open"] == len(ids)


def test_score_counts_a_permissive_disagreement_as_fail_open():
    cases = [
        {"id": "a", "kind": "decompose", "expect": {"ok": False}},
        {
            "id": "b",
            "kind": "decompose",
            "expect": {"ok": True, "heads": ["echo"], "reads": [], "writes": []},
        },
    ]
    verdicts = {
        "a": {"id": "a", "ok": True, "heads": ["rm"], "reads": [], "writes": []},
        "b": {"id": "b", "ok": False},
    }
    counts = score(cases, verdicts)
    assert counts == {"agree": 0, "disagree": 2, "fail_open": 1}


def test_a_missing_verdict_counts_as_a_disagreement_not_a_pass():
    cases = [
        {
            "id": "a",
            "kind": "decompose",
            "expect": {"ok": True, "heads": [], "reads": [], "writes": []},
        }
    ]
    assert score(cases, {}) == {"agree": 0, "disagree": 1, "fail_open": 0}


def test_json_shape_and_reproduce_line():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "verifier"
    assert body["reproduce"].startswith("uv run --no-sync daisugi bench verifier")
    assert body["corpus"]["path"].endswith("verifier.jsonl")


def test_the_reproduce_line_reruns_to_the_same_rows():
    a, b = run(BenchOpts()), run(BenchOpts())
    assert stable_rows(a) == stable_rows(b)


def test_the_printed_reproduce_line_actually_runs():
    """Two in-process calls never touch the printed string, so a typo in it is
    invisible. Shell the command the table prints and compare the rows."""
    import shlex
    import subprocess

    from opendaisugi.bench.options import repo_root

    root = repo_root()
    assert root is not None, "this test needs a checkout"
    table = run(BenchOpts())
    argv = shlex.split(table.reproduce) + ["--json"]
    proc = subprocess.run(argv, cwd=root, capture_output=True, text=True, timeout=180, check=False)
    assert proc.returncode == 0, proc.stderr[-500:]
    printed = json.loads(proc.stdout)
    volatile = {c["key"] for c in printed["columns"] if c["volatile"]}
    got = [{k: v for k, v in row.items() if k not in volatile} for row in printed["rows"]]
    assert got == stable_rows(table)


def test_the_bench_finishes_well_under_thirty_seconds():
    import time

    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0


# --- clients that hang, crash, emit garbage, or cannot run ---------------------

import os
import sys
import textwrap

from opendaisugi.bench.layers.verifier import CLIENT_TIMEOUT_S


def _fake_client(tmp_path, name: str, body: str) -> list[str]:
    script = tmp_path / f"{name}.py"
    script.write_text(textwrap.dedent(body), encoding="utf-8")
    return [sys.executable, str(script)]


def _cases():
    return load_jsonl(resolve_corpus("verifier.jsonl", None))


def test_the_default_client_timeout_fits_inside_the_bench_budget():
    assert CLIENT_TIMEOUT_S <= 15.0


def test_a_garbage_shape_client_is_named_and_scores_nothing(tmp_path):
    argv = _fake_client(
        tmp_path,
        "garbage",
        """
        import json, sys
        for line in sys.stdin:
            c = json.loads(line)
            print(json.dumps({"id": c["id"], "ok": True, "violations": [1, 2], "heads": 7}))
        """,
    )
    verdicts, _, failure = client_verdicts(argv, _cases(), timeout_s=CLIENT_TIMEOUT_S)
    assert verdicts == {}
    assert failure is not None and "malformed" in failure
    counts = score(_cases(), verdicts)
    assert counts["agree"] == 0
    assert counts["disagree"] == len(_cases())


def test_a_non_utf8_client_does_not_crash_the_bench(tmp_path):
    argv = _fake_client(
        tmp_path,
        "badutf8",
        """
        import sys
        sys.stdout.buffer.write(b'{"id": "\\xff", "ok": true}\\n')
        sys.stdout.buffer.write(b"\\xff\\xfe garbage\\n")
        """,
    )
    verdicts, _, failure = client_verdicts(argv, _cases(), timeout_s=CLIENT_TIMEOUT_S)
    assert failure is not None
    assert score(_cases(), verdicts)["agree"] == 0


def test_a_client_that_crashes_after_a_few_answers_is_named_and_the_rest_disagree(tmp_path):
    argv = _fake_client(
        tmp_path,
        "crash",
        """
        import json, sys
        n = 0
        for line in sys.stdin:
            c = json.loads(line)
            print(json.dumps({"id": c["id"], "ok": False}), flush=True)
            n += 1
            if n == 3:
                sys.exit(3)
        """,
    )
    verdicts, _, failure = client_verdicts(argv, _cases(), timeout_s=CLIENT_TIMEOUT_S)
    assert failure == "exit 3"
    assert len(verdicts) == 3
    counts = score(_cases(), verdicts)
    assert counts["agree"] + counts["disagree"] == len(_cases())
    assert counts["disagree"] >= len(_cases()) - 3


def test_a_hung_client_is_cut_off_at_the_timeout_and_named(tmp_path):
    import time

    argv = _fake_client(tmp_path, "hang", "import time\ntime.sleep(60)\n")
    t0 = time.monotonic()
    verdicts, _, failure = client_verdicts(argv, _cases(), timeout_s=1.0)
    assert time.monotonic() - t0 < 5.0
    assert verdicts == {}
    assert failure == "timed out after 1 s"


def test_a_missing_binary_is_named_not_scored(tmp_path):
    verdicts, _, failure = client_verdicts([str(tmp_path / "nope")], _cases(), timeout_s=1.0)
    assert verdicts == {}
    assert failure is not None and failure.startswith("cannot execute")


def test_score_survives_a_verdict_with_the_wrong_shape():
    cases = [
        {"id": "a", "kind": "verify", "expect": {"ok": False, "violations": []}},
        {
            "id": "b",
            "kind": "decompose",
            "expect": {"ok": True, "heads": [], "reads": [], "writes": []},
        },
    ]
    verdicts = {
        "a": {"id": "a", "ok": True, "violations": [1, 2]},
        "b": {"id": "b", "ok": True, "heads": 7},
    }
    assert score(cases, verdicts) == {"agree": 0, "disagree": 2, "fail_open": 1}


def test_a_failed_client_row_carries_the_failure_cell_and_the_other_rows_survive(
    tmp_path, monkeypatch
):
    from opendaisugi.bench.layers import verifier as mod
    from opendaisugi.bench.options import VERIFIER_CLIENTS

    hang = _fake_client(tmp_path, "hang", "import time\ntime.sleep(60)\n")
    real_argv = mod.client_argv

    def _argv(spec):
        return hang if spec.name == "go" else real_argv(spec)

    monkeypatch.setattr(mod, "client_argv", _argv)
    monkeypatch.setattr(mod, "CLIENT_TIMEOUT_S", 1.0)
    table = run(BenchOpts())
    rows = {r.name: r for r in table.rows}
    assert set(rows) == set(VERIFIER_CLIENTS)
    go = rows["go"]
    assert go.absent is None
    assert go.cells["failure"] == "timed out after 1 s"
    # A client that did not answer measured nothing. Its agreement and its
    # fail-open count are unknown, not zero, so the cells say so.
    assert go.cells["agree"] == "n/a"
    assert go.cells["disagree"] == "n/a"
    assert go.cells["fail_open"] == "n/a"
    assert go.cells["cases"] == len(_cases())
    assert rows["python"].cells["failure"] == ""
    assert rows["python"].cells["agree"] == len(_cases())
    assert "failure" in [c.key for c in COLUMNS]


def test_a_non_executable_probe_is_absent_with_its_build_hint(tmp_path, monkeypatch):
    from opendaisugi.bench import options

    spec = options.VERIFIER_CLIENTS["go"]
    fake_root = tmp_path / "root"
    (fake_root / "clients" / "go").mkdir(parents=True)
    (fake_root / spec.probe).write_text("not a binary")
    os.chmod(fake_root / spec.probe, 0o644)
    monkeypatch.setattr(options, "repo_root", lambda: fake_root)
    assert options.client_is_built(spec) is False
    assert options.client_argv(spec) == []


def test_an_unbuilt_client_prints_absent_with_its_build_hint(monkeypatch):
    """Every client is built on some boxes, so the absent branch needs a test
    that does not depend on which binaries this box has."""
    from opendaisugi.bench.layers import verifier as mod

    real = mod.client_is_built
    monkeypatch.setattr(mod, "client_is_built", lambda spec: spec.name != "lean" and real(spec))
    lean = next(r for r in run(BenchOpts()).rows if r.name == "lean")
    assert lean.absent == "cd clients/lean && lake build"


def test_a_core_profile_fake_client_reads_fail_open_one_on_exactly_the_predicate_case(
    tmp_path,
):
    """On a box with no compiled clients the profile tests above have nothing
    to loop over. This fake answers every case with the oracle's expectation
    except an envelope that carries an invariant, which it accepts."""
    argv = _fake_client(
        tmp_path,
        "core",
        """
        import json, sys
        for line in sys.stdin:
            c = json.loads(line)
            v = dict(c["expect"])
            if c["kind"] == "verify" and c["envelope"].get("invariants"):
                v = {"ok": True, "violations": []}
            v["id"] = c["id"]
            print(json.dumps(v))
        """,
    )
    cases = _cases()
    verdicts, _, failure = client_verdicts(argv, cases, timeout_s=CLIENT_TIMEOUT_S)
    assert failure is None
    ids = fail_open_ids(cases, verdicts)
    assert ids == ["351e3f0b43d62cd7"]
    assert score(cases, verdicts) == {"agree": len(cases) - 1, "disagree": 1, "fail_open": 1}
