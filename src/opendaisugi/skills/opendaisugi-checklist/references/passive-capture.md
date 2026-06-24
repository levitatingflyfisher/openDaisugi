# Passive capture (v0.21+)

openDaisugi has two deployment modes that share one journal:

| mode | role | invocation |
|---|---|---|
| **Active supervisor** | enforces envelopes, integrity, receipts | `Daisugi.run()` / MCP `run_plan` / Supervisor in-Python |
| **Passive hook** | captures tool calls from external runtimes | `daisugi hook record` wired into Claude Code / Hermes / OpenClaw |

The passive hook is what lets the reproduction substrate learn from runs
the user already does — Claude Code sessions, Hermes sessions, OpenClaw
sessions — without changing how those runtimes work.

## When to turn it on

Always, if you have any agent that issues tool calls. The hook is
non-blocking; the worst it can do is fail to capture. There's no risk to
the host runtime.

## When to convert captures to traces

When you want distillation to learn from a session. `daisugi hook to-trace
<session_id>` synthesizes a permissive envelope from observed tool calls,
builds an ActionPlan, runs `verify()`, and appends a Trace. The trace
then feeds normal `daisugi tend`.

You don't need to convert every session — only the ones whose pattern
you want available as a reusable pathway. Throwaway exploratory sessions
can stay as raw captures and get garbage-collected.

## Auto-tend for the closed loop (v0.22+)

`daisugi hook auto-tend` closes the captures → traces → distillation
loop in one cron-friendly call. It iterates captured sessions not yet
converted, runs `to-trace` on each, and if any new traces land, runs
`Daisugi.tend()`. A min-interval gate (default 1h) prevents thrashing.

Wire it into cron, systemd-timer, or Claude's `/loop` skill — whichever
scheduler your environment provides:

```bash
# cron — every 30 minutes, real work only every 1h via the gate
*/30 * * * * /usr/local/bin/daisugi hook auto-tend
```

This is what makes the reproduction substrate a *closed loop* in
practice: without auto-tend, captures accumulate but distillation never
sees them.

## What captures look like

Each session is one JSONL file at
`~/.opendaisugi/captures/<session_id>.jsonl`. Per-call rows look like:

```json
{"captured_at": 1777144631.8, "session_id": "abc", "tool_name": "Bash",
 "step_type": "shell", "command": "git status"}
```

File-write captures store `content_len` rather than full content — the
hook is for shaping distillation, not for exfiltrating data.

## Why this isn't just "claude-code-but-worse"

Claude Code's hooks (and Hermes' and OpenClaw's) all support tool-call
blocking; v0.21 deliberately doesn't compete on that surface. The wedge
is what those ecosystems don't ship: a journal-fed reproduction substrate
that turns successful sessions into reusable pathways. The hook is the
data tap; the supervisor is the enforcement layer; both feed the same
journal so distillation sees both kinds of runs.
