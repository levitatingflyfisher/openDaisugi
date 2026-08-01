"""Stage 10 — the rationale-durability ledger (typed strata store).

On an irreducible one-off — a heisenbug hunt, a novel design — there is no
internal repetition to compile and no prior corpus to reuse. What compaction
drops there is the *deliberation*: discovered facts, ruled-out hypotheses and
why, mid-task constraints, the goal/subgoal stack. openDaisugi externalizes
enforcement state (the envelope, the deed ledger) but not reasoning, so a
compacted agent re-explores dead branches and re-discovers facts it already had.

This is a *store plus a reconstruction API a harness calls* — openDaisugi does not
rewrite its own prompts. Facts and hypotheses inform reasoning but NEVER gate
actions; only constraint-promotion touches authority, and it flows through the
same monotone-narrowing tightening check (``verify_inheritance``) a captured
invariant may only tighten.
"""

from __future__ import annotations

from opendaisugi import strata
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileWriteStep,
    Invariant,
    Permission,
)
from opendaisugi.verify import verify


def _multitenant_env() -> Envelope:
    return Envelope(
        generated_by="test",
        task="multi-tenant",
        permissions=Permission(
            file_read=["/data/**"],
            file_write=["/data/tenantX/**", "/data/tenantY/**"],
        ),
    )


def _tenant_x_invariant() -> Invariant:
    # forall file_write steps: path does NOT start with /data/tenantX/ (a HARD Z3
    # regex node — verified empirically to make verify() reject, not a soft node).
    return Invariant(
        type="no_tenant_x",
        description="no writes under tenant X",
        enforce=True,
        expr={
            "op": "forall_steps",
            "pred": {
                "op": "implies",
                "a": {"op": "equals", "path": "type", "value": "file_write"},
                "b": {"op": "not_matches", "path": "path", "regex": "^/data/tenantX/"},
            },
        },
    )


def _write(path: str) -> ActionPlan:
    return ActionPlan(
        source="t", task="t", steps=[FileWriteStep(id="w", path=path, content="x")]
    )


# --------------------------------------------------------------------------- #
# A1. the typed store + the emission hook
# --------------------------------------------------------------------------- #
def test_emit_assigns_monotonic_seq_and_stores_the_stratum():
    store = strata.StrataStore()
    a = store.emit("fact", "the bug reproduces only with TZ=UTC", provenance="step-3")
    b = store.emit("hypothesis", "it's a leap-second bug", status="ruled_out")
    assert a.kind == "fact" and a.provenance == "step-3"
    assert a.seq == 1 and b.seq == 2  # monotonic, deterministic ordering
    assert [s.id for s in store.all()] == [a.id, b.id]
    assert store.get(a.id) is a
    assert store.get("nope") is None


def test_emit_records_all_four_kinds():
    store = strata.StrataStore()
    for kind in ("fact", "hypothesis", "constraint", "goal"):
        store.emit(kind, f"a {kind}")
    assert sorted({s.kind for s in store.all()}) == ["constraint", "fact", "goal", "hypothesis"]


# --------------------------------------------------------------------------- #
# A2. context reconstruction — pinned survives, the rest is lossy and honest
# --------------------------------------------------------------------------- #
def test_reconstruct_always_includes_pinned_and_open_constraints():
    store = strata.StrataStore()
    con = store.emit("constraint", "don't touch tenant X", pinned=True)
    for i in range(20):
        store.emit("fact", f"fact {i}")
    ctx = store.reconstruct_context(budget=3)  # tiny budget
    assert con.id in {s.id for s in ctx.strata}  # the pinned constraint never drops
    assert con.id in {s.id for s in ctx.pinned}
    assert con.id not in ctx.dropped_ids


def test_reconstruct_is_lossy_and_says_so():
    store = strata.StrataStore()
    for i in range(10):
        store.emit("fact", f"fact {i}")
    ctx = store.reconstruct_context(budget=3)
    assert len(ctx.dropped_ids) > 0
    # The honest boundary: a dropped fact is a fact the agent will re-derive.
    assert "re-derive" in ctx.note.lower() or "lossy" in ctx.note.lower()


def test_reconstruct_prefers_tag_matches_then_recency():
    store = strata.StrataStore()
    old_tagged = store.emit("fact", "old but relevant", tags=["auth"])
    for i in range(10):
        store.emit("fact", f"noise {i}")  # newer, untagged
    recent_tagged = store.emit("fact", "recent and relevant", tags=["auth"])
    ctx = store.reconstruct_context(budget=2, tags=["auth"])
    ids = {s.id for s in ctx.strata}
    assert recent_tagged.id in ids and old_tagged.id in ids  # both tag hits beat untagged noise


def test_repage_returns_verbatim_dropped_detail():
    store = strata.StrataStore()
    detailed = store.emit("fact", "the full stack trace: " + "x" * 500)  # oldest, untagged
    for i in range(10):
        store.emit("fact", f"fact {i}", tags=["hot"])  # newer + tagged → outrank it
    ctx = store.reconstruct_context(budget=2, tags=["hot"])
    assert detailed.id in ctx.dropped_ids  # force the premise: it MUST be dropped here
    assert store.repage(detailed.id).content == detailed.content  # still verbatim-recoverable


def test_ruled_out_hypotheses_are_retained_so_branches_are_not_reexplored():
    store = strata.StrataStore()
    dead = store.emit("hypothesis", "it's a race in the writer", status="ruled_out")
    ctx = store.reconstruct_context()  # no budget → everything, ruled-out included
    assert dead.id in {s.id for s in ctx.strata}


def test_store_json_roundtrips_including_seq():
    store = strata.StrataStore()
    store.emit("fact", "one")
    store.emit("constraint", "two", pinned=True)
    restored = strata.StrataStore.from_json(store.to_json())
    assert [(s.kind, s.content, s.seq, s.pinned) for s in restored.all()] == [
        (s.kind, s.content, s.seq, s.pinned) for s in store.all()
    ]
    # A restored store keeps counting from where it left off — no seq collision.
    nxt = restored.emit("fact", "three")
    assert nxt.seq == 3


# --------------------------------------------------------------------------- #
# A3. the re-derivation meter (mechanism now; model-driven numbers deferred)
# --------------------------------------------------------------------------- #
def test_rederivation_ledger_reports_saved_and_is_evidence_not_proof():
    led = strata.RederivationLedger(
        output_tokens_without_store=5000,
        output_tokens_with_store=1200,
        rederived_facts=7,
        reexplored_branches=2,
    )
    assert led.tokens_saved == 3800
    assert led.evidence_not_proof is True
    assert "defer" in led.note.lower() or "model" in led.note.lower()


# --------------------------------------------------------------------------- #
# B1. the guardrail — only a constraint touches authority
# --------------------------------------------------------------------------- #
def test_facts_and_hypotheses_never_touch_authority():
    """The load-bearing guardrail: a fact / hypothesis / goal can NEVER modify the
    envelope — only a constraint may, and only via promote_constraint. Tripwire: the
    envelope is byte-identical after emitting non-constraints and promotion refuses them."""
    store = strata.StrataStore()
    env = _multitenant_env()
    before = env.model_dump_json()

    fact = store.emit("fact", "tenant X uses schema v3")
    hypo = store.emit("hypothesis", "the leak is in tenant X", status="ruled_out")
    goal = store.emit("goal", "migrate tenant Y")
    store.reconstruct_context()

    assert env.model_dump_json() == before  # store ops never touch the envelope
    for non_constraint in (fact, hypo, goal):
        result = strata.promote_constraint(
            env, non_constraint, remove_file_write=["/data/tenantX/**"]
        )
        assert result.ok is False
        assert result.envelope.model_dump_json() == before  # unchanged
        assert non_constraint.kind in result.reason


# --------------------------------------------------------------------------- #
# B2. promotion tightens, only tightens, actually tightens, actually enforces
# --------------------------------------------------------------------------- #
def test_promote_by_removing_a_glob_tightens_and_verify_denies():
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "don't touch tenant X", pinned=True)
    result = strata.promote_constraint(env, con, remove_file_write=["/data/tenantX/**"])
    assert result.ok is True
    assert con.status == "promoted"
    assert verify(_write("/data/tenantX/secret.txt"), result.envelope).ok is False
    assert verify(_write("/data/tenantY/ok.txt"), result.envelope).ok is True


def test_promote_by_invariant_enforces_and_denies_its_witness():
    """The advisor's crux: assert the promoted invariant actually causes verify() to
    REJECT — a promotion that passes the tightening gate but compiles to a soft node is
    a constraint that doesn't constrain."""
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "don't touch tenant X", pinned=True)
    witness = _write("/data/tenantX/secret.txt")
    result = strata.promote_constraint(
        env, con, add_invariant=_tenant_x_invariant(), deny_witness=witness
    )
    assert result.ok is True, result.reason
    assert result.enforcement_proven is True  # a witness was supplied and was rejected
    assert verify(witness, result.envelope).ok is False  # actually denied
    assert verify(_write("/data/tenantY/ok.txt"), result.envelope).ok is True


def test_enforcement_proven_is_false_without_a_witness():
    """A glob-removal promotion enforces structurally, but without a witness the result
    honestly reports it was not *proven* to enforce — a harness can tell the two apart."""
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "don't touch tenant X")
    result = strata.promote_constraint(env, con, remove_file_write=["/data/tenantX/**"])
    assert result.ok is True
    assert result.enforcement_proven is False  # tightened, but no witness proof


def test_promote_refuses_an_already_promoted_constraint():
    """Re-promoting a constraint against the original envelope would build a second
    tightening that silently drops the first — refuse it."""
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "don't touch tenant X")
    first = strata.promote_constraint(env, con, remove_file_write=["/data/tenantX/**"])
    assert first.ok is True and con.status == "promoted"
    second = strata.promote_constraint(
        first.envelope, con, remove_file_write=["/data/tenantY/**"]
    )
    assert second.ok is False
    assert "already promoted" in second.reason


def test_promote_rejects_a_loosening_candidate():
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "widen writes")
    loosened = env.model_copy(
        update={
            "permissions": env.permissions.model_copy(
                update={"file_write": [*env.permissions.file_write, "/etc/**"]}
            )
        }
    )
    result = strata.promote_constraint(env, con, candidate=loosened)
    assert result.ok is False
    assert result.violations  # verify_inheritance caught the widening
    assert result.envelope.model_dump_json() == env.model_dump_json()  # unchanged
    assert con.status == "open"  # not marked promoted


def test_promote_rejects_a_noop_that_constrains_nothing():
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "a constraint with no enforceable delta")
    result = strata.promote_constraint(env, con)  # no invariant, no removal
    assert result.ok is False
    assert "no enforceable effect" in result.reason


def test_promote_rejects_an_unenforced_witness():
    """If the promoted constraint does not actually deny the witness (e.g. it compiled
    soft, or the witness isn't what it constrains), the promotion is refused — the
    fail-open shape of this stage."""
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "don't touch tenant X")
    # The tenant-X invariant does NOT deny a tenant-Y write, so a tenant-Y witness is
    # not actually denied → the enforceability gate must refuse.
    result = strata.promote_constraint(
        env, con, add_invariant=_tenant_x_invariant(), deny_witness=_write("/data/tenantY/ok.txt")
    )
    assert result.ok is False
    assert "does not actually deny" in result.reason


# --------------------------------------------------------------------------- #
# B3. criterion 3 — the tenant-X scenario held end-to-end under forced compaction
# --------------------------------------------------------------------------- #
def test_tenant_x_held_end_to_end_under_forced_compaction():
    store = strata.StrataStore()
    env = _multitenant_env()
    con = store.emit("constraint", "don't touch tenant X", pinned=True)
    for i in range(30):
        store.emit("fact", f"noise {i}")  # bulk that a budget would drop

    witness = _write("/data/tenantX/secret.txt")
    promotion = strata.promote_constraint(
        env, con, add_invariant=_tenant_x_invariant(), deny_witness=witness
    )
    assert promotion.ok is True
    tightened = promotion.envelope

    # FORCED COMPACTION: the transcript is gone; only the durable store survives.
    survivor = strata.StrataStore.from_json(store.to_json())
    ctx = survivor.reconstruct_context(budget=3)  # tiny window, most facts dropped

    # The promoted constraint survived reconstruction under a punishing budget…
    # (con.id is from the pre-compaction store; from_json preserves ids, which is the
    # whole point of the round-trip — the same constraint is found in the survivor.)
    assert con.id in {s.id for s in ctx.strata}
    # …and the enforcement envelope still denies tenant X after compaction.
    assert verify(witness, tightened).ok is False
    assert verify(_write("/data/tenantY/ok.txt"), tightened).ok is True
