package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// PINS.md is this plan's memory. Every fact a later step reads is written
// there once, from a live source: a module file, a source file, or a
// command's own output. This test reads the same sources and checks each
// recorded fact against them, so a fact that drifts from the tree it
// describes fails here, not in a later step that trusted a stale value.
// A fact with no cheap live source on every box this test runs on stays a
// pinned literal, named as such where it is checked.

const webPinsHeading = "## Web / phone (plan 06)"

// webPinsSection returns the Web / phone section of a PINS.md body, from
// its own heading up to the next level-2 heading or the end of the file.
func webPinsSection(pins string) (string, bool) {
	start := strings.Index(pins, webPinsHeading)
	if start < 0 {
		return "", false
	}
	rest := pins[start+len(webPinsHeading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return pins[start : start+len(webPinsHeading)+end], true
	}
	return pins[start:], true
}

// webPinsSources holds the live files webFactMismatches checks the section
// against. A test can build one from disk or from a value it made up, so
// the checker itself can be tested without touching a real file.
type webPinsSources struct {
	goMod, vt, attach, panes, stateEvent, agents, proto, cli string
}

func readWebPinsSources(t *testing.T) webPinsSources {
	t.Helper()
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	return webPinsSources{
		goMod:      read("../../go.mod"),
		vt:         read("../../internal/vt/vt.go"),
		attach:     read("../../internal/server/attach.go"),
		panes:      read("../../internal/server/panes.go"),
		stateEvent: read("../../internal/proto/state_event.go"),
		agents:     read("../../internal/server/agents.go"),
		proto:      read("../../internal/proto/proto.go"),
		cli:        read("../../internal/cli/cli.go"),
	}
}

var goModToolchainRe = regexp.MustCompile(`toolchain (go[0-9][^\s]*)`)

func goModVersion(mod, path string) (string, bool) {
	re := regexp.MustCompile(regexp.QuoteMeta(path) + ` (v[0-9][^\s]*)`)
	m := re.FindStringSubmatch(mod)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func goModToolchain(mod string) (string, bool) {
	m := goModToolchainRe.FindStringSubmatch(mod)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// attrsBlockRe requires the seven attrs names adjacent, each separated from
// the one before it by nothing but whitespace, ending right at the const
// block's own closing paren. Only "1 << iota" sets the first value; every
// later name in the same block takes the next power of two purely from its
// position. A name out of order, missing, or with anything else spliced
// between two of them, a comment included, would shift every bit after it,
// so adjacency is the whole proof, not just presence somewhere in order.
var attrsBlockRe = regexp.MustCompile(
	`AttrBold uint16 = 1 << iota\s*AttrFaint\s*AttrItalic\s*AttrUnderline\s*AttrBlink\s*AttrInverse\s*AttrStrike\s*\)`,
)

func attrsBlockIntact(vt string) bool {
	return attrsBlockRe.MatchString(vt)
}

// webFactMismatches checks every fact the Web / phone section is supposed
// to record against the live source that produced it, and returns one
// message per fact that is missing from the section or no longer true of
// the source. An empty result means the section and the tree agree.
func webFactMismatches(section string, src webPinsSources) []string {
	var bad []string
	report := func(cond bool, msg string) {
		if !cond {
			bad = append(bad, msg)
		}
	}

	if wsVer, ok := goModVersion(src.goMod, "github.com/coder/websocket"); ok {
		report(strings.Contains(section, "github.com/coder/websocket") && strings.Contains(section, wsVer),
			"PINS does not record github.com/coder/websocket "+wsVer+", the version go.mod pins")
	} else {
		bad = append(bad, "go.mod has no version for github.com/coder/websocket")
	}

	if qrVer, ok := goModVersion(src.goMod, "github.com/mdp/qrterminal/v3"); ok {
		report(strings.Contains(section, "github.com/mdp/qrterminal/v3") && strings.Contains(section, qrVer),
			"PINS does not record github.com/mdp/qrterminal/v3 "+qrVer+", the version go.mod pins")
	} else {
		bad = append(bad, "go.mod has no version for github.com/mdp/qrterminal/v3")
	}

	if tc, ok := goModToolchain(src.goMod); ok {
		report(strings.Contains(section, tc), "PINS does not record the toolchain "+tc+" that go.mod resolves to")
	} else {
		bad = append(bad, "go.mod has no toolchain directive")
	}

	report(attrsBlockIntact(src.vt),
		"internal/vt/vt.go's attrs block no longer runs Bold, Faint, Italic, Underline, Blink, Inverse, Strike in that order")
	report(strings.Contains(section, "bold 1, faint 2, italic 4, underline 8, blink 16, inverse 32, strike 64"),
		"PINS does not record the seven attrs bits read from internal/vt/vt.go")

	report(strings.Contains(src.attach, `r.Int("cols")`),
		`internal/server/attach.go no longer reads cols with r.Int("cols")`)
	report(strings.Contains(section, "attach, do not resize"),
		"PINS does not record that a missing cols/rows means attach, do not resize")

	for _, field := range []string{
		`"id":`, `"label":`, `"cwd":`, `"cmd":`, `"kind":`, `"harness":`,
		`"workspace":`, `"tab":`, `"closed":`, `"cols":`, `"rows":`,
		`"state":`, `"source":`, `"detail":`,
	} {
		report(strings.Contains(src.panes, field), "handlePaneList no longer sets row["+field+"]")
	}
	for _, cond := range []string{`row["ts"]`, `row["session_id"]`, `row["exit_code"]`, `row["ask"]`} {
		report(strings.Contains(src.panes, cond), "handlePaneList no longer conditionally sets "+cond)
	}
	report(strings.Contains(section, "id, label, cwd, cmd, kind, harness, workspace, tab, closed, cols, rows, state, source, detail"),
		"PINS does not record the pane.list row's base field list")
	report(strings.Contains(section, "`ts` and `session_id` once a current event exists"),
		"PINS does not record that ts and session_id appear once a current event exists")
	report(strings.Contains(section, "`exit_code` once the process exited"),
		"PINS does not record that exit_code appears once the process exited")
	report(strings.Contains(section, "`ask` once the gate holds one"),
		"PINS does not record that ask appears once the gate holds one")
	report(strings.Contains(section, "**flat**"), "PINS does not mark the pane.list row as flat")
	report(strings.Contains(section, "not a nested `PaneStateEvent`"),
		"PINS does not say pane.list's state is a string, not a nested PaneStateEvent")

	for _, tag := range []string{
		`json:"v"`, `json:"ts"`, `json:"session_id"`, `json:"harness_session_id"`,
		`json:"harness"`, `json:"pane"`, `json:"state"`, `json:"source"`,
		`json:"ask,omitempty"`, `json:"detail"`,
	} {
		report(strings.Contains(src.stateEvent, tag), "PaneStateEvent no longer carries "+tag)
	}
	report(strings.Contains(src.agents, `json:"event"`),
		`agents.go's stateEvent wrapper no longer carries json:"event"`)
	report(strings.Contains(section, `{"event":"state"`),
		`PINS does not record the state event's {"event":"state" flattening`)
	report(strings.Contains(section, "v, ts, session_id, harness_session_id, harness, pane, state, source, ask, detail"),
		"PINS does not record the state event's flattened field order")

	for _, code := range []string{
		`"bad_request"`, `"no_such_pane"`, `"no_such_workspace"`, `"no_such_tab"`,
		`"pane_closed"`, `"server_closed"`, `"not_attached"`, `"adapter_error"`,
		`"spawn_failed"`, `"timeout"`, `"unauthorized"`, `"internal"`,
	} {
		report(strings.Contains(src.proto, code), "internal/proto/proto.go no longer defines the error code "+code)
	}
	report(strings.Contains(section, `"error":{"code":`),
		`PINS does not record the refusal shape's "error":{"code": fragment`)
	report(strings.Contains(section,
		"bad_request, no_such_pane, no_such_workspace, no_such_tab, pane_closed, server_closed, not_attached, adapter_error, spawn_failed, timeout, unauthorized, internal"),
		"PINS does not record the closed error-code enum's twelve members")

	report(strings.Contains(src.cli, `case "workspace", "tab", "pane", "agent", "session":`),
		"internal/cli/cli.go's verb switch no longer has the case list this pin was read from")
	report(strings.Contains(section, "internal/cli/web.go"),
		"PINS does not record that the web verbs live in internal/cli/web.go")

	// tailscale's flag names, Let's Encrypt's validity window, and the
	// websocket library's licence have no cheap live source on every box
	// this test runs on: the first needs the tailscale binary, absent on
	// some boxes and in CI; the second comes from documentation, not a
	// file in this tree; the third would need a file inside the module
	// cache, which a clean checkout does not carry. All three stay pinned
	// literals, each with its own source named in the section's own row.
	for _, want := range []string{"--cert-file", "--key-file", "90 days", "ISC"} {
		report(strings.Contains(section, want), "PINS does not record "+want)
	}

	return bad
}

func TestPinsRecordsEveryWebFact(t *testing.T) {
	raw, err := os.ReadFile("../../PINS.md")
	if err != nil {
		t.Fatalf("read PINS.md: %v", err)
	}
	section, ok := webPinsSection(string(raw))
	if !ok {
		t.Fatalf("PINS.md has no %q section", webPinsHeading)
	}
	src := readWebPinsSources(t)
	for _, msg := range webFactMismatches(section, src) {
		t.Error(msg)
	}
}

// TestPinsCatchesADriftedVersion is this test's own red run, kept rather
// than thrown away. It feeds the checker a section with the recorded
// websocket version changed and confirms a mismatch comes back, so a clean
// TestPinsRecordsEveryWebFact means the facts truly match the tree, not
// that the checker stopped checking.
func TestPinsCatchesADriftedVersion(t *testing.T) {
	raw, err := os.ReadFile("../../PINS.md")
	if err != nil {
		t.Fatalf("read PINS.md: %v", err)
	}
	section, ok := webPinsSection(string(raw))
	if !ok {
		t.Fatalf("PINS.md has no %q section", webPinsHeading)
	}
	src := readWebPinsSources(t)
	wsVer, ok := goModVersion(src.goMod, "github.com/coder/websocket")
	if !ok {
		t.Fatal("go.mod has no version for github.com/coder/websocket")
	}
	drifted := strings.Replace(section, wsVer, "v9.9.9", 1)
	bad := webFactMismatches(drifted, src)
	for _, msg := range bad {
		if strings.Contains(msg, "github.com/coder/websocket") {
			return
		}
	}
	t.Fatalf("changing the recorded websocket version did not produce a mismatch; got %v", bad)
}

// TestAttrsBlockIntactCatchesANameSplicedBetweenTwoOthers is
// attrsBlockIntact's own red run. A name out of order, missing, or with
// something else landing in the gap between two of them would shift every
// bit after it, so nothing but whitespace may sit between one name and the
// next. This feeds it a const block with an extra line spliced between
// AttrFaint and AttrItalic and confirms that reads as broken, not merely as
// present-somewhere-in-order.
func TestAttrsBlockIntactCatchesANameSplicedBetweenTwoOthers(t *testing.T) {
	real := "AttrBold uint16 = 1 << iota\n\tAttrFaint\n\tAttrItalic\n\tAttrUnderline\n\tAttrBlink\n\tAttrInverse\n\tAttrStrike\n)"
	if !attrsBlockIntact(real) {
		t.Fatal("attrsBlockIntact rejected the real block shape")
	}
	spliced := "AttrBold uint16 = 1 << iota\n\tAttrFaint\n\tAttrExtra\n\tAttrItalic\n\tAttrUnderline\n\tAttrBlink\n\tAttrInverse\n\tAttrStrike\n)"
	if attrsBlockIntact(spliced) {
		t.Fatal("attrsBlockIntact accepted a name spliced between AttrFaint and AttrItalic")
	}
}
