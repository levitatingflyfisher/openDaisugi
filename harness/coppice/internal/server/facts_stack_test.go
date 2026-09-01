//go:build unix

package server

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// writeCoppiceToml writes the coppice.toml the server reads, under the
// XDG_CONFIG_HOME the test set.
func writeCoppiceToml(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "coppice")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coppice.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// routerServer is a server with an empty environment of its own and an
// empty config directory.
func routerServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newTestServer(t)
	s.getenv = func(string) string { return "" }
	return s
}

func routerFor(s *Server, env map[string]string) any {
	row := s.paneListRow(layout.Pane{ID: "w9:p1", Kind: layout.KindPTY, Env: env})
	st, _ := row["stack"].(map[string]any)
	return st["router"]
}

func TestTheRouterIsAbsentWhenNoBaseURLIsSet(t *testing.T) {
	s := routerServer(t)
	if r := routerFor(s, nil); r != nil {
		t.Fatalf("router %v, want absent", r)
	}
}

func TestABaseURLAtTheDefaultGatewayIsTheGateway(t *testing.T) {
	s := routerServer(t)
	for _, env := range []map[string]string{
		{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"},
		{"ANTHROPIC_BASE_URL": "http://localhost:8787/"},
		{"OPENAI_BASE_URL": "http://127.0.0.1:8787/v1"},
	} {
		if r := routerFor(s, env); r != "gateway" {
			t.Errorf("%v: router %v, want gateway", env, r)
		}
	}
}

func TestAnyOtherBaseURLIsDirect(t *testing.T) {
	s := routerServer(t)
	for _, env := range []map[string]string{
		{"ANTHROPIC_BASE_URL": "https://api.example.invalid"},
		{"ANTHROPIC_BASE_URL": "http://127.0.0.1:9999"},
		{"OPENAI_BASE_URL": "not a url"},
	} {
		if r := routerFor(s, env); r != "direct" {
			t.Errorf("%v: router %v, want direct", env, r)
		}
	}
}

func TestThePanesOwnEnvWinsOverTheServers(t *testing.T) {
	s := routerServer(t)
	s.getenv = func(k string) string {
		if k == "ANTHROPIC_BASE_URL" {
			return "http://127.0.0.1:8787"
		}
		return ""
	}
	if r := routerFor(s, nil); r != "gateway" {
		t.Fatalf("inherited: router %v, want gateway", r)
	}
	if r := routerFor(s, map[string]string{"ANTHROPIC_BASE_URL": "https://api.example.invalid"}); r != "direct" {
		t.Fatalf("pane env: router %v, want direct", r)
	}
	if r := routerFor(s, map[string]string{"ANTHROPIC_BASE_URL": ""}); r != nil {
		t.Fatalf("pane env set empty: router %v, want absent", r)
	}
}

func TestTheGatewayKeyMovesTheGatewayAndEmptyMeansNone(t *testing.T) {
	s := routerServer(t)
	writeCoppiceToml(t, "gateway = \"http://127.0.0.1:9999\"\n")
	if r := routerFor(s, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:9999"}); r != "gateway" {
		t.Fatalf("router %v, want gateway", r)
	}
	s.outside = outsideCache{}
	writeCoppiceToml(t, "gateway = \"\"\n")
	if r := routerFor(s, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"}); r != nil {
		t.Fatalf("router %v, want absent with no gateway", r)
	}
}

func TestAGatewaySetToRunSwitchyardIsSwitchyard(t *testing.T) {
	s := routerServer(t)
	cfg := filepath.Join(filepath.Dir(s.cfg.GateRoot), "config.yaml")
	if err := os.WriteFile(cfg, []byte("gate_mode: enforce\ngateway_router: \"switchyard\"  # fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := routerFor(s, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"}); r != "switchyard" {
		t.Fatalf("router %v, want switchyard", r)
	}
}

func TestFloorFactsSumsTheFloor(t *testing.T) {
	s := newFactsServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	writeCoppiceToml(t, "gateway = \"http://"+ln.Addr().String()+"\"\n")

	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	if err := s.updatePane("w1:p2", func(p *layout.Pane) { p.Harness = "claude" }); err != nil {
		t.Fatal(err)
	}
	p := projFixture(t, s, "claude-transcript.jsonl")
	send, next, stop := ownPane(t, s, "w1:p1")
	defer stop()
	send(gateReportLine("1", "w1:p1", `"mode":"enforcing","transcript_path":"`+p+`"`))
	mustOK(t, next())
	got := roundTrip(t, s, gateBlockedLine("w1:p2"), `{"id":"f","cmd":"floor.facts"}`)
	if !got[0].OK {
		t.Fatalf("blocked report: %+v", got[0].Error)
	}
	m := result(t, got[1])
	d, _ := m["daisugi"].(map[string]any)
	// One pane enforces and one has no gate: the floor claims no more than
	// its least guarded agent.
	if d["mode"] != "off" || d["armed"] != true || d["enforcing"] != 1.0 || d["off"] != 1.0 {
		t.Fatalf("daisugi %v", d)
	}
	if m["working"] != 1.0 || m["needing_you"] != 1.0 {
		t.Fatalf("working %v needing_you %v, want 1 and 1", m["working"], m["needing_you"])
	}
	gw, _ := m["gateway"].(map[string]any)
	if gw["answers"] != true {
		t.Fatalf("gateway %v, want it to answer", gw)
	}
	tok, _ := m["tokens_today"].(map[string]any)
	if tok["out"] != 12.0 || tok["cache_read"] != 300.0 {
		t.Fatalf("tokens_today %v", tok)
	}
}

func TestFloorFactsSaysAClosedGatewayDoesNotAnswer(t *testing.T) {
	s := routerServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	writeCoppiceToml(t, "gateway = \"http://"+addr+"\"\n")
	m := result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	gw, _ := m["gateway"].(map[string]any)
	if gw["answers"] != false {
		t.Fatalf("gateway %v, want answers false", gw)
	}
	d, _ := m["daisugi"].(map[string]any)
	if d["mode"] != "off" {
		t.Fatalf("an empty floor has daisugi %v, want off", d)
	}
}

func TestFloorFactsNeverDialsARemoteGateway(t *testing.T) {
	s := routerServer(t)
	writeCoppiceToml(t, "gateway = \"http://192.0.2.1:8787\"\n")
	m := result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	gw, _ := m["gateway"].(map[string]any)
	if _, ok := gw["answers"]; ok {
		t.Fatalf("gateway %v, want no answers key for a remote address", gw)
	}
}

// With no gateway to compare with, a base URL at the vendor's own API is
// still plainly direct. Anything else is not known.
func TestWithNoGatewayTheVendorsOwnAPIIsDirect(t *testing.T) {
	s := routerServer(t)
	writeCoppiceToml(t, "gateway = \"\"\n")
	if r := routerFor(s, map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}); r != "direct" {
		t.Fatalf("router %v, want direct", r)
	}
	if r := routerFor(s, map[string]string{"OPENAI_BASE_URL": "https://api.openai.com/v1"}); r != "direct" {
		t.Fatalf("router %v, want direct", r)
	}
	if r := routerFor(s, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000"}); r != nil {
		t.Fatalf("router %v, want absent", r)
	}
}

// floor.facts counts "needs you" exactly as every floor counts it from
// pane.list: a blocked pane whose ask no foreman holds. A held ask counts
// as working. A fork pane counts like any pane, and a block the screen
// manifest found counts like one the gate found.
func TestFloorFactsNeedsYouMatchesTheRail(t *testing.T) {
	s := newFactsServer(t)
	held := createShellPane(t, s, "sh", "-c", "sleep 30")
	screen := createShellPane(t, s, "sh", "-c", "sleep 30")
	fork := createShellPane(t, s, "sh", "-c", "sleep 30")
	t.Cleanup(func() { closePane(t, s, held); closePane(t, s, screen); closePane(t, s, fork) })
	if err := s.updatePane(fork, func(p *layout.Pane) { p.ParentPane = held }); err != nil {
		t.Fatal(err)
	}
	block := func(id, src string) {
		pid := id
		ev := proto.PaneStateEvent{
			V: 1, TS: nowSeconds(), SessionID: id, Harness: "sh", Pane: &pid,
			State: proto.StateBlocked, Source: src,
		}
		if src == proto.SrcGate {
			ev.Ask = &proto.Ask{ID: "a1", Tool: "Bash", Summary: "rm", Deadline: nowSeconds() + 60}
		}
		if src == proto.SrcManifest {
			// A screen read past the hold window, as the manifest tick
			// sends one once the process fact has gone stale.
			s.states.Apply(id, ev, nowSeconds()+10)
			return
		}
		s.ApplyState(id, ev)
	}
	block(held, proto.SrcGate)
	block(screen, proto.SrcManifest)
	block(fork, proto.SrcGate)
	s.holdsMu.Lock()
	s.holds[held] = &hold{pane: held, ask: "a1", foreman: "w9:p9", task: "t1", id: "h1", since: nowSeconds(), until: nowSeconds() + 60,
		timer: time.AfterFunc(time.Hour, func() {})}
	s.holdsMu.Unlock()

	rail, working := 0, 0
	for _, row := range listPanes(t, s) {
		switch {
		case row["state"] == "blocked" && row["held"] == nil:
			rail++
		case row["state"] == "blocked" || row["state"] == "working":
			working++
		}
	}
	if rail != 2 {
		t.Fatalf("pane.list shows %d panes that need you, want 2 (the screen block and the fork)", rail)
	}
	m := result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	if m["needing_you"] != float64(rail) {
		t.Fatalf("floor.facts needing_you = %v, the rail counts %d", m["needing_you"], rail)
	}
	if m["working"] != float64(working) {
		t.Fatalf("floor.facts working = %v, the rail counts %d", m["working"], working)
	}
}

// floor.facts says whether a daisugi gate hook is installed, read from
// the harness settings in the home, so an empty floor can say how to
// guard the agents before any runs.
func TestFloorFactsSaysWhetherAGateHookIsInstalled(t *testing.T) {
	s := newTestServer(t)
	installed := func() any {
		m := result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
		d, _ := m["daisugi"].(map[string]any)
		return d["installed"]
	}
	if got := installed(); got != false {
		t.Fatalf("no settings: installed %v", got)
	}
	dir := filepath.Join(s.cfg.HookHome, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := `{"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "DAISUGI_GATE_HOOK=opendaisugi.gate /x/daisugi gate check --mode shadow"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(hook), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := installed(); got != true {
		t.Fatalf("a gate hook: installed %v", got)
	}
}
