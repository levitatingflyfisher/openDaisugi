package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
	"github.com/opendaisugi/coppice/internal/testhome"
	"github.com/opendaisugi/coppice/skills"
)

// fakeSprigTurnScript stands in for sprig on a foreman turn. A call without
// --mcp-config is a compactor call, and goes to the shell fake. A turn call
// logs its argv, the mode and size of its system file, the size of
// descriptor 3 and whether it holds FAKE_SENTINEL (never the value), saves
// the system prompt and stdin, starts the MCP server its --mcp-config names,
// runs the calls in FAKE_TURN_SCRIPT through it, and prints sprig's --json.
// FAKE_TURN_MODE: ok (default), exit1, hang.
const fakeSprigTurnScript = `#!/usr/bin/python3
import json, os, subprocess, sys, time
argv = sys.argv[1:]
if "--mcp-config" not in argv:
    os.execv("/bin/sh", ["sh", os.environ["FAKE_SPRIG_SHELL"]] + argv)
log = open(os.environ["FAKE_LOG"], "a")
def say(*a):
    print(*a, file=log, flush=True)
def after(flag):
    return argv[argv.index(flag) + 1]
say("sprig argv:", json.dumps(argv))
if "--key-fd" in argv:
    key = os.fdopen(int(after("--key-fd")), "rb").read().decode()
    say("fd3: %d bytes" % len(key))
    import hashlib
    ok = hashlib.sha256(key.encode()).hexdigest() == os.environ.get("FAKE_SENTINEL_SHA")
    say("key: " + ("matches" if ok else "differs"))
say("sprig env ambient: " + ",".join(sorted(k for k in os.environ if k in ("ANTHROPIC_BASE_URL", "SPRIG_MODEL", "SPRIG_BACKEND", "AWS_SECRET_ACCESS_KEY", "OPENAI_API_KEY", "SOME_TOOL_TOKEN", "DESK_API_KEY", "ZZ_RANDOM_NAME"))))
say("sprig env holds a key: %s" % any(__import__("hashlib").sha256(v.encode()).hexdigest() == os.environ.get("FAKE_SENTINEL_SHA") for v in os.environ.values()))
sys_path = after("--system-file")
sysb = open(sys_path, "rb").read()
say("system mode: %o" % (os.stat(sys_path).st_mode & 0o777))
say("system bytes: %d" % len(sysb))
stdin = sys.stdin.buffer.read()
d = os.environ.get("FAKE_STDIN_DIR")
if d:
    n = len([x for x in os.listdir(d) if x.startswith("stdin-")]) + 1
    open(os.path.join(d, "sys-%d.txt" % n), "wb").write(sysb)
    open(os.path.join(d, "stdin-%d.txt" % n), "wb").write(stdin)
open(os.environ.get("FAKE_PIDFILE", os.devnull), "a").write("%d\n" % os.getpid())
mode = os.environ.get("FAKE_TURN_MODE", "ok")
if mode == "exit1":
    sys.stderr.write("sprig: the model said no\n")
    sys.exit(1)
if mode == "hang":
    time.sleep(60)
cfg = json.loads(after("--mcp-config"))["mcpServers"]["foreman"]
reply = os.environ.get("FAKE_TURN_REPLY") or "the foreman says hi"
calls = json.loads(os.environ.get("FAKE_TURN_SCRIPT") or "[]")
srv = subprocess.Popen([cfg["command"]] + cfg["args"], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
def rpc(i, method, params=None):
    msg = {"jsonrpc": "2.0", "id": i, "method": method}
    if params is not None:
        msg["params"] = params
    srv.stdin.write((json.dumps(msg) + "\n").encode())
    srv.stdin.flush()
    return json.loads(srv.stdout.readline())
rpc(1, "initialize", {})
tools = rpc(2, "tools/list")["result"]["tools"]
say("sprig tools:", ",".join(t["name"] for t in tools))
for k, c in enumerate(calls):
    r = rpc(10 + k, "tools/call", {"name": c["name"], "arguments": c.get("args", {})})["result"]
    say("sprig call %s: error=%s" % (c["name"], r["isError"]))
srv.stdin.close()
srv.wait()
print(json.dumps({"answer": reply, "turns": 2, "model_turns": 1, "model": "fake",
    "usage": {"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 900, "cache_creation_input_tokens": 70}}))
`

// sprigTurnServer is a chat-mode server whose turns run the sprig runner
// on the profile desk, with the fake sprig above.
type sprigTurnServer struct {
	*turnServer
}

const turnSentinel = "sk-SENTINEL-turn"

func newSprigTurnServer(t *testing.T, tweak func(*ForemanConfig)) *sprigTurnServer {
	t.Helper()
	pre := func(f *ForemanConfig) {
		f.TurnProfile = "desk"
		if tweak != nil {
			tweak(f)
		}
	}
	ts := newTurnServer(t, pre)
	shell := os.Getenv("COPPICE_SPRIG_BIN")
	turnFake := filepath.Join(ts.e.dir, "fake-sprig-turn")
	if err := os.WriteFile(turnFake, []byte(fakeSprigTurnScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SPRIG_SHELL", shell)
	t.Setenv("COPPICE_SPRIG_BIN", turnFake)
	setSentinel(t, turnSentinel)
	// The key is in coppice's environment, as key_env puts it there, under
	// that name and under a name nothing would guess; and the ambient
	// settings of a proxy, a model and another cloud.
	t.Setenv("DESK_API_KEY", turnSentinel)
	t.Setenv("ZZ_RANDOM_NAME", turnSentinel)
	t.Setenv("OPENAI_API_KEY", "sk-other-openai")
	t.Setenv("SOME_TOOL_TOKEN", "tok-other")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-other")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9/proxy")
	t.Setenv("SPRIG_MODEL", "ambient-model")
	t.Setenv("SPRIG_BACKEND", "api")
	ts.e.requireScratch(t)
	return &sprigTurnServer{ts}
}

// sprigArgv is the argv line the fake logged for the n-th turn call.
func (ts *sprigTurnServer) sprigArgv(t *testing.T) []string {
	t.Helper()
	var line string
	for _, l := range strings.Split(read(ts.e.log), "\n") {
		if p, ok := strings.CutPrefix(l, "sprig argv: "); ok {
			line = p
		}
	}
	var argv []string
	if err := json.Unmarshal([]byte(line), &argv); err != nil {
		t.Fatalf("argv line %q: %v", line, err)
	}
	return argv
}

func TestTheSprigTurnArgvIsExactAndTheSystemFileHoldsThePageAndTheView(t *testing.T) {
	ts := newSprigTurnServer(t, nil)
	t.Setenv("FAKE_TURN_REPLY", "Hello from the desk.")
	ts.say(t, "short words")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	argv := ts.sprigArgv(t)
	want := []string{"--wire", "openai", "--base-url", "http://127.0.0.1:1/v1", "--model", "desk-model", "--key-fd", "3",
		"--tools", "none", "--mcp-config", mcpConfigJSON(ts.turns.self, filepath.Join(ts.turnDir(), "t-0.sock")),
		"--system-file", filepath.Join(ts.turnDir(), "t-0.system"), "--json", "--max-turns", "8", "-"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv =\n%q\nwant\n%q", argv, want)
	}
	log := read(ts.e.log)
	for _, w := range []string{"system mode: 600", "key: matches", "fd3: " + fmt.Sprint(len(turnSentinel)) + " bytes",
		"sprig tools: zoom,date,spawn,status"} {
		if !strings.Contains(log, w) {
			t.Fatalf("log lacks %q:\n%s", w, log)
		}
	}
	// The system prompt is the page and the frozen view, the same bytes the
	// claude runner gets; the stdin is the tail, state and message.
	sys, in := read(filepath.Join(ts.stdins, "sys-1.txt")), read(filepath.Join(ts.stdins, "stdin-1.txt"))
	if sys != claudeSystem(nil) {
		t.Fatalf("system:\n%s", sys)
	}
	if !strings.HasPrefix(in, "<tail>\n</tail>\n<state>\nnow: 20") || !strings.HasSuffix(in, "<message>\nshort words\n</message>\n") || strings.Contains(in, "<chat>") {
		t.Fatalf("stdin:\n%s", in)
	}
	// The turn leaves nothing in the turn dir but the compactor's dir.
	ents, _ := os.ReadDir(ts.turnDir())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "t-") {
			t.Fatalf("%s is still in the turn dir", e.Name())
		}
	}
	msgs := ts.logMessages(t)
	if last := msgs[len(msgs)-1]; last.Kind != foreman.KindForeman || last.Text != "Hello from the desk." {
		t.Fatalf("log = %+v", msgs)
	}
	recs, _ := foreman.ReadTurns(ts.logDir)
	if len(recs) != 2 || recs[1].Status != "ok" || recs[1].Usage == nil || recs[1].Usage.CacheRead != 900 ||
		recs[1].Usage.Input != 10 || recs[1].Usage.Output != 20 || recs[1].Usage.CacheWrite != 70 {
		t.Fatalf("turns.jsonl = %+v", recs)
	}
}

func TestTheKeyReachesNoFileNoArgvAndNoRecord(t *testing.T) {
	ts := newSprigTurnServer(t, nil)
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	if hits := grepTree([]string{ts.logDir, ts.turnDir(), ts.e.dir}, turnSentinel); len(hits) != 0 {
		t.Fatalf("the key reached %v", hits)
	}
	// sprig and its MCP child get no key-like variable and no value that is
	// the key, though key_env put the key in coppice's own environment.
	log := read(ts.e.log)
	for _, want := range []string{"sprig env ambient: \n", "sprig env holds a key: False", "mcp env keyvars: \n", "mcp env ambient: \n", "mcp env holds a key: false"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
	for _, n := range noteTexts(ts.Server) {
		if strings.Contains(n, turnSentinel) {
			t.Fatalf("a note holds the key: %q", n)
		}
	}
}

func TestTurnModelBeatsTheProfilesModel(t *testing.T) {
	ts := newSprigTurnServer(t, func(f *ForemanConfig) { f.TurnModel = "pinned" })
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	argv := ts.sprigArgv(t)
	for i, a := range argv {
		if a == "--model" && argv[i+1] != "pinned" {
			t.Fatalf("model = %q", argv[i+1])
		}
	}
	if !strings.Contains(strings.Join(argv, " "), "--model pinned") {
		t.Fatalf("argv = %q", argv)
	}
}

func TestAProfileWithNoKeyGetsNoKeyFd(t *testing.T) {
	ts := newSprigTurnServer(t, func(f *ForemanConfig) { f.TurnProfile = "free" })
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	argv := strings.Join(ts.sprigArgv(t), " ")
	if strings.Contains(argv, "--key-fd") || !strings.Contains(argv, "--base-url http://127.0.0.1:2/v1") || !strings.Contains(argv, "--model free-model") {
		t.Fatalf("argv = %s", argv)
	}
}

func TestAProfileThatIsNotThereIsOneNoteNamingItAndNothingRuns(t *testing.T) {
	ts := newSprigTurnServer(t, func(f *ForemanConfig) { f.TurnProfile = "ghost" })
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	if strings.Contains(read(ts.e.log), "sprig argv:") {
		t.Fatalf("sprig ran:\n%s", read(ts.e.log))
	}
	var notes []string
	for _, m := range ts.logMessages(t) {
		if m.Kind == foreman.KindNote {
			notes = append(notes, m.Text)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"ghost"`) || !strings.Contains(notes[0], "turn failed") {
		t.Fatalf("notes = %q", notes)
	}
	recs, _ := foreman.ReadTurns(ts.logDir)
	if recs[len(recs)-1].Status != foreman.TurnFailed {
		t.Fatalf("turns = %+v", recs)
	}
}

func TestAnEmptyTurnProfileKeepsTheClaudeRunnerAndReadsNoBundle(t *testing.T) {
	ts := newTurnServer(t, nil)
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	log := read(ts.e.log)
	if !strings.Contains(log, "claude argv:") || strings.Contains(log, "sprig argv:") || strings.Contains(log, "agent-profiles") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestTheToolsReachTheModelThroughSprigAndEachCallIsLogged(t *testing.T) {
	ts := newSprigTurnServer(t, nil)
	t.Setenv("FAKE_TURN_SCRIPT", `[{"name":"zoom","args":{"id":0,"n":1}},{"name":"status"},{"name":"bash","args":{"cmd":"ls"}}]`)
	ts.say(t, "remember this exact sentence")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	var tools []string
	for _, m := range ts.logMessages(t) {
		if m.Kind == foreman.KindTool {
			tools = append(tools, m.Text)
		}
	}
	if len(tools) != 2 || tools[0] != "zoom({\"id\":0,\"n\":1})\nremember this exact sentence" || !strings.HasPrefix(tools[1], "status({})\n") {
		t.Fatalf("tool messages = %q", tools)
	}
	if !strings.Contains(read(ts.e.log), "sprig call bash: error=True") {
		t.Fatalf("an unknown tool was not refused:\n%s", read(ts.e.log))
	}
}

func TestASprigTurnThatTimesOutIsKilledAndNoted(t *testing.T) {
	ts := newSprigTurnServer(t, func(f *ForemanConfig) { f.TurnTimeout = 600 * time.Millisecond })
	t.Setenv("FAKE_TURN_MODE", "hang")
	ts.say(t, "hello")
	waitUntil(t, "the timeout", 90*time.Second, ts.turnsDone(1))
	recs, _ := foreman.ReadTurns(ts.logDir)
	if recs[len(recs)-1].Status != foreman.TurnTimeout {
		t.Fatalf("turns = %+v", recs)
	}
	pidText := strings.TrimSpace(read(ts.pids))
	waitUntil(t, "the fake to die", 10*time.Second, func() bool {
		_, err := os.Stat("/proc/" + pidText)
		return os.IsNotExist(err)
	})
	if ents, _ := os.ReadDir(ts.turnDir()); len(ents) > 0 {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "t-") {
				t.Fatalf("%s is still in the turn dir", e.Name())
			}
		}
	}
}

func TestASprigTurnThatFailsAddsOneNoteAndIsNotRetried(t *testing.T) {
	ts := newSprigTurnServer(t, nil)
	t.Setenv("FAKE_TURN_MODE", "exit1")
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(read(ts.e.log), "sprig argv:"); n != 1 {
		t.Fatalf("sprig ran %d times", n)
	}
	var notes int
	for _, m := range ts.logMessages(t) {
		if m.Kind == foreman.KindNote && strings.Contains(m.Text, "turn failed") && strings.Contains(m.Text, "sprig ended with an error: the turn failed") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("%d failure notes in %+v", notes, ts.logMessages(t))
	}
}

// --- one end-to-end turn: the real sprig, a fake API, the real tools server ---

var (
	sprigBuild    sync.Once
	sprigBuilt    string
	sprigBuildErr error
)

// buildSprig builds the sprig of this tree once. It runs go with the
// operator's build cache but nothing else of the real home: the cache is
// keyed by content, and the build writes only to the temp dir.
func buildSprig(t *testing.T) string {
	t.Helper()
	sprigBuild.Do(func() {
		dir, err := os.MkdirTemp("", "sprig-build-")
		if err != nil {
			sprigBuildErr = err
			return
		}
		out := filepath.Join(dir, "sprig")
		goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
		cmd := exec.Command(goBin, "build", "-o", out, "./cmd/sprig")
		cmd.Dir = filepath.Join("..", "..", "..", "sprig")
		cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(testhome.RealHome(), ".cache", "go-build"), "GOFLAGS=-p=1")
		if b, err := cmd.CombinedOutput(); err != nil {
			sprigBuildErr = fmt.Errorf("go build sprig: %v\n%s", err, b)
			return
		}
		sprigBuilt = out
	})
	if sprigBuildErr != nil {
		t.Fatal(sprigBuildErr)
	}
	return sprigBuilt
}

// dialSocket sends one tool request to a turn socket from this process.
func dialSocket(path string, req foreman.ToolRequest) (foreman.ToolResponse, error) {
	var resp foreman.ToolResponse
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	b, _ := json.Marshal(req)
	conn.Write(append(b, '\n'))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return resp, err
	}
	return resp, json.Unmarshal(line, &resp)
}

func TestARealSprigTurnReachesTheRealToolsServerThroughTheTurnSocket(t *testing.T) {
	sprigBin := buildSprig(t)
	var mu sync.Mutex
	var bodies []string
	var keyHits int
	var outsider foreman.ToolResponse
	var outsiderErr error
	sock := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, b.String())
		// The sentinel appears only here, as a match.
		if r.Header.Get("x-api-key") == turnSentinel {
			keyHits++
		}
		w.Header().Set("content-type", "application/json")
		if len(bodies) == 1 {
			// A worker of the same uid, outside the turn's tree, asks the
			// socket for the same tool at the same moment.
			outsider, outsiderErr = dialSocket(sock, foreman.ToolRequest{Tool: "status"})
			fmt.Fprint(w, `{"model":"fake-1","content":[{"type":"tool_use","id":"tu_1","name":"mcp__foreman__status","input":{}}],`+
				`"usage":{"input_tokens":11,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":300}}`)
			return
		}
		fmt.Fprint(w, `{"model":"fake-1","content":[{"type":"text","text":"All quiet on the floor."}],`+
			`"usage":{"input_tokens":4,"output_tokens":6,"cache_read_input_tokens":300,"cache_creation_input_tokens":0}}`)
	}))
	defer api.Close()

	ts := newSprigTurnServer(t, nil)
	t.Setenv("COPPICE_SPRIG_BIN", sprigBin)
	copied := filepath.Join(ts.e.dir, "sprig")
	data, err := os.ReadFile(sprigBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_SPRIG_BIN", copied)
	t.Setenv("FAKE_DESK_WIRE", "anthropic")
	t.Setenv("FAKE_DESK_BASE", api.URL)
	sock = filepath.Join(ts.turnDir(), "t-0.sock")
	ts.say(t, "how is the floor?")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("the fake API got %d requests: %v", len(bodies), bodies)
	}
	if keyHits != 2 {
		t.Fatalf("the key matched on %d of 2 requests", keyHits)
	}
	var first struct {
		Model  string `json:"model"`
		System []struct {
			Text  string         `json:"text"`
			Cache map[string]any `json:"cache_control"`
		} `json:"system"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Content []struct {
				Text  string         `json:"text"`
				Cache map[string]any `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &first); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range first.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "mcp__foreman__zoom,mcp__foreman__date,mcp__foreman__spawn,mcp__foreman__status" {
		t.Fatalf("tools = %v", names)
	}
	// One system block (the page, then the frozen view) with a mark.
	if first.Model != "desk-model" || len(first.System) != 1 || !strings.HasPrefix(first.System[0].Text, skills.ForemanTurn+"\n<chat>\n") ||
		!strings.HasSuffix(first.System[0].Text, "</chat>\n") || first.System[0].Cache["type"] != "ephemeral" {
		t.Fatalf("model and system: %q %+v", first.Model, first.System)
	}
	// The message is one block, and the end of the request is marked.
	blocks := first.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Cache["type"] != "ephemeral" ||
		!strings.Contains(blocks[0].Text, "<message>\nhow is the floor?\n</message>") || strings.Contains(blocks[0].Text, "<chat>") {
		t.Fatalf("message blocks = %+v", blocks)
	}
	// Each request of the tool loop has two marks: the system block and its end.
	for i, b := range bodies {
		if n := strings.Count(b, "cache_control"); n != 2 {
			t.Fatalf("request %d has %d cache marks, want 2:\n%s", i, n, b)
		}
	}
	if !strings.Contains(bodies[1], "workers: none") {
		t.Fatalf("the tool result did not reach the model:\n%s", bodies[1])
	}
	if outsiderErr == nil && outsider.OK || !strings.Contains(outsider.Error, "refused") {
		t.Fatalf("a worker outside the turn was served: %+v %v", outsider, outsiderErr)
	}
	msgs := ts.logMessages(t)
	last := msgs[len(msgs)-1]
	if last.Kind != foreman.KindForeman || last.Text != "All quiet on the floor." {
		t.Fatalf("log = %+v", msgs)
	}
	var tool string
	for _, m := range msgs {
		if m.Kind == foreman.KindTool {
			tool = m.Text
		}
	}
	if !strings.HasPrefix(tool, "status({})\n") {
		t.Fatalf("the status call was not logged: %q", tool)
	}
	recs, _ := foreman.ReadTurns(ts.logDir)
	u := recs[len(recs)-1].Usage
	if recs[len(recs)-1].Status != foreman.TurnOK || u == nil || u.Input != 15 || u.Output != 8 || u.CacheRead != 300 || u.CacheWrite != 300 {
		t.Fatalf("turns = %+v usage %+v", recs, u)
	}
	if hits := grepTree([]string{ts.logDir, ts.turnDir(), ts.e.dir}, turnSentinel); len(hits) != 0 {
		t.Fatalf("the key reached %v", hits)
	}
}

// noteHelperEnv runs in the test binary when it is the foreman's MCP relay:
// it logs which key-like variables it was given and whether any value is the
// turn's key (by hash), never a value.
func noteHelperEnv() {
	p := os.Getenv("FAKE_LOG")
	if p == "" {
		return
	}
	var names []string
	holds := false
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		switch name {
		case "ANTHROPIC_BASE_URL", "SPRIG_MODEL", "SPRIG_BACKEND", "AWS_SECRET_ACCESS_KEY", "OPENAI_API_KEY", "SOME_TOOL_TOKEN", "DESK_API_KEY", "ZZ_RANDOM_NAME":
			names = append(names, name)
		}
		sum := sha256.Sum256([]byte(val))
		if hex.EncodeToString(sum[:]) == os.Getenv("FAKE_SENTINEL_SHA") {
			holds = true
		}
	}
	sort.Strings(names)
	if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "mcp env keyvars: \nmcp env ambient: %s\nmcp env holds a key: %v\n", strings.Join(names, ","), holds)
		f.Close()
	}
}

func TestAClaudeCodeWireProfileIsRefusedWithTheFix(t *testing.T) {
	ts := newSprigTurnServer(t, func(f *ForemanConfig) { f.TurnProfile = "cc" })
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	if strings.Contains(read(ts.e.log), "sprig argv:") {
		t.Fatalf("sprig ran:\n%s", read(ts.e.log))
	}
	var notes []string
	for _, m := range ts.logMessages(t) {
		if m.Kind == foreman.KindNote {
			notes = append(notes, m.Text)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"cc"`) || !strings.Contains(notes[0], "Leave turn_profile empty") {
		t.Fatalf("notes = %q", notes)
	}
}

func TestTheDefaultProfileIsTheClaudeRunner(t *testing.T) {
	ts := newTurnServer(t, func(f *ForemanConfig) { f.TurnProfile = "default" })
	ts.say(t, "hello")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	log := read(ts.e.log)
	if !strings.Contains(log, "claude argv:") || strings.Contains(log, "agent-profiles") {
		t.Fatalf("log:\n%s", log)
	}
}

// A keyless profile gets no Authorization header, even with OPENAI_API_KEY
// set in coppice's environment.
func TestAKeylessOpenAIProfileSendsNoAuthorizationHeader(t *testing.T) {
	sprigBin := buildSprig(t)
	var mu sync.Mutex
	var auth []string
	var n int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth = append(auth, r.Header.Get("Authorization")+r.Header.Get("x-api-key"))
		n++
		w.Header().Set("content-type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"mcp__foreman__status","arguments":"{}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
			return
		}
		fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":"Quiet."}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	defer api.Close()
	ts := newSprigTurnServer(t, func(f *ForemanConfig) { f.TurnProfile = "free" })
	copied := filepath.Join(ts.e.dir, "sprig")
	data, err := os.ReadFile(sprigBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_SPRIG_BIN", copied)
	t.Setenv("FAKE_FREE_BASE", api.URL+"/v1")
	ts.say(t, "how is the floor?")
	waitUntil(t, "the turn", 90*time.Second, ts.turnsDone(1))
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 2 || auth[0] != "" || auth[1] != "" {
		t.Fatalf("headers on %d requests: %q", len(auth), auth)
	}
	msgs := ts.logMessages(t)
	if last := msgs[len(msgs)-1]; last.Kind != foreman.KindForeman || last.Text != "Quiet." {
		t.Fatalf("log = %+v", msgs)
	}
}

func TestAWireWithNoBaseUrlGetsItsDefaultAndNoEnvironmentCanChangeIt(t *testing.T) {
	if defaultBase("anthropic") != "https://api.anthropic.com" || defaultBase("openai") != "https://api.openai.com/v1" || defaultBase("claude-code") != "" {
		t.Fatal("default bases are wrong")
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9/proxy")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("OPENAI_API_KEY", "sk-other")
	t.Setenv("ZZ_RANDOM_NAME", "secret")
	env := strings.Join(sprigEnv(), "\n")
	for _, bad := range []string{"ANTHROPIC_BASE_URL", "HTTPS_PROXY", "OPENAI_API_KEY", "ZZ_RANDOM_NAME", "sk-other"} {
		if strings.Contains(env, bad) {
			t.Fatalf("%s reached sprig's environment", bad)
		}
	}
	if !strings.Contains(env, "PATH=") || !strings.Contains(env, "HOME=") {
		t.Fatalf("environment = %s", env)
	}
}

// A keyless compactor profile gets no Authorization header from the real
// sprig, with OPENAI_API_KEY set in coppice's environment.
func TestAKeylessCompactorProfileSendsNoAuthorizationHeader(t *testing.T) {
	sprigBin := buildSprig(t)
	var mu sync.Mutex
	var auth []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth = append(auth, r.Header.Get("Authorization")+r.Header.Get("x-api-key"))
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":"a short summary"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	defer api.Close()
	e := newFakeEnv(t)
	copied := filepath.Join(e.dir, "sprig")
	data, err := os.ReadFile(sprigBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_SPRIG_BIN", copied)
	t.Setenv("FAKE_FREE_BASE", api.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "sk-other-openai")
	s, _ := chatServer(t, e, func(f *ForemanConfig) { f.CompactorProfile = "free" })
	s.StartForeman()
	fc := s.ensureChat()
	if fc.chat == nil {
		t.Fatal(fc.problem)
	}
	longMessages(t, fc, 1)
	waitUntil(t, "the summary", 90*time.Second, func() bool { return pendingOf(fc) == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(auth) == 0 {
		t.Fatal("the compactor made no call")
	}
	for _, a := range auth {
		if a != "" {
			t.Fatalf("a header was sent: %q", a)
		}
	}
}
