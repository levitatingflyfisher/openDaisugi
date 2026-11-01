package tui

import (
	"fmt"
	"strings"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// Facts is what the floor header row shows from floor.facts: the agents
// by daisugi mode, the router, and the tokens used today. The working
// and needs-you counts come from the rows, so the header never disagrees
// with the rail under it.
type Facts struct {
	Enforcing, Watching, Off int
	Disarmed                 bool
	// NotInstalled is true when the server says no harness settings in
	// the home hold a daisugi warden hook.
	NotInstalled bool
	// HookRoot is the warden root the installed hook names when it is not
	// the server's, or "": then m sets nothing (F11-R-15).
	HookRoot string
	// Router is the router's URL, or "" when none is set. Answers is
	// "yes", "no", or "" when the server did not check.
	Router, Answers string
	Tokens          float64
	// Saved is the frontier tokens the router says it saved today, and
	// HasSaved is true only when the server sent that figure. With no
	// figure the header names no saving.
	Saved    float64
	HasSaved bool
}

// factsMinCols is the narrowest screen that shows the header row. A
// narrower one has no room for it next to everything else.
const factsMinCols = 100

// wardenHint is the line the floor shows while no harness has a daisugi
// warden hook and no agent is guarded, the web page's words.
const wardenHint = "daisugi is off: no agent is guarded. Run: daisugi install"

// wardenHintShown reports whether the facts call for wardenHint.
func wardenHintShown(fa *Facts) bool {
	return fa != nil && fa.NotInstalled && fa.Enforcing+fa.Watching == 0
}

// hintMinCols is the narrowest screen that can hold wardenHint.
var hintMinCols = textwidth.Width(wardenHint)

// decodeFacts reads a floor.facts reply, or nil when it has no daisugi
// object.
func decodeFacts(res map[string]any) *Facts {
	d, ok := res["daisugi"].(map[string]any)
	if !ok {
		return nil
	}
	n := func(m map[string]any, k string) int {
		v, _ := num(m, k)
		if v < 0 {
			return 0
		}
		return int(v)
	}
	f := &Facts{Enforcing: n(d, "enforcing"), Watching: n(d, "watching"), Off: n(d, "off")}
	if armed, ok := d["armed"].(bool); ok && !armed {
		f.Disarmed = true
	}
	if installed, ok := d["installed"].(bool); ok && !installed {
		f.NotInstalled = true
	}
	f.HookRoot = str(d, "hook_root")
	if gw, ok := res["router"].(map[string]any); ok {
		f.Router = str(gw, "url")
		if a, ok := gw["answers"].(bool); ok {
			f.Answers = map[bool]string{true: "yes", false: "no"}[a]
		}
	}
	if t, ok := res["tokens_today"].(map[string]any); ok {
		for _, k := range []string{"fresh", "cache_read", "cache_write", "out"} {
			if v, ok := num(t, k); ok && v > 0 {
				f.Tokens += v
			}
		}
	}
	if s, ok := res["saved_today"].(map[string]any); ok {
		if v, ok := num(s, "tokens"); ok && v >= 0 {
			f.Saved, f.HasSaved = v, true
		}
	}
	return f
}

// readFacts asks floor.facts for the header row, only on a screen wide
// enough to show it. A failed read keeps the last facts, so one slow
// answer never takes the row away and resizes every window.
func (f *floor) readFacts() {
	if cols, _ := f.o.Size(); cols < min(factsMinCols, hintMinCols) {
		return
	}
	res, err := call(f.o.Socket, "floor.facts", nil)
	if err != nil {
		return
	}
	if fa := decodeFacts(res); fa != nil {
		f.m.Facts = fa
	}
	// The tracked steward's window draws the chat. A steward that left
	// takes its chat and its Screen choice with it.
	fm, _ := res["steward"].(map[string]any)
	if id := str(fm, "pane"); id != f.m.StewardID {
		f.setSteward(id)
	}
}

// factsLine is the header row: the same facts, in the same words, as the
// web page's header. Working and needs you are counted from the rows.
func factsLine(m *Model) string {
	fa := m.Facts
	var mode []string
	for _, p := range []struct {
		n    int
		word string
	}{{fa.Enforcing, "enforcing"}, {fa.Watching, "watching"}, {fa.Off, "off"}} {
		if p.n > 0 {
			mode = append(mode, fmt.Sprintf("%d %s", p.n, p.word))
		}
	}
	daisugi := "daisugi · no agents"
	if len(mode) > 0 {
		daisugi = "daisugi · " + strings.Join(mode, " · ")
	}
	if fa.Disarmed {
		daisugi += " · disarmed"
	}
	router := "no router"
	switch {
	case fa.Router == "":
	case fa.Answers == "yes":
		router = "router answers"
	case fa.Answers == "no":
		router = "router silent"
	default:
		router = "router ?"
	}
	working := 0
	for _, r := range m.Rows {
		if r.Parent == "" && (r.State == "working" || r.Held != nil) {
			working++
		}
	}
	need := fmt.Sprintf("%d need you", m.NeedYou)
	if m.NeedYou == 1 {
		need = "1 needs you"
	}
	parts := []string{daisugi, router, fmt.Sprintf("%d working", working), need, "tokens today " + countText(fa.Tokens)}
	if fa.HasSaved {
		parts = append(parts, "saved "+countText(fa.Saved))
	}
	return textwidth.Printable(strings.Join(parts, "   "), 0)
}
