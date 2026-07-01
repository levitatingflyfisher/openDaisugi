"""Synthesizer: collect step outputs into a final answer (v0.32)."""

from __future__ import annotations

from opendaisugi.models import ActionPlan, ShellStep, TaskStep, VerificationResult
from opendaisugi.run_session import RunSession, RunStatus, StepOutcome
from opendaisugi.synthesizer import SynthesisResult, collect_outputs, synthesize


def _plan():
    return ActionPlan(source="t", task="write a haiku about the sea then count its lines", steps=[
        TaskStep(id="t1", prompt="write a haiku about the sea"),
        ShellStep(id="s1", command="wc -l", depends_on=["t1"]),
    ])


def _session(*outcomes):
    return RunSession(
        id="run_1", envelope_id="env_1", plan_id="plan_1",
        status=RunStatus.SUCCEEDED,
        verification=VerificationResult(ok=True, envelope_id="env_1", plan_id="plan_1", duration_ms=1.0),
        steps=list(outcomes),
    )


def _outcome(step_id, stdout, status="succeeded"):
    return StepOutcome(step_id=step_id, status=status, approved_by="allowlist",
                       rc=0, stdout=stdout, duration_ms=1.0, started_at="", error=None)


class _FakeCompletions:
    def __init__(self, result):
        self._result = result

    async def create(self, **kwargs):
        return self._result


class _FakeClient:
    def __init__(self, result):
        self.chat = type("C", (), {"completions": _FakeCompletions(result)})()


class _RaisingClient:
    class chat:
        class completions:
            @staticmethod
            async def create(**kwargs):
                raise RuntimeError("llm down")


def test_collect_outputs_pairs_output_with_kind():
    outs = collect_outputs(_session(_outcome("t1", "sea foam / white crests"), _outcome("s1", "2")), _plan())
    assert [o.step_id for o in outs] == ["t1", "s1"]
    assert outs[0].kind == "task"
    assert outs[0].output == "sea foam / white crests"
    assert outs[1].kind == "shell"


async def test_synthesize_uses_llm_answer_when_available():
    answer_obj = type("A", (), {"answer": "A haiku with 2 lines."})()
    res = await synthesize("write a haiku then count lines",
                           _session(_outcome("t1", "sea"), _outcome("s1", "2")),
                           _plan(), client=_FakeClient(answer_obj))
    assert isinstance(res, SynthesisResult)
    assert res.used_llm is True
    assert res.answer == "A haiku with 2 lines."


async def test_deterministic_fallback_when_llm_disabled():
    res = await synthesize("q", _session(_outcome("t1", "OUTPUT-ALPHA"), _outcome("s1", "OUTPUT-BETA")),
                           _plan(), use_llm=False)
    assert res.used_llm is False
    assert "OUTPUT-ALPHA" in res.answer
    assert "OUTPUT-BETA" in res.answer


async def test_synthesize_falls_back_when_client_raises():
    res = await synthesize("q", _session(_outcome("t1", "KEEP-ME")), _plan(), client=_RaisingClient())
    assert res.used_llm is False
    assert "KEEP-ME" in res.answer


async def test_failed_steps_are_marked_not_dropped():
    outs = collect_outputs(_session(_outcome("t1", "boom", status="failed")), _plan())
    assert outs[0].status == "failed"
