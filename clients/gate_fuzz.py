"""Seeded fuzz generators for the gate comparison (gate_compare.py --fuzz).

Each generator gives a list of items built from a fixed seed, so a run can
be repeated exactly. An item is one call: the envelope registered as the
default (a dict, or raw text), and the hook payload.

- shell: single-line compound commands against three envelopes.
- heredoc: heredocs and here-strings, with bodies that hold operators,
  substitutions and quotes, closed and unclosed.
- multiline: several lines, comments, line continuations and CR LF.
- unicode: non-ASCII text in commands, paths and URLs, lone surrogates
  and control characters included.
- envelope: envelopes with invariants, postconditions and robotics bounds
  built from the oracle's own models (models.Permission, Invariant,
  Postcondition and predicate.Expression), against random calls.
"""

from __future__ import annotations

import json
import random
from typing import Any

from gate_cases import envelope

HEADS = ["echo", "ls", "cat", "gi" + "t", "a", "wc"]

VARIANTS = {
    "narrow": envelope(shell_allowlist=HEADS),
    "narrow-decomposed": envelope(shell_allowlist=HEADS, shell_allow_decomposition=True),
    "permissive": envelope(shell_allowlist=["*", "/*/*", "./*", "*/*"], shell_allow_decomposition=True,
                           file_read=["/**", "./**"], file_write=["/**", "./**"],
                           shell_interpreter_policy="allow"),
}


def bash(cmd: str, sid: str) -> str:
    return json.dumps({"session_id": sid, "tool_name": "Bash", "tool_input": {"command": cmd}, "cwd": "/work"})


def over_variants(cmds: list[str]) -> list[dict[str, Any]]:
    """Each command against each of the three shell envelopes."""
    out = []
    for i, cmd in enumerate(cmds):
        for name, env in VARIANTS.items():
            out.append({"label": name, "envelope": env, "stdin": bash(cmd, f"f{i}"), "show": cmd})
    return out


# ---------------------------------------------------------------------------
# heredoc
# ---------------------------------------------------------------------------

_BODY = ["hello", "$x", "${x}", "$(rm -rf /)", "`id`", "a && b", "a | b", "'q'", '"q"', "\\$y", "EOF", " EOF",
         "\tEOF", "EOFX", "#c", "", "$((1+2))", "~/x", "*.py", "<(ls)", "rm -rf /work", "coppice allow",
         "cat /home/user/.ssh/id_rsa", "\\", "E\\OF"]
_AFTER = ["", " && ls", " | cat", " > /work/o", " > /etc/o", "; rm -rf /", " 2>&1", " && echo done", " &"]


def fuzz_heredocs(n: int, seed: int) -> list[dict[str, Any]]:
    rng = random.Random(seed)
    cmds: set[str] = set()
    while len(cmds) < n:
        delim = rng.choice(["EOF", "END", "X", "e o f", "EOF1"])
        quoted = rng.choice(["", "'", '"', "\\"])
        dash = rng.choice(["", "-"])
        head = rng.choice(["cat", "ls", "wc -l", "sh", "bash", "python3", "tee /work/o", "tee /etc/o", "git apply",
                           "a", "echo"])
        if quoted == "\\":
            spelled = "\\" + delim.replace(" ", "")
            delim = delim.replace(" ", "")
        elif quoted:
            spelled = quoted + delim + quoted
        else:
            delim = delim.replace(" ", "")
            spelled = delim
        body = "\n".join(rng.choice(_BODY) for _ in range(rng.randint(0, 4)))
        close = rng.choice([delim, delim, "\t" + delim, delim + " ", ""])
        after = rng.choice(_AFTER)
        kind = rng.random()
        if kind < 0.15:
            cmd = f"{head} <<< {rng.choice(_BODY)!r}{after}"
        elif kind < 0.3:
            # Two heredocs on one line.
            cmd = f"{head} <<{spelled} <<{dash}B{after}\n{body}\n{delim}\nb\nB"
        else:
            cmd = f"{head} <<{dash}{spelled}{after}\n{body}\n{close}"
            if rng.random() < 0.3:
                cmd += "\n" + rng.choice(["ls", "rm -rf /", "echo ok", "cat /etc/passwd"])
        cmds.add(cmd)
    return over_variants(sorted(cmds))


# ---------------------------------------------------------------------------
# multiline
# ---------------------------------------------------------------------------

_LINES = ["ls", "echo a", "cat /work/a", "cat /etc/passwd", "rm -rf /", "git status", "wc -l x", "a", "# a comment",
          "", "   ", "cd /work", "cd /", "echo $(id)", "ls \\", "  -la", "echo 'a\nb'", 'echo "x\ny"', "if true; then",
          "fi", "for f in *; do", "done", "{", "}", "(", ")", "x=1", "echo a &", "ls | cat", "ls && rm x",
          "coppice allow", "grep -r token ~", "echo ok # rm -rf /"]


def fuzz_multiline(n: int, seed: int) -> list[dict[str, Any]]:
    rng = random.Random(seed)
    cmds: set[str] = set()
    while len(cmds) < n:
        lines = [rng.choice(_LINES) for _ in range(rng.randint(2, 6))]
        sep = rng.choice(["\n", "\n", "\r\n", "\n\n", " \\\n"])
        cmd = sep.join(lines)
        if rng.random() < 0.2:
            cmd += "\n"
        if "\n" in cmd:
            cmds.add(cmd)
    return over_variants(sorted(cmds))


# ---------------------------------------------------------------------------
# unicode
# ---------------------------------------------------------------------------

_CHARS = ["é", "ü", "ß", "İ", "ı", "Σ", "ς", "ﬁ", "ǅ", "中", "文", "😀", "​", "‍", " ", " ",
          " ", "\u0085", "　", "﻿", "́", "Ａ", "／", "．", "∕", "⁄", "‮", "\x0b",
          "\x0c", "\x1c", "\x7f", "\x80", "\ud800", "\udfff", "\U0001d400", "K", "Å", "ẞ", "Ω", "\x00",
          "٣", "²", "①", "⁄"]
_UWORDS = ["ls", "cat", "echo", "rm", "/work/a", "/etc/x", "~/.ssh/id", "a", "x.txt", "-la", "coppice", "token",
           "grep", "-r", "/home/user/.opendaisugi/coppice/web/token"]


def _uword(rng: random.Random) -> str:
    w = list(rng.choice(_UWORDS))
    for _ in range(rng.randint(1, 3)):
        w.insert(rng.randint(0, len(w)), rng.choice(_CHARS))
    return "".join(w)


def fuzz_unicode(n: int, seed: int) -> list[dict[str, Any]]:
    rng = random.Random(seed)
    items: list[dict[str, Any]] = []
    seen: set[str] = set()
    while len(items) < n:
        i = len(items)
        sid = f"u{i}"
        kind = rng.random()
        if kind < 0.5:
            words = [rng.choice(["ls", "cat", "echo", "rm", "grep", _uword(rng)])]
            words += [_uword(rng) if rng.random() < 0.6 else rng.choice(_UWORDS) for _ in range(rng.randint(0, 3))]
            sep = rng.choice([" ", " ", " && ", " | ", " ; ", " > "])
            cmd = sep.join(words)
            stdin = bash(cmd, sid)
            show = cmd
        elif kind < 0.8:
            tool = rng.choice(["Read", "Write", "Edit", "Grep", "Glob"])
            path = rng.choice(["/work/", "/etc/", "~/", "", "/home/user/.opendaisugi/coppice/"]) + _uword(rng)
            inp: dict[str, Any] = {"file_path": path} if tool not in ("Grep", "Glob") else {"path": path}
            if tool == "Write":
                inp["content"] = _uword(rng)
            stdin = json.dumps({"session_id": sid, "tool_name": tool, "tool_input": inp, "cwd": "/work"})
            show = f"{tool} {path}"
        else:
            host = rng.choice(["example.com", "github.com", "x.test", "127.0.0.1", "[::1]"])
            url = rng.choice(["https://", "http://", ""]) + rng.choice([host, _uword(rng) + "." + host]) + "/" + _uword(rng)
            stdin = json.dumps({"session_id": sid, "tool_name": "WebFetch", "tool_input": {"url": url}, "cwd": "/work"})
            show = f"WebFetch {url}"
        if stdin in seen:
            continue
        seen.add(stdin)
        env = rng.choice(list(VARIANTS.values()))
        if rng.random() < 0.3:
            env = json.loads(json.dumps(env))
            env["permissions"]["network"] = True
            env["permissions"]["network_hosts"] = [rng.choice(["example.com", "*.example.com", "x.test", _uword(rng)])]
            if rng.random() < 0.5:
                env["permissions"]["file_read"].append("/work/" + _uword(rng))
                env["permissions"]["shell_allowlist"].append(_uword(rng))
        items.append({"label": "unicode", "envelope": env, "stdin": stdin, "show": show})
    return items


# ---------------------------------------------------------------------------
# envelope: predicates and robotics
# ---------------------------------------------------------------------------

_PATHS = ["type", "command", "path", "url", "id", "content", "server", "tool", "arguments", "arguments.x", "depends_on",
          "missing", "command.x", "timeout_s", "", ".", "type.upper", "__class__", "0", "id.0"]
_VALUES = ["shell", "file_read", "file_write", "network", "mcp", "ls", "/work/a", "", 0, 1, -1, 1.5, True, False,
           None, [], {}, "s0", "ls && ls", "https://example.com/x", ["shell"], {"a": 1}]
_REGEXES = ["^ls", "rm", ".*", "", "[", "(?i)LS", "^/work/", "\\d+", "a{2,}", "(a|b)*c", "^$", "\\bls\\b", "[^a-z]",
            "(?=x)", "x++", "(?P<n>a)(?P=n)", "\\", "a**", "[\\w-]+", "^(?!/etc)", "é", "\\Z", "(?a)\\w", "a{,3}",
            "\\u00e9", "(?x) l s", "[[:alpha:]]", "a|", "(", "\\1"]


def _expr(rng: random.Random, depth: int = 0) -> dict[str, Any]:
    leaves = ["equals", "not_equals", "in_set", "not_in_set", "matches", "not_matches", "numeric_range",
              "length_range", "exists", "is_empty", "depends_on", "before", "alias"]
    nodes = ["and", "or", "not", "implies", "forall_steps", "exists_step", "forall_outputs"]
    op = rng.choice(leaves + (nodes if depth < 3 else []))
    if rng.random() < 0.02:
        op = rng.choice(["llm_check", "bogus", 5])
    p = rng.choice(_PATHS)
    if op in ("equals", "not_equals"):
        e: dict[str, Any] = {"op": op, "path": p, "value": rng.choice(_VALUES)}
    elif op in ("in_set", "not_in_set"):
        e = {"op": op, "path": p, "values": [rng.choice(_VALUES) for _ in range(rng.randint(0, 3))]}
    elif op in ("matches", "not_matches"):
        e = {"op": op, "path": p, "regex": rng.choice(_REGEXES)}
    elif op == "numeric_range":
        e = {"op": op, "path": p, "min": rng.choice([0, -1, 1.5, 10, "3", 1e308]),
             "max": rng.choice([0, 5, 100, -2, "x", 1e308])}
    elif op == "length_range":
        e = {"op": op, "path": p, "min": rng.choice([0, 1, 3, -1])}
        if rng.random() < 0.7:
            e["max"] = rng.choice([0, 2, 10, None, 1.0, 2.5])
    elif op in ("exists", "is_empty"):
        e = {"op": op, "path": p}
    elif op in ("depends_on", "before"):
        e = {"op": op, "step_id_a": rng.choice(["s0", "s1", "x"]), "step_id_b": rng.choice(["s0", "s1", "x"])}
    elif op == "alias":
        e = {"op": op, "name": rng.choice(["no_network", "read_only", "x"]), "args": {}}
    elif op in ("and", "or"):
        e = {"op": op, "children": [_expr(rng, depth + 1) for _ in range(rng.randint(0, 3))]}
    elif op == "not":
        e = {"op": op, "child": _expr(rng, depth + 1)}
    elif op == "implies":
        e = {"op": op, "a": _expr(rng, depth + 1), "b": _expr(rng, depth + 1)}
    elif op in ("forall_steps", "exists_step", "forall_outputs"):
        e = {"op": op, "pred": _expr(rng, depth + 1)}
    elif op == "llm_check":
        e = {"op": op, "rule": "be nice"}
    else:
        e = {"op": op, "path": p}
    if rng.random() < 0.03:
        e["extra"] = 1
    if rng.random() < 0.03 and "path" in e:
        del e["path"]
    return e


def _triple(rng: random.Random) -> Any:
    t = [rng.choice([0, 1, -1, 0.5, 2, 1e9, -3.5]) for _ in range(3)]
    if rng.random() < 0.05:
        t = t[:2]
    if rng.random() < 0.03:
        t[0] = "x"
    return t


def _robotics(rng: random.Random, p: dict[str, Any]) -> None:
    if rng.random() < 0.5:
        p["workspace_bounds"] = [_triple(rng), _triple(rng)] if rng.random() < 0.95 else None
    if rng.random() < 0.4:
        p["obstacles"] = [[_triple(rng), _triple(rng)] for _ in range(rng.randint(0, 3))]
    if rng.random() < 0.4:
        p["velocity_limit"] = rng.choice([1.0, 0, -1, 2.5, None, "3"])
    if rng.random() < 0.4:
        p["joint_limits"] = {f"j{k}": [rng.choice([-1, 0, 1.5]), rng.choice([-2, 0, 3])] for k in range(rng.randint(0, 3))}
    if rng.random() < 0.3:
        p["torque_limit"] = rng.choice([1.0, 0, -5, None])


def _call(rng: random.Random, sid: str) -> tuple[str, str]:
    kind = rng.random()
    if kind < 0.4:
        cmd = rng.choice(["ls", "ls -la", "cat /work/a", "rm -rf /work/x", "echo hi && ls", "git status", "wc -l a",
                          "curl https://example.com", "ls | cat", "a"])
        return bash(cmd, sid), cmd
    if kind < 0.7:
        tool = rng.choice(["Read", "Write", "Edit"])
        path = rng.choice(["/work/a", "/work/src/b.py", "/etc/passwd", "/work/x y", "/work/ls"])
        inp: dict[str, Any] = {"file_path": path}
        if tool == "Write":
            inp["content"] = rng.choice(["", "x", "ls", "a" * 50])
        return json.dumps({"session_id": sid, "tool_name": tool, "tool_input": inp, "cwd": "/work"}), f"{tool} {path}"
    if kind < 0.85:
        url = rng.choice(["https://example.com/x", "http://github.com", "https://x.test/a?b=1"])
        return (json.dumps({"session_id": sid, "tool_name": "WebFetch", "tool_input": {"url": url}, "cwd": "/work"}),
                f"WebFetch {url}")
    tool = rng.choice(["mcp__github__list_issues", "mcp__x__y"])
    return (json.dumps({"session_id": sid, "tool_name": tool, "tool_input": {"x": rng.choice([1, "ls", None])},
                        "cwd": "/work"}), tool)


def fuzz_envelopes(n: int, seed: int) -> list[dict[str, Any]]:
    rng = random.Random(seed)
    items = []
    for i in range(n):
        sid = f"e{i}"
        kw: dict[str, Any] = {"shell_allowlist": HEADS + ["rm", "curl"], "network": rng.random() < 0.5,
                              "mcp_allowlist": ["github/*"]}
        if rng.random() < 0.5:
            kw["shell_allow_decomposition"] = True
        env = envelope(**kw)
        if rng.random() < 0.8:
            env["invariants"] = []
            for _ in range(rng.randint(1, 3)):
                inv: dict[str, Any] = {"type": rng.choice(["no_side_effects", "file_unchanged", "t", "expr"]),
                                       "description": "d"}
                if rng.random() < 0.8:
                    inv["expr"] = _expr(rng)
                if rng.random() < 0.3:
                    inv["enforce"] = rng.choice([True, False, "yes", 0])
                if rng.random() < 0.2:
                    inv["target"] = rng.choice(["/work/a", None])
                env["invariants"].append(inv)
        if rng.random() < 0.4:
            env["postconditions"] = []
            for _ in range(rng.randint(1, 2)):
                post: dict[str, Any] = {"type": rng.choice(["file_exists", "t", "exit_code"])}
                if rng.random() < 0.7:
                    post["expr"] = _expr(rng)
                for k in ("expected", "min", "max"):
                    if rng.random() < 0.2:
                        post[k] = rng.choice([0, 1, "2", 1.0, 1.5])
                env["postconditions"].append(post)
        if rng.random() < 0.4:
            _robotics(rng, env["permissions"])
        if rng.random() < 0.2:
            env["stakes"] = rng.choice(["low", "medium", "high", "physical"])
        stdin, show = _call(rng, sid)
        items.append({"label": "envelope", "envelope": env, "stdin": stdin, "show": show})
    return items
