package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters/sprig"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// Tokens is token use summed over an agent's turns. Fresh is input read
// fresh, CacheRead and CacheWrite are prompt cache reads and writes, and
// Out is output.
type Tokens struct {
	Fresh      int64 `json:"fresh"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Out        int64 `json:"out"`
}

func (a *Tokens) add(b Tokens) {
	a.Fresh += b.Fresh
	a.CacheRead += b.CacheRead
	a.CacheWrite += b.CacheWrite
	a.Out += b.Out
}

func (a *Tokens) sub(b Tokens) {
	a.Fresh -= b.Fresh
	a.CacheRead -= b.CacheRead
	a.CacheWrite -= b.CacheWrite
	a.Out -= b.Out
}

// nonNeg drops a negative count. A usage field is a count, and a negative
// one is a broken line, not a refund.
func nonNeg(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// transcriptKind names the line format a tailer reads.
type transcriptKind int

const (
	// kindClaude is a Claude Code transcript: one JSON object per line. An
	// assistant entry carries message.id, message.model and message.usage.
	// Claude Code writes one line per content block of a message, and each
	// of those lines repeats the message id and its usage.
	kindClaude transcriptKind = iota
	// kindSprig is a sprig session tree: one JSON object per line. An
	// assistant entry carries a unique id, model and usage with fresh,
	// cacheRead, cacheWrite and out. A verdict entry carries the gate's
	// decision on a tool call.
	kindSprig
)

// tailCap is the most bytes one poll reads. A transcript that grew by more
// is caught up over later polls. A single line longer than this is skipped.
const tailCap = 4 << 20

// idKeep is how many message ids a tailer remembers to count a split
// message once. Claude writes the lines of one message together, so a
// short memory is enough.
const idKeep = 1024

// gateFact is the last verdict known for a pane. At is unix seconds.
type gateFact struct {
	Decision string  `json:"decision"`
	Tool     string  `json:"tool"`
	Clause   string  `json:"clause"`
	At       float64 `json:"at"`
}

// tailer reads one transcript file from the start, then only the bytes
// added since its last read. It is not safe for concurrent use: its owner
// holds a lock around poll.
type tailer struct {
	path string
	kind transcriptKind
	cap  int64
	// open opens path for a read. See openRegular.
	open func(path string) (*os.File, uint64, uint64, bool, error)
	// mu is held for the whole of a poll. Every field below it is the
	// poll's own. snap is a copy for readers, under the owning paneFacts'
	// lock, so a reader never waits on a read.
	mu   sync.Mutex
	snap tailSnap

	off      int64
	dev, ino uint64
	opened   bool
	// skipping is true inside a line longer than cap, until its newline.
	skipping bool
	// refused is true once the path proved not to be a regular file. A
	// refused path is never opened again.
	refused bool
	// ok is true once one read succeeded, so a zero sum is a known zero.
	ok bool

	sum   Tokens
	model string
	byID  map[string]Tokens
	ids   []string

	// Sprig only: the last verdict and the mode it was made in, and the
	// last tool call, whose name the verdict that follows it takes.
	verdict      *gateFact
	mode         string
	lastCallID   string
	lastCallName string
}

func newTailer(path string, kind transcriptKind) *tailer {
	return &tailer{path: path, kind: kind, cap: tailCap, open: openRegular, byID: map[string]Tokens{}}
}

// tailSnap is what a tailer knew after its last poll.
type tailSnap struct {
	ok       bool
	refused  bool
	sum      Tokens
	model    string
	verdict  *gateFact
	mode     string
	off      int64
	dev, ino uint64
	skipping bool
}

// snapshot copies the tailer's facts. The caller holds t.mu.
func (t *tailer) snapshot() tailSnap {
	sn := tailSnap{
		ok: t.ok, refused: t.refused, sum: t.sum, model: t.model, mode: t.mode,
		off: t.off, dev: t.dev, ino: t.ino, skipping: t.skipping,
	}
	if t.verdict != nil {
		v := *t.verdict
		sn.verdict = &v
	}
	return sn
}

// reset forgets everything read, for a file that was cut short or
// replaced.
func (t *tailer) reset() {
	t.off, t.skipping = 0, false
	t.sum, t.model = Tokens{}, ""
	t.byID, t.ids = map[string]Tokens{}, nil
	t.verdict, t.mode, t.lastCallID, t.lastCallName = nil, "", "", ""
}

var errRefused = errors.New("this path is not a regular file and is never read")

// poll reads the bytes added since the last poll, at most cap of them, and
// folds each complete line into the facts. A line not yet ended is left
// for the next poll.
func (t *tailer) poll() error {
	if t.refused {
		return errRefused
	}
	if t.kind == kindClaude {
		// The claim is on the real path. A path that now leads elsewhere
		// is not read.
		if err := verifyReal(t.path); err != nil {
			t.refused = true
			t.reset()
			t.ok = false
			return err
		}
	}
	f, dev, ino, refuse, err := t.open(t.path)
	if err != nil {
		if refuse {
			t.refused = true
			t.reset()
			t.ok = false
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if t.opened && (dev != t.dev || ino != t.ino || fi.Size() < t.off) {
		t.reset()
	}
	t.opened, t.dev, t.ino, t.ok = true, dev, ino, true
	n := fi.Size() - t.off
	if n <= 0 {
		return nil
	}
	if n > t.cap {
		n = t.cap
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, t.off)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	buf = buf[:got]
	start := 0
	if t.skipping {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			t.off += int64(len(buf))
			return nil
		}
		start = i + 1
		t.skipping = false
	}
	last := bytes.LastIndexByte(buf[start:], '\n')
	if last < 0 {
		if start == 0 && int64(len(buf)) >= t.cap {
			// One line fills the whole read. Skip it to its end.
			t.skipping = true
			t.off += int64(len(buf))
			return nil
		}
		t.off += int64(start)
		return nil
	}
	end := start + last + 1
	for _, line := range bytes.Split(buf[start:end-1], []byte{'\n'}) {
		t.line(line)
	}
	t.off += int64(end)
	return nil
}

// assistantMark is a cheap test that a line may be an assistant entry, so
// a long tool result line is not decoded for nothing.
var assistantMark = []byte(`"assistant"`)

var verdictMark = []byte(`"verdict"`)

var toolCallMark = []byte(`"tool_call"`)

func (t *tailer) line(b []byte) {
	switch t.kind {
	case kindClaude:
		if bytes.Contains(b, assistantMark) {
			t.claudeLine(b)
		}
	case kindSprig:
		switch {
		case bytes.Contains(b, assistantMark):
			t.sprigAssistant(b)
		case bytes.Contains(b, toolCallMark), bytes.Contains(b, verdictMark):
			t.sprigGate(b)
		}
	}
}

func (t *tailer) claudeLine(b []byte) {
	var e struct {
		Type    string `json:"type"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				Out        int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &e) != nil || e.Type != "assistant" || e.Message == nil {
		return
	}
	m := e.Message
	// Claude Code writes an assistant entry of its own, not from any model,
	// for some errors. Its model is "<synthetic>".
	if m.Model != "" && m.Model != "<synthetic>" {
		t.model = truncate(m.Model, 200)
	}
	if u := m.Usage; u != nil {
		t.count(m.ID, Tokens{
			Fresh: nonNeg(u.Input), CacheRead: nonNeg(u.CacheRead),
			CacheWrite: nonNeg(u.CacheWrite), Out: nonNeg(u.Out),
		})
	}
}

func (t *tailer) sprigAssistant(b []byte) {
	var e struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Fresh      int64 `json:"fresh"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
			Out        int64 `json:"out"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &e) != nil || e.Type != "assistant" {
		return
	}
	if e.Model != "" {
		t.model = truncate(e.Model, 200)
	}
	if u := e.Usage; u != nil {
		t.count(e.ID, Tokens{
			Fresh: nonNeg(u.Fresh), CacheRead: nonNeg(u.CacheRead),
			CacheWrite: nonNeg(u.CacheWrite), Out: nonNeg(u.Out),
		})
	}
}

func (t *tailer) sprigGate(b []byte) {
	var e struct {
		Type      string  `json:"type"`
		TS        float64 `json:"ts"`
		ToolUseID string  `json:"toolUseId"`
		Name      string  `json:"name"`
		Decision  string  `json:"decision"`
		Mode      string  `json:"mode"`
		Clause    string  `json:"clause"`
	}
	if json.Unmarshal(b, &e) != nil {
		return
	}
	switch e.Type {
	case "tool_call":
		t.lastCallID, t.lastCallName = e.ToolUseID, e.Name
	case "verdict":
		if e.Decision != "allow" && e.Decision != "deny" {
			return
		}
		v := &gateFact{Decision: e.Decision, Clause: truncate(e.Clause, 200), At: e.TS}
		if e.ToolUseID != "" && e.ToolUseID == t.lastCallID {
			v.Tool = truncate(t.lastCallName, 200)
		}
		t.verdict = v
		switch e.Mode {
		case "enforce":
			t.mode = "enforcing"
		case "audit", "shadow":
			// shadow is the old name of audit mode: a session tree an
			// older daisugi wrote still says it.
			t.mode = "watching"
		}
	}
}

// count adds one entry's usage. An entry whose id was already counted
// replaces that count, so the lines of one split message count once.
func (t *tailer) count(id string, u Tokens) {
	if id == "" {
		t.sum.add(u)
		return
	}
	if prev, ok := t.byID[id]; ok {
		t.sum.sub(prev)
		t.sum.add(u)
		t.byID[id] = u
		return
	}
	t.sum.add(u)
	t.byID[id] = u
	t.ids = append(t.ids, id)
	if len(t.ids) > idKeep {
		delete(t.byID, t.ids[0])
		t.ids = t.ids[1:]
	}
}

// maxFilesPerPane is how many transcript files one pane reads. A Claude
// pane moves to a new file on /clear. Past this many, the file used
// longest ago is let go.
const maxFilesPerPane = 8

// retiredKeep bounds how many let-go files a pane remembers. Past it, the
// oldest one's tokens join carried and its claim is released.
const retiredKeep = 64

// paneFacts is what the floor knows about one pane beyond its state: the
// last verdict and mode its gate hook reported, and the transcripts it is
// allowed to read. Its lock is never held while a file is read, and never
// taken while Server.factsMu is held, or the reverse.
type paneFacts struct {
	mu      sync.Mutex
	mode    string
	verdict *gateFact
	// path is the transcript read now. files are the ones this pane reads,
	// in order of use, the one used last at the end.
	path  string
	files map[string]*tailer
	order []string
	// retired keeps each file let go of: where reading stopped and what it
	// had counted, so a file used again reads on from there and counts
	// nothing twice. retiredOrder is oldest first.
	retired      map[string]retiredAt
	retiredOrder []string
	// carried is the tokens of files forgotten past retiredKeep.
	carried Tokens
	// model is the model the newest read found, from any of its files.
	model string
	// polling is true while a read of path is in flight. lastPoll is when
	// the last one started. seen is when a report came or tokens grew.
	polling  bool
	lastPoll time.Time
	seen     time.Time
}

// retiredAt is a file let go of: where reading stopped and its tokens.
type retiredAt struct {
	off      int64
	dev, ino uint64
	skipping bool
	sum      Tokens
}

// use makes path the file this pane reads now, adding a tailer for it when
// it is new. It returns the paths it forgot for good, whose claims the
// caller releases once it holds no paneFacts lock.
func (pf *paneFacts) use(path string, kind transcriptKind, open func(string) (*os.File, uint64, uint64, bool, error)) (forgot []string) {
	pf.path = path
	if _, ok := pf.files[path]; ok {
		pf.moveLast(path)
		return nil
	}
	if pf.files == nil {
		pf.files = map[string]*tailer{}
	}
	t := newTailer(path, kind)
	if open != nil {
		t.open = open
	}
	if at, ok := pf.retired[path]; ok {
		// Read on from where this file was let go, with what it had
		// counted. If it was cut short or replaced since, poll starts it
		// again from the start.
		t.off, t.dev, t.ino, t.skipping, t.opened = at.off, at.dev, at.ino, at.skipping, true
		t.sum, t.ok = at.sum, true
		t.snap = t.snapshot()
		pf.forgetRetired(path)
	}
	pf.files[path] = t
	pf.order = append(pf.order, path)
	if len(pf.order) <= maxFilesPerPane {
		return nil
	}
	old := pf.order[0]
	pf.order = pf.order[1:]
	sn := pf.files[old].snap
	delete(pf.files, old)
	if sn.refused {
		return []string{old}
	}
	// A read of old still in flight lands nowhere. What it would have
	// added is read again, from sn.off, if old is used again.
	if pf.retired == nil {
		pf.retired = map[string]retiredAt{}
	}
	pf.retired[old] = retiredAt{off: sn.off, dev: sn.dev, ino: sn.ino, skipping: sn.skipping, sum: sn.sum}
	pf.retiredOrder = append(pf.retiredOrder, old)
	if len(pf.retiredOrder) > retiredKeep {
		gone := pf.retiredOrder[0]
		pf.carried.add(pf.retired[gone].sum)
		pf.forgetRetired(gone)
		forgot = append(forgot, gone)
	}
	return forgot
}

func (pf *paneFacts) forgetRetired(path string) {
	delete(pf.retired, path)
	for i, p := range pf.retiredOrder {
		if p == path {
			pf.retiredOrder = append(pf.retiredOrder[:i:i], pf.retiredOrder[i+1:]...)
			return
		}
	}
}

// moveLast moves path to the end of order, as the file used last.
func (pf *paneFacts) moveLast(path string) {
	for i, p := range pf.order {
		if p == path {
			pf.order = append(append(pf.order[:i:i], pf.order[i+1:]...), path)
			return
		}
	}
}

// tokens is the pane's summed token use. ok is false while no file was
// ever read, so a pane with nothing read shows no tokens at all.
func (pf *paneFacts) tokens() (Tokens, bool) {
	sum, ok := pf.carried, pf.carried != (Tokens{})
	for _, t := range pf.files {
		if t.snap.ok {
			ok = true
		}
		sum.add(t.snap.sum)
	}
	for _, r := range pf.retired {
		ok = true
		sum.add(r.sum)
	}
	return sum, ok
}

// factsFor returns the facts of pane id, making them when make is true.
func (s *Server) factsFor(id string, make bool) *paneFacts {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	pf := s.facts[id]
	if pf == nil && make {
		pf = &paneFacts{}
		s.facts[id] = pf
	}
	return pf
}

// claimRec is the pane that named one transcript, and when.
type claimRec struct {
	Pane string  `json:"pane"`
	At   float64 `json:"at"`
}

// claimsFile in the data dir keeps every transcript a pane named, so a
// claim outlives the pane that made it and the server.
const claimsFile = "transcripts.json"

// maxClaimsPerPane is how many transcripts one pane may name. Past it the
// pane's reports name no more, so no pane can grow the claims without end.
const maxClaimsPerPane = maxFilesPerPane + retiredKeep

// The refusals of a transcript a pane's report names.
const (
	transcriptNotOwnRefusal = "only a process inside the pane names its transcript."
	transcriptTakenRefusal  = "another pane named that transcript first. A pane reads only its own."
	transcriptOldRefusal    = "that transcript began before the pane started. A pane reads only its own."
	transcriptManyRefusal   = "this pane named too many transcripts. Start a new pane."
	transcriptBareRefusal   = "that transcript's first record has no time or no working directory. A pane reads only its own."
	transcriptCwdRefusal    = "that transcript began in another directory. A pane reads only its own."
	transcriptLongRefusal   = "that transcript's first line is too long to check. A pane reads only its own."
	transcriptMovedRefusal  = "that transcript's path now leads to another file. A pane reads only its own."
)

// firstRecordCap is how many bytes of a transcript the check of its first
// record reads.
const firstRecordCap = 64 << 10

// loadClaimsLocked reads the claims file once. A file that does not parse
// moves aside with a note, and the claims start empty. The caller holds
// factsMu.
func (s *Server) loadClaimsLocked() {
	if s.claimsLoaded {
		return
	}
	s.claimsLoaded = true
	path := filepath.Join(s.cfg.DataDir, claimsFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		// Not readable: claim nothing new until a restart can read it, so
		// no pane takes a path the file holds.
		s.claimsBroken = true
		go s.Note("cannot read "+path+". No transcript is claimed until it can be read.", "")
		return
	}
	var v struct {
		Claims map[string]claimRec `json:"claims"`
	}
	if json.Unmarshal(b, &v) != nil {
		moved := moveAsideFile(path, s.clock())
		go s.Note("A transcript claims file could not be read. It moved to "+moved+". The claims start fresh.", "")
		return
	}
	for p, c := range v.Claims {
		if c.Pane != "" {
			s.claims[p] = c
		}
	}
}

// moveAsideFile renames path to a free <path>.broken-<UTC time> name, mode
// 0600, and returns the new name, or path when the move failed.
func moveAsideFile(path string, now time.Time) string {
	base := path + ".broken-" + now.UTC().Format("20060102T150405Z")
	target := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if os.Rename(path, target) != nil {
		return path
	}
	_ = os.Chmod(target, 0o600)
	return target
}

// saveClaimsLocked writes the claims file, 0600. The caller holds factsMu.
func (s *Server) saveClaimsLocked() {
	b, _ := json.Marshal(map[string]any{"claims": s.claims})
	path := filepath.Join(s.cfg.DataDir, claimsFile)
	tmp := path + ".tmp"
	var err error
	if !s.writeData(func() {
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}) || err == nil {
		return
	}
	go s.Note("cannot write "+path+". Transcript claims are kept until the server stops.", "")
}

// claim gives the real path to pane id, for good. It refuses a path any
// other pane named first, live or ended, before or after a restart: one
// pane never reads what another pane's hook named. refusal is "" when the
// claim holds.
func (s *Server) claim(id, real string) (refusal string) {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	s.loadClaimsLocked()
	if c, held := s.claims[real]; held {
		if c.Pane == id {
			return ""
		}
		return transcriptTakenRefusal
	}
	if s.claimsBroken {
		return transcriptTakenRefusal
	}
	n := 0
	for _, c := range s.claims {
		if c.Pane == id {
			n++
		}
	}
	if n >= maxClaimsPerPane {
		return transcriptManyRefusal
	}
	s.claims[real] = claimRec{Pane: id, At: unixSeconds(s.clock())}
	s.saveClaimsLocked()
	return ""
}

// claimedBy is every transcript pane id claimed, oldest claim first.
func (s *Server) claimedBy(id string) []string {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	s.loadClaimsLocked()
	type pc struct {
		path string
		at   float64
	}
	var out []pc
	for p, c := range s.claims {
		if c.Pane == id {
			out = append(out, pc{p, c.At})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].at != out[j].at {
			return out[i].at < out[j].at
		}
		return out[i].path < out[j].path
	})
	paths := make([]string, len(out))
	for i, x := range out {
		paths[i] = x.path
	}
	return paths
}

// realTranscriptPath is path with every symlink resolved, or, for a file
// not made yet, its directory's real path joined with its name. ok is false
// when the directory cannot be resolved.
func realTranscriptPath(path string) (string, bool) {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r, true
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, filepath.Base(path)), true
}

// under reports whether dir is base or lies under it, as written or with
// base's symlinks resolved.
func under(dir, base string) bool {
	if dir == "" || base == "" {
		return false
	}
	dir = filepath.Clean(dir)
	bases := []string{filepath.Clean(base)}
	if r, err := filepath.EvalSymlinks(base); err == nil {
		bases = append(bases, r)
	}
	for _, b := range bases {
		if dir == b || strings.HasPrefix(dir, strings.TrimSuffix(b, "/")+"/") {
			return true
		}
	}
	return false
}

// cwdOfPane reports whether a transcript that began in dir began in pane
// p: in its working directory or under it. A pane that works in / or in
// the home dir would pass every transcript that way, so for it dir must
// be its working directory itself.
func (s *Server) cwdOfPane(dir string, p layout.Pane) bool {
	base := filepath.Clean(p.Cwd)
	home := s.paneEnv(p, "HOME")
	if base == "/" || (home != "" && base == filepath.Clean(home)) {
		d := filepath.Clean(dir)
		if d == base {
			return true
		}
		r1, e1 := filepath.EvalSymlinks(d)
		r2, e2 := filepath.EvalSymlinks(base)
		return e1 == nil && e2 == nil && r1 == r2
	}
	return under(dir, p.Cwd)
}

// checkFirstRecord checks that transcript path began in pane id: its first
// record (the first user or assistant entry) carries a time and a working
// directory, the time is no more than a second before the pane started,
// and the directory is the pane's own or under it. It answers the
// refusal, or "". A file with no record yet passes: a hook may report a
// transcript before its first line. The check reads the first
// firstRecordCap bytes, and a file with no record in them is refused.
func (s *Server) checkFirstRecord(id, path string) string {
	rec, ok := s.tree.Pane(id)
	if !ok {
		return ""
	}
	f, _, _, _, err := openRegular(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	buf := make([]byte, firstRecordCap)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := buf[:i]
		buf = buf[i+1:]
		var e map[string]any
		if json.Unmarshal(line, &e) != nil || e == nil {
			continue
		}
		if typ := str(e, "type"); typ != "user" && typ != "assistant" {
			continue
		}
		at, cwd := rfc3339Seconds(str(e, "timestamp")), str(e, "cwd")
		if at == 0 || cwd == "" {
			return transcriptBareRefusal
		}
		if lp, live := s.Live(id); live && !lp.started.IsZero() && at < unixSeconds(lp.started)-1 {
			return transcriptOldRefusal
		}
		if !s.cwdOfPane(cwd, rec) {
			return transcriptCwdRefusal
		}
		return ""
	}
	if fi.Size() >= firstRecordCap {
		return transcriptLongRefusal
	}
	return ""
}

// verifyReal answers an error when path, resolved again now, is no longer
// the real path it was claimed as: a directory on the way was swapped for
// a symlink since.
func verifyReal(path string) error {
	r, ok := realTranscriptPath(path)
	if ok && r != filepath.Clean(path) {
		return errors.New(transcriptMovedRefusal)
	}
	return nil
}

// moveClaims gives pane to every claim from, for a pane.resume that runs
// the same session on as a new pane.
func (s *Server) moveClaims(from, to string) {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	s.loadClaimsLocked()
	moved := false
	for p, c := range s.claims {
		if c.Pane == from {
			c.Pane = to
			s.claims[p] = c
			moved = true
		}
	}
	if moved {
		s.saveClaimsLocked()
	}
}

// claimOwner is the pane that claimed real, or "".
func (s *Server) claimOwner(real string) string {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	s.loadClaimsLocked()
	return s.claims[real].Pane
}

// kernelPane is the pane the kernel placed this connection in, or "". A
// hello names no pane here: only a process inside the pane counts.
func (c *Client) kernelPane() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.facts.checked || !c.facts.pane || c.facts.plugin != "" || c.facts.unknown {
		return ""
	}
	return c.facts.paneID
}

// checkTranscript checks the transcript path a report names for pane id.
// real is the path to read, or "" when the report names none the server
// reads. refusal is set when the report must be refused: it came from
// outside the pane, another pane named the path first, the file began
// before the pane, or the pane named too many.
func (s *Server) checkTranscript(c *Client, id, harness, path string) (real, refusal string) {
	if path == "" {
		return "", ""
	}
	if c.kernelPane() != id {
		return "", transcriptNotOwnRefusal
	}
	rec, ok := s.tree.Pane(id)
	if !ok || canonHarness(harness) != "claude" || !sameHarness(harness, rec.Harness) ||
		!s.claudeTranscriptPath(rec, path) {
		return "", ""
	}
	real, ok = realTranscriptPath(path)
	if !ok || !s.claudeTranscriptPath(rec, real) {
		return "", ""
	}
	// A file this pane claimed before, or that a pane.resume handed to
	// it, stays its own.
	switch owner := s.claimOwner(real); {
	case owner == id:
		return real, ""
	case owner != "":
		return "", transcriptTakenRefusal
	}
	if refusal := s.checkFirstRecord(id, real); refusal != "" {
		return "", refusal
	}
	if refusal := s.claim(id, real); refusal != "" {
		return "", refusal
	}
	return real, ""
}

// canonHarness names one harness one way: the gate calls Claude Code
// claude-code, and coppice calls it claude.
func canonHarness(h string) string {
	if h == "claude-code" {
		return "claude"
	}
	return h
}

// sameHarness reports whether a report's harness is the pane's own.
func sameHarness(reported, pane string) bool {
	return pane != "" && canonHarness(reported) == canonHarness(pane)
}

// claudeTranscriptPath reports whether path can be a Claude Code
// transcript of pane p: a .jsonl file under the projects directory of the
// Claude config directory p sees, CLAUDE_CONFIG_DIR or ~/.claude. A path
// anywhere else is never read, even from the pane's own hook.
func (s *Server) claudeTranscriptPath(p layout.Pane, path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	dir := s.paneEnv(p, "CLAUDE_CONFIG_DIR")
	if dir == "" {
		home := s.paneEnv(p, "HOME")
		if home == "" {
			return false
		}
		dir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(dir) {
		return false
	}
	roots := []string{filepath.Clean(dir)}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != roots[0] {
		roots = append(roots, r)
	}
	for _, r := range roots {
		if strings.HasPrefix(path, filepath.Join(r, "projects")+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// noteReport keeps what one pane.report_state said beyond the state
// itself: the mode, the verdict, and the transcript to read, real, which
// checkTranscript claimed for the pane, or "".
func (s *Server) noteReport(id string, x proto.ReportExtras, real string) {
	now := s.clock()
	pf := s.factsFor(id, true)
	pf.mu.Lock()
	if x.Mode != "" {
		pf.mode = x.Mode
	}
	if v := x.Verdict; v != nil {
		pf.verdict = &gateFact{Decision: v.Decision, Tool: v.Tool, Clause: v.Clause, At: unixSeconds(now)}
	}
	if real != "" {
		// A file let go of stays claimed: the claim is for good.
		_ = pf.use(real, kindClaude, s.openTranscript)
	}
	pf.seen = now
	pf.mu.Unlock()
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// factsView is a copy of one pane's facts for a row.
type factsView struct {
	model   string
	tokens  *Tokens
	mode    string
	verdict *gateFact
	seen    time.Time
}

// runPoll reads t once, off the request path, then publishes what it
// found. A read that took longer than pollDeadline marks the path refused:
// a file that can hang a read is not read again.
func (s *Server) runPoll(pf *paneFacts, t *tailer, done chan<- struct{}) {
	defer close(done)
	start := time.Now()
	t.mu.Lock()
	_ = t.poll()
	if time.Since(start) > s.pollDeadline {
		t.refused, t.ok = true, false
	}
	sn := t.snapshot()
	t.mu.Unlock()
	pf.mu.Lock()
	defer pf.mu.Unlock()
	pf.polling = false
	if pf.files[t.path] != t {
		return
	}
	if sn.sum != t.snap.sum {
		pf.seen = s.clock()
	}
	t.snap = sn
	if sn.model != "" {
		pf.model = sn.model
	}
}

// viewFacts copies a pane's facts, first starting a read of its current
// transcript when the last one began at least factsEvery ago and none is
// in flight. The read runs on its own goroutine, one at most per pane. A
// row waits pollWait for a read it started and then shows what is known,
// so a file that hangs a read never holds up a reply. Reads happen only
// when a client asks: no reader, no work.
//
// A sprig pane's file is its own session tree, found from its record, not
// from any report.
func (s *Server) viewFacts(p layout.Pane) factsView {
	sprigPath := ""
	if p.Harness == "sprig" && p.Kind == layout.KindHeadless {
		sprigPath, _ = sprig.TreePath(p.Argv, p.HarnessSessionID)
	}
	pf := s.factsFor(p.ID, sprigPath != "")
	if pf == nil {
		return factsView{}
	}
	now := s.clock()
	pf.mu.Lock()
	if sprigPath != "" && pf.path != sprigPath {
		_ = pf.use(sprigPath, kindSprig, s.openTranscript)
	}
	var done chan struct{}
	if t := pf.files[pf.path]; t != nil && !p.Closed && !pf.polling &&
		!t.snap.refused && now.Sub(pf.lastPoll) >= s.factsEvery {
		pf.polling, pf.lastPoll = true, now
		done = make(chan struct{})
		go s.runPoll(pf, t, done)
	}
	pf.mu.Unlock()
	if done != nil {
		timer := time.NewTimer(s.pollWait)
		select {
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
	}
	pf.mu.Lock()
	defer pf.mu.Unlock()
	v := factsView{model: pf.model, mode: pf.mode, verdict: pf.verdict, seen: pf.seen}
	if sum, ok := pf.tokens(); ok {
		v.tokens = &sum
	}
	if t := pf.files[pf.path]; t != nil && t.kind == kindSprig {
		if v.mode == "" {
			v.mode = t.snap.mode
		}
		if tv := t.snap.verdict; tv != nil {
			c := *tv
			// A tree's clock is the harness's own. A verdict dated after
			// now counts as now, before it is compared, so it cannot hide
			// a newer verdict from the gate.
			if nowS := unixSeconds(now); c.At > nowS {
				c.At = nowS
			}
			if v.verdict == nil || c.At >= v.verdict.At {
				v.verdict = &c
			}
		}
	}
	if v.verdict != nil {
		c := *v.verdict
		v.verdict = &c
	}
	return v
}

// pruneFacts drops the facts of every pane no longer in the tree.
func (s *Server) pruneFacts() {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	for id := range s.facts {
		if _, ok := s.tree.Pane(id); !ok {
			delete(s.facts, id)
		}
	}
}
