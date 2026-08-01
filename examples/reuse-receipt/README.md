# Zero-inference reuse, verified offline

The recurring win in one runnable file. No API key. The only network touch is a
one-time download of the small local embedder, cached after the first run.

```bash
python zero_inference_reuse.py
```

It seeds the distilled pathway the Distiller produces from repeated successes,
then reuses it for a matching prompt and prints the receipt:

```
=== zero-inference reuse receipt ===
prompt reused a distilled pathway : True
plan step types                   : ['shell']
synthesis used an LLM             : False
tokens spent (whole run)          : 0

final answer (from the real grep, assembled deterministically):
  Results for: Find the lines that contain TODO in the source files.

  [s1 · shell]
  <workspace>/notes.py:2:    pass  # TODO: handle the empty case
  <workspace>/notes.py:5:    return 1  # TODO: cover the negative branch

OK — reuse of a distilled deterministic pathway cost zero inference,
     and the Z3 guard re-verified it against the envelope first.
```

## What just happened (the whole ledger)

A distilled pathway is `envelope + plan_template`, and the template is a plan of
**typed steps**. This one's step is a `shell` grep. On reuse:

| Stage | Cost | Why |
|---|---|---|
| **Plan** (decompose) | **0** | the cached plan is served directly — the planner is skipped |
| **Guard** (Z3 verify vs. your envelope) | ~free | symbolic check; fail-closed, runs *every* time |
| **Do** (the `shell` step) | **0** | runs via `subprocess` — no model in the loop |
| **Assemble** (synthesis) | **0** | `--deterministic-synthesis` stitches the step outputs, no LLM |

`shell`, `file_read`, `file_write`, and `network` steps all execute this way,
with zero inference. Other step types do use a model: `task` and `agentic`
reason, `skill` and `mcp` are whatever their handler is, `vla` runs a policy — a
pathway made of those saves the planning call on reuse but still pays for the
reasoning.

## The honest part

- **Producing** the pathway costs one model call, once, offline — the
  Distiller's generalization pass, which needs at least three successful runs
  first. That cost is measured in
  [`../distillation-benchmark/`](../distillation-benchmark/). This receipt is
  about the **payoff**: once the pathway exists, a matching prompt skips the
  planner, and with `--deterministic-synthesis` the whole run spends nothing.
- Reuse fires when a prompt clears the **0.55 cosine-similarity threshold**
  against a stored pathway. It serves that pathway's plan verbatim, so the win
  is real for **work that repeats** — not for novel variations.
- "Free" is never "unchecked": the reused plan is **re-verified against your
  envelope** before it runs. A mismatch falls through to a fresh decomposition
  under your boundary; it never runs outside it.

## The same controls from the CLI

```bash
daisugi orchestrate "Find the lines that contain TODO in the source files." \
  --envelope envelope.yaml \   # skip envelope generation (itself a model call)
  --deterministic-synthesis    # assemble the answer without a model
```

`--deterministic-synthesis` drops the final synthesis call; `--budget N
--strict-budget` makes a run *stop* when the budget is exhausted instead of
downgrading; omit `--budget` to route every step to a right-sized model with no
constraint.

One honest caveat the script hides for you: the **zero-token number comes from
reuse**, and the CLI reuses only a pathway that is *already in your store*. The
script seeds one, so it spends nothing on the first run. From the CLI you get
there the normal way — run the task a few times, `daisugi tend` to distill it,
then the next matching run reuses it. A fresh store has nothing to reuse, so that
first CLI run still decomposes (and generating the envelope is a model call too,
which is why the example passes `--envelope`).
