package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

// fitFloor makes a server with one 40 by 10 pane and two connections to it.
// Each connection gets a call function that sends a request and returns its
// reply, skipping the events that arrive on the way.
type fitConn func(line, id string) map[string]any

func fitFloor(t *testing.T) (a, b fitConn, size func() [2]float64, stopA, stopB func(), srv *Server) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	srv = s
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	call := func(send func(string), next func() map[string]any) fitConn {
		return func(line, id string) map[string]any {
			t.Helper()
			send(line)
			for {
				if m := next(); m["id"] == id {
					return m
				}
			}
		}
	}
	sa, na, stopA := stream(t, s)
	sb, nb, stopB := stream(t, s)
	a, b = call(sa, na), call(sb, nb)
	if m := a(`{"id":"c","cmd":"pane.create","cwd":"`+t.TempDir()+
		`","cmd_argv":["sh","-c","exec sleep 600"],"kind":"pty","cols":40,"rows":10}`, "c"); m["ok"] != true {
		t.Fatalf("pane.create failed: %v", m)
	}
	size = func() [2]float64 {
		t.Helper()
		m := b(`{"id":"i","cmd":"pane.list"}`, "i")
		res, _ := m["result"].(map[string]any)
		rows, _ := res["panes"].([]any)
		for _, r := range rows {
			p, _ := r.(map[string]any)
			if p["id"] == "w1:p1" {
				return [2]float64{asNum(p["cols"]), asNum(p["rows"])}
			}
		}
		t.Fatalf("pane.list has no w1:p1: %v", m)
		return [2]float64{}
	}
	return
}

func asNum(v any) float64 { f, _ := v.(float64); return f }

func TestAttachFitSizesTheAgentOnlyWhenAloneAndPutsTheSizeBackOnDetach(t *testing.T) {
	a, _, size, stopA, stopB, _ := fitFloor(t)
	defer stopA()
	defer stopB()
	m := a(`{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "1")
	res, _ := m["result"].(map[string]any)
	if m["ok"] != true || res["fitted"] != true {
		t.Fatalf("attach with fit alone: %v", m)
	}
	if got := size(); got != [2]float64{50, 30} {
		t.Fatalf("fitted size = %v, want 50x30", got)
	}
	// A second fit from the same client moves the size and keeps the old one.
	m = a(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":44,"rows":20}}`, "2")
	res, _ = m["result"].(map[string]any)
	if res["fitted"] != true {
		t.Fatalf("refit: %v", m)
	}
	if got := size(); got != [2]float64{44, 20} {
		t.Fatalf("refitted size = %v, want 44x20", got)
	}
	a(`{"id":"3","cmd":"pane.detach","pane":"w1:p1"}`, "3")
	if got := size(); got != [2]float64{40, 10} {
		t.Fatalf("size after detach = %v, want the old 40x10", got)
	}
}

func TestAttachFitDoesNothingWhileAnotherClientIsAttached(t *testing.T) {
	a, b, size, stopA, stopB, _ := fitFloor(t)
	defer stopA()
	defer stopB()
	a(`{"id":"1","cmd":"pane.attach","pane":"w1:p1","view_only":true}`, "1")
	m := b(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "2")
	res, _ := m["result"].(map[string]any)
	if m["ok"] != true || res["fitted"] != false {
		t.Fatalf("attach with fit beside another client: %v", m)
	}
	if got := size(); got != [2]float64{40, 10} {
		t.Fatalf("size = %v, want 40x10 untouched", got)
	}
	// Detaching the one that did not fit changes nothing.
	b(`{"id":"3","cmd":"pane.detach","pane":"w1:p1"}`, "3")
	if got := size(); got != [2]float64{40, 10} {
		t.Fatalf("size after detach = %v, want 40x10", got)
	}
}

func TestASecondAttachWhileFittedPutsTheOldSizeBackFirst(t *testing.T) {
	a, b, size, stopA, stopB, _ := fitFloor(t)
	defer stopA()
	defer stopB()
	a(`{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "1")
	b(`{"id":"2","cmd":"pane.attach","pane":"w1:p1","view_only":true}`, "2")
	if got := size(); got != [2]float64{40, 10} {
		t.Fatalf("size after a second attach = %v, want the old 40x10", got)
	}
	// The fitted client detaches: its hold is gone, so nothing resizes again.
	a(`{"id":"3","cmd":"pane.detach","pane":"w1:p1"}`, "3")
	if got := size(); got != [2]float64{40, 10} {
		t.Fatalf("size after detach = %v, want 40x10", got)
	}
}

func TestAttachFitNeverResizesAViewAndRefusesBadSizes(t *testing.T) {
	a, _, size, stopA, stopB, _ := fitFloor(t)
	defer stopA()
	defer stopB()
	m := a(`{"id":"1","cmd":"pane.attach","pane":"w1:p1","view_only":true,"fit":{"cols":50,"rows":30}}`, "1")
	res, _ := m["result"].(map[string]any)
	if m["ok"] != true || res["fitted"] != false {
		t.Fatalf("view-only attach with fit: %v", m)
	}
	if got := size(); got != [2]float64{40, 10} {
		t.Fatalf("size = %v, want 40x10", got)
	}
	for i, bad := range []string{`{"cols":0,"rows":5}`, `"x"`, `{"cols":5}`, `{"cols":1001,"rows":5}`, `{"cols":5,"rows":501}`} {
		id := strconv.Itoa(10 + i)
		m := a(`{"id":"`+id+`","cmd":"pane.attach","pane":"w1:p1","fit":`+bad+`}`, id)
		e, _ := m["error"].(map[string]any)
		if m["ok"] != false || e["code"] != "bad_request" {
			t.Fatalf("fit %s: %v, want bad_request", bad, m)
		}
	}
}

func TestADroppedFittedClientPutsTheSizeBack(t *testing.T) {
	a, _, size, stopA, stopB, _ := fitFloor(t)
	defer stopB()
	a(`{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "1")
	if got := size(); got != [2]float64{50, 30} {
		t.Fatalf("fitted size = %v", got)
	}
	stopA()
	deadline := time.Now().Add(5 * time.Second)
	for size() != [2]float64{40, 10} {
		if time.Now().After(deadline) {
			t.Fatalf("size = %v after the client ended, want 40x10", size())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAFitNeverWritesTheRecordOrTheLayoutFile(t *testing.T) {
	a, _, size, stopA, stopB, s := fitFloor(t)
	defer stopA()
	defer stopB()
	a(`{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "1")
	if got := size(); got != [2]float64{50, 30} {
		t.Fatalf("fitted size = %v", got)
	}
	rec, _ := s.tree.Pane("w1:p1")
	if rec.Cols != 40 || rec.Rows != 10 {
		t.Fatalf("the record holds %dx%d while fitted, want the owner's 40x10", rec.Cols, rec.Rows)
	}
	// What a crash leaves on disk is what a restart resumes from.
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "layout.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"cols": 50`) || strings.Contains(string(b), `"cols":50`) {
		t.Fatalf("layout.json holds the phone's size: %s", b)
	}
	a(`{"id":"2","cmd":"pane.detach","pane":"w1:p1"}`, "2")
	rec, _ = s.tree.Pane("w1:p1")
	if rec.Cols != 40 || rec.Rows != 10 {
		t.Fatalf("the record holds %dx%d after detach, want 40x10", rec.Cols, rec.Rows)
	}
}

func TestAPaneResizeEndsAFitAndTheNewSizeIsNotUndone(t *testing.T) {
	a, b, size, stopA, stopB, _ := fitFloor(t)
	defer stopA()
	defer stopB()
	a(`{"id":"1","cmd":"pane.attach","pane":"w1:p1","fit":{"cols":50,"rows":30}}`, "1")
	if m := b(`{"id":"2","cmd":"pane.resize","pane":"w1:p1","cols":60,"rows":20}`, "2"); m["ok"] != true {
		t.Fatalf("pane.resize: %v", m)
	}
	a(`{"id":"3","cmd":"pane.detach","pane":"w1:p1"}`, "3")
	if got := size(); got != [2]float64{60, 20} {
		t.Fatalf("size = %v, want the resize's 60x20 kept", got)
	}
}

// An attach is in place before its save, which comes after the fit lock
// is let go and before the reply (CP-R-53). A detach that arrives while
// the attach waits in that save therefore finds the pane attached: it is
// ok and puts the size back. Once the attach replies, no frame pump, fit
// or subscription of that attach is left.
func TestADetachDuringAHeldAttachSaveIsOkAndLeavesNothing(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	var armed atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	s.beforeDataWrite = func() {
		if armed.Load() && armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	}
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	send, next, stop := stream(t, s)
	defer stop()
	reply := func(id string) map[string]any {
		t.Helper()
		for {
			if m := next(); m["id"] == id {
				return m
			}
		}
	}
	send(`{"id":"c","cmd":"pane.create","cwd":"` + t.TempDir() + `","cmd_argv":["sh","-c","exec sleep 600"],"kind":"pty","cols":40,"rows":10}`)
	reply("c")
	before := runtime.NumGoroutine()
	armed.Store(true)
	send(`{"id":"a","cmd":"pane.attach","pane":"w1:p1","cols":44,"rows":12,"fit":{"cols":50,"rows":30}}`)
	<-entered
	send(`{"id":"d0","cmd":"pane.detach","pane":"w1:p1"}`)
	if m := reply("d0"); m["ok"] != true {
		t.Fatalf("a detach during the attach's save: %v", m)
	}
	lp, _ := s.Live("w1:p1")
	if cols, rows := lp.Grid.Size(); cols != 44 || rows != 12 {
		t.Fatalf("size = %dx%d after the detach, want the explicit 44x12 back", cols, rows)
	}
	close(release)
	if m := reply("a"); m["ok"] != true {
		t.Fatalf("attach: %v", m)
	}
	if s.othersAttached("w1:p1", nil) {
		t.Fatal("the pane is still attached")
	}
	s.fitMu.Lock()
	left := len(s.fits)
	s.fitMu.Unlock()
	if left != 0 {
		t.Fatalf("%d fit holds left", left)
	}
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.mu.Lock()
		flow, sub, auto := c.flows["w1:p1"] != nil, c.SubPanes["w1:p1"], c.autoPanes["w1:p1"]
		c.mu.Unlock()
		if flow || sub || auto {
			t.Fatalf("the attach left flow %v, subscription %v, auto subscription %v", flow, sub, auto)
		}
	}
	// The pump and its drain end on the pump's next tick.
	waitForGoroutineBaseline(t, before, 3*time.Second)
}
