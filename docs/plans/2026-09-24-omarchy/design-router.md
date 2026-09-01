# Design: a router that learns from verified results

Status: proposal for the owner, 2026-09-26. Parts 0 and 4 are **Built** (2026-09-30, in Python,
Go and Rust; rulings RP-1 to RP-14 in `clients/ADJUDICATIONS.md`). Parts 1 to 3 are proposals.
Each part names the existing code it builds on.

## The problem

The gateway picks a model for each turn. Today the choice has three tiers:

- **Tier 0.** A stored pathway matches. The plan is reused and verified again. This costs almost
  nothing.
- **Tier 1.** A cheap model, local or remote.
- **Tier 2.** The frontier model.

A fixed rule of thumb (`routing.py`) chooses between Tier 1 and Tier 2. The rule does not learn,
and it has no signal for whether the cheap model's work was correct. Switchyard uses a trained
chooser, so it can beat our rule on this one choice.

## The one idea

openDaisugi is the only router that can see whether a turn's work was correct. The gate and the
verifier already judge every tool call against the envelope. Tests and the journal show whether
the task finished. So our router can learn from real results on the owner's own work, not from a
general benchmark.

## What exists today

There is no single router yet. Three choosers share one heuristic, `estimate_difficulty` in
`routing.py`:

| Chooser | File | Chooses for | Used by |
|---|---|---|---|
| `RouteAdvisor.advise` | `routing.py` | a task (advice only) | the hidden `daisugi route` command (`cli.py`) |
| `route_turn` | `gateway.py` | one gateway turn; downgrade only; cache-aware (ADR-0015) | `gateway_pipeline.py` |
| `size_step` / `size_plan` | `model_sizer.py` | one plan step, on a ladder (local, cheap, frontier) and a live budget | `orchestrator.py` |

The Go and Rust ports have `route_turn` and the route command (`clients/go/internal/gateway/route.go`,
`clients/rust/src/gateway/route.rs`). Per-step sizing and the orchestrator are Python only (checked
by grep over `clients/`).

## Five parts, from least to most work

### 0. Delegate grunt I/O

This is the cheapest lever. It comes from Spotify's Portal and its `shunt` plugin for Claude Code
([Spotify Engineering, September 2026](https://engineering.atspotify.com/2026/9/portal-by-spotify-cut-my-claude-code-token-usage-by-90)).
A PreToolUse hook blocks a file read over a line threshold (default 350, `SHUNT_MIN_LINES`) and
sends the agent to a `bulk-read` script instead. Two worker "modes" run on a cheap model (Gemini
2.5 Flash in the post): `bulk-reader` summarizes files, `code-writer` writes boilerplate. The post
reports about 90% fewer tokens. That number counts the Claude read-context tokens in selected
bulk-read cases on a Java monorepo. It is not an end-to-end saving: worker cost, latency and
retries are outside it. A third-party summary of the same pattern is at
[this gist](https://gist.github.com/vtri950/84b2261efbadba243870bf161764aeb7). The plugin repository
is named in the post as `spotify/portal-ai-plugins`; not verified.

Our form has three pieces:

- **A gate rule.** A read over a size threshold gets a deny whose reason names the delegate tool.
  This is what shunt does, and it needs no new verdict: the gate today returns allow, deny or ask
  (`gate.py`).
- **A `daisugi_delegate` MCP tool** with two modes, bulk-read and code-write. It runs on a cheap
  worker that the router picks. The MCP server (`mcp_server.py`) already carries `recall`,
  `run_plan` and others; the delegate joins them.
- **Skill text that says when to delegate.** `daisugi install` does not write a skill file. It
  links the bundled `opendaisugi-checklist` skill (`install.py`, `_link_skill`) and writes a
  CLAUDE.md block. That skill already has `references/delegation.md` on `preferred_model`. The
  "when to delegate" text goes there as a new reference.

**The general form is the rewrite verdict.** The grafts design (separate, not yet written; see
the strategy notes, "Token saving") adds a fourth verdict, rewrite: "do this cheaper call
instead". Rules are data, proved at install. Part 0 is its first rule, shipped first as a deny
with a reason. Which harness hooks can rewrite tool input in place is not verified here; that
belongs to the grafts research.

Part 0 edits a tool call's path, never the model request. So the prompt cache is kept, and Part 0
does not change the mapping doc's rule that the proxy does not edit requests
(`docs/research/token-saving-landscape-mapping.md`). The grafts design amends that rule.

**The delegate obeys the envelope.**

- A bulk-read has a file_read effect on the files, and a network effect to the worker. With a
  remote worker the file contents leave the machine. The envelope must grant both, and the gate
  checks both.
- The default worker is local. The local rung exists already: `tier1-local` in `route_turn`
  (`gateway.py`) and the `local` rung in `build_ladder` (`model_sizer.py`). A remote worker is
  opt-in.
- The delegate is refused under `stakes='physical'`, as `_check_delegation_safety` (`verify.py`)
  refuses any delegated step there.

**Caveats, carried from Spotify's own post:**

- **Line numbers from the worker are unreliable.** So the delegate returns quotes, not line
  numbers. The tool checks each quote is an exact substring of the file and marks any that is
  not. The frontier then edits with a targeted read of its own.
- **Each call takes 10 to 30 s.** So the threshold is a size, and it is tuned by Part 4's
  numbers: raise it when the delay costs more than the saving. The gist suggests 500 lines as the
  next step.
- **Cheap models miss subtle bugs.** Spotify's worker missed a thread-safety bug. The skill text
  keeps debugging, design and safety-critical code on the frontier.
- **Worker output is untrusted text.** It goes back to the frontier as data. It is never spliced
  into a command, the same rule `TaskStep` follows (`models.py`).

**Open: how code-write writes.** (a) The delegate writes the file as a file_write effect, verified
against the envelope and gated like any write. (b) The delegate returns a draft and the frontier
applies it through its own gated Write. (b) spends frontier output tokens on the draft. We
recommend (a): the saving is real and the write stays under the gate.

**Open: the tool name.** Existing tools have bare names (`recall`, `run_plan`) on the server
`opendaisugi`. `daisugi_delegate` breaks that pattern; `delegate` keeps it. We recommend
`delegate`, for consistency.

**Built 2026-09-30.** What shipped, and where it differs from the text above:

- **The rule is data.** A `deny_redirect` rule is a JSON file in `<gate root>/grafts/` (GR-5,
  RP-1): its id, version, state (`audit` records, `active` redirects), the tool (`Read`), the line
  threshold (default 350) and `allow_remote`. With no rule file, nothing changes. The installer,
  `daisugi graft install|status|remove`, refuses beside a rival PreToolUse hook (GR-3, GR-R-1,
  GR-R-2); a person or a bot may still write the file by hand.
- **The gate** (`gate.py` `_maybe_graft`; Go `internal/gate/graft.go`; Rust `src/gate/graft.rs`)
  denies an allowed whole `Read` of a text file over the threshold, with a reason that names
  `mcp__opendaisugi__delegate`. It is not a would-deny, so it stays out of the false-deny count
  (RP-4). An audit-mode gate only records it, whatever the rule's state (RP-4, as overturned). A `limit` at or under the threshold passes (RP-2). Only the claude format redirects,
  the one host format where the delegate's tool name is known, and only when the envelope would
  allow the delegate call. With no worker, the read goes through
  and `router status` says why (RP-5).
- **The gate checks the delegate call** as a read of its absolute path and, for a remote worker,
  a network send (RP-7). The MCP allowlist alone never grants the read.
- **The `delegate` MCP tool** (RT-2), bulk-read only (`mcp_server.py`; Go and Rust
  `mcp serve`). Code-write, ruled a draft by RT-1, is not built. The worker is the local model
  `daisugi tiers setup` recorded; a remote worker needs the rule's `allow_remote` and an
  envelope that names its host (GR-2, RP-6). Quotes are checked as exact substrings and a quote
  that is not is dropped; no line numbers (RP-9). The answer is returned as untrusted data.
- **The skill text** is `references/when-to-delegate.md` in the `opendaisugi-checklist` skill
  that `daisugi install` links.
- **Cases:** 131 gate cases, 36 MCP cases with a fake worker (a local chat wire, a remote worker
  through the fake proxy, the Messages wire), all 0 disagreements in Go and Rust.

### 1. Cheap first, verifier escalates

- A cheap model takes the turn first.
- If its tool calls fail the envelope, or its plan fails verify, the turn goes to the frontier
  model. The failed attempt is kept in the journal.
- The verifier makes the choice, not a guess about difficulty.
- Cost: one extra cheap turn when the cheap model fails. The prompt cache matters here: switching
  the model in the middle of a conversation loses the cache (ADR-0015), so escalation happens only
  at the start of a task, or it keeps the frontier model for the rest of the task.

### 2. The frontier plans, cheap models do the work

- For a task with many steps, the frontier model writes the plan (the decomposer does this
  today).
- The verifier checks each step against the envelope.
- Cheap models, or stored pathways, do each step.
- Frontier tokens go to planning only. This is where the orchestrator and pathways pay off.

**Each step gets its model from the router.** Every step has a `preferred_model` field
(`StepBase` in `models.py`). Today the orchestrator fills it, but not from the router:
`_apply_preferred_models` (`orchestrator.py`) copies the choice of `size_plan` (`model_sizer.py`),
a fixed difficulty ladder. `BudgetAwareDelegatingExecutor.run` then sizes each step again against
the live budget. `RouteAdvisor` and `route_turn` never touch a plan step. The change: the router
becomes the one interface that fills `preferred_model`. The sizer becomes its step adapter, and
the budget gate stays as it is. Then parts 1, 3 and 4 apply to steps as well as turns, and the
learned chooser of part 3 improves plans too.

**The router is an interface any runner calls, not only the gateway.** A plan can be run by a
runner outside the Python orchestrator: weave, the plan runner in the proposed ADR-0022 (not in
`docs/adr` at the time of writing). weave, sprig and grove are on hold with the owner, so this is
**Proposed**. The router must not assume the gateway or the orchestrator is its caller. It takes
a request (a turn, a step or a delegate call, with the envelope and budget) and returns a model,
a tier and a reason, and it logs the choice in the journal.

**Open: how an outside runner reaches the router.** Options: the Python API only; the existing
`daisugi route --json` command, widened to take a step; an MCP tool; a request on the coppice
socket. We recommend the command with `--json` first. It exists in Python, Go and Rust already,
it is easy to test with golden cases, and weave was built as a runner of a JSON graph.

### 3. Learn from the owner's journal

- On a small sample of frontier turns (for example 5%), a cheap model also answers in the
  background. The owner never sees this answer.
- The verifier and the tests judge both answers. The result is a labelled example: "for this kind
  of task, the cheap model was as good" or "it was not".
- A small local model learns from these examples. It uses the embedders openDaisugi already has
  (lexical, potion), so it needs no new dependency and no GPU.
- The router then asks this model: "for this task, will the cheap model pass?" and routes on the
  answer, with a confidence threshold.
- The shadow answers cost tokens. The sample rate and a daily token cap bound that cost, and the
  owner can turn it off.

### 4. Measure it honestly

- Every routing choice, every escalation and every shadow result goes into the gateway journal.
- `daisugi router status` shows, per week: tokens saved, the escalation rate, and how often the
  learned chooser was right.
- A new chooser replaces the old one only when it wins on the owner's own recent turns, held out
  from training. Otherwise the old one stays.
- For part 0: each delegate call records the file size, the worker, the delay, the worker's
  tokens and the frontier tokens it kept off the context. The last is an estimate and is marked as
  one, as the gateway meter marks its counterfactual (`gateway.py`).

**Built 2026-09-30, in part.** What shipped:

- **Every delegate call** is a row of `<data dir>/router/delegations.jsonl`: the file's size and
  lines, the worker, its tier and host, its tokens in and out, its billed cost (0 for a local
  worker; the gateway's price for a remote one, or unpriced), the time, the quotes kept and
  dropped, and the frontier tokens and dollars kept off the context, marked estimated (RP-10).
- **Every gateway turn** now records its time, `elapsed_ms` (RP-11); its tokens and billed cost
  were recorded already.
- **Every graft decision** is in the gate's audit log as `graft` (applied or not, and why).
- **`daisugi router status`** shows, per ISO week: tokens saved and billed cost saved (both
  marked estimated when an estimate is in them), escalations, delegations (ok, refused, the mean
  time), quotes kept and dropped, worker tokens and cost, reads redirected and not, and task
  outcomes (RP-12). It also names the rule and the worker in force. JSON with `--json`.

Not built: escalations are always 0 (part 1 is not built); no chooser learns, so none is
replaced (part 3); task outcomes are unknown until a label source exists (the ranking design),
so the promotion measure GR-6 names, billed cost per successful task, is not computed yet.
Cases: 20 new `router status` cases (33 in all), and the 127 turn cases now carry the time, 0
disagreements in Go and Rust.

## What we get that Switchyard does not

- A correctness signal: the verifier, the envelope and the tests.
- Training on the owner's own work, locally, with nothing sent anywhere.
- Reuse through pathways, verified before use.
- Cache-aware routing.

## Risks

- **Shadow cost.** Part 3 spends tokens to learn. The cap and the sample rate bound it.
- **Weak labels.** Passing the envelope does not prove the work is good. Tests and the task's end
  state are better labels, and they are not always present. Where no label exists, the turn does
  not train the chooser.
- **Privacy.** Shadow answers use the same credential and the same models the owner already uses.
  Nothing leaves the machine that did not leave it before, except the extra cheap turns. A remote
  delegate worker is different: it sends file contents to a new host. So the local worker is the
  default, and a remote one needs an envelope grant.
- **Cache loss.** Escalation in the middle of a task breaks the prompt cache. Part 1 limits where
  escalation may happen.
- **Delegate quality.** A cheap worker's summary can be wrong without looking wrong. Quote checks
  catch invented text, not a wrong conclusion. The skill text keeps judgement on the frontier.

## Order

1. Parts 0 and 4 first, together. Part 0 is the cheapest saving, and part 4 measures it and tunes
   its threshold.
2. Part 1, behind a flag.
3. Part 2, with stage K (the orchestrator and pathways).
4. Part 3, behind a flag, with the shadow cap.

Each part is built in Python first as the oracle, then in Go and Rust, with the same golden
cases, as every other stage.
