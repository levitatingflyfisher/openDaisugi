"""The rule regex subset (rule_regex.supported). The fixtures are shared with
the Go and Rust unit tests: ``rule_regex_subset.json`` holds the verdict for
each pattern, ``rule_regex_match.json`` holds what google-re2 answers on probe
strings for each accepted pattern."""

import json
from pathlib import Path

import pytest

from opendaisugi import rule_regex
from opendaisugi.mcp_rules import rule_lint
from opendaisugi.models import McpRule

FIX = Path(__file__).resolve().parents[1] / "clients" / "fixtures"
SUBSET = json.loads((FIX / "rule_regex_subset.json").read_text(encoding="utf-8"))
MATCH = json.loads((FIX / "rule_regex_match.json").read_text(encoding="utf-8"))


def test_the_fixture_has_both_kinds_of_row():
    assert sum(r["ok"] for r in SUBSET) > 100
    assert sum(not r["ok"] for r in SUBSET) > 150


@pytest.mark.parametrize("row", SUBSET, ids=lambda r: repr(r["pattern"])[:40])
def test_the_validator_gives_the_fixture_verdict(row):
    assert rule_regex.supported(row["pattern"]) is row["ok"]
    assert rule_regex.compiles(row["pattern"]) is row["ok"]


def test_the_common_real_patterns_are_accepted():
    for rx in ["^[a-z0-9_-]{1,64}$", "^acme/.+$", "(?i)^readme\\.md$", "^(get|list)_[a-z_]+$"]:
        assert rule_regex.supported(rx), rx


def test_the_f1_and_f2_inputs_are_refused():
    refused = {r["pattern"] for r in SUBSET if not r["ok"]}
    for rx in [
        "(?x)a|.*",
        "\\" + "U00000061",
        "(?u)a",
        "(?R)a",
        "(?-u:a)",
        "[a--b]",
        "[!--/]",
        "[a-c--b]",
        "[\\w--_]",
        "\\p{Ascii}",
        "\\p{ASCII}",
        "\\p{greek}",
        "\\p{Letter}",
        "\\p{Cn}",
        "\\p{gc=Lu}",
        "\\p{Alphabetic}",
    ]:
        assert rx in refused, rx
        assert not rule_regex.supported(rx), rx


def test_a_pattern_with_a_lone_surrogate_is_outside_the_subset():
    assert not rule_regex.supported("a\ud800")
    assert not rule_regex.supported("[\udc00]")


def test_the_refusal_reason_is_one_text():
    assert rule_regex.invalid_reason("(?x)a") == "regex '(?x)a' is outside the supported subset"


@pytest.mark.parametrize("row", MATCH, ids=lambda r: repr(r["pattern"])[:40])
def test_the_engine_answers_the_recorded_matches(row):
    got = "".join("1" if rule_regex.fullmatch(row["pattern"], t) else "0" for t in row["probes"])
    assert got == row["match"]


def test_the_lint_uses_the_same_validator():
    for rx in ["(?x)a|.*", "\\p{Ascii}", "[a--b]"]:
        out = rule_lint(
            [McpRule(tool="t/t", args=[{"op": "matches", "path": "a", "regex": rx}])], {}
        )[0]
        assert out == [f"mcp_rules: t/t: regex '{rx}' is outside the supported subset"]
    ok = rule_lint(
        [McpRule(tool="t/t", args=[{"op": "matches", "path": "a", "regex": "^[a-z]+$"}])], {}
    )[0]
    assert ok == []


def test_the_anchor_lint_reads_the_case_flag_and_suggests_one_inside_the_subset():
    def lint(rx):
        return rule_lint(
            [McpRule(tool="t/t", args=[{"op": "matches", "path": "a", "regex": rx}])], {}
        )[0]

    assert lint("(?i)^readme\\.md$") == []
    assert lint("(?i)readme") == [
        "mcp_rules: t/t: regex '(?i)readme' is not anchored; write (?i)^readme$"
    ]
    assert lint("(?i)a|b") == [
        "mcp_rules: t/t: regex '(?i)a|b' is not anchored; write (?i)^(?:a|b)$"
    ]
    for fix in ("(?i)^readme$", "(?i)^(?:a|b)$"):
        assert rule_regex.supported(fix)
