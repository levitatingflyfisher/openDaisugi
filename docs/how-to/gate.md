# Gate a live session's tool calls

*How to put the call-time gate (ADR-0007) in front of an agent you are
already running: shadow first, tune with the report, then flip one flag to
enforce. And how to turn it off in one command if it over-denies.*

The gate takes each intercepted tool call, synthesizes it into a one-step
plan, and runs the **full `verify()` pipeline** against the session's
registered envelope before the call executes. Deny-by-default: an unknown
tool, unparseable input, an internal exception, or a slow verifier all deny
(in enforce mode). Strictness comes from the envelope's `stakes` — the gate
never relaxes it.

## Enforcement class, stated up front

- **Claude Code: hard enforcement, contract-tested.** A committed live test
  (`tests/test_hook_gate_contract.py`) proves settings-injected PreToolUse
  hooks fire in headless runs and that the gate's exit-2 deny actually
  blocks the call, on the pinned CLI version. This includes a test where the
  *real shipped gate* denies a real `Read` in a real `claude -p` run.
- **Hermes / OpenClaw: unverified.** The block shapes are emitted
  belt-and-braces (Hermes gets both `decision` and `action` keys), but no
  per-version contract test exists yet — treat these paths as observation
  until roadmap Stage 5 lands. Claiming otherwise would be fail-open
  committed at the documentation level.
- **Fail-open edges outside our control**, on every host: the host's outer
  hook timeout, or a harness that silently stops firing hooks. The gate's
  inner verify timeout denies *before* the host's outer timeout can fail
  open, but it cannot help if the hook is never invoked.
- **A dead gate denies.** On Claude Code any hook exit that is not 2 is
  non-blocking, so a crashed gate or a broken install would otherwise allow
  silently. The command emitted by `gate settings` therefore ends in
  `|| exit 2`, mapping every nonzero-non-2 exit — including the package
  failing to import, where the gate's own error handling never runs — to a
  deny. **If you hand-write your hook command, keep that suffix.** Without
  it the gate fails open exactly when it is most broken; this is
  live-verified in `tests/test_hook_gate_contract.py`.

## 1. Register an envelope

The gate checks calls against a per-session envelope; without one, enforce
mode denies everything (fail-closed — the deny message tells you so).

```bash
# The default envelope — every session without its own binding uses it.
daisugi gate register my-envelope.json

# Or bind one session specifically:
daisugi gate register my-envelope.json --session <session-id>
```

Envelope files are JSON or YAML `Envelope` documents. For a first envelope,
generate one from a captured session (`daisugi hook to-trace` infers one),
write one by hand, or start from an example in `examples/`.

## 2. Shadow mode — observe before you trust

```bash
claude --settings "$(daisugi gate settings)"
```

Shadow mode evaluates every call and logs what enforce *would* have denied,
but always allows. **Shadow mode is observation, not protection.**

Review the verdicts:

```bash
daisugi gate report            # or --json for the full records
```

The report lists every would-deny verbatim, and flags the two known
over-denial classes as **false-positive candidates**: compound-command
metachar denials (`a && b` — the verifier offers a decomposition instead)
and host tools the classification map doesn't know (deny-by-default sweeps
those up wholesale). Tune the envelope until the would-denies are the calls
you actually want denied.

You can also tune offline against an existing passive capture, without
running an agent at all:

```bash
daisugi gate replay ~/.opendaisugi/captures/<session>.jsonl --envelope my-envelope.json
```

## 3. Enforce — the one-flag flip

```bash
claude --settings "$(daisugi gate settings --enforce)"
```

**Pinning the envelope (`--session`).** By default the gate selects an
envelope by the `session_id` in each hook payload, falling back to the
default. That is fine when the host is the only writer of `session_id`, but
authorization then keys on a field the caller can influence: a payload
claiming another registered session's id would be checked against *that*
session's (possibly more permissive) envelope. To close that off, pin it:

```bash
claude --settings "$(daisugi gate settings --enforce --session job7)"
```

With `--session` set, the payload's `session_id` is recorded (the report
keeps `payload_session_id` for traceability) but never authorizes. The pin
lives in the hook command, which the agent cannot rewrite — the same
principle as a sub-agent's gate root living outside its workspace.
`AgenticExecutor` pins its sub-agents automatically.

On the Claude Code path a deny is exit code 2 with the proof-backed reason
on stderr — the model sees *why* ("permissions: Step 's0' file_read path
'/etc/passwd' not permitted by file_read ['/allowed/**']") and can restate
its approach inside the envelope.

## 4. The exit: one command, no tool call required

```bash
daisugi gate disarm     # gate allows everything until re-armed
daisugi gate arm        # resume
daisugi gate status     # armed state + registered envelopes
```

`disarm` is deliberately a plain CLI command run from any shell — if an
over-denying gate has an agent locked up, the operator's way out does not
itself pass through the gate.

## Latency, measured

The hook command emitted by `gate settings` is `python -m opendaisugi.gate`
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

| Situation | Shadow | Enforce |
|---|---|---|
| Call verifies in envelope | allow | allow |
| Verification fails | allow, logged `would_deny` | **deny** (exit 2) |
| Unknown tool / bad payload | allow, logged | **deny** |
| Gate internal error / slow verifier | allow, logged | **deny** |
| Gate process crashes / won't import | allow (host proceeds) | **deny** (via `\|\| exit 2`) |
| No envelope registered | allow, logged | **deny** (message names `register` and `disarm`) |
| Disarmed | allow | allow |

Passive capture (`daisugi hook record`) is unchanged and still never
blocks — capture and gate share a seam, not a failure policy.
