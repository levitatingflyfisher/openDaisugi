from __future__ import annotations

from opendaisugi.config import Config
from opendaisugi.router_journal import RouterJournal
from opendaisugi.routing import estimate_difficulty
from opendaisugi.voice.cleanup import CLEANUP_PROMPT_V1, CleanupResult, clean_transcript


class FakeTransport:
    def __init__(self, corrected_text: str) -> None:
        self.corrected_text = corrected_text
        self.calls: list[tuple[str, str, str]] = []

    def complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]:
        self.calls.append((model, system, user))
        return self.corrected_text, {"input_tokens": 42, "output_tokens": 7}


class RaisingTransport:
    def __init__(self, error: Exception) -> None:
        self.error = error
        self.calls: list[tuple[str, str, str]] = []

    def complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]:
        self.calls.append((model, system, user))
        raise self.error


def test_clean_transcript_returns_original_when_cleanup_is_disabled(tmp_path):
    config = Config(
        data_dir=tmp_path, voice_cleanup=False, voice_cleanup_model="ollama/llama3.2:3b"
    )
    transport = FakeTransport("Hello, world.")
    result = clean_transcript("hello  world", config=config, transport=transport)
    assert result == CleanupResult(text="hello  world", cleaned=False, reason=None)
    assert transport.calls == []
    assert not (tmp_path / "router" / "turns.jsonl").exists()


def test_clean_transcript_returns_original_when_no_model_is_configured(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model=None)
    transport = FakeTransport("Hello, world.")
    result = clean_transcript("hello world", config=config, transport=transport)
    assert result == CleanupResult(text="hello world", cleaned=False, reason=None)
    assert transport.calls == []
    assert not (tmp_path / "router" / "turns.jsonl").exists()


def test_clean_transcript_calls_the_transport_with_the_fixed_prompt(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    transport = FakeTransport("Hello, world.")
    result = clean_transcript("hello  world", config=config, transport=transport)
    assert result == CleanupResult(text="Hello, world.", cleaned=True, reason=None)
    assert transport.calls == [("ollama/llama3.2:3b", CLEANUP_PROMPT_V1, "hello  world")]


def test_clean_transcript_falls_back_to_the_original_on_an_empty_correction(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    journal = RouterJournal(path=tmp_path / "router" / "turns.jsonl")

    result = clean_transcript(
        "hello world", config=config, transport=FakeTransport("   "), journal=journal
    )

    assert result.text == "hello world"
    assert result.cleaned is False
    assert result.reason == "The cleanup model returned no text. Check the model, then try again."
    # A reply with no usable text is not a turn worth journaling.
    assert journal.load() == []


def test_clean_transcript_keeps_the_corrected_text_when_the_journal_cannot_be_written(tmp_path):
    # journal.append does path.parent.mkdir(parents=True). Putting a plain
    # file where a parent directory needs to go makes that mkdir raise
    # NotADirectoryError, an OSError, without needing root or a chmod that
    # a CI box running as root would ignore.
    blocker = tmp_path / "not_a_directory"
    blocker.write_text("")
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    journal = RouterJournal(path=blocker / "sub" / "turns.jsonl")

    result = clean_transcript(
        "hello world", config=config, transport=FakeTransport("Hello, world."), journal=journal
    )

    assert result.text == "Hello, world."
    assert result.cleaned is False
    assert result.reason == (
        "The transcript could not be journaled. Check the data directory, then try again."
    )


def test_clean_transcript_returns_the_raw_transcript_when_the_transport_raises(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    journal = RouterJournal(path=tmp_path / "router" / "turns.jsonl")
    transport = RaisingTransport(ConnectionError("local endpoint refused the connection"))

    result = clean_transcript("hello world", config=config, transport=transport, journal=journal)

    assert result.text == "hello world"
    assert result.cleaned is False
    assert result.reason
    assert transport.calls == [("ollama/llama3.2:3b", CLEANUP_PROMPT_V1, "hello world")]
    # A failed call is never a turn worth journaling.
    assert journal.load() == []


def test_clean_transcript_journals_the_real_transcript_as_the_task(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    journal = RouterJournal(path=tmp_path / "router" / "turns.jsonl")

    clean_transcript(
        "first thing said",
        config=config,
        transport=FakeTransport("First thing said."),
        journal=journal,
    )
    clean_transcript(
        "second thing said",
        config=config,
        transport=FakeTransport("Second thing said."),
        journal=journal,
    )

    records = journal.load()
    assert len(records) == 2
    # The real transcript is the task, the same way the router journals any
    # other turn.
    assert records[0].task == "first thing said"
    assert records[1].task == "second thing said"
    assert records[0].tier == "tier1-local"
    assert records[1].tier == "tier1-local"
    # Two distinct transcripts give two distinct signatures.
    assert records[0].signature != records[1].signature
    # Never a phantom saving. Never priced at the fallback rate that would
    # otherwise book real dollars against a local call.
    assert records[0].downgraded is False
    assert records[0].actual_dollars == 0.0
    assert records[0].counterfactual_dollars == 0.0
    assert records[1].downgraded is False
    assert records[1].actual_dollars == 0.0
    assert records[1].counterfactual_dollars == 0.0


def test_clean_transcript_gives_the_same_transcript_the_same_signature(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    journal = RouterJournal(path=tmp_path / "router" / "turns.jsonl")

    clean_transcript(
        "say the weather",
        config=config,
        transport=FakeTransport("Say the weather."),
        journal=journal,
    )
    clean_transcript(
        "say the weather",
        config=config,
        transport=FakeTransport("Say the weather."),
        journal=journal,
    )

    records = journal.load()
    assert len(records) == 2
    assert records[0].signature == records[1].signature


def test_clean_transcript_pins_difficulty_to_the_real_transcript(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    journal = RouterJournal(path=tmp_path / "router" / "turns.jsonl")
    text = "please schedule a meeting with the whole team tomorrow at nine"

    clean_transcript(
        text,
        config=config,
        transport=FakeTransport("Please schedule a meeting with the whole team tomorrow at nine."),
        journal=journal,
    )

    records = journal.load()
    assert len(records) == 1
    assert records[0].difficulty == estimate_difficulty(text)


def test_clean_transcript_uses_the_default_journal_under_config_data_dir(tmp_path):
    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/llama3.2:3b")
    clean_transcript("hello world", config=config, transport=FakeTransport("Hello, world."))
    journal = RouterJournal(path=tmp_path / "router" / "turns.jsonl")
    assert len(journal.load()) == 1


def test_a_cut_cleanup_keeps_the_raw_text_of_that_chunk(tmp_path):
    from opendaisugi.voice.engines import Transcript

    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/x")

    class Truncating:
        def complete(self, *, model, system, user):
            return user[: len(user) // 2], {"input_tokens": 1, "output_tokens": 1}

    t = Transcript(
        text="window one window two",
        segments=[],
        duration_s=60.0,
        rtf=0.1,
        parts=("window one", "window two"),
    )
    from opendaisugi.voice.cleanup import clean_parts

    res = clean_parts(t, config=config, transport=Truncating())
    assert res.text == "window one window two"
    assert not res.cleaned
    assert res.reason and "much less text" in res.reason


def test_cleanup_runs_per_window_and_skips_gaps(tmp_path):
    from opendaisugi.voice.cleanup import clean_parts
    from opendaisugi.voice.engines import GAP_MARKER, Transcript

    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/x")

    class Tidy:
        def __init__(self):
            self.calls = []

        def complete(self, *, model, system, user):
            self.calls.append(user)
            return user.capitalize() + ".", {"input_tokens": 1, "output_tokens": 1}

    tidy = Tidy()
    t = Transcript(
        text=f"alpha beta {GAP_MARKER}",
        segments=[],
        duration_s=90.0,
        rtf=0.1,
        parts=("alpha", "beta", GAP_MARKER),
    )
    res = clean_parts(t, config=config, transport=tidy)
    assert res.text == f"Alpha. Beta. {GAP_MARKER}"
    assert res.cleaned
    assert tidy.calls == ["alpha", "beta"]


def test_cleanup_stops_calling_a_model_that_did_not_answer(tmp_path):
    from opendaisugi.voice.cleanup import clean_parts
    from opendaisugi.voice.engines import Transcript

    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/x")

    class Dead:
        calls = 0

        def complete(self, *, model, system, user):
            Dead.calls += 1
            raise OSError("no route")

    parts = tuple(f"window {i}" for i in range(6))
    t = Transcript(text=" ".join(parts), segments=[], duration_s=180.0, rtf=0.1, parts=parts)
    res = clean_parts(t, config=config, transport=Dead())
    assert Dead.calls == 1
    assert res.text == " ".join(parts)
    assert not res.cleaned
    assert res.reason and "failed to respond" in res.reason


def test_cleanup_past_its_time_budget_keeps_the_raw_text_of_the_rest(tmp_path):
    from opendaisugi.voice.cleanup import clean_parts
    from opendaisugi.voice.engines import Transcript

    config = Config(data_dir=tmp_path, voice_cleanup=True, voice_cleanup_model="ollama/x")
    now = [0.0]

    class Slow:
        calls = 0

        def complete(self, *, model, system, user):
            Slow.calls += 1
            now[0] += 50.0  # each call takes 50 s on the fake clock
            return user.capitalize() + ".", {"input_tokens": 1, "output_tokens": 1}

    parts = tuple(f"window {i}" for i in range(6))
    t = Transcript(text=" ".join(parts), segments=[], duration_s=180.0, rtf=0.1, parts=parts)
    res = clean_parts(t, config=config, transport=Slow(), clock=lambda: now[0], budget_s=120.0)
    assert Slow.calls == 3  # 0, 50, 100 s; at 150 s the budget is gone
    assert res.text.startswith("Window 0. Window 1. Window 2. window 3 window 4 window 5")
    assert res.cleaned
    assert res.reason and "took too long" in res.reason
