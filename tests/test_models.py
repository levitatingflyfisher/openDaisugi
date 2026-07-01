"""Tests for opendaisugi.models — data model validation."""

import pytest
from pydantic import ValidationError

from opendaisugi.models import (
    FallbackStrategy,
    Invariant,
    Permission,
    Postcondition,
)

# ----- Permission -----


def test_permission_defaults_are_restrictive():
    p = Permission()
    assert p.file_read == []
    assert p.file_write == []
    assert p.network is False
    assert p.shell is False
    assert p.shell_allowlist == []
    assert p.max_execution_time_s == 30
    assert p.max_output_size_mb == 10


def test_permission_accepts_globs():
    p = Permission(file_read=["*.csv", "/var/log/**"], file_write=["out/*.png"])
    assert "*.csv" in p.file_read
    assert "/var/log/**" in p.file_read
    assert "out/*.png" in p.file_write


def test_permission_rejects_wrong_types():
    with pytest.raises(ValidationError):
        Permission(shell="maybe")  # type: ignore[arg-type]


# ----- Invariant -----


def test_invariant_requires_type_and_description():
    inv = Invariant(type="file_unchanged", description="Source CSV must not be modified")
    assert inv.type == "file_unchanged"
    assert inv.description == "Source CSV must not be modified"
    assert inv.target is None
    assert inv.scope is None


def test_invariant_accepts_target_and_scope():
    inv = Invariant(
        type="no_side_effects",
        scope="outside /workspace",
        description="No writes outside workspace",
    )
    assert inv.scope == "outside /workspace"


# ----- Postcondition -----


def test_postcondition_file_exists():
    pc = Postcondition(type="file_exists", path="out/chart.png")
    assert pc.type == "file_exists"
    assert pc.path == "out/chart.png"
    assert pc.expected is None


def test_postcondition_exit_code():
    pc = Postcondition(type="exit_code", expected=0)
    assert pc.expected == 0


def test_postcondition_file_size_range():
    pc = Postcondition(type="file_size_range", path="out/chart.png", min=1024, max=10485760)
    assert pc.min == 1024
    assert pc.max == 10485760


# ----- FallbackStrategy -----


def test_fallback_strategy_defaults():
    fb = FallbackStrategy()
    assert fb.strategy == "tier2_recompute"
    assert fb.model == "anthropic/claude-sonnet-4-20250514"
    assert fb.include_refinement is True


# ----- Envelope -----


from opendaisugi.models import Envelope  # noqa: E402


def test_envelope_requires_core_fields():
    with pytest.raises(ValidationError):
        Envelope()  # type: ignore[call-arg]  # missing generated_by, task, permissions


def test_envelope_minimal_valid():
    env = Envelope(
        generated_by="test-model",
        task="Delete tmp files",
        permissions=Permission(),
    )
    assert env.generated_by == "test-model"
    assert env.task == "Delete tmp files"
    assert env.invariants == []
    assert env.postconditions == []
    assert env.parent_envelope is None
    assert env.tightening_only is True
    assert env.id.startswith("env_")
    assert len(env.id) > 4


def test_envelope_full_construction():
    env = Envelope(
        generated_by="anthropic/claude-sonnet-4-20250514",
        task="Convert sales.csv to bar chart",
        permissions=Permission(shell=True, shell_allowlist=["python3"], file_read=["sales.csv"]),
        invariants=[Invariant(type="file_unchanged", target="sales.csv", description="source immutable")],
        postconditions=[Postcondition(type="file_exists", path="out.png")],
    )
    assert env.permissions.shell is True
    assert len(env.invariants) == 1
    assert env.invariants[0].target == "sales.csv"
    assert len(env.postconditions) == 1


def test_envelope_ids_are_unique():
    a = Envelope(generated_by="m", task="t", permissions=Permission())
    b = Envelope(generated_by="m", task="t", permissions=Permission())
    assert a.id != b.id


def test_envelope_summary_defaults_to_none():
    env = Envelope(generated_by="t", task="t", permissions=Permission())
    assert env.summary is None


def test_envelope_summary_round_trip():
    env = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(),
        summary="reads /tmp/foo.csv and counts rows",
    )
    dumped = env.model_dump_json()
    env2 = Envelope.model_validate_json(dumped)
    assert env2.summary == "reads /tmp/foo.csv and counts rows"


def test_envelope_summary_max_length_rejected():
    with pytest.raises(ValidationError):
        Envelope(
            generated_by="t",
            task="t",
            permissions=Permission(),
            summary="x" * 81,
        )


# ----- ActionStep (discriminated union) / ActionPlan -----


from opendaisugi.models import (  # noqa: E402
    ActionPlan,
    FileWriteStep,
    ShellStep,
)


def test_action_step_requires_id_and_type():
    with pytest.raises(ValidationError):
        ShellStep(command="echo")  # missing id


def test_action_step_shell():
    step = ShellStep(id="s1", command="python3 chart.py")
    assert step.id == "s1"
    assert step.type == "shell"
    assert step.command == "python3 chart.py"
    assert step.depends_on == []


def test_action_step_file_write():
    step = FileWriteStep(id="s2", path="chart.py", content="print('hi')")
    assert step.path == "chart.py"
    assert step.content == "print('hi')"


def test_action_step_dependencies():
    step = ShellStep(id="s3", command="cat output", depends_on=["s1", "s2"])
    assert step.depends_on == ["s1", "s2"]


def test_action_plan_minimal():
    plan = ActionPlan(
        source="vanilla-llm",
        task="print hello",
        steps=[ShellStep(id="s1", command="echo hello")],
    )
    assert plan.source == "vanilla-llm"
    assert len(plan.steps) == 1
    assert plan.id.startswith("plan_")


def test_action_plan_requires_steps_list():
    with pytest.raises(ValidationError):
        ActionPlan(source="test", task="t")  # type: ignore[call-arg]


def test_action_plan_empty_steps_allowed():
    # An empty plan is structurally valid; DAG check may flag it.
    plan = ActionPlan(source="test", task="empty", steps=[])
    assert plan.steps == []


# ----- Violation / VerificationResult / Trace -----


from opendaisugi.models import Trace, VerificationResult, Violation  # noqa: E402


def test_violation_minimal():
    v = Violation(stage="permissions", message="shell command not in allowlist")
    assert v.stage == "permissions"
    assert v.detail == {}


def test_violation_with_detail():
    v = Violation(stage="z3", message="unsat", detail={"unsat_core": "[shell, shell_allowlist]"})
    assert v.detail["unsat_core"] == "[shell, shell_allowlist]"


def test_verification_result_ok():
    r = VerificationResult(ok=True, envelope_id="env_1", plan_id="plan_1", duration_ms=12.5)
    assert r.ok is True
    assert r.violations == []
    assert r.warnings == []
    assert r.duration_ms == 12.5


def test_verification_result_with_violation():
    r = VerificationResult(
        ok=False,
        violations=[Violation(stage="dag", message="cycle detected")],
        envelope_id="env_1",
        plan_id="plan_1",
        duration_ms=3.2,
    )
    assert r.ok is False
    assert len(r.violations) == 1
    assert r.violations[0].stage == "dag"


def test_trace_full_body():
    r = VerificationResult(ok=True, envelope_id="env_1", plan_id="plan_1", duration_ms=12.5)
    trace = Trace(
        id="2026-04-09-a1b2c3d4",
        created_at="2026-04-09T14:30:00Z",
        task="Delete .tmp files",
        plan_id="plan_1",
        envelope_id="env_1",
        ok=True,
        duration_ms=12.5,
        violations=[],
    )
    assert trace.ok is True
    assert trace.task == "Delete .tmp files"
    assert trace.violations == []


def test_envelope_cache_key_defaults_to_none():
    from opendaisugi.models import Envelope, Permission
    env = Envelope(generated_by="t", task="t", permissions=Permission())
    assert env.cache_key is None


def test_envelope_cache_key_round_trips():
    from opendaisugi.models import Envelope, Permission
    env = Envelope(
        generated_by="t", task="t", permissions=Permission(),
        cache_key="sha256_abc",
    )
    restored = Envelope.model_validate_json(env.model_dump_json())
    assert restored.cache_key == "sha256_abc"


def test_envelope_legacy_json_without_cache_key_deserializes():
    """Envelopes serialized before v0.2.1 (no cache_key) still load."""
    from opendaisugi.models import Envelope
    legacy_json = (
        '{"id":"env_1","generated_by":"t","task":"t",'
        '"permissions":{"file_read":[],"file_write":[],"network":false,'
        '"network_hosts":[],"shell":false,"shell_allowlist":[],'
        '"max_execution_time_s":30,"max_output_size_mb":10},'
        '"invariants":[],"postconditions":[],'
        '"fallback":{"strategy":"tier2_recompute","model":"m","include_refinement":true},'
        '"parent_envelope":null,"tightening_only":true,"summary":null}'
    )
    env = Envelope.model_validate_json(legacy_json)
    assert env.cache_key is None
