package opencode

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
)

// The test binary doubles as a fake `opencode serve`. The adapter under test
// runs os.Args[0] with COPPICE_OPENCODE_FAKE=1 in its environment, and
// TestMain turns that process into the fake before any test flag is read.
// No test ever runs the real opencode binary.
func TestMain(m *testing.M) {
	if os.Getenv("COPPICE_OPENCODE_FAKE") == "1" {
		os.Exit(runFake())
	}
	os.Exit(m.Run())
}

// runFake serves the endpoints PINS.md records, with basic auth checked
// against the password and user name the adapter put in its environment.
// FAKE_LOG receives one JSON line per request. FAKE_SSE names the events
// GET /event streams. FAKE_SSE_END=1 ends the stream after them.
// FAKE_SESSION is the id POST /session returns and GET /session/{id}
// knows. FAKE_EXIT_MS makes the process exit after that many ms.
// FAKE_HOST is the host the listen line names.
func runFake() int {
	args := os.Args[1:]
	logPath := os.Getenv("FAKE_LOG")
	var logMu sync.Mutex
	logLine := func(v map[string]any) {
		if logPath == "" {
			return
		}
		logMu.Lock()
		defer logMu.Unlock()
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		b, _ := json.Marshal(v)
		_, _ = f.Write(append(b, '\n'))
	}
	logLine(map[string]any{"argv": args})
	if name := os.Getenv("FAKE_LOG_ENV"); name != "" {
		v, ok := os.LookupEnv(name)
		if !ok {
			v = "<unset>"
		}
		logLine(map[string]any{"env": v})
	}
	if os.Getenv("FAKE_NEVER_LISTEN") == "1" {
		time.Sleep(30 * time.Second)
		return 0
	}

	user := os.Getenv("OPENCODE_SERVER_USERNAME")
	if user == "" {
		user = "opencode"
	}
	pass := os.Getenv("OPENCODE_SERVER_PASSWORD")
	session := os.Getenv("FAKE_SESSION")

	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != user || p != pass || pass == "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Secure Area"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.URL.Path != "/global/health" {
				logLine(map[string]any{"method": r.Method, "path": r.URL.Path, "body": body})
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/global/health", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.32"}`))
	}))
	mux.HandleFunc("/session", authed(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": session})
	}))
	mux.HandleFunc("/session/", authed(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/session/")
		if strings.HasSuffix(rest, "/prompt_async") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if rest == session || rest == os.Getenv("FAKE_EXISTING") {
			_, _ = w.Write([]byte(`{"id":"` + rest + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	mux.HandleFunc("/permission/", authed(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("FAKE_REPLY_FAIL") == "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("true"))
	}))
	mux.HandleFunc("/question/", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("true"))
	}))
	mux.HandleFunc("/event", authed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, err := os.Open(os.Getenv("FAKE_SSE")); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 4<<20)
			for sc.Scan() {
				_, _ = w.Write([]byte(sc.Text() + "\n"))
			}
			f.Close()
		}
		w.(http.Flusher).Flush()
		if os.Getenv("FAKE_SSE_END") == "1" {
			return
		}
		<-r.Context().Done()
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1
	}
	host := os.Getenv("FAKE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	fmt.Printf("opencode server listening on http://%s:%d\n", host, ln.Addr().(*net.TCPAddr).Port)
	if ms, err := strconv.Atoi(os.Getenv("FAKE_EXIT_MS")); err == nil {
		go func() {
			time.Sleep(time.Duration(ms) * time.Millisecond)
			os.Exit(0)
		}()
	}
	_ = http.Serve(ln, mux)
	return 0
}

type fakeRun struct {
	log string
	sse string
}

// startFake starts the adapter against the fake. env adds to the fake's
// environment. sse is the event stream the fake serves.
func startFake(t *testing.T, sse string, env map[string]string, opts pane.StartOpts) (pane.Proc, fakeRun, error) {
	t.Helper()
	dir := t.TempDir()
	run := fakeRun{log: filepath.Join(dir, "requests.jsonl"), sse: filepath.Join(dir, "events.sse")}
	if err := os.WriteFile(run.sse, []byte(sse), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cfg")
	plugin := filepath.Join(cfg, "opencode", "plugins", "daisugi-gate.ts")
	if err := os.MkdirAll(filepath.Dir(plugin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin, shippedPlugin(t), 0o600); err != nil {
		t.Fatal(err)
	}
	e := map[string]string{
		"XDG_CONFIG_HOME":       cfg,
		"COPPICE_OPENCODE_FAKE": "1",
		"FAKE_LOG":              run.log,
		"FAKE_SSE":              run.sse,
		"FAKE_SESSION":          ourSession,
	}
	for k, v := range opts.Env {
		e[k] = v
	}
	for k, v := range env {
		e[k] = v
	}
	opts.Env = e
	if opts.Cwd == "" {
		opts.Cwd = dir
	}
	a := adapter{Bin: os.Args[0]}
	p, err := a.Start(context.Background(), opts, nil)
	if err == nil {
		t.Cleanup(func() { _ = p.Stop() })
	}
	return p, run, err
}

func requests(t *testing.T, run fakeRun) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(run.log)
	if err != nil {
		t.Fatalf("read the request log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func findRequest(t *testing.T, run fakeRun, method, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range requests(t, run) {
			if r["method"] == method && r["path"] == path {
				return r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s %s in %v", method, path, requests(t, run))
	return nil
}

func nextState(t *testing.T, p pane.Proc, want string) pane.Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				t.Fatalf("events closed before state %s", want)
			}
			if ev.Kind == pane.EvState && ev.State == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for state %s", want)
		}
	}
}

func drainToEnd(t *testing.T, p pane.Proc) []pane.Event {
	t.Helper()
	var got []pane.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("events never closed, got %+v", got)
		}
	}
}

const permAsked = `data: {"type":"permission.asked","properties":{"id":"per_1","sessionID":"` + ourSession + `","permission":"bash","patterns":["rm -rf build"],"metadata":{},"always":[]}}` + "\n\n"

const questionAsked = `data: {"type":"question.asked","properties":{"id":"que_1","sessionID":"` + ourSession + `","questions":[{"question":"Which branch?"}]}}` + "\n\n"

func TestAdapterNameIsTheRegistryKey(t *testing.T) {
	if (adapter{}).Name() != "opencode" {
		t.Fatal(`Name() must be "opencode"`)
	}
}

func TestServeArgsBindLoopbackAndAppendExtraArgv(t *testing.T) {
	got := strings.Join(serveArgs([]string{"--print-logs"}), " ")
	want := "serve --hostname 127.0.0.1 --port 0 --print-logs"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOpencodeEnvTheTokenAndUserAlwaysWin(t *testing.T) {
	env := opencodeEnv(map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pane-value", "OPENCODE_SERVER_USERNAME": "mallory", "OTHER": "x",
	}, "real-token")
	if env["OPENCODE_SERVER_PASSWORD"] != "real-token" || env["OPENCODE_SERVER_USERNAME"] != "opencode" {
		t.Fatalf("got %v, want the adapter's own credentials", env)
	}
	if env["OTHER"] != "x" {
		t.Fatalf("got %v, want the rest of the pane env kept", env)
	}
}

func TestParseListenLineAcceptsOnlyLoopbackHTTP(t *testing.T) {
	good, err := parseListenLine("opencode server listening on http://127.0.0.1:38197")
	if err != nil || good != "http://127.0.0.1:38197" {
		t.Fatalf("got %q, %v", good, err)
	}
	for _, line := range []string{
		"opencode server listening on http://0.0.0.0:4096",
		"opencode server listening on http://example.com:4096",
		"opencode server listening on https://127.0.0.1:4096",
		"opencode server listening on http://127.0.0.1:4096/x",
		"opencode server listening on http://127.0.0.1",
		"something else",
	} {
		if _, err := parseListenLine(line); err == nil {
			t.Errorf("parseListenLine(%q) accepted it", line)
		}
	}
}

func TestStartCreatesASessionAndIsIdleWithThePaneEnvUnableToBreakAuth(t *testing.T) {
	p, run, err := startFake(t, "", map[string]string{"OPENCODE_SERVER_PASSWORD": "pane-value"},
		pane.StartOpts{Argv: []string{"--print-logs"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid, ok := p.SessionID(); !ok || sid != ourSession {
		t.Fatalf("SessionID() = %q, %v", sid, ok)
	}
	nextState(t, p, pane.StateIdleStr)
	reqs := requests(t, run)
	argv, _ := json.Marshal(reqs[0]["argv"])
	if string(argv) != `["serve","--hostname","127.0.0.1","--port","0","--print-logs"]` {
		t.Fatalf("argv = %s", argv)
	}
	create := findRequest(t, run, "POST", "/session")
	if body, _ := create["body"].(map[string]any); body["title"] == nil {
		t.Fatalf("create body = %v", create["body"])
	}
}

func TestPromptAndSteerSendAsyncTextPrompts(t *testing.T) {
	p, run, err := startFake(t, "", nil, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Prompt("say hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := p.Steer("and then stop"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	var texts []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(texts) < 2 {
		texts = nil
		for _, r := range requests(t, run) {
			if r["path"] == "/session/"+ourSession+"/prompt_async" {
				parts := r["body"].(map[string]any)["parts"].([]any)
				texts = append(texts, parts[0].(map[string]any)["text"].(string))
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Join(texts, "|") != "say hi|and then stop" {
		t.Fatalf("prompts = %v", texts)
	}
	if err := p.WriteStdin([]byte("x")); !errors.Is(err, pane.ErrUnsupported) {
		t.Fatalf("WriteStdin: %v, want ErrUnsupported", err)
	}
}

func TestTheFixtureReachesThePaneInOrder(t *testing.T) {
	raw, err := os.ReadFile("testdata/session_lifecycle.sse")
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := startFake(t, string(raw), nil, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextState(t, p, pane.StateIdleStr)
	blocked := nextState(t, p, pane.StateBlockedStr)
	if blocked.Ask == nil || blocked.Ask.ID != "per_1" || blocked.Ask.Tool != "bash" {
		t.Fatalf("blocked event %+v", blocked)
	}
	nextState(t, p, pane.StateUnknownStr)
}

func replies(t *testing.T, run fakeRun, prefix string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range requests(t, run) {
		if p, _ := r["path"].(string); strings.HasPrefix(p, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func promptsSent(t *testing.T, run fakeRun) int {
	t.Helper()
	return len(replies(t, run, "/session/"+ourSession+"/prompt_async"))
}

// A typed y never approves OpenCode's own permission prompt, from the
// operator or from a pane.
func TestATypedPromptNeverAnswersAPermission(t *testing.T) {
	p, run, err := startFake(t, permAsked, nil, pane.StartOpts{PaneID: "w1:p2"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextState(t, p, pane.StateBlockedStr)
	for _, operator := range []bool{true, false} {
		err := p.(pane.Prompter).PromptFrom("y", operator)
		if err == nil || !strings.Contains(err.Error(), "coppice agent allow w1:p2 per_1") {
			t.Fatalf("PromptFrom(y, %v) = %v, want a refusal naming agent allow", operator, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := replies(t, run, "/permission/"); len(got) != 0 {
		t.Fatalf("a typed prompt answered the permission: %v", got)
	}
	if promptsSent(t, run) != 0 {
		t.Fatal("a typed prompt went out while the permission was open")
	}
}

func TestAnswerAllowsOnceAndDenyRejectsWithTheReason(t *testing.T) {
	two := permAsked + strings.Replace(permAsked, "per_1", "per_2", 1)
	p, run, err := startFake(t, two, nil, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextState(t, p, pane.StateBlockedStr)
	nextState(t, p, pane.StateBlockedStr)
	an := p.(pane.Answerer)
	if !an.OwnsAsk("per_1") || !an.OwnsAsk("per_2") || an.OwnsAsk("per_9") {
		t.Fatal("OwnsAsk does not match the open asks")
	}
	if err := an.Answer("per_2", true, ""); err != nil {
		t.Fatalf("Answer allow: %v", err)
	}
	if err := an.Answer("per_1", false, "not now"); err != nil {
		t.Fatalf("Answer deny: %v", err)
	}
	r2 := findRequest(t, run, "POST", "/permission/per_2/reply")
	if body := r2["body"].(map[string]any); body["reply"] != "once" {
		t.Fatalf("per_2 reply = %v, want once", body)
	}
	r1 := findRequest(t, run, "POST", "/permission/per_1/reply")
	if body := r1["body"].(map[string]any); body["reply"] != "reject" || body["message"] != "not now" {
		t.Fatalf("per_1 reply = %v, want reject with the reason", body)
	}
	if an.OwnsAsk("per_1") || an.OwnsAsk("per_2") {
		t.Fatal("an answered ask is still owned")
	}
	if err := an.Answer("per_1", true, ""); err == nil {
		t.Fatal("a second answer to the same ask must fail")
	}
}

// A pane's y after the deadline reaches no approval: the adapter rejected
// the ask at its deadline, and the y goes out as plain text.
func TestAPanesYAfterTheDeadlineApprovesNothing(t *testing.T) {
	oldDeadline, oldTick := askDeadline, askTick
	askDeadline, askTick = 200*time.Millisecond, 20*time.Millisecond
	defer func() { askDeadline, askTick = oldDeadline, oldTick }()
	p, run, err := startFake(t, permAsked, nil, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextState(t, p, pane.StateBlockedStr)
	r := findRequest(t, run, "POST", "/permission/per_1/reply")
	if body := r["body"].(map[string]any); body["reply"] != "reject" {
		t.Fatalf("expiry reply = %v, want reject", body)
	}
	if p.(pane.Answerer).OwnsAsk("per_1") {
		t.Fatal("an expired ask is still owned")
	}
	if err := p.(pane.Prompter).PromptFrom("y", false); err != nil {
		t.Fatalf("PromptFrom after the deadline: %v", err)
	}
	for _, r := range replies(t, run, "/permission/") {
		if r["body"].(map[string]any)["reply"] == "once" {
			t.Fatal("a pane's y approved the permission")
		}
	}
}

func TestOnlyTheOperatorsTextAnswersAQuestion(t *testing.T) {
	p, run, err := startFake(t, questionAsked, nil, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextState(t, p, pane.StateBlockedStr)
	if err := p.(pane.Prompter).PromptFrom("main", false); err == nil {
		t.Fatal("a pane's text answered a question")
	}
	if err := p.(pane.Answerer).Answer("que_1", true, ""); err == nil {
		t.Fatal("agent.allow answered a question")
	}
	if err := p.(pane.Prompter).PromptFrom("main", true); err != nil {
		t.Fatalf("operator answer: %v", err)
	}
	r := findRequest(t, run, "POST", "/question/que_1/reply")
	answers, _ := json.Marshal(r["body"].(map[string]any)["answers"])
	if string(answers) != `[["main"]]` {
		t.Fatalf("answers = %s", answers)
	}
	if promptsSent(t, run) != 0 {
		t.Fatal("the answer also went out as a prompt")
	}
}

func TestAChildSessionsPermissionBlocksThePaneAndIsAnswerable(t *testing.T) {
	sse := `data: {"type":"session.created","properties":{"sessionID":"ses_child","info":{"id":"ses_child","parentID":"` + ourSession + `"}}}` + "\n\n" +
		strings.Replace(permAsked, `"sessionID":"`+ourSession+`"`, `"sessionID":"ses_child"`, 1)
	p, run, err := startFake(t, sse, nil, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ev := nextState(t, p, pane.StateBlockedStr)
	if ev.Ask == nil || ev.Ask.ID != "per_1" {
		t.Fatalf("blocked %+v", ev)
	}
	if err := p.(pane.Answerer).Answer("per_1", false, "no"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	findRequest(t, run, "POST", "/permission/per_1/reply")
}

func TestStartRefusesWhenTheGatePluginIsMissing(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "empty-cfg")
	_, _, err := startFake(t, "", map[string]string{"XDG_CONFIG_HOME": cfg}, pane.StartOpts{})
	if err == nil || !strings.Contains(err.Error(), "daisugi install --harness opencode") {
		t.Fatalf("Start = %v, want a refusal naming the install command", err)
	}
}

func TestGatePluginPathFollowsTheServersEnvironment(t *testing.T) {
	for env, want := range map[string]string{
		"HOME=/h":                        "/h/.config/opencode/plugins/daisugi-gate.ts",
		"HOME=/h\x00XDG_CONFIG_HOME=/x":  "/x/opencode/plugins/daisugi-gate.ts",
		"HOME=/h\x00XDG_CONFIG_HOME=rel": "/h/.config/opencode/plugins/daisugi-gate.ts",
	} {
		got, err := gatePluginPath(strings.Split(env, "\x00"))
		if err != nil || got != want {
			t.Fatalf("gatePluginPath(%q) = %q, %v, want %q", env, got, err, want)
		}
	}
	if _, err := gatePluginPath([]string{"PATH=/bin"}); err == nil {
		t.Fatal("no HOME and no XDG_CONFIG_HOME must fail")
	}
}

func TestResumeUsesAnExistingSession(t *testing.T) {
	p, run, err := startFake(t, "", map[string]string{"FAKE_EXISTING": "ses_old"}, pane.StartOpts{Resume: "ses_old"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid, _ := p.SessionID(); sid != "ses_old" {
		t.Fatalf("SessionID() = %q, want ses_old", sid)
	}
	for _, r := range requests(t, run) {
		if r["method"] == "POST" && r["path"] == "/session" {
			t.Fatal("a resume must not create a new session")
		}
	}
}

func TestResumeOfAMissingSessionSaysSoAndStartsANewOne(t *testing.T) {
	p, _, err := startFake(t, "", nil, pane.StartOpts{Resume: "ses_gone"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid, _ := p.SessionID(); sid != ourSession {
		t.Fatalf("SessionID() = %q, want the new session", sid)
	}
	select {
	case ev := <-p.Events():
		if ev.Kind != pane.EvError || !strings.Contains(ev.Detail, "ses_gone") {
			t.Fatalf("first event %+v, want an error that names ses_gone", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestProcessExitEndsTheStreamWithDone(t *testing.T) {
	p, _, err := startFake(t, "", map[string]string{"FAKE_EXIT_MS": "300"}, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := drainToEnd(t, p)
	last := got[len(got)-1]
	if last.Kind != pane.EvEnd || last.State != pane.StateDoneStr {
		t.Fatalf("last event %+v, want end done", last)
	}
	for _, ev := range got {
		if ev.State == pane.StateUnknownStr {
			t.Fatalf("a clean exit read as unknown: %+v", got)
		}
	}
	if err := p.Prompt("hi"); err == nil {
		t.Fatal("Prompt after the process ended must fail")
	}
}

func TestAStreamThatEndsWhileTheServerLivesIsUnknown(t *testing.T) {
	p, _, err := startFake(t, "", map[string]string{"FAKE_SSE_END": "1"}, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ev := nextState(t, p, pane.StateUnknownStr)
	if !strings.Contains(ev.Detail, "event stream") {
		t.Fatalf("unknown event %+v does not say the stream ended", ev)
	}
}

func TestStopClosesEventsAndASecondStopIsANoOp(t *testing.T) {
	p, _, err := startFake(t, "", nil, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if pids := p.(pane.Pider).Pids(); len(pids) != 1 || pids[0] <= 0 {
		t.Fatalf("Pids() = %v", pids)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	drainToEnd(t, p)
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := p.Steer("x"); err == nil {
		t.Fatal("Steer after Stop must fail")
	}
}

func TestStartRefusesAServerThatListensOffLoopback(t *testing.T) {
	_, _, err := startFake(t, "", map[string]string{"FAKE_HOST": "0.0.0.0"}, pane.StartOpts{})
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("Start = %v, want a refusal that names 127.0.0.1", err)
	}
}

func TestStartFailsWhenTheServerNeverListens(t *testing.T) {
	old := readyTimeout
	readyTimeout = 500 * time.Millisecond
	defer func() { readyTimeout = old }()
	start := time.Now()
	_, _, err := startFake(t, "", map[string]string{"FAKE_NEVER_LISTEN": "1"}, pane.StartOpts{})
	if err == nil {
		t.Fatal("want an error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Start took %v to give up", time.Since(start))
	}
}

func TestStartFailsWhenTheBinaryIsMissing(t *testing.T) {
	a := adapter{Bin: filepath.Join(t.TempDir(), "no-such-opencode")}
	if _, err := a.Start(context.Background(), pane.StartOpts{Cwd: t.TempDir()}, nil); err == nil {
		t.Fatal("want an error")
	}
}

func TestTheAdapterIsAPidSource(t *testing.T) {
	var a pane.Adapter = New()
	src, ok := a.(pane.PidSource)
	if !ok || src.PidProc() == nil {
		t.Fatal("the opencode adapter must name its pids")
	}
}

// --pure and OPENCODE_PURE make OpenCode skip every external plugin, the
// gate plugin too. A coppice pane must never start ungated that way.
func TestStartRefusesThePureFlag(t *testing.T) {
	for _, argv := range [][]string{{"--pure"}, {"--pure=true"}, {"--print-logs", "--pure"}} {
		a := adapter{Bin: filepath.Join(t.TempDir(), "never-run")}
		_, err := a.Start(context.Background(), pane.StartOpts{Cwd: t.TempDir(), Argv: argv}, nil)
		if err == nil || !strings.Contains(err.Error(), "--pure") {
			t.Fatalf("Start with %v = %v, want a refusal that names --pure", argv, err)
		}
	}
}

func TestOpencodeEnvDropsOpencodePure(t *testing.T) {
	env := opencodeEnv(map[string]string{"OPENCODE_PURE": "1"}, "tok")
	if _, ok := env["OPENCODE_PURE"]; ok {
		t.Fatalf("got %v, want OPENCODE_PURE removed", env)
	}
	base := baseEnv([]string{"PATH=/bin", "OPENCODE_PURE=1", "HOME=/h"})
	for _, kv := range base {
		if strings.HasPrefix(kv, "OPENCODE_PURE=") {
			t.Fatalf("baseEnv kept %q", kv)
		}
	}
	if len(base) != 2 {
		t.Fatalf("baseEnv = %v, want the other two kept", base)
	}
}

func TestTheServerNeverSeesOpencodePure(t *testing.T) {
	t.Setenv("OPENCODE_PURE", "1")
	p, run, err := startFake(t, "", map[string]string{"OPENCODE_PURE": "1", "FAKE_LOG_ENV": "OPENCODE_PURE"}, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = p
	for _, r := range requests(t, run) {
		if v, ok := r["env"]; ok {
			if v != "<unset>" {
				t.Fatalf("the server saw OPENCODE_PURE=%v", v)
			}
			return
		}
	}
	t.Fatal("the fake did not log its env")
}

// shippedPlugin is the gate plugin as the Python package ships it.
func shippedPlugin(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..",
		"src", "opendaisugi", "harness_opencode", "plugin", "daisugi-gate.ts"))
	if err != nil {
		t.Fatalf("read the shipped plugin: %v", err)
	}
	return b
}

// The hash Start checks must be the hash of the plugin the package ships.
// A change to the plugin fails here until the hash follows it.
func TestThePinnedPluginHashIsTheShippedPlugins(t *testing.T) {
	sum := sha256.Sum256(shippedPlugin(t))
	if got := hex.EncodeToString(sum[:]); got != gatePluginSHA256 {
		t.Fatalf("gatePluginSHA256 = %s, the shipped plugin hashes to %s", gatePluginSHA256, got)
	}
}

func TestStartRefusesAPluginThatIsNotTheShippedOne(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg")
	plugin := filepath.Join(cfg, "opencode", "plugins", "daisugi-gate.ts")
	if err := os.MkdirAll(filepath.Dir(plugin), 0o700); err != nil {
		t.Fatal(err)
	}
	edited := append(shippedPlugin(t), []byte("\n// edited\n")...)
	if err := os.WriteFile(plugin, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	a := adapter{Bin: filepath.Join(dir, "never-run")}
	_, err := a.Start(context.Background(), pane.StartOpts{Cwd: dir, Env: map[string]string{"XDG_CONFIG_HOME": cfg}}, nil)
	if err == nil || !strings.Contains(err.Error(), "daisugi install --harness opencode") {
		t.Fatalf("Start = %v, want a refusal naming the install command", err)
	}
}

// When the deadline reject fails, OpenCode still waits, so the pane must
// not read as working: it goes unknown with a line that says why.
func TestAFailedDeadlineRejectMakesThePaneUnknown(t *testing.T) {
	oldDeadline, oldTick := askDeadline, askTick
	askDeadline, askTick = 200*time.Millisecond, 20*time.Millisecond
	defer func() { askDeadline, askTick = oldDeadline, oldTick }()
	p, _, err := startFake(t, permAsked, map[string]string{"FAKE_REPLY_FAIL": "1"}, pane.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nextState(t, p, pane.StateBlockedStr)
	ev := nextState(t, p, pane.StateUnknownStr)
	if !strings.Contains(ev.Detail, "per_1") {
		t.Fatalf("unknown event %+v does not name the ask", ev)
	}
}
