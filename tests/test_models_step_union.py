"""Discriminated step union — round-trip + validation per kind."""

import pytest
import yaml
from pydantic import ValidationError

from opendaisugi.models import (
    ActionPlan, FileReadStep, FileWriteStep, NetworkStep, ShellStep,
)


def _roundtrip(plan: ActionPlan) -> ActionPlan:
    payload = plan.model_dump(mode="json")
    yaml_text = yaml.safe_dump(payload, sort_keys=False)
    return ActionPlan(**yaml.safe_load(yaml_text))


def test_shell_step_roundtrip():
    plan = ActionPlan(
        source="test", task="t",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], ShellStep)
    assert rt.steps[0].command == "echo hi"


def test_file_read_step_roundtrip():
    plan = ActionPlan(
        source="test", task="t",
        steps=[FileReadStep(id="s1", path="/tmp/x")],
    )
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], FileReadStep)
    assert rt.steps[0].path == "/tmp/x"


def test_file_write_step_roundtrip():
    plan = ActionPlan(
        source="test", task="t",
        steps=[FileWriteStep(id="s1", path="/tmp/x", content="hello")],
    )
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], FileWriteStep)
    assert rt.steps[0].content == "hello"


def test_network_step_roundtrip():
    plan = ActionPlan(
        source="test", task="t",
        steps=[NetworkStep(id="s1", url="https://example.com/x")],
    )
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], NetworkStep)
    assert rt.steps[0].method == "GET"
    assert rt.steps[0].url == "https://example.com/x"


def test_unknown_type_rejected_at_parse_time():
    raw = {
        "source": "test", "task": "t",
        "steps": [{"id": "s1", "type": "bogus", "command": "echo hi"}],
    }
    with pytest.raises(ValidationError):
        ActionPlan(**raw)


def test_missing_required_field_rejected():
    raw = {
        "source": "test", "task": "t",
        "steps": [{"id": "s1", "type": "file_read"}],  # missing path
    }
    with pytest.raises(ValidationError):
        ActionPlan(**raw)


def test_mixed_kind_plan_roundtrip():
    plan = ActionPlan(
        source="test", task="t",
        steps=[
            ShellStep(id="a", command="echo hi"),
            FileReadStep(id="b", path="/tmp/x", depends_on=["a"]),
            FileWriteStep(id="c", path="/tmp/y", content="data", depends_on=["b"]),
            NetworkStep(id="d", url="https://example.com/", depends_on=["c"]),
        ],
    )
    rt = _roundtrip(plan)
    assert [type(s).__name__ for s in rt.steps] == [
        "ShellStep", "FileReadStep", "FileWriteStep", "NetworkStep",
    ]


def test_network_hosts_empty_list_is_permissive_default():
    """Permission.network_hosts defaults to empty list, which means 'any host'.

    This pins the additive backwards-compat contract: an envelope from v0.1.0
    that says `network: true` without listing hosts must still permit any URL
    when v0.1.1 adds the field. Enforcement of non-empty allowlists ships in
    Task 7; this test guards against accidentally inverting the default.
    """
    from opendaisugi.models import Permission
    p = Permission(network=True)
    assert p.network_hosts == []
