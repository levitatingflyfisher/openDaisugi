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
daisugi gate quickstart
```

That generates and registers a **starter envelope** for the current directory
— read/write within the project, a conservative shell allowlist, no network —
and prints the whole flow. It answers the one question onboarding always
stumbles on ("where does the envelope come from?") with a sane, tight default
you then review, rather than a blank file or a dangerous blanket-allow.

## Walk through what it printed

**1. Launch in shadow mode.** Shadow mode watches every tool call and records
what it *would* have denied — but never blocks. It is observation, not
protection, and that is exactly what you want first:

```bash
claude --settings '<the shadow settings JSON it printed>'
```

Now use your session normally for a bit.

**2. See what it would have denied, and tune.**

```bash
daisugi gate report
```

Each `would_deny` is a call an enforcing gate would have stopped. Two things
show up as **false-positive candidates**: compound shell commands (`a && b` —
the gate denies these wholesale and suggests splitting them) and host tools it
doesn't recognize. Edit the registered envelope (the report prints its path)
until the would-denies are *only* the calls you actually want stopped. This is
the whole point of shadow mode: tune against your real session before trusting
it.

**3. Flip to enforce.** One flag. Now an out-of-envelope call is denied before
it runs, with the verifier's reason handed back to the model:

```bash
claude --settings "$(daisugi gate settings --enforce)"
```

**4. The exit.** If enforce ever over-denies and blocks something you needed,
one command turns the gate off — and it deliberately does not itself require an
allowed tool call, so a bricked gate can't trap you:

```bash
daisugi gate disarm     # ... and `daisugi gate arm` to resume
```

## What you did and did not just get

You got: every executed tool call proven inside your envelope before it ran,
fail-closed, on the contract-tested Claude Code path. See a real denial in
[`examples/injection-denied/`](../../examples/injection-denied/).

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
