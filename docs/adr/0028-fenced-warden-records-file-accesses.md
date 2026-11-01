# ADR-0028: Inside a fence, the kernel is the file boundary and the warden records file accesses

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

The warden reads the text of a shell line and places the files it reads and writes. That parse
can never be complete (`git -c k=v commit`, `sed w`, `sudo tee`, wrappers, globs), and the owner
froze it on 2026-11-01. In enforce mode an access the parser cannot place, or places outside
`file_read` / `file_write`, is denied. Inside `daisugi fence` that deny adds friction and no
safety: Landlock already stops the access, except where the fence grants it (Decision 8). The owner ruled (2026-11-01, 2026-11-03): the kernel
fence is the file boundary; the warden keeps the meaning-level checks; inside a fence an
unplaced or out-of-scope shell access is recorded, not denied.

For that, the warden must know that a call comes from inside a fence, from a signal the agent
cannot set.

## Decision

1. **The signal is a fence run the resident warden finds itself.** `daisugi fence`, outside the
   sandbox, writes `<warden root>/fence-runs/<pid>.json` (its pid, the start time the kernel
   gives that pid, the kernel's boot id, the workspace). It writes it last: after it starts any helper (the resident
   warden), just before it applies the ruleset and execs the harness, which keeps the pid. For
   each call, the resident warden reads the caller's pid with SO_PEERCRED and walks its parents
   (at most 64) to a record whose pid, start time and boot id match. A record from an earlier boot
   never matches: a pid and a start time in clock ticks can repeat after a reboot. A call is fenced when it finds one.
   This is the record that already turns checkpoints off inside the fence (FN-R-21, FN-R-36).
2. **No variable is the signal.** `DAISUGI_FENCED` only makes the hook stricter (no in-process
   fallback). No port reads it to relax a check. The in-process warden (the fallback, `warden
   check`, `gate check`, replay) is always unfenced.
3. **Why a child cannot fake it.** A fenced process cannot write `fence-runs/`: the warden's state
   is read only under the ruleset, and no process can leave its Landlock domain, nor can its
   children. An unfenced process is no descendant of a fence run, since every process the harness
   starts after the ruleset inherits it. To claim the fence, an unfenced process must write the
   warden's state, and that same write lets it write `DISARMED`. That is the unconfined case,
   and an unfenced run says so (task 4 of the plan). A reused pid has a new start time, and a pid after a reboot has a new boot id.
4. **Fail closed.** A missing, unreadable or malformed record, a start time or boot id that differs or is missing, no
   SO_PEERCRED, or no resident warden: the call is unfenced, and enforce denies as before. A
   fenced hook that cannot reach the resident warden denies, as before.
5. **What a fenced call changes.** Verify takes one option, `fenced`. With it, the permission
   stage keeps the shell-access findings (`_check_shell_accesses`: a shell read or write outside
   `file_read` / `file_write`, or one that cannot be placed) out of the violations, as warnings
   that start with `fence-enforced: `. The rest of the pipeline runs as if they were not there.
   The warden then allows the call (`would_deny` false, no ask) and records each finding, without
   the prefix, in the decision's `fence_enforced` list: the audit line's `fence_enforced` and the
   session-tree verdict's `fenceEnforced`, both written only when the list is not empty.
6. **What stays the same.** The file tools (Read, Write, Edit and the like: an exact path), the
   pane rule and every hard-deny rule (warden state, Pando, secrets search, floor and config),
   the head allowlist, the metacharacter and interpreter checks, MCP rules, network, the Z3
   stages, the deadline and spawn checks all decide as today.
7. **The network is not a file access.** The fence does not limit the network. Bash opens
   `/dev/tcp/HOST/PORT` and `/dev/udp/HOST/PORT` with socket and connect, which Landlock never
   sees. A shell access to a path under `/dev/tcp/` or `/dev/udp/` stays a violation inside a
   fence, as it is without one. This holds with decomposition on and off.
8. **The kernel stops only what the fence does not grant.** The fence grants the harness its own
   paths: its config and credentials, the transcripts under `~/.claude`, and the warden root
   with the captures and the audit log. Inside a fence, a shell read of such a path is recorded,
   not denied, because the kernel allows it. The same read without a fence is denied. A shell
   line a fenced agent runs, such as `python script.py`, could already reach these paths, so
   this is not a new route. It is a place where the record, not the kernel, is what remains.

## Consequences

- Inside a fence, the frozen-table friction (`git -c k=v commit`, `taskset -c 0 pytest`, `sed w`)
  goes away, and each such access stays visible in the audit log and the session tree.
- Without a fence, enforce denies a shell access it cannot place or finds out of scope. Only the
  fence confines files, so an unfenced run says that once.
- The three ports carry the same option, field names and golden cases. Fenced cases exist only
  over the resident socket (warden_server family).
- The record is worth only what the fence is worth: with `--allow-weak-ipc` the fence still
  confines files, so the record still holds.

## Alternatives considered

- **A per-fence resident warden started with `--fenced`.** Rejected. Landlock limits what the
  fenced child can reach, not who else connects to that socket: any unfenced process of the same
  user could ask it and get the relaxed verdict, unless it checked the caller's parents anyway.
  With that check it is the decision above plus one more daemon per pane to start and stop.
- **An environment variable.** Rejected. A process sets its children's environment, and a
  harness started by hand can carry any variable.
- **Ask the kernel whether the caller is under Landlock.** Not possible: Linux shows no Landlock
  domain of another process.
