# Limitations

An honest list of what opendaisugi does not do, cannot do today, or does
imperfectly. Read this before adopting. The goal is to save you an
afternoon of discovery.

## What opendaisugi is not

- **Not a sandbox.** `Supervisor` enforces permissions (shell allowlist,
  file glob bounds, network host bounds) at execution time, but it is a
  Python-level warden, not an OS-level container, seccomp profile, or
  hypervisor. If your threat model includes arbitrary native code
  escaping the Python process, opendaisugi does not help.
- **Not a hallucination detector.** If an LLM writes a plan that reads
  a file and then describes the file contents in natural-language
  output, opendaisugi cannot verify the description is faithful. It
  verifies the *plan*, not the *narration*.
- **Not a safety guarantee over LLM reasoning.** An LLM can still emit
  a plan that technically satisfies the envelope but accomplishes a
  goal you didn't want. The envelope is only as tight as the
  invariants you (or the envelope generator) declared.
- **Not a runtime policy engine for code you don't author.** opendaisugi
  verifies plans authored in its `ActionPlan` format. It will not
  inspect arbitrary Python code or shell scripts. You pass in structured
  steps; it reasons about structured steps.

## A wrong envelope that is consistent

The warden proves a plan stays inside its envelope. It does not prove the envelope fits
the task. An envelope that is too loose for the task, but has no contradiction and stays
inside the static base limits, passes. Tightening only caps the damage at those limits. It
does not find the mistake. What helps: a conservative base envelope, a second check of a
generated envelope by a rule library or another model, human approval for task classes the
system has not seen, and denial records that feed tighter envelopes. None of these is a proof.

## No temporal properties

The envelope is checked per step, and Z3 checks each constraint at one point in time. A rule such as "never above this speed
while within one metre of a person" spans time, and the envelope cannot state it. Signal
Temporal Logic is the natural next step for robot plans. It is not built.

## Predicate algebra boundaries

The predicate algebra (see [concepts.md](concepts.md)) can express a lot,
but not everything:

- **No recursion or fixpoint.** You cannot express "for all states
  reachable by executing this plan…" — only "for all steps in the
  written plan."
- **No unbounded quantification.** `ForallSteps` unrolls over the
  plan's concrete steps. If your plan has 5 steps, the quantifier
  becomes a conjunction of 5 clauses. Reasoning about "any plan the
  LLM could write" is handled by subsumption, not by unbounded ∀.
- **No arithmetic over symbolic strings.** You can compare strings by
  equality, regex, prefix/suffix (via allowlist semantics). You cannot
  write "length(command) < 1000" in the algebra today.
- **No cross-step dependencies beyond quantification.** "Step 3 only
  runs if step 2 wrote to file X" is not expressible as an invariant.
  Use the Supervisor's runtime hooks if you need this.

## Regex translator limits

`src/opendaisugi/regex_to_z3.py` covers a subset of Python's `re`. What
works:

- Literals, character classes, ranges (`[a-z]`)
- Alternation (`a|b`)
- Quantifiers (`*`, `+`, `?`, `{n,m}`)
- Groups (capturing and non-capturing)
- Category escapes (`\d`, `\w`, `\s`)
- Anchors `^` and `$` (full-match roundtrip)

What raises `UnsupportedRegexError`:

- Lookaround (`(?=...)`, `(?<=...)`, `(?!...)`)
- Backreferences (`\1`, `\k<name>`)
- Inline flags (`(?i)`, `(?m)`, etc.)
- Word boundaries (`\b`, `\B`)

When a regex fails to translate, the caller may choose to treat that
invariant as a soft node (see below) — but static verification cannot
prove anything about that step symbolically.

## Soft nodes

Predicates that can't be compiled (LLMCheck, unsupported regex fallbacks)
become free Z3 `Bool` variables. That has three consequences:

1. **Static proofs lose that clause.** The solver treats the soft node
   as unconstrained. If the plan's safety depends on the soft node
   being true, Stage 1 cannot prove it.
2. **Stage 2 must run.** The soft node is discharged concretely when
   the step actually runs. If your pipeline skips Stage 2, a soft
   constraint is effectively unchecked.
3. **Subsumption is optimistic for soft nodes.** In
   `envelope_subsumes`, soft-node Bools are assumed true — the most
   permissive interpretation. This is the right stance for proving
   "outer ⊨ inner" (we want to find counterexamples that don't depend
   on soft behavior), but it means soft nodes cannot block a
   delegation that would otherwise succeed.

`SubsumptionResult.unverified_invariants` surfaces any invariants
without an `expr` field. `CompiledPredicate.soft_nodes` surfaces
runtime-resolvable ones. Neither is silent.

As of v0.27.0, tautological predicates (always-true, constrains nothing) and
contradictory predicates (always-false, can never pass) are caught by Z3
vacuity detection at alias-registration time and at invariant-evaluation time.
A tautology becomes a Violation under strict mode; a contradiction is a hard
error at all stakes.

## Shell interpreter escape (v0.13.0 surface, v0.14.0 recursion)

The predicate algebra reasons about the command *string*, not about
what an interpreter does with its arguments. v0.13.0 surfaced the
risk; v0.14.0 closes it for tractable interpreters at Stage 1 verify.

**What v0.14.0 added.** ``verify()`` now parses shell steps with
``opendaisugi.interpreter_parse.parse_interpreter`` and recursively
verifies embedded commands against the same allowlist:

- ``sh -c "rm -rf /home"`` → extracts ``rm -rf /home`` → ``rm`` is
  checked against the allowlist (fails if not present).
- ``xargs rm``, ``find -exec rm {} +``, ``env PATH=/bin rm`` —
  equivalent: inner command verified at depth 1.
- Nested: ``bash -c "sh -c 'rm'"`` verified at depth 2.
- Recursion depth capped at 4 to bound pathological nesting.

**What remains under policy.** Opaque interpreters (``python``,
``perl``, ``ruby``, ``node``, ``awk``, ``sed``, ``make``) interpret a
different language than shell — their payloads cannot be parsed as
shell commands. ``Envelope.shell_interpreter_policy`` governs how
verify handles them:

- **`"surface"` (default).** Opaque interpreters pass verify; at
  subsumption time they are flagged in
  ``SubsumptionResult.unverified_invariants`` as
  ``shell_interpreter:<name>``.
- **`"strict"`.** Verify rejects any opaque interpreter invocation
  (we can't prove anything about its payload). Subsumption rejects
  when the inner allowlist admits any interpreter, tractable or not,
  with ``outer_violation == "shell_interpreter_policy"``.
- **`"allow"`.** Opaque interpreters pass both verify and
  subsumption without surfacing — the user has considered them and
  accepts residual risk.

**Subsumption still uses the surface model.** Recursion happens at
verify time against concrete plans. Subsumption (envelope-vs-envelope)
can't recurse because the command strings are symbolic — Z3 can't
parse an unknown string as a shell command. Subsumption continues to
flag interpreter presence in either allowlist per the v0.13 policy.

The recognised interpreter list is ``opendaisugi.models.SHELL_INTERPRETERS``
(sh/bash/zsh/xargs/find/env/python/perl/ruby/node/make/awk/sed/…).
Tractable interpreters (shell ``-c`` / ``xargs`` / ``find -exec`` /
``env``) are parsed. Opaque ones fall through to policy. Unlisted
binaries do not trigger the policy; extend
``src/opendaisugi/interpreter_parse.py`` downstream if you discover
more.

## What the warden does not place

The warden places the files a shell command reads and writes through its operands and
redirects, for the commands in two closed tables (`write_paths.READERS` and `WRITERS`).
It does not place:

- **An interpreter's work.** `python`, `awk`, `make`, `node` and `perl` read and write
  whatever their script says. `sed` reads its file operands, and that is placed. A sed
  script with a `w`, `W`, `e`, `r` or `R` command, an `s` flag `w` or `e`, or a `-f` script
  file makes the reads and writes of the line unknown, so enforce denies it unless the
  scope is everything.
- **A head that runs a command: allowlisting it allows any command it runs.** These
  heads run a command, a file of commands or a script the warden cannot read. A line
  that gives them an argument has unknown reads and writes, so enforce denies it unless
  the scope is everything. The allowlist and the file scopes never see the inner command.
  If you put one of these heads in `shell_allowlist`, you allow whatever it runs.
  - Always a runner when given an argument: `exec`, `eval`, `builtin`, `source`, `.`,
    `chroot`, `flock`, `busybox`, `script`, `parallel`, `coproc`, `trap`, `sudo`, `doas`,
    `watch`, `systemd-run`, `screen`, `unbuffer`, `runuser`, `taskset`, `chrt`, `prlimit`,
    `unshare`, `nsenter`, `entr`. `batch` and `expect` read their commands from standard
    input, so they are runners with no argument too. The ones named in
    `SHELL_INTERPRETERS` are opaque interpreters, so the `strict` policy rejects them.
  - A runner only in some forms: `git` with `-c`, `--config-env` or `--exec-path` before the
    subcommand (`-C` is not one); `ssh` with a word after the host, `-o` with a Command,
    Match, Include or Exec option, `-F` or `-E`; `su` with `-c`, `--command` or
    `--session-command`; `tmux` with a command that runs commands (`new-session`,
    `new-window`, `split-window`, `send-keys`, `run-shell`, `respawn-pane`, `pipe-pane`,
    `if-shell`, `display-popup`, `source-file`, and the others in `_TMUX_RUNS`, by name,
    alias or unique prefix) or `-c` or `-f`; `vim`, `nvim`, `vi`, `view`, `vimdiff` and
    `ex` with any option or `+cmd`; `crontab` and `at` with anything but `-l`.
  - Unwrapped exactly, so the inner command is checked: `env`, `timeout`, `nice`, `nohup`,
    `time`, `stdbuf`, `command`, `setsid`, `ionice`, `xargs`, `find -exec`, `sh -c`.
  - Not covered: a git alias or hook set in a `.git/config` the agent wrote earlier,
    `git filter-branch`, `git rebase --exec`, `git submodule foreach`, `rsync -e`,
    `scp -o ProxyCommand` and `sftp`; `make`, `npm run` and the like fall under "an
    interpreter's work". Allowlisting these heads allows what they run.
- **A path-qualified head.** The interpreter and runner lists use the program's name for an
  absolute path (`/usr/bin/env` is `env`). A relative path (`./timeout`) is the project's own file.
- **A long option it cannot match.** `env`, `xargs` and the transparent wrappers
  (`timeout`, `nice`, `nohup`, `time`, `stdbuf`, `command`, `setsid`, `ionice`) read
  long options as GNU getopt_long does: an exact name, else a unique prefix. An ambiguous
  or unknown long option makes the payload opaque, and the line is unknown.
- **The agent's `CDPATH`.** The warden cannot see the agent's environment. A relative
  `cd` is placed as if `CDPATH` is empty; with `CDPATH` set, bash may enter another
  directory.
- **A command in neither table.** `git`, `npm`, `curl`, `pip` and the like. The allowlist
  decides whether they run; no file scope sees their operands. A head that can run a
  command is listed above.
- **The file arguments of MCP tools.** The MCP-policy plan owns them.
- **Symlinks and bind mounts.** Every scope is a path string.
- **A persistent shell whose cwd is not the payload's cwd.** The warden trusts `cwd` for
  `Bash` as it does for `Write`. A harness that keeps one shell across calls can `cd` in
  one call and write a relative path in the next.
- **A pipeline's operands.** `find ... | xargs cat` is denied because `xargs` adds
  operands the line does not show. The warden does not follow the pipe from `find` to
  `xargs`.
- **A heredoc body as data.**
- **`uniq IN OUT` and `xxd IN OUT`.** The second operand is a write, and the table places it as one.

A path the warden cannot place (an unquoted glob, `$VAR`, a flag outside the command's
closed set) is a deny unless the scope is exactly `**` or `/**`.

## Files are confined by the fence, not the warden

The list above is the reason. The parse of a shell line is frozen, and it cannot
be complete. The file boundary is the kernel fence (`docs/how-to/fence.md`).

- **Inside a fence**, an access the warden cannot place, or places out of scope,
  is allowed and recorded as `fence_enforced`. The kernel stops what the fence
  does not grant. The warden still decides everything else about the call.
- **Without a fence**, enforce mode denies those accesses, as the list above says.
  A command in neither table, an interpreter's script and a program a head starts
  can still reach any file the user can. The warden checks commands. It does not
  confine files. The status commands and the coppice floor say so in one line.
- **Under coppice**, `fence = "auto"` is the default. A kernel without Landlock ABI 9
  runs agent work bare, and the floor shows the line with the reason. `fence = "on"` refuses to
  start without a fence. `fence = "off"` is the operator's choice.
- **A fence record can be forged only by a process that is not fenced.** Such a
  process can also write `DISARMED`. The fence is the boundary against an agent
  that is fenced, not against one that already runs free.
- **MCP rules take absolute paths.** A relative value in a `path_in` rule is
  refused, and `warden register` refuses a `path_in` over a `file_read` or
  `file_write` glob that is relative.


## Z3 practical limits

- **Decidability.** Z3 is complete for linear integer arithmetic and
  most string theories but not all combinations. A particular envelope
  + plan may return `unknown`. We raise `VerificationTimeout` when
  that happens; we do not approve.
- **String alphabet (v0.28.4+).** The regex-to-Z3 translator uses the
  Basic Multilingual Plane (`0x00-0xFFFF`) minus newline as the
  alphabet for `.` and negated character classes. This matches
  Python's `re` over BMP. Supplementary-plane codepoints
  (`0x10000-0x10FFFF` — emoji, ancient scripts, CJK extension B+) are
  outside Z3's string sort and silently disappear from membership
  checks. If a regex authored in the envelope or used at runtime
  contains supplementary-plane characters, results may diverge from
  Python's. Realistic envelope-authoring corpora are BMP-only; the gap
  is documented rather than fixed because Z3 itself rejects the wider
  range.
- **Timeouts.** Default Z3 timeout is 500ms for `verify`, 2000ms for
  `envelope_subsumes`. Configurable. Complex plans with many steps
  and dense invariants can hit the timeout; increase as needed.
- **String-theory performance.** Regex over long symbolic strings is
  the most expensive operation. If you author invariants with many
  disjunctive regex clauses, subsumption can slow noticeably.

## Feature maturity caveats

See [feature-status.md](feature-status.md) for the full matrix. Summary:

- **Core (production-candidate):** Envelope, predicate algebra,
  `verify`, `Contract`, `verify_delegation`, `Supervisor`, `Journal`.
- **Post-adoption depth (functional, lightly used):** Distiller,
  PathwayStore, Gardener (A/B/prune/merge), Tier-1 routing, MCP
  server, LoRA training-data pipeline.
- **Experimental:** MuJoCo-backed robotics executor, pathway
  export/import portability.
- **Planned, not shipped:** arithmetic-over-paths operator in the algebra.

## Platform constraints

- **Python 3.12+.** We use match statements and newer typing features
  throughout.
- **Z3 solver.** Required. `uv add opendaisugi` pulls `z3-solver`
  automatically but the solver is a native dependency.
- **LLM API for envelope generation.** `generate_envelope` calls out
  through our own model client (the `api` backend). Set `ANTHROPIC_API_KEY` (or equivalent),
  use a low-stakes permissive envelope if you don't want network
  calls, or set `OPENDAISUGI_LLM_BACKEND=claude-code` (v0.12.0+) to
  route through an existing Claude Code install instead of an API key.

- **The data home follows the environment.** `OPENDAISUGI_HOME` and
  `XDG_DATA_HOME` move it, as `HOME` does. An installed warden hook bakes its
  `--root`, so an agent's tool-call environment cannot move an installed
  warden. A harness an agent starts itself with `OPENDAISUGI_HOME=...` is the
  same case as one it starts with `HOME=...`: out of scope for the warden.
- **A POSIX `sh` at `/bin/sh`.** The executors run a shell step with
  `/bin/sh -c` (Go and Rust) or `subprocess` with `shell=True` (Python,
  which also uses `/bin/sh`). Every Linux, macOS and BSD target has it. A
  system with no `/bin/sh` (NixOS without the compatibility link, Termux)
  cannot run shell steps.
- **The default envelope writes under `/tmp/**`.** The library's
  low-stakes default (`defaults.py`, and the Go and Rust envelope
  generators) allows writes to `/tmp/**` and `./out/**`. On Linux that is
  the system temp directory. On macOS and the BSDs the per-user temp
  directory (`$TMPDIR`) is elsewhere, so a write there falls outside the
  default envelope and is refused. Pass an envelope that names it. The
  default is kept as it is, since a change would change what the warden
  allows.

## ClaudeCode LLM backend (v0.12.0+)

- Each call spawns a fresh `claude -p` subprocess (~0.5-1 s overhead vs
  direct API). Acceptable for envelope generation and distillation;
  noticeable for LLMCheck in tight loops. No batching.
- The subprocess inherits the parent environment, including
  `ANTHROPIC_API_KEY` if set. Unset it beforehand for strict API-key
  isolation.
- Schema-augmented prompts for structured output can be large (the
  `Envelope` schema is ~2 KB). Claude honors "respond with ONLY JSON"
  reliably in practice; pathological outputs surface as
  `EnvelopeGenerationError`.
- **Optional extras** are heavy: `torch`/`peft`/`bitsandbytes` for
  `[lora]`, `mujoco` for `[robotics]`, `sentence-transformers` for
  `[search]`. Install only what you use.

## MCP argument rules

`mcp_rules` (ADR-0027) decide one call from its own arguments. They do not do these:

- **Rules over tool results.** What a search returned is not checked.
- **Live schemas.** `warden schemas import` stores a `tools/list` result by hand. The warden
  never asks a server.
- **MCP tools reached through sprig's `Spawn`.**
- **Cross-call claims.** "At most three posts per session" needs history. A rule holds for one
  call.
- **Symlinks.** `path_in` matches a path as text and does not resolve symlinks, the same as the
  file tools. A link inside an allowed tree can point outside it.
- **A path the argument does not spell as a path.** A string that names a file only after the
  server resolves it (a name, an id) is not a read to the warden. Use `mcp_rules` with
  `in_set` or an anchored `matches`.

## Codex tools

The warden reads Codex's `apply_patch` (patch text in `tool_input.command`) as one file write
per path, the same as OpenCode's. Codex's `update_plan` and Claude Code's `TodoWrite` change
nothing on disk, but the warden does not know them. It denies them in enforce mode and counts a
would-deny in audit mode. This is a known false positive, not an allow. Admitting named
no-effect tools is a separate choice (PK-7).

## Planned, not shipped

These are named in specs and tracked in CHANGELOG but do not work today:

- **Arithmetic-over-paths** — e.g. `sum(step.tokens) < budget`. The
  v0.15 `length_range` operator covers the common case (string/list
  length bounds); arbitrary numeric arithmetic across paths is still
  future work.
- **Real-time plan refinement loops beyond basic CEGAR** — v0.2.0
  shipped a one-shot refinement; multi-round is future work.

If any of the above is a hard requirement, opendaisugi is not ready
for you yet.
