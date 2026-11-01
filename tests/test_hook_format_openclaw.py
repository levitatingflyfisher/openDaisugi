import pytest

from opendaisugi.hook import _classify_tool, _payload_to_record

PATCH = "*** Begin Patch\n*** Add File: /w/new.txt\n+hi\n*** End Patch"


def openclaw(tool, args, **extra):
    # What the shipped plugin sends to `daisugi hook record --format openclaw`.
    return {"tool": tool, "args": args, "session_id": "oc-1", "tool_use_id": "tc-1", **extra}


@pytest.mark.parametrize(
    "name,kind",
    [
        ("exec", "shell"),
        ("read", "file_read"),
        ("write", "file_write"),
        ("edit", "file_write"),
        ("apply_patch", "file_write"),
        ("web_fetch", "network"),
        ("web_search", "network"),
    ],
)
def test_openclaw_names_classify(name, kind):
    assert _classify_tool(name, fmt="openclaw") == kind


def test_openclaw_unknown_tool_is_dropped():
    assert _payload_to_record(openclaw("message", {"action": "send"}), fmt="openclaw") is None


def test_openclaw_exec_record():
    rec = _payload_to_record(openclaw("exec", {"command": "ls", "workdir": "/w"}), fmt="openclaw")
    assert rec["step_type"] == "shell" and rec["command"] == "ls" and rec["tool_use_id"] == "tc-1"


def test_openclaw_apply_patch_names_its_first_file():
    rec = _payload_to_record(openclaw("apply_patch", {"patch": PATCH}), fmt="openclaw")
    assert rec["step_type"] == "file_write" and rec["path"] == "/w/new.txt"


def test_openclaw_web_fetch_url():
    got = _payload_to_record(openclaw("web_fetch", {"url": "https://x/y"}), fmt="openclaw")
    assert got["url"] == "https://x/y"
