# Commonplace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give daisugi a semantic memory, Commonplace: a read-only view over directories of the owner's Markdown notes, an optional SQLite index built with the garden's matcher, a `daisugi notes` CLI, and a read-only MCP server a model can mount.

**Architecture:** A store is one directory of Markdown named in `[commonplace]` of daisugi's config. A reader lists, chunks and reads files and never leaves the root. An index (`<data home>/commonplace/<store>/index.db`) caches chunk vectors from the lexical matcher (or potion, int8) and is rebuilt from the files at will. Search is a brute-force cosine scan; with no index it is a substring scan that still answers. Python is the oracle; Go and Rust port it; golden cases compare them.

**Tech Stack:** Python 3.12, `sqlite3` and `numpy` (both already hard dependencies), FastMCP (the `[mcp]` extra, as `mcp_server.py`), Go (`clients/go`), Rust (`clients/rust`). No vector database. No new third-party package.

**Spec:** `docs/adr/0024-commonplace.md`. Related: ADR-0018, ADR-0019, ADR-0021 (the matchers), ADR-0023 (the two-store rule).

## Global Constraints

- Prose, comments and on-screen text in STE: short sentences, plain words, no em dashes, never "seam" or "the layer".
- Store names match `[a-z][a-z0-9-]{0,31}` (PD-R-1). `--store all` is refused with `one store at a time: pando stores are separate on purpose.` (CM-R-1, the PD-R-2 text). No verb reads two stores.
- Files: `.md`, `.markdown`, `.txt`; over 1 MiB skipped with `skipped: over 1 MiB` (CM-R-8); hidden dirs skipped; symlinks that resolve outside the root not followed (CM-R-3, message `outside the store`).
- A chunk is an ATX heading section; a section over 2,000 bytes is cut at paragraph breaks into pieces of at most 2,000 bytes; a file with no heading is one chunk named by its file name.
- The index records the matcher name and dimension; a mismatch rebuilds and says so on stderr (CM-R-4). Vectors are packed little-endian float32 blobs.
- Search is deterministic: ties by path, then chunk order (CM-R-5). No index: substring scan plus the line `no index yet; ranking is off. Run: daisugi notes index --store S`, exit 0 (CM-R-6).
- The MCP server serves one store named at start and has no write tool (CM-R-7).
- Tests: no network, no real model, no reads outside `tmp_path`; never `~/.opendaisugi`. Scratch under `~/opendaisugi-scratch/`, never `/tmp`.
- One heavy job at a time under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G`, `go test -p 1`, `CARGO_BUILD_JOBS=2`. Never `pkill -f`.
- Commits small, by explicit path, no attribution lines, no push. Never commit `CLAUDE.md`, `docs/superpowers/`, `uv.lock`.

## Review Focus

1. A note that is edited while `notes index` runs: the row keeps the sha of the bytes it read, and the next `index` sees the new mtime and rewrites it. Test in Task 3.
2. A store root given as a symlink to a real dir: allowed; files inside that resolve under the real dir are inside. Test in Task 2.
3. A query of only stopwords (`the of and`): the lexical vector is zero; search must answer the substring scan, not divide by zero. Test in Task 4.
4. Two stores whose roots are the same path under two names: refused at config load with `stores work and home share a root`. Test in Task 1.
5. A `.md` file with a UTF-8 BOM or CRLF endings: headings still split; the BOM is not part of the first heading. Test in Task 2.

---

### Task 1: `[commonplace]` stores in config

**Files:**
- Modify: `src/opendaisugi/config.py` (a `CommonplaceConfig(BaseModel)` with `stores: dict[str, str] = {}`; `Config.commonplace: CommonplaceConfig`)
- Create: `src/opendaisugi/commonplace/__init__.py`, `src/opendaisugi/commonplace/stores.py`
- Test: `tests/test_commonplace_stores.py`

**Interfaces:**
- Produces: `valid_store(name: str) -> bool`; `store_root(config: Config, name: str) -> Path` (raises `ValueError` with the CM-R-1 text for `all`, `bad store name ...` for a bad name, `no commonplace store <name>; add it under [commonplace] in <config path>` when unset; expands `~`; the path must be absolute after expansion); `check_stores(stores: dict[str, str]) -> None` (raises `ValueError`: `stores <a> and <b> share a root` for equal resolved roots, `store <a> is inside store <b>` for nesting); `index_dir(data_dir: Path, name: str) -> Path` = `<data_dir>/commonplace/<name>`.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_commonplace_stores.py
import pytest

from opendaisugi.commonplace.stores import check_stores, index_dir, store_root, valid_store
from opendaisugi.config import Config


def test_store_root_rules(tmp_path):
    cfg = Config(commonplace={"stores": {"work": str(tmp_path / "w"), "home": "~/notes"}})
    assert store_root(cfg, "work") == tmp_path / "w"
    assert store_root(cfg, "home").is_absolute()
    with pytest.raises(ValueError, match="one store at a time"):
        store_root(cfg, "all")
    with pytest.raises(ValueError, match="no commonplace store club"):
        store_root(cfg, "club")
    assert valid_store("work") and not valid_store("Work") and not valid_store("1x")


def test_shared_and_nested_roots_refused(tmp_path):
    (tmp_path / "a" / "b").mkdir(parents=True)
    with pytest.raises(ValueError, match="share a root"):
        check_stores({"work": str(tmp_path / "a"), "home": str(tmp_path / "a")})
    with pytest.raises(ValueError, match="is inside store"):
        check_stores({"work": str(tmp_path / "a"), "home": str(tmp_path / "a" / "b")})
    check_stores({"work": str(tmp_path / "a"), "home": str(tmp_path / "c")})


def test_index_dir(tmp_path):
    assert index_dir(tmp_path, "work") == tmp_path / "commonplace" / "work"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_commonplace_stores.py -v`
Expected: FAIL, `ModuleNotFoundError: opendaisugi.commonplace`.

- [ ] **Step 3: Implement**

`CommonplaceConfig` with a `model_validator` that calls `check_stores`. `store_root` resolves `~` with `Path.expanduser()` and refuses a relative result. Nothing here touches the disk beyond `Path.resolve()`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_commonplace_stores.py tests/test_config*.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/config.py src/opendaisugi/commonplace tests/test_commonplace_stores.py
git commit -m "commonplace: [commonplace] stores in config, one directory per store, nesting and sharing refused"
```

---

### Task 2: The reader: list, chunk, read, never leave the root

**Files:**
- Create: `src/opendaisugi/commonplace/reader.py`
- Test: `tests/test_commonplace_reader.py`

**Interfaces:**
- Consumes: Task 1 (`store_root`).
- Produces:
  - `@dataclass(frozen=True) class NoteFile: rel: str; size: int; mtime_ns: int; skipped: str | None`
  - `@dataclass(frozen=True) class Chunk: rel: str; ordinal: int; heading: str; text: str` (`heading` is the heading path joined by ` > `; for no heading, the file name)
  - `list_notes(root: Path, prefix: str = "") -> list[NoteFile]` sorted by `rel`; hidden dirs skipped; symlinks out of root skipped with `skipped="outside the store"`; files over `MAX_NOTE_BYTES = 1 << 20` listed with `skipped="over 1 MiB"`
  - `read_note(root: Path, rel: str, offset: int = 0, size: int = 0) -> str` (raises `ValueError("outside the store")` for `..`, absolute paths, or a symlink target outside; `size` 0 means all; `MAX_READ_BYTES = 64 * 1024` caps one read)
  - `chunk_markdown(rel: str, text: str) -> list[Chunk]` with `CHUNK_BYTES = 2000`
  - `sha256_of(data: bytes) -> str`

- [ ] **Step 1: Write the failing test**

```python
# tests/test_commonplace_reader.py
import os

import pytest

from opendaisugi.commonplace.reader import chunk_markdown, list_notes, read_note


def test_list_skips_hidden_big_and_outside(tmp_path):
    root = tmp_path / "notes"
    (root / ".obsidian").mkdir(parents=True)
    (root / ".obsidian" / "x.md").write_text("# hidden")
    (root / "a.md").write_text("# A\nbody")
    (root / "big.md").write_bytes(b"x" * ((1 << 20) + 1))
    (tmp_path / "secret.md").write_text("# out")
    os.symlink(tmp_path / "secret.md", root / "link.md")
    got = {n.rel: n.skipped for n in list_notes(root)}
    assert got == {"a.md": None, "big.md": "over 1 MiB", "link.md": "outside the store"}


def test_root_may_be_a_symlink_to_a_real_dir(tmp_path):
    real = tmp_path / "real"
    real.mkdir()
    (real / "n.md").write_text("# N")
    link = tmp_path / "link"
    os.symlink(real, link)
    assert [n.rel for n in list_notes(link)] == ["n.md"]
    assert read_note(link, "n.md") == "# N"


def test_read_refuses_escape(tmp_path):
    root = tmp_path / "notes"
    root.mkdir()
    (tmp_path / "pw.txt").write_text("no")
    for rel in ["../pw.txt", "/etc/hostname", "sub/../../pw.txt"]:
        with pytest.raises(ValueError, match="outside the store"):
            read_note(root, rel)


def test_chunks_by_heading_and_size_and_bom():
    text = "﻿# Top\r\nintro\r\n## Sub\r\n" + ("para " * 300 + "\r\n\r\n") * 3
    chunks = chunk_markdown("t.md", text)
    assert chunks[0].heading == "Top" and chunks[0].text.strip() == "intro"
    assert chunks[1].heading == "Top > Sub"
    assert all(len(c.text.encode()) <= 2000 for c in chunks)
    assert len(chunks) >= 3
    assert [c.ordinal for c in chunks] == list(range(len(chunks)))


def test_no_heading_is_one_chunk_named_by_file():
    chunks = chunk_markdown("plain.txt", "just words\n")
    assert len(chunks) == 1 and chunks[0].heading == "plain.txt"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_commonplace_reader.py -v`
Expected: FAIL, `cannot import name 'chunk_markdown'`.

- [ ] **Step 3: Implement `reader.py`**

`list_notes` walks with `os.walk(followlinks=False)`, prunes names that start with `.`, and for each candidate resolves `root.resolve()` and `path.resolve()`; a file whose resolved path is not under the resolved root is `skipped="outside the store"`. `read_note` does the same resolve check before opening and reads `offset` and `size` bytes, decoded `utf-8` with `errors="replace"`. `chunk_markdown` strips a leading BOM, normalizes `\r\n` to `\n`, splits on lines that match `^#{1,6} `, keeps a heading stack by level, and cuts long sections on `\n\n`; a paragraph that alone exceeds 2,000 bytes is cut at the last space before the limit.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_commonplace_reader.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/commonplace/reader.py tests/test_commonplace_reader.py
git commit -m "commonplace reader: list, read and chunk Markdown under one root, never outside it"
```

---

### Task 3: The index

**Files:**
- Create: `src/opendaisugi/commonplace/index.py`
- Test: `tests/test_commonplace_index.py`

**Interfaces:**
- Consumes: Task 2 (`list_notes`, `read_note`, `chunk_markdown`, `sha256_of`); `opendaisugi._search.active_model_name(config)` and `_get_model(config).encode(texts)` for vectors.
- Produces:
  - `class Index` with `open(data_dir: Path, store: str) -> Index` (creates `index_dir/index.db`, schema: `meta(key TEXT PRIMARY KEY, value TEXT)` holding `matcher`, `dim`, `schema=1`; `files(rel TEXT PRIMARY KEY, mtime_ns INTEGER, sha TEXT, size INTEGER)`; `chunks(rel TEXT, ordinal INTEGER, heading TEXT, text TEXT, vec BLOB, PRIMARY KEY(rel, ordinal))`)
  - `rebuild(self, root: Path, embed: Callable[[list[str]], np.ndarray], matcher: str, *, log: Callable[[str], None] = ...) -> IndexReport` with `@dataclass class IndexReport: added: int; updated: int; removed: int; unchanged: int; skipped: list[str]; rebuilt_for_matcher: bool`. A file is unchanged when `mtime_ns` and `size` match the row; otherwise its bytes are read, its sha compared, and only a changed sha re-chunks and re-embeds. A row whose file is gone is removed. When `meta.matcher` or `dim` differs from the given, every row is dropped first and `log(f"index was built with {old}; rebuilding with {matcher}")` is called (CM-R-4).
  - `search(self, qvec: np.ndarray, k: int) -> list[Hit]` with `@dataclass(frozen=True) class Hit: rel: str; ordinal: int; heading: str; text: str; score: float`; brute-force cosine; ties by `rel`, then `ordinal` (CM-R-5); a zero `qvec` returns `[]`.
  - `count(self) -> int`, `matcher(self) -> str | None`.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_commonplace_index.py
import os
import time

import numpy as np

from opendaisugi._search import _get_model
from opendaisugi.commonplace.index import Index


def lexical(texts):
    return _get_model(None).encode(texts)  # the default config is lexical


def make_root(tmp_path):
    root = tmp_path / "notes"
    root.mkdir()
    (root / "fdroid.md").write_text(
        "# F-Droid\n\n## Signing\nreproducible builds need a fork of the install base\n"
    )
    (root / "lunch.md").write_text("# Lunch\nsoup on tuesday\n")
    return root


def test_rebuild_is_incremental_and_removes_gone(tmp_path):
    root = make_root(tmp_path)
    ix = Index.open(tmp_path / "data", "work")
    r1 = ix.rebuild(root, lexical, "lexical")
    assert (r1.added, r1.updated, r1.removed) == (2, 0, 0) and ix.count() >= 3
    r2 = ix.rebuild(root, lexical, "lexical")
    assert (r2.added, r2.updated, r2.removed, r2.unchanged) == (0, 0, 0, 2)
    (root / "lunch.md").write_text("# Lunch\nsalad on tuesday\n")
    os.utime(root / "lunch.md", ns=(time.time_ns(), time.time_ns() + 10))
    r3 = ix.rebuild(root, lexical, "lexical")
    assert r3.updated == 1
    (root / "lunch.md").unlink()
    assert ix.rebuild(root, lexical, "lexical").removed == 1


def test_search_ranks_and_breaks_ties_by_path(tmp_path):
    root = make_root(tmp_path)
    ix = Index.open(tmp_path / "data", "work")
    ix.rebuild(root, lexical, "lexical")
    qvec = lexical(["f-droid signing reproducible"])[0]
    hits = ix.search(qvec, 2)
    assert hits[0].rel == "fdroid.md" and hits[0].heading == "F-Droid > Signing"
    assert ix.search(np.zeros_like(qvec), 2) == []


def test_matcher_change_rebuilds_and_logs(tmp_path):
    root = make_root(tmp_path)
    ix = Index.open(tmp_path / "data", "work")
    ix.rebuild(root, lexical, "lexical")
    lines = []
    r = ix.rebuild(root, lexical, "potion-test", log=lines.append)
    assert r.rebuilt_for_matcher and ix.matcher() == "potion-test"
    assert lines == ["index was built with lexical; rebuilding with potion-test"]
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_commonplace_index.py -v`
Expected: FAIL, `cannot import name 'Index'`.

- [ ] **Step 3: Implement `index.py`**

SQLite with `isolation_level=None`, one connection per `Index`, `PRAGMA user_version = 1`. Vectors stored as `np.asarray(v, dtype="<f4").tobytes()`; `search` loads all blobs into one `(n, dim)` array, normalizes rows once, dots with the normalized query, and sorts by `(-score, rel, ordinal)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_commonplace_index.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/commonplace/index.py tests/test_commonplace_index.py
git commit -m "commonplace index: sqlite chunk cache with matcher identity, incremental by mtime and sha, brute-force cosine"
```

---

### Task 4: Search with the no-index fallback, and the `daisugi notes` CLI

**Files:**
- Create: `src/opendaisugi/commonplace/search.py`
- Modify: `src/opendaisugi/cli.py` (a `notes_app = typer.Typer(...)` beside `journal_app` at line 178, `app.add_typer(notes_app, name="notes", rich_help_panel="Garden")`; verbs `stores`, `list`, `show`, `search`, `index`, `serve` (Task 5))
- Test: `tests/test_commonplace_search.py`, `tests/test_cli_notes.py`

**Interfaces:**
- Consumes: Tasks 1 to 3.
- Produces:
  - `search_store(config: Config, data_dir: Path, store: str, query: str, k: int = 5, *, warn: Callable[[str], None]) -> list[Hit]`: if the index exists and its count is above 0, embed the query with the effective matcher and call `Index.search`; a zero query vector, or no index, falls back to `scan_store(root, query, k)` and (for no index only) calls `warn("no index yet; ranking is off. Run: daisugi notes index --store " + store)` (CM-R-6).
  - `scan_store(root: Path, query: str, k: int) -> list[Hit]`: case-folded substring over each chunk's text and heading, score 1.0, order by `rel`, `ordinal`.
  - CLI: `daisugi notes stores` (names and roots); `notes list [--store S] [--prefix P]` (`rel<TAB>size<TAB>skipped-or-blank`); `notes show PATH [--offset B --size B]`; `notes search QUERY [-k N]` (`score<TAB>rel<TAB>heading<TAB>first 120 bytes`); `notes index` (prints the `IndexReport` in one line); every verb takes `--store` (default `work`), `--data-dir`, `--json`. Exit 0 ok; 1 store missing on disk (CM-R-2 text) or path refused; 2 usage or `all`.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_commonplace_search.py
from opendaisugi.commonplace.index import Index
from opendaisugi.commonplace.search import scan_store, search_store
from opendaisugi.config import Config


def setup(tmp_path):
    root = tmp_path / "notes"
    root.mkdir()
    (root / "a.md").write_text("# Rule\nthe warden denies by default\n")
    (root / "b.md").write_text("# Food\nsoup\n")
    cfg = Config(commonplace={"stores": {"work": str(root)}})
    return root, cfg


def test_no_index_scans_and_warns(tmp_path):
    root, cfg = setup(tmp_path)
    warned = []
    hits = search_store(cfg, tmp_path / "data", "work", "warden", warn=warned.append)
    assert [h.rel for h in hits] == ["a.md"]
    assert warned == ["no index yet; ranking is off. Run: daisugi notes index --store work"]


def test_stopword_query_falls_back_without_error(tmp_path):
    root, cfg = setup(tmp_path)
    from opendaisugi._search import _get_model

    Index.open(tmp_path / "data", "work").rebuild(
        root, lambda t: _get_model(None).encode(t), "lexical"
    )
    warned = []
    hits = search_store(cfg, tmp_path / "data", "work", "the of and", warn=warned.append)
    assert hits == [] and warned == []


def test_scan_is_ordered_by_path():
    import pathlib

    assert scan_store(pathlib.Path("/nonexistent"), "x", 5) == []
```

```python
# tests/test_cli_notes.py
from typer.testing import CliRunner

from opendaisugi.cli import app


def test_notes_all_refused_and_missing_root(tmp_path, monkeypatch):
    cfg = tmp_path / "config.toml"
    cfg.write_text(f'[commonplace.stores]\nwork = "{tmp_path / "missing"}"\n')
    monkeypatch.setenv("OPENDAISUGI_CONFIG", str(cfg))
    r = CliRunner(mix_stderr=False).invoke(
        app, ["notes", "list", "--store", "all", "--data-dir", str(tmp_path)]
    )
    assert r.exit_code == 2 and "one store at a time" in r.stderr
    r = CliRunner(mix_stderr=False).invoke(app, ["notes", "list", "--data-dir", str(tmp_path)])
    assert (
        r.exit_code == 1 and "commonplace store work:" in r.stderr and "does not exist" in r.stderr
    )
```

(If `OPENDAISUGI_CONFIG` is not how `load_config` finds its file, use the mechanism `tests/test_cli_config.py` uses and keep the assertions.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_commonplace_search.py tests/test_cli_notes.py -v`
Expected: FAIL, import errors and `No such command 'notes'`.

- [ ] **Step 3: Implement**

`search.py` as the interface says. CLI verbs are thin: resolve the store, call the module, print. `--json` prints a list of dicts with the dataclass fields.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_commonplace_search.py tests/test_cli_notes.py tests/test_cli_entry.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/commonplace/search.py src/opendaisugi/cli.py tests/test_commonplace_search.py tests/test_cli_notes.py
git commit -m "daisugi notes: stores, list, show, search, index; search answers with a scan when there is no index"
```

---

### Task 5: The MCP server, `daisugi notes serve`

**Files:**
- Create: `src/opendaisugi/commonplace/mcp.py`
- Modify: `src/opendaisugi/cli.py` (`notes serve --store S [--data-dir]`, stdio, like `mcp serve` at line 1487)
- Test: `tests/test_commonplace_mcp.py`

**Interfaces:**
- Consumes: Tasks 2 to 4.
- Produces: `build_notes_server(config: Config, data_dir: Path, store: str) -> FastMCP` named `commonplace`, tools:
  - `notes_search(query: str, k: int = 5) -> list[dict]` (fields of `Hit`; `k` clamped to 1 to 20)
  - `notes_read(path: str, offset: int = 0, size: int = 0) -> str` (CM-R-3 refusal becomes a tool error with the text `outside the store`)
  - `notes_list(prefix: str = "") -> list[dict]` (fields of `NoteFile`)
  - No other tool. No `store` argument on any tool (CM-R-7).

- [ ] **Step 1: Write the failing test**

```python
# tests/test_commonplace_mcp.py
import asyncio
import json

import pytest

from opendaisugi.commonplace.mcp import build_notes_server
from opendaisugi.config import Config


def test_three_read_tools_one_store(tmp_path):
    root = tmp_path / "notes"
    root.mkdir()
    (root / "r.md").write_text("# Rule\nfetch before push\n")
    (tmp_path / "pw.txt").write_text("x")
    cfg = Config(commonplace={"stores": {"work": str(root), "home": str(tmp_path / "h")}})
    mcp = build_notes_server(cfg, tmp_path / "data", "work")
    assert mcp.name == "commonplace"
    tools = asyncio.run(mcp.list_tools())
    assert {t.name for t in tools} == {"notes_search", "notes_read", "notes_list"}
    assert all("store" not in t.inputSchema.get("properties", {}) for t in tools)
    _, hits = asyncio.run(mcp.call_tool("notes_search", {"query": "push"}))
    assert "r.md" in json.dumps(hits)
    text = asyncio.run(mcp.call_tool("notes_read", {"path": "r.md"}))
    assert "fetch before push" in json.dumps(text, default=str)
    with pytest.raises(Exception, match="outside the store"):
        asyncio.run(mcp.call_tool("notes_read", {"path": "../pw.txt"}))
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_commonplace_mcp.py -v`
Expected: FAIL, `ModuleNotFoundError`.

- [ ] **Step 3: Implement**

Follow `mcp_server.build_server` (`FastMCP(name=...)`, `@mcp.tool()`), with the store resolved once at build time and closed over by the tools.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_commonplace_mcp.py tests/test_layer_boundary.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/commonplace/mcp.py src/opendaisugi/cli.py tests/test_commonplace_mcp.py
git commit -m "daisugi notes serve: a read-only MCP server named commonplace over one store"
```

---

### Task 6: Golden cases from the oracle

**Files:**
- Create: `clients/notes_cases.py` (lays out invented note trees under a scratch HOME, runs `daisugi notes` verbs through `PY_CLI` as `garden_cases.py` does, dumps `index.db` whole; `--out clients/fixtures/notes`)
- Create: `clients/notes_compare.py` (`--binary PATH [--oracle] [--only NAME]`; compares exit code, stdout, stderr, the tree after; a verb not in the binary must exit 2 with one line and leave the tree)
- Create: `clients/fixtures/notes/` (the cases and manifest)

**Interfaces:**
- Consumes: Tasks 1 to 5.
- Produces: cases `stores`, `list` (hidden, big, outside), `show` (ok, offset and size, escape refused), `search` (no index: scan plus warning; indexed; stopwords; ties), `index` (fresh, unchanged, one edited, one removed, matcher change with a seeded `meta` row), `--store all`, missing root, `--json` for each.

- [ ] **Step 1: Write the cases**

Run: `uv run --no-sync python clients/notes_cases.py --out clients/fixtures/notes`
Expected: the fixture dir with a manifest; no real path in any file (`clients/fixture_paths.py leaks` reports none).

- [ ] **Step 2: Check the oracle agrees with itself**

Run: `uv run --no-sync python clients/notes_compare.py --oracle`
Expected: 0 disagreements.

- [ ] **Step 3: Commit**

```bash
git add clients/notes_cases.py clients/notes_compare.py clients/fixtures/notes
git commit -m "notes cases: golden fixtures for daisugi notes from the oracle, and the compare script"
```

---

### Task 7: The Go port

**Files:**
- Create: `clients/go/internal/notes/{stores,reader,index,search,mcp}.go` and tests
- Create: `clients/go/internal/cli/notescmd.go`
- Modify: `clients/go/internal/cli/cli.go` (add `notes`), `clients/go/internal/config/config.go:56` (`[commonplace] stores` table), `clients/go/internal/cli/helpall_gen.go` through `clients/gen_help.py`
- Test: `clients/go/internal/notes/*_test.go`, the compare run

**Interfaces:**
- Consumes: `clients/go/internal/embed/lexical.Vector(text) []float64` for vectors; `clients/go/internal/sqlpath` and the SQLite driver the garden uses; `config.MatcherModel`.
- Produces: the same verbs, flags, messages and exit codes as the Python CLI; `index.db` byte-compatible in schema (same tables, same `meta` keys, float32 blobs) so Python can read what Go wrote (the compare script's guard).

- [ ] **Step 1: Run the compare to see it fail**

Run: `uv run --no-sync python clients/notes_compare.py --binary clients/go/daisugi`
Expected: every case disagrees with `notes is not in this binary yet.`

- [ ] **Step 2: Write the package tests first**

```go
// clients/go/internal/notes/reader_test.go
package notes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChunkHeadingPathAndCap(t *testing.T) {
	text := "﻿# Top\r\nintro\r\n## Sub\r\n" + repeat("para ", 300) + "\r\n\r\n" + repeat("para ", 300)
	cs := ChunkMarkdown("t.md", text)
	if cs[0].Heading != "Top" || cs[1].Heading != "Top > Sub" {
		t.Fatalf("%+v", cs[:2])
	}
	for _, c := range cs {
		if len(c.Text) > 2000 {
			t.Fatal("chunk over 2000 bytes")
		}
	}
}

func TestReadRefusesEscape(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(filepath.Dir(root), "pw.txt"), []byte("x"), 0o600)
	if _, err := ReadNote(root, "../pw.txt", 0, 0); err == nil || err.Error() != "outside the store" {
		t.Fatalf("got %v", err)
	}
}
```

(`repeat` is `strings.Repeat`.)

- [ ] **Step 3: Implement to the Python oracle**

Port module by module, keeping messages byte for byte. `Vector` gives float64; store as float32 little-endian like Python.

- [ ] **Step 4: Compare to zero**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./internal/notes/ ./internal/cli/ -run 'TestChunk|TestRead|TestNotes'` then `uv run --no-sync python clients/notes_compare.py --binary clients/go/daisugi`
Expected: PASS; 0 disagreements. Record any ruling as CM-R-n in `clients/ADJUDICATIONS.md`.

- [ ] **Step 5: Commit**

```bash
git add clients/go/internal/notes clients/go/internal/cli/notescmd.go clients/go/internal/cli/cli.go clients/go/internal/cli/helpall_gen.go clients/go/internal/config/config.go clients/ADJUDICATIONS.md
git commit -m "daisugi (Go) notes: the commonplace port, agreeing with the oracle on every case"
```

---

### Task 8: The Rust port

**Files:**
- Create: `clients/rust/src/notes/{mod,stores,reader,index,search,mcp}.rs` and tests
- Create: `clients/rust/src/cli/notescmd.rs`
- Modify: `clients/rust/src/cli/mod.rs:693` (add `"notes"`), `clients/rust/src/cli/config.rs:72` (`[commonplace] stores`), `clients/rust/src/cli/helpall_gen.rs` through `clients/gen_help.py`, `clients/rust/src/lib.rs`
- Test: the module tests and the compare run

**Interfaces:**
- Consumes: `crate::pathways::lexical::vector(text) -> Vec<f64>`; the `rusqlite` the garden uses (pinned 0.38 on this box, see the Rust toolchain note).
- Produces: the same CLI as Go and Python.

- [ ] **Step 1: Run the compare to see it fail**

Run: `uv run --no-sync python clients/notes_compare.py --binary clients/rust/target/release/daisugi`
Expected: every case disagrees.

- [ ] **Step 2: Write the module tests first**

```rust
// clients/rust/src/notes/reader_tests.rs
use super::reader::{chunk_markdown, read_note};

#[test]
fn chunk_heading_path_and_cap() {
    let text = format!("\u{feff}# Top\r\nintro\r\n## Sub\r\n{}\r\n\r\n{}", "para ".repeat(300), "para ".repeat(300));
    let cs = chunk_markdown("t.md", &text);
    assert_eq!(cs[0].heading, "Top");
    assert_eq!(cs[1].heading, "Top > Sub");
    assert!(cs.iter().all(|c| c.text.len() <= 2000));
}

#[test]
fn read_refuses_escape() {
    let root = crate::testtmp::dir();
    let err = read_note(&root, "../pw.txt", 0, 0).unwrap_err();
    assert_eq!(err.to_string(), "outside the store");
}
```

- [ ] **Step 3: Implement to the oracle**

- [ ] **Step 4: Compare to zero**

Run: `cd clients/rust && systemd-run --user --scope -q -p MemoryMax=5G env CARGO_BUILD_JOBS=2 cargo test notes` then `uv run --no-sync python clients/notes_compare.py --binary clients/rust/target/release/daisugi`
Expected: PASS; 0 disagreements.

- [ ] **Step 5: Commit**

```bash
git add clients/rust/src/notes clients/rust/src/cli/notescmd.rs clients/rust/src/cli/mod.rs clients/rust/src/cli/config.rs clients/rust/src/cli/helpall_gen.rs clients/rust/src/lib.rs clients/ADJUDICATIONS.md
git commit -m "daisugi (Rust) notes: the commonplace port, agreeing with the oracle on every case"
```

---

### Task 9: Docs

**Files:**
- Create: `docs/how-to/commonplace.md` (name a store in `config.toml`, `notes index`, `notes search`, mount `daisugi notes serve --store work` in `~/.claude.json` beside `opendaisugi`, the two-store rule, what it is not)
- Modify: `AGENTS.md` (the concern table: "Memory and reuse" gains `commonplace/`; the binaries table), `docs/adr/README.md` (row 0024), `docs/explanation/landscape.md` (the memory map table from ADR-0024 and the sentence that Mem0, Zep and Letta remain choices beside it), `docs/how-to/bring-your-own-foreman.md` (a steward can mount `commonplace` too)
- Test: `uv run --no-sync pytest tests/test_docs*.py -q`

- [ ] **Step 1: Write the pages in STE**

- [ ] **Step 2: Check every command shown exists**

Run: `for c in stores list show search index serve; do uv run --no-sync daisugi notes $c --help >/dev/null || echo "missing $c"; done`
Expected: no `missing` lines.

- [ ] **Step 3: Commit**

```bash
git add docs/how-to/commonplace.md AGENTS.md docs/adr/README.md docs/explanation/landscape.md docs/how-to/bring-your-own-foreman.md
git commit -m "docs: the commonplace how-to, the memory map in landscape, AGENTS and ADR index rows"
```

---

## Self-review notes

- Spec coverage: ADR-0024 decisions 1 to 8 map to Tasks 1 to 8; rulings CM-R-1 to CM-R-8 each have a test (CM-R-1, 2: Task 1 and 4; CM-R-3: Task 2 and 5; CM-R-4: Task 3; CM-R-5: Task 3; CM-R-6: Task 4; CM-R-7: Task 5; CM-R-8: Task 2).
- Sizes: Python about 700 lines plus 600 of tests (Tasks 1 to 5, one to two sessions); cases one session; Go and Rust about 700 lines each plus tests, one session each. About five sessions in all.
- Not in this plan: mounting `commonplace` into coppice's steward turn by default (the steward's tool list is the chair's; a one-line config follow-on once ADR-0023 lands), a potion index on the GPU box, and any write tool.
