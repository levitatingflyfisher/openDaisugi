# ADR-0024: Commonplace, the owner's notes as semantic memory

- **Status:** Proposed (owner to confirm)
- **Date:** 2026-10-30

## Context

daisugi's memory map has four parts:

| Part | Kind | What it holds | Where |
|---|---|---|---|
| journal | episodic | what agents did: traces, receipts, verdicts | `journal.py`, `<data home>/journal/` |
| Pando R2 | episodic | what was said: the verbatim log and its summary tree | ADR-0023, `<data home>/pando/<store>/` |
| garden | procedural | how to do things: pathways, skills, grafts | `pathway_store.py`, `gardener/` |
| **Commonplace** | semantic | what the owner knows and decided: Markdown notes | this ADR |

The fourth part does not exist. The owner's standing knowledge lives in Markdown already:
memory notes, `VISION.md`, `HOW-WE-BUILD.md`, project ledgers, a notes folder. A steward or a
worker reads these only when someone pastes a path. There is no way to ask "what do my notes
say about F-Droid signing?" from a model's tool list.

The usual answer is a memory product: Mem0, Zep, Letta, a vector database. Each adds a
service, a schema the owner does not own, often an account, and sometimes a network call with
the owner's text. The portfolio rule is local-first and privacy by default, and the project
rule is that every dependency must earn its place. The garden already has three matchers that
need no server and no download: `lexical` (ADR-0019, stdlib plus numpy), `potion` (ADR-0018)
and `int8` (ADR-0021). The Go and Rust ports carry `lexical` (`clients/go/internal/embed/lexical`,
`clients/rust/src/pathways/lexical.rs`).

## Decision

**Commonplace is a read-only view over directories of the owner's Markdown, plus an optional
basic index built with the garden's matcher. It is exposed to a model as an MCP server.**

1. **A store is one directory of Markdown.** `[commonplace]` in daisugi's config names stores:
   `work = "~/notes/work"`, `home = "~/notes/home"`. Stores are separate directories, never one
   store with a filter (the same rule as ADR-0023). A query names one store. A root inside
   another root is refused at config load. Files are `.md`, `.markdown` and `.txt`, under
   1 MiB each. Symlinks that leave the root are not followed. Hidden dirs are skipped.
2. **The notes are the truth. The index is a cache.** `<data home>/commonplace/<store>/index.db`
   is SQLite: one row per chunk (file path, heading path, chunk text, mtime, sha256, vector).
   `daisugi notes index --store S` rebuilds the rows of files whose mtime or sha changed and
   drops rows of files that are gone. Deleting `index.db` loses nothing. The index carries the
   matcher identity (`lexical`, `potion`, `int8`), as the pathway store does (ADR-0018); a
   search with another matcher than the index was built with rebuilds first.
3. **A chunk is a heading section.** Split on ATX headings; a section longer than 2,000 bytes
   is cut at paragraph breaks into pieces of at most 2,000 bytes that each keep the heading
   path. A file with no heading is one section named by its file name.
4. **Search ranks chunks by cosine on the matcher's vectors.** `lexical` is the default and
   needs no model. With no index, `notes search` falls back to a case-folded substring scan
   over the files and says one line: `no index yet; ranking is off. Run: daisugi notes index
   --store S`. It never refuses because an index is missing.
5. **No vector database and no service.** SQLite in the standard library, vectors as packed
   float32 blobs, cosine in numpy (Python), plain loops (Go, Rust). A store of ten thousand
   chunks is a few megabytes and a brute-force scan is under 100 ms. If a store ever outgrows
   that, the ruling is a flat file of vectors, not a server.
6. **A model reads it through MCP.** `daisugi notes serve --store S` is a stdio server named
   `commonplace` with `notes_search(query, k=5)`, `notes_read(path, offset?, size?)` and
   `notes_list(prefix?)`. All three are read-only. `notes_read` refuses a path outside the
   store root. There is no `notes_write`: the owner edits notes with an editor, a worker edits
   them as files under the warden, and neither path goes through this server.
7. **The CLI is `daisugi notes`:** `list`, `show PATH`, `search QUERY -k N`, `index`, `stores`,
   `serve`. Every verb takes `--store NAME` and `--data-dir DIR`.
8. **Parity.** Python is the oracle. Go and Rust port it. The part is small (about 700 lines
   per port) and every piece it needs exists in all three: SQLite, the lexical vector, the
   data home rule. Golden cases: `clients/notes_cases.py` and `clients/notes_compare.py`.

## What Commonplace is not

- Not a wiki, a notes app or an editor. It reads what the owner already writes.
- Not an extraction pipeline. No model turns chats into facts behind the owner's back. A fact
  reaches Commonplace when a person writes it in a file (a steward may propose a line; the
  owner saves it).
- Not a knowledge graph. Headings and file paths are the only structure.
- Not a substitute for Pando R2. "What did we say on Tuesday" is a Pando question. "What is
  our rule on F-Droid" is a Commonplace question. A steward with both tools asks the right one.
- Not a replacement for Mem0, Zep, Letta or a hosted memory. Those remain choices beside it
  for anyone who wants them. The portfolio does not depend on any of them, and a Commonplace
  store can be exported to any of them, since it is Markdown on disk.

## Consequences

- **Buys:** the owner's standing knowledge reaches every model that can mount an MCP server,
  with no new service, account or network call. Zero setup: `lexical` needs no download. The
  owner keeps editing in whatever editor they use. Work and home notes cannot mix.
- **Costs:** one more CLI group, one more MCP server, one more SQLite file per store. A third
  place where the matcher identity must be checked. Chunking by heading is crude for notes with
  no headings; the fallback is one chunk per file.
- **Forecloses:** a vector database dependency, a write tool for models, and a background
  extractor. Each would be a new ADR.

## Alternatives considered

1. **Mem0 or Zep as the semantic store.** Rejected: a service with its own schema and, in the
   hosted shape, the owner's text leaving the machine. They stay as options the owner can add
   beside Commonplace.
2. **A vector database (Chroma, LanceDB, sqlite-vec).** Rejected: a dependency for a scan that
   numpy does in under 100 ms at this size. Revisit with a measurement, not a feeling.
3. **Put notes into Pando R2 as `note` messages.** This exists for import (M6 in
   `design-foreman.md`). Rejected as the semantic store: a note changes, and an append-only log
   cannot show the current text; the summary tree would hold stale lines.
4. **Let the steward write notes through a tool.** Rejected for now: a model writing the
   owner's reference text is the fact-extraction pipeline by another door. A worker can still
   edit files under the warden, where the write is a visible, gated file effect.
5. **One store with a `home:` prefix.** Rejected by the two-store rule.

## Rulings proposed (CM-R-n), for `clients/ADJUDICATIONS.md` when built

- CM-R-1: store names follow PD-R-1; `--store all` is refused with the PD-R-2 text.
- CM-R-2: a root that does not exist is refused at command time with
  `commonplace store <name>: <path> does not exist`; config load only checks nesting.
- CM-R-3: `notes_read` and `notes show` refuse a path that resolves outside the root, and a
  symlink target outside the root, with `outside the store`.
- CM-R-4: the index stores the matcher name and dimension; a mismatch rebuilds the index and
  says so on stderr. It never mixes vectors from two matchers.
- CM-R-5: search output is deterministic: ties break by path, then by chunk order.
- CM-R-6: `notes search` with no index scans files and says the one line of decision 4; exit 0.
- CM-R-7: the MCP server serves one store, named at start. It takes no `store` argument, so a
  model cannot change stores inside one session.
- CM-R-8: files over 1 MiB are listed with `skipped: over 1 MiB` and are not indexed.
