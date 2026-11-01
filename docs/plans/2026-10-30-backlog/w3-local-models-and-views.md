# Wave 3: Local Models and Views Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Local models learn the words of the envelope DSL and of pathway plans from the garden's own distilled pathways; a local model writes those words through a schema so its output has the right shape; the warden still proves the meaning; coppice shows what the garden has learned (the Learned panel) and shows the floor as an RTS (the `rts` view), on the terminal and in the browser.

**Architecture:** Four parts, built after the W2 rename, in the new names. Part A extends `daisugi lora` (dataset from pathways as well as the journal, a holdout, `eval` by warden verdicts and proofs, `promote`, which runs by itself after an eval that meets its thresholds unless the owner turns it off, `to-gguf` through the ML pack). Part B adds a schema module and sends a JSON schema to every endpoint that supports constrained output (local OpenAI-compatible endpoints first; Anthropic's API too if its current docs say it does); every schema-shaped answer still goes through the existing envelope checks and the verifier. Part C adds `daisugi gardener learned --json` and a coppice `floor.learned` verb that runs it, with a terminal panel and a floor-page panel. Part D adds a shipped view plugin `rts` (shared JavaScript, so both coppice ports carry it), with own art, keyboard first, and optional skin packs read from the user's own files through a token-guarded `/skins/` route.

**Tech Stack:** Python 3.12 (pydantic, sqlite3, http.server fakes), Go (coppice, the Go daisugi), Rust (coppice-rs, the Rust daisugi), plain ES-module JavaScript with Node 22 `node --test`, PEFT/TRL in the ML pack (box only), Ollama 0.40 and llama.cpp on the GPU box.

**Spec:** this plan; the owner's rulings quoted in "Rulings"; `docs/research/ports-ml-2026-10-q3-lora.md` (training stays Python in the pack; GGUF adapters for inference); `docs/research/tool-interface-doctrine.md` (the bimodal law, section 7 for the TUI); `docs/research/cockpit-sources-2026-08-27/factorio___dwarf_fortress__data_dense_li.md` (alerts by class, honest staleness, ghost state); the Ollama structured outputs page (`https://docs.ollama.com/capabilities/structured-outputs`, read 2026-10-30: `format` carries a JSON schema on `/api/chat`; the OpenAI-compatible API takes it through `response_format`; the page validates the answer client-side and does not promise schema-valid output); the llama.cpp grammars README (`https://github.com/ggml-org/llama.cpp/blob/master/grammars/README.md`, read 2026-10-30: `grammar` and `json_schema` body fields on completion endpoints, `response_format` with `type: "json_schema"` on `/chat/completions`; unsupported JSON schema features are skipped silently: `patternProperties`, `uniqueItems`, `contains`, `not`, `if/then/else`, nested and remote `$ref`, `minimum` only on integers, `pattern` must start with `^` and end with `$`, `additionalProperties` defaults to false).

## Global Constraints

- Builds happen after the W2 rename. Write the new names: **warden** (was gate), **router** (was gateway), **routing_mode** (the inner policy), **steward** (was foreman). Paths: `src/opendaisugi/warden.py`, `<data>/warden/`, `<data>/router/`, `harness/coppice/internal/steward/`, `harness/coppice-rs/src/steward/`, `clients/fixtures/warden`. The verb is `daisugi warden check`. The pane object is `warden`. The compactor directory is the steward's.
- The laptop is small. One heavy job at a time, under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G`, with `-j2` (`go test -p 1`, `CARGO_BUILD_JOBS=2`). A heavy job is the full pytest suite, a Go or cargo build, a compare run, the coppice case run, a Node suite over the web statics.
- `/tmp` is RAM. Scratch goes under `/mnt/tera/.tt/w3-lmv/`. Delete scratch (datasets, adapters, exports) when a task is done.
- Tests use no network and no real model. Loopback fakes only (`clients/garden_cases.py` `FakeServer` for the oracle's cases; `tests/` start their own `http.server` on 127.0.0.1). The tiny-model CPU smoke (Task 7) is opt-in by an environment variable and is skipped by default.
- Never `pkill -f`. Stop a process by its pid.
- No AI attribution in commits. Never commit `CLAUDE.md` or `docs/superpowers/`. Commit by explicit path.
- Prose in the STE register: short sentences, plain words, active voice, no em dashes. Never "seam", never "the layer". No changelog voice: docs say what is, not what changed.
- Every coppice run passes its own `--socket` and `--data-dir`. Never touch the owner's live coppice server, its data dir, `~/.opendaisugi`, or `~/.claude`. Trials use a scratch HOME.
- No Qwen or Chinese-lab model is a curated default (`model_catalog.json` stays as it is). Never block, warn about, refuse or label a model's ancestry; the user may bring any model.
- Full parity: a daisugi command ships in Python (the oracle), Go and Rust, with golden cases and a compare at 0 disagreements. A coppice part ships in Go and coppice-rs, with cases under `harness/coppice-rs/cases/` replayed by `clients/coppice_compare.py` at 0 disagreements. Shared JavaScript under `harness/coppice/internal/web/static/` and `harness/coppice/plugins/` is carried by both ports (coppice-rs `build.rs` embeds the same files).
- A port is never looser than the oracle. A port that cannot decide a case refuses with one line, exit 2, nothing changed; a refusal needs a ruling in `clients/ADJUDICATIONS.md`.
- Fail closed. A schema-valid answer is a shape, not a proof. Nothing in this plan skips `_validate_envelope`, `verify_plan` or the warden.
- Box-gated tasks (marked **[box]**) build and test against fakes here and are verified on the GPU box later. They never block a merge.
- ADR number: 0028 for "Shape and meaning: constrained output and the warden". If another Wave 3 plan takes 0028 first, use the next free number and say so in the ledger.

## Review Focus

1. A local endpoint that rejects an unknown `response_format` type with HTTP 400 (an older llama-server, LM Studio). Expected: one retry without the schema, the reply marked `constrained=False`, the envelope still checked; never a silent fallthrough to the frontier. Test in Task 9 (`test_schema_400_retries_once_without`).
2. A schema-valid envelope that is too wide (`shell=True`, no scopes, `stakes` lowered). Expected: the existing checks narrow or refuse it exactly as before; the verifier denies a plan outside it. Test in Task 9 (`test_shape_pass_does_not_skip_meaning`).
3. A dataset export on a journal whose traces hold real local paths. Expected: the JSONL stays under the data dir, never under the repo; the manifest holds counts and hashes only; the export refuses a path inside the repository checkout. Test in Task 1 (`test_export_refuses_repo_path`).
4. `floor.learned` when `daisugi` is missing, slow, or prints bad JSON. Expected: the panel shows one plain line (`The garden is not known. Check that daisugi runs, then open Learned again.`), the floor keeps running, the call ends within 10 s. Test in Task 14 (`TestLearnedDaisugiMissing`, `TestLearnedDaisugiSlow`).
5. A skin pack with a path that escapes `<data>/skins/` (`..`, a symlink, an absolute path) or a 40 MB sheet. Expected: 404 for the escape, 413 over 4 MiB, never a byte from outside the directory; the view keeps its own art. Test in Task 18 (`TestSkinsRouteNoEscape`, `TestSkinsRouteSizeCap`).

## What exists already (evidence, read 2026-10-30)

- `src/opendaisugi/lora/dataset.py`: `TrainingExample(task, envelope_json, trace_id)`, `to_alpaca`, `to_chat`, `iter_training_examples(journal, since, min_task_chars)`, `emit_jsonl(journal, output_path, format, since, min_task_chars, system_prompt) -> DatasetStats`. Source is the journal only (`Journal.list_successful_traces`); kind is task to envelope only. Tests: `tests/test_lora_dataset.py`.
- `src/opendaisugi/lora/train.py`: PEFT/TRL QLoRA trainer, heavy imports lazy, `--jsonl --base-model --output --format --qlora --lora-r --lora-alpha --lora-dropout --epochs --lr --batch-size --grad-accum --max-seq-len --seed --dry-run`; `resolve_base_model` reads `daisugi models use` then the catalog default. Tests: `tests/test_lora_train_script.py`, `tests/test_cli_lora.py`.
- `daisugi lora train` and `daisugi lora export` exist in all three languages (`cli.py:1518-1580`, `clients/go/internal/cli/packcmd.go`, `loraexport.go`, `clients/rust/src/cli/packcmd.rs`, `loraexport.rs`). `train` runs in the ML pack's worker (`src/opendaisugi/pack/worker.py`, jobs `selftest`, `train`, `vla-chunk`). Golden cases: `clients/pack_cases.py`, `clients/pack_compare.py`, `clients/pack_fake.py`, `clients/fixtures/pack`. Doc: `docs/how-to/ml-pack.md`.
- `src/opendaisugi/model_catalog.json`: quiet defaults `ibm-granite/granite-4.1-3b` (capable) and `ibm-granite/granite-4.0-1b` (weak), plus Ministral 3 3B, gemma-4-E2B-it, Llama 3.2 1B/3B. Each entry names a GGUF repo.
- `src/opendaisugi/llm_client.py:236` sends `response_format: {"type": "json_object"}` on the OpenAI wire when `json_object` is set; `json_mode_system(response_model)` puts the pydantic schema in the prompt. `tier1.py` HTTP provider parses the first JSON object and declines on any failure. The Go twin is `clients/go/internal/llm/http.go:210`. No code sends a schema.
- `src/opendaisugi/envelope.py:48` builds `_ENVELOPE_SCHEMA = Envelope.model_json_schema()` for the prompt.
- The garden: `pathway_store.py` (`list_all()`, `get(id)`, `stats()`, columns `structure_signature`, `parameters_json`, `hit_count`, `failure_count`, `last_activation_at`, `version`), `pathway.py` (`CompiledPathway`, `PathwayParameter`), `gardener/report.py` (`run_gardener`), `gardener/pruner.py` (`PruneConfig.max_idle_days=30.0`, `max_failure_ratio=0.5`, `min_activations_before_prune=5`), `gardener/outcomes.py` (`record_run_outcome`). `Receipt.model_id` (`models.py:770`) records which model ran an LLM step; nothing in `gardener/` reads it yet. `daisugi gardener status --json` prints `count` and per-pathway `id, hit_count, failure_count, last_activation_at`. The Go and Rust ports carry `gardener`, `tend`, `pathways` (`gardencmd.go`, `tendcmd.go`, `pathwayscmd.go` and the Rust twins); `clients/garden_cases.py` and `garden_compare.py` cover them.
- `model_sizer.py` and `routing.py` already name the three levels: mechanical steps (shell, file, network, mcp, skill), the cheap local rung, the frontier (`tier0-pathway`, `tier1-cheap`, `tier2-frontier`).
- coppice: `floor.machines` is the pattern for an operator-only read verb that runs `daisugi ... --json` in the steward's compactor directory (`harness/coppice/internal/server/machines.go`: `handleFloorMachines`, `RegisterMachinesCommands`, `MachinesRefusal`, `MachinesProfilesLine`; `harness/coppice-rs/src/server/machines.rs`: `handle_floor_machines`); the web panel is `internal/web/static/machines.js` mounted from `app.js:645`; the case is `harness/coppice-rs/cases/facts/machines.jsonl` with `#!env COPPICE_DAISUGI_BIN={FIXTURES}/web/bin/fake-daisugi`. The TUI word list is `tui/prompt.go:27 Verbs`; the `tree` word opens a terminal panel (`tui/run.go:2042`).
- coppice views are plugins (`harness/coppice/plugins/README.md`): a view is a page in a sandboxed frame that holds no token, gets panes, tasks, events and the selection posted in, and may post back open, sel, watch (at most `WATCH_MAX = 9`) and close. Shipped views: `tree`, `minimap`, `kanban`, `colony` (panes as foragers, drag to select), `shift-log`, `herdr-grid`, `inbox`. `plugins/embed.go` names the shipped directories; coppice-rs `build.rs` embeds the same list. The shared library is `plugins/_lib/view.js` (`connect`, `boot`, `stateColor`, `byNeed`, `age`, `selection`). JS tests run with `node --test "harness/coppice/internal/web/static/_tests/*.test.mjs"` and `node --test "harness/coppice/plugins/*/_tests/*.test.mjs"` (the CI web step passes `--test-reporter=tap`).
- `internal/web/static/garden.js` already exists: it is the **picture of the floor as a daisugi tree** (key `g`). It is not a view of what the garden learned. See ruling LV-1.
- Owner rulings on record: garden view 4b deprioritized and parked (2026-10-24; this plan reopens it as part of the 2026-10-30 list); game skins (2026-10-21): own art only, optional packs load from the user's own installed game, RTS floor first, views stay live and typeable on the one-altitude floor; tiles take input (never a view that sends the operator to another page to type); constrained output proves shape, the warden proves meaning; no automatic model switch for frontier models. **Owner rulings of 2026-10-30:** a trained local model that passes eval is promoted automatically (C7), and the schema goes to any endpoint that supports constrained output (C8).

## Rulings (provisional, in force for this plan)

- **LV-1 Names.** The new coppice panel is **Learned** (verb `floor.learned`, TUI word `learned`, panel id `learned`, header `Learned: what the garden knows`). The existing `garden.js` overlay keeps its name and its key `g`. Cost if wrong: one rename of a panel.
- **LV-2 Step levels.** A step's level is `code` for `shell`, `file_read`, `file_write`, `network`, `mcp`, `skill`, `joint_move`, `cartesian_move`, `gripper`, `simulation_reset`, `vla` (no model runs it); for `task` and `agentic` it is `small` when every source receipt's `model_id` for that step is local (prefix `ollama/`, `openai/`, or an id in `model_catalog.json`), `frontier` when every one is not local, `mixed` when both, and `unknown` when no receipt names a model. A pathway's level is the highest of its steps in the order `code < small < mixed < frontier`, with `unknown` shown as `unknown`. Honest over flattering: a missing receipt is `unknown`, never `code`.
- **LV-3 Pathway status words.** `candidate`: `activation_count < PruneConfig.min_activations_before_prune`; `kept`: at or over it and `failure_count / activation_count <= max_failure_ratio`; `at risk`: over the ratio or idle longer than `max_idle_days`. The pruner's numbers are the source; the panel never computes its own thresholds.
- **LV-4 Promotion is automatic for a local model that passes eval (owner, 2026-10-30, C7).** `daisugi lora eval` runs against a served model and writes its report. When the report meets the thresholds and the config key `lora_auto_promote` is true (the default), eval itself promotes: it writes `<data>/lora/promoted.json`, appends an `auto: true` line to `<data>/lora/promotions.jsonl`, appends one journal event `lora_promoted`, and prints one notice line: `promoted <name> automatically (eval <sha8>: shape 0.99, proof 0.93); turn off with lora_auto_promote: false; undo with daisugi lora demote`. Coppice shows the same line in the Learned panel's worker row. The switch it makes is narrow: the promoted model becomes the **local worker** (the local rung and the Learned panel's worker), and nothing else. It never edits the harness's model, never changes a frontier model, never calls `daisugi models use` for a frontier id, and never overrides the no-auto-model-switch rule, which stays as written for frontier models. Training and eval stay explicit commands: `tend`, `hook auto-tend`, `distill-repeats` and the router never call `lora train` or `lora eval` (they are heavy). With `lora_auto_promote: false`, eval prints the old next-step lines and writes nothing. `daisugi lora demote` restores the previous record from `promotions.jsonl`. Cost if wrong: a worse local worker runs until `demote`; the warden still proves every plan it writes.
- **LV-5 Shape is not meaning; the schema goes wherever it is supported (owner, 2026-10-30, C8).** A schema is sent to any endpoint whose API supports structured or constrained output: local OpenAI-wire endpoints (`ollama`, `llamafile`, `llama.cpp`, loopback, tailnet, RFC 1918), hosted OpenAI-wire endpoints that accept `response_format` json_schema, and Anthropic's API **if its current documentation says it supports structured output**. The builder of Task 9 checks that page on the day, cites the URL and the date in the ADR and in `schema_support.py`, and pins the request shape from it. If the page says no, the Anthropic wire gets no schema and the ADR says so. The table of supported endpoints is data (`schema.SUPPORT`), not a host-name guess. Every answer still goes through the same parse, `_validate_envelope` and verifier path as an unconstrained one: the verifier still checks every answer. A refusal (HTTP 400 naming the schema) retries once without it. ADR-0028 records this.
- **LV-6 GBNF stays upstream.** daisugi ships the JSON schema (`daisugi schema show envelope|plan`) and a lint against the llama.cpp unsupported list. It does not ship a GBNF converter. The how-to names llama.cpp's own converter for a user who wants a grammar file. Cost if wrong: a later task adds a converter.
- **LV-7 Learned is a floor-page panel, not a plugin view.** It reads `floor.learned` with the floor's token, as Machines does. A plugin view holds no token and cannot ask the server. The RTS view is a plugin view because it needs only what the floor posts.
- **LV-8 Skins.** A skin pack is a directory `<data>/skins/<id>/` with `skin.json` and one PNG sheet, made by the user from their own installed game. The repo ships no game asset and no extractor for a named game. The web server serves a pack only to the floor page (token guard); the floor page posts the decoded sheet to the view as an `ImageBitmap`. Without a pack, the view draws its own art.
- **LV-9 Evaluation metrics.** For an envelope example: `shape` (parses as `Envelope`), `proof` (the held-out verified plan verifies inside the generated envelope), `not_wider` (the generated envelope is subsumed by the reference envelope, `subsumption.py`). For a plan example: `shape` (parses as `ActionPlan`), `proof` (verifies inside the reference envelope), `same_shape` (its `plan_structure_signature` equals the reference's). Promotion defaults: `shape >= 0.98`, `proof >= 0.90`, `not_wider >= 0.95` (envelope kind); `same_shape` is reported, not gated.

## File structure

Part A (daisugi, three languages)
- Modify `src/opendaisugi/lora/dataset.py` (kinds, pathway source, holdout, manifest).
- Create `src/opendaisugi/lora/evaluate.py` (eval by verdicts and proofs), `src/opendaisugi/lora/promote.py` (the promotion record and the Modelfile text).
- Modify `src/opendaisugi/lora/train.py` (`--kind` passthrough for the chat template only), `src/opendaisugi/pack/worker.py` (job `to-gguf`), `src/opendaisugi/cli.py` (verbs `lora export|eval|promote|demote|status|to-gguf`).
- Go: `clients/go/internal/cli/loraexport.go` (flags), create `loraeval.go`, `lorapromote.go`; `internal/pack` (job `to-gguf`). Rust: `clients/rust/src/cli/loraexport.rs`, create `loraeval.rs`, `lorapromote.rs`.
- Create `clients/lora_cases.py`, `clients/lora_compare.py`, `clients/fixtures/lora/`.

Part B (daisugi, three languages)
- Create `src/opendaisugi/schema.py`; modify `llm_client.py`, `tier1.py`, `envelope.py`, `decomposer.py`, `distiller.py`, `config.py`, `cli.py` (`schema show`).
- Go: `clients/go/internal/llm/http.go`, create `internal/schema/` with the generated asset; Rust: `clients/rust/src/llm/`, `src/schema/`.
- Create `clients/gen_schema.py` (writes the asset into both ports), `docs/adr/0028-shape-and-meaning.md`.

Part C (daisugi + coppice)
- Create `src/opendaisugi/gardener/learned.py`; modify `cli.py` (`gardener learned`); Go `gardencmd.go`, Rust `gardencmd.rs`; `clients/garden_cases.py` new cases.
- coppice Go: create `internal/server/learned.go`, `internal/tui/learned.go`, `internal/web/static/learned.js`; modify `tui/prompt.go`, `tui/run.go`, `web/static/app.js`, `web/static/index.html`, `PROTOCOL.md`, `README.md`.
- coppice-rs: create `src/server/learned.rs`, `src/tui/learned.rs`; cases `cases/facts/learned.jsonl`, `cases/tui/learned.jsonl`.

Part D (coppice, shared JS + two servers)
- Create `harness/coppice/plugins/rts/{manifest.json,index.html,rts.css,rts.js,rts-view.js,art.js,_tests/rts.test.mjs,_tests/art.test.mjs}`; modify `plugins/embed.go`, `plugins/README.md`.
- Create `internal/web/skins.go` (+ `_test.go`), `internal/web/static/skins.js`; coppice-rs `src/web/skins.rs`; cases `cases/web/skins.jsonl`; `docs/how-to/skins.md`.

---

## Part A: worker training

### Task 1: Dataset kinds, the pathway source, the holdout and the manifest

**Files:**
- Modify: `src/opendaisugi/lora/dataset.py`
- Test: `tests/test_lora_dataset.py`

**Interfaces:**
- Consumes: `PathwayStore.list_all() -> list[CompiledPathway]`, `Journal.list_successful_traces`, `Journal.load_trace`, `Journal.receipts_for_run`.
- Produces:
  - `Kind = Literal["envelope", "plan"]`
  - `TrainingExample(task: str, envelope_json: str, trace_id: str, kind: Kind = "envelope", plan_json: str = "", source: str = "journal", source_id: str = "")`; `to_alpaca()` and `to_chat(system_prompt)` build by kind: envelope kind as today; plan kind has `instruction = PLAN_INSTRUCTION`, `input = task + "\n\nEnvelope:\n" + envelope_json`, `output = plan_json`. `PLAN_INSTRUCTION = "Write the action plan, as JSON, that does this task inside this envelope."`
  - `iter_pathway_examples(store: PathwayStore, *, kinds: tuple[Kind, ...] = ("envelope", "plan")) -> Iterator[TrainingExample]`: one example per pathway per kind; `source="pathway"`, `source_id=pathway.id`, `trace_id=""`; the plan is `pathway.plan_template`.
  - `split_holdout(examples: list[TrainingExample], *, holdout: float, seed: int) -> tuple[list[TrainingExample], list[TrainingExample]]`: deterministic by `sha256(f"{seed}:{source}:{source_id or trace_id}")`; an example is held out when the first 8 hex digits, read as an integer, modulo 10000, is below `holdout * 10000`.
  - `emit_jsonl(journal, output_path, *, format, since, min_task_chars, system_prompt, store: PathwayStore | None = None, kinds=("envelope",), sources=("journal",), holdout: float = 0.0, seed: int = 0, repo_root: Path | None = None) -> DatasetStats`; `DatasetStats` gains `held_out: int`, `by_kind: dict[str, int]`, `by_source: dict[str, int]`, `holdout_path: str`, `manifest_path: str`. It writes `<output>.holdout.jsonl` when `holdout > 0` and `<output>.manifest.json` with `{"v": 1, "written", "held_out", "by_kind", "by_source", "sha256": {"train": ..., "holdout": ...}, "kinds", "sources", "seed", "holdout"}`. No task text and no path enters the manifest.
  - `ExportRefused(ValueError)`: raised when `output_path.resolve()` is inside `repo_root` (default: the first parent of `output_path` that holds a `.git` entry, when any).

- [ ] **Step 1: Write the failing tests**

```python
def test_pathway_examples_both_kinds(tmp_path):
    store = PathwayStore(tmp_path / "pathways.db")
    store.upsert(
        _pathway("pw1", task="List the files in src")
    )  # the helper seeds one CompiledPathway
    ex = list(iter_pathway_examples(store))
    assert [e.kind for e in ex] == ["envelope", "plan"]
    assert ex[1].source == "pathway" and ex[1].source_id == "pw1"
    assert json.loads(ex[1].plan_json)["steps"][0]["type"] == "shell"
    row = ex[1].to_alpaca()
    assert row["instruction"] == PLAN_INSTRUCTION
    assert row["input"].startswith("List the files in src\n\nEnvelope:\n{")


def test_split_holdout_is_deterministic():
    ex = [TrainingExample(task=f"t{i}", envelope_json="{}", trace_id=f"tr{i}") for i in range(200)]
    a = split_holdout(ex, holdout=0.2, seed=7)
    b = split_holdout(ex, holdout=0.2, seed=7)
    assert [e.trace_id for e in a[1]] == [e.trace_id for e in b[1]]
    assert 25 <= len(a[1]) <= 55
    assert set(e.trace_id for e in a[0]).isdisjoint(e.trace_id for e in a[1])


def test_emit_writes_holdout_and_manifest(tmp_path, journal_with_traces, store_with_pathways):
    out = tmp_path / "ds" / "train.jsonl"
    stats = emit_jsonl(
        journal_with_traces,
        out,
        store=store_with_pathways,
        kinds=("envelope", "plan"),
        sources=("journal", "pathway"),
        holdout=0.25,
        seed=1,
    )
    man = json.loads((tmp_path / "ds" / "train.manifest.json").read_text())
    assert man["written"] == stats.written and man["held_out"] == stats.held_out
    assert set(man["by_source"]) == {"journal", "pathway"}
    assert "task" not in json.dumps(man) and str(tmp_path) not in json.dumps(man)
    assert (tmp_path / "ds" / "train.holdout.jsonl").exists()


def test_export_refuses_repo_path(tmp_path, journal_with_traces):
    (tmp_path / ".git").mkdir()
    with pytest.raises(ExportRefused):
        emit_jsonl(journal_with_traces, tmp_path / "data" / "train.jsonl")
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run pytest tests/test_lora_dataset.py -q -k "pathway_examples or split_holdout or holdout_and_manifest or refuses_repo"`
Expected: FAIL, `ImportError` on `iter_pathway_examples`.

- [ ] **Step 3: Implement the interfaces above in `src/opendaisugi/lora/dataset.py`**

Keep `iter_training_examples` and the old `emit_jsonl` keyword defaults so `tests/test_lora_dataset.py`'s existing tests pass unchanged.

- [ ] **Step 4: Run the whole file green**

Run: `uv run pytest tests/test_lora_dataset.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/lora/dataset.py tests/test_lora_dataset.py
git commit -m "lora dataset: plan examples from distilled pathways, a deterministic holdout and a manifest without task text"
```

### Task 2: `daisugi lora export` flags in three languages, with the lora golden suite

**Files:**
- Modify: `src/opendaisugi/cli.py` (`lora_export_cmd`, `cli.py:1552`), `clients/go/internal/cli/loraexport.go`, `clients/rust/src/cli/loraexport.rs`, `clients/gen_help.py` output (`helpall_gen.go`, `helpall_gen.rs`), `.github/workflows/clients.yml` (compare matrix)
- Create: `clients/lora_cases.py`, `clients/lora_compare.py`, `clients/fixtures/lora/`
- Test: `tests/test_cli_lora.py`

**Interfaces:**
- Consumes: Task 1.
- Produces: `daisugi lora export OUTPUT [--format alpaca|chat] [--since ISO] [--kind envelope|plan|both] [--source journal|pathways|both] [--holdout F] [--seed N] [--data-dir D]`. Default `--kind envelope --source journal --holdout 0`. Stdout, one line: `wrote N examples (envelope A, plan B; journal J, pathways P), held out H -> OUTPUT`. A refused export: exit 2, stderr `lora export: OUTPUT is inside a repository checkout; write the dataset under the data dir or /mnt scratch.`
- Go and Rust: the same flags, the same line, the same refusal. They read the pathway store with the port's own SQLite reader (`internal/pathways`, the Rust `pathways` mod) and write the same JSONL bytes (the oracle's `json.dumps` default separators; the ports already match `json.dumps` through `pyjson`).
- `clients/lora_cases.py`: modelled on `clients/garden_cases.py` (a file tree with `journal.db` and `pathways.db`, one command, the tree after, databases dumped whole, `FakeServer` for the commands of Tasks 4 and 5). Cases: `export-journal-envelope`, `export-both-both-holdout`, `export-refused-repo`, `export-empty`. `clients/lora_compare.py` is `garden_compare.py` with the fixture dir changed; `--oracle` reruns the oracle.

- [ ] **Step 1: Write the failing Python CLI test**

```python
def test_lora_export_both_kinds_prints_counts(tmp_path, seeded_data_dir, runner):
    out = tmp_path / "out" / "train.jsonl"
    r = runner.invoke(
        app,
        [
            "lora",
            "export",
            str(out),
            "--kind",
            "both",
            "--source",
            "both",
            "--holdout",
            "0.5",
            "--seed",
            "3",
            "--data-dir",
            str(seeded_data_dir),
        ],
    )
    assert r.exit_code == 0, r.output
    assert r.output.startswith("wrote ")
    assert "held out" in r.output and (tmp_path / "out" / "train.manifest.json").exists()


def test_lora_export_refuses_repo(tmp_path, seeded_data_dir, runner):
    (tmp_path / ".git").mkdir()
    r = runner.invoke(
        app, ["lora", "export", str(tmp_path / "x.jsonl"), "--data-dir", str(seeded_data_dir)]
    )
    assert r.exit_code == 2 and "inside a repository checkout" in r.output
```

- [ ] **Step 2: Run to verify it fails**

Run: `uv run pytest tests/test_cli_lora.py -q -k "both_kinds or refuses_repo"`
Expected: FAIL (`No such option: --kind`).

- [ ] **Step 3: Implement the Python command, then `clients/lora_cases.py` and `lora_compare.py`; record the fixtures**

Run: `uv run --no-sync python clients/lora_cases.py --out clients/fixtures/lora`
Expected: four case files and a manifest; `clients/fixture_paths.py` reports no local path.

- [ ] **Step 4: Port the flags to Go and Rust; regenerate help**

Run: `uv run --no-sync python clients/gen_help.py`, then one heavy job each:
`systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh`
`systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml` (after `. clients/rust/tools/z3-build-env.sh`)

- [ ] **Step 5: Compare at 0 disagreements for both ports**

Run: `uv run --no-sync python clients/lora_compare.py --binary clients/go/daisugi` and the same with `clients/rust/target/release/daisugi`.
Expected: `4 agree, 0 refused, 0 disagreements` for each.

- [ ] **Step 6: Add the `lora` entry to the compare matrix in `.github/workflows/clients.yml`, then commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_lora.py clients/lora_cases.py clients/lora_compare.py clients/fixtures/lora clients/go/internal/cli/loraexport.go clients/go/internal/cli/helpall_gen.go clients/rust/src/cli/loraexport.rs clients/rust/src/cli/helpall_gen.rs .github/workflows/clients.yml
git commit -m "lora export: kinds, sources, holdout and the repo refusal in three languages, with the lora golden suite"
```

### Task 3: Evaluation by warden verdicts and proofs

**Files:**
- Create: `src/opendaisugi/lora/evaluate.py`
- Test: `tests/test_lora_evaluate.py`

**Interfaces:**
- Consumes: `llm_client.complete` (Task 9 adds `json_schema`; this task passes `json_object=True` and switches to the schema in Task 9), `verify.verify_plan(plan, envelope)`, `subsumption` (the function the tree rule uses to prove a child envelope fits a parent; name it as `subsumption.py` exports it), `distiller.plan_structure_signature`, `Envelope.model_validate_json`, `ActionPlan.model_validate_json`.
- Produces:
  - `@dataclass class ExampleResult: kind: Kind; source_id: str; shape: bool; proof: bool | None; not_wider: bool | None; same_shape: bool | None; reason: str`
  - `@dataclass class EvalReport: n: int; by_kind: dict[str, dict[str, float]]; results: list[ExampleResult]; model: str; endpoint: str; constrained: bool; started_at: str; finished_at: str`; `to_dict()`; `rate(kind, metric) -> float` (`None` results are left out of the denominator and counted in `by_kind[kind]["undecided"]`).
  - `async def evaluate(holdout_path: Path, *, model: str, base_url: str, api_key: str | None, timeout_s: float = 60.0, limit: int | None = None, concurrency: int = 1) -> EvalReport`. One model call per example; the envelope kind asks for an envelope from the task; the plan kind asks for a plan from the task and the reference envelope (the same prompts the dataset's chat format uses). A reply that does not parse scores `shape=False` and every other metric `None`. A Z3 timeout or `unknown` in `verify_plan` scores `proof=False` (fail closed), `reason="verifier: <text>"`.
  - `write_report(report: EvalReport, path: Path) -> None` (JSON) and `format_report(report: EvalReport) -> str` (text: one line per kind, `envelope: n=40 shape 0.98 proof 0.90 not_wider 0.95 undecided 1`).

- [ ] **Step 1: Write the failing tests (a loopback fake answers from a dict keyed by the task text)**

```python
def test_envelope_kind_scores_shape_proof_not_wider(fake_model, holdout_file):
    fake_model.answers["List the files in src"] = REFERENCE_ENVELOPE_JSON  # exact copy
    rep = asyncio.run(
        evaluate(holdout_file, model="openai/fake", base_url=fake_model.url, api_key=None)
    )
    r = rep.results[0]
    assert (r.shape, r.proof, r.not_wider) == (True, True, True)


def test_wider_envelope_fails_not_wider(fake_model, holdout_file):
    fake_model.answers["List the files in src"] = WIDE_ENVELOPE_JSON  # shell everywhere, no scopes
    rep = asyncio.run(
        evaluate(holdout_file, model="openai/fake", base_url=fake_model.url, api_key=None)
    )
    assert rep.results[0].proof is True and rep.results[0].not_wider is False


def test_garbage_scores_shape_false_rest_undecided(fake_model, holdout_file):
    fake_model.answers["List the files in src"] = "not json"
    rep = asyncio.run(
        evaluate(holdout_file, model="openai/fake", base_url=fake_model.url, api_key=None)
    )
    r = rep.results[0]
    assert r.shape is False and r.proof is None and r.not_wider is None
    assert rep.by_kind["envelope"]["undecided"] == 1


def test_plan_kind_outside_envelope_fails_proof(fake_model, plan_holdout_file):
    fake_model.answers_by_prefix[PLAN_INSTRUCTION] = (
        PLAN_WRITING_ETC_JSON  # a file_write to /etc/hosts
    )
    rep = asyncio.run(
        evaluate(plan_holdout_file, model="openai/fake", base_url=fake_model.url, api_key=None)
    )
    assert rep.results[0].shape is True and rep.results[0].proof is False
    assert rep.results[0].reason.startswith("verifier:")
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/test_lora_evaluate.py -q`
Expected: FAIL, `ModuleNotFoundError: opendaisugi.lora.evaluate`.

- [ ] **Step 3: Implement `evaluate.py` per the interfaces**

The fake server for tests lives in `tests/test_lora_evaluate.py` (an `http.server` thread on 127.0.0.1 answering `/v1/chat/completions` from the last user message's text); it is not shared with `clients/`.

- [ ] **Step 4: Run green, then commit**

```bash
git add src/opendaisugi/lora/evaluate.py tests/test_lora_evaluate.py
git commit -m "lora evaluate: a held-out envelope or plan is scored by shape, the verifier's proof and subsumption, never by eye"
```

### Task 4: `daisugi lora eval` in three languages

**Files:**
- Modify: `src/opendaisugi/cli.py`, `clients/go/internal/cli/packcmd.go` (the `lora` group), `clients/rust/src/cli/packcmd.rs`, `clients/lora_cases.py`
- Create: `clients/go/internal/cli/loraeval.go`, `clients/rust/src/cli/loraeval.rs`
- Test: `tests/test_cli_lora.py`, `clients/fixtures/lora/`

**Interfaces:**
- Consumes: Task 3.
- Produces: `daisugi lora eval --holdout FILE --model M --endpoint URL [--api-key-env VAR] [--limit N] [--timeout S] [--report PATH] [--json]`. Text output is `format_report`; `--json` prints `to_dict()`; `--report` writes the JSON file. Exit 0 when every example got a reply (metrics may be low); exit 3 when the endpoint never answered (`lora eval: no answer from URL: <reason>`); exit 2 on a bad flag. The ports call the same verifier and subsumption code they already carry (`internal/verify`, the Rust `verify` mod) and the same HTTP client as `tiers setup --endpoint`.
- Cases (recorded with `FakeServer` answers keyed by request body SHA-256): `eval-envelope-all-good`, `eval-envelope-wider`, `eval-garbage`, `eval-plan-outside`, `eval-endpoint-down` (exit 3).

- [ ] **Step 1: Write the failing CLI test** (Python; the fake server fixture from Task 3)

```python
def test_lora_eval_json_rates(fake_model, holdout_file, runner):
    fake_model.answers["List the files in src"] = REFERENCE_ENVELOPE_JSON
    r = runner.invoke(
        app,
        [
            "lora",
            "eval",
            "--holdout",
            str(holdout_file),
            "--model",
            "openai/fake",
            "--endpoint",
            fake_model.url,
            "--json",
        ],
    )
    assert r.exit_code == 0, r.output
    d = json.loads(r.output)
    assert d["by_kind"]["envelope"]["proof"] == 1.0 and d["constrained"] in (True, False)


def test_lora_eval_endpoint_down_exits_3(holdout_file, runner, free_port):
    r = runner.invoke(
        app,
        [
            "lora",
            "eval",
            "--holdout",
            str(holdout_file),
            "--model",
            "openai/fake",
            "--endpoint",
            f"http://127.0.0.1:{free_port}/v1",
        ],
    )
    assert r.exit_code == 3 and "no answer from" in r.output
```

- [ ] **Step 2: Run to verify it fails; implement Python; record the five cases; port to Go and Rust; compare**

Run: `uv run --no-sync python clients/lora_cases.py --out clients/fixtures/lora --only eval-` then the two compares as in Task 2 Step 5.
Expected: `9 agree, 0 refused, 0 disagreements` per port.

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/cli.py tests/test_cli_lora.py clients/lora_cases.py clients/fixtures/lora clients/go/internal/cli/loraeval.go clients/go/internal/cli/packcmd.go clients/go/internal/cli/helpall_gen.go clients/rust/src/cli/loraeval.rs clients/rust/src/cli/packcmd.rs clients/rust/src/cli/helpall_gen.rs
git commit -m "lora eval: score an adapter's endpoint on the holdout by verdicts and proofs, in three languages"
```

### Task 5: `daisugi lora promote`, auto-promotion after a passing eval, `lora demote` and `lora status`

Owner ruling C7 (2026-10-30): a trained local model that passes eval is promoted automatically, with a notice, and a config switch turns that off. The ruling is about the local worker only. It does not touch the rule that frontier models never switch by themselves.

**Files:**
- Create: `src/opendaisugi/lora/promote.py`, `clients/go/internal/cli/lorapromote.go`, `clients/rust/src/cli/lorapromote.rs`
- Modify: `src/opendaisugi/lora/evaluate.py` (the end of `run_eval` calls `promote.maybe_auto_promote`), `src/opendaisugi/config.py` (key `lora_auto_promote: bool = True`), `src/opendaisugi/journal.py` (event `lora_promoted`), `src/opendaisugi/cli.py`, `clients/lora_cases.py`, and a new `tests/test_lora_training_not_automatic.py`
- Modify (ports): `clients/go/internal/cli/loraeval.go`, `clients/rust/src/cli/loraeval.rs` (call the same function after a report), the config structs of both ports (`lora_auto_promote`)
- Test: `tests/test_lora_promote.py`, `tests/test_cli_lora.py`

**Interfaces:**
- Consumes: Task 4's report JSON.
- Produces:
  - `Thresholds(shape: float = 0.98, proof: float = 0.90, not_wider: float = 0.95)`; `check(report: dict, t: Thresholds) -> list[str]` returns the metrics under threshold as `"envelope proof 0.85 < 0.90"` lines (an empty list means every threshold is met; the plan kind contributes `shape` and `proof` only).
  - `PromotedRecord(adapter: str, base_model: str, gguf: str | None, report_sha256: str, report_path: str, promoted_at: str, thresholds: dict, auto: bool, model: str, endpoint: str)`; `write_promoted(data_dir: Path, rec: PromotedRecord) -> Path` writes `<data>/lora/promoted.json` (0600) and appends one line to `<data>/lora/promotions.jsonl` (the line holds the previous record's sha so `demote` can restore it); `read_promoted(data_dir) -> PromotedRecord | None`.
  - `maybe_auto_promote(data_dir: Path, report: dict, cfg, *, journal, out: TextIO) -> PromotedRecord | None`: returns None and prints nothing when `cfg.lora_auto_promote` is false, when `check` is non-empty, when the report has no `by_kind`, `model` or `endpoint` (an old report), or when the model is not a local id (LV-2's local test: prefix `ollama/`, `openai/` on a local host, or a catalog id). Otherwise it writes the record with `auto=True`, appends the journal event `lora_promoted` (`{"model", "report_sha256", "rates"}`) and prints the one notice line of LV-4.
  - `modelfile_text(base_gguf_ref: str, adapter_gguf: Path, name: str) -> str` returns `FROM <base_gguf_ref>\nADAPTER <adapter_gguf>\n`; `next_steps_text(rec, name) -> str` returns the lines `ollama create <name> -f <path>/Modelfile` and `daisugi tiers setup --endpoint <url> --model <name>`. (`daisugi models use <name>` is printed only for a model the owner chose to use as the harness's model; promotion does not need it.)
  - CLI: `daisugi lora promote --adapter DIR --report REPORT.json [--gguf FILE] [--min-shape F --min-proof F --min-not-wider F] [--name NAME] [--data-dir D]`: the manual path, for an eval that ran with auto-promote off or for a different threshold. It refuses with exit 2 and the under-threshold lines when `check` is non-empty, or when the report's `constrained`/`endpoint` fields are missing, or when `--report` was not produced by `lora eval`. On success it writes the record (`auto=False`), writes `<adapter>/Modelfile` when `--gguf` is given, prints `promoted <adapter> (report <sha8>)` and the next steps. `--force` does not exist.
  - `daisugi lora demote [--data-dir D]`: restores the previous record named in the last `promotions.jsonl` line, or removes `promoted.json` when there was none; prints `demoted <name>; the local worker is <previous or none>`; exit 0. A `lora_demoted` journal event.
  - `daisugi lora status [--json]`: the promoted record (with `auto` and when), the last eval report's rates, `lora_auto_promote` on or off, and `no adapter is promoted` when none.
  - The training-not-automatic test: `tests/test_lora_training_not_automatic.py` asserts that no module among `distiller.py`, `gardener/*.py`, `router*.py`, `orchestrator.py`, `cli.py`'s `tend`, `hook auto-tend`, `distill-repeats` code paths imports `opendaisugi.lora.train` or `opendaisugi.lora.evaluate` (static `ast` walk over `src/opendaisugi`, allowlist `cli.py` functions named `lora_*`). Only `lora eval` reaches `promote`, and only through `maybe_auto_promote`.

- [ ] **Step 1: Write the failing tests**

```python
def test_check_names_each_metric_under_threshold():
    rep = {"by_kind": {"envelope": {"shape": 1.0, "proof": 0.85, "not_wider": 0.99}}}
    assert check(rep, Thresholds()) == ["envelope proof 0.85 < 0.90"]


def test_a_passing_eval_promotes_by_itself_and_says_so(tmp_path, report_good, cfg_default, journal):
    out = io.StringIO()
    rec = maybe_auto_promote(tmp_path, report_good, cfg_default, journal=journal, out=out)
    assert rec.auto is True and read_promoted(tmp_path).model == report_good["model"]
    assert out.getvalue().startswith("promoted ") and "lora_auto_promote: false" in out.getvalue()
    assert journal.events("lora_promoted")


def test_the_switch_turns_auto_promote_off(tmp_path, report_good, cfg_default, journal):
    cfg_default.lora_auto_promote = False
    out = io.StringIO()
    assert maybe_auto_promote(tmp_path, report_good, cfg_default, journal=journal, out=out) is None
    assert out.getvalue() == "" and read_promoted(tmp_path) is None


def test_a_failing_eval_never_promotes(tmp_path, report_low, cfg_default, journal):
    assert (
        maybe_auto_promote(tmp_path, report_low, cfg_default, journal=journal, out=io.StringIO())
        is None
    )
    assert not (tmp_path / "lora" / "promoted.json").exists()


def test_a_frontier_model_is_never_promoted(tmp_path, report_good, cfg_default, journal):
    report_good["model"] = "anthropic/claude-x"
    assert (
        maybe_auto_promote(tmp_path, report_good, cfg_default, journal=journal, out=io.StringIO())
        is None
    )


def test_demote_restores_the_previous_record(
    tmp_path, report_good, report_good_2, cfg_default, journal
):
    maybe_auto_promote(tmp_path, report_good, cfg_default, journal=journal, out=io.StringIO())
    maybe_auto_promote(tmp_path, report_good_2, cfg_default, journal=journal, out=io.StringIO())
    r = runner.invoke(app, ["lora", "demote", "--data-dir", str(tmp_path)])
    assert r.exit_code == 0 and read_promoted(tmp_path).model == report_good["model"]


def test_promote_refuses_under_threshold(tmp_path, runner, report_low):
    r = runner.invoke(
        app,
        [
            "lora",
            "promote",
            "--adapter",
            str(tmp_path / "ad"),
            "--report",
            str(report_low),
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert r.exit_code == 2 and "proof 0.85 < 0.90" in r.output
    assert not (tmp_path / "lora" / "promoted.json").exists()


def test_promote_writes_record_and_modelfile(tmp_path, runner, report_good):
    ad = tmp_path / "ad"
    ad.mkdir()
    gg = ad / "adapter.gguf"
    gg.write_bytes(b"x")
    r = runner.invoke(
        app,
        [
            "lora",
            "promote",
            "--adapter",
            str(ad),
            "--report",
            str(report_good),
            "--gguf",
            str(gg),
            "--name",
            "robin",
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert r.exit_code == 0, r.output
    assert (ad / "Modelfile").read_text() == f"FROM ibm-granite/granite-4.0-1b-GGUF\nADAPTER {gg}\n"
    assert oct((tmp_path / "lora" / "promoted.json").stat().st_mode)[-3:] == "600"


def test_nothing_automatic_calls_training_or_eval():
    offenders = modules_importing({"opendaisugi.lora.evaluate", "opendaisugi.lora.train"})
    assert offenders == set()
```

- [ ] **Step 2: Run to verify they fail; implement; record cases `promote-refused`, `promote-ok`, `eval-auto-promotes`, `eval-auto-off`, `eval-fail-no-promote`, `demote`, `status-none`, `status-promoted`; port to Go and Rust; compare at 0 disagreements**

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/lora src/opendaisugi/config.py src/opendaisugi/journal.py src/opendaisugi/cli.py tests/test_lora_promote.py tests/test_lora_training_not_automatic.py tests/test_cli_lora.py clients/lora_cases.py clients/fixtures/lora clients/go/internal/cli/lorapromote.go clients/go/internal/cli/loraeval.go clients/rust/src/cli/lorapromote.rs clients/rust/src/cli/loraeval.rs clients/go/internal/cli/helpall_gen.go clients/rust/src/cli/helpall_gen.rs
git commit -m "lora: a local model that passes eval is promoted as the local worker, with a notice and an off switch (owner ruling C7)

Training and eval stay explicit. Frontier models never switch by themselves."
```

### Task 6: `lora to-gguf` through the ML pack

**Files:**
- Modify: `src/opendaisugi/pack/worker.py` (job `to-gguf`), `src/opendaisugi/pack/catalog.py` and the pack's requirements (add the `gguf` wheel to the train pack, hash-pinned, and vendor llama.cpp's `convert_lora_to_gguf.py` plus the `gguf-py` files it imports into the pack's `scripts/`, with their MIT notice in `NOTICE`), `src/opendaisugi/cli.py`, `clients/go/internal/cli/packcmd.go`, `clients/rust/src/cli/packcmd.rs`, `clients/pack_fake.py` (the fake worker answers `to-gguf`), `clients/pack_cases.py`, `docs/how-to/ml-pack.md`, `docs/reference/dependencies.md`
- Test: `tests/test_pack.py`

**Interfaces:**
- Produces: `daisugi lora to-gguf --adapter DIR --base MODEL_DIR_OR_ID --out FILE [--outtype f16|q8_0]`: runs pack job `to-gguf` with argv `["--adapter", DIR, "--base", BASE, "--out", FILE, "--outtype", T]`; the worker runs `convert_lora_to_gguf.py <adapter> --base <base> --outfile <out> --outtype <t>` under `sys.executable -I`. Without the train pack: exit 2, `lora to-gguf needs the train pack: daisugi pack install train`. Output one line: `wrote FILE (N bytes)`.
- Ask the owner before adding `gguf` as a pack dependency (the rule in AGENTS.md). Record the answer in the ledger. If the answer is no, `to-gguf` prints the converter command for the user to run in their own llama.cpp checkout and exits 2 with `lora to-gguf: the converter is not in the pack; run: python convert_lora_to_gguf.py ...`.

- [ ] **Step 1: Write the failing test** (`tests/test_pack.py`: the fake worker records the job name and argv; `to-gguf` without the pack exits 2 with the install hint)

- [ ] **Step 2: Implement in Python and the worker; extend `pack_fake.py`; record `pack` cases `lora-to-gguf-ok`, `lora-to-gguf-no-pack`; port the verb to Go and Rust; `pack_compare` at 0 disagreements**

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/pack src/opendaisugi/cli.py tests/test_pack.py clients/pack_fake.py clients/pack_cases.py clients/fixtures/pack clients/go/internal/cli/packcmd.go clients/go/internal/pack clients/rust/src/cli/packcmd.rs clients/rust/src/pack docs/how-to/ml-pack.md docs/reference/dependencies.md NOTICE
git commit -m "lora to-gguf: the adapter becomes a GGUF adapter through the train pack, so Ollama and llama-server can load it"
```

### Task 6b: The Stitch pack, built from the MIT CLI source (optional)

Owner ruling A14 (2026-10-30): daisugi ships Stitch as an optional pack. The pack builds the Stitch CLI from its MIT-licensed source. It never uses the PyPI bindings (`stitch_core`, no license) and nothing needs it: every daisugi command works without it. The dialect miner of `design-dialects.md` (DI-6) reads it only when it is installed.

**Files:**
- Modify: `packs/catalog.json` (a pack entry `stitch` of kind `source-build`: `repo` `https://github.com/mlb2251/stitch`, `commit` the full 40-hex SHA of the commit the 2026-09 test used (the short form is `350804b`; the builder resolves it from the repo and records it), `source_url` the GitHub tarball for that commit, `source_sha256`, `source_size`, `license` `MIT`, `build` `["cargo", "build", "--release", "--locked", "--bin", "compress", "--bin", "rewrite"]` (the builder reads the repo's `Cargo.toml` and lists the real bin names), `check` the two built binary names)
- Modify: `src/opendaisugi/pack/manage.py` (`_install_steps` branches on `kind`: `source-build` fetches the tarball, checks `source_sha256`, unpacks it into `<pack dir>/src`, runs the build under `-j2` with `CARGO_BUILD_JOBS=2` and `CARGO_TARGET_DIR=<pack dir>/target`, copies the binaries to `<pack dir>/bin`, writes the manifest with the commit, the sha256 of each binary and the cargo version; `run` for a `source-build` pack execs `<pack dir>/bin/<job>` with the args), `src/opendaisugi/pack/catalog.py` (read `kind`), `src/opendaisugi/cli.py` (no new verb: `pack install|status|remove|run stitch ...` work as for the other packs), `clients/go/internal/pack`, `clients/rust/src/pack` (same branch), `clients/pack_fake.py` (a fake source tarball with a two-line Rust crate is too heavy: the fake catalog uses a shell-script "build" that writes two small scripts into `bin/`, so the golden cases need no cargo), `clients/pack_cases.py`, `docs/how-to/ml-pack.md` (a section "The Stitch pack"), `docs/reference/dependencies.md` (one row: Stitch, MIT, optional pack, built from source, not a Python dependency, not in any lock file), `NOTICE` (Stitch's MIT notice, copied into the pack dir on install, not into the repo tree)
- Test: `tests/test_pack.py`

**Interfaces:**
- Produces: `daisugi pack install stitch`: fetch, hash check, build, self-test (each binary run with `--help` exits 0). It needs `cargo` on PATH; without it: exit 2 and `pack install stitch needs cargo (rustup.rs); nothing was changed`. A source tarball whose hash does not match its pin stops the install, names the file and leaves no pack behind (the rule of the other packs).
- Produces: the build runs under the box rules: one heavy job, `-j2`, and the caller wraps it in the memory cap (the how-to says `systemd-run --user --scope -p MemoryMax=5G daisugi pack install stitch`; the install itself prints that line when it finds no cgroup limit).
- Produces: `daisugi pack run stitch compress -- ARGS` and `... rewrite -- ARGS` exec the built binaries. The dialect miner calls them the same way and falls back to its own itemset miner, with one printed line, when the pack is absent. Nothing else changes when the pack is absent.
- Produces: the pack manifest lists the license (`MIT`) and the commit, so `pack status stitch` shows both.
- Rule: no network at run time; only `install` fetches.

- [ ] **Step 1: Write the failing tests**

```python
def test_stitch_pack_is_listed_optional_and_source_built(ctx):
    names = {p["name"]: p for p in catalog.load(FAKE)["packs"]}
    assert names["stitch"]["kind"] == "source-build" and names["stitch"]["license"] == "MIT"


def test_stitch_install_checks_the_source_hash(ctx, fake_source):
    fake_source.corrupt()
    assert manage.install(ctx, "stitch", None) == 1
    assert not (ctx.data_dir / "packs" / "stitch").exists()


def test_stitch_install_without_cargo_exits_2_and_changes_nothing(ctx, monkeypatch):
    monkeypatch.setenv("PATH", "")
    assert manage.install(ctx, "stitch", None) == 2
    assert not (ctx.data_dir / "packs" / "stitch").exists()


def test_stitch_run_execs_the_built_binary(ctx, built_fake_stitch):
    assert manage.run(ctx, "stitch", "compress", ["--", "x"]) == 0
    assert built_fake_stitch.calls == [["x"]]
```

Run: `uv run --no-sync pytest -q tests/test_pack.py -k stitch`
Expected: FAIL (no `stitch` entry, no `source-build` kind).

- [ ] **Step 2: Implement in Python; extend `pack_fake.py` and the `pack` cases (`stitch-install-ok`, `stitch-bad-hash`, `stitch-no-cargo`, `stitch-run-compress`, `stitch-remove`); port to Go and Rust; `pack_compare` at 0 disagreements**

- [ ] **Step 3: Box check [box]**

On the box, once: `systemd-run --user --scope -p MemoryMax=5G daisugi pack install stitch`, then `daisugi pack run stitch compress -- --help`. Expected: exit 0 for both. Record the build time and the binary sizes in the ledger.

- [ ] **Step 4: Commit**

```bash
git add packs/catalog.json src/opendaisugi/pack tests/test_pack.py clients/pack_fake.py clients/pack_cases.py clients/fixtures/pack clients/go/internal/pack clients/rust/src/pack docs/how-to/ml-pack.md docs/reference/dependencies.md
git commit -m "pack: an optional Stitch pack, built from the MIT CLI source and hash-pinned

Owner ruling A14 (2026-10-30). Not the PyPI bindings (no license). Not
required: the dialect miner falls back to itemsets when it is absent."
```

### Task 7: The tiny-model CPU smoke, opt-in **[box verifies the real run]**

**Files:**
- Create: `tests/test_lora_tiny_smoke.py`, `tests/fixtures/lora/tiny_config.json`
- Modify: `src/opendaisugi/lora/train.py` (`--kind` is read for the chat template's system line only: `envelope` keeps the present system prompt; `plan` uses `PLAN_INSTRUCTION`)

**Interfaces:**
- The smoke runs only when `DAISUGI_LORA_TINY=1` and `transformers`, `peft`, `trl`, `torch` import; otherwise `pytest.skip`. It builds a random-weight model from `tiny_config.json` (a LlamaConfig: `hidden_size 64, num_hidden_layers 2, num_attention_heads 2, intermediate_size 128, vocab_size 1024`) with `AutoModelForCausalLM.from_config`, saves it with a byte-level tokenizer built in the test to `tmp_path/base`, exports 12 examples from a seeded journal and store (Task 1), trains one epoch with `--epochs 1 --batch-size 2 --max-seq-len 128 --lora-r 4`, and asserts the adapter directory holds `adapter_config.json` and `adapter_model.safetensors`. No download, no network. Wall time under 3 minutes on 4 cores; the test fails over 10 minutes.

- [ ] **Step 1: Write the test; run it skipped by default**

Run: `uv run pytest tests/test_lora_tiny_smoke.py -q`
Expected: `1 skipped`.

- [ ] **Step 2: Run it for real once, under the cap, as the one heavy job**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env DAISUGI_LORA_TINY=1 CUDA_VISIBLE_DEVICES= uv run --extra lora pytest tests/test_lora_tiny_smoke.py -q`
Expected: PASS; record the wall time in the ledger.

- [ ] **Step 3: Commit**

```bash
git add tests/test_lora_tiny_smoke.py tests/fixtures/lora/tiny_config.json src/opendaisugi/lora/train.py
git commit -m "lora: an opt-in CPU smoke trains one epoch on a random tiny model from the exported dataset; no download, no network"
```

### Task 8: The box run and the how-to **[box]**

**Files:**
- Modify: `docs/how-to/ml-pack.md` (new section "Worker training: from the garden to an adapter"), `docs/roadmap.md` (the "Fine-tuning on distilled pathways" closed door opens when the measured corpus exists; write the measured numbers from this task or write that they are not measured yet)

- [ ] **Step 1: On the box, under its own scratch HOME, export the owner's dataset from a copy of the journal and pathway store** (`daisugi lora export /var/tmp/od/lora/train.jsonl --kind both --source both --holdout 0.2 --seed 1 --format chat`). Record `written`, `held_out`, `by_kind`.
- [ ] **Step 2: Train `ibm-granite/granite-4.0-1b` with `--qlora --epochs 2 --lora-r 16` through `daisugi lora train`; record wall time and peak VRAM (`nvidia-smi --query-gpu=memory.used --format=csv -l 5`).**
- [ ] **Step 3: `daisugi lora to-gguf`; `ollama create robin -f Modelfile`; `daisugi lora eval --holdout ... --model robin --endpoint http://127.0.0.1:11434/v1 --report eval.json`; run the same eval on the base model as the control.**
- [ ] **Step 4: Write both reports' rates into the how-to and the ledger. If auto-promote was on, the eval that met the thresholds already promoted the model; say so in the ledger, and run `daisugi lora demote` if the owner wants the old worker back. Delete the dataset and the adapter copies from scratch when the owner has what they need.**

```bash
git add docs/how-to/ml-pack.md docs/roadmap.md
git commit -m "docs: worker training from the garden, with the measured rates of the first box run"
```

---

## Part B: constrained output

### Task 9: The schema module and the schema on the wire

**Files:**
- Create: `src/opendaisugi/schema.py`, `docs/adr/0028-shape-and-meaning.md`
- Modify: `src/opendaisugi/llm_client.py` (`_body`, `complete`, `Reply`), `src/opendaisugi/tier1.py` (the HTTP provider), `src/opendaisugi/envelope.py`, `src/opendaisugi/decomposer.py`, `src/opendaisugi/distiller.py` (`_generalize_template`, `_improve_envelope`), `src/opendaisugi/config.py` (key `constrained_output: "auto" | "off"`, default `auto`), `src/opendaisugi/cli.py` (hidden group `schema`, verb `show`)
- Test: `tests/test_schema.py`, `tests/test_llm_client_schema.py`, `tests/test_tier1_schema.py`

**Interfaces:**
- `schema.grammar_schema(model: type[BaseModel], *, name: str) -> dict`: pydantic's `model_json_schema()` with `$defs` inlined where a `$ref` would nest inside another `$defs` entry (one level of `$ref` is kept, as llama.cpp supports it), `title`, `description`, `default`, `examples` removed, `anyOf: [X, {"type": "null"}]` rewritten to `X` with the property left out of `required`, `additionalProperties: false` set on every object, `pattern` values checked to start with `^` and end with `$` (else dropped), `format` values other than `date-time`, `date`, `time`, `uuid` dropped.
- `schema.lint(schema: dict) -> list[str]`: one line per use of `patternProperties`, `uniqueItems`, `contains`, `minContains`, `not`, `if`, `then`, `else`, `dependentSchemas`, `$anchor`, a `$ref` whose target holds another `$ref`, a `minimum`/`maximum` on a non-integer, a `pattern` without anchors, an object whose `properties` sits beside `anyOf`/`oneOf`. Empty means clean.
- `schema.ENVELOPE = grammar_schema(Envelope, name="envelope")`, `schema.PLAN = grammar_schema(ActionPlan, name="action_plan")`; `tests/test_schema.py` pins `lint(ENVELOPE) == []` and `lint(PLAN) == []` (this is the test that forces any pydantic shape change needed; make the change in `models.py` only by `Field` metadata, never by changing a type).
- `llm_client.complete(..., json_schema: dict | None = None, schema_name: str = "answer")`: on the OpenAI wire adds `response_format: {"type": "json_schema", "json_schema": {"name": schema_name, "strict": true, "schema": SCHEMA}}` in place of `json_object`; on the Anthropic wire it sends the schema in the form `schema.SUPPORT["anthropic"]["request"]` names, **if** the builder's check (Step 0) found that Anthropic's API supports structured output, and ignores it otherwise. `Reply` gains `constrained: bool`. On HTTP 400 whose body mentions `response_format` or `json_schema`, one retry without the schema, `constrained=False`, one `_log.info` line `endpoint declined response_format json_schema; retried without`.
- `schema.SUPPORT: dict`: data, one entry per wire: `{"openai": {"supported": true, "request": "response_format.json_schema", "verified_on": "2026-10-30", "url": "<the docs page>"}, "anthropic": {"supported": <bool the builder found>, "request": "<the exact field and any beta header the docs name>", "verified_on": "<date>", "url": "<the docs page>"}}`. A test fails when an entry has no `url` or `verified_on`.
- `llm_client.supports_schema(wire: str, backend: str | None, base_url: str | None, cfg) -> bool`: true when `cfg.constrained_output == "auto"` and `schema.SUPPORT[wire]["supported"]` (any OpenAI-wire endpoint, local or hosted, relies on the retry-once rule when it does not accept the field; the local kinds `ollama`, `llamafile`, `llama.cpp` and loopback, `.ts.net`, `100.64.0.0/10` and RFC 1918 hosts are known to). false otherwise.
- `tier1.py` HTTP provider's `probe_envelope` passes `json_schema=schema.ENVELOPE` when `supports_schema`; its outcome tuple's reason says `constrained` or `unconstrained`. `decomposer.py` passes `schema.PLAN` for plan writing; `distiller._generalize_template` and `_improve_envelope` pass their schemas. The parse, `_validate_envelope`, and `verify_plan` paths are unchanged.
- `daisugi schema show envelope|plan [--lint]`: prints the schema JSON (`indent=2`); `--lint` prints the lint lines and exits 1 when any.

- [ ] **Step 0: Verify Anthropic's current support, with a URL**

Read Anthropic's current documentation on structured outputs (start from the Claude API docs index, `https://docs.claude.com/`, and search for "structured outputs"; if the claude-api skill is loaded, use it as a second source, not the first). Write down, in `schema.SUPPORT["anthropic"]` and in ADR-0028: whether the Messages API accepts a JSON schema for the reply, the exact request field, any beta header, which models support it, the limits on the schema (so `lint` can add them), the page URL and today's date. A claim without a URL is not enough. If the docs say it is not supported, set `supported: false`, keep the URL, and the Anthropic test below asserts that no schema is sent. Never guess the field name.

- [ ] **Step 1: Write the failing tests**

```python
def test_envelope_and_plan_schemas_lint_clean():
    assert lint(ENVELOPE) == []
    assert lint(PLAN) == []
    assert ENVELOPE["additionalProperties"] is False


def test_openai_wire_sends_json_schema(fake_openai):
    asyncio.run(
        complete(
            [{"role": "user", "content": "x"}],
            model="openai/m",
            base_url=fake_openai.url,
            json_schema=ENVELOPE,
            schema_name="envelope",
        )
    )
    body = json.loads(fake_openai.last_body)
    assert body["response_format"]["type"] == "json_schema"
    assert body["response_format"]["json_schema"]["name"] == "envelope"
    assert body["response_format"]["json_schema"]["schema"] == ENVELOPE
    assert "json_object" not in json.dumps(body)


def test_schema_400_retries_once_without(fake_openai):
    fake_openai.reject_schema_with_400 = True
    rep = asyncio.run(
        complete(
            [{"role": "user", "content": "x"}],
            model="openai/m",
            base_url=fake_openai.url,
            json_schema=ENVELOPE,
        )
    )
    assert rep.constrained is False and fake_openai.calls == 2
    assert "response_format" not in fake_openai.last_body


def test_anthropic_wire_follows_the_verified_support(fake_messages):
    asyncio.run(
        complete(
            [{"role": "user", "content": "x"}],
            model="anthropic/claude-x",
            base_url=fake_messages.url,
            api_key="k",
            json_schema=ENVELOPE,
        )
    )
    sup = SUPPORT["anthropic"]
    assert sup["url"].startswith("https://") and sup["verified_on"]
    sent = (
        fake_messages.has_field(sup["request"])
        if sup["supported"]
        else not fake_messages.has_any_schema()
    )
    assert sent


def test_supports_schema_follows_the_table_and_the_switch():
    assert supports_schema("openai", "ollama", "http://127.0.0.1:11434/v1", cfg_auto) is True
    assert supports_schema("openai", None, "http://gpu.tail1.ts.net:8080/v1", cfg_auto) is True
    assert supports_schema("openai", None, "https://api.openai.com/v1", cfg_auto) is True
    assert supports_schema("anthropic", None, None, cfg_auto) is SUPPORT["anthropic"]["supported"]
    assert supports_schema("openai", "ollama", "http://127.0.0.1:11434/v1", cfg_off) is False


def test_shape_pass_does_not_skip_meaning(fake_openai, wide_envelope_json, plan_outside):
    fake_openai.answer = wide_envelope_json  # schema-valid, shell everywhere
    env, outcome, reason = asyncio.run(
        HTTPTier1Provider("m", base_url=fake_openai.url, backend="ollama").probe_envelope(
            "list src"
        )
    )
    assert "constrained" in reason
    assert env is None or verify_plan(plan_outside, env).ok is False
```

- [ ] **Step 2: Run to verify they fail; implement; run the three test files and `tests/test_tier1*.py`, `tests/test_envelope*.py`, `tests/test_decomposer*.py`, `tests/test_distiller*.py` green**

- [ ] **Step 3: Write ADR-0028** (`docs/adr/0028-shape-and-meaning.md`: context, the ruling "constrained output proves shape, the warden proves meaning", where the schema is sent and where it is not, the retry-once rule, the lint, the Anthropic finding of Step 0 with its URL and date; what it does not do: no GBNF converter, no skip of any check), then commit.

```bash
git add src/opendaisugi/schema.py src/opendaisugi/llm_client.py src/opendaisugi/tier1.py src/opendaisugi/envelope.py src/opendaisugi/decomposer.py src/opendaisugi/distiller.py src/opendaisugi/config.py src/opendaisugi/cli.py tests/test_schema.py tests/test_llm_client_schema.py tests/test_tier1_schema.py docs/adr/0028-shape-and-meaning.md docs/adr/README.md
git commit -m "constrained output: local OpenAI-compatible endpoints get the envelope and plan schemas as response_format; the shape check never replaces the verifier (ADR-0028)"
```

### Task 10: The schema in the Go and Rust daisugi

**Files:**
- Create: `clients/gen_schema.py` (writes `clients/go/internal/schema/assets/envelope.json`, `action_plan.json` and `clients/rust/src/schema/envelope.json`, `action_plan.json` from `schema.ENVELOPE` and `schema.PLAN`), `clients/go/internal/schema/schema.go` (`//go:embed`, `Envelope() json.RawMessage`, `Plan() json.RawMessage`, `Lint(raw) []string`), `clients/rust/src/schema/mod.rs` (the same, `include_str!`)
- Modify: `clients/go/internal/llm/http.go:210` and the Rust twin (the `response_format` json_schema body, the 400 retry, `constrained` on the reply, the Anthropic request form from `schema.SUPPORT`; the table is generated into both ports by `gen_schema.py`), the ports' `tiers setup --endpoint` probes (they send the envelope schema when `supports_schema` holds; the per-probe outcome text gains `constrained`/`unconstrained`), `schema show` verb in both ports
- Test: `tests/test_schema_assets_current.py` (the committed assets equal the oracle's schemas byte for byte; the test fails when `models.py` changes without `gen_schema.py` being rerun), `clients/k3_cases.py` re-recorded (the probe request bodies change, so every k3 `tiers setup --endpoint` case is re-recorded from the oracle), Go and Rust unit tests for the 400 retry

- [ ] **Step 1: Write `tests/test_schema_assets_current.py` and run it to see it fail (no assets yet); run `uv run --no-sync python clients/gen_schema.py`; run it green**
- [ ] **Step 2: Implement the Go and Rust schema packages and the wire change; write `TestHTTP400RetriesWithoutSchema` (Go) and `http_400_retries_without_schema` (Rust) against a loopback fake**
- [ ] **Step 3: Re-record k3 and the lora suite from the oracle; build both ports (two heavy jobs, one at a time); `k3_compare.py` and `lora_compare.py` at 0 disagreements for each port**
- [ ] **Step 4: Commit**

```bash
git add clients/gen_schema.py clients/go/internal/schema clients/go/internal/llm clients/go/internal/cli clients/rust/src/schema clients/rust/src/llm clients/rust/src/cli tests/test_schema_assets_current.py clients/fixtures/k3 clients/fixtures/lora clients/k3_cases.py
git commit -m "ports: the same envelope and plan schemas on the wire, generated from the oracle and pinned by a test"
```

### Task 11: The how-to for constrained output **[box verifies against Ollama]**

**Files:**
- Modify: `docs/how-to/token-saving-gateway.md` is renamed by W2 to `docs/how-to/router.md`; add the section "Local models write through a schema" there, and a paragraph in `docs/how-to/warden.md` ("A schema proves shape. The warden proves meaning.")
- Create: `docs/how-to/local-model-schema.md` (how `constrained_output` works, the `auto` rule, the Anthropic finding of Task 9 Step 0 with its URL, `daisugi schema show --lint`, the llama.cpp converter command for a GBNF file: `python3 examples/json_schema_to_grammar.py envelope.json > envelope.gbnf`, read from the llama.cpp repository on the day the how-to is written and cited with its URL, and the Ollama note: `format` on `/api/chat`, `response_format` on `/v1`, lower temperature, the schema also in the prompt)

- [ ] **Step 1 [box]: Against Ollama 0.40.7 with `gemma4-12b:q4_0` and `granite4:1b`, run `daisugi tiers setup --endpoint http://127.0.0.1:11434/v1 --model M --json` and record per probe whether the answer was `constrained` and whether it parsed. Against `llama-server` with the same GGUF, the same. Write the two tables into the how-to.**
- [ ] **Step 2: Commit**

```bash
git add docs/how-to/local-model-schema.md docs/how-to/router.md docs/how-to/warden.md docs/README.md
git commit -m "docs: how a local model writes envelopes and plans through a schema, and what that does and does not prove"
```

---

## Part C: the Learned panel

### Task 12: `daisugi gardener learned --json`

**Files:**
- Create: `src/opendaisugi/gardener/learned.py`
- Modify: `src/opendaisugi/cli.py` (`gardener_app` verb `learned`)
- Test: `tests/test_gardener_learned.py`, `tests/test_cli_gardener.py`

**Interfaces:**
- Consumes: `PathwayStore.list_all()`, `Journal.receipts_for_run(trace_id)`, `PruneConfig`, `model_catalog.py` (the local id set), `lora.promote.read_promoted`.
- Produces:
  - `Level = Literal["code", "small", "mixed", "frontier", "unknown"]`; `step_level(step_type: str, model_ids: list[str | None], local_ids: frozenset[str]) -> Level` per LV-2; `pathway_level(levels: list[Level]) -> Level`.
  - `status_word(p: CompiledPathway, cfg: PruneConfig, now: float) -> Literal["candidate", "kept", "at risk"]` per LV-3.
  - `@dataclass class StepRow: index: int; step_id: str; type: str; level: Level; models: list[str]`
  - `@dataclass class PathwayRow: id: str; task: str; cluster: str; level: Level; status: str; typed: bool; parameters: int; hits: int; failures: int; activations: int; last_activation_at: float; version: int; steps: list[StepRow]`
  - `@dataclass class ClusterRow: signature: str; pathways: int; level: Level; hits: int`
  - `@dataclass class LearnedReport: pathways: list[PathwayRow]; clusters: list[ClusterRow]; worker: dict | None; counts: dict[str, int]` where `counts` has `pathways`, `clusters`, `by_level`, `by_status`, and `worker` is the promoted record (adapter, base_model, promoted_at, the eval rates) or `None`.
  - `learned_report(data_dir: Path, *, now: float | None = None, limit: int = 500) -> LearnedReport`: clusters by `structure_signature` (a `None` signature is the cluster `"unsigned"`); `task` is the first 80 characters of `task_description`; model ids come from the receipts of `source_trace_ids` matched by `Receipt.step_id` to the template step's id, else by index; a trace whose receipts cannot be read contributes `None`.
  - CLI: `daisugi gardener learned [--json] [--limit N]`. Text: the counts line, then one line per cluster (`sig  n  level  hits`), then per pathway `id  level  status  hits/fails  task`. `--json` prints `asdict(report)`.

- [ ] **Step 1: Write the failing tests**

```python
def test_step_level_rules():
    local = frozenset({"ibm-granite/granite-4.0-1b"})
    assert step_level("shell", [], local) == "code"
    assert step_level("task", ["ollama/granite4:1b"], local) == "small"
    assert step_level("task", ["anthropic/claude-sonnet-4-5"], local) == "frontier"
    assert step_level("task", ["ollama/x", "anthropic/y"], local) == "mixed"
    assert step_level("task", [None], local) == "unknown"
    assert step_level("task", [], local) == "unknown"
    assert pathway_level(["code", "small", "frontier"]) == "frontier"
    assert pathway_level(["code", "unknown"]) == "unknown"


def test_status_words_follow_prune_config():
    cfg = PruneConfig()
    assert status_word(_pw(activations=2, failures=0), cfg, now=NOW) == "candidate"
    assert status_word(_pw(activations=10, failures=2), cfg, now=NOW) == "kept"
    assert status_word(_pw(activations=10, failures=6), cfg, now=NOW) == "at risk"
    assert (
        status_word(_pw(activations=10, failures=0, last=NOW - 31 * 86400), cfg, now=NOW)
        == "at risk"
    )


def test_learned_report_clusters_and_worker(seeded_data_dir):
    rep = learned_report(seeded_data_dir, now=NOW)
    assert rep.counts["pathways"] == 3 and rep.counts["clusters"] == 2
    assert {c.signature for c in rep.clusters} == {"shell→file_read", "task"}
    assert rep.pathways[0].steps[0].level == "code"
    assert rep.worker is None


def test_cli_gardener_learned_json(seeded_data_dir, runner):
    r = runner.invoke(app, ["gardener", "learned", "--json", "--data-dir", str(seeded_data_dir)])
    d = json.loads(r.output)
    assert d["counts"]["by_level"]["code"] >= 1 and "pathways" in d
```

- [ ] **Step 2: Run to verify they fail; implement; run green; commit**

```bash
git add src/opendaisugi/gardener/learned.py src/opendaisugi/cli.py tests/test_gardener_learned.py tests/test_cli_gardener.py
git commit -m "gardener learned: clusters, pathways, step levels and status words, read from the store, the receipts and the pruner's own numbers"
```

### Task 13: `gardener learned` in Go and Rust

**Files:**
- Modify: `clients/go/internal/cli/gardencmd.go`, `clients/go/internal/garden/` (a `learned.go`), `clients/rust/src/cli/gardencmd.rs`, `clients/rust/src/garden/learned.rs`, `clients/garden_cases.py` (cases `learned-empty`, `learned-three-pathways`, `learned-with-worker`, `learned-unreadable-receipts`), `clients/fixtures/garden/`
- Test: `garden_compare.py` at 0 disagreements for each port

- [ ] **Step 1: Record the four cases from the oracle; build each port (one heavy job at a time); compare**

Run: `uv run --no-sync python clients/garden_cases.py --out clients/fixtures/garden --only learned-`; then `uv run --no-sync python clients/garden_compare.py --binary clients/go/daisugi` and the Rust binary.
Expected: the four new cases agree on both; the rest unchanged.

- [ ] **Step 2: Commit**

```bash
git add clients/go/internal/cli/gardencmd.go clients/go/internal/garden clients/rust/src/cli/gardencmd.rs clients/rust/src/garden clients/garden_cases.py clients/fixtures/garden clients/go/internal/cli/helpall_gen.go clients/rust/src/cli/helpall_gen.rs
git commit -m "ports: gardener learned agrees with the oracle on the garden cases"
```

### Task 14: coppice (Go): `floor.learned`, the terminal panel, the floor-page panel

**Files:**
- Create: `harness/coppice/internal/server/learned.go` (+ `learned_test.go`), `internal/tui/learned.go` (+ `learned_test.go`), `internal/web/static/learned.js`, `internal/web/static/_tests/learned.test.mjs`
- Modify: `internal/tui/prompt.go:27` (`Verbs` gains `"learned"`), `internal/tui/run.go` (word `learned` opens the panel as `tree` does at `:2042`), `internal/web/static/app.js` (mount beside Machines at `:645`), `internal/web/static/index.html` (the `learned` element and its rail button), `internal/server/role.go:398` (`"floor.learned": LearnedRefusal`), `PROTOCOL.md` (section "The learned view" after "The machines view"), `README.md`

**Interfaces:**
- Server: `const LearnedRefusal = "only the operator reads what the garden learned."`; `const LearnedUnknownLine = "The garden is not known. Check that daisugi runs, then open Learned again."`; `func (s *Server) handleFloorLearned(_ *Client, r *proto.Request) proto.Response`; `func (s *Server) RegisterLearnedCommands()`. It runs `COPPICE_DAISUGI_BIN` (default `daisugi`) with `gardener learned --json --limit 500` in the steward's compactor directory, in a process group of its own, 10 s limit; the reply is `{"state": "ok"|"unknown", "line": "", "report": <the JSON as daisugi printed it>}` with `report` null and `line` set when the state is `unknown`. One reply is cached 30 s per server; `refresh: true` in the request bypasses the cache. A pane or a plugin gets `LearnedRefusal`.
- TUI: the word `learned` opens a panel over the windows (as `tree`): header `Learned: what the garden knows`, the counts line, then clusters, then pathways in rows `level status hits/fails task`; keys `j`/`k` move, `Enter` unfolds a pathway's steps (`index type level models`), `/` filters by task text, `r` refreshes, `Esc` closes. The bottom line prints the keys (`j/k move  Enter steps  / filter  r refresh  Esc close`), per the doctrine's section 7. Level and status are coloured with the five state colours' tokens: `code` quiet, `small` working, `mixed` and `frontier` the ask colour, `unknown` plain. The `unknown` state draws `LearnedUnknownLine` alone.
- Web: `learned.js` exports the pure parts (`rowsOf(report) -> {clusters, pathways}`, `filterRows(rows, text)`, `levelToken(level)`, `LEARNED_MS = 30000`) and `mountLearned({el, api, onOpen})`; the panel is a bottom sheet on a phone (the `.panel` rules of `app.css`), with the same rows, a filter field, a refresh button, and `?` that shows the key line. It asks `floor.learned` through the floor's own API with the token.

- [ ] **Step 1: Write the failing Go server tests**

```go
func TestLearnedRunsDaisugiInCompactorDir(t *testing.T)   // the fake daisugi records its cwd and argv; argv == ["gardener","learned","--json","--limit","500"]
func TestLearnedDaisugiMissing(t *testing.T)               // COPPICE_DAISUGI_BIN points at nothing: state unknown, line == LearnedUnknownLine, within 1 s
func TestLearnedDaisugiSlow(t *testing.T)                  // the fake sleeps 20 s: state unknown within 10.5 s, the child is gone (its pgid is dead)
func TestLearnedBadJSON(t *testing.T)                      // the fake prints "nope": state unknown
func TestLearnedCachedThirtySeconds(t *testing.T)          // two calls, one run; refresh:true runs again
func TestLearnedRefusedForPane(t *testing.T)               // a pane role gets LearnedRefusal
```

- [ ] **Step 2: Run to verify they fail; implement `learned.go`; run the package green**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G sh -c 'cd harness/coppice && go test -p 1 ./internal/server -run Learned'`

- [ ] **Step 3: TUI: write `TestLearnedWordOpensPanel`, `TestLearnedPanelKeysLine`, `TestLearnedUnknownDrawsOneLine`, `TestLearnedFilterNarrowsRows` against the shared fixture `testdata/learned/report.json`; implement; green**
- [ ] **Step 4: Web: write `learned.test.mjs` (`rowsOf`, `filterRows`, `levelToken`, the mount shows `LearnedUnknownLine` on state unknown); implement; run `node --test harness/coppice/internal/web/static/_tests/learned.test.mjs`**
- [ ] **Step 5: PROTOCOL and README sections; `go vet ./... && gofmt -l .` clean; commit**

```bash
git add harness/coppice/internal/server/learned.go harness/coppice/internal/server/learned_test.go harness/coppice/internal/server/role.go harness/coppice/internal/tui/learned.go harness/coppice/internal/tui/learned_test.go harness/coppice/internal/tui/prompt.go harness/coppice/internal/tui/run.go harness/coppice/internal/tui/testdata/learned harness/coppice/internal/web/static/learned.js harness/coppice/internal/web/static/_tests/learned.test.mjs harness/coppice/internal/web/static/app.js harness/coppice/internal/web/static/index.html harness/coppice/PROTOCOL.md harness/coppice/README.md
git commit -m "coppice: the Learned panel shows what the garden knows, on the terminal and the floor page, read through floor.learned"
```

### Task 15: coppice-rs: `floor.learned` and the terminal panel, with cases

**Files:**
- Create: `harness/coppice-rs/src/server/learned.rs`, `src/tui/learned.rs`, `harness/coppice-rs/cases/facts/learned.jsonl`, `cases/tui/learned.jsonl`, `cases/facts/fixtures/learned/` (the fake daisugi script `fake-daisugi` that answers `gardener learned --json` from `FAKE_LEARNED_FILE`, in the style of `cases/facts/machines.jsonl`'s `{FIXTURES}/web/bin/fake-daisugi`; extend that script rather than add a second one if it is shared)
- Modify: `src/server/mod.rs` (register), `src/tui/mod.rs`, `src/tui/prompt.rs` (the verbs), `src/tui/run.rs`

**Interfaces:**
- `fn handle_floor_learned(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response`, the same reply shape, limits, cache and refusal as Go.
- Cases: `facts/learned.jsonl` covers ok, unknown (bin missing), bad JSON, refresh, and the pane refusal, with `#!env COPPICE_DAISUGI_BIN={FIXTURES}/web/bin/fake-daisugi` and `#!env FAKE_LEARNED_FILE={WORK}/learned.json`; `tui/learned.jsonl` is one screen of the panel at 120x36 and one of the unknown line.

- [ ] **Step 1: Write the cases first and replay them against Go alone to see the Go side pass and the Rust side refuse**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/coppice_compare.py --go harness/coppice/build/coppice --rust harness/coppice-rs/target/release/coppice --only learned`
Expected: Rust disagrees (unknown verb).

- [ ] **Step 2: Implement the Rust verb and panel; `cargo build --release` under the cap with `CARGO_BUILD_JOBS=2`; replay again**

Expected: `0 disagreements` on `--only learned`, then on the full `facts` and `tui` suites.

- [ ] **Step 3: Commit**

```bash
git add harness/coppice-rs/src/server/learned.rs harness/coppice-rs/src/server/mod.rs harness/coppice-rs/src/tui/learned.rs harness/coppice-rs/src/tui/mod.rs harness/coppice-rs/src/tui/prompt.rs harness/coppice-rs/src/tui/run.rs harness/coppice-rs/cases/facts/learned.jsonl harness/coppice-rs/cases/tui/learned.jsonl harness/coppice-rs/cases/facts/fixtures
git commit -m "coppice-rs: floor.learned and the Learned panel agree with Go on the facts and tui cases"
```

---

## Part D: the RTS floor view

### Task 16: The pure RTS model: units, selection, groups, alerts, keys

**Files:**
- Create: `harness/coppice/plugins/rts/rts.js`, `harness/coppice/plugins/rts/_tests/rts.test.mjs`

**Interfaces:**
- `unitsOf(panes, tasks) -> Unit[]` where `Unit = {id, label, state, held, task, parent, lane, slot, cwd, model, warden: {last: "allow"|"deny"|null}}`. Lanes are tasks (one row per root task, children under their parent, as `tree` orders them); a pane with no task goes to lane `"loose"`. A pane's slot in a lane is its index in the lane's list order (stable, so a unit never moves while it lives).
- `Selection`: `select(sel, ids, mode: "set"|"add"|"toggle") -> sel` (an array of ids, at most `WATCH_MAX` watched but any number selected), `boxSelect(units, rect) -> ids`.
- `Groups`: `assignGroup(groups, n, ids) -> groups` and `recallGroup(groups, n) -> ids` for `n` in 1..9; groups are stored in `localStorage` under `coppice.rts.groups`, wrapped in try/catch, and a group that names a dead pane drops it on recall.
- `alertsOf(events, units, now) -> Alert[]` where `Alert = {id, pane, kind: "needs_you"|"denied"|"done", at}`; `kind` by the Factorio rule: a `state: blocked` event not `held` is `needs_you`, a `warden` deny is `denied`, a `done` state is `done`; alerts coalesce per pane (the newest wins); `needs_you` first, then `denied`, then `done`, each by time; at most 20.
- `keyTable` (the ramp): an ordered list `[{key, when, does}]` that the view draws on its bottom bar and in the `?` sheet; `handleKey(state, e) -> {state, post?}` where `post` is one of the four posts a view may send (`open`, `sel`, `watch`, `close`) or null. Keys: `1`..`9` recall group; `Ctrl+1`..`9` assign; `a` select all in the focused lane; `Tab`/`Shift+Tab` next/previous unit; `h`/`j`/`k`/`l` move the cursor; `Space` select the cursor unit (set) and `Shift+Space` add; `o` or `Enter` open the cursor unit (posts `open`, so the floor's tile takes the keyboard: the operator types into the agent from here); `w` watch the selection (first nine); `n` toggle labels; `m` toggle the minimap; `!` jump the cursor to the newest alert; `?` the key sheet; `Esc` closes the sheet, else clears the selection, else posts `close`.
- `modeWord(state) -> string`: `"SELECT 3"`, `"GROUP 2 (4)"`, `"ALERT needs you: alpha"`, `""`; never empty while a selection or a group recall is live (the doctrine: scream the mode).
- `staleness(from, now) -> {text, dim}`: when the floor did not post the ring (`from === null`) the alerts strip says `alerts: no history` and is dimmed; when `from` is later than two hours ago it says `alerts since <age>`.

- [ ] **Step 1: Write the failing tests** (`node --test harness/coppice/plugins/rts/_tests/rts.test.mjs`)

```js
test('units keep their slot while they live', () => { ... });
test('a pane with no task is in the loose lane', () => { ... });
test('box select picks the units inside the rect only', () => { ... });
test('groups assign and recall, and drop a dead pane on recall', () => { ... });
test('alerts coalesce per pane and sort needs_you first', () => { ... });
test('o on the cursor unit posts open with that pane', () => { ... });
test('w on twelve selected posts watch with the first nine', () => { ... });
test('Esc closes the sheet, then clears the selection, then posts close', () => { ... });
test('the mode word is never empty while a selection is live', () => { ... });
test('no history reads as no history, dimmed', () => { ... });
test('every key in keyTable is handled and every handled key is in keyTable', () => { ... });
```

- [ ] **Step 2: Run to verify they fail; implement `rts.js`; run green; commit**

```bash
git add harness/coppice/plugins/rts/rts.js harness/coppice/plugins/rts/_tests/rts.test.mjs
git commit -m "rts view: the pure model, units in task lanes, selection, control groups, alerts by class and a key table that is the on-screen ramp"
```

### Task 17: The RTS page with own art

**Files:**
- Create: `harness/coppice/plugins/rts/art.js` (+ `_tests/art.test.mjs`), `rts-view.js`, `index.html`, `rts.css`, `manifest.json`
- Modify: `harness/coppice/plugins/embed.go` (add `rts` to the `//go:embed` line), `harness/coppice/plugins/README.md` (a row: `rts | view | yes | Every pane as a unit in its task's lane. Keyboard first: numbers recall groups, o opens the unit, w watches the selection, ! jumps to the newest alert. Draws its own art; a skin pack from your own game is optional.`)

**Interfaces:**
- `art.js`: `spriteFor(state, held, tick) -> {shape, token, pulse}` and `drawUnit(ctx, unit, look, x, y, size, skin)`: without a skin, procedural shapes on the canvas in the page's own colour tokens: a `working` unit is a chevron that nods on `tick`, `blocked` is a square with a raised flag (the ask colour, pulsing), `idle` a circle, `done` a filled diamond in the done colour, a `denied` last verdict adds a red notch at the base (the same notch `garden.js` uses). With a skin (Task 18), it blits the sheet cell the skin names for the state, falling back to the procedural shape for a state the skin does not name.
- `rts-view.js`: `boot(start)` from `../_lib/view.js`; the layout: lanes left to right by task order, a minimap at the bottom right (the whole field shrunk, the viewport rectangle, click to pan), the alerts strip at the top left (a glyph per class with a count, click or `!` to jump), the bottom key bar from `keyTable`, the mode word at the bottom left, the `?` sheet as an overlay. `requestAnimationFrame` only while something pulses or nods; otherwise draw on data. The canvas resizes to the frame and the layout reflows without jumping the cursor (the doctrine: refresh without reflow).
- `manifest.json`: `{"id": "rts", "kind": "view", "title": "RTS", "about": "...", "page": "index.html", "ring": true}`.

- [ ] **Step 1: Write `art.test.mjs`** (`spriteFor` returns the five shapes for the five states; a skin with only `working` named falls back for the rest; `drawUnit` on a fake 2d context calls the blit for a skinned state and the path calls for a bare one)
- [ ] **Step 2: Implement; run both plugin test files and the whole `plugins/*/_tests` set green**
- [ ] **Step 3: Visual check with the `visual-loop` skill on a scratch floor** (`coppice --socket /mnt/tera/.tt/w3-lmv/s.sock --data-dir /mnt/tera/.tt/w3-lmv/d web serve --tls off --listen 127.0.0.1:0` with three `sleep` panes under two tasks): screenshots at 1280x800 and 390x844 saved under scratch, looked at, the two findings most worth fixing fixed. On the phone width the key bar collapses to `?` and the mode word.
- [ ] **Step 4: Confirm both ports carry the view** (`coppice web` from the Go build and from `harness/coppice-rs/target/release/coppice`: `GET /plugins/rts/manifest.json` answers 200 on each) and commit

```bash
git add harness/coppice/plugins/rts harness/coppice/plugins/embed.go harness/coppice/plugins/README.md
git commit -m "rts view: own art on a canvas, lanes, minimap, alerts strip, the key bar as the ramp; shipped by both coppice binaries"
```

### Task 18: Skin packs from the user's own game

**Files:**
- Create: `harness/coppice/internal/web/skins.go` (+ `skins_test.go`), `harness/coppice-rs/src/web/skins.rs`, `harness/coppice/internal/web/static/skins.js` (+ `_tests/skins.test.mjs`), `harness/coppice-rs/cases/web/skins.jsonl`, `docs/how-to/skins.md`
- Modify: `internal/web/api.go` (route), `internal/web/static/views.js` (post the skin to a view that declares `"skin": true` in its manifest), `plugins/rts/manifest.json` (`"skin": true`), `plugins/rts/rts-view.js` (accept the posted skin), `PROTOCOL.md` (the web part: `/skins/`), `plugins/README.md` (the `skin` manifest key)

**Interfaces:**
- Pack layout: `<data>/skins/<id>/skin.json` and the one sheet it names. `skin.json`: `{"v": 1, "id": "<id>", "title": "...", "sheet": "sheet.png", "cell": 32, "units": {"working": [col, row], "blocked": [col, row], "idle": [col, row], "done": [col, row], "denied": [col, row]}, "source": "free text: which game and version the user took the sprites from"}`. `id` matches `^[a-z0-9][a-z0-9-]{0,31}$`.
- Route: `GET /skins/` lists `[{id, title}]` for every directory with a parseable `skin.json`; `GET /skins/<id>/skin.json` and `GET /skins/<id>/<sheet>` serve the two files. The token guard applies as it does to `/api/`. `id` is checked by the regex; the file name must equal `skin.json` or the `sheet` value from that pack's `skin.json`; the path is built with `filepath.Join` then checked with `filepath.Rel` to stay under `<data>/skins/<id>/`; the file is opened with `O_NOFOLLOW` and refused when it is a symlink; over `4 << 20` bytes answers 413; a sheet whose first eight bytes are not the PNG signature answers 415; anything else 404. Nothing under `<data>/skins/` is ever written by coppice.
- `skins.js`: `loadSkin(api, id) -> Promise<{manifest, bitmap}>` (fetches both with the token, `createImageBitmap` on the blob), `rememberSkin(id)` / `recalledSkin()` in `localStorage` key `coppice.rts.skin` (try/catch), and a `<select>` in the floor page's Settings panel listing `/skins/` plus `own art` (the default). `views.js` posts `{type: "skin", manifest, bitmap}` (the bitmap transferred) to a view whose manifest has `"skin": true`, once on open and again when the setting changes.
- Case `cases/web/skins.jsonl`: a pack written under `{DATA}/skins/ok/`, the list, the two files, an escape (`/skins/ok/../../web/token`: 404), a bad id (`/skins/OK!/skin.json`: 404), a symlinked sheet (404), a 5 MiB sheet (413), a sheet without the PNG signature (415), and the routes without the token (the same refusal `/api/` gives).

- [ ] **Step 1: Write the failing Go tests**

```go
func TestSkinsListAndFiles(t *testing.T)
func TestSkinsRouteNoEscape(t *testing.T)      // "..", a symlinked sheet, an absolute sheet name in skin.json: 404, and nothing read outside the dir (a canary file outside is never opened: wrap the open in a counting fs)
func TestSkinsRouteSizeCap(t *testing.T)       // 4 MiB + 1 byte: 413
func TestSkinsRouteNeedsToken(t *testing.T)
func TestSkinsNotPNG(t *testing.T)             // 415
```

- [ ] **Step 2: Implement Go; write `skins.test.mjs` (`loadSkin` builds the two requests with the token; `recalledSkin` survives a throwing storage) and the `views.js` test (a view with `skin: true` is posted the skin, one without is not); implement; green**
- [ ] **Step 3: Write `cases/web/skins.jsonl`; implement the Rust route; replay `--only skins` and then the `web` suite at 0 disagreements (one heavy job)**
- [ ] **Step 4: Write `docs/how-to/skins.md`:** what a pack is, the `skin.json` fields, a worked example that uses a sheet the reader draws themselves, one paragraph on using sprites from a game you own ("Your copy, your machine, your files. coppice reads the directory and nothing else. Do not share a pack that holds another studio's art."), where the directory lives per port. No named game, no extractor.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/skins.go harness/coppice/internal/web/skins_test.go harness/coppice/internal/web/api.go harness/coppice/internal/web/static/skins.js harness/coppice/internal/web/static/_tests/skins.test.mjs harness/coppice/internal/web/static/views.js harness/coppice/internal/web/static/_tests/views.test.mjs harness/coppice/plugins/rts harness/coppice/plugins/README.md harness/coppice/PROTOCOL.md harness/coppice-rs/src/web/skins.rs harness/coppice-rs/src/web/mod.rs harness/coppice-rs/cases/web/skins.jsonl docs/how-to/skins.md docs/README.md
git commit -m "skin packs: a view may draw units from a sprite sheet in the user's own skins directory, served to the floor page alone, with path, size and type checks in both ports"
```

### Task 19: Docs, the ledger and the hand checks

**Files:**
- Modify: `docs/roadmap.md` (the "Later" item "Constrained decoding for local models, with worker training" becomes a solved-when section with the evidence this plan produces; the Learned panel and the `rts` view go under the floor section), `docs/plans/2026-09-24-omarchy/ROADMAP.md` ("Later" loses the item), `harness/coppice/README.md` (Learned, rts, skins, one paragraph each, current state only), `site/CLAIMS.md` (one claim line each for: the schema sends shape not meaning; promotion is never automatic; the repo ships no game art), `.superpowers/sdd/backlog/progress.md` (the rulings LV-1 to LV-9, the box-gated list, the measured numbers when they exist)

- [ ] **Step 1: Write the docs; run the names check** (`scripts/names-allow.txt` whole-word grep from W2: no `gate`, `gateway`, `foreman` in the new files)
- [ ] **Step 2: The full Python suite once, as the heavy job** (`systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run pytest -q`), `ruff check . && ruff format --check .`
- [ ] **Step 3: Commit**

```bash
git add docs/roadmap.md docs/plans/2026-09-24-omarchy/ROADMAP.md harness/coppice/README.md site/CLAIMS.md
git commit -m "docs: worker training, constrained output, the Learned panel and the rts view as they stand"
```

**Hand checks for the owner** (written to the ledger, not run by a builder):
1. `coppice`, type `learned`: does the panel answer the question "what has the garden learned" in one glance? Is `unknown` honest where it shows?
2. Open the `rts` view on the floor page with four real agents: select two with `Space`, `Ctrl+1`, press `1`, `o`, type into the agent, `Esc` back. Was there any moment it sent you to another page to type?
3. On the phone: the Learned sheet and the `rts` view at 390 px wide.
4. On the box: `daisugi lora status` after Task 8; compare the two eval reports (base and adapter); the passing one has already promoted itself unless `lora_auto_promote` is false, and `daisugi lora demote` undoes it.

---

## Box-gated tasks

| Task | What the box verifies | What is built and tested here first |
|---|---|---|
| 7 (second step) | nothing on the box; the tiny smoke runs here once under the cap | the test itself |
| 8 | the real export, train (on the GPU), to-gguf, `ollama create` with `ADAPTER`, eval of base vs adapter | every command against fakes and golden cases (Tasks 1 to 6) |
| 11 (first step) | `tiers setup --endpoint` against Ollama 0.40.7 and llama-server: which honour `response_format` json_schema | the wire change and the 400 retry against a loopback fake (Tasks 9, 10) |

## Self-review notes

- Spec coverage: scope 1 is Tasks 1 to 8; scope 2 is Tasks 9 to 11; scope 3 is Tasks 12 to 15; scope 4 is Tasks 16 to 18; docs and hand checks are Task 19. Full parity: daisugi verbs in three languages (Tasks 2, 4, 5, 6, 10, 13); coppice in Go and Rust (Tasks 14, 15, 18) with shared JavaScript (16, 17).
- Names used across tasks: `Kind`, `PLAN_INSTRUCTION`, `split_holdout`, `EvalReport`, `Thresholds`, `check`, `read_promoted`, `schema.ENVELOPE`, `schema.PLAN`, `supports_schema`, `SUPPORT`, `maybe_auto_promote`, `Level`, `LearnedReport`, `LearnedRefusal`, `LearnedUnknownLine`, `keyTable`, `WATCH_MAX`. Each is defined once, in the first task that names it.
- Review Focus lines map to tests in Tasks 9, 9, 1, 14 and 18.
