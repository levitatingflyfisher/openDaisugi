"""One format, two implementations, one fixture set.

The phone answers a gate ask by writing the file ask.py reads. If Go's
SafeID and Python's _safe ever disagree about the file stem, the phone
writes an answer nobody ever looks at, and the operator sees a button that
silently does nothing. This is the conformance pattern the verifier clients
already use, applied to a two-line function that would otherwise drift
unnoticed.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi.ask import _safe

FIXTURES = (
    Path(__file__).resolve().parents[1]
    / "harness"
    / "coppice"
    / "internal"
    / "web"
    / "testdata"
    / "safe_id_cases.json"
)

_MISSING_CASE_IN = "__missing_fixture__"


def _load_cases() -> list[dict[str, str]]:
    """Return the fixture cases.

    A missing or broken fixture never raises here. It returns one case
    whose expected value names the fixture path, so the parametrized test
    below fails on that one named case with the path in its message,
    instead of a collection error that hides every case in this file.
    """
    try:
        raw = FIXTURES.read_text(encoding="utf-8")
    except OSError as exc:
        return [{"in": _MISSING_CASE_IN, "out": f"cannot read fixture {FIXTURES}: {exc}"}]
    try:
        cases = json.loads(raw)["cases"]
    except (ValueError, KeyError, TypeError) as exc:
        return [{"in": _MISSING_CASE_IN, "out": f"fixture {FIXTURES} is not valid: {exc}"}]
    if not cases:
        return [{"in": _MISSING_CASE_IN, "out": f"fixture {FIXTURES} has no cases"}]
    if not all(
        isinstance(c, dict) and isinstance(c.get("in"), str) and isinstance(c.get("out"), str)
        for c in cases
    ):
        return [
            {
                "in": _MISSING_CASE_IN,
                "out": f"fixture {FIXTURES} has a case with no in and out strings",
            }
        ]
    return cases


def test_the_fixture_file_exists_and_has_cases():
    assert FIXTURES.exists(), f"missing {FIXTURES}"
    cases = _load_cases()
    assert len(cases) >= 8
    assert all(c["in"] != _MISSING_CASE_IN for c in cases)


@pytest.mark.parametrize("case", _load_cases(), ids=lambda c: repr(c.get("in")))
def test_python_safe_matches_the_go_fixture(case):
    if case["in"] == _MISSING_CASE_IN:
        pytest.fail(case["out"])
    assert _safe(case["in"]) == case["out"]
