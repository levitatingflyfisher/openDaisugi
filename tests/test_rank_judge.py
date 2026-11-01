"""rank judge: two swapped calls per pair, the author hidden, no vote on error."""

from __future__ import annotations

import hashlib
import json

import pytest
from typer.testing import CliRunner

from opendaisugi import agent_profiles, rank, rank_judge
from opendaisugi.cli import app


def _att(aid, author="", text=None):
    return {
        "id": aid,
        "content_hash": f"h-{aid}",
        "author": author,
        "where": {"kind": "output", "text": text or f"answer {aid}"},
    }


def _doc(*atts, judges=("local/judge-9b",)):
    return {
        "ranking_id": "r1",
        "task": "add two numbers",
        "attempts": list(atts),
        "comparisons": [],
        "policy": {"bootstrap": 200, "judges": list(judges)},
    }


def _profiles():
    return agent_profiles.resolve(
        {"local": {"wire": "openai", "base_url": "http://127.0.0.1:1/v1", "model": "judge-9b"}},
        {},
    )


def _calls(replies):
    seen = []

    def call(model, messages, **kw):
        seen.append((model, messages, kw))
        r = replies.pop(0)
        if isinstance(r, Exception):
            raise r
        return type("Reply", (), {"text": r})()

    return call, seen


def _judge(doc, tmp_path, call, judge=None):
    return rank_judge.judge_once(
        doc, tmp_path, profiles=_profiles(), env={}, judge=judge, now=1.0, call=call
    )


def test_family_of():
    assert rank.family_of("claude-haiku-4-5") == "claude"
    assert rank.family_of("gpt-5-mini") == rank.family_of("o4-mini") == "gpt"
    assert rank.family_of("gemma3:4b") == rank.family_of("gemini-2.5-flash") == "gemini"
    assert rank.family_of("Qwen3-8B") == "qwen"
    assert rank.family_of("") == "" and rank.family_of("9b") == ""
    for alias in ("haiku", "sonnet", "opus", "fable"):
        assert rank.family_of(alias) == "claude"


def test_a_claude_judge_is_refused_against_an_author_recorded_as_a_bare_alias(tmp_path):
    call, seen = _calls([])
    doc = _doc(_att("a", "haiku"), _att("b", "sonnet"), judges=("local/claude-judge",))
    out = _judge(doc, tmp_path, call)
    assert out["recorded"] is False and "same family" in out["why"] and seen == []


def test_prompt_bytes_are_pinned():
    m = rank_judge.prompt("t", "x", "y")
    digest = hashlib.sha256(
        json.dumps(m, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    assert digest == "f90271f4cb148385a65dcf40cf1c6b078777f6b8c0ed1bb4f76108d64328cc65"


def test_judge_records_both_orders_and_the_fit_moves(tmp_path):
    call, seen = _calls(["A", "B"])
    out = _judge(_doc(_att("a", "claude-x"), _att("b", "claude-y")), tmp_path, call)
    assert out["recorded"] is True and out["pair"] == ["a", "b"] and out["outcomes"] == ["a", "a"]
    rows = [
        json.loads(x)
        for x in (tmp_path / "journal/rankings/comparisons.jsonl").read_text().splitlines()
    ]
    assert [r["shown"] for r in rows] == ["ab", "ba"] and len({r["pair_id"] for r in rows}) == 1
    assert all("claude" not in json.dumps(m) for _, m, _ in seen)
    assert seen[0][0] == "openai/judge-9b" and seen[0][2]["base_url"] == "http://127.0.0.1:1/v1"
    assert out["fit"]["order"][0] == "a"


def test_judge_error_is_no_vote(tmp_path):
    call, _ = _calls(["A", RuntimeError("boom")])
    out = _judge(_doc(_att("a", "claude-x"), _att("b", "claude-y")), tmp_path, call)
    assert out["recorded"] is False and "no vote" in out["why"] and out["reason"] == "failed"
    assert not (tmp_path / "journal").exists()


def test_judge_writes_both_orders_or_neither(tmp_path):
    call, _ = _calls(["A", "maybe A"])
    out = _judge(_doc(_att("a", "claude-x"), _att("b", "claude-y")), tmp_path, call)
    assert out["recorded"] is False and not (tmp_path / "journal").exists()


def test_same_family_judge_is_refused_and_unknown_author_too(tmp_path):
    call, seen = _calls([])
    out = _judge(_doc(_att("a", "judge-9b"), _att("b", "claude-y")), tmp_path, call)
    assert out["recorded"] is False and "same family" in out["why"] and seen == []
    out = _judge(_doc(_att("a", ""), _att("b", "claude-y")), tmp_path, call)
    assert out["recorded"] is False and "no author" in out["why"] and seen == []


@pytest.mark.parametrize(
    ("text", "want"),
    [("A", "a"), (" b.\nreason", "b"), ("Tie", "tie"), ("A or B", None), ("", None)],
)
def test_parse_verdict(text, want):
    assert rank_judge.parse_verdict(text) == want


def _cli_judge(tmp_path, monkeypatch, replies, atts):
    it = iter(replies)
    monkeypatch.setattr(
        "opendaisugi.rank_judge.llm_client.complete",
        lambda model, messages, **kw: type("R", (), {"text": next(it)})(),
    )
    monkeypatch.setattr("opendaisugi.rank_judge.load_profiles", lambda: _profiles())
    f = tmp_path / "a.json"
    f.write_text(json.dumps(_doc(*atts)))
    return CliRunner().invoke(
        app, ["rank", "judge", str(f), "--loop", "--data-dir", str(tmp_path / "dd"), "--json"]
    )


def test_cli_judge_stops_when_one_vote_settles_it(tmp_path, monkeypatch):
    res = _cli_judge(
        tmp_path, monkeypatch, ["A", "B"], [_att("a", "claude-x"), _att("b", "claude-y")]
    )
    assert res.exit_code == 0, res.output
    out = json.loads(res.output)
    assert out["recorded"] == 1 and out["stop"] == "confident"


def test_cli_judge_loops_until_the_judges_are_exhausted(tmp_path, monkeypatch):
    atts = [_att("a", "claude-x"), _att("b", "claude-y"), _att("c", "claude-z")]
    res = _cli_judge(tmp_path, monkeypatch, ["tie"] * 4, atts)
    assert res.exit_code == 0, res.output
    out = json.loads(res.output)
    assert out["recorded"] == 2 and out["stop"] == "judges_exhausted"


def test_cli_judge_exit_codes(tmp_path, monkeypatch):
    atts = [_att("a", "claude-x"), _att("b", "claude-y")]
    res = _cli_judge(tmp_path, monkeypatch, ["A", "maybe"], atts)
    assert res.exit_code == 3, res.output
    assert not (tmp_path / "dd" / "journal").exists()
    res = _cli_judge(tmp_path, monkeypatch, ["A", ""], [_att("a", "j"), _att("b", "")])
    assert res.exit_code == 3


def test_a_dropped_judge_is_not_asked_and_the_why_names_it(tmp_path):
    from opendaisugi import rank as rk

    # five answered cards where local/judge-9b disagreed with the owner
    for n in range(5):
        rid = f"r{n}"
        rows = []
        for shown, a, b, o in (("ab", "a", "b", "a"), ("ba", "b", "a", "b")):
            rows.append(
                {
                    "ranking_id": rid, "a": a, "b": b, "a_hash": f"h-{a}", "b_hash": f"h-{b}",
                    "shown": shown, "outcome": o, "judge": "local/judge-9b", "pair_id": f"p{n}",
                }
            )  # fmt: skip
        rk.append_comparisons(tmp_path, rows)
        opened = {
            "choice_id": f"ch_{n}", "ranking_id": rid, "event": "opened", "chosen": "a",
            "options": {"survivors": [{"id": "a", "content_hash": "h-a"},
                                      {"id": "b", "content_hash": "h-b"}], "eliminated": []},
            "ts": 1.0,
        }  # fmt: skip
        ans = {"choice_id": f"ch_{n}", "ranking_id": rid, "event": "overridden", "pick": "b",
               "how": "answer", "ts": 2.0}  # fmt: skip
        rk.append_rows(tmp_path, [opened, ans])
    call, seen = _calls([])
    # the plan's own shape: the file carries comparisons: []
    doc = _doc(_att("a", "claude-x"), _att("b", "claude-y"))
    assert doc["comparisons"] == []
    doc["ranking_id"] = "fresh"
    out = _judge(doc, tmp_path, call)
    assert out["recorded"] is False and "dropped" in out["why"] and seen == []


def test_an_empty_comparisons_list_means_the_journal_supplies_the_votes(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi import rank as rk
    from opendaisugi.cli import app

    rows = [
        {"ranking_id": "r1", "a": "a", "b": "b", "a_hash": "h-a", "b_hash": "h-b", "shown": s,
         "outcome": "b", "judge": "j1", "pair_id": f"p{i}"}
        for i in range(4) for s in ("ab", "ba")
    ]  # fmt: skip
    rk.append_comparisons(tmp_path, rows)
    base = _doc(_att("a"), _att("b"), judges=())
    out = {}
    for name, comps in (("none", None), ("empty", [])):
        doc = dict(base)
        doc.pop("comparisons")
        if comps is not None:
            doc["comparisons"] = comps
        f = tmp_path / f"{name}.json"
        f.write_text(json.dumps(doc))
        res = CliRunner().invoke(
            app, ["rank", "fit", str(f), "--data-dir", str(tmp_path), "--json"]
        )
        out[name] = json.loads(res.stdout)
    assert out["none"]["leader"] == out["empty"]["leader"] == "b"
    assert out["none"]["status"] == out["empty"]["status"] == "ranked"
    assert out["none"].get("judge_weights") == out["empty"].get("judge_weights")
    assert out["empty"].get("judge_weights")


def test_family_of_drops_one_leading_provider_prefix():
    assert rank.family_of("anthropic/claude-haiku-4-5") == "claude"
    assert rank.family_of("openai/gpt-5-mini") == "gpt"
    assert rank.family_of("local/") == "local" and rank.family_of("/x") == ""
