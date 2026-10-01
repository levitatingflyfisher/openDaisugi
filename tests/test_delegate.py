"""The delegate library: rules, the measure of a file, the router's worker
choice, the quote check and the journal."""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from opendaisugi import delegate
from opendaisugi.llm_client import Reply
from opendaisugi.models import Envelope

RULE = {
    "id": "big-read",
    "version": 1,
    "shape": "deny_redirect",
    "state": "active",
    "match": {"tool": "Read", "file_lines_over": 3},
}


def _env(**perms) -> Envelope:
    p = {"file_read": ["/**"], **perms}
    return Envelope(generated_by="t", task="t", permissions=p)


# --- rules --------------------------------------------------------------


def test_parse_rule_defaults_and_refusals():
    r = delegate.parse_rule({**RULE, "match": {"tool": "Read"}}, "a.json")
    assert isinstance(r, delegate.Rule)
    assert r.min_lines == delegate.DEFAULT_MIN_LINES and r.allow_remote is False
    assert delegate.parse_rule([], "a") == "not a JSON object"
    assert "shape" in delegate.parse_rule({**RULE, "shape": "shell_rewrite"}, "a")
    assert "file_lines_over" in delegate.parse_rule(
        {**RULE, "match": {"tool": "Read", "file_lines_over": True}}, "a"
    )
    assert "match.tool" in delegate.parse_rule({**RULE, "match": {"tool": "Grep"}}, "a")
    assert "allow_remote" in delegate.parse_rule({**RULE, "worker": {"allow_remote": 1}}, "a")
    assert "id" in delegate.parse_rule({**RULE, "id": "a/b"}, "a")


def test_load_rules_by_file_name(tmp_path):
    d = tmp_path / "grafts"
    d.mkdir()
    (d / "b.json").write_text(json.dumps({**RULE, "id": "b"}))
    (d / "a.json").write_text(json.dumps({**RULE, "id": "a", "state": "retired"}))
    (d / "c.json").write_text("{")
    (d / "note.txt").write_text("x")
    rules, bad = delegate.load_rules(tmp_path)
    assert [r.id for r in rules] == ["a", "b"]
    assert bad == [("c.json", "not readable JSON")]
    assert delegate.acting_rule(tmp_path).id == "b"


# --- measure --------------------------------------------------------------


def test_count_lines():
    assert delegate.count_lines(b"") == 0
    assert delegate.count_lines(b"a") == 1
    assert delegate.count_lines(b"a\n") == 1
    assert delegate.count_lines(b"a\nb") == 2
    # Only \n counts: \r and the other Unicode breaks do not.
    assert delegate.count_lines(b"a\rb\x0bc\xc2\x85d\n") == 1


def test_measure_refuses_what_is_not_regular_text(tmp_path):
    good = tmp_path / "f.txt"
    good.write_text("one\ntwo\n")
    m = delegate.measure(str(good))
    assert isinstance(m, delegate.Measure) and m.lines == 2 and m.size == 8
    (tmp_path / "nul").write_bytes(b"a\x00b")
    assert "NUL" in delegate.measure(str(tmp_path / "nul"))
    (tmp_path / "latin").write_bytes(b"caf\xe9")
    assert "UTF-8" in delegate.measure(str(tmp_path / "latin"))
    big = tmp_path / "big"
    big.write_bytes(b"x" * (delegate.MAX_DELEGATE_BYTES + 1))
    assert "larger than" in delegate.measure(str(big))
    assert delegate.measure(str(tmp_path)) == "not a regular file"
    fifo = tmp_path / "fifo"
    os.mkfifo(fifo)
    assert delegate.measure(str(fifo)) == "not a regular file"
    assert delegate.measure(str(tmp_path / "missing")) == "the file cannot be opened"


# --- the router's worker -----------------------------------------------------


@pytest.mark.parametrize(
    "host,want",
    [
        ("localhost", True),
        ("::1", True),
        ("127.0.0.1", True),
        ("127.255.9.1", True),
        ("127.0.0.256", False),
        ("127.01.0.1", False),
        ("128.0.0.1", False),
        ("localhost.", False),
        ("worker.invalid", False),
    ],
)
def test_is_loopback(host, want):
    assert delegate.is_loopback(host) is want


def _tier1(tmp_path: Path, model: str, base_url: str | None) -> None:
    (tmp_path / "local_tier1.json").write_text(json.dumps({"model": model, "base_url": base_url}))


def test_route_no_tier1(tmp_path):
    r = delegate.route_delegate(tmp_path, None, allow_remote=False, env={})
    assert not r.ok and "no local model is set up" in r.reason


def test_route_local(tmp_path):
    _tier1(tmp_path, "qwen", "http://127.0.0.1:9/v1")
    r = delegate.route_delegate(tmp_path, None, allow_remote=False, env={})
    assert r.ok and r.tier == "local" and r.model == "openai/qwen" and r.host == "127.0.0.1"


def test_route_ollama_default_is_local(tmp_path):
    _tier1(tmp_path, "ollama/qwen", None)
    r = delegate.route_delegate(tmp_path, None, allow_remote=False, env={})
    assert r.ok and r.host == "localhost"


def test_route_remote_needs_rule_and_explicit_grant(tmp_path):
    _tier1(tmp_path, "qwen", "http://worker.invalid:8080/v1")
    r = delegate.route_delegate(tmp_path, _env(), allow_remote=False, env={})
    assert not r.ok and "rule does not allow a remote worker" in r.reason
    r = delegate.route_delegate(tmp_path, _env(network=True), allow_remote=True, env={})
    assert not r.ok and "envelope does not grant it" in r.reason
    r = delegate.route_delegate(
        tmp_path, _env(network=True, network_hosts=["worker.invalid"]), allow_remote=True, env={}
    )
    assert r.ok and r.tier == "remote"
    r = delegate.route_delegate(tmp_path, None, allow_remote=True, env={})
    assert not r.ok


def test_route_physical_stakes(tmp_path):
    _tier1(tmp_path, "qwen", "http://127.0.0.1:9/v1")
    env = Envelope(generated_by="t", task="t", stakes="physical", permissions={})
    r = delegate.route_delegate(tmp_path, env, allow_remote=False, env={})
    assert not r.ok and "physical" in r.reason


def test_route_messages_wire_without_key(tmp_path):
    _tier1(tmp_path, "anthropic/claude-haiku-4-5", None)
    r = delegate.route_delegate(tmp_path, None, allow_remote=True, env={})
    assert not r.ok and "ANTHROPIC_API_KEY" in r.reason


# --- the quote check ---------------------------------------------------------


def test_check_reply_drops_quotes_not_in_the_file():
    text = "alpha beta\ngamma delta\n"
    reply = json.dumps(
        {"answer": "a", "quotes": ["alpha beta", "not here", 7, "", "  ", "alpha beta", "gamma"]}
    )
    got = delegate.check_reply(reply, text)
    assert got.quotes == ["alpha beta", "gamma"]
    assert got.dropped == 4


def test_check_reply_fence_and_errors():
    got = delegate.check_reply('```json\n{"answer": "x", "quotes": []}\n```', "f")
    assert got.answer == "x"
    assert "not the JSON" in delegate.check_reply("nope", "f")
    assert "answer" in delegate.check_reply('{"quotes": []}', "f")
    assert "not a list" in delegate.check_reply('{"answer": "a", "quotes": "x"}', "f")
    long = delegate.check_reply(json.dumps({"answer": "y" * 5000}), "f")
    assert long.cut and len(long.answer) == delegate.MAX_ANSWER_CHARS


# --- the whole call ------------------------------------------------------------


def _setup(tmp_path: Path, lines: int = 5) -> Path:
    f = tmp_path / "work" / "big.py"
    f.parent.mkdir()
    f.write_text("".join(f"line {i}\n" for i in range(lines)))
    (tmp_path / "gate" / "grafts").mkdir(parents=True)
    (tmp_path / "gate" / "grafts" / "r.json").write_text(json.dumps(RULE))
    return f


def test_run_delegate_ok_and_journal(tmp_path, monkeypatch):
    f = _setup(tmp_path)
    _tier1(tmp_path, "qwen", "http://127.0.0.1:9/v1")
    seen = {}

    def fake_complete(model, messages, **kw):
        seen["model"], seen["messages"], seen["kw"] = model, messages, kw
        return Reply(
            json.dumps({"answer": "it counts", "quotes": ["line 3", "line 99"]}),
            input_tokens=40,
            output_tokens=9,
        )

    monkeypatch.setattr("opendaisugi.llm_client.complete", fake_complete)
    res = delegate.run_delegate(str(f), "what?", "bulk_read", data_dir=tmp_path, env={})
    assert res.ok and res.quotes == ["line 3"] and res.dropped == 1
    assert res.untrusted == delegate.UNTRUSTED_NOTE
    assert "at most 3 lines" in res.exact_text
    assert seen["model"] == "openai/qwen"
    assert seen["kw"]["json_object"] is True
    assert "File name: big.py" in seen["messages"][1]["content"]
    rows = delegate.load_records(tmp_path)
    assert len(rows) == 1 and rows[0]["ok"] is True and rows[0]["worker_dollars"] == 0.0
    assert rows[0]["quotes"] == 1 and rows[0]["dropped"] == 1
    assert rows[0]["estimated"] is True and rows[0]["task_ok"] is None
    assert oct(os.stat(delegate.journal_path(tmp_path)).st_mode & 0o777) == "0o600"


@pytest.mark.parametrize(
    "path,question,mode,want",
    [
        ("rel.py", "q", "bulk_read", "absolute path"),
        (None, "q", "bulk_read", "absolute path"),
        ("/x", "", "bulk_read", "question is empty"),
        ("/x", "q", "edit", "is not built"),
        ("/nonexistent/x", "q", "bulk_read", "cannot be opened"),
    ],
)
def test_run_delegate_refusals(tmp_path, path, question, mode, want):
    res = delegate.run_delegate(path, question, mode, data_dir=tmp_path, env={})
    assert not res.ok and want in res.reason
    assert len(delegate.load_records(tmp_path)) == 1


def test_run_delegate_no_worker_and_remote_refused(tmp_path):
    f = _setup(tmp_path)
    res = delegate.run_delegate(str(f), "q", "bulk_read", data_dir=tmp_path, env={})
    assert not res.ok and res.reason.startswith("no worker")
    _tier1(tmp_path, "qwen", "http://worker.invalid:8080/v1")
    res = delegate.run_delegate(str(f), "q", "bulk_read", data_dir=tmp_path, env={})
    assert not res.ok and "remote worker" in res.reason


@pytest.mark.parametrize(
    "url,want",
    [
        ("http://127.0.0.1:9/v1/chat/completions", "127.0.0.1"),
        ("http://u:p@Worker.Invalid:8/x", "worker.invalid"),
        ("http://[::1]:11434/v1", "::1"),
        ("http://[::1/x", None),
        ("http:///x", None),
        ("no-scheme", None),
        ("http://h?q=a@b", "h"),
    ],
)
def test_host_of(url, want):
    assert delegate._host_of(url) == want


def test_measure_a_path_with_a_nul_byte():
    assert delegate.measure("/tmp/a\x00b") == "the file cannot be opened"
    assert delegate.measure("/tmp/\udcff\ud800") == "the file cannot be opened"
