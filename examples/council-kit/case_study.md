# Council of AIs — Structural Gates Around Perceptual Judgment

Your governance body shouldn't multitask. A council of LLM evaluators
is the right pattern for *perceptual* questions ("is this valuable,
clear, worth including?") but the wrong pattern for *structural* checks
("does this contain an AWS secret or PII?"). When you load council
members with both jobs, the same priority-arbitration failure that bit
Ada Lin's Robin bites the council: member reads a genuinely
valuable snippet that happens to contain a secret, weighs value against
risk, and value wins.

Opendaisugi's answer is explicit staging:

1. **Pre-council gate** (deterministic, structural) — `no_secrets`,
   `no_pii_regex`, `size_bounded` run before any LLM sees the
   contribution. Deterministic rejects happen before council bandwidth
   is burned.
2. **Council members** each carry a scoped envelope: read-only file
   permissions, structured-JSON output via `structured_approval`, no
   shell, no outbound network except the KB endpoint.
3. **Post-approval gate** (deterministic, structural, redundant) —
   same checks re-run before the approved contribution commits to the
   shared KB, catching council hallucination or race conditions.

The council's real job — *perception* — becomes smaller, cleaner, and
harder to corrupt. Run `pytest test_council_pii_rejection.py` to see
the pre-council gate catch secrets and PII without consulting any LLM.
