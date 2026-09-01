# Multi-agent management landscape, September 2026

Research input for the coppice porcelain design. The question: how does one
person watch and steer many coding agents, what shipped, what died, and what
shape should coppice's operator surface take.

Method: a fan-out research run on 2026-09-12. Five search angles, 24 sources
fetched, 117 claims extracted, the top 25 adversarially verified with three
votes each. 17 claims survived, 8 were refuted. Everything in the "Verified"
section carries a vote. Everything in the "Field notes" section was extracted
from a source but never verified. Treat it as a lead, not a fact.

## Coverage warning

The verified corpus is narrow. Nothing survived verification for Google (Jules,
Gemini CLI, Antigravity), Cursor, Amp, Warp, Zed's own multi-session UI,
Anthropic agent teams or Managed Agents, or for Gauntlet AI, Conductor,
Terragon, Claude Squad, Crystal, Herdr, Multica, Ralph loops, Paperclip,
Sprites, Superset, CCManager. The games and ops-tools section has one verified
data point. The swappable-frontend recommendation leans on one protocol, ACP.
The field notes below fill some of these gaps from unverified extractions.

## Verified findings

### 1. Anthropic agent view is a state-grouped roster, not tabs

Confidence high. Votes 3-0 and 3-0.

`claude agents` shipped 2026-05-11 in Claude Code v2.1.139 as a research
preview. It is one fullscreen terminal screen: a table grouped by state in the
order Pinned, Ready for review, Needs input, Working, Completed. One line per
session shows a state icon, a name, a generated status line, age, and an
optional PR number. A natural-language dispatch input sits at the bottom.
Ctrl+T pins a row. Ctrl+S toggles state grouping to directory grouping.

The attention queue is the Needs input group. The row shows the specific
question. Space peeks the question above a reply input without attaching.
Enter attaches. While the view is open, the terminal tab title reads
"2 awaiting input". Any foreground claude session shows "2 agents" in its
footer, refreshed about every ten seconds.

This is the closest shipped analogue to coppice's per-pane state model.

The companion claim that each row summary is Haiku-generated at a small per
session cost was refuted 1-2. The cost of the roster's legibility is not
established.

Unverified detail from the same docs: `claude agents --json` exposes `state`
(working, blocked, done, failed, stopped), `status` (busy, waiting, idle), and
`waitingFor` (permission prompt, input needed, sandbox request, worker request,
dialog open). Sessions run under a supervisor daemon that keeps them alive
after the terminal closes, each in a worktree under `.claude/worktrees/`.

Sources: https://code.claude.com/docs/en/agent-view ,
https://claude.com/blog/agent-view-in-claude-code

### 2. OpenAI calls the Codex app a command center for agents

Confidence medium. Vote 2-1.

The Codex desktop app, announced 2026-01-30, macOS 2026-02-02, Windows
2026-03-04, is positioned as "a command center for agents". OpenAI states that
existing IDEs and terminal tools "are not built to support" directing and
supervising agents at scale. This is positioning, not a description of the UI.
Two detailed claims about its data model (threads per project, one worktree
per agent, diff review in the thread) and its attention model (Automations
land in a review queue) were both refuted 0-3.

Source: https://openai.com/index/introducing-the-codex-app/

### 3. Devin Desktop is a kanban with a task container above the session

Confidence high. Votes 3-0 and 2-1.

Devin Desktop (the rebranded Windsurf, relaunched June 2026) manages agents
through a kanban. The Agent Command Center shows all local and cloud sessions
in columns by status. Task-level "Spaces" group sessions, PRs, files, and
context. Parallel sessions are worktree-backed and the worktree is created in
the background so the session opens at once (v3.5.17, 2026-07-17). Any number
of agent windows run side by side and the command center follows the selected
Space (v3.8.20, 2026-08-21).

The claim that attention is routed through native OS notifications and
bindable permission shortcuts was refuted 1-2.

Unverified: v3.10.23 (2026-09-10) adds ACP support and remote agents over
SSH, so the vendor IDE itself acts as a swappable frontend over an agent
protocol.

Sources: https://docs.devin.ai/desktop/changelog ,
https://docs.devin.ai/desktop/agent-command-center , https://devin.ai/desktop

### 4. Zed's Agent Client Protocol is the working swappable-frontend shape

Confidence high. Votes 3-0, 3-0, 3-0.

An editor supports any ACP agent without a per-agent integration. An agent
joins by being listed in the client's `agent_servers` settings as a subprocess
command with args. Factory Droid registers as command `npx`, args
`droid exec --output-format acp`. ACP is an output format of the existing CLI,
not a separate server. The local transport is JSON-RPC 2.0 over stdio,
newline-delimited UTF-8, the agent a child process of the client. Streamable
HTTP and WebSocket for remote agents are still a draft (RFD merged 2026-04-22).

Independent adoption: JetBrains co-launched the ACP registry in January 2026
listing Claude Code, Codex CLI, Copilot CLI, OpenCode, and Gemini CLI.
Community clients exist for VS Code, goose, OpenClaw, and the ai-sdk. Some
agents reach ACP through adapters. Remote registry agents have known bugs
(zed issue 47910).

Unverified detail: ACP is modelled on LSP, reuses MCP's JSON types where it
can, adds only coding-UX types such as diffs, and fixes Markdown as the
user-readable text format.

Sources: https://agentclientprotocol.com/protocol/transports ,
https://agentclientprotocol.com/protocol/overview ,
https://zed.dev/acp/agent/factory-droid , https://docs.factory.ai/integrations/zed ,
https://blog.jetbrains.com/ai/2026/01/acp-agent-registry/ ,
https://zed.dev/docs/ai/external-agents

### 5. Vibe Kanban had users and no business

Confidence high. Votes 3-0 and 3-0.

bloop, the company behind Vibe Kanban, shut down on 2026-04-10 with thousands
of daily users, about 30,000 monthly actives and 25,000 GitHub stars. The vast
majority were free users. The founders "couldn't find a business model that we
could get excited about". Hosted features (remote issues, comments, orgs) were
discontinued after 30 days and subscribers refunded. The local workspace
product continues as an Apache-2.0 community project on best-effort time.

The founder's own words: "everybody who is making money is selling to
enterprise and reselling tokens, and we were doing neither" and "no fun
playing for eighth place".

Unverified: the server-side layer that died was specifically the kanban board
and collaboration features. The worktree workspaces survived.

Sources: https://www.vibekanban.com/blog/shutdown ,
https://github.com/BloopAI/vibe-kanban ,
https://x.com/tokengobbler/status/2042647208135123078

### 6. OpenAI: three to five interactive sessions is the human ceiling

Confidence high. Votes 3-0 and 3-0.

OpenAI engineers report a person comfortably supervises three to five
interactive Codex sessions before context switching hurts: "we'd forget which
session was doing what". The bottleneck was human attention, not agent speed.
This is an anecdote from one team, not a measurement.

Their answer, Symphony (Apache-2.0, github.com/openai/symphony, about 27k
stars, announced 2026-04-27, "engineering preview"), abandons the session as
the unit of management and makes the issue tracker the control plane. Each
open Linear issue owns an agent workspace. The orchestrator keeps one agent
running per active task and restarts crashed or stalled ones (stall timeout
default 300000 ms). Blocked issues in the DAG do not start until unblocked.
Agents file their own follow-up issues. OpenAI says ambiguous problems still
need interactive sessions and it will not maintain Symphony as a product.

Corroborating ceilings from other sources: Kilo Code says engineers actively
steer two to four foreground agents. Mitchell Hashimoto says at most two.
Anthropic's agent-teams guidance says start with three to five. Gas Town's 20
to 30 agents are orchestrated, not interactively steered, so no contradiction.

The claim that OpenAI "explicitly abandoned rigid state-machine transitions"
was refuted 0-3. No vendor Bitter-Lesson verdict can be attributed here. The
500 percent PR figure in the post is unaudited.

Unverified: the reference implementation drives Codex through the App Server's
headless JSON-RPC API rather than a CLI or tmux. The first prototype was a
Codex session in tmux polling Linear. The stated cost of the ticket model is
that humans lose the ability to nudge agents mid-flight.

Sources: https://openai.com/index/open-source-codex-orchestration-symphony/ ,
https://github.com/openai/symphony ,
https://www.infoq.com/news/2026/05/openai-symphony/

### 7. AgentCraft is the one shipped RTS-style orchestrator

Confidence high. Votes 3-0 and 3-0.

AgentCraft is installable (npm `@idosal/agentcraft`, latest 0.5.1, first
published 2026-02-11, last 2026-08-21, live at app.agentcraft.gg). It supports
Claude Code, Codex, OpenCode, Cursor agent mode, and OpenClaw experimentally.
Its stated bet: "The interface that made managing 200 units possible is now
ready for AI". No independent evidence exists on whether the RTS attention
model works for users. No repository URL in npm metadata, so open-source
status is unverified.

This is the only games-inspired design from the brief's list that survived
verification, and only as a description of the bet, not an outcome.

Unverified: plan approvals and permission grants can be answered from a phone
through push notifications with quick reply, Telegram, and Discord. Agents are
isolated in Docker or Apple Containers with network isolation. Multi-machine
shared rooms put remote agents on one map.

Sources: https://www.getagentcraft.com/ ,
https://registry.npmjs.org/@idosal/agentcraft

### 8. lazybox: inbox rows that own a worktree and a live PTY

Confidence medium. Votes 3-0, 3-0, 3-0. Lowered because it is one tiny
project describing itself.

lazybox (github.com/AntoineToussaint/lazybox, MIT, created 2026-06-03, about
1,500 commits, 5 stars, pre-1.0, macOS daily driver) is a lazygit-style
terminal inbox. Each PR, issue, or CI-failure row owns a git worktree and an
embedded live PTY using a vendored ghostty VT parser, spawning Claude Code,
Codex, Cursor, or a shell.

Attention is an inbox, not a modal. Per-row agent-state badges (Working,
InputNeeded, Done) come from an explicit state machine fed by permission-footer
patterns and hooks. The `!` key jumps the cursor to the next agent awaiting
input. `Shift-F` jumps to failing CI. `v` multi-selects workspaces across repos
and `Shift-B` broadcasts one instruction to every selected agent.

Cross-agent messaging is an MCP server hosted by the daemon exposing
`list_sessions`, `read_session`, `post_note`, `read_notes`, `notify_session`,
so spawned sessions observe and instruct each other.

Its state model is nearly coppice's. Its MCP verb set is the ready-made shape
for coppice's model-operated layer.

The sibling project lazyagent (tmux-scraping dashboard with a five-value enum)
had both its claims refuted 0-3, so it is not a precedent.

Sources: https://github.com/AntoineToussaint/lazybox ,
https://github.com/AntoineToussaint/lazybox/blob/main/docs/features/terminals-and-agents.md

### 9. Synthesis: what the survivors share

Confidence medium. Derived from findings 1 to 8. Each element traces to at
least two independent products.

- A single roster grouped by whether the human is needed (agent view, lazybox).
- A container above the session as the unit of work: a Space, an issue, a PR
  row (Devin, Symphony, lazybox).
- One isolated worktree per unit (Devin, lazybox, Vibe Kanban).
- Jump-to-blocked keys or counts, not modal interrupts (agent view tab and
  footer counts, lazybox `!`).
- A natural-language dispatch input where new work starts (agent view).

None of the verified sources measured the cost of a model-operated control
layer. None verified a vendor deleting hand-built orchestration in favour of
model judgment. None reported outcomes for RTS, kanban versus roster, or
heat-map surfaces.

## Field notes, unverified

Extracted from fetched sources but never voted on. Leads for follow-up.

### Vendors

- Cursor 3 replaced the Composer pane with an Agents Window: every running
  agent in one sidebar with live output, originating task, target repo, and
  local or cloud placement. Up to eight parallel subagents in isolated
  worktrees. A unified inbox aggregates tasks from mobile, Slack, GitHub,
  Linear, and n8n. Mid-session handoff from local to cloud. Source:
  https://www.gilricardo.com/blog/cursor-3-agents-window-multi-agent-coding-2026
- Google Antigravity: the Agent Manager spawns, monitors, and steers agents
  across workspaces. The Inbox is a centralized ledger of the latest message
  per conversation where approvals and feedback requests surface. The
  overview page does not document the UI shape. The claim that "conversation"
  is the unit of parallel work was refuted 0-3. Source:
  https://antigravity.google/docs/agent/
- Zed's own multi-agent story is agent selection inside one panel, not tabs
  or a kanban. Source: https://zed.dev/acp/agent/factory-droid
- Codex app: sandbox default restricts edits to the working folder; agents
  ask for permission only for elevated access such as network. Per-project
  rules can auto-approve. Session history and config are shared across app,
  CLI, and IDE extension. Source: https://openai.com/index/introducing-the-codex-app/

### Startups and open source

- Superset (superset-sh/superset, Electron, open source, about 9k stars by
  March 2026, three ex-YC CTOs): one worktree per agent with terminal,
  review, and open-in-editor. Supports Claude Code, Codex, Cursor Agent,
  Gemini CLI, Copilot, OpenCode. HN sentiment: review is the real
  bottleneck. Source: https://news.ycombinator.com/item?id=46368739
- Terragon: shut down January 2026 (cloud-VM isolation category). Crystal:
  deprecated February 2026, replaced by Nimbalyst. Claude Squad: TUI over
  worktrees plus tmux, vim keys, no Windows, the roundup's pick for solo
  developers. Bernstein and Microsoft Conductor keep no LLM in the
  orchestration loop. Git worktrees became the consensus isolation primitive
  within about eighteen months. Source:
  https://www.augmentcode.com/tools/open-source-agent-orchestrators
- Gas Town (Steve Yegge): dozens of Claude Code sessions in tmux windows, one
  conversational Mayor agent that operates the rest on the operator's behalf,
  work tracked as Beads (JSON issues in git), a Refinery merge queue. About
  189k lines in four weeks. Users hated the vocabulary (Polecats, Guzzoline,
  convoys, deacons, seances, wisps), the roughly 100 dollars per hour burn,
  Beads state polluting every PR, and 141 orphaned claude processes. One
  practitioner got 6 merged PRs from 7 beads overnight and still quit because
  the structure was too heavy for feedback-heavy work. Requires
  skip-permissions mode. Appleton's read: the patterns (single talk-to agent,
  persistent roles with ephemeral workers, git-tracked task units, merge
  queue) will be absorbed into simpler tools; Gas Town itself will not be
  adopted. Sources: https://maggieappleton.com/gastown ,
  https://tenzinwangdhen.com/posts/gastown-good-bad-ugly/ ,
  https://news.ycombinator.com/item?id=46678421
- VibeCraft: paid macOS RTS canvas for Claude Code and Codex, 20 dollars per
  month. Its HN launch drew one comment, the author's. The metaphor got
  built and did not catch fire. Source: https://vibecraft.build/

### Bitter Lesson sources

- Latent Space, "The Evolution of the Agent Harness" (2026-08-22): three eras,
  bolt-on scaffolding, co-training where the harness is absorbed into weights
  (Anthropic deleted about 80 percent of Claude Code's system prompt with no
  capability loss), then the attention era where the harness becomes the
  agent's interface to the human. Permission and approval systems are the
  one layer that cannot be absorbed, because a model that self-governs
  permissions has eliminated them. Predicts every vendor ships a "human
  attention policy surface" within a year. Source:
  https://www.latent.space/p/attention-interface
- Cognition, "Multi-Agents: What's Actually Working" (2026-04-22): writes
  stay single-threaded, extra agents contribute intelligence only. A
  clean-context review agent finds about 2 bugs per PR, 58 percent severe.
  What got deleted: parallel-writer swarms, and a weak model consulting a
  strong "smart friend", because the weak model cannot judge when to
  escalate or what to ask. Source: https://cognition.com/blog/multi-agents-working
- Patel et al., "What Happens When the Model Eats the Stack?" (2026-09-02):
  a plain coding agent on a newer model beats hand-scaffolded pipelines and
  the ranking reverses across model generations. What survives is persistent
  environmental context the model cannot learn in training. Source:
  https://arxiv.org/html/2609.03141
- Bowne-Anderson (2025-12-12): Manus re-architected five times, Vercel
  removed about 80 percent of its agent tools and got fewer steps. "Build for
  deletion; every piece of harness logic should have an expiration date."
  Source: https://hugobowne.substack.com/p/ai-agent-harness-3-principles-for
- Osmani (2026-03-26), the counterweight: developer-written context helps,
  LLM-generated AGENTS.md does not. Check every 5 to 10 minutes, no hovering.
  Agents coordinate through shared task lists and file locks rather than
  asking humans. Source: https://addyosmani.com/blog/code-agent-orchestra/

### Protocol precedents

- tmux control mode: a control client is a normal client that emits text
  instead of drawing. It accepts the same tmux commands and additionally
  streams asynchronous notifications (`%output`, `%sessions-changed`,
  `%window-add`, `%pane-mode-changed`). Command output is framed by
  `%begin` and `%end` guard lines with command numbers. Per-pane flow control
  through `%pause` and `refresh-client -A`. Stable opaque IDs over names.
  Source: https://github.com/tmux/tmux/wiki/Control-Mode
- Neovim remote UI: every UI is an external client over msgpack-rpc. A UI is
  a plugin: it forwards input and renders redraw events. Several GUIs attach
  to one running session at once. The event vocabulary is generated from one
  machine-readable header so a frontend discovers the protocol from the
  running server. Source:
  https://github.com/neovim/neovim/wiki/Remote-UI-architecture
- Omarchy shell plugins: one long-lived host process, nearly every element a
  plugin. First-party and third-party plugins discovered by the same
  mechanism. Enabled state is a plain JSON file: a third-party plugin is on
  when its id appears in `shell.json`; first-party ones are on unless listed
  in `disabledPlugins`. Each plugin has a `manifest.json` with id, kinds, and
  entry points. The `omarchy.` id namespace is reserved. Source:
  https://omarchy.org/manual/shell-plugins/
- Git plumbing and porcelain: plumbing has stable, script-chainable output;
  porcelains (git's own, lazygit, magit) are thin clients over it. Source:
  https://git-scm.com/book/en/v2/Git-Internals-Plumbing-and-Porcelain

## Best and worst

Best, verified:

- Claude agent view, for the attention queue. State-grouped roster, question
  text in the row, count in tab title and footer, peek without attaching.
- OpenAI Symphony, for the control-plane abstraction. The unit of work is
  above the session, the loop is thin, and stalled agents restart.
- Zed ACP, for the protocol. Multi-vendor adoption, one config entry per
  agent, JSON-RPC over stdio.

Worst, verified:

- Vibe Kanban, commercially. Adoption with no viable middleware business.
  The hosted kanban layer is what died.

Worst, unverified but consistent across three sources:

- Gas Town, as a product. A vocabulary the operator must memorize, process
  leaks, thin observability, high burn, skip-permissions required. Its
  patterns are worth keeping. Its surface is the avoid list.

No design-quality "worst" is supportable from verified evidence alone.

## Patterns to steal

- One roster screen grouped by whether the human is needed. Blocked on top.
- The question text in the row, and a peek that answers without attaching.
- Awaiting-input count pushed everywhere the operator already looks: tab
  title, foreground footer, phone push.
- A jump-to-next-blocked key.
- One container above the pane as the unit of work, with one worktree each.
- A natural-language dispatch input as the place new work starts.
- One talk-to agent that operates the rest (Gas Town's Mayor, without the
  vocabulary).
- Multi-select and broadcast (lazybox `v` and `Shift-B`).
- The floor verbs exposed to models as MCP tools (lazybox's shape).
- Plumbing with frozen machine-readable output, porcelains as thin clients.
- One verb vocabulary for humans and frontends plus an async notification
  stream (tmux control mode).
- Plugins enabled by listing an id in one JSON file, reserved namespace for
  built-ins (Omarchy).

## Avoid

- A role vocabulary the operator has to learn (Gas Town).
- Modal interrupts for attention. Counts and jumps instead.
- Parallel-writer swarms on one tree (Cognition).
- A small or weak operator model that decides when to escalate (Cognition:
  it cannot tell when to ask).
- Hand-built workflow state machines around current model limits. They have
  a six-month shelf life (Bowne-Anderson, Patel et al.).
- A separate command set for GUI clients (tmux's lesson: same verbs, text
  output).
- Betting the product on the board. The kanban layer is what Vibe Kanban lost.
- RTS canvases as the primary surface. Two shipped, neither shows adoption.
- Hardcoded automation policies instead of plugins (lazybox's own weakness).

## Bitter Lesson verdict on "conversation as the primary porcelain"

Medium confidence. The thesis is consistent with where the first-party tools
moved: agent view leads with a natural-language input, Symphony replaces
hand-steering with a thin loop, and Gas Town's one durable idea is a single
talk-to agent. Three independent sources (Latent Space, Patel et al.,
Bowne-Anderson) argue hand-built orchestration structure is a depreciating
asset and the model absorbs it. One source (Osmani) argues human-written
specs still pay. No verified source measured the cost or the ceiling of a
model-operated control layer. The claim that OpenAI deleted its state machines
was refuted. The verdict rests on design convergence, not vendor testimony.

Two things the sources say do not get absorbed: the permission and approval
layer (the model cannot govern its own permissions without eliminating them),
and persistent environmental context. Both are what coppice already is: the
gate and the floor state. The conversation sits on top of them, not instead
of them.

## Recommendation for coppice

### Keys

The cross-app trio the user already knows (Ctrl+T new, Ctrl+W close,
Shift+Tab cycle) plus three borrowed from agent view and lazybox: Space to
peek at a blocked pane's question, Enter to attach, and one jump-to-next-blocked
key. Ctrl+A d stays as the detach key inside an attached pane. Nothing else
is memorized. Everything else is typed as language.

### The model-operated layer

A normal pane in the harness of choice, promoted to foreman by giving it the
floor verbs as MCP tools: `list`, `read`, `spawn`, `prompt`, `wait`, `allow`,
`deny`, `close`, plus `notify`. Any pane can be foreman by pointing it at the
same tool set. This is lazybox's shape and Gas Town's Mayor without the
vocabulary.

Not a small local operator model. Cognition's evidence says a weak model
cannot judge when to escalate or what to ask, and the foreman's whole job is
escalation judgment. The foreman is frontier-class and costs one session's
turns. Each floor command is one tool call against a cached prefix. On a
subscription plan that is quota, not dollars. No source measured this; it is
an estimate.

The gate stays outside the foreman. `allow` and `deny` on the floor are
still proven by the envelope before they run. The foreman proposes, the gate
decides.

### The attention queue

The roster is the queue. One screen grouped by state, Needs input on top,
the question text in the row, Space to peek and answer, Enter to attach. The
awaiting-input count goes into the terminal tab title, into the footer of
whichever pane is attached, and out through ntfy to the phone. No modal
interrupts. The `unknown` state that live Claude panes show today is the
first bug to fix, because the queue is only as honest as the state source.

### The unit of work

A container above the pane: a task with a label, a worktree, and zero or
more panes. Devin's Space, Symphony's issue, lazybox's row. The pane is
where a harness runs. The task is what the operator talks about.

### Protocol for swappable frontends and plugins

Newline-delimited JSON-RPC 2.0 over the existing uid-checked unix socket.
The same verb vocabulary humans use, one method per verb, with an async
notification stream for state changes (tmux control mode's design). Stable
opaque pane and task ids. Per-pane flow control so a slow frontend does not
drown in output.

Shape it ACP-adjacent: reuse ACP's session, prompt-turn, streaming update,
and permission-request types where they fit, so an ACP client could target
coppice as an agent server later. That is an open question, because ACP's
stable transport is stdio subprocess, not a shared socket.

A frontend is any program that speaks this over the socket: the TUI, the
phone PWA, the voice bridge, a third party's weekend project. A harness joins
as one config entry of command plus args (ACP's registration shape). A
plugin is a manifest with an id, a kind, and an entry point, enabled by
listing the id in one config file, with the `coppice.` namespace reserved
(Omarchy's shape). Automation policies such as merge-on-green are plugins,
not built-ins.

## Refuted claims

Eight claims were killed. Do not reuse them.

- Agent view row summaries are Haiku-generated at a per-session cost (1-2).
- The Codex app's data model is threads per project with per-agent worktrees
  and in-thread diff review (0-3).
- Codex Automations land in a review queue (0-3).
- Antigravity's unit of parallel work is the conversation and the Agent
  Manager is its dedicated surface (0-3).
- Devin routes attention through native OS notifications and bindable
  permission shortcuts (1-2).
- OpenAI explicitly abandoned hand-crafted state machines for objectives
  plus tools (0-3).
- lazyagent discovers sessions in tmux panes and shows live state (0-3).
- lazyagent's state is a five-value enum derived by scraping tmux every two
  seconds (0-3).

## Open questions

- What does a model-operated control layer cost per command in tokens and
  latency? Nothing measured it. The answer decides how often the foreman is
  worth asking.
- Do kanban, roster, and RTS surfaces differ in measured outcomes, or only
  in taste? Every source describes a design. None compares.
- Can coppice present itself as an ACP agent server without losing the
  fail-closed allow and deny verbs, given ACP's local transport is a stdio
  subprocess rather than a shared socket?
- What happened to Conductor, Multica, Sprites, CCManager, Herdr, Paperclip,
  Ralph loops, and Gauntlet's tooling? The verified corpus is silent.

## Time sensitivity

Agent view is a research preview whose keys may change. Devin Desktop changes
weekly. ACP's remote transport is a draft. Vibe Kanban's community edition is
best-effort. lazybox is a five-star pre-1.0 personal project. openai.com
blocks direct fetches, so both OpenAI findings were confirmed through reader
proxies and InfoQ's quotation.

## Run statistics

| Item | Count |
|---|---|
| Search angles | 5 |
| Sources fetched | 24 |
| Claims extracted | 117 |
| Claims verified | 25 |
| Confirmed | 17 |
| Refuted | 8 |
| Findings after synthesis | 9 |
| Agent calls | 106 |
