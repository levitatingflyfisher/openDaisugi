import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.parsers import get_parser

runner = CliRunner()


def w(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(obj if isinstance(obj, str) else json.dumps(obj))


def tree(tmp_path, messages, parts):
    """Write storage/session|message|part files; return the session file."""
    st = tmp_path / "storage"
    sess = st / "session" / "p" / "ses_1.json"
    w(sess, {"id": "ses_1", "projectID": "p"})
    for mid, msg in messages.items():
        w(st / "message" / "ses_1" / f"{mid}.json", {"id": mid, "sessionID": "ses_1", **msg})
    for mid, plist in parts.items():
        for pid, part in plist.items():
            w(st / "part" / mid / f"{pid}.json", {"id": pid, "messageID": mid, **part})
    return sess


def tool(name, status="completed", **inp):
    return {"type": "tool", "callID": "c", "tool": name, "state": {"status": status, "input": inp}}


def basic(tmp_path):
    return tree(
        tmp_path,
        {
            "msg_2": {"role": "assistant", "time": {"created": 2}},
            "msg_1": {"role": "user", "time": {"created": 1}},
        },
        {
            "msg_1": {"prt_1": {"type": "text", "text": "fix the build"}},
            "msg_2": {
                "prt_1": tool("bash", command="make"),
                "prt_2": tool("read", filePath="/w/a.py"),
                "prt_3": tool("bash", status="error", command="boom"),
                "prt_4": tool("bash", status="pending", command="later"),
            },
        },
    )


def test_opencode_session_becomes_one_episode(tmp_path):
    res = get_parser("opencode", min_tools=1).parse(basic(tmp_path))
    assert res.source == "opencode"
    ep = res.episodes[0]
    assert ep.task == "fix the build"
    assert [s.type for s in ep.steps] == ["shell", "file_read"]
    assert ep.steps[1].path == "/w/a.py"


def test_opencode_unknown_tool_is_mcp(tmp_path):
    sess = tree(
        tmp_path,
        {
            "m1": {"role": "user", "time": {"created": 1}},
            "m2": {"role": "assistant", "time": {"created": 2}},
        },
        {"m1": {"p": {"type": "text", "text": "t"}}, "m2": {"p": tool("deploy", target="x")}},
    )
    ep = get_parser("opencode", min_tools=1).parse(sess).episodes[0]
    assert ep.steps[0].type == "mcp" and ep.steps[0].server == "opencode"


def test_opencode_bad_part_file_is_skipped(tmp_path):
    sess = basic(tmp_path)
    (tmp_path / "storage" / "part" / "msg_2" / "prt_0.json").write_text("{nope")
    assert len(get_parser("opencode", min_tools=1).parse(sess).episodes[0].steps) == 2


def test_opencode_missing_messages_is_a_parse_error(tmp_path):
    sess = tmp_path / "storage" / "session" / "p" / "ses_9.json"
    w(sess, {"id": "ses_9"})
    with pytest.raises(ValueError, match="no OpenCode messages for session ses_9"):
        get_parser("opencode", min_tools=1).parse(sess)
    res = runner.invoke(
        app, ["journal", "parse", str(sess), "-o", str(tmp_path / "o.json"), "--format", "opencode"]
    )
    assert res.exit_code == 2 and "Parse error: no OpenCode messages" in res.output


def test_opencode_id_inside_the_file_is_never_a_path(tmp_path):
    sess = tree(
        tmp_path,
        {"msg_1": {"role": "user", "time": {"created": 1}}},
        {"msg_1": {"prt_1": {"type": "text", "text": "fix the build"}}},
    )
    msg = tmp_path / "storage" / "message" / "ses_1" / "msg_1.json"
    msg.write_text(json.dumps({"id": "../../outside/leak", "role": "user", "time": {"created": 1}}))
    w(tmp_path / "outside" / "leak" / "prt.json", {"type": "text", "text": "LEAKED"})
    eps = get_parser("opencode").parse(sess)
    text = repr(eps)
    assert "LEAKED" not in text and "fix the build" in text
