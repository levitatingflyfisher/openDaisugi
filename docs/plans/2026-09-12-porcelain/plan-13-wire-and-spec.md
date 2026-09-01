# Plan 13: The Wire and the Spec Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The coppice socket protocol is a published, versioned spec with a conformance corpus any client can run; it accepts JSON-RPC 2.0 framing beside the existing JSONL envelope; it gains per-pane flow control and a `pane.fork` verb.

**Architecture:** The existing envelope (`{"id","cmd",...}` and `{"event",...}`) stays the native form. A framing adapter in `internal/proto` recognises a line carrying `"jsonrpc":"2.0"`, maps `method` to `cmd` and flattens `params`, and answers in JSON-RPC form on that connection for as long as it lives. Events become JSON-RPC notifications on such a connection. Flow control is two verbs, `events.pause` and `events.resume`, per pane, with a bounded buffer and a `frame_gap` event when frames are dropped. Fork is a verb that asks the adapter for a resume command. The spec is a Markdown file plus a corpus of request and expected-response pairs that the Go tests and the Python tests both replay.

**Tech Stack:** Go 1.26, Python 3.12, JSON.

**Spec:** ROADMAP plan 13; master spec §3.3; research report recommendation "Protocol"; `docs/research/boards/floor/index.html` "one socket".

## Global Constraints

Same as plan 12. In addition: no breaking change to the native envelope; every existing client test must pass untouched.

---

### Task 1: The protocol document and the conformance corpus

**Files:**
- Create: `harness/coppice/PROTOCOL.md`
- Create: `harness/coppice/testdata/protocol/README.md`
- Create: `harness/coppice/testdata/protocol/pane-create.jsonl` (first corpus file; one request line, one expected response line, alternating)
- Create: `harness/coppice/internal/proto/conformance_test.go`
- Test: the conformance test itself

**Interfaces:**
- Produces: corpus format. Odd lines are requests, even lines are expected responses. An expected response may use `"*"` for any value and `"$id"` to mean the request's id. A response line may start with `#skip` to mark a case the server does not implement yet, which counts as a listed gap, never a pass.

- [ ] **Step 1: Write the failing test**

```go
func TestConformanceCorpusReplays(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/protocol/*.jsonl")
	if len(files) == 0 {
		t.Fatal("no corpus files, the test would prove nothing")
	}
	s := newConformanceServer(t)
	for _, f := range files {
		replayCorpus(t, s, f)
	}
}
```

`replayCorpus` sends each request, decodes the response, and compares with the wildcard rules above. Wildcard matching lives in `conformance_match.go` so the Python side can copy its rules.

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd harness/coppice && go test -p 1 ./internal/proto -run TestConformanceCorpusReplays -v`
Expected: FAIL, no corpus files.

- [ ] **Step 3: Write the corpus and the document**

`pane-create.jsonl`:

```
{"id":"1","cmd":"server.status"}
{"id":"$id","ok":true,"result":{"pid":"*","socket":"*","panes":"*"}}
{"id":"2","cmd":"pane.create","cwd":"/","cmd_argv":["sh","-c","sleep 30"],"label":"c","kind":"pty"}
{"id":"$id","ok":true,"result":{"pane":"*"}}
{"id":"3","cmd":"pane.create","cwd":"/","cmd_argv":["no-such-harness-xyz"],"label":"g","kind":"pty"}
{"id":"$id","ok":false,"error":{"code":"spawn_failed","message":"*"}}
{"id":"4","cmd":"pane.list"}
{"id":"$id","ok":true,"result":{"panes":"*"}}
```

`PROTOCOL.md` sections: transport and auth (uid-checked unix socket, 0600); framing (native JSONL; JSON-RPC 2.0 accepted, see Task 2); the closed error enum (copy from `proto.go`); verbs, one table row each with params and result, taken from `RegisterPaneCommands`, `RegisterAttachCommands`, and the agent verbs; events (`frame`, `state`, `frame_gap` from Task 3, `tool` from plan 19); ids are opaque strings, stable for the life of the server's data dir; versioning: `PROTOCOL.md` carries `Version: 1`, and `server.status` returns `protocol: 1`.

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./internal/proto`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/PROTOCOL.md harness/coppice/testdata/protocol harness/coppice/internal/proto/conformance_test.go harness/coppice/internal/proto/conformance_match.go
git commit -m "coppice: the socket protocol as a document and a replayable corpus"
```

---

### Task 2: JSON-RPC 2.0 framing on the same socket

**Files:**
- Create: `harness/coppice/internal/proto/jsonrpc.go`
- Modify: `harness/coppice/internal/server/server.go` (the read loop near line 612 and the response writer; a per-client `framing` flag)
- Test: `harness/coppice/internal/proto/jsonrpc_test.go`, `harness/coppice/internal/server/jsonrpc_conn_test.go`

**Interfaces:**
- Produces: `proto.DecodeLine(b []byte) (Request, Framing, error)` where `Framing` is `Native` or `JSONRPC`; `proto.EncodeResponse(r Response, f Framing) []byte`; `proto.EncodeEvent(ev map[string]any, f Framing) []byte`. Method names map one to one: JSON-RPC `method` is the native `cmd`. Errors map to `{"code":-32000,"message":<message>,"data":{"code":"<native code>"}}`. Invalid JSON answers `-32700`. Unknown method answers `-32601` with `data.code = "bad_request"`.

- [ ] **Step 1: Write the failing tests**

```go
func TestDecodeJSONRPCRequestMapsMethodToCmd(t *testing.T) {
	r, f, err := DecodeLine([]byte(`{"jsonrpc":"2.0","id":7,"method":"pane.read","params":{"pane":"w1:p1","lines":3}}`))
	if err != nil || f != JSONRPC || r.Cmd != "pane.read" || r.ID != "7" {
		t.Fatalf("got %+v %v %v", r, f, err)
	}
	if n, _ := r.Int("lines"); n != 3 {
		t.Fatal("params not flattened")
	}
}

func TestEncodeErrorInJSONRPCForm(t *testing.T) {
	b := EncodeResponse(ErrResp("7", ErrNoSuchPane, "no pane w9"), JSONRPC)
	if !strings.Contains(string(b), `"code":-32000`) || !strings.Contains(string(b), `"no_such_pane"`) {
		t.Fatalf("bad encoding: %s", b)
	}
}
```

Server-level: open a socket client, send one JSON-RPC line, expect a JSON-RPC response, then subscribe and expect an event with `"method":"state"` rather than `"event":"state"`.

- [ ] **Step 2: Run them to make sure they fail**

Run: `cd harness/coppice && go test -p 1 ./internal/proto ./internal/server -run 'JSONRPC' -v`
Expected: FAIL, symbols undefined.

- [ ] **Step 3: Implement**

`jsonrpc.go` holds the two shapes and the mapping. In the server read loop, replace the direct `json.Unmarshal` with `proto.DecodeLine`; store the framing on the `Client`; route every write through `EncodeResponse` and `EncodeEvent` with the client's framing. Numeric JSON-RPC ids are stringified for the native `Request.ID` and restored on the way out.

- [ ] **Step 4: Run the tests, then the whole tree**

Run: `cd harness/coppice && go test -p 1 ./...`
Expected: PASS, including every pre-existing client test.

- [ ] **Step 5: Add a JSON-RPC corpus file and commit**

Create `testdata/protocol/jsonrpc.jsonl` with the same four cases in JSON-RPC form. Then:

```bash
git add harness/coppice/internal/proto/jsonrpc.go harness/coppice/internal/proto/jsonrpc_test.go harness/coppice/internal/server/server.go harness/coppice/internal/server/jsonrpc_conn_test.go harness/coppice/testdata/protocol/jsonrpc.jsonl harness/coppice/PROTOCOL.md
git commit -m "coppice: accept JSON-RPC 2.0 framing on the socket beside the native envelope"
```

---

### Task 3: Per-pane flow control

**Files:**
- Modify: `harness/coppice/internal/server/attach.go` (`framePump`)
- Modify: `harness/coppice/internal/server/panes.go` (`RegisterPaneCommands`: `events.pause`, `events.resume`)
- Test: `harness/coppice/internal/server/flow_test.go`

**Interfaces:**
- Produces: `events.pause {pane}` stops frames for that pane on this connection; `events.resume {pane}` sends one full frame then resumes deltas. A connection that does not drain within the buffer of 64 frames gets frames dropped and one `{"event":"frame_gap","pane":..,"dropped":N}` when it catches up.

- [ ] **Step 1: Write the failing test**

```go
func TestPausedPaneSendsNoFramesThenAFullFrameOnResume(t *testing.T) {
	s, c := newAttachedClient(t) // helper: server, client attached to a chatty pane
	c.send(`{"id":"p","cmd":"events.pause","pane":"` + c.pane + `"}`)
	c.expectOK("p")
	c.expectNoEvent("frame", 500*time.Millisecond)
	c.send(`{"id":"r","cmd":"events.resume","pane":"` + c.pane + `"}`)
	fr := c.expectEvent("frame")
	if fr["full"] != true {
		t.Fatalf("resume did not send a full frame: %v", fr)
	}
}

func TestASlowClientGetsAFrameGapNotAStall(t *testing.T) {
	s, c := newAttachedClient(t)
	c.stopReading()
	produceFrames(t, s, c.pane, 200)
	c.resumeReading()
	gap := c.expectEvent("frame_gap")
	if gap["dropped"].(float64) < 1 {
		t.Fatal("no drop reported")
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `cd harness/coppice && go test -p 1 ./internal/server -run 'TestPausedPane|TestASlowClient' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Give each attached pane on a client a `paused bool` and a bounded channel of 64 frames. `framePump` selects with a default that increments `dropped` when the channel is full. When a send succeeds after drops, emit `frame_gap` first. Resume sets a `wantFull` flag that the next pump iteration honours by sending the whole grid.

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./internal/server`
Expected: PASS.

- [ ] **Step 5: Document and commit**

Add the two verbs and the `frame_gap` event to `PROTOCOL.md` and a corpus file `testdata/protocol/flow.jsonl`. Then:

```bash
git add harness/coppice/internal/server/attach.go harness/coppice/internal/server/panes.go harness/coppice/internal/server/flow_test.go harness/coppice/PROTOCOL.md harness/coppice/testdata/protocol/flow.jsonl
git commit -m "coppice: per-pane flow control with an honest frame_gap"
```

---

### Task 4: The fork verb

**Files:**
- Modify: `harness/coppice/internal/pane/adapter.go` (optional interface `Forker`)
- Modify: `harness/coppice/internal/adapters/claude/claude.go` (implement `Forker`: `claude --resume <session> --fork-session`)
- Modify: `harness/coppice/internal/adapters/pi/pi.go` (returns `ErrAdapter` "fork not built" until plan 04)
- Modify: `harness/coppice/internal/server/panes.go` (`pane.fork`)
- Test: `harness/coppice/internal/server/fork_test.go`

**Interfaces:**
- Produces: `pane.fork {pane, label?}` returns `{pane: <new id>}`; the new pane records `parent_pane`. An adapter without `Forker` answers `adapter_error` with `"<harness> cannot fork a session yet"`.

- [ ] **Step 1: Write the failing test**

```go
func TestForkOfAShellPaneIsRefusedWithATeachingError(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	resp := call(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	if resp.OK || resp.Error.Code != proto.ErrAdapter || !strings.Contains(resp.Error.Message, "cannot fork") {
		t.Fatalf("got %+v", resp)
	}
}

func TestForkUsesTheAdaptersResumeCommand(t *testing.T) {
	s := newTestServer(t)
	adapters.Register(fakeForker{name: "fakeh"}) // Fork returns argv ["sh","-c","sleep 30"]
	id := createHarnessPane(t, s, "fakeh")
	resp := call(t, s, `{"id":"f","cmd":"pane.fork","pane":"`+id+`"}`)
	newID := resp.Result["pane"].(string)
	row := rowFor(t, listPanes(t, s), newID)
	if row["parent_pane"] != id {
		t.Fatalf("parent not recorded: %v", row)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `cd harness/coppice && go test -p 1 ./internal/server -run 'TestFork' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
type Forker interface {
	ForkArgv(sessionID string) ([]string, error)
}
```

`pane.fork` looks up the live pane's adapter, type-asserts `Forker`, builds a new `layout.Pane` with `ParentPane` set and the same cwd, and calls `startPane`. The claude adapter's `ForkArgv` returns `[]string{"claude", "--resume", sessionID, "--fork-session"}` and needs the harness session id it already parses from the hook events.

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./...`
Expected: PASS.

- [ ] **Step 5: Document and commit**

Add `pane.fork` to `PROTOCOL.md` and `testdata/protocol/fork.jsonl` with the refusal case. Then:

```bash
git add harness/coppice/internal/pane/adapter.go harness/coppice/internal/adapters/claude/claude.go harness/coppice/internal/adapters/pi/pi.go harness/coppice/internal/server/panes.go harness/coppice/internal/server/fork_test.go harness/coppice/PROTOCOL.md harness/coppice/testdata/protocol/fork.jsonl
git commit -m "coppice: pane.fork asks the adapter for a resume command"
```

---

### Task 5: The Python client replays the same corpus

**Files:**
- Create: `tests/floor/test_protocol_corpus.py`
- Modify: `src/opendaisugi/floor/coppice_backend.py` only if a mismatch is found

- [ ] **Step 1: Write the test**

```python
CORPUS = Path("harness/coppice/testdata/protocol")


@pytest.mark.skipif(not coppice_binary_available(), reason="coppice not built")
@pytest.mark.parametrize("path", sorted(CORPUS.glob("*.jsonl")))
def test_corpus_replays_through_the_python_client(path, coppice_sandbox):
    lines = path.read_text().splitlines()
    for req, want in zip(lines[0::2], lines[1::2]):
        if want.startswith("#skip"):
            continue
        got = coppice_sandbox.raw(json.loads(req))
        assert matches(json.loads(want), got, req_id=json.loads(req).get("id"))
```

`matches` copies the wildcard rules from `conformance_match.go`, in `tests/floor/protocol_match.py`.

- [ ] **Step 2: Run it**

Run: `uv run --no-sync pytest -q tests/floor/test_protocol_corpus.py`
Expected: PASS when the binary is built, SKIP when it is not. A failure is a real mismatch between the two clients and is fixed in the backend, never in the corpus.

- [ ] **Step 3: Commit**

```bash
git add tests/floor/test_protocol_corpus.py tests/floor/protocol_match.py
git commit -m "floor: the python client replays the coppice protocol corpus"
```
