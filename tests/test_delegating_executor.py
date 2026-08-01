"""Tests for DelegatingExecutor (v0.19 L3)."""

from unittest.mock import patch

from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.models import ShellStep


def _step(**kw):
    return ShellStep(id="s1", command="echo hi", **kw)


def test_resolves_preferred_model_over_default():
    """Step's preferred_model wins over the executor's default_model."""
    exe = DelegatingExecutor(default_model="haiku")
    s = _step(preferred_model="sonnet")
    with patch.object(exe, "_call", return_value='{"ok": true}') as mock_call:
        exe.run(s, timeout_s=10, max_output_bytes=1024)
    assert mock_call.call_args.args[0] == "sonnet"
    assert exe.last.model == "sonnet"


def test_call_receives_timeout_and_max_tokens_from_executor_protocol():
    """Supervisor passes timeout_s + max_output_bytes; the executor must
    propagate them so litellm respects the supervisor's ceilings (v0.21.1)."""
    exe = DelegatingExecutor(default_model="haiku")
    with patch.object(exe, "_call", return_value="{}") as mock_call:
        exe.run(_step(), timeout_s=12, max_output_bytes=4096)
    kwargs = mock_call.call_args.kwargs
    assert kwargs["timeout_s"] == 12
    assert kwargs["max_tokens"] == 1024  # 4096 // 4


def test_max_tokens_floors_at_256():
    """Even tiny max_output_bytes shouldn't produce a zero-token completion."""
    exe = DelegatingExecutor(default_model="haiku")
    with patch.object(exe, "_call", return_value="{}") as mock_call:
        exe.run(_step(), timeout_s=10, max_output_bytes=8)
    assert mock_call.call_args.kwargs["max_tokens"] == 256


def test_falls_back_to_default_model_when_step_has_no_preference():
    exe = DelegatingExecutor(default_model="haiku")
    s = _step()
    with patch.object(exe, "_call", return_value='{"ok": true}') as mock_call:
        exe.run(s, timeout_s=10, max_output_bytes=1024)
    assert mock_call.call_args.args[0] == "haiku"


def test_returns_executor_result_with_json_stdout():
    exe = DelegatingExecutor(default_model="haiku")
    with patch.object(exe, "_call", return_value='{"draft_hash": "abc"}'):
        r = exe.run(_step(), timeout_s=10, max_output_bytes=1024)
    assert r.rc == 0
    assert r.stdout == '{"draft_hash": "abc"}'
    assert r.timed_out is False


def test_terminal_failure_after_exhausting_retries():
    """If every call raises, executor returns rc=1 with the last error."""
    exe = DelegatingExecutor(default_model="haiku", max_retries=1)
    with patch.object(exe, "_call", side_effect=RuntimeError("boom")):
        r = exe.run(_step(), timeout_s=10, max_output_bytes=1024)
    assert r.rc == 1
    assert "boom" in r.stdout or "exhausted" in r.stdout
    assert exe.last.attempts == 2  # max_retries=1 + initial = 2 attempts


def test_schema_validation_retry_recovers():
    """If first response fails schema, retry; if second passes, return success."""
    from pydantic import BaseModel

    class Resp(BaseModel):
        draft_hash: str

    exe = DelegatingExecutor(default_model="haiku", response_schema=Resp, max_retries=2)
    responses = ['{"wrong_key": 1}', '{"draft_hash": "ok"}']
    call_idx = [0]

    def fake_call(model, prompt, *, timeout_s, max_tokens):
        i = call_idx[0]
        call_idx[0] += 1
        return responses[i]

    with patch.object(exe, "_call", side_effect=fake_call):
        r = exe.run(_step(), timeout_s=10, max_output_bytes=1024)
    assert r.rc == 0
    assert r.stdout == '{"draft_hash": "ok"}'


def test_custom_prompt_template_receives_step():
    captured = []
    exe = DelegatingExecutor(
        default_model="haiku",
        prompt_template=lambda step: captured.append(step) or f"PROMPT FOR {step.id}",
    )
    with patch.object(exe, "_call", return_value="{}") as mock_call:
        exe.run(_step(), timeout_s=10, max_output_bytes=1024)
    assert captured[0].id == "s1"
    # _call signature: (model, prompt, *, timeout_s, max_tokens)
    assert mock_call.call_args.args[1] == "PROMPT FOR s1"


def test_result_carries_resolved_model_tokens_and_cost():
    """v0.40: the ExecutorResult carries per-call model/tokens/cost so budget
    attribution and receipt model_id come from the RESULT (thread-safe) instead
    of the shared self.last slot — the enabler for parallel task steps."""
    exe = DelegatingExecutor(default_model="haiku")

    def fake_call(model, prompt, *, timeout_s, max_tokens):
        exe._last_usage = 123
        exe._last_cost = 0.0042
        return "{}"

    with patch.object(exe, "_call", side_effect=fake_call):
        r = exe.run(_step(preferred_model="sonnet"), timeout_s=10, max_output_bytes=1024)
    assert r.model == "sonnet"
    assert r.tokens == 123
    assert r.cost_usd == 0.0042


def test_concurrent_runs_report_correct_per_call_tokens():
    """Two steps with different token usages run concurrently on ONE executor
    instance (as parallel task-step prefetch does). Each result must carry its
    OWN tokens — the per-call side-channel must not be clobbered cross-thread."""
    import threading
    import time

    exe = DelegatingExecutor(default_model="haiku")
    start = threading.Barrier(2)

    def fake_call(model, prompt, *, timeout_s, max_tokens):
        usage = 111 if model == "haiku" else 222
        exe._last_usage = usage
        time.sleep(0.03)  # widen the read/clobber window
        return "{}"

    results: dict[str, int] = {}

    def go(model_name: str) -> None:
        start.wait()
        r = exe.run(_step(preferred_model=model_name), timeout_s=10, max_output_bytes=1024)
        results[model_name] = r.tokens

    with patch.object(exe, "_call", side_effect=fake_call):
        threads = [threading.Thread(target=go, args=(m,)) for m in ("haiku", "sonnet")]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

    assert results == {"haiku": 111, "sonnet": 222}
