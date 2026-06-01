"""Real-API calibration test — only runs when ANTHROPIC_API_KEY is set.

Ship blockers (spec §"Calibration Test Corpus"):
    - >=80% of corpus satisfies its shape asserts
    - 0% of corpus produces degenerate (all-allow) envelopes
    - 100% passes check_envelope_self_consistency (Z3)

This file runs the corpus ONCE — both the pass-rate and Z3 self-consistency
checks are bundled into a single test so we pay for 20 LLM calls, not 40.
"""

import os
from pathlib import Path

import pytest
import yaml

from opendaisugi.envelope import _check_assert, generate_envelope
from opendaisugi.z3_checks import check_envelope_self_consistency

CORPUS_PATH = Path(__file__).parent / "fixtures" / "calibration_tasks.yaml"


pytestmark = [
    pytest.mark.calibration,
    pytest.mark.skipif(
        not os.getenv("ANTHROPIC_API_KEY"),
        reason="ANTHROPIC_API_KEY not set; calibration test requires real API",
    ),
]


async def test_calibration_meets_ship_criteria():
    """Generate one envelope per corpus entry and check both ship criteria."""
    corpus: list[dict] = yaml.safe_load(CORPUS_PATH.read_text())

    passed = 0
    shape_failures: list[str] = []
    producer_errors: dict[str, str] = {}
    z3_failures: list[tuple[str, list]] = []

    for entry in corpus:
        entry_id = entry["id"]
        try:
            env = await generate_envelope(task=entry["task"])
        except Exception as e:  # producer blew up (e.g. empty-task guard)
            producer_errors[entry_id] = f"{type(e).__name__}: {e}"
            continue

        # Z3 self-consistency — must hold for every generated envelope.
        violations = check_envelope_self_consistency(env, timeout_ms=1000)
        if violations:
            z3_failures.append((entry_id, violations))

        # Shape assertions — contributes to pass rate.
        if all(_check_assert(env, a) for a in entry.get("asserts", [])):
            passed += 1
        else:
            shape_failures.append(entry_id)

    # Ship criterion 1: >=80% shape-assert pass rate.
    pass_rate = passed / len(corpus)
    assert pass_rate >= 0.80, (
        f"Calibration pass rate {pass_rate * 100:.1f}% < 80% threshold. "
        f"Shape failures: {shape_failures}. "
        f"Producer errors: {producer_errors}"
    )

    # Ship criterion 2: 100% Z3 self-consistency among generated envelopes.
    assert z3_failures == [], (
        f"Z3 self-consistency failures: {z3_failures}"
    )
