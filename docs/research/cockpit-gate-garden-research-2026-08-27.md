# The gate + garden cockpit: what to steal, what to avoid

*Deep-research run, 2026-08-27. Two workflows: the generic deep-research harness (107
agents, 25 sources fetched, 25 claims put to a 3-vote adversarial panel, 18 survived,
13 merged findings) and a per-tool pass (15 researchers, one per tool or tool pair,
each followed by an adversarial verifier of its 4 load-bearing claims; ~660 sources,
mostly primary docs and source code). Claims below carry a status: **[confirmed]**,
**[qualified]** (true with a correction, given inline), **[refuted]**, or
**[unverified]** (researched, verifier did not run). Everything else is argued craft.*

*Companions: [`tool-interface-doctrine.md`](tool-interface-doctrine.md) (the bimodal
doctrine), [`../plans/2026-08-26-daisugi-interface-two-lenses.md`](../plans/2026-08-26-daisugi-interface-two-lenses.md)
(W1-W9; this report is the design input for W8), and
[`../harness/harness-comparison.md`](../harness/harness-comparison.md) (paths A-E).*

---

## 0. Honest limits, first

- **Coverage.** The generic run verified only pi, Claude Code, Cline and the prompt-cache
  economics. The per-tool pass covered all fifteen targets; six verifiers (Hermes,
  OpenClaw, k9s/lazygit, htop/zellij/tmux, Factorio/DF, multi-agent UIs) hit a session
  limit on the first run and were re-run to completion. Every tool's four load-bearing
  claims now carry a verdict; only two tools (oh-my-pi, Conductor) rest on researcher
  reads alone because none of their claims was load-bearing enough to verify.
- **Time sensitivity.** pi moved from `badlogic/pi-mono` to `earendil-works/pi` and
  changes daily. Codex removed file undo in April 2026. Claude Code figures are from
  v2.1.247 docs and local session-file inspection. OpenClaw's rewind exists on
  `main` but may post-date the v2026.7.1 stable line.
- **Refuted, do not cite.** Cline's "shadow git repo" data model and a "three-way
  destructive restore" (0-3; the current SDK uses private refs in the user's own repo,
  see §3.5). "Naive full-context caching makes latency worse" (0-3). The prescriptive
  "gate review must finish inside 5 minutes" rule (1-2: the underlying gap mechanism is
  real, the rule is a hypothesis to measure). Anthropic's "sub-agents return a 1-2k
  token summary" (1-2). Manus's byte-stable-prefix checklist and logit masking (1-2:
  unconfirmed rather than false).
- **Web-search budget.** Several researchers exhausted WebSearch and fell back to
  direct fetches of primary docs and HN's Algolia API. Third-party critique is
  therefore thinner than primary evidence.

---

## 1. The verdict in one page

Ten laws for daisugi's cockpit. Each one is backed in §2 and §3.

1. **Roster first, transcript second.** The default screen lists agents grouped by
   what the operator must do, with *Needs input* on top and a peek pane that shows
   the exact proposal and takes the answer without attaching. (Claude Code agent
   view, oh-my-pi Agent Hub, Gas Town problems view, Vibe Kanban.) This is the direct
   fix for "it felt like a settings page".
2. **The transcript is an append-only tree.** Every entry has `id` and `parentId`;
   the session has a movable head. Rewind moves the head. Fork copies the active path
   into a new file that names its parent. Nothing is ever deleted. (pi, OpenHands V1
   SDK, OpenClaw, Codex `SessionMeta`.) Claude Code stores this tree but cannot
   navigate it: that is the trap to avoid.
3. **Rewind has two axes.** Conversation and workspace restore independently, the
   menu names which one, and the screen says what was *not* restored. (Claude Code
   rewind menu, Cline, `pi-rewind`, Devin `/revert`.) A rewind that leaves the
   working tree ahead of the transcript is a correctness bug (Codex, OpenClaw,
   Aider, OpenHands forks).
4. **Fail-closed must be built inside the hook.** Every harness hook layer we read
   lets the action proceed on timeout, crash, or invalid JSON; only an explicit exit 2
   or `deny` blocks. The exceptions (OpenClaw `before_tool_call`, Hermes
   `pre_tool_call`, pi `tool_call` errors) prove it is a choice. daisugi's hook exits 2
   on *any* internal error and keeps its own deadline below the host timeout.
5. **A byte-stable, append-only prefix is the whole caching story.** Real coding-agent
   steps are ~96% cache reads; a median step re-reads ~120k cached tokens and appends
   ~850. Compaction is the one deliberate invalidation. Per-step gate verdicts go in
   the tool result or an appended tail, never in the system prompt.
6. **The human pause is the cost cliff.** Misses concentrate on steps after a human
   pause (gaps over 5 minutes start missing; after an hour nearly every step misses).
   A gate that parks an agent for review is that pause. Show time-since-last-request
   and expected cache state per session; keep parked sessions warm.
7. **Meter from provider counters.** `cache_read` / `cache_write` / uncached / output
   per step, per agent, per pathway. "Tokens saved" is a fold over usage fields,
   never an estimate.
8. **Orchestration overhead is a full context per agent.** Forks that inherit the
   parent prefix are cache reads; fresh sub-agents pay a full prefix each; supervisors
   that are themselves LLM loops burn tokens forever (Gas Town). Watchdogs and gates
   are deterministic code. Sub-agents return sparse, structured results.
9. **Intervene at the tool-call boundary.** Steering text lands at the next boundary,
   not mid-action. Bulk verbs act on tagged rows. "Approve and remember" mints an
   envelope rule with a scope, proposed but never auto-applied.
10. **Alerts by class, honesty by glyph.** Most verdicts scroll in a log; a curated
    class pauses and zooms. Stale numbers are dim, estimates carry a `?`, dead
    controls are labeled. (Dwarf Fortress announcements, Factorio radar, daisugi's
    own `●/○/·`.)

---

## 2. The four key questions

### 2A. How do the best tools let you prompt + fork + time-travel a prompt tree?

**The comparison.**

| Tool | In-place tree navigation | Fork = copy | Workspace rewind | Data model | Status |
|---|---|---|---|---|---|
| **pi** | `/tree` (Esc Esc): move the leaf to any entry; user entry → text back in editor | `/fork`, `/clone` → new file with `parentSession` | No (`/tree is not code undo`); `@ayulab/pi-rewind` adds 3-scope git rewind | One JSONL per session; every entry `{id: 8-hex, parentId}`; `compaction`, `branch_summary`, `label`, `custom` entries | [qualified]: the leaf is **not persisted**; on resume it is the last entry in file order |
| **oh-my-pi** | Same `/tree` + `/branch`, model-callable `checkpoint`/`rewind`, `/btw` → `f` promotes a side answer to a branch | Yes | No | Same, plus `reset_boundary`, `providerPromptCacheKey` in header | [unverified] |
| **OpenHands V1 SDK** | `navigate_to(event_id)` moves `leaf_event_id`; abandoned events drop out of the view | `fork(from_event_id)` copies `path_to_root` | **No: fork shares the working directory** | One JSON file per event; `parent_id` on every event; storage linear, tree logical | [confirmed] (merged 2026-07-02); Canvas exposes only *Branch from here* |
| **OpenClaw** | `sessions.rewind` (head repoint), `sessions.branches.switch`; refused while a run is active or for harness-owned sessions | `sessions.fork` from a chosen message; `sessions.create {fork:true}` from the tail | No (files not reverted); worktree snapshots are a separate mechanism | Transcript entries `id` + `parentId`; `compaction`, `branch_summary`; per-agent SQLite on `main`/2026.8 betas, per-session JSONL in stable | [qualified]: rewind, branch-switch and mid-transcript fork exist only on `main` and the 2026.8.1 betas; stable v2026.7.1 has only the tail fork |
| **Claude Code** | **None.** `/rewind` moves the head in place; abandoned tails stay on disk but vanish from `/resume` (issue #55347, closed not planned) | `/branch`, `--fork-session`, `/fork` copy to a new session id; no parent link written | Yes, per user prompt: `file-history-snapshot` records + `~/.claude/file-history/<session>/<hash>@vN`; 100 checkpoints, 30 days; skips bash edits, most sub-agent edits, symlinks | JSONL with `uuid`/`parentUuid` (a DAG, observed locally with many branch points across many session files); head tracked by a `last-prompt {leafUuid}` record; format declared internal | [qualified]/[confirmed] |
| **Codex CLI** | Esc-Esc backtrack (conversation only) | `codex fork`, `thread/fork` (`lastTurnId`/`beforeTurnId`) | **None** since 2026-04-28 (ghost commits removed after 100 GB `.git` bloat) | Rollout JSONL; `SessionMeta{forked_from_id, forked_from_ordinal_exclusive, parent_thread_id, history_base}`; paginated mode: a fork **references** the parent's frozen prefix instead of copying | [qualified] |
| **Cursor** | None | Fork Chat copies the prefix (later tokens released); Side Chat = parallel thread with hidden parent context | Checkpoint restore = files only, messages stay | Flat ordered array, no parent pointers (reverse-engineered); one user's store hit 30 GB | [confirmed]/[qualified] |
| **Cline** | None; message restore starts a **new session** | Implicit | Yes: stash-shaped commit pinned at `refs/cline/checkpoints/<session>/<run>`, once per user run; restore wrapped in a rollback ref | Session rows + `restoredFromSessionId` in metadata | [qualified] |
| **Aider** | None (issues #3607/#2219 unanswered) | No | `/undo` = revert the last aider commit with explicit refusals | git DAG only; markdown chat log | [qualified] |
| **Devin CLI** | `/steps` enumerates; steps survive compaction | `/fork [step]` → new session | `/revert <step>` rewinds **files and conversation together** | ATIF-v1.4 trajectory with per-step cost; no documented parent pointer | [confirmed] |
| **Hermes** | None (`/undo` soft-deletes backward only) | `/branch` row copy with `parent_session_id` + `_branched_from`; the TUI copies the full persisted transcript, the CLI and gateway copy the live projection (summary + tail after compaction) | `/rollback` from a shared bare git store + agent-write ledger (opt-in, off by default) | SQLite (schema v26 on `main`, docs say v23); one overloaded `parent_session_id` FK plus `_branched_from`/`_delegate_from`/`_reset_from` markers; per-message `active`/`compacted` flags; `api_content` sidecar for cache-stable replay | [qualified] |
| **Gas Town** | None | `gt seance --talk <id>` = `claude --fork-session --resume <id>` | Crash checkpoints only | Dolt SQL for work state; Claude's own session store for conversations | [qualified] |
| **Conductor** | None | Fork = files up to the point + a chat *summary* | Per-turn private git ref; revert deletes later messages | Private refs | [unverified] |

**What the verified evidence says the right model is.** pi's substrate scored highest
and survived 3-0 votes: one append-only JSONL per session, `id`/`parentId` on every
entry, time-travel is moving one leaf pointer, context is rebuilt only on load,
navigate and compact (so the in-memory prefix stays append-only between events), and
on leaving a branch the operator may append a `branch_summary` so the rejected path
still informs the next one. Three corrections to carry: pi does not persist the leaf
(the daisugi store should); pi's `/tree` never touches files (pair it with a workspace
snapshot or say so on screen); extension runtime state is not rewound on `/tree`
(daisugi's envelope and verdict ledger must be reconstructed from the branch, or facts
from an abandoned branch leak into the new one).

Claude Code contributes the **user-facing rewind design**: checkpoint at every prompt
boundary; a menu with *Restore code and conversation / Restore conversation / Restore
code / Summarize from here / Summarize up to here / Never mind*; the two code rows
hidden when the checkpoint has no tracked edits; the original prompt dropped back into
the input box so "edit an earlier prompt" needs no new UI; and branching kept as a
separate verb from rewind. Codex contributes **reference-not-copy forks** (`history_base`
pointing at a frozen prefix) and **non-destructive revert** (a new immutable rollout
file with a distinct id). Devin contributes **one addressable step id** that the
journal, the verdicts and the snapshot all key on, and that survives compaction.

**The recommended daisugi tree** is in §4.3.

### 2B. How is prompt caching maximized, and how do orchestrators avoid a full context tax per step?

**The economics (verified).**

- TraceLab (~4,300 sessions, ~350k steps, Claude Code + Codex, Sep 2025-Jun 2026):
  ~96% of prompt tokens are served from the prefix cache; a median step re-reads 126k
  (Claude) / 116k (Codex) cached tokens and appends 857 / 886 fresh ones; tool-result
  steps hit ~98%, user-initiated steps after a human pause hit 79-87%; gaps over five
  minutes start producing misses and after an hour almost every step misses, giving
  5.3x overall prefill amplification (35x on user-initiated steps). **[confirmed 3-0]**
  ([arxiv 2606.30560](https://arxiv.org/abs/2606.30560))
- Cache reads are 0.1x base input on every Anthropic model; writes are 1.25x (5-min
  TTL) or 2x (1-hour). One local usage history across many responses: roughly
  428:1 input:output, ~97% cache reads, uncached cost would have run several
  times the actual billed amount. **[qualified 2-1]**
- Lumer et al. (DeepResearch Bench): caching cut API cost 41-80% and savings scale
  linearly with prompt size (88-89% at 50k tokens). **[qualified 2-1]**
  ([arxiv 2601.06007](https://arxiv.org/abs/2601.06007))

**The prefix rules (what every cache-competent harness converged on).**

1. **Fixed order, stable part first.** Claude Code: system prompt + tool definitions →
   project context → conversation; "Claude Code sends your full conversation with
   every request" and saves only through prefix caching. Aider: system → examples →
   read-only files → repo map → history → chat files → current → reminder, with up to
   three breakpoints. OpenHands: a two-block system message where block 0 is a
   byte-identical template guarded by a regression test and block 1 carries all
   per-conversation context unmarked. Hermes: stable → context → volatile tiers.
   OpenClaw: an explicit cache-prefix boundary with a normalized fingerprint.
2. **No dynamic values at the head.** pi shipped a per-second timestamp in its system
   prompt from at least 0.56.2 (Mar 2026) to 0.80.4 (Jul 2026) and forfeited caching
   for months (issue #1873). Cline's `{{CURRENT_DATE}}` sits at line 10 of its prompt,
   and for the Cline provider the prompt embeds the HEAD commit hash, so every commit
   before a rebuild changes the prefix. Session-stable values go at the *end* of the
   system prompt behind the breakpoint; per-step values go at the tail or nowhere.
   **[qualified 2-1]**
3. **Never touch the tools array.** Names, descriptions, schemas and order stay
   byte-stable; new capability arrives as deferred stubs (`defer_loading` + tool
   search) or is appended where it first appears (pi 0.80.7 "cache-friendly dynamic
   tool loading"). Anthropic: "Changing the tool set in the middle of a conversation
   is one of the most common ways people break prompt caching."
4. **Announce change, do not edit.** Claude Code appends a `<system-reminder>` when a
   file changes and does not apply CLAUDE.md edits until restart *precisely so they
   cannot break the prefix*; skills, plans and mode switches are injected as messages
   or implemented as tools (`EnterPlanMode`), never as a system-prompt swap. Cline's
   Plan/Act switch is "an invisible system-prompt swap" that invalidates everything.
5. **Compaction is the one deliberate invalidation.** Claude Code clears old tool
   results first, then summarizes; the summarization request rides the warm prefix
   ("a fork of a cached call"); the summary re-reads up to five recently modified
   files. pi's new harness materializes a `retainedTail` in the compaction entry so
   context rebuild never reads past it. Devin hides auto-compaction from the
   scrollback (avoid: it is the one event that explains a cost spike). **[confirmed 3-0]**
6. **Forks read the parent's cache; fresh sub-agents do not.** A Claude Code
   `context: fork` sub-agent inherits the exact prefix, so its first request is a
   cache read; an ordinary sub-agent builds its own with a 5-minute TTL by default
   (`subagentPromptCacheTtl: 1h` to extend). Dynamic-workflow fan-outs hold all but
   the first sibling for up to 5 s so they read one prefix. Hermes keys the provider
   `prompt_cache_key` to the lineage root and isolates branch children; OpenClaw pins
   a key per session and seeds forks from the parent header. Codex keys the cache to
   the thread id with no client control, so identical 9k startup prefixes across
   sessions got 0% hits (issue #35300). **[qualified]**
7. **Keep parked sessions warm.** Aider `--cache-keepalive-pings N` re-sends the cacheable
   prefix with `max_tokens=1` every ~295 s. OpenClaw runs a heartbeat every 30-55 min
   with a 1-hour TTL under Anthropic OAuth and prunes tool results only after the TTL
   has elapsed, resetting the clock only when content actually changed. Claude Code
   gives the main conversation a 1-hour TTL on a subscription.
8. **Measure, then alert.** Read `cache_read_input_tokens` / `cache_creation_input_tokens`
   from usage. Anthropic: "We alert on cache breaks and treat them as incidents." pi
   added `showCacheMissNotices`. OpenClaw runs a live cache regression gate with hard
   floors. OpenHands has a test that fails if dynamic content leaks into the static
   block. Aider hides cache stats under streaming (avoid).
9. **Cache scope is a design input.** Claude Code's key includes cwd, so worktrees of
   one repo miss each other; the summarization request through a custom
   `ANTHROPIC_BASE_URL` gateway is not cache-marked and bills uncached (relevant to
   daisugi's own gateway).

**Orchestration overhead: what the tax is and how tools dodge it.**

The tax is one full context per agent. Claude Code agent teams run ~7x tokens in plan
mode; Cursor says five parallel sub-agents use roughly five times the tokens; Devin
says a workflow "can consume far more ACUs than doing the same task in a single
session" and that "most practical use remains single Devin"; Gas Town's every
supervisor (Mayor, Deacon, Witness, Boot, Dogs, Refinery) is an LLM in a loop, Yegge
needed a second Claude account, and 0.9.0 removed a Boot "decision engine that consumed
unbounded tokens on failed installs".

The dodges that work:

- **Fork instead of spawn** when the child needs the parent's context (cache read).
- **Sparse, structured returns.** Devin's SWE-grep retrieval returns "a list of files
  with line ranges", not summaries, because >60% of first-turn time was retrieval;
  oh-my-pi's `task` tool returns "a schema-validated object the parent reads directly.
  No prose to parse"; Hermes bounds child results at 32k chars; pi's sub-agent example
  caps at 50 KB.
- **Deterministic fan-in.** Devin `devin_session_gather` waits for N sessions to
  settle with zero model tokens; Claude Code and Devin dynamic workflows hold the plan
  in a script, not in an LLM manager.
- **A tiny primer instead of tool schemas.** Beads installs one `SessionStart` hook
  that injects ~1-2k tokens and re-fires after compaction; the docs put MCP schemas at
  10-50k per request. Cursor's dynamic context discovery keeps only names in static
  context and loads bodies from files (46.9% fewer agent tokens on MCP runs). Cline
  loads ~100 tokens of metadata per skill. Hermes replaces MCP schemas with three
  bridge tools. **[qualified]** (Beads figures are the project's own)
- **Resume from state, not transcript.** Gas Town splits identity (permanent), sandbox
  (per assignment) and session (cycled after every step, on compaction, on crash) and
  restores from beads + git, so every LLM call starts near the beginning of its
  context window.
- **Deny cheaply.** pi's `tool_call` handler can return `terminate: true` so a fully
  blocked batch does not pay for a wasted follow-up turn.
- **daisugi already has the best rung:** a pathway hit costs zero model tokens, and
  ADR-0015's cache-aware sticky routing refuses to downgrade a conversation with a deep
  warm prefix. The cockpit's job is to make both visible.

### 2C. How do skills, tools and extensions plug in?

**Convergence.** Every harness in scope except Aider and the games now ships the same
three shapes:

| Shape | Standard | Who |
|---|---|---|
| **Skills** | `SKILL.md` + YAML frontmatter (Agent Skills / agentskills.io); name + description in the prefix, body on demand | Claude Code, Codex, Cursor, Cline, OpenHands (renamed from microagents, three directory generations), pi, Hermes, OpenClaw, Devin |
| **Hooks** | The Claude Code contract: JSON on stdin, exit 2 = block with stderr as the reason, or `hookSpecificOutput.permissionDecision allow\|deny\|ask` + `updatedInput` | Copied by Cursor ("matches Claude Code behavior for compatibility"), Codex, OpenHands ("matches the Claude Code hook contract"), Devin, Hermes shell hooks |
| **Plugins** | `plugin.json` + `skills/` + `mcp.json` (Agent Plugins 1.0.0, 2026-08-06, OpenAI/Microsoft/AWS/Cursor/Vercel; hooks and security out of scope) | Claude Code, Codex, Cursor, Cline, OpenHands, Devin, OpenClaw ("compatible bundles") |

Outliers worth knowing: **pi** has no MCP and no permission system; extensions are
TypeScript modules hot-reloaded in-process with full system access, and the gate is
whatever a `tool_call` handler returns. **Beads** plugins are `plugin.md` files fired
by gates (cooldown / cron / condition / event) and executed by worker agents. **zellij**
plugins are WASM with a per-capability permission prompt (the only sandboxed plugin
model in the set). **Aider** has none of it upstream; the MCP PR sat for months with no
maintainer comment and the community forked (cecli).

**The finding that matters most for a gate: hooks fail open.**

| Harness | Explicit block | Timeout / crash / invalid output | Status |
|---|---|---|---|
| Claude Code | exit 2, or JSON `deny`; `deny > defer > ask > allow` | **Proceeds.** "A timed-out command, http, or mcp_tool hook doesn't block the tool call"; exit 1 = non-blocking; a hook that cannot start = non-blocking; invalid JSON on exit 0 = proceeds. Only Agent SDK callback hooks fail closed on timeout | [qualified] |
| Cursor | exit 2 = deny; `ask` honored only by `beforeShellExecution`/`beforeMCPExecution` | **Proceeds** unless per-hook `failClosed: true` (default false) | [qualified] |
| Codex | exit 2 or JSON `deny`; `updatedInput` only with allow; hooks ON by default since 2026-04-23 | Not verified | [qualified] |
| Devin | exit 2 | "Other: Error, logged but doesn't block" | [qualified] |
| OpenHands | exit 2, JSON `deny`, or `continue:false` | Prompt/agent hooks with a missing or invalid decision **default to allow**; async PreToolUse hooks cannot block | [qualified] |
| Hermes | Python `pre_tool_call` → `{action: block\|approve\|modify}`; a callback past `hook_callback_timeout` (30 s) **blocks**; shell hooks block on exit 2 | Shell hooks' own spawn error, timeout (60 s) or bad stdout **proceed** unless `fail_closed: true` (honored only on `pre_tool_call`); a shell hook that is not allowlisted is silently not registered in non-TTY runs, and editing the script does not invalidate consent | [qualified] |
| OpenClaw | `before_tool_call` → block / modify / `requireApproval` | On `main` and the 2026.8 betas: **fails closed** on error or 15 s timeout, unresolved approvals always deny. In stable v2026.7.1 a timed-out handler is aborted and skipped, and `timeoutBehavior: "allow"` lets an unresolved approval proceed | [qualified] |
| pi | `tool_call` → `{block, reason, terminate}` | "tool_call errors block the tool (fail-safe)"; first blocking handler short-circuits | [confirmed] |
| Cline | file hook `cancel:true` aborts the whole run; per-tool `{enabled:false}` hides a tool from the model | `cancel` never reaches the model as a reason | [qualified] |

So: exit 2 on any internal error, an internal deadline well under the host's 600 s
default, and (from OpenClaw's CVE-2026-33579) **exactly one allow function** that every
surface (hook, MCP bridge, CLI, chat command) routes through, plus a test for it.
Hermes' RFC on Pi/OpenCode found OpenCode shipped a typed-but-dead `permission.ask`
hook for six months: add a CI check that every declared hook has a live dispatch site.

**Garden analogues (distillation with a review gate).**

- OpenClaw **Skill Workshop**: agent-drafted skills always land as PENDING proposals;
  `/learn` turns a conversation into one; evaluator hooks produce attributed outcomes;
  `curator status` tracks live usage; unused skills auto-retire, important ones pin,
  overlaps are flagged. Hermes **Curator**: active → stale → archived at 30/90 days,
  tar.gz snapshot before every pass, per-mutation audit ledger with single-entry
  rollback, LLM consolidation off by default; `/journey` is a time-scrubbed star map of
  learned skills. Devin **Knowledge Usage** labels each retrieved item *Useful* or
  *Misleading* per session. Codex **`ApprovedExecpolicyAmendment`** and Devin's
  once / session / project / global approval scopes turn a verdict into a durable rule.
  Hermes **`approvals suggest`** mines approval history into ranked allowlist
  proposals: never auto-applied, destructive classes never proposed.
- Factorio **parametrised blueprints** (typed holes, derived parameters, formulas over
  numbers, a fill-in dialog at placement) are the shape daisugi's ADR-0008
  parameterized pathways already have; the blueprint string (version byte + base64
  zlib JSON) is a proven shareable text format.

### 2D. Which interaction models fit a gated harness without the gate being the only surface?

- **Roster grouped by state, peek to answer.** `claude agents` groups sessions with
  Pinned and Ready for review (open PR) above Needs input, then Working, then
  Completed; `Space` peeks at the exact pending question and takes a reply or a
  numbered option without attaching; `--json` exposes `state` and `waitingFor`
  (`permission prompt`, `input needed`, `sandbox request`, `worker request`, `dialog
  open`) **[qualified]**. oh-my-pi's
  Agent Hub (Alt+A) shows status running/idle/parked/aborted, parent link, model,
  cost, tokens, tool-call count, context use; `Enter` focuses, `x` kills, `r` revives,
  `t` flat/tree. Gas Town's `gt feed --problems` groups by GUPP Violation / Stalled /
  Zombie / Working / Idle with `n` nudge and `h` handoff on the row **[qualified]**:
  four states come from work-state data, Zombie from a tmux liveness probe.
- **One line per agent, expand on exception.** OpenClaw collapses consecutive calls
  into "Ran 13 commands, read 6 files, edited 9 files" with the newest running call as
  the header; the session rail's headline is the model's latest safe preamble and it
  auto-expands once when a run is stuck or needs input. Cline's Task Timeline is one
  block per tool call; Kanban cards show only the latest message or call.
- **A proposal is a suspended command.** zellij parks a resurrected command behind a
  *Press ENTER to run...* banner and shows a finished command's exit code with ENTER
  to re-run and Ctrl-c to close. Factorio's ghosts are a placed intent with a tint
  that changes when a robot is assigned, openable and editable before they are built.
  daisugi's allow/deny moment already has an idiom operators know.
- **Steer at the next boundary.** Cursor (since 2026-08-19), pi, OpenClaw's default
  `steer` queue mode and Hermes `/steer` all inject a message at the next tool-call
  boundary rather than tearing a running action; OpenClaw pairs skipped calls with a
  synthetic result so the transcript stays paired. `/btw` (Claude Code, Codex `/side`,
  Cursor, oh-my-pi, OpenHands `/ask_agent`, Devin side chats) answers a side question
  outside history; oh-my-pi's `f` promotes the answer to a branch.
- **Tag, then verb.** tmux `choose-tree` tags with `t`, tags all with `C-t`, filters by
  a format expression with `f`, and runs one command per tagged item with `:`; htop's
  `Space`/`c` tag a row or subtree and F9 acts on the set. "Deny all tagged", "raise
  budget on tagged", "replay from tagged checkpoint" need no modal per item.
- **A verdict has two audiences and a third outcome.** Cursor hooks return
  `user_message` (for the human) and `agent_message` (for the model) separately;
  OpenHands feeds `reject_pending_actions(reason)` back as an observation the model
  sees next step; Cursor's auto-review can *ask the agent to try another approach*,
  the important state between allow and ask.
- **Approvals that mint rules, with scope and expiry.** Codex `ReviewDecision`:
  Approved / ApprovedForSession / ApprovedExecpolicyAmendment / Denied / Abort. Devin:
  once / session / project / project-local / global, plus "edit the command first"
  and "plain-language rewrite". Aider: `(Y)es/(N)o/(A)ll/(S)kip all/(D)on't ask again`
  with `explicit_yes_required` so a blanket "all" can never approve a shell command.
  OpenClaw binds `allow-always` to exact argv + cwd with a 30-day expiry and rejects
  execution if the plan changed between approval and run.
- **The machine-facing plane is the same plane.** Codex `app-server` (JSON-RPC 2.0;
  thread/turn/item; server-initiated `item/*/requestApproval`) is what the TUI, IDE
  and desktop all consume. pi's `--mode rpc` relays approval dialogs as typed
  `extension_ui_request`s and gives `get_entries {since}` cursors. OpenClaw's Gateway
  owns approvals as durable rows (pending → allowed | denied | expired | cancelled,
  first-answer-wins compare-and-set, a runtime epoch that cancels stale rows on
  restart) and projects a child's gate request to every ancestor session with a replay
  of up to 1,000 pending approvals on reconnect. tmux control mode frames every reply
  with `%begin/%end/%error`, streams per-pane deltas, lets the client declare
  backpressure (`pause-after`), and rate-limits derived values to once a second.
- **Consent cannot be relayed.** Claude Code marks every message from another agent as
  not-from-the-user; it cannot answer a permission prompt, change settings, or run
  slash commands. A fail-closed gate is only fail-closed if no agent can launder an
  approval through a sibling.
- **The ramp lives in the chrome.** k9s prints the hotkeys valid for the current view
  in its header, with `?` for the full list, and in-scope plugins appear in the same
  menu. lazygit's bottom bar shows the keys for the focused panel; its command log
  panel (`@`) shows the exact git commands and became one of its most valued features.
  zellij draws mode ribbons on line 1 and, on line 2, hints *generated from the live
  keymap*, with a compact bar + tooltip key for experts. htop's F-key bar has an
  explicit `--no-function-bar` escape.
- **Two grammars, one instrument.** k9s: `:` for commands (`:pod`, `:xray`, history
  with `[`/`]`, `-` for last) and `/` for filters (regex, `/!` inverse, `/-l` labels,
  `/-f` fuzzy). htop's named *screens* cycle with Tab and persist as a few `key=value`
  lines.
- **Alerts by class; interrupt policy by class.** Dwarf Fortress has three tiers
  (minor scrolls past; major sounds and may pause + recenter; game-changing pauses,
  zooms and boxes) and every event class carries its own BOX / PAUSE / RECENTER /
  ALERT flags in `announcements.txt`. Factorio aggregates alerts per class with a
  count and a fixed lifetime, each clickable to zoom to the cause, and turned the
  chatty turret-fire class off by default in 2.0.7.

---

## 3. Per-tool cards

Each card: what it is, the patterns to steal, the patterns to avoid, with sources.
Full researcher output (per tool, ~20-45 KB each, with every source and the verifier's
evidence) is in [`cockpit-sources-2026-08-27/`](cockpit-sources-2026-08-27/); this
section keeps the load-bearing parts.

### 3.1 Claude Code (v2.1.247)

**Steal**
- Append-only JSONL with `uuid`/`parentUuid`, a tiny `last-prompt {leafUuid}` head
  record, and checkpoint records interleaved in the same log with blobs content-
  addressed under `file-history/<session>/<hash>@vN`. Rewind = move the head; branch =
  copy the file. **[confirmed]** ([checkpointing](https://code.claude.com/docs/en/checkpointing), [claude-directory](https://code.claude.com/docs/en/claude-directory))
- The six-row rewind menu with two independent axes, code rows hidden when empty, and
  the prompt returned to the input box. **[confirmed 3-0]**
- Prefix discipline as engineering: every tool in every request, deferred MCP stubs,
  skills injected as messages, `<system-reminder>` appended for file changes, CLAUDE.md
  edits deferred to restart, `/rewind` truncating to a cached prefix, fork sub-agents
  reading the parent's cache, fan-out stagger ≤5 s, "alert on cache breaks and treat
  them as incidents". **[qualified]** ([prompt-caching](https://code.claude.com/docs/en/prompt-caching), [workflows](https://code.claude.com/docs/en/workflows))
- Hook payload carries `session_id`, `transcript_path`, `tool_use_id`, `agent_id`,
  `agent_type`, and hooks fire inside sub-agents: `agent_id` is the join key for a
  many-agent view. **[qualified]** ([hooks](https://code.claude.com/docs/en/hooks))
- Agent view: Pinned / Ready for review / Needs input / Working / Completed, peek +
  inline reply, `--json` with `waitingFor`. Workflow view: phases → agents with token
  totals, a *Large workflow* advisory at 25 agents / 1.5M projected tokens.
  **[qualified]** ([agent-view](https://code.claude.com/docs/en/agent-view))
- Cross-session trust law: relayed messages cannot approve anything. ([cross-session-messaging](https://code.claude.com/docs/en/cross-session-messaging))

**Avoid**
- Calling an in-place rewind a fork while the abandoned tail becomes unreachable
  (issue #55347, closed not planned).
- An undocumented, version-unstable transcript schema: upgrades have broken
  `parentUuid` chains (#24304); forked copies without title rows are invisible to the
  picker (#85008); sub-agent files carry no parent `tool_use_id` (#32175).
- Partial undo presented as undo: no bash edits, no background sub-agent edits, no
  symlinks, 100 checkpoints, 30 days.
- Agent teams as N full sessions over JSON mailboxes and tmux panes: ~7x tokens, no
  resume of teammates, teams forming when a sub-agent is merely named.
- Lossy auto-compaction as the default strategy for anything that must survive: the
  envelope and standing constraints must sit in the stable prefix or be re-injected
  after every compaction boundary (Beads' `SessionStart` re-fire is the cheap way).

### 3.2 OpenAI Codex CLI (0.150.x)

**Steal**
- `SessionMeta` as the tree header: `session_id`, `id`, `forked_from_id`,
  `forked_from_ordinal_exclusive`, `parent_thread_id`, `history_base {rollout_id,
  ordinal}`. Paginated forks reference the parent's frozen prefix instead of copying
  it; `thread/revert` writes a new immutable rollout with a distinct id. **[qualified]**
  ([protocol.rs](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs))
- Model-invisible lines inside the transcript: `turn_context` (approval policy, sandbox
  policy per turn) and `security_risk_score` per tool call, so a branch replays the
  envelope that was in force. ([security_risk.rs](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/security_risk.rs))
- `ReviewDecision` that mints durable rules (`ApprovedExecpolicyAmendment`,
  `ApprovedForSession`) beside plain Approved/Denied/Abort.
- Rollout-trace: a cheap raw spine on the hot path, an offline reducer that builds the
  graph. ([rollout-trace](https://github.com/openai/codex/blob/main/codex-rs/rollout-trace/README.md))
- A headless core behind JSON-RPC with server-initiated approval requests; TUI, IDE
  and desktop are clients. **[qualified]**: approvals arrive only for actions the
  `approvalPolicy` escalates, so a per-action gate must use `PreToolUse` hooks, not the
  approval channel. ([app-server](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md))

**Avoid**
- Checkpoints inside the user's `.git` (ghost commits, `refs/codex/turn-diffs`): one
  5.7 GB project grew 102 GB of orphans; tree-pointing refs broke libgit2 clients; the
  feature was deleted (PRs #19481, #20511). **[qualified]**
- Shipping conversation rewind without file rewind and calling it done (#11626 open).
- Binding the cache key to the thread id with no client key or breakpoint: 0% hits on
  identical 9k prefixes across sessions, ~54% for app-server fleets (#35300, #21796).
- An automated approver (Guardian) that can deny or abort with no path back to the
  human (#21975).
- A UI view of history that drifts from the durable file (0.147.0 backtrack failure,
  #37421): render from the same store the fork reads.

### 3.3 Cursor (3.x)

**Steal**
- Rewind restores files and never deletes messages or verdicts: users accept it.
  **[confirmed]** ([checkpoints](https://cursor.com/docs/agent/chat/checkpoints))
- Two branch primitives with different costs: Fork (prefix copy; later tokens
  released) and Side Chat (hidden parent context, `@`-mention to pull the result
  back, no nesting). **[confirmed]** ([side chats](https://cursor.com/help/ai-features/side-chats))
- Three-tier run mode: allowlist runs at once, sandboxable runs sandboxed, the residue
  goes to a judge that can allow, *ask the agent to try another approach*, or escalate.
  ([run-modes](https://cursor.com/docs/agent/security/run-modes))
- Hook output with two audiences (`user_message`, `agent_message`) and a `failClosed`
  switch. **[qualified]** ([hooks](https://cursor.com/docs/agent/hooks))
- Context as files with a names-only static index (46.9% fewer tokens on MCP runs),
  and the full history written to a file before compaction so the agent can grep what
  the summary lost. ([dynamic-context-discovery](https://cursor.com/blog/dynamic-context-discovery))
- Subscriptions: an idle agent costs zero and "needs input" is an event; the API
  reports `cacheReadTokens`/`cacheWriteTokens` per run.

**Avoid**
- Hooks fail open by default.
- An LLM classifier as the guardrail: "best-effort guardrails rather than a hard
  security boundary"; it "can allow a call you would have blocked".
- Flat copy-on-fork storage with no parent pointers, forks that cannot merge, docs
  that contradicted each other about what a fork copies, a 30 GB store.
- Orchestration as chat slash commands (`/worktree` runs setup as chat turns that
  "burn unnecessary tokens"; >20 failed attempts reported).
- A cockpit that hides both the code and the bill (context indicator removed in 2.0;
  per-request cost only in a dashboard; $2k/week reports).

### 3.4 Aider (v0.86.2)

**Steal**
- One git commit per applied step and a bounded `/undo` that refuses unsafe cases
  with named reasons (not this session's commit, pushed, merge commit, dirty file,
  file absent in parent). **[qualified]** ([commands.py](https://raw.githubusercontent.com/Aider-AI/aider/main/aider/commands.py))
- One budget line after every reply: `Tokens: X sent, Y cache write, Z cache hit, A
  received. Cost: $B message, $C session.`
- Fixed prompt layering with up to three breakpoints and a keepalive thread that
  re-sends only the cacheable prefix with `max_tokens=1` every ~295 s. **[qualified]**
  ([caching](https://aider.chat/docs/usage/caching.html))
- Confirmation prompts with group semantics and `explicit_yes_required` for shell.
- A token-budgeted, relevance-ranked map (PageRank over a tree-sitter symbol graph,
  personalized to files in chat and identifiers in the prompt, binary-searched to the
  budget). **[qualified]**: the default budget is 1k-4k depending on the model.
  ([repomap](https://aider.chat/docs/repomap.html))
- `/save` writes a human-readable command script that reconstructs context;
  `/copy-context` exports the exact assembled prompt.

**Avoid**
- `/undo` rewinds code but not the conversation; no message ids, no branch.
- Edits applied and committed before the operator sees them; architect mode
  auto-accepts by default since v0.81.
- Cache statistics hidden under the default streaming mode; "caching not working"
  issues unresolved (#2492, #4676).
- Integration demand left in PRs with no maintainer decision (MCP PR #3937 closed
  unmerged after months; community fork).

### 3.5 Cline (extension 4.1.16, SDK 0.0.81)

**Steal**
- Three independent restores: files only, conversation only, both; the CLI warns
  which git commands will run. ([checkpoints](https://docs.cline.bot/core-workflows/checkpoints))
- The current data model: a stash-shaped commit with a synthesized third parent for
  untracked files, pinned at `refs/cline/checkpoints/<session>/<run>` (invisible to
  `git stash list`), taken once per root user run before the first model call, with
  restore wrapped in a rollback-transaction ref. **[qualified]**: checkpoints are
  opt-in in the SDK, on by default in the extension. ([checkpoint-hooks.ts](https://github.com/cline/cline/blob/main/sdk/packages/core/src/hooks/checkpoint-hooks.ts))
- The gating stance: "Checkpoints make auto-approve practical." Cheap per-step rollback
  is what lets the operator loosen the gate for low-risk categories while high-risk
  stays fail-closed. **[confirmed 3-0]** ([auto-approve](https://docs.cline.bot/features/auto-approve))
- Task Timeline: one block per tool call / message / edit in the header.
- Per-tool policy trichotomy: `{autoApprove:true}`, `{autoApprove:false}`,
  `{enabled:false}` (the model does not see the tool).
- OpenTelemetry vocabulary: `task.tool_used{auto_approved}`, `task.tokens{cached_tokens}`,
  `task.checkpoint_used{action}`. ([otel events](https://docs.cline.bot/enterprise-solutions/monitoring/opentelemetry-events))

**Avoid**
- Docs that describe one data model (shadow git, commit per tool use) while the code
  does another.
- The legacy shadow-git path that renamed nested `.git` to `.git_disabled` and left
  repos broken on crash (#4385).
- Message restore that silently starts a new session; no tree UI.
- The cache breakpoint on the volatile last user message; a `{{CURRENT_DATE}}` and
  the HEAD hash in the system prompt; a fresh CLI per message for the Claude Code
  provider (zero cache hits). **[qualified]**
- Removing the "max N API requests before pausing" cap and the master toggle as
  "complexity without value" after users depended on them (v3.35).

### 3.6 OpenHands (SDK v1.44.0, Agent Canvas 1.15.0)

**Steal**
- A logical tree over a linear log: `parent_id` on every event, `leaf_event_id` as
  HEAD, `fork(from_event_id)`, `navigate_to(event_id)`, legacy events fall back to
  index-1 with zero migration. **[confirmed]** ([PR #3922](https://github.com/OpenHands/software-agent-sdk/pull/3922))
- Two-block system prompt: block 0 static and cache-marked, guarded by a regression
  test; block 1 dynamic and unmarked; second breakpoint on the last user/tool message.
  **[qualified]** ([test](https://github.com/OpenHands/software-agent-sdk/blob/main/tests/sdk/llm/test_prompt_caching_cross_conversation.py))
- Condensation as an event `{forgotten_event_ids, summary, summary_offset}` applied at
  view time, `keep_first` protecting the prefix. ([condenser](https://docs.openhands.dev/sdk/arch/condenser))
- Deny-with-reason as an observation the model sees; an ensemble risk signal where
  UNKNOWN counts as risky. ([security](https://docs.openhands.dev/sdk/guides/security))
- `/interrupt` (cancel in flight) distinct from `/pause` (finish the call);
  `/ask_agent` sidebar question; a Critic score posted under each finish.
- Path-triggered rules injected once per conversation at zero baseline cost, tracked
  in `activated_path_rules` (the "reuse" counter the garden needs).

**Avoid**
- Blocking delegation (`thread.join()` on every child; background mode open since Feb
  2026, #2047).
- Fork shares the working directory: branches from an earlier prompt run against a
  filesystem that already holds the later branch's edits.
- Security risk supplied by the policed model (a required `security_risk` argument in
  every tool schema; models that omit it break tool calling, #11309).
- Three generations of skill directories and keyword triggers on user messages only.
- Caps instead of budgets and docs that drift from code (condenser 240/2 in code,
  120/4 in docs; Gemini caching silently off).

### 3.7 Devin (cloud + CLI)

**Steal**
- The step is the unit: `/steps`, `/fork [step]`, `/revert <step>` (files and
  conversation together), steps addressable after compaction. **[confirmed]**
  ([commands](https://docs.devin.ai/cli/reference/commands))
- Per-step committed cost in the trajectory file (`committed_acu_cost`, tokens,
  `generation_model`, `request_id` per step).
- Hook lifecycle with a first-class `PermissionRequest`, `PreToolUse` `updatedInput`
  (narrow an action instead of denying it), `PostCompaction` with the summary on stdin.
  **[qualified]**: only exit 2 blocks; other non-zero exits proceed.
- Approval scopes with "edit the command first" and "plain-language rewrite", under
  `deny > ask > allow` precedence. ([permissions](https://docs.devin.ai/cli/reference/permissions))
- Session Insights *Knowledge Usage*: each retrieved item labeled Useful or Misleading.
- `devin_session_gather` for zero-token fan-in. **[confirmed]**
- Fusion's cache rule: switch models only at compaction, "which would trigger a cache
  miss anyway"; retrieval helpers return file + line ranges, not summaries.
  **[qualified]** ([devin-fusion](https://cognition.com/blog/devin-fusion))

**Avoid**
- A composite opaque cost unit (ACU) repriced four times in 18 months.
- Features announced in release notes with no reference page (web checkpoint and
  rewind).
- Autonomy with no stuck detector (Answer.AI: "work away for literally days").
- LLM-manager fan-out as the default; Cognition itself says most practical use is a
  single Devin.
- Hiding automatic compaction from the scrollback.

### 3.8 pi (0.84.3) and oh-my-pi

**Steal**
- The tree substrate (§2A) with `custom` entries for cockpit state the model never
  sees, `label` bookmarks, `branch_summary` on leaving a branch, `/fork`/`/clone` as
  file copies with `parentSession`. **[qualified]**: leaf not persisted.
  ([session-format.md](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/session-format.md))
- `/tree` keys: ↑/↓ move, ←/→ page, Ctrl+←/→ fold branch segments, Ctrl+O cycles
  filter modes (default → no-tools → user-only → labeled-only → all), Shift+L label,
  Shift+T timestamps, Enter select; selecting a user message prefills the editor.
  ([sessions docs](https://pi.dev/docs/latest/sessions))
- The `tool_call` contract: mutable `event.input`, `{block: true, reason, terminate}`;
  `terminate` skips the follow-up turn when every call in the batch was blocked;
  errors block (fail-safe). **[confirmed]** ([extensions.md](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md))
- RPC mode: LF-delimited JSONL, typed `extension_ui_request` dialogs, `get_entries
  {since}` cursors, `get_tree` with `leafId`. **[qualified]**: no built-in RPC command
  moves the leaf; an extension can expose `ctx.navigateTree` as a `/command`.
  ([rpc.md](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md))
- Cache hygiene shipped as features: date removed from the prompt (0.80.4),
  cache-friendly dynamic tool loading (0.80.7), summaries under a fresh routing id
  with caching disabled (0.82.0), `showCacheMissNotices`, `cacheRead`/`cacheWrite` in
  every usage line. Breakpoints: every system block, the last immediate tool, the last
  user block. **[qualified]** ([anthropic-messages.ts](https://github.com/earendil-works/pi/blob/main/packages/ai/src/api/anthropic-messages.ts))
- oh-my-pi's model-callable `checkpoint` + `rewind`: the concise report *is* the
  reusable artifact and the exploratory tokens leave the live path while staying in
  the file for audit. ([rewind.md](https://github.com/can1357/oh-my-pi/blob/main/docs/tools/rewind.md))
- oh-my-pi's Agent Hub and the rule "headless sub-agent + `prompt` = reject".

**Avoid**
- No permission system in core, YOLO by default in both (users report the agent
  "autonomously searching the author's home directory").
- Volatile content in the stable prefix (the timestamp bug, months long).
- Time-travel that rewinds only the transcript; extension state not rewound.
- Extensions and packages execute with full system access in the same process as the
  thing that blocks tool calls.
- Refusing XDG and closing the issue ("will not be changed, period"; HN 56 points,
  users leaving).

### 3.9 Gas Town + Beads (Steve Yegge)

**Steal**
- Problems view: agents grouped by health state derived from work-state data (Zombie
  from tmux liveness), with the intervention key on the row. **[qualified]**
  ([README](https://github.com/steveyegge/gastown))
- Seance: fork a predecessor conversation read-only via the harness's own
  `--fork-session --resume` and ask it a one-shot question. **[qualified]**: sessions
  are discovered from `~/gt/.events.jsonl` only; only the Claude preset supports fork.
- The events journal as a binlog: gapless per-clone `seq`, full post-mutation
  snapshot, `tail --since <seq> --follow`, an SSE endpoint, bounded retention that
  **fails loudly** (`events_journal_truncated`) instead of silently losing history.
  **[qualified]**: on `main` only and unreleased as of 2026-08-27 (v1.2.2 re-released
  the tested 1.1 line without it); opt-in, off by default; floors are a time/count
  window, not a consumer watermark. ([events-journal.md](https://raw.githubusercontent.com/steveyegge/beads/main/docs/reference/events-journal.md))
- Cost accounting from the harness's own transcripts (four token buckets per assistant
  turn, per session / role / rig, recorded by a Stop hook).
- A tiny stable primer (`bd prime`, ~1-2k tokens) on `SessionStart`, which re-fires
  after compaction, instead of 10-50k tokens of MCP schema. **[qualified]**
- Gates as typed dependency edges (human, timer, gh:run, gh:pr) that keep steps out of
  the ready frontier. ([gates.md](https://raw.githubusercontent.com/steveyegge/beads/main/docs/workflows/gates.md))
- The identity / sandbox / session split, with sessions cycled after every step.

**Avoid**
- The metaphor tax (Mayor, polecat, witness, deacon, dog, refinery, convoy, molecule,
  wisp, seance, GUPP): third parties had to publish decoder rings before people could
  evaluate the design (Brinker, Abrahms, Appleton).
- Every supervisor an LLM in a loop: "expensive as hell"; a second Claude account;
  a Boot decision engine that "consumed unbounded tokens".
- Shipping unattended default work the operator did not opt into (issue #3649: users'
  Mayors worked upstream issues on the users' credits and GitHub accounts).
- A commit-per-write versioned database as the hot path on an unreviewed 189k-LOC
  codebase ("I've never seen the code"): Dolt bloat, "database is read only",
  50+ race fixes for 1.0, an accidental untested Beads release.
- "Visibility without reading" through a chatty Mayor plus "don't watch your agents
  work": users report "almost no visibility of what's going on".

### 3.10 Hermes Agent (Nous Research, v0.20.6)

**Steal**
- Typed verdicts `{action: block|approve|modify}` where `approve` escalates to the
  human and a timed-out Python callback **blocks** (30 s default). Plus the RFC lesson:
  CI-check that every declared hook has a live dispatch site. **[qualified]**: the
  fail-closed timeout is for Python plugin callbacks; shell hooks fail open unless
  `fail_closed: true`. ([hooks](https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks), [RFC](https://github.com/NousResearch/hermes-agent/blob/main/docs/rfcs/2026-07-plugin-architecture-lessons-pi-opencode.md))
- Three-tier prompt (stable → context → volatile), a frozen memory snapshot, bounded
  plugin sections frozen per build, and a provider cache scope keyed to the lineage
  root with fork children isolated. **[qualified]**: current source places four
  breakpoints (end of the stable tier, end of the system prompt, last two messages;
  the docs still describe system + last three), and a date-only timestamp line sits
  inside the cached volatile tier, so the prefix rolls daily. Only `pre_llm_call`
  plugin context and later-turn recall ride the user turn.
  ([prompt-assembly](https://hermes-agent.nousresearch.com/docs/developer-guide/prompt-assembly))
- `/rollback`: one shared bare git store, per-project refs, a checkpoint before each
  destructive tool at most once per directory per turn, an agent-write ledger of
  content hashes so hand edits survive; rollback also undoes the last chat turn.
- `/agents` overlay: live tree by parent, per-branch cost/token/file rollups, kill and
  pause per child, `stalling · no progress 450s` vs `active 3s ago`, append-only
  per-task logs. ([delegation](https://hermes-agent.nousresearch.com/docs/user-guide/features/delegation))
- `hermes approvals suggest`: propose only, explicit apply, never propose the
  irreversible.
- Curator lifecycle + `/journey` time-scrubbed star map (§2C).
- `/context`: a 5x20 glyph grid (1 cell = 1% of the window) plus a per-category table.

**Avoid**
- Everything-in-one-repo sprawl (11.5k files, 25+ adapters, ~1,300 commits a week,
  36k open issues) with date-tag releases: an operator cannot audit what changed in
  the gate between two builds.
- Default bloat in the cached prefix (>10k tokens of bundled skill listings after the
  wizard; "debloating a fresh Windows install").
- Fail-open where a gate must fail closed (shell hooks; the `/goal` judge; checkpoints
  off by default), and consent that does not track the code: a shell hook that is not
  allowlisted is silently not registered in non-TTY runs, and editing the script does
  not invalidate the earlier consent. **[qualified]**
- `approvals.mode: smart` lets an auxiliary LLM auto-approve "low-risk" dangerous
  commands; the escalation prompt shows the command but no checkable reason.
- Trust erosion from defaults and governance (a betting skill in the default set;
  silent third-party routing claims; an edited issue).

### 3.11 OpenClaw (Node.js)

**Steal**
- A durable approval object: pending → allowed | denied | expired | cancelled with
  first-answer-wins CAS; timeout, no route, malformed verdict and run abort all resolve
  to deny/cancel; a runtime epoch cancels stale rows on restart while the deep link
  still explains what happened. **[qualified]**: this is `main`/2026.8-beta behaviour;
  stable v2026.7.1 still honours `timeoutBehavior: "allow"`.
  ([operator-approvals](https://docs.openclaw.ai/refactor/operator-approvals))
- Ancestor audience projection with replay of pending approvals on reconnect.
  ([protocol](https://docs.openclaw.ai/gateway/protocol))
- The append-only transcript tree (`id` + `parentId`, `branch_summary`), rewind as a
  head repoint, fork from a chosen message, a title-bar branch menu, all refused
  server-side while a run is active or for harness-owned sessions. **[qualified]**:
  `main` and 2026.8.1 betas only; stable has the tail fork alone.
  ([control-ui](https://docs.openclaw.ai/web/control-ui))
- Stable-prefix / volatile-suffix boundary, deterministic tool-catalog ordering,
  `cacheRetention` merged global → model → agent (effective only on Anthropic-family,
  OpenAI and Gemini routes), TTL-aware pruning that fires only once context exceeds
  ~30% of the window, heartbeat keep-warm, counters in every status surface, a live
  regression floor. **[qualified]** ([prompt-caching](https://docs.openclaw.ai/reference/prompt-caching))
- Approvals bound to a canonical execution plan: `allow-always` binds to argv + cwd
  with a 30-day expiry; the run is rejected if the plan changed between approval and
  execution. The runtime half of "the executed action equals the proven one".
  ([exec-approvals](https://docs.openclaw.ai/tools/exec-approvals))
- Collapsed tool rows, the session rail with a utility-model digest that expands only
  on exception, a read-only companion side chat.
- Skill Workshop (§2C).

**Avoid**
- Three hook systems with different blocking semantics; a typed name registered
  through the wrong API only warns; "the catalog is the registration API, not a promise
  that every runtime emits every hook".
- Authority split across code paths (CVE-2026-33579: a plugin command path skipped the
  scope check the RPC path had).
- Surface sprawl and drift (40+ channels, ~300 doc pages, five names in three months,
  a protocol reference that omits shipped RPCs; a 1.2 GB install, 13 s CLI startup).
- Sandbox off by default; native plugins in-process; opt-in mitigations; $560-weekend
  reports.
- Rewind that reverts chat context only while worktree snapshots exist unlinked.
- Safety semantics that differ between stable and `main` with no changelog line an
  operator can find: fail-closed hook timeouts and deny-on-unresolved approvals are
  `main`-only while stable allows through.

### 3.12 k9s, lazygit, lazydocker

**Steal**
- Header mnemonic menu scoped to the current view, `?` for the full list, in-scope
  plugins in the same menu. **[qualified]**: `--readonly` disables built-in mutating
  commands but skips a plugin only when it sets `dangerous: true`; enforcement is per
  view (v0.51.0 had to fix XRay ignoring it, #4026). ([k9s](https://github.com/derailed/k9s))
- `:` commands with history and `-` for last; `/` filters with `/!`, `/-l`, `/-f`.
  ([commands](https://k9scli.io/topics/commands/))
- lazygit's command log panel (`@`): the exact underlying commands. daisugi's version
  shows the envelope clause, the SMT query and the verdict for every gated action.
  ([v0.28](https://github.com/jesseduffield/lazygit/releases/tag/v0.28))
- `--readonly` plus plugin flags `confirm` and `dangerous`; delete needs TAB then
  ENTER.
- Undo by re-reading the substrate's own log (git reflog), with explicit refusal when
  the log lacks detail (any in-progress rebase, merge, cherry-pick or revert), and a
  confirmation before every undo. **[confirmed]** ([Undoing.md](https://github.com/jesseduffield/lazygit/blob/master/docs/Undoing.md))
- Declarative custom commands with a context, typed prompts, output modes, and the
  echo+popup debug trick; keyless commands live in the `?` menu. **[confirmed]**
  ([Custom_Command_Keybindings.md](https://github.com/jesseduffield/lazygit/blob/master/docs/Custom_Command_Keybindings.md))
- `jumps.yaml`: declarative drill links between related record types.

**Avoid**
- Timed full repaint (refreshRate ≥ 2 s) on top of watches: 50+ endpoints pegged the
  CPU (#2681); v0.51.0 had to add "skip reconcile when informer data unchanged"
  **[confirmed]**; redraw on events and diff instead.
- Moving a habitual confirmation key (k9s changed TAB+ENTER to ENTER and reverted,
  #3591): fix allow/deny keys early and never move them.
- Growing the core with side features (benchmarks, sanitizer, image scans).
- Text the operator cannot select and copy: verdicts, counterexamples and pathway ids
  must be copyable (k9s added OSC52 for this).
- Undo with silent gaps.

### 3.13 htop, btop, zellij, tmux

**Steal**
- A persistent mode ribbon plus a hint line generated from the live keymap, with a
  compact bar + tooltip key for experts. ([status-bar source](https://raw.githubusercontent.com/zellij-org/zellij/main/default-plugins/status-bar/src/main.rs))
- *Proposed but not yet run* as a first-class pane state (`Press ENTER to run...`,
  `--start-suspended`, exit code with ENTER to re-run / Ctrl-c to close).
  **[qualified]**: by default only the layout and each pane's command are serialized
  (every 1 s); viewport and scrollback are opt-in.
  ([session-resurrection](https://zellij.dev/documentation/session-resurrection.html))
- Stacked panes as a list of one-line titles with one expanded, and swap layouts with
  `min_panes`/`max_panes` constraints that re-tile as panes come and go.
  ([swap-layouts](https://zellij.dev/documentation/swap-layouts.html))
- A machine-facing control plane beside the human UI: `list-panes --json` (integer
  `id` + `is_plugin`; the CLI-facing form is `terminal_N`/`plugin_N`), `--pane-id`
  targeting on ~28 actions so supervisors never steal focus, `subscribe --format json`
  NDJSON with `is_initial`, `run --block-until-exit-success`. **[qualified]**: subscribe
  sends the whole changed viewport, not line deltas.
  ([programmatic-control](https://zellij.dev/documentation/programmatic-control.html))
- Tag-then-verb over a tree (tmux `choose-tree`; htop tagging + F9).
- Named screens over one live table, persisted as a few `key=value` lines (htop).
- tmux control-mode discipline: framed replies, per-pane output, client backpressure,
  rate-limited subscriptions. **[qualified]**: with `pause-after` set, output arrives
  as `%extended-output` with an age field; `set-hook -B` / `wait-for -E` are unreleased
  (3.8 master). ([Control-Mode](https://github.com/tmux/tmux/wiki/Control-Mode))
- WASM plugins gated by a permission prompt (one prompt for the whole requested set,
  not per capability) with `InterceptInput`, `ReadPaneContents` and `CommandChanged`
  events. **[qualified]**

**Avoid**
- Global single-modifier chords that collide with the programs inside (zellij #1399
  open since 2022; a second preset and a locked mode were needed).
- Chrome the operator cannot remove (two status lines, frames, a brand mark; htop
  needed `--no-function-bar` years later).
- Persistence hung on a UI side effect (tmux-continuum autosaves via `status-right`).
- "Resurrection" that replays command lines instead of restoring state.
- Apps that rewrite their own config on every UI action (btop) or forbid hand edits
  (htop): keep a declarative config and a separate ephemeral state file.

### 3.14 Factorio and Dwarf Fortress

**Steal**
- Three-tier interrupt taxonomy with per-class policy in a text file
  (`[CITIZEN_DEATH:A_D:D_D:UCR_A:ALERT]`; Popup / Pause / Recenter / Alert columns).
  **[confirmed]** (v50+; the classic line has no ALERT flag)
  ([announcements.txt](https://dwarffortresswiki.org/index.php/Announcements.txt))
- Alerts aggregated by class with a count and a fixed lifetime, each clickable to
  zoom to the cause; chatty classes off by default. ([Alerts](https://wiki.factorio.com/Alerts))
- Honest staleness: chunks outside radar coverage drawn as "a dim image that cannot
  be zoomed into" (covered chunks pulse about once a second); rounded stock counts
  shown as `20?`. **[confirmed]** ([Radar](https://wiki.factorio.com/Radar), [Stocks](https://dwarffortresswiki.org/index.php/Stocks))
- The production-statistics shape: production vs consumption side by side, one
  timescale ladder (5 s to 1000 h and all-time), per-surface vs global, click to
  filter, derived graphs added because raw series did not answer the question.
  ([FFF-408](https://www.factorio.com/blog/post/fff-408))
- Ghost-first execution with visible assignment state; the Bottleneck mod's
  green/yellow/red lights per machine, throttled under load.
- Undo with disclosure: the tooltip names exactly what will be undone and on which
  surface; a flying text confirms; undoing something older than one minute (the
  shipped 2.0.7 threshold; the pre-release post said "a few minutes") gets a
  confirmation dialog with a preview. **[qualified]**
  ([FFF-412](https://www.factorio.com/blog/post/fff-412))
- Parametrised blueprints as reusable templates with typed holes. ([FFF-392](https://www.factorio.com/blog/post/fff-392))

**Avoid**
- Per-screen key vocabularies with no discoverability (classic DF): the same verb must
  mean the same thing on every panel.
- Alert floods ("don't get ambitious and change all the combat events to Alert").
- Forward-only history (Factorio replays cannot skip or rewind and break across
  versions): store snapshots with parent ids, not only an event tape.
- The best fleet view living in a memory-scraping third party (Dwarf Therapist) that
  breaks on every release: ship the matrix in-product over a documented store.
- Feeding the model the human's dense view (the Factorio Learning Environment moved to
  sparse encoding because ASCII dumps were "problematic due to tokenization").

### 3.15 Multi-agent supervision UIs

Claude Squad, Conductor, Crystal (deprecated Feb 2026), Vibe Kanban (sunset
2026-04-10), Warp, Claude Code teams/agent view, Codex app.

**Steal**
- Triage list grouped by state with the blocked sessions on top and a peek pane that
  answers without attaching (§2D). **[qualified]**
- Veto at lifecycle boundaries with exit 2: `PreToolUse`, `TaskCreated` (rolls back),
  `TaskCompleted` (refuse to mark done), `TeammateIdle` (send feedback, keep working).
  **[qualified]**: the `PermissionRequest` hook cannot block, and the lead grants
  teammate *plan* approvals autonomously; teams are experimental and opt-in.
- Conductor: per-turn snapshot into a private git ref; a fork carries file state plus
  a chat summary (honest about what it carries). ([checkpoints](https://www.conductor.build/docs/reference/checkpoints))
- Prefix-sharing forks and the fan-out stagger (v2.1.229+; matching on model, effort,
  agent type, tools, output schema and cwd). **[qualified]**: a fork's first request
  reads the parent's cache, its later turns sit in the 5-minute bucket.
- Consent cannot be relayed. **[qualified]** (enforced by a classifier in auto mode)
- A drillable budget tree (phase → agent → prompt / recent calls / result, tokens per
  node) with an advisory banner and one-key stop.
- Scriptable dispatch and steering from outside the UI (`codex queue --thread X
  --message`, `claude --bg`, `claude agents --json`).

**Avoid**
- One terminal pane per agent as the default view (Claude Code moved the default to
  in-process in v2.1.179; panes are unsupported in several terminals and orphan tmux
  sessions).
- An auto-yes daemon that taps Enter on every prompt (Claude Squad `-y`; Warp's "Run
  Until Completion" bypassing the denylist by default).
- Betting on humans reviewing a fleet they cannot review (Vibe Kanban shut down:
  "couldn't find a business model"; "surprised people are running multiple agents,
  and are able to check their outputs diligently").
- A rewind that looks total but is partial.
- Over-broad OAuth scopes and default-on telemetry in a supervision tool.

---

## 4. The daisugi cockpit: gate + garden

### 4.1 Shape

One instrument, three views over one store, cycled with Tab (htop screens), each
with a `:` command line and a `/` filter (k9s), a footer of hints generated from the
live bindings (zellij) and `?` for the full list.

Terms. A **session** is one agent's thread of work: one transcript file, one cached
prompt prefix, one workspace state, one row on screen. A fork makes a new session.
The **multi-session view** is the live screen that shows every session's current
action and verdict. The three views:

- **MULTI-SESSION VIEW** (live): the roster of gated agents, the peek pane, the verdict log.
- **TREE** (one session): the prompt tree, rewind, fork, replay, checkpoints.
- **GARDEN** (over time): pathways growing, reuse, tokens saved.

The wiring/config screen the current TUI leads with becomes `:wiring`, reachable, not
default. The pure-stdlib `daisugi dashboard` stays the infant tier.

### 4.2 The multi-session view

```
 daisugi · sessions         gate: ENFORCE   cache 96% hit   fleet 7 agents   ?=keys
 ─────────────────────────────────────────────────────────────────────────────────
 NEEDS YOU (2)
 ▶ sonnet-3  Bash `rm -rf build/`        DENY  env§shell.no_delete_outside_tmp   12s
   opus-1    Write src/gate.py           ASK   env§files.write ⊄ workspace/src    41s
 WORKING (4)
   haiku-2   Read docs/adr/0007…         ok    step 41/60  ↑2.1k ⟳118k ✎0        3s
   sonnet-5  Edit tests/test_gate.py     ok    step 12/60  ↑0.9k ⟳96k  ✎1.2k     1s
   …
 PARKED (1)                                        cache cold in 3m40s · keep-warm on
   sonnet-4  (awaiting sub-agent)                                                 6m
 ─────────────────────────────────────────────────────────────────────────────────
 sonnet-3 · proposes Bash                                            [peek]
   rm -rf build/
   verdict DENY · clause shell.no_delete_outside_tmp · proof z3#a91f (0.4s)
   counterexample: path "build/" ∉ {/tmp/**, workspace/build/**}
   a allow (TAB+ENTER)  d deny  e edit input  r remember…  s steer  t tree  ⏎ attach
 ─────────────────────────────────────────────────────────────────────────────────
 ALERTS  3× denied shell.no_delete (sonnet-3, opus-1, haiku-6)   1× budget 90%
 LOG  12:01:07 sonnet-5 Edit allowed files.write §2   12:01:09 haiku-2 Read allowed …
```

Rules the multi-session view follows:

- **Rows are grouped by what the operator must do** (NEEDS YOU, WORKING, PARKED,
  DONE, DEAD), never by agent id. Each row is one line: agent, proposed action,
  verdict + clause, step budget, the four token buckets (fresh ↑, cache read ⟳,
  cache write ✎), time since last request.
- **The peek pane shows the proposal, the verdict, the envelope clause, the proof id
  and the counterexample.** This is lazygit's command log for a verifier: the operator
  sees *why* in checkable form, not a model's opinion.
- **Allow is two-step (TAB+ENTER) and the keys never move.** `e` edits the input
  (`updatedInput`); `r` remembers with a scope menu (once / session / project / global,
  proposed as an envelope edit, never auto-applied); `s` steers (delivered at the next
  tool boundary); `t` jumps to the session's tree; Enter attaches.
- **Tag then verb.** `Space` tags rows, `c` tags a parent and its sub-agents, then
  `d`/`a`/`b` (budget) act on the set. `/` filters: `/denied`, `/!allowed`,
  `/-l pathway=…`, `/-f fuzzy`.
- **Alerts by class with counts**, clickable to the causing row; the per-class policy
  (log / sound / pause+zoom / modal) lives in a versioned file; denials of destructive
  classes and budget breaches pause; everything else scrolls.
- **Cache state is a column.** Time since last request, expected warm/cold, and the
  keep-warm status for parked sessions. A cache-miss counter in the header; a miss on a
  session that should have hit is an alert, not a log line.
- **Redraw on journal events, diffed in place.** No timer repaint, no reflow under the
  eye.
- **Everything is copyable** (verdict text, counterexample, ids).

### 4.3 The Tree

**Store.** One append-only JSONL per session at `<data_dir>/sessions/<session-id>.jsonl`.

```
{"type":"session","v":1,"id":"…","cwd":"…","harness":"claude-code|codex|sprig|…",
 "harnessSessionId":"…","parentSession":"…","parentEntry":"…","cacheKey":"…"}
{"type":"prompt",      "id":"8hex","parentId":"…","ts":"…","text":"…"}
{"type":"assistant",   "id":"…","parentId":"…","usage":{"fresh":…,"cacheRead":…,"cacheWrite":…,"out":…},"model":"…"}
{"type":"tool_call",   "id":"…","parentId":"…","toolUseId":"…","name":"…","input":{…}}
{"type":"verdict",     "id":"…","parentId":"…","toolUseId":"…","decision":"allow|deny|ask|edit",
                        "clause":"…","proofId":"…","counterexample":"…","budget":{…},"latencyMs":…}
{"type":"tool_result", "id":"…","parentId":"…","toolUseId":"…","ref":"tool-results/…"}
{"type":"checkpoint",  "id":"…","parentId":"…","workspace":{"kind":"gitref","ref":"refs/daisugi/checkpoints/<session>/<id>"},"covers":["…"],"skipped":["…"]}
{"type":"compaction",  "id":"…","parentId":"…","summary":"…","retainedTail":[…],"tokensBefore":…}
{"type":"branch_summary","id":"…","parentId":"…","fromId":"…","summary":"…"}
{"type":"label",       "id":"…","parentId":"…","target":"…","label":"…"}
{"type":"head",        "leafId":"…"}
```

Decisions baked in, each traceable to §2-§3:

- `id`/`parentId` on every entry; `head` persisted (pi's gap).
- Verdicts are entries on the same tree, keyed by `toolUseId` (the join key that the
  Claude Code hook payload already carries), so replaying a branch replays the
  envelope that was in force (Codex `turn_context`).
- Checkpoints carry `covers` and `skipped` so the rewind menu can say what it will not
  restore (Claude Code's "Restored the code, but skipped N files").
- Workspace snapshots are private refs in the user's own repo
  (`refs/daisugi/checkpoints/<session>/<id>`, stash-shaped with an untracked parent,
  restore inside a rollback ref), never a shadow repo, never `.git`-visible refs, never
  per tool call in big trees (Cline v4; Codex's 100 GB lesson).
- Compaction is a self-contained checkpoint (`summary` + `retainedTail`); the standing
  envelope is re-injected after every compaction boundary.
- Fork = copy the active path into a new session file with `parentSession`/`parentEntry`;
  inherit `cacheKey`. Rewind = move `head`, and optionally restore the workspace, as
  two explicit axes. Nothing is ever deleted.
- Cross-session messages are entries marked not-from-user and can never carry a verdict.

**Keys (in TREE view).** `Esc Esc` opens the tree from the prompt; ↑/↓ move, ←/→ fold
branch segments, Ctrl+O cycles filters (default → no tools → prompts only → labeled
→ verdicts only → all), `L` label, `Enter` select. Selecting a prompt puts its text
back in the editor (edit-and-resend = new branch). Selecting anything else opens the
rewind menu: *Restore conversation / Restore workspace / Restore both / Summarize from
here / Summarize up to here / Fork here / Never mind*, with workspace rows hidden when
the checkpoint has no tracked edits, and a preview of what will be dropped when the
target is older than a few minutes (Factorio). Leaving a branch asks *No summary /
Summarize / Custom*.

**What each harness path can do.** This is the constraint from
`harness-comparison.md`, made explicit:

| Capability | E · direct API (sprig owns the loop) | D · hook-gated Claude Code | C · MCP bridge |
|---|---|---|---|
| daisugi owns the prefix and the tree | Yes: pi-model store, full `/tree` | No: Claude owns it; daisugi keeps an **index** over Claude's `sessionId`/`parentUuid` and its own verdict entries joined by `tool_use_id` | Same as D |
| Rewind in place | Yes | Operator's `Esc Esc` in Claude; daisugi observes the new head via the next hook payload | Same |
| Fork | Copy the session file | Drive `claude --resume <id> --fork-session` (Gas Town seance) and register the child session with `parentSession` | Same |
| Workspace checkpoint | daisugi ref per allowed effect | daisugi ref at each hook-observed prompt boundary (covers more than Claude's own: bash effects included) | Same |
| Keep-warm for parked sessions | Yes (max_tokens=1 ping on the cacheable prefix) | Not from daisugi (subscription path); rely on the 1-hour main-conversation TTL and show the clock | Same |
| Cache counters | From usage fields | From the transcript file (`message.usage`, as `gt costs` does) | Same |

### 4.4 The Garden

The garden is a fold over the append-only journal, never a poll of snapshots, with
loud truncation if history is ever pruned (Beads).

- **Production-statistics shape.** Per pathway and for the fleet: reuse (hits) vs
  distillations (new pathways) side by side; one timescale ladder (1 h, 1 d, 1 w,
  1 mo, all); per-session vs global; click a pathway to filter; derived series that
  answer the real questions: *net tokens after cache reads*, *denials per 1k steps*,
  *pathway hit rate*, *tokens per allowed effect*.
- **Lifecycle, with review.** pending (distilled, not yet active) → active → stale
  (unused 30 d) → archived (90 d); pin; overlap detection; a snapshot before every
  curator pass and a per-mutation ledger with single-entry rollback (Hermes Curator,
  OpenClaw Workshop). Agent-drafted pathways are always pending.
- **Feedback per reuse.** Each reuse row can be marked Useful / Misleading (Devin),
  which is what lets pathways be pruned rather than only counted.
- **Parametrised pathways** shown as templates with typed holes; reuse is a visible
  fill-in step (Factorio; ADR-0008).
- **Honest glyphs.** Estimated numbers carry `?`; last-known numbers are dim; every
  count states its source (provider usage field vs journal fold).

### 4.5 Cache discipline, applied

1. The envelope, the pathway index (names + one-liners only) and the tool catalog
   sit in the stable prefix, in a fixed order, byte-stable across a session.
2. Per-step gate output goes in the tool result (deny + clause + counterexample) or
   an appended tail block; never in the system prompt.
3. Pathway bodies load on demand from files (Cursor, Cline, Hermes).
4. `cacheKey` is pinned per session root; forks inherit; isolated sub-agents get their
   own; on path E the summarization request uses a fresh routing id with caching
   disabled (pi 0.82.0).
5. A regression test asserts the static block is byte-identical across sessions
   (OpenHands); a cache-miss floor test runs in CI (OpenClaw); a miss on a session that
   should have hit is an alert.
6. Deny with `terminate` when a whole batch is blocked, so no follow-up turn is paid.
7. Compaction is shown as an event with pre/post tokens (never hidden).

### 4.6 Extension surface

- One hook contract, one allow function, fail-closed inside the hook (exit 2 on any
  internal error; internal deadline ≪ host timeout); every surface (hook, MCP bridge,
  CLI, chat) routes through it, with a test; unknown hook names are refused; CI checks
  every declared hook has a dispatch site.
- Pathways are `SKILL.md`-shaped (Agent Skills) so they are portable to every harness
  in §2C; daisugi adds the parameters block and the envelope it was proven under.
- daisugi presents itself to the agent as a tiny primer on `SessionStart` (which
  re-fires after compaction) plus a CLI on demand, not as a fat MCP surface.
- "Approve and remember" produces a proposed envelope edit with a scope and an
  expiry; proposals are applied explicitly; destructive classes are never proposed.

### 4.7 Mapping onto what exists

| Today | Change |
|---|---|
| `src/opendaisugi/tui.py` leads with `detect_stages` wiring + swap knobs | Wiring becomes `:wiring`; the default screen is the multi-session view over hook captures (`hook.py` `record_call`/`list_sessions`) and the gateway journal (W8) |
| `journal.py`: one YAML trace per run, no links between runs | Add the session store (§4.3) beside it; traces stay as replayable bodies; `TraceRecord` gains `session`/`entry` ids |
| `gateway_journal.py` already counts `cache_read_tokens`/`cache_creation_tokens` per turn | Surface hit rate on the multi-session view header and per session; alert on misses that should have hit |
| `parsers/claude_code.py` flattens `parentUuid` away | Keep it: index `uuid`/`parentUuid`/`sessionId` so path-D sessions can render Claude's own tree and `gt seance`-style forks |
| `hook.py` payload → `_payload_to_record` | Record `tool_use_id`, `agent_id`, `agent_type`, `session_id` on every verdict entry (the join keys) |
| `docs/harness/harness-comparison.md` paths C/D/E | §4.3's capability matrix is the honest label per path; ship E (sprig) as the full-tree session first, D as the indexed session |
| Plan W5 (visible mode), W6 (every error teaches), W7 (honest markers) | The multi-session view header echoes `gate: ENFORCE`; every deny shows clause + next action; `[live]/[cfg]/[planned]` survive in `:wiring` |

Sequence: (1) session store + multi-session view over path D captures (no new harness work); (2) full
tree on path E through sprig, indexed tree on path D; (3) Garden fold + lifecycle.

---

## 5. The avoid list

1. **The settings-panel trap**: leading with config instead of live work (daisugi's
   own first use; Cursor's Agents Window hiding the bill).
2. **A tree the UI cannot reach** (Claude Code) or a fork with no parent link (Cline
   restores, Cursor forks, Claude Code copies).
3. **Rewind on one axis only**: chat without files (Codex, OpenClaw, Aider's inverse),
   or forks that share the working directory (OpenHands).
4. **Snapshots that mutate the user's VCS** (Codex ghost commits, Cline's
   `.git_disabled`) or that are taken per tool call in big trees.
5. **Undo that overclaims** (Claude Code's bash/sub-agent/symlink gaps; lazygit's
   mid-rebase). Say what cannot be rewound.
6. **Hooks that fail open** on timeout, crash, or bad JSON (Claude Code, Cursor,
   Devin, OpenHands prompt hooks, Hermes shell hooks).
7. **A second path to allow** that skips the check (OpenClaw CVE-2026-33579); an
   auto-yes daemon (Claude Squad `-y`, Warp "Run Until Completion"); YOLO by default
   (pi, oh-my-pi, Cline CLI `--auto-approve` default true, Warp).
8. **An LLM as the guardrail** (Cursor auto-review, Hermes `smart`, Codex Guardian
   with no human fallback, OpenHands' model-supplied `security_risk`).
9. **Dynamic values at the head of the prefix** (pi's timestamp, Cline's date and
   HEAD hash); **per-step tool-schema swaps or reordering**; **breakpoints on the
   volatile last turn only** (Cline); **a cache key you cannot pin** (Codex).
10. **Hiding cache and compaction**: stats only under `--no-stream` (Aider), compaction
    removed from scrollback (Devin), no per-request cost in the editor (Cursor).
11. **Estimating "tokens saved"** instead of reading usage counters.
12. **Every supervisor an LLM loop** (Gas Town); **LLM-manager fan-out as the default**
    (Devin, Cursor N×, Claude teams 7×); **blocking delegation** (OpenHands).
13. **The metaphor tax** (Gas Town) and per-screen key vocabularies (classic DF).
14. **Unattended default work the operator did not opt into** (Gas Town #3649);
    **default bloat in the prefix** (Hermes >10k tokens of skill listings).
15. **One tmux pane per agent as the default view**; **chrome that cannot be removed**;
    **global Ctrl chords that collide with hosted programs** (zellij #1399).
16. **Timer repaints** (k9s) and **moving a confirmation key** (k9s #3591).
17. **Alert floods and over-interrupting** (DF combat alerts, Factorio turret fire).
18. **Config the app rewrites** (btop) or forbids editing (htop); **XDG refused** (pi).
19. **Three hook systems with different semantics** (OpenClaw); **safety semantics
    that differ between stable and `main`** (OpenClaw's fail-closed timeouts and
    deny-on-unresolved are `main`-only); **consent that does not track the script**
    (Hermes shell hooks); **docs that drift from code** (Cline checkpoints, OpenHands
    condenser, Codex protocol page, Devin web checkpoints, Hermes schema version and
    breakpoint layout).
20. **Surface sprawl** (Hermes 11.5k files, OpenClaw 83k commits, daisugi's own 66
    commands): the assurance core stays small and reviewable.

---

## 6. Not established

- pi's SDK exposure of leaf navigation to an external cockpit (RPC has only
  fork/clone/get_tree/get_entries; an extension command is the workaround).
- Whether `/branch` or `--fork-session` copies Claude Code's checkpoint references so
  code restore works inside a branch; the number of `cache_control` breakpoints Claude
  Code sets.
- The measured prefill penalty per gate pause in daisugi's own traces (TraceLab's gap
  mechanism is confirmed; the "<5 minutes or batch approvals" rule is a hypothesis to
  measure with `gateway_journal`).
- The cheapest keep-warm on the subscription path (no `max_tokens=1` ping is possible
  through `claude -p`).
- Whether Hermes' post-hoc step-through and OpenClaw's rewind are in a stable release
  or only on `main`.
- The orchestration token overhead of Claude Code agent teams beyond the docs' "~7x in
  plan mode"; no cockpit publishes measured numbers.

---

## 7. Sources

Primary docs and source code, grouped. The per-tool researcher files list ~660 URLs;
these are the ones the claims above rest on.

**Prompt-cache economics.** [TraceLab, arXiv 2606.30560](https://arxiv.org/abs/2606.30560) ·
[Lumer et al., arXiv 2601.06007](https://arxiv.org/abs/2601.06007) ·
[Anthropic prompt caching](https://platform.claude.com/docs/en/docs/build-with-claude/prompt-caching) ·
[OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching) ·
[Manus: context engineering](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus) ·
[Anthropic: effective context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents) ·
[Claude Code: prompt caching is everything](https://claude.com/blog/lessons-from-building-claude-code-prompt-caching-is-everything)

**Claude Code.** [checkpointing](https://code.claude.com/docs/en/checkpointing) · [sessions](https://code.claude.com/docs/en/sessions) · [prompt-caching](https://code.claude.com/docs/en/prompt-caching) · [hooks](https://code.claude.com/docs/en/hooks) · [sub-agents](https://code.claude.com/docs/en/sub-agents) · [agent-teams](https://code.claude.com/docs/en/agent-teams) · [workflows](https://code.claude.com/docs/en/workflows) · [agent-view](https://code.claude.com/docs/en/agent-view) · [cross-session-messaging](https://code.claude.com/docs/en/cross-session-messaging) · [costs](https://code.claude.com/docs/en/costs) · [claude-directory](https://code.claude.com/docs/en/claude-directory) · issues [#55347](https://github.com/anthropics/claude-code/issues/55347), [#24304](https://github.com/anthropics/claude-code/issues/24304), [#32175](https://github.com/anthropics/claude-code/issues/32175), [#85008](https://github.com/anthropics/claude-code/issues/85008)

**Codex.** [protocol.rs](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs) · [app-server README](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md) · [v2 thread.rs](https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/src/protocol/v2/thread.rs) · [client.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs) · [rollout-trace](https://github.com/openai/codex/blob/main/codex-rs/rollout-trace/README.md) · [hooks docs](https://learn.chatgpt.com/docs/hooks) · [approvals & security](https://learn.chatgpt.com/docs/agent-approvals-security) · PRs [#19481](https://github.com/openai/codex/pull/19481), issues [#29388](https://github.com/openai/codex/issues/29388), [#35300](https://github.com/openai/codex/issues/35300), [#21975](https://github.com/openai/codex/issues/21975), [#37421](https://github.com/openai/codex/issues/37421), [#11626](https://github.com/openai/codex/issues/11626)

**Cursor.** [checkpoints](https://cursor.com/docs/agent/chat/checkpoints) · [side chats](https://cursor.com/help/ai-features/side-chats) · [hooks](https://cursor.com/docs/agent/hooks) · [run modes](https://cursor.com/docs/agent/security/run-modes) · [dynamic context discovery](https://cursor.com/blog/dynamic-context-discovery) · [self-summarization](https://cursor.com/blog/self-summarization) · [cloud agent capabilities](https://cursor.com/docs/cloud-agent/capabilities) · [cursaves storage doc](https://github.com/Callum-Ward/cursaves/blob/main/docs/how-cursor-stores-chats.md) · forum [30 GB store](https://forum.cursor.com/t/state-vscdb-grows-to-30gb-due-to-bubbleid-agentkv-entries/167641), [worktrees](https://forum.cursor.com/t/cursor-3-worktrees-best-of-n/156507)

**Aider.** [caching](https://aider.chat/docs/usage/caching.html) · [repomap](https://aider.chat/docs/repomap.html) · [commands](https://aider.chat/docs/usage/commands.html) · [commands.py](https://raw.githubusercontent.com/Aider-AI/aider/main/aider/commands.py) · [chat_chunks.py](https://raw.githubusercontent.com/Aider-AI/aider/main/aider/coders/chat_chunks.py) · [architect blog](https://aider.chat/2024/09/26/architect.html) · PR [#3937](https://github.com/Aider-AI/aider/pull/3937) · issues [#3607](https://github.com/Aider-AI/aider/issues/3607), [#4676](https://github.com/aider-ai/aider/issues/4676)

**Cline.** [checkpoints](https://docs.cline.bot/core-workflows/checkpoints) · [auto-approve](https://docs.cline.bot/features/auto-approve) · [checkpoint-hooks.ts](https://github.com/cline/cline/blob/main/sdk/packages/core/src/hooks/checkpoint-hooks.ts) · [ai-sdk.ts](https://github.com/cline/cline/blob/main/sdk/packages/llms/src/providers/ai-sdk.ts) · [cline.ts prompt](https://github.com/cline/cline/blob/main/sdk/packages/shared/src/prompt/cline.ts) · [hooks blog](https://cline.bot/blog/cline-v3-36-hooks) · [permission handling](https://docs.cline.bot/sdk/guides/permission-handling) · [OTel events](https://docs.cline.bot/enterprise-solutions/monitoring/opentelemetry-events) · [prompt-cache audit](https://github.com/OnlyTerp/prompt-cache-skills) · issue [#4385](https://github.com/cline/cline/issues/4385)

**OpenHands.** [PR #3922](https://github.com/OpenHands/software-agent-sdk/pull/3922) · [PR #3923](https://github.com/OpenHands/software-agent-sdk/pull/3923) · [llm.py](https://github.com/OpenHands/software-agent-sdk/blob/main/openhands-sdk/openhands/sdk/llm/llm.py) · [cross-conversation cache test](https://github.com/OpenHands/software-agent-sdk/blob/main/tests/sdk/llm/test_prompt_caching_cross_conversation.py) · [condenser](https://docs.openhands.dev/sdk/arch/condenser) · [security](https://docs.openhands.dev/sdk/guides/security) · [hooks](https://docs.openhands.dev/sdk/guides/hooks) · [skills](https://docs.openhands.dev/sdk/arch/skill) · [Canvas conversations](https://docs.openhands.dev/openhands/usage/agent-canvas/conversations) · issues [#2047](https://github.com/OpenHands/software-agent-sdk/issues/2047), [#11309](https://github.com/OpenHands/OpenHands/issues/11309)

**Devin.** [CLI commands](https://docs.devin.ai/cli/reference/commands) · [lifecycle hooks](https://docs.devin.ai/cli/extensibility/hooks/lifecycle-hooks) · [permissions](https://docs.devin.ai/cli/reference/permissions) · [Devin MCP](https://docs.devin.ai/work-with-devin/devin-mcp) · [session insights](https://docs.devin.ai/product-guides/session-insights) · [Fusion](https://cognition.com/blog/devin-fusion) · [Multi-agents: what's working](https://cognition.com/blog/multi-agents-working) · [SWE-grep](https://cognition.com/blog/swe-grep) · [blockdiff](https://cognition.com/blog/blockdiff) · HN [Answer.AI month with Devin](https://news.ycombinator.com/item?id=42734681)

**pi / oh-my-pi.** [session-format.md](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/session-format.md) · [extensions.md](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md) · [rpc.md](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md) · [sessions docs](https://pi.dev/docs/latest/sessions) · [CHANGELOG](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/CHANGELOG.md) · [anthropic-messages.ts](https://github.com/earendil-works/pi/blob/main/packages/ai/src/api/anthropic-messages.ts) · [harness.md](https://github.com/earendil-works/pi/blob/main/packages/agent/docs/harness.md) · [issue #1873](https://github.com/badlogic/pi-mono/issues/1873) · [pi-rewind](https://pi.dev/packages/@ayulab/pi-rewind) · [pi-cache-optimizer](https://pi.dev/packages/pi-cache-optimizer) · [Zechner: minimal coding agent](https://mariozechner.at/posts/2025-11-30-pi-coding-agent/) · oh-my-pi [tree.md](https://github.com/can1357/oh-my-pi/blob/main/docs/tree.md), [session.md](https://github.com/can1357/oh-my-pi/blob/main/docs/session.md), [agent-hub.md](https://github.com/can1357/oh-my-pi/blob/main/docs/agent-hub.md), [rewind.md](https://github.com/can1357/oh-my-pi/blob/main/docs/tools/rewind.md) · HN [XDG thread](https://news.ycombinator.com/item?id=49328206)

**Gas Town / Beads.** [gastown README](https://github.com/steveyegge/gastown) · [CHANGELOG](https://raw.githubusercontent.com/steveyegge/gastown/main/CHANGELOG.md) · [seance.go](https://raw.githubusercontent.com/steveyegge/gastown/main/internal/cmd/seance.go) · [costs.go](https://raw.githubusercontent.com/steveyegge/gastown/main/internal/cmd/costs.go) · [polecat lifecycle](https://raw.githubusercontent.com/steveyegge/gastown/main/docs/concepts/polecat-lifecycle.md) · [dolt storage](https://raw.githubusercontent.com/steveyegge/gastown/main/docs/design/dolt-storage.md) · [beads claude-code integration](https://raw.githubusercontent.com/steveyegge/beads/main/docs/integrations/claude-code.md) · [events journal](https://raw.githubusercontent.com/steveyegge/beads/main/docs/reference/events-journal.md) · [gates](https://raw.githubusercontent.com/steveyegge/beads/main/docs/workflows/gates.md) · [issue #3649](https://github.com/gastownhall/gastown/issues/3649) · [Yegge: Welcome to Gas Town](https://steve-yegge.medium.com/welcome-to-gas-town-4f25ee16dd04) · [Brinker: Gas Town Decoded](https://www.alilleybrinker.com/blog/gas-town-decoded/) · [Appleton](https://maggieappleton.com/gastown) · [Hartcher](https://simonhartcher.com/posts/2026-01-19-my-thoughts-on-gas-town-after-10000-hours-of-claude-code/)

**Hermes.** [slash commands](https://hermes-agent.nousresearch.com/docs/reference/slash-commands) · [session storage](https://hermes-agent.nousresearch.com/docs/developer-guide/session-storage) · [context compression and caching](https://hermes-agent.nousresearch.com/docs/developer-guide/context-compression-and-caching) · [prompt assembly](https://hermes-agent.nousresearch.com/docs/developer-guide/prompt-assembly) · [prompt_cache_scope.py](https://github.com/NousResearch/hermes-agent/blob/main/agent/prompt_cache_scope.py) · [hooks](https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks) · [RFC: lessons from Pi and OpenCode](https://github.com/NousResearch/hermes-agent/blob/main/docs/rfcs/2026-07-plugin-architecture-lessons-pi-opencode.md) · [checkpoints and rollback](https://hermes-agent.nousresearch.com/docs/user-guide/checkpoints-and-rollback) · [delegation](https://hermes-agent.nousresearch.com/docs/user-guide/features/delegation) · [curator](https://hermes-agent.nousresearch.com/docs/user-guide/features/curator) · [security](https://hermes-agent.nousresearch.com/docs/user-guide/security) · HN [migrate from OpenClaw](https://news.ycombinator.com/item?id=48586005)

**OpenClaw.** [control-ui](https://docs.openclaw.ai/web/control-ui) · [session management](https://docs.openclaw.ai/reference/session-management-compaction) · [prompt-caching](https://docs.openclaw.ai/reference/prompt-caching) · [plugin hooks](https://docs.openclaw.ai/plugins/hooks) · [exec approvals](https://docs.openclaw.ai/tools/exec-approvals) · [operator approvals design](https://docs.openclaw.ai/refactor/operator-approvals) · [gateway protocol](https://docs.openclaw.ai/gateway/protocol) · [subagents](https://docs.openclaw.ai/tools/subagents) · [skills](https://docs.openclaw.ai/tools/skills) · [sandboxing](https://docs.openclaw.ai/gateway/sandboxing) · HN [CVE-2026-33579](https://news.ycombinator.com/item?id=47628608)

**TUI craft.** [k9s](https://github.com/derailed/k9s) · [k9s commands](https://k9scli.io/topics/commands/) · [k9s v0.51.0](https://github.com/derailed/k9s/releases/tag/v0.51.0) · [lazygit Undoing](https://github.com/jesseduffield/lazygit/blob/master/docs/Undoing.md) · [lazygit custom commands](https://github.com/jesseduffield/lazygit/blob/master/docs/Custom_Command_Keybindings.md) · [lazygit v0.28](https://github.com/jesseduffield/lazygit/releases/tag/v0.28) · [bwplotka on lazygit](https://www.bwplotka.dev/2025/lazygit/) · [zellij status-bar source](https://raw.githubusercontent.com/zellij-org/zellij/main/default-plugins/status-bar/src/main.rs) · [zellij session resurrection](https://zellij.dev/documentation/session-resurrection.html) · [zellij programmatic control](https://zellij.dev/documentation/programmatic-control.html) · [zellij swap layouts](https://zellij.dev/documentation/swap-layouts.html) · [zellij FAQ](https://zellij.dev/documentation/faq.html) · [zellij #1399](https://github.com/zellij-org/zellij/issues/1399) · [tmux Control Mode](https://github.com/tmux/tmux/wiki/Control-Mode) · [tmux(1)](https://man.openbsd.org/tmux) · [htop(1)](https://man7.org/linux/man-pages/man1/htop.1.html) · [htop Settings.c](https://raw.githubusercontent.com/htop-dev/htop/main/Settings.c) · [btop README](https://github.com/aristocratos/btop/blob/main/README.md)

**Living views.** [Factorio production statistics](https://wiki.factorio.com/Production_statistics) · [Alerts](https://wiki.factorio.com/Alerts) · [Radar](https://wiki.factorio.com/Radar) · [Replay system](https://wiki.factorio.com/Replay_system) · [FFF-380 remote view](https://www.factorio.com/blog/post/fff-380) · [FFF-392 parametrised blueprints](https://www.factorio.com/blog/post/fff-392) · [FFF-408 statistics](https://www.factorio.com/blog/post/fff-408) · [FFF-412 undo/redo](https://www.factorio.com/blog/post/fff-412) · [Bottleneck mod](https://mods.factorio.com/mod/Bottleneck) · [DF Announcements.txt](https://dwarffortresswiki.org/index.php/Announcements.txt) · [DF Announcement](https://dwarffortresswiki.org/index.php/Announcement) · [DF Stocks](https://dwarffortresswiki.org/index.php/Stocks) · [DF Unit list](https://dwarffortresswiki.org/index.php/Unit_list) · [DF Save](https://dwarffortresswiki.org/index.php/Save) · [Dwarf Therapist](https://dwarffortresswiki.org/index.php/Dwarf_Therapist) · HN [DF interface](https://news.ycombinator.com/item?id=13592114) · HN [Factorio Learning Environment](https://news.ycombinator.com/item?id=43331582)

**Multi-agent UIs.** [Claude Squad](https://github.com/smtg-ai/claude-squad) · [Claude Squad daemon.go](https://github.com/smtg-ai/claude-squad/blob/main/daemon/daemon.go) · [Conductor checkpoints](https://www.conductor.build/docs/reference/checkpoints) · [Conductor workflow](https://www.conductor.build/docs/concepts/workflow) · [Crystal](https://github.com/stravu/crystal) · [Vibe Kanban](https://github.com/BloopAI/vibe-kanban) · [Vibe Kanban shutdown](https://www.vibekanban.com/blog/shutdown) · [Warp agent management](https://docs.warp.dev/platform/managing-cloud-agents/) · [Warp agent profiles](https://docs.warp.dev/agents/capabilities/agent-profiles-permissions/) · [Codex worktrees](https://learn.chatgpt.com/docs/environments/git-worktrees) · [Codex PR #39092 queue](https://github.com/openai/codex/pull/39092) · HN [agent teams](https://news.ycombinator.com/item?id=46902368), [Conductor](https://news.ycombinator.com/item?id=44594584), [Vibe Kanban](https://news.ycombinator.com/item?id=44533004)
