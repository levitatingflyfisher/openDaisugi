package pane

import (
	"fmt"
	"strings"
)

// The state strings a headless event maps to. They mirror proto's constants
// (proto.StateIdle, proto.StateWorking, ...): internal/pane already imports
// internal/proto (grid.go, for Cell/Frame/Ask), so this duplication is for
// readability at THIS file's own call sites - a StateOf/WriteTranscript
// switch reads better as StateWorkingStr than as proto.StateWorking - not
// for avoiding a dependency that already exists.
const (
	StateIdleStr    = "idle"
	StateWorkingStr = "working"
	StateBlockedStr = "blocked"
	StateDoneStr    = "done"
	StateUnknownStr = "unknown"
)

// WriteTranscript renders one adapter event into the pane's grid, so a headless
// pane is also just a grid to a client. Every event ends at column zero, or the
// next event would continue someone else's line.
//
// It returns the grid's own write error rather than
// swallowing it: a closed grid is not "nothing happened," and a caller that
// cannot tell the two apart cannot decide whether to keep trying.
func WriteTranscript(g *Grid, ev Event) error {
	var line string
	switch ev.Kind {
	case EvText:
		line = ev.Text
	case EvTool:
		line = "\x1b[36m" + ev.Tool + "\x1b[0m " + ev.Detail
	case EvState:
		line = "\x1b[33m[" + ev.State + "]\x1b[0m " + ev.Detail
		if ev.Ask != nil {
			line += " " + ev.Ask.Tool + ": " + ev.Ask.Summary
		}
	case EvEnd:
		line = "\x1b[32m[end]\x1b[0m " + ev.Detail
	case EvError:
		line = "\x1b[31m[error]\x1b[0m " + ev.Detail
	default:
		line = fmt.Sprintf("[%s] %s", ev.Kind, ev.Detail)
	}
	line = strings.ReplaceAll(strings.TrimRight(line, "\n"), "\n", "\r\n")
	_, err := g.Write([]byte(line + "\r\n"))
	return err
}

// StateOf is the state a headless event implies. A parse error means unknown,
// never idle: master spec 3.6 says a broken stream must not look like a quiet
// agent.
//
// The bool is always true - every branch below returns
// one, including the default case - so a caller's `if !ok { continue }`
// never fires. It stays, rather than being dropped, because pumpAdapter and
// every adapter test already destructure two return values against it; the
// EvEnd case is also unreachable from pumpAdapter, which breaks out of its
// loop on EvEnd before ever calling this function.
func StateOf(ev Event) (string, bool) {
	switch ev.Kind {
	case EvText, EvTool:
		return StateWorkingStr, true
	case EvState:
		if ev.State == "" {
			return StateUnknownStr, true
		}
		return ev.State, true
	case EvEnd:
		if ev.State == "" {
			return StateDoneStr, true
		}
		return ev.State, true
	case EvError:
		return StateUnknownStr, true
	default:
		return StateUnknownStr, true
	}
}
