# ADR-0014 — Envelope-checkable shell effects: redirections, substitutions, wrappers

**Status:** Accepted. Ships inside the existing `shell_allow_decomposition` opt-in
(ADR-0010) and the `opendaisugi[shell]` extra — no new flag, because this is not a new
permission: it is the same opt-in finally seeing what it previously refused to look at.

## Context

ADR-0010's decomposer proved *structure*: a compound command became a list of literal
heads, each checked against the allowlist. Everything else failed closed — every
redirection, every substitution, every command-taking wrapper, unconditionally. Measured
over the 250 largest real transcripts, 96.2% of captured shell calls carry a
metacharacter and only 29.4% of those decomposed; redirection alone accounted for ~87%
of the refusals (7,743 of ~8,963). An episode verifies only if every call in it does, so
onboarding over real agent work starved: one `> file` anywhere sank the trace, and no
envelope — however wide — could authorize `sort x > out.txt`, because the rejection
never consulted the envelope at all.

That last sentence names the defect. Fail-closed means "reject what the envelope does
not authorize." The blanket rules had drifted into "reject what the checker cannot
model" — a capability gap in the verifier presented as a safety policy, and one that
capped the distiller (the do-nothing gradual-automation loop, the system's biggest
token-saving lever) at a structural ceiling.

## Decision

Extend the decomposer and verifier so that the three big refusal categories become
*checkable effects* instead of unshapeable shapes. Fail-closed stays; what changes is
how much the grammar can prove.

1. **Redirections are file accesses spelled in shell.** A literal redirect target is
   returned as a `read` or `write` and checked against the envelope's own
   `file_read`/`file_write` scopes — exactly the scopes a `FileReadStep`/`FileWriteStep`
   faces. `echo x > /etc/cron.d/pwn` is still rejected, but by *scope*, not shape — and
   `sort x > out/sorted.txt` under `file_write: [out/**]` is authorized, because the
   envelope says so. FD duplications (`2>&1`) and closes (`2>&-`) touch no path and
   pass. `/dev/null`, `/dev/stdout`, `/dev/stderr` (writes) and `/dev/null`,
   `/dev/stdin` (reads) are sanctioned pathless endpoints — rejecting `> /dev/null`
   would fail half of real shell for zero safety. Heredocs and herestrings feed stdin
   *data* and pass as redirects, with their bodies still walked (below).

2. **Substitution bodies are recursively decomposed.** `$(...)`, backticks, and
   `<(...)` contain parseable commands, and we hold a parser: their inner commands
   surface in `heads`/`commands` like any others, so "everything that executes has a
   checked head" now holds *through* nesting — including a substitution inside an
   unquoted heredoc or herestring. `echo $(rm -rf /)` no longer rejects on shape; it
   rejects because `rm` is not on the allowlist.

3. **Wrappers are the interpreter layer's job, done properly.** The decomposer's
   wrapper denylist is gone. `timeout`, `nice`, `nohup`, `time`, `stdbuf`, `command`,
   `setsid`, `ionice` are now *transparent wrappers* in `interpreter_parse`: their flags
   (and `timeout`'s duration) are skipped and the wrapped command is recursively
   verified, exactly like `xargs`/`env`. This also closes a pre-existing hole the
   denylist had been papering over for compound commands only: none of these were in
   `SHELL_INTERPRETERS`, so a *standalone* `timeout 30 <anything>` ran `<anything>`
   completely unverified once `timeout` was allowlisted. `sudo`/`doas`/`watch` are
   classified opaque instead — privilege changes and re-execution loops are not worth a
   subtly-wrong transparent parse; strict policy rejects them, the fail-closed direction.

4. **Decomposed parts skip the raw metachar regex.** The grammar has proved each part a
   single simple command, so parts are verified through a dedicated simple-command path
   (head allowlist → interpreter recursion). Re-running the character-class regex on
   them rejected `grep "a|b"` — data mistaken for an operator the grammar had already
   ruled out. The regex remains the gate for every non-decomposed command.

5. **Inference collects what the verifier checks.** `infer_envelope`'s head collection
   became effect collection: simple-command heads, wrapped-payload heads, substitution
   inner heads, and redirect targets (as parent-dir globs in the file scopes). Anything
   the verifier will check that inference fails to collect makes the inferred envelope
   reject the very trace it was inferred from — the bulk-onboarding failure this ADR
   exists to end.

6. **Rejections carry their remedy.** A head-allowlist or redirect-scope violation
   names the minimal envelope amendment that would authorize it
   (`Add 'rm' to permissions.shell_allowlist`; `Add 'out.txt' … to
   permissions.file_write`) in `suggested_remediation`. Safety stays fail-closed; the
   cost of being fail-closed drops to one deliberate, auditable widening.

## What still fails closed — the honest core

A parse error or missing token; a non-literal head (`$CMD`, `${x:-rm}`); a non-literal
redirect target (`> $OUT`, `> out$N.txt`, `> $(mktemp)`, `> "$DIR/x"`); an unknown
redirect operator; opaque interpreters under strict policy. These are the cases where
the touched path or the executed program genuinely is not known until runtime — no
sound verifier can wave them through, and the corpus shows they are now the *whole* of
the refusals.

Scope boundary, stated plainly: this ADR checks **shell-level** effects — what the
shell itself does before any program runs. Program-level file access (`rm x`,
`tee out.txt`, `dd of=…`) remains governed by the head allowlist, exactly as it always
was for simple commands. The allowlist decides which programs may run; redirects are
checked because the *shell* performs them.

## Measured effect (2026-08-20, 250 largest real transcripts, 13,307 captured calls)

- 96.1% of captured shell calls carry a metacharacter (unchanged corpus shape).
- Of those, **95.9% now decompose** (was 29.4%). Remaining refusals: 268 non-literal
  heads, 213 non-literal redirect targets, 49 parse errors — nothing else.
- Whole-session round trips (capture → `infer_envelope` → `verify`, all-or-nothing over
  up to 400 steps per session, 17 shell-bearing sessions): **0% without decomposition,
  41.2% with it**. Every one of the 10 still-failing sessions fails on a genuinely
  non-literal construct (`$FL` as a head, `> $LOG`, `> "$SPIKE/gate.json"`).
- Granularity note: ADR-0010's "48 of 1,229 episodes" used a finer episode split; the
  per-command and per-session numbers above are the comparable before/after for this
  change and are deliberately the harsher metrics.

## Consequences

- Distillation over real shell-heavy traces is unblocked: the dominant refusal
  categories are gone, so clusters form from what agents actually ran.
- The single-command guarantee is *stronger*, not weaker: heads inside substitutions
  and wrapped payloads now face the allowlist where before the whole string was either
  rejected (compound) or partially unverified (standalone `timeout`).
- The wrapper denylist — flagged in ADR-0010 as "the weakest link" — no longer exists;
  wrapper handling is centralized in the interpreter layer with per-wrapper tests.
- `verify` and `infer_envelope` must stay mirror images; `_observed_effects` documents
  this as its contract and the round-trip tests enforce it.
