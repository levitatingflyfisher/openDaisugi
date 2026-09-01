package server

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/detect"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// s.tickCounts() is a per-Server total across every pane the tick has ever
// visited, not a per-pane count. Every tickCounts() assertion in this file
// is sound only because each of those tests creates exactly one pane -
// scanned/skipped attributed to "this pane" is really "the whole Server",
// and a future test that creates two or more panes cannot reuse this
// assertion shape and expect it to still prove anything about either one
// individually.
func newDetectServer(t *testing.T) (*Server, *detect.Set) {
	t.Helper()
	toolchain.RequireOrSkip(t)
	s := newTestServer(t)
	s.RegisterPaneCommands()
	s.RegisterAttachCommands()
	s.RegisterAgentCommands()
	set, err := detect.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	s.RegisterExplainCommand(set)
	return s, set
}

// A pane running a harness we have a manifest for, showing a screen the
// manifest calls idle, becomes idle from the manifest source.
func TestManifestTickSetsStateForAPaneWithNoBetterSource(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	s.StartManifestTick(set)
	defer s.StopManifestTick()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev, ok := s.States().Current("w1:p1"); ok && ev.Source == proto.SrcManifest {
			if ev.State != proto.StateIdle {
				t.Fatalf("manifest set state %s, want idle for a prompt box", ev.State)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the manifest tick never produced a state within 5 s")
}

// The gate outranks the screen. What this test can actually prove is
// narrower than it looks: state.Merge's own rule 6 (and, for THIS fixture,
// rule 4 - see below) would hold the merged state at gate/blocked on its
// own, whether or not scanOnce ever read the screen at all. A merged state
// that never changed is consistent with "scanOnce skipped it" AND with
// "scanOnce read it every 500 ms and lost every time" - Merge produces the
// identical outward result either way. The tickCounts() check below is what
// actually distinguishes the two: skipped increasing while scanned stays at
// zero proves scanOnce's own up-front skip fired and the screen was never
// read, not merely that whatever it said was discarded downstream.
//
// gateBlockedLine's ask carries deadline 9999999999.0 (the year 2286), so
// for the whole life of this test the operative rule is tickSkipReason's
// gate-ask-hold branch (Merge rule 4), not the two-second
// GateQuiet window (Merge rule 6) - either way scanOnce must skip, which is
// what the fixture's timestamp comment below already establishes.
func TestAPaneWithAFreshGateSourceIsNotScanned(t *testing.T) {
	// gateBlockedLine carries ts 1757300000.0, about a year in the past. This
	// test must pass because the STORE recorded the arrival just now, not
	// because the event claimed a recent timestamp.
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		gateBlockedLine("w1:p1"))
	s.StartManifestTick(set)
	defer s.StopManifestTick()

	time.Sleep(1200 * time.Millisecond)
	ev, ok := s.States().Current("w1:p1")
	if !ok || ev.Source != proto.SrcGate || ev.State != proto.StateBlocked {
		t.Fatalf("state = %+v, want the gate's blocked to still stand", ev)
	}
	if scanned, skipped := s.tickCounts(); scanned != 0 || skipped == 0 {
		t.Fatalf("tick counts = scanned=%d skipped=%d, want scanOnce to have skipped "+
			"this pane's screen entirely rather than read it and lose to Merge",
			scanned, skipped)
	}
}

// The other half of the same rule, and the one a "never scan a gate pane again"
// shortcut would break: a harness that stops calling the gate must recover, not
// freeze on its last gate state for the rest of the session.
//
// Like its sibling above, the eventual state transition alone is not proof
// that scanOnce paused during the first two seconds: state.Merge's rule 6 is
// coextensive with scanOnce's own up-front skip here (both gate on the
// identical receive-clock GateQuiet window), so Merge would produce the same
// eventual transition to manifest even if scanOnce read the screen and lost
// every time until the window elapsed. The mid-wait tickCounts() check is
// what actually proves an early tick was skipped, not merely outraced.
func TestAPaneWhoseGateHasGoneQuietIsScannedAgain(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\n❯ \n────────────────────\n'; sleep 10"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`)
	// A gate WORKING report, not blocked: a gate hold on blocked is a
	// separate rule (tickSkipReason's gate-ask-hold branch) and
	// stands until the gate clears it, unbounded by GateQuiet - this test is
	// about the ordinary window instead.
	id := "w1:p1"
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude", Pane: &id,
		State: proto.StateWorking, Source: proto.SrcGate, Detail: "verdict=allow",
	})
	s.StartManifestTick(set)
	defer s.StopManifestTick()

	// Inside GateQuiet: at least one tick (500 ms) has run by 900 ms, and
	// scanOnce must have skipped this pane's screen without reading it.
	time.Sleep(900 * time.Millisecond)
	if scanned, skipped := s.tickCounts(); scanned != 0 || skipped == 0 {
		t.Fatalf("tick counts = scanned=%d skipped=%d, want scanOnce to have skipped "+
			"this pane's screen during the hold, not read it and lose to Merge",
			scanned, skipped)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ev, ok := s.States().Current(id); ok && ev.Source == proto.SrcManifest {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the screen never spoke again after the gate went quiet for more than 2 s")
}

func TestAPaneWithNoHarnessIsNotScanned(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`)
	s.StartManifestTick(set)
	defer s.StopManifestTick()
	time.Sleep(1200 * time.Millisecond)
	if ev, ok := s.States().Current("w1:p1"); ok && ev.Source == proto.SrcManifest {
		t.Fatalf("a pane with no harness was scanned anyway: %+v", ev)
	}
}

// tickSkipReason must mirror scanOnce's own
// rec.Kind != KindPTY filter, not just the source/gate holds it already
// knew about - a headless pane's state comes from its adapter's events
// (source headless), never from reading its grid against a manifest.
//
// The pane here is a real pty (so it gets a real live grid and PTY), then
// flipped to headless in the tree record via updatePane - the same
// field-level edit path every other mutation in this package uses - so this
// exercises tickSkipReason/scanOnce's OWN kind check without needing a live
// adapter behind it.
func TestAHeadlessPaneIsNeverScannedByTheManifestTick(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"❯","timeout_ms":5000}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	if !got[1].OK {
		t.Fatalf("pane.wait_output failed: %+v", got[1].Error)
	}
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Kind = layout.KindHeadless }); err != nil {
		t.Fatal(err)
	}

	s.StartManifestTick(set)
	defer s.StopManifestTick()
	time.Sleep(1200 * time.Millisecond)
	if ev, ok := s.States().Current("w1:p1"); ok && ev.Source == proto.SrcManifest {
		t.Fatalf("a headless pane was scanned against the manifest anyway: %+v", ev)
	}
}

// pane.explain's tick field is the identical decision scanOnce itself makes
// (tickSkipReason), so a headless pane must read the same skip reason there
// too, not just inside the tick loop nothing here can observe directly.
func TestExplainShowsAHeadlessPaneIsSkippedByTheTick(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"❯","timeout_ms":5000}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	if !got[1].OK {
		t.Fatalf("pane.wait_output failed: %+v", got[1].Error)
	}
	if err := s.updatePane("w1:p1", func(p *layout.Pane) { p.Kind = layout.KindHeadless }); err != nil {
		t.Fatal(err)
	}

	got2 := roundTrip(t, s, `{"id":"3","cmd":"pane.explain","pane":"w1:p1"}`)
	if !got2[0].OK {
		t.Fatalf("pane.explain failed: %+v", got2[0].Error)
	}
	m := result(t, got2[0])
	if m["tick"] != "skipped: headless pane, not scanned" {
		t.Fatalf("tick = %v, want the headless skip reason", m["tick"])
	}
}

func TestExplainListsTheMatchedRuleAndTheRegionItRead(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"❯","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.explain","pane":"w1:p1"}`)
	if !got[2].OK {
		t.Fatalf("pane.explain failed: %+v", got[2].Error)
	}
	m := result(t, got[2])
	if m["agent"] != "claude" {
		t.Fatalf("explain agent = %v, want claude", m["agent"])
	}
	if m["rule_id"] == "" || m["rule_id"] == nil {
		t.Fatalf("explain named no rule: %v", m)
	}
	rules, _ := m["evaluated"].([]any)
	if len(rules) < 5 {
		t.Fatalf("explain listed %d rules, want every rule in the manifest", len(rules))
	}
	if _, ok := m["detection_text"]; !ok {
		t.Fatalf("explain does not show the detection window it read: %v", m)
	}
}

func TestExplainForAPaneWithNoManifestSaysSoPlainly(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c","sleep 5"],"kind":"pty"}`,
		`{"id":"2","cmd":"pane.explain","pane":"w1:p1"}`)
	if !got[1].OK {
		t.Fatalf("pane.explain failed: %+v", got[1].Error)
	}
	m := result(t, got[1])
	note, _ := m["note"].(string)
	if !strings.Contains(note, "harness") {
		t.Fatalf("note = %q, want it to say this pane has no harness to detect", note)
	}
	// The doc comment promises tick reports the identical
	// tickSkipReason decision scanOnce itself would make right now, but the
	// no-harness early return used to skip setting it at all.
	if _, ok := m["tick"]; !ok {
		t.Fatalf("explain omits tick for a pane with no harness: %v", m)
	}
}

// The command this exists for: a gate hold masking a screen that has moved
// on. state.EffectiveState of the current event says blocked (the gate's
// ask has not expired, so nothing releases it) while the manifest, reading
// the very same screen scanOnce would, calls the same pane idle - the two
// verdicts are MEANT to disagree here, and pane.explain must show both side
// by side rather than picking one and hiding the other.
func TestExplainShowsTheEffectiveStateAndTheManifestVerdictSideBySide(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"❯","timeout_ms":5000}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	if !got[1].OK {
		t.Fatalf("pane.wait_output failed: %+v", got[1].Error)
	}

	id := "w1:p1"
	s.ApplyState(id, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), SessionID: id, Harness: "claude", Pane: &id,
		State: proto.StateBlocked, Source: proto.SrcGate,
		Ask: &proto.Ask{ID: "toolu_1", Tool: "Bash", Summary: "rm -rf build/",
			Deadline: nowSeconds() + 300},
		Detail: "verdict=deny",
	})

	got2 := roundTrip(t, s, `{"id":"3","cmd":"pane.explain","pane":"w1:p1"}`)
	if !got2[0].OK {
		t.Fatalf("pane.explain failed: %+v", got2[0].Error)
	}
	m := result(t, got2[0])
	if m["effective_state"] != proto.StateBlocked {
		t.Fatalf("effective_state = %v, want blocked (the gate's ask has not expired)", m["effective_state"])
	}
	if m["effective_source"] != proto.SrcGate {
		t.Fatalf("effective_source = %v, want gate", m["effective_source"])
	}
	if m["manifest_state"] != proto.StateIdle {
		t.Fatalf("manifest_state = %v, want idle (the screen alone is a prompt box)", m["manifest_state"])
	}
	tick, _ := m["tick"].(string)
	if !strings.HasPrefix(tick, "skipped: gate blocked") {
		t.Fatalf("tick = %q, want it to start with %q", tick, "skipped: gate blocked")
	}
	if _, ok := m["received_age_s"]; !ok {
		t.Fatalf("explain does not show received_age_s: %v", m)
	}
	if _, ok := m["detection_warnings"]; !ok {
		t.Fatalf("explain does not echo detection_warnings: %v", m)
	}
}

// The evaluator sees the whole
// unwrapped screen, exactly as Herdr does, and each rule's own region does
// its own slicing from there. The old fixed 12-line window silently
// narrowed that input: 43 of the 21 vendored manifests' rules ask for more
// than 12 lines, and a phrase sitting on line 15 from the bottom of a
// 25-line screen is exactly the kind of match the old window could never
// see, even though a rule's own bottom_non_empty_lines(20) region asks for
// it. This is RED against the old 12-line window and GREEN once
// pane.ReadDetection hands the evaluator the whole screen.
func TestDetectionSeesTheWholeScreenNotJustTheBottomTwelveLines(t *testing.T) {
	toolchain.RequireOrSkip(t)
	g, err := pane.NewGrid(40, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for i := 1; i <= 25; i++ {
		line := fmt.Sprintf("line%02d", i)
		if i == 11 { // the 11th line from the top is the 15th from the bottom of 25
			line = "needle-phrase"
		}
		if _, err := g.Write([]byte(line + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	text, err := g.Read(pane.ReadDetection)
	if err != nil {
		t.Fatal(err)
	}

	_, c, err := detect.Parse("test", []byte(`
id = "x"
min_engine_version = 3
[[rules]]
id = "wide_region"
state = "working"
region = "bottom_non_empty_lines(20)"
contains = ["needle-phrase"]
`))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Evaluate(detect.Input{Screen: text})
	if !r.Matched {
		t.Fatalf("no rule matched a phrase 15 lines from the bottom under a 20-line region: %+v", r)
	}
}

func TestReadDetectionReturnsTheSameTextExplainUsed(t *testing.T) {
	s, _ := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf 'hello\\n'; sleep 5"],"kind":"pty","harness":"claude","cols":40,"rows":10}`,
		`{"id":"2","cmd":"pane.wait_output","pane":"w1:p1","contains":"hello","timeout_ms":5000}`,
		`{"id":"3","cmd":"pane.read","pane":"w1:p1","source":"detection"}`,
		`{"id":"4","cmd":"pane.explain","pane":"w1:p1"}`)
	read := result(t, got[2])["text"]
	explained := result(t, got[3])["detection_text"]
	if read != explained {
		t.Fatalf("pane.read --source detection = %q but explain read %q. "+
			"They must be the same text or the debugging window lies.", read, explained)
	}
}

// --- Lifecycle: the tick goroutine must not outlive Stop or Close ---------
//
// The four tests below share waitForGoroutineBaseline, defined in
// attach_test.go for the frame pump tests. This file is its second consumer;
// if a later task ever moves or renames that helper, these four break too.

// TestStopManifestTickEndsTheGoroutine proves StartManifestTick's goroutine
// actually exits when asked, rather than trusting that a passing assertion
// elsewhere means nothing was left running - the same discipline
// waitForGoroutineBaseline exists for in attach_test.go.
func TestStopManifestTickEndsTheGoroutine(t *testing.T) {
	s, set := newDetectServer(t)
	before := runtime.NumGoroutine()
	s.StartManifestTick(set)
	time.Sleep(50 * time.Millisecond) // let the ticker goroutine actually start
	s.StopManifestTick()
	waitForGoroutineBaseline(t, before, 2*time.Second)
}

// StopManifestTick must tolerate being called with no tick running, and
// twice in a row: Close calls it unconditionally on every server, including
// one whose test never started a tick at all.
func TestStopManifestTickWithNoTickRunningIsANoOp(t *testing.T) {
	s, _ := newDetectServer(t)
	s.StopManifestTick()
	s.StopManifestTick()
}

// TestServerCloseStopsTheManifestTick is the fact behind the "Close also
// stops the tick" half of the deal: a caller that starts a tick and then
// calls Close directly, without ever calling StopManifestTick itself, must
// not leak the goroutine past Close returning.
//
// newTestServer's own t.Cleanup calls Close again after this test's explicit
// call returns, so Close must tolerate running twice without double-closing
// tickStop. It does today only because Close captures tickStop and nils it
// under the SAME s.mu hold that sets s.closed - the second call sees
// s.closed already true and returns before it would ever read tickStop
// again. If that capture and nil ever move outside that lock hold, this
// test starts panicking on close of closed channel instead of failing
// quietly.
func TestServerCloseStopsTheManifestTick(t *testing.T) {
	s, set := newDetectServer(t)
	before := runtime.NumGoroutine()
	s.StartManifestTick(set)
	time.Sleep(50 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitForGoroutineBaseline(t, before, 2*time.Second)
}

// StartManifestTick called twice must not start a second competing
// goroutine: only StopManifestTick's one close(stop) must be needed to bring
// the goroutine count back to baseline.
func TestStartManifestTickTwiceStartsOnlyOneGoroutine(t *testing.T) {
	s, set := newDetectServer(t)
	before := runtime.NumGoroutine()
	s.StartManifestTick(set)
	s.StartManifestTick(set)
	time.Sleep(50 * time.Millisecond)
	s.StopManifestTick()
	waitForGoroutineBaseline(t, before, 2*time.Second)
}

// StartManifestTick after Close must be a no-op: a caller racing Close with
// a start, or simply calling it late, must not spin up a goroutine nothing
// will ever stop - a second Close just takes its early-return branch without
// ever touching tickStop again (see Close in server.go), so a tick started
// after the first Close would run forever.
func TestStartManifestTickAfterCloseIsANoOp(t *testing.T) {
	s, set := newDetectServer(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before := runtime.NumGoroutine()
	s.StartManifestTick(set)
	time.Sleep(150 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("StartManifestTick after Close started a goroutine: before=%d now=%d", before, n)
	}
}

// A pane.close is not the only way a Grid can close. This reaches
// Grid.Close directly, with the tree record left open (Closed: false) and
// the LivePane left in place, to isolate exactly that shape: only the grid
// itself is gone. scanOnce must not panic when Read returns ErrGridClosed,
// and must not apply a manifest state it could not possibly have read.
func TestScanOnceToleratesAClosedGridWithTheRecordStillOpen(t *testing.T) {
	s, set := newDetectServer(t)
	cwd := t.TempDir()
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+cwd+`","cmd_argv":["sh","-c",`+
			`"printf '────────────────────\\n❯ \\n────────────────────\\n'; sleep 5"],`+
			`"kind":"pty","harness":"claude","cols":40,"rows":10}`)
	if !got[0].OK {
		t.Fatalf("pane.create failed: %+v", got[0].Error)
	}
	lp, ok := s.Live("w1:p1")
	if !ok {
		t.Fatal("pane not live right after create")
	}
	lp.Grid.Close()

	s.StartManifestTick(set)
	defer s.StopManifestTick()
	// Several ticks: a panic in the tick goroutine would already have
	// crashed this test binary well before this returns.
	time.Sleep(1200 * time.Millisecond)

	if ev, ok := s.States().Current("w1:p1"); ok && ev.Source == proto.SrcManifest {
		t.Fatalf("a closed grid still produced a manifest state: %+v", ev)
	}
	// tickSkipReason found no reason to hold this pane back (no current
	// state exists yet), so every tick reached detectionInput and lost there
	// - recordTickScanned only fires once the screen is actually read, so
	// this must stay at zero, and recordTickSkipped never fires for this
	// pane at all (it is not what tickSkipReason's skip reasons are for).
	if scanned, skipped := s.tickCounts(); scanned != 0 {
		t.Fatalf("tick counts = scanned=%d skipped=%d, want scanned=0: a closed grid was never "+
			"actually read, so counting it as scanned would misreport what happened", scanned, skipped)
	}
}
