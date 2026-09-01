# Layer Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every layer of the pipeline one front door for comparing its options (`daisugi bench`), five cross-layer pairs, hot swap for every stage the architecture can reload in process, and a runtime verifier dispatch that falls back to Python loudly and never to allow.

**Architecture:** A new `opendaisugi.bench` package holds a tiny table renderer, a corpus loader, a registry of `BenchSpec` rows, and one module per layer that turns an existing instrument (the conformance harness, `scripts/matcher_fpr.py`, the gateway router, the gate) into a table over a small committed corpus under `bench/corpus/`. A new `opendaisugi.verifier_dispatch` runs a compiled conformance client through the wire protocol in `docs/spec/conformance.md` and returns the Python oracle's result on any client failure. Hot swap is finished by making four stages actually re-read their choice per call — the embedder cache gains a public `reload()`, `tend` re-embeds rows whose provenance is stale, `resolve_backend` reads the config file below the env var, and the gateway reloads its whole `Config` on mtime change, SIGHUP, and a loopback-only `POST /_reload`.

**Tech Stack:** Python 3.12, stdlib + the packages already in `[project.dependencies]`; typer for the CLI; pytest. No new hard dependency. The compiled verifier clients (Rust/Go/TypeScript/Lean) are optional binaries already in `clients/`.

**Spec:** `docs/plans/2026-09-08-workshop/spec-11-layer-management.md` (master: `docs/plans/2026-09-08-workshop/00-master-spec.md` §5.8, §5.9, §3.5)

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 1–8 and 10 are ready to build now** — the pinned options, the committed corpora, the
table/registry frontend and the five layer benches were checked against the repo and hold: every
`build_steps` string, `argv`, and `quote` in Task 1 is present in the file it cites; `_read_raw`,
`route_turn(..., local_model=, prefix_tokens=)`, `price_turn(..., prices=).dollars`,
`_cluster_with_centroids`, `_LEXICAL_THRESHOLD`/`_POTION_THRESHOLD`/`DEFAULT_PATHWAY_THRESHOLD`,
`_STEmbedder`/`_PotionEmbedder`/`_LexicalEmbedder`, `serve_lines`, `case_id`, `make_verify_case`
and `python -m opendaisugi.conformance` all exist with the signatures the plan assumes; and all
24 `gate-paths.jsonl` cases resolve as the corpus predicts, because `hook.py:170-176` already
accepts `tool_name|tool|name` and `tool_input|args|input`. The defects cluster in Tasks 9 and
11–14 — the four stages this plan turns `live`, and the runtime dispatch.

- **BLOCKER — Task 11/12 `verify_via` lets a Core-profile client's allow override the oracle.**
  The plan rules that "a client that runs cleanly owns its verdict, including a permissive one",
  and pins that ruling as a test (`test_a_permissive_client_is_reported_as_its_own_verdict_not_
  the_oracle_s`). `swap.py:112` already offers `lean (proven core)` as a `verifier_client`, and
  Task 12 makes that choice live at the gate. But `clients/lean/DaisugiVerify/Verify.lean:199-209`
  is `runVerify = delegation-safety → permissions → dag`, then `{ ok := true }` — it implements
  no Full-profile stage. `clients/lean/README.md:120-131` and `docs/client-diversity.md` say so:
  "34 cases whose oracle verdict turns on a Full-profile stage this client doesn't implement by
  design (predicate-algebra, Z3 envelope subsumption)". So with `verifier_client: lean`, any
  envelope whose protection is an invariant, a postcondition, a vacuity check, or plan-vs-envelope
  subsumption is **allowed at the gate with `fallback=None` and no warning**. ADR-0001: "When
  correctness cannot be *proven*, deny." Fix: a dispatched client may only ever *tighten*. In
  `verify_via`, always compute the oracle result and return `ok = client_ok and oracle_ok`,
  keeping `client=<name>`, adding `corroborated="python"`, and logging a `WARNING` on any
  disagreement. This costs nothing already unspent: `docs/spec/conformance.md` measures the
  in-process oracle at p50 0.132 ms while the dispatch pays a 1.7–850 ms process spawn per call,
  so the dispatch is strictly slower than the oracle it replaces and its only value is diversity.
  (Acceptable alternative: give `ClientSpec` a `profile: "core"|"full"` field and treat a
  core-profile `ok=true` on an envelope carrying invariants/postconditions/skill steps as a
  `ClientFailure`.) Delete or invert the permissive-client test. **Same blocker, second half:**
  Task 2's `VERIFY_CASES` has six cases, none of them predicate- or Z3-stage, so Task 4's
  `test_no_client_is_allowed_to_fail_open_on_the_committed_corpus` reads 0 for Lean and
  *certifies* the hole. Add at least one verify case with an envelope invariant. — applied in
  Task 11 steps 1, 3 and 4 (the conjunctive rule, `client_verdict`, and
  `test_dispatch_never_allows_what_the_oracle_denies` driven by a Core-profile fake client; the
  permissive test is deleted), with the predicate corpus case in Task 2 steps 1, 4 and 5

- **BLOCKER — Task 9 never flips `STAGE_EFFECT["backend"]`, and Task 14 then fails.** The task is
  titled "the backend stage becomes live", master §5.9 requires it, Task 12 Step 4 rewrites the
  `swap.py` docstring to list `backend` under `live`, and `prove_backend_swap` is registered in
  `LIVE_PROOFS` — but no step in 6060 lines edits `STAGE_EFFECT["backend"]` (grep: the only
  `STAGE_EFFECT` edits are verifier in Task 12, matcher in Task 13, router in Task 14).
  `swap.py:42` therefore stays `"backend": CFG`, and Task 14's
  `test_stage_effect_tags_match_reality` reaches the `else` branch with
  `CFG_REASON.get("backend", "") == ""` and fails. Fix: add to Task 9 Step 3 —
  `swap.py` line 41, `"backend": LIVE,  # resolve_backend reads the config file per call` — and
  add `src/opendaisugi/swap.py` to Task 9's file list and Step 9 `git add`. — applied in Task 9
  step 3, with `test_the_backend_stage_is_tagged_live` added in step 1

- **BLOCKER — Task 14's gateway reload rebuilds a `Gateway` and throws it away.** In the Step 3
  app snippet the only use of the reloader on the request path is `reloader.current()` with the
  return value discarded; the handler keeps serving from the `gateway` closed over by
  `make_gateway_app`. `serve_gateway` (`gateway_asgi.py:366-418`) also builds a second
  `openai_gateway` the reloader never touches. So `router` would be tagged `LIVE` on the strength
  of a control that changes nothing in the serving path — the dishonest control master §3.5
  forbids — and `prove_router_swap` would not catch it, because it exercises `ConfigReloader` in
  isolation rather than the app. Fix: the request handler must select the Gateway for the turn
  (`active = reloader.current() if reloader is not None else gateway`, falling back to `gateway`
  when `current()` is `None`) and use `active` everywhere it currently uses `gateway`; give the
  OpenAI wire its own reloader or state in the note that it does not reload. Add an app-level
  test: change `gateway_local_model` on disk, replay a turn through `app`, and assert the journal
  records the new route. — applied in Task 14 steps 1 and 3
  (`test_a_config_change_routes_the_next_request_differently`,
  `test_a_failed_rebuild_keeps_serving_the_original_gateway`, and the handler now serves
  `reloader.current() or gateway`); `prove_router_swap` drives the real app too

- **BLOCKER — four of the six `LIVE_PROOFS` never change the config or observe behaviour.**
  Spec 11's hot-swap table and master §5.9 require "change the config, do not restart, observe the
  new behaviour", and `test_stage_effect_tags_match_reality` is what enforces it — so a proof that
  proves something else lets the tag pass vacuously. As written: `prove_matcher_swap` (Task 13)
  mutates the module private `_search._LEXICAL_IDENTITY` and never writes `matcher_model`;
  `prove_shell_swap` (Task 14) writes `shell_allow_decomposition` and asserts
  `load_config(path).shell_allow_decomposition` returns it — a YAML round-trip, not a behaviour;
  `prove_distill_swap` is the same shape one function deeper; `prove_verifier_swap` (Task 12)
  calls `verify_via` directly and never touches `config.yaml`, so nothing proves
  `_dispatch_verify` re-reads the choice per call. Fixes, none needing an extra package:
  (a) matcher — write `matcher_model: lexical`, assert `active_model_name() == "lexical-hash-v1"`;
  write `matcher_model: int8`, assert `active_model_name()` raises `MatcherNotAvailable`
  (`_search.py:195` makes an unbuilt key raise, so this is a real config-driven behaviour change);
  keep the identity-bump half as the re-embed proof. (b) verifier — move the body of
  `test_the_client_choice_is_read_per_call_not_at_import` into `prove_verifier_swap`: write
  `verifier_client`, call `evaluate_record`, rewrite it, call again, assert the dispatched name
  changed. (c) shell — verify a compound command against an envelope built from each config value
  and assert the verdict changes. (d) distill — call the code path that consults `auto_tend`, not
  `load_config`. — applied in Task 9 step 1 (`_write_config` now goes through
  `load_config`/`save_config`), Task 12 step 1 (verifier proof drives `evaluate_record` and reads
  `last_dispatch`), Task 13 step 1 (matcher proof drives `PathwayStore.find`; the identity bump
  moves to the re-embed test), and Task 14 step 1 (shell proof drives `gate init` plus a compound
  verdict, distill proof drives `daisugi hook auto-tend`, router proof drives the ASGI app)

- **SHOULD-FIX — Task 9 `self_consistent()` calls a field that does not exist, and the bug is
  silent.** `check_vacuity` is `def check_vacuity(expr, *, timeout_ms=500) -> Verdict` where
  `Verdict = Literal["tautology", "contradiction", "non_trivial"]` (`vacuity.py:27,52`) — a bare
  string with no `.vacuous`. `"tautology".vacuous` raises `AttributeError`, which the function's
  own `except Exception: return False` swallows, so the `self-consistent` column reads 0 for every
  backend and no test notices: `test_a_satisfiable_envelope_is_self_consistent` uses an envelope
  with no invariants, so the loop body never runs. Fix: compare to the literal
  (`check_vacuity(expr) == "contradiction"`), narrow the `except` to the solver errors, and add a
  test with a contradictory invariant. Note "tautology" is not a self-inconsistency; do not treat
  the two verdicts alike under a column named self-consistent. — applied in Task 9 steps 5 and 6

- **SHOULD-FIX — Task 12's dispatch spends the gate's whole budget, so a hung client bricks the
  gate.** `_dispatch_verify(..., timeout_s=timeout_s)` passes the gate's inner budget
  (`_DEFAULT_VERIFY_TIMEOUT_S = 10.0`, `gate.py:48`) straight to `subprocess.run(timeout=...)`
  inside the worker thread `_verify_with_timeout` joined for the same `timeout_s`. A client that
  hangs consumes the entire budget, the Python fallback then runs past the join deadline, and
  `_verify_with_timeout` raises `TimeoutError` — every tool call denies, permanently, with the
  reason "gate internal error (denied fail-closed)" that does not name the client. Fail-closed, so
  not a blocker, but the operator has no path back except editing `config.yaml`. Fix: give the
  dispatch a fraction (`timeout_s * 0.5`) so the oracle fallback still fits inside the join, and
  name the client in the deny reason. — applied in Task 12 steps 1 and 3
  (`_DISPATCH_BUDGET_FRACTION`, the named deny reason, and
  `test_a_hung_client_leaves_room_for_the_oracle_inside_the_gate_budget`)

- **SHOULD-FIX — Task 12 dispatched denials lose the reason the operator reads.** `_result_from`
  rebuilds every violation as `message="{client} client rejected this step"` with
  `detail={"step": ...}`. `evaluate_record` (`gate.py:245-252`) builds the deny summary from
  `f"{v.stage}: {v.message}"` and the ask payload carries `decision.clause` and
  `decision.counterexample`, so a dispatched deny reaches the `--ask` prompt with no clause, no
  counterexample, and no command text. Fix: include the step's command/path in the rebuilt
  message, or re-run the oracle for the human reason when a dispatched client denies (free once
  the blocker-1 conjunctive rule lands, since the oracle already ran). — applied in Task 11 step 4
  (`_client_violations` + `_step_detail`; the oracle's own violations now lead the list) and
  Task 12 step 3

- **SHOULD-FIX — Task 10 `verifier_x_shell` labels a filter as a setting.** The "off" row is
  `[c for c in cases if c["expect"]["ok"]]` — a subset of the same corpus scored by the same
  client. Nothing sets `shell_allow_decomposition`, which is an *envelope* field
  (`models.py:104`), and decompose cases carry no envelope. The row prints
  `decomposition: off` beside real numbers that were produced with nothing turned off, and
  `test_verifier_x_shell_crosses_both_decomposition_settings` only checks the two labels exist.
  Fix: cross the *verify* cases (which do carry an envelope) with
  `Permission.shell_allow_decomposition` True/False, or drop the axis and rename the pair. —
  applied in Task 10 steps 1 and 3 (`_decomposition_cases` re-derives each case with the field set
  and the oracle's expectation recomputed)

- **SHOULD-FIX — Task 10 `backend_x_envelope` prints measured numbers under a source it did not
  measure.** The `evidence-inferred` row reuses the LLM-generated envelopes' `mean_clauses` and
  `envelopes` count and hardcodes `tokens=0`. An ADR-0016 evidence-inferred envelope is a
  different envelope with a different clause count; presenting the LLM's number under that label
  is a fabricated cell, and the docstring's "identical by construction, and that is the finding"
  is not a finding. Fix: build the evidence-inferred envelopes from the corpus with the ADR-0016
  path, or print `n/a` in the columns that row did not measure and keep only `tokens: 0`. —
  applied in Task 10 steps 1 and 3 (`n/a` cells plus
  `test_the_evidence_inferred_row_claims_only_what_it_measured`)

- **SHOULD-FIX — Task 8's battery must be named as a new battery.** The plan's reinterpretation is
  honest in substance and the quote it keeps is real (`docs/harness/harness-comparison.md:186`,
  "the envelope classifies by tool NAME"), but the doc's nine is `3 sprig paths × 3 tasks`
  (:83-103) and that battery is explicitly *not* a gate measurement: ":177 The battery above used
  stand-in gates" and ":208 The battery gate is allow-all (measuring the harness, not the gate)".
  Sharing the number nine invites the reader to think this is that battery. Fix: rename
  `bench/corpus/loop-battery.jsonl` → `loop-vocabulary-cases.jsonl` and the column `battery` →
  `gate cases`; strike the claim that it "keeps the doc's real finding"; add a note line saying
  these cases are new and unrelated to the nine in `docs/harness`. — applied in Task 8 (opening
  prose, plus steps 1 and 3) and Task 2 step 4

- **SHOULD-FIX — three benches print numbers they did not measure, and one test asserts nothing.**
  (a) Task 5 `_cache_mb` sums the entire `~/.cache/huggingface/hub` tree for both `potion` and
  `all-MiniLM-L6-v2`, so the `MB` column reports the operator's whole model cache as each
  backend's size; measure the backend's own snapshot directory or drop the column. (b) Task 5
  `test_the_bench_never_downloads_a_model` builds `seen`/`_spy`, never uses them, and asserts
  `os.environ.get("HF_HUB_OFFLINE") in (None, "0", "1")` — true for every run; assert instead that
  a monkeypatched `huggingface_hub` download raises if called. (c) Task 6 `choose("rules", ...)`
  never passes `local_model`, so `rules` can never reach the `tier1-local` rung
  (`gateway.py:213-222`) and its `local share` is structurally 0.0; pass `targets["local"]`. —
  applied in Task 5 steps 1 and 3 (per-snapshot `_cache_mb`, real download-raises test) and
  Task 6 steps 1 and 3 (`rules` gets the same three targets, plus
  `test_rules_local_share_is_not_structurally_zero`)

- **SHOULD-FIX — nothing runs the `reproduce:` line, so a typo in it is invisible.** Every bench
  asserts `stable_rows(run(...)) == stable_rows(run(...))` — two in-process calls, which does not
  test the printed string at all. Add one test that shells the printed command with `--json`
  appended and compares `stable_rows`. Note also that `ref.rel` is repo-relative, so the printed
  command only works from the checkout root; say so in the note line. — applied in Task 4 steps 1
  and 3 (`test_the_printed_reproduce_line_actually_runs` shells the command; the note line says
  to run it from the checkout root)

- **SHOULD-FIX — Task 10 runs every pair twice on `daisugi bench pairs`.** `run()` (the index)
  calls each `_RUNNERS[name](opts)` to count rows, then the CLI's pairs branch calls `run_all`,
  which runs all five again. Fix: build the five tables once and derive the index from them. —
  applied in Task 10 steps 1 and 3 (`index_of(tables)` plus
  `test_the_index_does_not_re_run_the_pairs`)

- **NOTE — Task 2 Step 6 cites the wrong `.gitignore` line.** Line 39 is `**/CLAUDE.md`;
  `gateway-spike-log/` is line 40. Append after line 40. — applied in Task 2 step 6

- **NOTE — the four compiled clients in this checkout are built from Aug 21–22 source, before
  `a7b37ae`.** `client_is_built` only checks that the file exists, so the bench and
  `daisugi modules` will call a binary that may not match `clients/*/`. Also, Go/Rust/TS reach
  Full profile by spawning a `z3` binary (`clients/go/internal/verify/z3client.go:31`); it is
  present here (`~/.local/bin/z3`) but its absence makes those clients deny every predicate
  envelope (`envelope_z3.go:26-28`, correctly fail-closed) — worth a note line on the bench table.
  — applied in Task 4 step 3 (two note lines on the verifier table: rebuild before trusting a row,
  and z3-off-PATH shows as disagreement)

- **NOTE — Task 14's `/_reload` accepts any HTTP method.** The spec says `POST`; the snippet
  branches on `scope["path"]` only. Loopback-only is enforced fail-closed, so this is cosmetic. —
  applied in Task 14 steps 1 and 3 (405 on a non-POST, with
  `test_reload_endpoint_refuses_a_non_post_method`)

Re-review 2026-09-08: 16 of 16 verified applied; open: non-blocking — Task 11 step 4's
verifier_dispatch.py module docstring still states the old permissive-client-owns-its-verdict
rule the code and tests now reject (verify_via itself is correct); Task 12 step 3's
test_a_hung_client_leaves_room_for_the_oracle_inside_the_gate_budget mocks verify_via and only
checks the timeout parameter passed to it, not an actual hang (_DISPATCH_BUDGET_FRACTION and the
named deny reason are correct).

## Global Constraints

Copied verbatim from master spec §4:

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty declares go 1.26.0; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. Zig 0.16 and CMake are *build-time* requirements for go-libghostty; the
  plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  Herdr's vendored commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` (their 1.3.2) is the
  reference for patch notes, not a requirement; the binding chooses the commit it fetches.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

Additional constraints binding this plan only:

- Every bench must finish on the committed corpus in under 30 s. No bench downloads a model,
  opens a network socket, or calls a paid API without `--live`.
- No bench prints a number its `reproduce:` line cannot regenerate. Timing columns are marked
  `volatile` and are excluded from the determinism comparison.
- An option that is not installed is printed as an `absent` row carrying the install command.
  It is never omitted.
- `bench/corpus/` is hand-authored and committed. It is never exported from a recorded
  conformance corpus: recorded cases embed real local paths (`docs/spec/conformance.md`,
  "Generating and growing the corpus"), and that is why the frozen corpora are not committed.

## File structure

```
src/opendaisugi/bench/__init__.py           # run_bench, LAYERS, PAIRS
src/opendaisugi/bench/options.py            # the pinned facts: clients, gate paths, matchers, loops
src/opendaisugi/bench/corpus.py             # CorpusRef, corpus_ref, load_jsonl, resolve_corpus
src/opendaisugi/bench/table.py              # Column, Row, Table, render, to_json, stable_rows
src/opendaisugi/bench/registry.py           # BenchSpec, register, get, layer_names
src/opendaisugi/bench/layers/__init__.py    # imports every layer module so registration happens
src/opendaisugi/bench/layers/verifier.py
src/opendaisugi/bench/layers/matcher.py
src/opendaisugi/bench/layers/router.py
src/opendaisugi/bench/layers/gate_path.py
src/opendaisugi/bench/layers/loop.py
src/opendaisugi/bench/layers/backend.py
src/opendaisugi/bench/pairs.py
src/opendaisugi/verifier_dispatch.py
bench/corpus/…                              # the committed corpora (task 2)
tests/bench/…                               # one test module per bench
tests/test_hot_swap.py
tests/test_verifier_dispatch.py
```

`bench/corpus/screens/` is **deliberately not created.** No bench in spec 11 reads a screen
fixture. Manifest screen fixtures belong to sub-projects 02 and 03 and live at
`harness/coppice/testdata/screens/` (master §7). Do not recreate the directory here.

---

### Task 1: Pin the layer options

The later benches must not guess a binary path, a build command, or a fail-open class. This task
records those facts once, in code, with a test that re-reads the source document and fails if the
recorded string is no longer there.

**Files:**
- Create: `src/opendaisugi/bench/__init__.py`
- Create: `src/opendaisugi/bench/options.py`
- Create: `tests/bench/__init__.py`
- Test: `tests/bench/test_options.py`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```python
  REPO_ROOT: Path                       # the checkout root, or None when installed from a wheel

  @dataclass(frozen=True)
  class ClientSpec:
      name: str                         # python | rust | go | typescript | lean
      argv: tuple[str, ...]             # repo-relative argv, e.g. ("clients/go/conform",)
      probe: str | None                 # repo-relative file whose existence means "built"
      build_steps: tuple[str, ...]      # verbatim commands, each present in readme
      build_cwd: str                    # repo-relative dir the build runs in
      readme: str                       # repo-relative path of the doc the steps came from

  @dataclass(frozen=True)
  class GatePathSpec:
      name: str                         # claude-hook | codex-hooks | pi-ext | opencode-plugin | sprig-gate | mcp
      title: str
      fail_open_class: str              # hard | soft | deny-only | advisory
      evidence: str                     # measured | designed
      source: str                       # repo-relative doc path
      quote: str                        # a phrase that must appear verbatim in source

  @dataclass(frozen=True)
  class MatcherSpec:
      name: str                         # the config value of matcher_model
      package: str | None               # import name whose absence triggers the lexical fallback
      extra: str | None                 # the pip extra that provides it
      note: str

  @dataclass(frozen=True)
  class LoopSpec:
      name: str                         # sprig | claude-code | codex | pi | opencode
      binary: str                       # what to look for on PATH
      install: str                      # the command that installs it
      gate_path: str                    # a key of GATE_PATHS

  VERIFIER_CLIENTS: dict[str, ClientSpec]
  GATE_PATHS: dict[str, GatePathSpec]
  MATCHER_BACKENDS: dict[str, MatcherSpec]
  LOOPS: dict[str, LoopSpec]

  def repo_root() -> Path | None
  def client_argv(spec: ClientSpec) -> list[str]        # absolute argv, or [] when not built
  def client_is_built(spec: ClientSpec) -> bool
  def build_hint(spec: ClientSpec) -> str               # "cd clients/go && go build -o conform ./cmd/conform"
  ```

- [ ] **Step 1: Write the failing test**

Create `tests/bench/__init__.py` as an empty file, then `tests/bench/test_options.py`:

```python
"""The pinned facts must stay true: every recorded command and class cites a
document that still says it. A fact nobody re-reads rots silently."""

import sys

from opendaisugi.bench.options import (
    GATE_PATHS,
    LOOPS,
    MATCHER_BACKENDS,
    VERIFIER_CLIENTS,
    build_hint,
    repo_root,
)


def _read(rel: str) -> str:
    root = repo_root()
    assert root is not None, "these tests need a checkout, not an installed wheel"
    return (root / rel).read_text(encoding="utf-8")


def test_every_client_build_step_is_verbatim_in_its_readme():
    for spec in VERIFIER_CLIENTS.values():
        if not spec.build_steps:
            continue
        readme = _read(spec.readme)
        for step in spec.build_steps:
            assert step in readme, f"{spec.name}: {step!r} is not in {spec.readme}"


def test_every_client_argv_appears_in_the_diversity_doc():
    doc = _read("docs/client-diversity.md")
    for spec in VERIFIER_CLIENTS.values():
        if spec.name == "python":
            continue
        assert " ".join(spec.argv) in doc, f"{spec.name} argv is not in docs/client-diversity.md"


def test_python_client_needs_no_build_and_runs_this_interpreter():
    py = VERIFIER_CLIENTS["python"]
    assert py.build_steps == ()
    assert py.argv == (sys.executable, "-m", "opendaisugi.conformance")


def test_matcher_backends_match_the_search_fallback_table():
    from opendaisugi._search import _FALLBACK_PACKAGE

    recorded = {
        name: (spec.package, spec.extra)
        for name, spec in MATCHER_BACKENDS.items()
        if spec.package is not None
    }
    assert recorded == dict(_FALLBACK_PACKAGE)


def test_lexical_needs_no_package_so_it_can_never_be_absent():
    assert MATCHER_BACKENDS["lexical"].package is None
    assert MATCHER_BACKENDS["lexical"].extra is None


def test_every_gate_path_quote_is_still_in_its_source():
    for spec in GATE_PATHS.values():
        text = _read(spec.source)
        assert spec.quote in text, f"{spec.name}: {spec.quote!r} is gone from {spec.source}"


def test_fail_open_classes_and_evidence_are_from_the_closed_sets():
    for spec in GATE_PATHS.values():
        assert spec.fail_open_class in {"hard", "soft", "deny-only", "advisory"}
        assert spec.evidence in {"measured", "designed"}


def test_codex_is_recorded_as_the_fail_open_path():
    assert GATE_PATHS["codex-hooks"].fail_open_class == "soft"


def test_every_loop_names_an_install_command_and_a_real_gate_path():
    for spec in LOOPS.values():
        assert spec.install.strip()
        assert spec.gate_path in GATE_PATHS


def test_build_hint_is_a_runnable_one_liner():
    assert build_hint(VERIFIER_CLIENTS["go"]) == (
        "cd clients/go && go build -o conform ./cmd/conform"
    )
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_options.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench'`

- [ ] **Step 3: Write the implementation**

Create `src/opendaisugi/bench/__init__.py`:

```python
"""`daisugi bench` — one front door for comparing the options within a layer.

Contracts make the layers independent, so the default is per-layer tables, not an
n by n matrix. Five cross-layer pairs earn a row because a real coupling exists
there (master spec section 5.8). Every table prints the command that reproduces
it and the corpus digest it read.
"""

from __future__ import annotations
```

Create `src/opendaisugi/bench/options.py`:

```python
"""The facts the benches must not guess: where each compiled verifier client
lives and how it is built, what each gate path does when it fails, which package
each matcher backend needs, and which loops we know how to look for.

Every fact cites the file it came from, and `tests/bench/test_options.py` re-reads
that file. A pinned fact with no test is a rumour.
"""

from __future__ import annotations

import shutil
import sys
from dataclasses import dataclass
from pathlib import Path


def repo_root() -> Path | None:
    """The checkout root (the directory holding pyproject.toml), or None.

    Installed from a wheel there is no checkout, so the compiled clients and the
    committed corpora are unreachable. Callers report that instead of guessing.
    """
    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "pyproject.toml").is_file() and (parent / "src" / "opendaisugi").is_dir():
            return parent
    return None


REPO_ROOT = repo_root()


@dataclass(frozen=True)
class ClientSpec:
    """One conformance client: how to run it, how to build it, where that is written."""

    name: str
    argv: tuple[str, ...]
    probe: str | None
    build_steps: tuple[str, ...]
    build_cwd: str
    readme: str


VERIFIER_CLIENTS: dict[str, ClientSpec] = {
    "python": ClientSpec(
        name="python",
        argv=(sys.executable, "-m", "opendaisugi.conformance"),
        probe=None,
        build_steps=(),
        build_cwd=".",
        readme="docs/spec/conformance.md",
    ),
    "rust": ClientSpec(
        name="rust",
        argv=("clients/rust/target/release/conform",),
        probe="clients/rust/target/release/conform",
        build_steps=("cargo build --release",),
        build_cwd="clients/rust",
        readme="clients/rust/README.md",
    ),
    "go": ClientSpec(
        name="go",
        argv=("clients/go/conform",),
        probe="clients/go/conform",
        build_steps=("go build -o conform ./cmd/conform",),
        build_cwd="clients/go",
        readme="clients/go/README.md",
    ),
    "typescript": ClientSpec(
        name="typescript",
        argv=("node", "clients/ts/dist/conform.js"),
        probe="clients/ts/dist/conform.js",
        build_steps=("npm install", "npm run build"),
        build_cwd="clients/ts",
        readme="clients/ts/README.md",
    ),
    "lean": ClientSpec(
        name="lean",
        argv=("clients/lean/.lake/build/bin/conform",),
        probe="clients/lean/.lake/build/bin/conform",
        build_steps=("lake build",),
        build_cwd="clients/lean",
        readme="clients/lean/README.md",
    ),
}


def client_is_built(spec: ClientSpec) -> bool:
    """True when this client can be run right now. Python is always available."""
    if spec.probe is None:
        return True
    root = repo_root()
    if root is None:
        return False
    if not (root / spec.probe).exists():
        return False
    if spec.argv[0] == "node":
        return shutil.which("node") is not None
    return True


def client_argv(spec: ClientSpec) -> list[str]:
    """Absolute argv for the client, or an empty list when it is not built."""
    if not client_is_built(spec):
        return []
    if spec.probe is None:
        return list(spec.argv)
    root = repo_root()
    assert root is not None  # client_is_built already proved the checkout exists
    return [str(root / a) if a.startswith("clients/") else a for a in spec.argv]


def build_hint(spec: ClientSpec) -> str:
    """The one line an operator can paste to build this client."""
    return f"cd {spec.build_cwd} && " + " && ".join(spec.build_steps)


@dataclass(frozen=True)
class GatePathSpec:
    """One way a tool call reaches the gate, and what happens when that way breaks."""

    name: str
    title: str
    fail_open_class: str
    evidence: str
    source: str
    quote: str


GATE_PATHS: dict[str, GatePathSpec] = {
    "claude-hook": GatePathSpec(
        name="claude-hook",
        title="claude PreToolUse hook",
        fail_open_class="hard",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="must exit 2",
    ),
    "codex-hooks": GatePathSpec(
        name="codex-hooks",
        title="codex hooks.json",
        fail_open_class="soft",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="hooks fail open",
    ),
    "pi-ext": GatePathSpec(
        name="pi-ext",
        title="pi extension",
        fail_open_class="hard",
        evidence="designed",
        source="docs/plans/2026-09-08-workshop/00-master-spec.md",
        quote="unreachable gate",
    ),
    "opencode-plugin": GatePathSpec(
        name="opencode-plugin",
        title="opencode plugin",
        fail_open_class="deny-only",
        evidence="designed",
        source="docs/plans/2026-09-08-workshop/00-master-spec.md",
        quote="deny-only by design",
    ),
    "sprig-gate": GatePathSpec(
        name="sprig-gate",
        title="sprig in-process gate",
        fail_open_class="hard",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="in-process `Executor`",
    ),
    "mcp": GatePathSpec(
        name="mcp",
        title="MCP bridge",
        fail_open_class="advisory",
        evidence="measured",
        source="docs/harness/harness-comparison.md",
        quote="near fail-open",
    ),
}


@dataclass(frozen=True)
class MatcherSpec:
    """One pathway embedder, and the package whose absence degrades it to lexical."""

    name: str
    package: str | None
    extra: str | None
    note: str


MATCHER_BACKENDS: dict[str, MatcherSpec] = {
    "all-MiniLM-L6-v2": MatcherSpec(
        name="all-MiniLM-L6-v2",
        package="sentence_transformers",
        extra="opendaisugi[search]",
        note="torch, about 90MB",
    ),
    "potion": MatcherSpec(
        name="potion",
        package="model2vec",
        extra="opendaisugi[potion]",
        note="torch-free, numpy only, about 30MB",
    ),
    "lexical": MatcherSpec(
        name="lexical",
        package=None,
        extra=None,
        note="keyword floor, no model, no download",
    ),
    "int8": MatcherSpec(
        name="int8",
        package="onnxruntime",
        extra="opendaisugi[int8]",
        note="quantized MiniLM, no torch (spec 10)",
    ),
}


@dataclass(frozen=True)
class LoopSpec:
    """One harness we can run in a pane, and the gate path it uses."""

    name: str
    binary: str
    install: str
    gate_path: str


LOOPS: dict[str, LoopSpec] = {
    "sprig": LoopSpec("sprig", "sprig", "cd harness/sprig && go build ./cmd/sprig", "sprig-gate"),
    "claude-code": LoopSpec(
        "claude-code", "claude", "npm install -g @anthropic-ai/claude-code", "claude-hook"
    ),
    "codex": LoopSpec("codex", "codex", "npm install -g @openai/codex", "codex-hooks"),
    "pi": LoopSpec("pi", "pi", "daisugi install --harness pi", "pi-ext"),
    "opencode": LoopSpec("opencode", "opencode", "npm install -g opencode-ai", "opencode-plugin"),
}


def loop_is_installed(spec: LoopSpec) -> bool:
    """True when the loop's binary is on PATH. Never raises."""
    return shutil.which(spec.binary) is not None
```

Note on `MATCHER_BACKENDS["int8"]`: `_FALLBACK_PACKAGE` gains an `int8` entry only when spec 10
lands. The test compares only backends whose `package` is not None, so run
`test_matcher_backends_match_the_search_fallback_table` after adding int8 to `options.py`; if
spec 10 has **not** landed, delete the `int8` entry above until it has. Do not weaken the test.

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_options.py -q`
Expected: PASS, 10 tests.

If `test_matcher_backends_match_the_search_fallback_table` fails on the `int8` key, remove the
`int8` entry from `MATCHER_BACKENDS` (spec 10 has not landed on this branch) and re-run.

- [ ] **Step 5: Run lint**

Run: `uv run --no-sync ruff check src/opendaisugi/bench tests/bench`
Expected: `All checks passed!`

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/bench/__init__.py src/opendaisugi/bench/options.py \
        tests/bench/__init__.py tests/bench/test_options.py
git commit -m "bench: pin the layer options against the docs that state them

A bench that guesses a binary path or a fail-open class is a bench that lies
quietly. Every recorded command and class now cites the file it came from, and
the test re-reads that file, so a doc edit that invalidates a fact fails the
suite instead of rotting."
```

---

### Task 2: The committed bench corpora and their loader

Every bench reads a small, hand-authored, committed corpus. Recorded conformance corpora are
never committed because they embed real local paths (`docs/spec/conformance.md`), so the verifier
corpus is generated from a hand-written case list by a committed script whose output is
byte-reproducible.

**Files:**
- Create: `bench/corpus/README.md`
- Create: `bench/corpus/tasks.jsonl`
- Create: `bench/corpus/paraphrases.jsonl`
- Create: `bench/corpus/journal-slice.jsonl`
- Create: `bench/corpus/prices.json`
- Create: `bench/corpus/switchyard-targets.json`
- Create: `bench/corpus/battery-envelope.json`
- Create: `bench/corpus/loop-vocabulary-cases.jsonl`
- Create: `bench/corpus/loop-vocabulary.json`
- Create: `bench/corpus/gate-paths.jsonl`
- Create: `bench/corpus/make_verifier_corpus.py`
- Create: `bench/corpus/verifier.jsonl` (generated by the script above, committed)
- Create: `bench/corpus/verifier.jsonl.manifest.json` (generated, committed)
- Create: `src/opendaisugi/bench/corpus.py`
- Modify: `.gitignore:39` (append the gate-cache rule after `gateway-spike-log/`)
- Test: `tests/bench/test_corpus.py`

**Interfaces:**
- Consumes: `opendaisugi.bench.options.repo_root`.
- Produces:
  ```python
  @dataclass(frozen=True)
  class CorpusRef:
      path: Path            # absolute
      rel: str              # repo-relative, for the printed line
      sha256: str           # full hex of the file bytes
      @property
      def short(self) -> str: ...        # sha256[:8]

  class CorpusMissing(RuntimeError): ...

  def corpus_dir() -> Path                              # $DAISUGI_BENCH_CORPUS or <repo>/bench/corpus
  def corpus_ref(path: Path) -> CorpusRef
  def resolve_corpus(default_rel: str, override: Path | None) -> CorpusRef
  def load_jsonl(ref: CorpusRef) -> list[dict]
  def load_json(ref: CorpusRef) -> dict
  ```

- [ ] **Step 1: Write the failing test**

Create `tests/bench/test_corpus.py`:

```python
"""The committed corpora must parse, must be pinned, and must regenerate
byte-for-byte. A corpus whose ids drift silently makes every bench read zero."""

import json
import subprocess
import sys

import pytest

from opendaisugi.bench.corpus import (
    CorpusMissing,
    corpus_dir,
    corpus_ref,
    load_json,
    load_jsonl,
    resolve_corpus,
)


def test_every_committed_corpus_parses_and_is_non_empty():
    for rel in (
        "tasks.jsonl",
        "paraphrases.jsonl",
        "journal-slice.jsonl",
        "loop-vocabulary-cases.jsonl",
        "gate-paths.jsonl",
        "verifier.jsonl",
    ):
        rows = load_jsonl(resolve_corpus(rel, None))
        assert rows, f"{rel} is empty"
        assert all(isinstance(r, dict) and r.get("id") for r in rows), rel


def test_task_and_paraphrase_ids_line_up():
    tasks = {r["id"] for r in load_jsonl(resolve_corpus("tasks.jsonl", None))}
    paras = load_jsonl(resolve_corpus("paraphrases.jsonl", None))
    assert len(tasks) == 12
    assert len(paras) == 12
    assert {p["of"] for p in paras} == tasks


def test_corpus_ref_digest_is_the_file_bytes(tmp_path):
    import hashlib

    f = tmp_path / "x.jsonl"
    f.write_bytes(b'{"id":"a"}\n')
    ref = corpus_ref(f)
    assert ref.sha256 == hashlib.sha256(b'{"id":"a"}\n').hexdigest()
    assert ref.short == ref.sha256[:8]


def test_missing_corpus_teaches_the_next_command(tmp_path):
    with pytest.raises(CorpusMissing) as exc:
        resolve_corpus("nope.jsonl", None)
    assert "bench/corpus" in str(exc.value)


def test_override_path_wins(tmp_path):
    f = tmp_path / "other.jsonl"
    f.write_text('{"id":"z"}\n')
    ref = resolve_corpus("tasks.jsonl", f)
    assert ref.path == f


def test_verifier_corpus_manifest_pins_the_file():
    ref = resolve_corpus("verifier.jsonl", None)
    manifest = json.loads((ref.path.parent / "verifier.jsonl.manifest.json").read_text())
    assert manifest["sha256"] == ref.sha256
    assert manifest["count"] == len(load_jsonl(ref))
    assert manifest["v"] == 1


def test_verifier_corpus_regenerates_byte_identical(tmp_path):
    """The ids are content addresses. If the generator and the committed file
    disagree, `run_corpus` silently reports 'no verdict' for every case."""
    script = corpus_dir() / "make_verifier_corpus.py"
    out = tmp_path / "verifier.jsonl"
    subprocess.run(
        [sys.executable, str(script), "--out", str(out)], check=True, capture_output=True
    )
    assert out.read_bytes() == (corpus_dir() / "verifier.jsonl").read_bytes()


def test_the_corpus_has_a_case_only_a_full_profile_client_can_deny():
    """Without this case the fail-open column reads 0 for a Core client and
    certifies the hole instead of finding it."""
    cases = load_jsonl(resolve_corpus("verifier.jsonl", None))
    predicate_denials = [
        c
        for c in cases
        if c["kind"] == "verify"
        and c["expect"]["ok"] is False
        and any(v["stage"] == "predicate" for v in c["expect"]["violations"])
    ]
    assert predicate_denials, "no predicate-stage denial in the corpus"


def test_verifier_corpus_cases_carry_computed_ids():
    from opendaisugi.conformance import case_id

    for case in load_jsonl(resolve_corpus("verifier.jsonl", None)):
        body = {k: v for k, v in case.items() if k != "id"}
        assert case["id"] == case_id(body), case["id"]


def test_battery_envelope_and_vocabulary_load():
    env = load_json(resolve_corpus("battery-envelope.json", None))
    assert env["permissions"]["file_write"] == ["build/**"]
    vocab = load_json(resolve_corpus("loop-vocabulary.json", None))
    assert set(vocab) == {"sprig", "claude-code", "codex", "pi", "opencode"}
    assert vocab["pi"]["names"] is None, "pi's vocabulary is not pinned until spec 04 lands"


def test_prices_cover_every_switchyard_target():
    prices = load_json(resolve_corpus("prices.json", None))
    targets = load_json(resolve_corpus("switchyard-targets.json", None))
    for model in targets.values():
        assert model in prices, model
    assert prices[targets["local"]] == [0.0, 0.0], "a local model costs no money"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_corpus.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.corpus'`

- [ ] **Step 3: Write the corpus loader**

Create `src/opendaisugi/bench/corpus.py`:

```python
"""Loading and pinning the committed bench corpora.

A bench prints the digest of the exact bytes it read. That is the whole reason a
`reproduce:` line is worth anything: rerun the command against the same digest
and you get the same rows.
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path

from opendaisugi.bench.options import repo_root


class CorpusMissing(RuntimeError):
    """The corpus file is not there. The message names the next command."""


@dataclass(frozen=True)
class CorpusRef:
    path: Path
    rel: str
    sha256: str

    @property
    def short(self) -> str:
        return self.sha256[:8]


def corpus_dir() -> Path:
    """Where the committed corpora live.

    ``DAISUGI_BENCH_CORPUS`` overrides, so a copy on real disk can stand in.
    Otherwise it is ``<checkout>/bench/corpus``. An installed wheel has no
    checkout, so the benches say so rather than inventing an empty corpus.
    """
    override = os.environ.get("DAISUGI_BENCH_CORPUS")
    if override:
        return Path(override)
    root = repo_root()
    if root is None:
        raise CorpusMissing(
            "The bench corpora ship with the source, not the wheel.\n"
            "Run `daisugi bench` from a checkout, or set DAISUGI_BENCH_CORPUS "
            "to a copy of bench/corpus."
        )
    return root / "bench" / "corpus"


def corpus_ref(path: Path) -> CorpusRef:
    """Pin one corpus file by the sha256 of its exact bytes."""
    data = path.read_bytes()
    root = repo_root()
    try:
        rel = str(path.relative_to(root)) if root else str(path)
    except ValueError:
        rel = str(path)
    return CorpusRef(path=path, rel=rel, sha256=hashlib.sha256(data).hexdigest())


def resolve_corpus(default_rel: str, override: Path | None) -> CorpusRef:
    """The corpus a bench will read: ``--corpus PATH`` when given, else the default."""
    path = Path(override) if override is not None else corpus_dir() / default_rel
    if not path.is_file():
        raise CorpusMissing(
            f"No corpus at {path}.\n"
            f"The committed corpora live in bench/corpus of the checkout.\n"
            f"Pass --corpus PATH, or set DAISUGI_BENCH_CORPUS to that directory."
        )
    return corpus_ref(path)


def load_jsonl(ref: CorpusRef) -> list[dict]:
    """Every non-blank line of a JSONL corpus, in file order."""
    return [
        json.loads(line)
        for line in ref.path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def load_json(ref: CorpusRef) -> dict:
    """A whole-file JSON corpus (the envelope, the vocabulary, the price table)."""
    return json.loads(ref.path.read_text(encoding="utf-8"))
```

- [ ] **Step 4: Write the hand-authored corpora**

Create `bench/corpus/README.md`:

```markdown
# The bench corpora

Small, hand-authored, committed. `daisugi bench` reads these and prints the
sha256 of the exact bytes it read, so a `reproduce:` line means something.

These are **not** exported from a recorded conformance corpus. Recorded cases
embed real local paths (docs/spec/conformance.md), which is why the frozen
11,450-case corpus is never committed. Everything here was written by hand.

| file | read by |
|---|---|
| `tasks.jsonl` | matcher, backend |
| `paraphrases.jsonl` | matcher |
| `verifier.jsonl` + `.manifest.json` | verifier (generated by `make_verifier_corpus.py`) |
| `journal-slice.jsonl` | router |
| `prices.json`, `switchyard-targets.json` | router |
| `loop-vocabulary-cases.jsonl`, `loop-vocabulary.json`, `battery-envelope.json` | loop |
| `gate-paths.jsonl`, `battery-envelope.json` | gate-path |
| `envelopes/<backend>.jsonl` | backend |

Regenerate the verifier corpus after editing its case list:

```sh
uv run --no-sync python bench/corpus/make_verifier_corpus.py --out bench/corpus/verifier.jsonl
```

There is no `screens/` directory. No bench here reads a screen fixture; the
manifest fixtures live with sub-projects 02 and 03.
```

Create `bench/corpus/tasks.jsonl` with exactly these twelve lines:

```jsonl
{"id":"t01","task":"add a pytest test for the shell decomposer","kind":"code","stakes":"medium"}
{"id":"t02","task":"bump the docker base image to bookworm","kind":"ops","stakes":"medium"}
{"id":"t03","task":"summarise the last ten journal traces","kind":"read","stakes":"low"}
{"id":"t04","task":"fix the flaky gateway streaming test","kind":"code","stakes":"medium"}
{"id":"t05","task":"list every file larger than ten megabytes under build","kind":"shell","stakes":"low"}
{"id":"t06","task":"write the release notes for version 0.44","kind":"write","stakes":"low"}
{"id":"t07","task":"rotate the signing key used by the registry","kind":"ops","stakes":"high"}
{"id":"t08","task":"explain why the potion threshold is 0.59","kind":"read","stakes":"low"}
{"id":"t09","task":"add a retry with backoff to the model download","kind":"code","stakes":"medium"}
{"id":"t10","task":"delete the stale pathways older than ninety days","kind":"ops","stakes":"high"}
{"id":"t11","task":"render the module map as json for a script","kind":"read","stakes":"low"}
{"id":"t12","task":"port the lean parser rule for heredocs to go","kind":"code","stakes":"medium"}
```

Create `bench/corpus/paraphrases.jsonl` with exactly these twelve lines:

```jsonl
{"id":"p01","of":"t01","task":"write a unit test that covers shell decomposition"}
{"id":"p02","of":"t02","task":"upgrade the base image in the dockerfile to bookworm"}
{"id":"p03","of":"t03","task":"give me a summary of the ten most recent journal traces"}
{"id":"p04","of":"t04","task":"repair the intermittent failure in the gateway streaming test"}
{"id":"p05","of":"t05","task":"show the files over ten megabytes inside the build directory"}
{"id":"p06","of":"t06","task":"draft release notes for the 0.44 release"}
{"id":"p07","of":"t07","task":"replace the registry signing key with a new one"}
{"id":"p08","of":"t08","task":"why was 0.59 chosen as the potion reuse threshold"}
{"id":"p09","of":"t09","task":"make the model download retry with exponential backoff"}
{"id":"p10","of":"t10","task":"prune pathways that have not been touched in ninety days"}
{"id":"p11","of":"t11","task":"print the module wiring as machine readable json"}
{"id":"p12","of":"t12","task":"bring the heredoc handling from the lean parser into the go client"}
```

Create `bench/corpus/journal-slice.jsonl` with exactly these twelve lines. `prefix_tokens` is
passed straight to `route_turn` so a large cached prefix is exercised without a large file:

```jsonl
{"id":"j01","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"what does ImportError No module named z3 mean"}]},"usage":{"input_tokens":900,"output_tokens":150,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
{"id":"j02","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"rename the flag to --plain"}]},"usage":{"input_tokens":420,"output_tokens":90,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
{"id":"j03","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"debug the race condition in the pane watcher"}]},"usage":{"input_tokens":2100,"output_tokens":800,"cache_read_input_tokens":12000,"cache_creation_input_tokens":0}}
{"id":"j04","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"design the schema migration for the pathway store"}]},"usage":{"input_tokens":1800,"output_tokens":950,"cache_read_input_tokens":9000,"cache_creation_input_tokens":0}}
{"id":"j05","prefix_tokens":6000,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"add a docstring here"}]},"usage":{"input_tokens":300,"output_tokens":120,"cache_read_input_tokens":24000,"cache_creation_input_tokens":0}}
{"id":"j06","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"list the files in src"}]},"usage":{"input_tokens":260,"output_tokens":70,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
{"id":"j07","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"prove the head extraction lemma stays sound under negation"}]},"usage":{"input_tokens":1500,"output_tokens":1100,"cache_read_input_tokens":0,"cache_creation_input_tokens":4000}}
{"id":"j08","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"what is the default gate mode"}]},"usage":{"input_tokens":240,"output_tokens":60,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
{"id":"j09","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"optimise the cosine batch for a thousand rows"}]},"usage":{"input_tokens":1300,"output_tokens":600,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
{"id":"j10","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"bump the version to 0.44.0"}]},"usage":{"input_tokens":280,"output_tokens":80,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
{"id":"j11","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"tidy the imports in cli.py"}]},"usage":{"input_tokens":500,"output_tokens":200,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
{"id":"j12","prefix_tokens":0,"body":{"model":"claude-opus-4-8","messages":[{"role":"user","content":"refactor the executor to drop the duplicate branch"}]},"usage":{"input_tokens":1600,"output_tokens":700,"cache_read_input_tokens":6000,"cache_creation_input_tokens":0}}
```

Create `bench/corpus/prices.json` (USD per million tokens, `[input, output]`):

```json
{
  "claude-opus-4-8": [15.0, 75.0],
  "claude-sonnet-5": [3.0, 15.0],
  "claude-haiku-4-5": [1.0, 5.0],
  "qwen2.5-coder-7b": [0.0, 0.0]
}
```

Create `bench/corpus/switchyard-targets.json`:

```json
{"local": "qwen2.5-coder-7b", "cheap": "claude-haiku-4-5", "capable": "claude-sonnet-5"}
```

Create `bench/corpus/battery-envelope.json`:

```json
{
  "id": "env_bench_battery",
  "generated_by": "hand-written for bench/corpus",
  "task": "the bench battery envelope",
  "stakes": "medium",
  "permissions": {
    "file_read": ["**"],
    "file_write": ["build/**"],
    "network": false,
    "network_hosts": [],
    "shell": true,
    "shell_allowlist": ["echo", "ls"],
    "shell_allow_decomposition": false,
    "mcp_allowlist": ["daisugi/*"],
    "custom_step_allowlist": [],
    "max_execution_time_s": 30,
    "max_output_size_mb": 10
  },
  "invariants": [],
  "postconditions": [],
  "tightening_only": true
}
```

Create `bench/corpus/loop-vocabulary-cases.jsonl`, the nine cases in gate vocabulary:

```jsonl
{"id":"c1","case":"write-allow","tool":"write","input":{"path":"build/out.txt","content":"ok"},"expect":"allow"}
{"id":"c2","case":"write-deny-scope","tool":"write","input":{"path":"/etc/passwd","content":"x"},"expect":"deny"}
{"id":"c3","case":"edit-allow","tool":"edit","input":{"path":"build/out.txt","content":"ok2"},"expect":"allow"}
{"id":"c4","case":"edit-deny-scope","tool":"edit","input":{"path":"/etc/cron.d/job","content":"x"},"expect":"deny"}
{"id":"c5","case":"bash-allow","tool":"bash","input":{"command":"echo ok"},"expect":"allow"}
{"id":"c6","case":"bash-deny-head","tool":"bash","input":{"command":"curl https://example.com"},"expect":"deny"}
{"id":"c7","case":"bash-deny-compound","tool":"bash","input":{"command":"echo ok && rm -rf /"},"expect":"deny"}
{"id":"c8","case":"read-allow","tool":"read","input":{"path":"README.md"},"expect":"allow"}
{"id":"c9","case":"unknown-deny","tool":"unknown","input":{},"expect":"deny"}
```

Create `bench/corpus/loop-vocabulary.json`. `names` maps a battery tool to the name that loop
actually delivers; `keys` maps the battery input key to the loop's key. A `null` name means the
loop has no such tool and the case counts as not applicable:

```json
{
  "claude-code": {
    "names": {"write": "Write", "edit": "Edit", "bash": "Bash", "read": "Read", "unknown": "Teleport"},
    "keys": {"path": "file_path", "content": "content", "command": "command"},
    "note": "Claude's native names are the envelope's vocabulary as-is"
  },
  "sprig": {
    "names": {"write": "Write", "edit": "Edit", "bash": "Bash", "read": "Read", "unknown": "Teleport"},
    "keys": {"path": "file_path", "content": "content", "command": "command"},
    "note": "names translated by sprig's DaisugiGate before they reach us (docs/harness/harness-comparison.md)"
  },
  "codex": {
    "names": {"write": "apply_patch", "edit": "apply_patch", "bash": "shell", "read": null, "unknown": "Teleport"},
    "keys": {"path": "path", "content": "content", "command": "command"},
    "note": "codex reads through shell; apply_patch is its only write tool (src/opendaisugi/parsers/codex.py)"
  },
  "pi": {"names": null, "keys": null, "note": "vocabulary not pinned until spec 04 lands"},
  "opencode": {"names": null, "keys": null, "note": "vocabulary not pinned until spec 05 lands"}
}
```

Create `bench/corpus/gate-paths.jsonl`, four contract cases per path. `payload` is the raw object
that path hands the gate:

```jsonl
{"id":"g01","path":"claude-hook","case":"allow","payload":{"session_id":"s1","tool_name":"Write","tool_input":{"file_path":"build/out.txt","content":"ok"}},"expect":"allow"}
{"id":"g02","path":"claude-hook","case":"deny","payload":{"session_id":"s1","tool_name":"Write","tool_input":{"file_path":"/etc/passwd","content":"x"}},"expect":"deny"}
{"id":"g03","path":"claude-hook","case":"malformed","payload":{"session_id":"s1"},"expect":"deny"}
{"id":"g04","path":"claude-hook","case":"alt-key","payload":{"session_id":"s1","tool_name":"Write","tool_input":{"path":"build/out.txt","content":"ok"}},"expect":"allow"}
{"id":"g05","path":"codex-hooks","case":"allow","payload":{"session_id":"s2","tool":"shell","args":{"command":"echo ok"}},"expect":"allow"}
{"id":"g06","path":"codex-hooks","case":"deny","payload":{"session_id":"s2","tool":"shell","args":{"command":"curl https://example.com"}},"expect":"deny"}
{"id":"g07","path":"codex-hooks","case":"malformed","payload":{"session_id":"s2","args":{"command":"echo ok"}},"expect":"deny"}
{"id":"g08","path":"codex-hooks","case":"alt-key","payload":{"session_id":"s2","tool":"shell","args":{"cmd":"echo ok"}},"expect":"allow"}
{"id":"g09","path":"pi-ext","case":"allow","payload":{"session_id":"s3","name":"Read","input":{"file_path":"README.md"}},"expect":"allow"}
{"id":"g10","path":"pi-ext","case":"deny","payload":{"session_id":"s3","name":"Bash","input":{"command":"rm -rf /"}},"expect":"deny"}
{"id":"g11","path":"pi-ext","case":"malformed","payload":{"session_id":"s3","name":""},"expect":"deny"}
{"id":"g12","path":"pi-ext","case":"alt-key","payload":{"session_id":"s3","name":"Read","input":{"path":"README.md"}},"expect":"allow"}
{"id":"g13","path":"opencode-plugin","case":"allow","payload":{"session_id":"s4","tool":"Bash","args":{"command":"ls"}},"expect":"allow"}
{"id":"g14","path":"opencode-plugin","case":"deny","payload":{"session_id":"s4","tool":"Bash","args":{"command":"curl https://example.com"}},"expect":"deny"}
{"id":"g15","path":"opencode-plugin","case":"malformed","payload":"not-an-object","expect":"deny"}
{"id":"g16","path":"opencode-plugin","case":"alt-key","payload":{"session_id":"s4","tool":"Bash","args":{"cmd":"ls"}},"expect":"allow"}
{"id":"g17","path":"sprig-gate","case":"allow","payload":{"session_id":"s5","tool_name":"Write","tool_input":{"file_path":"build/a.txt","content":"ok"}},"expect":"allow"}
{"id":"g18","path":"sprig-gate","case":"deny","payload":{"session_id":"s5","tool_name":"Bash","tool_input":{"command":"rm -rf /"}},"expect":"deny"}
{"id":"g19","path":"sprig-gate","case":"malformed","payload":{"session_id":"s5","tool_name":"write","tool_input":{"path":"build/a.txt"}},"expect":"deny"}
{"id":"g20","path":"sprig-gate","case":"alt-key","payload":{"session_id":"s5","tool_name":"Write","tool_input":{"path":"build/a.txt","content":"ok"}},"expect":"allow"}
{"id":"g21","path":"mcp","case":"allow","payload":{"session_id":"s6","name":"mcp__daisugi__verify","input":{"plan":"p"}},"expect":"allow"}
{"id":"g22","path":"mcp","case":"deny","payload":{"session_id":"s6","name":"mcp__github__create_issue","input":{}},"expect":"deny"}
{"id":"g23","path":"mcp","case":"malformed","payload":{"session_id":"s6","name":"mcp__"},"expect":"deny"}
{"id":"g24","path":"mcp","case":"alt-key","payload":{"session_id":"s6","name":"mcp__daisugi__verify","args":{"plan":"p"}},"expect":"allow"}
```

Note on `g19`: sprig's *untranslated* lowercase `write` is not in the gate's classification map, so
the gate denies it. That is the real finding in `docs/harness/harness-comparison.md`, kept here as a
contract case rather than hidden.

- [ ] **Step 5: Write the verifier-corpus generator and run it**

Create `bench/corpus/make_verifier_corpus.py`:

```python
#!/usr/bin/env -S uv run --script
"""Generate bench/corpus/verifier.jsonl from a hand-written case list.

The ids are content addresses (docs/spec/conformance.md), so they cannot be typed
by hand. This script builds them with the same helpers the oracle uses and writes
the manifest that pins the bytes. Output is deterministic: run it twice, get the
same file.

Nothing here is exported from a recorded corpus. Recorded cases embed real local
paths, which is why the frozen corpora are never committed.

    uv run --no-sync python bench/corpus/make_verifier_corpus.py --out bench/corpus/verifier.jsonl
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

from opendaisugi.conformance import (
    CONFORMANCE_VERSION,
    canonical_json,
    make_decompose_case,
    make_verify_case,
)
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileWriteStep,
    Invariant,
    Permission,
    ShellStep,
)
from opendaisugi.shell_decompose import decompose_command
from opendaisugi.verify import verify

# Forty decomposition commands. Two thirds are ordinary agent lines; the rest are
# the shapes the client campaign found interesting: pipes, and-or lists, heredocs,
# subshells, redirections, compounds, and a few that must fail closed.
COMMANDS: tuple[str, ...] = (
    "echo hello",
    "ls -la",
    "git status",
    "git commit -m 'fix the parser'",
    "cat foo.txt | grep bar",
    "grep -c x f | sort > out",
    "make build && make test",
    "pytest -q || echo failed",
    "cd src; ls",
    "FOO=1 git status",
    "FOO=1 BAR=2 make",
    "find . -name '*.py' -print",
    "python -m opendaisugi.conformance",
    "uv run --no-sync pytest tests -q",
    "rm -rf build",
    "mkdir -p build && cp a.txt build/",
    "tar czf out.tgz src",
    "sed -n '1,20p' README.md",
    "awk '{print $1}' data.txt",
    "curl https://example.com -o page.html",
    "echo $(date)",
    "echo `hostname`",
    "echo ${HOME}",
    "echo $((1 + 2))",
    "( cd src && ls )",
    "cat <<EOF\nhello\nEOF",
    "cat <<'EOF'\n$(date)\nEOF",
    "if [ -f a ]; then cat a; else echo none; fi",
    "for f in *.py; do echo $f; done",
    "while read line; do echo $line; done < in.txt",
    "until false; do break; done",
    "! grep -q x f",
    "ls > out.txt 2>&1",
    "diff <(sort a) <(sort b)",
    "case $x in a) echo a;; esac",
    "{ echo a; echo b; }",
    "git log --oneline | head -20 > log.txt",
    "npm install && npm run build",
    "go build -o conform ./cmd/conform",
    "lake build",
)

_ENV_STRICT = Envelope(
    id="env_case",
    generated_by="hand-written for bench/corpus",
    task="run the project's tests",
    stakes="medium",
    permissions=Permission(shell=True, shell_allowlist=["pytest", "echo"]),
)
_ENV_OPEN = Envelope(
    id="env_case",
    generated_by="hand-written for bench/corpus",
    task="run the project's tests",
    stakes="medium",
    permissions=Permission(shell=True, shell_allowlist=["pytest", "echo", "git", "ls"]),
)

# The predicate case. Its denial turns on a Full-profile stage: permissions,
# DAG and delegation all PASS, and only the invariant refuses the file_write
# step. A Core-profile client (clients/lean/DaisugiVerify/Verify.lean runs
# delegation, permissions, dag and then returns ok) answers `ok: true` here, so
# this one case is what lets `daisugi bench verifier`'s fail-open column read
# non-zero instead of certifying a hole. Do not remove it to make a row green.
_ENV_PREDICATE = Envelope(
    id="env_case",
    generated_by="hand-written for bench/corpus",
    task="write a build artifact",
    stakes="medium",
    permissions=Permission(
        shell=True, shell_allowlist=["pytest"], file_write=["build/**"], file_read=["**"]
    ),
    invariants=[
        Invariant(
            type="shell_only",
            description="every step is a shell step",
            expr={
                "op": "forall_steps",
                "pred": {"op": "equals", "path": "type", "value": "shell"},
            },
        )
    ],
)

# Six shell verification cases: three that pass, three that fail on different
# stages. The seventh is the predicate case above and is built separately,
# because it needs a non-shell step.
VERIFY_CASES: tuple[tuple[str, Envelope, tuple[tuple[str, str], ...]], ...] = (
    ("pass-echo", _ENV_OPEN, (("s1", "echo ok"),)),
    ("pass-two", _ENV_OPEN, (("s1", "git status"), ("s2", "ls"))),
    ("pass-pytest", _ENV_STRICT, (("s1", "pytest -q"),)),
    ("deny-head", _ENV_STRICT, (("s1", "curl https://example.com"),)),
    ("deny-second", _ENV_STRICT, (("s1", "echo ok"), ("s2", "rm -rf build"))),
    ("deny-compound", _ENV_STRICT, (("s1", "echo ok && rm -rf /"),)),
)


def _predicate_plan() -> ActionPlan:
    """A plan every structural stage accepts and only the invariant refuses."""
    return ActionPlan(
        id="plan_case",
        source="bench/deny-predicate",
        task=_ENV_PREDICATE.task,
        steps=[
            ShellStep(id="s1", command="pytest -q"),
            FileWriteStep(id="s2", path="build/out.txt", content="ok"),
        ],
    )


def build_cases() -> list[dict]:
    cases: list[dict] = []
    for command in COMMANDS:
        cases.append(make_decompose_case(command, decompose_command(command)))
    options = {"strict": None, "z3_timeout_ms": 500}
    plans = [
        (
            envelope,
            ActionPlan(
                id="plan_case",
                source=f"bench/{name}",
                task=envelope.task,
                steps=[ShellStep(id=sid, command=cmd) for sid, cmd in steps],
            ),
        )
        for name, envelope, steps in VERIFY_CASES
    ]
    plans.append((_ENV_PREDICATE, _predicate_plan()))
    for envelope, plan in plans:
        result = verify(plan, envelope, strict=None, z3_timeout_ms=500)
        cases.append(make_verify_case(plan, envelope, options, result))
    return sorted(cases, key=lambda c: c["id"])


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()

    cases = build_cases()
    body = "".join(canonical_json(c) + "\n" for c in cases)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(body, encoding="utf-8")
    manifest = {
        "v": CONFORMANCE_VERSION,
        "count": len(cases),
        "sha256": hashlib.sha256(body.encode("utf-8")).hexdigest(),
    }
    args.out.with_suffix(args.out.suffix + ".manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"{args.out}: {len(cases)} cases, sha256 {manifest['sha256'][:8]}")


if __name__ == "__main__":
    main()
```

Run it:

```bash
uv run --no-sync python bench/corpus/make_verifier_corpus.py --out bench/corpus/verifier.jsonl
```

Expected: `bench/corpus/verifier.jsonl: 47 cases, sha256 ########`. If the count is not 47, a
command or verify case was mistyped; fix it before continuing.

Then confirm the predicate case really denies on the predicate stage, because the whole point of
it is that only a Full-profile client can reproduce that denial:

```bash
uv run --no-sync python -c "
import sys; sys.path.insert(0, 'bench/corpus')
from make_verifier_corpus import _ENV_PREDICATE, _predicate_plan
from opendaisugi.verify import verify
r = verify(_predicate_plan(), _ENV_PREDICATE, strict=None)
print(r.ok, [v.stage for v in r.violations])
"
```

Expected: `False ['predicate']`. Anything else (especially a `permissions` violation) means the
plan is being refused by a stage a Core client also implements, so the case would no longer
expose the profile gap.

If `ShellStep` is not the step class name in `opendaisugi.models`, read the class list with
`grep -n "^class .*Step" src/opendaisugi/models.py` and use the shell step class it names; the
generator needs a shell step with `id` and `command`.

- [ ] **Step 6: Keep the oracle cache out of the corpus directory**

`clients/gate.py::_oracle_verdicts` writes `<corpus>.parent/.gate-cache/`, and its own docstring
says never to commit gate output. Append to `.gitignore` after line 40 (`gateway-spike-log/`;
line 39 is `**/CLAUDE.md`):

```gitignore

# clients/gate.py caches oracle verdicts beside the corpus it scored; mismatch
# samples embed corpus content, so the cache is never committed.
.gate-cache/
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/bench/test_corpus.py -q`
Expected: PASS, 10 tests.

- [ ] **Step 8: Run lint**

Run: `uv run --no-sync ruff check src/opendaisugi/bench bench tests/bench`
Expected: `All checks passed!`

- [ ] **Step 9: Commit**

```bash
git add bench/corpus src/opendaisugi/bench/corpus.py tests/bench/test_corpus.py .gitignore
git commit -m "bench: commit small hand-written corpora and pin them by digest

A reproduce line is worth nothing unless the reader can prove they read the same
bytes, so every corpus is loaded through a ref that carries its sha256. The
verifier cases are generated rather than typed because their ids are content
addresses; a hand-typed id would make every client report 'no verdict' and the
bench would silently read zero."
```

---

### Task 3: The bench frontend — table, registry, `daisugi bench`

**Files:**
- Create: `src/opendaisugi/bench/table.py`
- Create: `src/opendaisugi/bench/registry.py`
- Create: `src/opendaisugi/bench/layers/__init__.py`
- Modify: `src/opendaisugi/cli.py:3155` (add the `bench` command just above `@app.command("modules", hidden=True)`)
- Test: `tests/bench/test_table.py`
- Test: `tests/bench/test_cli_bench.py`

**Interfaces:**
- Consumes: `CorpusRef`, `resolve_corpus`, `CorpusMissing` from Task 2.
- Produces:
  ```python
  # table.py
  @dataclass(frozen=True)
  class Column:
      key: str
      title: str
      align: str = "left"        # left | right
      volatile: bool = False     # excluded from the determinism comparison

  @dataclass(frozen=True)
  class Row:
      name: str                          # the first column's value
      cells: dict[str, object] = field(default_factory=dict)
      absent: str | None = None          # the install command when the option is missing

  @dataclass(frozen=True)
  class Table:
      layer: str
      columns: tuple[Column, ...]
      rows: tuple[Row, ...]
      corpus: CorpusRef | None
      reproduce: str
      notes: tuple[str, ...] = ()

  def render(table: Table) -> str
  def to_json(table: Table) -> str
  def stable_rows(table: Table) -> list[dict]     # volatile columns dropped

  # registry.py
  @dataclass(frozen=True)
  class BenchOpts:
      corpus: Path | None = None
      live: bool = False
      data_dir: Path = DEFAULT_DATA_DIR

  @dataclass(frozen=True)
  class BenchSpec:
      name: str
      layer: str
      corpus: str                        # default corpus, relative to bench/corpus
      columns: tuple[Column, ...]
      run: Callable[[BenchOpts], Table]

  def register(spec: BenchSpec) -> None
  def get(layer: str) -> BenchSpec        # raises KeyError
  def layer_names() -> list[str]          # registration order
  def run_bench(layer: str, opts: BenchOpts) -> Table
  ```

- [ ] **Step 1: Write the failing test for the table**

Create `tests/bench/test_table.py`:

```python
"""A table must print what it read and how to re-read it, must say `absent`
rather than drop a row, and must compare equal across runs once the timing
columns are set aside."""

import json

from opendaisugi.bench.corpus import CorpusRef
from opendaisugi.bench.table import Column, Row, Table, render, stable_rows, to_json

COLUMNS = (
    Column("option", "option"),
    Column("cases", "cases", align="right"),
    Column("ms", "ms/case", align="right", volatile=True),
)


def _table() -> Table:
    return Table(
        layer="verifier",
        columns=COLUMNS,
        rows=(
            Row(name="python", cells={"option": "python", "cases": 46, "ms": 0.14}),
            Row(name="lean", absent="cd clients/lean && lake build"),
        ),
        corpus=CorpusRef(
            path=__import__("pathlib").Path("/x/verifier.jsonl"),
            rel="bench/corpus/verifier.jsonl",
            sha256="ab" * 32,
        ),
        reproduce="uv run --no-sync daisugi bench verifier",
        notes=("python is the oracle",),
    )


def test_render_shows_every_column_and_the_absent_row():
    art = render(_table())
    assert "option" in art and "ms/case" in art
    assert "python" in art and "46" in art
    assert "lean" in art and "absent" in art
    assert "cd clients/lean && lake build" in art


def test_render_ends_with_the_corpus_and_reproduce_lines():
    lines = [ln for ln in render(_table()).splitlines() if ln.strip()]
    assert lines[-2].startswith("corpus: bench/corpus/verifier.jsonl (abababab)")
    assert lines[-1] == "reproduce: uv run --no-sync daisugi bench verifier"


def test_json_carries_layer_corpus_rows_columns_and_reproduce():
    body = json.loads(to_json(_table()))
    assert body["layer"] == "verifier"
    assert body["corpus"] == {"path": "bench/corpus/verifier.jsonl", "sha256": "ab" * 32}
    assert body["reproduce"] == "uv run --no-sync daisugi bench verifier"
    assert [c["key"] for c in body["columns"]] == ["option", "cases", "ms"]
    assert body["columns"][2]["volatile"] is True
    assert body["rows"][1] == {"name": "lean", "absent": "cd clients/lean && lake build"}


def test_stable_rows_drop_the_volatile_columns():
    assert stable_rows(_table()) == [
        {"name": "python", "option": "python", "cases": 46},
        {"name": "lean", "absent": "cd clients/lean && lake build"},
    ]


def test_a_table_with_no_corpus_says_so_rather_than_printing_a_fake_digest():
    t = Table(layer="loop", columns=COLUMNS, rows=(), corpus=None, reproduce="daisugi bench loop")
    art = render(t)
    assert "corpus: none" in art
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_table.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.table'`

- [ ] **Step 3: Write table.py**

Create `src/opendaisugi/bench/table.py`:

```python
"""One table shape for every layer bench, human and JSON.

Two rules live here. A row for an option that is not installed prints `absent`
with the command that installs it, never nothing: a silently missing row reads
as "we compared everything". And a column that measures time is marked volatile,
so the determinism check can compare two runs without a stopwatch defeating it.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from opendaisugi.bench.corpus import CorpusRef


@dataclass(frozen=True)
class Column:
    key: str
    title: str
    align: str = "left"
    volatile: bool = False


@dataclass(frozen=True)
class Row:
    name: str
    cells: dict[str, object] = field(default_factory=dict)
    absent: str | None = None


@dataclass(frozen=True)
class Table:
    layer: str
    columns: tuple[Column, ...]
    rows: tuple[Row, ...]
    corpus: CorpusRef | None
    reproduce: str
    notes: tuple[str, ...] = ()


def _cell_text(value: object) -> str:
    if isinstance(value, float):
        return f"{value:.2f}"
    return "" if value is None else str(value)


def render(table: Table) -> str:
    """The human table: header, rows, notes, corpus, reproduce."""
    widths = {c.key: len(c.title) for c in table.columns}
    for row in table.rows:
        if row.absent is not None:
            widths[table.columns[0].key] = max(widths[table.columns[0].key], len(row.name))
            continue
        for c in table.columns:
            widths[c.key] = max(widths[c.key], len(_cell_text(row.cells.get(c.key))))

    def _pad(c: Column, text: str) -> str:
        return text.rjust(widths[c.key]) if c.align == "right" else text.ljust(widths[c.key])

    out: list[str] = [f"{table.layer} — {len(table.rows)} option(s)", ""]
    out.append("  " + "  ".join(_pad(c, c.title) for c in table.columns))
    for row in table.rows:
        if row.absent is not None:
            first = _pad(table.columns[0], row.name)
            out.append(f"  {first}  absent — {row.absent}")
            continue
        out.append(
            "  " + "  ".join(_pad(c, _cell_text(row.cells.get(c.key))) for c in table.columns)
        )
    if table.notes:
        out.append("")
        out.extend(f"note: {n}" for n in table.notes)
    out.append("")
    if table.corpus is None:
        out.append("corpus: none")
    else:
        out.append(f"corpus: {table.corpus.rel} ({table.corpus.short})")
    out.append(f"reproduce: {table.reproduce}")
    return "\n".join(out)


def _row_json(row: Row) -> dict:
    if row.absent is not None:
        return {"name": row.name, "absent": row.absent}
    return {"name": row.name, **row.cells}


def to_json(table: Table) -> str:
    """The machine form: layer, corpus, columns, rows, reproduce."""
    body = {
        "layer": table.layer,
        "corpus": (
            None
            if table.corpus is None
            else {"path": table.corpus.rel, "sha256": table.corpus.sha256}
        ),
        "columns": [
            {"key": c.key, "title": c.title, "align": c.align, "volatile": c.volatile}
            for c in table.columns
        ],
        "rows": [_row_json(r) for r in table.rows],
        "notes": list(table.notes),
        "reproduce": table.reproduce,
    }
    return json.dumps(body, indent=2)


def stable_rows(table: Table) -> list[dict]:
    """Rows with every volatile column removed — what a determinism check compares."""
    volatile = {c.key for c in table.columns if c.volatile}
    return [{k: v for k, v in _row_json(r).items() if k not in volatile} for r in table.rows]
```

- [ ] **Step 4: Run the table test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_table.py -q`
Expected: PASS, 5 tests.

- [ ] **Step 5: Write the failing test for the registry and the CLI**

Create `tests/bench/test_cli_bench.py`:

```python
"""`daisugi bench` dispatches to a registered layer, honours --json and
--corpus, and teaches the next command when the layer or corpus is wrong."""

import json

import pytest
from typer.testing import CliRunner

from opendaisugi.bench.registry import BenchOpts, BenchSpec, layer_names, register, run_bench
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.cli import app

runner = CliRunner()


@pytest.fixture()
def fake_layer():
    columns = (Column("option", "option"), Column("n", "n", align="right"))

    def _run(opts: BenchOpts) -> Table:
        from opendaisugi.bench.corpus import resolve_corpus

        ref = resolve_corpus("tasks.jsonl", opts.corpus)
        return Table(
            layer="fake",
            columns=columns,
            rows=(Row(name="one", cells={"option": "one", "n": 1}),),
            corpus=ref,
            reproduce="uv run --no-sync daisugi bench fake",
        )

    register(BenchSpec(name="fake", layer="fake", corpus="tasks.jsonl", columns=columns, run=_run))
    yield
    from opendaisugi.bench import registry

    registry._SPECS.pop("fake", None)


def test_registered_layer_is_listed_and_runnable(fake_layer):
    assert "fake" in layer_names()
    table = run_bench("fake", BenchOpts())
    assert table.rows[0].cells["n"] == 1


def test_cli_prints_the_human_table(fake_layer):
    result = runner.invoke(app, ["bench", "fake"])
    assert result.exit_code == 0, result.output
    assert "reproduce:" in result.output and "corpus:" in result.output


def test_cli_json_is_the_documented_shape(fake_layer):
    result = runner.invoke(app, ["bench", "fake", "--json"])
    assert result.exit_code == 0, result.output
    body = json.loads(result.output)
    assert set(body) >= {"layer", "corpus", "rows", "columns", "reproduce"}


def test_cli_unknown_layer_lists_the_real_ones():
    result = runner.invoke(app, ["bench", "nope"])
    assert result.exit_code == 1
    assert "nope" in result.output
    assert "verifier" in result.output or "fake" in result.output


def test_cli_missing_corpus_names_bench_corpus(fake_layer, tmp_path):
    result = runner.invoke(app, ["bench", "fake", "--corpus", str(tmp_path / "gone.jsonl")])
    assert result.exit_code == 1
    assert "bench/corpus" in result.output


def test_cli_all_runs_every_registered_layer(fake_layer):
    result = runner.invoke(app, ["bench", "all"])
    assert result.exit_code == 0, result.output
    assert result.output.count("reproduce:") >= 1
```

- [ ] **Step 6: Run it to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_cli_bench.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.registry'`

- [ ] **Step 7: Write registry.py, the layers package, and the CLI command**

Create `src/opendaisugi/bench/registry.py`:

```python
"""The set of benches, one per layer, in the order they were registered.

`BenchSpec.run` takes the options and returns a Table. That is the whole
contract, so a new layer is one module and one `register` call.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

from opendaisugi import DEFAULT_DATA_DIR
from opendaisugi.bench.table import Column, Table


@dataclass(frozen=True)
class BenchOpts:
    corpus: Path | None = None
    live: bool = False
    data_dir: Path = field(default_factory=lambda: DEFAULT_DATA_DIR)


@dataclass(frozen=True)
class BenchSpec:
    name: str
    layer: str
    corpus: str
    columns: tuple[Column, ...]
    run: Callable[[BenchOpts], Table]


_SPECS: dict[str, BenchSpec] = {}


def register(spec: BenchSpec) -> None:
    """Add a bench. Re-registering the same layer replaces it (module reload)."""
    _SPECS[spec.layer] = spec


def get(layer: str) -> BenchSpec:
    _load_layers()
    return _SPECS[layer]


def layer_names() -> list[str]:
    _load_layers()
    return list(_SPECS)


def run_bench(layer: str, opts: BenchOpts) -> Table:
    return get(layer).run(opts)


def _load_layers() -> None:
    """Import the layer modules once so their `register` calls have run."""
    from opendaisugi.bench import layers  # noqa: F401 — imported for its side effects
```

Create `src/opendaisugi/bench/layers/__init__.py`:

```python
"""Importing this package registers every layer bench.

Each module calls `registry.register` at import. Adding a layer means adding a
module and one line here — nothing else knows the list.
"""

from __future__ import annotations
```

Add the CLI command to `src/opendaisugi/cli.py`, immediately above `@app.command("modules", hidden=True)` at line 3155:

```python
@app.command("bench", rich_help_panel="Garden")
def bench_cmd(
    layer: str = typer.Argument(..., help="A layer name, or 'pairs', or 'all'."),
    data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory."),
    corpus: Path = typer.Option(None, "--corpus", help="Read this corpus instead of the default."),
    live: bool = typer.Option(False, "--live", help="Run the real option, not the recorded one."),
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Compare the options within one layer over a small committed corpus.

    Every table names the corpus it read and the command that reproduces it. An
    option that is not installed prints as absent with its install command.
    """
    from opendaisugi.bench.corpus import CorpusMissing
    from opendaisugi.bench.registry import BenchOpts, layer_names, run_bench
    from opendaisugi.bench.table import render, to_json

    names = layer_names()
    targets = names if layer == "all" else [layer]
    if layer != "all" and layer not in names:
        typer.echo(f"No bench for {layer!r}.", err=True)
        typer.echo(f"Layers: {', '.join(names)}, all.", err=True)
        typer.echo("Run `daisugi bench verifier` to see one.", err=True)
        raise typer.Exit(code=1)
    tables = []
    for name in targets:
        try:
            tables.append(run_bench(name, BenchOpts(corpus=corpus, live=live, data_dir=data_dir)))
        except CorpusMissing as exc:
            typer.echo(str(exc), err=True)
            raise typer.Exit(code=1) from exc
    if json_output:
        for table in tables:
            typer.echo(to_json(table))
        return
    for i, table in enumerate(tables):
        if i:
            typer.echo("")
        typer.echo(render(table))
```

- [ ] **Step 8: Run the CLI test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_cli_bench.py -q`
Expected: PASS, 6 tests.

- [ ] **Step 9: Run lint and the whole bench suite**

Run: `uv run --no-sync ruff check src/opendaisugi tests/bench && uv run --no-sync pytest tests/bench -q`
Expected: `All checks passed!` and 21 tests passing.

- [ ] **Step 10: Commit**

```bash
git add src/opendaisugi/bench/table.py src/opendaisugi/bench/registry.py \
        src/opendaisugi/bench/layers/__init__.py src/opendaisugi/cli.py \
        tests/bench/test_table.py tests/bench/test_cli_bench.py
git commit -m "bench: one table shape, one registry, one command

Layers are independent by contract, so the front door is per-layer tables rather
than a matrix. The table carries the corpus digest and the reproduce line because
a number nobody can regenerate is a claim, not a measurement, and it prints an
absent row with an install command so a missing option can never read as a
comparison that covered everything."
```

---

### Task 4: `daisugi bench verifier`

**Files:**
- Create: `src/opendaisugi/bench/layers/verifier.py`
- Modify: `src/opendaisugi/bench/layers/__init__.py` (add the import)
- Test: `tests/bench/test_bench_verifier.py`

**Interfaces:**
- Consumes: `ClientSpec`, `VERIFIER_CLIENTS`, `client_argv`, `client_is_built`, `build_hint`
  (Task 1); `resolve_corpus`, `load_jsonl` (Task 2); `Column`, `Row`, `Table`, `BenchOpts`,
  `BenchSpec`, `register` (Task 3); `opendaisugi.conformance.canonical_json`, `_normal_verify`,
  `_normal_decompose`.
- Produces:
  ```python
  COLUMNS: tuple[Column, ...]                # option, cases, agree, disagree, fail-open, ms/case
  def client_verdicts(argv: list[str], cases: list[dict], *, timeout_s: float) -> tuple[dict[str, dict], float]
  def score(cases: list[dict], verdicts: dict[str, dict]) -> dict[str, int]   # agree/disagree/fail_open
  def run(opts: BenchOpts) -> Table
  ```

`fail-open` counts a case where the oracle's expectation is a rejection (`ok` false) and the
client answered `ok` true. That is the one number the whole client campaign gates on, so it is a
column of its own and must be zero.

- [ ] **Step 1: Write the failing test**

Create `tests/bench/test_bench_verifier.py`:

```python
"""The verifier bench must score the oracle against itself perfectly, must count
a client that accepts what the oracle rejects as fail-open, and must print an
absent row rather than skip a client that is not built."""

import json

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers.verifier import COLUMNS, run, score
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json


def test_columns_are_the_spec_columns():
    assert [c.key for c in COLUMNS] == ["option", "cases", "agree", "disagree", "fail_open", "ms"]
    assert [c for c in COLUMNS if c.volatile] == [COLUMNS[-1]]


def test_python_oracle_agrees_with_itself_on_every_case():
    table = run(BenchOpts())
    python = next(r for r in table.rows if r.name == "python")
    cases = load_jsonl(resolve_corpus("verifier.jsonl", None))
    assert python.cells["cases"] == len(cases)
    assert python.cells["agree"] == len(cases)
    assert python.cells["disagree"] == 0
    assert python.cells["fail_open"] == 0


def test_every_client_row_is_present_or_absent_never_missing():
    from opendaisugi.bench.options import VERIFIER_CLIENTS

    table = run(BenchOpts())
    assert {r.name for r in table.rows} == set(VERIFIER_CLIENTS)
    for row in table.rows:
        assert row.absent is not None or "cases" in row.cells


def test_an_unbuilt_client_row_carries_its_build_command():
    table = run(BenchOpts())
    for row in table.rows:
        if row.absent is not None:
            assert row.absent.startswith("cd clients/")


def test_no_client_is_allowed_to_fail_open_on_the_committed_corpus():
    for row in run(BenchOpts()).rows:
        if row.absent is None:
            assert row.cells["fail_open"] == 0, f"{row.name} accepted what the oracle rejects"


def test_score_counts_a_permissive_disagreement_as_fail_open():
    cases = [
        {"id": "a", "kind": "decompose", "expect": {"ok": False}},
        {
            "id": "b",
            "kind": "decompose",
            "expect": {"ok": True, "heads": ["echo"], "reads": [], "writes": []},
        },
    ]
    verdicts = {
        "a": {"id": "a", "ok": True, "heads": ["rm"], "reads": [], "writes": []},
        "b": {"id": "b", "ok": False},
    }
    counts = score(cases, verdicts)
    assert counts == {"agree": 0, "disagree": 2, "fail_open": 1}


def test_a_missing_verdict_counts_as_a_disagreement_not_a_pass():
    cases = [
        {
            "id": "a",
            "kind": "decompose",
            "expect": {"ok": True, "heads": [], "reads": [], "writes": []},
        }
    ]
    assert score(cases, {}) == {"agree": 0, "disagree": 1, "fail_open": 0}


def test_json_shape_and_reproduce_line():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "verifier"
    assert body["reproduce"].startswith("uv run --no-sync daisugi bench verifier")
    assert body["corpus"]["path"].endswith("verifier.jsonl")


def test_the_reproduce_line_reruns_to_the_same_rows():
    a, b = run(BenchOpts()), run(BenchOpts())
    assert stable_rows(a) == stable_rows(b)


def test_the_printed_reproduce_line_actually_runs():
    """Two in-process calls never touch the printed string, so a typo in it is
    invisible. Shell the command the table prints and compare the rows."""
    import json as _json
    import shlex
    import subprocess

    from opendaisugi.bench.options import repo_root

    root = repo_root()
    assert root is not None, "this test needs a checkout"
    table = run(BenchOpts())
    argv = shlex.split(table.reproduce) + ["--json"]
    proc = subprocess.run(argv, cwd=root, capture_output=True, text=True, timeout=180, check=False)
    assert proc.returncode == 0, proc.stderr[-500:]
    printed = _json.loads(proc.stdout)
    volatile = {c["key"] for c in printed["columns"] if c["volatile"]}
    got = [{k: v for k, v in row.items() if k not in volatile} for row in printed["rows"]]
    assert got == stable_rows(table)


def test_the_bench_finishes_well_under_thirty_seconds():
    import time

    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_bench_verifier.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.layers.verifier'`

- [ ] **Step 3: Write the implementation**

Create `src/opendaisugi/bench/layers/verifier.py`:

```python
"""`daisugi bench verifier` — the five clients over the committed corpus.

This is the conformance harness (clients/gate.py, clients/compare.py) reduced to
one table. The one column that matters for safety is `fail-open`: cases where the
oracle rejects and the client accepts. It must be zero. Everything else is
information.

Python is the oracle, so its row is a self-check: it proves the corpus and the
comparison are wired correctly before any client is read.
"""

from __future__ import annotations

import subprocess
import time

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.options import (
    VERIFIER_CLIENTS,
    build_hint,
    client_argv,
    client_is_built,
)
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.conformance import _normal_decompose, _normal_verify, canonical_json

CORPUS = "verifier.jsonl"

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("cases", "cases", align="right"),
    Column("agree", "agree", align="right"),
    Column("disagree", "disagree", align="right"),
    Column("fail_open", "fail-open", align="right"),
    Column("ms", "ms/case", align="right", volatile=True),
)


def client_verdicts(
    argv: list[str], cases: list[dict], *, timeout_s: float = 120.0
) -> tuple[dict[str, dict], float]:
    """Feed every case on one stdin stream; return verdicts by id and elapsed ms.

    A client that dies, times out, or writes a non-JSON line yields no verdicts
    for the cases it did not answer. Those count as disagreements, never as
    passes — a silent client must not score well.
    """
    stream = "".join(canonical_json(c) + "\n" for c in cases)
    t0 = time.monotonic()
    try:
        proc = subprocess.run(  # noqa: S603 — the operator's own client binary
            argv, input=stream, capture_output=True, text=True, timeout=timeout_s, check=False
        )
        stdout = proc.stdout
    except (OSError, subprocess.TimeoutExpired):
        stdout = ""
    elapsed_ms = (time.monotonic() - t0) * 1000
    out: dict[str, dict] = {}
    for line in stdout.splitlines():
        if not line.strip():
            continue
        try:
            v = dict(__import__("json").loads(line))
        except ValueError:
            continue
        if "id" in v:
            out[str(v["id"])] = v
    return out, elapsed_ms


def score(cases: list[dict], verdicts: dict[str, dict]) -> dict[str, int]:
    """Agreement with the oracle's own expectation, plus the fail-open count.

    fail-open = the oracle's expectation rejects and the client accepts. That is
    the direction that lets a command through unchecked, so it gets its own
    number rather than hiding inside `disagree`.
    """
    agree = disagree = fail_open = 0
    for case in cases:
        normal = _normal_verify if case["kind"] == "verify" else _normal_decompose
        expected = normal(case["expect"])
        got = verdicts.get(case["id"])
        if got is None or "ok" not in got:
            disagree += 1
            continue
        if normal(got) == expected:
            agree += 1
            continue
        disagree += 1
        if bool(got["ok"]) and not bool(case["expect"]["ok"]):
            fail_open += 1
    return {"agree": agree, "disagree": disagree, "fail_open": fail_open}


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    rows: list[Row] = []
    for name, spec in VERIFIER_CLIENTS.items():
        if not client_is_built(spec):
            rows.append(Row(name=name, absent=build_hint(spec)))
            continue
        verdicts, elapsed_ms = client_verdicts(client_argv(spec), cases)
        counts = score(cases, verdicts)
        rows.append(
            Row(
                name=name,
                cells={
                    "option": name,
                    "cases": len(cases),
                    **counts,
                    "ms": elapsed_ms / max(1, len(cases)),
                },
            )
        )
    return Table(
        layer="verifier",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        reproduce=f"uv run --no-sync daisugi bench verifier --corpus {ref.rel}",
        notes=(
            "the corpus path is repo-relative, so run the reproduce line from the checkout root",
            "python is the oracle; agree means agrees with the reference, not provably correct",
            "fail-open counts cases the oracle rejects and the client accepts; it must be 0",
            "a built client may predate the current clients/ source; rebuild before you trust a row",
            "go, rust and ts reach Full profile by spawning a z3 binary; with z3 off PATH they "
            "deny every predicate envelope, which is fail-closed and shows here as disagreement",
        ),
    )


register(BenchSpec(name="verifier", layer="verifier", corpus=CORPUS, columns=COLUMNS, run=run))
```

Replace the body of `src/opendaisugi/bench/layers/__init__.py` below its docstring with:

```python
from __future__ import annotations

from opendaisugi.bench.layers import verifier  # noqa: F401 — registers the bench
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_bench_verifier.py -q`
Expected: PASS, 10 tests.

If `test_no_client_is_allowed_to_fail_open_on_the_committed_corpus` fails, do **not** relax it.
A non-zero fail-open on 46 hand-written cases is a real finding: record the failing client and
case ids in the commit message and stop for review.

- [ ] **Step 5: Check the real command**

Run: `uv run --no-sync daisugi bench verifier`
Expected: five rows, `python` at 47/47, unbuilt clients showing `absent — cd clients/… && …`, then
a `corpus:` line and a `reproduce:` line.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/bench/layers/verifier.py src/opendaisugi/bench/layers/__init__.py \
        tests/bench/test_bench_verifier.py
git commit -m "bench: score the five verifier clients as one table

The conformance campaign already had the numbers; it did not have a command that
prints them. Fail-open gets its own column rather than hiding inside disagree,
because a client that accepts what the oracle rejects is the only disagreement
that can let a command run unchecked."
```

---

### Task 5: `daisugi bench matcher`, and `scripts/matcher_fpr.py` becomes a shim

**Files:**
- Create: `src/opendaisugi/bench/layers/matcher.py`
- Modify: `src/opendaisugi/bench/layers/__init__.py` (add the import)
- Modify: `scripts/matcher_fpr.py:1-112` (replace the whole file with a shim that keeps `--journal`)
- Test: `tests/bench/test_bench_matcher.py`

**Interfaces:**
- Consumes: `MATCHER_BACKENDS` (Task 1); `resolve_corpus`, `load_jsonl` (Task 2); `Column`, `Row`,
  `Table`, `BenchOpts`, `BenchSpec`, `register` (Task 3).
- Produces:
  ```python
  COLUMNS: tuple[Column, ...]        # option, threshold, fpr, recall, ms_embed, mb
  THRESHOLDS: tuple[float, ...]
  TARGET_FPR: float                  # 0.0256 — MiniLM@0.55, the line's matching target

  def build_embedder(name: str) -> object | None      # None when the package or model is absent
  def measure(embed, tasks: list[str], paras: list[dict]) -> dict[str, float]
  def fpr_table(embed, tasks: list[str], thresholds) -> list[tuple[float, float]]
  def run(opts: BenchOpts) -> Table
  ```

The bench never downloads. `build_embedder` constructs each backend with `HF_HUB_OFFLINE=1` set
for the duration, so a package that is installed but whose model is not cached reports absent with
the fetch command rather than pulling ~90 MB inside a 30 s budget.

`ms/embed` is measured on a **second, warm** pass so the number is embedding cost, not model load.
`MB` is the on-disk size of the backend's cache directory, not process RSS.

- [ ] **Step 1: Write the failing test**

Create `tests/bench/test_bench_matcher.py`:

```python
"""The matcher bench compares the built embedders at their own thresholds on a
small committed corpus, never downloads, and reports the lexical floor always."""

import json
import time

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers.matcher import (
    COLUMNS,
    TARGET_FPR,
    build_embedder,
    fpr_table,
    measure,
    run,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json


def test_cache_mb_measures_one_backend_not_the_whole_hub(tmp_path, monkeypatch):
    """Summing the whole HF cache would report the operator's entire model
    collection as each backend's size."""
    from opendaisugi.bench.layers.matcher import _cache_mb, _hf_snapshot_dir

    assert _cache_mb("lexical") == 0.0
    assert _hf_snapshot_dir("minishlab/potion-base-8M").name == (
        "models--minishlab--potion-base-8M"
    )
    assert _hf_snapshot_dir("sentence-transformers/all-MiniLM-L6-v2").name == (
        "models--sentence-transformers--all-MiniLM-L6-v2"
    )


def test_columns_and_the_matched_target():
    assert [c.key for c in COLUMNS] == ["option", "threshold", "fpr", "recall", "ms_embed", "mb"]
    assert TARGET_FPR == 0.0256
    assert {c.key for c in COLUMNS if c.volatile} == {"ms_embed"}


def test_lexical_always_has_a_real_row_because_it_needs_no_package():
    lex = next(r for r in run(BenchOpts()).rows if r.name == "lexical")
    assert lex.absent is None
    assert lex.cells["threshold"] == 0.25
    assert 0.0 <= lex.cells["fpr"] <= 1.0
    assert 0.0 <= lex.cells["recall"] <= 1.0


def test_every_backend_appears_present_or_absent():
    from opendaisugi.bench.options import MATCHER_BACKENDS

    rows = run(BenchOpts()).rows
    assert {r.name for r in rows} == set(MATCHER_BACKENDS)
    for row in rows:
        assert row.absent is not None or "fpr" in row.cells


def test_an_absent_backend_teaches_the_install_or_fetch():
    for row in run(BenchOpts()).rows:
        if row.absent is not None:
            assert "opendaisugi[" in row.absent or "OPENDAISUGI_" in row.absent


def test_recall_counts_paraphrase_pairs_over_the_threshold():
    tasks = [r["task"] for r in load_jsonl(resolve_corpus("tasks.jsonl", None))]
    paras = load_jsonl(resolve_corpus("paraphrases.jsonl", None))
    lexical = build_embedder("lexical")
    assert lexical is not None
    stats = measure(lexical.encode, tasks, paras)
    assert stats["pairs_same"] == 12
    assert stats["pairs_diff"] == 66  # C(12, 2)
    assert 0.0 <= stats["recall"] <= 1.0


def test_fpr_is_monotone_non_increasing_in_the_threshold():
    lexical = build_embedder("lexical")
    tasks = [r["task"] for r in load_jsonl(resolve_corpus("tasks.jsonl", None))]
    rates = [f for _, f in fpr_table(lexical.encode, tasks, (0.1, 0.3, 0.5, 0.7))]
    assert rates == sorted(rates, reverse=True)


def test_the_bench_never_downloads_a_model(monkeypatch):
    """A download inside a 30 s budget is a 90 MB surprise. Make the hub raise:
    every backend must either load from cache or report absent."""
    import os

    calls: list[str] = []

    def _boom(*args, **kwargs):
        calls.append("download")
        raise AssertionError("the bench tried to download a model")

    for target in ("huggingface_hub.file_download.http_get", "huggingface_hub.snapshot_download"):
        monkeypatch.setattr(target, _boom, raising=False)

    before = os.environ.get("HF_HUB_OFFLINE")
    table = run(BenchOpts())
    assert calls == []
    assert table.rows
    # And the guard is restored, not left set for the rest of the suite.
    assert os.environ.get("HF_HUB_OFFLINE") == before


def test_offline_is_set_during_construction_and_restored_after(monkeypatch):
    import os

    from opendaisugi.bench.layers.matcher import _offline

    monkeypatch.delenv("HF_HUB_OFFLINE", raising=False)
    with _offline():
        assert os.environ["HF_HUB_OFFLINE"] == "1"
    assert "HF_HUB_OFFLINE" not in os.environ


def test_json_and_determinism():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "matcher"
    assert body["reproduce"].startswith("uv run --no-sync daisugi bench matcher")
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))


def test_the_bench_finishes_well_under_thirty_seconds():
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_bench_matcher.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.layers.matcher'`

- [ ] **Step 3: Write the implementation**

Create `src/opendaisugi/bench/layers/matcher.py`:

```python
"""`daisugi bench matcher` — the built embedders at their own thresholds.

This is `scripts/matcher_fpr.py` grown into a table. Each backend is scored at
the threshold it actually ships with (ADR-0018, ADR-0019), because comparing two
embedders at one shared cosine number compares nothing: their score scales differ.
False-positive rate comes from every distinct-task pair in the corpus; recall
comes from the hand-written paraphrase pairs.

The bench never downloads. A package that is installed but whose model is not
cached reports absent with the fetch command, so a 30 s budget cannot turn into a
90 MB pull.
"""

from __future__ import annotations

import contextlib
import itertools
import os
import time
from pathlib import Path

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.options import MATCHER_BACKENDS
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table

CORPUS = "tasks.jsonl"
TARGET_FPR = 0.0256  # MiniLM@0.55's measured rate (ADR-0018); every threshold matches it.
THRESHOLDS: tuple[float, ...] = (
    0.15,
    0.20,
    0.25,
    0.30,
    0.35,
    0.40,
    0.45,
    0.50,
    0.55,
    0.59,
    0.65,
    0.70,
)

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("threshold", "threshold", align="right"),
    Column("fpr", "FPR@thr", align="right"),
    Column("recall", "recall@thr", align="right"),
    Column("ms_embed", "ms/embed", align="right", volatile=True),
    Column("mb", "MB", align="right"),
)


@contextlib.contextmanager
def _offline():
    """Force the hub offline for the duration. A bench must never download."""
    prev = os.environ.get("HF_HUB_OFFLINE")
    os.environ["HF_HUB_OFFLINE"] = "1"
    try:
        yield
    finally:
        if prev is None:
            os.environ.pop("HF_HUB_OFFLINE", None)
        else:
            os.environ["HF_HUB_OFFLINE"] = prev


def build_embedder(name: str):
    """Construct one backend, or return None when it cannot run offline right now."""
    from opendaisugi import _search

    with _offline():
        try:
            if name == "lexical":
                return _search._LexicalEmbedder()
            if name == "potion":
                return _search._PotionEmbedder(_search.resolve_potion_model())
            if name == "all-MiniLM-L6-v2":
                return _search._STEmbedder()
            if name == "int8":
                return _search._Int8Embedder()  # spec 10; absent until it lands
        except Exception:  # noqa: BLE001 — a bench reports absence, it does not raise
            return None
    return None


def _threshold_for(name: str) -> float:
    from opendaisugi import _search
    from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

    return {
        "lexical": _search._LEXICAL_THRESHOLD,
        "potion": _search._POTION_THRESHOLD,
        "all-MiniLM-L6-v2": DEFAULT_PATHWAY_THRESHOLD,
    }.get(name, getattr(_search, "_INT8_THRESHOLD", 0.5))


def _hf_snapshot_dir(model_id: str) -> Path:
    """This model's own directory in the HF cache, never the whole cache tree.

    Summing `~/.cache/huggingface/hub` would report the operator's entire model
    collection as each backend's size, which is a wrong number printed with
    confidence.
    """
    return (
        Path.home() / ".cache" / "huggingface" / "hub" / ("models--" + model_id.replace("/", "--"))
    )


def _cache_mb(name: str) -> float:
    """On-disk size of THIS backend's model files, in MB. Zero for a model-free floor."""
    from opendaisugi._search import _MODEL_NAME, resolve_potion_model

    if name == "lexical":
        return 0.0
    if name == "potion":
        root = _hf_snapshot_dir(resolve_potion_model())
    elif name == _MODEL_NAME:
        root = _hf_snapshot_dir(f"sentence-transformers/{_MODEL_NAME}")
    elif name == "int8":
        import os

        override = os.environ.get("OPENDAISUGI_INT8_MODEL")
        root = (
            Path(override)
            if override
            else Path.home() / ".cache" / "opendaisugi" / "models" / "minilm-int8"
        )
    else:
        return 0.0
    if not root.exists():
        return 0.0
    total = sum(f.stat().st_size for f in root.rglob("*") if f.is_file())
    return round(total / (1024 * 1024), 1)


def fpr_table(embed, tasks: list[str], thresholds=THRESHOLDS) -> list[tuple[float, float]]:
    """(threshold, false-positive rate) over every distinct-task pair."""
    import numpy as np

    vecs = np.asarray(embed(tasks))
    pairs = list(itertools.combinations(range(len(tasks)), 2))
    idx = np.asarray(pairs)
    scores = np.sum(vecs[idx[:, 0]] * vecs[idx[:, 1]], axis=1)  # rows are L2-normalized
    return [(t, float((scores >= t).mean())) for t in thresholds]


def measure(embed, tasks: list[str], paras: list[dict], *, threshold: float = 0.0) -> dict:
    """Pair scores both ways, plus warm ms/embed.

    `same_scores` and `diff_scores` are returned raw so the caller can re-score at
    any threshold without embedding twice. `recall` is at the threshold given,
    which defaults to 0.0 (every pair counts) for callers that only want the raw
    scores; `run()` passes each backend's own shipped threshold.
    """
    import numpy as np

    task_by_id = {r["id"]: r["task"] for r in load_jsonl(resolve_corpus(CORPUS, None))}

    vecs = np.asarray(embed(tasks))
    idx = np.asarray(list(itertools.combinations(range(len(tasks)), 2)))
    diff_scores = np.sum(vecs[idx[:, 0]] * vecs[idx[:, 1]], axis=1)

    left = [task_by_id[p["of"]] for p in paras]
    right = [p["task"] for p in paras]
    same_scores = np.sum(np.asarray(embed(left)) * np.asarray(embed(right)), axis=1)

    t0 = time.monotonic()  # warm pass: the model is loaded, this is embedding cost
    embed(tasks)
    ms_embed = ((time.monotonic() - t0) * 1000) / max(1, len(tasks))

    return {
        "pairs_same": float(len(same_scores)),
        "pairs_diff": float(len(diff_scores)),
        "same_scores": same_scores,
        "diff_scores": diff_scores,
        "ms_embed": ms_embed,
        "recall": float((same_scores >= threshold).mean()),
        "fpr": float((diff_scores >= threshold).mean()),
    }


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    tasks = [r["task"] for r in load_jsonl(ref)]
    paras = load_jsonl(resolve_corpus("paraphrases.jsonl", None))
    rows: list[Row] = []
    for name, spec in MATCHER_BACKENDS.items():
        embedder = build_embedder(name)
        if embedder is None:
            hint = spec.extra or "OPENDAISUGI_POTION_MODEL=<local dir>"
            rows.append(Row(name=name, absent=f"pip install '{hint}' and cache the model"))
            continue
        thr = _threshold_for(name)
        stats = measure(embedder.encode, tasks, paras, threshold=thr)
        rows.append(
            Row(
                name=name,
                cells={
                    "option": name,
                    "threshold": thr,
                    "fpr": round(stats["fpr"], 4),
                    "recall": round(stats["recall"], 4),
                    "ms_embed": stats["ms_embed"],
                    "mb": _cache_mb(name),
                },
            )
        )
    return Table(
        layer="matcher",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        reproduce=f"uv run --no-sync daisugi bench matcher --corpus {ref.rel}",
        notes=(
            f"each backend is scored at its own shipped threshold, FPR-matched to {TARGET_FPR}",
            "this bench never downloads; derive a new threshold with scripts/matcher_fpr.py",
        ),
    )


register(BenchSpec(name="matcher", layer="matcher", corpus=CORPUS, columns=COLUMNS, run=run))
```

Add to `src/opendaisugi/bench/layers/__init__.py`:

```python
from opendaisugi.bench.layers import matcher  # noqa: F401 — registers the bench
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_bench_matcher.py -q`
Expected: PASS, 9 tests.

- [ ] **Step 5: Turn `scripts/matcher_fpr.py` into a shim**

ADR-0018 and ADR-0019 cite this script as the command that produced the shipped thresholds, so its
interface must not change: it still takes `--journal PATH` and still prints a per-threshold table
per backend. Replace the whole file with:

```python
"""Measure each built matcher backend's false-positive rate on real journal
tasks, at a range of thresholds — the tool the next threshold derivation reaches
for (ADR-0018, ADR-0019). Not a test: it prints, it doesn't assert.

The measurement now lives in `opendaisugi.bench.layers.matcher` so
`daisugi bench matcher` and this script cannot drift. This file stays because the
ADRs cite it by name as the command that produced the shipped thresholds.

Usage: uv run --no-sync python scripts/matcher_fpr.py [--journal PATH]
"""

from __future__ import annotations

import argparse
import sqlite3
from pathlib import Path

from opendaisugi.bench.layers.matcher import (
    MATCHER_BACKENDS,
    TARGET_FPR,
    THRESHOLDS,
    build_embedder,
    fpr_table,
)

DEFAULT_JOURNAL = Path.home() / ".opendaisugi" / "journal" / "index.db"
MIN_TASK_LEN = 4  # drop empty/placeholder tasks


def load_distinct_tasks(db_path: Path) -> list[str]:
    con = sqlite3.connect(str(db_path))
    try:
        rows = con.execute("SELECT DISTINCT task FROM traces").fetchall()
    finally:
        con.close()
    return sorted({r[0].strip() for r in rows if r[0] and len(r[0].strip()) >= MIN_TASK_LEN})


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--journal", type=Path, default=DEFAULT_JOURNAL)
    args = parser.parse_args()

    if not args.journal.exists():
        print(f"no journal at {args.journal}; nothing to measure.")
        return
    tasks = load_distinct_tasks(args.journal)
    print(f"{args.journal}: {len(tasks)} distinct usable tasks")
    if len(tasks) < 2:
        print("not enough distinct tasks to sample pairs.")
        return

    for name in MATCHER_BACKENDS:
        embedder = build_embedder(name)
        if embedder is None:
            print(f"\n=== {name} === skipped: not installed, or its model is not cached")
            continue
        print(f"\n=== {name} ===")
        best_thr, best_gap = THRESHOLDS[0], float("inf")
        for thr, fpr in fpr_table(embedder.encode, tasks, THRESHOLDS):
            gap = abs(fpr - TARGET_FPR)
            if gap < best_gap:
                best_gap, best_thr = gap, thr
            print(f"  thr={thr:.2f}  fpr={fpr:.4f}")
        print(f"  -> closest to target FPR {TARGET_FPR:.4f}: thr={best_thr:.2f}")


if __name__ == "__main__":
    main()
```

Add `MATCHER_BACKENDS` to the imports at the top of `src/opendaisugi/bench/layers/matcher.py`
re-export list so the shim's import works (it already imports it from `options`; add
`__all__` is not needed, the name is module-level).

The shim now uses every distinct pair rather than a 20,000-pair random sample. On a journal with
258 distinct tasks that is 33,153 pairs, which is more data than the sample and still one pass, so
the numbers get tighter, never looser. Record that in the commit message.

- [ ] **Step 6: Verify the shim still runs**

Run: `uv run --no-sync python scripts/matcher_fpr.py --journal /nonexistent.db`
Expected: `no journal at /nonexistent.db; nothing to measure.`

Run: `uv run --no-sync ruff check src/opendaisugi/bench scripts tests/bench`
Expected: `All checks passed!`

- [ ] **Step 7: Commit**

```bash
git add src/opendaisugi/bench/layers/matcher.py src/opendaisugi/bench/layers/__init__.py \
        scripts/matcher_fpr.py tests/bench/test_bench_matcher.py
git commit -m "bench: compare the embedders at their own thresholds, never downloading

Two embedders compared at one cosine number compare nothing, because their score
scales differ, so each row is scored at the threshold it ships with. The FPR
measurement moves into the package and the script becomes a shim: ADR-0018 and
ADR-0019 cite scripts/matcher_fpr.py by name as the command that produced the
shipped thresholds, so its interface has to keep working while the code stops
being a second copy. The shim now uses every distinct pair instead of a 20k
sample, which tightens the estimate rather than loosening it."
```

---

### Task 6: `daisugi bench router`

**Files:**
- Create: `src/opendaisugi/bench/layers/router.py`
- Modify: `src/opendaisugi/bench/layers/__init__.py` (add the import)
- Test: `tests/bench/test_bench_router.py`

**Interfaces:**
- Consumes: `resolve_corpus`, `load_jsonl`, `load_json` (Task 2); `Column`, `Row`, `Table`,
  `BenchOpts`, `BenchSpec`, `register` (Task 3); `opendaisugi.gateway.route_turn`,
  `estimate_prefix_tokens`, `price_turn`; `opendaisugi.routing.estimate_difficulty`.
- Produces:
  ```python
  COLUMNS: tuple[Column, ...]        # option, turns, local_share, cost, pass_rate, source
  ROUTERS: tuple[str, ...]           # ("rules", "switchyard:stage", "switchyard:escalation", "off")

  def choose(option: str, row: dict, targets: dict[str, str]) -> str    # the model this option picks
  def stage_router(difficulty: float, targets: dict[str, str]) -> str
  def escalation_router(difficulty: float, targets: dict[str, str]) -> str
  def run(opts: BenchOpts) -> Table
  ```

`stage_router` and `escalation_router` are **fakes**: pure functions that mimic the two strategies
Switchyard documents, so the row exists before spec 09 lands. Their `source` cell says `fake`. When
the real `switchyard-server` binary is on PATH, the row's `source` says `live` and the choice comes
from it. Nobody may read a `fake` row as a measurement of NVIDIA's router.

- [ ] **Step 1: Write the failing test**

Create `tests/bench/test_bench_router.py`:

```python
"""The router bench replays a committed journal slice through each routing
option, prices the result from a committed price table, and never invents a
pass-rate it has no outcomes for."""

import json
import time

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.layers.router import (
    COLUMNS,
    ROUTERS,
    choose,
    escalation_router,
    run,
    stage_router,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json

TARGETS = {"local": "L", "cheap": "C", "capable": "K"}


def test_columns_and_options():
    assert [c.key for c in COLUMNS] == [
        "option",
        "turns",
        "local_share",
        "cost",
        "pass_rate",
        "source",
    ]
    assert ROUTERS == ("rules", "switchyard:stage", "switchyard:escalation", "off")


def test_off_never_changes_the_requested_model():
    row = {
        "body": {
            "model": "claude-opus-4-8",
            "messages": [{"role": "user", "content": "list the files"}],
        },
        "prefix_tokens": 0,
    }
    assert choose("off", row, TARGETS) == "claude-opus-4-8"


def test_rules_takes_the_local_rung_on_an_easy_turn_and_keeps_a_hard_one():
    easy = {
        "body": {
            "model": "claude-opus-4-8",
            "messages": [{"role": "user", "content": "list the files in src"}],
        },
        "prefix_tokens": 0,
    }
    hard = {
        "body": {
            "model": "claude-opus-4-8",
            "messages": [
                {"role": "user", "content": "debug the race condition in the pane watcher"}
            ],
        },
        "prefix_tokens": 0,
    }
    assert choose("rules", easy, TARGETS) == "L"
    assert choose("rules", hard, TARGETS) == "claude-opus-4-8"


def test_rules_reaches_the_cheap_rung_when_no_local_model_is_offered():
    no_local = dict(TARGETS, local="claude-haiku-4-5")
    easy = {
        "body": {
            "model": "claude-opus-4-8",
            "messages": [{"role": "user", "content": "list the files in src"}],
        },
        "prefix_tokens": 0,
    }
    assert choose("rules", easy, no_local) == "claude-haiku-4-5"


def test_rules_local_share_is_not_structurally_zero():
    """The bug this guards: without local_model the built-in router can never
    reach tier1-local, so its `local share` column would always read 0.0 and the
    comparison against the Switchyard rows would be rigged."""
    share = {r.name: r.cells["local_share"] for r in run(BenchOpts()).rows}
    assert share["rules"] > 0.0


def test_rules_keeps_a_large_cached_prefix_on_the_requested_model_when_no_local_rung():
    """Stickiness never blocks the local rung (ADR-0015), so the sticky path is
    only observable when the local and cheap targets are the same model."""
    no_local = dict(TARGETS, local="claude-haiku-4-5", cheap="claude-haiku-4-5")
    sticky = {
        "body": {
            "model": "claude-opus-4-8",
            "messages": [{"role": "user", "content": "add a docstring here"}],
        },
        "prefix_tokens": 6000,
    }
    assert choose("rules", sticky, no_local) == "claude-opus-4-8"


def test_the_two_fake_switchyard_strategies_differ_on_a_middling_turn():
    assert stage_router(0.10, TARGETS) == "L"
    assert stage_router(0.35, TARGETS) == "C"
    assert stage_router(0.80, TARGETS) == "K"
    assert escalation_router(0.35, TARGETS) == "L"
    assert escalation_router(0.80, TARGETS) == "K"


def test_every_router_option_gets_a_row_with_the_corpus_turn_count():
    turns = len(load_jsonl(resolve_corpus("journal-slice.jsonl", None)))
    rows = run(BenchOpts()).rows
    assert [r.name for r in rows] == list(ROUTERS)
    for row in rows:
        assert row.cells["turns"] == turns


def test_off_costs_at_least_as_much_as_every_router():
    cells = {r.name: r.cells for r in run(BenchOpts()).rows}
    for name in ("rules", "switchyard:stage", "switchyard:escalation"):
        assert cells[name]["cost"] <= cells["off"]["cost"], name


def test_off_routes_nothing_local_and_the_fakes_say_they_are_fake():
    cells = {r.name: r.cells for r in run(BenchOpts()).rows}
    assert cells["off"]["local_share"] == 0.0
    assert cells["switchyard:stage"]["source"] in {"fake", "live"}
    assert cells["rules"]["source"] == "built-in"


def test_pass_rate_is_absent_not_a_fake_hundred_percent():
    for row in run(BenchOpts()).rows:
        assert row.cells["pass_rate"] == "n/a (no outcomes)"


def test_local_share_is_a_fraction_of_the_corpus():
    for row in run(BenchOpts()).rows:
        assert 0.0 <= row.cells["local_share"] <= 1.0


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "router"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0


def test_prices_come_from_the_committed_table():
    prices = load_json(resolve_corpus("prices.json", None))
    assert prices["qwen2.5-coder-7b"] == [0.0, 0.0]
```

- [ ] **Step 2: Run it to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_bench_router.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.layers.router'`

- [ ] **Step 3: Write the implementation**

Create `src/opendaisugi/bench/layers/router.py`:

```python
"""`daisugi bench router` — one committed journal slice, four routing options.

The gateway is the meter, so the comparison is honest only if every option sees
the same turns and the same price table (master spec section 5.7). Costs come
from `bench/corpus/prices.json`, not from a hard-coded table, so an operator can
edit the file for their own plan and re-run the reproduce line.

The two Switchyard rows use **fakes**: pure functions that mimic the stage and
escalation strategies Switchyard documents. Their source cell says `fake`. When
the real binary is on PATH the row says `live`. A fake row is a shape, not a
measurement of NVIDIA's router, and the note line says so on every table.
"""

from __future__ import annotations

import shutil

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.gateway import price_turn, route_turn
from opendaisugi.routing import estimate_difficulty

CORPUS = "journal-slice.jsonl"
ROUTERS: tuple[str, ...] = ("rules", "switchyard:stage", "switchyard:escalation", "off")

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("turns", "turns", align="right"),
    Column("local_share", "local share", align="right"),
    Column("cost", "est. cost $", align="right"),
    Column("pass_rate", "pass-rate", align="right"),
    Column("source", "source"),
)


def _latest_user_text(body: dict) -> str:
    from opendaisugi.gateway import _latest_user_text as _impl

    return _impl(body)


def stage_router(difficulty: float, targets: dict[str, str]) -> str:
    """Fake stage router: three bands, cheapest first. Mimics Switchyard's shape."""
    if difficulty < 0.25:
        return targets["local"]
    if difficulty < 0.5:
        return targets["cheap"]
    return targets["capable"]


def escalation_router(difficulty: float, targets: dict[str, str]) -> str:
    """Fake escalation router: try local, escalate only when the turn is hard."""
    return targets["capable"] if difficulty >= 0.5 else targets["local"]


def choose(option: str, row: dict, targets: dict[str, str]) -> str:
    """The model this option sends the turn to."""
    body = row["body"]
    if option == "off":
        return str(body.get("model", ""))
    if option == "rules":
        # The built-in router gets the SAME model set as the Switchyard rows, or
        # its `local share` is structurally 0.0 and the comparison is rigged: the
        # local rung only exists when a local model is configured
        # (gateway.py's ladder, ADR-0015).
        return route_turn(
            body,
            cheap_model=targets["cheap"],
            local_model=targets["local"],
            prefix_tokens=row.get("prefix_tokens"),
        ).model
    difficulty = estimate_difficulty(_latest_user_text(body))
    if option == "switchyard:stage":
        return stage_router(difficulty, targets)
    if option == "switchyard:escalation":
        return escalation_router(difficulty, targets)
    raise KeyError(option)


def _switchyard_source() -> str:
    """`live` once the real chooser is installed (spec 09), `fake` until then."""
    return "live" if shutil.which("switchyard-server") else "fake"


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    turns = load_jsonl(ref)
    targets = load_json(resolve_corpus("switchyard-targets.json", None))
    price_pairs = load_json(resolve_corpus("prices.json", None))
    prices = {k: (float(v[0]), float(v[1])) for k, v in price_pairs.items()}
    local = targets["local"]

    rows: list[Row] = []
    for option in ROUTERS:
        chosen = [choose(option, row, targets) for row in turns]
        cost = 0.0
        for model, row in zip(chosen, turns, strict=True):
            usage = row["usage"]
            cost += price_turn(
                model,
                int(usage.get("input_tokens", 0)),
                int(usage.get("output_tokens", 0)),
                cache_read_tokens=int(usage.get("cache_read_input_tokens", 0)),
                cache_creation_tokens=int(usage.get("cache_creation_input_tokens", 0)),
                prices=prices,
            ).dollars
        rows.append(
            Row(
                name=option,
                cells={
                    "option": option,
                    "turns": len(turns),
                    "local_share": round(sum(m == local for m in chosen) / max(1, len(turns)), 4),
                    "cost": round(cost, 4),
                    "pass_rate": "n/a (no outcomes)",
                    "source": ("built-in" if option in ("rules", "off") else _switchyard_source()),
                },
            )
        )
    return Table(
        layer="router",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        reproduce=f"uv run --no-sync daisugi bench router --corpus {ref.rel}",
        notes=(
            "prices come from bench/corpus/prices.json; edit it for your plan and re-run",
            "every option is offered the same three targets, so local share compares like for like",
            "a switchyard row marked fake mimics the documented strategy; it is not NVIDIA's router",
            "pass-rate needs recorded outcomes; this corpus has none, so the column says so",
        ),
    )


register(BenchSpec(name="router", layer="router", corpus=CORPUS, columns=COLUMNS, run=run))
```

Add to `src/opendaisugi/bench/layers/__init__.py`:

```python
from opendaisugi.bench.layers import router  # noqa: F401 — registers the bench
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_bench_router.py -q`
Expected: PASS, 11 tests.

- [ ] **Step 5: Run lint and check the command**

Run: `uv run --no-sync ruff check src/opendaisugi/bench tests/bench && uv run --no-sync daisugi bench router`
Expected: `All checks passed!` then four rows with a `pass-rate` of `n/a (no outcomes)`.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/bench/layers/router.py src/opendaisugi/bench/layers/__init__.py \
        tests/bench/test_bench_router.py
git commit -m "bench: replay one journal slice through four routing options

The gateway is the meter, so a router comparison is only honest when every option
sees the same turns at the same prices; both now come from committed files an
operator can edit. The Switchyard rows are explicitly fakes of the documented
strategies until spec 09 lands, and they say so in a column, because a shape
printed next to real numbers reads as a measurement unless it is labelled. The
pass-rate column prints its own absence rather than a fake hundred percent."
```

---

### Task 7: `daisugi bench gate-path`

**Files:**
- Create: `src/opendaisugi/bench/layers/gate_path.py`
- Modify: `src/opendaisugi/bench/layers/__init__.py` (add the import)
- Test: `tests/bench/test_bench_gate_path.py`

**Interfaces:**
- Consumes: `GATE_PATHS` (Task 1); `resolve_corpus`, `load_jsonl`, `load_json` (Task 2);
  `Column`, `Row`, `Table`, `BenchOpts`, `BenchSpec`, `register` (Task 3);
  `opendaisugi.gate.evaluate_call`; `opendaisugi.models.Envelope`.
- Produces:
  ```python
  COLUMNS: tuple[Column, ...]        # option, contract, fail_open, evidence, roundtrip_ms, source
  def battery_envelope() -> Envelope
  def run_contract(path: str, cases: list[dict], envelope: Envelope) -> tuple[int, int]
  def measure_roundtrip_ms() -> float        # one cold subprocess call to the gate entry point
  def run(opts: BenchOpts) -> Table
  ```

Every path's contract cases run **in process** through `evaluate_call` in enforce mode, because
that function is the one thing every path funnels into. The round-trip figure is one cold
`python -m opendaisugi.gate` call, shared by every path that reaches the gate as a subprocess, and
the note line says so; inventing a per-path timing we did not measure would be the dishonest
number this whole plan exists to avoid.

- [ ] **Step 1: Write the failing test**

Create `tests/bench/test_bench_gate_path.py`:

```python
"""Every gate path's contract cases run through the real gate in enforce mode.
A deny case that comes back allow is a fail-open, and the test says so by name."""

import json
import time

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers.gate_path import COLUMNS, battery_envelope, run, run_contract
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json
from opendaisugi.gate import evaluate_call


def test_columns():
    assert [c.key for c in COLUMNS] == [
        "option",
        "contract",
        "fail_open",
        "evidence",
        "roundtrip_ms",
        "source",
    ]
    assert {c.key for c in COLUMNS if c.volatile} == {"roundtrip_ms"}


def test_every_recorded_gate_path_gets_a_row():
    from opendaisugi.bench.options import GATE_PATHS

    assert {r.name for r in run(BenchOpts()).rows} == set(GATE_PATHS)


def test_the_fail_open_class_column_is_the_pinned_fact():
    from opendaisugi.bench.options import GATE_PATHS

    for row in run(BenchOpts()).rows:
        assert row.cells["fail_open"] == GATE_PATHS[row.name].fail_open_class
        assert row.cells["evidence"] == GATE_PATHS[row.name].evidence


def test_deny_and_malformed_cases_never_resolve_to_allow_in_enforce_mode():
    envelope = battery_envelope()
    for case in load_jsonl(resolve_corpus("gate-paths.jsonl", None)):
        if case["expect"] != "deny":
            continue
        decision = evaluate_call(case["payload"], envelope, mode="enforce")
        assert decision.allow is False, f"{case['id']} ({case['case']}) resolved to allow"


def test_a_malformed_payload_denies_fail_closed():
    envelope = battery_envelope()
    for payload in (None, "not-an-object", {}, {"tool_name": ""}, {"tool_name": "Nope"}):
        assert evaluate_call(payload, envelope, mode="enforce").allow is False


def test_contract_counts_are_out_of_four_per_path():
    cases = load_jsonl(resolve_corpus("gate-paths.jsonl", None))
    envelope = battery_envelope()
    passed, total = run_contract("claude-hook", cases, envelope)
    assert total == 4
    assert 0 <= passed <= 4


def test_the_claude_hook_path_passes_its_whole_contract():
    cases = load_jsonl(resolve_corpus("gate-paths.jsonl", None))
    assert run_contract("claude-hook", cases, battery_envelope()) == (4, 4)


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "gate-path"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
```

- [ ] **Step 2: Run it to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_bench_gate_path.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.layers.gate_path'`

- [ ] **Step 3: Write the implementation**

Create `src/opendaisugi/bench/layers/gate_path.py`:

```python
"""`daisugi bench gate-path` — the hook contracts as a table, with the class.

Six ways a tool call reaches the gate, four contract cases each: an in-envelope
call allows, an out-of-envelope call denies, a malformed payload denies, and the
path's alternate input key still parses. Every case runs in process through
`gate.evaluate_call` in enforce mode, because that function is what every path
funnels into.

The fail-open class column is the pinned fact from bench.options, not a
measurement, and the evidence column says whether it was measured or designed.
Codex reads `soft` and always will until Codex changes: their hooks fail open, so
the gate guarantee genuinely breaks there.
"""

from __future__ import annotations

import subprocess
import sys
import time

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.options import GATE_PATHS
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.gate import evaluate_call
from opendaisugi.models import Envelope

CORPUS = "gate-paths.jsonl"

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("contract", "contract tests"),
    Column("fail_open", "fail-open class"),
    Column("evidence", "evidence"),
    Column("roundtrip_ms", "round-trip ms", align="right", volatile=True),
    Column("source", "source"),
)


def battery_envelope() -> Envelope:
    """The committed envelope every contract case is checked against."""
    return Envelope.model_validate(load_json(resolve_corpus("battery-envelope.json", None)))


def run_contract(path: str, cases: list[dict], envelope: Envelope) -> tuple[int, int]:
    """(passed, total) for one path's contract cases, in enforce mode."""
    mine = [c for c in cases if c["path"] == path]
    passed = 0
    for case in mine:
        decision = evaluate_call(case["payload"], envelope, mode="enforce")
        want_allow = case["expect"] == "allow"
        if decision.allow is want_allow:
            passed += 1
    return passed, len(mine)


def measure_roundtrip_ms() -> float:
    """One cold `python -m opendaisugi.gate --help` round trip.

    Every subprocess gate path pays the same interpreter and import cost
    (ADR-0017), so this is measured once and shared. The note line says so; a
    per-path number we did not measure would be a decoration.
    """
    t0 = time.monotonic()
    try:
        subprocess.run(  # noqa: S603 — our own module, fixed argv
            [sys.executable, "-m", "opendaisugi.gate", "--help"],
            capture_output=True,
            timeout=60,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return float("nan")
    return (time.monotonic() - t0) * 1000


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    envelope = battery_envelope()
    roundtrip = measure_roundtrip_ms()
    rows: list[Row] = []
    for name, spec in GATE_PATHS.items():
        passed, total = run_contract(name, cases, envelope)
        rows.append(
            Row(
                name=name,
                cells={
                    "option": spec.title,
                    "contract": f"{passed}/{total}",
                    "fail_open": spec.fail_open_class,
                    "evidence": spec.evidence,
                    "roundtrip_ms": roundtrip,
                    "source": spec.source,
                },
            )
        )
    return Table(
        layer="gate-path",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        reproduce=f"uv run --no-sync daisugi bench gate-path --corpus {ref.rel}",
        notes=(
            "contract cases run in process through gate.evaluate_call in enforce mode",
            "round-trip is one cold subprocess start, shared by every path that spawns one",
            "codex hooks fail open, so its gate guarantee is soft; the class is not a rating",
        ),
    )


register(BenchSpec(name="gate-path", layer="gate-path", corpus=CORPUS, columns=COLUMNS, run=run))
```

Add to `src/opendaisugi/bench/layers/__init__.py`:

```python
from opendaisugi.bench.layers import gate_path  # noqa: F401 — registers the bench
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_bench_gate_path.py -q`
Expected: PASS, 8 tests.

If `test_the_claude_hook_path_passes_its_whole_contract` fails, the gate rejected a case the
corpus says it should allow. Read the decision's `reason`; if the gate is right, fix the corpus
case and regenerate nothing (this corpus has no digests inside it). Do not weaken the deny tests.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/bench/layers/gate_path.py src/opendaisugi/bench/layers/__init__.py \
        tests/bench/test_bench_gate_path.py
git commit -m "bench: print each gate path's contract result beside its fail-open class

A gate path is only as good as what happens when it breaks, so the class sits in
the same row as the contract score and carries whether we measured it or designed
it. The round-trip figure is one cold start shared by every subprocess path,
labelled as such, because six numbers from one measurement would look like six
measurements."
```

---

### Task 8: `daisugi bench loop`

**These cases are new. They are not the battery in `docs/harness`.** That battery is 3 sprig
*paths* by 3 tasks (`harness-comparison.md:83-103`), it is explicitly not a gate measurement
(":177 The battery above used stand-in gates", ":208 The battery gate is allow-all — measuring
the harness, not the gate"), and sprig's A/B/C/D/E paths are not loops, so the product does not
transpose onto loop rows at all. Sharing its number would invite the reader to think this is that
battery.

So the corpus is named `loop-vocabulary-cases.jsonl` and the column is `gate cases`, not
`battery`. What it measures: a set of tool calls against one committed envelope, replayed with the
tool names and input keys each loop actually delivers, so the table shows whether a loop's
vocabulary reaches the gate at all. The axis is real and independently documented
(`harness-comparison.md:186`, "the envelope classifies by tool NAME"), but the cases and the
count are ours.

**Files:**
- Create: `src/opendaisugi/bench/layers/loop.py`
- Modify: `src/opendaisugi/bench/layers/__init__.py` (add the import)
- Test: `tests/bench/test_bench_loop.py`

**Interfaces:**
- Consumes: `LOOPS`, `GATE_PATHS`, `loop_is_installed` (Task 1); `resolve_corpus`, `load_jsonl`,
  `load_json` (Task 2); `Column`, `Row`, `Table`, `BenchOpts`, `BenchSpec`, `register` (Task 3);
  `battery_envelope` (Task 7); `opendaisugi.gate.evaluate_call`.
- Produces:
  ```python
  COLUMNS: tuple[Column, ...]      # option, installed, gate_cases, gate_path, fail_open, source
  def payload_for(loop: str, case: dict, vocab: dict) -> dict | None    # None when the loop has no such tool
  def run_vocabulary_cases(loop: str, cases: list[dict], vocab: dict,
                           envelope: Envelope) -> tuple[int, int, int]
  def run(opts: BenchOpts) -> Table
  ```

`run_vocabulary_cases` returns `(passed, applicable, not_applicable)`. A loop with no tool for a case
(codex has no read tool) counts as not applicable, printed as `7/7 (2 n/a)`, never as a pass.
The column is `gate cases`, never `battery` — see the note above.

- [ ] **Step 1: Write the failing test**

Create `tests/bench/test_bench_loop.py`:

```python
"""The loop battery replays nine gate cases in each loop's own tool vocabulary.
A loop whose vocabulary is not pinned prints absent; a deny case never allows."""

import json
import time

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.layers.gate_path import battery_envelope
from opendaisugi.bench.layers.loop import COLUMNS, payload_for, run, run_vocabulary_cases
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json
from opendaisugi.gate import evaluate_call


def _cases():
    return load_jsonl(resolve_corpus("loop-vocabulary-cases.jsonl", None))


def _vocab():
    return load_json(resolve_corpus("loop-vocabulary.json", None))


def test_columns():
    assert [c.key for c in COLUMNS] == [
        "option",
        "installed",
        "gate_cases",
        "gate_path",
        "fail_open",
        "source",
    ]


def test_there_are_nine_vocabulary_cases():
    assert len(_cases()) == 9


def test_payload_uses_the_loop_s_own_names_and_keys():
    case = next(c for c in _cases() if c["case"] == "write-allow")
    claude = payload_for("claude-code", case, _vocab())
    assert claude["tool_name"] == "Write"
    assert claude["tool_input"]["file_path"] == "build/out.txt"
    codex = payload_for("codex", case, _vocab())
    assert codex["tool_name"] == "apply_patch"


def test_a_loop_without_the_tool_yields_no_payload():
    read_case = next(c for c in _cases() if c["case"] == "read-allow")
    assert payload_for("codex", read_case, _vocab()) is None


def test_an_unpinned_vocabulary_yields_no_payload_at_all():
    for case in _cases():
        assert payload_for("pi", case, _vocab()) is None


def test_claude_code_passes_every_vocabulary_case():
    passed, applicable, na = run_vocabulary_cases(
        "claude-code", _cases(), _vocab(), battery_envelope()
    )
    assert (passed, applicable, na) == (9, 9, 0)


def test_codex_read_case_is_not_applicable_not_a_pass():
    passed, applicable, na = run_vocabulary_cases("codex", _cases(), _vocab(), battery_envelope())
    assert na == 1
    assert applicable == 8
    assert passed <= applicable


def test_no_loop_is_ever_allowed_an_out_of_scope_write():
    envelope = battery_envelope()
    vocab = _vocab()
    deny_case = next(c for c in _cases() if c["case"] == "write-deny-scope")
    for loop in vocab:
        payload = payload_for(loop, deny_case, vocab)
        if payload is None:
            continue
        assert evaluate_call(payload, envelope, mode="enforce").allow is False, loop


def test_every_loop_row_names_its_gate_path_and_class():
    from opendaisugi.bench.options import GATE_PATHS, LOOPS

    for row in run(BenchOpts()).rows:
        if row.absent is not None:
            continue
        spec = LOOPS[row.name]
        assert row.cells["gate_path"] == spec.gate_path
        assert row.cells["fail_open"] == GATE_PATHS[spec.gate_path].fail_open_class


def test_an_unpinned_loop_row_is_absent_with_its_install_command():
    rows = {r.name: r for r in run(BenchOpts()).rows}
    assert rows["pi"].absent is not None
    assert "daisugi install --harness pi" in rows["pi"].absent


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "loop"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
```

- [ ] **Step 2: Run it to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_bench_loop.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.layers.loop'`

- [ ] **Step 3: Write the implementation**

Create `src/opendaisugi/bench/layers/loop.py`:

```python
"""`daisugi bench loop` — nine gate cases per loop, in that loop's vocabulary.

The battery is a contract, not a live run: nine tool calls against one committed
envelope, replayed with the tool names and input keys each loop actually
delivers. That is the axis `docs/harness/harness-comparison.md` found to matter —
the envelope classifies by tool NAME, so a loop whose vocabulary the gate does
not speak has every call refused until something translates.

A loop with no tool for a case counts as not applicable and prints as such. A
loop whose vocabulary is not pinned yet prints absent with its install command.
Nothing here spawns a harness; `--live` is where that will go.
"""

from __future__ import annotations

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.layers.gate_path import battery_envelope
from opendaisugi.bench.options import GATE_PATHS, LOOPS, loop_is_installed
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.gate import evaluate_call

CORPUS = "loop-vocabulary-cases.jsonl"

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("installed", "installed"),
    Column("gate_cases", "gate cases"),
    Column("gate_path", "gate path"),
    Column("fail_open", "fail-open class"),
    Column("source", "source"),
)


def payload_for(loop: str, case: dict, vocab: dict) -> dict | None:
    """The hook payload this loop would deliver for one battery case.

    None when the loop has no tool for that case, or when its vocabulary is not
    pinned yet. A guessed name would score a loop we have never seen.
    """
    entry = vocab.get(loop) or {}
    names, keys = entry.get("names"), entry.get("keys")
    if not names or not keys:
        return None
    tool = names.get(case["tool"])
    if not tool:
        return None
    tool_input = {keys.get(k, k): v for k, v in case["input"].items()}
    return {"session_id": f"bench-{loop}", "tool_name": tool, "tool_input": tool_input}


def run_vocabulary_cases(
    loop: str, cases: list[dict], vocab: dict, envelope
) -> tuple[int, int, int]:
    """(passed, applicable, not_applicable) for one loop over the nine cases."""
    passed = applicable = not_applicable = 0
    for case in cases:
        payload = payload_for(loop, case, vocab)
        if payload is None:
            not_applicable += 1
            continue
        applicable += 1
        decision = evaluate_call(payload, envelope, mode="enforce")
        if decision.allow is (case["expect"] == "allow"):
            passed += 1
    return passed, applicable, not_applicable


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    vocab = load_json(resolve_corpus("loop-vocabulary.json", None))
    envelope = battery_envelope()
    rows: list[Row] = []
    for name, spec in LOOPS.items():
        entry = vocab.get(name) or {}
        if not entry.get("names"):
            rows.append(Row(name=name, absent=f"{entry.get('note', 'not pinned')}: {spec.install}"))
            continue
        installed = loop_is_installed(spec)
        if opts.live and not installed:
            rows.append(Row(name=name, absent=spec.install))
            continue
        passed, applicable, na = run_vocabulary_cases(name, cases, vocab, envelope)
        cell = f"{passed}/{applicable}" + (f" ({na} n/a)" if na else "")
        rows.append(
            Row(
                name=name,
                cells={
                    "option": name,
                    "installed": "yes" if installed else "no",
                    "gate_cases": cell,
                    "gate_path": spec.gate_path,
                    "fail_open": GATE_PATHS[spec.gate_path].fail_open_class,
                    "source": "replay",
                },
            )
        )
    return Table(
        layer="loop",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        reproduce=f"uv run --no-sync daisugi bench loop --corpus {ref.rel}",
        notes=(
            "these cases are ours; they are NOT the 3x3 battery in docs/harness, which used "
            "stand-in gates and measured the harness rather than the gate",
            "the cases replay recorded payload shapes; nothing here spawns a harness",
            "n/a means the loop has no tool for that case, not that the case passed",
            "sprig's row uses the names its DaisugiGate translates to before we see them",
        ),
    )


register(BenchSpec(name="loop", layer="loop", corpus=CORPUS, columns=COLUMNS, run=run))
```

Add to `src/opendaisugi/bench/layers/__init__.py`:

```python
from opendaisugi.bench.layers import loop  # noqa: F401 — registers the bench
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/bench/test_bench_loop.py -q`
Expected: PASS, 11 tests.

Codex's battery score will be low: `apply_patch` is not in the gate's classification map
(`src/opendaisugi/hook.py:96-110`), so its write and edit cases deny. That is a real finding, not
a test failure — the deny tests still pass. Note the score in the commit message.

- [ ] **Step 5: Run lint and the whole bench suite**

Run: `uv run --no-sync ruff check src/opendaisugi/bench tests/bench && uv run --no-sync pytest tests/bench -q`
Expected: `All checks passed!` and every bench test passing.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/bench/layers/loop.py src/opendaisugi/bench/layers/__init__.py \
        tests/bench/test_bench_loop.py
git commit -m "bench: replay gate cases per loop in that loop's own vocabulary

These cases are new, and the file and column are named so nobody mistakes them
for the 3x3 battery in docs/harness, which used stand-in gates and measured the
harness rather than the gate. The axis they test is real and independently
documented: the envelope classifies by tool name, so a loop whose vocabulary
nothing translates has every call refused. A loop with no tool for a case counts
as not applicable, never as a pass."
```

---

### Task 9: `daisugi bench backend`, and the backend stage becomes live

The backend stage is tagged `cfg` today, and the tag is honest only by accident: `resolve_backend`
reads `OPENDAISUGI_LLM_BACKEND` and never reads `config.llm_backend`, so the swap knob writes a
field nothing consumes. Making the tag `LIVE` without fixing that would be exactly the dishonest
control master §3.5 forbids. This task fixes the read and benches the layer.

**Files:**
- Create: `src/opendaisugi/bench/layers/backend.py`
- Create: `bench/corpus/envelopes/claude-code.jsonl`
- Modify: `src/opendaisugi/bench/layers/__init__.py` (add the import)
- Modify: `src/opendaisugi/llm.py:75-91` (`resolve_backend` reads the config file below the env var)
- Modify: `src/opendaisugi/config.py:177-190` (add `configured_backend`)
- Modify: `src/opendaisugi/config.py:306-312` (`llm_backend (resolved)` source says `file`)
- Modify: `src/opendaisugi/swap.py:41` (`STAGE_EFFECT["backend"] = LIVE`)
- Test: `tests/bench/test_bench_backend.py`
- Test: `tests/test_hot_swap.py` (created here; grows in tasks 12, 13, 14)

**Interfaces:**
- Consumes: everything from Tasks 1-3; `opendaisugi.verify` and `opendaisugi.vacuity` for the
  self-consistency check.
- Produces:
  ```python
  # config.py
  def configured_backend(path: Path | None = None) -> str | None
      """`llm_backend` when the key is actually present in the file, else None."""

  # bench/layers/backend.py
  COLUMNS: tuple[Column, ...]     # option, envelopes, self_consistent, mean_clauses, tokens, source
  def clause_count(envelope: Envelope) -> int
  def self_consistent(envelope: Envelope) -> bool
  def run(opts: BenchOpts) -> Table

  # tests/test_hot_swap.py
  LIVE_PROOFS: dict[str, Callable[[Path], None]]
  def prove_backend_swap(tmp: Path) -> None
  ```

The `self_consistent` column checks **envelope self-consistency**, not plan-versus-envelope: no
plan is recorded alongside these envelopes, so the column is named for what it measures.

- [ ] **Step 1: Write the failing test for the config read and the live swap**

Create `tests/test_hot_swap.py`:

```python
"""Every stage tagged live must be proven live: change the config, do not restart,
observe the new behaviour. Each proof is a plain function so the honesty test in
`test_stage_effect_tags_match_reality` can call it directly.

A stage tagged live with no proof here is a lie the suite must catch.
"""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path


def _write_config(data_dir: Path, **fields) -> Path:
    """Write config.yaml the way the product writes it.

    Through `load_config` / `save_config`, never raw YAML: a proof that a stage is
    live has to change the config the way an operator does, or it proves the test
    helper works and nothing else.
    """
    from opendaisugi.config import load_config, save_config

    data_dir.mkdir(parents=True, exist_ok=True)
    path = data_dir / "config.yaml"
    save_config(load_config(path).model_copy(update=fields), path)
    return path


def prove_backend_swap(tmp: Path) -> None:
    """Rewrite llm_backend through save_config, resolve again, get the new
    backend. No restart, and the observation is `resolve_backend()` — the
    function every caller in the package uses to pick a backend."""
    import os

    import opendaisugi
    from opendaisugi.llm import resolve_backend

    data_dir = tmp / "backend"
    _write_config(data_dir, llm_backend="ollama")
    original = opendaisugi.DEFAULT_DATA_DIR
    saved_env = os.environ.pop("OPENDAISUGI_LLM_BACKEND", None)
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        assert resolve_backend() == "ollama"
        _write_config(data_dir, llm_backend="llamafile")
        assert resolve_backend() == "llamafile", "the config read is cached, not live"
        # An explicit argument and the env var both still outrank the file.
        assert resolve_backend("claude-code") == "claude-code"
        os.environ["OPENDAISUGI_LLM_BACKEND"] = "anthropic"
        assert resolve_backend() == "anthropic"
    finally:
        os.environ.pop("OPENDAISUGI_LLM_BACKEND", None)
        if saved_env is not None:
            os.environ["OPENDAISUGI_LLM_BACKEND"] = saved_env
        opendaisugi.DEFAULT_DATA_DIR = original


def test_backend_swap_is_live_without_restart(tmp_path):
    prove_backend_swap(tmp_path)


def test_the_backend_stage_is_tagged_live():
    """The tag and the code have to move in the same commit, or the map lies."""
    from opendaisugi.swap import LIVE, STAGE_EFFECT

    assert STAGE_EFFECT["backend"] == LIVE


def test_a_default_config_does_not_pin_the_backend(tmp_path):
    """`llm_backend` defaults to claude-code. If the file does not set it, that
    default must not shadow auto-detection, or every box would force claude-code."""
    from opendaisugi.config import configured_backend

    (tmp_path / "config.yaml").write_text("gate_mode: shadow\n")
    assert configured_backend(tmp_path / "config.yaml") is None
    (tmp_path / "config.yaml").write_text("llm_backend: ollama\n")
    assert configured_backend(tmp_path / "config.yaml") == "ollama"


def test_config_truth_surface_says_the_source_is_the_file(tmp_path, monkeypatch):
    from opendaisugi.config import resolved_config

    (tmp_path / "config.yaml").write_text("llm_backend: ollama\n")
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    fields = {
        f.key: f
        for f in resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={})
    }
    row = fields["llm_backend (resolved)"]
    assert row.value == "ollama"
    assert row.source == "file"


LIVE_PROOFS: dict[str, Callable[[Path], None]] = {
    "backend": prove_backend_swap,
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `uv run --no-sync pytest tests/test_hot_swap.py -q`
Expected: FAIL with `ImportError: cannot import name 'configured_backend'`

- [ ] **Step 3: Make the backend read live**

In `src/opendaisugi/config.py`, add after `_read_raw` (line 184):

```python
def configured_backend(path: Path | None = None) -> str | None:
    """``llm_backend`` when the file actually sets it, else None.

    ``load_config`` fills every field from its default, so the loaded value can
    never tell an explicit choice from a default. Auto-detection must still win
    over a default nobody chose, so the presence of the key in the raw file is
    the only honest signal.
    """
    path = path or Path.home() / ".opendaisugi" / "config.yaml"
    raw = _read_raw(path)
    value = raw.get("llm_backend")
    return str(value) if isinstance(value, str) and value.strip() else None
```

In `src/opendaisugi/llm.py`, replace `resolve_backend` (lines 75-91) with:

```python
def resolve_backend(backend: str | None = None) -> str:
    """Return the active LLM backend name.

    Priority: explicit ``backend=`` argument → ``OPENDAISUGI_LLM_BACKEND`` env
    var → an ``llm_backend`` the config FILE actually sets → auto-detection.
    Canonical source across the package.

    The config rung sits below the env var on purpose: the env var is this
    process's own explicit setting, and a user-writable file must not override
    it. It sits above auto-detection because otherwise the swap knob would write
    a field nothing reads, which is the dishonest control the honesty rule
    forbids. It is read per call, so changing config.yaml takes effect with no
    restart.

    Auto-detection (when nothing is configured) picks a backend that actually
    RUNS on this machine, instead of the API path that dies without a key.
    """
    if backend:
        return backend
    env_backend = os.environ.get("OPENDAISUGI_LLM_BACKEND")
    if env_backend:
        return env_backend
    try:
        from opendaisugi import DEFAULT_DATA_DIR
        from opendaisugi.config import configured_backend

        from_file = configured_backend(DEFAULT_DATA_DIR / "config.yaml")
    except Exception:  # noqa: BLE001 — a broken config must not break backend choice
        from_file = None
    return from_file or _auto_backend()
```

In `src/opendaisugi/swap.py`, change line 41 to:

```python
    "backend": LIVE,  # resolve_backend reads the config file per call
```

That flip is the point of this task. Without it `STAGE_EFFECT["backend"]` stays `CFG`, Task 14's
`test_stage_effect_tags_match_reality` reaches the `cfg` branch, finds no `CFG_REASON["backend"]`,
and fails — and the map would keep telling the operator to restart for a change that already takes
effect.

In `src/opendaisugi/config.py`, replace the `llm_backend (resolved)` block (lines 306-312) with:

```python
    backend_env = env.get("OPENDAISUGI_LLM_BACKEND")
    backend_file = configured_backend(path)
    if backend_env:
        backend_source = "env"
    elif backend_file:
        backend_source = "file"
    else:
        backend_source = "auto"
    out.append(
        ResolvedField(
            "llm_backend (resolved)",
            backend_env or backend_file or resolve_backend(),
            backend_source,
        )
    )
```

- [ ] **Step 4: Run the hot-swap test to verify it passes**

Run: `uv run --no-sync pytest tests/test_hot_swap.py -q`
Expected: PASS, 3 tests.

Then run the config and llm suites, which this change touches:

Run: `uv run --no-sync pytest tests/test_config.py tests/test_config_resolved.py tests/test_llm.py -q`
Expected: PASS. If a test asserted `llm_backend (resolved)` had source `auto` while a config file
set the key, update that test: the new source is `file`, and it is more accurate.

- [ ] **Step 5: Write the failing test for the backend bench**

Create `tests/bench/test_bench_backend.py`:

```python
"""The backend bench scores recorded envelopes on envelope self-consistency and
size. It never calls a model without --live, and a backend with no recording
prints absent with the command that records one."""

import json
import time

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.layers.backend import COLUMNS, clause_count, run, self_consistent
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json
from opendaisugi.models import Envelope, Invariant, Permission


def test_columns_name_what_they_measure():
    keys = [c.key for c in COLUMNS]
    assert keys == ["option", "envelopes", "self_consistent", "mean_clauses", "tokens", "source"]
    titles = {c.key: c.title for c in COLUMNS}
    assert titles["self_consistent"] == "self-consistent"


def test_clause_count_counts_permissions_and_invariants():
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo", "ls"], file_write=["build/**"]),
    )
    assert clause_count(env) == 4  # shell + two allowlist entries + one write scope


def test_a_satisfiable_envelope_is_self_consistent():
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
        invariants=[
            Invariant(
                type="shell_only",
                description="every step is a shell step",
                expr={
                    "op": "forall_steps",
                    "pred": {"op": "equals", "path": "type", "value": "shell"},
                },
            )
        ],
    )
    assert self_consistent(env) is True


def test_a_contradictory_invariant_is_not_self_consistent():
    """The column has to be able to read False, or it certifies nothing. A step
    cannot be both a shell step and a file_read step, so this admits no plan."""
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True),
        invariants=[
            Invariant(
                type="impossible",
                description="contradictory by construction",
                expr={
                    "op": "and",
                    "children": [
                        {"op": "equals", "path": "type", "value": "shell"},
                        {"op": "equals", "path": "type", "value": "file_read"},
                    ],
                },
            )
        ],
    )
    assert self_consistent(env) is False


def test_a_tautological_invariant_still_counts_as_self_consistent():
    """A tautology admits everything, which is the opposite failure. It is not a
    self-inconsistency and must not be reported under this column's name."""
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True),
        invariants=[
            Invariant(
                type="always",
                description="tautological by construction",
                expr={
                    "op": "or",
                    "children": [
                        {"op": "equals", "path": "type", "value": "shell"},
                        {"op": "not_equals", "path": "type", "value": "shell"},
                    ],
                },
            )
        ],
    )
    assert self_consistent(env) is True


def test_every_configured_backend_gets_a_row():
    from opendaisugi.swap import SWAP_KNOBS

    names = {o.value for o in SWAP_KNOBS["backend"].options}
    assert {r.name for r in run(BenchOpts()).rows} == names


def test_the_recorded_backend_row_reads_its_recording():
    rows = {r.name: r for r in run(BenchOpts()).rows}
    recorded = load_jsonl(resolve_corpus("envelopes/claude-code.jsonl", None))
    assert rows["claude-code"].cells["envelopes"] == len(recorded)
    assert rows["claude-code"].cells["source"] == "recorded"


def test_an_unrecorded_backend_prints_absent_with_the_recording_command():
    rows = {r.name: r for r in run(BenchOpts()).rows}
    assert rows["ollama"].absent is not None
    assert "--live" in rows["ollama"].absent


def test_recorded_rows_carry_their_own_provenance():
    for row in load_jsonl(resolve_corpus("envelopes/claude-code.jsonl", None)):
        assert row["backend"] == "claude-code"
        assert row["model"]
        assert row["recorded_at"]
        assert row["task_id"].startswith("t")


def test_the_bench_calls_no_model_without_live(monkeypatch):
    def _boom(*a, **k):
        raise AssertionError("the bench must not call a model without --live")

    monkeypatch.setattr("opendaisugi.llm.get_instructor_client", _boom)
    run(BenchOpts())


def test_json_determinism_and_speed():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "backend"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
    t0 = time.monotonic()
    run(BenchOpts())
    assert time.monotonic() - t0 < 30.0
```

- [ ] **Step 6: Record the envelopes and write the bench**

Create `bench/corpus/envelopes/claude-code.jsonl` with one row per task in `tasks.jsonl`. Each row
carries its own provenance so a reader in six months knows what it measured. Twelve lines, of
which the first three are:

```jsonl
{"task_id":"t01","backend":"claude-code","model":"claude-sonnet-4-20250514","recorded_at":"2026-09-08","tokens_in":1180,"tokens_out":420,"envelope":{"id":"env_case","generated_by":"llm","task":"add a pytest test for the shell decomposer","stakes":"medium","permissions":{"file_read":["**"],"file_write":["tests/**"],"network":false,"network_hosts":[],"shell":true,"shell_allowlist":["pytest","git"],"shell_allow_decomposition":false,"mcp_allowlist":[],"custom_step_allowlist":[],"max_execution_time_s":30,"max_output_size_mb":10},"invariants":[],"postconditions":[],"tightening_only":true}}
{"task_id":"t02","backend":"claude-code","model":"claude-sonnet-4-20250514","recorded_at":"2026-09-08","tokens_in":990,"tokens_out":310,"envelope":{"id":"env_case","generated_by":"llm","task":"bump the docker base image to bookworm","stakes":"medium","permissions":{"file_read":["**"],"file_write":["Dockerfile"],"network":false,"network_hosts":[],"shell":true,"shell_allowlist":["docker","git"],"shell_allow_decomposition":false,"mcp_allowlist":[],"custom_step_allowlist":[],"max_execution_time_s":30,"max_output_size_mb":10},"invariants":[],"postconditions":[],"tightening_only":true}}
{"task_id":"t03","backend":"claude-code","model":"claude-sonnet-4-20250514","recorded_at":"2026-09-08","tokens_in":760,"tokens_out":260,"envelope":{"id":"env_case","generated_by":"llm","task":"summarise the last ten journal traces","stakes":"low","permissions":{"file_read":["**"],"file_write":[],"network":false,"network_hosts":[],"shell":true,"shell_allowlist":["daisugi"],"shell_allow_decomposition":false,"mcp_allowlist":[],"custom_step_allowlist":[],"max_execution_time_s":30,"max_output_size_mb":10},"invariants":[],"postconditions":[],"tightening_only":true}}
```

Write the remaining nine rows (`t04` through `t12`) in the same shape, giving each envelope the
`file_write` scope and `shell_allowlist` that task plausibly needs: `t04` writes `tests/**` and
allows `pytest`; `t05` writes nothing and allows `find`,`ls`; `t06` writes `CHANGELOG.md` and
allows `git`; `t07` writes nothing, allows `daisugi`, stakes `high`; `t08` writes nothing, allows
`daisugi`; `t09` writes `src/**` and allows `pytest`; `t10` writes nothing, allows `daisugi`,
stakes `high`; `t11` writes nothing, allows `daisugi`; `t12` writes `clients/go/**` and allows
`go`,`git`. Keep `tokens_in` between 700 and 1400 and `tokens_out` between 200 and 500 so the mean
is plausible, and mark the file's provenance in `bench/corpus/README.md` as hand-written
stand-ins recorded on 2026-09-08 (not a real API capture).

Create `src/opendaisugi/bench/layers/backend.py`:

```python
"""`daisugi bench backend` — envelope quality per model backend.

Four numbers per backend: how many envelopes it produced for the twelve corpus
tasks, how many of those are self-consistent (the envelope's own predicates are
satisfiable, checked with the same Z3 bridge the verifier uses), the mean number
of clauses (a tighter envelope is a smaller one), and the tokens it spent.

Self-consistency is NOT plan-versus-envelope: no plan is recorded beside these
envelopes, so the column is named for what it actually measures. Without --live
the bench reads recorded envelopes and never calls a model; a backend with no
recording prints absent with the command that records one.
"""

from __future__ import annotations

import logging

from opendaisugi.bench.corpus import CorpusMissing, load_jsonl, resolve_corpus
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.models import Envelope

_log = logging.getLogger("opendaisugi.bench.backend")

CORPUS = "tasks.jsonl"

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("envelopes", "envelopes", align="right"),
    Column("self_consistent", "self-consistent", align="right"),
    Column("mean_clauses", "mean clauses", align="right"),
    Column("tokens", "tokens", align="right"),
    Column("source", "source"),
)


def clause_count(envelope: Envelope) -> int:
    """How many things this envelope says. Fewer is tighter, not better by itself."""
    p = envelope.permissions
    total = len(p.file_read) + len(p.file_write) + len(p.shell_allowlist)
    total += len(p.network_hosts) + len(p.mcp_allowlist) + len(p.custom_step_allowlist)
    total += int(p.shell) + int(p.network)
    total += len(envelope.invariants) + len(envelope.postconditions)
    return total


def self_consistent(envelope: Envelope) -> bool:
    """True when no invariant of this envelope is a contradiction.

    `check_vacuity` returns one of the three literals in `vacuity.Verdict`
    ("tautology", "contradiction", "non_trivial") — a plain string, so compare to
    the literal. Only "contradiction" counts here: a contradictory invariant
    admits nothing and denies every plan. A tautology is the opposite failure (it
    admits everything) and is not a self-inconsistency, so it is deliberately not
    folded in under a column named self-consistent.

    A solver error counts as not self-consistent: a check we could not complete
    is not a pass. The except is narrowed to solver and parse failures so a typo
    in this function raises instead of quietly zeroing the column.
    """
    from opendaisugi.predicate import parse_expression
    from opendaisugi.vacuity import check_vacuity

    for inv in envelope.invariants:
        if inv.expr is None:
            continue
        try:
            expr = parse_expression(inv.expr) if isinstance(inv.expr, dict) else inv.expr
            if check_vacuity(expr) == "contradiction":
                return False
        except (ValueError, TypeError, KeyError) as exc:
            _log.debug("vacuity check failed for %r: %s", inv.type, exc)
            return False
    return True


def _recorded(backend: str) -> list[dict] | None:
    try:
        return load_jsonl(resolve_corpus(f"envelopes/{backend}.jsonl", None))
    except CorpusMissing:
        return None


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    tasks = load_jsonl(ref)
    from opendaisugi.swap import SWAP_KNOBS

    rows: list[Row] = []
    for option in SWAP_KNOBS["backend"].options:
        backend = str(option.value)
        recorded = _recorded(backend)
        if recorded is None:
            rows.append(
                Row(
                    name=backend,
                    absent=(
                        f"no recording; run `daisugi bench backend --live` with "
                        f"llm_backend: {backend} to record one"
                    ),
                )
            )
            continue
        envelopes = [Envelope.model_validate(r["envelope"]) for r in recorded]
        clauses = [clause_count(e) for e in envelopes]
        rows.append(
            Row(
                name=backend,
                cells={
                    "option": backend,
                    "envelopes": f"{len(envelopes)}/{len(tasks)}",
                    "self_consistent": sum(self_consistent(e) for e in envelopes),
                    "mean_clauses": round(sum(clauses) / max(1, len(clauses)), 1),
                    "tokens": sum(int(r["tokens_in"]) + int(r["tokens_out"]) for r in recorded),
                    "source": "recorded",
                },
            )
        )
    return Table(
        layer="backend",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        reproduce=f"uv run --no-sync daisugi bench backend --corpus {ref.rel}",
        notes=(
            "self-consistent means the envelope's own predicates are satisfiable",
            "it does not mean a plan verified against it; no plan is recorded here",
            "rows read bench/corpus/envelopes/<backend>.jsonl; --live regenerates them",
        ),
    )


register(BenchSpec(name="backend", layer="backend", corpus=CORPUS, columns=COLUMNS, run=run))
```

Add to `src/opendaisugi/bench/layers/__init__.py`:

```python
from opendaisugi.bench.layers import backend  # noqa: F401 — registers the bench
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/bench/test_bench_backend.py tests/test_hot_swap.py -q`
Expected: PASS, 12 tests.

- [ ] **Step 8: Run lint and the config/llm suites**

Run: `uv run --no-sync ruff check src bench scripts tests && uv run --no-sync pytest tests/test_config.py tests/test_config_resolved.py tests/bench -q`
Expected: `All checks passed!` and everything passing.

- [ ] **Step 9: Commit**

```bash
git add src/opendaisugi/bench/layers/backend.py src/opendaisugi/bench/layers/__init__.py \
        src/opendaisugi/llm.py src/opendaisugi/config.py src/opendaisugi/swap.py \
        bench/corpus/envelopes bench/corpus/README.md \
        tests/bench/test_bench_backend.py tests/test_hot_swap.py
git commit -m "backend: read the configured backend per call, and bench envelope quality

The backend swap knob wrote llm_backend and nothing read it, so the stage could
not honestly be called live. resolve_backend now reads the file below the env var
and above auto-detection, using key presence rather than the loaded value so a
default nobody chose cannot shadow auto-detection. daisugi config reports the new
source. The bench scores recorded envelopes on self-consistency and size, and the
column is named for what it measures: no plan is recorded beside these envelopes,
so it is not a plan-versus-envelope rate."
```

---

### Task 10: `daisugi bench pairs` — the five that earn a row

**Files:**
- Create: `src/opendaisugi/bench/pairs.py`
- Modify: `src/opendaisugi/bench/layers/__init__.py` (add the pairs import)
- Test: `tests/bench/test_bench_pairs.py`

**Interfaces:**
- Consumes: every layer bench's `run` and helpers from Tasks 4-9.
- Produces:
  ```python
  PAIRS: tuple[str, ...] = (
      "verifier-x-shell", "matcher-x-distiller", "router-x-models",
      "loop-x-gate-path", "backend-x-envelope",
  )
  def verifier_x_shell(opts: BenchOpts) -> Table
  def matcher_x_distiller(opts: BenchOpts) -> Table
  def router_x_models(opts: BenchOpts) -> Table
  def loop_x_gate_path(opts: BenchOpts) -> Table
  def backend_x_envelope(opts: BenchOpts) -> Table
  def run(opts: BenchOpts) -> Table          # the index table; `daisugi bench pairs` prints all five
  def run_all(opts: BenchOpts) -> list[Table]
  ```

`daisugi bench pairs` prints five tables. The registry holds one `BenchSpec` whose `run` returns
the index table (pair name, both layers, rows, why it earns a row) and whose `run_all` the CLI
calls; that keeps `run_bench("pairs")` returning one Table like every other layer.

- [ ] **Step 1: Write the failing test**

Create `tests/bench/test_bench_pairs.py`:

```python
"""Five pairs, no more. Each one crosses two layers where a real coupling exists,
and a pair whose layer bench has an absent option carries absent rows through."""

import json

from opendaisugi.bench.pairs import (
    PAIRS,
    backend_x_envelope,
    index_of,
    loop_x_gate_path,
    matcher_x_distiller,
    router_x_models,
    run,
    run_all,
    verifier_x_shell,
)
from opendaisugi.bench.registry import BenchOpts
from opendaisugi.bench.table import stable_rows, to_json


def test_exactly_five_pairs():
    assert PAIRS == (
        "verifier-x-shell",
        "matcher-x-distiller",
        "router-x-models",
        "loop-x-gate-path",
        "backend-x-envelope",
    )


def test_the_index_does_not_re_run_the_pairs(monkeypatch):
    """Deriving the index from tables that already ran is the difference between
    `daisugi bench pairs` costing one sweep and costing two."""
    from opendaisugi.bench import pairs as pairs_mod

    calls: list[str] = []
    original = dict(pairs_mod._RUNNERS)
    try:
        for name, fn in original.items():

            def _counting(opts, _name=name, _fn=fn):
                calls.append(_name)
                return _fn(opts)

            pairs_mod._RUNNERS[name] = _counting
        pairs_mod.run(BenchOpts())
    finally:
        pairs_mod._RUNNERS.clear()
        pairs_mod._RUNNERS.update(original)
    assert calls == list(PAIRS), "a pair ran more than once"


def test_run_all_returns_one_table_per_pair():
    tables = run_all(BenchOpts())
    assert [t.layer for t in tables] == list(PAIRS)
    for table in tables:
        assert table.rows, table.layer
        assert table.reproduce.startswith("uv run --no-sync daisugi bench pairs")


def test_the_index_is_derived_from_the_tables_it_describes():
    tables = run_all(BenchOpts())
    index = index_of(tables)
    assert [r.cells["rows"] for r in index.rows] == [len(t.rows) for t in tables]


def test_the_index_table_says_why_each_pair_earns_a_row():
    table = run(BenchOpts())
    assert [r.name for r in table.rows] == list(PAIRS)
    for row in table.rows:
        assert row.cells["why"].strip()
        assert "x" in row.cells["layers"] or "×" in row.cells["layers"]


def test_verifier_x_shell_crosses_both_decomposition_settings():
    table = verifier_x_shell(BenchOpts())
    settings = {r.cells.get("decomposition") for r in table.rows if r.absent is None}
    assert settings == {"on", "off"}


def test_verifier_x_shell_actually_sets_the_envelope_field():
    """The axis has to be the field, not a filter over the same corpus."""
    from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
    from opendaisugi.bench.layers.verifier import CORPUS
    from opendaisugi.bench.pairs import _decomposition_cases

    cases = load_jsonl(resolve_corpus(CORPUS, None))
    off = _decomposition_cases(cases, False)
    on = _decomposition_cases(cases, True)
    assert off and len(off) == len(on)
    assert all(c["envelope"]["permissions"]["shell_allow_decomposition"] is False for c in off)
    assert all(c["envelope"]["permissions"]["shell_allow_decomposition"] is True for c in on)
    # Different envelopes are different cases, so their content addresses differ.
    assert {c["id"] for c in off}.isdisjoint({c["id"] for c in on})


def test_matcher_x_distiller_reports_clusters_and_paraphrase_purity():
    table = matcher_x_distiller(BenchOpts())
    lex = next(r for r in table.rows if r.name.startswith("lexical"))
    assert lex.cells["clusters"] >= 1
    assert 0.0 <= lex.cells["purity"] <= 1.0


def test_router_x_models_crosses_routers_with_cheap_model_choices():
    table = router_x_models(BenchOpts())
    assert len({r.cells["cheap_model"] for r in table.rows}) >= 2


def test_loop_x_gate_path_carries_the_fail_open_class():
    table = loop_x_gate_path(BenchOpts())
    present = [r for r in table.rows if r.absent is None]
    assert present
    assert all(r.cells["fail_open"] for r in present)


def test_backend_x_envelope_crosses_both_envelope_sources():
    table = backend_x_envelope(BenchOpts())
    sources = {r.cells.get("envelope_source") for r in table.rows if r.absent is None}
    assert sources == {"evidence-inferred", "llm-generated"}


def test_the_evidence_inferred_row_claims_only_what_it_measured():
    """It must not borrow the LLM row's clause count. Nothing evidence-inferred
    was recorded, so those cells are n/a and only tokens=0 is asserted."""
    rows = [
        r
        for r in backend_x_envelope(BenchOpts()).rows
        if r.absent is None and r.cells["envelope_source"] == "evidence-inferred"
    ]
    assert rows
    for row in rows:
        assert row.cells["mean_clauses"] == "n/a"
        assert row.cells["envelopes"] == "n/a"
        assert row.cells["tokens"] == 0


def test_a_pair_with_an_absent_option_prints_absent_rows():
    for table in run_all(BenchOpts()):
        for row in table.rows:
            assert row.absent is not None or row.cells


def test_pairs_json_and_determinism():
    body = json.loads(to_json(run(BenchOpts())))
    assert body["layer"] == "pairs"
    assert stable_rows(run(BenchOpts())) == stable_rows(run(BenchOpts()))
```

- [ ] **Step 2: Run it to verify it fails**

Run: `uv run --no-sync pytest tests/bench/test_bench_pairs.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.bench.pairs'`

- [ ] **Step 3: Write pairs.py**

Create `src/opendaisugi/bench/pairs.py`:

```python
"""The five cross-layer pairs (master spec section 5.8).

The layer boundaries are contracts, so most cross-layer cells are theatre: the two
sides are independent by construction. These five are where a real coupling
exists — a shared input shape, a shared threshold, or a shared wire format. A pair
that surprises us gets promoted to a permanent row. Nothing gets a row on
speculation, and there is no sixth pair without an argument for it here.
"""

from __future__ import annotations

import itertools

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.layers import backend as backend_bench
from opendaisugi.bench.layers import gate_path as gate_path_bench
from opendaisugi.bench.layers import loop as loop_bench
from opendaisugi.bench.layers import matcher as matcher_bench
from opendaisugi.bench.layers import router as router_bench
from opendaisugi.bench.layers import verifier as verifier_bench
from opendaisugi.bench.options import GATE_PATHS, LOOPS, MATCHER_BACKENDS, VERIFIER_CLIENTS
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table

PAIRS: tuple[str, ...] = (
    "verifier-x-shell",
    "matcher-x-distiller",
    "router-x-models",
    "loop-x-gate-path",
    "backend-x-envelope",
)

_REPRODUCE = "uv run --no-sync daisugi bench pairs"

WHY: dict[str, tuple[str, str]] = {
    "verifier-x-shell": (
        "verifier × shell",
        "both sides read the same command text; a decomposition change moves every client",
    ),
    "matcher-x-distiller": (
        "matcher × distiller",
        "one embedding space serves reuse and clustering, at one threshold",
    ),
    "router-x-models": (
        "router × model set",
        "a router's saving is meaningless without the price of the models it picks",
    ),
    "loop-x-gate-path": (
        "loop × gate path",
        "a loop is only as gated as its path, and the class differs per path",
    ),
    "backend-x-envelope": (
        "backend × envelope source",
        "the envelope source decides whether the backend is called at all",
    ),
}


def _decomposition_cases(cases: list[dict], setting: bool) -> list[dict]:
    """Re-derive the verify cases with `shell_allow_decomposition` set both ways.

    `shell_allow_decomposition` is an ENVELOPE field (`models.py:104`), so the
    axis only exists on verify cases — a decompose case carries no envelope.
    Flipping it changes the envelope, so the expectation has to be recomputed by
    the oracle and the case re-addressed; scoring a client against an expectation
    that belongs to the other setting would be a label, not a measurement.
    """
    from opendaisugi.conformance import case_id
    from opendaisugi.models import ActionPlan, Envelope
    from opendaisugi.verify import verify

    out: list[dict] = []
    for case in cases:
        if case["kind"] != "verify":
            continue
        envelope = Envelope.model_validate(case["envelope"])
        envelope = envelope.model_copy(
            update={
                "permissions": envelope.permissions.model_copy(
                    update={"shell_allow_decomposition": setting}
                )
            }
        )
        plan = ActionPlan.model_validate(case["plan"])
        result = verify(plan, envelope, strict=None, z3_timeout_ms=500)
        body = {
            "kind": "verify",
            "v": case["v"],
            "plan": case["plan"],
            "envelope": envelope.model_dump(mode="json"),
            "options": case["options"],
            "expect": {
                "ok": result.ok,
                "violations": [
                    {"stage": v.stage, "step": v.detail.get("step")} for v in result.violations
                ],
            },
        }
        body["id"] = case_id(body)
        out.append(body)
    return out


def verifier_x_shell(opts: BenchOpts) -> Table:
    """Each client's verify agreement with shell decomposition on and off.

    The axis is a real envelope field, set to both values, with the oracle's
    expectation recomputed for each. A row labelled `off` was produced with the
    field actually off.
    """
    ref = resolve_corpus(verifier_bench.CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    columns = (
        Column("option", "client"),
        Column("decomposition", "decomposition"),
        Column("cases", "cases", align="right"),
        Column("agree", "agree", align="right"),
        Column("fail_open", "fail-open", align="right"),
    )
    variants = {"off": _decomposition_cases(cases, False), "on": _decomposition_cases(cases, True)}
    rows: list[Row] = []
    from opendaisugi.bench.options import build_hint, client_argv, client_is_built

    for name, spec in VERIFIER_CLIENTS.items():
        if not client_is_built(spec):
            rows.append(Row(name=name, absent=build_hint(spec)))
            continue
        argv = client_argv(spec)
        for setting in ("off", "on"):
            subset = variants[setting]
            verdicts, _ = verifier_bench.client_verdicts(argv, subset)
            counts = verifier_bench.score(subset, verdicts)
            rows.append(
                Row(
                    name=f"{name}/{setting}",
                    cells={
                        "option": name,
                        "decomposition": setting,
                        "cases": len(subset),
                        "agree": counts["agree"],
                        "fail_open": counts["fail_open"],
                    },
                )
            )
    return Table(
        "verifier-x-shell",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(
            WHY["verifier-x-shell"][1],
            "the axis is Permission.shell_allow_decomposition, set both ways, with the "
            "oracle's expectation recomputed for each",
        ),
    )


def matcher_x_distiller(opts: BenchOpts) -> Table:
    """Cluster the corpus with each embedder at its own threshold; report purity."""
    import numpy as np

    from opendaisugi.distiller import _cluster_with_centroids

    ref = resolve_corpus(matcher_bench.CORPUS, opts.corpus)
    tasks = [r["task"] for r in load_jsonl(ref)]
    paras = load_jsonl(resolve_corpus("paraphrases.jsonl", None))
    task_by_id = {r["id"]: r["task"] for r in load_jsonl(ref)}
    texts = tasks + [p["task"] for p in paras]
    truth = list(range(len(tasks))) + [tasks.index(task_by_id[p["of"]]) for p in paras]

    columns = (
        Column("option", "embedder"),
        Column("threshold", "threshold", align="right"),
        Column("clusters", "clusters", align="right"),
        Column("purity", "paraphrase purity", align="right"),
        Column("singletons", "singletons", align="right"),
    )
    rows: list[Row] = []
    for name, spec in MATCHER_BACKENDS.items():
        embedder = matcher_bench.build_embedder(name)
        if embedder is None:
            rows.append(Row(name=name, absent=f"pip install '{spec.extra or name}'"))
            continue
        thr = matcher_bench._threshold_for(name)
        vecs = np.asarray(embedder.encode(texts))
        clusters = _cluster_with_centroids(list(range(len(texts))), vecs, threshold=thr)
        together = 0
        for members, _ in clusters:
            labels = [truth[i] for i in members]
            together += sum(1 for a, b in itertools.combinations(labels, 2) if a == b)
        rows.append(
            Row(
                name=f"{name}@{thr}",
                cells={
                    "option": name,
                    "threshold": thr,
                    "clusters": len(clusters),
                    "purity": round(together / max(1, len(paras)), 3),
                    "singletons": sum(1 for m, _ in clusters if len(m) == 1),
                },
            )
        )
    return Table(
        "matcher-x-distiller",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(WHY["matcher-x-distiller"][1],),
    )


def router_x_models(opts: BenchOpts) -> Table:
    """Each router crossed with each cheap-model choice in the committed price table."""
    ref = resolve_corpus(router_bench.CORPUS, opts.corpus)
    turns = load_jsonl(ref)
    targets = load_json(resolve_corpus("switchyard-targets.json", None))
    price_pairs = load_json(resolve_corpus("prices.json", None))
    prices = {k: (float(v[0]), float(v[1])) for k, v in price_pairs.items()}
    from opendaisugi.gateway import price_turn

    columns = (
        Column("option", "router"),
        Column("cheap_model", "cheap model"),
        Column("turns", "turns", align="right"),
        Column("local_share", "local share", align="right"),
        Column("cost", "est. cost $", align="right"),
    )
    candidates = [targets["cheap"], targets["local"]]
    rows: list[Row] = []
    for option in router_bench.ROUTERS:
        for cheap in candidates:
            local = targets["local"]
            these = dict(targets, cheap=cheap)
            chosen = [router_bench.choose(option, r, these) for r in turns]
            cost = 0.0
            for model, row in zip(chosen, turns, strict=True):
                usage = row["usage"]
                cost += price_turn(
                    model,
                    int(usage.get("input_tokens", 0)),
                    int(usage.get("output_tokens", 0)),
                    cache_read_tokens=int(usage.get("cache_read_input_tokens", 0)),
                    cache_creation_tokens=int(usage.get("cache_creation_input_tokens", 0)),
                    prices=prices,
                ).dollars
            rows.append(
                Row(
                    name=f"{option}/{cheap}",
                    cells={
                        "option": option,
                        "cheap_model": cheap,
                        "turns": len(turns),
                        "local_share": round(sum(m == local for m in chosen) / len(turns), 4),
                        "cost": round(cost, 4),
                    },
                )
            )
    return Table(
        "router-x-models", columns, tuple(rows), ref, _REPRODUCE, notes=(WHY["router-x-models"][1],)
    )


def loop_x_gate_path(opts: BenchOpts) -> Table:
    """Every loop against its gate path: battery, contract, class."""
    ref = resolve_corpus(loop_bench.CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    vocab = load_json(resolve_corpus("loop-vocabulary.json", None))
    envelope = gate_path_bench.battery_envelope()
    gate_cases = load_jsonl(resolve_corpus(gate_path_bench.CORPUS, None))
    columns = (
        Column("option", "loop"),
        Column("gate_path", "gate path"),
        Column("gate_cases", "gate cases"),
        Column("contract", "path contract"),
        Column("fail_open", "fail-open class"),
    )
    rows: list[Row] = []
    for name, spec in LOOPS.items():
        entry = vocab.get(name) or {}
        if not entry.get("names"):
            rows.append(Row(name=name, absent=spec.install))
            continue
        passed, applicable, na = loop_bench.run_vocabulary_cases(name, cases, vocab, envelope)
        cpassed, ctotal = gate_path_bench.run_contract(spec.gate_path, gate_cases, envelope)
        rows.append(
            Row(
                name=f"{name}/{spec.gate_path}",
                cells={
                    "option": name,
                    "gate_path": spec.gate_path,
                    "gate_cases": f"{passed}/{applicable}" + (f" ({na} n/a)" if na else ""),
                    "contract": f"{cpassed}/{ctotal}",
                    "fail_open": GATE_PATHS[spec.gate_path].fail_open_class,
                },
            )
        )
    return Table(
        "loop-x-gate-path",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(WHY["loop-x-gate-path"][1],),
    )


def backend_x_envelope(opts: BenchOpts) -> Table:
    """Each backend crossed with each envelope source.

    evidence-inferred spends nothing on any backend: ADR-0016 builds the envelope
    from observed steps with no model call, so `tokens: 0` is a fact about that
    source. Everything else about an evidence-inferred envelope is NOT known
    here — it is a different envelope with a different clause count, and none are
    recorded — so those cells print `n/a`. Reusing the LLM row's numbers under
    this label would be a fabricated cell.
    """
    ref = resolve_corpus(backend_bench.CORPUS, opts.corpus)
    columns = (
        Column("option", "backend"),
        Column("envelope_source", "envelope source"),
        Column("envelopes", "envelopes", align="right"),
        Column("mean_clauses", "mean clauses", align="right"),
        Column("tokens", "tokens", align="right"),
    )
    from opendaisugi.swap import SWAP_KNOBS

    rows: list[Row] = []
    for option in SWAP_KNOBS["backend"].options:
        name = str(option.value)
        recorded = backend_bench._recorded(name)
        if recorded is None:
            rows.append(Row(name=name, absent="record with `daisugi bench backend --live`"))
            continue
        from opendaisugi.models import Envelope

        envelopes = [Envelope.model_validate(r["envelope"]) for r in recorded]
        clauses = [backend_bench.clause_count(e) for e in envelopes]
        mean = round(sum(clauses) / max(1, len(clauses)), 1)
        tokens = sum(int(r["tokens_in"]) + int(r["tokens_out"]) for r in recorded)
        rows.append(
            Row(
                name=f"{name}/llm-generated",
                cells={
                    "option": name,
                    "envelope_source": "llm-generated",
                    "envelopes": len(envelopes),
                    "mean_clauses": mean,
                    "tokens": tokens,
                },
            )
        )
        rows.append(
            Row(
                name=f"{name}/evidence-inferred",
                cells={
                    "option": name,
                    "envelope_source": "evidence-inferred",
                    # Nothing evidence-inferred was recorded, so nothing is
                    # claimed for it beyond the one fact ADR-0016 guarantees:
                    # no model call, therefore no tokens.
                    "envelopes": "n/a",
                    "mean_clauses": "n/a",
                    "tokens": 0,
                },
            )
        )
    return Table(
        "backend-x-envelope",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(
            WHY["backend-x-envelope"][1],
            "evidence-inferred rows show n/a for what was not recorded; only tokens=0 "
            "is claimed, and that follows from ADR-0016 making no model call",
        ),
    )


_RUNNERS = {
    "verifier-x-shell": verifier_x_shell,
    "matcher-x-distiller": matcher_x_distiller,
    "router-x-models": router_x_models,
    "loop-x-gate-path": loop_x_gate_path,
    "backend-x-envelope": backend_x_envelope,
}


def run_all(opts: BenchOpts) -> list[Table]:
    """One table per pair, in PAIRS order. Each pair runs exactly once."""
    return [_RUNNERS[name](opts) for name in PAIRS]


def index_of(tables: list[Table]) -> Table:
    """The index table, derived from tables that already ran.

    Deriving rather than re-running is the point: the index needs a row count,
    and re-running five benches to count their rows would double the cost of
    `daisugi bench pairs` for no new information.
    """
    columns = (
        Column("option", "pair"),
        Column("layers", "layers"),
        Column("rows", "rows", align="right"),
        Column("why", "why it earns a row"),
    )
    rows = []
    for name, table in zip(PAIRS, tables, strict=True):
        title, why = WHY[name]
        rows.append(
            Row(
                name=name,
                cells={"option": name, "layers": title, "rows": len(table.rows), "why": why},
            )
        )
    return Table(
        "pairs",
        columns,
        tuple(rows),
        None,
        _REPRODUCE,
        notes=("five pairs, by argument; a sixth needs a reason recorded in pairs.py",),
    )


def run(opts: BenchOpts) -> Table:
    """The index: which pairs exist, which layers they cross, and why."""
    return index_of(run_all(opts))


register(BenchSpec(name="pairs", layer="pairs", corpus="tasks.jsonl", columns=(), run=run))
```

Add to `src/opendaisugi/bench/layers/__init__.py`:

```python
from opendaisugi.bench import pairs  # noqa: F401 — registers the pairs index
```

Then make `daisugi bench pairs` print all five without running them twice. In `bench_cmd`
(Task 3), replace the loop that builds `tables` with:

```python
tables = []
for name in targets:
    try:
        if name == "pairs":
            from opendaisugi.bench.pairs import index_of, run_all

            pair_tables = run_all(BenchOpts(corpus=corpus, live=live, data_dir=data_dir))
            # `bench pairs` shows the five; `bench all` shows only the index,
            # so a full sweep stays one screen.
            tables.extend(pair_tables if layer == "pairs" else [index_of(pair_tables)])
        else:
            tables.append(run_bench(name, BenchOpts(corpus=corpus, live=live, data_dir=data_dir)))
    except CorpusMissing as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=1) from exc
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/bench/test_bench_pairs.py -q`
Expected: PASS, 10 tests.

- [ ] **Step 5: Check the command and the timing**

Run: `time uv run --no-sync daisugi bench pairs`
Expected: five tables, each with a `reproduce:` line, in well under 30 s.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/bench/pairs.py src/opendaisugi/bench/layers/__init__.py \
        src/opendaisugi/cli.py tests/bench/test_bench_pairs.py
git commit -m "bench: the five cross-layer pairs, and the argument for each

Contracts make most cross-layer cells theatre, so the matrix is five rows rather
than n by n, and each carries in code the reason it earns one: a shared command
text, a shared embedding threshold, a shared price table, a shared gate path, a
shared decision about whether the model is called at all. A sixth pair needs an
entry in WHY before it needs code."
```

---

### Task 11: `verifier_dispatch.verify_via` — the compiled clients at runtime

**Files:**
- Create: `src/opendaisugi/verifier_dispatch.py`
- Modify: `src/opendaisugi/models.py:586-595` (`VerificationResult` gains two optional fields)
- Test: `tests/test_verifier_dispatch.py`

**Interfaces:**
- Consumes: `VERIFIER_CLIENTS`, `client_argv`, `client_is_built` (Task 1);
  `opendaisugi.conformance.make_verify_case`, `canonical_json`; `opendaisugi.verify.verify`.
- Produces:
  ```python
  # models.py — additive, all defaulted, so every existing construction is unchanged
  class VerificationResult(BaseModel):
      ...
      client: str = "python"           # which client was dispatched for this verdict
      fallback: str | None = None      # "python" when the dispatched client failed
      client_verdict: bool | None = None   # what the client alone said; None when it failed

  # verifier_dispatch.py
  class ClientFailure(RuntimeError): ...
  LAST_DISPATCH_NAME = "last_dispatch.json"

  def verify_via(client: str, plan: ActionPlan, envelope: Envelope, *,
                 timeout_s: float = 5.0, root: Path | None = None) -> VerificationResult
  def last_dispatch(root: Path | None = None) -> dict          # {} when nothing recorded
  def _record_dispatch(root: Path, client: str, ok: bool, error: str) -> None
  ```

**The dispatch rule: a client may only ever tighten.** The Python oracle always runs, and the
returned verdict is `ok = oracle.ok and client_ok`. A dispatched client can turn an allow into a
deny; it can never turn a deny into an allow.

This is not caution for its own sake. `swap.py:112` offers `lean (proven core)` as a
`verifier_client`, and `clients/lean/DaisugiVerify/Verify.lean` runs delegation safety,
permissions and DAG, then returns ok — it implements no Full-profile stage, which
`clients/lean/README.md` and `docs/client-diversity.md` both state ("34 cases whose oracle verdict
turns on a Full-profile stage this client doesn't implement by design"). Letting a clean client's
allow stand would mean that selecting `lean` silently switches off every invariant, postcondition,
vacuity check and subsumption check in every envelope. ADR-0001: when correctness cannot be
proven, deny.

The cost is nothing already unspent. `docs/spec/conformance.md` measures the in-process oracle at
p50 0.132 ms while a dispatch pays a 1.7 ms to 850 ms process spawn, so the oracle was never the
expensive half. The value of dispatch is diversity, not speed, and the conjunction is what makes
diversity safe to switch on.

`client_verdict` records what the client alone said, so `daisugi bench verifier` and
`daisugi modules` can attribute a disagreement without re-running anything.

On any client failure (missing binary, non-zero exit, parse error, timeout, missing verdict) the
oracle's verdict stands alone with `fallback="python"`, `client_verdict=None`, and a `WARNING`
naming the failure.

**Rejected non-goal:** *client-authoritative dispatch* — letting a clean client's verdict replace
the oracle's. It is faster on paper only (see the timings above), and it makes the safety of every
envelope depend on which profile the selected client happens to implement.

- [ ] **Step 1: Write the failing test**

Create `tests/test_verifier_dispatch.py`:

```python
"""Dispatching to a compiled verifier client must never turn a failure into an
allow, and must say loudly when it fell back to the Python oracle."""

import json
import logging
from pathlib import Path

import pytest

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileWriteStep,
    Invariant,
    Permission,
    ShellStep,
)
from opendaisugi.verifier_dispatch import last_dispatch, verify_via

ENV_STRICT = Envelope(
    generated_by="test",
    task="run tests",
    permissions=Permission(shell=True, shell_allowlist=["pytest"]),
)


def _plan(command: str) -> ActionPlan:
    return ActionPlan(source="test", task="run tests", steps=[ShellStep(id="s1", command=command)])


def _fake_client(tmp_path: Path, body: str) -> Path:
    script = tmp_path / "fake_client.py"
    script.write_text(body)
    return script


def _argv(script: Path) -> list[str]:
    import sys

    return [sys.executable, str(script)]


@pytest.fixture()
def register_fake(monkeypatch, tmp_path):
    def _register(body: str, name: str = "fake"):
        from opendaisugi.bench import options

        script = _fake_client(tmp_path, body)
        spec = options.ClientSpec(
            name=name,
            argv=tuple(_argv(script)),
            probe=None,
            build_steps=(),
            build_cwd=".",
            readme="docs/spec/conformance.md",
        )
        monkeypatch.setitem(options.VERIFIER_CLIENTS, name, spec)
        return name

    return _register


AGREEING = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": case["expect"]["ok"],
                      "violations": case["expect"]["violations"]}), flush=True)
"""

CRASHING = "import sys; sys.exit(3)\n"
GARBAGE = "import sys; sys.stdin.read(); print('not json', flush=True)\n"
HANGING = "import sys, time; sys.stdin.read(); time.sleep(30)\n"
# A Core-profile client: it answers ok for anything, which is what a client that
# implements no Full-profile stage does on an envelope whose protection is a
# predicate. Named for the profile, not for "permissive", because the profile is
# the real reason the verdict cannot be trusted alone.
CORE_PROFILE = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": True, "violations": []}), flush=True)
"""

REFUSING = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": False,
                      "violations": [{"stage": "permissions", "step": "s1"}]}), flush=True)
"""


def test_a_working_client_answers_and_is_named(register_fake, tmp_path):
    name = register_fake(AGREEING)
    result = verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is True
    assert result.client == name
    assert result.fallback is None
    assert result.client_verdict is True


def test_a_working_client_reproduces_a_denial(register_fake, tmp_path):
    name = register_fake(AGREEING)
    result = verify_via(name, _plan("rm -rf /"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is False
    assert result.violations
    assert result.fallback is None
    assert result.client_verdict is False


def test_dispatch_never_allows_what_the_oracle_denies(register_fake, tmp_path):
    """The blocker case. A Core-profile client runs delegation, permissions and
    DAG and then says ok — exactly what clients/lean/DaisugiVerify/Verify.lean
    does. On an envelope whose only protection is an invariant, its allow must
    not stand, or selecting `lean` would silently switch off the predicate
    algebra for every envelope."""
    envelope = Envelope(
        generated_by="test",
        task="write a build artifact",
        permissions=Permission(
            shell=True, shell_allowlist=["pytest"], file_write=["build/**"], file_read=["**"]
        ),
        invariants=[
            Invariant(
                type="shell_only",
                description="every step is a shell step",
                expr={
                    "op": "forall_steps",
                    "pred": {"op": "equals", "path": "type", "value": "shell"},
                },
            )
        ],
    )
    plan = ActionPlan(
        source="test",
        task="write a build artifact",
        steps=[
            ShellStep(id="s1", command="pytest -q"),
            FileWriteStep(id="s2", path="build/out.txt", content="ok"),
        ],
    )
    # The oracle refuses this on the predicate stage; a Core client cannot see it.
    from opendaisugi.verify import verify

    assert verify(plan, envelope, strict=None).ok is False

    name = register_fake(CORE_PROFILE, name="core")
    result = verify_via(name, plan, envelope, root=tmp_path / "gate")
    assert result.ok is False, "a core-profile allow overrode the oracle"
    assert result.client_verdict is True, "the client's own allow is still recorded"
    assert result.client == name
    assert any(v.stage == "predicate" for v in result.violations)


def test_a_client_may_tighten_an_allow_into_a_deny(register_fake, tmp_path):
    """The other direction is allowed: diversity exists to catch what the oracle
    missed, so a client that refuses what the oracle accepts wins."""
    name = register_fake(REFUSING, name="strict")
    result = verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is False
    assert result.client_verdict is False
    assert any("strict" in v.message for v in result.violations)


def test_dispatch_never_allows_on_client_failure(register_fake, tmp_path):
    """The whole point. A crash, a timeout, garbage, or a missing binary must
    all resolve to the oracle's verdict, which for this plan is a denial."""
    for body in (CRASHING, GARBAGE, HANGING):
        name = register_fake(body, name=f"fake-{len(body)}")
        result = verify_via(
            name, _plan("rm -rf /"), ENV_STRICT, timeout_s=1.0, root=tmp_path / "gate"
        )
        assert result.ok is False, body[:20]
        assert result.fallback == "python"
        assert result.client_verdict is None


def test_an_unknown_client_name_falls_back_and_never_allows(tmp_path):
    result = verify_via("no-such-client", _plan("rm -rf /"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is False
    assert result.fallback == "python"


def test_a_failure_still_returns_the_oracle_s_allow_when_the_plan_is_fine(tmp_path):
    result = verify_via("no-such-client", _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is True
    assert result.fallback == "python"
    assert result.client == "no-such-client"


def test_the_fallback_warns_and_names_the_failure(register_fake, tmp_path, caplog):
    name = register_fake(CRASHING, name="crashy")
    with caplog.at_level(logging.WARNING, logger="opendaisugi.verifier_dispatch"):
        verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert any("crashy" in r.message for r in caplog.records)
    assert any("python" in r.message for r in caplog.records)


def test_the_last_dispatch_is_recorded_for_the_module_map(register_fake, tmp_path):
    root = tmp_path / "gate"
    name = register_fake(AGREEING, name="recorded")
    verify_via(name, _plan("pytest -q"), ENV_STRICT, root=root)
    record = last_dispatch(root)
    assert record["client"] == "recorded"
    assert record["ok"] is True
    verify_via("no-such-client", _plan("pytest -q"), ENV_STRICT, root=root)
    assert last_dispatch(root)["ok"] is False


def test_last_dispatch_on_a_fresh_root_is_empty(tmp_path):
    assert last_dispatch(tmp_path / "nothing") == {}


def test_the_verify_case_body_shape_did_not_change():
    """The additive result fields are safe only because make_verify_case projects
    through _normative_violations. If a result is ever dumped straight into a
    case, every content-addressed id in every corpus shifts silently."""
    from opendaisugi.conformance import make_verify_case
    from opendaisugi.verify import verify

    plan = _plan("pytest -q")
    result = verify(plan, ENV_STRICT, strict=None)
    body = make_verify_case(plan, ENV_STRICT, {"strict": None, "z3_timeout_ms": 500}, result)
    assert set(body) == {"kind", "v", "plan", "envelope", "options", "expect", "id"}
    assert set(body["expect"]) == {"ok", "violations"}
    for v in body["expect"]["violations"]:
        assert set(v) == {"stage", "step"}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `uv run --no-sync pytest tests/test_verifier_dispatch.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.verifier_dispatch'`

- [ ] **Step 3: Add the two result fields**

In `src/opendaisugi/models.py`, extend `VerificationResult` (line 586):

```python
class VerificationResult(BaseModel):
    """Result of running the verification pipeline on a plan+envelope pair."""

    ok: bool
    violations: list[Violation] = Field(default_factory=list)
    warnings: list[str] = Field(default_factory=list)
    envelope_id: str
    plan_id: str
    duration_ms: float
    client: str = Field(
        default="python",
        description="Which verifier client was dispatched alongside the oracle. "
        "python means no client was dispatched; the oracle decided alone.",
    )
    fallback: str | None = Field(
        default=None,
        description="Set to 'python' when the dispatched client failed and the "
        "oracle's verdict stands alone. None means the client answered.",
    )
    client_verdict: bool | None = Field(
        default=None,
        description="What the dispatched client alone said, kept for attribution. "
        "The verdict in `ok` is the conjunction of this and the oracle's: a client "
        "may tighten an allow into a deny, never the reverse. None when the client "
        "failed or none was dispatched.",
    )
```

- [ ] **Step 4: Write verifier_dispatch.py**

Create `src/opendaisugi/verifier_dispatch.py`:

```python
"""Run a compiled conformance client as the runtime verifier.

The five clients already speak one wire protocol for conformance scoring
(`docs/spec/conformance.md`): a case as JSON on stdin, a verdict as JSON on
stdout. This module reuses that protocol for a live verification, so the runtime
choice and the differential harness cannot drift apart.

The fallback rule is the whole safety argument. A client that is missing, exits
non-zero, writes garbage, or runs long is a FAILED client, and a failed client's
answer is never used. The Python oracle decides instead, the result says
`fallback="python"`, and a WARNING names what broke. There is no path here that
turns a failure into an allow.

A client that runs cleanly is a second opinion, never a replacement: the oracle
always runs and the gate acts on ``oracle.ok and client_ok``. A clean client can
turn an allow into a deny; it can never turn a deny into an allow. Its own answer
is recorded as ``client_verdict`` so ``daisugi bench verifier`` can attribute
disagreement, and ``daisugi modules`` marks a client ACTIVE only after a dispatch
that succeeded.
"""

from __future__ import annotations

import json
import logging
import subprocess
import time
from pathlib import Path
from typing import Any

from opendaisugi.models import ActionPlan, Envelope, VerificationResult, Violation

_log = logging.getLogger("opendaisugi.verifier_dispatch")

DEFAULT_GATE_ROOT = Path.home() / ".opendaisugi" / "gate"
LAST_DISPATCH_NAME = "last_dispatch.json"


class ClientFailure(RuntimeError):
    """The dispatched client did not produce a usable verdict."""


def _case_for(plan: ActionPlan, envelope: Envelope) -> dict[str, Any]:
    """One conformance verify case, built without an expectation.

    `make_verify_case` needs a result to embed as `expect`; a live dispatch has
    none yet. The client ignores `expect`, so an empty one keeps the wire shape
    without pretending to know the answer.
    """
    from opendaisugi.conformance import CONFORMANCE_VERSION, case_id

    plan_body = plan.model_dump(mode="json")
    envelope_body = envelope.model_dump(mode="json")
    plan_body["id"] = "plan_case"
    envelope_body["id"] = "env_case"
    body = {
        "kind": "verify",
        "v": CONFORMANCE_VERSION,
        "plan": plan_body,
        "envelope": envelope_body,
        "options": {"strict": None, "z3_timeout_ms": 500},
        "expect": {"ok": False, "violations": []},
    }
    body["id"] = case_id(body)
    return body


def _run_client(argv: list[str], case: dict, timeout_s: float) -> dict:
    from opendaisugi.conformance import canonical_json

    try:
        proc = subprocess.run(  # noqa: S603 — the operator's own client binary
            argv,
            input=canonical_json(case) + "\n",
            capture_output=True,
            text=True,
            timeout=timeout_s,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise ClientFailure(f"timed out after {timeout_s}s") from exc
    except OSError as exc:
        raise ClientFailure(f"could not start: {exc}") from exc
    if proc.returncode != 0:
        raise ClientFailure(f"exited {proc.returncode}: {proc.stderr.strip()[-200:]}")
    for line in proc.stdout.splitlines():
        if not line.strip():
            continue
        try:
            verdict = json.loads(line)
        except ValueError as exc:
            raise ClientFailure(f"wrote a non-JSON verdict: {line[:120]!r}") from exc
        if verdict.get("id") == case["id"]:
            if "ok" not in verdict:
                raise ClientFailure(f"error verdict: {verdict.get('error', 'no ok field')}")
            return verdict
    raise ClientFailure("produced no verdict for the case")


def _step_detail(plan: ActionPlan, step_id: object) -> str:
    """The command or path of the step a client cited, for the human reason.

    A dispatched verdict carries only (stage, step); the operator reading an ask
    prompt needs to know WHAT was refused, so the text comes back from the plan.
    """
    for step in plan.steps:
        if step.id == step_id:
            for attr in ("command", "path", "url", "skill_id", "tool"):
                value = getattr(step, attr, None)
                if value:
                    return f"{attr}={value}"
            return f"step type {getattr(step, 'type', 'unknown')}"
    return "no matching step in the plan"


def _client_violations(verdict: dict, plan: ActionPlan, client: str) -> list[Violation]:
    """The client's own violations, rebuilt with a reason a human can act on.

    Only `ok` and the (stage, step) pairs are normative, so the message is
    reconstructed. It names the client AND the step's command or path, because
    `evaluate_record` builds the deny summary and the --ask prompt from these
    strings, and "the rust client rejected this step" tells an operator nothing.
    """
    return [
        Violation(
            stage=str(v.get("stage", "unknown")),
            message=(
                f"the {client} verifier client refused this step "
                f"({_step_detail(plan, v.get('step'))})"
            ),
            detail={"step": v.get("step"), "client": client},
        )
        for v in verdict.get("violations", [])
    ]


def _record_dispatch(root: Path, client: str, ok: bool, error: str) -> None:
    """Remember the last dispatch so `daisugi modules` can be honest.

    Best-effort: a read-only disk must never change a verdict.
    """
    try:
        target = root / "verifier"
        target.mkdir(parents=True, exist_ok=True, mode=0o700)
        (target / LAST_DISPATCH_NAME).write_text(
            json.dumps({"client": client, "ok": ok, "error": error, "at": time.time()}),
            encoding="utf-8",
        )
    except OSError:
        pass


def last_dispatch(root: Path | None = None) -> dict:
    """The last recorded dispatch, or {} when there is none."""
    base = root or DEFAULT_GATE_ROOT
    try:
        return json.loads((base / "verifier" / LAST_DISPATCH_NAME).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}


def verify_via(
    client: str,
    plan: ActionPlan,
    envelope: Envelope,
    *,
    timeout_s: float = 5.0,
    root: Path | None = None,
) -> VerificationResult:
    """Verify with the oracle AND a compiled client. The client may only tighten.

    The returned ``ok`` is ``oracle.ok and client_ok``. A client that runs
    cleanly can turn an allow into a deny; it can never turn a deny into an
    allow, because a Core-profile client implements none of the predicate or Z3
    stages and its allow means "I did not look", not "this is safe".

    Any client failure leaves the oracle's verdict standing alone with
    ``fallback="python"``, ``client_verdict=None``, and a WARNING naming what
    broke.
    """
    from opendaisugi.bench.options import VERIFIER_CLIENTS, client_argv, client_is_built
    from opendaisugi.verify import verify

    base = root or DEFAULT_GATE_ROOT
    oracle = verify(plan, envelope, strict=None)

    spec = VERIFIER_CLIENTS.get(client)
    failure = ""
    verdict: dict | None = None
    if spec is None:
        failure = f"no client named {client!r}"
    elif not client_is_built(spec):
        from opendaisugi.bench.options import build_hint

        failure = f"not built; {build_hint(spec)}"
    else:
        argv = client_argv(spec) or list(spec.argv)
        try:
            verdict = _run_client(argv, _case_for(plan, envelope), timeout_s)
        except ClientFailure as exc:
            failure = str(exc)

    if verdict is None:
        _log.warning(
            "verifier client %r failed (%s); the python oracle decided alone. "
            "Run `daisugi bench verifier` to see which clients are built.",
            client,
            failure,
        )
        _record_dispatch(base, client, False, failure)
        return oracle.model_copy(
            update={"client": client, "fallback": "python", "client_verdict": None}
        )

    _record_dispatch(base, client, True, "")
    client_ok = bool(verdict["ok"])
    if client_ok != oracle.ok:
        _log.warning(
            "verifier client %r disagreed with the python oracle (client ok=%s, "
            "oracle ok=%s); the stricter verdict stands. This is a finding: "
            "run `daisugi bench verifier` on the corpus.",
            client,
            client_ok,
            oracle.ok,
        )
    return oracle.model_copy(
        update={
            "ok": oracle.ok and client_ok,
            # The oracle's violations carry the clause and counterexample an
            # operator reads; the client's are appended so a tightening is
            # attributable rather than anonymous.
            "violations": list(oracle.violations) + _client_violations(verdict, plan, client),
            "client": client,
            "fallback": None,
            "client_verdict": client_ok,
        }
    )
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_verifier_dispatch.py -q`
Expected: PASS, 10 tests.

Run: `uv run --no-sync pytest tests/test_conformance.py tests/test_verify.py -q`
Expected: PASS. The two new fields are defaulted and `make_verify_case` projects through
`_normative_violations`, so no case id moves.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/verifier_dispatch.py src/opendaisugi/models.py \
        tests/test_verifier_dispatch.py
git commit -m "verifier: dispatch to a compiled client that may only ever tighten

The five clients already speak one wire protocol for conformance, so a runtime
dispatch reuses it rather than inventing a second one that could drift. The
oracle always runs and the verdict is the conjunction: a client can turn an
allow into a deny, never the reverse. That is not caution for its own sake — the
Lean client implements no Full-profile stage by design, so letting its allow
stand would mean selecting it silently switches off every invariant and
subsumption check in every envelope. The oracle was never the expensive half
either: it is 0.13 ms against a 1.7 ms process spawn. A client that is missing,
crashes, hangs or writes garbage leaves the oracle alone with fallback=python and
a warning, and its own answer is recorded as client_verdict for attribution."
```

---

### Task 12: The gate dispatches, the module map tells the truth, the verifier stage goes live

**Files:**
- Modify: `src/opendaisugi/gate.py:174-198` (`_verify_with_timeout` dispatches by config)
- Modify: `src/opendaisugi/gate.py:245-252` (the fail-closed deny names the configured client)
- Modify: `src/opendaisugi/swap.py:35-46` (`STAGE_EFFECT["verifier"] = LIVE`)
- Modify: `src/opendaisugi/swap.py:1-19` (docstring: verifier is no longer planned)
- Modify: `src/opendaisugi/modules.py:145-158` (verifier modules reflect built and last-dispatch)
- Modify: `src/opendaisugi/tui_wiring.py:36-45` (drop the verifier hint; it is live now)
- Modify: `tests/test_swap.py:54-70` (verifier is live and a knob)
- Modify: `tests/test_modules.py:27-33` (the verifier stage is a real swap point when a client is built)
- Modify: `tests/test_modules.py:56-66` (the verifier box no longer renders `[planned]`)
- Modify: `tests/test_tui_honesty.py:196-205` (assert the gate's effnote, which stays `cfg` forever)
- Modify: `tests/test_hot_swap.py` (add `prove_verifier_swap` and register it)
- Test: `tests/test_gate_dispatch.py`

**Interfaces:**
- Consumes: `verify_via`, `last_dispatch` (Task 11); `LIVE_PROOFS` (Task 9).
- Produces:
  ```python
  # gate.py
  def _dispatch_verify(plan: ActionPlan, envelope: Envelope, *, timeout_s: float) -> VerificationResult
      """verify() when config.verifier_client is python, else verify_via."""

  # tests/test_hot_swap.py
  def prove_verifier_swap(tmp: Path) -> None
  ```

- [ ] **Step 1: Write the failing tests**

Create `tests/test_gate_dispatch.py`:

```python
"""The gate reads verifier_client per call, and every dispatch failure still
denies. Fail-closed is not negotiable at the boundary."""

from pathlib import Path

from opendaisugi.gate import evaluate_record
from opendaisugi.models import Envelope, Permission

ENV = Envelope(
    generated_by="test",
    task="run tests",
    permissions=Permission(shell=True, shell_allowlist=["pytest"]),
)


def _record(command: str) -> dict:
    return {"tool_name": "Bash", "step_type": "shell", "command": command}


def _config(data_dir: Path, client: str) -> None:
    """Write the choice the way the swap menu writes it, not raw YAML."""
    from opendaisugi.config import load_config, save_config

    data_dir.mkdir(parents=True, exist_ok=True)
    path = data_dir / "config.yaml"
    save_config(load_config(path).model_copy(update={"verifier_client": client}), path)


def test_python_is_the_default_and_still_decides(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    assert evaluate_record(_record("pytest -q"), ENV, mode="enforce").allow is True
    assert evaluate_record(_record("rm -rf /"), ENV, mode="enforce").allow is False


def test_an_unbuilt_client_falls_back_and_still_denies(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _config(tmp_path, "lean")
    decision = evaluate_record(_record("rm -rf /"), ENV, mode="enforce")
    assert decision.allow is False


def test_a_dispatch_that_raises_denies_fail_closed_and_names_the_client(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _config(tmp_path, "rust")

    def _boom(*a, **k):
        raise RuntimeError("dispatch exploded")

    monkeypatch.setattr("opendaisugi.verifier_dispatch.verify_via", _boom)
    decision = evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    assert decision.allow is False
    assert "denied fail-closed" in decision.reason
    assert "verifier_client=rust" in decision.reason
    assert "verifier_client: python" in decision.reason


def test_a_hung_client_leaves_room_for_the_oracle_inside_the_gate_budget(tmp_path, monkeypatch):
    """A dispatch gets half the inner budget. Without that split a hung client
    eats the whole budget, the join fires, and every call denies for good."""
    import opendaisugi
    from opendaisugi.gate import _DISPATCH_BUDGET_FRACTION, _dispatch_verify

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _config(tmp_path, "rust")
    seen = {}

    def _spy(client, plan, envelope, *, timeout_s, root):
        from opendaisugi.verify import verify

        seen["timeout_s"] = timeout_s
        seen["root"] = root
        return verify(plan, envelope, strict=None)

    monkeypatch.setattr("opendaisugi.verifier_dispatch.verify_via", _spy)
    from opendaisugi.models import ActionPlan, ShellStep

    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="pytest -q")])
    _dispatch_verify(plan, ENV, timeout_s=10.0)
    assert seen["timeout_s"] == 10.0 * _DISPATCH_BUDGET_FRACTION
    assert seen["root"] == tmp_path / "gate"


def test_the_client_choice_is_read_per_call_not_at_import(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    seen: list[str] = []

    def _spy(client, plan, envelope, **kwargs):
        from opendaisugi.verify import verify

        seen.append(client)
        return verify(plan, envelope, strict=None)

    monkeypatch.setattr("opendaisugi.verifier_dispatch.verify_via", _spy)
    _config(tmp_path, "rust")
    evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    _config(tmp_path, "go")
    evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    assert seen == ["rust", "go"], "the gate cached the client choice"


def test_the_gate_root_follows_the_data_dir_so_a_dispatch_never_writes_to_home(
    tmp_path, monkeypatch
):
    """`_dispatch_verify` passes `root=DEFAULT_DATA_DIR / "gate"`. Without that,
    every test dispatch would stamp the operator's real ~/.opendaisugi/gate."""
    import opendaisugi
    from opendaisugi.verifier_dispatch import last_dispatch

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _config(tmp_path, "lean")
    evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    assert last_dispatch(tmp_path / "gate")["client"] == "lean"


def test_a_bad_client_name_in_config_denies_nothing_extra_but_still_verifies(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _config(tmp_path, "not-a-client")
    assert evaluate_record(_record("rm -rf /"), ENV, mode="enforce").allow is False
    assert evaluate_record(_record("pytest -q"), ENV, mode="enforce").allow is True
```

Add to `tests/test_hot_swap.py`, above `LIVE_PROOFS`:

```python
def prove_verifier_swap(tmp: Path) -> None:
    """Rewrite verifier_client in config.yaml, call the gate, and the gate
    dispatches to the new client. No restart.

    The observation runs through `gate.evaluate_record` — the function every hook
    payload lands in — and reads back the dispatch that actually happened from
    `verifier_dispatch.last_dispatch`, which the gate writes under the data dir.
    Nothing here inspects a private attribute or re-reads the YAML.

    The clients are fake scripts, never compiled binaries, so the proof holds on
    a box where nothing is built. A proof that skips would let the live tag pass
    vacuously, which is exactly what the honesty test exists to catch.
    """
    import sys

    import opendaisugi
    from opendaisugi.bench import options
    from opendaisugi.gate import evaluate_record
    from opendaisugi.models import Envelope, Permission
    from opendaisugi.verifier_dispatch import last_dispatch

    data_dir = tmp / "verifier"
    data_dir.mkdir(parents=True, exist_ok=True)
    script = data_dir / "agree.py"
    script.write_text(
        "import json, sys\n"
        "for line in sys.stdin:\n"
        "    line = line.strip()\n"
        "    if not line:\n"
        "        continue\n"
        "    case = json.loads(line)\n"
        "    print(json.dumps({'id': case['id'], 'ok': True, 'violations': []}), flush=True)\n"
    )
    envelope = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["pytest"]),
    )
    record = {"tool_name": "Bash", "step_type": "shell", "command": "pytest -q"}
    original = opendaisugi.DEFAULT_DATA_DIR
    saved = dict(options.VERIFIER_CLIENTS)
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        for name in ("alpha", "beta"):
            options.VERIFIER_CLIENTS[name] = options.ClientSpec(
                name=name,
                argv=(sys.executable, str(script)),
                probe=None,
                build_steps=(),
                build_cwd=".",
                readme="docs/spec/conformance.md",
            )

        _write_config(data_dir, verifier_client="alpha")
        assert evaluate_record(record, envelope, mode="enforce").allow is True
        assert last_dispatch(data_dir / "gate")["client"] == "alpha"

        _write_config(data_dir, verifier_client="beta")
        assert evaluate_record(record, envelope, mode="enforce").allow is True
        assert last_dispatch(data_dir / "gate")["client"] == "beta", (
            "the gate cached the client choice"
        )
    finally:
        options.VERIFIER_CLIENTS.clear()
        options.VERIFIER_CLIENTS.update(saved)
        opendaisugi.DEFAULT_DATA_DIR = original


def test_verifier_swap_is_live_without_restart(tmp_path):
    prove_verifier_swap(tmp_path)
```

and extend the registry at the bottom of the file:

```python
LIVE_PROOFS: dict[str, Callable[[Path], None]] = {
    "backend": prove_backend_swap,
    "verifier": prove_verifier_swap,
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run --no-sync pytest tests/test_gate_dispatch.py tests/test_hot_swap.py -q`
Expected: FAIL — `test_the_client_choice_is_read_per_call_not_at_import` fails because
`_verify_with_timeout` calls `verify` directly and `seen` stays empty.

- [ ] **Step 3: Make the gate dispatch**

In `src/opendaisugi/gate.py`, add above `_verify_with_timeout` (line 174):

```python
# A dispatched client gets HALF the gate's inner budget. The other half is the
# oracle's, which verify_via runs anyway and which must still finish inside the
# join in _verify_with_timeout — otherwise one hung client turns every tool call
# into a permanent "gate internal error" deny with no clue which client did it.
_DISPATCH_BUDGET_FRACTION = 0.5


def _dispatch_verify(plan: ActionPlan, envelope: Envelope, *, timeout_s: float):
    """Verify with the client this config names, right now.

    The choice is read per call, which is what makes the verifier stage live: the
    resident gate is long-lived, so a cached choice would need a restart.
    verify_via runs the oracle too and returns the conjunction, so a dispatch
    failure costs a warning and never an allow; anything that still escapes is
    caught by evaluate_record's fail-closed except.

    The gate root is taken under the data dir, matching resolve_gate_mode, so a
    dispatch record never lands in the operator's real home during a test.
    """
    from opendaisugi.verify import verify as _verify

    try:
        from opendaisugi import DEFAULT_DATA_DIR
        from opendaisugi.config import load_config

        data_dir = DEFAULT_DATA_DIR
        client = load_config(data_dir / "config.yaml").verifier_client
    except Exception:  # noqa: BLE001 — a broken config must not skip verification
        return _verify(plan, envelope, strict=None)
    if client == "python":
        return _verify(plan, envelope, strict=None)
    from opendaisugi.verifier_dispatch import verify_via

    return verify_via(
        client,
        plan,
        envelope,
        timeout_s=timeout_s * _DISPATCH_BUDGET_FRACTION,
        root=data_dir / "gate",
    )
```

and change the body of `_verify_with_timeout`'s worker (line 185) from
`box.append(verify(plan, envelope, strict=None))` to
`box.append(_dispatch_verify(plan, envelope, timeout_s=timeout_s))`.

Update `_verify_with_timeout`'s docstring first line to:
`"""Run the configured verifier in a worker thread with an inner deny-on-timeout."""`

Then name the client in the timeout deny, so an operator whose chosen client hangs can see which
one to change. In `evaluate_record` (line 245), replace the bare `except Exception` handler's
reason string with:

```python
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        try:
            from opendaisugi import DEFAULT_DATA_DIR
            from opendaisugi.config import load_config

            chosen = load_config(DEFAULT_DATA_DIR / "config.yaml").verifier_client
        except Exception:  # noqa: BLE001
            chosen = "python"
        via = "" if chosen == "python" else f" (verifier_client={chosen})"
        return _deny(
            mode,
            f"gate internal error{via} (denied fail-closed): {exc}. "
            f"Set verifier_client: python in config.yaml to rule out the client.",
            tool_name=tool_name,
            step_type=step_type,
            detail=detail,
            t0=t0,
        )
```

- [ ] **Step 4: Make the honesty surfaces true**

In `src/opendaisugi/swap.py`, change line 38 to `"verifier": LIVE,  # dispatched per call by verifier_dispatch` and update the module
docstring (lines 5-12) so `planned` no longer lists the verifier:

```python
* ``live``    — takes effect now, no restart (shell, distill, matcher, backend,
                verifier, router).
* ``cfg``     — a real choice, but it needs an install / restart / relaunch
                (gate, harness, envelope). ``CFG_REASON`` says why, per stage.
* ``planned`` — you can record it, but nothing reads it yet (stores).
```

In `src/opendaisugi/modules.py`, replace the verifier stage's module list (lines 149-157):

```python
(Module("python (in-process)", ACTIVE, "the runtime checker"),)
(*_client_modules(),)
```

and add above `detect_stages` (line 67):

```python
def _client_modules() -> list[Module]:
    """One module per compiled client, honest about built and last dispatch.

    ACTIVE only when the binary is present AND the last dispatch through it
    succeeded. A binary that exists but has never answered is AVAILABLE, not
    ACTIVE — presence is not proof it works.
    """
    from opendaisugi.bench.options import VERIFIER_CLIENTS, build_hint, client_is_built
    from opendaisugi.verifier_dispatch import last_dispatch

    record = last_dispatch()
    labels = {
        "rust": "rust",
        "go": "go (mvdan)",
        "typescript": "typescript",
        "lean": "lean (proven core)",
    }
    out: list[Module] = []
    for key, label in labels.items():
        spec = VERIFIER_CLIENTS[key]
        if not client_is_built(spec):
            out.append(Module(label, POSSIBLE, build_hint(spec)))
        elif record.get("client") == key and record.get("ok"):
            out.append(Module(label, ACTIVE, "built; last dispatch succeeded"))
        else:
            out.append(Module(label, AVAILABLE, "built; no successful dispatch yet"))
    return out
```

In `src/opendaisugi/tui_wiring.py`, delete the `"verifier"` entry from `_EFFECT_HINT` (line 42).
The stage is live, so the guard at line 135 (`if not is_live(st.key)`) no longer renders a note
for it.

- [ ] **Step 5: Update the four tests that asserted the old truth**

`tests/test_swap.py:54-61` — replace the last three assertions of
`test_effect_is_three_valued_and_covers_every_stage` with:

```python
    # shell/distill/verifier are live; stores is planned; gate is cfg.
    assert is_live("shell") and is_live("distill") and is_live("verifier")
    assert effect_of("stores") == "planned"
    assert effect_of("gate") == "cfg"
```

Leave `assert set(STAGE_EFFECT) == stage_keys` alone. The edits in this plan are additive to
`STAGE_EFFECT`; sub-project 03 adds a `floor` stage to both sides at once, so the equality holds.

`tests/test_swap.py:64-70` — replace `test_verifier_is_a_knob_but_not_live` with:

```python
def test_verifier_is_a_knob_and_now_live(tmp_path):
    # verifier_dispatch reads the choice per verify call, so no restart is needed.
    assert is_swappable("verifier")
    assert is_live("verifier")
    cfg = apply_swap("verifier", "go (mvdan)", config_path=_cfg_path(tmp_path))
    assert cfg.verifier_client == "go"
```

`tests/test_modules.py:27-33` — replace `test_verifier_is_single_impl_today_not_falsely_swappable`
with:

```python
def test_verifier_clients_are_only_active_after_a_successful_dispatch(tmp_path, monkeypatch):
    # A binary on disk is not proof it works, so presence alone is AVAILABLE.
    monkeypatch.setattr("opendaisugi.verifier_dispatch.last_dispatch", lambda root=None: {})
    verifier = next(s for s in detect_stages(tmp_path) if s.key == "verifier")
    active = [m.name for m in verifier.modules if m.state == ACTIVE]
    assert active == ["python (in-process)"]
```

`tests/test_modules.py:56-66` — in `test_map_marks_live_cfg_planned`, change the verifier branch:

```python
        if line.startswith("┌─ verifier"):
            assert "[live]" in line  # dispatched per call since the runtime swap landed
```

`tests/test_tui_honesty.py:196-205` — change the query in `test_wiring_keeps_its_effect_tags` from
`#effnote-verifier` to `#effnote-gate`, and the message to
`"cfg stages must still say why they are not live"`. The gate stays `cfg` permanently (the hook
file belongs to the harness), so it is the stable anchor for this assertion.

- [ ] **Step 6: Run the affected suites**

Run: `uv run --no-sync pytest tests/test_gate_dispatch.py tests/test_hot_swap.py tests/test_swap.py tests/test_modules.py tests/test_tui_honesty.py tests/test_gate.py -q`
Expected: PASS.

Run: `uv run --no-sync pytest -q`
Expected: the whole suite green. If `tests/test_dashboard.py` or `tests/test_cockpit.py` asserted
the verifier stage's old text, update the assertion to the new honest text; do not revert the tag.

- [ ] **Step 7: Commit**

```bash
git add src/opendaisugi/gate.py src/opendaisugi/swap.py src/opendaisugi/modules.py \
        src/opendaisugi/tui_wiring.py tests/test_gate_dispatch.py tests/test_hot_swap.py \
        tests/test_swap.py tests/test_modules.py tests/test_tui_honesty.py
git commit -m "gate: dispatch the configured verifier per call, and stop calling it planned

The verifier stage was tagged planned because nothing read the choice. It reads
it now, once per verify call, so the resident gate picks up a change with no
restart and the tag becomes true rather than aspirational. A client is ACTIVE in
the module map only after a dispatch through it actually succeeded: a binary on
disk is not proof it runs. Every failure path still denies, which the gate's own
fail-closed except and the new dispatch tests both pin."
```

---

### Task 13: The matcher swaps live — `_search.reload()` and `tend` re-embeds

`PathwayStore.find` already resolves the active identity per call, so a backend change takes hold
without a restart for *lookup*. What is missing is the public reload and the orphan problem: rows
embedded under the old identity are excluded from `find` forever, and `tend` distils a fresh
pathway beside them rather than re-embedding them.

**Files:**
- Modify: `src/opendaisugi/_search.py:353-363` (public `reload()`; `_reset_embedder_cache` becomes an alias)
- Modify: `src/opendaisugi/pathway_store.py:203` (add `reembed_stale` above `find`)
- Modify: `src/opendaisugi/distiller.py:355-380` (`tend` re-embeds stale rows first)
- Modify: `src/opendaisugi/swap.py:42` (`STAGE_EFFECT["matcher"] = LIVE`)
- Modify: `src/opendaisugi/tui_wiring.py:43` (drop the matcher hint)
- Modify: `tests/test_hot_swap.py` (add `prove_matcher_swap` and register it)
- Test: `tests/test_matcher_reembed.py`

**Interfaces:**
- Consumes: `LIVE_PROOFS` (Task 9).
- Produces:
  ```python
  # _search.py
  def reload() -> None
      """Drop cached embedders and the fallback-warned set; the next call re-resolves."""

  # pathway_store.py
  def reembed_stale(self, *, embed=None, model=None, version=None) -> int
      """Re-embed rows whose provenance is not the active identity. Returns the count."""
  ```

- [ ] **Step 1: Write the failing tests**

Create `tests/test_matcher_reembed.py`:

```python
"""Switching the embedder orphans every stored pathway until something re-embeds
it. `tend` does that now, including the legacy rows that carry no provenance."""

import json
import sqlite3

from opendaisugi.models import ActionPlan, Envelope, Permission
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _pathway(pid: str, task: str, vec: list[float], model: str, version: str) -> CompiledPathway:
    return CompiledPathway(
        id=pid,
        task_description=task,
        task_embedding=vec,
        embedding_model=model,
        embedding_model_version=version,
        envelope=Envelope(generated_by="test", task=task, permissions=Permission()),
        plan_template=ActionPlan(source="test", task=task, steps=[]),
        source_trace_ids=["t1"],
        distilled_at=0.0,
    )


def _rows(store: PathwayStore) -> list[dict]:
    con = sqlite3.connect(store._db_path)
    con.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in con.execute("SELECT * FROM pathways")]
    finally:
        con.close()


def test_reembed_restamps_a_stale_row_and_returns_the_count(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "old-model", "1"))
    n = store.reembed_stale(
        embed=lambda texts: [[1.0, 0.0] for _ in texts], model="new-model", version="3"
    )
    assert n == 1
    row = _rows(store)[0]
    assert row["embedding_model"] == "new-model"
    assert row["embedding_model_version"] == "3"
    assert json.loads(row["task_embedding_json"]) == [1.0, 0.0]


def test_a_current_row_is_left_alone(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "new-model", "3"))
    assert (
        store.reembed_stale(
            embed=lambda texts: [[9.0, 9.0] for _ in texts], model="new-model", version="3"
        )
        == 0
    )
    assert json.loads(_rows(store)[0]["task_embedding_json"]) == [0.0, 1.0]


def test_a_legacy_row_with_no_provenance_gets_stamped_and_still_matches(tmp_path):
    """Legacy rows are admitted as wildcards by find (pathway_store.py:258-263).
    After re-stamping they are concretely current, and find must still return them."""
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [1.0, 0.0], "", ""))
    assert (
        store.reembed_stale(
            embed=lambda texts: [[1.0, 0.0] for _ in texts], model="new-model", version="3"
        )
        == 1
    )
    row = _rows(store)[0]
    assert row["embedding_model"] == "new-model"

    store._embed_query = lambda task: [1.0, 0.0]
    import opendaisugi._search as search

    original = search.active_model_name
    search.active_model_name = lambda config=None: "new-model"
    try:
        import opendaisugi.distiller as distiller

        saved = distiller._EMBEDDING_MODEL_VERSION
        distiller._EMBEDDING_MODEL_VERSION = "3"
        try:
            match = store.find("add a test", threshold=0.5)
        finally:
            distiller._EMBEDDING_MODEL_VERSION = saved
    finally:
        search.active_model_name = original
    assert match is not None and match.pathway.id == "a"


def test_reembed_reports_zero_rather_than_crashing_when_the_embedder_is_missing(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "old-model", "1"))

    def _boom(texts):
        raise ImportError("no embedder here")

    assert store.reembed_stale(embed=_boom, model="new", version="3") == 0
    assert _rows(store)[0]["embedding_model"] == "old-model"


def test_an_identity_bump_orphans_the_store_until_tend_re_embeds(tmp_path, monkeypatch):
    """The identity string is the provenance stamp; ADR-0019 says to bump it when
    the feature scheme changes. Bumping it here reproduces the orphan case
    end to end: find stops matching, and reembed_stale brings the row back."""
    import opendaisugi
    from opendaisugi import _search
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("matcher_model: lexical\n")
    _search.reload()

    store = PathwayStore(tmp_path / "p.db")
    vec = [float(x) for x in _search._get_model().encode(["add a test"])[0]]
    store.put(_pathway("a", "add a test", vec, "lexical-hash-v1", _EMBEDDING_MODEL_VERSION))
    assert store.find("add a test") is not None

    monkeypatch.setattr(_search, "_LEXICAL_IDENTITY", "lexical-hash-v2")
    _search.reload()
    assert store.find("add a test") is None, "a stale row must not match across spaces"

    assert store.reembed_stale() == 1
    assert store.get("a").embedding_model == "lexical-hash-v2"
    assert store.find("add a test") is not None


def test_reload_drops_the_cached_embedder(tmp_path, monkeypatch):
    import opendaisugi
    from opendaisugi import _search

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("matcher_model: lexical\n")
    _search.reload()
    first = _search._get_model()
    assert _search._get_model() is first
    _search.reload()
    assert _search._get_model() is not first
```

Add to `tests/test_hot_swap.py`, above `LIVE_PROOFS`:

```python
def prove_matcher_swap(tmp: Path) -> None:
    """Rewrite matcher_model through save_config and watch matching change.

    Both halves are public: the config is written the way the swap menu writes
    it, and the behaviour is read off `PathwayStore.find`, which is what actually
    reuses a pathway. With `lexical` the stored row matches; with a matcher key
    that is not built, `_search.py`'s `active_model_name` raises
    `MatcherNotAvailable`, `find` degrades to no reuse, and the operator gets
    zero pathways — a real, config-driven behaviour change with no second model
    package and nothing private touched.
    """
    import opendaisugi
    from opendaisugi import _search
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION
    from opendaisugi.exceptions import MatcherNotAvailable
    from opendaisugi.models import ActionPlan, Envelope, Permission
    from opendaisugi.pathway import CompiledPathway
    from opendaisugi.pathway_store import PathwayStore

    data_dir = tmp / "matcher"
    _write_config(data_dir, matcher_model="lexical")
    original = opendaisugi.DEFAULT_DATA_DIR
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        _search.reload()
        assert _search.active_model_name() == "lexical-hash-v1"

        store = PathwayStore(data_dir / "pathways.db")
        vec = [float(x) for x in _search._get_model().encode(["add a test"])[0]]
        store.put(
            CompiledPathway(
                id="a",
                task_description="add a test",
                task_embedding=vec,
                embedding_model="lexical-hash-v1",
                embedding_model_version=_EMBEDDING_MODEL_VERSION,
                envelope=Envelope(generated_by="t", task="add a test", permissions=Permission()),
                plan_template=ActionPlan(source="t", task="add a test", steps=[]),
                source_trace_ids=["t1"],
                distilled_at=0.0,
            )
        )
        assert store.find("add a test") is not None

        # A matcher key with no shipped embedder. `not-a-matcher` is used rather
        # than `int8` so the proof holds whether or not spec 10 has landed.
        _write_config(data_dir, matcher_model="not-a-matcher")
        _search.reload()
        try:
            _search.active_model_name()
        except MatcherNotAvailable:
            pass
        else:
            raise AssertionError("the matcher choice is cached, not read per call")
        assert store.find("add a test") is None, "an unbuilt matcher must not still match"

        # And back again, in the same process.
        _write_config(data_dir, matcher_model="lexical")
        _search.reload()
        assert _search.active_model_name() == "lexical-hash-v1"
        assert store.find("add a test") is not None
    finally:
        _search.reload()
        opendaisugi.DEFAULT_DATA_DIR = original


def test_matcher_swap_is_live_without_restart(tmp_path):
    prove_matcher_swap(tmp_path)


def test_two_real_backends_swap_live_when_both_are_installed(tmp_path):
    """The same proof across two genuinely different embedders. Skips, with the
    reason, when only one is installed; the hermetic proof above is the one the
    honesty test calls."""
    import importlib.util

    import pytest

    if importlib.util.find_spec("model2vec") is None:
        pytest.skip("model2vec is not installed; potion cannot be compared to lexical")

    import opendaisugi
    from opendaisugi import _search

    data_dir = tmp_path / "pair"
    _write_config(data_dir, matcher_model="lexical")
    original = opendaisugi.DEFAULT_DATA_DIR
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        _search.reload()
        assert _search.active_model_name() == "lexical-hash-v1"
        _write_config(data_dir, matcher_model="potion")
        _search.reload()
        assert _search.active_model_name() != "lexical-hash-v1"
    finally:
        _search.reload()
        opendaisugi.DEFAULT_DATA_DIR = original
```

and extend the registry:

```python
LIVE_PROOFS: dict[str, Callable[[Path], None]] = {
    "backend": prove_backend_swap,
    "verifier": prove_verifier_swap,
    "matcher": prove_matcher_swap,
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run --no-sync pytest tests/test_matcher_reembed.py tests/test_hot_swap.py -q`
Expected: FAIL with `AttributeError: module 'opendaisugi._search' has no attribute 'reload'`

- [ ] **Step 3: Add the public reload**

In `src/opendaisugi/_search.py`, replace `_reset_embedder_cache` (lines 353-363):

```python
def reload() -> None:
    """Drop cached embedders and the fallback-warned set.

    The next call re-resolves the configured backend, loads the right embedder,
    and warns again if the fallback still applies. This is what makes the matcher
    stage live: a long-lived process (the resident gate, the cockpit, the
    gateway) picks up a config change without a restart. Stored pathways embedded
    under the old identity stay excluded from find until `tend` re-embeds them.
    """
    _embedder_cache.clear()
    _fallback_warned.clear()


# The old private name, kept because several test modules call it.
_reset_embedder_cache = reload
```

- [ ] **Step 4: Add `reembed_stale`**

In `src/opendaisugi/pathway_store.py`, insert above `find` (line 203):

```python
def reembed_stale(self, *, embed=None, model=None, version=None) -> int:
    """Re-embed every row whose provenance is not the active identity.

    Switching embedder changes the vector space, so `find` excludes rows
    stamped with another identity — correct, but it orphans them forever
    unless something re-embeds. `tend` calls this first, which is the second
    half of making the matcher stage live: lookup follows the config
    immediately, and the store catches up on the next tend.

    Legacy rows with no stamp at all are re-embedded too. They were admitted
    as wildcards, which quietly risked comparing across widths; a stamp makes
    them concretely current instead.

    Returns the number of rows rewritten. An unavailable embedder returns 0
    with a warning rather than raising: distillation degrades, it does not
    crash.
    """
    from opendaisugi.exceptions import MatcherNotAvailable

    if model is None or version is None:
        from opendaisugi._search import active_model_name
        from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

        try:
            model = model or active_model_name()
        except MatcherNotAvailable as exc:
            _log.warning("re-embed skipped: %s", exc)
            return 0
        version = version or _EMBEDDING_MODEL_VERSION
    if embed is None:

        def embed(texts):
            from opendaisugi._search import _get_model

            return _get_model().encode(list(texts), convert_to_numpy=True)

    rows = self._load_all_rows()
    stale = [
        r for r in rows if r["embedding_model"] != model or r["embedding_model_version"] != version
    ]
    if not stale:
        return 0
    try:
        vectors = embed([r["task_description"] for r in stale])
    except (ImportError, ModuleNotFoundError, MatcherNotAvailable) as exc:
        _log.warning(
            "re-embed skipped for %d stale pathway(s): %s. Run `daisugi tend` "
            "again once the embedder is available.",
            len(stale),
            exc,
        )
        return 0
    with self._connect() as con:
        for row, vec in zip(stale, vectors, strict=True):
            con.execute(
                "UPDATE pathways SET task_embedding_json = ?, embedding_model = ?, "
                "embedding_model_version = ? WHERE id = ?",
                (json.dumps([float(x) for x in vec]), model, version, row["id"]),
            )
    _log.info("re-embedded %d stale pathway(s) under %s/%s", len(stale), model, version)
    return len(stale)
```

- [ ] **Step 5: Call it from `tend`**

In `src/opendaisugi/distiller.py`, inside `tend` (line 356), immediately after
`warnings: list[str] = []`:

```python
# Re-embed rows stamped with another identity before clustering. Without
# this, switching embedder orphans the whole store: find excludes the old
# rows and tend distils fresh pathways beside them forever.
reembedded = self.pathway_store.reembed_stale()
if reembedded:
    warnings.append(f"re-embedded {reembedded} pathway(s) under the current embedder.")
```

- [ ] **Step 6: Flip the tag**

In `src/opendaisugi/swap.py` line 42, change to
`"matcher": LIVE,  # _search.reload() + tend re-embeds; find re-resolves per call`.

In `src/opendaisugi/tui_wiring.py`, delete the `"matcher"` entry from `_EFFECT_HINT` (line 43).

- [ ] **Step 7: Run the tests**

Run: `uv run --no-sync pytest tests/test_matcher_reembed.py tests/test_hot_swap.py tests/test_swap.py tests/test_distiller_tend.py tests/test_matcher_backend.py tests/test_pathway_store.py -q`
Expected: PASS. If a distiller test asserted an exact `warnings` list, extend it for the
re-embed line rather than dropping the line.

Run: `uv run --no-sync pytest -q`
Expected: the whole suite green.

- [ ] **Step 8: Commit**

```bash
git add src/opendaisugi/_search.py src/opendaisugi/pathway_store.py \
        src/opendaisugi/distiller.py src/opendaisugi/swap.py src/opendaisugi/tui_wiring.py \
        tests/test_matcher_reembed.py tests/test_hot_swap.py
git commit -m "matcher: reload the embedder in place and let tend re-embed the orphans

Lookup already re-resolved the identity per call, but switching backends orphaned
every stored pathway: find excluded them for good reason and nothing ever brought
them back, so tend quietly distilled duplicates beside them. tend now re-embeds
stale rows first, including the legacy rows that carried no stamp at all, and the
embedder cache gains a public reload so a long-lived process picks up a config
change without a restart. The stage is tagged live because both halves are now
true, and the proof does not need a second model package installed."
```

---

### Task 14: The gateway reloads, and every tag is proven

The last two stages. The gateway re-reads its whole `Config` on mtime change, on SIGHUP, and on a
loopback-only `POST /_reload`, so `router.kind` (spec 09) and `gateway_local_model` (today) both
take effect on the next turn. The gate stays `cfg` forever and now says why. The honesty test
walks `STAGE_EFFECT` and refuses to accept a `live` tag that no proof backs.

**Files:**
- Modify: `src/opendaisugi/gateway_asgi.py:159-221` (`make_gateway_app` gains a config reloader and `/_reload`)
- Modify: `src/opendaisugi/gateway_asgi.py:366-418` (`serve_gateway` installs the SIGHUP handler)
- Modify: `src/opendaisugi/swap.py:35-53` (`router` LIVE; add `CFG_REASON` and `tag_for`)
- Modify: `src/opendaisugi/tui_wiring.py:36-45` (`_EFFECT_HINT` sources from `CFG_REASON`)
- Modify: `tests/test_hot_swap.py` (add the router, shell and distill proofs; add the honesty test)
- Modify: `src/opendaisugi/gateway_asgi.py:184-190` (the handler selects the Gateway per request)
- Test: `tests/test_gateway_reload.py`

**Interfaces:**
- Consumes: `LIVE_PROOFS` (Tasks 9, 12, 13).
- Produces:
  ```python
  # gateway_asgi.py
  class ConfigReloader:
      def __init__(self, config_path: Path, rebuild, *, min_interval_s: float = 2.0) -> None
      def current(self): ...            # the live Gateway, reloading when the file changed
      def reload(self) -> bool          # force a reload now; True when it rebuilt

  def make_gateway_app(gateway, *, upstream_base_url=..., openai_gateway=None,
                       openai_upstream_base_url=..., client=None,
                       reloader: "ConfigReloader | None" = None)
      """The Anthropic wire is served by `reloader.current() or gateway` per request,
      so a config change reaches the next turn. The OpenAI wire does not reload:
      its cheap model is a launch flag, not a config field."""

  # swap.py
  CFG_REASON: dict[str, str]            # stage -> why a choice there needs a restart or reinstall
  def tag_for(stage_key: str) -> str    # EFFECT_TAG line, plus the reason for a cfg stage
  ```

- [ ] **Step 1: Write the failing tests**

Create `tests/test_gateway_reload.py`:

```python
"""The gateway re-reads its config without a restart, and only loopback may ask
it to. A reload that anyone on the network can trigger is a control surface."""

import asyncio
import json
import signal
from pathlib import Path

import pytest
import yaml

from opendaisugi.gateway_asgi import ConfigReloader, make_gateway_app


def _write(path: Path, **fields) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    existing = yaml.safe_load(path.read_text()) if path.exists() else {}
    existing = existing or {}
    existing.update(fields)
    path.write_text(yaml.safe_dump(existing, sort_keys=True))


def test_reloader_rebuilds_when_the_file_changes(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    seen: list[str] = []

    def _rebuild(config):
        seen.append(config.gateway_local_model)
        return config.gateway_local_model

    r = ConfigReloader(path, _rebuild, min_interval_s=0.0)
    assert r.current() == "one"
    _write(path, gateway_local_model="two")
    assert r.current() == "two"
    assert seen == ["one", "two"]


def test_reloader_hands_the_rebuild_a_whole_fresh_config(tmp_path):
    """Structural, not behavioural: whatever spec 09 adds to Config arrives here
    with no new wiring, because the reloader passes the whole object."""
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one", verifier_client="rust", matcher_model="lexical")
    captured = {}

    def _rebuild(config):
        captured["config"] = config
        return config

    ConfigReloader(path, _rebuild, min_interval_s=0.0).current()
    config = captured["config"]
    assert config.gateway_local_model == "one"
    assert config.verifier_client == "rust"
    assert config.matcher_model == "lexical"


def test_reloader_throttles_stat_calls(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    calls = []
    r = ConfigReloader(path, lambda c: calls.append(1) or c, min_interval_s=3600.0)
    r.current()
    _write(path, gateway_local_model="two")
    r.current()
    assert len(calls) == 1, "a within-interval call must not re-read the file"
    assert r.reload() is True, "an explicit reload always re-reads"
    assert len(calls) == 2


def test_a_missing_or_broken_config_keeps_the_running_gateway(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    r = ConfigReloader(path, lambda c: c.gateway_local_model, min_interval_s=0.0)
    assert r.current() == "one"
    path.write_text("{ this: is: not: yaml\n")
    assert r.current() == "one", "a broken config must not take the gateway down"


def _call(app, scope, body=b""):
    sent = []

    async def receive():
        return {"type": "http.request", "body": body, "more_body": False}

    async def send(message):
        sent.append(message)

    asyncio.run(app(scope, receive, send))
    return sent


def _scope(path: str, client):
    return {"type": "http", "path": path, "method": "POST", "headers": [], "client": client}


def test_reload_endpoint_accepts_loopback(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    reloads = []
    r = ConfigReloader(
        path, lambda c: reloads.append(c.gateway_local_model) or c, min_interval_s=3600.0
    )
    app = make_gateway_app(object(), reloader=r)
    sent = _call(app, _scope("/_reload", ("127.0.0.1", 51234)))
    assert sent[0]["status"] == 200
    assert json.loads(sent[1]["body"])["reloaded"] is True


def test_reload_endpoint_refuses_a_remote_client(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    r = ConfigReloader(path, lambda c: c, min_interval_s=3600.0)
    app = make_gateway_app(object(), reloader=r)
    for client in (("10.0.0.4", 51234), ("192.168.1.9", 80), None):
        sent = _call(app, _scope("/_reload", client))
        assert sent[0]["status"] == 403, client


def test_reload_endpoint_is_absent_without_a_reloader(tmp_path):
    app = make_gateway_app(object())
    sent = _call(app, _scope("/_reload", ("127.0.0.1", 1)))
    assert sent[0]["status"] == 404


async def test_a_config_change_routes_the_next_request_differently(tmp_path):
    """The blocker case, end to end: no restart, no new app object. Change the
    config on disk, replay a turn through the SAME app, and the model that
    reaches the upstream is the new one."""
    import httpx

    from opendaisugi.config import load_config, save_config
    from opendaisugi.gateway_asgi import build_default_gateway

    path = tmp_path / "config.yaml"
    save_config(load_config(path).model_copy(update={"gateway_local_model": None}), path)

    def _rebuild(config):
        return build_default_gateway(
            data_dir=tmp_path, journalling=False, local_model=config.gateway_local_model
        )

    reloader = ConfigReloader(path, _rebuild, min_interval_s=0.0)
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content)["model"])
        return httpx.Response(200, json={"content": [], "usage": {}})

    app = make_gateway_app(
        build_default_gateway(data_dir=tmp_path, journalling=False),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        reloader=reloader,
    )
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 64,
        "messages": [{"role": "user", "content": "list the files in src"}],
    }

    async def _post():
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://gw") as c:
            return await c.post(
                "/v1/messages",
                content=json.dumps(body).encode(),
                headers={"content-type": "application/json"},
            )

    await _post()
    assert seen[-1] == "claude-haiku-4-5", "the easy turn should have gone to the cheap model"

    save_config(
        load_config(path).model_copy(update={"gateway_local_model": "qwen2.5-coder-7b"}), path
    )
    await _post()
    assert seen[-1] == "qwen2.5-coder-7b", "the handler is still serving the old gateway"


async def test_a_failed_rebuild_keeps_serving_the_original_gateway(tmp_path):
    """Fail-open on routing only: a broken config costs a saving, not a service."""
    import httpx

    from opendaisugi.gateway_asgi import build_default_gateway

    path = tmp_path / "config.yaml"
    path.write_text("{ not: valid: yaml\n")
    reloader = ConfigReloader(path, lambda c: None, min_interval_s=0.0)
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content)["model"])
        return httpx.Response(200, json={"content": [], "usage": {}})

    app = make_gateway_app(
        build_default_gateway(data_dir=tmp_path, journalling=False),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        reloader=reloader,
    )
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as c:
        resp = await c.post(
            "/v1/messages",
            content=json.dumps(
                {
                    "model": "claude-opus-4-8",
                    "max_tokens": 64,
                    "messages": [{"role": "user", "content": "list the files"}],
                }
            ).encode(),
            headers={"content-type": "application/json"},
        )
    assert resp.status_code == 200
    assert seen == ["claude-haiku-4-5"], "a failed reload must not stop metering or routing"


def test_reload_endpoint_refuses_a_non_post_method(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    r = ConfigReloader(path, lambda c: c, min_interval_s=3600.0)
    app = make_gateway_app(object(), reloader=r)
    scope = dict(_scope("/_reload", ("127.0.0.1", 1)), method="GET")
    sent = _call(app, scope)
    assert sent[0]["status"] == 405


def test_serve_installs_a_sighup_handler(tmp_path, monkeypatch):
    """uvicorn installs its own handlers, so 'we registered one that never fires'
    is the silent failure here. Record the registration itself."""
    registered = {}

    def _fake_signal(sig, handler):
        registered[sig] = handler

    monkeypatch.setattr(signal, "signal", _fake_signal)
    monkeypatch.setattr("uvicorn.run", lambda *a, **k: None)
    from opendaisugi.gateway_asgi import serve_gateway

    serve_gateway(data_dir=tmp_path, port=0)
    assert signal.SIGHUP in registered
    assert callable(registered[signal.SIGHUP])
```

Add to `tests/test_hot_swap.py`, above `LIVE_PROOFS`:

```python
def prove_shell_swap(tmp: Path) -> None:
    """Flip shell_allow_decomposition in config.yaml and the gate's verdict on a
    compound command changes. No restart.

    The whole path is public: `daisugi gate init` reads config.yaml and registers
    a starter envelope, `gate.load_envelope` reads it back, and
    `gate.evaluate_record` rules on `echo a && echo b`. With decomposition off
    the compound is refused outright; with it on, every head is checked against
    the starter allowlist and `echo` is in it.
    """
    from typer.testing import CliRunner

    from opendaisugi.cli import app
    from opendaisugi.gate import evaluate_record, load_envelope
    from opendaisugi.shell_decompose import parser_available

    assert parser_available(), (
        "this proof needs the bash grammar. It is in the dev extra "
        "(pyproject `dev = [... opendaisugi[shell] ...]`); install it with "
        "`uv sync --extra dev`. Without it the config value cannot change a "
        "verdict, and the shell stage would not be live."
    )

    data_dir = tmp / "shell"
    root = data_dir / "gate"
    runner = CliRunner()
    record = {"tool_name": "Bash", "step_type": "shell", "command": "echo a && echo b"}

    _write_config(data_dir, shell_allow_decomposition=False)
    assert (
        runner.invoke(
            app, ["gate", "init", "--root", str(root), "--workspace", str(tmp), "--force"]
        ).exit_code
        == 0
    )
    off = evaluate_record(record, load_envelope(None, root=root), mode="enforce")
    assert off.allow is False, "a compound command must be refused with decomposition off"

    _write_config(data_dir, shell_allow_decomposition=True)
    assert (
        runner.invoke(
            app, ["gate", "init", "--root", str(root), "--workspace", str(tmp), "--force"]
        ).exit_code
        == 0
    )
    on = evaluate_record(record, load_envelope(None, root=root), mode="enforce")
    assert on.allow is True, "the config change did not reach the verdict"


def prove_distill_swap(tmp: Path) -> None:
    """Flip auto_tend and the code that consults it behaves differently.

    The observation is `daisugi hook auto-tend`, the command cron and the
    detached spawn actually run — it prints "skipped" when consent is off and
    proceeds when it is on. That is the behaviour the flag exists to control.
    """
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    data_dir = tmp / "distill"
    runner = CliRunner()

    _write_config(data_dir, auto_tend=False)
    off = runner.invoke(app, ["hook", "auto-tend", "--data-dir", str(data_dir)])
    assert off.exit_code == 0
    assert "skipped: background distillation is off" in off.output

    _write_config(data_dir, auto_tend=True)
    on = runner.invoke(app, ["hook", "auto-tend", "--data-dir", str(data_dir)])
    assert on.exit_code == 0
    assert "background distillation is off" not in on.output, (
        "the consent gate did not follow the config"
    )


def prove_router_swap(tmp: Path) -> None:
    """Rewrite the gateway's config, and the SAME running app routes the next
    turn to the new model. No restart, no new app object.

    The behavioural half uses `gateway_local_model` because it exists today. The
    structural half (`test_reloader_hands_the_rebuild_a_whole_fresh_config`) is
    what makes spec 09's `router.kind` covered with no new wiring: the reloader
    hands the rebuild a whole fresh Config, not a list of named fields.
    """
    import asyncio
    import json

    import httpx

    from opendaisugi.gateway_asgi import ConfigReloader, build_default_gateway, make_gateway_app

    data_dir = tmp / "router"
    path = _write_config(data_dir, gateway_local_model=None)
    reloader = ConfigReloader(
        path,
        lambda config: build_default_gateway(
            data_dir=data_dir, journalling=False, local_model=config.gateway_local_model
        ),
        min_interval_s=0.0,
    )
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content)["model"])
        return httpx.Response(200, json={"content": [], "usage": {}})

    app = make_gateway_app(
        build_default_gateway(data_dir=data_dir, journalling=False),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        reloader=reloader,
    )
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 64,
        "messages": [{"role": "user", "content": "list the files in src"}],
    }

    async def _post() -> None:
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://gw") as c:
            await c.post(
                "/v1/messages",
                content=json.dumps(body).encode(),
                headers={"content-type": "application/json"},
            )

    asyncio.run(_post())
    assert seen[-1] == "claude-haiku-4-5"

    _write_config(data_dir, gateway_local_model="qwen2.5-coder-7b")
    asyncio.run(_post())
    assert seen[-1] == "qwen2.5-coder-7b", "the running app cached its gateway"
```

and replace the registry plus add the honesty test at the bottom of the file:

```python
LIVE_PROOFS: dict[str, Callable[[Path], None]] = {
    "shell": prove_shell_swap,
    "distill": prove_distill_swap,
    "backend": prove_backend_swap,
    "verifier": prove_verifier_swap,
    "matcher": prove_matcher_swap,
    "router": prove_router_swap,
}


def test_router_swap_is_live_without_restart(tmp_path):
    prove_router_swap(tmp_path)


def test_shell_swap_is_live_without_restart(tmp_path):
    prove_shell_swap(tmp_path)


def test_distill_swap_is_live_without_restart(tmp_path):
    prove_distill_swap(tmp_path)


def test_stage_effect_tags_match_reality(tmp_path):
    """Every live tag is backed by a proof that runs here, and every cfg tag says
    why. A tag nobody checks is the dishonest control master section 3.5 forbids."""
    from opendaisugi.swap import CFG_REASON, LIVE, PLANNED, STAGE_EFFECT

    for stage, effect in STAGE_EFFECT.items():
        if effect == LIVE:
            proof = LIVE_PROOFS.get(stage)
            assert proof is not None, (
                f"stage {stage!r} is tagged live with no proof. "
                f"Add one to LIVE_PROOFS in tests/test_hot_swap.py, or change the tag."
            )
            proof(tmp_path / f"proof-{stage}")
        elif effect == PLANNED:
            continue
        else:
            reason = CFG_REASON.get(stage, "")
            assert reason.strip(), f"cfg stage {stage!r} must say why it is not live"


def test_the_gate_stage_stays_cfg_and_says_the_hook_file_is_not_ours():
    from opendaisugi.swap import CFG_REASON, STAGE_EFFECT, tag_for

    assert STAGE_EFFECT["gate"] == "cfg"
    assert "harness" in CFG_REASON["gate"]
    assert "reinstall" in tag_for("gate")
```

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run --no-sync pytest tests/test_gateway_reload.py tests/test_hot_swap.py -q`
Expected: FAIL with `ImportError: cannot import name 'ConfigReloader'`

- [ ] **Step 3: Add the reloader and the endpoint**

In `src/opendaisugi/gateway_asgi.py`, add above `make_gateway_app` (line 159):

```python
class ConfigReloader:
    """Re-read config.yaml in place so a running gateway follows a config change.

    The check is one `stat` at the start of a request, throttled to
    `min_interval_s`, plus an explicit `reload()` for SIGHUP and `POST /_reload`.
    A thread would buy nothing here: the gateway only acts on config when a turn
    arrives, so checking then is the same behaviour with none of the lifetime
    problems.

    A config that will not load keeps the running one. Routing is an
    optimization, never policy, so a typo must cost a saving, not a service.
    """

    def __init__(self, config_path: Path, rebuild, *, min_interval_s: float = 2.0) -> None:
        self._path = Path(config_path)
        self._rebuild = rebuild
        self._min_interval = min_interval_s
        self._built = None
        self._mtime: float | None = None
        self._checked_at = 0.0

    def _load(self) -> bool:
        from opendaisugi.config import load_config

        try:
            mtime = self._path.stat().st_mtime if self._path.exists() else 0.0
        except OSError:
            mtime = 0.0
        if self._built is not None and mtime == self._mtime:
            return False
        try:
            built = self._rebuild(load_config(self._path))
        except Exception as exc:  # noqa: BLE001 — a bad config keeps the running one
            _log.warning("gateway config reload failed, keeping the running one: %s", exc)
            return False
        self._built, self._mtime = built, mtime
        return True

    def current(self):
        """What the gateway should use for this turn."""
        import time as _time

        now = _time.monotonic()
        if self._built is None or now - self._checked_at >= self._min_interval:
            self._checked_at = now
            self._load()
        return self._built

    def reload(self) -> bool:
        """Force a re-read now. True when it rebuilt."""
        import time as _time

        self._checked_at = _time.monotonic()
        self._mtime = None
        return self._load()


def _is_loopback(client) -> bool:
    """True only for a connection from this machine's loopback address.

    Absent or unparseable peer information is refused. A reload endpoint anyone
    on the network can hit is a control surface, so it fails closed.
    """
    if not client:
        return False
    host = str(client[0])
    return host in ("127.0.0.1", "::1", "localhost")
```

Change `make_gateway_app`'s signature (line 159) to take `reloader: "ConfigReloader | None" = None`
and add, as the first statement inside `async def app` after the `scope["type"]` guard:

```python
        if scope["path"] == "/_reload":
            if reloader is None:
                await _json_response(send, 404, {"error": "no reloader configured"})
                return
            if scope.get("method", "POST").upper() != "POST":
                await _json_response(send, 405, {"error": "reload accepts POST"})
                return
            if not _is_loopback(scope.get("client")):
                await _json_response(send, 403, {"error": "reload is loopback only"})
                return
            await _json_response(send, 200, {"reloaded": reloader.reload()})
            return
```

Then make the reloader actually govern the turn. This is the whole point: a reloader whose result
the handler discards is a control that changes nothing. Replace the line

```python
        active_gateway = openai_gateway if wire == "openai" else gateway
```

with

```python
        # The Anthropic wire is served by whatever the reloader currently holds,
        # so a config change reaches the NEXT turn with no restart. A reload that
        # produced nothing usable falls back to the gateway this app was built
        # with, never to "no gateway" (that would silently stop metering).
        if wire == "openai":
            active_gateway = openai_gateway
        else:
            active_gateway = gateway
            if reloader is not None:
                active_gateway = reloader.current() or gateway
```

`active_gateway` is already the name every downstream call uses (`_prepare`, `_dispatch_streaming`,
`_dispatch_buffered`), so nothing else in the handler changes. The OpenAI wire is deliberately not
reloaded: `serve_gateway` builds it from `openai_cheap_model`, which is a launch flag rather than a
config field. Say so in the docstring of `make_gateway_app` and in `daisugi gateway --help`.

and add this helper next to `_response_headers` (line 151):

```python
async def _json_response(send, status: int, body: dict) -> None:
    payload = json.dumps(body).encode()
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [(b"content-type", b"application/json")],
        }
    )
    await send({"type": "http.response.body", "body": payload, "more_body": False})
```

In `serve_gateway` (line 366), just before `uvicorn.run(...)`:

```python
config_path = resolved_dir / "config.yaml"
reloader = ConfigReloader(
    config_path,
    lambda config: build_default_gateway(
        data_dir=resolved_dir,
        cheap_model=cheap_model,
        capture_answers=capture_answers,
        # The launch flag still wins; the config file is the rung below it,
        # the same precedence resolve_backend uses for llm_backend.
        local_model=local_model or config.gateway_local_model,
    ),
)
app = make_gateway_app(
    gateway,
    upstream_base_url=upstream_base_url,
    openai_gateway=openai_gateway,
    openai_upstream_base_url=openai_upstream_base_url,
    reloader=reloader,
)


def _on_sighup(signum, frame):  # noqa: ARG001 — signal handler signature
    reloader.reload()


with contextlib.suppress(ValueError, AttributeError, OSError):
    # Not on the main thread, or no SIGHUP on this platform: the mtime check
    # and POST /_reload still work, so this is a convenience, not the mechanism.
    signal.signal(signal.SIGHUP, _on_sighup)

uvicorn.run(app, host=host, port=port)
```

Delete the old `app = make_gateway_app(...)` block that this replaces, and add
`import contextlib` and `import signal` to the module imports (line 24).

- [ ] **Step 4: Add `CFG_REASON` and `tag_for`, and flip the router**

In `src/opendaisugi/swap.py`, change line 43 to
`"router": LIVE,  # the gateway re-reads config on mtime, SIGHUP, and POST /_reload`, then replace
`EFFECT_TAG` (lines 49-53) with:

```python
# One short line per effect, shown as the stage's tag.
EFFECT_TAG: dict[str, str] = {
    LIVE: "live — takes effect now",
    CFG: "cfg — needs a restart/reinstall",
    PLANNED: "planned — not wired yet",
}

# Why a cfg stage cannot be live, per stage. The GUI shows this so the operator
# reads a reason rather than a shrug. Every cfg stage must have one; the honesty
# test in tests/test_hot_swap.py enforces that.
CFG_REASON: dict[str, str] = {
    "gate": "reinstall: the hook file belongs to the harness, not to us",
    "harness": "reinstall: run `daisugi install` inside your agent host",
    "envelope": "pick a backend first, then this choice has something to run",
}


def tag_for(stage_key: str) -> str:
    """The stage's tag line, with the reason appended for a cfg stage."""
    effect = STAGE_EFFECT.get(stage_key)
    if effect is None:
        return ""
    tag = EFFECT_TAG[effect]
    reason = CFG_REASON.get(stage_key, "")
    return f"{tag} ({reason})" if effect == CFG and reason else tag
```

In `src/opendaisugi/tui_wiring.py`, replace `_EFFECT_HINT` (lines 36-45) with:

```python
def _effect_hint(stage_key: str) -> str:
    """Why this stage is not live, from the one source of truth in swap.py."""
    from opendaisugi.swap import CFG_REASON

    return CFG_REASON.get(stage_key, "not live yet")
```

and change its two call sites (lines 62 and 136) from `_EFFECT_HINT.get(stage_key, 'not live yet')`
and `_EFFECT_HINT.get(st.key, 'not live yet')` to `_effect_hint(stage_key)` and
`_effect_hint(st.key)`.

- [ ] **Step 5: Run the tests**

Run: `uv run --no-sync pytest tests/test_gateway_reload.py tests/test_hot_swap.py -q`
Expected: PASS, 9 + 11 tests.

Run: `uv run --no-sync pytest tests/test_gateway_asgi.py tests/test_gateway.py tests/test_tui_honesty.py tests/test_swap.py tests/test_modules.py -q`
Expected: PASS.

Run: `uv run --no-sync pytest -q && uv run --no-sync ruff check .`
Expected: the whole suite green and `All checks passed!`

- [ ] **Step 6: Check the whole front door once**

Run: `time uv run --no-sync daisugi bench all`
Expected: seven tables (matcher, verifier, router, gate-path, loop, backend, pairs), each with a
`corpus:` line and a `reproduce:` line, in well under 30 s per bench.

Run: `uv run --no-sync daisugi modules | grep -E "verifier|matcher|router|backend"`
Expected: those four stage boxes now carry `[live]`; gate carries `[cfg]`.

- [ ] **Step 7: Commit**

```bash
git add src/opendaisugi/gateway_asgi.py src/opendaisugi/swap.py src/opendaisugi/tui_wiring.py \
        tests/test_gateway_reload.py tests/test_hot_swap.py
git commit -m "gateway: reload config in place, and prove every live tag

The gateway only acts on config when a turn arrives, so it checks the file then,
throttled, plus SIGHUP and a loopback-only POST /_reload for an immediate one. The
rebuild is handed a whole fresh Config rather than named fields, so whatever the
router work adds is picked up with no new wiring. The gate stays cfg permanently
and now carries the reason in one place both the map and the GUI read: the hook
file belongs to the harness. The honesty test walks the stage map and refuses a
live tag with no proof behind it, so a tag can no longer outrun the code."
```

---

## Self-review (revised after adversarial review round 1)

**What round 1 changed.** Four blockers and nine should-fix items were applied in place; each
entry in the corrections block above names the task and step that carries it. The four structural
changes: `verify_via` now always runs the oracle and returns the conjunction, so a Core-profile
client can tighten but never widen (Task 11); `STAGE_EFFECT["backend"]` is actually flipped
(Task 9 step 3); the gateway handler resolves its Gateway through the reloader on every request
rather than discarding the reload (Task 14 step 3); and every `LIVE_PROOFS` entry now writes
config.yaml through `load_config`/`save_config` and observes behaviour through a public entry
point — `resolve_backend`, `gate.evaluate_record`, `PathwayStore.find`, `daisugi gate init`,
`daisugi hook auto-tend`, and the ASGI app — with no private attribute mutation and no YAML
round-trip standing in for a behaviour.

**Spec coverage.** Every file and requirement in `spec-11-layer-management.md` maps to a task:
`bench/registry.py` (3), `layers/verifier.py` (4), `layers/matcher.py` + the script shim (5),
`layers/router.py` (6), `layers/gate_path.py` (7), `layers/loop.py` (8), `layers/backend.py` (9),
`pairs.py` (10), `table.py` (3), `bench/corpus/` (2), the `daisugi bench` CLI (3),
`swap.py` STAGE_EFFECT changes (9, 12, 13, 14), `_search.reload()` (13), `pathway_store` lazy
re-embed on tend (13), `gateway.py` reload on SIGHUP and loopback `POST /_reload` (14),
`verifier_dispatch.py` (11), `gate.py` using it (12), `tests/bench/…` (2-10),
`tests/test_hot_swap.py` (9, 12, 13, 14), `tests/test_verifier_dispatch.py` (11). The hot-swap
table's six rows are all covered: matcher (13), backend (9), router (14), verifier (12), loop
floor (8, replay contract; a pane is a process and spec 03 owns spawn), gate stays `cfg` with the
reason text asserted (14). The `--json` shape, the `reproduce:` line, the `corpus:` digest line,
the `absent` row rule, the sub-30-second budget, the determinism check, and
`test_stage_effect_tags_match_reality` each have named tests.

**Placeholder scan.** No "TBD", no "add error handling", no "similar to Task N". Every code step
carries the code. Two steps deliberately instruct a *check* rather than a guess (the `int8`
entry in Task 1 depends on whether spec 10 landed; the
shell-step class name in Task 2), and each names the exact command to resolve it and what to do
with the answer. The vacuity check is no longer one of them: `check_vacuity` returns one of the
three literals in `vacuity.Verdict`, and Task 9 compares to `"contradiction"` directly.

**Fail-closed audit (round 1).** Every failure path was re-walked. A dispatched client that fails
leaves the oracle alone (Task 11); a client that succeeds can only tighten (Task 11); a dispatch
that raises still denies and now names the client (Task 12); a hung client gets half the gate
budget so the oracle still fits inside the join (Task 12); an unbuilt matcher makes `find` return
no reuse rather than a wrong match (Task 13); a config that will not load keeps the running
gateway serving and metering (Task 14); and `/_reload` refuses a non-loopback peer, a non-POST
method, and an absent peer address.

**Type consistency.** `CorpusRef` is defined in Task 2 and consumed from Task 3 onward.
`Column`/`Row`/`Table` are defined in Task 3 and used in Tasks 4-10. `BenchOpts`/`BenchSpec` are
defined in Task 3 and consumed by every layer. `ClientSpec`/`GatePathSpec`/`MatcherSpec`/`LoopSpec`
are defined in Task 1 and used in Tasks 4-11. `battery_envelope` and `run_contract` are defined in
Task 7 and reused in Tasks 8 and 10. `verify_via`/`last_dispatch` are defined in Task 11 and used
in Task 12; `client_verdict` is added to `VerificationResult` in Task 11 step 3 before any task
reads it. `_decomposition_cases` and `index_of` are defined in Task 10 and used only there.
`LIVE_PROOFS` is created in Task 9 and extended in Tasks 12, 13 and 14; every proof function it
names is defined in the task that adds it, and the honesty test that calls them lands last (Task
14) so it can never pass with a proof still missing. `CFG_REASON`/`tag_for` are defined in Task
14, and the only test that reads them is added in Task 14.
