# Cheap-model delegation (v0.19+)

The reproduction substrate (v0.18) made selection trustworthy via per-step
receipts and the integrity check. Selection only matters if runs differ in
cost, and runs only differ in cost if some steps execute against a cheap
model. v0.19 wires that.

## When to set `preferred_model`

Set it on a step when the step's work is suited to a small, cheap model
producing structured output. Good fits:

- **Drafting** — first-pass email body, summary, social post, agenda
- **Classification** — "is this draft formal or casual?", "does this email mention payment?"
- **Filtering / triage** — "which of these incoming messages need a human reply?"
- **Routine reformatting** — JSON ↔ YAML, date normalization, casing

```python
DraftEmail(
    id="s0",
    recipient="alice@example.com",
    body="...",
    signature="Ada",
    preferred_model="haiku",   # cheap drafting
)
```

The supervisor will run the step via `DelegatingExecutor` (when registered
for that step type) and prompt the model with the step's serialized fields.
The model's structured response becomes evidence on the receipt; the receipt
records `model_id="haiku"` for selection-signal attribution.

## When NOT to set `preferred_model`

- **Cryptographic operations** — use deterministic code, not an LLM
- **Deterministic shell** — `git status`, `ls`, `cat`, etc. — no model needed
- **Robotic motion (`stakes="physical"`)** — the verifier rejects ANY
  delegation under physical stakes; LLM-produced joint targets cannot be
  statically grounded
- **Authoritative judgement on high-stakes decisions** — escalate to Sonnet
  or Opus, or to a human approval step

## `llm_check` postconditions for perceptual claims

Some checks are inherently perceptual: "did the email content match the
topic?", "is this review substantive?", "does this draft maintain Ada's
tone?". These can't be Z3-proved structurally; they need an LLM verifier.

The predicate algebra ships an `llm_check` primitive (v0.9+):

```python
Postcondition(
    type="review_substantive",
    expr={
        "op": "llm_check",
        "rule": "The review body contains at least one specific critique "
                "of the contribution, not just a thumbs-up or vague praise.",
    },
)
```

When the verifier evaluates this postcondition, `evaluate_llm_check` calls a
cheap LLM (Haiku by default) with the rule + the step's evidence and reads
back yes/no. The result becomes the postcondition's `verify_result`.
Physical-stakes envelopes refuse `llm_check` entirely — sound primitives only.

## The selection signal — `model_id` in the journal

Every receipt now carries the model that produced its evidence. Over time,
the Gardener can query things like:

- "Which steps keep failing integrity when delegated to haiku?"
- "Does sonnet's failure rate on `ReviewDraft` justify the cost vs haiku?"
- "Is opus actually adding value over sonnet on `AggregateVotes`, or am I
  paying for nothing?"

This is the data the v0.19 substrate produces. Future Gardener work surfaces
it as a per-pathway-per-model fitness panel; for now the receipts are
sqlite-queryable directly.

## Pattern: drafter / reviewer split

A common shape: one cheap step drafts, one more capable step reviews.

```
DraftEmail(preferred_model="haiku") → ReviewDraft(preferred_model="sonnet") → SendEmail
```

The drafter handles 90% of cases at 10× lower cost; the reviewer catches the
edge cases the cheap model gets wrong. Per-step receipts make the cost split
visible; if the reviewer rarely overrides the drafter, you can drop the
review step. If it overrides often, the drafter needs a stronger model — the
data tells you which.
