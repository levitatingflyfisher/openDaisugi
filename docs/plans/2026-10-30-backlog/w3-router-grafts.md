# Router parts 1 to 4, grafts and GEPA over graft rules: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the router (cheap first with the verifier as the escalation signal, the router fills each plan step's model, a learned chooser trained on graded and shadowed turns, the meter for all of it), add the rewrite verdict to the warden (shell rewrite and output replace, each proved at install and re-checked on every call, the journal holding the original and the rewrite), and give the garden an optimizer that proposes better graft rules scored by proofs, replay and warden verdicts. Every saving in the docs comes from the meter.

**Architecture:** The Python package stays the oracle; Go and Rust follow with the same golden cases through `clients/router_cases.py`, `clients/warden_cases.py`, `clients/garden_cases.py` and their compare scripts. The router learns only from signals it can see in the conversation itself (tool results that carry a warden deny or a failed verify) and from the outcome table that knn, the grader and the human labels already fill. A graft is data in `<data>/warden/grafts/`, proved by `envelope_subsumes` against the envelope in force, and applied only after the rewritten call passes the whole decide path again. The optimizer mutates rule files through a model whose prompt bytes are fixed, scores them mechanically, and installs winners in `audit` state only.

**Tech Stack:** Python 3.12 (numpy, sqlite3, httpx MockTransport in tests), Go (cgo, Z3 5.1.0), Rust, Z3 subsumption, the fake upstream and fake model server the case writers already have.

**Spec:** `docs/plans/2026-09-24-omarchy/design-router.md` (parts 1 to 4), `docs/plans/2026-09-24-omarchy/design-grafts.md` (shapes a and b, the two checks, the learning loop), `docs/roadmap.md` sections 4 and 5 (GEPA), `docs/research/prior-art-2026-09-26.md` section 6. Rulings: `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md` (Router and Grafts tables), `clients/ADJUDICATIONS.md` (RP-1 to RP-15, SW-6 to SW-20, GR-R-1 to GR-R-3, RT-R-1 to RT-R-13a).

## What is already built (do not plan it again)

Evidence read on 2026-10-30 from `src/opendaisugi/`, `clients/ADJUDICATIONS.md` and `.superpowers/sdd/omarchy/progress.md`:

| Part | State | Where |
|---|---|---|
| Router part 0: `deny_redirect` graft rule, the `spawn` MCP tool (bulk_read, and code_write as a draft), skill text, 131 warden cases, 36 MCP cases | Built 2026-09-30 and 2026-10-01 (RP-1 to RP-9, SW-6 to SW-9, GR-R-1, GR-R-2) | `gate.py` `_maybe_graft`, `spawn.py`, `graft_install.py`, `mcp_server.py` `spawn` (today `delegate`), Go `internal/gate/graft.go`, `internal/spawn/`, Rust `gate/graft.rs`, `spawn.rs` |
| Router part 4, in part: delegations journal, `elapsed_ms` per turn, graft audit records, `router status` by week, trial arms, operator labels, session cost, what promotion would do | Built (RP-10 to RP-12, SW-10 to SW-16, SW-20) | `router_report.py` (today's name), `spawn.py` `SpawnRecord`, `gateway_journal.py` |
| Rules routing with cache-aware stickiness; Switchyard as a managed child | Built (ADR-0015) | `gateway.py` `route_turn`, `router_switchyard.py` |
| knn routing mode: outcome table `routing.db`, retry labels, `route feedback`, golden table in all three ports | Built 2026-10-23 (R2; RT-R-1 to RT-R-8) | `route_knn.py`, Go `gateway/knn.go`, Rust `gateway/knn.rs`, `clients/fixtures/gateway/knn_golden.json` |
| model routing mode and the grader; outcome `source` ranking human > grader > auto | Built 2026-10-23 (R3; RT-R-9 to RT-R-13a) | `gateway_pipeline.py` `_prepare_model`, Go `gateway/model.go`, Rust `gateway/model.rs` |
| The operator-edit recheck (GR-8) | Built (SW-5) | `gate.py` `recheck_edit`, Go `recheckEdit`, Rust `recheck_edit` |
| A downgraded turn that the upstream rejects with 4xx is re-served on the requested model and booked as not downgraded | Built | `gateway_asgi.py` `_record` (`used_original`) |

Not built, and in this plan: part 1 (escalations are always 0 today), part 2 (`_apply_preferred_models` copies `size_plan`, the router never fills a step), part 3 (RT-R-12: "the learned net is not built"; no shadow sampling), the rest of part 4 (escalation, shadow and chooser meters; a chooser replaced only when it wins held out), graft shapes (a) and (b), `daisugi-spawn`, the second decide on a rewritten input, `--format codex`, the pi and OpenCode rewrite path, the rival post-call check (GR-3, second half), tool result sizes in the transcript reader, and the optimizer.

## Global Constraints

- **Build after the W2 rename.** Write every identifier in the new names: gate -> **warden** (`src/opendaisugi/warden.py`, `daisugi warden <verb>`, `<data>/warden/`, format name `claude` for the host stays), gateway -> **router** (`router.py`, `router_pipeline.py`, `router_journal.py`, `router_asgi.py`, `router_openai.py`, `daisugi router serve|report|status|label|stop`, `<data>/router/`), the inner policy -> **routing mode** (config key `routing_mode`: `off|rules|switchyard|knn|model`, `routing_model`, `router_grader`, `router_local_model`), foreman -> **steward**. Go: `clients/go/internal/warden/`, `internal/router/`, `internal/spawn/`, `internal/cli/`. Rust: `clients/rust/src/warden/`, `src/router/`, `src/spawn.rs`, `src/cli/`. Fixtures: `clients/fixtures/warden/`, `clients/fixtures/router/`, `clients/fixtures/garden/`; writers `clients/warden_cases.py`, `clients/router_cases.py`, `clients/garden_cases.py`; compares `clients/warden_compare.py`, `clients/router_compare.py`, `clients/garden_compare.py`. Where W2 left a class name, use it; this plan writes `Router` for the pipeline class (`Gateway` today) and `RouterJournal` for the journal.
- **Name collision to settle before Task 1:** W2 renames `gateway_report.py` to `router_report.py`, and a `router_report.py` (the `router status` report) exists today. This plan calls the `router status` module **`router_status.py`** and the turn-meter report `router_report.py`. If W2 chose otherwise, use W2's names and change nothing else.
- Laptop: 4 cores, 15 GB. **One heavy job at a time** (a Z3 or cgo build, a cargo build, a full compare, a fuzz run, the full pytest suite), each under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G`, with `go test -p 1`, `CARGO_BUILD_JOBS=2`, `DAISUGI_NATIVE_JOBS=2`. Never rebuild Z3; never `cargo clean` in `clients/rust`.
- `/tmp` is RAM. Scratch is `/mnt/tera/.tt/w3-router-grafts/`. Worktree: `/mnt/tera/working/programming/od-wt/w3-router-grafts`, branch `w3-router-grafts`, made from master after the W2 merge.
- No network and no real model in any test or case. Fake upstreams only: httpx `MockTransport` in unit tests, the case writers' fake upstream (answers keyed by the SHA-256 of path and body bytes) and fake model server for golden cases. Helper prompts are byte-fixed in this plan so the ports send identical requests.
- Never `pkill -f`. Stop a process by the pid a test recorded.
- No AI attribution in commits. Never commit `CLAUDE.md` or `docs/superpowers/`. Commit by explicit path. No push: the fold script pushes on the owner's word.
- Prose: ASD-STE100. No em dashes. Never "seam" or "the layer" (write daisugi). No changelog voice in docs. Every number names its source.
- Every coppice run in a hand check passes its own `--socket` and `--data-dir` under scratch. Never touch `~/.opendaisugi`, `~/.local/state/coppice`, `~/.claude/settings.json`.
- **Owner rules that bind the router:** the router never switches a turn to a model the harness did not ask for. A downgrade and the escalation back to the requested model are the router's own choice inside the harness's request (ruling W3R-1 below; owner ruled it on 2026-10-30: allowed, default on). On an upstream quota or server failure the router fails over only across accounts for the **same model**; with no second account it returns the upstream status unchanged with a pause header, and the harness or the steward requeues. Local workers (the delegate, the local rung) keep running. Nothing in this plan picks a model above the requested one.
- **Every saving claim is measured.** Tokens come from the provider's own `usage` fields (`input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`). Dollars are those counts at the price table's list prices (`price_turn`), marked `estimated` whenever any input to the number is an estimate. Docs quote only `daisugi router status --markdown` output with its date (Task 20). A test fails any router doc that states a percentage or a multiplier outside the generated file.
- Fail closed. A graft never turns a deny into an allow. A rule that cannot be proved is not installed. A rewritten call that the whole decide path denies is not applied; the original allow stands and the attempt is logged. A worker never runs inside a pre-tool hook.
- Full parity: Python first, then Go and Rust on the same golden cases, 0 disagreements, every refusal under a ruling in `clients/ADJUDICATIONS.md`.

## Provisional rulings made by this plan (W3R-n; owner to confirm; each easy to undo)

| ID | Ruling | Cost if wrong |
|---|---|---|
| W3R-1 | Escalation goes to the model the harness requested, never above it, and a conversation escalates at most once; this is not an "auto model switch" because the requested model is the harness's own choice. | **Owner, 2026-10-30 (B2): allowed, default ON** (was proposed default off). The flag `routing_escalate` (config), `--no-escalate` (on `router serve` and `install --router`) turns it off. With it off, a deny or a failed verify is only a log line and the conversation stays on the cheap model. One task. |
| W3R-2 | The verifier's signal reaches the router through the conversation: a tool result marked `is_error` that carries the warden's deny prefix, or a result from an `mcp__opendaisugi__*` tool whose JSON carries `escalation_signal`. No new channel between warden and router. | A session-keyed channel later; the detector is one function. |
| W3R-3 | Cheap first is a flag, `routing_cheap_first` (default off), applied only to the first human ask of a conversation. | Delete the flag. |
| W3R-4 | Same-model account failover reads credentials from environment variables named in `router_accounts`; an OAuth credential has no second account and gets the pause header. | A keyring later; the list is one field. |
| W3R-5 | Shadow rows are `turns` rows with `shadow_of` set and `source = grader`; knn counts them as neighbours. This amends RT-R-5 (the router now explores, bounded by rate and a daily token cap). | Filter `shadow_of IS NULL` in `recent()`; one line per port. |
| W3R-6 | The learned chooser is a logistic regression over the matcher's vectors, trained by batch gradient descent with fixed constants, so all three ports produce the same weights from the same table. | Replace the trainer; the chooser file format stays. |
| W3R-7 | A chooser file is replaced only when its held-out accuracy is at or above the one in force (or `--force`). | A flag. |
| W3R-8 | A shell rewrite on Claude Code is emitted with `permissionDecision: "ask"` and `updatedInput`, so the host's own prompt still stands; `"allow"` only when the rule sets `conditions.skip_host_prompt: true`. On Codex a rewrite needs `skip_host_prompt` (Codex takes `updatedInput` only with allow); otherwise the rule is audit-only there. | One branch in `_outcome` per port. |
| W3R-9 | The rule file holds its proof with the SHA-256 of the envelope it was proved against. A call under a different envelope treats the rule as audit and says `run daisugi graft prove`. | Re-prove on every call instead (slower). |
| W3R-10 | `daisugi-spawn` is its own executable and shell head in all three ports; the envelope must list that head for a shell rewrite to pass the second decide. No installer edits an envelope. | A subcommand would grant every `daisugi` verb (design, "the shell allowlist trap"). |
| W3R-11 | The output_replace hook digests the tool response it was given, never the file on disk, and replaces only the `content` field inside the host's own response shape. | Nothing stored depends on it. |
| W3R-12 | The optimizer installs a proposal only in state `audit`, through `graft_install`, with `provenance.proposed_by = "garden"`; it never promotes, retires or edits an installed rule. | A flag later. |
| W3R-13 | A proposal is rejected before any score when its proof fails or when it matches any corpus record the warden denied (ruling RK-1's order: quality first, cost last). | The order is a sort key. |

Tasks that depend on provisional DECISIONS-PENDING rows that are not retired: RT-2 (the name `spawn`, ruled 2026-10-30; Tasks 2, 13, 15), GR-3 second half (the post-call check; Task 15), GR-4 (`daisugi-spawn`; Tasks 13, 14), RK-1 (the order quality then cost; Task 18). All four were confirmed as written in the owner's 2026-09-28 bulk confirmation; they stay in the table as provisional, so the tasks say so in their commit messages.

## Review Focus

1. A tool result that quotes a warden deny line from a file the model read (an audit log, a test fixture) must not escalate. Only a result with `is_error: true` counts as a warden deny, and only a result whose tool use names an `mcp__opendaisugi__*` tool counts as a verify signal. Test `test_quoted_deny_is_not_a_signal` in Task 2.
2. A conversation escalates once and never re-downgrades; cheap first never applies to a tool-loop continuation. Tests `test_escalates_once` and `test_cheap_first_skips_continuation` in Task 3.
3. The shadow daily cap counts spend across restarts and counts a shadow call that crashed before its result row. The intent row is written before the call. Test `test_shadow_cap_counts_intent_rows` in Task 9.
4. A rewrite of a command whose operand holds a space, a quote or a newline is quoted with `shlex.quote`, and a symlink operand is resolved before the second decide. Tests `test_rewrite_quotes_operands` and `test_rewrite_symlink_resolved` in Task 14.
5. The account failover retry goes to the same upstream URL and model, never to another host, and never with the harness's own credential replaced by a key for a different provider. Test `test_failover_same_url_same_model` in Task 4.

---

### Task 1: Worktree, scratch, baseline, and the meter's new fields as zeros

**Files:**
- Modify: `src/opendaisugi/router_status.py` (the `_empty(week)` row and `week_line`)
- Modify: `src/opendaisugi/cli.py` (`router_status_cmd`: add `--markdown`)
- Test: `tests/test_router_status.py`

**Interfaces:**
- Produces: every weekly row of `router_status.weekly(data_dir)` carries `escalations: {"count": int, "by_reason": dict[str,int], "cache_tokens_lost_est": int}`, `account_failovers: int`, `shadows: {"count": int, "tokens": int, "cost": float, "good": int, "bad": int}`, `chooser: {"trained_at": str|None, "n": int, "held_out_accuracy": float|None, "precision_at_threshold": float|None}`, `grafts_by_shape: {shape: {"matched": int, "applied": int, "tokens_kept_est": int, "extra_turns": int, "not_applied": dict[str,int]}}`, `incidents: int`. All zeros or None now; later tasks fill them.
- Produces: `router_status.markdown(doc: dict) -> str`: the weekly table as a Markdown table with a first line `Measured by daisugi router status on <YYYY-MM-DD> from <data dir name>.`; `daisugi router status --markdown` prints it.

- [ ] **Step 1: Make the worktree and scratch**

```bash
git -C /mnt/tera/working/programming/openDaisugi worktree add -b w3-router-grafts /mnt/tera/working/programming/od-wt/w3-router-grafts master
mkdir -p /mnt/tera/.tt/w3-router-grafts
df -h /mnt/tera | tail -1
```
Expected: the worktree exists; more than 20 GB free.

- [ ] **Step 2: Write the failing test**

```python
# tests/test_router_status.py
def test_weekly_row_has_the_new_fields_as_zeros(tmp_path):
    from opendaisugi import router_status

    rows = router_status.weekly(tmp_path)  # no journals: one empty current week
    r = rows[-1]
    assert r["escalations"] == {"count": 0, "by_reason": {}, "cache_tokens_lost_est": 0}
    assert r["account_failovers"] == 0
    assert r["shadows"] == {"count": 0, "tokens": 0, "cost": 0.0, "good": 0, "bad": 0}
    assert r["chooser"] == {
        "trained_at": None,
        "n": 0,
        "held_out_accuracy": None,
        "precision_at_threshold": None,
    }
    assert r["grafts_by_shape"] == {}
    assert r["incidents"] == 0


def test_markdown_names_its_source(tmp_path):
    from opendaisugi import router_status

    doc = {"weeks": router_status.weekly(tmp_path), "data_dir": str(tmp_path), "date": "2026-11-02"}
    text = router_status.markdown(doc)
    assert (
        text.splitlines()[0]
        == f"Measured by daisugi router status on 2026-11-02 from {tmp_path.name}."
    )
    assert "| week |" in text.splitlines()[2]
```

- [ ] **Step 3: Run to verify it fails**

Run: `cd /mnt/tera/working/programming/od-wt/w3-router-grafts && uv run --no-sync pytest -q tests/test_router_status.py -k "new_fields or markdown" -x`
Expected: FAIL (KeyError `escalations`; no attribute `markdown`).

- [ ] **Step 4: Add the fields to `_empty` and the two functions**

`markdown(doc)` takes the dict `router status --json` prints plus `date` (today, UTC) and `data_dir`. Columns: week, turns, tokens saved (est), $ saved (est), escalations, shadows good/bad, chooser accuracy, grafts applied, incidents. `--markdown` on the command prints it and nothing else.

- [ ] **Step 5: Run the file and the existing status cases**

Run: `uv run --no-sync pytest -q tests/test_router_status.py tests/test_cli_route.py -x`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/router_status.py src/opendaisugi/cli.py tests/test_router_status.py
git commit -m "router status: the meter names every field this wave will fill, as zeros, and prints Markdown that states its date and source"
```

### Task 2: The verifier's signal, read from the conversation

**Files:**
- Create: `src/opendaisugi/router_escalate.py`
- Modify: `src/opendaisugi/warden.py` (export `DENY_PREFIX = "openDaisugi warden: DENIED"`; `_outcome` uses it)
- Modify: `src/opendaisugi/mcp_server.py` (`verify_plan`, `verify_completed_step`, `run_plan` results)
- Test: `tests/test_router_escalate.py`, `tests/test_mcp_server.py`

**Interfaces:**
- Produces: `warden.DENY_PREFIX: str`.
- Produces: in `mcp_server`, a `verify_plan` or `verify_completed_step` result with `valid == False` carries `"escalation_signal": "verify_failed"`; a `run_plan` result that refused or failed carries `"escalation_signal": "run_refused"`. Other results carry no such key.
- Produces: `router_escalate.Signal(kind: str, detail: str)` (frozen dataclass; `kind` in `("warden_deny", "verify_failed", "run_refused")`, `detail` at most 200 code points) and `router_escalate.escalation_signals(body: dict, *, after_message: int) -> list[Signal]`, for both the Messages wire (`messages[].content[]` blocks `tool_use`, `tool_result`) and the OpenAI wire (`role: "tool"` messages, `tool_calls`), scanning only messages at index `>= after_message`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_router_escalate.py
from opendaisugi.router_escalate import Signal, escalation_signals
from opendaisugi.warden import DENY_PREFIX


def _body(*msgs):
    return {"model": "m", "messages": list(msgs)}


def _use(id_, name):
    return {
        "role": "assistant",
        "content": [{"type": "tool_use", "id": id_, "name": name, "input": {}}],
    }


def _res(id_, text, err=False):
    b = {"type": "tool_result", "tool_use_id": id_, "content": text}
    if err:
        b["is_error"] = True
    return {"role": "user", "content": [b]}


def test_warden_deny_in_an_error_result_is_a_signal():
    body = _body(
        _use("t1", "Bash"), _res("t1", f"{DENY_PREFIX} \u2014 rm outside the workspace", err=True)
    )
    assert escalation_signals(body, after_message=0) == [
        Signal("warden_deny", "rm outside the workspace")
    ]


def test_quoted_deny_is_not_a_signal():
    body = _body(_use("t1", "Read"), _res("t1", f"log line: {DENY_PREFIX} \u2014 x"))
    assert escalation_signals(body, after_message=0) == []


def test_verify_failed_only_from_our_tool():
    ours = _body(
        _use("t1", "mcp__opendaisugi__verify_plan"),
        _res("t1", '{"valid": false, "escalation_signal": "verify_failed"}'),
    )
    theirs = _body(
        _use("t1", "mcp__other__verify_plan"),
        _res("t1", '{"valid": false, "escalation_signal": "verify_failed"}'),
    )
    assert [s.kind for s in escalation_signals(ours, after_message=0)] == ["verify_failed"]
    assert escalation_signals(theirs, after_message=0) == []


def test_only_messages_after_the_index_count():
    body = _body(_use("t1", "Bash"), _res("t1", f"{DENY_PREFIX} \u2014 a", err=True))
    assert escalation_signals(body, after_message=2) == []


def test_openai_wire_tool_message():
    body = {
        "model": "m",
        "messages": [
            {
                "role": "assistant",
                "tool_calls": [
                    {
                        "id": "c1",
                        "type": "function",
                        "function": {"name": "mcp__opendaisugi__run_plan", "arguments": "{}"},
                    }
                ],
            },
            {
                "role": "tool",
                "tool_call_id": "c1",
                "content": '{"status": "refused", "escalation_signal": "run_refused"}',
            },
        ],
    }
    assert [s.kind for s in escalation_signals(body, after_message=0)] == ["run_refused"]
```

```python
# tests/test_mcp_server.py (add)
def test_invalid_verify_plan_carries_the_signal(mcp_tools, sample_plan, bad_envelope):
    out = mcp_tools.verify_plan(
        plan=sample_plan.model_dump(mode="json"), envelope=bad_envelope.model_dump(mode="json")
    )
    assert out["valid"] is False and out["escalation_signal"] == "verify_failed"
```
(Use the fixtures `tests/test_mcp_server.py` already builds for calling tools in process; `bad_envelope` is an envelope whose permissions exclude the plan's step.)

- [ ] **Step 2: Run to verify they fail**

Run: `uv run --no-sync pytest -q tests/test_router_escalate.py tests/test_mcp_server.py -k "signal" -x`
Expected: FAIL (`ModuleNotFoundError: opendaisugi.router_escalate`).

- [ ] **Step 3: Implement**

`escalation_signals`: build `id -> tool name` from `tool_use` blocks and `tool_calls` over the whole body (the use may be before `after_message`); for each result at or after `after_message`: text = string content, or the joined `text` of block content; `warden_deny` only when `is_error` is true and `DENY_PREFIX` is in the text, detail = the text after the first `U+2014 ` or `: ` following the prefix; `verify_failed` and `run_refused` only when the tool name starts with `mcp__opendaisugi__` and the text parses as JSON with `escalation_signal` equal to that kind. `_outcome` in `warden.py` builds its stderr as `f"{DENY_PREFIX} \u2014 {decision.reason}"`, so the text is one constant in one place.

- [ ] **Step 4: Run to verify they pass, then the warden suite**

Run: `uv run --no-sync pytest -q tests/test_router_escalate.py tests/test_mcp_server.py tests/test_warden*.py -x`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/router_escalate.py src/opendaisugi/warden.py src/opendaisugi/mcp_server.py tests/test_router_escalate.py tests/test_mcp_server.py
git commit -m "router: read the verifier's verdicts from the conversation, so escalation needs no channel between warden and router (W3R-2; RT-2 provisional)"
```

### Task 3: Part 1, cheap first and escalation in the pipeline

**Files:**
- Modify: `src/opendaisugi/router_pipeline.py` (the sticky map becomes `ConversationState`; `prepare`, `finish`)
- Modify: `src/opendaisugi/router_journal.py` (`TurnRecord` fields)
- Modify: `src/opendaisugi/config.py` (`routing_cheap_first: bool = False`, `routing_escalate: bool = True`; escalation is ON by default, owner ruling B2)
- Modify: `src/opendaisugi/router_asgi.py` (`_rebuild` passes both flags), `src/opendaisugi/cli.py` (`router serve --cheap-first --no-escalate`, `install --router --cheap-first --no-escalate`; the help text says escalation is on and `--no-escalate` turns it off)
- Test: `tests/test_router_escalation_policy.py`

**Interfaces:**
- Consumes: `router_escalate.escalation_signals`, `route_knn.OutcomeStore.set_outcome(turn_id, outcome, source)`, `router.estimate_prefix_tokens(body)`.
- Produces: `router_pipeline.ConversationState(model: str, requested: str, downgraded: bool, escalated: bool, escalation_reason: str | None, after_message: int, last_turn_id: str | None)`; `Router.states: dict[str, ConversationState]` keyed by the conversation key (replaces the model-only sticky map; `_remember` keeps its name and writes the state).
- Produces: `Router(cheap_first: bool = False, escalate: bool = True, ...)`; `TurnRecord.escalated: bool = False`, `TurnRecord.escalation_reason: str | None = None`, `TurnRecord.cache_tokens_lost_est: int = 0`, `TurnRecord.cheap_first: bool = False` (JSON keys the same; written only when true or set).
- Produces: reason lines. Cheap first: `cheap first: <model>; escalates on a warden deny, a failed verify or an upstream failure`. Escalation: `escalated: <kind> <detail>; the attempt on <cheap model> is booked bad; <requested> for the rest of this conversation`. After: `escalated earlier (<kind>); stays on <requested>`.

Policy (applied in `prepare`, after the routing mode chose and before stickiness):
1. State exists and `escalated`: the requested model, tier `tier2-frontier`, `downgraded False`, the "escalated earlier" reason. Never downgraded again.
2. State exists, `downgraded`, `escalate` on, and `escalation_signals(body, after_message=state.after_message)` is not empty: escalate as in the reason above; `state.escalated = True`; if `state.last_turn_id`, `store.set_outcome(last_turn_id, "bad", "auto")`; the record gets `escalated`, `escalation_reason` (`"<kind>: <detail>"`), `cache_tokens_lost_est = estimate_prefix_tokens(body)` and `estimated = True`.
3. No state, `cheap_first` on, the turn has new human text (RT-R-4's "no signal means no downgrade"), the mode kept the requested model, and the candidate list (RT-R-4: local, then cheap, strictly cheaper by the price table) is not empty: the first candidate, tier `tier1-local` or `tier1-cheap`, `downgraded True`, the "cheap first" reason, `cheap_first = True` on the record.
4. Upstream failure or refusal on a downgraded turn (`finish(..., failed=True)`): the state is marked `escalated` with reason `upstream_failed`, so rule 1 applies to the next turn. (The existing `used_original` retry is unchanged.)
5. `state.after_message = len(body["messages"])` at the end of every `prepare`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_router_escalation_policy.py
import json, httpx, pytest
from opendaisugi import route_knn as rk
from opendaisugi.router_pipeline import Router
from opendaisugi.warden import DENY_PREFIX
from tests.test_router_model import _reply, OPUS, HAIKU, VEC  # the httpx MockTransport helpers


def _router(tmp_path, **kw):
    store = rk.OutcomeStore(tmp_path / "routing.db")
    return Router(
        knn=rk.KnnRouter(store, lambda t: (VEC, "fake"), 0.25), cheap_model=HAIKU, **kw
    ), store


def _ask(text, conv="c1"):
    return {
        "model": OPUS,
        "max_tokens": 10,
        "metadata": {"user_id": conv},
        "messages": [{"role": "user", "content": text}],
    }


def test_cheap_first_takes_the_first_ask(tmp_path):
    r, _ = _router(tmp_path, cheap_first=True)
    p = r.prepare(_ask("rename a variable"))
    assert p.decision.model == HAIKU and p.decision.downgraded
    assert p.decision.reason.startswith("cheap first: ")


def test_cheap_first_skips_continuation(tmp_path):
    r, _ = _router(tmp_path, cheap_first=True)
    body = _ask("x")
    body["messages"].append(
        {
            "role": "assistant",
            "content": [{"type": "tool_use", "id": "t", "name": "Read", "input": {}}],
        }
    )
    body["messages"].append(
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t", "content": "ok"}]}
    )
    p = r.prepare(body)
    assert p.decision.model == OPUS and not p.decision.downgraded


def test_escalates_once_and_books_the_cheap_row_bad(tmp_path):
    r, store = _router(tmp_path, cheap_first=True)
    body = _ask("do it")
    p1 = r.prepare(body)
    r.finish(p1, {"input_tokens": 5, "output_tokens": 2})
    row = store.recent("fake")[0]
    body["messages"] += [
        {
            "role": "assistant",
            "content": [
                {"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "rm -rf /"}}
            ],
        },
        {
            "role": "user",
            "content": [
                {
                    "type": "tool_result",
                    "tool_use_id": "t1",
                    "is_error": True,
                    "content": f"{DENY_PREFIX} \u2014 outside",
                }
            ],
        },
    ]
    p2 = r.prepare(body)
    assert p2.decision.model == OPUS and p2.decision.reason.startswith(
        "escalated: warden_deny outside"
    )
    _, rec = r.finish(p2, {"input_tokens": 50, "output_tokens": 2})
    assert (
        rec.escalated
        and rec.escalation_reason == "warden_deny: outside"
        and rec.cache_tokens_lost_est > 0
        and rec.estimated
    )
    assert store.recent("fake")[-1].outcome == "bad"
    body["messages"] += [
        {"role": "assistant", "content": "done"},
        {"role": "user", "content": "now the next tiny thing"},
    ]
    p3 = r.prepare(body)
    assert p3.decision.model == OPUS and p3.decision.reason.startswith(
        "escalated earlier (warden_deny)"
    )


def test_escalation_is_on_by_default(tmp_path):
    from opendaisugi.config import Config

    assert Config().routing_escalate is True
    r, _ = _router(tmp_path, cheap_first=True)  # no escalate argument: the default
    assert r.escalate is True


def test_escalate_off_keeps_the_cheap_model(tmp_path):
    r, _ = _router(tmp_path, cheap_first=True, escalate=False)
    body = _ask("do it")
    r.prepare(body)
    body["messages"] += [
        {
            "role": "assistant",
            "content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {}}],
        },
        {
            "role": "user",
            "content": [
                {
                    "type": "tool_result",
                    "tool_use_id": "t1",
                    "is_error": True,
                    "content": f"{DENY_PREFIX} \u2014 x",
                }
            ],
        },
    ]
    assert r.prepare(body).decision.model == HAIKU


def test_never_above_requested(tmp_path):
    r, _ = _router(tmp_path, cheap_first=True)
    body = _ask("do it")
    body["model"] = HAIKU
    assert r.prepare(body).decision.model == HAIKU
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run --no-sync pytest -q tests/test_router_escalation_policy.py -x`
Expected: FAIL (`TypeError: unexpected keyword argument 'cheap_first'`).

- [ ] **Step 3: Implement the state, the policy and the record fields**

Keep `_remember(key, model)` as the writer of `states[key]`. The policy is one method `_escalation_policy(self, key, body, decision) -> RouteDecision` called from `_prepare` for every mode but `external` and `off`. The booking of the cheap row needs `state.last_turn_id`: set it in `finish` from `_book_knn`'s return. In `rules` mode there is no outcome store; then `set_outcome` is skipped and the record still says `escalated`.

- [ ] **Step 4: Run the new file and every router test**

Run: `systemd-run --user --scope -q -p MemoryMax=5G uv run --no-sync pytest -q tests/test_router_escalation_policy.py tests/test_router*.py -x`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/router_pipeline.py src/opendaisugi/router_journal.py src/opendaisugi/config.py src/opendaisugi/router_asgi.py src/opendaisugi/cli.py tests/test_router_escalation_policy.py
git commit -m "router part 1: cheap first behind a flag; a warden deny, a failed verify or an upstream failure escalates once to the requested model (W3R-1, W3R-3)"
```

### Task 4: Same-model account failover, else pause

**Files:**
- Create: `src/opendaisugi/router_accounts.py`
- Modify: `src/opendaisugi/router_asgi.py` (the upstream send), `src/opendaisugi/config.py` (`router_accounts: list[str] = []`), `src/opendaisugi/router_journal.py` (`TurnRecord.account_failovers: int = 0`)
- Test: `tests/test_router_accounts.py`

**Interfaces:**
- Produces: `router_accounts.Accounts.from_env(names: list[str], environ: Mapping[str, str]) -> Accounts`; `Accounts.credentials() -> list[tuple[str, str]]` (name, value), in the order given, skipping unset names.
- Produces: `router_accounts.plan_retry(status: int, headers_sent: dict[str, str], accounts: Accounts, tried: int) -> Retry | None`: `Retry(header: str, value: str, account: str)` for status in `(429, 503, 529)` when the request carried `x-api-key` or `authorization: Bearer sk-...` (an API key, never an OAuth token) and an untried account remains; else None.
- Produces: the pause reply: the upstream status and body unchanged, plus `x-daisugi-router: paused; same model only; <n> accounts` and `Retry-After` copied from the upstream or `30` when absent.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_router_accounts.py
from opendaisugi.router_accounts import Accounts, plan_retry


def test_accounts_skip_unset_names():
    a = Accounts.from_env(["K1", "K2", "K3"], {"K1": "sk-a", "K3": "sk-c"})
    assert a.credentials() == [("K1", "sk-a"), ("K3", "sk-c")]


def test_retry_only_same_header_kind_and_only_api_keys():
    a = Accounts.from_env(["K1", "K2"], {"K1": "sk-a", "K2": "sk-b"})
    assert plan_retry(429, {"x-api-key": "sk-a"}, a, tried=1).value == "sk-b"
    assert plan_retry(429, {"authorization": "Bearer oauth-token"}, a, tried=1) is None
    assert plan_retry(500, {"x-api-key": "sk-a"}, a, tried=1) is None
    assert plan_retry(429, {"x-api-key": "sk-a"}, a, tried=2) is None


async def test_failover_same_url_same_model(tmp_path, monkeypatch):
    # the fake upstream answers 429 to the first key and 200 to the second; both hits share the URL and the model
    from tests.test_router_model import _gateway, _reply, OPUS
    import httpx, json
    from opendaisugi.router_asgi import make_router_app

    monkeypatch.setenv("K1", "sk-a")
    monkeypatch.setenv("K2", "sk-b")
    hits = []

    def handler(req):
        hits.append((str(req.url), req.headers["x-api-key"], json.loads(req.content)["model"]))
        return (
            httpx.Response(429, headers={"retry-after": "7"})
            if req.headers["x-api-key"] == "sk-a"
            else _reply("ok")
        )

    app = make_router_app(
        _gateway(tmp_path),
        upstream_base_url="http://up",
        transport=httpx.MockTransport(handler),
        accounts=["K1", "K2"],
    )
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://r") as c:
        r = await c.post(
            "/v1/messages",
            json={"model": OPUS, "max_tokens": 5, "messages": [{"role": "user", "content": "hi"}]},
            headers={"x-api-key": "sk-a"},
        )
    assert r.status_code == 200
    assert hits == [
        ("http://up/v1/messages", "sk-a", OPUS),
        ("http://up/v1/messages", "sk-b", OPUS),
    ]


async def test_pause_header_when_no_second_account(tmp_path):
    from tests.test_router_model import _gateway, OPUS
    import httpx
    from opendaisugi.router_asgi import make_router_app

    app = make_router_app(
        _gateway(tmp_path),
        upstream_base_url="http://up",
        transport=httpx.MockTransport(lambda req: httpx.Response(429, json={"e": 1})),
    )
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://r") as c:
        r = await c.post(
            "/v1/messages",
            json={"model": OPUS, "max_tokens": 5, "messages": [{"role": "user", "content": "hi"}]},
            headers={"x-api-key": "sk-a"},
        )
    assert r.status_code == 429 and r.json() == {"e": 1}
    assert (
        r.headers["x-daisugi-router"] == "paused; same model only; 1 accounts"
        and r.headers["retry-after"] == "30"
    )
```
(Use the keyword the asgi factory already takes for a transport; if it takes none, add `transport=` for tests as `test_router_model.py` does.)

- [ ] **Step 2: Run to verify they fail**

Run: `uv run --no-sync pytest -q tests/test_router_accounts.py -x`
Expected: FAIL (module missing).

- [ ] **Step 3: Implement**

The retry re-sends the same bytes to the same URL with the one credential header swapped; at most one try per further account; the record's `account_failovers` is the number of swaps. A turn that was downgraded and gets a 4xx keeps today's `used_original` path first (same model request as the harness's), and failover applies to that re-send too. The streaming path: a 429 arrives before any body byte, so the retry happens before the client saw anything; after the first byte no retry.

- [ ] **Step 4: Run and commit**

Run: `uv run --no-sync pytest -q tests/test_router_accounts.py tests/test_router_asgi.py tests/test_router_upstream.py -x`
Expected: PASS.

```bash
git add src/opendaisugi/router_accounts.py src/opendaisugi/router_asgi.py src/opendaisugi/config.py src/opendaisugi/router_journal.py tests/test_router_accounts.py
git commit -m "router: on a quota or server failure, fail over across accounts for the same model only, else return the status with a pause header (W3R-4)"
```

### Task 5: The meter for part 1 (escalations, cache lost, failovers)

**Files:**
- Modify: `src/opendaisugi/router_status.py` (`weekly`, `week_line`, `markdown`)
- Test: `tests/test_router_status.py`

**Interfaces:**
- Consumes: `TurnRecord` JSON lines with `escalated`, `escalation_reason`, `cache_tokens_lost_est`, `cheap_first`, `account_failovers`.
- Produces: per week: `escalations.count`, `escalations.by_reason` keyed by the kind before the colon, `escalations.cache_tokens_lost_est`, `cheap_first_turns`, `account_failovers`; the week is `estimated` when any escalation carries a cache estimate. `week_line` adds `escalations N (warden_deny 2, verify_failed 1; ~T cache tokens lost, est); failovers F`.

- [ ] **Step 1: Write the failing test**

```python
def test_weekly_counts_escalations(tmp_path):
    import json
    from opendaisugi import router_status

    d = tmp_path / "router"
    d.mkdir()
    base = {
        "created_at": "2026-11-02T10:00:00Z",
        "signature": "s",
        "task": "t",
        "tier": "tier2-frontier",
        "requested_model": "o",
        "model": "o",
        "difficulty": 0.5,
        "downgraded": False,
        "estimated": True,
        "input_tokens": 1,
        "output_tokens": 1,
        "frontier_tokens_saved": 0,
        "actual_dollars": 0.0,
        "counterfactual_dollars": 0.0,
    }
    lines = [
        dict(base, escalated=True, escalation_reason="warden_deny: x", cache_tokens_lost_est=1200),
        dict(base, escalated=True, escalation_reason="verify_failed: y", cache_tokens_lost_est=300),
        dict(base, cheap_first=True, downgraded=True, account_failovers=1),
    ]
    (d / "turns.jsonl").write_text("".join(json.dumps(x) + "\n" for x in lines))
    w = router_status.weekly(tmp_path)[-1]
    assert w["escalations"] == {
        "count": 2,
        "by_reason": {"warden_deny": 1, "verify_failed": 1},
        "cache_tokens_lost_est": 1500,
    }
    assert w["cheap_first_turns"] == 1 and w["account_failovers"] == 1 and w["estimated"]
    assert (
        "escalations 2 (verify_failed 1, warden_deny 1; ~1,500 cache tokens lost, est); failovers 1"
        in router_status.week_line(w)
    )
```

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `uv run --no-sync pytest -q tests/test_router_status.py -x`
Reasons in `by_reason` are listed sorted by name. The thousands separator is the one `week_line` uses already.

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/router_status.py tests/test_router_status.py
git commit -m "router status: escalations by reason, the cache tokens an escalation loses (estimated), cheap-first turns and account failovers, per week"
```

### Task 6: Go and Rust: part 1, failover, the meter; router golden cases

**Files:**
- Modify: Go `clients/go/internal/router/pipeline.go`, `server.go`, `report.go`, a new `escalate.go`, `accounts.go`; `internal/cli/routingmeasure.go`; `internal/mcpwire` (the `escalation_signal` key); `internal/warden/` (the deny prefix constant)
- Modify: Rust `clients/rust/src/router/pipeline.rs`, `server.rs`, `report.rs`, new `escalate.rs`, `accounts.rs`; `src/cli/routermeasure.rs`; `src/mcpwire`; `src/warden/`
- Modify: `clients/router_cases.py` (new cases), `clients/fixtures/router/` regenerated, `clients/ADJUDICATIONS.md`
- Test: Go `escalate_test.go`, `accounts_test.go`; Rust unit tests in the same files

**Interfaces:**
- Consumes: Task 2 to 5 behaviour, byte for byte: reason lines, journal keys, the pause header, the Markdown header line.
- Produces: the same functions in Go (`EscalationSignals(body any, after int) []Signal`, `PlanRetry(...)`) and Rust (`escalation_signals`, `plan_retry`), and the fixture `clients/fixtures/router/cases.jsonl` grown by the cases below.

Cases to add to `clients/router_cases.py` (`build_proxy_cases`): `cheap first: first ask goes to the cheap rung`, `cheap first: continuation stays`, `escalate: warden deny in an error result`, `escalate: quoted deny is not a signal`, `escalate: verify_failed from our tool only`, `escalate: once; later asks stay on the requested model`, `escalate: off keeps the cheap model`, `failover: second account same url same model`, `failover: oauth token gets the pause header`, `failover: no second account gets the pause header`; and in `router_measure_cases`: `router status: escalations and failovers by week`, `router status --markdown`.

- [ ] **Step 1: Write the cases from the oracle**

Run: `uv run --no-sync python clients/router_cases.py --only "cheap first*" --only "escalate*" --only "failover*" --only "router status*"`
Expected: the manifest count grows by 12; `git diff --stat clients/fixtures/router/` shows the two files.

- [ ] **Step 2: Write the failing Go unit tests**

```go
// clients/go/internal/router/escalate_test.go
func TestQuotedDenyIsNotASignal(t *testing.T) {
    body := mustJSON(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"log: openDaisugi warden: DENIED \u2014 x"}]}]}`)
    if got := EscalationSignals(body, 0); len(got) != 0 { t.Fatalf("got %v", got) }
}
func TestErrorDenyIsASignal(t *testing.T) {
    body := mustJSON(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"openDaisugi warden: DENIED \u2014 outside"}]}]}`)
    got := EscalationSignals(body, 0)
    if len(got) != 1 || got[0].Kind != "warden_deny" || got[0].Detail != "outside" { t.Fatalf("got %v", got) }
}
```
Mirror the two in Rust as `#[test] fn quoted_deny_is_not_a_signal()` and `error_deny_is_a_signal()`.

- [ ] **Step 3: Build and run Go, one heavy job**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd clients/go && go test -p 1 ./internal/router/... ./internal/warden/... ./internal/mcpwire/...'`
Expected: FAIL on the two new tests, then PASS after the port.

- [ ] **Step 4: Port, then compare Go against the oracle**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh` then
`systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/router_compare.py --binary clients/go/daisugi --oracle`
Expected: `0 disagreements`; refusals 0 or under a ruling.

- [ ] **Step 5: Rust, the same**

Run: `. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo test -j2 --release --manifest-path clients/rust/Cargo.toml router::` then the compare with `--binary clients/rust/target/release/daisugi`.
Expected: PASS; `0 disagreements`.

- [ ] **Step 6: Record and commit**

Add to `clients/ADJUDICATIONS.md` a dated section `the router escalates (RE-1 to RE-n)` with one ruling per reason-line format, the pause header, and any port difference found. Commit in three parts: oracle cases, Go, Rust.

```bash
git add clients/router_cases.py clients/fixtures/router/ clients/ADJUDICATIONS.md
git commit -m "router cases: cheap first, escalation, same-model failover and their weekly measure, written by the oracle"
git add clients/go/internal/router clients/go/internal/warden clients/go/internal/mcpwire clients/go/internal/cli
git commit -m "go router: part 1 and same-model failover, 0 disagreements against the oracle"
git add clients/rust/src/router clients/rust/src/warden clients/rust/src/mcpwire clients/rust/src/cli
git commit -m "rust router: part 1 and same-model failover, 0 disagreements against the oracle"
```

### Task 7: Part 2, the router fills each plan step's model

**Files:**
- Modify: `src/opendaisugi/routing.py` (add `route_step`, `StepRoute`)
- Modify: `src/opendaisugi/orchestrator.py:421` (`_apply_preferred_models` calls `route_step`)
- Modify: `src/opendaisugi/cli.py:3118` (`route --plan FILE [--json]`)
- Create: `src/opendaisugi/router_steps.py` (the steps journal)
- Test: `tests/test_route_step.py`, `tests/test_cli_route.py`

**Interfaces:**
- Consumes: `model_sizer.size_step`, `model_sizer.build_ladder`, `route_knn.OutcomeStore`, `route_knn.decide`, `route_knn.candidate_models`.
- Produces: `routing.StepRoute(step_id: str, model: str | None, tier: str, reason: str, difficulty: float)` and `routing.route_step(step, *, plan_model: str, ladder, stakes: str, store: OutcomeStore | None = None, embed=None, prices=None) -> StepRoute`. Rules: `stakes == "physical"` -> `model None`, reason `stakes physical: no delegated step`; else start from `size_step` (the adapter); with a store and an embedder, apply the knn decision (RT-R-3, RT-R-3a numbers) over candidates at or below the sizer's pick, requested = `plan_model`; never above `plan_model`; reason `step: <model>; sizer <tier>` or `step: <model>; knn <N> neighbours, <K> known` (the knn tail as RT-R-3 formats it).
- Produces: `router_steps.append(data_dir, plan_hash: str, routes: list[StepRoute])` writing `<data>/router/steps.jsonl` lines `{created_at, plan_hash, step_id, model, tier, reason, difficulty}`.
- Produces: `daisugi route --plan FILE [--json] [--knn]`: reads an `ActionPlan` JSON file, prints one line per step `<step_id>  <model or ->  <reason>`; `--json` prints the list of `StepRoute` dicts. Books nothing.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_route_step.py
from opendaisugi.routing import route_step
from opendaisugi.model_sizer import build_ladder
from opendaisugi.models import ShellStep


def _ladder():
    return build_ladder(frontier="opus", cheap="haiku", local=None)


def test_physical_stakes_routes_no_model():
    s = ShellStep(step_id="s1", command="ls")
    r = route_step(s, plan_model="opus", ladder=_ladder(), stakes="physical")
    assert r.model is None and r.reason == "stakes physical: no delegated step"


def test_sizer_is_the_adapter_without_a_store():
    s = ShellStep(step_id="s1", command="ls")
    r = route_step(s, plan_model="opus", ladder=_ladder(), stakes="medium")
    assert r.model in ("haiku", "opus") and r.reason.startswith(f"step: {r.model}; sizer ")


def test_never_above_the_plan_model():
    s = ShellStep(step_id="s1", command="ls")
    r = route_step(s, plan_model="haiku", ladder=_ladder(), stakes="medium")
    assert r.model == "haiku"
```

```python
# tests/test_cli_route.py (add)
def test_route_plan_prints_one_line_per_step(runner, tmp_path, sample_plan):
    p = tmp_path / "plan.json"
    p.write_text(sample_plan.model_dump_json())
    r = runner.invoke(app, ["route", "--plan", str(p), "--json"])
    assert r.exit_code == 0
    rows = json.loads(r.output)
    assert [x["step_id"] for x in rows] == [s.step_id for s in sample_plan.steps]
    assert all(set(x) >= {"step_id", "model", "tier", "reason", "difficulty"} for x in rows)
```

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `uv run --no-sync pytest -q tests/test_route_step.py tests/test_cli_route.py tests/test_orchestrator*.py -x`
`_apply_preferred_models(plan, sizings)` becomes `_apply_preferred_models(plan, routes: list[StepRoute])`; the orchestrator builds routes with `route_step` once per step (store and embedder from the data dir when `routing_mode` is `knn`, `model` or `learned`), appends them with `router_steps.append`, and the budget gate in `BudgetAwareDelegatingExecutor.run` stays as it is.

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/routing.py src/opendaisugi/orchestrator.py src/opendaisugi/router_steps.py src/opendaisugi/cli.py tests/test_route_step.py tests/test_cli_route.py
git commit -m "router part 2: one function fills each step's model, the sizer is its adapter, and route --plan shows the choice (RT-3)"
```

### Task 8: Go and Rust: `route --plan`

**Files:**
- Modify: Go `clients/go/internal/cli/routingcmd.go`, `internal/orchestrate/sizing.go` (export `SizeStep`), new `internal/router/step.go`; Rust `src/cli/routingcmd.rs`, new `src/router/step.rs` and a `sizing.rs` port of `size_step` and `build_ladder` (Rust has no orchestrate yet; port only the two functions)
- Modify: `clients/router_cases.py` (`build_cli_cases`: `route --plan` cases), fixtures regenerated

**Interfaces:**
- Consumes: Task 7's output format byte for byte.
- Produces: `daisugi route --plan FILE [--json]` in both binaries, 0 disagreements on the cases `route plan: three steps sizer only`, `route plan: physical stakes`, `route plan: knn store present`, `route plan: plan model is the cheap model`.

- [ ] **Step 1: Cases from the oracle**

Run: `uv run --no-sync python clients/router_cases.py --only "route plan*"`

- [ ] **Step 2: Port Go, build, compare (one heavy job each)**

Run: build as in Task 6, then `uv run --no-sync python clients/router_compare.py --binary clients/go/daisugi --only "route plan*"`
Expected: `0 disagreements`.

- [ ] **Step 3: Port Rust, build, compare**

Run: the Rust build and compare as in Task 6 with `--only "route plan*"`.
Expected: `0 disagreements`.

- [ ] **Step 4: Commit (oracle cases; Go; Rust)**

```bash
git commit -m "route --plan in Go and Rust: the step router agrees with the oracle on the plan cases"
```

### Task 9: Part 3a, shadow sampling bounded by rate and a daily token cap

**Files:**
- Create: `src/opendaisugi/router_shadow.py`
- Modify: `src/opendaisugi/route_knn.py` (`_SCHEMA`: columns `requested TEXT NOT NULL DEFAULT ''`, `shadow_of TEXT`; `_migrate` adds them to an older table; `record(..., requested=, shadow_of=)`)
- Modify: `src/opendaisugi/router_pipeline.py` (`finish` schedules a shadow), `src/opendaisugi/router_asgi.py` (runs it on the grader's worker thread), `src/opendaisugi/config.py` (`routing_shadow_rate: float = 0.0`, `routing_shadow_daily_tokens: int = 200000`)
- Test: `tests/test_router_shadow.py`, `tests/test_route_knn.py`

**Interfaces:**
- Consumes: the grader (`router_grader`), the helper request builders in `router_pipeline` (`grader_body`), `spawn.price_worker` for a local rung's cost (0).
- Produces: `router_shadow.should_shadow(turn_id: str, rate: float) -> bool`: `rate > 0` and `int(sha256(turn_id)[:8], 16) / 2**32 < rate`.
- Produces: `router_shadow.ShadowJournal(path)` over `<data>/router/shadows.jsonl` with `intent(turn_id, cheap_model, est_tokens, ts)` and `result(turn_id, grade, input_tokens, output_tokens, cost, elapsed_ms, ts)`; `spent_today(now) -> int` = the sum of result tokens for turns with a result, plus `est_tokens` for intents with no result, over lines whose `ts` falls on `now`'s UTC day.
- Produces: `router_shadow.shadow_turn(router, prepared, body, *, cheap_model, send, grade, journal, store, now) -> str | None`: writes the intent (est_tokens = `estimate_prefix_tokens(body) + max_tokens`), sends the same body with `model` swapped to `cheap_model`, grades the reply text with the grader prompt (RT-R-10, byte for byte), writes the result, and books a `turns` row (`requested = prepared.decision.requested_model`, `model = cheap_model`, `outcome = grade`, `source = "grader"`, `shadow_of = prepared turn id`, cost). Returns the grade.
- Conditions in `finish`: the turn has new human text, was not downgraded, ended with `end_turn`, has reply text, `router_grader` is set, `should_shadow`, and `spent_today + est_tokens <= routing_shadow_daily_tokens`. With `routing_shadow_rate > 0` and no grader, `router serve` prints once at start `shadow: needs router_grader; shadow sampling is off` and shadows nothing.
- Shadow prompt body: the turn's body with `model` replaced, nothing else changed (so the cheap model answers the same question).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_router_shadow.py
import json, time
from opendaisugi.router_shadow import ShadowJournal, should_shadow


def test_should_shadow_is_deterministic_and_bounded():
    ids = [f"t{i}" for i in range(2000)]
    share = sum(should_shadow(i, 0.05) for i in ids) / 2000
    assert 0.03 < share < 0.07 and should_shadow("t1", 0.05) == should_shadow("t1", 0.05)
    assert not any(should_shadow(i, 0.0) for i in ids)


def test_shadow_cap_counts_intent_rows(tmp_path):
    j = ShadowJournal(tmp_path / "shadows.jsonl")
    day = 1_800_000_000.0
    j.intent("t1", "haiku", est_tokens=5000, ts=day)  # crashed: no result
    j.intent("t2", "haiku", est_tokens=100, ts=day)
    j.result("t2", "good", 60, 40, 0.0, 900.0, ts=day + 1)
    j.intent("t3", "haiku", est_tokens=9000, ts=day - 86400)  # yesterday
    assert j.spent_today(now=day + 2) == 5100
```

```python
# tests/test_route_knn.py (add)
def test_schema_gains_requested_and_shadow_of_and_migrates(tmp_path):
    import sqlite3

    p = tmp_path / "routing.db"
    con = sqlite3.connect(p)
    con.executescript(OLD_SCHEMA_V1)
    con.close()  # the RT-R-1 table, copied into the test
    store = rk.OutcomeStore(p)
    tid = store.record(
        conversation="c",
        ts=1.0,
        task="x",
        vec=VEC,
        embedder="fake",
        model="haiku",
        cost=0.0,
        latency_ms=1.0,
        requested="opus",
        shadow_of="t9",
    )
    row = store.recent("fake")[0]
    assert row.requested == "opus" and row.shadow_of == "t9" and row.turn_id == tid
```
And an asgi test in `tests/test_router_model.py` style: a frontier turn with `routing_shadow_rate=1.0` and a grader produces exactly three upstream requests (turn, shadow on `HAIKU`, grade), one `shadows.jsonl` result line, and a `turns` row with `shadow_of` set and `outcome == "good"`.

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `systemd-run --user --scope -q -p MemoryMax=5G uv run --no-sync pytest -q tests/test_router_shadow.py tests/test_route_knn.py tests/test_router_model.py tests/test_knn_golden.py -x`
`knn_golden.json` must still pass: the two new columns do not change any score (RT-R-6).

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/router_shadow.py src/opendaisugi/route_knn.py src/opendaisugi/router_pipeline.py src/opendaisugi/router_asgi.py src/opendaisugi/config.py tests/test_router_shadow.py tests/test_route_knn.py tests/test_router_model.py
git commit -m "router part 3: shadow a sampled frontier turn on the cheap rung, grade it, and book it as a neighbour; bounded by a rate and a daily token cap (W3R-5)"
```

### Task 10: Part 3b, the learned chooser and `routing_mode: learned`

**Files:**
- Create: `src/opendaisugi/router_chooser.py`
- Modify: `src/opendaisugi/router_pipeline.py` (`ROUTER_MODES` gains `"learned"`; `_prepare_learned`), `src/opendaisugi/config.py` (`routing_confidence: float = 0.8`), `src/opendaisugi/cli.py` (`router train [--json] [--force]`; `install --router --routing-mode learned`), `src/opendaisugi/router_status.py` (the `chooser` block)
- Test: `tests/test_router_chooser.py`, `tests/test_router_learned.py`

**Interfaces:**
- Consumes: `route_knn.OutcomeStore.recent(embedder)` rows with `requested`, `model`, `outcome`, `vec`, `seq`.
- Produces: `router_chooser.label_rows(rows) -> list[tuple[vec, int, seq]]`: rows where `requested != ""`, `model != requested`, `outcome in (good, bad)`; label 1 for good.
- Produces: `router_chooser.train(samples, *, dim: int) -> tuple[list[float], float]`: logistic regression by batch gradient descent, float64, `w = 0`, `b = 0`, learning rate `0.5`, `300` iterations, L2 `1e-3` on `w`, samples in ascending `seq`, every sum left to right, outputs rounded to 9 decimals (`floor(x * 1e9 + 0.5) / 1e9`).
- Produces: `router_chooser.Chooser(weights, bias, embedder, dim, trained_at, n_train, n_held, held_out_accuracy, precision_at_threshold)` with `save(path)` / `load(path)` over `<data>/router/chooser.json` and `p(vec) -> float` = `1 / (1 + exp(-(w . v + b)))` rounded to 6 decimals (RT-R-6 rounding).
- Produces: `router_chooser.fit_and_compare(rows, *, embedder, threshold, old: Chooser | None, held_out=0.2, min_rows=50) -> Report | str`: refuses with `need at least 50 labelled cheap rows; have N`; the newest 20% by `seq` are held out (at least 10); `accuracy = correct / held`; `precision_at_threshold = good / predicted-cheap among held-out rows with p >= threshold` (None when none); the report says `replace: True` when `old is None or new.accuracy >= old.accuracy`.
- Produces: `daisugi router train [--json] [--force]`: prints `trained on N rows, held out H: accuracy new A (in force B); precision at 0.80: P; chooser.json written` or `... kept the chooser in force`; writes only on replace or `--force`. Exit 1 on refusal, with the refusal line.
- Produces: routing mode `learned`: needs a `KnnRouter` (for the embedder) and a chooser file; with no file the mode falls to the rules router with reason prefix `learned: no chooser trained; rules router: `; with text and candidates (RT-R-4) it downgrades to the first candidate when `p >= routing_confidence`, reason `learned: p(cheap passes)=0.83 >= 0.80; <model>`; else `learned: p(cheap passes)=0.41 < 0.80; <requested>`; stickiness as knn; the turn is booked (so escalation and the grader label it).
- Golden: `clients/fixtures/router/chooser_golden.json` written by a new `clients/chooser_golden.py` from a fixed table of 80 rows (seeded, 4 dimensions): the weights, bias, and `p` for 10 probe vectors; a port may differ by `5e-7` on `p` and `1e-7` on weights.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_router_chooser.py
import numpy as np
from opendaisugi.router_chooser import train, Chooser, fit_and_compare, label_rows


def _sep(n=60, seed=3):
    rng = np.random.default_rng(seed)
    xs = rng.normal(size=(n, 4)).astype("<f4")
    xs /= np.linalg.norm(xs, axis=1, keepdims=True)
    return [(xs[i], int(xs[i][0] > 0), i) for i in range(n)]


def test_train_is_deterministic_and_separates():
    s = _sep()
    w1, b1 = train(s, dim=4)
    w2, b2 = train(s, dim=4)
    assert (w1, b1) == (w2, b2) and w1[0] > 1.0
    c = Chooser(w1, b1, "fake", 4, "2026-11-02T00:00:00Z", 60, 0, None, None)
    assert c.p(np.array([1, 0, 0, 0], "<f4")) > 0.9 > c.p(np.array([-1, 0, 0, 0], "<f4"))


def test_fit_refuses_under_fifty_rows():
    rows = [
        FakeRow(vec=v, outcome="good" if y else "bad", requested="opus", model="haiku", seq=i)
        for v, y, i in _sep(40)
    ]
    assert (
        fit_and_compare(rows, embedder="fake", threshold=0.8, old=None)
        == "need at least 50 labelled cheap rows; have 40"
    )


def test_fit_holds_out_the_newest_fifth_and_replaces_only_when_not_worse():
    rows = [
        FakeRow(vec=v, outcome="good" if y else "bad", requested="opus", model="haiku", seq=i)
        for v, y, i in _sep(100)
    ]
    rep = fit_and_compare(rows, embedder="fake", threshold=0.8, old=None)
    assert rep.n_train == 80 and rep.n_held == 20 and rep.replace
    worse = Chooser([0, 0, 0, 0], 0.0, "fake", 4, "x", 1, 1, 1.0, None)
    assert not fit_and_compare(rows, embedder="fake", threshold=0.8, old=worse).replace


def test_golden_file_matches():
    import json

    g = json.load(open("clients/fixtures/router/chooser_golden.json"))
    w, b = train([(np.array(r["vec"], "<f4"), r["y"], i) for i, r in enumerate(g["rows"])], dim=4)
    assert w == g["weights"] and b == g["bias"]
```
`FakeRow` is a small dataclass in the test with the `KnnRow` fields used. `tests/test_router_learned.py`: three pipeline tests in the `test_router_model.py` style: no chooser -> rules reason prefix; `p` above the threshold downgrades and books; `p` below keeps the requested model.

- [ ] **Step 2: Run (FAIL), implement, write the golden, run (PASS)**

Run: `uv run --no-sync python clients/chooser_golden.py && uv run --no-sync pytest -q tests/test_router_chooser.py tests/test_router_learned.py tests/test_cli_route.py -x`

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/router_chooser.py src/opendaisugi/router_pipeline.py src/opendaisugi/config.py src/opendaisugi/cli.py src/opendaisugi/router_status.py clients/chooser_golden.py clients/fixtures/router/chooser_golden.json tests/test_router_chooser.py tests/test_router_learned.py
git commit -m "router part 3: a learned chooser over the matcher's vectors, trained deterministically, replaced only when it wins held out; routing_mode learned (W3R-6, W3R-7; retires RT-R-12)"
```

### Task 11: Go and Rust: shadow, learned mode, train; router cases

**Files:**
- Modify: Go `internal/router/knn.go` (columns, migration), new `shadow.go`, `chooser.go`; `server.go`; `internal/cli/routercmd.go` (`router train`); Rust `src/router/knn.rs`, new `shadow.rs`, `chooser.rs`; `server.rs`; `src/cli/routercmd.rs`
- Modify: `clients/router_cases.py` (cases: `shadow: sampled frontier turn is graded and booked`, `shadow: cap reached writes nothing`, `shadow: no grader says so once`, `learned: no chooser falls to rules`, `learned: above threshold downgrades`, `learned: below threshold keeps`, `router train: refuses under 50`, `router train: writes chooser.json`, `router train: keeps the one in force`), fixtures regenerated; Go and Rust golden tests read `chooser_golden.json` as `knn_golden.json` is read today

**Interfaces:**
- Consumes: Tasks 9 and 10 byte for byte (prompts, reason lines, the `chooser.json` keys and rounding, the `shadows.jsonl` keys).

- [ ] **Step 1: Cases from the oracle**

Run: `uv run --no-sync python clients/router_cases.py --only "shadow*" --only "learned*" --only "router train*"`

- [ ] **Step 2: Go golden test, port, build, compare (one heavy job at a time)**

```go
func TestChooserGolden(t *testing.T) {
    g := loadChooserGolden(t)
    w, b := TrainChooser(g.Samples(), 4)
    for i := range w { if math.Abs(w[i]-g.Weights[i]) > 1e-7 { t.Fatalf("w[%d]", i) } }
    if math.Abs(b-g.Bias) > 1e-7 { t.Fatal("bias") }
}
```
Run: `go test -p 1 ./internal/router/...` under the cap; then build; then `clients/router_compare.py --binary clients/go/daisugi --oracle`. Expected: `0 disagreements`.

- [ ] **Step 3: Rust the same**

Expected: `0 disagreements`.

- [ ] **Step 4: Record and commit (oracle cases; Go; Rust)**

Add rulings `LC-1 to LC-n` (the chooser's constants and rounding, the shadow journal, the amended RT-R-5) to `clients/ADJUDICATIONS.md`.

### Task 12: Graft rules v2 and the proof at install

**Files:**
- Modify: `src/opendaisugi/spawn.py` (`Rule` gains `shape: str`, `match: dict`, `conditions: dict`, `replace: dict`, `proof: dict | None`, `provenance: dict`; `parse_rule` accepts v2; `SHAPES = ("deny_redirect", "shell_rewrite", "output_replace")`; `RULE_STATES = ("proposed", "audit", "trial", "active", "retired")`)
- Create: `src/opendaisugi/graft_proof.py`
- Modify: `src/opendaisugi/graft_install.py` (`install_rule(root, rule: Rule, *, envelope, home, cwd) -> Installed | Refused`; `prove_all(root, envelope) -> list[tuple[Rule, Proof]]`), `src/opendaisugi/cli.py` (`graft install --rule FILE`, `graft prove [--id ID] [--json]`, `graft status` shows the proof), `src/opendaisugi/effects.py` (class `network_send`, permanent; head `daisugi-spawn` is `read` with a local route)
- Test: `tests/test_graft_proof.py`, `tests/test_graft_install.py`

**Interfaces:**
- Produces: the rule file (JSON) keys: `id`, `version`, `shape`, `state`, `match` (`tool`, `command_heads`, `single_simple_command`, `operands`, `file_lines_over`), `conditions` (`stakes_max`, `harness`, `not_after_edit_intent`, `recent_calls` default 5, `skip_host_prompt` default false, `worker_timeout_s` default 120, `after_timeout_s` default 60), `replace` (`command` template with `{operands}`), `worker` (`choose`, `allow_remote`), `provenance` (`proposed_by`, `evidence`, `proof`), `trial` (`seed`). A v1 file (no `match.command_heads`, `shape deny_redirect`) parses unchanged.
- Produces: `graft_proof.Proof(result: Literal["holds","fails","unknown"], reason: str, counterexample: str | None, envelope_sha256: str, checked_at: str)` and `graft_proof.prove(rule: Rule, envelope: Envelope, *, remote_host: str | None = None, timeout_ms: int = 2000) -> Proof`.
- Proof steps, in order, first failure wins: (1) `envelope.stakes == "physical"` -> fails `physical stakes: no graft`; (2) `rule.conditions.stakes_max` below the envelope's stakes -> fails; (3) the contract envelope: `permissions.shell_allowlist = ["daisugi-spawn"]`, `file_read = envelope.permissions.file_read`, `file_write = []`, `network = [remote_host]` when `rule.worker.allow_remote and remote_host` else `[]`, same stakes; `envelope_subsumes(outer=envelope, inner=contract, timeout_ms)`; a `VerificationTimeout` -> `unknown`; a failed result -> `fails` with its reason and counterexample; (4) effect tier: `tier_for(class of the replacement)` must not be worse than `tier_for("read")` unless the route is remote and the envelope's network grants the host (then `network_send`, permanent, is accepted because the envelope named it); (5) `rule.conditions.worker_timeout_s <= envelope.max_execution_time_s` when that limit is set, else fails `worker timeout 120 s exceeds the envelope's 30 s`; (6) `deny_redirect` and `output_replace` skip step 4's head check and use the same contract. `envelope_sha256` is the SHA-256 of the registered envelope file's bytes.
- Produces: `graft install --rule FILE` parses, proves against the default registered envelope of the warden root (or `--workspace`), refuses on `fails` or `unknown` with the proof's reason on one line (exit 1), refuses beside a rival hook (unchanged), writes the file with `provenance.proof` filled and `version` bumped. `graft prove` re-proves every rule and rewrites each proof block; prints one line per rule `<id> v<n>: holds|fails|unknown (<reason>)`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_graft_proof.py
from opendaisugi.graft_proof import prove
from opendaisugi.spawn import parse_rule
from opendaisugi.models import Envelope

SHELL = {
    "id": "big-cat",
    "version": 1,
    "shape": "shell_rewrite",
    "state": "audit",
    "match": {
        "command_heads": ["cat", "head", "tail"],
        "single_simple_command": True,
        "operands": "files_only",
        "file_lines_over": 350,
    },
    "conditions": {
        "stakes_max": "high",
        "harness": ["claude", "codex", "pi", "opencode"],
        "not_after_edit_intent": True,
    },
    "replace": {"command": "daisugi-spawn read --digest -- {operands}"},
    "worker": {"choose": "router", "allow_remote": False},
}


def _env(**kw):
    p = {
        "file_read": ["/work/**"],
        "shell_allowlist": ["cat", "daisugi-spawn"],
        **kw.pop("perm", {}),
    }
    return Envelope(
        generated_by="t", task="t", stakes=kw.pop("stakes", "medium"), permissions=p, **kw
    )


def test_local_rule_holds():
    pr = prove(parse_rule(SHELL, "f"), _env())
    assert pr.result == "holds" and len(pr.envelope_sha256) == 64


def test_physical_fails_first():
    assert prove(parse_rule(SHELL, "f"), _env(stakes="physical")).result == "fails"


def test_remote_without_network_grant_fails():
    r = parse_rule({**SHELL, "worker": {"choose": "router", "allow_remote": True}}, "f")
    pr = prove(r, _env(), remote_host="worker.invalid")
    assert pr.result == "fails" and "worker.invalid" in pr.reason


def test_worker_timeout_over_the_envelope_limit_fails():
    pr = prove(parse_rule(SHELL, "f"), _env(max_execution_time_s=30))
    assert pr.result == "fails" and pr.reason == "worker timeout 120 s exceeds the envelope's 30 s"


def test_v1_rule_still_parses():
    r = parse_rule(
        {
            "id": "big-read",
            "version": 1,
            "shape": "deny_redirect",
            "state": "active",
            "match": {"tool": "Read", "file_lines_over": 5},
        },
        "f",
    )
    assert r.shape == "deny_redirect" and r.min_lines == 5
```

```python
# tests/test_graft_install.py (add)
def test_install_rule_refuses_an_unproved_rule(tmp_path, runner):
    # a registered envelope with stakes physical, then: daisugi graft install --rule shell.json
    ...
    assert (
        r.exit_code == 1 and r.output.strip() == "Refused: proof fails: physical stakes: no graft"
    )


def test_install_rule_writes_the_proof(tmp_path, runner):
    ...
    doc = json.loads((root / "grafts" / "big-cat.json").read_text())
    assert doc["provenance"]["proof"]["result"] == "holds" and doc["version"] == 1
```

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `systemd-run --user --scope -q -p MemoryMax=5G uv run --no-sync pytest -q tests/test_graft_proof.py tests/test_graft_install.py tests/test_warden_graft.py tests/test_spawn.py -x`

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/spawn.py src/opendaisugi/graft_proof.py src/opendaisugi/graft_install.py src/opendaisugi/cli.py src/opendaisugi/effects.py tests/test_graft_proof.py tests/test_graft_install.py
git commit -m "grafts: rule files carry a shape, a match, conditions and a proof; install proves the rule against the envelope in force with Z3 subsumption and refuses what it cannot prove (W3R-9)"
```

### Task 13: `daisugi-spawn`, its own head

**Files:**
- Create: `src/opendaisugi/spawn_cli.py`; `pyproject.toml` `[project.scripts] daisugi-spawn = "opendaisugi.spawn_cli:main"`
- Modify: `src/opendaisugi/spawn.py` (`DIGEST_QUESTION = "Outline this file: its sections or symbols, each with one exact quote, and what each part does."`, `run_digest(path, *, data_dir, env) -> Result`)
- Modify: the skill text `references/when-to-spawn.md` (one paragraph: the two commands)
- Test: `tests/test_spawn_cli.py`

**Interfaces:**
- Produces: `daisugi-spawn read --digest -- FILE...`: for each file, `run_digest`; prints the fenced digest (`spawn.fence(text, "daisugi-spawn digest: <path>")`) followed by `exact_text_note(min_lines)` naming `daisugi-spawn read --raw -- <path> --from N --lines M`; a refused file prints `daisugi-spawn: <path>: <reason>` to stderr and the run exits 1 after the other files. `read --raw -- FILE [--from N] [--lines M]` prints the lines as they are (no worker). Exit 2 on usage errors. `--data-dir` defaults to `data_home()`. Each digest is one `delegations.jsonl` row with `mode = "digest"` and `caller = "daisugi-spawn"`.
- Produces: `spawn_cli.main(argv: list[str] | None = None) -> int`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_spawn_cli.py
from opendaisugi import spawn_cli


def test_raw_prints_the_lines(tmp_path, capsys):
    f = tmp_path / "a.py"
    f.write_text("".join(f"l{i}\n" for i in range(20)))
    assert spawn_cli.main(["read", "--raw", "--from", "3", "--lines", "2", "--", str(f)]) == 0
    assert capsys.readouterr().out == "l2\nl3\n"


def test_digest_with_a_fake_worker(tmp_path, capsys, fake_worker):
    # fake_worker: the httpx MockTransport fixture test_spawn.py uses, answering one quote that is in the file
    f = tmp_path / "a.py"
    f.write_text("def alpha():\n    return 1\n" * 200)
    rc = spawn_cli.main(["read", "--digest", "--data-dir", str(fake_worker.data_dir), "--", str(f)])
    out = capsys.readouterr().out
    assert (
        rc == 0
        and out.startswith("```daisugi-spawn digest: ")
        and "daisugi-spawn read --raw -- " in out
    )


def test_usage_error_exits_2(capsys):
    assert spawn_cli.main(["read"]) == 2
```

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `uv run --no-sync pytest -q tests/test_spawn_cli.py tests/test_spawn.py -x`

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/spawn_cli.py src/opendaisugi/spawn.py pyproject.toml uv.lock tests/test_spawn_cli.py src/opendaisugi/skills/opendaisugi-checklist/references/when-to-spawn.md
git commit -m "daisugi-spawn: the spawn tool as its own shell head, so an envelope can allow it alone (GR-4, W3R-10)"
```
(Use the skill's real path under `src/opendaisugi/`; `_link_skill` in `install.py` names it.)

### Task 14: The rewrite verdict, shape (a) shell rewrite

**Files:**
- Modify: `src/opendaisugi/warden.py` (`GateDecision.rewritten_input: dict | None = None`; new `_maybe_rewrite`; dispatch after `_maybe_graft`; `_outcome` emission; `_log_audit` records `original_input` and `rewritten_input`; `REWRITE_FORMATS = frozenset({"claude", "codex", "pi", "opencode"})`)
- Modify: `src/opendaisugi/hook.py` (`--format codex`: the deny path as claude; the rewrite path per W3R-8), `src/opendaisugi/install.py` (`_patch_codex_warden` writes `--format codex`)
- Modify: `src/opendaisugi/harness_pi/extension/index.ts`, `PINS.md`; `src/opendaisugi/harness_opencode/plugin/daisugi-warden.ts` (read `stdout`, apply `rewrite.command`; fix the header comment)
- Test: `tests/test_warden_rewrite.py`, `tests/test_hook_format_opencode.py`, `tests/test_opencode_pins.py`, `tests/test_install_codex.py`

**Interfaces:**
- Consumes: `spawn.acting_rule`, `graft_proof` (the stored proof), `shell_decompose` (one simple command, head, operands), `evaluate_record`, `route_spawn`, `effects`.
- Produces: `_maybe_rewrite(root, payload, decision, envelope, fmt, *, verify_timeout_s, session_id, now) -> GateDecision`. The fail-closed list; the first failing item sets `graft.applied = False` with its `why` and returns the original decision with the `graft` record attached:
  1. `fmt in REWRITE_FORMATS`; else `why = "host format <fmt> cannot carry a rewrite"`.
  2. `decision.allow and not decision.would_deny and decision.mode == "enforce" and not decision.ask`; else `why` names which.
  3. an acting rule of shape `shell_rewrite` (state `active`, or `trial` with the session in the graft arm; `audit` records only, `why = "the rule is in audit"`).
  4. `rule.proof.result == "holds"` and `rule.proof.envelope_sha256` equals the SHA-256 of the envelope file in force; else `why = "envelope changed since the proof; run daisugi graft prove"`.
  5. `fmt == "codex"` needs `conditions.skip_host_prompt`; else `why = "codex takes a rewrite only with allow; the rule does not skip the host prompt"`.
  6. `decision.step_type == "shell"` and the command decomposes to exactly one simple command with no redirect, pipe, substitution or list; head in `match.command_heads`; every operand that is not an option is an existing regular file after resolving symlinks from the call's cwd; the largest has more than `file_lines_over` lines (`spawn.measure`).
  7. `not_after_edit_intent`: none of the operands' resolved paths appears as the target of a `Write`, `Edit`, `MultiEdit`, `NotebookEdit`, `apply_patch` or a shell write in the session's last `recent_calls` audit records; else `why = "the file was edited in the last N calls"`.
  8. `route_spawn(...)` ok; else its reason.
  9. the rewritten command = `replace.command` with `{operands}` replaced by the `shlex.quote`d resolved paths joined by spaces; run through `evaluate_record` as a `shell` record with the same cwd and session; must be `allow` and not `would_deny`; else `why = "the rewritten call would be denied: <reason>"`.
  On success: `replace(decision, rewritten_input={"command": new}, graft={..., "shape": "shell_rewrite", "applied": True, "original_input": {"command": old}, "rewritten_input": {"command": new}, "lines": n, "worker": route.as_dict()})`.
- Produces: `_outcome`: for a decision with `rewritten_input` and `graft.applied`: claude -> stdout `{"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "ask" | "allow", "permissionDecisionReason": "daisugi graft <id> v<n>: <old head> on <n> lines goes to daisugi-spawn", "updatedInput": {"command": new}}}`, exit 0 (`allow` only with `skip_host_prompt`); codex -> the same with `permissionDecision: "allow"`; pi and opencode -> stdout `{"rewrite": {"command": new}, "rule": "<id> v<n>"}`, exit 0 (the resident warden's socket reply carries it as `stdout`).
- Produces: the pi extension sets `event.input.command = reply.rewrite.command` when `stdout` parses and has `rewrite`; the OpenCode plugin sets `output.args.command` likewise; both only when `exit_code == 0`. `PINS.md` gains the two lines. The OpenCode header comment now reads: `OpenCode's tool.execute.before hook can deny, by a throw, or change the call's arguments, by mutating output.args. This plugin does both and never grants anything.`
- Produces: `_log_audit` writes `graft.original_input` and `graft.rewritten_input` on every rewrite record (applied or not; for a not-applied record `rewritten_input` is the command that would have run when the attempt got that far, else absent).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_warden_rewrite.py
import json, os
from pathlib import Path
from opendaisugi import warden
from tests.test_warden_graft import (
    _setup as _setup_redirect,
)  # the fixture helper of the part-0 tests
from tests.test_graft_proof import SHELL


def _setup(tmp_path, *, state="active", lines=400, skip_prompt=False, fmt="claude", **perms):
    root, f = _setup_redirect(
        tmp_path, rule=None, lines=lines, shell_allowlist=["cat", "head", "daisugi-spawn"], **perms
    )
    from opendaisugi.graft_install import install_rule
    from opendaisugi.spawn import parse_rule

    r = dict(SHELL, state=state)
    r["conditions"] = dict(SHELL["conditions"], skip_host_prompt=skip_prompt)
    out = install_rule(
        root, parse_rule(r, "f"), envelope=warden.load_envelope(root), home=tmp_path, cwd=tmp_path
    )
    assert not isinstance(out, str), out
    return root, f


def _bash(cmd, cwd):
    return json.dumps(
        {"session_id": "s1", "cwd": str(cwd), "tool_name": "Bash", "tool_input": {"command": cmd}}
    ).encode()


def test_rewrite_emits_ask_and_updated_input(tmp_path):
    root, f = _setup(tmp_path)
    out = warden.run_hook(_bash(f"cat {f}", f.parent), root=root, mode="enforce", fmt="claude")
    assert out.exit_code == 0
    h = json.loads(out.stdout)["hookSpecificOutput"]
    assert h["permissionDecision"] == "ask" and h["updatedInput"] == {
        "command": f"daisugi-spawn read --digest -- {f}"
    }
    rec = warden.last_audit_record(root)
    assert (
        rec["graft"]["applied"]
        and rec["graft"]["original_input"] == {"command": f"cat {f}"}
        and rec["graft"]["rewritten_input"] == h["updatedInput"]
    )


def test_skip_host_prompt_emits_allow(tmp_path):
    root, f = _setup(tmp_path, skip_prompt=True)
    out = warden.run_hook(_bash(f"cat {f}", f.parent), root=root, mode="enforce", fmt="claude")
    assert json.loads(out.stdout)["hookSpecificOutput"]["permissionDecision"] == "allow"


def test_codex_without_skip_is_audit_only(tmp_path):
    root, f = _setup(tmp_path)
    out = warden.run_hook(_bash(f"cat {f}", f.parent), root=root, mode="enforce", fmt="codex")
    assert out.exit_code == 0 and out.stdout == json.dumps({"continue": True})
    assert warden.last_audit_record(root)["graft"]["why"].startswith(
        "codex takes a rewrite only with allow"
    )


def test_pipe_is_not_rewritten(tmp_path):
    root, f = _setup(tmp_path)
    out = warden.run_hook(
        _bash(f"cat {f} | head", f.parent), root=root, mode="enforce", fmt="claude"
    )
    assert (
        out.stdout == json.dumps({"continue": True})
        and warden.last_audit_record(root)["graft"]["applied"] is False
    )


def test_rewrite_quotes_operands(tmp_path):
    root, f = _setup(tmp_path)
    odd = f.parent / "a b'c.py"
    odd.write_text(f.read_text())
    out = warden.run_hook(_bash(f"cat '{odd}'", f.parent), root=root, mode="enforce", fmt="claude")
    import shlex

    assert (
        json.loads(out.stdout)["hookSpecificOutput"]["updatedInput"]["command"]
        == f"daisugi-spawn read --digest -- {shlex.quote(str(odd))}"
    )


def test_rewrite_symlink_resolved(tmp_path):
    root, f = _setup(tmp_path)
    link = f.parent / "link.py"
    os.symlink(f, link)
    out = warden.run_hook(_bash(f"cat {link}", f.parent), root=root, mode="enforce", fmt="claude")
    assert json.loads(out.stdout)["hookSpecificOutput"]["updatedInput"]["command"].endswith(
        f"-- {f}"
    )


def test_edited_recently_is_not_rewritten(tmp_path):
    root, f = _setup(tmp_path)
    warden.run_hook(
        json.dumps(
            {
                "session_id": "s1",
                "cwd": str(f.parent),
                "tool_name": "Write",
                "tool_input": {"file_path": str(f), "content": "x"},
            }
        ).encode(),
        root=root,
        mode="enforce",
        fmt="claude",
    )
    out = warden.run_hook(_bash(f"cat {f}", f.parent), root=root, mode="enforce", fmt="claude")
    assert (
        warden.last_audit_record(root)["graft"]["why"] == "the file was edited in the last 5 calls"
    )


def test_envelope_change_makes_the_rule_audit(tmp_path):
    root, f = _setup(tmp_path)
    env = warden.load_envelope(root)
    env.permissions.file_read.append("/tmp/**")
    warden.register_envelope(env, root=root)
    out = warden.run_hook(_bash(f"cat {f}", f.parent), root=root, mode="enforce", fmt="claude")
    assert (
        warden.last_audit_record(root)["graft"]["why"]
        == "envelope changed since the proof; run daisugi graft prove"
    )


def test_head_not_allowed_means_no_rewrite(tmp_path):
    root, f = _setup(tmp_path, shell_allowlist=["cat"])
    out = warden.run_hook(_bash(f"cat {f}", f.parent), root=root, mode="enforce", fmt="claude")
    assert warden.last_audit_record(root)["graft"]["why"].startswith(
        "the rewritten call would be denied: "
    )


def test_pi_reply_carries_the_rewrite(tmp_path):
    root, f = _setup(tmp_path)
    out = warden.run_hook(_bash(f"cat {f}", f.parent), root=root, mode="enforce", fmt="pi")
    assert out.exit_code == 0 and json.loads(out.stdout)["rewrite"] == {
        "command": f"daisugi-spawn read --digest -- {f}"
    }
```
(`warden.run_hook`, `warden.last_audit_record` and `warden.load_envelope`: use the names the part-0 tests in `tests/test_warden_graft.py` already use for the same three things; add a thin helper there if one is missing.)

Pins: in `tests/test_opencode_pins.py` assert the plugin source contains `output.args.command = reply.rewrite.command` and no longer contains `can only deny`; in the pi pins test assert `event.input.command = reply.rewrite.command`. In `tests/test_install_codex.py` assert the Codex hook command written by install ends with `--format codex`.

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `systemd-run --user --scope -q -p MemoryMax=5G uv run --no-sync pytest -q tests/test_warden_rewrite.py tests/test_warden_graft.py tests/test_hook_format_opencode.py tests/test_opencode_pins.py tests/test_install_codex.py tests/test_warden*.py -x`

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/warden.py src/opendaisugi/hook.py src/opendaisugi/install.py src/opendaisugi/harness_pi src/opendaisugi/harness_opencode tests/test_warden_rewrite.py tests/test_hook_format_opencode.py tests/test_opencode_pins.py tests/test_install_codex.py
git commit -m "warden: the rewrite verdict for shell reads; the rewritten call passes the whole decide path again, the host keeps its own prompt, and the audit record holds the original and the rewrite (W3R-8; GR-4 provisional)"
```

### Task 15: The after-hook: rival check and shape (b) output replace

**Files:**
- Create: `src/opendaisugi/warden_after.py`
- Modify: `src/opendaisugi/warden.py` (the session record stores `tool_use_id` and the input it verified; `warden after` verb in `cli.py`; the resident server dispatches `["warden", "after"]`), `src/opendaisugi/install.py` (`--warden` writes a PostToolUse entry `daisugi warden after --format claude` with `timeout: 90`; the pi extension registers `tool_result`), `src/opendaisugi/harness_pi/extension/index.ts`, `PINS.md`
- Test: `tests/test_warden_after.py`, `tests/test_install.py`

**Interfaces:**
- Produces: `warden_after.run(payload: dict, *, root: Path, data_dir: Path, fmt: str, now) -> AfterOutcome(stdout: str, exit_code: int, record: dict)`.
- Rival check: the PostToolUse payload has `tool_use_id`, `tool_input`, `tool_response`; the pre record for that id (the session file written by the pre hook, keyed by `tool_use_id`) holds `verified_input` (the rewritten input when a rewrite was applied, else the original). A difference between `tool_input` and `verified_input` appends `<data>/warden/incidents.jsonl` `{ts, session_id, tool_use_id, tool_name, verified_sha256, ran_sha256}` and the record says `incident: True`. No pre record (the pre hook was not installed or timed out) -> `incident: None`, nothing written. Detection only; the hook never blocks.
- Output replace: an acting `output_replace` rule, `fmt == "claude"` and `tool_name == "Read"` (or `fmt == "pi"` and `tool_name == "read"`), the response's `content` has more than `file_lines_over` lines, `not_after_edit_intent` as in Task 14, the route ok, and `now - started < after_timeout_s` -> `run_digest` over the **response content** (written to a scratch file under `<data>/warden/tmp/`, never the file on disk; W3R-11), and stdout `{"hookSpecificOutput": {"hookEventName": "PostToolUse", "updatedToolOutput": <the response object with only "file"."content" replaced by the fenced digest plus the exact-text note>}}` for claude; for pi `{"content": [{"type": "text", "text": <digest>}]}`. The graft audit record carries `original_output_sha256`, `original_output_chars`, `digest_chars`, `applied`, `why`. A worker timeout emits nothing (exit 0, empty stdout): fails open on cost only.
- The hook's own deadline: `after_timeout_s` (default 60) is under the installed `timeout: 90`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_warden_after.py
def test_incident_when_the_input_that_ran_differs(tmp_path, pre_record):
    root, data = pre_record(tool_use_id="u1", verified_input={"command": "ls"})
    out = warden_after.run(
        {
            "session_id": "s1",
            "tool_use_id": "u1",
            "tool_name": "Bash",
            "tool_input": {"command": "ls -la"},
            "tool_response": "x",
        },
        root=root,
        data_dir=data,
        fmt="claude",
        now=0.0,
    )
    assert out.exit_code == 0 and out.record["incident"] is True
    assert json.loads((data / "warden" / "incidents.jsonl").read_text())["tool_use_id"] == "u1"


def test_no_pre_record_is_not_an_incident(tmp_path, bare_root):
    out = warden_after.run(
        {
            "session_id": "s1",
            "tool_use_id": "u9",
            "tool_name": "Bash",
            "tool_input": {},
            "tool_response": "x",
        },
        root=bare_root,
        data_dir=bare_root.parent,
        fmt="claude",
        now=0.0,
    )
    assert (
        out.record["incident"] is None
        and not (bare_root.parent / "warden" / "incidents.jsonl").exists()
    )


def test_output_replace_keeps_the_read_shape(tmp_path, output_rule, fake_worker):
    root, data = output_rule(lines=5)
    resp = {
        "type": "text",
        "file": {
            "filePath": "/work/a.py",
            "content": "l\n" * 10,
            "numLines": 10,
            "startLine": 1,
            "totalLines": 10,
        },
    }
    out = warden_after.run(
        {
            "session_id": "s1",
            "tool_use_id": "u1",
            "tool_name": "Read",
            "tool_input": {"file_path": "/work/a.py"},
            "tool_response": resp,
        },
        root=root,
        data_dir=data,
        fmt="claude",
        now=0.0,
    )
    got = json.loads(out.stdout)["hookSpecificOutput"]["updatedToolOutput"]
    assert (
        set(got) == set(resp)
        and set(got["file"]) == set(resp["file"])
        and got["file"]["numLines"] == 10
    )
    assert got["file"]["content"].startswith("```daisugi-spawn digest: ")
    assert (
        out.record["graft"]["original_output_sha256"]
        == hashlib.sha256(resp["file"]["content"].encode()).hexdigest()
    )


def test_output_replace_digests_the_response_not_the_disk(tmp_path, output_rule, fake_worker):
    root, data = output_rule(lines=5)
    (tmp_path / "work" / "a.py").write_text("changed on disk\n" * 50)
    resp = {
        "type": "text",
        "file": {
            "filePath": str(tmp_path / "work" / "a.py"),
            "content": "from the response\n" * 10,
            "numLines": 10,
            "startLine": 1,
            "totalLines": 10,
        },
    }
    warden_after.run(
        {
            "session_id": "s1",
            "tool_use_id": "u1",
            "tool_name": "Read",
            "tool_input": {},
            "tool_response": resp,
        },
        root=root,
        data_dir=data,
        fmt="claude",
        now=0.0,
    )
    assert fake_worker.last_file_text().startswith("from the response")


def test_worker_timeout_emits_nothing(tmp_path, output_rule, slow_worker):
    root, data = output_rule(lines=5, after_timeout_s=0.01)
    out = warden_after.run(
        {
            "session_id": "s1",
            "tool_use_id": "u1",
            "tool_name": "Read",
            "tool_input": {},
            "tool_response": {"type": "text", "file": {"filePath": "/w/a", "content": "l\n" * 10}},
        },
        root=root,
        data_dir=data,
        fmt="claude",
        now=0.0,
    )
    assert (
        out.exit_code == 0
        and out.stdout == ""
        and out.record["graft"]["why"].startswith("worker timeout")
    )
```
Fixtures `pre_record`, `bare_root`, `output_rule`, `fake_worker`, `slow_worker` live in this test file; `fake_worker` is the MockTransport worker of `tests/test_spawn.py` with a `last_file_text()` accessor over the request it saw. In `tests/test_install.py` assert that `install --warden` writes exactly one `PostToolUse` entry whose command ends with `warden after --format claude` and whose `timeout` is 90, and that `install --warden --dry-run` names it.

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `systemd-run --user --scope -q -p MemoryMax=5G uv run --no-sync pytest -q tests/test_warden_after.py tests/test_install.py tests/test_warden_server*.py -x`

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/warden_after.py src/opendaisugi/warden.py src/opendaisugi/cli.py src/opendaisugi/install.py src/opendaisugi/harness_pi tests/test_warden_after.py tests/test_install.py
git commit -m "warden after: a post-tool hook that records when the input that ran differs from the one verified, and replaces a large Read's content with a digest inside the host's own shape (GR-3 second half, W3R-11)"
```

### Task 16: Tool result sizes and the graft meter

**Files:**
- Modify: `src/opendaisugi/claude_transcript.py` (`Turn.tool_result_chars: int = 0`; `_content` sums the text of `tool_result` blocks)
- Modify: `src/opendaisugi/router_status.py` (`grafts_by_shape`, `incidents`)
- Test: `tests/test_claude_transcript.py`, `tests/test_router_status.py`

**Interfaces:**
- Produces: per week, per shape in `grafts_by_shape`: `matched` (records with a `graft` field of that shape), `applied`, `not_applied` (`why` counted by its text before the first `:`), `tokens_kept_est` (sum over applied records of `_tokens(original bytes or chars) - _tokens(digest chars)` using `spawn._tokens`; for `deny_redirect` the delegation rows' existing estimate), `extra_turns` (`deny_redirect` only: delegate calls whose row follows a redirect in the same session). `incidents` = lines in `incidents.jsonl` that week. The week is `estimated` when `tokens_kept_est > 0`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_claude_transcript.py (add)
def test_tool_result_chars_are_summed():
    text = (
        json.dumps(
            {
                "type": "user",
                "uuid": "u",
                "parentUuid": None,
                "message": {
                    "role": "user",
                    "content": [
                        {"type": "tool_result", "tool_use_id": "t", "content": "abc"},
                        {
                            "type": "tool_result",
                            "tool_use_id": "t2",
                            "content": [{"type": "text", "text": "de"}],
                        },
                    ],
                },
            }
        )
        + "\n"
    )
    assert parse_turns(text)[0].tool_result_chars == 5
```
```python
# tests/test_router_status.py (add)
def test_grafts_by_shape(tmp_path, audit_lines):
    audit_lines(
        [
            {
                "graft": {
                    "shape": "shell_rewrite",
                    "applied": True,
                    "original_input": {"command": "cat a"},
                    "lines": 400,
                    "original_chars": 16000,
                    "digest_chars": 2000,
                }
            },
            {
                "graft": {
                    "shape": "shell_rewrite",
                    "applied": False,
                    "why": "the file was edited in the last 5 calls",
                }
            },
        ]
    )
    w = router_status.weekly(tmp_path)[-1]
    s = w["grafts_by_shape"]["shell_rewrite"]
    assert (
        s["matched"] == 2
        and s["applied"] == 1
        and s["not_applied"] == {"the file was edited in the last 5 calls": 1}
        and s["tokens_kept_est"] > 0
        and w["estimated"]
    )
```
(`audit_lines` writes `<data>/warden/audit/<day>.jsonl` lines with `ts` in the current week; copy the helper the trial tests in this file use.)

- [ ] **Step 2: Run (FAIL), implement, run (PASS), commit**

```bash
git commit -m "router status: grafts by shape with their estimated tokens kept off the context and the reasons they did not apply; tool result sizes in the transcript reader"
```

### Task 17: Go and Rust: grafts, the spawn head, the after-hook; warden cases

**Files:**
- Modify: Go `internal/warden/graft.go` (rewrite), new `graftproof.go`, `after.go`; `internal/spawn/` (digest); new `cmd/daisugi-spawn/main.go`; `internal/cli/graftcmd.go` (`--rule`, `prove`), `wardencmds.go` (`after`); `internal/install` (PostToolUse entry, codex format); `internal/effects` (`network_send`)
- Modify: Rust `src/warden/graft.rs`, new `graftproof.rs`, `after.rs`; `src/spawn.rs`; new `src/bin/daisugi_spawn.rs` (binary name `daisugi-spawn`); `src/cli/graftcmd.rs`, `gatecmds.rs` -> the warden verbs; install; effects
- Modify: `clients/warden_cases.py` (`graft_cases` grows: the rewrite cases of Task 14 under both modes and all four formats, the proof refusals of Task 12, `graft prove`, the after-hook cases of Task 15 driven as `warden after` stdin cases), `clients/fixtures/warden/` regenerated with `--redo "graft*"`; `scripts/release.sh` and `packaging/aur/` list `daisugi-spawn`
- Test: Go `graft_test.go` additions; Rust tests in `graft.rs`

**Interfaces:**
- Consumes: Tasks 12 to 16 byte for byte (reason strings, `why` strings, the stdout shapes, the audit record keys, the proof block keys).
- Produces: 0 disagreements on `clients/warden_compare.py` for `clients/go/daisugi-warden` and the Rust `daisugi-warden`, and on the k3 MCP cases that touch the spawn tool.

- [ ] **Step 1: Cases from the oracle**

Run: `systemd-run --user --scope -q -p MemoryMax=5G uv run --no-sync python clients/warden_cases.py --redo "graft*"`
Expected: the manifest count grows by about 90; `redirects.txt` unchanged except new graft lines.

- [ ] **Step 2: Go, one heavy job at a time**

Run: `go test -p 1 ./internal/warden/... ./internal/spawn/...` under the cap; `clients/go/scripts/build.sh`; `uv run --no-sync python clients/warden_compare.py --binary clients/go/daisugi-warden --oracle`.
Expected: `0 disagreements`, `0 fail-open`.

- [ ] **Step 3: Rust, the same**

Expected: `0 disagreements`, `0 fail-open`.

- [ ] **Step 4: Fuzz the rewrite path in chunks**

Run: `uv run --no-sync python clients/warden_compare.py --binary clients/go/daisugi-warden --fuzz 2000 --seed 7 --only "graft*"` under the cap; repeat for Rust and seeds 8 and 9.
Expected: no call a binary rewrote or allowed that the oracle denied.

- [ ] **Step 5: Record and commit (cases; Go; Rust; packaging)**

Add rulings `GF-1 to GF-n` to `clients/ADJUDICATIONS.md`. Commit messages name GR-3 and GR-4 as provisional.

### Task 18: GEPA over graft rules, the garden's optimizer

**Files:**
- Create: `src/opendaisugi/gardener/graft_optimizer.py`
- Modify: `src/opendaisugi/cli.py` (`gardener optimize grafts [--rounds 4] [--candidates 4] [--model M] [--max-calls 16] [--dry-run] [--json]`), `src/opendaisugi/gardener/__init__.py`
- Test: `tests/test_graft_optimizer.py`

**Interfaces:**
- Consumes: `graft_proof.prove`, `graft_install.install_rule`, `spawn.parse_rule`, `router_status._read_audit`, `router_status.read_labels`, `router_status.trial_state`, `claude_transcript.read_turns` (`tool_result_chars`), `llm_client` (injectable `ask(messages) -> str`).
- Produces: `graft_optimizer.Corpus.load(data_dir, root) -> Corpus` with `records` (every audit record whose `step_type` is `shell` with a read head, or `tool_name == "Read"`, or that carries `graft`), each with `denied: bool` (`would_deny`), `lines: int | None` (from the record or `spawn.measure` when the path still exists), `session`, `ts`; `labels` (session -> pass|fail); `trial` (the arms table).
- Produces: `graft_optimizer.Score(matched: int, denied_conflicts: int, tokens_kept_est: int, extra_turns: int, sessions_fail_share: float | None, proof: str)` and `score(rule, corpus, envelope) -> Score`; `admissible(score) -> bool` = `proof == "holds" and denied_conflicts == 0` (W3R-13).
- Produces: `pareto(scored: list[tuple[Rule, Score]]) -> list[tuple[Rule, Score]]`: keep a candidate unless another is at least as good on all of (`tokens_kept_est` higher, `extra_turns` lower, `sessions_fail_share` lower or equal, None counting as 0) and better on one; ties keep the lower `version` then the earlier id.
- Produces: `reflect_prompt(rule: Rule, score: Score, failures: list[str]) -> list[dict]`: one user message, exact text:
  ```
  You improve one graft rule for openDaisugi. A graft replaces an expensive tool call with a cheaper one; the rule is JSON data. Change only these fields: match.file_lines_over (an integer from 100 to 5000), match.command_heads (a subset of cat, head, tail, less, more), conditions.recent_calls (1 to 20), conditions.not_after_edit_intent (true or false). Keep every other field as it is. Reply with the whole rule as one JSON object and nothing else.

  Current rule:
  <rule JSON, sorted keys, indent 2>

  How it scored on the owner's recent work: matched <m> calls, would apply to <a>, kept about <t> tokens off the context, added <x> extra turns, proof <p>.

  What went wrong, if anything:
  <failures, one per line, or "nothing">
  ```
  `max_tokens` 1024. The reply is parsed with `parse_rule` after `state` is forced to `audit`, `version` to parent `+ 1`, `provenance.proposed_by` to `garden`, `provenance.evidence` to the parent's id and the run id; a reply that does not parse, or changes a field outside the list, is dropped with a note `rejected: <reason>`.
- Produces: `optimize(corpus, seeds: list[Rule], *, envelope, ask, rounds=4, candidates=4, max_calls=16, grid=(200, 350, 500, 800)) -> Run(frontier, notes, calls_made)`: round 0 scores the seeds and the grid variants of `file_lines_over` (no model call); each later round asks the model once per frontier member (bounded by `candidates` per round and `max_calls` in all), scores, filters by `admissible`, recomputes the frontier. With `ask is None` only the grid runs. Deterministic order: frontier sorted by id then version.
- Produces: the command writes `<data>/garden/grafts/frontier.jsonl` (one line per frontier member per run: run id, rule, score) and installs each frontier member not yet in `<data>/warden/grafts/` through `install_rule` in state `audit` (W3R-12); prints a table `id vN  matched  apply  ~tokens kept  extra turns  fail share  proof  installed|present|refused: <why>`; `--dry-run` installs nothing; exit 1 when nothing was admissible.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_graft_optimizer.py
from opendaisugi.gardener import graft_optimizer as go
from opendaisugi.spawn import parse_rule
from tests.test_graft_proof import SHELL, _env


def _corpus(records):
    return go.Corpus(records=records, labels={}, trial=None)


def _rec(lines, denied=False, head="cat", session="s"):
    return go.Record(
        tool_name="Bash",
        step_type="shell",
        head=head,
        path="/work/a.py",
        lines=lines,
        denied=denied,
        session=session,
        ts=1.0,
        graft=None,
    )


def test_rule_matching_a_denied_record_is_inadmissible():
    r = parse_rule(SHELL, "f")
    s = go.score(r, _corpus([_rec(500), _rec(600, denied=True)]), _env())
    assert s.matched == 2 and s.denied_conflicts == 1 and not go.admissible(s)


def test_pareto_keeps_non_dominated():
    a = (parse_rule(dict(SHELL, id="a"), "f"), go.Score(10, 0, 5000, 0, None, "holds"))
    b = (parse_rule(dict(SHELL, id="b"), "f"), go.Score(10, 0, 4000, 0, None, "holds"))
    c = (parse_rule(dict(SHELL, id="c"), "f"), go.Score(10, 0, 6000, 2, None, "holds"))
    assert [r.id for r, _ in go.pareto([a, b, c])] == ["a", "c"]


def test_reflect_prompt_is_byte_fixed():
    r = parse_rule(SHELL, "f")
    msgs = go.reflect_prompt(r, go.Score(3, 0, 1200, 0, None, "holds"), [])
    assert msgs == [
        {
            "role": "user",
            "content": go.PROMPT_HEAD
            + "\n\nCurrent rule:\n"
            + go.rule_json(r)
            + "\n\nHow it scored on the owner's recent work: matched 3 calls, would apply to 3, kept about 1200 tokens off the context, added 0 extra turns, proof holds.\n\nWhat went wrong, if anything:\nnothing",
        }
    ]


def test_optimize_with_a_fake_model_installs_audit_only(tmp_path, registered_root):
    root, env = (
        registered_root  # a warden root with a registered envelope that allows cat and daisugi-spawn
    )
    corpus = _corpus([_rec(400), _rec(900), _rec(120)])

    def ask(messages):  # the model lowers the threshold
        import json

        d = json.loads(
            messages[0]["content"].split("Current rule:\n", 1)[1].split("\n\nHow it scored", 1)[0]
        )
        d["match"]["file_lines_over"] = 300
        return json.dumps(d)

    run = go.optimize(
        corpus, [parse_rule(SHELL, "f")], envelope=env, ask=ask, rounds=2, candidates=1, max_calls=2
    )
    assert run.calls_made == 2 and all(s.proof == "holds" for _, s in run.frontier)
    installed = go.install_frontier(
        run, root=root, data_dir=tmp_path, home=tmp_path, cwd=tmp_path, dry_run=False
    )
    import json

    for p in (root / "grafts").glob("*.json"):
        d = json.loads(p.read_text())
        assert d["state"] == "audit" and d["provenance"]["proposed_by"] == "garden"
    assert installed and (tmp_path / "garden" / "grafts" / "frontier.jsonl").exists()


def test_model_reply_changing_a_locked_field_is_rejected():
    r = parse_rule(SHELL, "f")
    bad = go.rule_json(r).replace(
        '"daisugi-spawn read --digest -- {operands}"', '"rm -rf {operands}"'
    )
    got, note = go.parse_candidate(bad, parent=r, run_id="r1")
    assert got is None and note.startswith("rejected: replace.command changed")
```

- [ ] **Step 2: Run (FAIL), implement, run (PASS)**

Run: `systemd-run --user --scope -q -p MemoryMax=5G uv run --no-sync pytest -q tests/test_graft_optimizer.py tests/test_cli_gardener.py -x`

- [ ] **Step 3: Commit**

```bash
git add src/opendaisugi/gardener/graft_optimizer.py src/opendaisugi/gardener/__init__.py src/opendaisugi/cli.py tests/test_graft_optimizer.py
git commit -m "garden: an optimizer over graft rules; a model proposes, proofs and replay against denied calls filter, a Pareto frontier keeps the winners, and every proposal lands in audit (W3R-12, W3R-13; RK-1 provisional)"
```

### Task 19: Go and Rust: the optimizer; garden cases with the fake model server

**Files:**
- Modify: Go `internal/garden/` new `graftopt.go`; `internal/cli/gardencmd.go` (`optimize grafts`); Rust `src/garden/graftopt.rs`; `src/cli/gardencmd.rs`
- Modify: `clients/garden_cases.py` (cases: `optimize grafts: grid only, no model`, `optimize grafts: fake model lowers the threshold`, `optimize grafts: reply with a locked field changed is rejected`, `optimize grafts: a candidate that matches a denied call is dropped`, `optimize grafts: dry run installs nothing`, `optimize grafts: nothing admissible exits 1`), fixtures regenerated

The fake model server answers only exact request bytes (as `k3_cases.py` does), so the prompt text of Task 18 is the parity contract.

- [ ] **Step 1: Cases from the oracle**

Run: `uv run --no-sync python clients/garden_cases.py --only "optimize grafts*"`

- [ ] **Step 2: Go port, build, compare; Step 3: Rust the same** (one heavy job at a time)

Run: `uv run --no-sync python clients/garden_compare.py --binary clients/go/daisugi --only "optimize grafts*"`; then Rust.
Expected: `0 disagreements`.

- [ ] **Step 4: Record and commit (cases; Go; Rust)**

Rulings `GP-1 to GP-n` in `clients/ADJUDICATIONS.md` (the prompt bytes, the Pareto tie rule, the rounding of `tokens_kept_est`).

### Task 20: Docs that say only what the meter shows

**Files:**
- Create: `docs/reference/router-measured.md` (generated: `daisugi router status --markdown --data-dir <a fixture data dir under tests/fixtures/router-measured/>`; the fixture is synthetic and says so in its first line), `docs/adr/0027-the-rewrite-verdict.md`
- Modify: `docs/how-to/token-saving-router.md` (W2's name for the gateway how-to; sections "Cheap first and escalation", "Shadow sampling and the learned chooser", "Grafts: shell rewrite and output replace", "What the meter shows" linking the reference file), `docs/how-to/warden.md` (`warden after`, `graft install --rule`, `graft prove`), `docs/feature-status.md` (rows for parts 1 to 4, grafts shapes a and b, the optimizer: Built in all three, with the case counts), `docs/roadmap.md` section 4 "Where it stands", `docs/plans/2026-09-24-omarchy/design-router.md` and `design-grafts.md` (a dated "Built" note per part, in the current-state voice), `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md` (a "Router and grafts, wave 3" table with W3R-1 to W3R-13; RT-2, GR-3, GR-4 rows get "Retires when: built 2026-11" notes), `docs/research/token-saving-landscape-mapping.md` (one row: daisugi edits a tool call's input or result only by a proved, logged rule; never the model request), `docs/reference/dependencies.md` (no new dependency: say so), `README.md` (the router cell: `rules, switchyard, knn, model, learned`), `AGENTS.md` (the binaries table: `daisugi-spawn`; the concern map rows for `router_escalate.py`, `router_shadow.py`, `router_chooser.py`, `graft_proof.py`, `warden_after.py`, `gardener/graft_optimizer.py`)
- Create: `tests/test_docs_router_claims.py`

**Interfaces:**
- Produces: the claims test: for each file in `docs/how-to/token-saving-router.md`, `docs/how-to/warden.md`, `README.md`, `VISION.md`, `docs/roadmap.md`, `docs/feature-status.md`, `docs/explanation/modular-stack.md`, `site/index.html`: no line that matches `\b\d+(\.\d+)?\s?%` or `\b\d+(\.\d+)?x\b` within 80 characters of `token|cost|saving|cheaper|fewer` unless the line also contains `router-measured.md` or `measured by daisugi router status`. The generated file is exempt. Quoted third-party numbers (Spotify, RTK, the arXiv study) stay only in `docs/research/`, which is not scanned.

- [ ] **Step 1: Write the failing claims test and run it**

Run: `uv run --no-sync pytest -q tests/test_docs_router_claims.py`
Expected: FAIL on the lines that state a saving today; fix each line by removing the number or pointing it at the measured file.

- [ ] **Step 2: Generate the measured file, write the ADR and the doc edits**

ADR-0027 (status Accepted on merge): context (tokens are not cost; the warden sits on every call), decision (a fourth verdict, rewrite, data rules proved at install and re-decided per call, the journal holds both inputs, the host keeps its prompt), consequences (cost denies apart from safety denies; a rule proved against one envelope is audit under another; Codex only with skip_host_prompt), alternatives (proxy-side compression, rejected per the mapping doc).

- [ ] **Step 3: Run the claims test, the names check and the docs link check that the repo has**

Run: `uv run --no-sync pytest -q tests/test_docs_router_claims.py tests/test_docs*.py`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add docs/ README.md AGENTS.md tests/test_docs_router_claims.py
git commit -m "docs: the router and grafts as built, every saving number from the meter's own Markdown with its date; ADR-0027 the rewrite verdict"
```

### Task 21: The full run, the ledger, cleanup

**Files:**
- Modify: `.superpowers/sdd/backlog/progress.md` (git-ignored ledger; the entry below), `clients/go/README.md` and `clients/rust/README.md` (the new suite counts with the date)

- [ ] **Step 1: The full Python suite, one heavy job**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync pytest -q -p no:cacheprovider && uv run --no-sync ruff check . && uv run --no-sync ruff format --check .`
Expected: green. A known load flake (`tests/test_agentic_sprig.py`) passes alone.

- [ ] **Step 2: Every compare, one at a time**

Run, each under the cap: `clients/warden_compare.py`, `clients/router_compare.py`, `clients/garden_compare.py`, `clients/k3_compare.py`, each with `--binary` for Go then Rust, `--oracle` once per suite.
Expected: `0 disagreements` everywhere; every refusal under a ruling.

- [ ] **Step 3: Hand check on the box (box-gated; record as not done if the box is down)**

With a scratch data dir and warden root under `/mnt/tera/.tt/w3-router-grafts/live/`: `claude --settings "$(daisugi warden settings --root /mnt/tera/.tt/w3-router-grafts/live/warden)"` on a workspace with one 600-line file; `cat` it; confirm Claude Code shows the rewritten command in its prompt (W3R-8) and the audit record holds both inputs. Then `daisugi router status --markdown`. Write what happened, including "not verified on a live host" where that is the truth, into the ledger. At most three short sessions.

- [ ] **Step 4: Ledger entry and cleanup**

Append to `.superpowers/sdd/backlog/progress.md`: the branch head, the suite counts, the rulings W3R-1 to W3R-13 with "owner to confirm", the open questions below, and the hand-check result. Then: `rm -rf clients/rust/target/release/deps/*router* clients/rust/target/release/deps/*warden*` is **not** done (never partial-clean a cargo target); instead note the target size; delete `/mnt/tera/.tt/w3-router-grafts/live/` and any model scratch.

- [ ] **Step 5: Commit the README counts**

```bash
git add clients/go/README.md clients/rust/README.md
git commit -m "ports: suite counts after the router, graft and optimizer cases"
```
Leave the branch for review and merge; no push.

## Open questions, with this plan's rulings

1. **Is escalation back to the requested model an "auto model switch"?** Ruled by the owner on 2026-10-30 (B2): no, it is allowed and it is on by default; `routing_escalate: false` or `--no-escalate` turns it off. `routing_cheap_first` stays off by default (W3R-3), so a default install never downgrades on its own and escalation only acts on a downgrade the routing mode or the flag made.
2. **`router_report.py` name collision after W2.** Ruling: the `router status` module becomes `router_status.py`; W2's builder must move it before `gateway_report.py` takes the name.
3. **Does Claude Code honour `permissionDecision: "ask"` with `updatedInput` and show the rewritten command?** Per the grafts research, yes; not verified on a live host here. Ruling: ship `"ask"` as the default (W3R-8); the Task 21 hand check settles it; if the host ignores it, the fallback is `"allow"` only under `skip_host_prompt`, which keeps the rule audit-only by default.
4. **Shadow rows as knn neighbours (RT-R-5 said no exploration in v1).** Ruling W3R-5: shadows are the bounded exploration RT-R-5's "retires when" named; amend RT-R-2 and RT-R-5 in the adjudications. If the owner wants them out, `recent()` filters `shadow_of IS NULL`.
5. **Account failover credentials.** Ruling W3R-4: environment variable names in `router_accounts`; API keys only. An OAuth subscription has no second account; the pause header is the honest answer.
6. **The chooser's trainer in three languages.** Ruling W3R-6: fixed-constant batch gradient descent in float64 with a golden file; if a port cannot match within `1e-7`, the ruling becomes "Python trains, the ports apply `chooser.json`", and `router train` refuses in Go and Rust with a one-line reason under an adjudication.
7. **Who may run `gardener optimize grafts`.** Ruling: the operator, like `router label` (SW-12); the warden denies an agent that runs it. Proposals are audit-only (W3R-12), so a wrong rule costs nothing until a person promotes it.
8. **Output replace on Codex** (only through `decision: "block"`, which the research marks partial). Ruling: not in this wave; Codex gets shape (a) under `skip_host_prompt` and shape (c) as today.
