#!/usr/bin/env python3
"""Zero-inference reuse receipt, verified offline.

Run it. No API key; the only network touch is a one-time download of the small
local embedder, cached after the first run. It seeds the distilled pathway the
Distiller produces from repeated successes, then reuses it for a matching prompt
and prints the payoff:

  - the planner (decompose) is SKIPPED — the cached plan is served directly;
  - the shell step runs via subprocess — no inference;
  - the answer is assembled deterministically (--deterministic-synthesis) —
    no synthesis model call.

So the whole run touches no model at all: ``budget.spent == 0``.

    $ python zero_inference_reuse.py

Honesty note: producing the pathway (the Distiller's one-time generalization)
does use a model, once, offline — that cost is measured separately in
``examples/distillation-benchmark/``. What THIS script proves is the *reuse*
payoff, which is the recurring win: the second (third, hundredth) time you run
a task that matches a distilled deterministic pathway, it costs zero inference.
The Z3 guard still re-verifies the reused plan against your envelope every time
— cheap and fail-closed — so "free" never means "unchecked".
"""

from __future__ import annotations

import asyncio
import tempfile
import time
from pathlib import Path

from opendaisugi import Daisugi
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway


def _read_only_envelope(workspace: str) -> Envelope:
    """A read-only, network-off boundary that permits a `grep` over the workspace."""
    real = Path(workspace).resolve()
    return Envelope(
        generated_by="reuse-receipt",
        task="find TODO lines",
        permissions=Permission(
            file_read=["**", f"{real}/**"],
            shell=True,
            shell_allowlist=["grep"],
            network=False,
        ),
        stakes="low",
    )


def _seed_distilled_pathway(store, *, task_description: str, command: str, envelope: Envelope):
    """Put the pathway the Distiller would produce for this recurring task.

    Stamped with the current embedder model/version and a real task embedding
    so ``PathwayStore.find`` matches it exactly as it would a distilled row —
    nothing here is faked past the one-time distillation the benchmark measures.
    """
    from opendaisugi._search import _MODEL_NAME, _get_model
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    embedding = _get_model().encode([task_description], convert_to_numpy=True)[0].tolist()
    template = ActionPlan(
        source="distilled",
        task=task_description,
        steps=[ShellStep(id="s1", command=command)],
    )
    pathway = CompiledPathway(
        id="pathway_demo",
        task_description=task_description,
        task_embedding=embedding,
        embedding_model=_MODEL_NAME,
        embedding_model_version=_EMBEDDING_MODEL_VERSION,
        envelope=envelope,
        plan_template=template,
        source_trace_ids=["demo-trace"],
        distilled_at=time.time(),
    )
    store.put(pathway)


async def _run(data_dir: Path, workspace: Path):
    # A small, self-contained corpus so the demo is deterministic regardless of
    # what TODOs happen to live in the repo when you run it.
    (workspace / "notes.py").write_text(
        "def a():\n    pass  # TODO: handle the empty case\n\n"
        "def b():\n    return 1  # TODO: cover the negative branch\n",
        encoding="utf-8",
    )

    command = f"grep -rn TODO {workspace.resolve()}"
    envelope = _read_only_envelope(str(workspace))

    d = Daisugi(data_dir=data_dir)
    # Seed the distilled pathway (what `daisugi tend` writes after repeated wins).
    _seed_distilled_pathway(
        d.pathway_store,
        task_description="Find lines containing the word TODO in the source files.",
        command=command,
        envelope=envelope,
    )

    # A near-identical recurring prompt — clears the 0.55 similarity threshold.
    prompt = "Find the lines that contain TODO in the source files."
    result = await d.orchestrate(
        prompt,
        envelope=envelope,
        synth_llm=False,  # deterministic assembly → no synthesis model call
    )
    return result


def main() -> int:
    with tempfile.TemporaryDirectory() as td:
        root = Path(td)
        data_dir = root / "daisugi"
        workspace = root / "workspace"
        workspace.mkdir()
        result = asyncio.run(_run(data_dir, workspace))

    step_types = [s.type for s in result.plan.steps]
    print("=== zero-inference reuse receipt ===")
    print(f"prompt reused a distilled pathway : {result.reused_pathway}")
    print(f"plan step types                   : {step_types}")
    print(f"synthesis used an LLM             : {result.used_llm_synthesis}")
    print(f"tokens spent (whole run)          : {result.budget.spent}")
    print()
    print("final answer (from the real grep, assembled deterministically):")
    print("  " + "\n  ".join(result.final_answer.splitlines()))
    print()

    # Self-checking: this file IS the proof, so it fails loudly if the claim breaks.
    assert result.reused_pathway is True, "expected the distilled pathway to be reused"
    assert step_types == ["shell"], f"expected an all-shell plan, got {step_types}"
    assert result.used_llm_synthesis is False, "synthesis should not have called a model"
    assert result.budget.spent == 0, f"expected zero tokens, spent {result.budget.spent}"
    assert "TODO" in result.final_answer, "grep should have found the TODO lines"
    print("OK — reuse of a distilled deterministic pathway cost zero inference,")
    print("     and the Z3 guard re-verified it against the envelope first.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
