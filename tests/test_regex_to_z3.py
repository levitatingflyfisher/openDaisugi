"""Tests for the Python-re → Z3 regex translator (v0.11.0)."""

from __future__ import annotations

import re

import pytest
import z3

from opendaisugi.regex_to_z3 import UnsupportedRegexError, translate


def _matches_via_z3(pattern: str, s: str) -> bool:
    solver = z3.Solver()
    solver.add(z3.InRe(z3.StringVal(s), translate(pattern)))
    return solver.check() == z3.sat


@pytest.mark.parametrize(
    ("pattern", "s"),
    [
        (r"rm ", "rm -rf /"),
        (r"^rm ", "rm -rf /"),
        (r"AKIA[0-9A-Z]{16}", "AKIAIOSFODNN7EXAMPLE"),
        (r"[a-z]+", "hello"),
        (r"a|b", "b"),
        (r"ab?c", "ac"),
        (r"ab?c", "abc"),
        (r"a+b", "aaab"),
        (r"^hello world$", "hello world"),
        (r"drop table", "please drop table users"),
        (r"[0-9]{3}-[0-9]{2}-[0-9]{4}", "ssn: 123-45-6789 leak"),
    ],
)
def test_z3_matches_where_python_matches(pattern, s):
    assert re.search(pattern, s) is not None
    assert _matches_via_z3(pattern, s)


@pytest.mark.parametrize(
    ("pattern", "s"),
    [
        (r"^rm ", " rm x"),
        (r"AKIA[0-9A-Z]{16}", "nothing here"),
        (r"[a-z]+", "ABC"),
        (r"^hello$", "hello world"),
        (r"drop table", "just drop"),
        (r"[0-9]{3}-[0-9]{2}-[0-9]{4}", "no ssn here"),
    ],
)
def test_z3_rejects_where_python_rejects(pattern, s):
    assert re.search(pattern, s) is None
    assert not _matches_via_z3(pattern, s)


def test_unsupported_features_raise():
    for bad in [
        r"(?i)hello",        # inline flag
        r"\bword\b",         # word boundary
        r"(?=hello)",        # lookahead
        r"(?!secret)",       # negative lookahead
        r"(\w+)\1",          # backreference
    ]:
        with pytest.raises(UnsupportedRegexError):
            translate(bad)


def test_anchored_full_match_roundtrip():
    pat = translate(r"^echo$")
    s = z3.String("s")
    solver = z3.Solver()
    solver.add(z3.InRe(s, pat))
    # Only "echo" satisfies — with another constraint we can confirm.
    solver.add(s != z3.StringVal("echo"))
    # Should still be sat because many strings could fit? No — anchored
    # ^echo$ matches only "echo". Expect unsat.
    assert solver.check() == z3.unsat


# v0.28.4 — H2 soundness regression. Pre-fix the alphabet was
# Range(0x20, 0x7e); a Python regex like `.+` admits non-ASCII (`é`) but
# the Z3 translation didn't. Subsumption against an outer that explicitly
# bounded ASCII (`[ -~]+`) returned `unsat` ("subsumed") while reality
# had `é` as a counterexample. v0.28.4 widens to BMP (0x00-0xFFFF) minus
# newline so `.` matches Python's "any non-newline" semantics.


def test_any_char_admits_non_ascii():
    """v0.28.4: ``.`` admits BMP non-ASCII (Latin extended, CJK, etc.).
    Pre-fix the printable-ASCII alphabet rejected anything outside 0x20-0x7e.
    """
    pat = translate(r".")
    s = z3.String("s")
    for ch in ("é", "中", "α"):
        solver = z3.Solver()
        solver.set("timeout", 5000)
        solver.add(z3.InRe(z3.StringVal(ch), pat))
        assert solver.check() == z3.sat, f"`.` must admit {ch!r}"


def test_any_char_rejects_newline():
    """v0.28.4: ``.`` still excludes ``\\n`` (Python default semantics)."""
    pat = translate(r".")
    solver = z3.Solver()
    solver.set("timeout", 5000)
    solver.add(z3.InRe(z3.StringVal("\n"), pat))
    assert solver.check() == z3.unsat


def test_h2_non_ascii_subsumption_unsoundness_closed():
    """v0.28.4: H2 unsoundness was that outer=``[ -~]+`` would subsume
    inner=``.+`` despite Python's ``.+`` admitting non-ASCII strings the
    outer rejects. Post-fix, Z3's ``.+`` admits non-ASCII, so the
    counterexample exists for Z3 too.
    """
    inner = translate(r".+")
    outer = translate(r"[ -~]+")
    s = z3.String("cmd")
    solver = z3.Solver()
    solver.set("timeout", 30000)
    solver.add(z3.InRe(s, inner))
    solver.add(z3.Not(z3.InRe(s, outer)))
    res = solver.check()
    # Either Z3 finds a non-ASCII counterexample (sat) OR it times out
    # (unknown — fails closed via VerificationTimeout). What it MUST NOT
    # return is `unsat` ("subsumed") — that was the unsoundness.
    assert res != z3.unsat, (
        "subsumption must NOT approve `.+` ⊆ `[ -~]+`; reality has non-ASCII counterexamples"
    )
