# Let `a && b` through, soundly

The verifier rejects any shell command carrying a metacharacter — `;` `|` `&` `` ` `` `<`
`>` `$(` or a newline. That blanket rule is why a captured `cd /repo && pytest -q` shows up
in your journal as *rejected*, and why an onboarding run over months of real transcripts
converts far fewer episodes than you expected. In one measured corpus of 13,232 captured
shell calls, **96.2% carried a metacharacter.**

[ADR-0010](../adr/0010-compound-shell-decomposition.md) replaces the blanket rule with a
real bash grammar, behind an opt-in, and
[ADR-0014](../adr/0014-envelope-checkable-shell-effects.md) extends the grammar's reach
to redirections, substitutions, and wrappers. This page is how to turn it on and what it
will and will not buy you.

## Turn it on

```bash
uv add 'opendaisugi[shell]'          # the tree-sitter bash grammar
daisugi install --allow-shell-decomposition   # persist the default
daisugi status                        # confirm it is on AND usable
```

`install` writes `shell_allow_decomposition: true` to `~/.opendaisugi/config.yaml`. That
file is the default for every command that produces an envelope. Any single run overrides
it in either direction:

```bash
daisugi onboard --allow-shell-decomposition       # this run, on
daisugi hook to-trace sess --no-allow-shell-decomposition   # this run, off
```

The flag is available on `onboard`, `journal ingest`, `hook to-trace`, `hook auto-tend`,
`generate-envelope`, `gate init`, and `gate quickstart`.

Persisting it is not a convenience. `daisugi hook auto-tend` is how captures become traces
in the background — it runs from cron and from a detached spawn, with no argv to carry a
flag and its output on `/dev/null`. Config is the only channel that reaches it.

## What it actually admits

Every simple command inside the compound is checked against the allowlist, exactly as if
you had written them as separate steps. `cd /repo && pytest -q | tail -5` needs `cd`,
`pytest`, **and** `tail` on the allowlist — all three, or the command is denied.

Since ADR-0014 the grammar also proves *effects*, not just structure:

| construct | how it is checked |
|---|---|
| `sort x > out.txt`, `wc < in.txt` | the literal target is checked against the envelope's `file_write` / `file_read` scopes — `> /etc/passwd` is rejected by *scope* unless your envelope authorizes it |
| `> /dev/null`, `2>&1`, `2>&-` | sanctioned pathless sinks and fd operations — always pass |
| `echo $(git rev-parse HEAD)`, backticks, `<(sort a)` | the substitution body is recursively decomposed; `git` faces the allowlist like any head |
| `timeout 30 git fetch`, `nice -n10 make`, `xargs rm`, `sh -c '…'` | the wrapped command is extracted and recursively verified — the wrapper *and* its payload's head both face the allowlist |
| `grep "a\|b" f` | quoted metacharacters are data; the grammar has proved it, so the part is not re-rejected |

These still fail closed, because the touched path or program genuinely is not known
until runtime:

| rejected | why |
|---|---|
| `echo hi > $OUT`, `> out$N.txt`, `> $(mktemp)` | non-literal redirect target |
| `$CMD --flag`, `${x:-rm} -rf /` | non-literal head |
| `sudo x`, `doas x`, `watch x` | opaque wrappers — strict policy rejects |
| malformed shell | a parse error is never treated as permission |

Measured over the 250 largest real transcripts (13,307 captured calls, 96.1% carrying a
metacharacter): **95.9% of metacharacter-bearing commands now decompose** (29.4% before
ADR-0014). The refusals that remain are 268 non-literal heads, 213 non-literal redirect
targets, and 49 parse errors — nothing else.

## Fail-closed, including when it is missing

With the field on and the grammar *not* installed, every compound command is denied — the
verifier never treats a missing capability as permission. That is the one state worth
watching for, because it looks exactly like the problem you turned the opt-in on to solve.
`daisugi status` names it, and `gate init`, `journal ingest`, `generate-envelope` and
`install` warn at the moment they write it.

## What it recovers, honestly

Under ADR-0010 alone the opt-in recovered 48 of 1,229 rejected episodes (22.3% → 25.3%)
— modest, because an episode passes only if *every* step passes and a long session
usually contains at least one redirection. ADR-0014 removed exactly that ceiling.
Re-measured on the same corpus shape (2026-08-20): whole sessions round-tripping through
capture → `infer_envelope` → `verify`, all-or-nothing across up to 400 steps, went from
**0% to 41.2%**, and every still-failing session fails on a genuinely non-literal
construct (`$FL` as a head, `> $LOG`). At the per-command level — the granularity the
distiller actually clusters at — 95.9% of metacharacter-bearing commands decompose.

The recovery lands on the capture path (`hook to-trace`, `hook auto-tend`), where
openDaisugi holds the real commands: inference collects every head the verifier will
check (including wrapped payloads and substitution bodies) *and* the redirect targets,
which land in the inferred `file_read`/`file_write` scopes. On `onboard` and
`journal ingest` the envelope is generated by a model from the task text; the opt-in
composes with a provider that writes wider allowlists or an envelope you reviewed by
hand.

When a command is still rejected, the violation now names its remedy — the exact
allowlist entry or file-scope glob that would authorize it (`suggested_remediation`).
Widening stays a deliberate, auditable act; it is just no longer a research project.
