package tui

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// m sets the selected agent's daisugi mode (F11): watching asks once
// first, enforcing does not, and off is never a choice.

// fakeModeServer answers every request ok with an empty result and keeps
// each agent.set_mode it gets.
func fakeModeServer(t *testing.T) (sock string, got func() []map[string]any) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var sets []map[string]any
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req map[string]any
					_ = json.Unmarshal(sc.Bytes(), &req)
					if req["cmd"] == "agent.set_mode" {
						mu.Lock()
						sets = append(sets, req)
						mu.Unlock()
					}
					_, _ = c.Write([]byte(`{"id":"1","ok":true,"result":{"panes":[]}}` + "\n"))
				}
			}(c)
		}
	}()
	return sock, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), sets...)
	}
}

func TestTheFooterNamesMOnlyOnAGatedAgent(t *testing.T) {
	plain := &Model{Rows: []Row{{ID: "a", State: "working"}}}
	if strings.Contains(footerText(plain), "m mode") {
		t.Fatalf("a row with no gate names m: %q", footerText(plain))
	}
	gated := &Model{Rows: []Row{{ID: "a", State: "working", Mode: "enforcing"}}}
	if !strings.Contains(footerText(gated), "ctrl-w stop  m mode") {
		t.Fatalf("a gated row does not name m: %q", footerText(gated))
	}
}

func TestStackLineShowsTheMarkerTheOwnerSet(t *testing.T) {
	p := map[string]any{"stack": map[string]any{"loop": "claude",
		"daisugi": map[string]any{"mode": "enforcing", "armed": true, "marker": "watching"}}}
	if got := stackText(p); got != "claude · daisugi enforcing → watching" {
		t.Fatalf("stack line %q", got)
	}
	p["stack"].(map[string]any)["daisugi"] = map[string]any{"mode": "watching", "armed": true, "marker": "watching"}
	if got := stackText(p); got != "claude · daisugi watching" {
		t.Fatalf("stack line %q", got)
	}
	if mode, marker := daisugiOf(p); mode != "watching" || marker != "watching" {
		t.Fatalf("daisugiOf = %q %q", mode, marker)
	}
	if mode, marker := daisugiOf(map[string]any{}); mode != "" || marker != "" {
		t.Fatalf("daisugiOf of nothing = %q %q", mode, marker)
	}
}

func TestMAsksOnceThenEnterLowersTheAgentToWatching(t *testing.T) {
	sock, got := fakeModeServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Mode: "enforcing"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 'm'})
	if m.Confirm != "w1:p1" || !m.ConfirmMode {
		t.Fatalf("m did not ask: confirm %q mode %v", m.Confirm, m.ConfirmMode)
	}
	if want := "Watch only? The gate will log the calls of auth and stop none. While it watches, it can also change the gate for other agents. Enter yes, Esc no."; m.PromptLine() != want {
		t.Fatalf("prompt %q, want %q", m.PromptLine(), want)
	}
	if len(got()) != 0 {
		t.Fatalf("m set the mode before it asked: %v", got())
	}
	handleAll(t, f, key{kind: keyByte, b: '\r'})
	sets := got()
	if len(sets) != 1 || sets[0]["pane"] != "w1:p1" || sets[0]["mode"] != "watching" {
		t.Fatalf("set_mode requests %v", sets)
	}
	if m.Confirm != "" || m.ConfirmMode {
		t.Fatalf("the question stayed: %q %v", m.Confirm, m.ConfirmMode)
	}
}

func TestMThenEscKeepsEnforcing(t *testing.T) {
	sock, got := fakeModeServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", State: "idle", Mode: "enforcing"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 'm'}, key{kind: keyEsc})
	if len(got()) != 0 || m.Confirm != "" || m.ConfirmMode {
		t.Fatalf("Esc set a mode: %v, confirm %q", got(), m.Confirm)
	}
	// ctrl-w after an Esc asks the stop question, not the mode question.
	handleAll(t, f, key{kind: keyByte, b: 0x17})
	if !strings.HasPrefix(m.PromptLine(), "Stop ") {
		t.Fatalf("prompt %q", m.PromptLine())
	}
}

func TestMOnAWatchingAgentEnforcesAtOnceAndOffIsNeverOffered(t *testing.T) {
	sock, got := fakeModeServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Mode: "enforcing", Marker: "watching"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 'm'})
	sets := got()
	if len(sets) != 1 || sets[0]["mode"] != "enforcing" || m.Confirm != "" {
		t.Fatalf("set_mode requests %v, confirm %q", sets, m.Confirm)
	}
	if m.Message != "auth goes back to the gate's own mode from its next tool call." {
		t.Fatalf("status %q", m.Message)
	}
	off := &Model{Rows: []Row{{ID: "w1:p2", Label: "plain", State: "idle", Mode: "off"}}}
	fo := testFloor(off, 120)
	fo.o.Socket = sock
	handleAll(t, fo, key{kind: keyByte, b: 'm'})
	if off.Confirm != "" || off.Message != ModeNoGate("plain") || len(got()) != 1 {
		t.Fatalf("m on an agent with no gate: confirm %q message %q", off.Confirm, off.Message)
	}
}

// F11-R-14: when the whole gate watches (the hook runs in audit), m offers
// nothing to lower and the footer does not name it.
func TestMOnAnAgentTheWholeGateWatchesLowersNothing(t *testing.T) {
	sock, got := fakeModeServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Mode: "watching"}}}
	f := testFloor(m, 120)
	f.o.Socket = sock
	if strings.Contains(footerText(m), "m mode") {
		t.Fatalf("the footer names m: %q", footerText(m))
	}
	handleAll(t, f, key{kind: keyByte, b: 'm'})
	if m.Confirm != "" || len(got()) != 0 {
		t.Fatalf("m acted: confirm %q requests %v", m.Confirm, got())
	}
	if m.Message != GateWatches("auth") {
		t.Fatalf("message %q", m.Message)
	}
}

// F11-R-15: when the installed hook names another gate root, m says so and
// sets nothing.
func TestMSetsNothingWhenTheHookReadsAnotherRoot(t *testing.T) {
	sock, got := fakeModeServer(t)
	m := &Model{Rows: []Row{{ID: "w1:p1", Label: "auth", State: "idle", Mode: "enforcing"}},
		Facts: decodeFacts(map[string]any{"daisugi": map[string]any{"hook_root": "/elsewhere/gate"}})}
	f := testFloor(m, 120)
	f.o.Socket = sock
	handleAll(t, f, key{kind: keyByte, b: 'm'})
	if m.Confirm != "" || len(got()) != 0 || m.Message != HookRootLine("/elsewhere/gate") {
		t.Fatalf("confirm %q requests %v message %q", m.Confirm, got(), m.Message)
	}
}
