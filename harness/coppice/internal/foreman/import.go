package foreman

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Import of old Claude Code and Codex chats (FM-R-71 to FM-R-77).
//
// Import is the owner's choice and never automatic. Two passes: Scan counts
// what a transcript tree holds, with no model call and no write; the
// Importer then feeds the messages one at a time into the chat as kind
// past. The queue of sessions and the owner's choice live in import.json,
// apart from the chat, so a pause and a restart need nothing from the log.
// Every imported message carries an act id built from its transcript, so a
// message is never logged twice, whatever a crash or a re-import does.

// ImportFile is the state file in the foreman dir.
const ImportFile = "import.json"

// Source names of the two transcript kinds.
const (
	SrcClaude = "claude"
	SrcCodex  = "codex"
)

// importToolBytes caps the text of a tool call or its result.
const importToolBytes = 2000

// ImportOpts says what a scan or an import takes.
type ImportOpts struct {
	// From is "claude", "codex" or "all" (or "": all).
	From string
	// Since, when set, drops messages before it.
	Since time.Time
	// Tools adds tool calls and their results. Thinking is never taken.
	Tools bool
	// Skip are dirs whose sessions are never taken: a session whose cwd is
	// under one is coppice's own (FM-R-78).
	Skip []string
}

func (o ImportOpts) takes(src string) bool { return o.From == "" || o.From == "all" || o.From == src }

// ImportMsg is one message of a transcript, ready for the chat.
type ImportMsg struct {
	Text string
	TS   time.Time
	Act  string
}

// ImportSession is one transcript file: its key, where it is, and how many
// new messages it holds.
type ImportSession struct {
	Key, Src, Path string
	// New is the number of messages not in the chat yet.
	New int
	// First is the time of the first of them.
	First time.Time
}

// ParseImportTime reads an RFC 3339 time, to the second, in UTC.
func ParseImportTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC().Truncate(time.Second), true
}

// ImportStateWord is what the owner sees of the choice and the queue:
// off (not chosen), declined, running, paused or done.
func (s ImportState) Word() string {
	switch {
	case s.Decision == "declined":
		return "declined"
	case s.Decision != "accepted":
		return "off"
	case s.Paused:
		return "paused"
	case len(s.Queue) == 0:
		return "done"
	}
	return "running"
}

// ImportQueued is one session waiting to be imported.
type ImportQueued struct {
	Key  string `json:"key"`
	Src  string `json:"src"`
	Path string `json:"path"`
	// N is how many new messages the scan found; Fed how many are logged.
	N   int `json:"n"`
	Fed int `json:"fed"`
	// Since and Tools are the options this session was accepted with. A
	// later accept with other options changes only the sessions it adds
	// (FM-R-79).
	Since string `json:"since"`
	Tools bool   `json:"tools"`
}

// Opts is the scan options this session was queued with.
func (q ImportQueued) Opts(st ImportState) ImportOpts {
	o := ImportOpts{From: st.From, Tools: q.Tools}
	if t, ok := ParseImportTime(q.Since); ok {
		o.Since = t
	}
	return o
}

// ImportState is import.json.
type ImportState struct {
	V        int    `json:"v"`
	Offered  bool   `json:"offered"`
	Decision string `json:"decision"`
	Paused   bool   `json:"paused"`
	From     string `json:"from"`
	Since    string `json:"since"`
	Tools    bool   `json:"tools"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`
	// Last is the time of the newest message logged for each session.
	Last  map[string]string `json:"last"`
	Queue []ImportQueued    `json:"queue"`
}

// Opts is the scan options the state holds.
func (s ImportState) Opts() ImportOpts {
	o := ImportOpts{From: s.From, Tools: s.Tools}
	if t, ok := ParseImportTime(s.Since); ok {
		o.Since = t
	}
	return o
}

// ReadImportState reads import.json in dir. A missing file is the empty
// state. A file that cannot be read as state is set aside as
// import.json.broken-<n> and the empty state is returned, so import never
// stops coppice.
func ReadImportState(dir string) (ImportState, error) {
	path := filepath.Join(dir, ImportFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyImport(), nil
	}
	if err != nil {
		return ImportState{}, err
	}
	var s ImportState
	if json.Unmarshal(data, &s) != nil || s.V != 1 {
		_ = os.Rename(path, freeName(path+".broken"))
		return emptyImport(), nil
	}
	if s.Last == nil {
		s.Last = map[string]string{}
	}
	if s.Queue == nil {
		s.Queue = []ImportQueued{}
	}
	return s, nil
}

// emptyImport is the state before any choice. Its map and list are empty,
// not null, so the file reads the same in both ports.
func emptyImport() ImportState {
	return ImportState{V: 1, Last: map[string]string{}, Queue: []ImportQueued{}}
}

// UpdateImportState reads import.json under a short lock, lets fn change
// it, and writes it back, so the command and the server never lose each
// other's change.
func UpdateImportState(dir string, fn func(*ImportState) error) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	unlock, err := lockImport(filepath.Join(dir, "import.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadImportState(dir)
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	data, err := encodeLine(s)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, ImportFile), data)
}

// ---- transcripts ----

// claudeSkip are the user texts Claude Code writes for its own commands.
var claudeSkip = []string{"<command-name>", "<command-message>", "<local-command-", "<system-reminder>",
	"Caveat: The messages below were generated by the user while running local commands."}

// codexSkip are the user texts Codex adds for its own setup.
var codexSkip = []string{"<environment_context>", "<user_instructions>", "<INSTRUCTIONS>", "# AGENTS.md"}

func hasAnyPrefix(s string, ps []string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func objOf(v any) map[string]any { m, _ := v.(map[string]any); return m }

func strOf(v any) string { s, _ := v.(string); return s }

// blockText joins the text of a content value: a string, or the text of
// each block that has the given types.
func blockText(content any, types ...string) string {
	if s, ok := content.(string); ok {
		return s
	}
	list, _ := content.([]any)
	var parts []string
	for _, b := range list {
		o := objOf(b)
		if o == nil {
			continue
		}
		ok := false
		for _, t := range types {
			if strOf(o["type"]) == t {
				ok = true
			}
		}
		if t, isStr := o["text"].(string); ok && isStr {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// callSummary is a tool call on one line: its name and the one field that
// says what it does.
func callSummary(name string, input any) string {
	o := objOf(input)
	line := name
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "prompt"} {
		var v string
		switch x := o[k].(type) {
		case string:
			v = x
		case []any:
			var ps []string
			for _, p := range x {
				ps = append(ps, strOf(p))
			}
			v = strings.Join(ps, " ")
		}
		if v != "" {
			line += " " + v
			break
		}
	}
	return clip(line, importToolBytes)
}

// readHook, when set, sees the path of every transcript read. Tests use it
// to prove where a read runs.
var readHook func(path string)

// sessionRead is what one transcript holds.
type sessionRead struct {
	key     string
	project string
	msgs    []ImportMsg
}

// underAny reports whether dir is one of the dirs or inside one. Both are
// compared as written and with symlinks resolved, because Claude Code
// records the resolved cwd and a dir may be reached through a link (N-3).
func underAny(dir string, dirs []string) bool {
	if dir == "" {
		return false
	}
	forms := pathForms(dir)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		for _, df := range pathForms(d) {
			for _, f := range forms {
				if f == df || strings.HasPrefix(f, strings.TrimRight(df, "/")+"/") {
					return true
				}
			}
		}
	}
	return false
}

// pathForms is the cleaned path, and the symlink-resolved one when the path
// exists and differs.
func pathForms(p string) []string {
	p = filepath.Clean(p)
	// Resolve the longest part that exists: a cwd may be gone by now.
	for head, tail := p, ""; ; {
		if r, err := filepath.EvalSymlinks(head); err == nil {
			if r = filepath.Join(r, tail); r != p {
				return []string{p, r}
			}
			break
		}
		up := filepath.Dir(head)
		if up == head {
			break
		}
		tail = filepath.Join(filepath.Base(head), tail)
		head = up
	}
	return []string{p}
}

// readSession reads the importable messages of one transcript file. A line
// that is not JSON, or has no time, is skipped. A session of coppice's own
// (its cwd is under o.Skip, or its first user message is a compactor
// question) gives no message (FM-R-78).
func readSession(path, src string, o ImportOpts) (sessionRead, error) {
	return readSessionKey(path, src, o, "")
}

// readSessionKey is readSession with the queue key fixed by the caller, for
// a second file that holds the same session id (FM-R-80).
func readSessionKey(path, src string, o ImportOpts, force string) (sessionRead, error) {
	if readHook != nil {
		readHook(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return sessionRead{}, err
	}
	defer f.Close()
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	r := sessionRead{key: src + ":" + stem}
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		return sessionRead{}, err
	}
	// First pass: the session id and the project, which every message names.
	cwd, sid := "", ""
	for _, ln := range lines {
		var v map[string]any
		if json.Unmarshal(ln, &v) != nil {
			continue
		}
		if src == SrcClaude {
			if sid == "" {
				sid = strOf(v["sessionId"])
			}
			if cwd == "" {
				cwd = strOf(v["cwd"])
			}
		} else if strOf(v["type"]) == "session_meta" {
			p := objOf(v["payload"])
			if sid == "" {
				sid = strOf(p["id"])
			}
			if cwd == "" {
				cwd = strOf(p["cwd"])
			}
		}
	}
	if sid != "" {
		r.key = src + ":" + sid
		// A file whose name does not hold its session id (a copy, a moved
		// or forked transcript) is its own session for the queue.
		if !strings.Contains(stem, sid) {
			r.key += "~" + stem
		}
	}
	if force != "" {
		r.key = force
	}
	if underAny(cwd, o.Skip) {
		return r, nil
	}
	if cwd != "" {
		r.project = filepath.Base(cwd)
	} else if src == SrcClaude {
		r.project = filepath.Base(filepath.Dir(path))
	}
	head := src + " "
	where := ""
	if r.project != "" {
		where = " in " + r.project
	}
	add := func(role, text string, ts time.Time, act string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if !o.Since.IsZero() && ts.Before(o.Since) {
			return
		}
		r.msgs = append(r.msgs, ImportMsg{Text: head + role + where + ": " + text, TS: ts, Act: act})
	}
	seen := map[string]int{}
	for n, ln := range lines {
		var v map[string]any
		if json.Unmarshal(ln, &v) != nil {
			continue
		}
		ts, ok := ParseImportTime(strOf(v["timestamp"]))
		if !ok {
			continue
		}
		if src == SrcClaude {
			readClaudeLine(v, n, r.key, o, ts, add)
		} else {
			readCodexLine(v, codexAct(ln, seen, sid, r.key), o, ts, add)
		}
	}
	// A compactor call of coppice itself: its one user message is the
	// compactor's question (FM-R-78).
	user := head + "user"
	for _, m := range r.msgs {
		if strings.HasPrefix(m.Text, user) {
			if strings.Contains(m.Text, CompactorMarker) {
				r.msgs = nil
			}
			break
		}
	}
	return r, nil
}

func readClaudeLine(v map[string]any, n int, key string, o ImportOpts, ts time.Time, add func(role, text string, ts time.Time, act string)) {
	typ := strOf(v["type"])
	if typ != "user" && typ != "assistant" {
		return
	}
	if v["isMeta"] == true || v["isSidechain"] == true {
		return
	}
	id := strOf(v["uuid"])
	if id == "" {
		id = fmt.Sprintf("%s.%d", key, n)
	}
	base := "imp:claude:" + id
	content := objOf(v["message"])["content"]
	text := blockText(content, "text")
	if !(typ == "user" && hasAnyPrefix(text, claudeSkip)) {
		add(typ, text, ts, base)
	}
	if !o.Tools {
		return
	}
	list, _ := content.([]any)
	for k, b := range list {
		bo := objOf(b)
		switch strOf(bo["type"]) {
		case "tool_use":
			add("tool", callSummary(strOf(bo["name"]), bo["input"]), ts, fmt.Sprintf("%s:t%d", base, k))
		case "tool_result":
			add("tool result", clip(blockText(bo["content"], "text"), importToolBytes), ts, fmt.Sprintf("%s:t%d", base, k))
		}
	}
}

// codexAct names a Codex line by its session id and its own bytes, so a
// copied or forked rollout that repeats a line brings it once (N-5). A line
// that repeats inside one file gets a count after its hash.
func codexAct(ln []byte, seen map[string]int, sid, key string) string {
	if sid == "" {
		sid = key
	}
	sum := sha256.Sum256(ln)
	h := hex.EncodeToString(sum[:8])
	k := seen[h]
	seen[h]++
	act := "imp:codex:" + sid + ":" + h
	if k > 0 {
		act += fmt.Sprintf(".%d", k)
	}
	return act
}

func readCodexLine(v map[string]any, act string, o ImportOpts, ts time.Time, add func(role, text string, ts time.Time, act string)) {
	if strOf(v["type"]) != "response_item" {
		return
	}
	p := objOf(v["payload"])
	switch strOf(p["type"]) {
	case "message":
		role := strOf(p["role"])
		if role != "user" && role != "assistant" {
			return
		}
		text := blockText(p["content"], "input_text", "output_text", "text")
		if role == "user" && hasAnyPrefix(strings.TrimSpace(text), codexSkip) {
			return
		}
		add(role, text, ts, act)
	case "function_call":
		if !o.Tools {
			return
		}
		var args any
		_ = json.Unmarshal([]byte(strOf(p["arguments"])), &args)
		add("tool", callSummary(strOf(p["name"]), args), ts, act)
	case "function_call_output":
		if !o.Tools {
			return
		}
		add("tool result", clip(strOf(p["output"]), importToolBytes), ts, act)
	}
}

// transcriptFiles lists the transcript files under home for the sources o
// takes, in path order.
func transcriptFiles(home string, o ImportOpts) []ImportQueued {
	var out []ImportQueued
	if o.takes(SrcClaude) {
		names, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
		sort.Strings(names)
		for _, p := range names {
			out = append(out, ImportQueued{Src: SrcClaude, Path: p})
		}
	}
	if o.takes(SrcCodex) {
		root := filepath.Join(home, ".codex", "sessions")
		var names []string
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
				names = append(names, p)
			}
			return nil
		})
		sort.Strings(names)
		for _, p := range names {
			out = append(out, ImportQueued{Src: SrcCodex, Path: p})
		}
	}
	return out
}

// TranscriptSig is a cheap signature of the transcript files under home:
// their count, the newest mtime and the total size. It reads no file. A
// cached estimate is fresh only while the signature is the same (F-1).
func TranscriptSig(home string) string {
	files := transcriptFiles(home, ImportOpts{})
	var newest, size int64
	for _, f := range files {
		if fi, err := os.Stat(f.Path); err == nil {
			if t := fi.ModTime().UnixNano(); t > newest {
				newest = t
			}
			size += fi.Size()
		}
	}
	return fmt.Sprintf("%d/%d/%d", len(files), newest, size)
}

// newMsgs are the messages of r that are not in the chat. With the chat at
// hand the act ids decide, exactly: a later import with tool calls on still
// brings the calls that are older than the last message. Without it (the
// server's estimate), a message older than the session's last logged time
// counts as imported.
func newMsgs(r sessionRead, last string, has func(string) bool) []ImportMsg {
	lt, haveLast := ParseImportTime(last)
	var out []ImportMsg
	for _, m := range r.msgs {
		if has != nil {
			if has(m.Act) {
				continue
			}
		} else if haveLast && m.TS.Before(lt) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Scan is the estimate pass: it reads the transcripts under home and lists
// the sessions that hold new messages, oldest first (first message time,
// then key). It calls no model and writes nothing. has says whether an act
// id is in the chat already; nil means none is.
func Scan(home string, o ImportOpts, st ImportState, has func(string) bool) []ImportSession {
	return ScanContext(context.Background(), home, o, st, has)
}

// ScanContext is Scan that stops between files when ctx ends.
func ScanContext(ctx context.Context, home string, o ImportOpts, st ImportState, has func(string) bool) []ImportSession {
	var out []ImportSession
	seen := map[string]bool{}
	for _, f := range transcriptFiles(home, o) {
		if ctx.Err() != nil {
			return nil
		}
		r, err := readSession(f.Path, f.Src, o)
		if err != nil {
			continue
		}
		if seen[r.key] {
			// Two files with one session id and one name: the second is its
			// own session, keyed by its path (FM-R-80).
			sum := sha256.Sum256([]byte(f.Path))
			key := r.key + "~" + hex.EncodeToString(sum[:4])
			if seen[key] {
				continue
			}
			if r, err = readSessionKey(f.Path, f.Src, o, key); err != nil {
				continue
			}
		}
		seen[r.key] = true
		fresh := newMsgs(r, st.Last[r.key], has)
		if len(fresh) == 0 {
			continue
		}
		out = append(out, ImportSession{Key: r.key, Src: f.Src, Path: f.Path, New: len(fresh), First: fresh[0].TS})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].First.Equal(out[b].First) {
			return out[a].First.Before(out[b].First)
		}
		return out[a].Key < out[b].Key
	})
	return out
}

// ImportCount totals a scan.
type ImportCount struct{ Sessions, Messages int }

// Totals counts the sessions and messages of a scan, all and by source.
func Totals(ss []ImportSession) (all ImportCount, by map[string]ImportCount) {
	by = map[string]ImportCount{}
	for _, s := range ss {
		all.Sessions++
		all.Messages += s.New
		c := by[s.Src]
		c.Sessions++
		c.Messages += s.New
		by[s.Src] = c
	}
	return all, by
}

func plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// Describe is the estimate as the owner reads it.
func Describe(ss []ImportSession) string {
	all, by := Totals(ss)
	var b strings.Builder
	fmt.Fprintf(&b, "past chats: %d (about %s)\n", all.Sessions, plural(all.Messages, "message"))
	for _, src := range []string{SrcClaude, SrcCodex} {
		if c, ok := by[src]; ok {
			fmt.Fprintf(&b, "  %s: %s, %s\n", src, plural(c.Sessions, "chat"), plural(c.Messages, "message"))
		}
	}
	return b.String()
}

// Accept records the owner's choice to import and queues the sessions the
// scan found that are not queued already. It resets the progress when
// nothing was queued before.
func Accept(dir string, o ImportOpts, ss []ImportSession) error {
	return UpdateImportState(dir, func(st *ImportState) error {
		st.Decision, st.Paused, st.Offered = "accepted", false, true
		st.From, st.Tools = o.From, o.Tools
		st.Since = ""
		if !o.Since.IsZero() {
			st.Since = o.Since.Format(DateLayout)
		}
		if len(st.Queue) == 0 {
			st.Total, st.Done = 0, 0
		}
		have := map[string]bool{}
		for _, q := range st.Queue {
			have[q.Key] = true
		}
		for _, s := range ss {
			if have[s.Key] {
				continue
			}
			st.Queue = append(st.Queue, ImportQueued{Key: s.Key, Src: s.Src, Path: s.Path, N: s.New, Since: st.Since, Tools: o.Tools})
			st.Total += s.New
		}
		return nil
	})
}

// Decline records that the owner said no. The command still works later.
func Decline(dir string) error {
	return UpdateImportState(dir, func(st *ImportState) error {
		st.Decision, st.Offered = "declined", true
		return nil
	})
}

// SetPaused pauses or resumes the queue.
func SetPaused(dir string, paused bool) error {
	return UpdateImportState(dir, func(st *ImportState) error {
		st.Paused = paused
		return nil
	})
}

// ---- the importer ----

// Importer feeds the queue into a chat, one message at a time.
type Importer struct {
	Dir string
	// Skip are the dirs whose sessions are never taken (FM-R-78).
	Skip []string

	cur string
	// raw is the session as read; pending is raw without the messages the
	// chat holds. The read runs outside the chat lock (preload); the filter
	// needs the chat.
	raw      []ImportMsg
	filtered bool
	pending  []ImportMsg
}

// load reads the queue's head session, with the options it was queued with.
func (im *Importer) load(q ImportQueued, st ImportState) {
	im.cur, im.raw, im.pending, im.filtered = q.Key, nil, nil, false
	o := q.Opts(st)
	o.Skip = im.Skip
	if r, err := readSessionKey(q.Path, q.Src, o, q.Key); err == nil {
		im.raw = r.msgs
	}
}

// preload reads the head session ahead of Step, so a long transcript is
// never parsed under the chat lock (FM-R-81).
func (im *Importer) preload() {
	st, err := ReadImportState(im.Dir)
	if err != nil || st.Decision != "accepted" || st.Paused || len(st.Queue) == 0 || im.cur == st.Queue[0].Key {
		return
	}
	im.load(st.Queue[0], st)
}

// HasAct says whether the log holds a message with this client act id.
func (c *Chat) HasAct(act string) bool { return len(c.acts[act]) > 0 }

// importDone says whether the log holds all of an imported message. A
// message with some of its parts logged (an append that failed part way) is
// not done; an id that carries other words is done, so it is never logged
// twice under one id (FM-R-82).
func (c *Chat) importDone(m ImportMsg) bool {
	if !c.HasAct(m.Act) {
		return false
	}
	if !c.ActMatches(m.Act, m.Text) {
		return true
	}
	return len(c.acts[m.Act]) >= len(split(strings.ToValidUTF8(m.Text, "�"), c.cfg.SplitBytes))
}

// AppendImported logs one past message, dated when it was said, with the
// act id that keeps it from being logged twice. A call after a failed one
// logs only the parts the log does not hold, and none when it holds all
// (FM-R-82).
func (c *Chat) AppendImported(ctx context.Context, text string, date time.Time, act string) ([]int64, error) {
	have := c.acts[act]
	if len(have) == 0 {
		return c.appendAct(ctx, KindPast, text, date, act, 0)
	}
	if !c.ActMatches(act, text) || c.importDone(ImportMsg{Text: text, Act: act}) {
		return have, nil
	}
	return c.appendAct(ctx, KindPast, text, date, act, len(have))
}

// Step logs the next queued message. fed is false when there is nothing to
// log now: no choice made, paused, or the queue empty. The caller drains
// the chat before it steps again, so at most one message waits for the
// compactor. A session whose file is gone is dropped from the queue.
func (im *Importer) Step(ctx context.Context, c *Chat) (fed bool, err error) {
	fed, _, err = im.step(ctx, c, true)
	return fed, err
}

// step is Step. With load false it never reads a transcript: when the head
// session is not the one in hand, or a session ended, it says again and the
// caller preloads outside the chat lock and steps again (FM-R-81).
func (im *Importer) step(ctx context.Context, c *Chat, load bool) (fed, again bool, err error) {
	st, err := ReadImportState(im.Dir)
	if err != nil {
		return false, false, err
	}
	for st.Decision == "accepted" && !st.Paused && len(st.Queue) > 0 {
		q := st.Queue[0]
		if im.cur != q.Key {
			if !load {
				return false, true, nil
			}
			im.load(q, st)
		}
		if !im.filtered {
			im.pending = im.pending[:0]
			for _, m := range im.raw {
				if !c.importDone(m) {
					im.pending = append(im.pending, m)
				}
			}
			im.raw, im.filtered = nil, true
		}
		if len(im.pending) == 0 {
			im.cur = ""
			if err := UpdateImportState(im.Dir, func(s *ImportState) error { return s.pop(q.Key) }); err != nil {
				return false, false, err
			}
			if !load {
				// The next head is read outside the lock.
				return false, true, nil
			}
			if st, err = ReadImportState(im.Dir); err != nil {
				return false, false, err
			}
			continue
		}
		m := im.pending[0]
		if _, err := c.AppendImported(ctx, m.Text, m.TS, m.Act); err != nil {
			return false, false, err
		}
		im.pending = im.pending[1:]
		stamp := m.TS.Format(DateLayout)
		err := UpdateImportState(im.Dir, func(s *ImportState) error {
			if s.Last == nil {
				s.Last = map[string]string{}
			}
			if lt, ok := ParseImportTime(s.Last[q.Key]); !ok || lt.Before(m.TS) {
				s.Last[q.Key] = stamp
			}
			for k := range s.Queue {
				if s.Queue[k].Key == q.Key {
					s.Queue[k].Fed++
					s.Done++
				}
			}
			if len(im.pending) == 0 {
				return s.pop(q.Key)
			}
			return nil
		})
		return true, false, err
	}
	return false, false, nil
}

// Feed logs the next queued message when the chat has nothing left to
// build and idle says no live work waits (no turn runs, none is queued).
// It is the import side of the one compactor loop: the loop drains the
// live nodes first, then feeds one import message, then drains again, so a
// live message never waits behind more than the one import message in
// flight, and an import call never starts while a live node is pending
// (FM-R-72). mu is the chat lock; nil means none.
func (im *Importer) Feed(ctx context.Context, c *Chat, mu sync.Locker, idle func() bool) (fed bool, err error) {
	for {
		// idle takes the server's turn lock, which talk holds while it takes
		// the chat lock: ask it before the chat lock, never under it. A turn
		// queued after the ask lets this one message through.
		if idle != nil && !idle() {
			return false, nil
		}
		im.preload()
		again, fed, err := im.feedLocked(ctx, c, mu)
		if err != nil || fed || !again {
			return fed, err
		}
	}
}

func (im *Importer) feedLocked(ctx context.Context, c *Chat, mu sync.Locker) (again, fed bool, err error) {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	if c.Pending() > 0 {
		return false, false, nil
	}
	fed, again, err = im.step(ctx, c, false)
	return again, fed, err
}

// pop takes a finished session off the queue. A session that gave fewer
// messages than the scan counted (a shorter file, a repeat) lowers the
// total, so done reaches total.
func (s *ImportState) pop(key string) error {
	for k, q := range s.Queue {
		if q.Key == key {
			if q.Fed < q.N {
				s.Total -= q.N - q.Fed
			}
			s.Queue = append(s.Queue[:k:k], s.Queue[k+1:]...)
			return nil
		}
	}
	return nil
}

// Progress is "done of total" for the status line.
func (s ImportState) Progress() string {
	return fmt.Sprintf("%d of %s", s.Done, plural(s.Total, "message"))
}
