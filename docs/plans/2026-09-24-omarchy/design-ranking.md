# Design: ranking is a primitive

Status: proposal for the owner, 2026-09-26. Revised 2026-09-28 after the owner's ranking ruling
(RK-10 in `DECISIONS-PENDING.md`). **Built 2026-09-30, in part**, in Python, Go and Rust (rulings
RK-R-1 to RK-R-15 in `clients/ADJUDICATIONS.md`): the records, the pure fit (stages 1 and 2 of the
staged plan), choices and cards with the switch cost and decay (stage 3), the permanent-step join
in weave (stage 4, through weave's own ask), and weave's attempts (part of stage 10). The commands
are `daisugi rank fit`, `choose`, `queue`, and the owner's `record pick` and `record drop` (the
verbs live under `record`, so the gate's existing deny covers them). Not built: the judge step
(stage 5), `rank show`, the router labels (stage 6), the coppice cards view (stage 7), the pivot
(stage 8), judge trust (stage 9), and the SQLite index of RK-6. Each part names the code it builds
on.
Uses by sprig or a parent sprig are **Proposed**, because sprig is with the owner
(`design-sprig.md`). weave is a `daisugi weave` command on the supervisor (ADR-0022, accepted in
part 2026-09-28). grove is retired; coppice replaced it.

Sources: the strategy notes (`strategy-2026-09-26.md`, "Ranking is a primitive" and "Human
review at speed"), the research note `docs/research/ranking-inputs-2026-09-26.md`, and the
owner's ruling of 2026-09-28 (recorded in `.superpowers/sdd/omarchy/progress.md`). Outside
claims below cite the research note by its source number, for example "note [10]". This design
did not fetch those pages again.

## What changed on 2026-09-28

The owner ruled that a choice never blocks work unless the next step is irreversible. The
2026-09-26 version had an "ask policy": when the judges could not settle a pair, the ranking
waited for the owner, inside a per-ranking budget and a daily cap. That policy is gone. In its
place:

- The runner proceeds on the top-ranked option. The alternatives are logged.
- Each real choice opens a **card** in an asynchronous review queue. The owner reads cards when
  there is time, out of context, often on the phone.
- A card the owner does not review closes as "kept" when switching gets expensive, or after a
  set time.
- A later pick pivots the work: undo and apply the alternative, or start a follow-up task.
- No daily caps. No per-task question limits. No new database.

Retired by this ruling: RK-2 (the owner's pick as a weighted vote), RK-3 (the stop threshold as
an ask stop, and the budgets), RK-4 (the daily cap), RK-9 (calibration questions). The fit, the
judges, the elimination order and the eloEngine ports stay.

## The problem

Several parts of openDaisugi need the same answer: "given N attempts at one task, which is best,
and how sure are we?"

- The router (`design-router.md`, parts 3 and 4) needs a label: "for this task, the cheap model
  was as good as the frontier", or "it was not".
- The garden's A/B test (`gardener/ab_test.py`) needs a reason to promote or drop a pathway.
- A parent agent that starts several children on one task must keep one result
  (`design-delegation-tree.md`, "How ranking picks among children's attempts"). The owner now calls
  this the delegation tree.
- The owner wants to review several agents' work by picking the better of two, not by reading
  every diff (strategy notes, "Human review at speed"). The owner must not be a bottleneck.

Today each would need its own ad hoc rule. There is no ranking code in daisugi (grep for "rank"
over `src/opendaisugi` finds only the gateway's reuse worklist, `cli.py`, and unrelated uses).

## The one idea

Rank like a proof system, not like a beauty contest. Hard evidence first, opinions last:

1. **Hard constraints eliminate.** Out of envelope, a failed proof, a failed required test.
2. **Measured scores rank.** Tests passed, features green.
3. **Preferences break ties.** Model judges, fitted with Bradley-Terry. Cost is the last
   tie-break.
4. **Never wait for a person unless the next step is permanent.** Proceed on the leader. Show the
   choice to the owner later, as a card. Let a later pick change the work.

Every comparison and every choice goes into the journal as append-only JSONL, so every ranking
can be replayed and every label can be traced to the votes behind it. One call, `daisugi rank`,
sits beside `daisugi verify`.

## The contract

`rank` is a pure function. It calls no model and runs no test. It reads attempts and
comparisons and returns a ranking. Model judges are a separate step that adds comparisons (see
"The judge step"). The owner's answers arrive through cards (see "The review queue").

### Input

| Field | Meaning |
|---|---|
| `ranking_id` | One ranking = one task with its attempts. Comparisons are keyed by it. |
| `task` | The task text. Shown to judges and on the card. Never used by the fit. |
| `project` | The project the task belongs to (a repo path or a coppice task's project). Used to sort cards. Never used by the fit. |
| `attempts[]` | 1 to 8 attempts. Each has the fields below. |
| `attempts[].id` | A short id, unique in the ranking. |
| `attempts[].content_hash` | sha256 of the attempt's canonical artifact (the diff, or the result text). A vote binds to this hash. |
| `attempts[].author` | Model and harness. Kept for the journal and for the self-preference rule. Never shown to a judge. |
| `attempts[].edge_proof` | `ok`, `failed` or absent. `failed` means a child's envelope was not inside its parent's, so it never ran (`design-delegation-tree.md`). |
| `attempts[].started_by_parent` | True when a parent agent started this attempt. Then `edge_proof` is required. |
| `attempts[].verify` | The `VerificationResult` fields `ok` and the violation count (`models.py`), when the attempt ran a plan. |
| `attempts[].gate` | Counts of gate verdicts. An allow by operator override outside the envelope is counted apart. |
| `attempts[].tests` | Per test: name, `required` bool, result `pass`, `fail` or `not_run`. |
| `attempts[].features` | Optional: the pass/fail feature list (strategy notes, "What the labs say"), each item with a `required` bool. Missing everywhere today. |
| `attempts[].cost` | `tokens`, `wall_ms`, `diff_lines`. Each is marked `measured` or `estimate`. |
| `attempts[].where` | Where the attempt lives, so a later pick can reach it: a coppice worktree task, a branch, or a checkpoint ref. The fit never reads it. |
| `attempts[].previews` | Optional pointers for the card: screenshot paths, a preview URL, a one-line summary. The fit never reads them. |
| `comparisons[]` | Optional. When absent, `rank` reads them from the journal by `ranking_id`. |
| `policy` | The label threshold and the prior. Defaults in "The fit". |

### Output

| Field | Meaning |
|---|---|
| `status` | `ranked` (the leader is confident), `single` (one survivor), `provisional` (a leader exists but is not confident; the runner still proceeds on it), or `none_survived`. |
| `eliminated[]` | Each eliminated attempt with every reason, for example `required test failed: test_login`, `edge proof failed: never ran`. |
| `quality_tiers` | The survivors grouped by the quality stage only. Tier 1 is best. **This is the field a consumer reads as a quality label.** It has no cost in it. |
| `order` | The full order after all stages. |
| `decided_by` | For each adjacent pair in `order`: `quality`, `preference`, `owner` or `cost`. |
| `scores` | Per survivor: the BT strength, and a 90% bootstrap interval. |
| `leader`, `confidence` | The top attempt, and the share of bootstrap fits in which it beats every other survivor in its quality tier. |
| `label` | The leader's id when `confidence` clears the threshold or the owner picked it, else `no_label`. The router and the garden train only on a real label (router part 4, "where no label exists, the turn does not train the chooser"). |
| `next` | The next pair for a model judge, `{pair, judge, why}`. Null when no untried judge is left for any useful pair. It never names the owner. |
| `stop` | Why `next` is null: `confident`, `judges_exhausted`, `nothing_to_ask`. |
| `owner_constraints[]` | Pairs the owner decided through a card. Each one fixes the order of that pair (see "The fit"). |
| `warnings[]` | For example an inconsistent judge, a cost that is an estimate, a vote whose attempt hash changed. |

Fail closed:

- If every attempt is eliminated, `status` is `none_survived` and there is no leader. `rank`
  never returns the least bad attempt (research note, Recommendation 1). The runner stops that
  branch; it does not proceed on nothing.
- A required test with no result counts as failed. A missing `edge_proof` on an attempt with
  `started_by_parent` true counts as failed. A missing `verify` on an attempt that ran a plan
  counts as failed.
- A malformed input exits 2 with the house three-line error, the way `daisugi verify` does
  (`cli.py`, `verify_cmd`).

## The lexicographic order

### As the owner stated it

Hard constraints, then measured scores (tests passed, tokens, time, size of the change), then
preferences.

### Why it needs one change

Taken literally, cost ranks before preferences. Two attempts almost never tie on tokens, time
or diff size. So the judges would almost never decide anything, and the cheaper attempt would
always win among equally correct ones. That is wrong for the main consumers:

- The router asks "was the cheap attempt as good?" That is a quality question. If cost is in
  the order, the cheap model wins by being cheap.
- In the garden, cost always favors the pathway (Tier 0 costs nothing). Promotion needs to know
  whether the pathway's result is as good.

**Provisional ruling RK-1: option (c).** Quality measures, then preferences, then cost as the
last tie-break. The other options were (a) the literal order, and (b) cost only outside a band
such as 2x. Every consumer that cares about cost reads it from `scores` and the input directly.
`quality_tiers` stays a separate output with no cost in it.

### The stages

1. **Eliminate.** `edge_proof` failed; `verify.ok` false; any required test failed or not run;
   any required feature not green. A required feature is a required test in another form, so it
   is treated the same way. Gate denies alone do not eliminate: a deny means the gate stopped the
   action, so the attempt stayed inside its envelope.
2. **Quality tiers.** Compare survivors by a fixed tuple, larger first: (optional tests passed,
   optional features green). Equal tuples share a tier.
3. **Preference.** Inside each tier, fit BT over the model judges' comparisons. Apply the owner's
   constraints.
4. **Cost.** Inside a tier, attempts the fit cannot separate at the threshold are ordered by
   tokens, then wall time, then diff lines. Cost never reorders a pair the owner decided.

**Provisional ruling RK-8: an operator override.** An attempt the owner let out of its envelope
with a one-time allow (coppice `agent.allow`) is kept, with a warning. The owner's own allow
should not throw away the attempt; the strategy's three hard constraints do not name it.

## The fit

Inside one quality tier, with the comparisons for that tier's attempts.

**Bradley-Terry with a weak prior.** P(i beats j) = s_i / (s_i + s_j). The fit is a maximum
likelihood over all votes at once, so the vote order does not matter and a replay from the
journal gives the same answer (research note [1], [2]). The prior is a pseudo-count: each
attempt gets one virtual tie (weight `prior`, default 1.0) against a virtual reference attempt.
This fixes two gaps the research note found in eloEngine's fit: an attempt with no wins no
longer gets strength 0, and a disconnected comparison graph still has a finite answer.

**Votes.**

| Source | Counts as | Weight |
|---|---|---|
| Model judge, both orders agree | a win | 1 |
| Model judge, the two orders disagree | a tie (half a win each) | 1 (research note [10]) |
| Model judge, error or timeout | **no vote** | 0 |

A judge error is never a win. `ab_test.py` does the opposite today: a judge that raises falls
back to the structural verdict, which can pass. See "Conflicts".

**One vote per judge per pair.** A judge votes once on a pair of content hashes. A judge that
is asked again gives the same opinion, and six copies of one opinion are not six pieces of
evidence. More evidence on a pair comes from another judge, or from the owner.

**The owner's answers are constraints, not votes.** Before 2026-09-28 the owner's pick was a
weighted BT vote (k = 10) with a per-pair veto (RK-2, now retired). Under the new ruling the
owner answers a card after the work has moved on. That answer is a verdict on a real choice, not
one more sample. So:

- An owner answer on a card fixes the order of the pairs it covers: "keep a" places `a` before
  each alternative on the card; "switch to b" places `b` before `a`. These go in
  `owner_constraints`.
- The final order is the fitted order with the least change that satisfies every owner
  constraint. `decided_by` says `owner` for those pairs.
- The judges' votes on those pairs stay in the journal. They are not dropped, because judge trust
  compares them with the owner's answer. They no longer move the order of that pair.
- If the owner's constraints form a cycle (a over b, b over c, c over a), `rank` applies none of
  the constraints in the cycle, orders those attempts by the fit, and names the cycle in
  `warnings`. The newest answer does not silently win.
- An owner answer makes the label: `label` is the owner's pick, whatever `confidence` says.

**Bootstrap intervals.** N is at most 8, so a bootstrap is cheap. Resample the judge votes
B = 1000 times, refit, and read:

- the 5th and 95th percentile of each strength (the interval),
- `confidence`: the share of refits in which the leader beats every other survivor in its tier.

Owner constraints are applied after each refit, so they hold in every refit.

**The label threshold.** `confidence` at or over 0.9 gives a label. This is a fit parameter, a
starting value to tune from the journal. It no longer stops any question to the owner, because
the owner is never asked inline.

**Parity (Python oracle, then Go and Rust).** A bootstrap is random, and three languages must
agree on golden cases. So:

- The generator is splitmix64, written out in each language.
- The seed is the first 8 bytes of sha256 over the canonical form of only the fields the fit
  reads: attempt ids, content hashes, elimination and quality inputs, cost, the comparisons, the
  owner constraints and the policy. `task`, `project`, `where` and `previews` are left out, so a
  renamed screenshot cannot change a label. The canonical JSON follows Python's
  `json.dumps(sort_keys=True, separators=(",", ":"))`; Go already has a package that writes JSON
  the Python way (`clients/go/internal/pyjson`).
- Golden cases always pass `comparisons` inline, never from the journal.
- The fit iterates in a fixed order, as eloEngine's Zermelo loop does
  (`parametric_algorithms.dart`, `bradleyTerryRanking`), with a fixed iteration cap and tolerance.
- Every float is rounded to 9 decimals before any threshold test.
- Golden cases compare `status`, `order`, `decided_by`, `label`, `next` and `stop` exactly, and
  floats within 1e-9.

## The judge step

`daisugi rank judge` is the only part that calls a model. It follows the research note, section 3:

- **Position swap.** Call the judge twice, with the pair in both orders. A win counts only when
  both calls agree (note [10]; GPT-4 agreed with itself in only 65% of swapped cases).
- **Hide the author.** The judge sees the task and the two artifacts, labelled A and B, never the
  model names.
- **A different family.** The judge must not be from the same model family as either attempt's
  author (note [13]). If no such judge is configured, the step records nothing and says why.
- **Length.** Record each artifact's length with the vote. Length control as AlpacaEval does it
  (note [12]) needs many votes; it is a later stage.
- **Tests come first.** A judge only ever sees attempts that share a quality tier. It never
  overrules a test (note [14]).
- The judge's model id goes into the vote's `judge` field. Its answer is untrusted text; only a
  parsed `A`, `B` or `tie` is recorded, anything else is no vote.

**The bound on judge cost.** RK-3's budget of 6 judge comparisons is retired. The bound that
remains is structural: one vote per judge per pair, so at most (configured judges) x (pairs in
the tier) votes, each two calls. With 2 attempts and 2 judges that is 4 calls. The judge step
also stops when the leader is confident. The owner asked for token care, so the default judge
list is short (one or two cheap models from a family other than the authors').

**Which pair next.** Pick the challenger: the survivor whose fitted P(leader beats it) is
closest to 0.5. Ties go to the pair with fewer votes, then to the lower id. Ask the next judge
that has not voted on that pair. This is the research note's synthesis, a top-1 form of the
"count wins and stop" idea (note [5]); it is not a published algorithm. It does not use
eloEngine's `nextMatch()` heuristic, and it does not use eloEngine's convergence floor of
max(N, 20) matches (`engine.dart`, `_updateConvergence`).

## Never block, except on a permanent step

This replaces the old ask policy.

### The rule

A choice never stops work unless the step that the choice leads to is irreversible.
"Irreversible" means the gate's `permanent` tier, as `effects.py` defines it:

- `undoable`: reads, writes inside the workspace, network gets, test runs
  (`effects.py`, `UNDOABLE_CLASSES`).
- `permanent`: every other effect, including `unknown`. The classifier is an allowlist, so a call
  it cannot place is permanent. That keeps the rule fail closed: when in doubt, it asks.

### What the runner does

1. Run `rank`. If `status` is `none_survived`, stop that branch (fail closed).
2. Otherwise take the `leader`, even when `status` is `provisional`. Record the choice (see
   "What gets journaled"). If there were two or more survivors, the choice opens a card.
3. Look at the next step built on the leader.
   - Its effects are all `undoable`: go on. The owner sees the card later.
   - Any effect is `permanent`: the gate asks now, through the existing permanent-tier ask. The
     operator types the pane name to allow it (`effects.py` module text; `gate.py`, the tier
     rank `_TIER_RANK`; `ask.py`). The ask shows the card, so the operator sees the
     alternatives at that moment. An answer there closes the card as `confirmed` or
     `overridden`.
4. Keep the alternatives where they are (`attempts[].where`), so a later pick can reach them.

No new ask path is needed. The permanent-tier ask exists; the only new part is that the ask
carries the card.

The runners are the parent in a delegation tree, weave, and a coppice task that starts several
attempts. Each calls `rank` and then follows these steps. The sprig use is **Proposed**.

## The review queue

### What a card is

One card per choice with two or more survivors. A card must be readable fully out of context: in
a line at the store, or while feeding a baby. So it assumes nothing on screen but itself.

| Part | What it shows | Where it comes from |
|---|---|---|
| Headline | One line: what was chosen and what else was on the table. For example "Kept the small fix (a) over a rewrite (b) and one failed try (c)." | `leader`, the survivors, `eliminated`, and each attempt's one-line summary in `previews` |
| Why | Plain words, no jargon. For example "Both passed all tests. Two reviewers preferred a. a changed 12 lines, b changed 140." | Written from `decided_by`, the reasons in `eliminated`, the test counts and the cost. A template, not a model call, so it cannot drift from the facts. |
| What switching would do | The honest cost of taking an alternative now. For example "Switching undoes 3 later steps and applies b." or "Cannot undo: a push ran after this choice. Switching starts a follow-up task." | The switch cost (below) |
| Drill-down | The diffs, the previews, the test output, the judges' votes, the fit's scores and intervals, the reasoning in each attempt's transcript. | The journal, the attempt's `where`, the ranking output |
| Answer | Keep, switch to one alternative, or drop. | The owner |

Drop means "the recommendation was kept". It is an owner act, so it records `confirmed`.

### Where the queue lives

The phone is its natural home. The queue is a coppice view on the floor, not a separate page.
On the phone a card slides in over the floor, as other panes do. The two review poles of the
2026-09-26 version become two drill-down levels of one card:

- **See no code.** The headline, the why, the screenshots or a running preview, the feature list,
  the tests as a count, the cost. Eliminated attempts folded, one reason line each.
- **See the code.** The chosen diff and one alternative's diff side by side, and a plain unified
  diff against the base. A full review tool like GitHub's is not in scope.

One switch on the card moves between them. The answer is the same record either way.

### Sort

The owner sorts the queue by any of:

| Key | Meaning |
|---|---|
| Reversibility | The switch cost, cheapest first. A card about to decay sorts to the top here. |
| Impact or complexity | The size of the difference between the chosen and the best alternative: diff lines and files touched, and how close the fit was (a low `confidence` is more worth a look). |
| Date | When the choice was made. |
| Project | The `project` field. |

### Switch cost

The switch cost says what a later pick would take. It is computed when the card is shown, from
the journal only:

- **Cheap.** No work was built on the chosen option yet, or every later step has a reversal the
  ledger can undo (`Receipt.reversibility` is `none` or `reversible`, `models.py`). Switching
  undoes those steps from the ledger (`deeds.py`) or restores a checkpoint (`checkpoints.py`,
  `restore`), then applies the alternative from its worktree.
- **Costly.** Later work built on the chosen option can be undone, but switching throws it away.
  The card says how much: "undoes N steps, M files".
- **Follow-up only.** A later step has `reversibility` `irreversible`, or `deeds.py` would skip
  it, or `checkpoints.py` `restore` would refuse because a path is not fully recoverable, or the
  alternative's worktree is gone. Then undo is not honest. The card says so, and a switch starts
  a follow-up task that carries the alternative forward on top of the current state.

A switch never runs a permanent step on its own. The follow-up task goes through the gate like
any other task.

### Decay

An unreviewed card closes as kept at the first of:

- the switch cost reaches **follow-up only**: switching is now expensive, and keeping the card
  open would only nag;
- a set time passes since the choice.

A decayed card records `ignored`, not `confirmed`. Decay is not an owner act.

**Open, the decay time and the "expensive" line.** (a) Time only. (b) Follow-up only, or time,
whichever is first. (c) Also close when the costly count passes a number of later steps.
**Recommendation: (b), with 7 days as the starting time.** It is the ruling's own wording, and
the one extra signal it needs (a follow-up-only switch cost) comes straight from the ledger.
Tune the time from how often the owner answers old cards.

### A later pick

When the owner answers "switch to b":

1. Record the answer (`overridden`, with `b`).
2. If the switch cost is cheap or costly: undo the later steps built on the chosen option from
   the ledger or a checkpoint, then apply `b` from its worktree. Each step of this goes through
   the gate.
3. If the switch cost is follow-up only: start a follow-up task that carries `b` forward. The
   card said so before the owner answered.

**Only the operator may answer a card.** An agent that answers its own card would poison every
label, and could make the runner undo other work. The coppice route that records an answer must
refuse any connection coppice does not place as the operator, as `agent.allow` does
(`PROTOCOL.md`, Agents). This is not a boundary against the user's own processes: any process of
the owner's uid can run `daisugi rank answer`. The gate is the bound there: an agent's envelope
should not allow the `daisugi` shell head. No such self-protection rule was found in `gate.py`
by grep; not verified.

A card answer is a label, not an allow. The rule "a plugin can propose. It cannot allow."
(`PROTOCOL.md`, Plugins) still holds: an answer widens no envelope. A switch runs its steps
through the gate like any other work. The floor supervises and does not decide (ADR-0020):
coppice shows and relays; daisugi fits and records.

### What coppice must change

The view plugin model cannot record an answer today:

- A view runs in a sandboxed frame and holds no token (`plugins/README.md`;
  `internal/web/views.go`, `viewCSP`).
- The floor page posts a view only panes, tasks, events and the selection
  (`plugins/_lib/view.js`). It posts no cards.
- A view may post back only `open-pane`, `set-sel`, `watch` and `close` (`plugins/_lib/view.js`;
  the floor page checks them in `internal/web/static/views.js`).

So a `cards` view needs two additions:

1. **A data kind that carries the queue.** A guarded web route (next to `/api/tasks` and
   `/api/events` in `views.go`) runs `daisugi rank cards --json` and the floor page posts the
   result, with the previews and diffs, to the view.
2. **An `answer` post-back.** The view posts `{type: 'answer', choice_id, outcome, pick}`. The
   floor page accepts it only for an open card it sent, then sends it on a guarded,
   operator-only route that runs `daisugi rank answer`.

## What gets journaled

No new database. Two kinds of append-only JSONL record, under `<data_dir>/journal/rankings/`,
as the gateway journal and the gate's audit log already are (`gateway_journal.py`;
`clients/go/internal/journal/report.go`). A record is never edited. A state change is a new
record.

**A comparison**, one per judge call pair (unchanged):

| Field | Meaning |
|---|---|
| `ranking_id` | The ranking. |
| `a`, `b` | Attempt ids, and `a_hash`, `b_hash`: the content hashes shown. |
| `shown` | The order shown: `ab` or `ba`. |
| `outcome` | `a`, `b`, `tie`, or `skip`. |
| `judge_id` | The model id. |
| `pair_id` | Links the two swapped calls of one judge comparison. |
| `lengths` | The artifact lengths shown. |
| `ts` | Time. |

**A choice**, the one small row the router and the garden learn from:

| Field | Meaning |
|---|---|
| `choice_id` | One choice. |
| `ranking_id`, `project`, `task` | What the choice was about. |
| `options` | The survivors' ids and content hashes, and the eliminated ids. |
| `chosen` | The leader the runner took. |
| `status` | The rank status at the time (`ranked`, `provisional`, `single`). |
| `event` | `opened`, or a close: `confirmed`, `overridden`, `ignored`. |
| `pick` | For `overridden`: the alternative the owner picked. |
| `how` | For a close: `answer`, `drop`, `permanent_ask` or `decay`. |
| `trigger`, `fired_at` | For a decay: `time` or `follow_up_only`, and when the condition first held. |
| `ts` | Time. |

A card is a view: the fold of a choice's records, plus the switch cost computed from the ledger
when it is shown. A card with an `opened` record and no close is open.

**Learning.** Only `confirmed` and `overridden` are owner labels. `ignored` is not a
confirmation; the router and the garden must not train on it as one. It is kept, because how
often cards decay unread is itself a signal.

**Bulky data stays out of the journal.** The alternatives' diffs and builds live in their
worktrees (`attempts[].where`) and go with normal worktree cleanup. When a worktree is gone the
card can no longer switch by undo, and says "follow-up only".

**Reading creates nothing.** Viewing the queue or running `rank show` must not create any file
or database, the same rule the oracle now follows for `status`, `modules` and `dashboard` (K4-8
in `progress.md`). RK-6's index table may live in the existing journal index
`<data_dir>/journal/index.db`, which Go and Rust already read and write
(`clients/go/internal/tracejournal/journal.go`, `clients/rust/src/tracejournal/`). It is
rebuildable from the JSONL files, and no card state lives only in SQLite.

A comparison or answer whose content hash no longer matches the attempt is ignored with a
warning. So a vote binds to exactly what the judge or the owner saw.

## How the consumers use it

| Consumer | Calls rank with | Reads | Card? | State |
|---|---|---|---|---|
| Router part 3 | {the cheap model's side answer, the frontier answer} | `quality_tiers`, `label`: "as good" when both share tier 1 and the preference stage does not confidently favor the frontier; "not as good" when the cheap one is in a lower tier or eliminated; `no_label` otherwise | No. Nothing proceeds on this choice; it only makes a label | Designed (router part 3 is not built) |
| Router part 4 | nothing new | Counts labels and `no_label`s per week, and the choice rows' owner labels | n/a | Designed |
| Garden A/B | {pathway run, fresh run} when both ran | Promote when the pathway is in tier 1 and never eliminated over the last W rankings; drop on elimination. The structural envelope check in `ab_test.py` stays as a cheap filter before it | Yes, when a promotion changes what later runs do | Designed. Needs outcomes: `ab_test.py` does not execute plans, by design |
| A parent in a delegation tree | its children's attempts, with `edge_proof` | `leader`, `status` | Yes | Designed for daisugi and coppice; the sprig use is **Proposed** |
| weave | a step's retries or alternatives | `leader`, `status` | Yes | Designed (ADR-0022, accepted in part) |
| coppice cards view | open choices | the cards | is the queue | Designed; needs protocol work (above) |

## What to port from eloEngine

eloEngine is pure Dart, MIT (`OpenHearth/eloEngine/README.md`). Port by reading, then write the
Python oracle first.

| Port | From | Change |
|---|---|---|
| Kendall tau | `elo_math.dart`, `kendallTau` | Handle ties (tau-b), because quality tiers make ties |
| BT fit (Zermelo iteration) | `parametric_algorithms.dart`, `bradleyTerryRanking` | Add the prior; return strengths, not only an order |
| HodgeRank cyclic magnitude | `parametric_algorithms.dart`, `hodgeRanking` | Run per judge; report "not measurable" under the minimum. Do not port `harmonicMagnitude`: it is the share of uncompared pairs, not the Hodge harmonic part |
| Closeness to 0.5 | `engine.dart`, the closeness term in `nextMatch()` | Use the BT win chance, not Elo ratings |

Do not port:

- The 15-algorithm ensemble and its harmonic-mean consensus (`engine.dart`). At N of 8 or less,
  BT with a bootstrap is enough, and one method is easier to keep equal across three languages.
- Online Elo, Glicko-2 and TrueSkill. Attempts do not change skill over time, and an online
  update depends on vote order (note [2]).
- The convergence floor max(N, 20) (`engine.dart`, `EloConfig`).
- `EloMatch` as the vote record: it has no judge id (`models.dart`).
- `EloMerge` (`merge.dart`) for now. Its minimum strategy ("every judge must like it") could
  become a strict mode later.

## Judge trust

Two measures, per judge, from the journal:

- **Agreement with the owner.** On every card the owner answered (`confirmed` or `overridden`)
  where the judge voted on the pair, the share where they agree. Over whole rankings, Kendall tau
  between the judge's order and the owner's order.
- **Inconsistency.** HodgeRank's cyclic magnitude over one judge's votes: the share of the vote
  flow no single order explains. It is always 0 for 2 attempts. It is only measurable when the
  judge's votes in one ranking cover at least 3 attempts and at least 3 pairs that close a loop.
  Below that, `rank` reports "not measurable", never 0.

Use: a judge whose agreement with the owner drops under a floor is dropped from the default
judge list, and `rank` says so (**provisional ruling RK-5**: fixed weight 1 with a drop floor; a
learned weight waits for enough answers).

**No calibration questions.** RK-9 (1 in 10 owner questions on pairs the judges settled) is
retired. Every card the owner answers is already a calibration point, because the judges voted
on it first. The cost: the owner picks which cards to read, so the sample leans toward cards
the owner found worth a look (for example low `confidence`). Judge trust should report how many
answered cards it rests on, and treat the number as biased toward the hard cases.

## Where it lives

**Provisional ruling RK-7:** a daisugi command beside `verify`, with the coppice cards view over
it.

- **Python oracle:** `src/opendaisugi/rank.py`, beside `verify.py`. The fit is pure and needs no
  new dependency (no numpy call is needed at N of 8 or less).
- **Go:** `clients/go/internal/rank`. **Rust:** `clients/rust/src/rank.rs`. The command joins the
  command lists in `clients/go/internal/cli/cli.go` and `clients/rust/src/cli/mod.rs`.
- **Golden cases:** a `rank` family in the conformance corpus (`docs/spec/conformance.md`),
  recorded from the oracle. The switch cost and decay rules get cases too, over fixture ledgers.
- The judge step calls models, so it is Python first and ported last, the same way the other
  model-calling parts are.

### Commands

```
daisugi rank ATTEMPTS.json [--json]            fit, and print the ranking and the next judge pair
daisugi rank judge --ranking R [--model M]     run the next judge comparison (two calls), record it
daisugi rank choose --ranking R [--json]       record the choice the runner takes; open a card
daisugi rank cards [--sort reversibility|impact|date|project] [--open] [--json]
                                               the review queue, with switch costs
daisugi rank answer --choice C keep|drop|switch:<id>
                                               record the owner's answer; operator only
daisugi rank show --ranking R [--json]         the ranking, its votes and its choice, from the journal
```

Exit codes for `daisugi rank`: 0 when `status` is `ranked` or `single`; 3 for `provisional` (a
leader exists; a loop may run `rank judge` while `next` is not null); 1 for `none_survived`; 2
for bad input. The runner proceeds on 0 and 3, and stops on 1 and 2.

Decay needs no daemon. `rank cards` computes decay when it reads: an open card whose decay
condition holds is shown as closed, and nothing is appended. Only a writer (`rank choose`,
`rank answer`, or the runner) appends the `ignored` record. That record names the trigger that
fired (`time` or `follow_up_only`) and the time it fired, not the time it was written, so a
replay does not depend on which writer ran next.

### Wire shape (abridged)

```json
{
  "ranking_id": "r-7f3a",
  "status": "provisional",
  "eliminated": [{"id": "c", "reasons": ["required test failed: test_login"]}],
  "quality_tiers": [["a", "b"], ["d"]],
  "order": ["a", "b", "d"],
  "decided_by": ["preference", "quality"],
  "scores": {"a": {"strength": 0.58, "lo": 0.41, "hi": 0.72}},
  "leader": "a",
  "confidence": 0.71,
  "label": "no_label",
  "next": null,
  "stop": "judges_exhausted",
  "owner_constraints": [],
  "warnings": []
}
```

The runner proceeds on `a` and opens a card: "Kept a over b (d ranked lower, c failed a test)."

## What exists and what is missing

| Part | State | Where |
|---|---|---|
| Verify result per plan | exists (Python, Go, Rust) | `verify.py`, `clients/go/internal/verify`, `clients/rust/src/verify.rs` |
| Effect tiers `undoable` and `permanent` | exists | `effects.py`, `gate.py` |
| Permanent-tier ask | exists | `gate.py`, `ask.py` |
| Receipts with `reversibility` | exists | `models.py`, `Receipt` |
| Undo from the ledger | exists | `deeds.py` |
| Checkpoints and restore | exists | `checkpoints.py` |
| Edge proof for a child | designed | `design-delegation-tree.md` |
| Garden A/B | exists as a library; **no caller in `src/`** (grep finds only exports and types in `gardener/__init__.py`, `report.py`, `regression.py`) | `gardener/ab_test.py` |
| A/B token cost | placeholder: `tier2_tokens=0`, and the code says not to trust it | `gardener/ab_test.py` |
| Test results as structured input | missing. No harness reports tests per attempt | |
| Pass/fail feature list | missing. Not found in `src/` or `harness/` | strategy notes |
| Journal with Go and Rust access | exists | `journal.py`, `clients/*/tracejournal` |
| Comparison and choice records | missing | |
| BT fit, bootstrap | missing in daisugi; prior art in Dart | `OpenHearth/eloEngine/lib/src/` |
| Model judge with swap | missing | |
| Switch cost, decay, pivot | missing | |
| coppice worktree tasks (where parallel attempts run) | exists | `internal/server/tasks.go`, `PROTOCOL.md` Tasks |
| Cards view, `answer` post-back, queue data kind | missing | `plugins/_lib/view.js`, `internal/web/views.go` |
| Router labels | designed (router parts 3 and 4) | `design-router.md` |

## Risks

- **Judge bias.** Position, length and self-preference bias are real (note [9] to [13]). The
  swap, the hidden author, the other-family rule and tests-first cover the known ones. Code
  judges stay order-sensitive (note [11]).
- **Judges that agree with each other, not with the owner.** Two judges from one family share
  biases. Answered cards measure this, with the selection bias named in "Judge trust".
- **Work built on a wrong choice.** The runner proceeds on a provisional leader, so some work
  will be built on the option the owner would not pick. The cost of that is bounded by the switch
  cost, and the card says it plainly. The permanent-tier ask stops the one case that cannot be
  undone.
- **Decay closes a card the owner would have overturned.** Decay records `ignored`, not
  `confirmed`, so no label is learned from it, and a switch through a follow-up task is still
  possible. What is lost is the cheap undo.
- **A switch cost that lies.** If the ledger says "reversible" and the undo fails, the card
  misled the owner. `deeds.py` and `checkpoints.py` refuse rather than half-undo; the switch must
  surface that refusal and fall back to a follow-up task, never report success.
- **Gaming.** An attempt can learn to please the judge: longer, more confident, more
  comments. Mitigations: tests eliminate and rank before any judge; judges never see each other's
  reasons; lengths are recorded so a length bias can be measured; the owner's answers constrain
  the order. An agent that answers a card is covered in "Only the operator may answer a card". An
  agent that edits its own tests to pass is not covered by rank; the envelope must deny writes to
  required tests, and that is a policy question for each task.
- **Weak quality signals.** With no tests and no feature list, every survivor lands in one tier,
  and the ranking is only opinion. `rank` reports that in `warnings`, and a consumer may refuse
  to train on it.
- **Thresholds from nowhere.** 0.9, 1000 refits, the prior of 1.0, the 7-day decay: none is
  measured. They are starting values to tune from the journal after the dogfood week.

## Staged plan

Each stage: Python oracle with tests first (TDD), then Go and Rust on the same golden cases.
One heavy job at a time on the box. The dogfood week comes before any of it (strategy notes,
"The worry, and the answer").

1. **Records.** The comparison and choice records, `rank show`, the JSONL store. Golden cases
   for the record format.
2. **The pure fit.** Elimination, quality tiers, BT with the prior, the seeded bootstrap, owner
   constraints. `daisugi rank`. The largest set of golden cases, including N = 1, N = 2, all
   eliminated, a disconnected graph, an owner cycle, a stale hash.
3. **Choices and cards.** `rank choose`, `rank cards`, `rank answer`; the switch cost from the
   ledger; decay. Golden cases over fixture ledgers.
4. **The permanent-step join.** The runner's rule, and the card carried on the permanent-tier ask.
5. **The judge step.** `daisugi rank judge`: swap, hidden author, other-family rule, no vote on
   error. Ported last, as the other model-calling parts are.
6. **First consumer: the router's labels** (router parts 3 and 4). This measures how often rank
   yields a label at all.
7. **The cards view** in coppice: the queue data kind, the `answer` post-back, the operator-only
   route, both drill-down levels, the phone layout.
8. **The pivot.** Switch by undo and worktree, and the follow-up task.
9. **Judge trust.** Kendall tau and per-judge cyclic magnitude from answered cards.
10. **Garden A/B, the delegation tree parent and weave**, when they have attempts with outcomes to
    rank. The sprig use is **Proposed**.

## Open questions and rulings

The numbers match the RK rulings in `DECISIONS-PENDING.md`.

1. **Where cost goes in the order.** Provisional ruling RK-1: (c), quality, then preferences,
   then cost.
2. **Weight of the owner's pick.** Retired 2026-09-28 by RK-10. The owner's answer is a
   constraint on the pairs it covers, not a weighted vote.
3. **Stop threshold and budgets.** Retired 2026-09-28 by RK-10. The 0.9 threshold stays only as
   the label threshold; the judge bound is one vote per judge per pair; there is no owner budget.
4. **A daily cap on owner questions.** Retired 2026-09-28 by RK-10. No caps.
5. **Judge weight from trust.** Provisional ruling RK-5: fixed weight 1 and a drop floor first.
6. **Where comparisons are stored.** Provisional ruling RK-6: JSONL as the source, plus a
   rebuildable index in the existing journal index. Consistent with RK-10's "no new database".
7. **Where it lives.** Provisional ruling RK-7: a daisugi command, with the coppice cards view
   over it. The sprig use stays **Proposed**.
8. **An operator override.** Provisional ruling RK-8: keep it with a warning.
9. **Calibration questions.** Retired 2026-09-28 by RK-10. Answered cards measure judge trust.
10. **Never block on a choice.** Owner ruling RK-10, 2026-09-28. This design's "Never block,
    except on a permanent step", "The review queue" and "What gets journaled" carry it.
11. **Open: the decay time and the "expensive" line.** See "Decay". **Recommendation: follow-up
    only or 7 days, whichever is first.**

## Conflicts found

- **The literal order makes preferences degenerate.** The strategy lists cost among the measured
  scores that rank before preferences. See "The lexicographic order".
- **`ab_test` has no caller.** The strategy says "the garden's A/B test promotes with them". The
  A/B test is not wired into any flow in `src/` today.
- **`ab_test`'s judge fallback is fail-open.** A judge that raises is logged and the structural
  verdict stands, so the result can pass with no judge verdict (`ab_test.py`, the `except` around
  `judge`). rank treats a judge error as no vote. Fixing `ab_test.py` is outside this design.
- **`tier2_tokens` is a placeholder 0** (`ab_test.py`). No cost comparison may read it.
- **coppice's docs disagree on post-backs.** `plugins/README.md` says a view may post back three
  things; `plugins/_lib/view.js` says four (it adds `close`).
- **ADR-0022's header still lists data flow between steps as open.** The owner ruled WV-1 (typed
  slots) on 2026-09-28. The ADR was not edited here.
