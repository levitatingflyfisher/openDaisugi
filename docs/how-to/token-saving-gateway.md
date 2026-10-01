# Save tokens across any harness with the gateway

*How to run the token-saving gateway in front of an agent you already use —
Claude Code, Codex, pi, opencode, grok — and read back what it saved. It works
on whole agent turns, with no step distillation required.*

The gateway is a small local proxy that speaks the Anthropic Messages API. You
point a harness at it with `base_url`; it forwards each whole turn to the real
provider, and before forwarding it routes an easy turn onto the cheap model. The
provider still does the work and still authenticates you — the proxy carries the
credential it is handed straight through and never needs an API key of its own.

## What class of thing this is, stated up front

- **This is a *saver*, and it fails open.** If the gateway can't parse or route a
  turn, or the cheap model rejects the routed body, the original turn goes to the
  provider untouched. You lose the saving on that turn, never the turn itself.
  That is a deliberate, safe default *for a saver* — it is the whole reason the
  saving path is kept separate from enforcement.
- **This is not the safety gate.** Action governance lives in the call-time gate
  ([ADR-0007](../adr/), fail-*closed*, on the per-harness tool hook — see
  [how-to/gate.md](gate.md)). The gateway routes and meters; it never decides
  whether an action is *allowed*. Run both: the gate for safety, the gateway for
  cost.
- **The routing signal is a transparent text heuristic, not a trained model.** It
  reads the latest human ask and keeps a hard-looking turn on the requested
  model. Its one residual blind spot is an easy-*sounding* ask whose work turns
  out hard; that risk is inherent to any pre-dispatch text router, and the
  conservative rule below bounds the damage to "you paid what you'd have paid".

## Run it

```bash
uv add 'opendaisugi[gateway]'          # httpx + uvicorn
daisugi gateway                        # listens on http://127.0.0.1:8787
```

Then point a harness at it. For Claude Code:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude
```

That is all. Every turn now flows through the proxy.

### One command to wire it (install)

`daisugi install` can write that pointer for you, alongside the skill, the MCP
server, and — if you ask — the fail-closed verify hook:

```bash
daisugi install --gateway --gate            # save (base_url) + verify (audit gate)
daisugi install --gateway --gate --enforce  # same, but the gate denies out-of-envelope calls
```

`--enforce` needs a registered envelope, or the hook would deny every call;
with none, install refuses and writes nothing. Run
`daisugi gate init --workspace DIR` first for a starter envelope.

`--gateway` and `--gate` are both **opt-in** — a plain `daisugi install` wires
only the skill, MCP, capture, and instructions, and changes nothing about your
model routing. Every write is idempotent, backed up, and reversible with
`daisugi install --uninstall`; `--dry-run` shows the exact edits first. The
gate is **audit by default** ([ADR-0007](../adr/)) — it observes and never
denies until you pass `--enforce`.

Harness coverage is stated honestly, not faked. The base_url pointer is wired
for **Claude Code** and **OpenClaw** (both speak the Anthropic Messages wire) and
for **Codex** (the gateway also answers the OpenAI `/chat/completions` wire);
**Hermes-custom** expects an OpenAI wire that `install` does not wire yet, so
`--gateway` reports it as an *honest gap* rather than handing you a broken config. The verify gate is wired for **Claude Code**;
OpenClaw needs a small JS shim and Codex has no external gate at all (its safety
rides the MCP boundary) — both surfaced in the install output, not silently
dropped ([ADR-0013](../adr/)).

## What it does to each turn

1. **Routes by the latest human ask.** A tool-loop continuation (a `user` turn
   that carries only a `tool_result`) has no human text of its own, so the router
   walks back to the ask that governs the loop and routes the whole loop the same
   way — session affinity, which also keeps one session on one model and spares
   the model-keyed prompt cache from thrash. An easy ask routes to the cheap
   model; a hard ask stays on the requested model. **No signal at all keeps the
   requested model** — an absent ask never triggers a downgrade.
2. **Swaps the model** in a copy of the body (never the original), then forwards
   with your `Authorization`, `anthropic-version`, and `anthropic-beta` headers
   intact.
3. **Streams the reply back byte-for-byte** while reading the `usage` off the SSE
   events in passing — it never buffers the stream to measure it.
4. **Journals the turn** to `~/.opendaisugi/gateway/turns.jsonl`.

A harness can name its pane. A base URL of the form
`<gateway>/pane/<floor>/<pane>` (the floor is 16 lower-case hex digits and the
pane is `w<n>:p<n>`, as coppice names them) is routed as the plain
`<gateway>/...` path: the gateway takes the prefix off before it routes, so the
upstream never sees it, and it adds `"pane"` and `"floor"` to that turn's
journal line. A line with no well-formed prefix has neither key.

The gateway says that it takes the prefix: `GET /_caps` from the same machine
answers `{"pane_prefix": true}`. Like `/_reload`, it is answered by the gateway
itself, loopback only, and never sent on to the model provider. coppice asks once
for each gateway address, and sets the prefix on the base URL of an agent it
starts only when the answer is yes, so its floor can show the tokens saved for each
agent. An older gateway has no such route and does not strip the prefix; coppice
then leaves the base URL plain, and the pane works as before with no savings figure
of its own. Restart the gateway after you update daisugi to turn it on.

If the cheap model returns a 4xx on the swapped body (say a `max_tokens` above its
ceiling), the turn is retried once on the original model before the client sees
anything — a pre-dispatch decision, so a committed stream is never unwound. A turn
served that way is booked as *not* downgraded: no saving that didn't happen. The
rejected attempt is not free — a 4xx rarely bills tokens but still counts as one
request against your rate limit — so this path is a safe fallback, not a no-op.

## Read what it saved — tokens first, dollars alongside

```python
from opendaisugi.gateway_journal import GatewayJournal

s = GatewayJournal(path="~/.opendaisugi/gateway/turns.jsonl").summary()
print(s.frontier_tokens_saved)  # frontier-quota tokens preserved (the binding constraint)
print(s.dollars_saved, s.blended_multiplier)  # and how cheap those tokens were
print(s.repeats)  # asks you made more than once
```

Two currencies, because they answer different questions. On a subscription plan
the wall you actually hit is the **frontier quota** — the pool that returns a 429
— so the resource the gateway conserves is the frontier tokens a downgraded turn
did not spend. That is `frontier_tokens_saved`. **Dollars** (`dollars_saved`,
`blended_multiplier`) show how cheap those tokens are on a metered API key. The
meter reads the prompt-cache buckets, so a heavily-cached turn is priced
correctly — and on such turns the two numbers diverge sharply: a large quota
relief for a modest dollar delta, because cache reads are already cheap.

The dollar counterfactual is an **estimate** (flagged as one on every record): it
prices what the frontier model *would* have cost at the same token split, and does
not model the frontier's own warm cache. Actual spend is always exact, taken from
the model's own usage report.

## Learn from outcomes: the knn router

The rules router uses a guess: short and plain tasks are easy. The knn router
learns instead. It sends a turn to the model that did well on similar past tasks.

```bash
daisugi install --gateway --router knn   # or: daisugi gateway --router knn
```

How it works:

- After each routed turn the gateway books one row in
  `<data-dir>/gateway/routing.db`: a hash of the task, its vector (from the
  matcher you already use for pathways), the model, the cost, the time and an
  outcome. The text of the task is not stored.
- A turn is **bad** when the upstream answers with an error or the model refuses.
  The next new ask in the same conversation, within 10 minutes, labels the turn
  before it: a near copy of the earlier ask is a retry, so the earlier turn was
  **bad**. An ask that moves on makes it **good**. Every other turn stays
  **unknown**.
- To route a turn, the router takes the 8 nearest past turns. With at least 3
  known outcomes, it scores each model by the share of good outcomes (weighted by
  similarity, smoothed by one) less a small cost term, and picks the best. With
  fewer, it says so and the rules router decides. The router never picks a model
  above the one the harness asked for. Only rows made by a model the router may pick count as known. A cheaper model
  wins only with at least one good row and a success value at or above the
  requested model's.
- It does not try cheaper models to learn about them. Its first cheaper-model
  rows come from the rules router, which decides while knn has too few outcomes.
  Cache stickiness (ADR-0015) applies on top, unchanged.

Every turn in the journal carries the reason line, for example
`knn: claude-haiku-4-5; 6 neighbours, 5 known; scores claude-haiku-4-5 0.79, claude-opus-4-8 0.55`.
A booked turn also carries its `turn_id`.

Check and correct it:

```bash
daisugi route "rename the helper in the parser" --knn   # the choice and its nearest turns; books nothing
daisugi router status                                    # counts by outcome and the last turns
daisugi route feedback t12 bad                           # set an outcome by hand
```

The numbers and the rules are in ruling RT-R-1 to RT-R-7 and RT-R-3a of
`clients/ADJUDICATIONS.md`. The Go and Rust binaries do the same, and the three
are checked on one fixed outcome table.

## Pick how smart the router is

The router slot holds five choices, from no thinking to a model that thinks:

| `gateway_router` | What decides | Cost per turn |
|---|---|---|
| `off` | Nothing. The turn goes as sent and is metered. | none |
| `rules` | A heuristic: short and plain means easy. | none |
| `knn` | Past outcomes. Cosine over the matcher's vectors; no model call. | one local embed |
| `model` | A model you name picks from the allowed names. | one short model call |
| `switchyard` | NVIDIA NeMo Switchyard, a trained router. | its own |

`knn` and `model` never pick a model above the one the harness asked for. A bad
pick, an error or a timeout falls back to the rules router, and the reason line says so.

**The model router.** Set `gateway_router: model` and `gateway_router_model: <model>`
in `config.yaml`. For each turn with new text, and a cheaper model to pick,
the gateway asks that model, through your own upstream, to name one of the
allowed models (the requested one and the cheaper rungs). The answer must be one
of those names. A tool-loop step is not asked: it stays on the model the
conversation used last.

**The grader.** Set `gateway_grader: <model>` (default `off`). After the reply is
sent, the gateway asks that model one question: did the answer do the task, good
or bad. It asks only when the reply is a finished answer, not a tool call. The answer goes to the outcome table, above the automatic labels. A grade
from you (`daisugi route feedback`) always wins over the grader. A grader error or
an odd answer writes nothing and logs one line. The grader reads the task and the
answer, so it sees what your upstream sees. It needs `knn` or `model`, which book
the turns.

A small learned net that picks from the vectors and the grader labels is planned,
once enough labels exist. It will fill the same slot.

## Compose with a trained router: NeMo Switchyard

The gateway can hand the model choice to NVIDIA NeMo Switchyard and stay the
meter. Use `daisugi gateway --router switchyard`. The gateway starts
Switchyard on loopback, sends each turn to its route, and journals the target
that served the turn. Do not point `--upstream` at a Switchyard endpoint by
hand. The rules router would then send model names that Switchyard does not
know. See [Hand model choice to NeMo Switchyard](router-switchyard.md).

## Reuse — the opt-in tools (Phase 2)

Beyond routing, the larger multiplier comes from **reuse** — answering a repeated ask
without the frontier model. It is here as tools the harness *opts into*, never a
transparent intercept: silently returning a cached result would be the gateway driving
the harness, which the assurance layer forbids ([ADR-0004](../adr/),
[ADR-0012](../adr/)).

- **`daisugi distill-repeats`** ranks your repeated asks by the frontier spend they
  represent (tokens first, dollars alongside) and flags which are already reusable — the
  worklist of what is worth reusing first.
- the **`recall`** MCP tool returns a *verified* reusable plan for an ask — re-checked
  against your own envelope before it is trusted — or a miss, so a harness can try reuse
  before the model and fall open on a miss.
- the **`recall_answer`** MCP tool serves a *freshness-gated* past answer for a plan-less
  ask (it must clear confidence, age, and ground-shift), or a miss.

The answer store is the one place the gateway keeps **raw response text on disk** — at
`~/.opendaisugi/gateway/answers.jsonl`, a bounded ring of the newest 1000. It is opt-in:
build the facade with `answer_store=False` to keep no answer content at all. (The turn
journal beside it, `turns.jsonl`, holds only ask text and token counts — never responses.)

Answer capture is now wired into the live proxy (Phase 3): start the gateway with
`daisugi gateway --capture-answers` and a plain-text answer to a signed ask is journaled as
it streams, byte-for-byte passthrough untouched (off by default — it is the one place raw
response text is kept). To see a day of work calibrated — realized routing
(measured) beside the reuse ceiling (a best case, kept separate and labelled) — run:

```bash
daisugi gateway-report                 # three groups: routing · potential reuse · combined
```

The honest ceiling is worth stating plainly — routing is ~3× on a blended day, and reuse
adds on top only to the extent your work repeats; the "1/100th" figure is a target the
report calibrates against, not a promise this build makes. Only the routing group is
*measured*; the reuse and combined groups are ceilings that assume every repeat-after-first
reuses perfectly, which nothing yet does automatically.
