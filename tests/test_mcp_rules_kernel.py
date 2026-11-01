import pytest

from opendaisugi import rule_regex
from opendaisugi.mcp_rules import evaluate_rules, known_keys, place_arg_path, rule_lint
from opendaisugi.models import McpRule, Permission
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import _eval_scalar

PERMS = Permission(file_read=["/work/**"], file_write=["/work/out/**"])


def rule(tool, *args, known=None):
    return McpRule(tool=tool, args=list(args), known=known)


def run(rules, server, tool, arguments, cwd="/work"):
    return evaluate_rules(rules, server, tool, arguments, perms=PERMS, cwd=cwd)


def test_in_set_rule_admits_and_denies():
    r = [rule("github/get_issue", {"op": "in_set", "path": "repo", "values": ["acme/app"]})]
    assert run(r, "github", "get_issue", {"repo": "acme/app"}) == []
    out = run(r, "github", "get_issue", {"repo": "evil/app"})
    assert out == ["MCP tool 'github/get_issue': argument rule does not hold: in_set on 'repo'"]


def test_unknown_top_level_key_denies():
    r = [rule("github/get_issue", {"op": "in_set", "path": "repo", "values": ["acme/app"]})]
    out = run(r, "github", "get_issue", {"repo": "acme/app", "body": "x"})
    assert out == [
        "MCP tool 'github/get_issue': argument 'body' is not named by its rule (known: repo)"
    ]


def test_nested_smuggle_under_an_unnamed_key_denies():
    r = [rule("github/get_issue", {"op": "numeric_range", "path": "number", "min": 1, "max": 9})]
    out = run(r, "github", "get_issue", {"number": 3, "opts": {"path": "/etc/shadow"}})
    assert out and "argument 'opts' is not named" in out[0]


def test_known_list_overrides_derived_keys():
    r = [rule("github/get_issue", known=["repo", "number"])]
    assert run(r, "github", "get_issue", {"number": 1}) == []
    assert run(r, "github", "get_issue", {"x": 1})


def test_known_keys_walks_children():
    r = rule(
        "t/t",
        {
            "op": "and",
            "children": [
                {"op": "exists", "path": "a"},
                {"op": "not", "child": {"op": "exists", "path": "b.c"}},
            ],
        },
    )
    assert known_keys(r) == {"a", "b"}


def test_arguments_not_an_object_denies_under_a_rule():
    r = [rule("github/get_issue", known=["repo"])]
    assert run(r, "github", "get_issue", ["acme/app"]) == [
        "MCP tool 'github/get_issue': arguments are not an object"
    ]


def test_a_tool_no_rule_names_is_not_decided_here():
    r = [rule("github/get_issue", known=["repo"])]
    assert run(r, "slack", "post", {"anything": 1}) == []


@pytest.mark.parametrize(
    "value,cwd,want",
    [
        ("/work/a", "/work", "/work/a"),
        ("src/a.py", "/work", None),  # relative: never placed (owner, 2026-11-03)
        ("../etc/passwd", "/work/sub", None),
        ("a/b/../c", "/work", None),
        ("file:///etc/passwd", "/work", None),
        ("C:/x", "/work", None),
        ("a\\b", "/work", None),
        (" /a", "/work", None),
        ("/a\n", "/work", None),
        ("/work/../etc/passwd", "/work", "/etc/passwd"),
        ("~/.ssh/id", "/work", None),
        ("$HOME/x", "/work", None),
        ("rel", None, None),
        ("rel", "relative-cwd", None),
        ("", "/work", None),
    ],
)
def test_place_arg_path(value, cwd, want):
    assert place_arg_path(value, cwd) == want


def test_path_in_scalar_and_list():
    r = [rule("fs/read_file", {"op": "path_in", "path": "path", "scope": "file_read"})]
    assert run(r, "fs", "read_file", {"path": "/work/a"}) == []
    assert run(r, "fs", "read_file", {"path": "/etc/passwd"})
    assert run(r, "fs", "read_file", {"path": 5})
    assert run(r, "fs", "read_file", {"path": {"a": "/work/a"}})
    assert run(r, "fs", "read_file", {"path": []})
    assert run(r, "fs", "read_file", {"path": ""})
    many = [rule("fs/read_many", {"op": "path_in", "path": "paths", "scope": "file_read"})]
    assert run(many, "fs", "read_many", {"paths": ["/work/a", "/work/b"]}) == []
    assert run(many, "fs", "read_many", {"paths": ["/work/a", "/home/u/.aws/c"]})
    assert run(many, "fs", "read_many", {"paths": ["/work/a", 3]})


def test_path_in_write_scope_uses_file_write():
    r = [rule("fs/write_file", {"op": "path_in", "path": "path", "scope": "file_write"})]
    assert run(r, "fs", "write_file", {"path": "/work/out/a"}) == []
    assert run(r, "fs", "write_file", {"path": "/work/a"})


def test_an_argument_cannot_spoof_the_scope():
    r = [
        rule(
            "fs/read_file",
            {"op": "path_in", "path": "path", "scope": "file_read"},
            known=["path", "perms"],
        )
    ]
    assert run(r, "fs", "read_file", {"path": "/etc/passwd", "perms": {"file_read": ["/**"]}})


def test_bool_is_not_a_number_in_equals_and_in_set():
    def ev(d, scope):
        return _eval_scalar(parse_expression(d), scope)

    assert ev({"op": "in_set", "path": "n", "values": [1]}, {"n": True}) is False
    assert ev({"op": "equals", "path": "n", "value": True}, {"n": 1}) is False
    assert ev({"op": "not_in_set", "path": "n", "values": [1]}, {"n": True}) is True
    assert ev({"op": "not_equals", "path": "n", "value": 1}, {"n": True}) is True
    assert ev({"op": "in_set", "path": "n", "values": [1]}, {"n": 1}) is True
    assert ev({"op": "equals", "path": "n", "value": True}, {"n": True}) is True
    assert ev({"op": "numeric_range", "path": "n", "min": 0, "max": 2}, {"n": True}) is False


def test_caps_are_counts():
    r = [rule("t/t", known=["a"])]
    big = {"a": "x" * (256 * 1024)}
    assert run(r, "t", "t", big) == [
        "MCP tool 't/t': mcp arguments are longer than 262144 characters; "
        "the warden does not read them"
    ]
    deep = {"a": None}
    cur = deep
    for _ in range(65):
        cur["a"] = {"a": None}
        cur = cur["a"]
    assert "nested deeper than 64 levels" in run(r, "t", "t", deep)[0]
    ok = {"a": None}
    cur = ok
    for _ in range(62):
        cur["a"] = {"a": None}
        cur = cur["a"]
    assert run(r, "t", "t", ok) == []


def test_lint_refuses_unanchored_regex_and_unknown_schema_name():
    rules = [rule("github/get_issue", {"op": "matches", "path": "repo", "regex": "acme/.*"})]
    refusals, notes = rule_lint(rules, {})
    assert refusals == [
        "mcp_rules: github/get_issue: regex 'acme/.*' is not anchored; write ^acme/.*$"
    ]
    assert notes == [
        "mcp_rules: no schema on file for github; "
        "argument names are not checked (daisugi warden schemas import)"
    ]
    schema = {
        "tools": [
            {
                "name": "get_issue",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "repo": {"type": "string"},
                        "number": {"type": "integer"},
                        "state": {"enum": ["open", "closed"]},
                    },
                },
            }
        ]
    }
    rules = [rule("github/get_issue", {"op": "equals", "path": "repo_name", "value": "x"})]
    assert rule_lint(rules, {"github": schema})[0] == [
        "mcp_rules: github/get_issue names argument 'repo_name'; "
        "the schema has: number, repo, state"
    ]
    rules = [
        rule("github/get_issue", {"op": "in_set", "path": "state", "values": ["open", "merged"]})
    ]
    assert "merged" in rule_lint(rules, {"github": schema})[0][0]
    rules = [rule("github/*", {"op": "exists", "path": "anything"})]
    assert rule_lint(rules, {"github": schema}) == ([], [])
    rules = [rule("github/get_issue", {"op": "llm_check", "rule": "x"})]
    assert "cannot be used in a rule" in rule_lint(rules, {"github": schema})[0][0]


def test_permission_round_trips_mcp_rules():
    p = Permission.model_validate(
        {"mcp_rules": [{"tool": "a/b", "args": [{"op": "exists", "path": "x"}]}]}
    )
    assert p.mcp_rules[0].args[0].op == "exists"
    assert Permission.model_validate(p.model_dump()).mcp_rules[0].tool == "a/b"
    with pytest.raises(ValueError):
        Permission.model_validate(
            {
                "mcp_rules": [
                    {"tool": "a/b", "args": [{"op": "path_in", "path": "x", "scope": "network"}]}
                ]
            }
        )


# ---- fix round 2: RE2 engine, 4096 cap -------------------------------------


def _lint(rx):
    r = [rule("t/t", {"op": "matches", "path": "a", "regex": rx})]
    return rule_lint(r, {})[0]


# Outside the subset: a stray ")", an open "(", lookahead, lookbehind, a
# backreference, a repeat count over 1000, flags, \\p names (F1, F2).
UNCOMPILABLE = [
    "^a)|(b$",
    "^(a$",
    "^(?=a)a$",
    "^(?!a).*$",
    "^(?<=a)b$",
    "^(a)\\1$",
    "^a{1001}$",
    "(?x)a|.*",
    "[!--/]",
    "\\p{Ascii}",
    "\\p{Letter}",
]


@pytest.mark.parametrize("rx", UNCOMPILABLE)
def test_lint_and_evaluation_refuse_a_pattern_re2_cannot_compile(rx):
    assert _lint(rx) == [f"mcp_rules: t/t: regex '{rx}' is outside the supported subset"]
    r = [rule("t/t", {"op": "matches", "path": "a", "regex": rx})]
    assert run(r, "t", "t", {"a": "aEVIL"}) == [
        f"MCP tool 't/t': argument rule refused: regex '{rx}' is outside the supported subset"
    ]
    nm = [rule("t/t", {"op": "not_matches", "path": "a", "regex": rx})]
    assert run(nm, "t", "t", {"a": "aEVIL"}) == [
        f"MCP tool 't/t': argument rule refused: regex '{rx}' is outside the supported subset"
    ]


@pytest.mark.parametrize(
    "rx",
    ["^[a-z0-9_-]{1,64}$", "^(foo|bar)+$", "^([a-z]+-)?[a-z]+$", "^(?:ab|cd){2}$", "^a+b*c?$"],
)
def test_a_benign_pattern_loads_and_runs(rx):
    assert _lint(rx) == []
    r = [rule("t/t", {"op": "matches", "path": "a", "regex": rx})]
    assert run(r, "t", "t", {"a": "foobar"}) in (
        [],
        ["MCP tool 't/t': argument rule does not hold: matches on 'a'"],
    )


@pytest.mark.parametrize("rx", ["^(a|aa)+$", "^(a+)+$", "^(.*)*$", "^a*a*a*a*b$"])
def test_old_hanging_patterns_answer_within_a_second_at_4096_characters(rx):
    import time

    for text in ("a" * 4096, "a" * 4095 + "!"):
        r = [rule("t/t", {"op": "matches", "path": "a", "regex": rx})]
        t0 = time.monotonic()
        run(r, "t", "t", {"a": text})
        assert time.monotonic() - t0 < 1.0


def test_a_string_over_4096_characters_denies_with_its_own_reason():
    r = [rule("t/t", {"op": "matches", "path": "a", "regex": "^[a-z]+$"})]
    assert run(r, "t", "t", {"a": "a" * 4096}) == []
    out = run(r, "t", "t", {"a": "a" * 4097})
    assert out == [
        "MCP tool 't/t': argument rule refused: argument 'a' is longer than 4096 characters"
    ]
    nm = [rule("t/t", {"op": "not_matches", "path": "a", "regex": "^x$"})]
    assert run(nm, "t", "t", {"a": "a" * 5000})


def test_a_subset_pattern_is_not_rewritten_before_the_engine_reads_it():
    assert rule_regex.fullmatch("^a$", "a")
    assert not rule_regex.fullmatch("a", "ab")
    assert not hasattr(rule_regex, "bridge")


@pytest.mark.parametrize("bad", ["\ud800", "x\udfffy", "\udc00"])
def test_a_value_with_a_lone_surrogate_denies_before_matching(bad):
    expect = ["MCP tool 't/t': argument rule refused: argument 'a' is not valid UTF-8 text"]
    for op in ("matches", "not_matches"):
        r = [
            rule("t/t", {"op": op, "path": "a", "regex": "^evil$" if op == "not_matches" else ".*"})
        ]
        assert run(r, "t", "t", {"a": bad}) == expect, op


def test_a_benign_neighbour_of_the_surrogate_case_still_matches():
    r = [rule("t/t", {"op": "not_matches", "path": "a", "regex": "^evil$"})]
    assert run(r, "t", "t", {"a": "good\ufffd"}) == []
    assert run(r, "t", "t", {"a": "evil"}) == [
        "MCP tool 't/t': argument rule does not hold: not_matches on 'a'"
    ]


def test_flat_name_keeps_ascii_alphanumerics_lowercased_and_maps_the_rest_to_underscores():
    from opendaisugi.mcp_rules import flat_name

    assert flat_name("GitHub-Get.Issue") == "github_get_issue"
    assert flat_name("a1B2") == "a1b2"
    assert flat_name("caf\u00e9") == "caf_"  # one underscore per code point
    assert flat_name("\U0001f600x") == "_x"
    assert flat_name("a__b") == "a__b"  # runs stay
    assert flat_name("") == ""


def test_flat_pattern_keeps_glob_characters_and_class_marks():
    from opendaisugi.mcp_rules import flat_pattern

    assert flat_pattern("github/get_issue") == "github_get_issue"
    assert flat_pattern("opencode/*") == "opencode_*"
    assert flat_pattern("a/b?c") == "a_b?c"
    assert flat_pattern("x/[a-c]y") == "x_[a-c]y"
    assert flat_pattern("x/[!a-c]") == "x_[!a-c]"
    assert flat_pattern("x/[^A.]") == "x_[^a_]"
    assert flat_pattern("Git-Hub") == "git_hub"


def test_host_named_rules_find_a_rule_by_the_flat_name_for_opencode_and_pi_only():
    from opendaisugi.mcp_rules import host_named_rules

    perms = Permission(
        mcp_allowlist=["opencode/github_get_issue"],
        mcp_rules=[
            rule("github/get_issue", {"op": "exists", "path": "n"}),
            rule("github/list_*", {"op": "exists", "path": "n"}),
            rule("other/x", {"op": "exists", "path": "n"}),
        ],
    )
    found = host_named_rules(perms, "opencode/github_get_issue")
    assert [r.tool for r in found] == ["github/get_issue"]
    assert [r.tool for r in host_named_rules(perms, "pi/GitHub-Get.Issue")] == ["github/get_issue"]
    assert [r.tool for r in host_named_rules(perms, "opencode/github_list_pulls")] == [
        "github/list_*"
    ]
    # Another server is never read loosely.
    assert host_named_rules(perms, "github/get_issue") == []
    assert host_named_rules(perms, "claude/github_get_issue") == []
    # No tool part, no match.
    assert host_named_rules(perms, "opencode/") == []
    assert host_named_rules(perms, "opencode/unknown_tool") == []
