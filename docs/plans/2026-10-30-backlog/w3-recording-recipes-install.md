# W3: recording for every harness, `daisugi recipe apply`, install only what I name

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every supported harness's tool calls land in the journal (pi, OpenCode, Hermes, OpenClaw, beside Claude Code and Codex); the four recipes in `docs/recipes/` become data files that `daisugi recipe apply` runs through the install primitives, with a revert; `daisugi install --only` and `--list` let a user install (and uninstall) only the components they name. All three ports, with golden cases for every new behaviour.

**Architecture:** Python is the oracle; Go and Rust mirror it and the golden cases prove it (`clients/k4_cases.py` for the hook family, `clients/warden_server_cases.py` for the resident warden, `clients/cli_cases.py` for `install` and `recipe`). Recording reuses the capture pipeline that exists: a capture record per call (`hook.record_call`), captures to traces (`hook to-trace`, `hook auto-tend`), traces to the garden. The gaps this plan closes are small and exact: the resident warden mirrors allowed calls into captures but classifies every payload as Claude Code (so pi's lowercase tool names are dropped), it writes them under `<data>/warden/captures` where the journal feed never looks, and nothing triggers auto-tend on that path; the Hermes and OpenClaw hooks send real tool names (`terminal`, `read_file`, `exec`) the classifier does not know; the OpenClaw plugin reads a session id from a field the host does not send. Recipes are JSON files shipped inside each binary; a recipe step names one install primitive (`install`, `harness`, `config`, `warden-init`, `note`) and never a shell command. Component selection is one new set on the existing `Layer` path, with `reverse` learning to take the same set.

**Tech Stack:** Python 3.12 (typer, pytest), Go (cgo), Rust (cargo), Node 22 only for the TypeScript extension tests (skipped when absent, as `tests/harness_pi/_node_check.py` does today).

**Spec:** `docs/plans/2026-10-30-backlog/INDEX.md` (wave 3: recording for more harnesses, `daisugi recipe apply`, install only what I name), the ledger `.superpowers/sdd/backlog/progress.md`. Builds run after W1 (`w1-ollama-and-record.md`: the warden hook is part of every install, audit mode) and W2 (`w2-rename.md`: gate is warden, gateway is router, the router's inner policy is routing mode, foreman is steward). This plan uses the new names and the post-rename paths throughout.

## Global Constraints

- Laptop: 4 cores, 15 GB. ONE heavy job at a time (a Go or cargo build, a cargo test, the full pytest suite, a full compare, a fixture regeneration), each under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G <command>`, with `-j2` (`go test -p 1`, `CARGO_BUILD_JOBS=2`). Never rebuild Z3; never `cargo clean` in `clients/rust`; never change `clients/go/scripts/native.sh`.
- `/tmp` is RAM. Scratch goes under `/mnt/tera/.tt/w3/` (`DAISUGI_CLI_SCRATCH=/mnt/tera/.tt/w3/cli`, `DAISUGI_K4_SCRATCH=/mnt/tera/.tt/w3/k4`, and the same pattern for every writer and compare that takes one). Worktree on real disk: `/mnt/tera/working/programming/od-wt/w3`, branch `w3-recording-recipes-install`.
- No network and no real models in tests. No real agents (`claude`, `codex`, `pi`, `opencode`, `hermes`, `openclaw`, `sprig`). Fake HTTP on loopback only. Node tests run the extension source with a fake socket server, as the pi tests do today.
- Never read `~/.claude`, `~/.codex`, `~/.hermes`, `~/.openclaw`, `~/.pi`, `~/.config/opencode` or any real user data. Every test transcript, session file and hook payload in this plan is synthetic, written by the test itself. Never `pkill -f`. Never touch `~/.opendaisugi`. Never run `daisugi install` outside a test HOME.
- Every coppice run passes its own `--socket` and `--data-dir` (this plan runs no coppice server).
- Commits: atomic, one concern each, the message states the why. No AI attribution lines. Never commit `CLAUDE.md` or `docs/superpowers/`. Commit by explicit path. Push only through the fold script, on the owner's word.
- Writing: ASD-STE100. Short sentences, plain words, active voice. No em dashes in new text. Never the words "seam" or "the layer". Docs state the current behaviour once; no changelog voice.
- Names: warden (not gate), router (not gateway), routing mode (the router's inner policy, config key `routing_mode`), steward (not foreman). Paths as W2 leaves them: `src/opendaisugi/warden.py`, `warden_server.py`, `warden_client.py`; `<data>/warden/` (socket `warden.sock`); Go `clients/go/internal/warden/`; Rust `clients/rust/src/warden/`; pi extension dir `~/.pi/agent/extensions/daisugi-warden/`, OpenCode plugin `daisugi-warden.ts`; recipes `docs/recipes/warden-only.md`, `router-and-routing-mode.md`, `coppice-claude-warden.md`, `bring-your-own.md`; install flags `--enforce`, `--router` (point the harness at the router), `--routing-mode X`, `--harness`. If a name in this plan and the post-rename tree disagree, the tree wins; note the departure in the ledger.
- Full parity: a behaviour the oracle gains, Go and Rust gain in the same wave, and the golden cases cover it. A port that cannot do a case refuses it (exit 2, one line, nothing written) and that refusal gets a ruling in `clients/ADJUDICATIONS.md`.
- JSON key order and file bytes matter: the compare scripts read trees whole. Insert new keys at the positions this plan names. `clients/gen_help.py` regenerates `helpall_gen.go` and `helpall_gen.rs` after any help text change.
- Fail closed stays: nothing in this plan changes a verdict. Recording is best-effort and never changes what the host is told.

## Review Focus

1. Task 1 must not change any verdict or any audit line: only the capture record. The warden server tests that assert exit codes and stderr must stay byte-identical.
2. Task 3's scan order: the primary root (`<data>/captures`) is converted before the warden root (`<data>/warden/captures`). A Claude Code session with the resident warden up is captured in both; `Journal.is_session_converted` dedupes by session id, so the second copy is skipped. A test pins the order.
3. Task 4 and the Hermes map (Task 2) are built from the documented wire shapes. The box checks (Task 9) are the only proof they fire on a real host. The plan says so in every doc line it writes.
4. `install --only` with `--enforce` but no `warden`, or with `--router` but no `router`, refuses before any write (Task 10). The refusal lines are pinned in the cli cases.
5. `recipe apply` without `--yes` writes nothing anywhere, including the receipt (Task 15). A test asserts the HOME tree is byte-identical after a dry run.
6. `recipe revert` restores config values from the receipt, never from defaults. A recipe applied over a changed config and then reverted leaves the earlier value (Task 15 pins it).

## Rulings (open questions decided here; the ledger records them)

- **R-W3-1, one recorder per harness.** pi and OpenCode: the resident warden's capture mirror (their extensions already send every call to it, as `--format pi` and `--format opencode`). Hermes: the shell hook `hooks.pre_tool_call` that `install` writes (`daisugi hook record --format hermes`). OpenClaw: the `before_tool_call` plugin that `install` writes. Claude Code: the capture hook (unchanged). Codex: the resident mirror when the warden is resident, and `journal parse --format codex` (unchanged). No new session-file parser is required for a harness to land in the journal. Reason: every harness exposes a pre-call hook with the tool name and arguments (sources in Task 2 and Task 4); session files are a second copy of the same facts, and for Hermes and OpenClaw the store is SQLite with an undocumented schema.
- **R-W3-2, the warden's captures root stays where it is.** `daisugi start` keeps `--captures-root <warden root>/captures` (FN-R-32, FN-R-35: the fence and the sub-agent runs rely on `<root>/captures`). The journal feed (`hook list`, `hook to-trace`, `hook auto-tend`) learns the second root instead. Cost if wrong: one more scan directory. The alternative (one root) would double-record every Claude Code call into one file when the resident warden is up.
- **R-W3-3, pi and OpenCode history import is built in this wave, with live recording (owner, 2026-10-30, B10; was "last, optional").** Tasks 8a and 8b add `journal parse --format pi|opencode` for sessions recorded before install. They are not optional and not last: they are built right after Tasks 1 to 4, in the oracle, Go and Rust, and the wave is not complete without them. pi's JSONL and OpenCode's JSON part files are documented and typed; a parser is a subclass of `ClaudeCodeParser`, as `CodexParser` is.
- **R-W3-4, recipes are JSON.** `docs/recipes/README.md` said `recipe.yaml`. The ports share a byte-stable JSON model (`pyjson`) and the recipe files are machine-written data, not a user's config. Cost if wrong: one serializer.
- **R-W3-5, a recipe step is one install primitive.** Kinds: `install` (layers, runtimes, enforce, base_url, report), `harness` (pi or opencode extension), `config` (the keys `_plan_router_update` sets), `warden-init` (a starter envelope for a workspace), `note` (text; nothing runs). No shell. A recipe that needs a tool we do not install (Switchyard, coppice) says so in a `note` step.
- **R-W3-6, `--only` takes component names; `--harness` stays its own flag.** Components are the install layers under their user-facing names: `skill`, `mcp`, `capture`, `instructions`, `warden`, `router`. The pi and OpenCode extensions are a different axis (which harness) and keep `--harness`. `--only` is repeatable and comma-separated; there is no positional form (one way to say it).
- **R-W3-7, the OpenClaw plugin writes no unverified config keys.** Whether a discovered extension needs `plugins.entries.opendaisugi.enabled` is a box check (Task 9). Until then `install` prints the directory it wrote and the `openclaw plugins install <dir>` line from the docs as a note, and writes nothing to `openclaw.json` beyond the MCP entry it writes today.
- **R-W3-8, pi's package name.** The extension imports `@earendil-works/pi-coding-agent` (pinned in `harness_pi/extension/PINS.md`, from `earendil-works/pi`). The public docs at `pi.dev` and the npm package `@mariozechner/pi-coding-agent` describe the same session and extension API. This plan does not change the import. The pi parser (Task 8a) cites `pi.dev/docs/latest/session-format`.

---

## Part A: recording for every harness

### Task 1: `record_call` carries the host format; the warden mirror and `hook record` pass it

**Files:**
- Modify: `src/opendaisugi/hook.py` (`record_call`, `record_and_contract`)
- Modify: `src/opendaisugi/warden.py` (the mirror in `warden_and_contract`, the block that reads `if captures_root is not None and decision.allow`)
- Test: `tests/test_hook_format_pi.py`, `tests/test_hook_format_opencode.py`, `tests/test_hook.py`

**Interfaces:**
- Produces: `hook.record_call(payload: dict, *, root: Path = DEFAULT_CAPTURES_ROOT, fmt: str = "claude") -> Path | None`; it calls `_payload_to_record(payload, fmt=fmt)`. `record_and_contract(raw, *, root, fmt)` passes its `fmt` to `record_call`. The warden mirror passes `fmt=fmt`.
- Unchanged: `_payload_to_record`, `_classify_tool`, every verdict, every audit and tree line.

- [ ] **Step 1: Write the failing tests**

In `tests/test_hook_format_pi.py`:

```python
def test_record_call_under_pi_format_keeps_lowercase_bash(tmp_path):
    from opendaisugi.hook import record_call

    out = record_call(
        {"tool_name": "bash", "tool_input": {"command": "ls"}, "session_id": "s1"},
        root=tmp_path / "captures",
        fmt="pi",
    )
    assert out == tmp_path / "captures" / "s1.jsonl"
    rec = json.loads(out.read_text().splitlines()[0])
    assert rec["step_type"] == "shell" and rec["command"] == "ls"


def test_record_call_default_format_is_still_claude(tmp_path):
    from opendaisugi.hook import record_call

    assert (
        record_call({"tool_name": "bash", "tool_input": {"command": "ls"}}, root=tmp_path) is None
    )


def test_resident_mirror_records_a_pi_call(tmp_path):
    root = _gate_root(tmp_path, _envelope(shell_allowlist=["ls"]))
    caps = tmp_path / "caps"
    out = gate_and_contract(
        _payload("bash", {"command": "ls"}), root=root, fmt="pi", captures_root=caps
    )
    assert out.exit_code == 0
    rec = json.loads((caps / "s1.jsonl").read_text().splitlines()[0])
    assert rec["tool_name"] == "bash" and rec["step_type"] == "shell"


def test_resident_mirror_never_records_a_denied_pi_call(tmp_path):
    root = _gate_root(tmp_path, _envelope())
    caps = tmp_path / "caps"
    out = gate_and_contract(
        _payload("bash", {"command": "rm -rf /"}), root=root, fmt="pi", captures_root=caps
    )
    assert out.exit_code == 2
    assert not caps.exists()
```

(`gate_and_contract`, `_gate_root`, `_envelope`, `_payload` are the names the file already imports and defines; after W2 the import is `from opendaisugi.warden import ... warden_and_contract`. Use the file's names.)

In `tests/test_hook.py`, beside the `record_and_contract` tests:

```python
def test_record_and_contract_passes_the_format_to_the_record(tmp_path):
    from opendaisugi.hook import record_and_contract

    raw = json.dumps(
        {"tool_name": "bash", "tool_input": {"command": "ls"}, "session_id": "p"}
    ).encode()
    record_and_contract(raw, root=tmp_path, fmt="pi")
    assert (tmp_path / "p.jsonl").exists()
```

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_hook_format_pi.py tests/test_hook.py -k "record_call or resident_mirror or passes_the_format"`
Expected: FAIL (`TypeError: unexpected keyword 'fmt'`; the mirror writes no file).

- [ ] **Step 3: Implement**

`record_call` gains `fmt: str = "claude"` and passes it on. `record_and_contract` passes `fmt=fmt`. In `warden.py` the mirror becomes `record_call(payload, root=captures_root, fmt=fmt)`. Update the two docstrings that say the mirror classifies as Claude Code does.

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_hook_format_pi.py tests/test_hook_format_opencode.py tests/test_hook.py tests/test_warden_resident_paths.py tests/test_warden_dispatch.py`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/hook.py src/opendaisugi/warden.py tests/test_hook_format_pi.py tests/test_hook.py
git commit -m "hook: a capture record is classified under the host's own format

The resident warden mirrored allowed calls into captures but classified
every payload as Claude Code, so pi's lowercase bash/read/write/edit were
dropped and a pi session never reached the journal. record_call now takes
the format its callers already know."
```

### Task 2: Hermes and OpenClaw tool names classify; their payload shapes are pinned

**Files:**
- Modify: `src/opendaisugi/hook.py` (`_classify_tool`, new `_HERMES_TOOL_TYPE_MAP`, `_OPENCLAW_TOOL_TYPE_MAP`, `_payload_to_record` path and url extraction)
- Create: `tests/test_hook_format_hermes.py`, `tests/test_hook_format_openclaw.py`

**Sources (fetched 2026-10-30; quote them in the module comment):**
- Hermes shell hooks: `https://hermes-agent.nousresearch.com/docs/user-guide/features/hooks`. A `hooks:` block in `~/.hermes/config.yaml`, keyed by event, entries `matcher`, `command`, `timeout`; stdin JSON carries `hook_event_name`, `tool_name`, `tool_input`, `session_id`, `cwd`, `profile`, `extra` (`task_id`, `tool_call_id`). Default fail-open; `{}` on stdout is a no-op.
- Hermes tool names: `https://hermes-agent.nousresearch.com/docs/user-guide/features/tools` (`terminal`, `process`, `read_file`, `patch`, `write_file`, `web_search`, `web_extract`, `search_files` and others). Argument names from `https://raw.githubusercontent.com/NousResearch/hermes-agent/main/tools/file_tools.py`: `read_file(path, offset, limit)`, `write_file(path, content)`, `patch(path, old_string, new_string, replace_all)`, `search_files(pattern, target, path, ...)`; `terminal(command, background, pty)` from the tools page. `web_extract` and `web_search` argument names are not documented on those pages: the record takes `url`, then `urls[0]`, then `query`, and the box check confirms.
- OpenClaw hook: `https://docs.openclaw.ai/plugins/hooks/tool-policy`. `before_tool_call` event: `toolName`, `params`, `toolCallId?`, `runId?`; `ctx.sessionId`, `ctx.agentId`, `ctx.runId`. `after_tool_call`: `toolName`, `params`, `result?`, `error?`, `durationMs?`.
- OpenClaw tool names: `https://docs.openclaw.ai/tools` (`exec`, `process`, `read`, `write`, `edit`, `apply_patch`, `web_search`, `web_fetch`, `browser`, `message` and others). `exec` arguments from `https://docs.openclaw.ai/tools/exec`: `command`, `workdir`, `env`, `background`, `timeoutSeconds`, `pty`, `elevated`. `web_fetch` from `https://docs.openclaw.ai/tools/web-fetch`: `url`, `extractMode`, `maxChars`. `read`, `write`, `edit` argument names are not on the index page; the record takes `path`, then `file_path`, then `filePath` (the chain that exists), and the box check confirms.

**Interfaces:**
- `_HERMES_TOOL_TYPE_MAP = {"terminal": "shell", "read_file": "file_read", "search_files": "file_read", "write_file": "file_write", "patch": "file_write", "web_search": "network", "web_extract": "network"}`.
- `_OPENCLAW_TOOL_TYPE_MAP = {"exec": "shell", "read": "file_read", "write": "file_write", "edit": "file_write", "apply_patch": "file_write", "web_fetch": "network", "web_search": "network"}`.
- `_classify_tool(name, *, fmt)`: after the shared map, `fmt == "hermes"` consults the Hermes map, `fmt == "openclaw"` the OpenClaw map; unknown names under these two formats return `None` (dropped, as today). Only `fmt == "pi"` keeps the MCP-style fallback.
- `_payload_to_record`: for `network`, `record["url"] = inp.get("url") or (inp.get("urls") or [None])[0] or inp.get("query") or ""` (the `urls` form is a list). For `file_write` under `openclaw` with `tool_name == "apply_patch"`: `record["path"]` is the first path `parse_apply_patch(inp.get("patch") or inp.get("input") or "")` returns, else `""`. Hermes `patch` keeps `path`. The `cwd` join from the payload applies as today (Hermes sends `cwd`; OpenClaw does not).

- [ ] **Step 1: Write the failing tests**

`tests/test_hook_format_hermes.py`:

```python
import json

import pytest

from opendaisugi.hook import _classify_tool, _payload_to_record, record_call


def hermes(tool_name, tool_input, **extra):
    # The stdin shape of a Hermes shell hook (docs/user-guide/features/hooks).
    return {
        "hook_event_name": "pre_tool_call",
        "tool_name": tool_name,
        "tool_input": tool_input,
        "session_id": "h-1",
        "cwd": "/w",
        "profile": "default",
        "extra": {"task_id": "t", "tool_call_id": "c1"},
        **extra,
    }


@pytest.mark.parametrize(
    "name,kind",
    [
        ("terminal", "shell"),
        ("read_file", "file_read"),
        ("search_files", "file_read"),
        ("write_file", "file_write"),
        ("patch", "file_write"),
        ("web_search", "network"),
        ("web_extract", "network"),
    ],
)
def test_hermes_names_classify(name, kind):
    assert _classify_tool(name, fmt="hermes") == kind


def test_hermes_names_do_not_classify_under_claude():
    assert _classify_tool("terminal", fmt="claude") is None


def test_hermes_unknown_tool_is_dropped_not_mcp():
    assert _classify_tool("vision_analyze", fmt="hermes") is None
    assert _payload_to_record(hermes("vision_analyze", {"image": "x"}), fmt="hermes") is None


def test_hermes_terminal_record():
    rec = _payload_to_record(
        hermes("terminal", {"command": "make test", "pty": True}), fmt="hermes"
    )
    assert rec["step_type"] == "shell" and rec["command"] == "make test"
    assert rec["session_id"] == "h-1"


def test_hermes_relative_read_joins_cwd():
    rec = _payload_to_record(hermes("read_file", {"path": "a.txt"}), fmt="hermes")
    assert rec["path"] == "/w/a.txt"


def test_hermes_write_keeps_no_content():
    rec = _payload_to_record(
        hermes("write_file", {"path": "/w/o.txt", "content": "secret"}), fmt="hermes"
    )
    assert "content" not in rec and rec["content_len"] == 6


def test_hermes_web_extract_url_forms():
    assert (
        _payload_to_record(hermes("web_extract", {"url": "https://a/b"}), fmt="hermes")["url"]
        == "https://a/b"
    )
    assert (
        _payload_to_record(hermes("web_extract", {"urls": ["https://c/d"]}), fmt="hermes")["url"]
        == "https://c/d"
    )
    assert _payload_to_record(hermes("web_search", {"query": "q"}), fmt="hermes")["url"] == "q"


def test_record_call_hermes_writes_a_capture(tmp_path):
    out = record_call(hermes("terminal", {"command": "ls"}), root=tmp_path, fmt="hermes")
    assert out == tmp_path / "h-1.jsonl"
    assert json.loads(out.read_text())["tool_name"] == "terminal"
```

`tests/test_hook_format_openclaw.py`:

```python
import pytest

from opendaisugi.hook import _classify_tool, _payload_to_record

PATCH = "*** Begin Patch\n*** Add File: /w/new.txt\n+hi\n*** End Patch"


def openclaw(tool, args, **extra):
    # What the shipped plugin sends to `daisugi hook record --format openclaw` (Task 4).
    return {"tool": tool, "args": args, "session_id": "oc-1", "tool_use_id": "tc-1", **extra}


@pytest.mark.parametrize(
    "name,kind",
    [
        ("exec", "shell"),
        ("read", "file_read"),
        ("write", "file_write"),
        ("edit", "file_write"),
        ("apply_patch", "file_write"),
        ("web_fetch", "network"),
        ("web_search", "network"),
    ],
)
def test_openclaw_names_classify(name, kind):
    assert _classify_tool(name, fmt="openclaw") == kind


def test_openclaw_unknown_tool_is_dropped():
    assert _payload_to_record(openclaw("message", {"action": "send"}), fmt="openclaw") is None


def test_openclaw_exec_record():
    rec = _payload_to_record(openclaw("exec", {"command": "ls", "workdir": "/w"}), fmt="openclaw")
    assert rec["step_type"] == "shell" and rec["command"] == "ls" and rec["tool_use_id"] == "tc-1"


def test_openclaw_apply_patch_names_its_first_file():
    rec = _payload_to_record(openclaw("apply_patch", {"patch": PATCH}), fmt="openclaw")
    assert rec["step_type"] == "file_write" and rec["path"] == "/w/new.txt"


def test_openclaw_web_fetch_url():
    assert (
        _payload_to_record(openclaw("web_fetch", {"url": "https://x/y"}), fmt="openclaw")["url"]
        == "https://x/y"
    )
```

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_hook_format_hermes.py tests/test_hook_format_openclaw.py`
Expected: FAIL on every classify test (`None`).

- [ ] **Step 3: Implement**

The two maps, the two branches in `_classify_tool`, the `urls[0]` form, the `apply_patch` path. Keep `_PI_TOOL_TYPE_MAP` and the pi fallback as they are. The module comment under the maps names the four source URLs and the date, and says which argument names are unverified (`web_extract`, `web_search`, OpenClaw `read`/`write`/`edit`).

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_hook_format_hermes.py tests/test_hook_format_openclaw.py tests/test_hook_format_pi.py tests/test_hook_format_opencode.py tests/test_hook_format.py tests/test_hook.py`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/hook.py tests/test_hook_format_hermes.py tests/test_hook_format_openclaw.py
git commit -m "hook: classify the tool names Hermes and OpenClaw really send

The capture hook installed for Hermes and OpenClaw received terminal,
read_file, exec, web_fetch and dropped every one as unknown. The names and
argument fields come from each project's docs (URLs in the module)."
```

### Task 3: The journal feed reads the warden's captures too, and the resident path triggers auto-tend

**Files:**
- Modify: `src/opendaisugi/hook.py` (new `capture_roots`, `find_capture`, `list_sessions_in`)
- Modify: `src/opendaisugi/cli.py` (`hook list`, `hook to-trace`, `hook auto-tend`)
- Modify: `src/opendaisugi/warden.py` (after the mirror: `maybe_trigger_background_tend`)
- Test: `tests/test_hook.py`, `tests/test_hook_auto_trigger.py`, `tests/test_cli_pipe.py` (or the file that drives `hook auto-tend` through the CLI runner today; find it with `grep -ln "auto-tend" tests/*.py`)

**Interfaces:**
- `hook.capture_roots(data_dir: Path) -> list[Path]` returns `[data_dir / "captures", data_dir / "warden" / "captures"]` (primary first).
- `hook.list_sessions_in(roots: list[Path]) -> list[dict]`: one row per session id; a session present in two roots appears once, with `root` the first root that holds it and `calls` counted from that file only. Rows sorted newest-first as today. Each row gains `"root": str(root)`.
- `hook.find_capture(session_id: str, roots: list[Path]) -> Path | None`: the first `<root>/<session_id>.jsonl` that exists.
- CLI: `hook list`, `hook to-trace` and `hook auto-tend` keep `--captures-root`; when it is passed, only that root is used (one root, as today). When it is not passed, the roots are `capture_roots(data_dir)` (`hook list` and `to-trace` take `--data-dir` already or gain it with default `data_home()`). `hook list` text output gains the root as a trailing column only when more than one root holds sessions; JSON rows carry `root` always.
- Warden: after a mirrored record is written, `maybe_trigger_background_tend(captures_root.parent, now=time.time())` inside the same best-effort `try`. For `<data>/warden/captures` the parent is `<data>/warden`, so pass `captures_root.parent.parent` when `captures_root.parent.name == "warden"`, else `captures_root.parent`. (A fence or sub-agent root has no `config.yaml`, so `auto_tend_enabled` is false there and nothing spawns.)

- [ ] **Step 1: Write the failing tests**

In `tests/test_hook.py`:

```python
def _cap(root, sid, n=1):
    root.mkdir(parents=True, exist_ok=True)
    (root / f"{sid}.jsonl").write_text(
        "".join(
            json.dumps(
                {
                    "captured_at": 1.0 + i,
                    "session_id": sid,
                    "tool_name": "Bash",
                    "step_type": "shell",
                    "command": "ls",
                }
            )
            + "\n"
            for i in range(n)
        )
    )


def test_capture_roots_primary_first(tmp_path):
    from opendaisugi.hook import capture_roots

    assert capture_roots(tmp_path) == [tmp_path / "captures", tmp_path / "warden" / "captures"]


def test_list_sessions_in_merges_roots_primary_wins(tmp_path):
    from opendaisugi.hook import capture_roots, list_sessions_in

    _cap(tmp_path / "captures", "same", 2)
    _cap(tmp_path / "warden" / "captures", "same", 5)
    _cap(tmp_path / "warden" / "captures", "pi-only", 1)
    rows = {r["session_id"]: r for r in list_sessions_in(capture_roots(tmp_path))}
    assert rows["same"]["calls"] == 2 and rows["same"]["root"] == str(tmp_path / "captures")
    assert rows["pi-only"]["root"] == str(tmp_path / "warden" / "captures")


def test_find_capture_checks_roots_in_order(tmp_path):
    from opendaisugi.hook import capture_roots, find_capture

    _cap(tmp_path / "warden" / "captures", "x")
    assert (
        find_capture("x", capture_roots(tmp_path)) == tmp_path / "warden" / "captures" / "x.jsonl"
    )
    assert find_capture("nope", capture_roots(tmp_path)) is None
```

In the CLI test file that drives `hook auto-tend` (add beside its existing cases):

```python
def test_auto_tend_converts_the_warden_root_and_skips_the_duplicate(tmp_path, runner):
    data = tmp_path / "d"
    (data / "config.yaml").parent.mkdir(parents=True)
    (data / "config.yaml").write_text("auto_tend: true\n")
    _cap(data / "captures", "claude-s", 2)
    _cap(data / "warden" / "captures", "claude-s", 2)
    _cap(data / "warden" / "captures", "pi-s", 1)
    res = runner.invoke(
        app, ["hook", "auto-tend", "--data-dir", str(data), "--force", "--skip-distill"]
    )
    assert res.exit_code == 0, res.output
    assert res.output.count("converted claude-s") == 1
    assert "converted pi-s" in res.output
    assert "converted 2 sessions" in res.output


def test_auto_tend_explicit_captures_root_uses_only_that_root(tmp_path, runner):
    data = tmp_path / "d"
    data.mkdir()
    _cap(data / "warden" / "captures", "pi-s", 1)
    _cap(tmp_path / "other", "o", 1)
    res = runner.invoke(
        app,
        [
            "hook",
            "auto-tend",
            "--data-dir",
            str(data),
            "--captures-root",
            str(tmp_path / "other"),
            "--force",
            "--skip-distill",
        ],
    )
    assert "converted o" in res.output and "pi-s" not in res.output
```

In `tests/test_hook_auto_trigger.py`:

```python
def test_resident_mirror_triggers_auto_tend_from_the_data_home(tmp_path, monkeypatch):
    import opendaisugi.warden as w

    spawned = []
    monkeypatch.setattr("opendaisugi.hook._spawn_detached_auto_tend", lambda d: spawned.append(d))
    data = tmp_path / "d"
    (data).mkdir()
    (data / "config.yaml").write_text("auto_tend: true\n")
    root = _gate_root(data / "warden", _envelope(shell_allowlist=["ls"]))  # the file's helpers
    w.warden_and_contract(
        _payload("bash", {"command": "ls"}),
        root=root,
        fmt="pi",
        captures_root=data / "warden" / "captures",
    )
    assert spawned == [data]
```

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_hook.py tests/test_hook_auto_trigger.py -k "capture_roots or list_sessions_in or find_capture or resident_mirror"` and the auto-tend CLI tests.
Expected: FAIL (`ImportError`; one conversion missing; `spawned == []`).

- [ ] **Step 3: Implement**

The three helpers in `hook.py`. `list_sessions` stays as the one-root form and `list_sessions_in` folds it over roots with a `seen` set. The three commands: `roots = [captures_root] if captures_root_was_passed else capture_roots(data_dir)` (typer: make the option default `None` and keep the old default in the help text). `to-trace` uses `find_capture`; its "no capture at" message names every root searched. The warden mirror block gains the trigger. `hook list --json` row key order: `session_id, calls, first_at, last_at, root`.

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_hook.py tests/test_hook_auto_trigger.py tests/test_cli_pipe.py tests/test_start.py tests/test_hook_format_pi.py`
Expected: PASS. `test_start_gives_the_resident_gate_the_captures_root_the_fence_gives` still passes unchanged (R-W3-2).

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/hook.py src/opendaisugi/cli.py src/opendaisugi/warden.py tests/test_hook.py tests/test_hook_auto_trigger.py tests/test_cli_pipe.py
git commit -m "hook: the journal feed reads the warden's captures; the resident mirror triggers auto-tend

pi and OpenCode calls reach the journal only through the resident warden's
mirror, which writes under <data>/warden/captures. hook list, to-trace and
auto-tend scanned <data>/captures alone, and nothing on the resident path
ever started a tend. The feed now reads both roots, primary first, and the
mirror calls the same consent-gated trigger the capture hook uses."
```

### Task 4: The OpenClaw plugin sends the session id the host gives it

**Files:**
- Modify: `src/opendaisugi/install_assets/openclaw_plugin/index.mjs`
- Modify: `clients/go/internal/install/assets/openclaw_plugin/index.mjs` (same bytes; `tests/test_integrations_openclaw.py` or a new test asserts the two files are identical, as `tests/harness_opencode/test_plugin_copies.py` does for the OpenCode plugin)
- Create: `tests/harness_openclaw/__init__.py`, `tests/harness_openclaw/run_before_tool_call.mjs`, `tests/harness_openclaw/test_plugin_payload.py`

**Interfaces:**
- The handler is `api.on("before_tool_call", async (event, ctx) => ...)`. The payload written to `daisugi hook record --format openclaw` stdin is `{"tool": event.toolName, "args": event.params, "session_id": (ctx && ctx.sessionId) || event.sessionId || "no-session", "tool_use_id": event.toolCallId || "", "run_id": (ctx && ctx.runId) || event.runId || ""}`. Keys in that order.
- The plugin still never blocks (passive) and still fails open on a stalled `daisugi`.
- `record(event, ctx)` takes a second argument; the exported shape for tests: `export { payloadFor }` where `payloadFor(event, ctx)` returns the object above. (`definePluginEntry` import stays; the test driver stubs the module path with a Node `--import` loader or copies the file and rewrites the one import line to a local stub, as `tests/harness_opencode/drive.mjs` does for OpenCode's `@opencode-ai/plugin` import. Follow that file.)

- [ ] **Step 1: Write the failing test**

`tests/harness_openclaw/test_plugin_payload.py`:

```python
import json
import subprocess
from pathlib import Path

import pytest

from tests.harness_pi._node_check import node_status

HERE = Path(__file__).parent
PLUGIN = Path("src/opendaisugi/install_assets/openclaw_plugin/index.mjs")

ok, why = node_status()
pytestmark = pytest.mark.skipif(not ok, reason=why)


def run(event, ctx):
    res = subprocess.run(
        [
            "node",
            str(HERE / "run_before_tool_call.mjs"),
            str(PLUGIN.resolve()),
            json.dumps(event),
            json.dumps(ctx),
        ],
        capture_output=True,
        text=True,
        timeout=20,
        check=True,
    )
    return json.loads(res.stdout)


def test_session_id_comes_from_ctx():
    p = run(
        {"toolName": "exec", "params": {"command": "ls"}, "toolCallId": "tc-9"},
        {"sessionId": "oc-sess", "runId": "run-1", "agentId": "main"},
    )
    assert p == {
        "tool": "exec",
        "args": {"command": "ls"},
        "session_id": "oc-sess",
        "tool_use_id": "tc-9",
        "run_id": "run-1",
    }


def test_no_session_anywhere_is_no_session():
    p = run({"toolName": "read", "params": {"path": "a"}}, {})
    assert p["session_id"] == "no-session" and p["tool_use_id"] == ""


def test_go_copy_is_byte_identical():
    go = Path("clients/go/internal/install/assets/openclaw_plugin/index.mjs")
    assert go.read_bytes() == PLUGIN.read_bytes()
```

`run_before_tool_call.mjs` imports `payloadFor` from the plugin file (through a stub for `openclaw/plugin-sdk/plugin-entry` that exports `definePluginEntry = (x) => x`, the same device `tests/harness_opencode/drive.mjs` uses) and prints `JSON.stringify(payloadFor(event, ctx))`.

- [ ] **Step 2: Run it to verify it fails**

Run: `uv run pytest -q tests/harness_openclaw`
Expected: FAIL (`payloadFor` is not exported; session id is `no-session`). If Node 22 is absent the tests skip; then the byte-identity test must still run: keep it outside the `skipif` (module-level mark only on the two Node tests).

- [ ] **Step 3: Implement**

The `payloadFor` function, the `(event, ctx)` handler, the export. Copy the file to the Go asset path. The header comment cites `https://docs.openclaw.ai/plugins/hooks/tool-policy` for the event and ctx fields.

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/harness_openclaw tests/test_integrations_openclaw.py tests/test_install.py`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/install_assets/openclaw_plugin/index.mjs clients/go/internal/install/assets/openclaw_plugin/index.mjs tests/harness_openclaw
git commit -m "openclaw plugin: take the session id from ctx, the field the host sends

before_tool_call carries sessionId on ctx, not on the event, so every
OpenClaw capture landed under no-session. Also carries toolCallId and runId."
```

### Task 5: Golden cases for the recording paths (oracle side)

**Files:**
- Modify: `clients/k4_cases.py` (`build_hook_cases`; new `build_hook_feed_cases`)
- Modify: `clients/warden_server_cases.py` (the mirror cases)
- Regenerate: `clients/fixtures/k4/`, `clients/fixtures/warden_server/`

**Interfaces:**
- Consumes: `hook(name, *argv, stdin=...)` and `case(...)` in k4; the server case helpers in `warden_server_cases.py` (a request is `{"v":1,"argv":[...],"stdin_b64":...}`; the server is started with `--root` and `--captures-root` by the writer; find the existing captures case and copy its shape).

- [ ] **Step 1: Add the k4 hook record cases**

With `terminal = {"hook_event_name": "pre_tool_call", "tool_name": "terminal", "tool_input": {"command": "ls"}, "session_id": "h", "cwd": "/w", "extra": {"tool_call_id": "c"}}` and `exec_ = {"tool": "exec", "args": {"command": "ls"}, "session_id": "o", "tool_use_id": "t"}`:

| name | argv | stdin |
|---|---|---|
| `hook record hermes terminal` | `--format hermes` | `terminal` |
| `hook record hermes read relative` | `--format hermes` | terminal with `tool_name: read_file`, `tool_input: {"path": "a.txt"}` |
| `hook record hermes write` | `--format hermes` | `write_file`, `{"path": "/w/o", "content": "xyz"}` |
| `hook record hermes web urls` | `--format hermes` | `web_extract`, `{"urls": ["https://a/b"]}` |
| `hook record hermes unknown` | `--format hermes` | `vision_analyze`, `{"image": "x"}` |
| `hook record hermes name under claude` | `--format claude` | `terminal` |
| `hook record openclaw exec` | `--format openclaw` | `exec_` |
| `hook record openclaw apply patch` | `--format openclaw` | `apply_patch`, `{"patch": "*** Begin Patch\n*** Add File: /w/n\n+x\n*** End Patch"}` |
| `hook record openclaw web fetch` | `--format openclaw` | `web_fetch`, `{"url": "https://x/y"}` |
| `hook record openclaw unknown` | `--format openclaw` | `message`, `{"action": "send"}` |
| `hook record pi lowercase bash` | `--format pi` | `{"tool_name": "bash", "tool_input": {"command": "ls"}, "session_id": "p"}` |
| `hook record pi unknown is mcp` | `--format pi` | `{"tool_name": "ls", "tool_input": {}, "session_id": "p"}` |

- [ ] **Step 2: Add the k4 feed cases**

`build_hook_feed_cases()`, wired into `main` beside the other builders, with `before` trees that hold capture files (use the `cap(sid, n)` shape from Task 3's tests, under `.opendaisugi/captures/` and `.opendaisugi/warden/captures/`):

| name | argv | before |
|---|---|---|
| `hook list two roots` | `hook list` | `same` in both, `pi-only` in warden |
| `hook list two roots json` | `hook list --json` | the same |
| `hook list one root flag` | `hook list --captures-root {HOME}/.opendaisugi/warden/captures` | the same |
| `hook to-trace from warden root` | `hook to-trace pi-only` | `pi-only` in warden only |
| `hook to-trace missing names both roots` | `hook to-trace nope` | empty |
| `hook auto-tend two roots` | `hook auto-tend --force --skip-distill` | `config.yaml` with `auto_tend: true`; `same` in both, `pi-only` in warden |

- [ ] **Step 3: Add the warden server mirror cases**

In `warden_server_cases.py`, beside the existing captures case: `server mirror pi bash` (argv `["--format","pi","--verify-timeout","4"]`, payload `{"tool_name":"bash","tool_input":{"command":"ls"},"session_id":"s"}`, an envelope that allows `ls`; expect `captures/s.jsonl` with one record, `tool_name` `bash`), `server mirror pi denied` (command `rm -rf /`; expect no captures file), `server mirror opencode read` (`--format opencode`, `{"tool_name":"Read","tool_input":{"filePath":"/allowed/a"},...}`), `server mirror codex` (`--format codex`, a `Bash` payload).

- [ ] **Step 4: Regenerate and read**

Run: `DAISUGI_K4_SCRATCH=/mnt/tera/.tt/w3/k4 uv run --no-sync python clients/k4_cases.py` then the warden server writer with its scratch variable. Then: `grep -c '"tool_name": "terminal"' clients/fixtures/k4/cases.jsonl` (at least 1), and open `hook record hermes name under claude` to confirm no file is written. Confirm `server mirror pi denied` leaves no captures directory.

- [ ] **Step 5: Commit**

```bash
git add clients/k4_cases.py clients/warden_server_cases.py clients/fixtures/k4 clients/fixtures/warden_server
git commit -m "k4 and warden server cases: Hermes, OpenClaw and pi names record; the feed reads both roots"
```

### Task 6: The Go port of Tasks 1 to 3

**Files:**
- Modify: `clients/go/internal/warden/hookrecord.go` (`recordCall` takes the runner's `fmt`), `clients/go/internal/warden/warden.go` (the mirror passes `r.fmt`), `clients/go/internal/warden/record.go` or wherever `payloadToRecord` and `classifyTool` live (the two maps, the `urls[0]` form, the `apply_patch` path), `clients/go/internal/warden/logs.go` (the trigger after the mirror)
- Modify: `clients/go/internal/cli/hookcapcmd.go` (`hook list`, `hook to-trace`), `clients/go/internal/cli/autotendcmd.go`
- Test: `clients/go/internal/warden/hookrecord_test.go`, `clients/go/internal/cli/hookcmd_test.go`

**Interfaces:** the same names as the oracle: `CaptureRoots(dataDir) []string`, `ListSessionsIn(roots)`, `FindCapture(sid, roots)`. JSON rows gain `root` last.

- [ ] **Step 1: Write the failing tests**

In `hookrecord_test.go`:

```go
func TestRecordCallKeepsPiLowercaseBash(t *testing.T) {
	root := t.TempDir()
	r := &runner{env: map[string]string{}, fmt: "pi"}
	r.recordCall([]byte(`{"tool_name":"bash","tool_input":{"command":"ls"},"session_id":"s"}`), root)
	b, err := os.ReadFile(filepath.Join(root, "s.jsonl"))
	if err != nil {
		t.Fatalf("no capture written: %v", err)
	}
	if !strings.Contains(string(b), `"step_type": "shell"`) {
		t.Fatalf("record = %s", b)
	}
}

func TestClassifyHermesAndOpenClaw(t *testing.T) {
	for _, c := range []struct{ fmt, name, want string }{
		{"hermes", "terminal", "shell"}, {"hermes", "read_file", "file_read"},
		{"hermes", "patch", "file_write"}, {"hermes", "web_extract", "network"},
		{"hermes", "vision_analyze", ""}, {"claude", "terminal", ""},
		{"openclaw", "exec", "shell"}, {"openclaw", "apply_patch", "file_write"},
		{"openclaw", "web_fetch", "network"}, {"openclaw", "message", ""},
	} {
		got, _ := classifyTool(c.name, c.fmt)
		if got != c.want {
			t.Errorf("%s %s = %q, want %q", c.fmt, c.name, got, c.want)
		}
	}
}
```

In `hookcmd_test.go`, a `hook auto-tend --force --skip-distill` run over a HOME with the two roots, asserting `converted claude-s` appears once and `converted 2 sessions`.

- [ ] **Step 2: Build and run them to verify they fail**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd clients/go && go test -p 1 ./internal/warden/ ./internal/cli/ -run "RecordCall|ClassifyHermes|AutoTend" 2>&1 | tail -20'`
Expected: FAIL.

- [ ] **Step 3: Implement, then build**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh`

- [ ] **Step 4: Compare**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/k4_compare.py --binary clients/go/daisugi --only "hook record"`, then `--only "hook list"`, `--only "hook to-trace"`, `--only "hook auto-tend"`, then `clients/warden_server_compare.py --binary clients/go/daisugi --only "server mirror"`.
Expected: 0 disagreements. Any refusal gets a ruling in `clients/ADJUDICATIONS.md`.

- [ ] **Step 5: Commit**

```bash
git add clients/go/internal/warden clients/go/internal/cli/hookcapcmd.go clients/go/internal/cli/autotendcmd.go clients/go/internal/cli/hookcmd_test.go
git commit -m "go: capture records classify under the host format; the feed reads both roots"
```

### Task 7: The Rust port of Tasks 1 to 3

**Files:**
- Modify: `clients/rust/src/warden/record.rs` (`payload_to_record` callers pass the format; the two maps), `clients/rust/src/warden/logs.rs` (line that calls `payload_to_record(p, "claude")` passes `self.fmt`; the trigger), `clients/rust/src/cli/hookcmd.rs`, `clients/rust/src/cli/hookcapcmd.rs`, `clients/rust/src/cli/autotendcmd.rs`
- Test: unit tests in the same files under `#[cfg(test)]`, mirroring Task 6's.

- [ ] **Step 1: Write the failing tests** (the same three as Task 6, in Rust)
- [ ] **Step 2: Run them**

Run: `. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path clients/rust/Cargo.toml record_call classify_hermes auto_tend 2>&1 | tail -8`
Expected: FAIL, then PASS after Step 3.

- [ ] **Step 3: Implement and build**

Run: `. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml`

- [ ] **Step 4: Compare** (the same four `k4_compare.py --only` runs and the `warden_server_compare.py --only "server mirror"` run, with `--binary clients/rust/target/release/daisugi`). Expected: 0 disagreements.

- [ ] **Step 5: Commit**

```bash
git add clients/rust/src/warden clients/rust/src/cli/hookcmd.rs clients/rust/src/cli/hookcapcmd.rs clients/rust/src/cli/autotendcmd.rs
git commit -m "rust: capture records classify under the host format; the feed reads both roots"
```

### Task 8a: `journal parse --format pi` (history import; built in this wave, owner ruling B10)

**Files:**
- Create: `src/opendaisugi/parsers/pi.py`
- Modify: `src/opendaisugi/parsers/__init__.py` (`register_parser("pi", PiParser)`), `src/opendaisugi/cli.py` (`journal parse --format` help lists `claude-code | codex | pi | opencode`; `onboard` the same if it names formats)
- Create: `tests/test_pi_parser.py`
- Modify: `clients/f_cases.py` (pi cases), regenerate `clients/fixtures/f/`

**Source:** `https://pi.dev/docs/latest/session-format` and `https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/ai/src/types.ts` (fetched 2026-10-30). A session is JSONL, `version` 3. The first line is `{"type":"session","version":3,"id","timestamp","cwd","parentSession"?}`. Every other line has `id`, `parentId` (`null` at the root), `type`, `timestamp` (ISO 8601). Entry types: `session`, `message`, `model_change`, `thinking_level_change`, `usage`, `compaction`, `context_edit`, `branch_summary`, `custom`, `custom_message`, `label`, `session_info`. A `message` entry is `{"type":"message","id","parentId","timestamp","message":{"role","content","timestamp"}}`. An assistant message's `content` is a list of `{"type":"text"}`, `{"type":"thinking"}` and `{"type":"toolCall","id","name","arguments"}` blocks. A tool result is its own message with `role: "toolResult"`, `toolCallId`, `toolName`, `content`, `isError`. Sessions live at `~/.pi/agent/sessions/<cwd-slug>/<timestamp>_<uuid>.jsonl`. Tool names are lowercase (`bash`, `read`, `write`, `edit`, `grep`, `find`, `ls`); argument names `command`, `path` as `harness_pi/extension/PINS.md` records.

**Interfaces:**
- `PiParser(ClaudeCodeParser)`, `SOURCE = "pi"`. `_read_messages(path)` translates each `message` entry into the flat shape: `role` `user` or `assistant` as given; a `toolCall` block becomes `{"type":"tool_use","id":<id>,"name":<Claude name>,"input":<arguments>}` with the name mapped `bash→Bash`, `read→Read`, `write→Write`, `edit→Edit`, `grep→Grep`, `find→Glob`, `ls→Glob`; any other name becomes `mcp__pi__<name>`. `toolResult` messages are dropped (as Codex outputs are). Each flat message carries `uuid` from `id`, `parentUuid` from `parentId`, `sessionId` from the header's `id`. The header and every other entry type are dropped. Malformed lines are skipped.
- `ActionPlan.source` in `ingest.py` stays `claude-code-import` (ingest does not branch on source today; leave it).

- [ ] **Step 1: Write the failing tests** (`tests/test_pi_parser.py`; every transcript written by the test)

```python
import json

from opendaisugi.parsers import get_parser


def line(**d):
    return json.dumps(d)


def session(tmp_path, *entries):
    p = tmp_path / "s.jsonl"
    p.write_text(
        "\n".join([line(type="session", version=3, id="sess", timestamp="t", cwd="/w"), *entries])
        + "\n"
    )
    return p


def user(i, text, parent=None):
    return line(
        type="message",
        id=i,
        parentId=parent,
        timestamp="t",
        message={"role": "user", "content": [{"type": "text", "text": text}], "timestamp": 1},
    )


def asst(i, parent, *calls):
    return line(
        type="message",
        id=i,
        parentId=parent,
        timestamp="t",
        message={"role": "assistant", "content": list(calls), "timestamp": 1},
    )


def call(name, **args):
    return {"type": "toolCall", "id": f"c-{name}", "name": name, "arguments": args}


def result(i, parent, name):
    return line(
        type="message",
        id=i,
        parentId=parent,
        timestamp="t",
        message={
            "role": "toolResult",
            "toolCallId": f"c-{name}",
            "toolName": name,
            "content": [],
            "isError": False,
        },
    )


def test_pi_session_becomes_one_episode_with_mapped_tools(tmp_path):
    p = session(
        tmp_path,
        user("u1", "fix the build"),
        asst(
            "a1",
            "u1",
            call("bash", command="make"),
            call("read", path="/w/a.py"),
            call("write", path="/w/b.py", content="x"),
        ),
        result("r1", "a1", "bash"),
        line(type="usage", id="x", parentId="a1", timestamp="t", input=1),
    )
    res = get_parser("pi", min_tools=1).parse(p)
    assert res.source == "pi"
    assert len(res.episodes) == 1
    ep = res.episodes[0]
    assert ep.task == "fix the build"
    assert [s.type for s in ep.steps] == ["shell", "file_read", "file_write"]
    assert ep.steps[0].command == "make"


def test_pi_unknown_tool_is_mcp_under_pi(tmp_path):
    p = session(tmp_path, user("u1", "t"), asst("a1", "u1", call("ls", path="/w")))
    ep = get_parser("pi", min_tools=1).parse(p).episodes[0]
    assert ep.steps[0].type == "mcp" and ep.steps[0].server == "pi" and ep.steps[0].tool == "ls"


def test_pi_tree_ids_survive(tmp_path):
    from opendaisugi.parsers.pi import PiParser

    p = session(tmp_path, user("u1", "t"), asst("a1", "u1", call("bash", command="ls")))
    msgs = PiParser()._read_messages(p)
    assert [(m["uuid"], m["parentUuid"], m["sessionId"]) for m in msgs] == [
        ("u1", None, "sess"),
        ("a1", "u1", "sess"),
    ]


def test_pi_bad_lines_are_skipped(tmp_path):
    p = tmp_path / "s.jsonl"
    p.write_text(
        "{nope\n" + user("u1", "t") + "\n" + asst("a1", "u1", call("bash", command="ls")) + "\n"
    )
    assert len(get_parser("pi", min_tools=1).parse(p).episodes) == 1
```

- [ ] **Step 2: Run them to verify they fail** (`ValueError: Unknown parser format: 'pi'`)
- [ ] **Step 3: Implement** `parsers/pi.py` (about 90 lines; copy `codex.py`'s shape), register it, update the help strings.
- [ ] **Step 4: Golden cases.** In `clients/f_cases.py` add `pi` cases beside the codex ones (`parse("pi one episode", <text>, "--format", "pi", ...)`, `pi unknown tool`, `pi bad lines`, `pi empty file`, and one `journal ingest` of the pi output). Regenerate with the f writer under `DAISUGI_F_SCRATCH=/mnt/tera/.tt/w3/f` (or the variable the writer names). Go: `clients/go/internal/transcript/pi.go` mirroring `codex.go`; Rust: `clients/rust/src/transcript/pi.rs`. Run `f_compare.py --only pi` for both binaries, one at a time under the cap.
- [ ] **Step 5: Run** `uv run pytest -q tests/test_pi_parser.py tests/test_parsers.py tests/test_codex_parser.py` and commit:

```bash
git add src/opendaisugi/parsers/pi.py src/opendaisugi/parsers/__init__.py src/opendaisugi/cli.py tests/test_pi_parser.py clients/f_cases.py clients/fixtures/f clients/go/internal/transcript clients/rust/src/transcript
git commit -m "journal parse: pi sessions (JSONL v3) become episodes, for history recorded before install"
```

### Task 8b: `journal parse --format opencode` (history import; built in this wave, owner ruling B10)

**Source:** `https://unpkg.com/@opencode-ai/sdk/dist/gen/types.gen.d.ts` (types) and the storage tree as third-party readers describe it (`https://docs.rs/lore-cli/0.1.12/src/lore_cli/capture/watchers/opencode.rs.html`): `~/.local/share/opencode/storage/session/<projectID>/<sessionID>.json`, `message/<sessionID>/<messageID>.json`, `part/<messageID>/<partID>.json`. A `ToolPart` is `{"id","sessionID","messageID","type":"tool","callID","tool","state":{"status","input","output","title","metadata","time":{"start","end"}}}`. A `UserMessage` is `{"id","sessionID","role":"user","time":{"created"}}` with its text in a `TextPart` (`type: "text"`, `text`). Whether a current release still writes this tree is **unverified** (the layout "has shifted across releases"); the parser takes a session directory or a single session id and says so in its help.

**Interfaces:**
- `OpenCodeParser(ClaudeCodeParser)`, `SOURCE = "opencode"`. `parse(path)` accepts a `storage/` directory plus `--session <id>` (the CLI option exists for this format only) or a path `storage/session/<project>/<sid>.json`. It reads `message/<sid>/*.json` sorted by `time.created`, and for each message the parts under `part/<messageID>/*.json`; a user message with a text part becomes a flat user message; an assistant message's tool parts become `tool_use` blocks with the name mapped through the OpenCode plugin's table (`bash→Bash`, `read→Read`, `write→Write`, `edit→Edit`, `glob→Glob`, `grep→Grep`, `webfetch→WebFetch`, `websearch→WebSearch`, others `mcp__opencode__<tool>`), input `state.input`. Parts whose `state.status` is `pending` or `error` are dropped.
- Tests build the tree with the writer from the test (`storage/session/p/ses_1.json`, `message/ses_1/msg_1.json`, `part/msg_1/prt_1.json`); the same four shapes as Task 8a (one episode, unknown tool, bad file skipped, missing dir is a clean parse error exit 2).
- Golden cases in `f_cases.py` (`opencode one episode`, `opencode error part dropped`, `opencode missing session`), Go `transcript/opencode.go`, Rust `transcript/opencode.rs`, `f_compare.py --only opencode`.

- [ ] Steps 1 to 5 as in Task 8a. Commit message: `journal parse: OpenCode storage trees become episodes, for history recorded before install`.

### Task 9: Docs for recording, and the box checks

**Files:**
- Modify: `docs/hook-integration.md` (the Hermes and OpenClaw sections: the payload shapes, the tool names that classify, the unverified argument names, the box-check status in one sentence each), `docs/integrations.md` (one row per harness: recorder, where captures land, how they reach the journal), `docs/feature-status.md` (recording row per harness), `src/opendaisugi/harness_pi/extension/PINS.md` (one line: captures land under `<data>/warden/captures` through the resident mirror, Task 3), `docs/how-to/import-old-chats.md` (pi and OpenCode formats, with the unverified note for OpenCode's tree)
- Append: `.superpowers/sdd/backlog/progress.md` (the box-check list below)

**Box checks (on the GPU box or a friend's install; not runnable here):**
1. Hermes: `daisugi install --runtime hermes --yes` on a box with Hermes; run one `terminal` call; `daisugi hook list` shows the session; confirm the stdin shape has `tool_input` (not `args`) and record the `web_extract` argument name.
2. OpenClaw: after `daisugi install --runtime openclaw --yes`, confirm whether the extension loads without `plugins.entries.opendaisugi.enabled` (R-W3-7); run one `exec`; confirm `ctx.sessionId` is set; record `read`/`write`/`edit` argument names.
3. pi: with `daisugi start` up, run one `bash` call in pi; `daisugi hook list` shows the session under the warden root; `hook auto-tend --force` converts it.
4. OpenCode: the same as 3, and confirm whether the storage tree in Task 8b still exists on the installed release.

- [ ] **Step 1: Write the docs.** Each page states the current behaviour once. Each unverified field is named as unverified beside its URL.
- [ ] **Step 2: Run** `grep -nE " — |seam|the layer|now |no longer|used to" docs/hook-integration.md docs/integrations.md docs/how-to/import-old-chats.md` and fix any hit in new text.
- [ ] **Step 3: Commit**

```bash
git add docs/hook-integration.md docs/integrations.md docs/feature-status.md docs/how-to/import-old-chats.md src/opendaisugi/harness_pi/extension/PINS.md
git commit -m "docs: how each harness's calls reach the journal, and what only a real install can prove"
```

---

## Part B: install only what I name

### Task 10: The oracle: `--only`, `--list`, component names, symmetric uninstall

**Files:**
- Modify: `src/opendaisugi/install.py` (`COMPONENTS`, `component_layers`, `reverse(home, layers=None)` on every runtime, `uninstall(..., layers=None)`)
- Modify: `src/opendaisugi/cli.py` (`install_cmd`: `--only`, `--list`)
- Test: `tests/test_install_only.py` (new), `tests/test_install.py`, `tests/test_cli_install_help.py`

**Interfaces:**
- `install.COMPONENTS: tuple[tuple[str, Layer, str, str], ...]` in this order:
  `("skill", Layer.SKILL, "the opendaisugi-checklist skill directory", "claude codex hermes openclaw")`,
  `("mcp", Layer.MCP, "the opendaisugi MCP server entry", "claude codex hermes openclaw")`,
  `("capture", Layer.CAPTURE, "the passive capture hook (daisugi hook record)", "claude hermes openclaw")`,
  `("instructions", Layer.INSTRUCTIONS, "the pathway note in the harness instructions file", "claude codex openclaw")`,
  `("warden", Layer.WARDEN, "the warden hook, audit mode; --enforce makes it deny", "claude codex")`,
  `("router", Layer.BASE_URL, "the harness base_url pointed at the router", "claude codex openclaw")`.
  `DEFAULT_COMPONENTS = ("skill", "mcp", "capture", "instructions", "warden")` (the five W1 installs).
- `install.component_layers(names: list[str]) -> set[Layer]`: raises `ValueError("unknown component 'x'. Components: skill, mcp, capture, instructions, warden, router. Run: daisugi install --list")` on an unknown name. Names are split on commas and stripped; duplicates fold.
- `install.list_components() -> str`: a table, one line per component: `name` left-justified to 13, the description, then ` (` + the harness list + `)`. After the table, a blank line, then `Default: skill, mcp, capture, instructions, warden. Add router with --router or --only router.` and `The pi and OpenCode extensions are separate: daisugi install --harness pi|opencode.`
- Every runtime's `reverse(self, home, layers=None)`: `None` means everything it manages (today's behaviour); a set removes only those layers. Claude Code: SKILL → the skill link, MCP → the `~/.claude.json` entry, CAPTURE → the record hooks (PreToolUse and the report hooks that `is_record_hook` matches), INSTRUCTIONS → the managed block, WARDEN → the warden hook entry, BASE_URL → the env key. Codex, Hermes, OpenClaw: the same split over what each writes today. `uninstall(*, home, runtimes, layers=None)` passes it through.
- CLI: `--only TEXT` (repeatable, `help="Install only these components, comma-separated or repeated: skill, mcp, capture, instructions, warden, router. Default: the first five. See --list."`), placed right after `--harness`; `--list` (`help="List the components install can write, and stop."`), placed right after `--print-skill`. Behaviour:
  1. `--list` prints `list_components()` and exits 0 before any detection.
  2. `selected = component_layers(only) if only else set(DEFAULT_LAYERS)`. Unknown name: the `ValueError` text on stderr, exit 2.
  3. `--enforce` with `only` lacking `warden`: stderr `--enforce needs the warden component: add warden to --only` and exit 2, before the envelope check.
  4. `--router` with `only` lacking `router`: stderr `--router is a component: add router to --only, or drop --router` and exit 2. Without `--only`, `--router` adds `Layer.BASE_URL` as today.
  5. `--only` with `--harness`: allowed; `--harness` runs its own path first as today and returns, so `--only` is ignored there. Print one line `Note: --only does not apply to --harness.` before the harness path when both are given.
  6. `--uninstall --only a,b` removes only those; the summary line becomes `Uninstalled skill, mcp from: Claude Code` (component names in COMPONENTS order). `--uninstall` alone is unchanged.
  7. The plan print and the "Tool calls are captured" line: the captures line prints only when `Layer.CAPTURE in selected`; the warden paragraph only when `Layer.WARDEN in selected`.

- [ ] **Step 1: Write the failing tests** (`tests/test_install_only.py`)

```python
import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.install import COMPONENTS, DEFAULT_LAYERS, Layer, component_layers, list_components

runner = CliRunner()


def test_component_names_and_order():
    assert [c[0] for c in COMPONENTS] == [
        "skill",
        "mcp",
        "capture",
        "instructions",
        "warden",
        "router",
    ]


def test_component_layers_splits_commas_and_folds():
    assert component_layers(["warden,capture", "warden"]) == {Layer.WARDEN, Layer.CAPTURE}


def test_component_layers_unknown_names_the_list():
    with pytest.raises(
        ValueError,
        match="unknown component 'journal'. Components: skill, mcp, capture, instructions, warden, router",
    ):
        component_layers(["warden,journal"])


def test_list_prints_six_rows_and_the_default_line():
    text = list_components()
    assert text.count("\n") >= 8
    assert "Default: skill, mcp, capture, instructions, warden." in text
    assert "--harness pi|opencode" in text


def test_install_list_exits_before_detection(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)  # no runtimes at all
    res = runner.invoke(app, ["install", "--list"])
    assert res.exit_code == 0 and "warden " in res.output and "No supported" not in res.output


def test_only_warden_writes_the_hook_and_nothing_else(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "warden", "--yes", "--runtime", "claude"])
    assert res.exit_code == 0, res.output
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    cmds = [h["command"] for e in settings["hooks"]["PreToolUse"] for h in e["hooks"]]
    assert any("warden" in c for c in cmds) and not any("hook record" in c for c in cmds)
    assert not (tmp_path / ".claude.json").exists()
    assert not (tmp_path / ".claude" / "CLAUDE.md").exists()
    assert "Tool calls are captured" not in res.output


def test_only_unknown_is_exit_2(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "warden,journal", "--yes"])
    assert res.exit_code == 2 and "unknown component 'journal'" in res.output
    assert not (tmp_path / ".claude" / "settings.json").exists()


def test_enforce_needs_warden_in_only(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "skill", "--enforce", "--yes"])
    assert res.exit_code == 2 and "--enforce needs the warden component" in res.output


def test_router_flag_needs_router_in_only(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    res = runner.invoke(app, ["install", "--only", "skill", "--router", "--yes"])
    assert res.exit_code == 2 and "--router is a component" in res.output


def test_uninstall_only_removes_the_named_components(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    assert runner.invoke(app, ["install", "--yes", "--runtime", "claude"]).exit_code == 0
    res = runner.invoke(
        app, ["install", "--uninstall", "--only", "capture,instructions", "--runtime", "claude"]
    )
    assert res.exit_code == 0, res.output
    assert "Uninstalled capture, instructions from: Claude Code" in res.output
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    cmds = [h["command"] for e in settings["hooks"].get("PreToolUse", []) for h in e["hooks"]]
    assert any("warden" in c for c in cmds) and not any("hook record" in c for c in cmds)
    assert "opendaisugi" in (tmp_path / ".claude.json").read_text()
    assert (
        not (tmp_path / ".claude" / "CLAUDE.md").exists()
        or "opendaisugi-managed" not in (tmp_path / ".claude" / "CLAUDE.md").read_text()
    )


def test_default_without_only_is_the_five():
    assert DEFAULT_LAYERS == {
        Layer.SKILL,
        Layer.MCP,
        Layer.CAPTURE,
        Layer.INSTRUCTIONS,
        Layer.WARDEN,
    }
```

(If `Path.home` is not how the existing install tests pick a HOME, use the fixture they use: `grep -n "HOME\|monkeypatch" tests/test_install.py | head`.)

- [ ] **Step 2: Run them to verify they fail**

Run: `uv run pytest -q tests/test_install_only.py`
Expected: FAIL (`ImportError: COMPONENTS`; `No such option: --only`).

- [ ] **Step 3: Implement**

`COMPONENTS`, `component_layers`, `list_components`, `reverse(home, layers)` per runtime (split the body each runtime has today by layer; `None` keeps the full list), `uninstall(layers=...)`, the CLI flags and the seven behaviours. The `install_cmd` docstring gains one sentence: `--only names the components to write; --list shows them.`

- [ ] **Step 4: Run the tests**

Run: `uv run pytest -q tests/test_install_only.py tests/test_install.py tests/test_install_gate_baseurl.py tests/test_install_gate_report.py tests/test_cli_install_help.py tests/test_install_pi.py tests/test_install_opencode.py`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/install.py src/opendaisugi/cli.py tests/test_install_only.py
git commit -m "install: --only names the components to write, --list shows them, --uninstall --only removes them

A user who wants the warden alone had no flag for it, and uninstall took
everything. Components are the install layers under their user-facing
names; the default is unchanged."
```

### Task 11: Golden cases for `install --only` and `--list` (oracle side)

**Files:**
- Modify: `clients/cli_cases.py` (`layer_cases`: new block `only`)
- Regenerate: `clients/fixtures/cli/`

- [ ] **Step 1: Add the cases**

With `y = ["install", "--yes"]`, `claude`, `codex`, `hermes`, `openclaw`, `every` as the function defines them:

| name | argv | before |
|---|---|---|
| `install list` | `["install", "--list"]` | none |
| `install list ignores runtimes` | `["install", "--list"]` | `every` |
| `only warden claude` | `[*y, "--only", "warden"]` | `claude` |
| `only warden codex` | `[*y, "--only", "warden"]` | `codex` |
| `only skill mcp every harness` | `[*y, "--only", "skill,mcp"]` | `every` |
| `only repeated flags` | `[*y, "--only", "skill", "--only", "mcp"]` | `claude` |
| `only capture hermes` | `[*y, "--only", "capture"]` | `hermes` |
| `only instructions openclaw` | `[*y, "--only", "instructions"]` | `openclaw` |
| `only router claude` | `[*y, "--only", "router"]` | `claude` |
| `only router flag and name` | `[*y, "--only", "router", "--router"]` | `claude` |
| `only router flag without name` | `[*y, "--only", "skill", "--router"]` | `claude` |
| `only enforce without warden` | `[*y, "--only", "skill", "--enforce"]` | `claude` |
| `only enforce with warden and policy` | `[*y, "--only", "warden", "--enforce"]` | `claude` plus the envelope tree the `install gate enforce` case uses |
| `only unknown` | `[*y, "--only", "warden,journal"]` | `claude` |
| `only empty` | `[*y, "--only", ""]` | `claude` |
| `only dry run` | `["install", "--dry-run", "--only", "warden,capture"]` | `every` |
| `only twice` | `[*y, "--only", "warden"]`, `pre=[[*y, "--only", "warden"]]` | `claude` |
| `only then uninstall only` | `["install", "--uninstall", "--only", "capture,instructions"]`, `pre=[y]` | `claude` |
| `uninstall only unknown` | `["install", "--uninstall", "--only", "x"]`, `pre=[y]` | `claude` |
| `uninstall only every harness` | `["install", "--uninstall", "--only", "skill"]`, `pre=[y]` | `every` |
| `only with harness note` | `["install", "--only", "skill", "--harness", "pi", "--dry-run"]` | none |

- [ ] **Step 2: Regenerate and read**

Run: `DAISUGI_CLI_SCRATCH=/mnt/tera/.tt/w3/cli systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/cli_cases.py --only "only"` then `--only "install list"`. Open `only warden claude` and confirm the tree holds `settings.json` with one warden hook and no `.claude.json`. Confirm `only unknown` exits 2 with an empty tree.

- [ ] **Step 3: Commit**

```bash
git add clients/cli_cases.py clients/fixtures/cli
git commit -m "cli cases: install --only and --list, with the symmetric uninstall"
```

### Task 12: The Go port of `--only` and `--list`

**Files:**
- Modify: `clients/go/internal/install/layers.go` (`Opts` gains `Only map[string]bool`; `PlanAllSteps` and `PlanInstall` honour it; `PlanReverseAll(rt, home, gone, only)`), `clients/go/internal/install/components.go` (new: `Components`, `ComponentLayers`, `ListComponents`), `clients/go/internal/cli/installcmd.go` (`--only` with `multiple: true`, `--list`, the refusals, the uninstall summary), `clients/go/internal/cli/helpall_gen.go` (regenerated)
- Test: `clients/go/internal/install/components_test.go`, `clients/go/internal/cli/installcmd_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestComponentLayersUnknownNamesTheList(t *testing.T) {
	_, err := ComponentLayers([]string{"warden,journal"})
	want := "unknown component 'journal'. Components: skill, mcp, capture, instructions, warden, router. Run: daisugi install --list"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
}

func TestListComponentsMatchesOracleText(t *testing.T) {
	got := ListComponents()
	for _, s := range []string{"skill        ", "router       ", "Default: skill, mcp, capture, instructions, warden.", "--harness pi|opencode"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in %q", s, got)
		}
	}
}
```

- [ ] **Step 2: Run** (under the cap, `go test -p 1 ./internal/install/ -run Component`): FAIL, then implement.
- [ ] **Step 3: Regenerate help** `uv run --no-sync python clients/gen_help.py` and build `clients/go/scripts/build.sh` under the cap.
- [ ] **Step 4: Compare** `cli_compare.py --binary clients/go/daisugi --only "only"` then `--only "install list"` then `--only layers` then `--only install`. Expected: 0 disagreements.
- [ ] **Step 5: Commit**

```bash
git add clients/go/internal/install clients/go/internal/cli/installcmd.go clients/go/internal/cli/installcmd_test.go clients/go/internal/cli/helpall_gen.go clients/rust/src/cli/helpall_gen.rs
git commit -m "go: install --only, --list and --uninstall --only"
```

### Task 13: The Rust port of `--only` and `--list`

**Files:**
- Modify: `clients/rust/src/cli/layers.rs` (`Opts { only: Option<BTreeSet<Layer>> }`, the plan and reverse paths), `clients/rust/src/cli/components.rs` (new), `clients/rust/src/cli/installcmd.rs`, `clients/rust/src/cli/mod.rs` (module line)
- Test: `#[cfg(test)]` in `components.rs`, the same two tests as Task 12.

- [ ] Steps as Task 12 with the cargo commands from Task 7 and `cli_compare.py --binary clients/rust/target/release/daisugi`. Commit: `rust: install --only, --list and --uninstall --only`.

---

## Part C: `daisugi recipe`

### Task 14: Recipe files, loader and `recipe list | show` (oracle)

**Files:**
- Create: `src/opendaisugi/recipes/__init__.py` (loader, model, `RECIPE_DIR`), `src/opendaisugi/recipes/warden-only.json`, `router-and-routing-mode.json`, `coppice-claude-warden.json`, `bring-your-own.json`
- Modify: `src/opendaisugi/cli.py` (`recipe_app`: `list`, `show`)
- Create: `tests/test_recipes.py`

**The recipe file (JSON, keys in this order):**

```json
{
  "name": "warden-only",
  "title": "The warden alone, in Claude Code",
  "doc": "docs/recipes/warden-only.md",
  "slots": {"harness": "Claude Code (theirs)", "warden": "daisugi warden (ours)", "router": "none"},
  "params": {"workspace": {"help": "the directory the starter envelope admits", "required": false}},
  "steps": [
    {"kind": "warden-init", "workspace": "{workspace}", "when": "workspace"},
    {"kind": "install", "only": ["warden"], "runtime": ["claude"]},
    {"kind": "note", "text": "For one session instead: claude --settings \"$(daisugi warden settings)\". Add --enforce to deny; it needs an envelope."}
  ]
}
```

Step kinds and their fields:
- `install`: `only` (list of component names, required), `runtime` (list; empty means detect), `enforce` (bool, default false), `report` (string or null), `base_url` (string or null). Runs `install.install(home, runtimes, layers=component_layers(only), enforce, base_url, report)` with `yes=True`.
- `harness`: `name` (`pi` or `opencode`). Runs `install_harness_extension`.
- `config`: `set` (an object of config keys to values; allowed keys are exactly the ones `_plan_router_update` can set: `routing_mode`, `routing_model`, `router_local_model` and the Switchyard tier keys as the post-rename `config.py` names them; the loader rejects any other key). Runs `save_config(load_config(path).model_copy(update=set), path)` after `_plan_router_update`'s checks (call the same validation).
- `warden-init`: `workspace` (a `{param}` or a path). Runs `register_envelope(starter_envelope(Path(workspace).expanduser(), allow_shell_decomposition=<config default>), root=<data>/warden)` under the `default` session, exactly as `warden init --workspace` does. If an envelope is already registered, the step reports `kept` and writes nothing.
- `note`: `text`. Prints; runs nothing.
- `when`: optional; a param name. The step runs only when that param was given.

The four recipes, from the four docs:
- `warden-only`: above.
- `router-and-routing-mode`: `params`: `routing_mode` (default `"rules"`); steps: `install` `only: ["router"]`, `runtime: []`; `config` `{"routing_mode": "{routing_mode}"}`; `note` "Start the router: daisugi router serve. Codex speaks the OpenAI wire on /chat/completions."
- `coppice-claude-warden`: `params`: `workspace` (optional); steps: `warden-init` (when workspace); `install` `only: ["warden"]`, `runtime: ["claude"]`; `note` "Run coppice, or coppice web for the browser and the phone. To enforce: daisugi install --only warden --enforce, after an envelope exists."
- `bring-your-own`: `params`: `efficient_model` (required); steps: `note` "Build Switchyard yourself: cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard --tag v0.3.0 switchyard-server"; `install` `only: ["router"]`, `runtime: []`; `config` `{"routing_mode": "switchyard", "<efficient tier key>": "{efficient_model}"}`; `note` "Hermes needs its OpenAI-compatible base URL set to http://127.0.0.1:8787 by hand; install does not write it. Mem0 joins by its own means."

**Interfaces:**
- `recipes.Recipe` (pydantic): `name`, `title`, `doc`, `slots: dict[str,str]`, `params: dict[str, Param]`, `steps: list[Step]`; `Step` is a discriminated union on `kind`. Unknown kind, unknown `config` key, an `install.only` name not in `COMPONENTS`, a `harness.name` not in `SUPPORTED_HARNESSES`, or a `{param}` that no `params` entry declares: `ValueError` naming the recipe file and the field.
- `recipes.load(name: str) -> Recipe` from the package directory; `recipes.names() -> list[str]` sorted; `recipes.load_all()` validates every shipped file (a test calls it).
- `recipes.render_show(recipe) -> str`: the title, the slot table (`slot: filled by`), the params (`name (required|default X): help`), then the steps numbered, one line each: `1. warden-init  workspace={workspace}  (only with --set workspace=DIR)`, `2. install  --only warden --runtime claude`, `3. note  <text>`.
- CLI: `daisugi recipe list` prints `name` left-justified to 26 then the title, one per line; `--json` a list of `{name, title, doc}`. `daisugi recipe show NAME` prints `render_show`; `--json` the recipe as loaded. Unknown name: stderr `unknown recipe 'x'. Recipes: bring-your-own, coppice-claude-warden, router-and-routing-mode, warden-only` and exit 2.
- The Go and Rust binaries embed the same four JSON files (Task 17, 18); a test in each port asserts byte-identity with the Python files.

- [ ] **Step 1: Write the failing tests** (`tests/test_recipes.py`)

```python
import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.recipes import RECIPE_DIR, load, load_all, names, render_show

runner = CliRunner()


def test_four_recipes_ship_and_validate():
    assert names() == [
        "bring-your-own",
        "coppice-claude-warden",
        "router-and-routing-mode",
        "warden-only",
    ]
    assert len(load_all()) == 4


def test_every_install_step_names_known_components_only():
    from opendaisugi.install import COMPONENTS

    known = {c[0] for c in COMPONENTS}
    for r in load_all():
        for s in r.steps:
            if s.kind == "install":
                assert set(s.only) <= known, (r.name, s.only)


def test_loader_rejects_unknown_kind(tmp_path, monkeypatch):
    bad = tmp_path / "x.json"
    bad.write_text(
        json.dumps(
            {
                "name": "x",
                "title": "t",
                "doc": "d",
                "slots": {},
                "params": {},
                "steps": [{"kind": "shell", "command": "rm -rf /"}],
            }
        )
    )
    monkeypatch.setattr("opendaisugi.recipes.RECIPE_DIR", tmp_path)
    with pytest.raises(ValueError, match="x.json: steps\\[0\\].kind 'shell'"):
        load("x")


def test_loader_rejects_undeclared_param(tmp_path, monkeypatch):
    bad = tmp_path / "x.json"
    bad.write_text(
        json.dumps(
            {
                "name": "x",
                "title": "t",
                "doc": "d",
                "slots": {},
                "params": {},
                "steps": [{"kind": "warden-init", "workspace": "{workspace}"}],
            }
        )
    )
    monkeypatch.setattr("opendaisugi.recipes.RECIPE_DIR", tmp_path)
    with pytest.raises(ValueError, match="param 'workspace' is not declared"):
        load("x")


def test_recipe_list_text_and_json():
    res = runner.invoke(app, ["recipe", "list"])
    assert res.exit_code == 0 and res.output.startswith("bring-your-own")
    js = json.loads(runner.invoke(app, ["recipe", "list", "--json"]).output)
    assert [r["name"] for r in js] == names() and set(js[0]) == {"name", "title", "doc"}


def test_recipe_show_numbers_the_steps():
    res = runner.invoke(app, ["recipe", "show", "warden-only"])
    assert res.exit_code == 0
    assert (
        "1. warden-init" in res.output
        and "2. install  --only warden --runtime claude" in res.output
    )


def test_recipe_show_unknown_exits_2():
    res = runner.invoke(app, ["recipe", "show", "nope"])
    assert res.exit_code == 2 and "unknown recipe 'nope'. Recipes: bring-your-own," in res.output
```

- [ ] **Step 2: Run them to verify they fail** (`ModuleNotFoundError: opendaisugi.recipes`).
- [ ] **Step 3: Implement.** Package data ships with the wheel (the package already ships `skills/` and `harness_pi/`; confirm with `uv build` and `unzip -l dist/*.whl | grep recipes` once).
- [ ] **Step 4: Run** `uv run pytest -q tests/test_recipes.py` and commit:

```bash
git add src/opendaisugi/recipes tests/test_recipes.py src/opendaisugi/cli.py
git commit -m "recipe: the four recipes are data files; recipe list and show read them"
```

### Task 15: `recipe apply` (dry run by default, `--yes` runs, idempotent) and `recipe revert`

**Files:**
- Modify: `src/opendaisugi/recipes/__init__.py` (`plan_apply`, `apply`, `revert`, the receipt)
- Modify: `src/opendaisugi/cli.py` (`recipe apply`, `recipe revert`)
- Test: `tests/test_recipes_apply.py`

**Interfaces:**
- `recipes.resolve_params(recipe, given: dict[str,str]) -> dict[str,str]`: defaults filled; a missing required param raises `ValueError("recipe 'bring-your-own' needs --set efficient_model=VALUE")`; an unknown `--set` key raises `ValueError("recipe 'x' has no param 'y'")`.
- `recipes.plan_apply(recipe, params, *, home: Path) -> list[StepPlan]`: `StepPlan(index, kind, summary, writes: list[Path], state: "run" | "skip" | "kept")`. `install` steps call `install.install(..., dry_run=True)` and list its planned targets; `harness` lists `harness_extension_target`; `config` lists the config path and the keys with `old -> new` (state `kept` when every value is already equal); `warden-init` lists `<data>/warden/envelopes/default.json` (state `kept` when it exists); `note` has no writes; a `when` step whose param is absent has state `skip`.
- `recipes.apply(recipe, params, *, home, out) -> Receipt`: runs each step in order, prints `[n/N] kind  summary` then `  wrote <path>` per path or `  kept`; stops at the first failure and prints `recipe stopped at step n; nothing after it ran. Run: daisugi recipe revert NAME` (exit 1). Writes the receipt after the last step (or after the failing step, with `completed: n-1`).
- The receipt: `<data>/recipes/<name>.json`, keys `recipe`, `applied_at` (ISO UTC, seconds), `params`, `completed` (int), `steps`: a list, per executed step, of `{"index", "kind", "undo"}` where `undo` is: for `install` `{"uninstall_only": [...], "runtime": [...]}`; for `harness` `{"harness": name}`; for `config` `{"restore": {key: old_value_or_null}}`; for `warden-init` `{"remove_envelope": "default"}` only when the step wrote it (a `kept` step has `undo: null`); for `note` `null`.
- `recipes.revert(name, *, home, out) -> int`: reads the receipt, runs the undo of every executed step in reverse order, prints one line per step, deletes the receipt, returns 0. No receipt: stderr `nothing to revert: no receipt for 'x' under <data>/recipes/` exit 1. Config restore writes the old value, or removes the key when the old value was null.
- CLI: `daisugi recipe apply NAME [--set K=V]... [--yes] [--json]`. Without `--yes`: the plan, one line per step (`[n/N] kind  summary` then `  would write <path>` or `  kept` or `  skipped (no --set k)`), then `Dry run. Add --yes to apply.`; exit 0; nothing written anywhere. With `--yes`: apply. `--json` emits the plan (or the receipt) as JSON. `daisugi recipe revert NAME [--yes]`: dry run by default, same shape.
- Idempotent: `apply --yes` twice: the second run prints `kept` for every step and rewrites the receipt with `completed` unchanged; the `install` step is already idempotent; `config` with equal values is `kept`.

- [ ] **Step 1: Write the failing tests** (`tests/test_recipes_apply.py`)

```python
import json
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def snapshot(root: Path) -> dict[str, bytes]:
    return {str(p.relative_to(root)): p.read_bytes() for p in root.rglob("*") if p.is_file()}


def home(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    return tmp_path


def test_apply_without_yes_writes_nothing(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    before = snapshot(h)
    res = runner.invoke(app, ["recipe", "apply", "warden-only", "--set", f"workspace={h}"])
    assert res.exit_code == 0, res.output
    assert "Dry run. Add --yes to apply." in res.output and "would write" in res.output
    assert snapshot(h) == before


def test_apply_yes_runs_steps_and_writes_a_receipt(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    res = runner.invoke(app, ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"])
    assert res.exit_code == 0, res.output
    assert (h / ".opendaisugi" / "warden" / "envelopes" / "default.json").exists()
    assert "warden" in (h / ".claude" / "settings.json").read_text()
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert receipt["recipe"] == "warden-only" and receipt["completed"] == 3
    assert receipt["steps"][0]["undo"] == {"remove_envelope": "default"}
    assert receipt["steps"][1]["undo"] == {"uninstall_only": ["warden"], "runtime": ["claude"]}


def test_apply_twice_keeps_everything(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    argv = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    runner.invoke(app, argv)
    after_one = snapshot(h)
    res = runner.invoke(app, argv)
    assert res.exit_code == 0 and res.output.count("kept") >= 2
    now = snapshot(h)
    now.pop(".opendaisugi/recipes/warden-only.json")
    after_one.pop(".opendaisugi/recipes/warden-only.json")
    assert now == after_one


def test_skipped_when_step_without_its_param(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    res = runner.invoke(app, ["recipe", "apply", "warden-only"])
    assert "skipped (no --set workspace)" in res.output


def test_missing_required_param_refuses_before_any_write(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    before = snapshot(h)
    res = runner.invoke(app, ["recipe", "apply", "bring-your-own", "--yes"])
    assert res.exit_code == 2 and "needs --set efficient_model=VALUE" in res.output
    assert snapshot(h) == before


def test_revert_restores_the_earlier_config_value(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    cfg = h / ".opendaisugi" / "config.yaml"
    cfg.parent.mkdir(parents=True)
    cfg.write_text("routing_mode: knn\n")
    assert (
        runner.invoke(app, ["recipe", "apply", "router-and-routing-mode", "--yes"]).exit_code == 0
    )
    assert "routing_mode: rules" in cfg.read_text()
    res = runner.invoke(app, ["recipe", "revert", "router-and-routing-mode", "--yes"])
    assert res.exit_code == 0, res.output
    assert "routing_mode: knn" in cfg.read_text()
    assert not (h / ".opendaisugi" / "recipes" / "router-and-routing-mode.json").exists()


def test_revert_without_receipt_is_exit_1(tmp_path, monkeypatch):
    home(tmp_path, monkeypatch)
    res = runner.invoke(app, ["recipe", "revert", "warden-only", "--yes"])
    assert res.exit_code == 1 and "nothing to revert" in res.output
```

- [ ] **Step 2: Run them to verify they fail.**
- [ ] **Step 3: Implement.** Every write goes through an existing primitive; `recipes/__init__.py` imports `install`, `config`, `warden` and nothing above them (`tests/test_layer_boundary.py` must stay green).
- [ ] **Step 4: Run** `uv run pytest -q tests/test_recipes.py tests/test_recipes_apply.py tests/test_layer_boundary.py tests/test_install_only.py` and commit:

```bash
git add src/opendaisugi/recipes/__init__.py src/opendaisugi/cli.py tests/test_recipes_apply.py
git commit -m "recipe apply and revert: dry run by default, --yes runs the install primitives, a receipt makes revert exact"
```

### Task 16: Golden cases for `recipe` (oracle side)

**Files:**
- Modify: `clients/cli_cases.py` (new `recipe_cases(add)`)
- Regenerate: `clients/fixtures/cli/`

- [ ] **Step 1: Add the cases**

| name | argv | before / pre |
|---|---|---|
| `recipe list` | `["recipe", "list"]` | |
| `recipe list json` | `["recipe", "list", "--json"]` | |
| `recipe show each` | one case per recipe: `["recipe", "show", NAME]` | |
| `recipe show json` | `["recipe", "show", "warden-only", "--json"]` | |
| `recipe show unknown` | `["recipe", "show", "nope"]` | |
| `recipe apply dry` | `["recipe", "apply", "warden-only", "--set", "workspace={HOME}/w"]` | `claude`, `w` dir |
| `recipe apply dry json` | the same with `--json` | |
| `recipe apply yes` | the same with `--yes` | |
| `recipe apply twice` | the same, `pre=[the same]` | |
| `recipe apply no param skips` | `["recipe", "apply", "warden-only", "--yes"]` | `claude` |
| `recipe apply missing required` | `["recipe", "apply", "bring-your-own", "--yes"]` | `claude` |
| `recipe apply unknown set` | `["recipe", "apply", "warden-only", "--set", "zzz=1"]` | `claude` |
| `recipe apply router` | `["recipe", "apply", "router-and-routing-mode", "--yes"]` | `claude` with `config.yaml` `routing_mode: knn` |
| `recipe revert dry` | `["recipe", "revert", "router-and-routing-mode"]`, `pre=[apply router]` | |
| `recipe revert yes` | `["recipe", "revert", "router-and-routing-mode", "--yes"]`, `pre=[apply router]` | |
| `recipe revert no receipt` | `["recipe", "revert", "warden-only", "--yes"]` | `claude` |
| `recipe apply no runtime` | `["recipe", "apply", "warden-only", "--yes"]` | empty HOME |

`applied_at` is a timestamp: normalize it in `cli_compare.py` the way `--version` is normalized (compare presence and shape `YYYY-MM-DDTHH:MM:SSZ`, not value). Add that one rule.

- [ ] **Step 2: Regenerate** under the cap with `--only recipe`; open `recipe apply yes` and confirm the receipt and the envelope are in the tree.
- [ ] **Step 3: Commit**

```bash
git add clients/cli_cases.py clients/cli_compare.py clients/fixtures/cli
git commit -m "cli cases: recipe list, show, apply and revert"
```

### Task 17: The Go port of `recipe`

**Files:**
- Create: `clients/go/internal/recipes/recipes.go` (`//go:embed assets/*.json`; the four files copied byte for byte from `src/opendaisugi/recipes/`; a test asserts identity against the Python files by relative path, as `clients/go/internal/install/install_test.go` does for the skill), `clients/go/internal/cli/recipecmd.go`
- Modify: `clients/go/internal/cli/cli.go` (dispatch `case "recipe"`), `helpall_gen.go` (regenerated)
- Test: `clients/go/internal/recipes/recipes_test.go` (identity; loader refusals), `clients/go/internal/cli/recipecmd_test.go` (apply dry run writes nothing)

- [ ] **Step 1: Write the failing tests** (identity test and `TestRecipeApplyDryWritesNothing` over a temp HOME).
- [ ] **Step 2: Implement.** The primitives exist in Go: `PlanInstall`/`ApplyChange` with `Opts.Only`, `InstallHarness`, `saveInstallConfig`, the warden `init` path in `wardencmds.go`. The receipt is written with `pyjson.DumpsIndent(v, 2, true)` so bytes match the oracle's `json.dumps(indent=2)`.
- [ ] **Step 3: Build under the cap; compare** `cli_compare.py --binary clients/go/daisugi --only recipe`. Expected: 0 disagreements.
- [ ] **Step 4: Commit** `go: recipe list, show, apply and revert from the embedded recipe files`.

### Task 18: The Rust port of `recipe`

**Files:**
- Create: `clients/rust/src/cli/recipes.rs` (`include_str!` of the four files from `src/opendaisugi/recipes/`, the same way `layer_assets.rs` includes the skill), `clients/rust/src/cli/recipecmd.rs`
- Modify: `clients/rust/src/cli/mod.rs` (dispatch), `helpall_gen.rs` (regenerated)

- [ ] Steps as Task 17 with the cargo commands from Task 7. Commit: `rust: recipe list, show, apply and revert`.

### Task 19: Docs state the current state once; whole-suite green; the ledger

**Files:**
- Modify: `docs/recipes/README.md` (the table gains a `Command` column: `daisugi recipe apply warden-only --set workspace=~/Work`, and so on; the "is planned" paragraph is replaced by: `Each recipe is a data file inside the binary. daisugi recipe list names them; daisugi recipe show NAME prints the steps; daisugi recipe apply NAME shows what it would write and writes it with --yes; daisugi recipe revert NAME undoes it from the receipt.`), the four recipe pages (each gains, under its slot table, the one `recipe apply` line and keeps its hand block below it under `By hand:`), `docs/feature-status.md` (rows: recording per harness; `install --only`; `recipe`), `AGENTS.md` (the Install block: add `daisugi install --list`, `daisugi install --only warden,capture`, `daisugi recipe apply warden-only`; the binaries table: Go and Rust carry `recipe`; the parts table: `recipes/` under Surfaces), `README.md` (one sentence where install is introduced: `daisugi install --only warden` for the warden alone), `docs/how-to/warden.md` (the permanent-form sentence names `--only warden`)
- Append: `.superpowers/sdd/backlog/progress.md`

- [ ] **Step 1: Write the docs.** Run `grep -nE " — |seam|the layer|now |no longer|used to|planned" docs/recipes/*.md AGENTS.md README.md docs/feature-status.md` and fix every hit in new text.
- [ ] **Step 2: Whole suite, one at a time under the cap:** `uv run pytest -q`; `uv run ruff check . && uv run ruff format --check .`; `clients/go` `go test -p 1 ./...`; the Rust `cargo test --release` with `CARGO_BUILD_JOBS=2`; then `cli_compare.py`, `k4_compare.py`, `warden_server_compare.py`, `f_compare.py` in full for each binary. Record the counts.
- [ ] **Step 3: Ledger.** Append: the commit range; compare counts per suite and binary; the rulings R-W3-1 to R-W3-8; the four box checks from Task 9 as open; that Tasks 8a and 8b were built (they are required, R-W3-3); and the line `W3 done; the owner re-runs scripts/install.sh --replace-daisugi, then daisugi install once (the hook commands did not change in this wave; the feed and the plugin did).`
- [ ] **Step 4: Commit**

```bash
git add docs/recipes AGENTS.md README.md docs/feature-status.md docs/how-to/warden.md
git commit -m "docs: recipes run with daisugi recipe apply; install --only and --list; how each harness is recorded"
```

---

## Task ranking

1. Task 1 (format through the mirror): the one-line bug that drops every pi call. Highest value per line.
2. Task 3 (the feed reads the warden root; the trigger): without it Task 1's records never reach the journal.
3. Task 2 (Hermes and OpenClaw names), Task 4 (OpenClaw session id).
4. Task 8a, then 8b (history import, in the oracle, Go and Rust; owner ruling B10, R-W3-3), then Task 5, 6, 7 (cases and ports for Part A).
5. Task 10, 11, 12, 13 (`--only`, `--list`; Part B). Part C depends on `component_layers` from Task 10.
6. Task 14, 15, 16, 17, 18 (recipes; Part C).
7. Task 9 and 19 (docs, box checks, ledger). Task 9 can run any time after Task 4.

Each task is independently testable: its tests run on its own files, and its golden cases are regenerated with `--only`.

## Self-review notes

- Spec coverage. Item 1: every harness records through one chosen source (R-W3-1; Tasks 1 to 4), the records reach the journal (Task 3), fixtures are synthetic (every test writes its own), Hermes and OpenClaw real-host proof is a named box check (Task 9), formats cited with URLs (Task 2, 8a, 8b), ports and cases (Tasks 5 to 7). Item 2: list, show, apply (dry by default, `--yes`), idempotent, data files in the binaries, uninstall and revert symmetry (Tasks 14 to 18), primitives not shell (R-W3-5). Item 3: `--only`, `--list`, default unchanged, uninstall symmetric (Tasks 10 to 13).
- Parity: Go and Rust tasks follow each oracle task; compare commands named per task; refusals go to `clients/ADJUDICATIONS.md`.
- Nothing here changes a verdict, a hook command string, or the W2 one-time move.
- Open after this wave: the OpenClaw enable key (R-W3-7) after the box check; Codex captures under the resident mirror are classified under the Codex payload's own names, which this plan did not inspect (the `server mirror codex` case pins the Claude-shaped `Bash` payload only).
