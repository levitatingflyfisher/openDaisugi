package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// journalTitle heads the journal: what it lists, in the words the web
// page uses.
const journalTitle = "Journal · the verdicts the gate and each agent's loop report, newest first. 100 per agent, 500 in all, oldest first"

// JournalRow is one verdict from floor.journal.
type JournalRow struct {
	At                     float64
	Pane                   string
	Decision, Tool, Clause string
}

// JournalView is the open journal: the verdicts newest first, how far the
// owner scrolled, and the words of a read that failed.
type JournalView struct {
	Rows []JournalRow
	// Top is the first row drawn. Down moves toward older verdicts.
	Top int
	// Err is the server's refusal when the last read failed, or "".
	Err string
}

// decodeJournal reads a floor.journal reply. A row that is not shaped as a
// verdict is dropped.
func decodeJournal(res map[string]any) []JournalRow {
	list, _ := res["entries"].([]any)
	var out []JournalRow
	for _, v := range list {
		e, ok := v.(map[string]any)
		if !ok {
			continue
		}
		at, ok := num(e, "at")
		if !ok || str(e, "pane") == "" || str(e, "decision") == "" {
			continue
		}
		out = append(out, JournalRow{At: at, Pane: str(e, "pane"), Decision: str(e, "decision"), Tool: str(e, "tool"), Clause: str(e, "clause")})
	}
	return out
}

// readJournal asks floor.journal and keeps the rows. A failed read keeps
// the last rows and says why.
func (f *floor) readJournal() {
	j := f.m.Journal
	if j == nil {
		return
	}
	res, err := call(f.o.Socket, "floor.journal", nil)
	if err != nil {
		j.Err = err.Error()
		return
	}
	j.Err = ""
	j.Rows = decodeJournal(res)
	if j.Top >= len(j.Rows) {
		j.Top = max(len(j.Rows)-1, 0)
	}
}

// openJournal opens the journal over the windows and reads it.
func (f *floor) openJournal() {
	f.m.Journal = &JournalView{}
	f.readJournal()
}

// journalKey applies one keystroke while the journal is open. Up and Down
// scroll. Esc or j closes it. Every other key does nothing.
func (f *floor) journalKey(k key) {
	m := f.m
	switch k.kind {
	case keyUp:
		m.Journal.Top = max(m.Journal.Top-1, 0)
	case keyDown:
		m.Journal.Top = min(m.Journal.Top+1, max(len(m.Journal.Rows)-1, 0))
	case keyEsc:
		m.Journal = nil
	case keyByte:
		if k.b == 'j' || k.b == 'q' {
			m.Journal = nil
		}
	}
}

var journalGlyph = map[string]string{"allow": "✓", "deny": "✕", "ask": "?"}

// journalLines draws the journal in height lines of cols cells: a title,
// then one line per verdict from the scroll point.
func journalLines(m *Model, cols, height int) []string {
	v := m.Journal
	lines := []string{sgrAccent + textwidth.Pad(textwidth.Truncate(journalTitle, cols), cols) + sgrReset}
	switch {
	case v.Err != "":
		lines = append(lines, sgrAmber+textwidth.Pad(textwidth.Truncate("The journal did not load: "+textwidth.Printable(v.Err, 0), cols), cols)+sgrReset)
	case len(v.Rows) == 0:
		lines = append(lines, textwidth.Pad("No gate verdicts yet.", cols))
	}
	for i := v.Top; i < len(v.Rows) && len(lines) < height; i++ {
		r := v.Rows[i]
		label := r.Pane
		if row, ok := m.row(r.Pane); ok && row.Label != "" {
			label = row.Label
		}
		glyph := journalGlyph[r.Decision]
		if glyph == "" {
			glyph = "?"
		}
		what := []string{r.Decision}
		if r.Tool != "" {
			what = append(what, r.Tool)
		}
		if r.Clause != "" {
			what = append(what, "clause "+r.Clause)
		}
		text := fmt.Sprintf(" %s %s  %s  %s", time.Unix(int64(r.At), 0).Format("15:04:05"), glyph,
			textwidth.Printable(label, 0), textwidth.Printable(strings.Join(what, " · "), 0))
		color := ""
		if r.Decision != "allow" {
			color = sgrAmber
		}
		lines = append(lines, color+textwidth.Pad(textwidth.Truncate(text, cols), cols)+sgrReset)
	}
	for len(lines) < height {
		lines = append(lines, textwidth.Pad("", cols))
	}
	return fit(lines, height)
}
