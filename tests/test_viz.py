"""Tests for opendaisugi.viz — render an ActionPlan as an execution-monitor page."""

from __future__ import annotations

import json

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileReadStep,
    MCPStep,
    Permission,
    ShellStep,
    TaskStep,
)
from opendaisugi.viz import plan_to_viz_data, render_dag_html


def _fixture():
    env = Envelope(
        generated_by="t",
        task="triage and act",
        permissions=Permission(shell=True, shell_allowlist=["find"], mcp_allowlist=[]),
    )
    plan = ActionPlan(
        source="t",
        task="triage and act",
        steps=[
            ShellStep(id="s1", command="find . -name '*.log'"),
            FileReadStep(id="s2", path="cfg.conf"),
            TaskStep(id="s3", prompt="decide what to delete", depends_on=["s1", "s2"]),
            MCPStep(id="s4", server="github", tool="create_issue", depends_on=["s3"]),
            ShellStep(id="s5", command="rm -rf /tmp/x", depends_on=["s3"]),
        ],
    )
    return plan, env


def test_plan_to_viz_data_levels_blocked_and_llm():
    plan, env = _fixture()
    d = plan_to_viz_data(plan, env)

    # dependency levels: s1,s2 parallel -> s3 -> s4,s5
    assert d["levels"][0] == ["s1", "s2"]
    assert d["levels"][1] == ["s3"]
    assert set(d["levels"][2]) == {"s4", "s5"}

    by = {s["id"]: s for s in d["steps"]}
    # the mcp call and the rm shell are refused (deny-by-default / not in allowlist)
    assert by["s4"]["blocked"] is True
    assert by["s5"]["blocked"] is True
    assert by["s1"]["blocked"] is False
    # only the task step actually spends a model
    assert by["s3"]["runs_llm"] is True
    assert by["s3"]["model"]
    assert by["s1"]["runs_llm"] is False
    # a refused step carries its proven reason
    assert by["s5"]["violations"] and "allowlist" in by["s5"]["violations"][0]["message"]
    assert d["ok"] is False
    assert d["n_violations"] >= 2


def test_render_dag_html_is_self_contained():
    plan, env = _fixture()
    html = render_dag_html(plan, env)

    # the data was injected (placeholder gone)
    assert "/*__DATA__*/" not in html
    assert "decide what to delete" in html  # a real step label made it in
    # self-contained: no external resource fetches (CSP would block them anyway)
    for bad in ('src="http', "src='http", 'href="http', "href='http", "@import url(http"):
        assert bad not in html, f"external resource reference found: {bad}"
    # the injected data is valid JSON round-tripping the task
    start = html.index("const DATA = ") + len("const DATA = ")
    end = html.index(";", start)
    data = json.loads(html[start:end])
    assert data["task"] == "triage and act"


def test_render_dag_html_escapes_script_close_in_labels():
    # A command containing </script> must not break out of the data block.
    env = Envelope(generated_by="t", task="x", permissions=Permission(shell=True, shell_allowlist=["echo"]))
    plan = ActionPlan(
        source="t", task="x", steps=[ShellStep(id="s1", command="echo '</script>'")]
    )
    html = render_dag_html(plan, env)
    assert "</script>'" not in html.split("const DATA = ")[1].split(";", 1)[0]


def _seed_store(tmp_path):
    from opendaisugi.pathway import CompiledPathway
    from opendaisugi.pathway_store import PathwayStore

    plan, env = _fixture()
    pw = CompiledPathway(
        id="pathway_test1",
        task_description="triage and act",
        task_embedding=[0.1, 0.2],
        envelope=env,
        plan_template=plan,
        source_trace_ids=["a", "b", "c"],
        distilled_at=0.0,
    )
    store = PathwayStore(tmp_path / "pathways.db")
    store.put(pw)
    return store


def test_viz_cli_renders_stored_pathway(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    _seed_store(tmp_path)
    out = tmp_path / "viz.html"
    res = CliRunner().invoke(
        app, ["viz", "pathway_test1", "--data-dir", str(tmp_path), "-o", str(out)]
    )
    assert res.exit_code == 0, res.output
    assert out.exists()
    html = out.read_text(encoding="utf-8")
    assert "/*__DATA__*/" not in html
    assert "triage and act" in html


def test_viz_cli_lists_pathways_when_no_id(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    _seed_store(tmp_path)
    res = CliRunner().invoke(app, ["viz", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "pathway_test1" in res.output


def test_viz_cli_missing_store_errors_cleanly(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    res = CliRunner().invoke(app, ["viz", "whatever", "--data-dir", str(tmp_path)])
    assert res.exit_code == 1
    assert "No pathway store" in res.output
