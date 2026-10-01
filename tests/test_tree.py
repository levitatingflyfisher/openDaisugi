"""The delegation tree: the deadline field, the edge rule, the ledger and
the commands."""

from __future__ import annotations

import json
import math

import pytest
from pydantic import ValidationError

from opendaisugi.models import Envelope


def _env(**perm) -> dict:
    extra = {k: perm.pop(k) for k in list(perm) if k in _TOP}
    p = {"file_read": ["/w/**"], "shell": True, "shell_allowlist": ["git", "ls"]}
    p.update(perm)
    return {"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": p, **extra}


_TOP = {
    "stakes",
    "deadline",
    "invariants",
    "postconditions",
    "shell_interpreter_policy",
    "id",
}


# ---------------------------------------------------------------------------
# The deadline field (AT-2)
# ---------------------------------------------------------------------------


def test_a_deadline_is_left_out_of_the_dump_when_absent():
    env = Envelope(**_env())
    assert "deadline" not in env.model_dump()
    assert '"deadline"' not in env.model_dump_json()
    assert "deadline" not in json.dumps(Envelope.model_json_schema())


def test_a_deadline_dumps_as_a_float():
    env = Envelope(**_env(deadline=1790000000))
    assert env.deadline == 1790000000.0
    assert json.loads(env.model_dump_json())["deadline"] == 1790000000.0
    assert Envelope.model_validate_json(env.model_dump_json()).deadline == 1790000000.0


@pytest.mark.parametrize("bad", [math.nan, math.inf, "soon"])
def test_a_deadline_that_is_not_a_finite_number_is_refused(bad):
    with pytest.raises(ValidationError):
        Envelope(**_env(deadline=bad))


# ---------------------------------------------------------------------------
# The edge rule
# ---------------------------------------------------------------------------

from opendaisugi import tree  # noqa: E402


def _edge(parent: dict, child: dict | None, **kw) -> tree.EdgeResult:
    return tree.edge_ok(Envelope(**parent), Envelope(**child) if child is not None else None, **kw)


def test_a_narrower_child_fits_at_any_depth():
    root = _env(file_read=["/w/**"], file_write=["/w/**"], shell_allowlist=["git", "ls"])
    mid = _env(file_read=["/w/src/**"], file_write=["/w/src/**"], shell_allowlist=["git"])
    leaf = _env(file_read=["/w/src/app/**"], shell_allowlist=["git status"])
    assert _edge(root, mid).holds
    res = _edge(mid, leaf)
    assert res.holds and res.reasons == []
    # Depth is not limited: an envelope that names its own parent is fine.
    assert _edge({**mid, "parent_envelope": "env_root"}, leaf).holds


def test_every_failing_part_is_reported_in_order():
    parent = _env(
        stakes="high",
        deadline=100.0,
        shell_interpreter_policy="strict",
        file_read=["/w/**"],
        shell_allowlist=["git"],
    )
    child = _env(
        stakes="low",
        custom_step_allowlist=["launch"],
        max_execution_time_s=60,
        deadline=200.0,
        shell_interpreter_policy="allow",
        file_read=["/etc/**"],
        shell_allowlist=["git", "rm", "python"],
        network=True,
    )
    res = _edge(parent, child)
    assert not res.holds
    assert res.reasons == [
        'stakes: the child\'s "low" is lower than the parent\'s "high"',
        'custom_step_allowlist: the child adds ["launch"]',
        "max_execution_time_s: the child's 60 is more than the parent's 30",
        "deadline: the child's 200.0 is after the parent's 100.0",
        'shell_interpreter_policy: the child\'s "allow" is looser than the parent\'s "strict"',
        "file_read: the child's \"/etc/**\" is not inside the parent's",
        "network: the child uses the network; the parent does not",
        'shell_allowlist: the child adds ["rm", "python"]',
        'shell_interpreter_policy: the parent is strict and the child allows the interpreters ["python"]',
    ]


def test_a_child_with_no_deadline_takes_the_parents():
    res = _edge(_env(deadline=100), _env())
    assert res.holds and res.deadline_inherited and res.child.deadline == 100.0
    res = _edge(_env(), _env(deadline=5))
    assert res.holds and not res.deadline_inherited and res.child.deadline == 5.0


def test_a_missing_envelope_is_refused():
    assert _edge(_env(), None).reasons == ["envelope: the child declares no envelope"]
    assert tree.edge_ok(None, Envelope(**_env())).reasons == [
        "envelope: the parent has no envelope"
    ]


def test_the_parents_enforced_invariants_must_stay():
    inv = {
        "type": "no_rm",
        "description": "d",
        "expr": {"op": "not_equals", "path": "command", "value": "rm"},
    }
    soft = {"type": "doc", "description": "d", "enforce": False}
    parent = _env(invariants=[inv, soft])
    assert _edge(parent, _env(invariants=[inv])).holds
    res = _edge(parent, _env(invariants=[{**inv, "enforce": False}]))
    assert res.reasons[0] == 'invariants: the child drops the parent\'s ["no_rm"]'


def test_an_opaque_child_invariant_cannot_be_proved():
    res = _edge(_env(), _env(invariants=[{"type": "mystery", "description": "d"}]))
    assert res.reasons == [
        'invariants: the child\'s ["mystery"] have no expr, so they cannot be proved'
    ]


def test_the_scope_axes_name_what_does_not_fit():
    res = _edge(
        _env(network=True, network_hosts=["a.example"], mcp_allowlist=["fs.*"]),
        _env(network=True, mcp_allowlist=["git.*"], shell_allow_decomposition=True),
    )
    assert res.reasons == [
        "shell_allow_decomposition: the child allows compound commands; the parent does not",
        "mcp_allowlist: the child's \"git.*\" is not inside the parent's",
        'network_hosts: the child allows any host; the parent allows only ["a.example"]',
    ]
    res = _edge(_env(file_read=["/w/a*b*"]), _env(file_read=["/w/x"]))
    assert res.reasons == [
        'file_read: the parent\'s pattern "/w/a*b*" has a shape the proof cannot read'
    ]


def test_robot_bounds_are_named_by_axis():
    box = [[0, 0, 0], [1, 1, 1]]
    res = _edge(_env(workspace_bounds=box), _env())
    assert res.reasons == ["robot: the child's workspace_bounds are not inside the parent's"]
    res = _edge(_env(joint_limits={"j1": [0, 1]}), _env(joint_limits={"j1": [0, 2]}))
    assert res.reasons == ["robot: the child's range for joint \"j1\" is not within the parent's"]


_NO_PUSH = {
    "type": "no_push",
    "description": "d",
    "expr": {"op": "not_equals", "path": "command", "value": "git push"},
}


def test_the_proof_runs_last_and_a_timeout_refuses(monkeypatch):
    from opendaisugi.subsumption import SubsumptionResult

    def unfinished(*a, **k):
        return SubsumptionResult(holds=False, counterexample=None, timed_out=True)

    monkeypatch.setattr("opendaisugi.subsumption.envelope_subsumes", unfinished)
    res = _edge(_env(invariants=[_NO_PUSH]), _env(invariants=[_NO_PUSH]), timeout_ms=7)
    assert res.reasons == ["proof: the proof did not finish in 7 ms"]
    # With no invariant to prove, the head rule decides and Z3 does not run.
    assert _edge(_env(), _env()).holds


def test_a_child_invariant_can_narrow_and_the_proof_sees_it():
    parent = _env(shell_allowlist=["git"])
    assert _edge(parent, _env(shell_allowlist=["git"], invariants=[_NO_PUSH])).holds
    res = _edge(
        {**parent, "invariants": [_NO_PUSH]},
        _env(shell_allowlist=["git"], invariants=[_NO_PUSH, _NO_PUSH]),
    )
    assert res.holds


def test_a_head_with_a_metacharacter_admits_nothing():
    assert _edge(_env(shell_allowlist=["git"]), _env(shell_allowlist=["rm; ls"])).holds


def test_an_invariant_the_child_lacks_fails_the_proof():
    parent = _env(
        invariants=[
            {
                "type": "no_force",
                "description": "d",
                "expr": {"op": "not_equals", "path": "command", "value": "git push"},
            }
        ]
    )
    child = {**parent, "invariants": []}
    res = _edge(parent, child)
    assert res.reasons[0] == 'invariants: the child drops the parent\'s ["no_force"]'


# ---------------------------------------------------------------------------
# The commands and the ledger
# ---------------------------------------------------------------------------

from typer.testing import CliRunner  # noqa: E402

from opendaisugi.cli import app  # noqa: E402


def _write(path, doc) -> str:
    path.write_text(json.dumps(doc), encoding="utf-8")
    return str(path)


def _tree(tmp_path, *args):
    res = CliRunner().invoke(app, ["tree", *args, "--data-dir", str(tmp_path / "d")])
    return res


def _rows(tmp_path) -> list[dict]:
    text = (tmp_path / "d" / "tree" / "ledger.jsonl").read_text()
    return [json.loads(x) for x in text.splitlines()]


def test_check_prints_the_reasons_and_exits_by_the_verdict(tmp_path):
    p = _write(tmp_path / "p.json", _env(deadline=100))
    c = _write(tmp_path / "c.json", _env(shell_allowlist=["git"]))
    res = CliRunner().invoke(app, ["tree", "check", p, c])
    assert res.exit_code == 0
    assert res.stdout == (
        "edge ok: the child fits inside the parent.\nThe child takes the parent's deadline 100.0.\n"
    )
    w = _write(tmp_path / "w.json", _env(shell_allowlist=["rm"]))
    res = CliRunner().invoke(app, ["tree", "check", p, w, "--json"])
    assert res.exit_code == 1
    assert json.loads(res.stdout) == {
        "holds": False,
        "reasons": ['shell_allowlist: the child adds ["rm"]'],
        "deadline": 100.0,
        "deadline_inherited": True,
    }
    res = CliRunner().invoke(app, ["tree", "check", p, str(tmp_path / "none.json")])
    assert res.exit_code == 2 and "There is no such file." in res.stderr


def test_spawn_reserves_end_returns_and_the_gate_root_holds_the_child(tmp_path):
    d = tmp_path / "d"
    root_env = _write(tmp_path / "r.json", _env(id="env_root"))
    child_env = _write(tmp_path / "c.json", _env(id="env_kid", shell_allowlist=["git"]))
    res = _tree(tmp_path, "root", root_env, "--session", "top", "--tokens", "1000", "--turns", "10")
    assert res.exit_code == 0, res.output
    res = _tree(
        tmp_path, "spawn", child_env, "--parent", "top", "--session", "kid", "--tokens", "600"
    )
    assert res.exit_code == 1
    assert res.stderr == (
        "not started: the edge from top to kid is refused: "
        "turns: the parent has a budget, so the child must ask for an amount\n"
    )
    res = _tree(
        tmp_path,
        "spawn",
        child_env,
        "--parent",
        "top",
        "--session",
        "kid",
        "--tokens",
        "600",
        "--turns",
        "4",
    )
    assert res.exit_code == 0, res.output
    reg = json.loads((d / "gate" / "envelopes" / "kid.json").read_text())
    assert reg["parent_envelope"] == "env_root"
    res = _tree(
        tmp_path,
        "spawn",
        child_env,
        "--parent",
        "top",
        "--session",
        "kid2",
        "--tokens",
        "500",
        "--turns",
        "1",
    )
    assert "tokens: the child asks for 500; the parent has 400 left" in res.stderr
    doc = json.loads(_tree(tmp_path, "status", "--json").stdout)
    top = doc["nodes"][0]
    assert top["tokens"] == {"budget": 1000, "used": None, "left": 400}
    assert top["refused"] == 1 and top["children"] == ["kid"]
    res = _tree(tmp_path, "end", "kid", "--tokens-used", "100", "--turns-used", "1")
    assert res.exit_code == 0
    assert "tokens: used 100 of 600; 500 go back to top." in res.stdout
    assert not (d / "gate" / "envelopes" / "kid.json").exists()
    doc = json.loads(_tree(tmp_path, "status", "--json").stdout)
    assert doc["nodes"][0]["tokens"]["left"] == 900
    assert [r["event"] for r in _rows(tmp_path)] == ["root", "refused", "spawn", "refused", "end"]


def test_the_fourth_refused_proposal_goes_to_the_operator(tmp_path):
    root_env = _write(tmp_path / "r.json", _env(id="env_root", shell_allowlist=["git", "rm"]))
    mid_env = _write(tmp_path / "m.json", _env(id="env_mid", shell_allowlist=["git"]))
    wide = _write(tmp_path / "w.json", _env(shell_allowlist=["rm"]))
    assert _tree(tmp_path, "root", root_env, "--session", "top").exit_code == 0
    assert _tree(tmp_path, "spawn", mid_env, "--parent", "top", "--session", "mid").exit_code == 0
    for _ in range(3):
        res = _tree(tmp_path, "spawn", wide, "--parent", "mid", "--session", "kid")
        assert res.exit_code == 1
    res = _tree(tmp_path, "spawn", wide, "--parent", "mid", "--session", "kid")
    assert res.exit_code == 3
    ask = _rows(tmp_path)[-1]
    assert ask["event"] == "ask" and ask["ask_id"] in res.stderr
    doc = json.loads(_tree(tmp_path, "status", "--json").stdout)
    assert [a["ask_id"] for a in doc["asks"]] == [ask["ask_id"]]
    res = _tree(tmp_path, "spawn", wide, "--parent", "mid", "--session", "kid")
    assert res.exit_code == 1 and "An ask for session kid is open." in res.stderr
    res = _tree(tmp_path, "answer", ask["ask_id"], "allow")
    assert res.exit_code == 0, res.output
    doc = json.loads(_tree(tmp_path, "status", "--json").stdout)
    assert doc["asks"] == [] and doc["nodes"][2]["proved"] is False
    assert doc["nodes"][1]["refused"] == 0
    assert "operator allow, not proved" in _tree(tmp_path, "status").stdout
    res = _tree(tmp_path, "answer", ask["ask_id"], "deny")
    assert res.exit_code == 1 and "answered already: allow" in res.stderr


def test_an_operator_allow_starts_no_agent_under_a_physical_parent(tmp_path):
    root_env = _write(tmp_path / "r.json", _env(id="env_root", stakes="physical"))
    kid = _write(tmp_path / "k.json", _env(stakes="physical"))
    assert _tree(tmp_path, "root", root_env, "--session", "arm").exit_code == 0
    for _ in range(4):
        res = _tree(tmp_path, "spawn", kid, "--parent", "arm", "--session", "kid")
    assert res.exit_code == 3
    res = _tree(tmp_path, "answer", _rows(tmp_path)[-1]["ask_id"], "allow")
    assert res.exit_code == 1
    assert "the parent's stakes are physical" in res.stderr
    assert _rows(tmp_path)[-1]["event"] == "ask"


def test_an_operator_allow_never_widens_past_the_root(tmp_path):
    root_env = _write(tmp_path / "r.json", _env(id="env_root"))
    wide = _write(tmp_path / "w.json", _env(shell_allowlist=["rm"]))
    assert _tree(tmp_path, "root", root_env, "--session", "top").exit_code == 0
    for _ in range(4):
        res = _tree(tmp_path, "spawn", wide, "--parent", "top", "--session", "kid")
    assert res.exit_code == 3
    ask = _rows(tmp_path)[-1]["ask_id"]
    res = _tree(tmp_path, "answer", ask, "allow")
    assert res.exit_code == 1
    assert (
        'The child does not fit inside the root top: shell_allowlist: the child adds ["rm"].'
        in (res.stderr)
    )
    assert _rows(tmp_path)[-1]["event"] == "ask"


def test_end_refuses_a_node_with_running_children_and_a_physical_parent_starts_nothing(tmp_path):
    root_env = _write(tmp_path / "r.json", _env(stakes="physical"))
    kid = _write(tmp_path / "k.json", _env(stakes="physical"))
    assert _tree(tmp_path, "root", root_env, "--session", "arm").exit_code == 0
    res = _tree(tmp_path, "spawn", kid, "--parent", "arm", "--session", "kid")
    assert res.exit_code == 1
    assert "stakes: the parent's stakes are physical, so it cannot start an agent" in res.stderr
    ok = _write(tmp_path / "o.json", _env())
    assert _tree(tmp_path, "root", ok, "--session", "top").exit_code == 0
    assert _tree(tmp_path, "spawn", ok, "--parent", "top", "--session", "a").exit_code == 0
    res = _tree(tmp_path, "end", "top")
    assert res.exit_code == 1 and "It has running children: a." in res.stderr


def test_session_ids_and_reuse_are_refused(tmp_path):
    env = _write(tmp_path / "e.json", _env())
    res = _tree(tmp_path, "root", env, "--session", "default")
    assert res.exit_code == 2
    res = _tree(tmp_path, "root", env, "--session", "../x")
    assert res.exit_code == 2
    assert _tree(tmp_path, "root", env, "--session", "top").exit_code == 0
    res = _tree(tmp_path, "root", env, "--session", "top")
    assert res.exit_code == 1 and "in use already" in res.stderr
    assert _tree(tmp_path, "status").stdout.startswith("top (running) tokens -, turns -")


def test_gate_register_with_a_parent_proves_the_edge(tmp_path):
    root = tmp_path / "g"
    parent = _write(tmp_path / "p.json", _env(id="env_p", deadline=50))
    child = _write(tmp_path / "c.json", _env(shell_allowlist=["git"]))
    wide = _write(tmp_path / "w.json", _env(shell_allowlist=["rm"]))
    run = lambda *a: CliRunner().invoke(app, ["gate", "register", *a, "--root", str(root)])  # noqa: E731
    assert run(parent, "--session", "p").exit_code == 0
    res = run(wide, "--session", "c", "--parent", "p")
    assert res.exit_code == 1
    assert res.stderr == (
        'not registered: the edge from p to c is refused: shell_allowlist: the child adds ["rm"]\n'
    )
    res = run(child, "--parent", "p")
    assert res.exit_code == 1 and "--parent needs --session" in res.stderr
    res = run(child, "--session", "c", "--parent", "nobody")
    assert "envelope: the parent has no envelope" in res.stderr
    assert run(child, "--session", "c", "--parent", "p").exit_code == 0
    reg = json.loads((root / "envelopes" / "c.json").read_text())
    assert reg["parent_envelope"] == "env_p" and reg["deadline"] == 50.0
    assert "already" in run(child, "--session", "c", "--parent", "p").stderr
