"""The forall_writes quantifier: each step's write paths in the predicate algebra.

A step's write paths are a file_write step's path and a shell step's
literal write redirects (``write_paths.py``). The quantifier reads them in
evaluation, in the Z3 compiler and in vacuity.
"""

from __future__ import annotations

import pytest

from opendaisugi.aliases import Alias, AliasRegistry
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import compile_to_z3, evaluate_predicate, verify_predicate_z3
from opendaisugi.vacuity import check_vacuity
from opendaisugi.write_paths import step_write_paths


def _env(**kw) -> Envelope:
    perms = {
        "shell": True,
        "shell_allowlist": ["echo", "sh", "cat", "ls"],
        "file_write": ["**"],
        "file_read": ["**"],
        "shell_allow_decomposition": True,
    }
    perms.update(kw.pop("permissions", {}))
    return Envelope(task="t", generated_by="test", permissions=perms, **kw)


def _plan(*steps) -> ActionPlan:
    return ActionPlan(task="t", source="test", steps=list(steps))


def _shell(sid: str, command: str) -> dict:
    return {"id": sid, "type": "shell", "command": command}


def _write(sid: str, path: str) -> dict:
    return {"id": sid, "type": "file_write", "path": path, "content": "x"}


NO_SRC = parse_expression(
    {
        "op": "forall_steps",
        "pred": {
            "op": "forall_writes",
            "pred": {"op": "not_matches", "path": "path", "regex": "^src/"},
        },
    }
)


# --- the write paths --------------------------------------------------------


@pytest.mark.parametrize(
    ("step", "expected"),
    [
        (_write("a", "./src/a.py"), ["src/a.py"]),
        (_write("a", "lib/../src/a.py"), ["src/a.py"]),
        (_shell("a", "ls"), []),
        (_shell("a", "echo hi > out.txt"), ["out.txt"]),
        (_shell("a", "echo hi >> ./out.txt 2>/dev/null"), ["out.txt"]),
        (_shell("a", "echo hi > /dev/null"), []),
        (_shell("a", "sh -c 'echo x > src/a.py'"), ["src/a.py"]),
        (_shell("a", "ls && sh -c 'echo x > a' > b"), ["b", "a"]),
        (_shell("a", "echo x > $OUT"), None),
        (_shell("a", "cp x src/a.py"), ["src/a.py", "src/a.py/x"]),
        ({"id": "a", "type": "network", "url": "https://x.org", "method": "GET"}, []),
    ],
)
def test_step_write_paths(step, expected):
    got = step_write_paths(_plan(step).steps[0].model_dump())
    assert got == expected


def test_write_paths_are_not_in_the_step_record():
    step = _plan(_shell("a", "echo x > out")).steps[0].model_dump()
    assert "write_paths" not in step and "writes" not in step


# --- forall_writes: evaluation and Z3 ---------------------------------------


@pytest.mark.parametrize(
    ("steps", "holds"),
    [
        ([_shell("a", "ls")], True),
        ([_shell("a", "echo x > out/a")], True),
        ([_shell("a", "echo x > src/a")], False),
        ([_shell("a", "echo x > ./src/a")], False),
        ([_write("a", "lib/../src/a.py")], False),
        ([_shell("a", "echo x > $OUT")], False),
        ([_shell("a", "sh -c 'echo x > src/a'")], False),
        ([], True),
    ],
)
def test_forall_writes_in_eval_and_z3_agree(steps, holds):
    plan, env = _plan(*steps), _env()
    assert evaluate_predicate(NO_SRC, plan, env) is holds
    assert verify_predicate_z3(NO_SRC, plan, env)[0] is holds


@pytest.mark.parametrize(
    "expr",
    [
        {"op": "forall_writes", "pred": {"op": "exists", "path": "path"}},
        {
            "op": "forall_outputs",
            "pred": {"op": "forall_writes", "pred": {"op": "exists", "path": "path"}},
        },
        {
            "op": "forall_steps",
            "pred": {
                "op": "forall_writes",
                "pred": {"op": "forall_writes", "pred": {"op": "exists", "path": "path"}},
            },
        },
    ],
)
def test_forall_writes_outside_a_step_is_an_error(expr):
    plan = _plan(_shell("a", "echo x > out"))
    plan.steps[0].metadata["output"] = "o"
    with pytest.raises(ValueError, match="forall_writes"):
        evaluate_predicate(parse_expression(expr), plan, _env())
    with pytest.raises(ValueError, match="forall_writes"):
        compile_to_z3(parse_expression(expr), plan, _env())


def test_forall_writes_is_never_a_contradiction_and_only_a_tautology_when_its_body_is():
    body_false = {
        "op": "and",
        "children": [
            {"op": "equals", "path": "path", "value": "a"},
            {"op": "equals", "path": "path", "value": "b"},
        ],
    }
    assert (
        check_vacuity(parse_expression({"op": "forall_writes", "pred": body_false}))
        == "non_trivial"
    )
    body_true = {
        "op": "or",
        "children": [
            {"op": "matches", "path": "path", "regex": "a"},
            {"op": "not_matches", "path": "path", "regex": "a"},
        ],
    }
    assert (
        check_vacuity(parse_expression({"op": "forall_writes", "pred": body_true})) == "tautology"
    )
    assert check_vacuity(NO_SRC) == "non_trivial"


def test_an_alias_over_forall_writes_registers_and_resolves():
    reg = AliasRegistry()
    reg.register(Alias(name="no_src", expr=NO_SRC.model_dump()))
    out = reg.resolve(parse_expression({"op": "alias", "name": "no_src"}))
    assert out.model_dump() == NO_SRC.model_dump()
