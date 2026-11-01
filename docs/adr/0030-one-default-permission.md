# ADR-0030: One default permission per project

- **Status:** Accepted (owner rulings of 2026-11-06)
- **Date:** 2026-11-07
- **Builds on:** [ADR-0029](0029-pando-branches.md) (Pando branches), the steward's
  envelope proposals, and the tree's roots (`daisugi tree root`).

## Context

A worker the steward spawns runs under its project's envelope,
`<config dir>/envelopes/<project>.json`. Only the owner wrote that file: the steward
proposed one, and the owner approved a card. On 2026-11-06 the first real project (Trellis)
stalled on that path again and again: a card for each new envelope, a root that kept the
old envelope after an approve, a worker that asked for the whole budget, and a run cap that
killed a worker with many short steps at 30 s.

The owner ruled:

> The steward should approve most things ... something we've done before and have set up on
> this box.

> One default permission per project: the steward grants known projects without a card.

Push, release, deploy and money still go to the owner.

## Decision

1. **A known project gets the default envelope with no card.** A project is known when its
   dir is under a trusted root: `[steward] trusted_roots` (default `~/code`) and the pinned
   projects. The first spawn into a known project with no envelope file writes the default
   (`clients/go/envdefault`): read and write the project dir, any one-word shell command and
   `./script`, compound commands checked part by part, no network steps, no budget, the
   validator's step cap (30 s), stakes `low`. The file says `"granted_by": "default"`.
2. **The deny list is one list, and it is an invariant.** `git push`, `gh release`,
   `gh pr merge`, `flutter build appbundle`, `fastlane`, `stripe`, `npm publish` and
   `cargo publish` are each an envelope invariant: `forall_steps` over shell steps,
   `not_matches` on the command. The warden enforces them with the predicate algebra it has.
   No field is added to the envelope model, so the verifier and warden cross-check (Go, Rust,
   Python) is untouched. Every envelope the steward proposes carries them, unless the
   proposal names a verb in `allow` and the owner approves the card.
3. **A card appears only for more than the default.** `envelope.propose` compares the
   proposal with the default field by field. A proposal that fits a known project is written
   at once. One that is wider, or one for a project outside the trusted roots, is a card, and
   the card lists each way it is wider.
4. **A root per envelope file.** The tree root of a project is
   `project-<name>-<hash>`, the hash of the envelope bytes and the budget. A changed file
   names a new root, so the next worker runs under it.
5. **A budget is asked for, never assumed.** An envelope with no budget asks for none. A
   worker of a budgeted project asks the root for what it has left.
6. **The envelope's cap is per step.** `max_execution_time_s` caps each bash call sprig makes
   (`SPRIG_STEP_TIMEOUT_S`). `tree run --max-run-s` caps a whole run; it has no default. A task
   with more than a few steps, or any build or test loop, is a lead (`long_running: true`).

## Consequences

- The daily path needs no tap for a project under `~/code` or a pinned one.
- The deny list is a guard rail on the command text. A script, an alias or a renamed copy of a
  tool that pushes is not seen. The kernel fence and missing credentials are the walls behind
  it, and the fence is on only when the warden is on and the kernel has Landlock ABI 9, or
  `fence = "on"`. So the default is granted with no card only to a worker that will run
  fenced; with the fence off, the first spawn cards and says so.
- A proposal that fits is granted the default envelope itself, never its own bytes. A relative
  path, a `..` or a link out of the project, invariants of its own, or a file the owner wrote
  make it wider.
- A steward worker's run is capped by `[steward] worker_max_run_s` (1800 s). A lead has no cap.
- An approved envelope now carries the deny invariants too. A project that must push needs a
  card with `allow`.
- An envelope file is keyed by the dir's base name. The file records its dir, and a file
  written for another dir of the same name is refused.
- Roots of old envelope files stay in the tree. They hold no running worker and cost a few
  lines.
- The Rust and Python twins do not build this (owner ruling 2026-11-06: the floor is Go only).
