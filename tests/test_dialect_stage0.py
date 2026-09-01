"""Dialect stage 0: the keep_unchanged words, audit first.

- ``glob_regex``: the target glob becomes a regex that matches every path
  the file-scope matcher matches (never fewer).
- ``keep_unchanged`` and its synonyms: one word, proved equal by Z3.
- audit first: a word's would-deny is a warning until a pin enforces it.
"""

from __future__ import annotations

import json
import posixpath
import random
import re

import pytest
import z3

from opendaisugi import dialect
from opendaisugi.conformance import compare_verdict, make_verify_case, serve_lines
from opendaisugi.dialect import (
    AUDIT_PREFIX,
    DIALECT_HASH,
    KEEP_UNCHANGED,
    SYNONYMS,
    UnsupportedGlob,
    glob_regex,
    unfold,
    word_for,
)
from opendaisugi.models import ActionPlan, Envelope, Invariant
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import (
    _compile_scalar,
    _Scope,
)
from opendaisugi.verify import _match_glob, verify


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


# --- the glob translation ---------------------------------------------------

_SEGS = [
    "a",
    "b",
    "ab",
    ".",
    "..",
    "",
    "*",
    "?",
    "**",
    "a*",
    "*b",
    "a?b",
    ".x",
    "*.py",
    "\n",
    "é",
    "+",
    "(",
]
_PSEGS = ["a", "b", "ab", ".", "..", "x.py", "ab.py", ".x", "c", "\n", "é", "+", "(", "*"]


def test_glob_regex_matches_what_the_file_scope_matcher_matches():
    rng = random.Random(7)
    checked = 0
    for _ in range(40_000):
        glob = "/".join(rng.choice(_SEGS) for _ in range(rng.randint(1, 5)))
        if rng.random() < 0.2:
            glob = "/" + glob
        try:
            regex = glob_regex(glob)
        except UnsupportedGlob:
            continue
        path = "/".join(rng.choice(_PSEGS) for _ in range(rng.randint(1, 5)))
        if rng.random() < 0.3:
            path = "/" + path
        path = posixpath.normpath(path)
        assert _match_glob(path, glob) == bool(re.search(regex, path)), (glob, path, regex)
        checked += 1
    assert checked > 30_000


@pytest.mark.parametrize(
    "glob", ["", "[ab]", "a/]", "./**", "a/../**", "**/**/**/**/**/x", "*" * 17, "a" * 1025]
)
def test_glob_regex_refuses(glob):
    with pytest.raises(UnsupportedGlob):
        glob_regex(glob)


def test_glob_regex_shapes():
    assert glob_regex("/**") == "^(?:/[^/]*)+\\Z"
    assert glob_regex("src/**") == "^src(?:/[^/]*)*\\Z"
    assert glob_regex("src/*/**") == "^src/\\*(?:/[^/]*)*\\Z"
    assert glob_regex("*.py") == "^(?:[^/]*\\.py)\\Z"


# --- the words --------------------------------------------------------------


def test_the_dialect_hash_is_pinned():
    # A change to any definition is a new hash. Update this only with the
    # definitions, and say so in the release notes.
    assert DIALECT_HASH == "108a89a256798dfc"
    assert json.loads(dialect.DIALECT_JSON)["words"] == dialect.WORDS


def test_synonyms_name_the_one_word():
    for name in (
        "file_unchanged",
        "read_only",
        "no_modifications",
        "file_immutable",
        "file_preservation",
        KEEP_UNCHANGED,
    ):
        assert word_for(name) == KEEP_UNCHANGED
    assert word_for("no_force_push") is None
    assert word_for("exit_code") is None


@pytest.mark.parametrize("target", ["src/**", "**", "*.py", "/abs/**", "pubspec.yaml"])
def test_every_synonym_unfolds_to_a_term_z3_proves_equal(target):
    base = unfold(KEEP_UNCHANGED, target)
    with_lock = z3.Solver()
    for name in SYNONYMS:
        other = unfold(word_for(name), target)
        a = _compile_scalar(base.pred, _Scope("s", None), [], "s")
        b = _compile_scalar(other.pred, _Scope("s", None), [], "s")
        with_lock.push()
        with_lock.add(a != b)
        assert with_lock.check() == z3.unsat, name
        with_lock.pop()


def test_keep_unchanged_proves_no_write_inside_target():
    # For every write path w: body(w) -> not (w in target). A claim the
    # name makes, proved over a free path. The Z3 regex encoding leaves the
    # newline out of negated classes and knows only the Basic Multilingual
    # Plane, so the proof is over such paths; the concrete check uses re
    # and covers every path.
    body = unfold(KEEP_UNCHANGED, "src/**").pred.pred
    sc = _Scope("c", None)
    term = _compile_scalar(body, sc, [], "c")
    path = sc.vars["c__path"]
    solver = z3.Solver()
    solver.add(term, z3.PrefixOf(z3.StringVal("src/"), path))
    bmp_no_newline = z3.Intersect(z3.Range("\x00", "\uffff"), z3.Complement(z3.Re("\n")))
    solver.add(z3.InRe(path, z3.Star(bmp_no_newline)))
    assert solver.check() == z3.unsat
    solver = z3.Solver()
    solver.add(term, path == z3.StringVal("src"))
    assert solver.check() == z3.unsat


# --- audit first ------------------------------------------------------------


def _inv(type_name, target=None, **kw):
    d = {"type": type_name, "description": "d", **kw}
    if target is not None:
        d["target"] = target
    return d


def test_audit_records_a_would_deny_and_keeps_the_verdict():
    env = _env(invariants=[_inv("file_unchanged", "src/**")])
    plan = _plan(_shell("s1", "sh -c 'echo x > ./src/a.py'"))
    r = verify(plan, env)
    assert r.ok
    assert r.warnings == [
        f"{AUDIT_PREFIX}invariant 'file_unchanged' is keep_unchanged('src/**'); "
        "step 's1' writes 'src/a.py'; enforcing would deny"
    ]


def test_audit_changes_no_strict_verdict():
    env = _env(stakes="high", invariants=[_inv("read_only")])
    r = verify(_plan(_shell("s1", "ls")), env)
    assert not r.ok and r.violations[0].detail["reason"] == "opaque_unrecognized"
    assert r.warnings == []
    r = verify(_plan(_shell("s1", "echo x > out")), env)
    assert [v.detail["reason"] for v in r.violations] == ["opaque_unrecognized"]
    assert r.warnings[0].startswith(AUDIT_PREFIX + "invariant 'read_only' is keep_unchanged('**')")


def test_the_pin_enforces_the_word():
    env = _env(stakes="high", invariants=[_inv("read_only")])
    assert verify(_plan(_shell("s1", "ls")), env, dialect_pin=DIALECT_HASH).ok
    r = verify(_plan(_write("s1", "out")), env, dialect_pin=DIALECT_HASH)
    assert not r.ok
    assert (
        r.violations[0].message
        == "invariant 'read_only' violated: keep_unchanged('**'); step 's1' writes 'out'"
    )
    assert r.violations[0].detail["reason"] == "word_violated"
    assert r.warnings == []


def test_a_pin_this_build_does_not_have_denies_every_word_use():
    env = _env(invariants=[_inv("file_unchanged", "src/**")])
    r = verify(_plan(_shell("s1", "ls")), env, dialect_pin="0000000000000000")
    assert not r.ok and r.violations[0].detail["reason"] == "dialect_pin_mismatch"
    # An envelope with no word is not touched by the pin.
    assert verify(_plan(_shell("s1", "ls")), _env(), dialect_pin="0000000000000000").ok


@pytest.mark.parametrize(
    ("inv", "warned"),
    [
        (_inv("file_unchanged", "src/**", enforce=False), False),
        (_inv("file_unchanged", "src/**", expr={"op": "exists", "path": "id"}), False),
        (_inv("no_force_push"), False),
        (_inv("file_unchanged", "[x]"), True),
    ],
)
def test_what_the_audit_reads(inv, warned):
    r = verify(_plan(_shell("s1", "echo x > src/a")), _env(invariants=[inv]))
    assert any(w.startswith(AUDIT_PREFIX) for w in r.warnings) is warned


def test_a_word_in_a_postcondition_is_not_a_word():
    env = _env(postconditions=[{"type": "file_unchanged", "path": "src/a"}])
    r = verify(_plan(_shell("s1", "echo x > src/a")), env)
    assert r.ok and r.warnings == []


def test_an_unsupported_target_says_why():
    env = _env(invariants=[_inv("file_unchanged", "[x]")])
    r = verify(_plan(_shell("s1", "ls")), env)
    assert "the target is not a supported glob" in r.warnings[0]
    r = verify(_plan(_shell("s1", "ls")), env, dialect_pin=DIALECT_HASH)
    assert not r.ok


# --- conformance ------------------------------------------------------------


def test_a_verify_case_carries_the_word_audit_and_the_oracle_answers_it():
    env = _env(invariants=[_inv("file_unchanged", "src/**")])
    plan = _plan(_shell("s1", "echo x > src/a"))
    r = verify(plan, env)
    case = make_verify_case(plan, env, {"strict": None, "z3_timeout_ms": 500}, r)
    assert case["expect"]["word_audit"] == r.warnings
    verdict = json.loads(next(serve_lines([json.dumps(case)])))
    assert compare_verdict(case, verdict) is None
    assert compare_verdict(case, {**verdict, "word_audit": []}) is not None
    old = json.loads(json.dumps(case))
    del old["expect"]["word_audit"]
    assert compare_verdict(old, {**verdict, "word_audit": []}) is None


def test_a_verify_case_carries_the_pin():
    env = _env(invariants=[_inv("file_unchanged", "src/**")])
    plan = _plan(_shell("s1", "echo x > src/a"))
    r = verify(plan, env, dialect_pin=DIALECT_HASH)
    case = make_verify_case(plan, env, {"dialect_pin": DIALECT_HASH}, r)
    verdict = json.loads(next(serve_lines([json.dumps(case)])))
    assert verdict["ok"] is False and compare_verdict(case, verdict) is None


# --- the gate ---------------------------------------------------------------


def _gate_env(tmp_path, stakes="medium"):
    from opendaisugi.gate import register_envelope, starter_envelope

    root = tmp_path / "data" / "gate"
    env = starter_envelope(tmp_path, stakes=stakes)
    env.invariants = [
        Invariant(type="file_unchanged", target=f"{tmp_path.resolve()}/src/**", description="d")
    ]
    register_envelope(env, root=root)
    return root


def _call(tmp_path, root, path, mode="audit"):
    from opendaisugi.gate import gate_and_contract

    payload = {
        "session_id": "s1",
        "tool_name": "Write",
        "tool_input": {"file_path": str(path), "content": "x"},
        "cwd": str(tmp_path),
    }
    return gate_and_contract(json.dumps(payload).encode(), root=root, mode=mode)


def _log(root):
    return [json.loads(x) for x in (root / "audit" / "s1.jsonl").read_text().splitlines()]


def test_the_gate_logs_a_word_would_deny_and_allows(tmp_path):
    from opendaisugi.gate import audit_report

    root = _gate_env(tmp_path)
    out = _call(tmp_path, root, tmp_path / "src" / "a.py", mode="enforce")
    assert out.exit_code == 0
    _call(tmp_path, root, tmp_path / "doc.md", mode="enforce")
    recs = _log(root)
    assert recs[0]["allow"] is True and recs[0]["word_audit"][0].startswith(AUDIT_PREFIX)
    assert "word_audit" not in recs[1]
    rep = audit_report(root=root)
    assert rep["word_would_deny"] == 1 and rep["would_deny"] == 0
    assert rep["word_denied"] == [recs[0]]


def test_the_gate_enforces_the_word_when_config_pins_the_hash(tmp_path):
    root = _gate_env(tmp_path)
    (root.parent / "config.yaml").write_text(f"dialect_enforce: {DIALECT_HASH}\n")
    out = _call(tmp_path, root, tmp_path / "src" / "a.py", mode="enforce")
    assert out.exit_code == 2
    rec = _log(root)[0]
    assert rec["would_deny"] is True and "keep_unchanged" in rec["reason"]
    assert "word_audit" not in rec


def test_gate_status_and_report_show_the_word_count(tmp_path, monkeypatch):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    root = _gate_env(tmp_path)
    _call(tmp_path, root, tmp_path / "src" / "a.py")
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.chdir(tmp_path)
    runner = CliRunner()
    res = runner.invoke(app, ["gate", "status", "--root", str(root)])
    assert f"dialect: {DIALECT_HASH} · words: audit · word would-denies: 1" in res.output
    res = runner.invoke(app, ["gate", "status", "--root", str(root), "--json"])
    assert json.loads(res.output.splitlines()[0])["dialect"] == {
        "hash": DIALECT_HASH,
        "pin": None,
        "mode": "audit",
        "word_would_deny": 1,
    }
    res = runner.invoke(app, ["gate", "report", "--root", str(root)])
    assert res.output.splitlines()[0].endswith("word_would_deny=1")
    assert "  WORD Write " in res.output
    (root.parent / "config.yaml").write_text('dialect_enforce: "0000"\n')
    res = runner.invoke(app, ["gate", "status", "--root", str(root)])
    assert "pinned 0000, not this build's: every word use denies" in res.output


def test_the_word_count_reads_what_it_can(tmp_path):
    from opendaisugi.gate import audit_report, word_would_deny

    root = tmp_path / "gate"
    (root / "audit").mkdir(parents=True)
    (root / "audit" / "s1.jsonl").write_text(
        '{"tool_name": "Write", "detail": "/w/src/a", "would_deny": false, '
        '"word_audit": ["dialect audit: x"]}\n'
        '{"tool_name": "Write", "would_deny": false, "word_audit": "not a list"}\n'
        "not json\n[1]\n"
    )
    (root / "audit" / "s2.jsonl").write_bytes(b"\xff\xfe")
    (root / "audit" / "s3.jsonl").write_text('{"would_deny": true, "reason": 5}\n')
    assert word_would_deny(root=root) == 1
    # The report itself fails on such a log (a bad UTF-8 file); the count
    # that gate status shows does not.
    with pytest.raises(UnicodeDecodeError):
        audit_report(root=root)
    (root / "audit" / "s2.jsonl").unlink()
    (root / "audit" / "s3.jsonl").unlink()
    rep = audit_report(root=root)
    assert rep["word_would_deny"] == 1 and rep["calls"] == 2
