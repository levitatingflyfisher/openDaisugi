"""Tests for opendaisugi.envelope — system prompt, generate_envelope, calibration."""

import pytest

from opendaisugi.envelope import (
    ENVELOPE_PROMPT_VERSION,
    ENVELOPE_SYSTEM_PROMPT,
    CalibrationReport,
    _check_assert,
    _validate_task_length,
    generate_envelope,
    run_calibration,
)
from opendaisugi.exceptions import EnvelopeGenerationError, TaskTooLongError
from opendaisugi.models import Envelope, Permission


def test_prompt_has_role_framing():
    # Section 1: role framing. The LLM must understand it generates envelopes,
    # not executes anything.
    assert "generate safety envelopes" in ENVELOPE_SYSTEM_PROMPT.lower()
    assert "do not execute" in ENVELOPE_SYSTEM_PROMPT.lower()


def test_prompt_embeds_envelope_schema():
    # Section 2: schema embedding. The schema must be literally present so the
    # model sees the exact fields it must produce.
    assert '"permissions"' in ENVELOPE_SYSTEM_PROMPT
    assert '"invariants"' in ENVELOPE_SYSTEM_PROMPT
    assert '"postconditions"' in ENVELOPE_SYSTEM_PROMPT
    assert '"file_read"' in ENVELOPE_SYSTEM_PROMPT
    assert '"shell_allowlist"' in ENVELOPE_SYSTEM_PROMPT


def test_prompt_has_calibration_guidance():
    # Section 3: calibration guidance — all four of the spec's bullets.
    text = ENVELOPE_SYSTEM_PROMPT.lower()
    assert "minimize permissions" in text
    assert "specific paths over globs" in text
    assert "allowlist" in text
    assert "network=false" in text


def test_prompt_has_failure_modes_section():
    # Section 5: explicit failure modes to avoid.
    text = ENVELOPE_SYSTEM_PROMPT.lower()
    assert "do not produce" in text
    assert "all permissions" in text or "allow all" in text
    # Must call out aspirational invariants as a failure mode.
    assert "aspirational" in text


def test_prompt_has_four_few_shot_examples():
    # Four distinct examples covering: filesystem read, filesystem write with
    # allowlist, shell with tight allowlist, composed task.
    text = ENVELOPE_SYSTEM_PROMPT
    # We mark each example with "Example N:" for grepability.
    for n in (1, 2, 3, 4):
        assert f"Example {n}:" in text, f"missing Example {n} marker"
    # Each example pairs a Task line with an Envelope JSON block.
    assert text.count("Task:") >= 4
    assert text.count('"permissions"') >= 5  # 1 in schema + 4 in examples


def test_prompt_examples_cover_distinct_scenarios():
    text = ENVELOPE_SYSTEM_PROMPT
    # Rough scenario coverage — each example mentions a distinctive term.
    assert ".csv" in text              # filesystem read (row count task)
    assert "out/" in text              # filesystem write with allowlist
    assert "find" in text              # shell with tight allowlist
    assert "https://" in text          # network-using composed task


def test_validate_task_length_accepts_short_task():
    # Within limit — no exception.
    _validate_task_length(task="short task", context=None, max_task_chars=100)


def test_validate_task_length_counts_context():
    # Task alone fits, but task + context exceeds the limit.
    with pytest.raises(TaskTooLongError) as exc:
        _validate_task_length(
            task="a" * 50,
            context="b" * 60,
            max_task_chars=100,
        )
    assert "110" in str(exc.value)
    assert "100" in str(exc.value)
    assert "summarize" in str(exc.value).lower()


def test_validate_task_length_ignores_none_context():
    # None context is treated as empty.
    _validate_task_length(task="a" * 50, context=None, max_task_chars=100)


def test_validate_task_length_at_exact_limit_is_allowed():
    _validate_task_length(task="a" * 100, context=None, max_task_chars=100)


def test_validate_task_length_rejects_empty_task():
    with pytest.raises(ValueError, match="non-empty"):
        _validate_task_length(task="", context=None, max_task_chars=100)


def test_validate_task_length_rejects_whitespace_only_task():
    with pytest.raises(ValueError, match="non-empty"):
        _validate_task_length(task="   \n\t", context=None, max_task_chars=100)


async def test_generate_envelope_rejects_empty_task(mock_llm_client):
    with pytest.raises(ValueError, match="non-empty"):
        await generate_envelope(task="")
    # Mocked client must not have been called.
    assert mock_llm_client.chat.completions.last_call == {}


async def test_mock_llm_client_fixture_returns_sample_envelope(
    mock_llm_client, sample_envelope
):
    # Calling .chat.completions.create(...) on the mocked client returns
    # the sample_envelope regardless of args.
    result = await mock_llm_client.chat.completions.create(
        model="anything",
        response_model=object,
        messages=[],
    )
    assert result is sample_envelope


async def test_generate_envelope_returns_envelope(mock_llm_client, sample_envelope):
    result = await generate_envelope(task="Delete .tmp files in /var/log")
    assert result is sample_envelope


async def test_generate_envelope_sends_system_and_user_messages(mock_llm_client):
    await generate_envelope(task="Read /data/sales.csv")
    messages = mock_llm_client.chat.completions.last_call["messages"]
    assert len(messages) == 2
    assert messages[0]["role"] == "system"
    assert messages[0]["content"] == ENVELOPE_SYSTEM_PROMPT
    assert messages[1]["role"] == "user"
    assert "Read /data/sales.csv" in messages[1]["content"]


async def test_generate_envelope_passes_response_model(mock_llm_client):
    await generate_envelope(task="noop")
    captured = mock_llm_client.chat.completions.last_call
    assert captured["response_model"] is Envelope


async def test_generate_envelope_passes_context_into_user_message(mock_llm_client):
    await generate_envelope(
        task="Transform input.csv",
        context="input.csv schema: id,name,email",
    )
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Transform input.csv" in user_msg
    assert "input.csv schema: id,name,email" in user_msg
    assert user_msg.startswith("Task: ")
    assert "Context:" in user_msg


async def test_generate_envelope_omits_context_when_none(mock_llm_client):
    await generate_envelope(task="simple task")
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Context:" not in user_msg


async def test_generate_envelope_passes_max_retries_through(mock_llm_client):
    await generate_envelope(task="x", max_retries=7)
    assert mock_llm_client.chat.completions.last_call["max_retries"] == 7


async def test_generate_envelope_default_max_retries_is_three(mock_llm_client):
    await generate_envelope(task="x")
    assert mock_llm_client.chat.completions.last_call["max_retries"] == 3


async def test_generate_envelope_passes_model_through(mock_llm_client):
    await generate_envelope(task="x", model="openai/gpt-4o-mini")
    assert mock_llm_client.chat.completions.last_call["model"] == "openai/gpt-4o-mini"


async def test_generate_envelope_task_too_long_raises(mock_llm_client):
    with pytest.raises(TaskTooLongError):
        await generate_envelope(task="a" * 5000, max_task_chars=4000)
    # Mocked client must not have been called.
    assert mock_llm_client.chat.completions.last_call == {}


async def test_generate_envelope_wraps_client_error(monkeypatch, sample_envelope):
    # Swap in a client whose create() raises.
    class ExplodingCompletions:
        async def create(self, **kwargs):
            raise RuntimeError("429 rate limit from upstream")

    class ExplodingChat:
        completions = ExplodingCompletions()

    class ExplodingClient:
        chat = ExplodingChat()

    from opendaisugi import llm
    monkeypatch.setattr(llm, "get_instructor_client", lambda model: ExplodingClient())

    with pytest.raises(EnvelopeGenerationError) as exc:
        await generate_envelope(task="ratelimited task")
    assert "429 rate limit" in str(exc.value)
    # Original cause preserved.
    assert isinstance(exc.value.__cause__, RuntimeError)


def test_check_assert_equals_passes():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=False),
    )
    assert _check_assert(env, {"path": "permissions.shell", "equals": False}) is True


def test_check_assert_equals_fails():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True),
    )
    assert _check_assert(env, {"path": "permissions.shell", "equals": False}) is False


def test_check_assert_contains_glob_passes():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(file_read=["/data/sales.csv"]),
    )
    assertion = {"path": "permissions.file_read", "contains_glob": "/data/sales.csv"}
    assert _check_assert(env, assertion) is True


def test_check_assert_contains_glob_wildcard():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(file_write=["out/sales.png", "out/revenue.png"]),
    )
    assertion = {"path": "permissions.file_write", "contains_glob": "out/*.png"}
    assert _check_assert(env, assertion) is True


def test_check_assert_not_empty():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell_allowlist=["find"]),
    )
    assert _check_assert(env, {"path": "permissions.shell_allowlist", "not_empty": True}) is True


def test_check_assert_empty():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(file_write=[]),
    )
    assert _check_assert(env, {"path": "permissions.file_write", "empty": True}) is True


def test_check_assert_not_glob_rejects_degenerate_envelope():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(file_write=["/**"]),
    )
    # "/**" is the canonical all-write glob — must be rejected.
    assertion = {"path": "permissions.file_write", "not_glob": "/**"}
    assert _check_assert(env, assertion) is False


def test_check_assert_list_assertion_on_scalar_raises():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True),
    )
    # permissions.shell is a bool — not_empty should raise, not silently fail.
    with pytest.raises(TypeError, match="expects a list"):
        _check_assert(env, {"path": "permissions.shell", "not_empty": True})


def test_resolve_path_error_includes_full_path():
    from opendaisugi.envelope import _resolve_path

    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(),
    )
    with pytest.raises(AttributeError, match="permissions.file_reaad"):
        _resolve_path(env, "permissions.file_reaad")


async def test_run_calibration_scores_mock_producer(sample_envelope):
    # A producer that always returns the same envelope — matches only the
    # asserts that happen to describe sample_envelope's shape.
    async def always_sample(task, **kwargs):
        return sample_envelope

    entries = [
        {
            "id": "match",
            "task": "t",
            "asserts": [{"path": "permissions.shell", "equals": True}],
        },
        {
            "id": "miss",
            "task": "t",
            "asserts": [{"path": "permissions.network", "equals": True}],
        },
    ]
    report = await run_calibration(entries, produce=always_sample)

    assert isinstance(report, CalibrationReport)
    assert report.total == 2
    assert report.passed == 1
    assert report.pass_rate == 0.5
    assert report.failures == ["miss"]


async def test_run_calibration_empty_entries():
    async def producer(task, **kwargs):
        raise AssertionError("should not be called")

    report = await run_calibration([], produce=producer)
    assert report.total == 0
    assert report.passed == 0
    assert report.pass_rate == 0.0
    assert report.failures == []


async def test_run_calibration_counts_producer_exceptions_as_failures(sample_envelope):
    async def flaky(task, **kwargs):
        if task == "t1":
            return sample_envelope
        raise RuntimeError("simulated API failure")

    entries = [
        {"id": "ok", "task": "t1", "asserts": [{"path": "permissions.shell", "equals": True}]},
        {"id": "err", "task": "t2", "asserts": []},
    ]
    report = await run_calibration(entries, produce=flaky)
    assert report.total == 2
    assert report.passed == 1
    assert "err" in report.failures
    assert "err" in report.errors


def test_envelope_prompt_version_is_a_string():
    from opendaisugi.envelope import ENVELOPE_PROMPT_VERSION
    assert isinstance(ENVELOPE_PROMPT_VERSION, str)
    assert len(ENVELOPE_PROMPT_VERSION) > 0


async def test_generate_envelope_summarize_false_default(mock_llm_client):
    # Default: no summary instruction in user message.
    await generate_envelope(task="read /tmp/x")
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "summary" not in user_msg.lower()


async def test_generate_envelope_summarize_true_appends_instruction(mock_llm_client):
    await generate_envelope(task="read /tmp/x", summarize=True)
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "summary" in user_msg.lower()
    assert "80" in user_msg  # char budget surfaced


async def test_generate_envelope_summarize_does_not_modify_system_prompt(mock_llm_client):
    # System prompt must remain byte-stable so caches don't invalidate.
    await generate_envelope(task="read /tmp/x", summarize=True)
    system_msg = mock_llm_client.chat.completions.last_call["messages"][0]["content"]
    assert system_msg == ENVELOPE_SYSTEM_PROMPT


async def test_generate_envelope_summarize_with_context_preserves_context(mock_llm_client):
    await generate_envelope(task="t", context="ctx-body", summarize=True)
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "ctx-body" in user_msg
    assert "summary" in user_msg.lower()


@pytest.mark.asyncio
async def test_generate_envelope_stamps_cache_key(mock_llm_client):
    """The returned envelope must carry the cache key used to store it."""
    env = await generate_envelope("some task", model="anthropic/test-model")
    assert env.cache_key is not None
    assert len(env.cache_key) == 64  # sha256 hex


@pytest.mark.asyncio
async def test_generate_envelope_cache_key_matches_make_cache_key(mock_llm_client):
    """Stamped cache_key equals make_cache_key() for the call args."""
    from opendaisugi.envelope_cache import make_cache_key
    expected = make_cache_key(
        task="task-x", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    )
    env = await generate_envelope("task-x", model="anthropic/test-model")
    assert env.cache_key == expected


# ---------------------------------------------------------------------------
# _refinement_hints_block tests (v0.2.1)
# ---------------------------------------------------------------------------


def test_refinement_hints_block_empty_for_no_records():
    from opendaisugi.envelope import _refinement_hints_block
    assert _refinement_hints_block([]) == ""


def test_refinement_hints_block_formats_violations():
    from opendaisugi.envelope import _refinement_hints_block
    from opendaisugi.models import ShellStep, Violation
    from opendaisugi.refinement import RefinementRecord

    rec = RefinementRecord(
        step=ShellStep(id="s1", command="rm -rf /"),
        violations=[
            Violation(stage="permissions",
                      message="shell command 'rm' not in allowlist",
                      detail={"step": "s1"}),
            Violation(stage="invariants",
                      message="file_unchanged invariant violated for /etc/hosts",
                      detail={}),
        ],
        z3_counterexample=None,
        envelope_id="env_1",
        fallback_action="halted",
        timestamp=1.0,
        cache_key="k",
    )
    block = _refinement_hints_block([rec])
    assert "## Prior Rejections" in block
    assert "[permissions] shell command 'rm' not in allowlist" in block
    assert "[invariants] file_unchanged invariant violated for /etc/hosts" in block


def test_refinement_hints_block_deduplicates_by_message():
    """Same violation message across 3 records appears once."""
    from opendaisugi.envelope import _refinement_hints_block
    from opendaisugi.models import ShellStep, Violation
    from opendaisugi.refinement import RefinementRecord

    def _rec(ts):
        return RefinementRecord(
            step=ShellStep(id="s1", command="rm"),
            violations=[Violation(stage="permissions",
                                   message="shell not allowed",
                                   detail={})],
            z3_counterexample=None, envelope_id="e", fallback_action="halted",
            timestamp=ts, cache_key="k",
        )
    block = _refinement_hints_block([_rec(1.0), _rec(2.0), _rec(3.0)])
    assert block.count("shell not allowed") == 1


def test_refinement_hints_block_caps_at_ten():
    """Unique messages beyond 10 are dropped, keeping the most recent."""
    from opendaisugi.envelope import _refinement_hints_block
    from opendaisugi.models import ShellStep, Violation
    from opendaisugi.refinement import RefinementRecord

    records = [
        RefinementRecord(
            step=ShellStep(id="s1", command="rm"),
            violations=[Violation(stage="permissions",
                                   message=f"violation number {i}",
                                   detail={})],
            z3_counterexample=None, envelope_id="e", fallback_action="halted",
            timestamp=float(i), cache_key="k",
        )
        for i in range(15)
    ]
    block = _refinement_hints_block(records)
    # Most-recent 10 kept (indices 5..14), older 5 dropped.
    # Use the full formatted bullet to avoid substring collisions (e.g.
    # "violation number 1" is a prefix of "violation number 10..14").
    for i in range(5, 15):
        assert f"[permissions] violation number {i}" in block
    for i in range(0, 5):
        assert f"violation number {i}\n" not in block
        assert block.endswith(f"violation number {i}") is False


# ---------------------------------------------------------------------------
# Task 8: journal injection into generate_envelope (v0.2.1)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_generate_envelope_injects_refinement_hints(tmp_path, mock_llm_client):
    """When journal has refinements for this cache key, hints appear in user msg."""
    from opendaisugi.envelope_cache import make_cache_key
    from opendaisugi.journal import Journal
    from opendaisugi.models import ShellStep, Violation
    from opendaisugi.refinement import RefinementRecord

    journal = Journal(data_dir=tmp_path)
    key = make_cache_key(
        task="do risky thing", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    )
    journal.write_refinement(
        RefinementRecord(
            step=ShellStep(id="s1", command="rm -rf /"),
            violations=[Violation(stage="permissions",
                                   message="shell not allowed",
                                   detail={})],
            z3_counterexample=None, envelope_id="e", fallback_action="halted",
            timestamp=1.0, cache_key=key,
        ),
        session_id="run_x",
    )

    await generate_envelope(
        "do risky thing",
        model="anthropic/test-model",
        journal=journal,
    )

    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "## Prior Rejections" in user_msg
    assert "shell not allowed" in user_msg


@pytest.mark.asyncio
async def test_generate_envelope_no_hints_when_no_refinements(tmp_path, mock_llm_client):
    from opendaisugi.journal import Journal

    journal = Journal(data_dir=tmp_path)
    await generate_envelope("clean task", journal=journal)

    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Prior Rejections" not in user_msg


@pytest.mark.asyncio
async def test_generate_envelope_no_journal_is_backward_compatible(mock_llm_client):
    """Omitting journal produces the exact same user message as pre-v0.2.1."""
    await generate_envelope("task", model="anthropic/test-model")
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Prior Rejections" not in user_msg
    assert user_msg.startswith("Task: task")


@pytest.mark.asyncio
async def test_generate_envelope_journal_query_failure_is_tolerated(tmp_path, mock_llm_client):
    """If the journal query raises, generation proceeds with no hints."""
    from opendaisugi.journal import Journal

    class BrokenJournal(Journal):
        def get_refinements_by_key(self, cache_key):
            raise RuntimeError("boom")

    journal = BrokenJournal(data_dir=tmp_path)
    env = await generate_envelope("task", journal=journal)
    assert env is not None
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Prior Rejections" not in user_msg


# ---------------------------------------------------------------------------
# Task 9: cache invalidation on newer refinements (v0.2.1)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_generate_envelope_cache_bust_on_newer_refinement(tmp_path, mock_llm_client):
    """If a cached entry is older than a refinement record, cache is invalidated."""
    from opendaisugi.envelope_cache import EnvelopeCache, make_cache_key
    from opendaisugi.journal import Journal
    from opendaisugi.models import Envelope, Permission, ShellStep, Violation
    from opendaisugi.refinement import RefinementRecord

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version=ENVELOPE_PROMPT_VERSION)
    journal = Journal(data_dir=tmp_path)

    # Pre-seed cache.
    old_env = Envelope(
        generated_by="t", task="risky", permissions=Permission(shell=True),
    )
    cache.put(
        old_env, task="risky", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False,
    )
    cached_at = cache.get_inserted_at(make_cache_key(
        task="risky", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False,
    ))

    # Write a refinement AFTER the cached entry.
    key = make_cache_key(
        task="risky", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False,
    )
    journal.write_refinement(
        RefinementRecord(
            step=ShellStep(id="s1", command="rm"),
            violations=[Violation(stage="permissions",
                                   message="shell not allowed",
                                   detail={})],
            z3_counterexample=None, envelope_id="e", fallback_action="halted",
            timestamp=cached_at + 1.0,  # strictly newer
            cache_key=key,
        ),
        session_id="run_x",
    )

    # Configure mock to return a NEW envelope on regeneration.
    new_env = Envelope(
        generated_by="t", task="risky",
        permissions=Permission(shell=False),  # tightened
    )
    mock_llm_client.set_next_envelope(new_env)

    result = await generate_envelope(
        "risky", model="anthropic/test-model",
        cache=cache, journal=journal,
    )
    # Should be the tightened one — cache was busted, LLM was called.
    assert result.id == new_env.id
    assert mock_llm_client.call_count == 1

    # Hints were injected into the regeneration.
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Prior Rejections" in user_msg


@pytest.mark.asyncio
async def test_generate_envelope_no_cache_bust_when_refinement_is_older(tmp_path, mock_llm_client):
    """Cached entry newer than all refinements → serve from cache, no LLM call."""
    from opendaisugi.envelope_cache import EnvelopeCache, make_cache_key
    from opendaisugi.journal import Journal
    from opendaisugi.models import Envelope, Permission, ShellStep, Violation
    from opendaisugi.refinement import RefinementRecord

    cache = EnvelopeCache(tmp_path / "c.db", prompt_version=ENVELOPE_PROMPT_VERSION)
    journal = Journal(data_dir=tmp_path)

    key = make_cache_key(
        task="t", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False,
    )
    # Write an old refinement first.
    journal.write_refinement(
        RefinementRecord(
            step=ShellStep(id="s1", command="rm"),
            violations=[Violation(stage="permissions", message="m", detail={})],
            z3_counterexample=None, envelope_id="e", fallback_action="halted",
            timestamp=1.0, cache_key=key,
        ),
        session_id="run_x",
    )

    # Then cache a fresh envelope (inserted_at will be now — much later).
    fresh = Envelope(generated_by="t", task="t", permissions=Permission())
    cache.put(
        fresh, task="t", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False,
    )

    initial_calls = mock_llm_client.call_count
    result = await generate_envelope(
        "t", model="anthropic/test-model",
        cache=cache, journal=journal,
    )
    # Cache served; no LLM call.
    assert result.id == fresh.id
    assert mock_llm_client.call_count == initial_calls
