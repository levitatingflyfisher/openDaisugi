# The backlog, 2026-10-30

Every item from the owner's list of 2026-10-30, in waves. Each plan is its own file here. The ledger is `.superpowers/sdd/backlog/progress.md`.

## Wave 1: small fixes the box needs
- W1-a `tiers setup` against Ollama: send no-thinking, label truncation as truncated, no "via llamafile" on an Ollama host. Plan: `w1-ollama-and-record.md`.
- W1-b Record by default: a plain install records every call (audit mode, blocks nothing); `--enforce` opts into blocking. Same plan.

## Wave 2: the rename, alone
gate -> warden, gateway -> router (inner policy -> routing mode), foreman -> steward, the conversation memory -> Pando R2, and (owner, 2026-10-30) the delegate tool -> spawn. Plan: `w2-rename.md`.

## Wave 3: fan-out (at most 3 builders, one heavy verify at a time)
- Safety gaps: sprig family gate on by default; opaque invariants; shell write targets; `cp -r`, `tar`, `find | xargs cat`.
- Recording for more harnesses (pi, OpenCode, Hermes, OpenClaw).
- `daisugi recipe apply`.
- Argument-level MCP policy.
- Small parked fixes (port-neutral SKILL.md body, data-dir edge, preflight line twice, box-gotest exit codes, relative-root traceback parity, Python oracle `start --data-dir ../dd`).
- Install only what I name; branching; worker training; constrained decoding for local models; garden view; RTS floor view.
- Router parts 0 to 4; grafts; GEPA over graft rules.
- Ranking as a primitive (judges weigh by a Thompson sample; cards decay in 14 days); delegation tree follow-ons.
- Robot kernel gaps (tuple indexing in paths, a force field, a delta-sat verdict class for nonlinear dynamics): `w3-robot-kernel-gaps.md` (to be written).
- Fewer dependencies (our own QR encoder replaces two modules; the Stitch pack is optional); self-testing and faster test runs (CI runs on push and by hand, no timer).

## Design track (no builds)
- ADR: Pando R2 moves from coppice into daisugi.
- ADR: Commonplace (semantic memory: the owner's notes plus an optional index).
- Owner walkthrough of DECISIONS-PENDING.md.

## Box-gated (build against fakes, verify on the box later)
Hermes on Ollama, pi live test, friend-setup compat run, night benchmark timer, Rust suite on the box, Muse Glimmer timing.

## Long term
Full Rust parity (about 95k lines): many sessions; queued, not finished this round.
