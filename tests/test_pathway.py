"""Tests for CompiledPathway + PathwayMatch data types (v0.3.0)."""

import time

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway, PathwayMatch


def _mk_pathway(**overrides):
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    defaults = dict(
        id="pathway_abc12345",
        task_description="generalized task",
        task_embedding=[0.1, 0.2, 0.3],
        envelope=env,
        plan_template=plan,
        source_trace_ids=["trace_1", "trace_2"],
        version=1,
        hit_count=0,
        distilled_at=time.time(),
    )
    defaults.update(overrides)
    return CompiledPathway(**defaults)


def test_compiled_pathway_roundtrips_json():
    p = _mk_pathway()
    js = p.model_dump_json()
    p2 = CompiledPathway.model_validate_json(js)
    assert p2.id == p.id
    assert p2.task_embedding == p.task_embedding
    assert p2.envelope.id == p.envelope.id
    assert p2.plan_template.id == p.plan_template.id


def test_compiled_pathway_defaults():
    # defaults are explicit on the class — construct without version/hit_count:
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    p2 = CompiledPathway(
        id="pathway_xyz",
        task_description="x",
        task_embedding=[0.0],
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
    )
    assert p2.version == 1
    assert p2.hit_count == 0


def test_pathway_match_optional_adapted_plan():
    p = _mk_pathway()
    m = PathwayMatch(pathway=p, similarity=0.92)
    assert m.adapted_plan is None

    plan2 = ActionPlan(source="adapt", task="T", steps=[ShellStep(id="s2", command="ls")])
    m2 = PathwayMatch(pathway=p, similarity=0.91, adapted_plan=plan2)
    assert m2.adapted_plan is plan2
