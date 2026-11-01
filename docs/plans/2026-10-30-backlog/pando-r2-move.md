# Pando R2 Move Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the conversation memory (Pando R2: log, tree, view, compactor scheduling, importer, read tools) out of coppice into daisugi, with two separate stores, a CLI, an MCP server and a migration that moves the owner's data aside and never deletes it.

**Architecture:** One `pando` package per port owns the files under `<data home>/pando/<store>/`. coppice keeps the chair (turns, spawn, status, failover, pool) and links the package for memory. Go is the oracle for this part and Rust the twin, checked by the golden fixtures Go already writes; Python is a read-only client (ADR-0023, option B). Tasks P-A to P-C at the end add the Python writer if the owner keeps full parity.

**Tech Stack:** Go 1.26 (`clients/go`, module `daisugi-verify`), Rust (`clients/rust`), Python 3.12 (`src/opendaisugi`), SQLite only in the Python reader's tests, no new third-party packages.

**Spec:** `docs/adr/0023-pando-r2-in-daisugi.md`. Design background: `docs/plans/2026-09-24-omarchy/design-foreman.md`, `docs/how-to/import-old-chats.md`.

## Global Constraints

- Prose, comments and on-screen text in STE: short sentences, plain words, no em dashes, never the words "seam" or "the layer".
- Names: prose says Pando R2, steward, warden. Code identifiers stay `foreman` and `gate` until Wave 2 (`w2-rename.md`). New code names its own things `pando`.
- The store path is `<data home>/pando/<store>/`; store names match `[a-z][a-z0-9-]{0,31}`; `work` and `home` are the two named stores; `work` is the default (PD-R-1).
- No verb reads two stores. `--store all` is refused with `one store at a time: pando stores are separate on purpose.` (PD-R-2).
- Node bytes 512; view 64 to 128 KiB; message split 32 KiB; dates `2006-01-02T15:04:05Z` UTC. Unchanged from `harness/coppice/internal/foreman/chat.go`.
- The Go package is `clients/go/pando` (not under `internal`) and imports no cgo. The Rust crate is `clients/rust/pando`, no Z3 (PD-R-3).
- One writer per store, lock file `lock`, second writer exits 1 with `another process has this pando store open` (PD-R-4).
- Tests never run a real `claude`, never use the network but loopback, never touch `~/.opendaisugi`, `~/.local/state/coppice`, the owner's live coppice socket or data dir. Every coppice run in a test or trial passes its own `--socket` and `--data-dir`. Scratch goes under `~/opendaisugi-scratch/`, never `/tmp`.
- One heavy job at a time: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G <cmd>`, with `go test -p 1`, `CARGO_BUILD_JOBS=2`.
- Never `pkill -f`. Stop a test process by its pid.
- Commits: small, by explicit path, no attribution lines, no push. Never commit `CLAUDE.md`, `docs/superpowers/`, `uv.lock`.
- Migration never deletes. It copies, checks, then renames the old dir to `foreman.moved-<UTC stamp>` (PD-R-6).
- Imported messages are deduplicated by (source, session id, message id) (PD-R-8). `--map OLD=NEW` touches project paths only, never message text (PD-R-9).
- The Python package never writes a store (PD-R-10).

## Review Focus

1. A store dir that exists but holds a torn last line in `main/` or `tree/`: the open repairs it as today (FM-R-68 to FM-R-70) and the CLI prints the notice once. Test in Task 1.
2. `daisugi pando zoom` on a store that coppice has open for writing: the reader must not take the lock and must see the lines already fsynced. Test in Task 2.
3. `migrate` run while coppice still has the old dir open: it must refuse with `coppice has this chat open; stop it first` and copy nothing. Test in Task 6.
4. A web export with two conversations that share a message uuid (an export bug) must log each once per conversation, keyed by (conversation uuid, message uuid). Test in Task 5.
5. `--store home` on a box with no `home` store yet: read verbs say `no store home at <path>` and exit 1; `append` and `import --accept` create it 0700. Test in Task 2 and Task 3.

---

### Task 1: The `pando` Go package, moved from coppice

**Files:**
- Create: `clients/go/pando/` from `harness/coppice/internal/foreman/{chat,order,view,build,fsutil,dir,lock_unix,lock_other}.go` (`git mv`, then `package pando`)
- Create: `clients/go/pando/store.go`
- Move: `harness/coppice/internal/foreman/testdata/golden/` to `clients/fixtures/pando/golden/`
- Move: the tests of the moved files (`chat_test.go`, `order_test.go`, `view*_test.go`, `golden_test.go`, `fresh_test.go`, `repair_test.go`, `snapshot_test.go`, `perms_test.go`, `fake_test.go`, `clip_test.go`, `appendfail_test.go`) to `clients/go/pando/`
- Test: `clients/go/pando/store_test.go`

**Interfaces:**
- Consumes: nothing new. `Open(ctx, Config) (*Chat, error)`, `OpenReadOnly(dir) (*Chat, error)`, `(*Chat).Append/AppendAt/AppendAct/ZoomPage/Date/Day/Roots/ViewText/Count/Pending/Notice/Close` keep their names and behaviour.
- Produces: `func StoreDir(dataDir, store string) (string, error)`; `func ValidStore(name string) bool`; `const DefaultStore = "work"`; `var ErrStoreAll = errors.New("one store at a time: pando stores are separate on purpose.")`; `func ListStores(dataDir string) ([]string, error)` (dirs under `<dataDir>/pando` that hold `main/`, sorted).

- [ ] **Step 1: Write the failing test**

```go
// clients/go/pando/store_test.go
package pando

import (
	"path/filepath"
	"testing"
)

func TestStoreDirRules(t *testing.T) {
	d := t.TempDir()
	got, err := StoreDir(d, "work")
	if err != nil || got != filepath.Join(d, "pando", "work") {
		t.Fatalf("work: %q %v", got, err)
	}
	if _, err := StoreDir(d, "all"); err != ErrStoreAll {
		t.Fatalf("all must be refused, got %v", err)
	}
	for _, bad := range []string{"", "Work", "1x", "a/b", "a b", "x-very-long-name-that-goes-past-thirty-two-chars"} {
		if _, err := StoreDir(d, bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
	if DefaultStore != "work" {
		t.Fatal("default store is work")
	}
}

func TestListStoresOnlyRealOnes(t *testing.T) {
	d := t.TempDir()
	mk := func(name string, withMain bool) {
		p := filepath.Join(d, "pando", name)
		if withMain {
			p = filepath.Join(p, "main")
		}
		if err := osMkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mk("work", true)
	mk("home", true)
	mk("junk", false)
	got, err := ListStores(d)
	if err != nil || len(got) != 2 || got[0] != "home" || got[1] != "work" {
		t.Fatalf("got %v %v", got, err)
	}
}
```

(`osMkdirAll` is `os.MkdirAll`; import `os` and call it directly.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run 'TestStoreDir|TestListStores' -v`
Expected: FAIL, `undefined: StoreDir`.

- [ ] **Step 3: Move the files**

`git mv` each file named above into `clients/go/pando/`, change `package foreman` to `package pando`, change the import of `internal/datahome` to `daisugi-verify/internal/datahome`, delete `DefaultDir` and `LogDir` from `dir.go` (the store path rule replaces them). Fix the golden test's path to `../../fixtures/pando/golden` from the package dir. Keep every test green that moved.

- [ ] **Step 4: Implement `store.go`**

`StoreDir(dataDir, store string) (string, error)`: refuse `all` with `ErrStoreAll`; refuse a name that fails `ValidStore` with `fmt.Errorf("bad store name %q: use a-z, 0-9 and -, 1 to 32 letters, starting with a letter", store)`; return `filepath.Join(dataDir, "pando", store)`. `ListStores` reads `<dataDir>/pando`, keeps dirs that hold a `main` dir, sorts.

- [ ] **Step 5: Run the package tests**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/...`
Expected: ok, including the golden test over `clients/fixtures/pando/golden`.

- [ ] **Step 6: Commit**

```bash
git add clients/go/pando clients/fixtures/pando harness/coppice/internal/foreman
git commit -m "pando: move the store (log, tree, view, order) from coppice into clients/go/pando with a two-store path rule"
```

---

### Task 2: `daisugi pando` read verbs in Go

**Files:**
- Create: `clients/go/internal/cli/pandocmd.go`
- Modify: `clients/go/internal/cli/cli.go` (the subcommand switch; add `pando`)
- Modify: `clients/go/internal/cli/helpall_gen.go` through its generator (`clients/gen_help.py`), so `--help` lists `pando`
- Test: `clients/go/internal/cli/pandocmd_test.go`

**Interfaces:**
- Consumes: Task 1 (`StoreDir`, `OpenReadOnly`, `ListStores`, `(*Chat).ZoomPage/Date/Day/ViewText/Count/Pending`).
- Produces: `func (e *Env) pando(args []string) error` with verbs `stores`, `status`, `log --day YYYY-MM-DD`, `zoom ID N [--offset B --size B]`, `date ID`, `view`, `recall QUERY [-k N]`. Flags on every verb: `--store NAME` (default `work`), `--data-dir DIR` (default the data home), `--json`. Exit codes: 0 ok; 1 no store or bad id; 2 usage.
- Produces: `func recallLines(c *pando.Chat, query string, k int) []RecallHit` in `clients/go/pando/recall.go`, `type RecallHit struct{ ID string; Score float64; Line string }`, ranking view lines and raw messages by cosine on `embed/lexical.Vector`, ties by id (PD-R-5).

- [ ] **Step 1: Write the failing test**

```go
// clients/go/internal/cli/pandocmd_test.go
package cli

import (
	"context"
	"strings"
	"testing"

	"daisugi-verify/pando"
)

func seedStore(t *testing.T, data, store string, lines ...string) {
	t.Helper()
	dir, err := pando.StoreDir(data, store)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pando.Open(context.Background(), pando.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, l := range lines {
		if _, err := c.Append(context.Background(), "user", l); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPandoZoomAndDateReadWithoutLock(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "we chose sqlite for the index", "and potion stays optional")
	// A writer holds the store open while the CLI reads.
	dir, _ := pando.StoreDir(data, "work")
	w, err := pando.Open(context.Background(), pando.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var out, errs strings.Builder
	code := Main(&Env{Args: []string{"pando", "zoom", "0", "1", "--data-dir", data}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errs})
	if code != 0 || !strings.Contains(out.String(), "we chose sqlite") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errs.String())
	}
	out.Reset()
	code = Main(&Env{Args: []string{"pando", "date", "1", "--data-dir", data}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errs})
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out.String()), "Z") {
		t.Fatalf("date: code %d out %q", code, out.String())
	}
}

func TestPandoRefusesAllAndMissingHome(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "x")
	var out, errs strings.Builder
	code := Main(&Env{Args: []string{"pando", "status", "--store", "all", "--data-dir", data}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errs})
	if code != 2 || !strings.Contains(errs.String(), "one store at a time") {
		t.Fatalf("all: code %d err %q", code, errs.String())
	}
	errs.Reset()
	code = Main(&Env{Args: []string{"pando", "status", "--store", "home", "--data-dir", data}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errs})
	if code != 1 || !strings.Contains(errs.String(), "no store home at ") {
		t.Fatalf("home: code %d err %q", code, errs.String())
	}
}

func TestPandoRecallRanksByWords(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "the index is sqlite", "lunch was late", "sqlite index rebuilds from files")
	var out, errs strings.Builder
	code := Main(&Env{Args: []string{"pando", "recall", "sqlite index", "-k", "2", "--data-dir", data}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errs})
	if code != 0 {
		t.Fatal(errs.String())
	}
	if strings.Contains(out.String(), "lunch") {
		t.Fatalf("recall must rank the two sqlite lines first: %q", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./internal/cli/ -run TestPando -v`
Expected: FAIL, `pando` is an unknown command (exit 2) or `undefined`.

- [ ] **Step 3: Implement `pandocmd.go` and `recall.go`**

`pando(args)` parses `--store`, `--data-dir`, `--json`, then the verb. Read verbs open with `OpenReadOnly` and never take the lock. A missing store dir prints `no store <name> at <path>` and exits 1. Output lines: `zoom` prints the text as `ZoomPage` returns it; `date` prints the date; `log --day` prints `i<TAB>kind<TAB>date<TAB>text` per message; `view` prints `ViewText` lines as `id<TAB>line`; `status` prints `store, messages, pending nodes, view bytes, notice`; `recall` prints `id<TAB>score<TAB>line`. `--json` prints one JSON object or list. `recallLines` embeds the query and each candidate with `lexical.Vector`, cosine, takes the top k.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./internal/cli/ -run TestPando -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add clients/go/internal/cli/pandocmd.go clients/go/internal/cli/pandocmd_test.go clients/go/internal/cli/cli.go clients/go/internal/cli/helpall_gen.go clients/go/pando/recall.go
git commit -m "daisugi pando: read verbs (stores, status, log, zoom, date, view, recall) over a named store, no lock taken"
```

---

### Task 3: `append` and `tend` (the compactor) in daisugi

**Files:**
- Move: `harness/coppice/internal/foreman/compactor.go`, `prompt_compress.txt`, `prompt_merge.txt`, `compactor_test.go`, `compactor_best_test.go`, `retry_test.go` to `clients/go/pando/`
- Create: `clients/go/pando/tend.go`
- Modify: `clients/go/internal/cli/pandocmd.go` (add `append`, `tend`)
- Test: `clients/go/pando/tend_test.go`, `clients/go/internal/cli/pandocmd_test.go`

**Interfaces:**
- Consumes: `Summarizer` interface, `ModelSummarizer` (moved), `(*Chat).Pending`, the agent profiles in `clients/go/internal/agentprof` for the model behind `--profile`.
- Produces: `func Tend(ctx context.Context, c *Chat, s Summarizer, max int) (built int, err error)`: builds up to `max` pending nodes (0 means all), one call at a time, in queue order, and stops at the first failure after the retry schedule in `compactor.go` (30 s, 2 min, 5 min; a test clock). `daisugi pando append --kind user|note TEXT` appends (takes the lock). `daisugi pando tend [--profile NAME] [--max N] [--dry-run]`; `--dry-run` lists the pending node ids and calls no model.

- [ ] **Step 1: Write the failing test**

```go
// clients/go/pando/tend_test.go
package pando

import (
	"context"
	"strings"
	"testing"
)

func TestTendBuildsPendingInOrderWithFake(t *testing.T) {
	c := openTestChat(t) // the helper fake_test.go already has, with a test clock
	long := strings.Repeat("word ", 300) // over 512 bytes, needs a Compress call
	for i := 0; i < 4; i++ {
		if _, err := c.Append(context.Background(), "user", long); err != nil {
			t.Fatal(err)
		}
	}
	before := c.Pending()
	if before == 0 {
		t.Fatal("test needs pending nodes")
	}
	fake := &Fake{} // fake_test.go's summarizer; it records calls in order
	built, err := Tend(context.Background(), c, fake, 2)
	if err != nil || built != 2 || c.Pending() != before-2 {
		t.Fatalf("built %d err %v pending %d", built, err, c.Pending())
	}
	if _, err := Tend(context.Background(), c, fake, 0); err != nil || c.Pending() != 0 {
		t.Fatalf("tend all: pending %d err %v", c.Pending(), err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run TestTend -v`
Expected: FAIL, `undefined: Tend`.

- [ ] **Step 3: Implement `Tend` and the two verbs**

`Tend` loops: take the front pending node, ask the summarizer (`Compress` for a leaf over the limit, `Merge` for a pair), write the node, count; stop at `max` or at an error after the retry schedule says give up. The coppice server's loop in `foreman_chat.go` (`Next`/`nextLocked`) is the source; move the loop body, not the server's mutex and timers. `append` opens with `Open` (lock), appends, closes. `tend --profile` builds a `ModelSummarizer` from the named agent profile through the same HTTP client the garden uses (`internal/llm`); `--profile` empty means Claude Code through `claude -p`, as the compactor does today. `--dry-run` prints `pending: N` and the ids.

- [ ] **Step 4: Run tests and the moved compactor tests**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ ./internal/cli/ -run 'TestTend|TestCompactor|TestRetry|TestPando'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add clients/go/pando clients/go/internal/cli/pandocmd.go clients/go/internal/cli/pandocmd_test.go harness/coppice/internal/foreman
git commit -m "daisugi pando: append and tend; the compactor's schedule moves into the pando package"
```

---

### Task 4: `daisugi pando serve`, the MCP server

**Files:**
- Create: `clients/go/pando/mcp.go` (the stdio JSON-RPC loop; move the MCP framing from `harness/coppice/internal/foreman/tools.go`, drop the socket relay)
- Modify: `clients/go/internal/cli/pandocmd.go` (add `serve`)
- Test: `clients/go/pando/mcp_test.go`

**Interfaces:**
- Consumes: Task 1 and Task 2 (`OpenReadOnly`, `ZoomPage`, `Date`, `recallLines`).
- Produces: `func ServeMCP(ctx context.Context, c *Chat, in io.Reader, out io.Writer) error`. Server name `pando`, protocol `2024-11-05`. Tools and schemas:
  - `zoom`: `{id: integer, n: integer, offset?: integer, size?: integer}`, required `id`, `n`. Description: the one in `toolList()` today, minus the `label` sentence.
  - `date`: `{id: integer}`.
  - `recall`: `{query: string, k?: integer}`, required `query`; returns lines `id<TAB>score<TAB>line`, k default 8, max 32.
  - Any other tool name: JSON-RPC error `-32601`.

- [ ] **Step 1: Write the failing test**

```go
// clients/go/pando/mcp_test.go
package pando

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func rpc(t *testing.T, c *Chat, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := ServeMCP(context.Background(), c, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		res = append(res, m)
	}
	return res
}

func TestMCPListsThreeReadToolsAndZooms(t *testing.T) {
	c := openTestChat(t)
	c.Append(context.Background(), "user", "the warden denies by default")
	res := rpc(t, c,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"spawn","arguments":{}}}`,
	)
	if got := res[0]["result"].(map[string]any)["serverInfo"].(map[string]any)["name"]; got != "pando" {
		t.Fatalf("server name %v", got)
	}
	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("want zoom, date, recall; got %d", len(tools))
	}
	if !strings.Contains(string(mustJSON(res[2])), "warden denies") {
		t.Fatalf("zoom: %v", res[2])
	}
	if res[3]["error"] == nil {
		t.Fatal("spawn must be unknown here")
	}
}
```

(`mustJSON` is `json.Marshal` with a panic on error; add it to the test file.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run TestMCP -v`
Expected: FAIL, `undefined: ServeMCP`.

- [ ] **Step 3: Implement `ServeMCP` and `serve`**

Reuse `rpcRequest`, `rpcReply`, `objectSchema` and the line cap `MaxToolLine` from the moved `tools.go`. `initialize` answers `protocolVersion`, `capabilities: {tools: {}}`, `serverInfo: {name: "pando", version}`. `tools/call` for `zoom` maps args as `foreman_tools.go` does (`intArg`, offset and size, `ZoomPageBytes` cap). `serve` opens read-only and runs `ServeMCP` on stdin and stdout until EOF.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run TestMCP -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add clients/go/pando/mcp.go clients/go/pando/mcp_test.go clients/go/internal/cli/pandocmd.go
git commit -m "daisugi pando serve: a stdio MCP server named pando with zoom, date and recall, read-only"
```

---

### Task 5: The importer moves, and learns the web export, `--map` and `--accept`

**Files:**
- Move: `harness/coppice/internal/foreman/import.go`, `importcli.go`, `import_test.go`, `import_fix_test.go`, `import_fix2_test.go`, `testdata/transcripts/` to `clients/go/pando/` and `clients/fixtures/pando/transcripts/`
- Create: `clients/go/pando/import_web.go`
- Create: `clients/fixtures/pando/transcripts/web/conversations.json` (invented content, two conversations, one shared message uuid)
- Modify: `clients/go/internal/cli/pandocmd.go` (add `import`)
- Test: `clients/go/pando/import_web_test.go`

**Interfaces:**
- Consumes: `ImportOpts`, `Scan`, `Accept`, `Importer.Feed`, `(*Chat).AppendImported`, `HasAct` (moved as they are).
- Produces: `ImportOpts.From` gains `web`; `ImportOpts.WebZip string` (path of the export zip or of an unpacked `conversations.json`); `ImportOpts.Map []PathMap` with `type PathMap struct{ Old, New string }`; `func ParseMap(s string) (PathMap, error)` for `OLD=NEW`. `func ScanWeb(path string, o ImportOpts, has func(string) bool) ([]ImportSession, error)`: one `ImportSession` per conversation, `Key = "web:" + conversation uuid`, each message's act = `Key + ":" + message uuid`. `func WriteProposal(w io.Writer, ss []ImportSession) error` writes one line per chat: `<key>\t<proposed store>\t<reason>\t<title>`; proposed store is `skip` for an empty chat, else `work`. `daisugi pando import --from claude|codex|web|all [--web PATH] [--since DATE] [--map OLD=NEW]... [--dry-run] [--propose FILE | --accept FILE] [--store NAME]`. With `--accept FILE`, each line's second field names the store the chat goes to; a line that says `skip` is skipped; a line that names `all` is refused (PD-R-2).

- [ ] **Step 1: Write the failing test**

```go
// clients/go/pando/import_web_test.go
package pando

import (
	"path/filepath"
	"testing"
)

func TestScanWebKeysByConversationAndMessage(t *testing.T) {
	p := filepath.Join("..", "..", "fixtures", "pando", "transcripts", "web", "conversations.json")
	ss, err := ScanWeb(p, ImportOpts{From: "web"}, func(string) bool { return false })
	if err != nil || len(ss) != 2 {
		t.Fatalf("sessions %d err %v", len(ss), err)
	}
	// The fixture repeats one message uuid across both chats. Each chat keeps it.
	if ss[0].New+ss[1].New != 5 {
		t.Fatalf("want 5 new messages across both chats, got %d and %d", ss[0].New, ss[1].New)
	}
	seen := func(act string) bool { return act == ss[0].Key+":m1" }
	ss2, _ := ScanWeb(p, ImportOpts{From: "web"}, seen)
	if ss2[0].New != ss[0].New-1 {
		t.Fatal("a message already logged by its act must not count as new")
	}
}

func TestParseMapAndApplyToProjectOnly(t *testing.T) {
	m, err := ParseMap("/home/old=/home/new")
	if err != nil || m.Old != "/home/old" || m.New != "/home/new" {
		t.Fatal(m, err)
	}
	if _, err := ParseMap("nope"); err == nil {
		t.Fatal("a map needs =")
	}
	o := ImportOpts{Map: []PathMap{m}}
	if got := o.project("/home/old/repo"); got != "/home/new/repo" {
		t.Fatalf("project %q", got)
	}
	if got := o.project("/home/olden/x"); got != "/home/olden/x" {
		t.Fatalf("a prefix match is by path element: %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run 'TestScanWeb|TestParseMap' -v`
Expected: FAIL, `undefined: ScanWeb`.

- [ ] **Step 3: Implement**

`import_web.go`: open the path; if it ends in `.zip`, read `conversations.json` from the archive with `archive/zip`; parse a list of conversations `{uuid, name, created_at, chat_messages: [{uuid, sender, text, created_at}]}` (the claude.ai export shape, 2026). Map `sender` `human` to the imported role the Claude reader uses for the user and `assistant` to the model, kind `past` for both. `o.project(p)` applies the first `PathMap` whose `Old` is `p` or a parent of it by path element. `--propose FILE` writes the proposal and exits 0 with no store touched; `--accept FILE` reads it, groups sessions by store, calls `Accept` once per store, and then `Feed` per store with a summarizer only when `--tend` is given, else leaves the nodes pending for `daisugi pando tend`. Keep the Claude Code and Codex readers byte for byte; only their `cwd` goes through `o.project`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run 'TestScanWeb|TestParseMap|TestImport' -v`
Expected: PASS, moved import tests included.

- [ ] **Step 5: Commit**

```bash
git add clients/go/pando clients/fixtures/pando/transcripts clients/go/internal/cli/pandocmd.go harness/coppice/internal/foreman
git commit -m "daisugi pando import: the importer moves in; web export, --map for paths between machines, propose and accept"
```

---

### Task 6: `daisugi pando migrate`, move aside and never delete

**Files:**
- Create: `clients/go/pando/migrate.go`
- Modify: `clients/go/internal/cli/pandocmd.go` (add `migrate`)
- Test: `clients/go/pando/migrate_test.go`

**Interfaces:**
- Consumes: `OpenReadOnly`, `(*Chat).Count`, `ViewJSON`, the lock file name `lock`.
- Produces: `func Migrate(from, dataDir, store string, now time.Time) (Report, error)`; `type Report struct{ Copied []string; Kept []string; MovedTo string; Messages int64 }`. `from` is the old coppice foreman dir. It copies `main/`, `tree/`, `view.json`, `import.json` to `StoreDir(dataDir, store)`, opens the copy read-only, checks `Count` equals the source's and `ViewJSON` is byte-equal, then renames `from` to `from + ".moved-" + now.UTC().Format("20060102T150405Z")`. `Kept` lists the chair files found (`turns.jsonl`, `calls.jsonl`, `failover.json`, `queue*`), which stay inside the renamed dir for coppice to copy (Task 7). Errors: `ErrOpenElsewhere` (`coppice has this chat open; stop it first`) when `from/lock` is held; `ErrTargetExists` when the store dir already holds `main/`; a second run on a dir that no longer exists but has a `.moved-*` sibling returns `Report{MovedTo: that}` and the CLI prints `already moved: <path>` exit 0 (PD-R-6).

- [ ] **Step 1: Write the failing test**

```go
// clients/go/pando/migrate_test.go
package pando

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigrateCopiesChecksRenamesNeverDeletes(t *testing.T) {
	old := filepath.Join(t.TempDir(), "foreman")
	c, err := Open(context.Background(), Config{Dir: old})
	if err != nil {
		t.Fatal(err)
	}
	c.Append(context.Background(), "user", "keep me")
	c.Close()
	os.WriteFile(filepath.Join(old, "turns.jsonl"), []byte("{}\n"), 0o600)

	data := t.TempDir()
	rep, err := Migrate(old, data, "work", time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 1 || !strings.HasSuffix(rep.MovedTo, "foreman.moved-20261030T120000Z") {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old dir must be renamed, not left")
	}
	if _, err := os.Stat(filepath.Join(rep.MovedTo, "turns.jsonl")); err != nil {
		t.Fatal("chair files stay inside the moved dir")
	}
	dir, _ := StoreDir(data, "work")
	r, err := OpenReadOnly(dir)
	if err != nil || r.Count() != 1 {
		t.Fatalf("copy: %v %d", err, r.Count())
	}
}

func TestMigrateRefusesWhileOpen(t *testing.T) {
	old := filepath.Join(t.TempDir(), "foreman")
	c, _ := Open(context.Background(), Config{Dir: old})
	defer c.Close()
	_, err := Migrate(old, t.TempDir(), "work", time.Now())
	if err != ErrOpenElsewhere {
		t.Fatalf("want ErrOpenElsewhere, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run TestMigrate -v`
Expected: FAIL, `undefined: Migrate`.

- [ ] **Step 3: Implement `migrate.go`**

Probe the lock with the same non-blocking flock `lock_unix.go` uses; held means `ErrOpenElsewhere`. Copy files with `fsutil.go`'s atomic write, modes 0700 for dirs and 0600 for files. Check, then `os.Rename`. The CLI verb: `daisugi pando migrate [--from DIR] [--store work]`; `--from` defaults to the old rule (`$OPENDAISUGI_HOME/foreman`, else the data home's `foreman`), and the CLI prints the report, one line per file class.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd clients/go && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./pando/ -run TestMigrate -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add clients/go/pando/migrate.go clients/go/pando/migrate_test.go clients/go/internal/cli/pandocmd.go
git commit -m "daisugi pando migrate: copy the coppice foreman store into pando/work, check it, rename the old dir aside"
```

---

### Task 7: coppice (Go) links the package and keeps the chair

**Files:**
- Modify: `harness/coppice/go.mod` (`require daisugi-verify v0.0.0`, `replace daisugi-verify => ../../clients/go`)
- Delete: the files Tasks 1, 3 and 5 moved out of `harness/coppice/internal/foreman/` (they are gone after `git mv`; what remains is `turns.go`, `calls.go`, `failover.go`, `pool.go`, `poolcli.go`, `cli.go`, `tools.go` relay parts, and their tests)
- Modify: `harness/coppice/internal/foreman/cli.go` (`coppice foreman zoom|date|log|stats|import` call the package or print the `daisugi pando` command), `harness/coppice/internal/server/foreman_chat.go:223` (`pando.Open` with `pando.StoreDir(dataHome, cfg.Store)`), `foreman_tools.go` (relay `zoom` and `date` to `*pando.Chat`), `foreman_import.go` (use `pando.Scan`, `pando.Importer`), `harness/coppice/internal/config/config.go` (`Foreman.Store string`, default `work`, validated by `pando.ValidStore`, `all` refused)
- Modify: `harness/coppice/PROTOCOL.md` and `harness/coppice/README.md` (the store line; the import how-to moves to daisugi)
- Test: `harness/coppice/internal/server/foreman_store_test.go`

**Interfaces:**
- Consumes: everything Tasks 1 to 6 produce.
- Produces: `[foreman] store = "work"`; the server's `fc.chat` is a `*pando.Chat`; on start, if the old dir `LogDir(dataDir)` holds `main/` and no store dir exists, the server appends one `note` line to the floor's chat: `your conversation memory is in the old place. Run: daisugi pando migrate` and runs in pane mode until it is done. A `foreman.moved-*` dir triggers nothing. The chair files that `Migrate` kept are copied by coppice from the moved dir into `<coppice data dir>/foreman/` on the first start after a migration (`turns.jsonl`, `calls.jsonl`, `failover.json`, queue files), once, with a one-line note.

- [ ] **Step 1: Write the failing test**

```go
// harness/coppice/internal/server/foreman_store_test.go
package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOldForemanDirMakesOneNoteAndNoChat(t *testing.T) {
	h := newTestServer(t) // the existing helper with its own --data-dir and socket
	old := filepath.Join(h.dataDir, "foreman", "main")
	os.MkdirAll(old, 0o700)
	os.WriteFile(filepath.Join(old, "2026-10-01.jsonl"), []byte(`{"i":0,"kind":"user","text":"x","size":1,"date":"2026-10-01T00:00:00Z"}`+"\n"), 0o600)
	h.restartChatMode(t)
	chat := h.floorChat(t)
	if !strings.Contains(chat, "daisugi pando migrate") {
		t.Fatalf("want the migrate note, got %q", chat)
	}
	if h.foremanChatOpen() {
		t.Fatal("no store must be opened while the old dir stands")
	}
}

func TestStoreAllRefusedInConfig(t *testing.T) {
	_, err := parseForemanTable(`[foreman]
mode = "chat"
store = "all"`)
	if err == nil || !strings.Contains(err.Error(), "one store at a time") {
		t.Fatalf("got %v", err)
	}
}
```

(`newTestServer`, `restartChatMode`, `floorChat`, `foremanChatOpen`, `parseForemanTable`: use the helpers `foreman_chat_test.go` and `foreman_test.go` already have; add the two small ones that are missing beside them.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd harness/coppice && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./internal/server/ -run 'TestOldForemanDir|TestStoreAll' -v`
Expected: FAIL to compile (the moved symbols) or FAIL on the note.

- [ ] **Step 3: Rewire**

Add the `replace`. Replace `foreman.Open`, `foreman.Config`, `foreman.Chat`, `foreman.Scan`, `foreman.Importer`, `foreman.ModelSummarizer` with the `pando` names. The compactor loop in the server now calls `pando.Tend(ctx, chat, summarizer, 1)` per tick instead of its own node loop. `coppice foreman import` keeps its flags and calls `pando.Scan` and `Accept` on the configured store; its `--status`, `--pause`, `--resume` stay. Keep every existing `foreman_*_test.go` green; where a test reached a moved symbol, import `pando`.

- [ ] **Step 4: Run the coppice suites**

Run: `cd harness/coppice && systemd-run --user --scope -q -p MemoryMax=5G go test -p 1 ./...`
Expected: ok. Then `e2e/run.sh web` and `e2e/run.sh tui` on a scratch data dir under `~/opendaisugi-scratch/`, with `--socket` and `--data-dir` named.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/go.mod harness/coppice/go.sum harness/coppice/internal harness/coppice/PROTOCOL.md harness/coppice/README.md
git commit -m "coppice: link clients/go/pando for memory; [foreman] store; the migrate note; the chair stays"
```

---

### Task 8: The Rust `pando` crate, moved from coppice-rs

**Files:**
- Create: `clients/rust/pando/Cargo.toml` (crate `pando`, deps: `serde`, `serde_json` only), `clients/rust/pando/src/lib.rs`
- Move: `harness/coppice-rs/src/foreman/{chat,order,fsutil,compactor,import,ctx}.rs` and their tests, `golden_tests.rs`, `import_tests.rs`, `chat_tests.rs` to `clients/rust/pando/src/`
- Create: `clients/rust/pando/src/store.rs`, `recall.rs`, `mcp.rs`, `migrate.rs`, `import_web.rs`, `tend.rs`
- Modify: `harness/coppice-rs/Cargo.toml` (`pando = { path = "../../clients/rust/pando" }`), `clients/rust/Cargo.toml` (same path dep)
- Test: `clients/rust/pando/src/store_tests.rs`, and the moved tests

**Interfaces:**
- Consumes: the Go behaviour of Tasks 1 to 6 as the oracle, through `clients/fixtures/pando/golden` and the new fixtures Task 9 writes.
- Produces: the same functions as the Go package with Rust names: `store_dir(data_dir, store) -> Result<PathBuf, StoreError>`, `valid_store(&str) -> bool`, `list_stores`, `recall_lines`, `serve_mcp(chat, read, write)`, `migrate(from, data_dir, store, now) -> Result<Report, MigrateError>`, `scan_web`, `parse_map`, `tend(chat, summarizer, max)`.

- [ ] **Step 1: Write the failing test**

```rust
// clients/rust/pando/src/store_tests.rs
use super::store::{store_dir, valid_store, StoreError, DEFAULT_STORE};

#[test]
fn store_dir_rules_match_go() {
    let d = crate::chat_tests::temp_dir();
    assert_eq!(store_dir(&d, "work").unwrap(), d.join("pando").join("work"));
    assert!(matches!(store_dir(&d, "all"), Err(StoreError::All)));
    for bad in ["", "Work", "1x", "a/b", "a b"] {
        assert!(store_dir(&d, bad).is_err(), "{bad:?} must be refused");
    }
    assert!(!valid_store("x-very-long-name-that-goes-past-thirty-two-chars"));
    assert_eq!(DEFAULT_STORE, "work");
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd clients/rust && systemd-run --user --scope -q -p MemoryMax=5G env CARGO_BUILD_JOBS=2 cargo test -p pando store_dir_rules`
Expected: FAIL to compile, `store` module missing.

- [ ] **Step 3: Move and add**

`git mv` the modules; fix `use` paths; point `golden_tests.rs` at `../../fixtures/pando/golden` from the crate dir. Write the new modules as twins of the Go files, same messages byte for byte (`StoreError::All` displays the PD-R-2 text).

- [ ] **Step 4: Run the crate's tests**

Run: `cd clients/rust && systemd-run --user --scope -q -p MemoryMax=5G env CARGO_BUILD_JOBS=2 cargo test -p pando`
Expected: ok, golden tests included.

- [ ] **Step 5: Commit**

```bash
git add clients/rust/pando clients/rust/Cargo.toml clients/rust/Cargo.lock harness/coppice-rs/Cargo.toml harness/coppice-rs/src/foreman
git commit -m "pando (Rust): the store, compactor and importer move into clients/rust/pando; coppice-rs and daisugi-rs take it by path"
```

---

### Task 9: Rust `daisugi pando`, coppice-rs rewired, and the compare script

**Files:**
- Create: `clients/rust/src/cli/pandocmd.rs`
- Modify: `clients/rust/src/cli/mod.rs:693` (the verb match; add `"pando"`), `clients/rust/src/cli/helpall_gen.rs` through `clients/gen_help.py`
- Modify: `harness/coppice-rs/src/server/foreman_chat.rs`, `foreman_tools.rs`, `foreman_import.rs`, `src/config.rs` (the same changes Task 7 made in Go)
- Create: `clients/pando_cases.py` (runs the **Go** `daisugi pando` over invented stores and records exit code, stdout, stderr and the tree after, in the `garden_cases.py` style; `--out clients/fixtures/pando/cli`)
- Create: `clients/pando_compare.py` (`--binary PATH` replays the cases through any `daisugi`; `--oracle` reruns the Go one)
- Modify: `clients/ADJUDICATIONS.md` (a section `2026-xx-xx: Pando R2 moves into daisugi (PD-R-1 to PD-R-10)` recording the rulings of ADR-0023 and every disagreement found)
- Test: the compare run itself

**Interfaces:**
- Consumes: Task 8's crate; Task 2 to Task 6's verbs as the oracle.
- Produces: `daisugi-rs pando` with every verb of the Go one; `clients/fixtures/pando/cli/` cases; 0 disagreements.

- [ ] **Step 1: Write the cases from Go**

Run: `uv run --no-sync python clients/pando_cases.py --out clients/fixtures/pando/cli --binary clients/go/daisugi`
Expected: cases for `stores`, `status`, `log`, `zoom` (leaf, node, page, bad id), `date`, `view`, `recall`, `append`, `tend --dry-run`, `import --dry-run` (claude, codex, web), `import --propose`, `import --accept`, `migrate` (fresh, already moved, open elsewhere), `--store all`, `--store home` missing. Content invented; paths written `{HOME}`.

- [ ] **Step 2: Run the compare on Rust to see it fail**

Run: `uv run --no-sync python clients/pando_compare.py --binary clients/rust/target/release/daisugi`
Expected: disagreements on every case (`pando is not in this binary yet.`).

- [ ] **Step 3: Implement `pandocmd.rs` and rewire coppice-rs**

Same verbs, flags, messages and exit codes as Go. coppice-rs: `[foreman] store`, `pando::Chat` in the server, the migrate note, chair files copied once.

- [ ] **Step 4: Compare to zero**

Run: `uv run --no-sync python clients/pando_compare.py --binary clients/rust/target/release/daisugi` then `cd harness/coppice-rs && systemd-run --user --scope -q -p MemoryMax=5G env CARGO_BUILD_JOBS=2 cargo test` and `uv run --no-sync python clients/coppice_compare.py --go harness/coppice/coppice --rust harness/coppice-rs/target/release/coppice --suite cases`
Expected: 0 disagreements; coppice suites green on both binaries.

- [ ] **Step 5: Commit**

```bash
git add clients/rust/src/cli clients/pando_cases.py clients/pando_compare.py clients/fixtures/pando/cli clients/ADJUDICATIONS.md harness/coppice-rs
git commit -m "daisugi-rs pando and coppice-rs on the pando crate; Go-written cases and the compare script agree"
```

---

### Task 10: The Python reader and its MCP server

**Files:**
- Create: `src/opendaisugi/pando/__init__.py`, `store.py`, `recall.py`, `mcp.py`
- Modify: `src/opendaisugi/cli.py` (a `pando_app` Typer group beside `journal_app` at line 178: `stores`, `status`, `log`, `zoom`, `date`, `view`, `recall`, `serve`; `append`, `tend`, `import`, `migrate` print the PD-R-10 refusal and exit 2)
- Modify: `src/opendaisugi/datahome.py` (nothing; `data_home()` is the rule the reader uses)
- Test: `tests/test_pando_store.py`, `tests/test_pando_mcp.py`

**Interfaces:**
- Consumes: the file format as the Go package writes it; `clients/fixtures/pando/golden` as test data; `opendaisugi._search.effective_matcher` and the lexical embedder for `recall`.
- Produces:
  - `store_dir(data_dir: Path, store: str) -> Path` (raises `ValueError` with the PD-R-1 and PD-R-2 texts)
  - `class PandoStore` with `open(path: Path) -> PandoStore` (read-only; raises `FileNotFoundError` with `no store <name> at <path>`), `count: int`, `message(i: int) -> Message`, `date(i: int) -> str`, `day(day: str) -> list[Message]`, `zoom(id: int, n: int, offset: int = 0, size: int = 0) -> str`, `view_lines() -> list[tuple[str, str]]`, `recall(query: str, k: int = 8) -> list[RecallHit]`
  - `build_pando_server(store: PandoStore) -> FastMCP` named `pando` with tools `zoom`, `date`, `recall`.
  - `@dataclass(frozen=True) class Message: i: int; kind: str; text: str; size: int; date: str; act: str | None`

- [ ] **Step 1: Write the failing test**

```python
# tests/test_pando_store.py
from pathlib import Path

import pytest

from opendaisugi.pando import PandoStore, store_dir

GOLDEN = Path(__file__).resolve().parents[1] / "clients" / "fixtures" / "pando" / "golden"


def test_store_dir_rules(tmp_path):
    assert store_dir(tmp_path, "work") == tmp_path / "pando" / "work"
    with pytest.raises(ValueError, match="one store at a time"):
        store_dir(tmp_path, "all")
    for bad in ["", "Work", "1x", "a/b"]:
        with pytest.raises(ValueError):
            store_dir(tmp_path, bad)


def test_reads_a_golden_store_like_go():
    case = sorted(p for p in GOLDEN.iterdir() if (p / "out").is_dir())[0]
    s = PandoStore.open(case / "out")
    assert s.count > 0
    m = s.message(0)
    assert m.i == 0 and m.date.endswith("Z")
    assert s.zoom(0, 1) == m.text
    assert s.date(0) == m.date
    assert s.view_lines()  # the saved view.json, not a rebuild


def test_python_never_writes(tmp_path):
    s = PandoStore.open  # the class has no append
    assert not hasattr(PandoStore, "append")
```

```python
# tests/test_pando_mcp.py
import asyncio
import json
from pathlib import Path

from opendaisugi.pando import PandoStore
from opendaisugi.pando.mcp import build_pando_server

GOLDEN = Path(__file__).resolve().parents[1] / "clients" / "fixtures" / "pando" / "golden"


def test_server_has_three_read_tools_and_zooms():
    case = sorted(p for p in GOLDEN.iterdir() if (p / "out").is_dir())[0]
    mcp = build_pando_server(PandoStore.open(case / "out"))
    assert mcp.name == "pando"
    names = {t.name for t in asyncio.run(mcp.list_tools())}
    assert names == {"zoom", "date", "recall"}
    out = asyncio.run(mcp.call_tool("zoom", {"id": 0, "n": 1}))
    assert PandoStore.open(case / "out").message(0).text in json.dumps(out, default=str)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_pando_store.py tests/test_pando_mcp.py -v`
Expected: FAIL, `ModuleNotFoundError: opendaisugi.pando`.

- [ ] **Step 3: Implement**

`store.py` reads `main/*.jsonl` by day (skip a torn last line as `ReadTurns` does, never repair), `tree/*.jsonl`, `view.json`; `zoom` follows `ZoomPage` in `chat.go:1130` (a leaf returns the text; a node returns its two halves as lines; a bad id returns the root hint; `offset` and `size` page at `ZoomPageBytes`). `recall.py` embeds with the garden's effective matcher and ranks as Task 2 does. `mcp.py` follows `mcp_server.build_server` with `FastMCP(name="pando")`. Add `opendaisugi.pando` to nothing in `tests/test_layer_boundary.py`: it is part of daisugi and imports nothing above.

- [ ] **Step 4: Run tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_pando_store.py tests/test_pando_mcp.py tests/test_layer_boundary.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/pando src/opendaisugi/cli.py tests/test_pando_store.py tests/test_pando_mcp.py
git commit -m "opendaisugi.pando: a read-only Python client and MCP server over pando stores (PD-R-10)"
```

---

### Task 11: Docs

**Files:**
- Modify: `docs/how-to/import-old-chats.md` (the commands become `daisugi pando import ...`; coppice's bar still offers it and runs the same code; the `--map`, `--propose`, `--accept` and `web` sections)
- Create: `docs/how-to/pando.md` (the store, two stores, the verbs, `serve` in `~/.claude.json`, `tend` from cron, `migrate`)
- Modify: `docs/how-to/bring-your-own-foreman.md` (a harness with no memory of its own can mount `daisugi pando serve --store work` and get `zoom`, `date`, `recall`)
- Modify: `AGENTS.md` (the parts table: `pando` rows for Go, Rust and Python; the binaries table; the `src/opendaisugi/` concern table: "Memory and reuse" gains `pando/`)
- Modify: `docs/adr/README.md` (rows 0023 and 0024), `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md` (a row: PD, the parity exception, owner to confirm), `docs/plans/2026-09-24-omarchy/design-foreman.md` (one line under M1: moved to daisugi, ADR-0023)
- Test: `uv run --no-sync pytest tests/test_docs*.py -q` (the doc tests that check command names exist)

- [ ] **Step 1: Write the pages**

Each page in STE. Every command shown must exist in Task 2 to Task 10 with the flags written.

- [ ] **Step 2: Check the commands exist**

Run: `for c in stores status log zoom date view recall tend import migrate serve; do clients/go/daisugi pando $c --help >/dev/null || echo "missing $c"; done`
Expected: no `missing` lines.

- [ ] **Step 3: Commit**

```bash
git add docs/how-to/import-old-chats.md docs/how-to/pando.md docs/how-to/bring-your-own-foreman.md AGENTS.md docs/adr/README.md docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md docs/plans/2026-09-24-omarchy/design-foreman.md
git commit -m "docs: pando how-to, import how-to on daisugi pando import, AGENTS and ADR index rows"
```

---

## If the owner keeps full parity (Python oracle): Tasks P-A to P-C

Add these before Task 9, and change Task 9 so `clients/pando_cases.py` runs the Python `daisugi pando` as the writer of cases.

### Task P-A: The Python writer

**Files:** `src/opendaisugi/pando/order.py` (NodeID, `most_due`, the planner heap: a port of `order.go`), `src/opendaisugi/pando/writer.py` (`PandoWriter.open(path)`: the lock, `append`, `append_at`, `append_act`, the split at 32 KiB, the torn-tail repair, `view_step`, `save_view`), `tests/test_pando_writer.py`, `tests/test_pando_order.py`.
**Test to pin:** the merge order equals the rollback push list for t = 0 to 20,000 (the test `order_test.go` has); the golden fixtures replayed by Python produce the same bytes under `out/` as Go.
**Size:** about 1,400 lines plus 1,500 of tests; 2 to 3 sessions.

### Task P-B: The Python compactor and `tend`

**Files:** `src/opendaisugi/pando/compactor.py` (`ModelSummarizer` over `llm.py` backends with the two prompt files, the ruler, the "too long" retry at most 5, the strike list), `tend` in `cli.py`; `tests/test_pando_compactor.py` with a fake summarizer and the fake model server `clients/fake_proxy.py` uses.
**Size:** about 400 lines plus 500 of tests; 1 session.

### Task P-C: The Python importer

**Files:** `src/opendaisugi/pando/imports.py` (Claude Code reader on `claude_transcript.py`, Codex rollout reader on `parsers/`, web reader, `--map`, propose and accept, the import state file); `tests/test_pando_import.py` over `clients/fixtures/pando/transcripts`.
**Size:** about 1,100 lines plus 1,000 of tests; 1 to 2 sessions.

---

## Self-review notes

- Spec coverage: ADR-0023 decisions 1 to 9 map to Tasks 1 to 10; rulings PD-R-1 to PD-R-10 each have a test (PD-R-1, 2: Task 1 and 2; PD-R-3: the package paths; PD-R-4: Task 2's lock test; PD-R-5: Task 2's recall test; PD-R-6: Task 6; PD-R-7, 8, 9: Task 5; PD-R-10: Task 10).
- Not in this plan: the `home` chat in coppice's UI (a second chat window) and the work-order message from home to work. They need their own design; the two stores exist after this plan so that design has somewhere to write.
- Open with the owner: see ADR-0023, "Parity", and the rulings list.
