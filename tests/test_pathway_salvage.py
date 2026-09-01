"""Do-nothing salvage of divergent clusters (the gradual-automation inversion).

A cluster whose plans agree in shape but genuinely differ at some positions used
to distill into a frozen pathway replaying the representative's concrete step —
wrong for every member that did something else there. Now the divergent
positions become **delegated leaves**: an `AgenticStep` prompted with the
observed variants, run under the same envelope through the call-time gate. The
invariant steps stay concrete (zero-token replay), the data-only variance stays
typed holes, and the parts that truly varied in kind go to the LLM — Dan
Slimmon's do-nothing script with the model as the human.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from opendaisugi.models import (
    ActionPlan,
    AgenticStep,
    Envelope,
    FileWriteStep,
    Permission,
    ShellStep,
)
from opendaisugi.pathway_params import build_delegated_template, plan_divergence
from opendaisugi.verify import verify


def _plan(commands: list[str], task: str = "t") -> ActionPlan:
    steps = []
    prev = None
    for i, c in enumerate(commands):
        steps.append(ShellStep(id=f"s{i}", command=c, depends_on=[prev] if prev else []))
        prev = f"s{i}"
    return ActionPlan(source="test", task=task, steps=steps)


# --- plan_divergence ------------------------------------------------------------


def test_shell_variance_is_divergent_not_refused():
    plans = [
        _plan(["git status", "pytest -q"]),
        _plan(["git status", "cargo test"]),
        _plan(["git status", "pytest -x tests/x"]),
    ]
    divergent, params = plan_divergence(plans)
    assert divergent == [1]
    assert params == []


def test_identical_plans_have_no_divergence():
    plans = [_plan(["git status", "pytest -q"]), _plan(["git status", "pytest -q"])]
    assert plan_divergence(plans) == ([], [])


def test_file_write_same_dir_stays_a_typed_hole():
    def p(path):
        return ActionPlan(
            source="test",
            task="t",
            steps=[
                ShellStep(id="s0", command="git status"),
                FileWriteStep(id="s1", path=path, content="", depends_on=["s0"]),
            ],
        )

    divergent, params = plan_divergence([p("out/a.txt"), p("out/b.txt")])
    assert divergent == []
    assert len(params) == 1 and params[0].field == "path"


def test_file_write_different_dirs_becomes_divergent_not_refuse_all():
    def p(path, cmd="git status"):
        return ActionPlan(
            source="test",
            task="t",
            steps=[
                ShellStep(id="s0", command=cmd),
                FileWriteStep(id="s1", path=path, content="", depends_on=["s0"]),
            ],
        )

    divergent, params = plan_divergence([p("out/a.txt"), p("/var/log/b.txt")])
    assert divergent == [1]
    assert params == []


def test_all_positions_divergent_refuses():
    plans = [_plan(["pytest -q", "make x"]), _plan(["cargo test", "ninja y"])]
    assert plan_divergence(plans) == ([], [])


def test_mixed_signatures_refuse():
    a = _plan(["git status"])
    b = ActionPlan(source="test", task="t", steps=[FileWriteStep(id="s0", path="x", content="")])
    assert plan_divergence([a, b]) == ([], [])


# --- build_delegated_template ---------------------------------------------------


def test_delegated_template_replaces_divergent_step_with_agentic_leaf():
    plans = [
        _plan(["git status", "pytest -q"]),
        _plan(["git status", "cargo test"]),
    ]
    template = build_delegated_template(plans[0], [1], plans)
    assert isinstance(template.steps[0], ShellStep)
    leaf = template.steps[1]
    assert isinstance(leaf, AgenticStep)
    assert leaf.id == "s1"
    assert leaf.depends_on == ["s0"]
    assert leaf.tools == ["Bash"]
    assert "pytest -q" in leaf.prompt and "cargo test" in leaf.prompt


def test_delegated_template_verifies_under_the_cluster_envelope():
    plans = [
        _plan(["git status", "pytest -q"]),
        _plan(["git status", "cargo test"]),
    ]
    template = build_delegated_template(plans[0], [1], plans)
    env = Envelope(
        generated_by="test",
        task="t",
        stakes="low",
        permissions=Permission(
            shell=True,
            shell_allowlist=["git", "pytest", "cargo"],
            file_read=["./**"],
        ),
    )
    result = verify(template, env)
    assert result.ok, result.violations


# --- distiller wiring: a divergent cluster salvages without any LLM call --------


@pytest.mark.asyncio
async def test_distill_cluster_salvages_divergent_cluster_without_llm(tmp_path, monkeypatch):
    import numpy as np

    from opendaisugi.distiller import Distiller
    from opendaisugi.pathway_store import PathwayStore

    env = Envelope(
        generated_by="test",
        task="t",
        stakes="low",
        permissions=Permission(
            shell=True,
            shell_allowlist=["git", "pytest", "cargo"],
            file_read=["./**"],
        ),
    )
    records = [
        SimpleNamespace(
            plan=_plan(["git status", "pytest -q"], task="run the tests"), envelope=env
        ),
        SimpleNamespace(
            plan=_plan(["git status", "cargo test"], task="run the tests"), envelope=env
        ),
        SimpleNamespace(
            plan=_plan(["git status", "pytest -x"], task="run the tests"), envelope=env
        ),
    ]
    metas = [SimpleNamespace(trace_id=f"t{i}", task="run the tests", run_id=None) for i in range(3)]

    distiller = Distiller(
        journal=SimpleNamespace(),  # untouched: no run_ids, no LLM on the salvage path
        pathway_store=PathwayStore(tmp_path / "p.db"),
        min_traces=3,
    )
    monkeypatch.setattr(
        distiller, "_load_records", lambda ts, w: [records[i] for i in range(len(ts))][: len(ts)]
    )

    warnings: list[str] = []
    pathway = await distiller._distill_cluster(metas, np.zeros(4, dtype=np.float32), warnings)
    assert pathway is not None, warnings
    leaf = pathway.plan_template.steps[1]
    assert isinstance(leaf, AgenticStep)
    assert isinstance(pathway.plan_template.steps[0], ShellStep)
    assert pathway.plan_template.steps[0].command == "git status"


# --- the salvaged leaf's workspace ------------------------------------------------


@pytest.mark.parametrize(
    ("file_read", "workspace"),
    [
        (["/work/**"], "/work"),
        (["**"], "."),
        (["./**"], "."),
        (["/**"], "/"),
        (["/work/a/../b/**"], "/work/b"),
        (["/work/out.txt"], None),
        (["/work/src/*.py"], None),
        (["/work/src/*.py", "/work/**"], "/work/src"),
        (["/work/src/*.py", "/other/**"], "/other"),
        ([], None),
    ],
)
def test_salvage_workspace_is_the_fixed_prefix_the_globs_admit(file_read, workspace):
    from opendaisugi.pathway_params import salvage_workspace

    assert salvage_workspace(file_read) == workspace


def _cluster_env(file_read: list[str]) -> Envelope:
    return Envelope(
        generated_by="test",
        task="t",
        stakes="low",
        permissions=Permission(shell=True, shell_allowlist=["git", "pytest", "cargo"], file_read=file_read),
    )


async def _distill_divergent(tmp_path, monkeypatch, env: Envelope):
    import numpy as np

    from opendaisugi.distiller import Distiller
    from opendaisugi.pathway_store import PathwayStore

    records = [
        SimpleNamespace(plan=_plan(["git status", cmd], task="run the tests"), envelope=env)
        for cmd in ("pytest -q", "cargo test", "pytest -x")
    ]
    metas = [SimpleNamespace(trace_id=f"t{i}", task="run the tests", run_id=None) for i in range(3)]
    distiller = Distiller(journal=SimpleNamespace(), pathway_store=PathwayStore(tmp_path / "p.db"), min_traces=3)
    monkeypatch.setattr(distiller, "_load_records", lambda ts, w: [records[i] for i in range(len(ts))])

    async def no_model(*a, **k):
        raise RuntimeError("no model in this test")

    import opendaisugi.distiller as distiller_module

    monkeypatch.setattr(distiller_module, "_generalize_template", no_model)
    warnings: list[str] = []
    try:
        pathway = await distiller._distill_cluster(metas, np.zeros(4, dtype=np.float32), warnings)
    except RuntimeError:
        pathway = None
    return pathway, warnings


@pytest.mark.asyncio
async def test_salvage_under_an_absolute_glob_uses_its_prefix(tmp_path, monkeypatch):
    pathway, warnings = await _distill_divergent(tmp_path, monkeypatch, _cluster_env(["/work/**"]))
    assert pathway is not None, warnings
    leaf = pathway.plan_template.steps[1]
    assert isinstance(leaf, AgenticStep)
    assert leaf.workspace == "/work"


@pytest.mark.asyncio
async def test_salvage_is_skipped_when_no_glob_gives_a_workspace(tmp_path, monkeypatch):
    pathway, warnings = await _distill_divergent(tmp_path, monkeypatch, _cluster_env(["/work/out.txt"]))
    assert pathway is None or not any(isinstance(s, AgenticStep) for s in pathway.plan_template.steps)
    assert (
        "delegated salvage skipped: no file_read glob gives the leaf a workspace; "
        "falling back to frozen generalization"
    ) in warnings
