package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

const refusal = "a pane can propose. It cannot allow."

// streamFacts is stream with kernel facts about the peer, the way Serve
// hands them over for a real socket.
func streamFacts(t *testing.T, s *Server, f *peerFacts) (send func(string), next func() map[string]any, stop func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.serveConn(inR, outW, f); _ = outW.Close() }()
	d := proto.NewDecoder(outR)
	send = func(line string) {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	next = func() map[string]any {
		t.Helper()
		line, err := d.Next()
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	stop = func() { _ = inW.Close() }
	return
}

func refusedWith(t *testing.T, r map[string]any) {
	t.Helper()
	if r["ok"] != false {
		t.Fatalf("reply %v, want a refusal", r)
	}
	e, _ := r["error"].(map[string]any)
	if e["code"] != "unauthorized" || e["message"] != refusal {
		t.Fatalf("refusal %v, want unauthorized %q", e, refusal)
	}
}

// plantAsk writes one pending ask the way the gate does.
func plantAsk(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, "asks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"toolUseId":"` + id + `","nonce":"n1","postedAt":1,"deadline":9999999999}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAConnectionThatSaysItIsAPaneCannotAllowOrDeny(t *testing.T) {
	s := newTestServer(t)
	plantAsk(t, s.cfg.GateRoot, "ask-3")
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("hello: %v", r)
	}
	send(`{"id":"1","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	refusedWith(t, next())
	send(`{"id":"2","cmd":"agent.deny","pane":"w1:p1","ask":"ask-3"}`)
	refusedWith(t, next())
	send(`{"id":"3","cmd":"pane.report_state","pane":"w1:p1","event":{"v":1,"ts":1,"session_id":"s",` +
		`"harness":"claude","pane":"w1:p1","state":"idle","source":"operator"}}`)
	refusedWith(t, next())
	if _, err := os.Stat(filepath.Join(s.cfg.GateRoot, "answers", "ask-3.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused allow wrote an answer: %v", err)
	}
}

// The kernel decides on Linux: a pane process that says it is the operator
// is still a pane.
func TestAKernelPaneCannotAllowWhateverItSays(t *testing.T) {
	s := newTestServer(t)
	plantAsk(t, s.cfg.GateRoot, "ask-3")
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p1"})
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"operator"}`)
	next()
	send(`{"id":"1","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	refusedWith(t, next())
}

// A peer the kernel cannot place is refused the allow verbs, and only them.
func TestAnUnreadablePeerCannotAllowButCanList(t *testing.T) {
	s := newTestServer(t)
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, unknown: true})
	defer stop()
	send(`{"id":"1","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	refusedWith(t, next())
	send(`{"id":"2","cmd":"pane.list"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("pane.list: %v", r)
	}
}

// The operator answers an ask through the same file the gate reads.
func TestTheOperatorAllowsThroughTheGateFile(t *testing.T) {
	s := newAgentServer(t)
	plantAsk(t, s.cfg.GateRoot, "ask-3")
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		strings.Replace(gateBlockedLine("w1:p1"), "toolu_1", "ask-3", 1),
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","confirm":"auth fix"}`,
		`{"id":"3","cmd":"agent.deny","pane":"w1:p1","ask":"ask-9"}`)
	got = got[1:]
	if !got[1].OK {
		t.Fatalf("agent.allow: %+v", got[1].Error)
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.GateRoot, "answers", "ask-3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ans map[string]any
	if err := json.Unmarshal(b, &ans); err != nil {
		t.Fatal(err)
	}
	if ans["decision"] != "allow" || ans["nonce"] != "n1" {
		t.Fatalf("answer file = %v", ans)
	}
	if got[2].OK || got[2].Error.Code != "bad_request" || !strings.Contains(got[2].Error.Message, "ask-9") {
		t.Fatalf("a deny on no ask answered %+v", got[2])
	}
}

// Every other verb a pane runs is printed as a note, led by its label.
func TestAPanesVerbsBecomeNotes(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	watch, events, stopWatch := stream(t, s)
	defer stopWatch()
	watch(`{"id":"1","cmd":"events.subscribe","kinds":["note"],"panes":"*"}`)
	events()
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}`)
	next()
	send(`{"id":"1","cmd":"pane.create","cwd":"` + t.TempDir() + `","cmd_argv":["sh","-c","sleep 5"],` +
		`"kind":"pty","label":"docs"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("pane.create: %v", r)
	}
	ev := events()
	text, _ := ev["text"].(string)
	if ev["event"] != "note" || !strings.HasPrefix(text, "auth fix › pane.create  docs") {
		t.Fatalf("note = %v", ev)
	}
	if ev["pane"] != "w1:p1" {
		t.Fatalf("note pane = %v, want w1:p1", ev["pane"])
	}
}

// An operator connection runs verbs with no note.
func TestAnOperatorsVerbsAreNotNotes(t *testing.T) {
	s := newTestServer(t)
	roundTrip(t, s, `{"id":"1","cmd":"pane.list"}`)
	list := result(t, roundTrip(t, s, `{"id":"2","cmd":"floor.notes"}`)[0])
	if notes, _ := list["notes"].([]any); len(notes) != 0 {
		t.Fatalf("operator verbs left notes: %v", notes)
	}
}

// classifyPeer walks parent links. stat maps a pid to its parent and its
// session.
func TestClassifyPeer(t *testing.T) {
	stat := func(tab map[int][2]int) func(int) (int, int, error) {
		return func(pid int) (int, int, error) {
			v, ok := tab[pid]
			if !ok {
				return 0, 0, errors.New("gone")
			}
			return v[0], v[1], nil
		}
	}
	panes := map[int]string{500: "w1:p1"}
	tab := map[int][2]int{
		100: {1, 100},   // the server, its own session leader
		500: {100, 500}, // a pty pane, a session leader
		510: {500, 500}, // a shell in the pane
		520: {510, 500}, // coppice run from that shell
		530: {1, 500},   // a double fork out of the pane, same session
		600: {100, 100}, // a headless harness, child of the server
		610: {600, 100}, // its tool call
		700: {1, 700},   // the operator's shell
		710: {700, 700}, // the operator's floor
	}
	cases := []struct {
		pid     int
		pane    bool
		id      string
		unknown bool
	}{
		{100, false, "", false},
		{520, true, "w1:p1", false},
		{530, true, "w1:p1", false},
		{610, true, "", false},
		{710, false, "", false},
		{999, false, "", true},
	}
	for _, c := range cases {
		got := classifyPeer(c.pid, 100, true, panes, nil, stat(tab))
		if got.pane != c.pane || got.paneID != c.id || got.unknown != c.unknown || !got.checked {
			t.Errorf("pid %d: %+v, want pane %v id %q unknown %v", c.pid, got, c.pane, c.id, c.unknown)
		}
	}
	// When the server does not lead its own session, sharing its session
	// says nothing.
	tab[800] = [2]int{1, 100}
	if got := classifyPeer(800, 100, false, panes, nil, stat(tab)); got.pane {
		t.Errorf("a peer in a session the server does not lead counted as a pane: %+v", got)
	}
}

// A web server names the local peer it serves in hello, and the server
// places that pid the way it places a socket peer. The names can only take
// rights away: a pane pid or an unknown peer loses allow, and pid 1, which
// is no pane, changes nothing.
func TestHelloPeerPIDsPlaceTheWebClient(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	lp, ok := s.Live("w1:p1")
	if !ok || lp.PTY.Pid() <= 0 {
		t.Fatal("no live pty pane")
	}
	cases := []struct {
		hello string
		allow bool
		role  string
	}{
		{fmt.Sprintf(`{"id":"0","cmd":"hello","peer_pids":[%d]}`, lp.PTY.Pid()), false, "pane"},
		{`{"id":"0","cmd":"hello","peer_unknown":true}`, false, "operator"},
		{`{"id":"0","cmd":"hello","peer_pids":[1]}`, true, "operator"},
		{`{"id":"0","cmd":"hello","peer_pids":[]}`, false, "operator"},
	}
	for _, c := range cases {
		got := roundTrip(t, s, c.hello, `{"id":"1","cmd":"agent.allow","pane":"w1:p1","ask":"none"}`)
		res := result(t, got[0])
		if res["allow"] != c.allow || res["role"] != c.role {
			t.Errorf("%s: hello answered %v, want allow %v role %s", c.hello, res, c.allow, c.role)
		}
		refused := !got[1].OK && got[1].Error.Code == "unauthorized"
		if refused == c.allow {
			t.Errorf("%s: agent.allow answered %+v", c.hello, got[1])
		}
	}
}

// A pane cannot type into a pane that waits on its harness's own question.
// The operator can.
func TestAPaneCannotTypeIntoABlockedPane(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1), gateBlockedLine("w1:p1"))
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p2"})
	defer stop()
	for _, line := range []string{
		`{"id":"1","cmd":"pane.send_keys","pane":"w1:p1","keys":["1","enter"]}`,
		`{"id":"2","cmd":"pane.send_text","pane":"w1:p1","text":"1"}`,
		`{"id":"3","cmd":"pane.run","pane":"w1:p1","line":"yes"}`,
		`{"id":"4","cmd":"agent.prompt","pane":"w1:p1","text":"yes"}`,
	} {
		send(line)
		refusedWith(t, next())
	}
	got := roundTrip(t, s, `{"id":"5","cmd":"pane.send_text","pane":"w1:p1","text":"1"}`)
	if !got[0].OK {
		t.Fatalf("the operator's send_text was refused: %+v", got[0].Error)
	}
}

// A pane that is not blocked still takes a pane's text.
func TestAPaneCanTypeIntoAWorkingPane(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p2"})
	defer stop()
	send(`{"id":"1","cmd":"pane.send_text","pane":"w1:p1","text":"hi"}`)
	if r := next(); r["ok"] != true {
		t.Fatalf("send_text to a working pane: %v", r)
	}
}

// An ask answers only through the pane that holds it.
func TestAnAllowNamesThePaneThatHoldsTheAsk(t *testing.T) {
	s := newAgentServer(t)
	plantAsk(t, s.cfg.GateRoot, "ask-3")
	dir := t.TempDir()
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", dir, 1),
		strings.Replace(strings.Replace(paneCreate, "%s", dir, 1), `"id":"1"`, `"id":"2"`, 1),
		strings.Replace(gateBlockedLine("w1:p1"), "toolu_1", "ask-3", 1),
		`{"id":"4","cmd":"agent.allow","pane":"w1:p2","ask":"ask-3"}`)
	last := got[3]
	if last.OK || last.Error.Code != "bad_request" || !strings.Contains(last.Error.Message, "w1:p2") {
		t.Fatalf("an allow on the wrong pane answered %+v", last)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.GateRoot, "answers", "ask-3.json")); err == nil {
		t.Fatal("an allow on the wrong pane wrote an answer")
	}
}

// A pane the kernel cannot name, a headless one, cannot borrow another
// pane's label through its hello.
func TestAKernelPaneWithNoIDIgnoresTheHelloPane(t *testing.T) {
	c := &Client{facts: peerFacts{checked: true, pane: true}, helloRole: "pane", helloPane: "w1:p9"}
	if ro := c.roleOf(); !ro.pane || ro.paneID != "" {
		t.Fatalf("role = %+v, want a pane with no id", ro)
	}
	c = &Client{helloRole: "pane", helloPane: "w1:p9"}
	if ro := c.roleOf(); ro.paneID != "w1:p9" {
		t.Fatalf("a hello-only pane lost its id: %+v", ro)
	}
}

// A headless adapter that names its pid is placed like a pty pane.
func TestPanePIDsNamesAHeadlessAdapterWithAPid(t *testing.T) {
	s := newTestServer(t)
	s.liveMu.Lock()
	s.live["w1:p4"] = &LivePane{Adapter: pidProc{pids: []int{4241, 4242}}}
	s.liveMu.Unlock()
	// The fake entry has no process to stop, so it leaves before Close.
	t.Cleanup(func() {
		s.liveMu.Lock()
		delete(s.live, "w1:p4")
		s.liveMu.Unlock()
	})
	if got := s.panePIDs(); got[4241] != "w1:p4" || got[4242] != "w1:p4" {
		t.Fatalf("panePIDs = %v", s.panePIDs())
	}
}

type pidProc struct {
	pane.Proc
	pids []int
}

func (p pidProc) Pids() []int { return p.pids }

// A pane reports state only for itself. It cannot clear another pane's
// blocked state and then type into it.
func TestAPaneReportsStateOnlyForItself(t *testing.T) {
	s := newAgentServer(t)
	dir := t.TempDir()
	roundTrip(t, s,
		strings.Replace(paneCreate, "%s", dir, 1),
		strings.Replace(strings.Replace(paneCreate, "%s", dir, 1), `"id":"1"`, `"id":"2"`, 1),
		gateBlockedLine("w1:p1"))
	send, next, stop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p2"})
	defer stop()
	clear := `{"id":"1","cmd":"pane.report_state","pane":"w1:p1","event":{"v":1,"ts":1,"session_id":"s",` +
		`"harness":"claude","pane":"w1:p1","state":"idle","source":"gate"}}`
	send(clear)
	refusedWith(t, next())
	send(`{"id":"2","cmd":"pane.send_keys","pane":"w1:p1","keys":["1","enter"]}`)
	refusedWith(t, next())
	send(`{"id":"3","cmd":"pane.report_child","pane":"w1:p1","child":"a1","state":"working"}`)
	refusedWith(t, next())
	own := strings.ReplaceAll(clear, "w1:p1", "w1:p2")
	send(own)
	if r := next(); r["ok"] != true {
		t.Fatalf("a pane's report on itself was refused: %v", r)
	}
}

// A hello-only pane is held to the pane its hello names.
func TestAHelloPaneReportsStateOnlyForItsPane(t *testing.T) {
	s := newAgentServer(t)
	dir := t.TempDir()
	roundTrip(t, s,
		strings.Replace(paneCreate, "%s", dir, 1),
		strings.Replace(strings.Replace(paneCreate, "%s", dir, 1), `"id":"1"`, `"id":"2"`, 1))
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"0","cmd":"hello","role":"pane","pane":"w1:p2"}`)
	next()
	send(`{"id":"1","cmd":"pane.report_state","pane":"w1:p1","event":{"v":1,"ts":1,"session_id":"s",` +
		`"harness":"claude","pane":"w1:p1","state":"idle","source":"gate"}}`)
	refusedWith(t, next())
}

// A harness may hold two asks at once. The operator answers either one,
// on the pane that holds it, while it is live.
func TestAnAllowAnswersAnyLiveAskOfThePane(t *testing.T) {
	s := newAgentServer(t)
	plantAsk(t, s.cfg.GateRoot, "ask-3")
	plantAsk(t, s.cfg.GateRoot, "ask-4")
	plantAsk(t, s.cfg.GateRoot, "ask-5")
	dir := t.TempDir()
	expired := strings.Replace(strings.Replace(gateBlockedLine("w1:p1"), "toolu_1", "ask-5", 1),
		"9999999999.0", "1.0", 1)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", dir, 1),
		strings.Replace(strings.Replace(paneCreate, "%s", dir, 1), `"id":"1"`, `"id":"2"`, 1),
		expired,
		strings.Replace(gateBlockedLine("w1:p1"), "toolu_1", "ask-3", 1),
		strings.Replace(gateBlockedLine("w1:p1"), "toolu_1", "ask-4", 1),
		`{"id":"5","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","confirm":"auth fix"}`,
		`{"id":"6","cmd":"agent.deny","pane":"w1:p2","ask":"ask-4"}`,
		`{"id":"7","cmd":"agent.deny","pane":"w1:p1","ask":"ask-4"}`,
		`{"id":"8","cmd":"agent.allow","pane":"w1:p1","ask":"ask-5","confirm":"auth fix"}`)
	if !got[5].OK {
		t.Fatalf("the older live ask was refused: %+v", got[5].Error)
	}
	if got[6].OK || got[6].Error.Code != "bad_request" {
		t.Fatalf("an ask answered on the wrong pane: %+v", got[6])
	}
	if !got[7].OK {
		t.Fatalf("the newer live ask was refused: %+v", got[7].Error)
	}
	if got[8].OK || got[8].Error.Code != "bad_request" {
		t.Fatalf("an expired ask was answered: %+v", got[8])
	}
}
