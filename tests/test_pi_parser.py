import json

from opendaisugi.parsers import get_parser


def line(**d):
    return json.dumps(d)


def session(tmp_path, *entries):
    p = tmp_path / "s.jsonl"
    p.write_text(
        "\n".join([line(type="session", version=3, id="sess", timestamp="t", cwd="/w"), *entries])
        + "\n"
    )
    return p


def user(i, text, parent=None):
    return line(
        type="message",
        id=i,
        parentId=parent,
        timestamp="t",
        message={"role": "user", "content": [{"type": "text", "text": text}], "timestamp": 1},
    )


def asst(i, parent, *calls):
    return line(
        type="message",
        id=i,
        parentId=parent,
        timestamp="t",
        message={"role": "assistant", "content": list(calls), "timestamp": 1},
    )


def call(name, **args):
    return {"type": "toolCall", "id": f"c-{name}", "name": name, "arguments": args}


def result(i, parent, name):
    return line(
        type="message",
        id=i,
        parentId=parent,
        timestamp="t",
        message={
            "role": "toolResult",
            "toolCallId": f"c-{name}",
            "toolName": name,
            "content": [],
            "isError": False,
        },
    )


def test_pi_session_becomes_one_episode_with_mapped_tools(tmp_path):
    p = session(
        tmp_path,
        user("u1", "fix the build"),
        asst(
            "a1",
            "u1",
            call("bash", command="make"),
            call("read", path="/w/a.py"),
            call("write", path="/w/b.py", content="x"),
        ),
        result("r1", "a1", "bash"),
        line(type="usage", id="x", parentId="a1", timestamp="t", input=1),
    )
    res = get_parser("pi", min_tools=1).parse(p)
    assert res.source == "pi"
    assert len(res.episodes) == 1
    ep = res.episodes[0]
    assert ep.task == "fix the build"
    assert [s.type for s in ep.steps] == ["shell", "file_read", "file_write"]
    assert ep.steps[0].command == "make"


def test_pi_unknown_tool_is_mcp_under_pi(tmp_path):
    p = session(tmp_path, user("u1", "t"), asst("a1", "u1", call("grep", path="/w")))
    ep = get_parser("pi", min_tools=1).parse(p).episodes[0]
    assert ep.steps[0].type == "file_read"
    p = session(tmp_path, user("u1", "t"), asst("a1", "u1", call("deploy", target="x")))
    ep = get_parser("pi", min_tools=1).parse(p).episodes[0]
    assert ep.steps[0].type == "mcp" and ep.steps[0].server == "pi" and ep.steps[0].tool == "deploy"


def test_pi_tree_ids_survive(tmp_path):
    from opendaisugi.parsers.pi import PiParser

    p = session(tmp_path, user("u1", "t"), asst("a1", "u1", call("bash", command="ls")))
    msgs = PiParser()._read_messages(p)
    assert [(m["uuid"], m["parentUuid"], m["sessionId"]) for m in msgs] == [
        ("u1", None, "sess"),
        ("a1", "u1", "sess"),
    ]


def test_pi_bad_lines_are_skipped(tmp_path):
    p = tmp_path / "s.jsonl"
    p.write_text(
        "{nope\n" + user("u1", "t") + "\n" + asst("a1", "u1", call("bash", command="ls")) + "\n"
    )
    assert len(get_parser("pi", min_tools=1).parse(p).episodes) == 1
