# Wiring openDaisugi into existing agent runtimes (v0.28)

**The easy path: `daisugi install`.** It detects Claude Code, Codex, Hermes,
and OpenClaw and wires all three layers (skill, MCP, capture hook) from one
bundled source of truth — idempotent, backed up, reversible with
`--uninstall`. The rest of this doc documents the manual wiring it performs.

| Layer | Claude Code | Codex | Hermes | OpenClaw |
|-------|-------------|-------|--------|----------|
| Skill | `~/.claude/skills` | `~/.agents/skills` → `~/.codex/skills` | `~/.hermes/skills/opendaisugi/` | `~/.openclaw/workspace/skills` |
| MCP | `~/.claude.json` `mcpServers` | `config.toml` | `config.yaml` `mcp_servers:` | `openclaw.json` `mcp.servers` |
| Capture | PreToolUse hook | (verify per version) | `config.yaml` `hooks.pre_tool_call` | `before_tool_call` plugin |

Install writes the **absolute path** of the `daisugi` you ran, for example
`/home/you/.local/bin/daisugi hook record --format claude`, not a bare
`daisugi`. An agent starts its hooks and MCP servers with its own `PATH`, which
often lacks `~/.local/bin`; a bare name then fails with "command not found",
the hook fails open, and nothing is recorded. Running `daisugi install` again
changes an old bare entry to the absolute one in place. `--uninstall` removes
both forms. After it writes, install reads each runtime's config back and
checks that the program of every hook and MCP command in it exists and can
run. If one cannot, it prints one line for each runtime and exits 1. Install
changes only the program word of a hook that is one plain command. A hook with
an operator or a redirection (`daisugi hook record ... || true`, `... 2>/dev/null`)
or an `env X=1` prefix stays as it is, and install prints a note that names
the runtime. A program named `daisugi-local` or any name outside `daisugi`,
`daisugi-py`, `daisugi-go` and `daisugi-rs` is yours: install never replaces
or removes it. If the path is in uv's cache (`/.cache/uv/`), install says the
path is temporary and to run `uv tool install opendaisugi` first. Restart
the agent so the hook loads, use one tool, then run `daisugi hook list` to see
the call recorded. If you move or reinstall `daisugi`, run `daisugi install`
again.

The hook `daisugi hook record --format <host>` reads a JSON tool-call payload
from stdin and prints the host's allow contract on stdout (`{"continue": true}`
for Claude, `{}` for Hermes/OpenClaw). It never blocks.

> **Want the hook to actually deny calls?** That is the *call-time warden* —
> the same interception point with the opposite failure policy (fail-closed, verified
> per call against a registered envelope). See **[Warden a live
> session](how-to/warden.md)**; this document covers only the passive capture path.

> **Enforcement (blocking) status — read before you rely on the warden.**
> *Capture* (this doc) works on all four runtimes. *Enforcement* (the warden
> emitting a block contract) is **verified only on Claude Code**, where a
> live sub-agent's out-of-envelope call is denied end to end. The Hermes and
> OpenClaw block contracts are **shipped but unverified against a real host** —
> their docs disagree with the shapes originally observed, so `daisugi` emits a
> fail-closed superset (see `hook.stdout_for_format`), but until a per-host
> contract test exists (roadmap Stage 5) treat Hermes/OpenClaw enforcement as
> *best-effort, not proven*. Codex enforcement is via the MCP path, not a
> blocking hook.

## Claude Code

Add to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|Edit|Write|Read|Glob|Grep|WebFetch|WebSearch",
        "hooks": [
          {"type": "command", "command": "daisugi hook record"}
        ]
      }
    ]
  }
}
```

Claude Code passes a JSON payload (`session_id`, `tool_name`, `tool_input`)
to the hook on stdin and reads `{"continue": true}` from stdout. The hook
is non-blocking; if `daisugi` is missing from PATH, Claude Code logs a
warning and continues — no tool calls are interrupted.

## Hermes (Nous Research)

Hermes reads its live config from `~/.hermes/config.yaml` (the repo's
`cli-config.yaml.example` is only a template — writing to `cli-config.yaml`
is a no-op, which was a bug in openDaisugi ≤ 0.27). Shell hooks:

```yaml
hooks:
  pre_tool_call:
    - matcher: ".*"
      command: daisugi hook record --format hermes
      timeout: 10
```

Hermes spawns a subprocess per matching hook and pipes a JSON payload to
stdin. The documented keys are `hook_event_name`, `tool_name`, `tool_input`,
`session_id`, `cwd`, `profile` and `extra` (`task_id`, `tool_call_id`). It reads
stdout as JSON; `{}` is a no-op allow. Sources:
`hermes-agent.nousresearch.com/docs/user-guide/features/hooks` and `.../tools`.

The hook records the tools that name a file, a command or a URL. Under
`--format hermes` the classifier knows `terminal` (shell), `read_file` and
`search_files` (read), `write_file` and `patch` (write), and `web_search` and
`web_extract` (network). It drops any other tool name. The argument names of
`web_extract` and `web_search` are not in those pages; the record takes `url`,
then the first of `urls`, then `query`. Only a run on a real Hermes box confirms
them. That check is still open (box check 1 in the ledger).

## OpenClaw

OpenClaw's `before_tool_call` lifecycle hook is exposed via the Plugin SDK
(in-process, not a shell subprocess). `daisugi install` ships a tiny ESM
plugin to `~/.openclaw/extensions/opendaisugi/` that registers
`before_tool_call` and spawns `daisugi hook record --format openclaw`:

```javascript
api.on("before_tool_call", async (event, ctx) => {
  const decision = await record(event, ctx);     // spawns daisugi hook record
  return decision && decision.block ? decision : undefined;  // passive by default
}, { priority: 10 });
```

The event carries `toolName`, `params`, `toolCallId` and `runId`. The session id
is on `ctx` (`ctx.sessionId`), not on the event. The plugin sends the hook
`{tool, args, session_id, tool_use_id, run_id}`. Source:
`docs.openclaw.ai/plugins/hooks/tool-policy`.

Under `--format openclaw` the classifier knows `exec` (shell), `read` (read),
`write`, `edit` and `apply_patch` (write), and `web_fetch` and `web_search`
(network). An `apply_patch` record names the first file in the patch. The
argument names of `read`, `write` and `edit` are not on the docs index page; the
record takes `path`, then `file_path`, then `filePath`. Whether the extension
loads without a `plugins.entries.opendaisugi.enabled` key is unknown, so
`install` writes no such key. Both checks need a real OpenClaw box (box check 2
in the ledger).

This is also where runtime assurance is enforced: `before_tool_call` can
return `{block, blockReason}` or `{requireApproval}` once verified-pathway
checking is wired. Installing the plugin requires a router restart
(`openclaw router restart`). The `before_tool_call` wiring is version-
dependent (open upstream issues); the plugin degrades to a passive no-op if
the event never fires.

## pi and OpenCode

These two have no shell hook. Their extension asks the resident warden
(`daisugi start`) about every call, and the warden mirrors each call it allows
into `<data>/warden/captures`, classified under the host's own format (`pi` or
`opencode`). `hook list`, `hook to-trace` and `hook auto-tend` read that
directory after `<data>/captures`. A session found in both is read once, from
the first. The mirror starts the same consent-first background tend the capture
hook starts. A real run on a pi and an OpenCode box confirms the path (box
checks 3 and 4 in the ledger).

## Verifying the hook is firing

After wiring, run any tool call from the host (e.g., a shell command in
Claude Code). Then:

```bash
daisugi hook list
```

You should see a session row with `calls > 0`. From there:

```bash
daisugi hook to-trace <session_id>      # synthesize a journal trace
daisugi tend                            # run distillation; pathways may emerge
```

## What captures store

Per call: a JSONL row with the tool name, the step type (shell /
file_read / file_write / network), and the relevant primitive (command,
path, or url). File writes store `content_len` rather than the full
content — captures shape distillation, not data exfiltration.
