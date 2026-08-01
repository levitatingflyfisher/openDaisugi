"""The committed semantics fixture must stay in sync with the oracle.

clients/fixtures/semantics.json freezes the oracle's matching semantics for
the multi-client ports. If any of these fail, the oracle's semantics changed:
regenerate the fixture (uv run python clients/fixtures/generate.py), bump the
conformance corpus, and re-run every client — silently drifting instead
would invalidate the differential guarantee.
"""

import json
from pathlib import Path

import pytest

from opendaisugi.interpreter_parse import parse_interpreter
from opendaisugi.models import Envelope, Permission
from opendaisugi.verify import (
    _SHELL_METACHAR_RE,
    _extract_shell_head,
    _head_allowed,
    _path_matches_any,
    resolve_strict,
)

FIXTURE = json.loads(
    (Path(__file__).parent.parent / "clients" / "fixtures" / "semantics.json").read_text()
)


def _ids(section, key):
    return [repr(c[key])[:40] for c in FIXTURE[section]]


@pytest.mark.parametrize("case", FIXTURE["head_allowed"], ids=_ids("head_allowed", "head"))
def test_head_allowed(case):
    assert _head_allowed(case["head"], case["allowlist"]) == case["allowed"]


@pytest.mark.parametrize("case", FIXTURE["path_match"], ids=_ids("path_match", "path"))
def test_path_match(case):
    assert _path_matches_any(case["path"], case["globs"]) == case["matched"]


@pytest.mark.parametrize("case", FIXTURE["extract_head"], ids=_ids("extract_head", "line"))
def test_extract_head(case):
    assert _extract_shell_head(case["line"].strip()) == case["head"]


@pytest.mark.parametrize("case", FIXTURE["metachar"], ids=_ids("metachar", "command"))
def test_metachar(case):
    assert bool(_SHELL_METACHAR_RE.search(case["command"])) == case["hit"]


@pytest.mark.parametrize("case", FIXTURE["interpreter"], ids=_ids("interpreter", "command"))
def test_interpreter(case):
    payload = parse_interpreter(case["command"])
    if case["payload"] is None:
        assert payload is None
    else:
        assert payload is not None
        assert payload.head == case["payload"]["head"]
        assert payload.opaque == case["payload"]["opaque"]
        assert list(payload.inner_commands) == case["payload"]["inner_commands"]


@pytest.mark.parametrize(
    "case",
    FIXTURE["resolve_strict"],
    ids=[f"{c['strict']}-{c['stakes']}" for c in FIXTURE["resolve_strict"]],
)
def test_resolve_strict(case):
    env = Envelope(
        generated_by="fixture", task="t", stakes=case["stakes"], permissions=Permission()
    )
    assert resolve_strict(case["strict"], env) == case["effective"]
