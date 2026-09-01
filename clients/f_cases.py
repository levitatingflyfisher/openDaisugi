"""Synthetic stage-F cases: the harness transcript parsers (`daisugi
journal parse` over Claude Code sessions and Codex rollouts, and the same
parsers under `daisugi onboard`), the split of a large episode through the
fake model, `daisugi journal ingest` of what the parsers wrote, and the
capture commands `daisugi hook list` and `daisugi hook to-trace`, run
through the Python oracle.

    uv run --no-sync python clients/f_cases.py [--out clients/fixtures/f] [--only NAME] [--fresh]

A case runs as clients/k3_cases.py runs one: a scratch HOME, the fake
`claude` and the fake model server answering only exact request bytes,
and the exit code, stdout, stderr, the tree after and the model requests
recorded. Every transcript is written here from the parsers' own code:
none comes from a real session.

Two tree forms keep the fixtures small. An entry may be written as
`{"parts": [[text, count], ...]}` (the text of each part repeated count
times), and a file of more than 32 KiB, in the tree before or after, is
recorded as its size and SHA-256, not its bytes.

The generator keeps each case's answer in a cache in the scratch
directory as it goes, so a run cut short resumes where it stopped.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import k3_cases  # noqa: E402, F401 - sibling module, run as a script (it patches garden_cases)
from garden_cases import body_id, record_replies, run_case, write_jsonl  # noqa: E402
from k3_cases import cmd_for  # noqa: E402
from pathway_cases import REPO  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "f"
SCRATCH = Path(
    os.environ.get("DAISUGI_F_SCRATCH") or Path.home() / "opendaisugi-scratch" / "f" / "runs"
)
CASE_VERSION = 1
CONFIG = ".opendaisugi/config.yaml"
LEX = {CONFIG: {"text": "matcher_model: lexical\n"}}
CC = {"OPENDAISUGI_LLM_BACKEND": "claude-code"}
KEY = {"ANTHROPIC_API_KEY": "sk-test-f-0000000000000000"}
API = {**KEY, "OPENDAISUGI_LLM_BACKEND": "api"}
BIG_FILE = 32 * 1024

# ---------------------------------------------------------------------------
# Running a case
# ---------------------------------------------------------------------------


def expand(case: dict[str, Any]) -> dict[str, Any]:
    """The case with each `parts` entry of its tree written out as text."""
    before = case.get("before") or {}
    if not any("parts" in v for v in before.values()):
        return case
    c = copy.deepcopy(case)
    for spec in c["before"].values():
        if "parts" in spec:
            text = "".join(t * n for t, n in spec.pop("parts"))
            if "hex_tail" in spec:
                spec["hex"] = text.encode("utf-8").hex() + spec.pop("hex_tail")
            else:
                spec["text"] = text
    return c


def squash(tree: dict[str, Any]) -> dict[str, Any]:
    """A tree with each file over BIG_FILE bytes as its size and SHA-256."""
    out = {}
    for rel, entry in tree.items():
        raw = None
        if isinstance(entry, dict) and "text" in entry:
            raw = entry["text"].encode("utf-8", "surrogatepass")
        elif isinstance(entry, dict) and "hex" in entry:
            raw = bytes.fromhex(entry["hex"])
        if raw is not None and len(raw) > BIG_FILE:
            entry = {
                "mode": entry["mode"],
                "size": len(raw),
                "sha256": hashlib.sha256(raw).hexdigest(),
            }
        out[rel] = entry
    return out


def run(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    res = run_case(expand(case), cmd, work)
    res["tree"] = squash(res["tree"])
    return res


# ---------------------------------------------------------------------------
# Transcript rows, as the parsers read them
# ---------------------------------------------------------------------------


def row(**kw: Any) -> str:
    return json.dumps(kw, ensure_ascii=False)


def asst(*blocks: Any) -> str:
    return row(type="assistant", message={"role": "assistant", "content": list(blocks)})


def tu(name: Any, **inp: Any) -> dict[str, Any]:
    return {"type": "tool_use", "id": "toolu_x", "name": name, "input": inp}


def tu_raw(name: Any, inp: Any) -> dict[str, Any]:
    return {"type": "tool_use", "id": "toolu_x", "name": name, "input": inp}


def user(text: Any) -> str:
    return row(type="user", message={"role": "user", "content": text}, uuid="u", sessionId="S")


def lines(*rows: str) -> str:
    return "\n".join(rows) + "\n"


def cx(**payload: Any) -> str:
    """One Codex rollout line: a response item under its wrapper."""
    return row(type="response_item", payload=payload)


def fcall(name: str, args: Any, **kw: Any) -> str:
    return cx(
        type="function_call",
        name=name,
        arguments=args if isinstance(args, str) else json.dumps(args),
        **kw,
    )


def umsg(text: str) -> str:
    return row(type="event_msg", payload={"type": "user_message", "message": text})


# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------


def parse(
    name: str,
    text: str | None = None,
    *flags: str,
    fmt: str | None = None,
    hex_: str | None = None,
    parts: list | None = None,
    as_json: bool = True,
    **kw: Any,
) -> dict[str, Any]:
    """`journal parse t.jsonl -o eps.(json|yaml)` over one transcript."""
    spec: dict[str, Any]
    if hex_ is not None:
        spec = {"hex": hex_}
    elif parts is not None:
        spec = {"parts": parts}
    else:
        spec = {"text": text}
    out = "eps.json" if as_json else "eps.yaml"
    argv = ["journal", "parse", "t.jsonl", "-o", out]
    if as_json:
        argv.append("--json")
    if fmt:
        argv += ["--format", fmt]
    argv += list(flags)
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": argv,
        "before": {**LEX, "t.jsonl": spec, **kw.pop("extra", {})},
        "env": kw.pop("env", CC),
    }
    c.update(kw)
    return c


M1 = ("--min-tools", "1")


def build_claude_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append

    # Row shapes.
    add(
        parse(
            "claude rows modern and flat",
            lines(
                row(type="summary", summary="s"),
                user("Build it"),
                asst(tu("Bash", command="make")),
                row(role="user", content="flat turn"),
                row(role="assistant", content=[tu("Read", file_path="/w/a")]),
                row(role="human", content="human role"),
                asst(tu("Read", file_path="/w/b")),
                row(type="assistant", message={"role": "user", "content": "msg role wins"}),
                row(type="user", message="not a dict", role="user", content="has role"),
                row(type="user", message={"content": "no role in message"}),
                row(type="assistant", message={"role": "assistant"}),
                row(type="system", role="user", content="system row with role"),
                row(type=None, role="user", content="null type"),
                row(type=0, role="user", content="zero type"),
                row(type=["user"], role="user", content="list type"),
                row(type="", role="user", content="empty type"),
                row(role=None, content="null role"),
                row(role="user"),
                row(role=["user"], content="list role"),
            ),
            *M1,
        )
    )
    add(
        parse(
            "claude malformed lines",
            lines(
                "not json at all",
                "[1, 2]",
                "null",
                '"a string"',
                "42",
                "{} trailing",
                "{bad",
                "   ",
                user("Task one"),
                "\t" + asst(tu("Bash", command="ls")) + "  ",
                '{"type": "user", "message": {"role": "user", "content": "raw\ttab"}}',
                '{"type": "user", "message": {"role": "user", "content": "nan", "x": NaN}}',
                asst(tu("Read", file_path="/w/nan")),
                '{"type": "user", "message": {"role": "user", "content": "dup"}, "type": "x"}',
            ),
            *M1,
        )
    )
    add(
        parse(
            "claude newlines crlf and cr",
            "\r\n".join([user("CRLF task"), asst(tu("Bash", command="make a"))])
            + "\r"
            + user("CR task")
            + "\r"
            + asst(tu("Bash", command="make b"))
            + "\r\n",
            *M1,
        )
    )
    add(
        parse(
            "claude bom first line",
            "\ufeff" + lines(user("after bom"), asst(tu("Bash", command="x"))),
            *M1,
        )
    )
    add(parse("claude no final newline", user("t") + "\n" + asst(tu("Bash", command="z")), *M1))
    add(parse("claude only assistant", lines(asst(tu("Bash", command="z"))), *M1))
    add(parse("claude empty file", ""))
    add(parse("claude blank lines only", "\n\n  \n\t\n"))

    # Task labels.
    add(
        parse(
            "claude task cleaning",
            lines(
                user("  <system-reminder>x\ny</system-reminder> Fix   the\n\tbuild  "),
                asst(tu("Bash", command="a")),
                user("<system-reminder>only this</system-reminder>"),
                asst(tu("Bash", command="b")),
                user(
                    "<command-message>m</command-message><command-name>/deploy</command-name>"
                    "<command-args>prod</command-args>"
                ),
                asst(tu("Bash", command="c")),
                user("<local-command-stdout>out</local-command-stdout>go on"),
                asst(tu("Bash", command="d")),
                user("Base directory for this skill: /home/user/.claude/skills/foo/\n\n# Foo"),
                asst(tu("Bash", command="e")),
                user("Base directory for this skill:"),
                asst(tu("Bash", command="f")),
                user("Base directory for this skill: /\nbody"),
                asst(tu("Bash", command="g")),
                user("This session is being continued from before."),
                asst(tu("Bash", command="h")),
                user("wide\u3000space\u00a0and\u2028line sep"),
                asst(tu("Bash", command="i")),
                user("   "),
                asst(tu("Bash", command="j")),
                user(""),
                asst(tu("Bash", command="k")),
            ),
            *M1,
        )
    )
    add(
        parse(
            "claude task long",
            lines(
                user("é" * 2500),
                asst(tu("Bash", command="a")),
                user("<system-reminder>" + "x" * 300 + "</system-reminder>"),
                asst(tu("Bash", command="b")),
                user("\U0001f600" * 2100),
                asst(tu("Bash", command="c")),
            ),
            *M1,
        )
    )
    add(
        parse(
            "claude user content shapes",
            lines(
                user([{"type": "text", "text": "one text block"}]),
                asst(tu("Bash", command="a")),
                user([{"type": "text", "text": "two"}, {"type": "text", "text": "blocks"}]),
                asst(tu("Bash", command="b")),
                user([{"type": "tool_result", "content": "r"}]),
                asst(tu("Bash", command="c")),
                user([{"type": "text", "text": 5}]),
                user([{"type": "text"}]),
                user(["plain string in list"]),
                user({"type": "text", "text": "a dict"}),
                user(None),
                user(7),
                asst(tu("Bash", command="d")),
            ),
            *M1,
        )
    )

    # Tool calls.
    add(
        parse(
            "claude every tool",
            lines(
                user("Use every tool"),
                asst(
                    {"type": "text", "text": "hi"},
                    tu("Read", file_path="/w/r.py"),
                    tu("Read"),
                    tu("Edit", file_path="/w/e.py", old_string="a", new_string="b"),
                    tu("Write", path="/w/p.txt"),
                    tu("Glob", pattern="**/*.go"),
                    tu("Grep", pattern="TODO", path="/w"),
                    tu("Grep", pattern="x", file_path="", path=""),
                    tu("WebFetch", url="https://example.com/a?b=c&d=é"),
                    tu("WebFetch", url="", query="fallback q"),
                    tu("WebSearch", query="spaces & sym/bols ~_.- é ✓"),
                    tu("WebSearch"),
                    tu("Bash", command="echo hi", description="d"),
                    tu("Bash"),
                    tu("Bash", command=""),
                    tu("Bash", query="query as command"),
                    tu("Task", prompt="sub task"),
                    tu("Task"),
                    tu("Agent", prompt="", description="x"),
                    tu("Skill", skill="deslop", args="tighten"),
                    tu("Skill", command="alt", args={"k": [1, 2]}),
                    tu("Skill", skill="s", args=["a", "b"]),
                    tu("Skill", skill="s", args=0),
                    tu("Skill", skill="s", args=""),
                    tu("Skill"),
                    tu("mcp__github__create_issue", title="t", n=None, f=1.5),
                    tu("mcp__solo"),
                    tu("mcp__a__b__c"),
                    tu("mcp____empty"),
                    tu("TodoWrite", todos=[]),
                    tu("TaskCreate", subject="s"),
                    tu("Workflow", script="x"),
                    tu("Unknown", x=1),
                    {"type": "tool_use", "name": "Read"},
                    {"type": "tool_use", "input": {"file_path": "/w/noname"}},
                    {"type": "tool_result", "name": "Read", "input": {"file_path": "/x"}},
                    "a string block",
                    5,
                ),
            ),
        )
    )
    add(
        parse(
            "claude tool input shapes",
            lines(
                user("Odd inputs"),
                asst(
                    tu_raw("mcp__srv__tool", [1, 2]),
                    tu_raw("mcp__srv__tool", "text input"),
                    tu_raw("mcp__srv__tool", None),
                    tu_raw("Unknown", [1]),
                    tu_raw("Read", None),
                    tu_raw("Read", []),
                    tu_raw("Read", ""),
                    tu_raw("Read", 0),
                ),
            ),
        )
    )
    add(
        parse(
            "claude assistant content shapes",
            lines(
                user("Content shapes"),
                row(type="assistant", message={"role": "assistant", "content": "just text"}),
                row(type="assistant", message={"role": "assistant", "content": {"a": 1}}),
                row(type="assistant", message={"role": "assistant", "content": None}),
                row(type="assistant", message={"role": "assistant", "content": []}),
                asst(tu("Bash", command="after")),
            ),
            *M1,
        )
    )
    for label, inp in (
        ("list", [1]),
        ("string", "cmd"),
        ("number", 3),
        ("null", None),
    ):
        add(
            parse(
                f"claude agent input {label}",
                lines(user("t"), asst(tu_raw("Agent", inp))),
                *M1,
            )
        )
    for label, name in (("null", None), ("number", 5), ("list", ["Read"]), ("dict", {"a": 1})):
        add(parse(f"claude tool name {label}", lines(user("t"), asst(tu_raw(name, {}))), *M1))
    for label, inp in (
        ("null", None),
        ("list", ["ls"]),
        ("string", "ls"),
        ("command number", {"command": 5}),
        ("command list", {"command": ["a;b"]}),
        ("command null", {"command": None}),
        ("command false", {"command": False}),
    ):
        add(parse(f"claude bash input {label}", lines(user("t"), asst(tu_raw("Bash", inp))), *M1))
    add(
        parse(
            "claude mcp arguments nan",
            lines(user("t"))
            + '{"type": "assistant", "message": {"role": "assistant", "content": [{"type": "tool_use", '
            + '"name": "mcp__a__b", "input": {"x": NaN, "y": [Infinity]}}]}}\n',
            *M1,
            as_json=False,
        )
    )
    add(
        parse(
            "claude bash command list with separators",
            lines(user("t"), asst(tu_raw("Bash", {"command": ["a", ";", "b", "&", "&", "c"]}))),
            *M1,
        )
    )
    add(
        parse(
            "claude bash command list with a number",
            lines(user("t"), asst(tu_raw("Bash", {"command": ["a", ";", 5]}))),
            *M1,
        )
    )
    add(
        parse(
            "claude bash command dict",
            lines(user("t"), asst(tu_raw("Bash", {"command": {"a": 1}}))),
            *M1,
        )
    )
    for label, inp in (
        ("path number", {"file_path": 5}),
        ("path list", {"file_path": ["a"]}),
        ("path nan", {"file_path": float("nan")}),
        ("url number", {"url": 5}),
        ("query number", {"query": 5}),
        ("prompt number", {"prompt": 5}),
        ("skill number", {"skill": 5}),
    ):
        tool = {
            "path": "Read",
            "url": "WebFetch",
            "query": "WebSearch",
            "prompt": "Task",
            "skill": "Skill",
        }[label.split()[0]]
        add(
            parse(
                f"claude tool value {label}",
                lines(user("t"), asst(tu_raw(tool, inp))),
                *M1,
            )
        )

    # Compound shell commands split into steps.
    add(
        parse(
            "claude compound shell",
            lines(
                user("Compound"),
                asst(
                    tu("Bash", command="cd /w && make; echo 'a;b' || true"),
                    tu("Bash", command='echo "x && y" && ls'),
                    tu("Bash", command="a;;b; ;c;"),
                    tu("Bash", command="echo $(a; b) && c"),
                    tu("Bash", command="echo \\; x && y"),
                    tu("Bash", command="a & b | c || d"),
                    tu("Bash", command="'unterminated && x"),
                    tu("Bash", command=";"),
                    tu("Bash", command="  lone  "),
                    tu("Bash", command="é && ü"),
                ),
            ),
        )
    )

    # Episodes: merging and the tool thresholds.
    many = lines(
        user("first"),
        asst(tu("Bash", command="a")),
        user("second"),
        asst(tu("Bash", command="b"), tu("Bash", command="c"), tu("Bash", command="d")),
        user("third"),
        user("fourth"),
        asst(tu("Bash", command="e"), tu("Bash", command="f")),
    )
    add(parse("claude merge default", many))
    add(parse("claude merge min tools 0", many, "--min-tools", "0"))
    add(parse("claude merge min tools negative", many, "--min-tools", "-2"))
    add(parse("claude merge min tools 3 yaml", many, "--min-tools", "3", as_json=False))
    add(
        parse(
            "claude values in yaml",
            lines(
                user("YAML: quoting 'x' \"y\" #z & *a ! % @ `b` é\u2028 \x85 end"),
                asst(
                    tu("Bash", command="echo 'it''s' \"q\" : - ? [x] {y}"),
                    tu("Read", file_path="/w/yes"),
                    tu("Read", file_path="null"),
                    tu("Read", file_path="0123"),
                    tu("mcp__s__t", a=True, b=None, c=1e100, d=[], e={}, f="multi\nline"),
                ),
            ),
            as_json=False,
        )
    )
    add(
        parse(
            "claude lone surrogate json",
            lines('{"type": "user", "message": {"role": "user", "content": "bad \\ud800 turn"}}')
            + lines(asst(tu("Bash", command="echo \\udfff"))),
            *M1,
        )
    )
    add(
        parse(
            "claude lone surrogate yaml",
            lines('{"type": "user", "message": {"role": "user", "content": "bad \\ud800 turn"}}')
            + lines(asst(tu("Bash", command="x"))),
            *M1,
            as_json=False,
        )
    )
    add(
        parse(
            "claude lone surrogate in command yaml",
            lines(user("ok"))
            + '{"type": "assistant", "message": {"role": "assistant", "content": '
            + '[{"type": "tool_use", "name": "Bash", "input": {"command": "x \\udc80"}}]}}\n',
            *M1,
            as_json=False,
        )
    )

    # Huge and deep lines.
    add(
        parse(
            "claude huge dropped line",
            None,
            *M1,
            parts=[
                [user("before huge") + "\n", 1],
                ['{"type": "summary", "summary": "', 1],
                ["abcdefgh", 150_000],
                ['"}\n', 1],
                [asst(tu("Bash", command="after")) + "\n", 1],
            ],
        )
    )
    add(
        parse(
            "claude huge command",
            None,
            *M1,
            parts=[
                [user("huge command") + "\n", 1],
                ['{"type": "assistant", "message": {"role": "assistant", "content": [', 1],
                ['{"type": "tool_use", "name": "Bash", "input": {"command": "echo ', 1],
                ["0123456789", 7000],
                ['"}}]}}\n', 1],
            ],
        )
    )
    add(
        parse(
            "claude many lines",
            None,
            *M1,
            parts=[[user("t") + "\n" + asst(tu("Bash", command="make")) + "\n", 1500]],
        )
    )
    add(
        parse(
            "claude deep nesting dropped row",
            lines(user("t"), '{"type": "summary", "x": ' + "[" * 20000 + "]" * 20000 + "}"),
            *M1,
        )
    )
    add(
        parse(
            "claude long integer",
            lines(user("t"), '{"type": "summary", "n": ' + "7" * 5000 + "}"),
            *M1,
        )
    )
    add(
        parse(
            "claude long integer at limit",
            lines(user("t"), '{"type": "summary", "n": -' + "7" * 4300 + "}"),
            *M1,
        )
    )
    add(
        parse(
            "claude big float",
            lines(user("t"), '{"type": "summary", "n": 1e400, "m": ' + "9" * 400 + ".5}"),
            *M1,
        )
    )

    # Bytes that are not UTF-8: the decode error names the position within
    # the 8192-byte chunk the reader decoded.
    head = (user("t") + "\n" + asst(tu("Bash", command="make")) + "\n").encode()
    for label, data in (
        ("at start", b"\xff" + head),
        ("in first chunk", head + b'{"x": "\xc3("}\n'),
        ("second chunk", head * 200 + b'{"x": "\xe2\x82"}\n'),
        ("surrogate bytes", head + b"\xed\xa0\x80\n"),
        ("overlong", head + b"\xc0\xaf\n"),
        ("four byte bad", head + b"\xf0\x9f\x98\n"),
        ("f4 90", head + b"\xf4\x90\x80\x80\n"),
        ("incomplete at end", head + b"\xe2\x82"),
        ("latin1 text", "caf\xe9\n".encode("latin-1")),
    ):
        add(parse(f"claude not utf8 {label}", None, *M1, hex_=data.hex()))
    pad = b"a" * (8192 - 1)
    for label, data in (
        ("straddle ok", b"\n" + pad[:-1] + "é".encode() + b"\n" + head),
        ("straddle bad", b"\n" + pad[:-1] + b"\xc3\n" + head),
        ("straddle three", b"\n" + pad[:-2] + b"\xe2\x82\n" + head),
        ("after long integer", head + b'{"n": ' + b"7" * 5000 + b"}\n" + pad + b"\xff\n"),
        ("long integer after", head + pad + b"\xff\n" + b'{"n": ' + b"7" * 5000 + b"}\n"),
    ):
        add(parse(f"claude not utf8 {label}", None, *M1, hex_=data.hex()))
    return C


def build_codex_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append

    def cp(name: str, text: str, *flags: str, **kw: Any) -> dict[str, Any]:
        return parse(name, text, *flags, fmt="codex", **kw)

    add(
        cp(
            "codex wrappings",
            lines(
                row(type="session_meta", payload={"id": "s"}),
                row(type="turn_context", payload={"cwd": "/w"}),
                umsg("Run the tests"),
                cx(
                    type="message",
                    role="user",
                    content=[{"type": "input_text", "text": "Run the tests"}],
                ),
                fcall("shell", {"command": ["bash", "-lc", "pytest -q"]}),
                row(
                    item={
                        "type": "function_call",
                        "name": "shell",
                        "arguments": '{"command": "ls"}',
                    }
                ),
                row(type="function_call", name="shell", arguments='{"command": "pwd"}'),
                row(item={"item": {"type": "local_shell_call", "action": {"command": ["id"]}}}),
                row(
                    item={
                        "item": {
                            "item": {
                                "item": {
                                    "type": "local_shell_call",
                                    "action": {"command": ["deep4"]},
                                }
                            }
                        }
                    }
                ),
                row(
                    item={
                        "item": {
                            "item": {
                                "item": {
                                    "item": {
                                        "type": "local_shell_call",
                                        "action": {"command": ["deep5"]},
                                    }
                                }
                            }
                        }
                    }
                ),
                row(type="compacted", payload={"message": "summary"}),
                row(type="response_item", payload="not a dict"),
                row(type="event_msg", payload=None),
                row(type="unknown_kind", payload={"type": "function_call", "name": "shell"}),
                row(type="reasoning", summary=[]),
                cx(type="function_call_output", call_id="c", output="ok"),
                row(item=5),
                "[]",
                "garbage",
            ),
            *M1,
        )
    )
    add(
        cp(
            "codex user turns",
            lines(
                umsg("A"),
                cx(type="message", role="user", content=[{"type": "input_text", "text": "A"}]),
                fcall("shell", {"command": "a"}),
                umsg("B"),
                fcall("shell", {"command": "b"}),
                umsg("A"),
                fcall("shell", {"command": "c"}),
                umsg("  "),
                row(
                    type="event_msg",
                    payload={"type": "user_message", "message": [{"text": "list msg"}]},
                ),
                fcall("shell", {"command": "d"}),
                row(type="event_msg", payload={"type": "user_message", "message": 5}),
                row(type="event_msg", payload={"type": "agent_message", "message": "not user"}),
                cx(type="message", role="user", content="plain string"),
                fcall("shell", {"command": "e"}),
                cx(
                    type="message",
                    role="user",
                    content=[{"text": "a"}, {"text": 5}, "x", {"text": "b"}],
                ),
                fcall("shell", {"command": "f"}),
                cx(
                    type="message",
                    role="assistant",
                    content=[{"type": "output_text", "text": "done"}],
                ),
                cx(type="message", role="system", content=[{"type": "input_text", "text": "sys"}]),
                cx(type="message", role="developer", content=[]),
                cx(type="message", content="no role"),
                fcall("shell", {"command": "g"}),
            ),
            *M1,
        )
    )
    add(
        cp(
            "codex shell commands",
            lines(
                umsg("Shells"),
                fcall("shell", {"command": ["bash", "-lc", "make  test "]}),
                fcall("shell", {"command": ["bash", "-l", "-c", "a && b"]}),
                fcall("shell", {"command": ["zsh", "-c", "z"]}),
                fcall("shell", {"command": ["dash", "-c", "  "]}),
                fcall("shell", {"command": ["fish", "-c", "f"]}),
                fcall("shell", {"command": ["bash", "script.sh"]}),
                fcall("shell", {"command": ["bash", "x", "-c", "y"]}),
                fcall("shell", {"command": ["ls", "-la", "my dir", "it's", "", "$HOME"]}),
                fcall("shell", {"command": ["echo", 1, 2.5, None, True, 1e100]}),
                fcall("shell", {"command": []}),
                fcall("shell", {"command": "  spaced  "}),
                fcall("shell", {"command": ""}),
                fcall("shell", {"command": 5}),
                fcall("shell", {"command": {"a": 1}}),
                fcall("shell", {}),
                fcall("exec_command", {"command": ["git", "status"]}),
                fcall("container.exec", {"command": ["uname"]}),
                fcall("Shell", {"command": ["nope"]}),
                cx(type="local_shell_call", action={"command": ["bash", "-lc", "local"]}),
                cx(type="local_shell_call", action={}),
                cx(type="local_shell_call"),
                cx(type="local_shell_call", action=None),
                cx(type="local_shell_call", action={"command": "str cmd"}),
            ),
        )
    )
    add(
        cp(
            "codex patches",
            lines(
                umsg("Patch"),
                cx(
                    type="custom_tool_call",
                    name="apply_patch",
                    input="*** Begin Patch\n*** Update File: src/a.py\n@@\n*** Add File:  b.md \n"
                    "*** Delete File: c.txt\r\n*** Move File: d\n*** Add File: \n*** End Patch\n",
                ),
                fcall("apply_patch", {"input": "*** Delete File: old.txt\n"}),
                fcall("apply_patch", {"patch": "*** Add File: via patch key\n"}),
                fcall("apply_patch", {"input": "", "patch": "*** Add File: fallback\n"}),
                fcall("apply_patch", {}),
                cx(type="custom_tool_call", name="other", input="*** Add File: no\n"),
                cx(type="custom_tool_call", name="apply_patch"),
                cx(type="custom_tool_call", name="apply_patch", input=""),
                fcall("apply_patch", {"input": "no files here"}),
            ),
        )
    )
    add(
        cp(
            "codex function calls",
            lines(
                umsg("Functions"),
                fcall("search", {"q": "x"}, namespace="docs"),
                fcall("tool", {}, namespace=""),
                fcall("tool", {"a": [1]}, namespace=3),
                fcall("update_plan", "{}"),
                fcall("view_image", {"path": "/x.png"}),
                fcall("shell", "not json"),
                fcall("shell", ""),
                fcall("shell", "null"),
                fcall("shell", "[1, 2]"),
                fcall("shell", '"str"'),
                fcall("shell", '{"command": "after bad"}'),
                cx(type="function_call", name="shell"),
                cx(type="function_call", arguments='{"command": "no name"}'),
                cx(type="function_call", name=None, arguments="{}", namespace="ns"),
                cx(type="web_search_call", action={"query": "codex docs é"}),
                cx(type="web_search_call", action={"query": ""}),
                cx(type="web_search_call", action={}),
                cx(type="web_search_call"),
            ),
        )
    )
    for label, item in (
        ("type list", {"type": ["message"], "payload": {}}),
        ("type dict", {"item": {"type": {"a": 1}}}),
        ("name list", {"type": "function_call", "name": ["shell"], "arguments": "{}"}),
        (
            "arguments dict",
            {"type": "function_call", "name": "shell", "arguments": {"command": "x"}},
        ),
        ("arguments number", {"type": "function_call", "name": "shell", "arguments": 5}),
        ("arguments list", {"type": "function_call", "name": "shell", "arguments": ["x"]}),
        ("arguments true", {"type": "function_call", "name": "shell", "arguments": True}),
        ("patch input number", {"type": "custom_tool_call", "name": "apply_patch", "input": 5}),
        (
            "patch input dict",
            {"type": "custom_tool_call", "name": "apply_patch", "input": {"a": 1}},
        ),
        ("action list", {"type": "local_shell_call", "action": ["ls"]}),
        ("action string", {"type": "local_shell_call", "action": "ls"}),
        ("search action list", {"type": "web_search_call", "action": ["q"]}),
        ("search query number", {"type": "web_search_call", "action": {"query": 5}}),
        (
            "namespace list",
            {"type": "function_call", "name": "t", "namespace": ["n"], "arguments": "{}"},
        ),
        ("name number", {"type": "function_call", "name": 7, "namespace": "n", "arguments": "{}"}),
        (
            "long integer argument",
            {"type": "function_call", "name": "shell", "arguments": '{"n": ' + "1" * 4400 + "}"},
        ),
    ):
        add(cp(f"codex {label}", lines(umsg("t"), cx(**item)), *M1))
    add(
        cp(
            "codex no final newline crlf",
            "\r\n".join([umsg("crlf"), fcall("shell", {"command": "x"})]),
            *M1,
        )
    )
    add(cp("codex empty", ""))
    add(
        cp("codex claude transcript", lines(user("claude row"), asst(tu("Bash", command="x"))), *M1)
    )
    add(
        parse(
            "claude reads a codex rollout",
            lines(umsg("codex row"), fcall("shell", {"command": "x"})),
            *M1,
        )
    )
    add(cp("codex not utf8", None, *M1, hex_=(umsg("t") + "\n").encode().hex() + "ff0a"))
    add(
        cp(
            "codex yaml",
            lines(umsg("Yaml out"), fcall("shell", {"command": ["git", "log", "--format=%H: %s"]})),
            *M1,
            as_json=False,
        )
    )
    return C


def build_format_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    t = lines(user("t"), asst(tu("Bash", command="x")))
    for fmt in ("pi", "opencode", "hermes", "openclaw", "Claude-Code", ""):
        C.append(parse(f"format {fmt or 'empty'}", t, fmt=fmt or " "))
    return C


def build_split_cases() -> list[dict[str, Any]]:
    """An episode over --max-tools is split by one model call."""
    C: list[dict[str, Any]] = []
    add = C.append
    reads = [tu("Read", file_path=f"/w/f{k}.py") for k in range(4)]
    runs = [tu("Bash", command=f"make t{k} && echo ok{k}") for k in range(2)]
    two = lines(
        user("Refactor everything"),
        asst(*reads),
        asst(*runs),
        user("Now the docs"),
        asst(*[tu("Write", file_path=f"/w/d{k}.md") for k in range(6)]),
        user("small"),
        asst(tu("Bash", command="ls")),
    )

    def sub(*spans: tuple[int, int, Any]) -> str:
        return json.dumps(
            {"subtasks": [{"start_index": a, "end_index": b, "task": t} for a, b, t in spans]}
        )

    first = sub((0, 3, "read the files"), (4, 7, "run the tests"))
    second = sub((0, 1, "write two"), (2, 5, "write four"))
    add(
        parse(
            "split two episodes claude",
            two,
            "--max-tools",
            "5",
            replies=[{"claude_raw": first}, {"claude_raw": second}],
        )
    )
    add(
        parse(
            "split two episodes api",
            two,
            "--max-tools",
            "5",
            env=API,
            replies=[{"http": first}, {"http": second}],
        )
    )
    add(
        parse(
            "split first fails second splits",
            two,
            "--max-tools",
            "5",
            replies=[{"claude_raw": "", "exit": 1, "stderr": "boom"}, {"claude_raw": second}],
        )
    )
    for label, reply in (
        ("overlap", sub((0, 4, "a"), (4, 7, "b"))),
        ("negative end", sub((0, -1, "a"))),
        ("past the end", sub((0, 3, "a"), (4, 9, "b"))),
        (
            "bool start",
            json.dumps({"subtasks": [{"start_index": False, "end_index": 7, "task": "b"}]}),
        ),
        ("one span", sub((0, 7, "all of it"))),
        ("reversed", sub((4, 7, "b"), (0, 3, "a"))),
        ("task number", sub((0, 7, 5))),
        ("task null", sub((0, 7, None))),
        ("task empty", sub((0, 7, ""))),
        (
            "extra keys",
            json.dumps(
                {"subtasks": [{"start_index": 0, "end_index": 7, "task": "t", "why": [1]}], "x": 1}
            ),
        ),
        (
            "string end",
            json.dumps({"subtasks": [{"start_index": 0, "end_index": "7", "task": "t"}]}),
        ),
        ("subtasks null", json.dumps({"subtasks": None})),
        ("subtasks string", json.dumps({"subtasks": "none"})),
        ("in fences", "```json\n" + sub((0, 7, "fenced")) + "\n```"),
        ("two objects", sub((0, 7, "first")) + " and " + sub((0, 7, "second"))),
    ):
        add(
            parse(
                f"split reply {label}",
                two,
                "--max-tools",
                "7",
                replies=[{"claude_raw": reply}],
            )
        )
    add(
        parse(
            "split max tools 0",
            lines(user("a"), asst(tu("Bash", command="x"), tu("Bash", command="y")), user("empty")),
            "--max-tools",
            "0",
            "--min-tools",
            "0",
            replies=[{"claude_raw": sub((0, 0, "x"), (1, 1, "y"))}],
        )
    )
    add(
        parse(
            "split max tools negative",
            lines(user("a"), asst(tu("Bash", command="x")), user("empty")),
            "--max-tools",
            "-1",
            "--min-tools",
            "0",
            replies=[
                {"claude_raw": sub((0, 0, "x"))},
                {"claude_raw": json.dumps({"subtasks": []})},
            ],
        )
    )
    add(
        parse(
            "split compound keeps depends",
            lines(
                user("deps"),
                asst(tu("Bash", command="a && b && c"), tu("Bash", command="d; e")),
            ),
            "--max-tools",
            "3",
            "--min-tools",
            "1",
            replies=[{"claude_raw": sub((0, 1, "ab"), (2, 4, "cde"))}],
        )
    )
    add(
        parse(
            "split codex",
            lines(umsg("codex big"), *[fcall("shell", {"command": f"step {k}"}) for k in range(4)]),
            "--max-tools",
            "3",
            "--min-tools",
            "1",
            fmt="codex",
            replies=[{"claude_raw": sub((0, 1, "early"), (2, 3, "late"))}],
        )
    )
    add(
        parse(
            "split codex api",
            lines(umsg("codex big"), *[fcall("shell", {"command": f"step {k}"}) for k in range(4)]),
            "--max-tools",
            "3",
            "--min-tools",
            "1",
            fmt="codex",
            env=API,
            replies=[{"http": sub((0, 1, "early"), (2, 3, "late"))}],
        )
    )
    add(
        parse(
            "split yaml output",
            two,
            "--max-tools",
            "7",
            as_json=False,
            replies=[{"claude_raw": sub((0, 3, "yaml: a"), (4, 7, "b"))}],
        )
    )
    add(
        parse(
            "split labels in prompt",
            lines(
                user("labels é"),
                asst(
                    tu("WebSearch", query="q"),
                    tu("WebFetch", url="https://x.example/"),
                    tu("Task", prompt="p"),
                    tu("Skill", skill="s"),
                    tu("mcp__a__b"),
                    tu("Grep", pattern="pat", path=""),
                ),
            ),
            "--max-tools",
            "5",
            replies=[{"claude_raw": sub((0, 5, "labels"))}],
        )
    )
    return C


def build_onboard_cases() -> list[dict[str, Any]]:
    """The parsers under `daisugi onboard --dry-run`: a transcript whose
    parse fails is a warning, and the run goes on."""
    C: list[dict[str, Any]] = []
    P = ".claude/projects/p"
    ok = {"text": lines(user("fine"), asst(tu("Bash", command="make"))), "mtime": -300}
    head = (user("t") + "\n" + asst(tu("Bash", command="make")) + "\n").encode()

    def ob(name: str, before: dict[str, Any], *flags: str, **kw: Any) -> dict[str, Any]:
        c: dict[str, Any] = {
            "kind": "cli",
            "name": name,
            "argv": ["onboard", "--dry-run", "--min-tools", "1", *flags],
            "before": {**LEX, f"{P}/ok.jsonl": ok, **before},
            "env": kw.pop("env", CC),
        }
        c.update(kw)
        return c

    C.append(
        ob("onboard not utf8", {f"{P}/bad.jsonl": {"hex": (head + b"\xff\n").hex(), "mtime": -100}})
    )
    C.append(
        ob(
            "onboard not utf8 second chunk",
            {f"{P}/bad.jsonl": {"hex": (head * 200 + b"\xc3\n").hex(), "mtime": -100}},
        )
    )
    C.append(
        ob(
            "onboard long integer",
            {
                f"{P}/big.jsonl": {
                    "text": lines(user("t"), '{"n": ' + "1" * 5000 + "}"),
                    "mtime": -100,
                }
            },
        )
    )
    C.append(
        ob(
            "onboard codex argument dict",
            {
                ".codex/sessions/2026/01/c.jsonl": {
                    "text": lines(
                        umsg("t"), cx(type="function_call", name="shell", arguments={"c": 1})
                    ),
                    "mtime": -100,
                }
            },
        )
    )
    C.append(
        ob(
            "onboard tool name number",
            {f"{P}/n.jsonl": {"text": lines(user("t"), asst(tu_raw(5, {}))), "mtime": -100}},
        )
    )
    C.append(
        ob(
            "onboard pydantic error",
            {
                f"{P}/v.jsonl": {
                    "text": lines(user("t"), asst(tu_raw("Read", {"file_path": 5}))),
                    "mtime": -100,
                }
            },
        )
    )
    C.append(
        ob(
            "onboard other harnesses",
            {
                "pi/s.jsonl": {"text": lines(user("t")), "mtime": -100},
                "oc/s.jsonl": {"text": lines(user("t")), "mtime": -90},
            },
            env={**CC, "OPENDAISUGI_TRANSCRIPT_ROOTS": "pi={HOME}/pi:opencode={HOME}/oc"},
        )
    )
    return C


# -- journal ingest of what the parsers wrote ---------------------------------


def st(i: int, typ: str, **kw: Any) -> dict[str, Any]:
    return {"id": f"s{i}", "depends_on": [], "metadata": {}, "type": typ, **kw}


def episodes(*eps: tuple[str, list[dict[str, Any]]], source: str = "t.jsonl") -> str:
    body = {
        "source": "claude-code",
        "source_file": source,
        "parsed_at": "2020-01-01T00:00:00Z",
        "episodes": [
            {
                "id": f"ep_{i:02d}",
                "task": task,
                "steps": steps,
                "source_range": {"first_message": i, "last_message": i},
            }
            for i, (task, steps) in enumerate(eps)
        ],
    }
    return json.dumps(body, indent=2)


def build_ingest_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def ing(name: str, text: str, *flags: str, **kw: Any) -> dict[str, Any]:
        c: dict[str, Any] = {
            "kind": "cli",
            "name": name,
            "argv": ["journal", "ingest", "eps.json", *flags],
            "before": {**LEX, "eps.json": {"text": text}, **kw.pop("extra", {})},
            "env": CC,
        }
        c.update(kw)
        return c

    mixed = episodes(
        ("shell ok", [st(0, "shell", command="make test"), st(1, "file_read", path="/w/a.py")]),
        (
            "compound",
            [st(2, "shell", command="cd /w && make"), st(3, "shell", command="ls | wc -l")],
        ),
        (
            "writes and fetches",
            [
                st(4, "file_write", path="/w/out/r.txt", content=""),
                st(
                    5,
                    "network",
                    url="https://web-search.invalid/?q=a%20b",
                    method="GET",
                    headers={},
                ),
                st(6, "network", url="", method="GET", headers={}),
            ],
        ),
        (
            "agentic",
            [
                st(7, "task", prompt="review"),
                st(8, "skill", skill_id="deslop", skill_input={"args": "x"}),
                st(9, "mcp", server="github", tool="create_issue", arguments={"t": 1}),
                st(10, "mcp", server="solo", tool="", arguments={}),
            ],
        ),
        (
            "unparseable shell",
            [
                st(11, "shell", command="echo $(whoami) > /tmp/x"),
                st(12, "shell", command="$CMD run"),
                st(13, "shell", command="'unterminated"),
                st(14, "shell", command=""),
            ],
        ),
        (
            "paths",
            [
                st(15, "file_read", path="relative/p.txt"),
                st(16, "file_read", path="/"),
                st(17, "file_read", path="**/*.py"),
                st(18, "file_write", path="/w/dir/", content=""),
            ],
        ),
    )
    C.append(ing("ingest mixed", mixed))
    C.append(ing("ingest mixed json", mixed, "--json"))
    C.append(ing("ingest mixed dry run", mixed, "--dry-run"))
    C.append(ing("ingest mixed decomposition", mixed, "--allow-shell-decomposition"))
    C.append(
        ing(
            "ingest mixed decomposition json",
            mixed,
            "--allow-shell-decomposition",
            "--json",
        )
    )
    return C


# -- hook list and hook to-trace -----------------------------------------------


def cap(*records: dict[str, Any], at: float = 1_700_000_000.0) -> dict[str, Any]:
    out = []
    for k, r in enumerate(records):
        out.append(json.dumps(dict({"captured_at": at + k, "tool": "Bash"}, **r)))
    return {"text": "\n".join(out) + "\n"}


def caps(**sessions: Any) -> dict[str, Any]:
    return {f".opendaisugi/captures/{sid}.jsonl": v for sid, v in sessions.items()}


MAKE = {"step_type": "shell", "command": "make test"}
READ = {"step_type": "file_read", "path": "/work/src/a.py"}


def build_list_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def hl(name: str, before: dict[str, Any], *flags: str, **kw: Any) -> dict[str, Any]:
        c: dict[str, Any] = {
            "kind": "cli",
            "name": name,
            "argv": ["hook", "list", *flags],
            "before": {**LEX, **before},
            "env": CC,
        }
        c.update(kw)
        return c

    three = caps(
        a=cap(MAKE, READ, at=1_700_000_100.0),
        b=cap(MAKE, at=1_700_000_300.0),
        c=cap(READ, MAKE, READ, at=1_700_000_200.0),
    )
    for flags in ((), ("--json",)):
        sfx = " json" if flags else ""
        C.append(hl("hook list none" + sfx, {}, *flags))
        C.append(hl("hook list empty root" + sfx, {".opendaisugi/captures": {"dir": True}}, *flags))
        C.append(hl("hook list three" + sfx, three, *flags))
        C.append(
            hl(
                "hook list odd rows" + sfx,
                {
                    **caps(
                        blank={"text": "\n  \n\n"},
                        empty={"text": ""},
                        badfirst={"text": "{bad\n" + json.dumps({"captured_at": 5.0}) + "\n"},
                        badlast={"text": json.dumps({"captured_at": 5.0}) + "\n{bad\n"},
                        noat={"text": json.dumps({"tool": "x"}) + "\n"},
                        nullat={"text": json.dumps({"captured_at": None}) + "\n"},
                        intat={"text": json.dumps({"captured_at": 1_700_000_500}) + "\n"},
                        zero={"text": json.dumps({"captured_at": 0}) + "\n"},
                        neg={"text": json.dumps({"captured_at": -3.5}) + "\n"},
                        boolat={"text": json.dumps({"captured_at": True}) + "\n"},
                        small={"text": json.dumps({"captured_at": 1e-7}) + "\n"},
                        big={"text": json.dumps({"captured_at": 1e22}) + "\n"},
                        crlf={"text": '{"captured_at": 2.5}\r\n{"captured_at": 3.25}\r\n'},
                        firstonly={
                            "text": json.dumps({"captured_at": 1.0})
                            + "\n"
                            + json.dumps({"x": 1})
                            + "\n"
                        },
                    ),
                    ".opendaisugi/captures/notes.txt": {"text": "x"},
                    ".opendaisugi/captures/sub/deep.jsonl": cap(MAKE),
                },
                *flags,
            )
        )
        C.append(
            hl(
                "hook list string times" + sfx,
                caps(
                    s1={"text": json.dumps({"captured_at": "2026-01-02"}) + "\n"},
                    s2={"text": json.dumps({"captured_at": "2026-03-04"}) + "\n"},
                    s3={"text": json.dumps({"captured_at": ""}) + "\n"},
                ),
                *flags,
            )
        )
        C.append(
            hl(
                "hook list names" + sfx,
                {
                    ".opendaisugi/captures/.jsonl": cap(MAKE),
                    ".opendaisugi/captures/a.b.jsonl": cap(MAKE),
                    ".opendaisugi/captures/sp ace.jsonl": cap(MAKE),
                    ".opendaisugi/captures/é.jsonl": cap(MAKE),
                    ".opendaisugi/captures/" + "L" * 45 + ".jsonl": cap(MAKE),
                    ".opendaisugi/captures/x.JSONL": cap(MAKE),
                },
                *flags,
            )
        )
    C.append(
        hl(
            "hook list only string times",
            caps(
                s1={"text": json.dumps({"captured_at": "2026-01-02"}) + "\n"},
                s2={"text": json.dumps({"captured_at": "2026-03-04"}) + "\n"},
                s3={"text": json.dumps({"captured_at": "2026-02-01"}) + "\n"},
                s4={"text": json.dumps({"captured_at": "é"}) + "\n"},
            ),
        )
    )
    C.append(
        hl(
            "hook list many sessions",
            caps(
                **{
                    f"m{k:03d}": {
                        "text": json.dumps({"captured_at": 1_700_000_000 + (k * 37) % 50}) + "\n"
                    }
                    for k in range(70)
                }
            ),
        )
    )
    C.append(
        hl(
            "hook list nan among several",
            caps(
                a={"text": '{"captured_at": 3}\n'},
                b={"text": '{"captured_at": NaN}\n'},
                c={"text": '{"captured_at": 5}\n'},
                d={"text": '{"captured_at": 1}\n'},
                e={"text": '{"captured_at": NaN}\n'},
                f={"text": '{"captured_at": 4}\n'},
            ),
        )
    )
    C.append(
        hl(
            "hook list infinities and big ints",
            caps(
                a={"text": '{"captured_at": Infinity}\n'},
                b={"text": '{"captured_at": -Infinity}\n'},
                c={"text": '{"captured_at": 99999999999999999999999}\n'},
                d={"text": '{"captured_at": 1e22}\n'},
                e={"text": '{"captured_at": 10000000000000000000000}\n'},
            ),
            "--json",
        )
    )
    C.append(
        hl(
            "hook list mixed time types",
            caps(n={"text": '{"captured_at": 1.5}\n'}, s={"text": '{"captured_at": "x"}\n'}),
        )
    )
    C.append(hl("hook list first row a list", caps(a={"text": "[1]\n"})))
    C.append(hl("hook list last row a string", caps(a={"text": '{"captured_at": 1}\n"s"\n'})))
    C.append(
        hl("hook list not utf8", caps(a={"hex": (json.dumps(MAKE) + "\n").encode().hex() + "ff0a"}))
    )
    C.append(
        hl("hook list a directory named jsonl", {".opendaisugi/captures/d.jsonl": {"dir": True}})
    )
    C.append(
        hl("hook list captures root", {"caps/z.jsonl": cap(MAKE)}, "--captures-root", "{HOME}/caps")
    )
    C.append(
        hl(
            "hook list captures root is a file",
            {"caps": {"text": "x"}},
            "--captures-root",
            "{HOME}/caps",
        )
    )
    C.append(hl("hook list relative root", {"rel/r.jsonl": cap(MAKE)}, "--captures-root", "rel"))
    C.append(
        hl("hook list long integer", caps(a={"text": '{"captured_at": ' + "9" * 5000 + "}\n"}))
    )
    C.append(
        hl(
            "hook list nan time",
            caps(a={"text": '{"captured_at": NaN}\n'}, b={"text": '{"captured_at": 3}\n'}),
        )
    )
    C.append(hl("hook list nan time json", caps(a={"text": '{"captured_at": NaN}\n'}), "--json"))
    C.append(hl("hook list object time", caps(a={"text": '{"captured_at": {"t": 1}}\n'})))
    C.append(hl("hook list bad option", {}, "--bogus"))
    return C


def build_to_trace_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def tt(name: str, before: dict[str, Any], *args: str, **kw: Any) -> dict[str, Any]:
        c: dict[str, Any] = {
            "kind": "cli",
            "name": name,
            "argv": ["hook", "to-trace", *args],
            "before": {**LEX, **before},
            "env": CC,
        }
        c.update(kw)
        return c

    C.append(tt("to-trace converts", caps(s1=cap(MAKE, READ)), "s1"))
    C.append(tt("to-trace task", caps(s1=cap(MAKE)), "s1", "--task", "build the thing"))
    C.append(tt("to-trace empty task", caps(s1=cap(MAKE)), "s1", "--task", ""))
    C.append(
        tt(
            "to-trace every step type",
            caps(
                s=cap(
                    MAKE,
                    READ,
                    {"step_type": "file_write", "path": "/work/out/r.txt"},
                    {"step_type": "network", "url": "https://API.Example.com/v1?q=1"},
                    {
                        "step_type": "mcp",
                        "mcp_server": "fs",
                        "mcp_tool": "read",
                        "arguments": {"p": "/x"},
                    },
                    {"step_type": "task"},
                    {"step_type": "unknown"},
                )
            ),
            "s",
        )
    )
    compound = {"step_type": "shell", "command": "cd /work && make test | tee log"}
    redirect = {"step_type": "shell", "command": "echo hi > /tmp/out.txt"}
    C.append(tt("to-trace fails verify", caps(s=cap(compound, redirect, MAKE)), "s"))
    C.append(
        tt(
            "to-trace decomposition flag",
            caps(s=cap(compound, redirect)),
            "s",
            "--allow-shell-decomposition",
        )
    )
    C.append(
        tt(
            "to-trace decomposition config",
            {
                **caps(s=cap(compound)),
                CONFIG: {"text": "matcher_model: lexical\nshell_allow_decomposition: true\n"},
            },
            "s",
        )
    )
    C.append(
        tt(
            "to-trace no decomposition flag",
            {
                **caps(s=cap(compound)),
                CONFIG: {"text": "matcher_model: lexical\nshell_allow_decomposition: true\n"},
            },
            "s",
            "--no-allow-shell-decomposition",
        )
    )
    C.append(
        tt(
            "to-trace metachar and wrappers",
            caps(
                s=cap(
                    {"step_type": "shell", "command": "echo $(whoami)"},
                    {"step_type": "shell", "command": "$CMD run"},
                    {"step_type": "shell", "command": "sudo make install"},
                    {"step_type": "shell", "command": "bash -c 'ls'"},
                    {"step_type": "shell", "command": "'unterminated"},
                    {"step_type": "shell", "command": ""},
                )
            ),
            "s",
        )
    )
    C.append(tt("to-trace missing session", {}, "nope"))
    C.append(tt("to-trace missing session dotdot", {}, "../x"))
    C.append(tt("to-trace empty capture", caps(s={"text": "\n\n"}), "s"))
    C.append(tt("to-trace bad lines only", caps(s={"text": "{bad\n[1\n"}), "s"))
    C.append(
        tt("to-trace some bad lines", caps(s={"text": "{bad\n" + json.dumps(MAKE) + "\n"}), "s")
    )
    C.append(
        tt(
            "to-trace not utf8",
            caps(s={"hex": (json.dumps(MAKE) + "\n").encode().hex() + "ff0a"}),
            "s",
        )
    )
    C.append(tt("to-trace row a list", caps(s={"text": json.dumps(MAKE) + "\n[1]\n"}), "s"))
    C.append(tt("to-trace no step type", caps(s={"text": '{"tool": "Bash"}\n'}), "s"))
    C.append(
        tt(
            "to-trace nan argument",
            caps(
                s={
                    "text": '{"step_type": "mcp", "mcp_server": "a", "mcp_tool": "b", "arguments": {"x": NaN}}\n'
                }
            ),
            "s",
        )
    )
    C.append(tt("to-trace a directory", {".opendaisugi/captures/d.jsonl": {"dir": True}}, "d"))
    C.append(
        tt(
            "to-trace long integer",
            caps(s={"text": json.dumps(MAKE) + '\n{"n": ' + "1" * 4400 + "}\n"}),
            "s",
        )
    )
    C.append(
        tt(
            "to-trace step type number",
            caps(s={"text": '{"step_type": 5}\n' + json.dumps(MAKE) + "\n"}),
            "s",
        )
    )
    C.append(tt("to-trace dotted id", {".opendaisugi/captures/a.b.jsonl": cap(MAKE)}, "a.b"))
    C.append(
        tt("to-trace subdirectory id", {".opendaisugi/captures/sub/x.jsonl": cap(MAKE)}, "sub/x")
    )
    C.append(
        tt(
            "to-trace captures root",
            {"caps/z.jsonl": cap(MAKE)},
            "z",
            "--captures-root",
            "{HOME}/caps",
        )
    )
    C.append(tt("to-trace data dir", caps(s=cap(MAKE)), "s", "--data-dir", "{HOME}/dd"))
    C.append(
        tt(
            "to-trace data dir config",
            {
                **caps(s=cap(compound)),
                "dd/config.yaml": {"text": "shell_allow_decomposition: true\n"},
            },
            "s",
            "--data-dir",
            "{HOME}/dd",
        )
    )
    C.append(tt("to-trace no argument", {}))
    C.append(
        tt(
            "to-trace config invalid",
            {**caps(s=cap(MAKE)), CONFIG: {"text": "shell_allow_decomposition: maybe\n"}},
            "s",
        )
    )
    C.append(
        tt(
            "to-trace existing journal",
            {
                **caps(s=cap(MAKE)),
                ".opendaisugi": {
                    "journal": {
                        "traces": [],
                        "converted": [["s", "2020-01-01-00000000"]],
                    }
                },
            },
            "s",
        )
    )
    C.append(
        tt(
            "to-trace paths",
            caps(
                s=cap(
                    {"step_type": "file_read", "path": "relative/p.txt"},
                    {"step_type": "file_read", "path": "/"},
                    {"step_type": "file_read", "path": "/work/../etc/x"},
                    {"step_type": "file_write", "path": "/work/dir/"},
                    {"step_type": "file_read", "path": "~/notes"},
                    {"step_type": "network", "url": "ftp://x.example/a"},
                    {"step_type": "network", "url": "not a url"},
                )
            ),
            "s",
        )
    )
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = (
        build_claude_cases()
        + build_codex_cases()
        + build_format_cases()
        + build_split_cases()
        + build_onboard_cases()
        + build_ingest_cases()
        + build_list_cases()
        + build_to_trace_cases()
    )
    seen: set[str] = set()
    for c in cases:
        if c["name"] in seen:
            raise SystemExit(f"duplicate case name: {c['name']}")
        seen.add(c["name"])
    return cases


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument("--fresh", action="store_true", help="rerun every case")
    ap.add_argument(
        "--max", type=int, default=0, help="stop after running this many cases (0: no limit)"
    )
    args = ap.parse_args()
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out / "cases.jsonl").exists() and not args.fresh:
        for ln in (out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    cache_path = SCRATCH / "gen-cache.jsonl"
    if cache_path.exists() and not args.fresh:
        for ln in cache_path.read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old.setdefault(c["id"], c)
    ran = 0
    missing = 0
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        if prev is not None and not args.only:
            c["expect"] = prev["expect"]
            if "model" in prev:
                c["model"] = prev["model"]
            continue
        if args.max and ran >= args.max:
            missing += 1
            continue
        ran += 1
        work = SCRATCH / "gen" / f"{i:04d}"
        if c.get("replies"):
            full = expand(c)
            record_replies(full, work)
            c["model"] = full["model"]
        c["expect"] = run(c, cmd_for(c, None), work)
        if any(r.get("key_ok") is False for r in c["expect"]["requests"]):
            raise SystemExit(f"{c['name']}: the oracle sent a credential that is not the case's")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:8000]
            )
        else:
            with cache_path.open("a", encoding="utf-8") as fh:
                rec = {"id": body_id(c), "expect": c["expect"]}
                if "model" in c:
                    rec["model"] = c["model"]
                fh.write(json.dumps(rec) + "\n")
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    if missing:
        print(f"{missing} cases still to run; run again to go on")
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out / "cases.jsonl", cases)
    (out / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    cache_path.unlink(missing_ok=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
