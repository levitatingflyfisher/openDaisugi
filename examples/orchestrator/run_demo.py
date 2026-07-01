"""Orchestrator — run a whole prompt end to end, offline. Runnable demo.

Shows the forward-looking pipeline:

    prompt → decompose (typed-step DAG, verified) → size (cheapest capable model
    under budget) → supervised execute (each step re-verified) → synthesize

To keep this runnable WITHOUT an API key, the two LLM stages (decompose and
synthesize) are given injected fake clients. In real use you would just call
``await Daisugi().orchestrate(prompt, budget_tokens=...)`` and the library wires
the real model calls for you (see the commented block at the bottom).

Run:  python run_demo.py
"""

import asyncio

from opendaisugi import (
    DEFAULT_LADDER,
    Envelope,
    Orchestrator,
    Permission,
    size_plan,
)
from opendaisugi.decomposer import DecomposedPlan, DecomposedStep


# --- fake LLM clients so the demo runs offline ------------------------------

class _FakeCompletions:
    def __init__(self, result):
        self._result = result

    async def create(self, **kwargs):
        return self._result


class _FakeClient:
    def __init__(self, result):
        self.chat = type("C", (), {"completions": _FakeCompletions(result)})()


async def main() -> None:
    # 1. The authorization boundary. The decomposed plan must verify against this,
    #    and every step is re-verified at execution time. Here we allow only echo.
    envelope = Envelope(
        generated_by="demo",
        task="print two greetings and combine them",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )

    # 2. What the decomposer "returns" (offline). In real use the LLM authors this.
    decomposed = DecomposedPlan(steps=[
        DecomposedStep(id="s1", type="shell", command="echo hello"),
        DecomposedStep(id="s2", type="shell", command="echo world", depends_on=["s1"]),
    ])

    orch = Orchestrator()
    result = await orch.orchestrate(
        "print two greetings and combine them",
        envelope=envelope,
        budget_tokens=20_000,                       # gates routing DURING the run
        decompose_client=_FakeClient(decomposed),
        synth_client=_FakeClient(type("A", (), {"answer": "hello world"})()),
    )

    print("final answer :", result.final_answer)
    print("status       :", result.status)
    print("reused path  :", result.reused_pathway)
    print("budget       :", result.budget.spent, "tokens across",
          result.budget.step_count, "model call(s)")
    print("per-step sizing:")
    for s in result.sizings:
        print(f"  {s.step_id}: difficulty={s.difficulty:.2f} → {s.tier} ({s.model})")

    # The sizer is composable on its own — inspect what a plan would cost without
    # running it:
    print("\nstandalone sizing of the plan:")
    for s in size_plan(result.plan, ladder=DEFAULT_LADDER):
        print(f"  {s.step_id}: {s.tier} (~{s.est_tokens} tok)")

    # --- Real use (needs a model / API key) ---------------------------------
    #   from opendaisugi import Daisugi
    #   result = await Daisugi().orchestrate(
    #       "summarize the open PRs and draft a standup note",
    #       budget_tokens=20_000,
    #   )
    #   print(result.final_answer)


if __name__ == "__main__":
    asyncio.run(main())
