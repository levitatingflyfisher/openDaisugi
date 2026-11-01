# ADR-0023: Pando R2, the conversation memory, moves from coppice into daisugi

- **Status:** Proposed (owner to confirm)
- **Date:** 2026-10-30
- **Needs the owner's word on:** the parity ruling (section "Parity"), the ruling list at the end.

## Names

- **Pando R2** (Reversible Recollections) is the conversation memory. Today the code calls it
  the foreman's chat: `harness/coppice/internal/foreman` and `harness/coppice-rs/src/foreman`.
- **steward** is the role in the chair: whoever answers the owner, on any model, in any harness.
  The code still says `foreman`. The rename is Wave 2 of the backlog and is not part of this ADR.
- **warden** is the gate. The code still says `gate`.

This ADR uses the new names in prose and the current identifiers where it cites code.

## Context

Pando R2 is built and in daily use. It follows Taelin's UniiChat recipe: an append-only
verbatim log (`main/YYYY-MM-DD.jsonl`), a binary summary tree built once and never rebuilt
(`tree/YYYY-MM-DD.jsonl`, nodes of at most 512 bytes, named `first+count`), a view that is
fine near now and coarse in the past (`view.json`, 64 to 128 KiB, the merge order
`due = (T - last) / 2^l` in `order.go`), and the two read tools `zoom(id, n)` and `date(id)`.
UniiChat is Apache-2.0. OptMem is unlicensed and no code is copied from it. The Go and Rust
twins agree on compare cases (FR-R-1 onward).

The problem is where it lives. The store, the compactor, the importer and the two read tools
are all inside coppice. So:

1. **Without coppice the memory is unreachable.** A harness that is not a coppice pane cannot
   zoom the owner's own log. `daisugi mcp serve` cannot serve it. A script cannot read it.
2. **The chair and the memory are one process.** `foreman_chat.go` opens the chat
   (`foreman.Open`), runs turns, runs the compactor, serves the tool socket, and imports old
   chats, all in the coppice server. A change to the turn runner risks the log.
3. **Business and personal memory.** The owner ruled two chats on 2026-10-08 (the work chat and
   a private household chat). Today one store exists, `<data home>/foreman`. A second store
   needs a store name in every path, and coppice is the wrong place to own that rule.
4. **The memory map has four parts and three of them are in daisugi.** The journal (episodic:
   what agents did, `journal.py`), the garden (procedural: pathways and skills), and the new
   Commonplace (semantic: the owner's notes, ADR-0024) are daisugi's. Pando R2 (episodic: what
   was said) is the one part that is not.

ADR-0020 says daisugi imports nothing above it and the floor supervises and does not decide.
A memory store is below the floor by that rule: it is data the floor reads and writes, like
the journal.

## Decision

**Move Pando R2 into daisugi. coppice keeps the chair.**

1. **daisugi owns the store.** The log, the tree, the view, the merge order, the compactor's
   scheduling, the importer and the read tools move into daisugi, as one package per port:
   Go `clients/go/pando`, Rust `clients/rust/pando` (its own crate, no Z3), and Python
   `opendaisugi/pando/`. The package opens no socket and starts no server. Its only outside
   call is the summarizer, an interface that a model backend or a test fake fills.
2. **The store path is `<data home>/pando/<store>/`.** Under it: `main/`, `tree/`,
   `view.json`, `import.json`, `lock`. The data home is the daisugi rule in `datahome.py`
   (`$OPENDAISUGI_HOME`, else XDG, else `~/.opendaisugi`), not coppice's.
3. **Stores are separate directories, never one store with a filter.** The two named stores
   are `work` and `home`. A command names one store. There is no `--store all`, no union view,
   no cross-store zoom. A `home` message can never reach a `work` view, because no code reads
   two stores at once. The default store is `work`.
4. **The CLI is `daisugi pando`:** `append`, `log --day`, `zoom ID N`, `date ID`, `view`,
   `status`, `recall QUERY`, `tend`, `import`, `migrate`, `stores`. Every verb takes
   `--store NAME` and `--data-dir DIR`.
5. **The MCP server is `daisugi pando serve --store NAME`**, a stdio server named `pando` with
   three read tools: `zoom(id, n, offset?, size?)`, `date(id)`, `recall(query, k?)`. `recall`
   ranks view lines and raw messages by the garden's matcher (lexical by default, ADR-0019) and
   returns ids, so the model zooms next. The server has no write tool. It is a separate server
   from `opendaisugi`, so `mcp__pando__recall` never collides with the pathway tool
   `mcp__opendaisugi__recall`.
6. **The compactor is `daisugi pando tend`.** It builds pending nodes one call at a time, in
   the order `MostDue` gives, with the model named by a daisugi agent profile. coppice runs it
   in process as before, through the package; a box with no coppice runs it from cron. Tests
   use a fake summarizer. The gap rule stands: a message is never out of sight.
7. **coppice calls daisugi for memory and keeps the chair.** The chair is: the turn
   (`foreman_turn.go`, `foreman_turn_sprig.go`), the four-tool socket and the standing socket,
   `spawn` and `status`, failover across accounts, the worker pool and queue, `turns.jsonl` and
   `calls.jsonl`, panes, scheduling and screens. coppice links the `pando` package for the
   log, tree, view, zoom and date, and relays `zoom` and `date` from its tool socket to it. It
   deletes its own copies of the moved files. `[foreman] store = "work"` names the store.
8. **Imports are daisugi's.** `daisugi pando import --from claude|codex|web`. The Claude Code
   and Codex readers move from `import.go`. `web` is new: the claude.ai export
   (`conversations.json` in the zip). A message is logged once, by its session id and its
   message id (the act key, FM-R-86 onward). `--map OLD=NEW` rewrites a path prefix so a
   transcript from another machine names the same project. Imported lines keep kind `past`.
9. **Migration moves aside and never deletes.** `daisugi pando migrate` copies `main/`,
   `tree/`, `view.json` and `import.json` from the coppice foreman dir to
   `<data home>/pando/work/`, opens the copy and checks it loads byte for byte
   (the same view, the same message count), then renames the old dir to
   `foreman.moved-<UTC stamp>` beside where it was. `turns.jsonl`, `calls.jsonl`,
   `failover.json` and the queue files stay with the chair and are copied to coppice's own data
   dir. Nothing is deleted. A coppice that starts and finds the old dir says one line that names
   the command. A coppice that finds a `foreman.moved-*` dir says nothing.

## Parity

The owner's ruling of 2026-09-26 is full parity: Python is the oracle, Go and Rust agree with
it on golden cases. Pando R2 was built against that order. It exists in Go and Rust and not in
Python. The numbers (non-test lines, `wc -l`, 2026-10-30):

| Part | Go today | Rust today | Python today |
|---|---|---|---|
| store: log, tree, view, order, lock, fs (`chat`, `order`, `view`, `build`, `fsutil`, `dir`, `lock_*`) | 2,420 | 3,200 | 0 |
| compactor scheduling and model call (`compactor`) | 290 | 840 | 0 |
| importer (`import`, `importcli`) | 1,075 | 1,390 | 0 |
| the two tools and the relay (`tools`, part of `cli`) | 540 | 880 | 0 |
| tests for the above | about 5,000 | about 3,000 | 0 |
| chair (turns, calls, failover, pool, server glue): **stays in coppice** | about 6,600 | about 7,400 | 0 |

Two ways to meet the ruling:

**A. Full parity, Python oracle.** Write the store, compactor, importer and tools in Python
(about 3,000 lines plus 3,000 of tests), then write `clients/pando_cases.py` from it and make
the Go and Rust packages agree. Cost: Python 4 to 6 sessions; Go move 2 to 3; Rust move 2 to 3;
cases, compare and rulings 1 to 2; coppice and coppice-rs rewired 2. **About 12 to 16
sessions.** The Python code has no user on day one: the owner's memory is written by the Go
coppice and will stay so.

**B. A written exception: Go is the oracle for Pando R2, Rust is the twin, Python is a
read-only client.** The Go package is moved, not rewritten. The Rust package is moved and kept
in step by the compare cases that already exist for the foreman (Go writes the cases, as
`clients/coppice_compare.py` already does for coppice). Python gets `opendaisugi/pando/`
with `open_store`, `zoom`, `date`, `recall` and `log --day` over the files (about 600 lines
plus tests), and no writer. Cost: Go move 2 to 3 sessions; Rust move 2 to 3; Python reader 1;
cases and rulings 1; coppice and coppice-rs rewired 2. **About 7 to 9 sessions.**

**Recommendation: B, as a written exception, for this part only.** The reasons: the writer
already exists twice and agrees with itself; the Python oracle would be the third copy of a
deterministic file format with no Python writer in use; coppice (Go) stays the only process
that appends live messages, so a Python writer would add a second lock holder to the owner's
log for no gain. The exception is narrow: Python still reads every file, so `daisugi mcp
serve` in Python can mount the memory, the fixtures stay readable from Python, and the file
format is specified in `docs/spec/` so a Python writer can be added later without a second
design. **This reverses the 2026-09-26 ruling for one part and needs the owner's word.** If
the owner keeps full parity, the plan's tasks P-A to P-C add the Python writer and the cost
is option A.

## Consequences

- **Buys:** the memory outlives coppice. Any MCP harness, a cron job, or a shell can read it.
  Two stores by construction, not by filter. The chair becomes smaller and safer to change.
  The memory map is whole inside daisugi: journal, Pando R2, garden, Commonplace.
- **Costs:** a Go module boundary (coppice gets a `replace` to `clients/go`) and a Rust path
  dependency. Two more CLI surfaces to keep in step. A migration step for the owner's live
  store. Under option B, the first written exception to the parity ruling.
- **Forecloses:** a second memory format in another harness adapter, and a cross-store query.
  A later wish to search work and home together is answered with two queries and a human.

## Alternatives considered

1. **Leave it in coppice and add `coppice foreman zoom` for scripts.** Rejected: the memory
   is still lost without coppice, the two-store rule lands in the floor, and the chair keeps
   growing around the log.
2. **Make Pando R2 its own daemon with a socket.** Rejected: a daemon is one more thing to
   start, and the owner wants the memory readable when nothing runs. Files plus a lock do that.
   The MCP server is started by the harness that needs it and exits with it.
3. **Put it in sprig.** Rejected: sprig is on hold with the owner, and the memory is not loop
   state.
4. **One store with a `scope` field.** Rejected by the owner's rule: a filter can be forgotten
   by one caller. Two directories cannot be joined by accident.
5. **Keep the compactor in coppice and move only the files.** Rejected: a store whose tree only
   grows while coppice runs is not independent. `daisugi pando tend` is small and reuses the
   agent profiles daisugi already has.

## Rulings proposed (PD-R-n), for `clients/ADJUDICATIONS.md` when built

- PD-R-1: the store path is `<data home>/pando/<store>/`; the names `work` and `home` are
  reserved; any other name is allowed, `[a-z][a-z0-9-]{0,31}`.
- PD-R-2: no verb reads two stores. `--store all` is refused with
  `one store at a time: pando stores are separate on purpose.`
- PD-R-3: the Go package is `clients/go/pando` (outside `internal`, so coppice can import it)
  and imports no cgo; the Rust crate is `clients/rust/pando` with no Z3 dependency.
- PD-R-4: one writer per store. The lock file is `lock` in the store dir; a second writer gets
  `another process has this pando store open` and exits 1. Readers never take the lock.
- PD-R-5: `recall` is a lexical ranking by default and uses the garden's `matcher_model`
  setting; it returns ids and one-line text, never a raw message. The model zooms.
- PD-R-6: migrate copies, checks, then renames. It never deletes. A second run on a migrated
  dir says `already moved: <path>` and exits 0.
- PD-R-7: the `web` importer reads `conversations.json` from the export zip on this machine,
  writes a proposal list (`work`, `home` or `skip` per chat, one-line reason) and writes
  nothing to any store until the owner confirms the list with `--accept FILE`.
- PD-R-8: imported messages are deduplicated by (source, session id, message id). A copied or
  forked Codex rollout keeps its lines once. A web chat is keyed by its conversation `uuid`
  and message `uuid`.
- PD-R-9: `--map OLD=NEW` applies to the project path a transcript names (its `cwd` or
  rollout dir) and to nothing in message text.
- PD-R-10: under option B, Python never writes a store. Its `open_store` opens read-only and
  refuses `append` with `the Python package reads pando stores; coppice or daisugi (Go or Rust)
  writes them.`
