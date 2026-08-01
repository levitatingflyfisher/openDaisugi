# Protect an agent you're already running

*Learning-oriented — from nothing to a working shadow-mode gate over a live
session in a few minutes, then, when you trust it, one flag to enforce.*

You have an agent (Claude Code) running in a project and you want a runtime
guarantee that it cannot read, write, or run outside a scope you set — the
call-time gate ([ADR-0007](../adr/0007-call-time-gate.md)). This is the fast
path. For the reference on every flag, see [Gate a live session](../how-to/gate.md).

## Prerequisites

- `daisugi` installed (`pip install -e .` in this repo, or your package install).
- The `claude` CLI on your PATH, logged in.

## One command

From your project directory:

```bash
daisugi start
```

Four steps, each reported done / skipped / failed / would: find the harness,
install the gate hook into **this directory's** `.claude/settings.json`
(never a machine-global hook — a second project you `start` later gets its own
hook and its own envelope, never falls back to this one's), register a
**starter envelope** keyed to this directory — read/write within the project,
a conservative shell allowlist, no network — start the resident gate, and open
the live view. It answers the one question onboarding always stumbles on
("where does the envelope come from?") with a sane, tight default you then
review, rather than a blank file or a dangerous blanket-allow. `--dry-run`
shows the same four steps and changes nothing.

## Walk through what it did

**1. It's already shadow mode.** Shadow mode watches every tool call and
records what it *would* have denied — but never blocks. It is observation,
not protection, and that is exactly what you want first. The hook is now in
`.claude/settings.json`, so launching `claude` normally in this directory
*should* be gated — no `--settings` flag to remember. **Confirm it, don't
assume it:** this file-based delivery is a different mechanism from the one
`tests/test_hook_gate_contract.py` contract-tests (an inline `--settings`
flag), and Claude Code may ask you to trust the folder the first time it
sees a project-level `.claude/settings.json`. Use your session normally for
a bit, then check the next step before you trust the hook is live.

**2. See what it would have denied, and tune.**

```bash
daisugi gate report
```

If `calls=0`, the hook did not fire — approve the folder-trust prompt if
Claude Code showed one, then use the session again before re-checking.
Otherwise, each `would_deny` is a call an enforcing gate would have stopped.
Two things show up as **false-positive candidates**: compound shell commands
(`a && b` — the gate denies these wholesale and suggests splitting them) and
host tools it doesn't recognize. Edit the registered envelope (`daisugi
start`'s "envelope" step prints its path) until the would-denies are *only*
the calls you actually want stopped. This is the whole point of shadow mode:
tune against your real session before trusting it.

**3. Flip to enforce.** Now an out-of-envelope call is denied before it runs,
with the verifier's reason handed back to the model. A hook, once installed,
isn't silently rewritten to a different mode — that could just as easily
downgrade enforce to shadow as the reverse — so remove the gate hook line from
`.claude/settings.json` and run `daisugi start --enforce` again:

```bash
daisugi start --enforce
```

**4. The exit.** If enforce ever over-denies and blocks something you needed,
one command turns the gate off — and it deliberately does not itself require an
allowed tool call, so a bricked gate can't trap you:

```bash
daisugi gate disarm     # ... and `daisugi gate arm` to resume
```

## What you did and did not just get

You got: every executed tool call proven inside your envelope before it ran,
fail-closed — once step 2 confirmed the hook is actually firing. The
verification proof itself (a real denial, contract-tested) is on the
inline-`--settings` delivery path, not this project-file one — see
[Enforcement class, stated up front](gate.md#enforcement-class-stated-up-front)
for the exact distinction. A real denial: [`examples/injection-denied/`](../../examples/injection-denied/).

You did **not** get a guarantee that the *trajectory* is benign — a call being
inside the envelope doesn't make the whole run safe (individually-allowed calls
can still compose into harm), and the gate enforces safety properties only.
That boundary is stated precisely in the [yellow paper §8](../spec/yellow-paper.md).
Tighten the envelope (drop the network grant, narrow the workspace) to narrow
what "inside" means.

## Harden it (optional)

Pin the envelope so a payload can't select a different one:

```bash
claude --settings "$(daisugi gate settings --enforce --session my-session)"
```

Run the same adversarial corpus that gates this project's own merges against
your understanding of the gate:

```bash
daisugi gate audit
```
