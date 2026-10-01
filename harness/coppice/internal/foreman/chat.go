package foreman

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/opendaisugi/coppice/internal/broken"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Message kinds. A thought is not a kind: model thoughts are never logged.
const (
	KindUser    = "user"    // the owner's words
	KindForeman = "foreman" // the foreman's reply
	KindWork    = "work"    // a worker's final report
	KindPast    = "past"    // an imported message from an earlier chat
	KindNote    = "note"    // an imported note, such as a memory file
	KindTool    = "tool"    // a tool call and its result, word for word
)

var kinds = map[string]bool{KindUser: true, KindForeman: true, KindWork: true, KindPast: true, KindNote: true, KindTool: true}

// Defaults for Config.
const (
	DefaultNodeBytes  = 512
	DefaultViewMax    = 128 * 1024
	DefaultViewMin    = 64 * 1024
	DefaultSplitBytes = 32 * 1024
)

// DateLayout is how a message date is written: UTC, to the second.
const DateLayout = "2006-01-02T15:04:05Z"

// Unbuilt stands in a view line for a node the compactor has not made yet.
const Unbuilt = "(not summarized yet: zoom it)"

// ErrLocked means another process has the chat open for writing.
var ErrLocked = errors.New("another process has this foreman chat open")

// Message is one line of the log, in main/YYYY-MM-DD.jsonl.
type Message struct {
	I    int64  `json:"i"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	Size int    `json:"size"`
	Date string `json:"date"`
	// Act is the client act id of the owner's words (FM-R-65). It is set on
	// the last part of a split text, in the same append, so a repeat of the
	// act is known after a restart.
	Act string `json:"act,omitempty"`
}

// Node is one line of the tree, in tree/YYYY-MM-DD.jsonl. The day is the
// day it was built.
type Node struct {
	L    int    `json:"l"`
	I    int64  `json:"i"`
	Text string `json:"text"`
	Size int    `json:"size"`
}

// ID is the node's name as a NodeID.
func (n Node) ID() NodeID { return NodeID{L: n.L, I: n.I} }

// Summarizer makes tree lines with a model (ModelSummarizer), or a fake
// in tests. Each result should fit in limit bytes; a longer one is
// clipped.
type Summarizer interface {
	// Compress makes one line for a message longer than limit.
	Compress(ctx context.Context, m Message, limit int) (string, error)
	// Merge makes one line for two sibling lines that do not fit in limit
	// when joined.
	Merge(ctx context.Context, id NodeID, left, right string, limit int) (string, error)
}

// Config opens a chat.
type Config struct {
	// Dir is the foreman data dir. It is made 0700 if missing.
	Dir string
	// Now is the clock. It defaults to time.Now.
	Now func() time.Time
	// NodeBytes caps a tree line. It defaults to 512.
	NodeBytes int
	// ViewMax starts a batch merge, which runs until the view is at most
	// ViewMin. They default to 128 KiB and 64 KiB.
	ViewMax, ViewMin int
	// SplitBytes caps one message; a longer text is logged as several
	// messages in a row. It defaults to 32 KiB.
	SplitBytes int

	// writeFile replaces a file atomically. Tests inject a failing one;
	// nil means writeAtomic.
	writeFile func(path string, data []byte) error
}

func (c *Config) fill() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.NodeBytes <= 0 {
		c.NodeBytes = DefaultNodeBytes
	}
	if c.ViewMax <= 0 {
		c.ViewMax = DefaultViewMax
	}
	if c.ViewMin <= 0 {
		c.ViewMin = DefaultViewMin
	}
	if c.SplitBytes <= 0 {
		c.SplitBytes = DefaultSplitBytes
	}
}

// msgRef says where a message's line is, so its text need not stay in
// memory.
type msgRef struct {
	file string
	off  int64
	n    int
	date string
	// past is true for an imported message (kind past).
	past bool
}

// Chat is one open foreman chat. It is not safe for concurrent use: the
// caller runs one call at a time.
type Chat struct {
	cfg      Config
	readOnly bool
	lock     *os.File

	msgs  []msgRef
	nodes map[NodeID]Node
	// acts maps a client act id to the messages that carry it, oldest
	// first: every part of a text that was split.
	acts map[string][]int64

	// pending are the nodes still to write, in write order.
	pending []NodeID
	// wait is the retry state of the front node after a failed call, or nil.
	wait *retryState
	// held is a line the model made whose write to the tree file failed.
	// The next Next writes it again; the model is not asked twice.
	held *heldLine

	view viewState

	// broken is set when a failed append could not be cut back off the
	// log. The chat then refuses appends; the next Open repairs or moves
	// the history.
	broken error

	// notice is set when Open moved a broken history aside or repaired it.
	notice string

	// badTree are the tree lines a load could not use (U3), by file. A
	// writer repairs them; a reader skips them.
	badTree []badTreeFile
	// badView is why view.json could not be used, or "".
	badView string
	// tornParts are the notes of torn tree tails a load copied and cut.
	tornParts []string
	// skip are the nodes a replay of the view steps past (I-1): the nodes
	// a repair cut from the tree, which the compactor builds again later.
	skip map[NodeID]bool
	// onTorn, when set, sees the torn last line of a file before a writer
	// cuts it off.
	onTorn func(path string, tail []byte) error
}

// badTreeFile are the bad lines of one tree file, by line index (0 is the
// first line).
type badTreeFile struct {
	path string
	idx  map[int]bool
}

// parseNode reads one tree line. A line is a node only when it is a JSON
// object that is valid UTF-8 and holds the keys l, i, text and size in
// exactly that case, as the Rust port reads it. null, a wrong key case and
// invalid UTF-8 are bad lines.
func parseNode(line []byte) (Node, bool) {
	var n Node
	t := bytes.TrimLeft(line, " \t\r\n")
	if len(t) == 0 || t[0] != '{' || !utf8.Valid(line) {
		return n, false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(line, &keys); err != nil {
		return n, false
	}
	want := []string{"l", "i", "text", "size"}
	for _, k := range want {
		raw, ok := keys[k]
		if !ok || string(bytes.TrimSpace(raw)) == "null" {
			return n, false
		}
	}
	// Go matches a key without case; serde does not. A key that is one of
	// the four only in another case would change a value here.
	for k := range keys {
		for _, w := range want {
			if k != w && strings.EqualFold(k, w) {
				return n, false
			}
		}
	}
	if loneSurrogate(line) {
		return n, false
	}
	if err := json.Unmarshal(line, &n); err != nil {
		return n, false
	}
	return n, true
}

// loneSurrogate is true when valid JSON holds a \u escape of half a
// surrogate pair. Go reads it as U+FFFD; serde_json refuses the line.
func loneSurrogate(b []byte) bool {
	hex := func(k int) (int, bool) {
		if k+6 > len(b) || b[k] != '\\' || b[k+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(b[k+2:k+6]), 16, 32)
		return int(v), err == nil
	}
	for k := 0; k < len(b); k++ {
		if b[k] != '\\' {
			continue
		}
		if k+1 < len(b) && b[k+1] == 'u' {
			v, ok := hex(k)
			switch {
			case ok && v >= 0xD800 && v <= 0xDBFF:
				if lo, ok := hex(k + 6); !ok || lo < 0xDC00 || lo > 0xDFFF {
					return true
				}
				k += 11
			case ok && v >= 0xDC00 && v <= 0xDFFF:
				return true
			default:
				k += 5
			}
			continue
		}
		k++
	}
	return false
}

// copyTorn copies the torn last line of a tree file to a .broken file before
// a writer cuts it off, and keeps the words for the repair note.
func (c *Chat) copyTorn(path string, tail []byte) error {
	if filepath.Dir(path) != c.treeDir() {
		return nil
	}
	saved := freeName(path + ".broken-" + c.stampNow())
	if err := writeNew(saved, tail); err != nil {
		return err
	}
	broken.Prune(saved)
	rel, err := filepath.Rel(c.cfg.Dir, path)
	if err != nil {
		rel = path
	}
	c.tornParts = append(c.tornParts, fmt.Sprintf("1 torn last line in %s was copied to %s and removed.", filepath.ToSlash(rel), saved))
	return nil
}

// atomic is writeAtomic, or the test's replacement for it.
func (c *Chat) atomic(path string, data []byte) error {
	if c.cfg.writeFile != nil {
		return c.cfg.writeFile(path, data)
	}
	return writeAtomic(path, data)
}

func (c *Chat) mainDir() string  { return filepath.Join(c.cfg.Dir, "main") }
func (c *Chat) treeDir() string  { return filepath.Join(c.cfg.Dir, "tree") }
func (c *Chat) viewPath() string { return filepath.Join(c.cfg.Dir, "view.json") }

// Open opens the chat in cfg.Dir for writing. It takes the chat's lock,
// drops a line a crash left half written, builds the nodes that need no
// model, and brings the saved view up to the last message whose nodes are
// all written. Nodes that need the model wait for Drain.
//
// coppice must always start. If the history cannot be read (a bad line, a
// missing or doubled id, a view outside the log, an unreadable file), Open
// moves the whole dir aside to <dir>.broken-<UTC time> (the newest
// broken_keep copies stay, default 5; older ones are removed), starts a new empty chat, and says so in Notice.
func Open(ctx context.Context, cfg Config) (*Chat, error) {
	cfg.fill()
	c, err := open(ctx, cfg)
	var herr *historyError
	if !errors.As(err, &herr) {
		return c, err
	}
	broken, merr := moveAside(cfg)
	if merr != nil {
		return nil, fmt.Errorf("foreman: %v; %w", herr.err, merr)
	}
	c, err = open(ctx, cfg)
	if errors.As(err, &herr) {
		return nil, fmt.Errorf("foreman: a new chat in %s could not be read: %w", cfg.Dir, herr.err)
	}
	if c != nil {
		c.notice = "Foreman history could not be read (" + herr.reason + "); it was saved to " +
			broken + " and a new chat started."
		// A tool call the old history left without an end is still unknown.
		if cerr := CarryDanglingCalls(broken, cfg.Dir); cerr != nil {
			c.notice += " Its call log could not be read (" + cerr.Error() + "): check the workers."
		}
	}
	return c, err
}

// historyError is a parse or consistency fault in the history: a bad
// JSON line, a missing or doubled id, a node outside the log, a view that
// does not match. Only these move the history aside. An I/O error (a
// failed read, EACCES, EIO, EMFILE) is returned as it is, and the dir
// stays where it is.
type historyError struct {
	reason string // short, for the owner's notice
	err    error
}

func corrupt(reason string, err error) error { return &historyError{reason: reason, err: err} }

func (e *historyError) Error() string { return e.err.Error() }
func (e *historyError) Unwrap() error { return e.err }

// moveAside renames cfg.Dir to a free <dir>.broken-<UTC time> name in the
// same parent, mode 0700, and returns the new path.
func moveAside(cfg Config) (string, error) {
	base := cfg.Dir + ".broken-" + cfg.Now().UTC().Format("20060102T150405Z")
	target := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(target); err != nil {
			break
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(cfg.Dir, target); err != nil {
		return "", fmt.Errorf("the history could not be moved aside: %w", err)
	}
	broken.Prune(target)
	if err := chmod(target, dirMode); err != nil {
		return "", fmt.Errorf("the history was moved to %s, but %w", target, err)
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return "", fmt.Errorf("the history was moved to %s, but %w", target, err)
	}
	return target, nil
}

// open is Open without the move aside. A parse or consistency fault in
// the history comes back as a *historyError.
func open(ctx context.Context, cfg Config) (*Chat, error) {
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, "main"), filepath.Join(cfg.Dir, "tree")} {
		if err := ensureDir(d); err != nil {
			return nil, err
		}
	}
	if err := tightenFiles(cfg.Dir); err != nil {
		return nil, err
	}
	lock, err := lockDir(filepath.Join(cfg.Dir, "lock"))
	if err != nil {
		return nil, err
	}
	c := &Chat{cfg: cfg, lock: lock}
	if err := c.load(); err != nil {
		lock.Close()
		return nil, err
	}
	var notes []string
	if len(c.badTree) > 0 || len(c.tornParts) > 0 {
		n, err := c.repairTree()
		if err != nil {
			lock.Close()
			return nil, err
		}
		notes = append(notes, n)
	}
	viewSaved := ""
	if c.badView != "" {
		saved, err := c.copyViewAside()
		if err != nil {
			lock.Close()
			return nil, err
		}
		viewSaved = saved
	}
	if c.badView != "" || c.viewMissing() {
		c.skip = c.cutNodes()
	}
	c.queueMissing()
	err = c.settle()
	c.skip = nil
	if err != nil {
		lock.Close()
		// Only a failed save of the rebuilt view.json moves the history.
		// Any other error is not the view's and is returned as it is.
		var vs *viewSaveError
		if c.badView != "" && errors.As(err, &vs) {
			return nil, corrupt("view.json could not be rebuilt", err)
		}
		return nil, err
	}
	if viewSaved != "" {
		note := "Foreman view.json could not be used. It was copied to " + viewSaved
		if c.view.Count > 0 || len(c.msgs) == 0 {
			note += " and the view was rebuilt from the tree."
		} else {
			note += ". The view is empty and will be rebuilt as the tree is built."
		}
		notes = append(notes, note)
	}
	c.notice = strings.Join(notes, " ")
	return c, nil
}

// stampNow is the UTC time in a .broken-<time> name.
func (c *Chat) stampNow() string { return c.cfg.Now().UTC().Format("20060102T150405Z") }

// freeName is base, or base-2, base-3 ... the first name nothing has.
func freeName(base string) string {
	target := base
	for n := 2; ; n++ {
		// Any lookup error takes the name: the create that follows fails
		// with the real error. Looping on it would never end.
		if _, err := os.Lstat(target); err != nil {
			return target
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
}

// repairTree copies the bad tree lines verbatim to <file>.broken-<time>,
// rewrites each tree file without them (temp file, fsync, rename), and
// returns the one note. The nodes the lines held are not in c.nodes, so
// queueMissing builds them again from the log. The newest broken_keep copies
// stay (default 5); older ones are removed.
func (c *Chat) repairTree() (string, error) {
	parts := c.tornParts
	c.tornParts = nil
	for _, bf := range c.badTree {
		data, err := readFile(bf.path)
		if err != nil {
			return "", err
		}
		// Split on the raw newlines: the copy holds each bad line exactly
		// as it was, and the rest of the file keeps its bytes.
		var keep, bad []byte
		for k, line := range bytes.SplitAfter(data, []byte("\n")) {
			if bf.idx[k] {
				bad = append(bad, line...)
			} else {
				keep = append(keep, line...)
			}
		}
		saved := freeName(bf.path + ".broken-" + c.stampNow())
		if err := writeNew(saved, bad); err != nil {
			return "", err
		}
		broken.Prune(saved)
		if err := c.atomic(bf.path, keep); err != nil {
			return "", err
		}
		rel, rerr := filepath.Rel(c.cfg.Dir, bf.path)
		if rerr != nil {
			rel = bf.path
		}
		n := len(bf.idx)
		if n == 1 {
			parts = append(parts, fmt.Sprintf("1 bad line in %s was copied to %s and removed.", filepath.ToSlash(rel), saved))
		} else {
			parts = append(parts, fmt.Sprintf("%d bad lines in %s were copied to %s and removed.", n, filepath.ToSlash(rel), saved))
		}
	}
	c.badTree = nil
	return "Foreman tree repaired: " + strings.Join(parts, " ") + " The nodes those lines held will be built again from the log.", nil
}

// copyViewAside copies view.json alone to view.json.broken-<time> and leaves
// it where it is. The view starts empty and settle rebuilds it by replay over
// the tree; the save of the rebuilt view replaces the bad file. An open that
// fails first leaves the bad file in place, so the next open repairs it
// again and says so (N-2).
func (c *Chat) copyViewAside() (string, error) {
	data, err := readFile(c.viewPath())
	if err != nil {
		return "", err
	}
	saved := freeName(c.viewPath() + ".broken-" + c.stampNow())
	if err := writeNew(saved, data); err != nil {
		return "", err
	}
	broken.Prune(saved)
	c.view = viewState{}
	return saved, nil
}

// viewMissing is true when there is no view.json and the log is not empty.
// The replay then steps past the cut nodes as for a bad view (N-2).
func (c *Chat) viewMissing() bool {
	if len(c.msgs) == 0 {
		return false
	}
	_, err := os.Lstat(c.viewPath())
	return errors.Is(err, os.ErrNotExist)
}

// writeNew writes data to a new file, 0600, and fsyncs it and its dir. The
// file must not exist.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if !noSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// OpenReadOnly reads the chat in dir without the lock and writes nothing.
// A line a writer has half written is skipped. A history it cannot read
// is reported with its path and never moved: only a writer moves it.
func OpenReadOnly(dir string) (*Chat, error) {
	cfg := Config{Dir: dir}
	cfg.fill()
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	c := &Chat{cfg: cfg, readOnly: true}
	if err := c.load(); err != nil {
		return nil, fmt.Errorf("foreman: the history in %s could not be read: %w", dir, err)
	}
	var notes []string
	if n := c.skipNote(); n != "" {
		notes = append(notes, n)
	}
	if c.badView != "" || c.viewMissing() {
		c.skip = c.cutNodes()
		c.replayView()
		c.skip = nil
	}
	if c.badView != "" {
		if c.view.Count > 0 || len(c.msgs) == 0 {
			notes = append(notes, "view.json could not be used ("+c.badView+"). The view was rebuilt in memory. Nothing was changed.")
		} else {
			notes = append(notes, "view.json could not be used ("+c.badView+"). The view is empty until the tree is built. Nothing was changed.")
		}
	}
	c.notice = strings.Join(notes, " ")
	return c, nil
}

// skipNote is what a reader says about the tree lines it skipped, or "".
func (c *Chat) skipNote() string {
	var parts []string
	for _, bf := range c.badTree {
		rel, err := filepath.Rel(c.cfg.Dir, bf.path)
		if err != nil {
			rel = bf.path
		}
		if len(bf.idx) == 1 {
			parts = append(parts, fmt.Sprintf("1 bad line in %s was skipped.", filepath.ToSlash(rel)))
		} else {
			parts = append(parts, fmt.Sprintf("%d bad lines in %s were skipped.", len(bf.idx), filepath.ToSlash(rel)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " Their nodes show as not summarized yet. Nothing was changed."
}

// Close releases the lock.
func (c *Chat) Close() error {
	if c.lock == nil {
		return nil
	}
	err := c.lock.Close()
	c.lock = nil
	return err
}

// Notice is a one-line note for the owner from Open, or "".
func (c *Chat) Notice() string { return c.notice }

// Count is the number of messages in the chat.
func (c *Chat) Count() int64 { return int64(len(c.msgs)) }

// Pending is the number of nodes not built yet.
func (c *Chat) Pending() int { return len(c.pending) }

// dayFiles lists dir's YYYY-MM-DD.jsonl files in name order.
func dayFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".jsonl") {
			if _, err := time.Parse("2006-01-02", strings.TrimSuffix(n, ".jsonl")); err == nil {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// readLines calls fn for each whole line of path with its offset. A last
// line with no newline was cut by a crash: a writer truncates it away, a
// reader skips it.
func (c *Chat) readLines(path string, fn func(off int64, line []byte) error) error {
	data, err := readFile(path)
	if err != nil {
		return err
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	if end < len(data) && !c.readOnly {
		if c.onTorn != nil {
			if err := c.onTorn(path, data[end:]); err != nil {
				return err
			}
		}
		if err := os.Truncate(path, int64(end)); err != nil {
			return err
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(data[:end]))
	sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	var off int64
	for sc.Scan() {
		line := sc.Bytes()
		if err := fn(off, line); err != nil {
			rel, rerr := filepath.Rel(c.cfg.Dir, path)
			if rerr != nil {
				rel = path
			}
			return corrupt("a bad line in "+filepath.ToSlash(rel), fmt.Errorf("%s at byte %d: %w", path, off, err))
		}
		off += int64(len(line)) + 1
	}
	return sc.Err()
}

// loadHook, when set, runs between the reads of a load. Tests use it to
// write while a reader reads.
var loadHook func(stage string)

func hook(stage string) {
	if loadHook != nil {
		loadHook(stage)
	}
}

// load reads the log, the tree and the view.
//
// A reader takes no lock, so a writer may append while it reads. It reads
// view.json first and then only the messages it covers and the nodes
// inside them. The writer logs a message and builds its nodes before it
// saves the view, so that prefix is always whole: the reader sees one
// consistent snapshot, however far the writer has gone since.
func (c *Chat) load() error {
	var vf *viewFile
	var err error
	if c.readOnly {
		vf, err = c.readViewFile()
		if err != nil {
			if vf, err = c.viewFault(err); err != nil {
				return err
			}
		}
		hook("view")
	}
	limit := int64(-1)
	if vf != nil {
		limit = vf.Count
	}
	if err := c.loadMessages(limit); err != nil {
		return err
	}
	hook("main")
	if err := c.loadTree(); err != nil {
		return err
	}
	if !c.readOnly {
		if vf, err = c.readViewFile(); err != nil {
			if vf, err = c.viewFault(err); err != nil {
				return err
			}
		}
	}
	if err := c.applyView(vf); err != nil {
		if _, err = c.viewFault(err); err != nil {
			return err
		}
		c.view = viewState{}
		if limit >= 0 {
			// The log was capped by a view that is now rejected: read it all.
			if err := c.loadMessages(-1); err != nil {
				return err
			}
			if err := c.loadTree(); err != nil {
				return err
			}
		}
	}
	return nil
}

// cutNodes are the nodes the tree lacks that it should hold. Nodes are built
// in queue order and a failed node stops all behind it, so a node that is
// pending in the normal way has no built node after it. A missing node that
// has one was cut from the tree. A replay of the view steps past these
// (I-1); a node that is merely pending stays in the tail word for word.
func (c *Chat) cutNodes() map[NodeID]bool {
	var order []NodeID
	last := -1
	for i := int64(0); i < int64(len(c.msgs)); i++ {
		for _, id := range chain(i) {
			if _, ok := c.nodes[id]; ok {
				last = len(order)
			}
			order = append(order, id)
		}
	}
	cut := map[NodeID]bool{}
	for k := 0; k < last; k++ {
		if _, ok := c.nodes[order[k]]; !ok {
			cut[order[k]] = true
		}
	}
	return cut
}

// viewFault turns a view.json that cannot be used (bad JSON, or not
// matching the log) into c.badView and an empty view, so the opener can
// rebuild it. Any other error is returned as it is.
func (c *Chat) viewFault(err error) (*viewFile, error) {
	var herr *historyError
	if !errors.As(err, &herr) {
		return nil, err
	}
	c.badView = herr.reason
	return nil, nil
}

// loadMessages reads the log. With limit >= 0 it keeps only the messages
// before limit.
func (c *Chat) loadMessages(limit int64) error {
	files, err := dayFiles(c.mainDir())
	if err != nil {
		return err
	}
	type found struct {
		i   int64
		ref msgRef
		act string
	}
	var all []found
	acts := map[string][]int64{}
	for _, f := range files {
		path := filepath.Join(c.mainDir(), f)
		err := c.readLines(path, func(off int64, line []byte) error {
			var m Message
			if err := json.Unmarshal(line, &m); err != nil {
				return err
			}
			if limit < 0 || m.I < limit {
				all = append(all, found{m.I, msgRef{file: path, off: off, n: len(line), date: m.Date, past: m.Kind == KindPast}, m.Act})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Slice(all, func(a, b int) bool { return all[a].i < all[b].i })
	for _, f := range all {
		if f.act != "" {
			acts[f.act] = append(acts[f.act], f.i)
		}
	}
	c.acts = acts
	c.msgs = make([]msgRef, len(all))
	for k, f := range all {
		if f.i < int64(k) {
			return corrupt(fmt.Sprintf("message %d is in the log twice", f.i),
				fmt.Errorf("foreman: the log has message %d twice", f.i))
		}
		if f.i > int64(k) {
			return corrupt(fmt.Sprintf("message %d is missing", k), fmt.Errorf("foreman: the log has no message %d", k))
		}
		c.msgs[k] = f.ref
	}
	return nil
}

// loadTree reads the tree. A node past the end of the log is an error for
// a writer; a reader skips it, as the writer built it after the snapshot.
func (c *Chat) loadTree() error {
	c.nodes = map[NodeID]Node{}
	c.badTree = nil
	c.tornParts = nil
	c.onTorn = c.copyTorn
	defer func() { c.onTorn = nil }()
	files, err := dayFiles(c.treeDir())
	if err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(c.treeDir(), f)
		var bad *badTreeFile
		idx := -1
		note := func() {
			if bad == nil {
				bad = &badTreeFile{path: path, idx: map[int]bool{}}
			}
			bad.idx[idx] = true
		}
		err := c.readLines(path, func(_ int64, line []byte) error {
			idx++
			n, ok := parseNode(line)
			if !ok {
				note()
				return nil
			}
			if n.L < 0 || n.L > maxLevel || n.I < 0 || n.I > (int64(1)<<(maxLevel-n.L)) {
				note()
				return nil
			}
			id := n.ID()
			if id.Last() >= int64(len(c.msgs)) {
				// A reader skips a node past its snapshot: the writer built
				// it later. For a writer it is a bad line.
				if !c.readOnly {
					note()
				}
				return nil
			}
			c.nodes[id] = n
			return nil
		})
		if err != nil {
			return err
		}
		if bad != nil {
			c.badTree = append(c.badTree, *bad)
		}
	}
	return nil
}

// queueMissing lists every node that should exist but does not, in the
// order a live chat would have built it.
func (c *Chat) queueMissing() {
	c.pending = nil
	for i := int64(0); i < int64(len(c.msgs)); i++ {
		c.queueFor(i)
	}
}

// queueFor queues message i's node and every parent it completes: a binary
// carry up the tree.
func (c *Chat) queueFor(i int64) {
	id := NodeID{L: 0, I: i}
	for {
		if _, ok := c.nodes[id]; !ok {
			c.pending = append(c.pending, id)
		}
		if id.I%2 == 0 || id.L >= maxLevel {
			return
		}
		id = id.Parent()
	}
}

// Append logs text as one message of kind, or several in a row when it is
// longer than SplitBytes, dated now. See AppendAt.
func (c *Chat) Append(ctx context.Context, kind, text string) ([]int64, error) {
	return c.AppendAt(ctx, kind, text, time.Time{})
}

// AppendAt logs text as one or more messages of kind with the given date
// (now if zero). The text is logged word for word, split when it is long;
// the log is the owner's private data, protected by file modes. Each line
// is on disk, fsynced, before the next step. Then the new nodes are
// queued, the ones that need no model are written, and the view takes
// every message whose nodes are all written. It never calls a model: the
// nodes that need one wait for Drain, or for Next and Commit.
//
// A failed write is cut back off the log and its id is not used. When a
// split text fails part way, the parts already logged stay, take their
// place in the view when their nodes are written, and their ids are
// returned with the error.
func (c *Chat) AppendAt(ctx context.Context, kind, text string, date time.Time) ([]int64, error) {
	return c.appendAct(ctx, kind, text, date, "", 0)
}

// AppendAct logs the owner's words with a client act id: the id is written
// in the line of every part of the text, so a failure part way keeps it. A
// later call with a known id is the caller's to refuse (ActMsg, ActMatches).
func (c *Chat) AppendAct(ctx context.Context, kind, text, act string) ([]int64, error) {
	return c.appendAct(ctx, kind, text, time.Time{}, act, 0)
}

// ActMsg is the last message that carries the client act id, if the log
// holds one. The set is rebuilt from the log when the chat opens.
func (c *Chat) ActMsg(act string) (int64, bool) {
	ids := c.acts[act]
	if len(ids) == 0 {
		return 0, false
	}
	return ids[len(ids)-1], true
}

// ActMatches reports whether text is what the known act id was used for:
// the parts the log holds for the id are the first parts of text, split as
// an append splits it (a text whose last parts failed to log still
// matches). A caller that sends one id for other words gets false and must
// refuse (FM-R-65).
func (c *Chat) ActMatches(act, text string) bool {
	ids := c.acts[act]
	parts := split(strings.ToValidUTF8(text, "�"), c.cfg.SplitBytes)
	if len(ids) == 0 || len(ids) > len(parts) {
		return false
	}
	for k, i := range ids {
		m, err := c.Message(i)
		if err != nil || m.Text != parts[k] {
			return false
		}
	}
	return true
}

// appendAct logs text in parts; from is how many of the first parts the log
// holds already (a retry of a failed append), which are not logged again.
func (c *Chat) appendAct(ctx context.Context, kind, text string, date time.Time, act string, from int) ([]int64, error) {
	if c.readOnly || c.lock == nil {
		return nil, errors.New("foreman: the chat is not open for writing")
	}
	if c.broken != nil {
		return nil, fmt.Errorf("foreman: the log takes no more appends until it is opened again: %w", c.broken)
	}
	if !kinds[kind] {
		return nil, fmt.Errorf("foreman: %q is not a message kind", kind)
	}
	if date.IsZero() {
		date = c.cfg.Now()
	}
	date = date.UTC()
	stamp := date.Format(DateLayout)
	path := filepath.Join(c.mainDir(), date.Format("2006-01-02")+".jsonl")

	text = strings.ToValidUTF8(text, "�")
	var ids []int64
	var werr error
	parts := split(text, c.cfg.SplitBytes)
	if from > len(parts) {
		from = len(parts)
	}
	for _, part := range parts[from:] {
		i := int64(len(c.msgs))
		m := Message{I: i, Kind: kind, Text: part, Size: len(part), Date: stamp}
		m.Act = act
		line, err := encodeLine(m)
		if err == nil {
			var off int64
			if off, err = appendLine(path, line); err == nil {
				c.msgs = append(c.msgs, msgRef{file: path, off: off, n: len(line) - 1, date: stamp, past: kind == KindPast})
				if m.Act != "" {
					if c.acts == nil {
						c.acts = map[string][]int64{}
					}
					c.acts[m.Act] = append(c.acts[m.Act], i)
				}
				c.queueFor(i)
				ids = append(ids, i)
				continue
			}
		}
		if errors.Is(err, errCutBack) {
			c.broken = err
		}
		werr = err
		break
	}
	if len(ids) == 0 {
		return nil, werr
	}
	// The owner is here: a failed node is asked again at the next Next.
	c.RetryNow()
	if err := c.settle(); err != nil {
		return ids, err
	}
	return ids, werr
}

// Message reads message i from the log.
func (c *Chat) Message(i int64) (Message, error) {
	if i < 0 || i >= int64(len(c.msgs)) {
		return Message{}, fmt.Errorf("foreman: no message %d", i)
	}
	r := c.msgs[i]
	f, err := os.Open(r.file)
	if err != nil {
		return Message{}, err
	}
	defer f.Close()
	buf := make([]byte, r.n)
	if _, err := io.ReadFull(io.NewSectionReader(f, r.off, int64(r.n)), buf); err != nil {
		return Message{}, err
	}
	var m Message
	if err := json.Unmarshal(buf, &m); err != nil {
		return Message{}, err
	}
	if m.I != i {
		return Message{}, fmt.Errorf("foreman: the log line for message %d holds message %d", i, m.I)
	}
	return m, nil
}

// Node returns a built node.
func (c *Chat) Node(id NodeID) (Node, bool) {
	n, ok := c.nodes[id]
	return n, ok
}

// Date is the date message i was logged, the date(id) tool.
func (c *Chat) Date(i int64) (string, error) {
	if i < 0 || i >= int64(len(c.msgs)) {
		return "", fmt.Errorf("foreman: no message %d", i)
	}
	return c.msgs[i].date, nil
}

// Day is every message logged on one UTC day, YYYY-MM-DD, in id order.
func (c *Chat) Day(day string) ([]Message, error) {
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return nil, fmt.Errorf("foreman: %q is not a day like 2026-10-08", day)
	}
	var out []Message
	for i, r := range c.msgs {
		if !strings.HasPrefix(r.date, day+"T") {
			continue
		}
		m, err := c.Message(int64(i))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// ZoomPageBytes is the most a zoom reply holds, and the page it takes when
// no size is given.
const ZoomPageBytes = 32 * 1024

// Roots are the largest whole nodes that cover the chat, oldest first: the
// ids a zoom can start from. The binary form of the message count names
// them.
func (c *Chat) Roots() []NodeID {
	var out []NodeID
	first := int64(0)
	for l := maxLevel; l >= 0; l-- {
		if n := int64(len(c.msgs)); n>>l&1 == 1 {
			out = append(out, NodeID{L: l, I: first >> l})
			first += int64(1) << l
		}
	}
	return out
}

// rootHint is the sentence a failed zoom ends with.
func (c *Chat) rootHint() string {
	roots := c.Roots()
	if len(roots) == 0 {
		return " The chat is empty."
	}
	names := make([]string, len(roots))
	for k, r := range roots {
		names[k] = r.Name()
	}
	return " The root nodes are " + strings.Join(names, ", ") + "."
}

// Zoom opens a node, the zoom(id, n) tool. For n = 1 it is the message
// itself. For a larger node it is the node's two halves as view lines.
func (c *Chat) Zoom(id, n int64) (string, error) { return c.ZoomPage(id, n, 0, 0) }

// ZoomPage is Zoom with a page: size bytes from byte offset of the node's
// text (size 0 means ZoomPageBytes; more is cut to it). A page that is not
// the whole text ends with a line that says which bytes it holds and
// where the next page starts. A cut never splits a UTF-8 rune. A bad id
// answers with the root ids.
func (c *Chat) ZoomPage(id, n, offset int64, size int) (string, error) {
	text, notNode, err := c.zoomText(id, n)
	if err != nil {
		if notNode {
			err = fmt.Errorf("%w.%s", err, c.rootHint())
		}
		return "", err
	}
	if offset < 0 || size < 0 {
		return "", errors.New("foreman: offset and size must not be negative")
	}
	if size == 0 || size > ZoomPageBytes {
		size = ZoomPageBytes
	}
	if offset == 0 && len(text) <= size {
		return text, nil
	}
	if offset > int64(len(text)) {
		offset = int64(len(text))
	}
	start := int(offset)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	if offset != 0 && start >= len(text) {
		return "", fmt.Errorf("foreman: the offset is past the end: the node has %d bytes", len(text))
	}
	end := start + size
	if end >= len(text) {
		end = len(text)
	} else {
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == start { // the size is under one rune: take the rune
			_, w := utf8.DecodeRuneInString(text[start:])
			end = start + w
		}
	}
	page := text[start:end]
	tail := fmt.Sprintf("More: offset %d", end)
	if end >= len(text) {
		tail = "End."
	}
	return fmt.Sprintf("%s\n[bytes %d-%d of %d. %s]", page, start, end, len(text), tail), nil
}

// zoomText is the whole text of a node. notNode is set when err is a bad
// node name, which gets the root hint.
func (c *Chat) zoomText(id, n int64) (text string, notNode bool, err error) {
	node, err := NodeOf(id, n)
	if err != nil {
		return "", true, err
	}
	if node.Last() >= int64(len(c.msgs)) {
		return "", true, fmt.Errorf("foreman: node %s: %w; the chat has %d messages", node.Name(), errNotNode, len(c.msgs))
	}
	if node.L == 0 {
		m, err := c.Message(id)
		if err != nil {
			return "", false, err
		}
		return m.Text, false, nil
	}
	l, r := node.Children()
	return c.line(l) + c.line(r), false, nil
}
