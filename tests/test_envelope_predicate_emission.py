"""Confirms generate_envelope emits predicate-tree invariants/postconditions when asked."""

from __future__ import annotations

from opendaisugi.envelope import build_envelope_prompt_hint


def test_prompt_hint_mentions_predicate_algebra():
    hint = build_envelope_prompt_hint()
    assert "forall_steps" in hint or "predicate" in hint.lower()
    assert "equals" in hint


def test_prompt_hint_lists_full_primitive_vocabulary():
    hint = build_envelope_prompt_hint()
    for primitive in (
        "equals", "not_equals", "in_set", "matches", "numeric_range",
        "exists", "and", "or", "not", "implies",
        "forall_steps", "exists_step", "alias", "llm_check",
    ):
        assert primitive in hint, f"missing primitive in prompt hint: {primitive}"


def test_system_prompt_includes_predicate_hint():
    from opendaisugi.envelope import ENVELOPE_SYSTEM_PROMPT
    assert "forall_steps" in ENVELOPE_SYSTEM_PROMPT
