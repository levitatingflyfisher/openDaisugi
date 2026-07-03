# ADR-0001: Fail-closed is the default posture

- **Status:** Accepted
- **Date:** 2026-07-02 (documenting a decision load-bearing since v0.1)

## Context

openDaisugi's whole job is to answer one question safely: *may this action run?*
The failure modes are not symmetric. A **fail-closed** error rejects something
that was actually safe — annoying, recoverable, visible. A **fail-open** error
admits something that was actually unsafe — silent, and it defeats the entire
reason the library exists. For a verification tool, a fail-open is not a bug of
degree; it is a breach of contract. (The v0.34 security campaign was, almost
entirely, the hunt for fail-opens that had crept into the verifier.)

## Decision

When correctness cannot be *proven*, deny. Concretely, across the codebase:

- Unprovable subsumption ⇒ `holds = False`, not "probably fine."
- An undeclared capability, an unknown step type, an unparseable input, a Z3
  `unknown`/timeout, an unsupported regex/glob, a soft constraint we can't verify
  ⇒ treated as a violation, not waved through.
- Missing backing data for a declared invariant (e.g. a workspace invariant with
  no `workspace_bounds`) ⇒ rejected as vacuous, not silently unenforced.
- Provenance we can't establish (unsigned/untrusted when a trust set is supplied,
  no trust anchor for a signed artifact) ⇒ rejected.

## Consequences

- **Buys:** the library's core promise is trustworthy — "verify returned ok" means
  something. Errors surface loudly and early.
- **Costs:** more false rejections. Legitimate-but-unusual plans get denied and
  need an explicit widening (or `strict=False` at low stakes). This is the price
  we choose to pay, deliberately.
- **Forecloses:** any "best-effort" or "warn-but-continue" default in the
  verification core. Warnings are allowed *in addition to* a decision, never *in
  place of* denying an unprovable claim.

## Alternatives considered

- **Fail-open with warnings** (log the doubt, allow the action): rejected — it
  makes the verifier decorative. The user can always opt into lower stakes for a
  softer posture; the *default* must be safe.
- **Three-valued "unknown" surfaced to the caller without a decision:** rejected
  as the default — pushes the safety decision onto every call site, which is
  exactly the mistake harnesses make. We still expose the reasons, but we also
  make the safe call.
