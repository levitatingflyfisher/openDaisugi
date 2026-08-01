"""B3 — cluster-diff parameter discovery (ADR-0008).

A field becomes a typed data-hole only when it varies across cluster members
while its capability head (a path's directory, a URL's host) stays fixed. Shell
is excluded (its operands aren't glob-checked — see pathway_params._CAP_FIELD),
so shell pathways stay frozen. Anything else stays frozen or refuses the cluster.
"""

from __future__ import annotations

from opendaisugi.models import ActionPlan, FileReadStep, NetworkStep, ShellStep, TaskStep
from opendaisugi.pathway_params import diff_plans_for_parameters, rekey_to_template


def _shell(cmd: str, sid: str = "s1") -> ActionPlan:
    return ActionPlan(source="t", task="x", steps=[ShellStep(id=sid, command=cmd)])


def _read(path: str, sid: str = "s1") -> ActionPlan:
    return ActionPlan(source="t", task="x", steps=[FileReadStep(id=sid, path=path)])


def _net(url: str, sid: str = "s1") -> ActionPlan:
    return ActionPlan(source="t", task="x", steps=[NetworkStep(id=sid, url=url)])


def test_single_plan_is_frozen():
    assert diff_plans_for_parameters([_read("/repo/a.py")]) == []


def test_identical_plans_are_frozen():
    assert diff_plans_for_parameters([_read("/repo/a.py")] * 2) == []


def test_shell_command_is_not_parameterized():
    # Shell holes are excluded — argv[0] pins only the program, not the file
    # operands. grep stays FROZEN (still 0-token reuse), never a typed hole.
    assert (
        diff_plans_for_parameters([_shell("grep -rn TODO src"), _shell("grep -rn FIXME src")]) == []
    )


def test_varying_path_yields_a_full_parameter():
    params = diff_plans_for_parameters([_read("/repo/a.py"), _read("/repo/b.py")])
    assert len(params) == 1
    p = params[0]
    assert p.step_index == 0
    assert p.step_id == "s1"
    assert p.field == "path"
    assert p.head == "/repo"  # the directory is pinned; only the basename varies
    assert set(p.observed) == {"/repo/a.py", "/repo/b.py"}


def test_varying_path_different_dir_refuses():
    assert diff_plans_for_parameters([_read("/repo/a.py"), _read("/etc/passwd")]) == []


def test_varying_url_path_same_host_is_a_parameter():
    params = diff_plans_for_parameters(
        [_net("https://api.example.com/a"), _net("https://api.example.com/b")]
    )
    assert len(params) == 1
    assert params[0].field == "url"
    assert params[0].head == "https://api.example.com"


def test_differing_url_host_refuses_the_cluster():
    assert (
        diff_plans_for_parameters([_net("https://api.example.com/a"), _net("https://evil.com/a")])
        == []
    )


def test_differing_structure_refuses():
    assert diff_plans_for_parameters([_read("/repo/a.py"), _net("https://x.com/a")]) == []


def test_non_capability_steps_are_ignored():
    plans = [
        ActionPlan(source="t", task="x", steps=[TaskStep(id="s1", prompt="summarize A")]),
        ActionPlan(source="t", task="x", steps=[TaskStep(id="s1", prompt="summarize B")]),
    ]
    assert diff_plans_for_parameters(plans) == []


def test_rekey_to_template_remaps_step_id_by_position():
    params = diff_plans_for_parameters([_read("/repo/a.py"), _read("/repo/b.py")])
    # A generalized template with a DIFFERENT step id at the same position.
    template = ActionPlan(
        source="distilled",
        task="find",
        steps=[FileReadStep(id="read_step", path="/repo/TEMPLATE.py")],
    )
    remapped = rekey_to_template(params, template)
    assert len(remapped) == 1
    assert remapped[0].step_id == "read_step"
    assert remapped[0].step_index == 0


def test_rekey_bails_to_frozen_on_shape_mismatch():
    params = diff_plans_for_parameters([_read("/repo/a.py"), _read("/repo/b.py")])
    # Template's step at position 0 is a shell step, not file_read — can't bind there.
    template = ActionPlan(source="distilled", task="x", steps=[ShellStep(id="s", command="ls")])
    assert rekey_to_template(params, template) == []
