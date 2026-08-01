"""Stage 9 — within-instance batch compilation.

An agent *declares* a batch — program P (a step template), item set I (bindings),
effect footprint F (declared write globs), acceptance postcondition Q — and the
library proves ``F ⊆ envelope`` from the concrete resolved write-set *before any
iteration* (reject on any unprovable write), marks irreversible programs
non-batchable, sample-validates Q on k items in a deed-ledger fork, then executes
all N under the supervisor's per-step monitor with per-element rollback — halting
the instant a deed comes back irreversible. Every number the meter emits is
labelled evidence, not proof, and the two ledgers (within-instance vs
cross-instance) are reported separately and never merged.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi import batch
from opendaisugi.journal import Journal
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileReadStep,
    FileWriteStep,
    Permission,
    Postcondition,
    ShellStep,
)
from opendaisugi.pathway import PathwayParameter

pytestmark = pytest.mark.asyncio


# --------------------------------------------------------------------------- #
# helpers
# --------------------------------------------------------------------------- #
def _env(tmp_path: Path, *, writable: str | None = None) -> Envelope:
    return Envelope(
        generated_by="test",
        task="batch",
        permissions=Permission(
            file_read=[f"{tmp_path}/**"],
            file_write=[writable or f"{tmp_path}/**"],
        ),
    )


def _write_program(out: Path) -> ActionPlan:
    """A one-write program whose path is a hole; content is constant."""
    step = FileWriteStep(id="w", path=f"{out}/base.txt", content="STAMP")
    return ActionPlan(source="batch", task="stamp", steps=[step])


def _path_param(out: Path) -> PathwayParameter:
    # head = the fixed directory; apply_bindings refuses any binding that would
    # move the write out of it — the batch cannot redirect to another directory.
    return PathwayParameter(
        name="w.path", step_index=0, step_id="w", field="path", head=str(out), observed=[]
    )


def _decl(
    out: Path,
    names: list[str],
    *,
    footprint: list[str] | None = None,
    acceptance: Postcondition | None = None,
    sample_k: int = 2,
) -> "batch.BatchDeclaration":
    return batch.BatchDeclaration(
        program=_write_program(out),
        parameters=[_path_param(out)],
        items=[{"w.path": f"{out}/{n}"} for n in names],
        footprint=footprint if footprint is not None else [f"{out}/**"],
        acceptance=acceptance,
        sample_k=sample_k,
    )


# --------------------------------------------------------------------------- #
# 1. batchability classifier
# --------------------------------------------------------------------------- #
async def test_batchable_types_are_effect_reversible_or_read_only():
    assert batch.is_batchable_type("file_write")
    assert batch.is_batchable_type("file_read")
    assert batch.is_batchable_type("network")  # GET-only ⇒ no local effect
    for t in ("shell", "mcp", "task", "skill", "agentic", "sim_reset"):
        assert not batch.is_batchable_type(t), t


async def test_classify_rejects_a_program_with_a_shell_step(tmp_path):
    out = tmp_path / "out"
    prog = ActionPlan(
        source="batch",
        task="x",
        steps=[
            FileWriteStep(id="w", path=f"{out}/a.txt", content="c"),
            ShellStep(id="s", command="echo hi"),
        ],
    )
    decl = batch.BatchDeclaration(program=prog, parameters=[], items=[{}], footprint=[f"{out}/**"])
    cls = batch.classify_declaration(decl)
    assert cls.batchable is False
    assert any(nb["type"] == "shell" for nb in cls.non_batchable)


async def test_classify_accepts_a_pure_file_write_program(tmp_path):
    out = tmp_path / "out"
    cls = batch.classify_declaration(_decl(out, ["a.txt", "b.txt"]))
    assert cls.batchable is True
    assert cls.non_batchable == []


# --------------------------------------------------------------------------- #
# 2. static footprint proof — the concrete matcher, before any iteration
# --------------------------------------------------------------------------- #
async def test_prove_footprint_accepts_when_all_writes_in_envelope_and_F(tmp_path):
    out = tmp_path / "out"
    decl = _decl(out, ["a.txt", "b.txt", "c.txt"])
    proof = batch.prove_footprint(decl, _env(tmp_path))
    assert proof.ok is True
    assert sorted(proof.resolved_writes) == sorted([f"{out}/a.txt", f"{out}/b.txt", f"{out}/c.txt"])
    assert proof.out_of_envelope == []
    assert proof.under_declared == []


async def test_prove_footprint_rejects_a_write_outside_the_envelope(tmp_path):
    out = tmp_path / "out"
    # Envelope only grants *.txt directly under out; the batch writes a .log.
    decl = batch.BatchDeclaration(
        program=_write_program(out),
        parameters=[_path_param(out)],
        items=[{"w.path": f"{out}/a.txt"}, {"w.path": f"{out}/evil.log"}],
        footprint=[f"{out}/**"],
    )
    proof = batch.prove_footprint(decl, _env(tmp_path, writable=f"{out}/*.txt"))
    assert proof.ok is False
    assert f"{out}/evil.log" in proof.out_of_envelope


async def test_prove_footprint_rejects_under_declared_write(tmp_path):
    """An honest declaration's F must cover every write. A write inside the
    envelope but outside the declared footprint is a dishonest (too-small)
    declaration — hard reject."""
    out = tmp_path / "out"
    decl = batch.BatchDeclaration(
        program=_write_program(out),
        parameters=[_path_param(out)],
        items=[{"w.path": f"{out}/a.txt"}, {"w.path": f"{out}/b.txt"}],
        footprint=[f"{out}/a.txt"],  # declares only a.txt, but writes b.txt too
    )
    proof = batch.prove_footprint(decl, _env(tmp_path))
    assert proof.ok is False
    assert f"{out}/b.txt" in proof.under_declared


async def test_prove_footprint_rejects_binding_that_changes_capability_head(tmp_path):
    out = tmp_path / "out"
    # Binding tries to redirect into a sub-directory — a different capability
    # head; apply_bindings returns None and the batch is rejected fail-closed.
    decl = batch.BatchDeclaration(
        program=_write_program(out),
        parameters=[_path_param(out)],
        items=[{"w.path": f"{out}/sub/escape.txt"}],
        footprint=[f"{out}/**"],
    )
    proof = batch.prove_footprint(decl, _env(tmp_path))
    assert proof.ok is False
    assert proof.bad_bindings == [0]


async def test_prove_footprint_uses_concrete_matcher_not_z3_glob(tmp_path):
    """Soundness crux: the proof must match the runtime gate exactly. A write in
    a sub-directory is admitted by subsumption's Z3 glob encoding (single ``*``
    crosses ``/``, no normalization) but *rejected* by the concrete
    ``_path_matches_any`` the executor gate uses. The batch proof must reject it —
    else it would admit-then-reject, a bypass. F is broadened to isolate the
    envelope check from the under-declaration check."""
    out = tmp_path / "out"
    # A program that writes directly to a sub-path (no parameter → no head guard).
    prog = ActionPlan(
        source="batch",
        task="x",
        steps=[FileWriteStep(id="w", path=f"{out}/sub/c.txt", content="c")],
    )
    decl = batch.BatchDeclaration(program=prog, parameters=[], items=[{}], footprint=[f"{out}/**"])
    # Envelope grants only *.txt DIRECTLY under out (single star, no /).
    proof = batch.prove_footprint(decl, _env(tmp_path, writable=f"{out}/*.txt"))
    assert proof.ok is False
    assert f"{out}/sub/c.txt" in proof.out_of_envelope
    assert proof.under_declared == []  # F=**  covers it; only the envelope rejects


# --------------------------------------------------------------------------- #
# 3. reversibility pre-probe (static, before any write)
# --------------------------------------------------------------------------- #
async def test_would_be_reversible_classifies_targets(tmp_path, monkeypatch):
    absent = tmp_path / "absent.txt"
    small = tmp_path / "small.txt"
    small.write_text("tiny")
    assert batch.would_be_reversible(str(absent)) is True
    assert batch.would_be_reversible(str(small)) is True

    monkeypatch.setattr("opendaisugi.executor.MAX_REVERSAL_BYTES", 4)
    big = tmp_path / "big.txt"
    big.write_text("way more than four bytes")
    assert batch.would_be_reversible(str(big)) is False  # prior image too large

    binary = tmp_path / "bin.dat"
    binary.write_bytes(b"\xff\xfe\x00not utf8")
    assert batch.would_be_reversible(str(binary)) is False  # non-UTF-8 prior


# --------------------------------------------------------------------------- #
# 4. the net-token meter + two-ledger discipline
# --------------------------------------------------------------------------- #
async def test_within_instance_ledger_reports_no_token_win_honestly(tmp_path):
    """Against the *honest* baseline (a competent agent already scripts the bulk
    job), the within-instance win is the proven blast radius, NOT tokens: the meter
    must show net ≤ 0, not manufacture a saving."""
    out = tmp_path / "out"
    led = batch.NetTokenLedger.within_instance(_decl(out, ["a.txt", "b.txt"]))
    assert led.label == "within-instance"
    assert "script" in led.baseline.lower()  # not the manual-turn baseline
    assert led.evidence_not_proof is True
    assert led.net <= 0
    assert led.net_positive is False


async def test_net_token_meter_surfaces_the_skill_disco_trap():
    """The meter exists to CATCH the trap where injecting the spec costs more than
    it saves (SKILL-DISCO's measured +net-cost). Net must go negative, not hide it."""
    trap = batch.NetTokenLedger(
        label="within-instance",
        baseline="honest-script",
        output_tokens_saved=100,
        calls_saved=1,
        tokens_per_call=2000,
        spec_input_injected=3000,  # exceeds the 2100 saved
    )
    assert trap.net == 100 + 2000 - 3000  # == -900
    assert trap.net_positive is False
    win = trap.model_copy(update={"spec_input_injected": 1000})
    assert win.net == 1100
    assert win.net_positive is True


async def test_two_ledgers_are_separate_and_cross_instance_is_deferred(tmp_path):
    out = tmp_path / "out"
    report = batch.two_ledger_report(_decl(out, ["a.txt"]))
    assert report.within_instance.label == "within-instance"
    assert report.cross_instance is None  # Stage-4's question; not merged in
    assert "separ" in report.note.lower() and "merg" in report.note.lower()


# --------------------------------------------------------------------------- #
# 5. execution — sample-validate, monitor, per-element rollback, halt
# --------------------------------------------------------------------------- #
async def test_run_batch_executes_all_and_is_reversible_from_the_ledger(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    out = tmp_path / "out"
    journal = Journal(data_dir=tmp_path / "journal")
    decl = _decl(
        out,
        ["a.txt", "b.txt", "c.txt"],
        footprint=[f"{out}/**"],
        acceptance=Postcondition(type="file_exists"),
    )
    result = await batch.run_batch(decl, _env(tmp_path), journal=journal)
    assert result.status == "succeeded", result
    assert result.executed == 3
    assert (out / "a.txt").read_text() == "STAMP"
    assert (out / "c.txt").exists()
    # Per-element rollback from the collected ledger alone — no model, no re-run.
    report = batch.rollback_result(result)
    assert not (out / "a.txt").exists()
    assert not (out / "c.txt").exists()
    assert sorted(report.undone) == sorted([f"{out}/a.txt", f"{out}/b.txt", f"{out}/c.txt"])


async def test_run_batch_rejects_a_non_batchable_program(tmp_path, monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    out = tmp_path / "out"
    prog = ActionPlan(source="b", task="x", steps=[ShellStep(id="s", command="echo hi")])
    decl = batch.BatchDeclaration(program=prog, parameters=[], items=[{}], footprint=[f"{out}/**"])
    result = await batch.run_batch(decl, _env(tmp_path), journal=Journal(data_dir=tmp_path / "j"))
    assert result.status == "rejected"
    assert result.executed == 0
    assert "batchable" in result.reason.lower()


async def test_run_batch_rejects_when_footprint_is_unprovable_and_writes_nothing(
    tmp_path, monkeypatch
):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    out = tmp_path / "out"
    decl = batch.BatchDeclaration(
        program=_write_program(out),
        parameters=[_path_param(out)],
        items=[{"w.path": f"{out}/a.txt"}, {"w.path": f"{out}/evil.log"}],
        footprint=[f"{out}/**"],
    )
    result = await batch.run_batch(
        decl, _env(tmp_path, writable=f"{out}/*.txt"), journal=Journal(data_dir=tmp_path / "j")
    )
    assert result.status == "rejected"
    assert result.executed == 0
    assert not (out / "a.txt").exists()  # proof fails BEFORE any iteration


async def test_run_batch_rejects_a_known_irreversible_target_before_writing(tmp_path, monkeypatch):
    """The pre-flight probe is the primary "never enter a batch" guarantee: a target
    whose write is already irreversible (a large existing file) is rejected before
    any iteration — nothing is written."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    monkeypatch.setattr("opendaisugi.executor.MAX_REVERSAL_BYTES", 8)
    out = tmp_path / "out"
    out.mkdir()
    (out / "a.txt").write_text("A0")  # small
    (out / "b.txt").write_text("a very large prior image that exceeds the cap")  # irreversible
    decl = _decl(out, ["a.txt", "b.txt", "c.txt"], footprint=[f"{out}/**"])
    result = await batch.run_batch(decl, _env(tmp_path), journal=Journal(data_dir=tmp_path / "j"))
    assert result.status == "rejected"
    assert result.executed == 0
    assert str(out / "b.txt") in result.reason  # names the offending target, not just a word
    assert (out / "a.txt").read_text() == "A0"  # untouched — proof failed before iteration


async def test_run_batch_halts_on_the_first_irreversible_element(tmp_path, monkeypatch):
    """The blocking guarantee, defense in depth: reversibility is not a property of
    the type, so even past the pre-probe (here bypassed to simulate a TOCTOU growth)
    a deed can come back irreversible at runtime. The batch must halt THERE, not
    write the rest, roll back the reversible elements it already did, and surface the
    irreversible one in ``skipped``. This is the fail-open Stage 8 prevents, one
    layer up."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    # Simulate a probe that passed, then the target grew past the cap by execution.
    monkeypatch.setattr("opendaisugi.batch.would_be_reversible", lambda _p: True)
    monkeypatch.setattr("opendaisugi.executor.MAX_REVERSAL_BYTES", 8)
    out = tmp_path / "out"
    out.mkdir()
    (out / "a.txt").write_text("A0")  # small, reversible overwrite
    (out / "b.txt").write_text("a very large prior image that exceeds the cap")  # irreversible
    decl = _decl(
        out,
        ["a.txt", "b.txt", "c.txt"],
        footprint=[f"{out}/**"],
        acceptance=Postcondition(type="file_exists"),
        sample_k=1,  # sample only a.txt (reversible), so the halt lands in the full run
    )
    result = await batch.run_batch(decl, _env(tmp_path), journal=Journal(data_dir=tmp_path / "j"))
    assert result.status == "halted", result
    # a.txt ran and was rolled back to its original; c.txt never ran.
    assert (out / "a.txt").read_text() == "A0"
    assert not (out / "c.txt").exists()
    assert result.executed == 1  # only a.txt completed before the halt at b.txt
    assert result.rollback is not None
    assert any("b.txt" in s.get("path", "") for s in result.rollback.skipped)


async def test_network_step_is_get_only_by_the_type_system(tmp_path):
    """``network`` is batchable only because it is read-only. Pin that invariant so
    the classification can't silently rot into a wrong 'never' if a POST path is ever
    added — ``NetworkStep.method`` is ``Literal["GET"]``, so a non-GET step is a
    validation error, not a silent irreversible effect inside a batch."""
    import typing

    import pydantic

    from opendaisugi.models import NetworkStep

    assert typing.get_args(NetworkStep.model_fields["method"].annotation) == ("GET",)
    assert NetworkStep(id="n", url="https://example.com").method == "GET"
    with pytest.raises(pydantic.ValidationError):
        NetworkStep(id="n", url="https://example.com", method="POST")


# --------------------------------------------------------------------------- #
# 6. rollback-report honesty (a rollback must never claim more than it undid)
# --------------------------------------------------------------------------- #
async def test_rollback_records_apply_failures_in_skipped_and_undoes_the_rest(monkeypatch):
    """A failed ``apply_reversal`` (permissions, disk full) must land in ``skipped``,
    not raise past the caller — the report must never claim more undone than it did.
    The other handles still roll back."""
    from opendaisugi.models import ReversalHandle

    good = ReversalHandle(kind="file_write", path="/w/good.txt", prior_existed=False)
    bad = ReversalHandle(kind="file_write", path="/w/bad.txt", prior_existed=False)

    def fake_apply(h):
        if h.path == "/w/bad.txt":
            raise OSError("disk full")

    monkeypatch.setattr("opendaisugi.deeds.apply_reversal", fake_apply)
    report = batch._rollback([good, bad])  # execution order; reversed → bad first, good second
    assert report.undone == ["/w/good.txt"]
    assert any("bad.txt" in s["path"] and "rollback failed" in s["reason"] for s in report.skipped)


async def test_rollback_result_returns_the_internal_report_for_a_halted_batch():
    """A harness that uniformly calls ``rollback_result`` after any batch must get the
    real report on a halted/rejected batch — not a vacuous empty 'nothing to undo'
    (the batch already rolled back internally)."""
    rep = batch.RollbackReport(
        undone=["/x/a"], skipped=[{"path": "/x/b", "reason": "irreversible"}]
    )
    result = batch.BatchResult(status="halted", rollback=rep)
    assert batch.rollback_result(result) is rep


async def test_prove_footprint_flags_a_read_only_batch_as_vacuous(tmp_path):
    """A read-only program has no writes to prove: the write-set proof is vacuously
    ok and must SAY so, so 'the proof passed' is never read as a meaningful claim."""
    out = tmp_path / "out"
    prog = ActionPlan(source="b", task="x", steps=[FileReadStep(id="r", path=f"{out}/a.txt")])
    decl = batch.BatchDeclaration(program=prog, parameters=[], items=[{}], footprint=[])
    proof = batch.prove_footprint(decl, _env(tmp_path))
    assert proof.ok is True
    assert proof.vacuous is True
    assert proof.resolved_writes == []


async def test_run_batch_sample_validation_rejects_on_Q_failure(tmp_path, monkeypatch):
    """If the acceptance postcondition Q fails on the sampled fork, the full batch
    never runs — and the sampled writes are rolled back, leaving no trace."""
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    out = tmp_path / "out"
    out.mkdir()
    (out / "a.txt").write_text("ORIGINAL")  # a pre-existing sample target
    # Q that can never hold — an impossible postcondition type is fail-closed.
    decl = _decl(
        out,
        ["a.txt", "b.txt", "c.txt", "d.txt"],
        footprint=[f"{out}/**"],
        acceptance=Postcondition(type="never_holds_unknown_type"),
        sample_k=2,
    )
    result = await batch.run_batch(decl, _env(tmp_path), journal=Journal(data_dir=tmp_path / "j"))
    assert result.status == "rejected"
    assert result.sample_ok is False
    assert result.executed == 0
    assert (out / "a.txt").read_text() == "ORIGINAL"  # sample write rolled back
    assert not (out / "c.txt").exists()  # never reached the full run
