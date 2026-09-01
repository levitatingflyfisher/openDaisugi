// The headless side of a pane. A headless pane wraps a harness process and
// normalises its event stream into two things: transcript text written into the
// pane's grid, so a client cannot tell the two pane kinds apart except by a
// badge, and PaneStateEvents with source headless.

package pane

import (
	"context"
	"errors"

	"github.com/opendaisugi/coppice/internal/proto"
)

// ErrUnsupported is what an adapter returns for a verb its harness has no
// equivalent for. It is not an error condition; it is an honest "no".
var ErrUnsupported = errors.New("this harness does not support that")

type EventKind string

const (
	EvText  EventKind = "text"
	EvTool  EventKind = "tool"
	EvState EventKind = "state"
	EvEnd   EventKind = "end"
	EvError EventKind = "error"
)

// Event is one normalised thing a harness said.
//
// Ask is the pending question when State is "blocked". pi's extension_ui_request
// (spec-04) and OpenCode's permission.ask (spec-05) both carry a tool name and a
// summary, and a floor client cannot render "blocked on what?" without them, so
// the field lives here rather than being invented twice.
type Event struct {
	Kind   EventKind
	Text   string
	Tool   string
	State  string // idle | working | blocked | done | unknown, for EvState and EvEnd
	Detail string
	Ask    *proto.Ask
}

type StartOpts struct {
	Cwd    string
	Env    map[string]string
	Argv   []string // extra argv beyond the adapter's own
	Resume string   // a harness session id to resume, when one was recorded
	Sock   string
	PaneID string
}

// Proc is one running headless harness.
type Proc interface {
	Prompt(text string) error
	Steer(text string) error // may return ErrUnsupported
	WriteStdin(b []byte) error
	// Events must return the SAME channel on every call: pumpAdapter's drain
	// step calls it again after Stop(), and a different channel there would
	// silently stop draining the one it was already reading.
	Events() <-chan Event
	SessionID() (string, bool)
	Stop() error
}

// Adapter starts one kind of harness headlessly.
//
// Four rules bind every implementation, including the ones spec-04 and spec-05
// write, so they do not each invent an answer:
//
//  1. Set cmd.Env = BuildEnv(os.Environ(), o.Env, o.Sock, o.PaneID). That is the
//     only path by which a headless harness's gate hook finds COPPICE_SOCK and
//     COPPICE_PANE.
//  2. The SERVER owns rendering. g is passed for an adapter that wants to write
//     raw VT bytes itself; claude, codex and sprig all ignore it, because
//     pumpAdapter renders their events into the same grid. Ignoring it is the
//     expected case.
//  3. Stop() signals and kills. The goroutine producing into Events() is the
//     one that closes it, after it observes the stop signal. Closing it from
//     Stop races a producer into a closed channel, and that panic takes the
//     whole daemon down.
//  4. EvEnd is terminal, and closing Events() afterward is the adapter's own
//     job: the pump only falls back to calling Stop() and draining (bounded)
//     when an adapter that already sent one keeps its channel open anyway -
//     that is a safety net, not a substitute for the adapter closing its own
//     stream. Two things every implementation, spec-04's and spec-05's most
//     of all, must also get right on the way there: emit one Event per
//     message, not one per streamed token delta - the pump renders one grid
//     row per Event it sees, so a token-by-token harness must coalesce its
//     own deltas before calling in; and know that Text is written into the
//     grid RAW, unescaped, so the pump's own "[end]"/"[error]" marker lines
//     are not authoritative to a reader unless the adapter's model text
//     cannot itself produce that literal text, or the adapter escapes its
//     own model text before handing it over - pick one and say which in the
//     adapter's own doc comment.
type Adapter interface {
	Name() string
	Start(ctx context.Context, o StartOpts, g *Grid) (Proc, error)
}

// Forker is the optional interface an Adapter implements when its harness
// can start a new session that continues from an existing one. ForkArgv
// returns the EXTRA argv the server puts in StartOpts.Argv for the fork's
// first start, beside StartOpts.Resume set to sessionID. Start builds the
// binary and the resume flag itself, so ForkArgv never repeats them. An
// adapter that does not implement this cannot fork, and the server says so.
type Forker interface {
	ForkArgv(sessionID string) ([]string, error)
}
