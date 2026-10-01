"""The delegate's code-write mode: the worker returns a draft, the tool
writes nothing.

A draft is a whole file or a unified diff against the target. A diff is
applied to the current file in memory: each hunk's old lines (context and
removed) must match the file exactly once, after the hunk before it. A
diff that does not apply is returned marked as not applying. The draft
goes back fenced, as untrusted data, with the target path; the frontier
applies it with its own gated Write or Edit.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi import delegate
from opendaisugi.gate import evaluate_call, register_envelope
from opendaisugi.llm_client import Reply
from opendaisugi.models import Envelope

TEXT = "def a():\n    return 1\n\n\ndef b():\n    return 2\n"


# --- the diff applier -----------------------------------------------------


def test_a_diff_applies_by_its_lines_not_its_numbers():
    diff = "--- a/x.py\n+++ b/x.py\n@@ -90,2 +90,2 @@\n def b():\n-    return 2\n+    return 3\n"
    got = delegate.apply_diff(diff, TEXT)
    assert got.applies and got.why is None
    assert got.text == TEXT.replace("return 2", "return 3")


def test_hunks_apply_in_order():
    diff = "@@\n def a():\n-    return 1\n+    return 10\n@@\n def b():\n-    return 2\n+    return 20\n"
    got = delegate.apply_diff(diff, TEXT)
    assert got.applies
    assert "return 10" in got.text and "return 20" in got.text


def test_a_blank_body_line_is_an_empty_context_line():
    diff = "@@ -1 +1 @@\n     return 1\n\n\n def b():\n"
    assert delegate.apply_diff(diff, TEXT).applies


@pytest.mark.parametrize(
    "diff,why",
    [
        ("", "the diff has no hunk"),
        ("--- a\n+++ b\n", "the diff has no hunk"),
        ("hello\n@@\n def a():\n", "line 1 before the first hunk is not a diff header"),
        ("@@\n def c():\n", "hunk 1 does not match the file"),
        ("@@\n+new\n", "hunk 1 has no context or removed lines, so it has no place in the file"),
        ("@@\n def a():\n*bad\n", "line 3 is not a context, removed or added line"),
        (
            "@@\n-    return 1\n+    return 9\n\\ No newline at end of file\n",
            "line 4 is a \\ line (no newline at the end), which the applier does not read; "
            "send a whole file instead",
        ),
        ("@@\n-\n", "hunk 1 matches 3 places in the file"),
        ("@@\n def b():\n@@\n def a():\n", "hunk 2 does not match the file"),
    ],
)
def test_a_diff_that_does_not_apply_says_why(diff, why):
    got = delegate.apply_diff(diff, TEXT)
    assert not got.applies and got.why == why and got.text is None


def test_line_endings_are_literal():
    crlf = TEXT.replace("\n", "\r\n")
    assert not delegate.apply_diff("@@\n def a():\n", crlf).applies
    assert delegate.apply_diff("@@\n def a():\r\n", crlf).applies


# --- the fence -------------------------------------------------------------


@pytest.mark.parametrize(
    "text,info,want",
    [
        ("x\n", "", "```\nx\n```"),
        ("x", "diff", "```diff\nx\n```"),
        ("a ``` b\n", "", "````\na ``` b\n````"),
        ("````` five\n", "diff", "``````diff\n````` five\n``````"),
    ],
)
def test_the_fence_is_longer_than_any_backtick_run(text, info, want):
    assert delegate.fence(text, info) == want


# --- the reply ---------------------------------------------------------------


def test_check_draft_reads_the_form_and_the_text():
    d = delegate.check_draft(json.dumps({"form": "file", "text": "x = 1\n"}), TEXT)
    assert d.form == "file" and d.text == "x = 1\n" and d.applies and d.why is None
    d = delegate.check_draft(
        "```json\n" + json.dumps({"form": "diff", "text": "@@\n def c():\n"}) + "\n```", TEXT
    )
    assert d.form == "diff" and not d.applies and d.why == "hunk 1 does not match the file"


@pytest.mark.parametrize(
    "reply,why",
    [
        ("no json", "the worker's reply is not the JSON object asked for"),
        ("[1]", "the worker's reply is not the JSON object asked for"),
        (
            json.dumps({"form": "patch", "text": "x"}),
            "the worker's reply has no form of file or diff",
        ),
        (json.dumps({"form": "file"}), "the worker's reply has no draft text"),
        (json.dumps({"form": "file", "text": 5}), "the worker's reply has no draft text"),
    ],
)
def test_check_draft_errors(reply, why):
    assert delegate.check_draft(reply, TEXT) == why


def test_a_diff_against_no_file_does_not_apply():
    d = delegate.check_draft(json.dumps({"form": "diff", "text": "@@\n+x\n"}), None)
    assert not d.applies and d.why == "there is no file to apply a diff to"


def test_a_draft_too_long_is_refused():
    big = "x" * (delegate.MAX_DRAFT_CHARS + 1)
    assert delegate.check_draft(json.dumps({"form": "file", "text": big}), None) == (
        f"the draft is longer than {delegate.MAX_DRAFT_CHARS} characters"
    )


# --- the tool --------------------------------------------------------------


def _tier1(tmp_path: Path) -> None:
    (tmp_path / "local_tier1.json").write_text(
        json.dumps({"model": "qwen", "base_url": "http://127.0.0.1:9/v1"})
    )


def _fake(monkeypatch, reply: dict, seen: dict) -> None:
    def fake_complete(model, messages, **kw):
        seen["messages"], seen["kw"] = messages, kw
        return Reply(json.dumps(reply), input_tokens=30, output_tokens=12)

    monkeypatch.setattr("opendaisugi.llm_client.complete", fake_complete)


def test_code_write_returns_a_fenced_diff_and_writes_nothing(tmp_path, monkeypatch):
    f = tmp_path / "x.py"
    f.write_text(TEXT)
    _tier1(tmp_path)
    seen: dict = {}
    diff = "@@\n def b():\n-    return 2\n+    return 3\n"
    _fake(monkeypatch, {"form": "diff", "text": diff}, seen)
    res = delegate.run_delegate(str(f), "make b return 3", "code_write", data_dir=tmp_path, env={})
    assert res.ok and res.mode == "code_write" and res.path == str(f)
    assert res.form == "diff" and res.applies is True and res.apply_reason is None
    assert res.draft == "```diff\n" + diff + "```"
    assert res.untrusted == delegate.DRAFT_NOTE
    assert res.answer is None and res.quotes == []
    assert f.read_text() == TEXT
    assert seen["kw"]["max_tokens"] == delegate.WRITER_MAX_TOKENS
    assert seen["messages"][0]["content"] == delegate.WRITER_SYSTEM
    assert "Request: make b return 3" in seen["messages"][1]["content"]
    assert "<file>\n" + TEXT + "\n</file>" in seen["messages"][1]["content"]
    rows = delegate.load_records(tmp_path)
    assert rows[-1]["mode"] == "code_write" and rows[-1]["ok"] is True
    assert rows[-1]["form"] == "diff" and rows[-1]["applied"] is True
    assert rows[-1]["worker_input_tokens"] == 30 and rows[-1]["worker_output_tokens"] == 12
    assert rows[-1]["frontier_tokens_kept"] is None and rows[-1]["estimated"] is False


def test_a_diff_that_does_not_apply_is_still_returned_marked(tmp_path, monkeypatch):
    f = tmp_path / "x.py"
    f.write_text(TEXT)
    _tier1(tmp_path)
    _fake(monkeypatch, {"form": "diff", "text": "@@\n def z():\n"}, {})
    res = delegate.run_delegate(str(f), "q", "code_write", data_dir=tmp_path, env={})
    assert res.ok and res.applies is False and res.apply_reason == "hunk 1 does not match the file"
    assert delegate.load_records(tmp_path)[-1]["applied"] is False


def test_a_new_file_is_written_whole(tmp_path, monkeypatch):
    _tier1(tmp_path)
    seen: dict = {}
    _fake(monkeypatch, {"form": "file", "text": "print(1)\n"}, seen)
    target = tmp_path / "new" / "y.py"
    res = delegate.run_delegate(str(target), "print one", "code_write", data_dir=tmp_path, env={})
    assert res.ok and res.form == "file" and res.applies is True and res.lines is None
    assert res.draft == "```\nprint(1)\n```"
    assert "The file does not exist yet." in seen["messages"][1]["content"]
    assert not target.exists()


def test_a_target_that_exists_but_is_not_text_is_refused(tmp_path):
    d = tmp_path / "dir"
    d.mkdir()
    res = delegate.run_delegate(str(d), "q", "code_write", data_dir=tmp_path, env={})
    assert not res.ok and res.reason == "the file cannot be delegated: not a regular file"


def test_a_symlinked_target_is_refused_and_not_read(tmp_path, monkeypatch):
    # The gate may have seen no file there; a parallel call can plant a
    # link before the tool reads. The tool opens the target without
    # following a link.
    secret = tmp_path / "secret.txt"
    secret.write_text("token\n")
    link = tmp_path / "x.py"
    link.symlink_to(secret)
    _tier1(tmp_path)
    seen: dict = {}
    _fake(monkeypatch, {"form": "file", "text": "x\n"}, seen)
    res = delegate.run_delegate(str(link), "q", "code_write", data_dir=tmp_path, env={})
    assert not res.ok and res.reason == "the file cannot be delegated: it is a symbolic link"
    assert seen == {}


def test_a_dangling_symlinked_target_is_refused(tmp_path):
    link = tmp_path / "x.py"
    link.symlink_to(tmp_path / "none")
    res = delegate.run_delegate(str(link), "q", "code_write", data_dir=tmp_path, env={})
    assert not res.ok and res.reason == "the file cannot be delegated: it is a symbolic link"


def test_a_bulk_read_still_follows_a_link(tmp_path):
    f = tmp_path / "x.py"
    f.write_text(TEXT)
    link = tmp_path / "l.py"
    link.symlink_to(f)
    m = delegate.measure(str(link))
    assert not isinstance(m, str) and m.text == TEXT


def test_a_draft_at_the_limit_counts_characters_not_bytes():
    text = "\u00e9" + "x" * (delegate.MAX_DRAFT_CHARS - 1)
    d = delegate.check_draft(json.dumps({"form": "file", "text": text}), None)
    assert not isinstance(d, str) and d.applies


def test_an_unknown_mode_names_both(tmp_path):
    res = delegate.run_delegate("/x", "q", "edit", data_dir=tmp_path, env={})
    assert res.reason == "mode 'edit' is not built; the modes are bulk_read and code_write"


# --- the gate ---------------------------------------------------------------


def _delegate_call(path: str, mode: str) -> dict:
    return {
        "session_id": "s1",
        "tool_name": "mcp__opendaisugi__delegate",
        "tool_input": {"path": path, "question": "q", "mode": mode},
        "cwd": "/w",
    }


@pytest.fixture
def root(tmp_path):
    r = tmp_path / "data" / "gate"
    env = Envelope(
        generated_by="t",
        task="t",
        permissions={
            "file_read": [str(tmp_path / "in") + "/**"],
            "mcp_allowlist": ["opendaisugi/*"],
        },
    )
    register_envelope(env, root=r)
    return r


def test_the_gate_checks_a_read_of_an_existing_target(tmp_path, root):
    out = tmp_path / "out.py"
    out.write_text("x\n")
    env = Envelope.model_validate_json((root / "envelopes" / "default.json").read_text())
    d = evaluate_call(_delegate_call(str(out), "code_write"), env, mode="enforce", root=root)
    assert d.would_deny and d.reason.startswith(f"delegate reads {out}")


def test_a_new_target_needs_no_read(tmp_path, root):
    env = Envelope.model_validate_json((root / "envelopes" / "default.json").read_text())
    new = tmp_path / "elsewhere" / "new.py"
    d = evaluate_call(_delegate_call(str(new), "code_write"), env, mode="enforce", root=root)
    assert not d.would_deny, d.reason
    d = evaluate_call(_delegate_call(str(new), "bulk_read"), env, mode="enforce", root=root)
    assert d.would_deny


def test_a_refused_code_write_is_not_an_estimate(tmp_path):
    res = delegate.run_delegate("rel.py", "q", "code_write", data_dir=tmp_path, env={})
    assert not res.ok
    assert delegate.load_records(tmp_path)[-1]["estimated"] is False
