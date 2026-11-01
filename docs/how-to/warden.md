# Warden a live session's tool calls

*How to put the call-time warden (ADR-0007) in front of an agent you are
already running: audit first, tune with the report, then flip one flag to
enforce. And how to turn it off in one command if it over-denies.*

The warden takes each intercepted tool call, synthesizes it into a one-step
plan, and runs the **full `verify()` pipeline** against the session's
registered envelope before the call executes. Deny-by-default: an unknown
tool, unparseable input, an internal exception, or a slow verifier all deny
(in enforce mode). Strictness comes from the envelope's `stakes` — the warden
never relaxes it.

## Enforcement class, stated up front

- **Claude Code: hard enforcement, contract-tested — for the `--settings`-injected
  path.** A committed live test (`tests/test_hook_warden_contract.py`) proves a
  PreToolUse hook passed inline via `claude --settings '<json>'` fires in
  headless runs and that the warden's exit-2 deny actually blocks the call, on
  the pinned CLI version. This includes a test where the *real shipped warden*
  denies a real `Read` in a real `claude -p` run. **`daisugi start` uses a
  different delivery mechanism** — a hook written into the project's own
  `.claude/settings.json`, not passed on the command line — and that path is
  *not* covered by the same contract test. Claude Code's project-level
  settings are also typically subject to a one-time folder-trust approval
  that inline `--settings` is not. Confirm the hook actually fired before
  relying on it: `daisugi warden report` after a session shows calls once it
  has, and audit mode never blocks, so there is nothing to lose by checking
  first.
- **Hermes / OpenClaw: unverified.** The block shapes are emitted
  belt-and-braces (Hermes gets both `decision` and `action` keys), but no
  per-version contract test exists yet — treat these paths as observation
  until roadmap Stage 5 lands. Claiming otherwise would be fail-open
  committed at the documentation level.
- **Fail-open edges outside our control**, on every host: the host's outer
  hook timeout, or a harness that silently stops firing hooks. The warden's
  inner verify timeout denies *before* the host's outer timeout can fail
  open, but it cannot help if the hook is never invoked.
- **A dead warden denies (in enforce mode).** On Claude Code any hook exit that
  is not 2 is non-blocking, so a crashed warden or a broken install would
  otherwise allow silently. The **enforce**-mode command emitted by `warden
  settings --enforce` therefore ends in `|| exit 2`, mapping every
  nonzero-non-2 exit — including the package failing to import, where the
  warden's own error handling never runs — to a deny. **If you hand-write an
  enforce hook command, keep that suffix.** Without it the warden fails open
  exactly when it is most broken; this is live-verified in
  `tests/test_hook_warden_contract.py`. (Audit mode deliberately omits the
  suffix: audit must never block the host, so a crashed audit warden stays
  non-blocking.)

## Just want it working? One command

```bash
daisugi start          # in your project directory
```

This finds the harness, installs the warden hook into this directory's
`.claude/settings.json` (never a machine-global hook), registers a starter
envelope keyed to this directory, starts the resident warden, and opens the
live view — four steps, each reported done / skipped / failed / would;
`--dry-run` changes nothing; `--enforce` arms it. The hand-held version is the
tutorial
[Protect an agent you're already running](../tutorials/protect-your-existing-session.md).
The rest of this page is the reference for each underlying step (`warden init`,
`warden report`, `warden settings`, `warden disarm`) if you want to wire them by
hand instead.

## 1. Register an envelope

The warden checks calls against a per-session envelope; without one, enforce
mode denies everything (fail-closed — the deny message tells you so).

```bash
# Generate a reviewable starter envelope for a directory and register it:
daisugi warden init --workspace .

# Or register one you wrote / generated yourself:
daisugi warden register my-envelope.json

# Bind one session specifically instead of the default:
daisugi warden register my-envelope.json --session <session-id>
```

Envelope files are JSON or YAML `Envelope` documents. `warden init` writes a tight
starter (read/write the workspace, a conservative shell allowlist, no network)
for you to review; you can also generate one from a captured session
(`daisugi hook to-trace` infers one), write one by hand, or start from an
example in `examples/`.

## 2. Audit mode — observe before you trust

```bash
claude --settings "$(daisugi warden settings)"
```

The permanent form is `daisugi install --only warden`: it writes the audit hook alone into
`~/.claude/settings.json`. A plain `daisugi install` also writes the skill, the MCP server, the capture hook and
the pathway note.

Audit mode evaluates every call and logs what enforce *would* have denied,
but always allows. **Audit mode is observation, not protection.**

Audit mode was called shadow mode before 2026-09-28. The old word is gone:
`--mode shadow` and `install --shadow` are refused with one line that names
the new one. A hook installed before the rename still passes `--mode shadow`,
so the warden refuses every call it sees; `daisugi warden status` warns about it.
Run `daisugi install --uninstall`, then `daisugi install`. The log is
in `~/.opendaisugi/warden/audit/`; the report also reads the old
`~/.opendaisugi/warden/shadow/`, so the history is kept.

Review the verdicts:

```bash
daisugi warden report            # or --json for the full records
```

The report lists every would-deny verbatim, and flags the two known
over-denial classes as **false-positive candidates**: compound-command
metachar denials (`a && b` — the verifier offers a decomposition instead)
and host tools the classification map doesn't know (deny-by-default sweeps
those up wholesale). Tune the envelope until the would-denies are the calls
you actually want denied.

### Letting `a && b` through, soundly

Compound commands are the first over-denial most sessions hit. You can admit
them without loosening the allowlist ([ADR-0010](../adr/)):

```bash
uv add 'opendaisugi[shell]'                    # the bash grammar this needs
daisugi warden init --allow-shell-decomposition  # --force to regenerate an existing one
```

The verifier then parses the command with a real bash grammar and checks
**every** head against the allowlist, so `git status && ls` passes while
`git status && rm -rf /` still denies on `rm`. Redirection, `$(…)`, a
non-literal head like `$CMD`, and command-taking wrappers (`xargs`, `sh -c`)
are still rejected outright — the opt-in is "check each command", never
"allow compound shell".

It fails closed on a missing capability: with the field on and the grammar
absent, compound commands are **denied**, not waved through. `warden init` warns
when that is the case. Note that `--force` regenerates the envelope from
scratch, discarding any hand-tuning.

The same opt-in governs the bulk paths that replay history — `onboard`,
`journal ingest`, `hook to-trace`, `hook auto-tend` — with the default persisted
in `config.yaml`. See **[Let `a && b` through](compound-shell.md)** for that
side, including what the opt-in measurably recovers and what it does not.

### What the warden places

The warden reads the files a shell command touches through its operands, not
only its redirects. Each file a command reads faces `file_read`; each file it
writes faces `file_write`. `cp /work/a ~/.ssh/authorized_keys`, `tee /etc/cron.d/x`,
`sed -i s/a/b/ /etc/hosts` and `cat /etc/passwd` all deny when the scope is
`/work/**`.

- **Trees.** A recursive access (`cp -r`, `rm -r`, `tar -x -C DIR`, `grep -r`,
  `find START`, `mv`) covers the directory and everything under it. Only a
  glob that ends in `/**` covers a tree. `/work/out/*` does not, and `/**/*`
  is not everything.
- **The base.** A relative path is placed from the call's cwd. A first command
  `cd DIR` with one literal operand doubles the base (the line is checked from
  the cwd and from DIR, since a failed `cd` stays put). The `cd` counts only when
  the line starts with it. A `cd` inside a loop, a function, a `( )` group or an
  `if` is a mover, because it can run any number of times. Any other command that
  moves the cwd (`pushd`, `env -C`, a second `cd`, …) makes every relative path
  unknown. `exec`, `eval`, `builtin`, `source` and `.` run a command the warden
  cannot read, so a line with one of them (and an argument) is unknown whole.
- **Unknown is a deny.** When the warden cannot place a file (an unquoted glob
  or `$VAR`, a flag outside the command's closed set, `xargs`, a `tar` without
  one mode and one `-f`), the call is denied unless the scope is exactly `**`
  or `/**`. The message names the reason. Name each file instead.
- **`xargs` and `find -exec`.** `find DIR -exec CMD {} +` is placed: `find`
  reads DIR, and `CMD` reads its literal operands. `ls | xargs cat` is not, so
  it denies. Write `find DIR -exec cat {} +`, name the files, or use `grep -r`.

The commands in the tables are `cat`, `head`, `tail`, `grep`, `rg`, `ls`, `find`,
`diff`, `sort`, `wc`, `cp`, `mv`, `install`, `rsync`, `tar`, `dd`, `sed`, `tee`,
`rm`, `touch` and a few more (`write_paths.READERS` and `WRITERS`). The work of an
interpreter (`python`, `awk`, `make`) and a command in neither table (`git`,
`npm`, `curl`) is not placed; see [limitations](../limitations.md).

You can also tune offline against an existing passive capture, without
running an agent at all:

```bash
daisugi warden replay ~/.opendaisugi/captures/<session>.jsonl --envelope my-envelope.json
```

## 3. Enforce — the one-flag flip

```bash
claude --settings "$(daisugi warden settings --enforce)"
```

The permanent form is `daisugi install --enforce`. It refuses when no envelope is registered. When a plain `daisugi install` already wrote the audit hook, `--enforce` changes that hook to enforce in place and says so. A plain install over an enforce hook leaves it in enforce and says so; to go back to audit, run `daisugi install --uninstall`, then `daisugi install`.

**Pinning the envelope (`--session`).** By default the warden selects an
envelope by the `session_id` in each hook payload, falling back to the
default. That is fine when the host is the only writer of `session_id`, but
authorization then keys on a field the caller can influence: a payload
claiming another registered session's id would be checked against *that*
session's (possibly more permissive) envelope. To close that off, pin it:

```bash
claude --settings "$(daisugi warden settings --enforce --session job7)"
```

With `--session` set, the payload's `session_id` is recorded (the report
keeps `payload_session_id` for traceability) but never authorizes. The pin
lives in the hook command, which the agent cannot rewrite — the same
principle as a sub-agent's warden root living outside its workspace.
`AgenticExecutor` pins its sub-agents automatically.

On the Claude Code path a deny is exit code 2 with the proof-backed reason
on stderr — the model sees *why* ("permissions: Step 's0' file_read path
'/etc/passwd' not permitted by file_read ['/allowed/**']") and can restate
its approach inside the envelope.

## 4. The exit: one command, no tool call required

```bash
daisugi warden disarm     # warden allows everything until re-armed
daisugi warden arm        # resume
daisugi warden status     # armed state + registered envelopes
```

`disarm` is deliberately a plain CLI command run from any shell — if an
over-denying warden has an agent locked up, the operator's way out does not
itself pass through the warden.

These verbs are the operator's. The warden denies an agent's shell line that
runs `warden disarm`, `warden arm`, `warden serve`, `install` or `graft
install|remove`, in every mode, before the envelope. Run them in your own
terminal.

## Allow an MCP tool only with certain arguments

`mcp_allowlist` names a tool. `mcp_rules` names a tool and what its arguments may be. A tool that
a rule names is decided by its rules alone; a tool no rule names is decided by `mcp_allowlist` on
its name.

```yaml
permissions:
  file_read: ["/work/**"]
  mcp_rules:
    - tool: github/get_issue
      args:
        - {op: in_set, path: repo, values: ["acme/app"]}
        - {op: numeric_range, path: number, min: 1, max: 100000}
    - tool: fs/read_file
      args:
        - {op: path_in, path: path, scope: file_read}
```

- `tool` is a `server/tool` pattern, matched like `mcp_allowlist`. Every rule that matches a tool
  must hold.
- `args` are predicate expressions. Each `path` is an argument key (dotted for nested keys).
  `in_set`, `equals`, `numeric_range`, `length_range`, `matches`, `exists`, `and`, `or`, `not`
  and `implies` work as everywhere else. A bool is never a number: `true` does not match `1`.
- `path_in` takes absolute paths only. It matches a string, or each string of a list, against the
  `file_read` or `file_write` globs; `..` is resolved. A value that does not start with `/` (a
  relative path, `~`, `$`) is refused with one message, even under `not`. A value is never placed
  when it has a URI scheme (`file:///etc/passwd`), a Windows drive prefix, a backslash, a NUL, or
  white space at either end. Such a value fails the rule. `warden register` refuses a `path_in`
  whose scope holds a relative glob; write the glob from `/`. The match is on the text of the
  path: a symlink is not resolved.
- `known` lists the top-level keys the call may carry. Without it, the keys the expressions name.
  A call with any other key is denied, at any depth the rule does not name. `known: []` with no
  `args` admits the tool only with no arguments.
- `matches` and `not_matches` in a rule match the whole string, whatever the anchors say. The
  regex is limited to one small subset, so the Python, Go and Rust warden give the same answer.
  Inside the subset: literals, and `\` before any ASCII punctuation; `.`, `^`, `$`; ASCII classes
  `[...]` with ranges and a leading `^` (inside, only literals, escaped punctuation, ranges and
  `\d \w \s`, with `-` first, last or as a range); `\d \w \s \D \W \S` (ASCII); groups `(...)` and
  `(?:...)`; `|`; the quantifiers `* + ? {m} {m,} {m,n}` (m and n up to 1000) and their lazy forms;
  and `(?i)` at the very start. Everything else is outside it: `\p`, `\x`, `\u`, `\b`, `\A`, `\z`,
  other flags, named groups, lookahead, lookbehind, backreferences, `--`, `&&` and `[:alpha:]`
  inside a class. A rule with such a pattern is refused at registration, and the warden denies a
  call to it with "regex '...' is outside the supported subset". The engine runs in time linear in
  the string, so no pattern can hang a hook. Registration also refuses a regex that is not
  anchored (`^...$`) or that has a `|` outside a group, because the author most likely meant
  something else.
- A string longer than 4096 characters, or one that is not valid UTF-8 text (a lone surrogate), is
  denied when a `matches` or `not_matches` would read it.
- A call whose arguments are not an object (a list, a string, a number, null) is denied by any
  rule that names the tool. `known: []` admits only a call with no arguments.
- Arguments longer than 262144 characters, or nested deeper than 64 levels, are denied before any
  rule runs.

A string argument that starts with `/` or `~/`, and names something that exists, is a read of that
path for every MCP tool the envelope admits. A file must match `file_read`. A directory must match
as the directory and as a tree below it. A string that names nothing is not a read. A relative string
is not placed (a repo slug `a/b` is not a path); `path_in` refuses it. The spawn tool keeps its own
read check.

`daisugi warden schemas import <server> <tools-list.json>` stores a `tools/list` result. With it on
file, `warden register` refuses a rule that names an argument the tool does not have, or an
`in_set`/`equals` value outside a property's `enum`. With none on file, it prints one note and
registers. The warden never reads a schema to decide a call.

Under OpenCode a tool arrives as `opencode/<id>`, and under pi an unknown tool arrives as
`pi/<name>`. The payload does not say which part is the MCP server and which is the tool, so the
warden cannot place the call under `github/get_issue`. Write the rule with the name the host
gives (`opencode/github_get_issue`). A wildcard entry such as `opencode/*` or `pi/*` in
`mcp_allowlist` admits the tools of every server: `opencode/*` is not a list of servers. A rule
binds a tool name, not a server. When some rule names the tool with its `/` written as `_`
(`github/get_issue` for `opencode/github_get_issue`), the warden compares the two with case
ignored and every character but a letter or digit read as `_`. With no wildcard rule, the
call is denied and the reason says which rule. With a wildcard rule such as `opencode/*`, the
named rule applies as well, so both must hold. Name the tool in full in `mcp_allowlist` to admit
it with any arguments. A rule written with the host's own name (`opencode/github_get_issue`) and a
rule found by the loose comparison (`github/get_issue`) both bind the call: every expression of both
must hold.

What this does not do: a rule holds for one call, not a session (no "three posts at most"); it
does not read what a tool returns; schemas are not fetched from a live server; it does not cover
MCP tools reached through sprig's `Spawn`; it cannot read the session's history.

## Latency, measured

The hook command emitted by `warden settings` is `python -m opendaisugi.warden`
(argparse-only entry — no typer import). Measured full round trip per tool
call, subprocess spawn to verdict, on the development machine (claude
2.1.204, Python 3.12):

- deny path: **559–670 ms** (min–max over repeated runs, median ~590 ms)
- earlier spike, same boundary: ~0.55 s steady state, ~0.73 s p95

The cost is import-dominated (~455 ms `import opendaisugi`; the warm
verifier itself is 13–15 ms per call, 0.3 ms for a deny). A resident
verifier process would cut it and remains the documented fallback; v1
accepts the half-second because correctness of the deny path beats latency
of the allow path.

## Failure-policy summary

| Situation | Audit | Enforce |
|---|---|---|
| Call verifies in envelope | allow | allow |
| Verification fails | allow, logged `would_deny` | **deny** (exit 2) |
| Unknown tool / bad payload | allow, logged | **deny** |
| Warden internal error / slow verifier | allow, logged | **deny** |
| Warden process crashes / won't import | allow (host proceeds) | **deny** (via `\|\| exit 2`) |
| No envelope registered | allow, logged | **deny** (message names `register` and `disarm`) |
| Disarmed | allow | allow |

Passive capture (`daisugi hook record`) is unchanged and still never
blocks — capture and warden share a code path, not a failure policy.
