package foreman

import (
	"fmt"
	"io"
	"os"
	"time"
)

// importFlags are the words of coppice foreman import.
type importFlags struct {
	from, since                               string
	dry, pause, resume, status, tools, chosen bool
	many                                      bool
}

func (f *importFlags) set(a string) {
	switch a {
	case "--tools":
		f.tools = true
		return
	case "--dry-run":
		f.dry = true
	case "--pause":
		f.pause = true
	case "--resume":
		f.resume = true
	case "--status":
		f.status = true
	}
	if f.chosen {
		f.many = true
	}
	f.chosen = true
}

// runImport is coppice foreman import. It reads transcripts under HOME and
// writes import.json; the running server logs the messages (FM-R-71).
func runImport(dir, dataDir string, f importFlags, asJSON bool, out, errw io.Writer) int {
	if asJSON && !f.status {
		// --json alone is not a mode: it must never start an import (IM-R-9).
		fmt.Fprintln(errw, "--json goes with --status")
		return 1
	}
	if f.many {
		fmt.Fprintln(errw, "import takes one of --dry-run, --pause, --resume and --status")
		return 1
	}
	if f.from != "claude" && f.from != "codex" && f.from != "all" {
		fmt.Fprintf(errw, "--from needs claude, codex or all, not %q\n", f.from)
		return 1
	}
	o := ImportOpts{From: f.from, Tools: f.tools, Skip: []string{dir, dataDir}}
	if d, err := DefaultDir(); err == nil {
		o.Skip = append(o.Skip, d)
	}
	if f.since != "" {
		t, err := time.Parse("2006-01-02", f.since)
		if err != nil {
			fmt.Fprintf(errw, "--since needs a date like 2026-03-01, not %q\n", f.since)
			return 1
		}
		o.Since = t.UTC()
	}
	switch {
	case f.pause, f.resume:
		if err := SetPaused(dir, f.pause); err != nil {
			fmt.Fprintf(errw, "cannot write the import state in %s: %v\n", dir, err)
			return 1
		}
		st, _ := ReadImportState(dir)
		word := "resumed"
		if f.pause {
			word = "paused"
		}
		line := "import " + word
		if st.Total > 0 {
			line += " (" + st.Progress() + ")"
		}
		fmt.Fprintln(out, line)
		return 0
	case f.status:
		st, err := ReadImportState(dir)
		if err != nil {
			fmt.Fprintf(errw, "cannot read the import state in %s: %v\n", dir, err)
			return 1
		}
		if asJSON {
			line, _ := encodeLine(StatusOf(st))
			out.Write(line)
			return 0
		}
		fmt.Fprintf(out, "import: %s\n", st.Word())
		if st.Total > 0 {
			fmt.Fprintf(out, "progress: %s\n", st.Progress())
		}
		return 0
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(errw, "cannot find the home dir: %v\n", err)
		return 1
	}
	st, err := ReadImportState(dir)
	if err != nil {
		fmt.Fprintf(errw, "cannot read the import state in %s: %v\n", dir, err)
		return 1
	}
	var has func(string) bool
	if c, err := OpenReadOnly(dir); err == nil {
		has = c.HasAct
	}
	ss := Scan(home, o, st, has)
	fmt.Fprint(out, Describe(ss))
	if f.dry {
		fmt.Fprintln(out, "nothing was changed")
		return 0
	}
	if len(ss) == 0 {
		fmt.Fprintln(out, "nothing to import")
		return 0
	}
	if err := Accept(dir, o, ss); err != nil {
		fmt.Fprintf(errw, "cannot write the import state in %s: %v\n", dir, err)
		return 1
	}
	fmt.Fprintln(out, "queued: they import one message at a time while coppice runs, never ahead of a live message")
	return 0
}

// ImportStatus is the import state as floor.import and --status --json
// give it.
type ImportStatus struct {
	State   string `json:"state"`
	Offered bool   `json:"offered"`
	Paused  bool   `json:"paused"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Queued  int    `json:"queued"`
}

// StatusOf is the status of a state.
func StatusOf(s ImportState) ImportStatus {
	return ImportStatus{State: s.Word(), Offered: s.Offered, Paused: s.Paused, Done: s.Done, Total: s.Total, Queued: len(s.Queue)}
}
