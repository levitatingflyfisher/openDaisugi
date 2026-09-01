# Grafts: research inputs (2026-09-26)

Input for the grafts design: a fourth gate verdict, **rewrite**, that replaces a tool call with a
cheaper one. Source of the idea: `docs/plans/2026-09-24-omarchy/strategy-2026-09-26.md`, section
"Token saving". Grafts amend the rule in `docs/research/token-saving-landscape-mapping.md` (line 46)
that daisugi must not edit requests. Every outside claim cites a page read on 2026-09-26.

## 1. Spotify Portal and shunt

**Source.** Dimitri Mazmanov, Spotify Engineering, 3 September 2026
([post](https://engineering.atspotify.com/2026/9/portal-by-spotify-cut-my-claude-code-token-usage-by-90)).
Summaries: [ZenML LLMOps database](https://www.zenml.io/llmops-database/reducing-coding-agent-token-costs-with-declarative-model-routing),
[DEV](https://dev.to/jamilxt/spotify-cut-claude-code-token-usage-by-90-percent-the-pattern-works-in-any-ai-agent-2i4b).

**Code.** Public. The plugin is `plugins/shunt` in
[spotify/portal-ai-plugins](https://github.com/spotify/portal-ai-plugins/tree/main/plugins/shunt).
The GitHub API reports the license as **Apache-2.0**. (ZenML says no license is given. That is wrong.)
shunt needs Portal: it calls one `aika:invoke-chat` action per delegation through `portal-cli`.

**Three parts** (README: "Hooks block ... Scripts handle the AiKA invocation ... Skills tell Claude
when and how to call the scripts"):

| Part | What it does |
|---|---|
| `hooks/check-file-size` | PreToolUse on `Read`. Allows a read with `offset` or `limit`, a missing file, or a file of `SHUNT_MIN_LINES` (default 350) lines or fewer. Otherwise prints `{"decision": "block", "reason": "File is N lines ... Use the /bulk-reader skill ... If you need exact content for editing, re-read with an offset/limit"}`. |
| `hooks/check-bash-read` | PreToolUse on `Bash`. Same test for a command that starts with `cat`, `head`, `tail`, `less` or `more`. Allows any command with `|` or `>`. |
| `scripts/bulk-read` | Wraps files in `<file path="...">` tags, sends files plus a question to the `bulk-reader` mode. |
| `scripts/code-write` | Sends a spec plus a required `--reference` file to `code-writer`; strips fences; `--target` writes to disk. "Claude never sees the generated code." |
| `skills/*` | Tell Claude when to call each script. |

**What the hook does, exactly.** It denies and redirects. It does not rewrite. It uses the
top-level `decision` field, which Claude Code calls deprecated for PreToolUse: "The deprecated
values `"approve"` and `"block"` map to `"allow"` and `"deny"`"
([hooks docs](https://code.claude.com/docs/en/hooks)). Its allow output, `{"decision": "allow"}`,
is not a documented value. The effect is not verified. It exits 0. The claude model then makes a second call (the skill).

**Models.** Both modes default to Gemini 2.5 Flash at temperature 0.2. Any AiKA-configured model
works. The bulk-reader prompt says "Output structured bullets only ... Lead every bullet with the
exact name, type, or line number."

**Results** (README, a 162K-line Java monorepo, measured by the author):

| Scenario | Lines | Without | With | Saved |
|---|---:|---:|---:|---:|
| Single large file | 4,014 | 33,684 | 5,737 | 82% |
| Source + test pair | 7,408 | 75,990 | 4,148 | 94% |
| Multi-file cross-service | 1,281 | 16,221 | 821 | 94% |
| Code-write | 3,667 | 40,614 + generation | 833 lines to disk | n/a |

The measure is tokens "Claude would consume". It is not billed cost. No variance and no task
success rate are given. ZenML notes the "comparison was authored by the implementer".

**Caveats from the author.** "You can't delegate editing. The worker model's summaries don't
include reliable line numbers." The worker "missed a subtle thread-safety bug". Latency is "10–30
seconds, and Portal caps a single invocation at 30 seconds". (The README's `SHUNT_TIMEOUT_SECONDS`
defaults to 180. The two figures disagree.) Only bulk-reader is enforced; code-writer relies on
the skill. Payloads pass through argv (120 KB on Linux). Bypasses the hook does not catch:
`sed -n`, `awk`, `python -c`, or any pipe or redirect.

## 2. Can each harness rewrite a tool call?

**The main finding.** No harness lets a hook change *which* tool runs. Each rewrite field changes
the arguments of the same tool. (This is inferred from each API's shape. No doc says it outright.)
So "Read becomes delegate-read" has three possible shapes:

- **(a) Same-tool input rewrite.** Works only where the target is a shell: `cat Big.java`
  becomes `daisugi delegate-read Big.java`. RTK does this in production (below). For a built-in
  read tool, the only same-tool rewrite is adding `offset`/`limit`, which truncates and does not
  delegate.
- **(b) Output replacement.** The local tool runs (free), then a post hook swaps the result for the
  worker's summary before the model sees it.
- **(c) Deny and redirect.** What shunt does. It costs one extra model turn, which re-reads the
  cached prefix.

| Harness | Deny | Rewrite input | Replace output |
|---|---|---|---|
| Claude Code | Yes: `permissionDecision: "deny"` or exit 2 | Yes: `updatedInput` with `allow` or `ask` | Yes: PostToolUse `updatedToolOutput` |
| Codex | Yes: `deny` or exit 2 | Yes: `updatedInput` with `allow` | Partly: `decision: "block"` replaces the result with the hook's text; `updatedMCPToolOutput` unsupported |
| OpenCode | Yes: throw | Yes: mutate `output.args` | Yes: mutate `output.output` (source-read) |
| pi | Yes: `{block: true, reason}` | Yes: mutate `event.input` | Yes: `tool_result` returns `content`, `details`, `isError` |
| Hermes | Yes: `{"action":"block","message":...}` | Yes: `{"action":"modify","args":{...}}`, plugin and shell hook | Yes: `transform_tool_result` |
| OpenClaw | Yes: `block: true` | Yes: return `params` | Transcript only: `tool_result_persist` returns `{message}` |

**Claude Code** ([hooks docs](https://code.claude.com/docs/en/hooks)). `updatedInput` "Replaces the
entire input object". Permission rules are evaluated "against the input your hook returns". `allow`
"skips the permission prompt". For built-in tools, a `updatedToolOutput` that "doesn't match the
tool's output schema is ignored and the original output is used." "All matching hooks run in
parallel"; the docs do not say what happens when two return different `updatedInput`. "A timed-out
command hook lets the tool call continue."

**Codex** ([hooks docs](https://learn.chatgpt.com/docs/hooks)). "For Bash commands and
`apply_patch`, `updatedInput` must include a string `command` field. For MCP and other local
function tools, `updatedInput` is the replacement arguments object." PostToolUse `decision:
"block"` makes Codex "replace the tool result with that feedback". For unsupported fields, "Codex
marks the hook run as failed, reports the error, and continues the tool call": fail-open.

**OpenCode** ([plugin docs](https://opencode.ai/docs/plugins/)). The docs' example rewrites
input: `output.args.command = escape(output.args.command)`. In
[`session/tools.ts`](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/session/tools.ts)
the registry wrapper passes the same `args` object to `tool.execute.before` and then to
`execute`, and returns the `output` object after `tool.execute.after`, so both mutations take effect.

**pi** ([types.ts](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/types.ts),
[extension docs](https://pi.dev/docs/latest/extensions)). "`event.input` is mutable. Mutate it in
place ... Later `tool_call` handlers see earlier mutations. No re-validation is performed after
mutation."

**Hermes** ([hooks.md](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/hooks.md)).
Shell hooks also accept `{"decision": "modify", "tool_input": {...}}`. Plugin `pre_tool_call`
timeouts fail closed; "By default shell hooks **fail open**" unless the entry sets
`fail_closed: true`.

**OpenClaw** ([tool policy hooks](https://docs.openclaw.ai/plugins/hooks/tool-policy)). Each
handler "sees an isolated copy of the original event". `tool_result_persist` changes what is stored: "Blocking persistence is not a
tool-execution veto." Whether the persisted message is what the model reads next is **not
verified**.

**What openDaisugi uses today.**
- `src/opendaisugi/gate.py` (about lines 1240-1275) already emits `hookSpecificOutput.updatedInput`
  with `allow` on the Claude path, for an operator edit from `ask`. For pi and OpenCode (exit-code
  formats) it denies an edit, saying the format "has no updatedInput channel".
- `src/opendaisugi/harness_pi/extension/index.ts` returns `{block: true, reason}` or nothing. Its
  socket client times out at 5 s; the gate's verify budget is 4 s.
- `src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts` only throws.
- `src/opendaisugi/hook.py` `stdout_for_format` emits Hermes and OpenClaw block shapes, marked
  unverified in `docs/hook-integration.md`.
- `src/opendaisugi/install.py` wires the gate on Claude Code and Codex (`_patch_codex_gate`).
  Nothing uses PostToolUse or any output hook.

## 3. Other delegation and offload tools

- **RTK** ([rtk-ai/rtk](https://github.com/rtk-ai/rtk), Apache-2.0, about 82k stars). This is shape
  (a) in production. [`hooks/claude/rtk-rewrite.sh`](https://github.com/rtk-ai/rtk/blob/main/hooks/claude/rtk-rewrite.sh)
  returns `permissionDecision: "allow"` with `updatedInput` (and a second branch that omits
  `permissionDecision` so Claude Code still prompts). The README lists installers for Codex, pi and
  Hermes. The README claims "up to 90% of the bash output" and says "it is not the
  same as cutting your bill by 90%". It does not cover built-in `Read`, `Grep` or `Glob`.
  [Quesma](https://quesma.com/blog/does-rtk-make-ai-coding-cheaper/) (11 Sept 2026, Terminal-Bench
  2.1, 1,740 attempts) measured 5% lower cost on Claude Code and 5% higher on OpenCode/DeepSeek;
  "Extra turns can erase the savings"; terminal output was "~7% of Fable's context"; verdict "We do
  not recommend RTK as a generic cost-saving tool."
- **Headroom** ([headroomlabs-ai/headroom](https://github.com/headroomlabs-ai/headroom),
  Apache-2.0). A proxy, library and MCP server that compresses tool output. It keeps originals
  locally so the model can call `headroom_retrieve`. It claims 21% to 57% on four agent scenarios,
  with accuracy flat on N=100 sets. Vendor numbers. As a proxy it edits requests, which
  the mapping doc rejects.
- **Anthropic context editing** ([blog](https://claude.com/blog/context-management), 29 Sept
  2025). It "clears stale tool calls and results". Anthropic reports 84% fewer tokens on a 100-turn
  internal search eval, plus 29% better results (39% with the memory tool).
- **Sub-agents** ([Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)).
  Subagents condense results for the lead agent, but "multi-agent systems use about 15× more
  tokens than chats". Delegation moves tokens to other models; it does not remove them.
- **Tokens are not cost** ([arXiv 2607.12161](https://arxiv.org/abs/2607.12161), Weinberger and
  Hozez, 2026). On Claude Code, the most aggressive compression "reduced delivered tool-output
  tokens by 38.4% but increased billed cost by 6.8%". Correlation was r = 0.15. Cache reads and
  writes dominate input cost, and compression added turns.

## 4. Risks

- **Prompt injection through the summary.** The worker reads untrusted file text and returns free
  text into the claude model's context. [Beurer-Kellner et al.](https://arxiv.org/html/2506.08837)
  say a quarantined LLM's output "must satisfy certain safety constraints", and prefer symbolic
  returns that the privileged model does not read. A bulk-reader summary does not meet that bar.
  The gate still checks the next call. Mark delegated output as untrusted in the journal.
- **Hooks that rewrite after the gate checks.** In pi, later handlers see earlier mutations with
  "no re-validation". Claude Code runs hooks in parallel and does not document conflicting
  `updatedInput`. If RTK or shunt runs beside the gate, the gate can verify input X while Y runs.
  This matters for fail-closed even with no grafts.
- **Approval raise.** On Claude Code, `allow` plus `updatedInput` skips the permission prompt.
  Today the gate's allow is `{"continue": true}`, which grants nothing. A graft must not raise a
  call's approval level: omit `permissionDecision` (as RTK's ask branch does) unless the original
  call was already allowed.
- **Effects the envelope must grant.** A delegate read adds a network effect (or a local-model
  effect). shunt's `code-write --target` writes files the gate never sees; it sees only the script.
- **Line numbers.** Summaries lose exact lines, so edits fail or need a second targeted read.
  shunt's own reason text tells Claude to re-read with offset/limit.
- **Latency and fail-open.** A worker takes 10 to 30 s. A Claude Code PreToolUse timeout lets the
  call continue; Codex and Hermes shell hooks also fail open. pi's gate client waits 5 s. So the
  worker must not run inside the gate's decision path. Rewrite to a delegate command (shape a), and
  let the command call the worker.
- **Silent no-op.** A wrong-shape `updatedToolOutput` is ignored; the full output reaches the
  model. That fails open on cost, not safety.
- **Quality.** Cheap models miss subtle bugs (shunt's thread-safety case). Extra turns can cancel
  the savings (Quesma). Measure billed cost per successful task, in shadow first.

## Open

1. **Graft shape.** Recommend (a) for shell reads: one turn, worker outside the hook, every
   harness supports it. Built-in reads (`Read`, pi `read`, OpenCode `read`, Hermes `read_file`)
   are the main case and (a) cannot reach them: use (b) where the output shape is tested per tool,
   else (c).
2. **Worker.** Local model (local-first) or a cloud model the envelope grants. Recommend local
   default, with the router picking.
3. **Gate order.** Refuse to install beside other input-rewriting hooks, or verify the final input
   (PostToolUse check). Recommend refusal plus a warning at install.
4. **Output channel for pi and OpenCode.** Add a rewrite field to the gate socket reply, so both
   harnesses can carry grafts and operator edits.
