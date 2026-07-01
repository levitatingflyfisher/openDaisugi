"""The adversarial suite as a merge gate (roadmap Stage 3).

The deterministic layer's defining property: it is exactly reproducible, so
every attack it contains MUST be denied — a miss is a bug, not a flake. That
is what licenses this test being a required check. Both error directions are
asserted: attacks all denied, and no *unexpected* benign false positives
(the known ones are budgeted and reported, not silently tolerated).
"""

from __future__ import annotations

from opendaisugi.adversarial import (
    ATTACKS,
    BENIGN,
    compare_arms,
    corpus_hash,
    run_deterministic_corpus,
)


def test_every_attack_in_the_deterministic_layer_is_denied():
    rep = run_deterministic_corpus()
    assert rep["unexpected_allowed_attacks"] == [], (
        f"the gate ALLOWED attacks it must deny: "
        f"{rep['unexpected_allowed_attacks']}"
    )
    assert rep["attack_denial_rate"] == 1.0


def test_no_unexpected_benign_false_positives():
    rep = run_deterministic_corpus()
    assert rep["unexpected_denied_benign"] == [], (
        f"the gate wrongly denied benign calls not marked known-FP: "
        f"{rep['unexpected_denied_benign']}"
    )


def test_false_positive_rate_is_measured_and_bounded():
    """Both directions are first-class. The FP rate is published; it is not
    zero (compound commands and unmapped tools deny), and it must stay within
    the budget of *known* false positives — a new unexpected FP breaks this."""
    rep = run_deterministic_corpus()
    assert rep["benign_false_positives"] == rep["known_false_positives"]
    assert 0 < rep["false_positive_rate"] < 0.5


def test_corpus_is_content_addressed_and_stable():
    assert corpus_hash() == run_deterministic_corpus()["corpus_hash"]
    assert len(corpus_hash()) == 16


def test_every_attack_category_is_represented():
    cats = {a.category for a in ATTACKS}
    assert {
        "credential-read", "compound-shell", "out-of-pattern-shell",
        "undeclared-mcp", "hook-rewrite", "scheme-smuggle", "unknown-tool",
    } <= cats


def test_gate_beats_the_no_gate_and_literal_glob_arms_on_attacks():
    arms = compare_arms()
    # The gate denies every attack; no-gate denies none.
    assert arms["gate"]["attack_denial_rate"] == 1.0
    assert arms["no_gate"]["attack_denial_rate"] == 0.0
    # Literal glob matching catches the path-based attacks but misses the
    # compound-shell / scheme-smuggle class the solver-backed gate catches.
    assert arms["host_glob_only"]["attack_denial_rate"] < 1.0


def test_provenance_is_recorded_for_every_attack():
    for a in ATTACKS:
        assert a.source, f"{a.id} has no provenance source"
        assert a.adaptation, f"{a.id} has no adaptation note"


def test_benign_corpus_is_shared_shape_for_stage4_reuse():
    # Stage 4 reuses this benign corpus as its task set; assert it's non-empty
    # and every case carries an envelope + payload it can replay.
    assert len(BENIGN) >= 6
    for b in BENIGN:
        assert b.payload and b.permissions
