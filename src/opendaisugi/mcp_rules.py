"""Argument rules for MCP calls (plan w3-mcp-args-and-parked, MP-1 to MP-10).

An envelope can allow an MCP tool only with certain arguments. A rule is a
``server/tool`` pattern plus predicate expressions that see the call's
arguments object. The functions here are pure: the permission stage of
``verify`` calls them, and the Go and Rust ports mirror them message for
message.
"""

from __future__ import annotations

import json
import os
import posixpath
import re
from typing import Any

from opendaisugi import rule_regex
from opendaisugi.models import McpRule, Permission
from opendaisugi.predicate import (
    And,
    Equals,
    Implies,
    InSet,
    Matches,
    Not,
    NotMatches,
    Or,
    PathIn,
)

MAX_MCP_ARGUMENTS_CHARS = 256 * 1024
MAX_MCP_ARGUMENTS_DEPTH = 64

#: Ops an argument rule may use. The step quantifiers, the plan-shape ops,
#: ``alias`` and ``llm_check`` need a plan or a model, not an arguments object.
RULE_OPS = frozenset(
    {
        "equals",
        "not_equals",
        "in_set",
        "not_in_set",
        "matches",
        "not_matches",
        "numeric_range",
        "length_range",
        "exists",
        "is_empty",
        "path_in",
        "and",
        "or",
        "not",
        "implies",
    }
)


#: The owner's ruling (2026-11-03): an MCP server runs in its own working
#: directory, so a relative path argument cannot be placed.
PATH_IN_ABSOLUTE = "path_in takes absolute paths only"

_URI_SCHEME = re.compile(r"[A-Za-z][A-Za-z0-9+.\-]*:")
_PATH_SPACE = " \t\n\r\x0b\x0c"


class ArgScope(dict):
    """The arguments object, with the call's cwd and the envelope's
    permissions beside it. They are attributes, never keys, so an argument
    cannot spoof them."""

    cwd: str | None = None
    perms: Permission | None = None
    #: A rule's ``matches`` and ``not_matches`` test the whole string.
    full_match = True


def place_arg_path(value: str, cwd: str | None) -> str | None:
    """Place one absolute path argument, normalised (``..`` resolved), or None.

    A relative value is never placed: the server's own working directory is
    not the hook's (owner, 2026-11-03), and ``path_in`` refuses it with
    ``PATH_IN_ABSOLUTE`` before it gets here. A value a server could read
    another way cannot be placed either: one with a URI scheme or a Windows
    drive prefix, a backslash, a NUL, or white space at either end. ``cwd``
    is not read.
    """
    if not isinstance(value, str) or not value:
        return None
    if (
        _URI_SCHEME.match(value)
        or "\\" in value
        or "\x00" in value
        or value[0] in _PATH_SPACE
        or value[-1] in _PATH_SPACE
    ):
        return None
    if os.path.isabs(value):
        return posixpath.normpath(value)
    return None


def _walk_paths(expr: Any, out: list[str]) -> None:
    """The ``path`` of every leaf under and/or/not/implies, in order."""
    if isinstance(expr, (And, Or)):
        for child in expr.children:
            _walk_paths(child, out)
    elif isinstance(expr, Not):
        _walk_paths(expr.child, out)
    elif isinstance(expr, Implies):
        _walk_paths(expr.a, out)
        _walk_paths(expr.b, out)
    else:
        path = getattr(expr, "path", None)
        if isinstance(path, str):
            out.append(path)


def known_keys(rule: McpRule) -> set[str]:
    """The top-level argument keys the rule admits."""
    if rule.known is not None:
        return set(rule.known)
    paths: list[str] = []
    for expr in rule.args:
        _walk_paths(expr, paths)
    return {p.split(".", 1)[0] for p in paths}


def _derived_keys(rule: McpRule) -> list[str]:
    paths: list[str] = []
    for expr in rule.args:
        _walk_paths(expr, paths)
    seen: list[str] = []
    for p in paths:
        key = p.split(".", 1)[0]
        if key not in seen:
            seen.append(key)
    return seen


def _describe(expr: Any) -> str:
    path = getattr(expr, "path", None)
    op = getattr(expr, "op", type(expr).__name__)
    if isinstance(path, str):
        return f"{op} on '{path}'"
    return str(op)


def matching_rules(rules: list[McpRule], key: str) -> list[McpRule]:
    """The rules whose ``tool`` pattern matches ``server/tool``."""
    from opendaisugi.verify import _head_allowed

    return [r for r in rules if _head_allowed(key, [r.tool])]


#: The servers whose hosts name an MCP tool ``<server>/<flat name>``: OpenCode
#: joins the MCP server and tool names with ``_``, and pi names an unknown
#: tool as it arrives.
HOST_FLAT_SERVERS = ("opencode", "pi")


def flat_name(text: str) -> str:
    """A host-flattened tool name, compared loosely: ASCII letters lowercased,
    ASCII digits kept, every other character (one code point each) as ``_``."""
    return "".join(c.lower() if c.isascii() and c.isalnum() else "_" for c in text)


def flat_pattern(pattern: str) -> str:
    """A rule's ``server/tool`` pattern in the same form. The glob characters
    ``* ? [ ]`` stay, and inside a ``[...]`` class so do ``-``, ``!`` and ``^``."""
    out: list[str] = []
    in_class = False
    for c in pattern:
        if in_class:
            if c == "]":
                in_class = False
                out.append(c)
            elif c in "-!^":
                out.append(c)
            else:
                out.append(flat_name(c))
        elif c == "[":
            in_class = True
            out.append(c)
        elif c in "*?":
            out.append(c)
        else:
            out.append(flat_name(c))
    return "".join(out)


def host_named_rules(perms: Permission, key: str) -> list[McpRule]:
    """The rules whose pattern names this host-flattened tool once both sides
    are written in the loose form. Empty for any server but ``opencode`` and
    ``pi``. The payload does not split ``opencode/github_get_issue`` into a
    server and a tool, so a rule written for ``github/get_issue`` is found by
    the flat name."""
    from opendaisugi.verify import _head_allowed

    server, _, flat = key.partition("/")
    if server not in HOST_FLAT_SERVERS or not flat:
        return []
    flat = flat_name(flat)
    return [r for r in perms.mcp_rules if _head_allowed(flat, [flat_pattern(r.tool)])]


def ruled_under_host_name(perms: Permission, key: str) -> str | None:
    """The ``server/tool`` pattern of a rule that names this host-flattened
    tool, when only an allowlist entry for another name admits the call.

    It does not let ``opencode/*`` admit a tool a rule governs (None when no
    rule does, or when the allowlist names the tool itself)."""
    if key in perms.mcp_allowlist:
        return None
    found = host_named_rules(perms, key)
    return found[0].tool if found else None


def _too_deep(value: Any, depth: int) -> bool:
    """True when a dict/list nests deeper than the cap. ``depth`` counts the
    containers already entered."""
    if isinstance(value, dict):
        depth += 1
        if depth > MAX_MCP_ARGUMENTS_DEPTH:
            return True
        return any(_too_deep(v, depth) for v in value.values())
    if isinstance(value, list):
        depth += 1
        if depth > MAX_MCP_ARGUMENTS_DEPTH:
            return True
        return any(_too_deep(v, depth) for v in value)
    return False


def check_caps(arguments: Any) -> str | None:
    """MP-10: a size cap, then a depth cap, both counts and not clocks."""
    try:
        size = len(json.dumps(arguments, ensure_ascii=False, separators=(",", ":")))
    except (TypeError, ValueError, RecursionError):
        return f"mcp arguments are nested deeper than {MAX_MCP_ARGUMENTS_DEPTH} levels"
    if size > MAX_MCP_ARGUMENTS_CHARS:
        return (
            f"mcp arguments are longer than {MAX_MCP_ARGUMENTS_CHARS} characters; "
            "the warden does not read them"
        )
    if _too_deep(arguments, 0):
        return f"mcp arguments are nested deeper than {MAX_MCP_ARGUMENTS_DEPTH} levels"
    return None


def evaluate_rules(
    rules: list[McpRule],
    server: str,
    tool: str,
    arguments: Any,
    *,
    perms: Permission,
    cwd: str | None,
    host_rules: list[McpRule] | None = None,
) -> list[str]:
    """The violation messages for one call (empty means admitted).

    ``host_rules`` are rules the flat host name finds (``host_named_rules``);
    they apply after the rules that match the key and add no duplicates.

    Order: caps, "not an object", unknown keys, then each matching rule's
    expressions in order. Rules that do not match ``server/tool`` are ignored.
    """
    from opendaisugi.predicate_z3 import RuleRefused, _eval_scalar

    key = f"{server}/{tool}"
    matching = matching_rules(rules, key)
    if matching:
        for extra in host_rules or []:
            if not any(extra is r for r in matching):
                matching.append(extra)
    prefix = f"MCP tool '{key}': "
    if not matching:
        return []
    capped = check_caps(arguments)
    if capped is not None:
        return [prefix + capped]
    if not isinstance(arguments, dict):
        return [prefix + "arguments are not an object"]
    known: set[str] = set()
    for r in matching:
        known |= known_keys(r)
    out: list[str] = []
    for name in arguments:
        if name not in known:
            out.append(
                prefix
                + f"argument '{name}' is not named by its rule (known: {', '.join(sorted(known))})"
            )
    if out:
        return out
    scope = ArgScope(arguments)
    scope.cwd = cwd
    scope.perms = perms
    for r in matching:
        for expr in r.args:
            bad = [rx for rx in rule_regexes(expr) if not rule_regex.compiles(rx)]
            if bad:
                out.append(prefix + f"argument rule refused: {rule_regex.invalid_reason(bad[0])}")
                continue
            try:
                ok = _eval_scalar(expr, scope)
            except RuleRefused as exc:
                out.append(prefix + str(exc))
                continue
            except Exception:  # noqa: BLE001 - a rule that cannot run denies
                out.append(prefix + f"argument rule could not be evaluated: {_describe(expr)}")
                continue
            if not ok:
                out.append(prefix + f"argument rule does not hold: {_describe(expr)}")
    return out


def path_shaped_strings(arguments: Any, home: str | None) -> list[tuple[str, str]]:
    """``(dotted name, expanded path)`` for each string argument at any depth
    that starts with ``/`` or ``~/``. Objects walk in key order, lists by
    index (``paths[1]``). A ``~/`` string with no ``home`` is left out."""
    out: list[tuple[str, str]] = []

    def walk(value: Any, name: str) -> None:
        if isinstance(value, str):
            if value.startswith("/"):
                out.append((name, value))
            elif value.startswith("~/") and home:
                out.append((name, home.rstrip("/") + "/" + value[2:]))
        elif isinstance(value, dict):
            for k, v in value.items():
                walk(v, f"{name}.{k}" if name else str(k))
        elif isinstance(value, list):
            for i, v in enumerate(value):
                walk(v, f"{name}[{i}]")

    walk(arguments, "")
    return out


def _literal_tool(pattern: str) -> tuple[str, str] | None:
    if any(c in pattern for c in "*?[") or "/" not in pattern:
        return None
    server, tool = pattern.split("/", 1)
    return server, tool


def _top_level_alternation(regex: str) -> bool:
    """True when a ``|`` stands outside every group and character class."""
    depth = 0
    in_class = False
    i = 0
    while i < len(regex):
        c = regex[i]
        if c == "\\":
            i += 2
            continue
        if in_class:
            if c == "]":
                in_class = False
        elif c == "[":
            in_class = True
            if regex[i + 1 : i + 2] == "^":
                i += 1
            if regex[i + 1 : i + 2] == "]":
                i += 1
        elif c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
        elif c == "|" and depth == 0:
            return True
        i += 1
    return False


#: A rule-checked string longer than this denies. RE2 runs in linear time, but
#: the hook has a budget, and no rule needs more.
MAX_RULE_STRING_CHARS = 4096


def rule_regexes(expr: Any) -> list[str]:
    """The regex of every ``matches`` and ``not_matches`` under ``expr``, in order."""
    out: list[str] = []

    def walk(e: Any) -> None:
        if isinstance(e, (Matches, NotMatches)):
            out.append(e.regex)
        elif isinstance(e, (And, Or)):
            for child in e.children:
                walk(child)
        elif isinstance(e, Not):
            walk(e.child)
        elif isinstance(e, Implies):
            walk(e.a)
            walk(e.b)

    walk(expr)
    return out


def _split_flag(regex: str) -> tuple[str, str]:
    """A leading ``(?i)`` (the one flag the subset allows) and the rest."""
    return ("(?i)", regex[4:]) if regex.startswith("(?i)") else ("", regex)


def _anchored(regex: str) -> bool:
    _, body = _split_flag(regex)
    return body.startswith("^") and body.endswith("$") and not _top_level_alternation(body)


def _lint_exprs(rule: McpRule, refusals: list[str], perms: Permission | None = None) -> None:
    def walk(expr: Any) -> None:
        op = getattr(expr, "op", "")
        if op not in RULE_OPS:
            refusals.append(f"mcp_rules: {rule.tool}: op '{op}' cannot be used in a rule")
            return
        if isinstance(expr, PathIn) and perms is not None:
            for glob in getattr(perms, expr.scope):
                if not glob.startswith("/"):
                    refusals.append(
                        f"mcp_rules: {rule.tool}: path_in on '{expr.path}' reads {expr.scope}, "
                        f"which holds the relative glob '{glob}'. {PATH_IN_ABSOLUTE}: "
                        "write the glob from /"
                    )
        if isinstance(expr, (Matches, NotMatches)) and not rule_regex.compiles(expr.regex):
            refusals.append(f"mcp_rules: {rule.tool}: {rule_regex.invalid_reason(expr.regex)}")
        elif isinstance(expr, (Matches, NotMatches)):
            if not _anchored(expr.regex):
                flag, body = _split_flag(expr.regex)
                if _top_level_alternation(body):
                    fix = f"{flag}^(?:{body})$"
                else:
                    fix = flag + ("" if body.startswith("^") else "^") + body
                    fix += "" if body.endswith("$") else "$"
                refusals.append(
                    f"mcp_rules: {rule.tool}: regex '{expr.regex}' is not anchored; write {fix}"
                )
        if isinstance(expr, (And, Or)):
            for child in expr.children:
                walk(child)
        elif isinstance(expr, Not):
            walk(expr.child)
        elif isinstance(expr, Implies):
            walk(expr.a)
            walk(expr.b)

    for expr in rule.args:
        walk(expr)


def _enum_values(expr: Any) -> list[tuple[str, Any]]:
    """``(top-level path, value)`` for equals/in_set under the top-level
    expressions and their ``and`` children. Other shapes may be negated, so
    they do not say the rule can never pass."""
    out: list[tuple[str, Any]] = []

    def walk(e: Any) -> None:
        if isinstance(e, And):
            for c in e.children:
                walk(c)
        elif isinstance(e, Equals) and "." not in e.path:
            out.append((e.path, e.value))
        elif isinstance(e, InSet) and "." not in e.path:
            for v in e.values:
                out.append((e.path, v))

    walk(expr)
    return out


def rule_lint(
    rules: list[McpRule], schemas: dict[str, dict], perms: Permission | None = None
) -> tuple[list[str], list[str]]:
    """``(refusals, notes)`` for registration (MP-5, MP-8). With ``perms``, a
    ``path_in`` whose scope holds a relative glob is refused too."""
    refusals: list[str] = []
    notes: list[str] = []
    for rule in rules:
        _lint_exprs(rule, refusals, perms)
        lit = _literal_tool(rule.tool)
        if lit is None:
            continue
        server, tool = lit
        schema = schemas.get(server)
        if schema is None:
            note = (
                f"mcp_rules: no schema on file for {server}; "
                "argument names are not checked (daisugi warden schemas import)"
            )
            if note not in notes:
                notes.append(note)
            continue
        tools = {t.get("name"): t for t in schema.get("tools", []) if isinstance(t, dict)}
        if tool not in tools:
            have = ", ".join(sorted(str(n) for n in tools))
            refusals.append(
                f"mcp_rules: {rule.tool}: the schema of {server} has no tool '{tool}'; "
                f"it has: {have}"
            )
            continue
        input_schema = tools[tool].get("inputSchema")
        props = input_schema.get("properties", {}) if isinstance(input_schema, dict) else {}
        if not isinstance(props, dict):
            props = {}
        names = list(rule.known or [])
        for k in _derived_keys(rule):
            if k not in names:
                names.append(k)
        for name in names:
            if name not in props:
                refusals.append(
                    f"mcp_rules: {rule.tool} names argument '{name}'; "
                    f"the schema has: {', '.join(sorted(props))}"
                )
        for expr in rule.args:
            for path, value in _enum_values(expr):
                prop = props.get(path)
                enum = prop.get("enum") if isinstance(prop, dict) else None
                if isinstance(enum, list) and not any(
                    type(value) is type(e) and value == e for e in enum
                ):
                    shown = ", ".join(str(e) for e in enum)
                    refusals.append(
                        f"mcp_rules: {rule.tool}: value '{value}' for argument '{path}' is not "
                        f"in the schema enum ({shown}); the rule can never pass"
                    )
    return refusals, notes
