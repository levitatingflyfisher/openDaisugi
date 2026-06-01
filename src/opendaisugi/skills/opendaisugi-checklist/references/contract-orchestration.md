# Z3-backed contracts between agents

The v0.9 predicate algebra (see `models.Invariant`, `models.Postcondition`) compiles to SMT-LIB2 and Z3 solves. For cross-step claims — the kind a per-call allowlist cannot express — this is opendaisugi's distinguishing capability vs Claude Code's built-in permissions.

## When Z3 earns its keep

**Good fit — structural/quantified claims Z3 can actually solve:**

- "*For all* steps of type `SendEmail`, `step.metadata.signature != 'Robin'` when `recipient_type == 'Robin_contact'`" — quantifier over a collection
- "*Count* of steps with `approve=true` ≥ quorum_M" — arithmetic
- "*Forall* reviews: `pii_flag == false`" — quantifier over a subset
- "*Implies*: if any step writes to `/prod/**`, there exists a step with `approved_by == human` earlier in the DAG" — logical implication across the plan
- "*Consistency*: envelope permission `file_write: []` AND postcondition `file_exists(...)` — Z3 proves these inconsistent"

**Poor fit — semantic/judgement claims Z3 cannot handle:**

- "Is the email well-written" → LLM-as-judge step
- "Does this code pass review" → LLM-as-judge step
- "Does this respect user intent" → human or LLM step, not invariant

Rule of thumb: structural claims go in envelope invariants; perceptual claims go in explicit review steps.

## Contract shape between orchestrator and sub-agent

A contract between agents is an envelope invariant that names the responsibility one agent has to the other:

- Orchestrator authorizes the sub-agent with an envelope that constrains what the sub-agent may produce
- Sub-agent produces a plan; orchestrator runs `verify()` against the envelope
- If verify rejects, sub-agent sees the violations (including `suggested_remediation`) and revises
- If verify passes, orchestrator executes under the supervisor
- Per-step receipts + integrity check ensure the sub-agent's claims of completion are evidence-backed

This is what makes the Ada email kit work: Ada (orchestrator) delegates drafting to a sub-agent under an envelope whose invariant says "no draft that would send as Robin to a Robin contact." Either the sub-agent's draft satisfies the invariant or Ada never sends it.

## Authoring an invariant

```yaml
invariants:
  - type: no_impersonation
    description: "No step may send email with Robin's signature to a Robin contact"
    enforce: true
    expr:
      op: forall_steps
      pred:
        op: implies
        a:
          op: equals
          path: "type"
          value: "send_email"
        b:
          op: not
          child:
            op: and
            args:
              - { op: equals, path: "metadata.signature", value: "Robin" }
              - { op: equals, path: "metadata.recipient_type", value: "Robin_contact" }
```

`op` names pick from the v0.9 closed vocabulary (`forall_steps`, `exists_step`, `and`, `or`, `not`, `implies`, `equals`, `not_equals`, `in_set`, `matches`, `numeric_range`, `exists`, `depends_on`, `before`). The compiler lowers these to SMT-LIB2 and Z3 solves.

## Multi-party contracts

Several agents can each hold signed envelopes (v0.15 ed25519 contract signing) that compose via permission intersection. A sub-agent's envelope MUST be a tightening of the orchestrator's envelope — opendaisugi's subsumption check enforces this. If an orchestrator delegates to sub-agent A with "file_write: ./src/**" and sub-agent A delegates to sub-agent B with "file_write: ./src/tests/**", B can write tests but not production code, and A can write production code but not outside ./src/. Contracts nest.
