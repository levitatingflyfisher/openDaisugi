"""B5 — composing distilled pathways as callable skills."""

from __future__ import annotations

from opendaisugi.compose import (
    pathway_contract_envelopes,
    pathway_skill_handler,
    pathway_skill_handlers_for,
)
from opendaisugi.executor import default_executors
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep, SkillStep
from opendaisugi.pathway import CompiledPathway, PathwayParameter


def _frozen(pid: str = "pw", cmd: str = "echo composed") -> CompiledPathway:
    return CompiledPathway(
        id=pid,
        task_description="d",
        task_embedding=[0.1],
        envelope=Envelope(
            generated_by="t", task="x", permissions=Permission(shell=True, shell_allowlist=["echo"])
        ),
        plan_template=ActionPlan(
            source="distilled", task="x", steps=[ShellStep(id="s1", command=cmd)]
        ),
        source_trace_ids=["t1"],
        distilled_at=0.0,
    )


def test_handler_runs_a_frozen_pathway_plan():
    handler = pathway_skill_handler(_frozen(), executors=default_executors())
    out = handler(SkillStep(id="c1", skill_id="pw"))
    assert "composed" in out


class _Store:
    def __init__(self, pathways):
        self._pathways = pathways

    def list_all(self):
        return list(self._pathways)


def test_contract_envelopes_published_for_frozen():
    envs = pathway_contract_envelopes(_Store([_frozen("frozen")]))
    assert "frozen" in envs
    assert envs["frozen"].permissions.shell is True


def test_targeted_resolver_frozen_only_via_store_get(tmp_path):
    # Resolve only referenced ids via cheap store.get; skip missing AND typed.
    from opendaisugi.pathway_store import PathwayStore

    typed = _frozen("typed").model_copy(
        update={
            "parameters": [
                PathwayParameter(
                    name="n",
                    step_index=0,
                    step_id="s1",
                    field="path",
                    head="/repo",
                    observed=["/repo/a", "/repo/b"],
                )
            ]
        }
    )
    store = PathwayStore(tmp_path / "p.db")
    store.put(_frozen("a"))
    store.put(typed)
    assert store.get("a").id == "a"
    assert store.get("missing") is None

    handlers = pathway_skill_handlers_for(
        store, {"a", "typed", "missing"}, executors=default_executors()
    )
    assert set(handlers) == {"a"}  # frozen + referenced + existing only
