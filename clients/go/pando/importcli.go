package pando

import (
	"fmt"
	"io"
	"os"
	"time"
)

// ImportFlags are the words of coppice steward import.
type ImportFlags struct {
	From, Since                               string
	Dry, Pause, Resume, Status, Tools, chosen bool
	many                                      bool
}

// Set takes one flag word (--tools, --dry-run, --pause, --resume, --status).
func (f *ImportFlags) Set(a string) {
	switch a {
	case "--tools":
		f.Tools = true
		return
	case "--dry-run":
		f.Dry = true
	case "--pause":
		f.Pause = true
	case "--resume":
		f.Resume = true
	case "--status":
		f.Status = true
	}
	if f.chosen {
		f.many = true
	}
	f.chosen = true
}

// RunImport is the old coppice steward import: it queues. It reads transcripts under HOME and
// writes import.json; the running server logs the messages (FM-R-71).
// skip lists dirs whose transcripts are not read (the chair's own).
func RunImport(dir string, skip []string, f ImportFlags, asJSON bool, out, errw io.Writer) int {
	if asJSON && !f.Status {
		// --json alone is not a mode: it must never start an import (IM-R-9).
		fmt.Fprintln(errw, "--json goes with --status")
		return 1
	}
	if f.many {
		fmt.Fprintln(errw, "import takes one of --dry-run, --pause, --resume and --status")
		return 1
	}
	if f.From != "claude" && f.From != "codex" && f.From != "all" {
		fmt.Fprintf(errw, "--from needs claude, codex or all, not %q\n", f.From)
		return 1
	}
	o := ImportOpts{From: f.From, Tools: f.Tools, Skip: append(append([]string{dir}, skip...), routeDirs(dir)...), SkipSessions: skipWithRoutes(dir)}
	if f.Since != "" {
		t, err := time.Parse("2006-01-02", f.Since)
		if err != nil {
			fmt.Fprintf(errw, "--since needs a date like 2026-03-01, not %q\n", f.Since)
			return 1
		}
		o.Since = t.UTC()
	}
	switch {
	case f.Pause, f.Resume:
		if err := SetPaused(dir, f.Pause); err != nil {
			fmt.Fprintf(errw, "cannot write the import state in %s: %v\n", dir, err)
			return 1
		}
		st, _ := ReadImportState(dir)
		word := "resumed"
		if f.Pause {
			word = "paused"
		}
		line := "import " + word
		if st.Total > 0 {
			line += " (" + st.Progress() + ")"
		}
		fmt.Fprintln(out, line)
		return 0
	case f.Status:
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
	if f.Dry {
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
