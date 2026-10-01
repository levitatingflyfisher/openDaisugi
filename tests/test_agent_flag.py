"""--agent on weave, run and orchestrate: the runtime of agentic steps.

run and orchestrate carry an agentic executor too, so a reused delegated
pathway runs its agentic leaves. No real agent runs: the claude call and
the sprig process are spies.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
import yaml
from typer.testing import CliRunner

from opendaisugi import agentic_executor
from opendaisugi.cli import app
from opendaisugi.models import ActionPlan, AgenticStep, Envelope, Permission
from opendaisugi.orchestrator import Orchestrator
from opendaisugi.pathway import CompiledPathway, PathwayMatch


@pytest.fixture
def spies(monkeypatch):
    """Both runtimes, faked: record which one ran and answer."""
    seen: list[tuple[str, list[str]]] = []

    def _claude(prompt, *, timeout_s, model, binary, cwd, extra_args):
        seen.append(("claude", list(extra_args)))
        return json.dumps({"result": "claude did it", "is_error": False, "usage": {}})

    def _sprig(argv, *, stdin, cwd, timeout_s):
        seen.append(("sprig", list(argv)))
        return 0, json.dumps({"answer": "sprig did it", "turns": 2}).encode(), b"", False

    monkeypatch.setattr(agentic_executor, "call_claude_p_sync", _claude)
    monkeypatch.setattr(agentic_executor, "_run_sprig", _sprig)
    return seen


def _agentic(w: Path) -> dict:
    return {
        "id": "g",
        "type": "agentic",
        "prompt": "fix it",
        "workspace": str(w),
        "tools": ["Read"],
        "depends_on": [],
    }


def _files(tmp_path: Path) -> tuple[Path, Path, Path]:
    w = tmp_path / "w"
    w.mkdir()
    plan = {"id": "plan_00000001", "source": "script", "task": "t", "steps": [_agentic(w)]}
    env = {
        "id": "env_00000001",
        "generated_by": "t",
        "task": "t",
        "permissions": {"file_read": [f"{w}/**"]},
    }
    (tmp_path / "p.json").write_text(json.dumps(plan))
    (tmp_path / "e.yaml").write_text(yaml.safe_dump(env))
    return w, tmp_path / "p.json", tmp_path / "e.yaml"


def _invoke(verb, tmp_path, *extra):
    _, p, e = _files(tmp_path)
    args = [verb, str(p), "-e", str(e), "--data-dir", str(tmp_path / "d"), "--yes", "--json"]
    out = CliRunner().invoke(app, [*args, *extra])
    return out


@pytest.mark.parametrize("verb", ["weave", "run"])
def test_the_default_agent_is_claude(tmp_path, spies, verb):
    out = _invoke(verb, tmp_path)
    assert out.exit_code == 0, out.output
    assert [s[0] for s in spies] == ["claude"]
    assert json.loads(out.stdout)["steps"][0]["stdout"] == "claude did it"


@pytest.mark.parametrize("verb", ["weave", "run"])
def test_agent_sprig_runs_the_step_on_sprig(tmp_path, spies, verb):
    out = _invoke(verb, tmp_path, "--agent", "sprig")
    assert out.exit_code == 0, out.output
    assert [s[0] for s in spies] == ["sprig"]
    assert json.loads(out.stdout)["steps"][0]["stdout"] == "sprig did it"


@pytest.mark.parametrize("verb", ["weave", "run"])
def test_a_bad_agent_value_exits_2(tmp_path, spies, verb):
    out = _invoke(verb, tmp_path, "--agent", "pi")
    assert out.exit_code == 2
    assert "Invalid --agent 'pi'; choose from ['claude', 'sprig']." in out.stderr
    assert spies == []


def test_orchestrate_refuses_a_bad_agent_value(tmp_path):
    out = CliRunner().invoke(app, ["orchestrate", "say hi", "--agent", "pi"])
    assert out.exit_code == 2
    assert "Invalid --agent 'pi'; choose from ['claude', 'sprig']." in out.stderr


def test_run_dry_run_covers_an_agentic_step(tmp_path, spies):
    out = _invoke("run", tmp_path, "--dry-run")
    w = tmp_path / "w"
    assert out.exit_code == 0, out.output
    assert spies == []
    doc = json.loads(out.stdout.split("\n", 1)[1])
    assert doc["steps"][0]["stdout"] == f"[dry-run] would agentic: 'fix it' in {w} tools=['Read']"


def _pathway(w: Path) -> CompiledPathway:
    template = ActionPlan(
        source="distilled",
        task="fix the thing",
        steps=[AgenticStep(id="g", prompt="fix it", workspace=str(w), tools=["Read"])],
    )
    return CompiledPathway(
        id="pw_1",
        task_description="fix the thing",
        task_embedding=[0.1],
        envelope=Envelope(generated_by="t", task="t", permissions=Permission()),
        plan_template=template,
        source_trace_ids=["tr1"],
        distilled_at=0.0,
    )


@pytest.mark.parametrize("agent", ["claude", "sprig"])
async def test_orchestrate_runs_the_agentic_leaf_of_a_reused_pathway(tmp_path, spies, agent):
    w = tmp_path / "w"
    w.mkdir()
    pathway = _pathway(w)

    class _Store:
        def find(self, task, *, threshold):
            return PathwayMatch(pathway=pathway, similarity=0.99)

    orch = Orchestrator(pathway_store=_Store(), agent=agent)
    result = await orch.orchestrate(
        "fix the thing",
        envelope=Envelope(
            generated_by="t", task="t", permissions=Permission(file_read=[f"{w}/**"])
        ),
        synth_llm=False,
    )
    assert result.reused_pathway is True
    assert [s[0] for s in spies] == [agent]
    step = result.session.steps[0]
    assert (step.status, step.stdout) == ("succeeded", f"{agent} did it")
