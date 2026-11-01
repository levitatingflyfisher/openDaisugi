package pando

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/pandoskip"
)

// Round 7: a message enters the log when it is said. Every agent writes
// into the one store, and each line keeps who said it, in which harness and
// in which chat. A live message is told apart from an import by its kind
// (user or steward, not past) and is never logged twice: its act id is built
// from the source and the source's own message id.

// Meta says where a message came from.
type Meta struct {
	Src  string // the harness: claude-code, hermes, ...
	Chat string // the chat, session or thread inside it
	Who  string // the person, where the harness records one
}

// Live is one message to log as it is said.
type Live struct {
	Kind string // KindUser or KindSteward
	Text string
	At   time.Time
	// Act is the key that keeps the message from being logged twice.
	Act string
	Meta
}

// AppendLive logs m unless its act is in the log already. fresh is false
// when the log held it. A message whose parts were only partly logged is
// finished, as AppendImported does.
func (c *Chat) AppendLive(ctx context.Context, m Live) (fresh bool, err error) {
	if m.Act == "" {
		return false, errors.New("pando: a live message needs an act id")
	}
	if strings.TrimSpace(m.Text) == "" {
		return false, nil
	}
	have := c.acts[m.Act]
	if len(have) > 0 && (!c.ActMatches(m.Act, m.Text) || c.importDone(ImportMsg{Text: m.Text, Act: m.Act})) {
		return false, nil
	}
	_, err = c.appendMeta(ctx, m.Kind, m.Text, m.At, m.Act, len(have), m.Meta)
	return err == nil, err
}

// SkipOn is whether this process is not a chat of the owner's: a worker the
// steward started, or a model call daisugi makes for itself. It then gets
// no view and logs nothing.
func SkipOn(getenv func(string) string) bool {
	return getenv != nil && pandoskip.On(getenv(pandoskip.Var))
}

// SkipVar is the variable that marks a process as not the owner's chat.
const SkipVar = pandoskip.Var

// SkipListVar names the skip list a marked process lists its sessions in.
const SkipListVar = pandoskip.ListVar

// SkipEnviron is environ with SkipVar set. coppice uses it for the steward's
// turns and for the workers it spawns.
func SkipEnviron(environ []string) []string { return pandoskip.Environ(environ) }

// SkipEnvironWithList is SkipEnviron with the skip list of the data home that
// holds the stores named too. Every `claude -p` call below the process then
// lists its session id before it starts.
func SkipEnvironWithList(environ []string, dataHome string) []string {
	return pandoskip.WithList(pandoskip.Environ(environ), SkipListIn(dataHome))
}

// SkipDirs are the dirs whose Claude Code sessions the live feed never reads:
// the store itself and the coppice data dir, where the steward's pool workers
// run. A worker's transcript is its own; it stays in its child store. The
// steward dir is not here: workers carry the skip marker and list, and a
// Claude Code pane steward is logged like any other chat.
func SkipDirs(storeDir, home string, getenv func(string) string) []string {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	state := getenv("XDG_STATE_HOME")
	if state == "" && home != "" {
		state = filepath.Join(home, ".local", "state")
	}
	out := []string{storeDir}
	if d := getenv("COPPICE_DATA_DIR"); d != "" {
		out = append(out, d)
	}
	// The coppice data dir sits in the daisugi data home, which is
	// <data>/pando/<store> above the store: the one this process was given,
	// the XDG one and the old ~/.opendaisugi.
	if storeDir != "" {
		out = append(out, filepath.Join(filepath.Dir(filepath.Dir(storeDir)), "coppice"))
	}
	if x := getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		out = append(out, filepath.Join(x, "opendaisugi", "coppice"))
	} else if home != "" {
		out = append(out, filepath.Join(home, ".local", "share", "opendaisugi", "coppice"))
	}
	if home != "" {
		out = append(out, filepath.Join(home, ".opendaisugi", "coppice"))
	}
	if state != "" {
		out = append(out, filepath.Join(state, "coppice"))
	}
	return out
}

// ClaudeSrc is the Src of a Claude Code message.
const ClaudeSrc = "claude-code"

// IngestClaude reads one Claude Code transcript and logs the user and
// assistant messages the log does not hold. The act id is the same as the
// importer's (imp:claude:<uuid>), so an import and the live feed never log a
// message twice. It returns how many messages it added. A session of the
// compactor itself gives none (readSession drops it).
func (c *Chat) IngestClaude(ctx context.Context, path string, skip []string) (int, error) {
	// The list is read for each file: a worker's hook may add its session at
	// any time, and the file is small.
	r, err := readSession(path, SrcClaude, ImportOpts{From: SrcClaude, Skip: skip,
		SkipSessions: LoadSkip(SkipListPath(c.cfg.Dir))})
	if err != nil {
		return 0, err
	}
	chat := strings.TrimPrefix(r.key, SrcClaude+":")
	// A session on the store's session map belongs to a branch: its
	// messages go there and never into this log.
	target := c
	if len(r.msgs) > 0 {
		rt := NewRouter(c)
		defer rt.Close()
		sid, _, _ := strings.Cut(chat, "~")
		if br := rt.Target(sid, path, r.cwd); br != "" {
			if target, err = rt.Chat(ctx, br); err != nil {
				return 0, err
			}
		}
	}
	n := 0
	for _, m := range r.msgs {
		kind := KindUser
		if strings.HasPrefix(m.Text, "claude assistant") {
			kind = KindSteward
		}
		fresh, err := target.AppendLive(ctx, Live{Kind: kind, Text: m.Text, At: m.TS, Act: m.Act,
			Meta: Meta{Src: ClaudeSrc, Chat: chat, Who: "owner"}})
		if err != nil {
			return n, err
		}
		if fresh {
			n++
		}
	}
	return n, nil
}

// HookEvent is the JSON a Claude Code hook gets on standard input.
type HookEvent struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Prompt         string `json:"prompt"`
	Source         string `json:"source"`
}

// ReadHookEvent reads the event from in. A bad or empty input is an empty
// event, never an error: a hook must not fail the chat.
func ReadHookEvent(in *bufio.Reader) HookEvent {
	var ev HookEvent
	dec := json.NewDecoder(in)
	_ = dec.Decode(&ev)
	return ev
}

// ViewContext is the text a SessionStart hook prints as context: the view
// with a short note on the two tools. "" when the store holds no message.
func (c *Chat) ViewContext() string {
	lines := BranchLines(c.cfg.Dir)
	if c.Count() == 0 && lines == "" {
		return ""
	}
	var b strings.Builder
	if IsBranch(c.cfg.Dir) {
		bi, _ := ReadBranch(c.cfg.Dir)
		fmt.Fprintf(&b, "Pando memory: the view of the branch %s, the chats of this agent's own part of the tree. ", bi.Path)
	} else {
		b.WriteString("Pando memory: the view of every chat the owner and the household had with any agent. ")
	}
	b.WriteString("Each line is a node named first+count. Use the pando tools: recall(query) finds where a word was said, ")
	b.WriteString("zoom(id, n) opens a node, date(id) gives the time, view() shows this again. ")
	b.WriteString("Fine detail is near now; the past is coarse.\n\n")
	b.WriteString(c.Render())
	// Messages the view does not hold yet (their nodes wait for a model)
	// are shown word for word, newest 20, so nothing said is out of sight.
	if n := c.NotInView(); n > 0 {
		from := c.view.Count
		if n > 20 {
			from = c.Count() - 20
		}
		b.WriteString("Not in the view yet:\n")
		for i := from; i < c.Count(); i++ {
			if m, err := c.Message(i); err == nil {
				fmt.Fprintf(&b, "%d: %s\n", i, clip(flat(m.Text), 300))
			}
		}
	}
	b.WriteString(lines)
	return b.String()
}

// SkipSession puts the session of a hook event on the skip list: its id and
// its transcript path. It never fails the hook. A hook runs on every event of
// a long session, so an id that is listed already is not written again: the
// list grows by a session, not by a turn.
func SkipSession(list string, ev HookEvent) {
	have := LoadSkip(list)
	var add []string
	for _, id := range []string{ev.SessionID, ev.TranscriptPath} {
		if id = strings.TrimSpace(id); id != "" && !have[id] {
			add = append(add, id)
		}
	}
	_ = AppendSkip(list, add...)
}

// HookLogName is the file in the store dir where a hook writes its errors.
const HookLogName = "hook.log"

// RunHook handles one Claude Code hook call and returns the text for
// standard output. It never returns an error: a failure goes to hook.log
// in the store and the chat goes on. SessionStart prints the view as
// context. UserPromptSubmit and Stop log the transcript's new messages.
func RunHook(ctx context.Context, dir string, ev HookEvent, skip []string, now func() time.Time) (out string) {
	logErr := func(err error) {
		if err == nil {
			return
		}
		_ = os.MkdirAll(dir, 0o700)
		f, e := os.OpenFile(filepath.Join(dir, HookLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if e != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s %s: %v\n", time.Now().UTC().Format(DateLayout), ev.Event, err)
	}
	defer func() {
		if r := recover(); r != nil {
			logErr(fmt.Errorf("panic: %v", r))
			out = ""
		}
	}()
	switch ev.Event {
	case "SessionStart":
		text, err := ViewContextOf(dir)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logErr(err)
			}
			return ""
		}
		if text == "" {
			return ""
		}
		b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "SessionStart", "additionalContext": text}})
		return string(b) + "\n"
	case "UserPromptSubmit", "Stop", "SessionEnd", "PreCompact":
		if ev.TranscriptPath == "" {
			return ""
		}
		c, err := OpenWait(ctx, Config{Dir: dir, Now: now}, 3*time.Second)
		if err != nil {
			logErr(err)
			return ""
		}
		defer c.Close()
		if _, err := c.IngestClaude(ctx, ev.TranscriptPath, skip); err != nil {
			logErr(err)
		}
	}
	return ""
}
