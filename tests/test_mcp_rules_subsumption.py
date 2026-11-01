from opendaisugi.models import McpRule, Permission
from opendaisugi.subsumption import _mcp_rules_subsume


def P(**k):
    return Permission(**k)


R = lambda tool, *args, known=None: McpRule(tool=tool, args=list(args), known=known)  # noqa: E731


def test_inner_rule_narrower_than_outer_allowlist_is_subsumed():
    outer = P(mcp_allowlist=["github/*"])
    inner = P(
        mcp_rules=[R("github/get_issue", {"op": "in_set", "path": "repo", "values": ["acme/app"]})]
    )
    assert _mcp_rules_subsume(inner, outer, timeout_ms=2000) is None


def test_inner_allowlist_wider_than_outer_rule_is_not_subsumed():
    outer = P(
        mcp_rules=[R("github/get_issue", {"op": "in_set", "path": "repo", "values": ["acme/app"]})]
    )
    inner = P(mcp_allowlist=["github/get_issue"])
    assert "github/get_issue" in _mcp_rules_subsume(inner, outer, timeout_ms=2000)


def test_inner_values_subset_is_subsumed_and_superset_is_not():
    outer = P(
        mcp_rules=[
            R(
                "github/get_issue",
                {"op": "in_set", "path": "repo", "values": ["acme/app", "acme/web"]},
            )
        ]
    )
    inner_ok = P(
        mcp_rules=[R("github/get_issue", {"op": "in_set", "path": "repo", "values": ["acme/app"]})]
    )
    inner_bad = P(
        mcp_rules=[
            R(
                "github/get_issue",
                {"op": "in_set", "path": "repo", "values": ["acme/app", "evil/x"]},
            )
        ]
    )
    assert _mcp_rules_subsume(inner_ok, outer, timeout_ms=2000) is None
    assert _mcp_rules_subsume(inner_bad, outer, timeout_ms=2000)


def test_inner_known_superset_is_not_subsumed():
    outer = P(mcp_rules=[R("t/t", known=["a"])])
    inner = P(mcp_rules=[R("t/t", known=["a", "b"])])
    assert "known" in _mcp_rules_subsume(inner, outer, timeout_ms=2000)


def test_path_in_scope_compares_by_glob_subsumption():
    outer = P(
        file_read=["/work/**"],
        mcp_rules=[R("fs/read_file", {"op": "path_in", "path": "path", "scope": "file_read"})],
    )
    inner = P(
        file_read=["/work/src/**"],
        mcp_rules=[R("fs/read_file", {"op": "path_in", "path": "path", "scope": "file_read"})],
    )
    wider = P(
        file_read=["/**"],
        mcp_rules=[R("fs/read_file", {"op": "path_in", "path": "path", "scope": "file_read"})],
    )
    assert _mcp_rules_subsume(inner, outer, timeout_ms=2000) is None
    assert _mcp_rules_subsume(wider, outer, timeout_ms=2000)
