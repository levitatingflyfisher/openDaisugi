"""Per-step model sizer: difficulty → model, budgeted (v0.32)."""

from __future__ import annotations

from opendaisugi.budget import BudgetTracker
from opendaisugi.models import ActionPlan, ShellStep, SkillStep, TaskStep
from opendaisugi.model_sizer import (
    DEFAULT_LADDER,
    ModelLadder,
    ModelRung,
    StepSizing,
    estimate_step_difficulty,
    size_plan,
    size_step,
)


def test_difficulty_is_per_step_and_text_driven_for_tasks():
    easy = TaskStep(id="t1", prompt="say hello")
    hard = TaskStep(id="t2", prompt="design a distributed consensus algorithm and prove it correct")
    assert estimate_step_difficulty(hard) > estimate_step_difficulty(easy)


def test_mechanical_steps_are_low_difficulty():
    shell = ShellStep(id="s1", command="ls -la")
    task = TaskStep(id="t1", prompt="architect a fault-tolerant migration strategy")
    assert estimate_step_difficulty(shell) < estimate_step_difficulty(task)


def test_skill_reuse_is_cheapest():
    skill = SkillStep(id="k1", skill_id="tidy")
    assert estimate_step_difficulty(skill) <= 0.1


def test_fan_in_raises_difficulty():
    lone = TaskStep(id="t1", prompt="summarize")
    integrative = TaskStep(id="t2", prompt="summarize", depends_on=["a", "b", "c", "d"])
    assert estimate_step_difficulty(integrative) > estimate_step_difficulty(lone)


def test_easy_step_sizes_to_a_cheap_rung_hard_to_frontier():
    easy = size_step(TaskStep(id="t1", prompt="say hi"))
    hard = size_step(TaskStep(id="t2", prompt="prove this concurrency algorithm is deadlock-free and race-free"))
    assert easy.tier != "frontier"
    assert hard.tier == "frontier"
    assert isinstance(easy, StepSizing)


def test_plain_reasoning_task_routes_to_free_local_model():
    # The token-saving default: an un-signaled reasoning subtask goes to the
    # local (free) rung, not a cloud model.
    sized = size_step(TaskStep(id="t1", prompt="summarize the meeting notes into three bullets"))
    assert sized.tier == "local"


def test_budget_pressure_downgrades_model():
    hard = TaskStep(id="t2", prompt="prove this concurrency algorithm is deadlock-free and optimize the schema")
    # Unbudgeted: routes to frontier.
    assert size_step(hard).tier == "frontier"
    # A budget too small for the frontier rung forces a downgrade.
    tight = BudgetTracker(total_tokens=2500)  # < frontier est_tokens (4500)
    sized = size_step(hard, budget=tight)
    assert sized.tier != "frontier"
    assert sized.downgraded is True


def test_unaffordable_even_at_cheapest_is_flagged():
    hard = TaskStep(id="t2", prompt="design a fault-tolerant distributed migration and prove correctness")
    broke = BudgetTracker(total_tokens=10)  # cannot afford any rung
    sized = size_step(hard, budget=broke)
    assert sized.affordable is False


def test_size_plan_returns_one_sizing_per_step():
    plan = ActionPlan(source="t", task="demo", steps=[
        TaskStep(id="t1", prompt="a"),
        ShellStep(id="s1", command="ls", depends_on=["t1"]),
    ])
    sizings = size_plan(plan)
    assert [s.step_id for s in sizings] == ["t1", "s1"]


def test_target_model_is_honored_as_starting_rung():
    # An easy step would size to 'local', but an explicit target lifts it.
    easy = TaskStep(id="t1", prompt="say hi")
    sized = size_step(easy, target_model=DEFAULT_LADDER.rungs[-1].model)
    assert sized.tier == "frontier"


def test_target_model_off_ladder_falls_back_to_difficulty():
    easy = TaskStep(id="t1", prompt="say hi")
    sized = size_step(easy, target_model="some-unknown-model")
    assert sized.tier != "frontier"  # fell back to difficulty-based sizing


def test_target_model_still_downgrades_under_budget():
    easy = TaskStep(id="t1", prompt="say hi")
    tight = BudgetTracker(total_tokens=1500)  # < frontier est
    sized = size_step(easy, target_model=DEFAULT_LADDER.rungs[-1].model, budget=tight)
    assert sized.tier != "frontier"
    assert sized.downgraded is True


def test_rung_for_model_lookup():
    assert DEFAULT_LADDER.rung_for_model(DEFAULT_LADDER.rungs[0].model) is DEFAULT_LADDER.rungs[0]
    assert DEFAULT_LADDER.rung_for_model("nope") is None


def test_custom_ladder_is_honored():
    ladder = ModelLadder([
        ModelRung(name="tiny", model="tiny-model", max_difficulty=1.0, est_tokens=500),
    ])
    sized = size_step(TaskStep(id="t1", prompt="anything at all here"), ladder=ladder)
    assert sized.model == "tiny-model"
    assert sized.tier == "tiny"
