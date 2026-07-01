"""Dynamic step-type registration (v0.18 L5)."""
from typing import Literal

from opendaisugi.models import StepBase, get_step_type_registry, step_type


def test_register_and_retrieve_custom_step_type():
    @step_type
    class DraftEmail(StepBase):
        type: Literal["draft_email"] = "draft_email"
        recipient: str
        body: str

    reg = get_step_type_registry()
    assert "draft_email" in reg
    assert reg["draft_email"] is DraftEmail


def test_builtin_step_types_registered():
    reg = get_step_type_registry()
    # Built-ins self-register at import
    for t in ("shell", "file_read", "file_write", "network"):
        assert t in reg, f"{t} should be registered"


def test_robotics_step_types_registered():
    """v0.8 robotics types are in the registry too (domain-agnostic substrate)."""
    reg = get_step_type_registry()
    # Only assert present IF they're imported — JointMoveStep lives in models
    assert "joint_move" in reg
    assert "cartesian_move" in reg


def test_step_accepts_preferred_model_hint():
    """v0.19 L2: any step type carries an optional preferred_model hint."""
    from opendaisugi.models import ShellStep
    s = ShellStep(id="s1", command="echo hi", preferred_model="haiku")
    assert s.preferred_model == "haiku"
    s2 = ShellStep(id="s2", command="echo bye")
    assert s2.preferred_model is None


def test_step_preferred_model_roundtrips():
    """v0.19 L2: preferred_model survives JSON serialization for journal/pathway storage."""
    from opendaisugi.models import ShellStep
    s = ShellStep(id="s1", command="echo hi", preferred_model="sonnet")
    s2 = ShellStep.model_validate_json(s.model_dump_json())
    assert s2.preferred_model == "sonnet"


def test_registry_returns_copy_not_reference():
    """Callers should not be able to mutate the real registry by modifying
    the returned dict."""
    reg1 = get_step_type_registry()
    reg1["fake"] = None  # type: ignore
    reg2 = get_step_type_registry()
    assert "fake" not in reg2


def test_step_type_collision_raises_by_default():
    """A second registration of the same discriminator must raise — protects
    built-ins (and any earlier-loaded kit) from silent shadowing by a later
    import."""
    import pytest as _pytest

    with _pytest.raises(ValueError, match="collision"):
        @step_type
        class ImpostorShell(StepBase):
            type: Literal["shell"] = "shell"


def test_step_type_override_replaces_explicitly():
    """Callers who really want to replace a registered type pass override=True."""
    from opendaisugi.models import STEP_TYPE_REGISTRY
    original = STEP_TYPE_REGISTRY["shell"]
    try:
        @step_type(override=True)
        class CustomShell(StepBase):
            type: Literal["shell"] = "shell"
        assert STEP_TYPE_REGISTRY["shell"] is CustomShell
    finally:
        # Restore so other tests aren't affected
        STEP_TYPE_REGISTRY["shell"] = original


def test_step_type_idempotent_reregistration_of_same_class():
    """Re-applying @step_type to the same class is a no-op, not a collision."""
    @step_type
    class OnlyOnce(StepBase):
        type: Literal["only_once"] = "only_once"

    # Second decorator application should NOT raise (same class).
    step_type(OnlyOnce)


def test_coerce_step_dispatches_dict_to_subclass():
    """coerce_step is the shared helper used by ActionPlan and RefinementRecord."""
    from opendaisugi.models import ShellStep, coerce_step
    out = coerce_step({"id": "s1", "type": "shell", "command": "echo hi"})
    assert isinstance(out, ShellStep)
    assert out.command == "echo hi"


def test_coerce_step_passes_through_already_instantiated():
    from opendaisugi.models import ShellStep, coerce_step
    s = ShellStep(id="s1", command="echo hi")
    assert coerce_step(s) is s


def test_coerce_step_passes_through_unknown_dicts_for_pydantic_to_error():
    from opendaisugi.models import coerce_step
    weird = {"id": "s1", "type": "definitely_not_registered"}
    out = coerce_step(weird)
    # coerce_step doesn't raise — it returns the dict and lets the outer
    # Pydantic validation report the error.
    assert out is weird
