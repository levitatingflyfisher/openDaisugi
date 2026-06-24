# Wiring openDaisugi into existing agent runtimes (v0.28)

**The easy path: `daisugi install`.** It detects Claude Code, Codex, Hermes,
and OpenClaw and wires all three layers (skill, MCP, capture hook) from one
bundled source of truth — idempotent, backed up, reversible with
`--uninstall`. The rest of this doc documents the manual wiring it performs.

| Layer | Claude Code | Codex | Hermes | OpenClaw |
|-------|-------------|-------|--------|----------|
| Skill | `~/.agents/skills` → `~/.claude/skills` | `~/.agents/skills` → `~/.codex/skills` | `~/.hermes/skills/opendaisugi/` | `~/.openclaw/workspace/skills` |
| MCP | `~/.claude.json` `mcpServers` | `config.toml` | `config.yaml` `mcp_servers:` | `openclaw.json` `mcp.servers` |
| Capture | PreToolUse hook | (verify per version) | `config.yaml` `hooks.pre_tool_call` | `before_tool_call` plugin |

The hook `daisugi hook record --format <host>` reads a JSON tool-call payload
from stdin and prints the host's allow contract on stdout (`{"continue": true}`
for Claude, `{}` for Hermes/OpenClaw). It never blocks.

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

Hermes spawns a subprocess per matching hook, pipes a JSON payload
(`tool`, `args`, `session_id`) to stdin, and reads stdout as JSON — `{}`
is a no-op allow. The `_payload_to_record` helper accepts both Claude Code's
and Hermes's payload shapes.

## OpenClaw

OpenClaw's `before_tool_call` lifecycle hook is exposed via the Plugin SDK
(in-process, not a shell subprocess). `daisugi install` ships a tiny ESM
plugin to `~/.openclaw/extensions/opendaisugi/` that registers
`before_tool_call` and spawns `daisugi hook record --format openclaw`:

```javascript
api.on("before_tool_call", async (event) => {
  const decision = await record(event);          // spawns daisugi hook record
  return decision && decision.block ? decision : undefined;  // passive by default
}, { priority: 10 });
```

This is also the runtime-assurance enforcement seam: `before_tool_call` can
return `{block, blockReason}` or `{requireApproval}` once verified-pathway
gating is wired. Installing the plugin requires a gateway restart
(`openclaw gateway restart`). The `before_tool_call` wiring is version-
dependent (open upstream issues); the plugin degrades to a passive no-op if
the event never fires.

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
