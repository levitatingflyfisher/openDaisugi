# ADR-0010 — Compound-shell decomposition with one bash grammar

**Status:** Accepted (prototype). Ships behind the opt-in `shell_allow_decomposition`
envelope field and the optional `opendaisugi[shell]` extra.

## Context

The verifier's shell gate rejects *any* command containing a metacharacter
(`;` `|` `&` `` ` `` `<` `>` `$(` newline) — see the INVARIANT in
`verify.py:_check_shell_command`. The reason is sound: a single allowlisted head
can smuggle more work past a head-classifier (`git status; rm -rf /`), so the
gate refuses to reason about compound shell and demands one command per step.

Measured against a real 80-transcript onboarding run, this is also the single
biggest reason real agent work fails verification: **1005 of ~3766 violations**
were "dangerous metacharacters", because real Claude Code Bash is saturated with
pipes, `&&`, newlines, and redirects. The unconditional rejection means importing
those transcripts distils almost nothing.

We want to replace the blanket rejection with the sound alternative: parse the
command with a *real* bash grammar, and — only if it is a safe composition of
plain simple-commands — verify **each** head against the allowlist. This requires
a shell parser. A dedicated `/deep-research` pass (92 agents, adversarially
verified against primary sources) evaluated the FLOSS landscape.

## Decision

**Adopt exactly ONE parser: `tree-sitter-bash` via `py-tree-sitter`**, behind the
optional `opendaisugi[shell]` extra, used with **mandatory ERROR/MISSING-node
rejection** (tree-sitter is error-*tolerant*, not fail-closed, so the adopter must
reject on any error node — this was the single most decision-relevant fact).

Decomposition is **opt-in per envelope** (`Permission.shell_allow_decomposition`,
default `False`) so a plan's verdict stays a pure function of `(plan, envelope)`.
With the field `True` but the parser absent, the step is **rejected**
(fail-closed on a missing capability) — never silently blanket-rejected as if the
field were `False`. Implementation: `shell_decompose.decompose_command`.

A command is decomposed **only if** it is a composition of plain simple-commands
joined by safe operators (pipes, `&&`/`||`/`;`, newlines). It is **rejected**
(fail-closed) on any of:

- a parse error or missing token (`root.has_error` / `is_missing`);
- command substitution `$(…)`/`` `…` `` or process substitution `<(…)` — either
  runs an unbounded second command;
- **any redirection** — `echo x > /etc/cron.d/pwn` has a harmless head but writes
  a file the `file_write` scope never authorized;
- a **non-literal head** (`$CMD`, `${x:-rm}`, quoted/concatenated) — checked by
  node type (the head must be a single `word` child), not by regex;
- a **command-taking wrapper** (`eval`, `sh -c`, `xargs`, `env`, …) — a short
  explicit denylist, and the weakest link; the non-literal-head and redirection
  rejections are the load-bearing ones.

When decomposition succeeds, each simple command is re-verified through
`_check_shell_command` itself (guarded against re-decomposition), so every head
still faces the full allowlist **and** the interpreter policy — decomposition
does **not** weaken strict opaque-interpreter rejection (`grep x | sed …` under
`strict` still rejects `sed`).

### Rejected alternatives (the "should several coexist?" answer: no)

| Candidate | License | Why not |
|---|---|---|
| **bashlex** | GPLv3+ | Hard blocker for in-process import under MIT. The subprocess-isolation loophole doesn't apply to a pip-imported library. |
| **Parable** | MIT ✓ | Best pure-Python fit (single file, zero-dep), but v0.1.0 self-reports fuzzer-found divergences from GNU bash; for a fail-closed gate, divergence from the executing shell *is* the failure mode. |
| **mvdan/sh** | BSD-3 ✓ | Most battle-tested (Bash 5.2, fuzzed, `shfmt`), but Go-only → subprocess/cgo/RPC bridge; breaks pure-pip install and adds its own surface. |
| **shlex (stdlib)** | ✓ | A lexer, not a parser — no command tree, not fail-closed on dangerous syntax. Tokenizer floor only. |
| **libbash / libdash** | GPL-3.0 | License blocker. |

**We do NOT build an accept-biased fast-primary/heavy-fallback chain.** Letting a
second parser rescue what the first rejected gives malformed input a second
chance to be accepted — the wrong direction for a fail-closed gate. If
defense-in-depth is later wanted, the sound design is *conjunctive*: require two
parsers to both parse AND agree on the extracted head set, else reject. Out of
scope for this prototype.

## Consequences

**Measured effect** (read-only re-verify of the 283 failed onboarding traces with
the field flipped on, one variable changed):

- Of ~1660 metacharacter-bearing commands, **~708 (43%) decompose safely** into
  allowlist-checkable heads; metachar *violations* dropped 1005 → 844. (The two
  numbers differ because the 708 is per-command decomposability, whereas the
  violation delta is net of `verify`'s short-circuiting and of metachar
  violations being *replaced* by allowlist violations on the same step.)
- The remainder is **correctly** rejected: **707 redirections**, 186
  malformed/multiline, 48 substitution, ~10 wrapper/non-literal.
- **0 traces flipped to fully-`ok`.** Every trace still fails on *other* axes —
  75 head-not-in-allowlist, 60 network-host, 27 file-scope. **Decomposition is
  necessary but not sufficient:** the dominant blocker is that the envelope is
  generated from the task label alone (step-blind, prescriptive) and under-permits
  the commands/hosts/paths the trace actually used.

**Follow-ups this surfaces (not in this ADR):**

1. **Redirections are the #1 remaining shell blocker.** The prototype blanket-
   rejects them; the higher-fidelity move is to check a redirect's *target* against
   the `file_write` scope rather than reject outright.
2. **Descriptive envelopes for retrospective import.** The 0-flip result is
   empirical evidence that fixing the metachar ceiling alone can't rescue
   onboarding; the envelope must be fit to observed steps. Separate design fork.

**Open questions inherited from the research:**

- **Dialect match.** Decompose-and-verify is only sound if the parser's shell
  dialect matches the shell opendaisugi actually invokes; a bash-dialect parse of
  a command run under `dash`/POSIX `sh` can misrepresent execution.
- **Parable divergence rate** on our real corpus — would decide whether a
  pure-Python primary is ever viable.

Relates to [ADR-0001](0001-fail-closed-default.md) (fail-closed default) and
[ADR-0003](0003-envelope-as-contract.md) (the envelope is the contract).
