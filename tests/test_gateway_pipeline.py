"""The gateway pipeline — the two touchpoints the async proxy calls around a dispatch.

Before forwarding a turn: route it, and (only on a downgrade) swap the model in a COPY of the
body — never mutating the caller's, because the fail-open path still needs the original. After
the response: measure the saving from the model's own usage and record it. No sockets here;
the ASGI shell (proven separately) supplies the request body and the response usage.
"""

from __future__ import annotations

from opendaisugi.gateway_answers import AnswerStore
from opendaisugi.gateway_journal import GatewayJournal
from opendaisugi.gateway_pipeline import Gateway, PreparedTurn


def _req(model: str, user_text: str) -> dict:
    return {
        "model": model,
        "max_tokens": 1024,
        "messages": [{"role": "user", "content": user_text}],
    }


_USAGE = {"input_tokens": 50_000, "output_tokens": 800}


def test_prepare_swaps_the_model_on_a_downgrade_without_mutating_input() -> None:
    body = _req("claude-opus-4-8", "say hi")
    prepared = Gateway().prepare(body)
    assert isinstance(prepared, PreparedTurn)
    assert prepared.decision.downgraded is True
    assert prepared.outbound_body["model"] != "claude-opus-4-8"  # cheap model goes on the wire
    assert body["model"] == "claude-opus-4-8"  # caller's body is untouched (fail-open safety)


def test_prepare_leaves_a_hard_turn_on_the_requested_model() -> None:
    body = _req("claude-opus-4-8", "fix the deadlock race condition under concurrency")
    prepared = Gateway().prepare(body)
    assert prepared.decision.downgraded is False
    assert prepared.outbound_body["model"] == "claude-opus-4-8"


def test_finish_measures_and_records_to_the_journal(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")
    gw = Gateway(journal=journal)
    prepared = gw.prepare(_req("claude-opus-4-8", "say hi"))
    saving, record = gw.finish(prepared, _USAGE)
    assert saving.frontier_tokens_saved > 0
    assert record.dollars_saved > 0.0
    assert len(journal.load()) == 1


def test_finish_without_a_journal_still_returns_the_saving() -> None:
    gw = Gateway(journal=None)
    prepared = gw.prepare(_req("claude-opus-4-8", "say hi"))
    saving, record = gw.finish(prepared, _USAGE)
    assert saving.frontier_tokens_saved > 0  # measuring never depends on persistence


def _continuation() -> dict:
    """A tool-loop continuation: a user turn carrying only a tool_result block."""
    return {
        "model": "claude-opus-4-8",
        "max_tokens": 1024,
        "messages": [
            {"role": "user", "content": "add a docstring to this function"},
            {"role": "assistant", "content": [{"type": "text", "text": "reading…"}]},
            {
                "role": "user",
                "content": [{"type": "tool_result", "tool_use_id": "t", "content": "ok"}],
            },
        ],
    }


def test_tool_loop_continuations_do_not_inflate_repeats(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")
    gw = Gateway(journal=journal)
    # One opening ask, then two continuation turns of the SAME loop.
    gw.finish(gw.prepare(_req("claude-opus-4-8", "add a docstring to this function")), _USAGE)
    gw.finish(gw.prepare(_continuation()), _USAGE)
    gw.finish(gw.prepare(_continuation()), _USAGE)
    s = journal.summary()
    assert s.turns == 3
    assert s.downgraded_turns == 3  # walk-back routes the whole easy loop to the cheap model
    assert s.frontier_tokens_saved > 0
    assert s.repeats == []  # the loop is one ask, not three — continuations are not re-asks


def test_repeated_opening_asks_across_sessions_are_grouped(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")
    gw = Gateway(journal=journal)
    # The same ask opened in two separate sessions IS a genuine repeat.
    gw.finish(gw.prepare(_req("claude-opus-4-8", "add a docstring to this function")), _USAGE)
    gw.finish(gw.prepare(_req("claude-opus-4-8", "add a docstring to this function")), _USAGE)
    s = journal.summary()
    assert len(s.repeats) == 1
    assert s.repeats[0].count == 2


def test_gateway_blends_a_day_of_turns_in_the_summary(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")
    gw = Gateway(journal=journal)
    for task in ("say hi", "say hi", "prove the migration is thread-safe under load"):
        gw.finish(gw.prepare(_req("claude-opus-4-8", task)), _USAGE)
    s = journal.summary()
    assert s.turns == 3
    assert s.downgraded_turns == 2  # the hard one stayed on the frontier
    assert s.frontier_tokens_saved > 0
    assert s.dollars_saved > 0.0
    assert s.repeats[0].count == 2  # "say hi" asked twice


class _BoomStore:
    """A fake answer store whose ``append`` always raises — proves fail-open capture."""

    def append(self, entry) -> None:
        raise RuntimeError("boom")


def test_finish_captures_the_answer_when_opted_in(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")
    gw = Gateway(answer_store=store, capture_answers=True)
    prepared = gw.prepare(_req("claude-opus-4-8", "what does this function do?"))
    gw.finish(prepared, _USAGE, answer_text="it returns the answer", now=1000.0)
    entries = store.load()
    assert len(entries) == 1
    assert entries[0].answer == "it returns the answer"
    assert entries[0].task == prepared.task
    assert entries[0].created_at == 1000.0


def test_finish_does_not_capture_when_opted_out(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")
    gw = Gateway(answer_store=store, capture_answers=False)  # default is off
    prepared = gw.prepare(_req("claude-opus-4-8", "what does this function do?"))
    gw.finish(prepared, _USAGE, answer_text="it returns the answer")
    assert store.load() == []


def test_finish_with_capture_on_but_no_store_does_not_raise() -> None:
    gw = Gateway(capture_answers=True)  # answer_store left at its None default
    prepared = gw.prepare(_req("claude-opus-4-8", "say hi"))
    saving, record = gw.finish(prepared, _USAGE, answer_text="hi there")
    assert saving.frontier_tokens_saved > 0


def test_finish_does_not_capture_a_tool_loop_continuation(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")
    gw = Gateway(answer_store=store, capture_answers=True)
    prepared = gw.prepare(_continuation())  # empty .ask — not a standalone human question
    gw.finish(prepared, _USAGE, answer_text="some answer")
    assert store.load() == []


def test_finish_does_not_capture_a_blank_answer(tmp_path) -> None:
    store = AnswerStore(path=tmp_path / "answers.jsonl")
    gw = Gateway(answer_store=store, capture_answers=True)
    prepared = gw.prepare(_req("claude-opus-4-8", "say hi"))
    gw.finish(prepared, _USAGE, answer_text="")
    assert store.load() == []


def test_finish_survives_a_capture_failure() -> None:
    gw = Gateway(answer_store=_BoomStore(), capture_answers=True)
    prepared = gw.prepare(_req("claude-opus-4-8", "say hi"))
    saving, record = gw.finish(prepared, _USAGE, answer_text="hi there")
    assert saving.frontier_tokens_saved > 0
    assert record is not None
