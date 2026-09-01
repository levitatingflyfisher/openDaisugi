"""weave: typed slots between steps, the router for task steps, resume."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
import yaml
from typer.testing import CliRunner

from opendaisugi import weave
from opendaisugi.cli import app
from opendaisugi.models import ActionPlan


def _plan(steps: list[dict]) -> dict:
    return {"id": "plan_00000001", "source": "script", "task": "t", "steps": steps}


def _env(w: Path, **perm) -> dict:
    p = {
        "file_read": [f"{w}/**"],
        "file_write": [f"{w}/**"],
        "shell": True,
        "shell_allowlist": ["printf", "cat", "false"],
        "shell_allow_decomposition": True,
    }
    p.update(perm)
    return {"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": p}


def _slots(steps: list[dict]):
    plan = ActionPlan(**_plan(steps))
    return weave.read_slots(steps, plan)


def test_read_slots_accepts_a_path_slot_into_a_later_read():
    steps = [
        {"id": "a", "type": "shell", "command": "x", "outputs": {"f": "path"}},
        {
            "id": "b",
            "type": "file_read",
            "path": "/w/out/x.txt",
            "depends_on": ["a"],
            "inputs": {"path": "a.f"},
        },
    ]
    spec = _slots(steps)
    assert spec.outputs == {"a": {"f": "path"}}
    assert spec.inputs == {"b": {"path": ("a", "f")}}


@pytest.mark.parametrize(
    ("steps", "want"),
    [
        (
            [
                {"id": "a", "type": "shell", "command": "x", "outputs": {"c": "string"}},
                {
                    "id": "b",
                    "type": "shell",
                    "command": "y",
                    "depends_on": ["a"],
                    "inputs": {"command": "a.c"},
                },
            ],
            "never a command",
        ),
        (
            [
                {"id": "a", "type": "shell", "command": "x", "outputs": {"f": "path"}},
                {"id": "b", "type": "file_read", "path": "/w/x", "inputs": {"path": "a.f"}},
            ],
            "does not depend on",
        ),
        (
            [
                {
                    "id": "a",
                    "type": "file_write",
                    "path": "/w/x",
                    "content": "",
                    "outputs": {"f": "path"},
                }
            ],
            "cannot declare outputs",
        ),
        ([{"id": "a", "type": "shell", "command": "x", "outputs": {"F": "path"}}], "slot name"),
        ([{"id": "a", "type": "shell", "command": "x", "outputs": {"f": "dict"}}], "the types are"),
        (
            [
                {"id": "a", "type": "shell", "command": "x", "outputs": {"n": "number"}},
                {
                    "id": "b",
                    "type": "file_read",
                    "path": "/w/x",
                    "depends_on": ["a"],
                    "inputs": {"path": "a.n"},
                },
            ],
            "takes path",
        ),
    ],
)
def test_read_slots_refuses(steps, want):
    with pytest.raises(weave.WeaveError, match=want):
        _slots(steps)


def _task_then_write(slot_type: str, field: str) -> list[dict]:
    return [
        {"id": "t", "type": "task", "prompt": "p", "outputs": {"v": slot_type}},
        {
            "id": "b",
            "type": "file_write",
            "path": "/w/out/x.txt",
            "content": "",
            "depends_on": ["t"],
            "inputs": {field: "t.v"},
        },
    ]


@pytest.mark.parametrize("slot_type", ["string", "number", "path", "list[string]"])
def test_a_model_slot_never_fills_content(slot_type):
    """A task step's output is model text: it is tainted, so it may not
    become a file's content (a later step may run that file)."""
    with pytest.raises(weave.WeaveError, match="model text never fills a file's content"):
        _slots(_task_then_write(slot_type, "content"))


def test_a_model_slot_may_fill_a_path_under_the_directory_rule():
    spec = _slots(_task_then_write("path", "path"))
    assert spec.inputs == {"b": {"path": ("t", "v")}}


def test_a_model_slot_taints_through_the_reference_not_the_type():
    """The taint is the source step's kind: a shell step's string still
    fills content."""
    steps = _task_then_write("string", "content")
    steps[0] = {"id": "t", "type": "shell", "command": "x", "outputs": {"v": "string"}}
    assert _slots(steps).inputs == {"b": {"content": ("t", "v")}}


def test_collect_checks_types():
    outs = {"p": "path", "n": "number", "l": "list[string]"}
    assert weave.collect(outs, '{"p": "/a/b", "n": 3, "l": ["x"]}', fence=False) == {
        "p": "/a/b",
        "n": 3,
        "l": ["x"],
    }
    assert "not a path" in weave.collect(outs, '{"p": "a/../b", "n": 3, "l": []}', fence=False)
    assert "not a number" in weave.collect(outs, '{"p": "/a", "n": true, "l": []}', fence=False)
    assert "no slot l" in weave.collect(outs, '{"p": "/a", "n": 1}', fence=False)
    assert "JSON object" in weave.collect(outs, "words", fence=False)
    assert weave.collect({"n": "number"}, '```json\n{"n": 1.5}\n```', fence=True) == {"n": 1.5}


def _run(tmp_path, steps, *extra, env_over=None):
    w = tmp_path / "w"
    w.mkdir(exist_ok=True)
    (tmp_path / "p.json").write_text(json.dumps(_plan(steps)))
    (tmp_path / "e.yaml").write_text(yaml.safe_dump(env_over or _env(w)))
    args = ["weave", str(tmp_path / "p.json"), "-e", str(tmp_path / "e.yaml")]
    args += ["--data-dir", str(tmp_path / "d"), "--yes", "--json", *extra]
    out = CliRunner().invoke(app, args)
    return out.exit_code, (json.loads(out.stdout) if out.stdout.strip().startswith("{") else out)


def test_a_slot_fills_a_later_write_and_is_verified_again(tmp_path):
    w = tmp_path / "w"
    steps = [
        {
            "id": "a",
            "type": "shell",
            "command": f'printf \'{{"to": "{w}/out.txt", "body": "hi"}}\'',
            "outputs": {"to": "path", "body": "string"},
        },
        {
            "id": "b",
            "type": "file_write",
            "path": f"{w}/placeholder",
            "content": "",
            "depends_on": ["a"],
            "inputs": {"path": "a.to", "content": "a.body"},
        },
    ]
    code, doc = _run(tmp_path, steps)
    assert code == 0, doc
    assert (w / "out.txt").read_text() == "hi"
    assert doc["weave"]["filled"] == {"b": {"path": f"{w}/out.txt", "content": "hi"}}


def test_a_slot_that_moves_the_directory_halts(tmp_path):
    w = tmp_path / "w"
    steps = [
        {
            "id": "a",
            "type": "shell",
            "command": 'printf \'{"to": "/etc/x"}\'',
            "outputs": {"to": "path"},
        },
        {
            "id": "b",
            "type": "file_write",
            "path": f"{w}/placeholder",
            "content": "",
            "depends_on": ["a"],
            "inputs": {"path": "a.to"},
        },
    ]
    code, doc = _run(tmp_path, steps)
    assert code == 1
    assert doc["status"] == "halted_by_simplex"
    assert "would change the directory" in doc["steps"][-1]["error"]


def test_task_step_gets_its_model_from_the_router(tmp_path, monkeypatch):
    seen = {}

    def fake_run(self, step, *, timeout_s, max_output_bytes):
        from opendaisugi.executor import ExecutorResult

        seen[step.id] = (step.preferred_model, self.prompt_template(step))
        return ExecutorResult(rc=0, stdout='{"n": 2}', duration_ms=1.0, timed_out=False)

    monkeypatch.setattr("opendaisugi.delegating_executor.DelegatingExecutor.run", fake_run)
    steps = [{"id": "t", "type": "task", "prompt": "count", "outputs": {"n": "number"}}]
    code, doc = _run(tmp_path, steps)
    assert code == 0, doc
    assert seen["t"][0] == "claude-haiku-4-5"
    assert "Its keys and their types: n (number)" in seen["t"][1]
    assert doc["weave"]["slots"] == {"t": {"n": 2}}


def test_resume_skips_done_steps_and_reruns_reads(tmp_path):
    w = tmp_path / "w"
    (w).mkdir()
    (w / "f.txt").write_text("x")
    steps = [
        {"id": "a", "type": "shell", "command": "printf a"},
        {"id": "r", "type": "file_read", "path": f"{w}/f.txt", "depends_on": ["a"]},
        {"id": "b", "type": "shell", "command": "false", "depends_on": ["r"]},
    ]
    code, doc = _run(tmp_path, steps)
    assert code == 1
    code, doc = _run(tmp_path, steps, "--resume")
    assert code == 1
    assert [s["status"] for s in doc["steps"]] == ["skipped", "succeeded", "failed"]
    assert doc["integrity_passed"] is True
    assert list(doc["weave"]["skipped"]) == ["a"]


def test_resume_stops_before_a_started_step_with_no_receipt(tmp_path):
    steps = [
        {"id": "a", "type": "shell", "command": "printf a"},
        {"id": "b", "type": "shell", "command": "printf b", "depends_on": ["a"]},
    ]
    code, doc = _run(tmp_path, steps, "--resume")
    assert code == 0
    state = Path(doc["weave"]["state"])
    state.write_text(state.read_text() + json.dumps({"run": "run_deadbeef", "step": "b"}) + "\n")
    # b has a receipt from the first run, so it is skipped.
    code, doc = _run(tmp_path, steps, "--resume")
    assert code == 0 and doc["weave"]["skipped"] == {
        "a": doc["weave"]["skipped"]["a"],
        "b": doc["weave"]["skipped"]["b"],
    }
    # A fresh data dir: only the mark, no receipt.
    d2 = tmp_path / "d2" / "weave"
    d2.mkdir(parents=True)
    (d2 / state.name).write_text(json.dumps({"run": "run_deadbeef", "step": "a"}) + "\n")
    w = tmp_path / "w"
    (tmp_path / "e.yaml").write_text(yaml.safe_dump(_env(w)))
    base = ["weave", str(tmp_path / "p.json"), "-e", str(tmp_path / "e.yaml"), "--yes", "--json"]
    out = CliRunner().invoke(app, [*base, "--data-dir", str(tmp_path / "d2"), "--resume"])
    doc = json.loads(out.stdout)
    assert out.exit_code == 130 and doc["status"] == "aborted"
    assert "--rerun a" in doc["steps"][0]["error"]
    out = CliRunner().invoke(
        app, [*base, "--data-dir", str(tmp_path / "d2"), "--resume", "--rerun", "a"]
    )
    assert out.exit_code == 0


def test_the_filled_plan_is_verified_again_whole(tmp_path):
    # The per-step verify skips plan-level invariants; the placeholder
    # passed them, the filled value does not.
    w = tmp_path / "w"
    env = _env(w)
    env["invariants"] = [
        {
            "type": "no_secret",
            "description": "no secret in a write",
            "expr": {
                "op": "forall_steps",
                "pred": {"op": "not_matches", "path": "content", "regex": "secret"},
            },
        }
    ]
    steps = [
        {
            "id": "a",
            "type": "shell",
            "command": 'printf \'{"v": "a secret"}\'',
            "outputs": {"v": "string"},
        },
        {
            "id": "b",
            "type": "file_write",
            "path": f"{w}/x.txt",
            "content": "",
            "depends_on": ["a"],
            "inputs": {"content": "a.v"},
        },
    ]
    code, doc = _run(tmp_path, steps, env_over=env)
    assert code == 1 and doc["status"] == "halted_by_simplex"
    assert "the filled plan: invariant 'no_secret' violated" in doc["steps"][-1]["error"]
    assert not (w / "x.txt").exists()


def test_a_crash_in_a_later_run_is_asked_about(tmp_path):
    # b failed with a receipt in the first run; a later run marked b started
    # and left no receipt, so b may have run there.
    steps = [
        {"id": "a", "type": "shell", "command": "printf a"},
        {"id": "b", "type": "shell", "command": "false", "depends_on": ["a"]},
    ]
    code, doc = _run(tmp_path, steps)
    assert code == 1
    state = Path(doc["weave"]["state"])
    state.write_text(state.read_text() + json.dumps({"run": "run_0badc0de", "step": "b"}) + "\n")
    code, doc = _run(tmp_path, steps, "--resume")
    assert code == 130
    assert "started in run_0badc0de" in doc["steps"][-1]["error"]


def test_a_start_mark_that_fails_stops_the_step(tmp_path):
    from opendaisugi.supervisor import Supervisor  # noqa: F401 - the hook's caller

    steps = [{"id": "a", "type": "shell", "command": "printf a"}]
    d = tmp_path / "d"
    d.mkdir()
    (d / "weave").write_text("a file where the state directory goes")
    code, doc = _run(tmp_path, steps)
    assert code == 130 and doc["status"] == "aborted"
    assert doc["steps"][0]["error"].startswith("the start mark was not written:")


@pytest.mark.parametrize(
    ("step", "want"),
    [
        ({"id": "a", "type": "shell", "command": "x", "attempts": 2}, "only a task step"),
        ({"id": "a", "type": "task", "prompt": "p", "attempts": 1}, "from 2 to 8"),
        ({"id": "a", "type": "task", "prompt": "p", "attempts": 9}, "from 2 to 8"),
        ({"id": "a", "type": "task", "prompt": "p", "attempts": True}, "from 2 to 8"),
        ({"id": "a", "type": "task", "prompt": "p", "attempts": "3"}, "from 2 to 8"),
        ({"id": "a b", "type": "task", "prompt": "p", "attempts": 3}, "needs an id"),
    ],
)
def test_attempts_are_checked(step, want):
    with pytest.raises(weave.WeaveError, match=want):
        _slots([step])


def test_attempts_are_read():
    assert _slots([{"id": "a", "type": "task", "prompt": "p", "attempts": 3}]).attempts == {"a": 3}


def test_step_tier_is_fail_closed(tmp_path):
    from opendaisugi.models import ActionPlan

    plan = ActionPlan(
        **_plan(
            [
                {"id": "r", "type": "file_read", "path": "/etc/x"},
                {"id": "w", "type": "file_write", "path": f"{tmp_path}/a/b", "content": ""},
                {"id": "o", "type": "file_write", "path": "/elsewhere/b", "content": ""},
                {"id": "u", "type": "file_write", "path": f"{tmp_path}/../x", "content": ""},
                {"id": "s", "type": "shell", "command": "true"},
            ]
        )
    )
    tiers = {s.id: weave.step_tier(s, str(tmp_path)) for s in plan.steps}
    assert tiers == {
        "r": "undoable",
        "w": "undoable",
        "o": "permanent",
        "u": "permanent",
        "s": "permanent",
    }


def test_a_permanent_step_below_an_open_choice_is_asked_even_under_yes(tmp_path, monkeypatch):
    from opendaisugi.approval import ApprovalDecision
    from opendaisugi.models import ActionPlan

    steps = [
        {"id": "t", "type": "task", "prompt": "p", "attempts": 2},
        {"id": "s", "type": "shell", "command": "true", "depends_on": ["t"]},
        {
            "id": "w",
            "type": "file_write",
            "path": f"{tmp_path}/x",
            "content": "",
            "depends_on": ["t"],
        },
    ]
    plan = ActionPlan(**_plan(steps))
    hook = weave.WeaveHook(
        weave.read_slots(steps, plan), state=tmp_path / "st", prior=None, rerun=set(), plan=plan
    )
    hook.choices["t"] = {"choice_id": "ch_1", "chosen": "t#1", "status": "provisional"}

    class Yes:
        def decide(self, step, envelope):
            return ApprovalDecision(approved=True, approved_by="env", reason="yes")

    ask = weave.ChoiceAsk(Yes(), hook, str(tmp_path))
    by = {s.id: s for s in plan.steps}
    denied = ask.decide(by["s"], None)
    assert not denied.approved and "ch_1" in denied.reason
    assert ask.decide(by["w"], None).approved
