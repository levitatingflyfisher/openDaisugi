"""A word placed from the call's cwd: relative targets and relative writes.

At the gate a model's target is mostly relative (``src/**``) while the
call's paths are absolute. ``verify(..., dialect_base=cwd)`` joins a
relative target to the cwd and resolves each relative write path against
it, so both sides are absolute. Hand-written ``forall_writes`` terms are
not placed.
"""

from __future__ import annotations

import pytest

from opendaisugi import dialect
from opendaisugi.dialect import AUDIT_PREFIX, UnsupportedGlob, resolve_target, unfold
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.predicate_z3 import evaluate_predicate
from opendaisugi.verify import verify


def _env(*invariants) -> Envelope:
    perms = {
        "shell": True,
        "shell_allowlist": ["echo", "cp", "ls", "cd", "touch"],
        "file_write": ["**"],
        "file_read": ["**"],
        "shell_allow_decomposition": True,
    }
    return Envelope(task="t", generated_by="test", permissions=perms, invariants=list(invariants))


def _plan(*steps) -> ActionPlan:
    return ActionPlan(task="t", source="test", steps=list(steps))


def _shell(command: str) -> dict:
    return {"id": "s1", "type": "shell", "command": command}


def _write(path: str) -> dict:
    return {"id": "s1", "type": "file_write", "path": path, "content": "x"}


def _inv(type_name: str, target: str | None = None) -> dict:
    d = {"type": type_name, "description": "d"}
    if target is not None:
        d["target"] = target
    return d


def _audit(plan, env, base):
    res = verify(plan, env, dialect_base=base)
    return [w for w in res.warnings if w.startswith(AUDIT_PREFIX)]


@pytest.mark.parametrize(
    ("target", "base", "expected"),
    [
        ("src/**", "/repo", "/repo/src/**"),
        ("*.py", "/repo", "/repo/*.py"),
        ("./**", "/repo", "/repo/./**"),
        ("../lib/**", "/repo/sub", "/repo/sub/../lib/**"),
        ("/abs/**", "/repo", "/abs/**"),
        ("**", "/repo", "**"),
        ("**/*.py", "/repo", "**/*.py"),
        ("src/**", "/", "/src/**"),
        ("src/**", None, "src/**"),
    ],
)
def test_resolve_target(target, base, expected):
    assert resolve_target(target, base) == expected


@pytest.mark.parametrize(
    ("target", "base"),
    [
        ("src/**", "/re*po"),
        ("src/**", "/re[1]po"),
        ("~/x/**", "/repo"),
        ("./src/*.py", "/repo"),
        ("src/../a.py", "/repo"),
    ],
)
def test_resolve_target_refuses(target, base):
    with pytest.raises(UnsupportedGlob):
        resolve_target(target, base)


def test_a_relative_target_meets_an_absolute_write():
    env = _env(_inv("file_unchanged", "src/**"))
    plan = _plan(_write("/repo/src/a.py"))
    assert _audit(plan, env, None) == []
    got = _audit(plan, env, "/repo")
    assert got == [
        AUDIT_PREFIX + "invariant 'file_unchanged' is keep_unchanged('src/**'); "
        "step 's1' writes '/repo/src/a.py'; enforcing would deny"
    ]
    assert _audit(_plan(_write("/repo/out/a.py")), env, "/repo") == []


def test_a_relative_write_meets_an_absolute_target():
    env = _env(_inv("keep_unchanged", "/repo/src/**"))
    plan = _plan(_shell("echo x > src/a.py"))
    assert _audit(plan, env, None) == []
    assert _audit(plan, env, "/repo") == [
        AUDIT_PREFIX + "invariant 'keep_unchanged' is keep_unchanged('/repo/src/**'); "
        "step 's1' writes '/repo/src/a.py'; enforcing would deny"
    ]


def test_an_operand_write_is_seen_and_placed():
    env = _env(_inv("read_only", "src/**"))
    plan = _plan(_shell("cp a.py src/"))
    assert _audit(plan, env, "/repo") == [
        AUDIT_PREFIX + "invariant 'read_only' is keep_unchanged('src/**'); "
        "step 's1' writes '/repo/src'; enforcing would deny"
    ]


def test_a_cd_makes_a_relative_write_unknown():
    env = _env(_inv("read_only", "docs/**"))
    got = _audit(_plan(_shell("cd src && touch a.py")), env, "/repo")
    assert got == [
        AUDIT_PREFIX + "invariant 'read_only' is keep_unchanged('docs/**'); "
        "step 's1' has write paths that cannot be read; enforcing would deny"
    ]


def test_the_default_target_is_not_placed():
    env = _env(_inv("read_only"))
    got = _audit(_plan(_write("/elsewhere/a")), env, "/repo")
    assert got and "'**'" in got[0] and "/elsewhere/a" in got[0]


def test_a_relative_base_places_nothing():
    env = _env(_inv("file_unchanged", "src/**"))
    assert _audit(_plan(_write("/repo/src/a.py")), env, "repo") == []


def test_enforced_under_the_pin():
    env = _env(_inv("file_unchanged", "src/**"))
    res = verify(
        _plan(_write("/repo/src/a.py")),
        env,
        dialect_pin=dialect.DIALECT_HASH,
        dialect_base="/repo/",
    )
    assert not res.ok
    assert res.violations[0].detail["reason"] == "word_violated"


def test_a_hand_written_term_is_not_placed():
    from opendaisugi.predicate import parse_expression

    expr = parse_expression(
        {
            "op": "forall_steps",
            "pred": {
                "op": "forall_writes",
                "pred": {"op": "not_matches", "path": "path", "regex": "^/repo/"},
            },
        }
    )
    assert evaluate_predicate(expr, _plan(_shell("echo > src/a")), _env())
    placed = unfold("keep_unchanged", "src/**", "/repo")
    assert not evaluate_predicate(placed, _plan(_shell("echo > src/a")), _env())
    assert "_base" not in placed.model_dump_json()


def test_the_hash_covers_the_write_paths_version():
    assert '"write_paths":2' in dialect.DIALECT_JSON


def test_the_base_travels_in_the_conformance_options(monkeypatch):
    seen = {}

    def record(plan, envelope, options, result):
        seen.update(options)

    monkeypatch.setattr("opendaisugi.conformance.record_verify", record)
    verify(_plan(_shell("ls")), _env(), dialect_base="/repo//x/")
    assert seen["dialect_base"] == "/repo/x"
    seen.clear()
    verify(_plan(_shell("ls")), _env())
    assert "dialect_base" not in seen
