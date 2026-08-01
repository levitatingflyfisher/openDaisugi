"""The conformance suite: language-neutral corpus + differential harness.

The multi-client campaign (Rust/Go/Lean4/TS verifiers) needs three things from
the Python oracle: a **corpus** of self-contained cases (plan + envelope +
expected verdict; command + expected decomposition) harvested from real
verify() calls, a **wire protocol** any client binary can speak (case JSON in
on stdin, verdict JSON out on stdout, one per line), and a **differential
runner** that feeds the corpus to a client and reports disagreements. Verdict
comparison is structural only — ``ok`` plus (stage, step) pairs — because
message strings are informative, never normative: two correct clients may word
a rejection differently, but they may not disagree about *what* was rejected.

Recording is a product feature, not test scaffolding: any process that sets
``OPENDAISUGI_CONFORMANCE_RECORD=<dir>`` harvests its live verifications as
corpus cases, so the corpus grows from real usage as well as from the suite.
"""

from __future__ import annotations

import json
import stat
import sys

import pytest
from typer.testing import CliRunner

from opendaisugi.conformance import (
    RECORD_ENV,
    bench_corpus,
    canonical_json,
    case_id,
    compare_verdict,
    export_corpus,
    make_decompose_case,
    make_verify_case,
    run_corpus,
    serve_lines,
)
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.verify import verify

runner = CliRunner()


def _plan(command: str = "ls -la") -> ActionPlan:
    return ActionPlan(source="test", task="t", steps=[ShellStep(id="s1", command=command)])


def _env(allowlist: list[str]) -> Envelope:
    return Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=allowlist),
    )


def _case(command: str, allowlist: list[str]) -> dict:
    plan, env = _plan(command), _env(allowlist)
    return make_verify_case(plan, env, {"strict": None, "z3_timeout_ms": 500}, verify(plan, env))


# --- canonical form --------------------------------------------------------------


def test_canonical_json_is_key_ordered_and_compact():
    s = canonical_json({"b": 1, "a": [{"z": 2, "y": 3}]})
    assert s == '{"a":[{"y":3,"z":2}],"b":1}'


def test_case_id_deterministic_and_key_order_independent():
    a = case_id({"x": 1, "y": 2})
    b = case_id({"y": 2, "x": 1})
    assert a == b
    assert len(a) == 16 and a != case_id({"x": 1, "y": 3})


# --- case construction -----------------------------------------------------------


def test_make_verify_case_normative_expectation_on_rejection():
    case = _case("rm -rf /", ["ls"])
    assert case["kind"] == "verify"
    assert case["expect"]["ok"] is False
    assert {"stage": "permissions", "step": "s1"} in case["expect"]["violations"]
    # Messages are informative, never normative — they must not be in expect.
    assert "message" not in json.dumps(case["expect"])


def test_make_verify_case_pass():
    case = _case("ls -la", ["ls"])
    assert case["expect"] == {"ok": True, "violations": []}
    assert case["id"] == case_id({k: v for k, v in case.items() if k != "id"})


def test_identical_logical_cases_share_one_id():
    # Envelope.id / ActionPlan.id are random uuids — bookkeeping, not
    # semantics. Two fresh constructions of the same logical case must
    # content-address identically or dedupe is theatre.
    assert _case("ls", ["ls"])["id"] == _case("ls", ["ls"])["id"]


def test_make_decompose_case_shape():
    pytest.importorskip("tree_sitter_bash")
    from opendaisugi.shell_decompose import decompose_command

    case = make_decompose_case("ls && pwd", decompose_command("ls && pwd"))
    assert case["kind"] == "decompose"
    assert case["command"] == "ls && pwd"
    assert case["expect"]["ok"] is True
    assert case["expect"]["heads"] == ["ls", "pwd"]


# --- live recording --------------------------------------------------------------


def test_recording_env_captures_real_verify(tmp_path, monkeypatch):
    rec = tmp_path / "rec"
    monkeypatch.setenv(RECORD_ENV, str(rec))
    verify(_plan(), _env(["ls"]))
    files = list(rec.glob("*.jsonl"))
    assert files, "recording dir has no case files"
    lines = [json.loads(x) for f in files for x in f.read_text().splitlines()]
    assert any(c["kind"] == "verify" and c["expect"]["ok"] for c in lines)


def test_no_recording_without_env(tmp_path, monkeypatch):
    monkeypatch.delenv(RECORD_ENV, raising=False)
    verify(_plan(), _env(["ls"]))
    assert not list(tmp_path.glob("**/*.jsonl"))


def test_recording_skips_alias_calls(tmp_path, monkeypatch):
    from opendaisugi.aliases import AliasRegistry

    rec = tmp_path / "rec"
    monkeypatch.setenv(RECORD_ENV, str(rec))
    verify(_plan(), _env(["ls"]), aliases=AliasRegistry())
    assert not rec.exists() or not list(rec.glob("*.jsonl"))


def test_decompose_recording(tmp_path, monkeypatch):
    pytest.importorskip("tree_sitter_bash")
    from opendaisugi.shell_decompose import decompose_command

    rec = tmp_path / "rec"
    monkeypatch.setenv(RECORD_ENV, str(rec))
    decompose_command("ls && pwd")
    lines = [json.loads(x) for f in rec.glob("*.jsonl") for x in f.read_text().splitlines()]
    assert any(c["kind"] == "decompose" and c["expect"]["heads"] == ["ls", "pwd"] for c in lines)


def test_recording_skips_custom_step_plans(tmp_path, monkeypatch):
    # A process-locally registered step type makes the case non-portable: a
    # fresh oracle process cannot validate the plan. Same category as aliases —
    # process state — so recording must skip it.
    from typing import Literal

    from opendaisugi.models import STEP_TYPE_REGISTRY, StepBase, step_type

    @step_type
    class _ConfTestStep(StepBase):
        type: Literal["_conf_test_step"] = "_conf_test_step"

    try:
        rec = tmp_path / "rec"
        monkeypatch.setenv(RECORD_ENV, str(rec))
        plan = ActionPlan(source="test", task="t", steps=[_ConfTestStep(id="s1")])
        verify(plan, _env(["ls"]))
        assert not rec.exists() or not list(rec.glob("*.jsonl"))
    finally:
        STEP_TYPE_REGISTRY.pop("_conf_test_step", None)


def test_serve_contains_bad_case_and_runner_flags_it():
    # One malformed case must produce an error verdict (a mismatch), never
    # abort the stream — the differential runner needs the other 13k verdicts.
    bad = {
        "kind": "verify",
        "v": 1,
        "id": "deadbeef00000000",
        "plan": {
            "source": "x",
            "task": "t",
            "steps": [{"id": "s1", "type": "_not_registered_anywhere"}],
        },
        "envelope": _env(["ls"]).model_dump(mode="json"),
        "options": {},
        "expect": {"ok": True, "violations": []},
    }
    good = _case("ls", ["ls"])
    outs = [json.loads(v) for v in serve_lines([canonical_json(bad), canonical_json(good)])]
    assert outs[0]["id"] == "deadbeef00000000" and "error" in outs[0]
    assert compare_verdict(bad, outs[0]) is not None
    assert compare_verdict(good, outs[1]) is None


# --- export ----------------------------------------------------------------------


def test_export_dedupes_and_writes_manifest(tmp_path):
    raw = tmp_path / "raw"
    raw.mkdir()
    a, b = _case("ls", ["ls"]), _case("pwd", ["pwd"])
    (raw / "cases-1.jsonl").write_text(
        canonical_json(a) + "\n" + canonical_json(a) + "\n" + canonical_json(b) + "\n"
    )
    out = tmp_path / "corpus.jsonl"
    manifest = export_corpus(raw, out)
    assert manifest["count"] == 2
    assert len(out.read_text().splitlines()) == 2
    saved = json.loads(out.with_suffix(".manifest.json").read_text())
    assert saved["sha256"] == manifest["sha256"] and len(manifest["sha256"]) == 64


# --- verdict comparison ----------------------------------------------------------


def test_compare_verdict_match_and_mismatch():
    case = _case("rm x", ["ls"])
    good = {"id": case["id"], "ok": False, "violations": [{"stage": "permissions", "step": "s1"}]}
    assert compare_verdict(case, good) is None
    assert compare_verdict(case, {"id": case["id"], "ok": True, "violations": []}) is not None
    extra = {
        "id": case["id"],
        "ok": False,
        "violations": [{"stage": "permissions", "step": "s1"}, {"stage": "dag", "step": None}],
    }
    assert compare_verdict(case, extra) is not None


# --- the oracle speaks its own protocol ------------------------------------------


def test_serve_lines_oracle_round_trip():
    cases = [_case("ls", ["ls"]), _case("rm x", ["ls"])]
    verdicts = [json.loads(v) for v in serve_lines(canonical_json(c) for c in cases)]
    assert [v["ok"] for v in verdicts] == [True, False]
    for c, v in zip(cases, verdicts, strict=True):
        assert compare_verdict(c, v) is None


def test_run_corpus_against_oracle_subprocess(tmp_path):
    corpus = tmp_path / "corpus.jsonl"
    corpus.write_text(
        "".join(canonical_json(c) + "\n" for c in [_case("ls", ["ls"]), _case("rm x", ["ls"])])
    )
    report = run_corpus(corpus, [sys.executable, "-m", "opendaisugi.conformance"])
    assert report.total == 2 and report.matched == 2 and report.mismatches == []


def test_run_corpus_flags_lying_client(tmp_path):
    corpus = tmp_path / "corpus.jsonl"
    corpus.write_text(
        "".join(canonical_json(c) + "\n" for c in [_case("ls", ["ls"]), _case("rm x", ["ls"])])
    )
    liar = tmp_path / "liar.py"
    liar.write_text(
        "import json,sys\n"
        "for line in sys.stdin:\n"
        "    c=json.loads(line)\n"
        "    print(json.dumps({'id':c['id'],'ok':True,'violations':[]}),flush=True)\n"
    )
    liar.chmod(liar.stat().st_mode | stat.S_IEXEC)
    report = run_corpus(corpus, [sys.executable, str(liar)])
    assert report.matched == 1 and len(report.mismatches) == 1
    assert (
        report.mismatches[0].case_id
        == [c for c in map(json.loads, corpus.read_text().splitlines()) if not c["expect"]["ok"]][
            0
        ]["id"]
    )


# --- benchmarks ------------------------------------------------------------------


def test_bench_corpus_stats(tmp_path):
    corpus = tmp_path / "corpus.jsonl"
    corpus.write_text(
        "".join(canonical_json(c) + "\n" for c in [_case("ls", ["ls"]), _case("rm x", ["ls"])])
    )
    stats = bench_corpus(corpus, repeat=3)
    assert stats.n_cases == 2 and stats.repeat == 3
    assert 0 < stats.p50_ms <= stats.p95_ms <= stats.p99_ms
    assert stats.cases_per_s > 0


# --- CLI -------------------------------------------------------------------------


def test_cli_conformance_export_run_bench(tmp_path):
    from opendaisugi.cli import app

    raw = tmp_path / "raw"
    raw.mkdir()
    (raw / "cases-1.jsonl").write_text(
        canonical_json(_case("ls", ["ls"])) + "\n" + canonical_json(_case("rm x", ["ls"])) + "\n"
    )
    corpus = tmp_path / "corpus.jsonl"
    res = runner.invoke(app, ["conformance", "export", str(raw), "--out", str(corpus)])
    assert res.exit_code == 0, res.output
    assert "2" in res.output

    res = runner.invoke(
        app,
        [
            "conformance",
            "run",
            str(corpus),
            "--client",
            f"{sys.executable} -m opendaisugi.conformance",
        ],
    )
    assert res.exit_code == 0, res.output
    assert "2/2" in res.output

    res = runner.invoke(app, ["conformance", "bench", str(corpus), "--repeat", "2"])
    assert res.exit_code == 0, res.output
    assert "p50" in res.output


def test_cli_conformance_run_nonzero_exit_on_mismatch(tmp_path):
    from opendaisugi.cli import app

    corpus = tmp_path / "corpus.jsonl"
    corpus.write_text(canonical_json(_case("rm x", ["ls"])) + "\n")
    liar = tmp_path / "liar.py"
    liar.write_text(
        "import json,sys\n"
        "for line in sys.stdin:\n"
        "    c=json.loads(line)\n"
        "    print(json.dumps({'id':c['id'],'ok':True,'violations':[]}),flush=True)\n"
    )
    res = runner.invoke(
        app, ["conformance", "run", str(corpus), "--client", f"{sys.executable} {liar}"]
    )
    assert res.exit_code == 1
    assert "mismatch" in res.output.lower()
