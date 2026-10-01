package foreman

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Message kinds. A thought is not a kind: model thoughts are never logged.
const (
	KindUser    = "user"    // the owner's words
	KindForeman = "foreman" // the foreman's reply
	KindWork    = "work"    // a worker's final report
	KindPast    = "past"    // an imported message from an earlier chat
	KindNote    = "note"    // an imported note, such as a memory file
)

var kinds = map[string]bool{KindUser: true, KindForeman: true, KindWork: true, KindPast: true, KindNote: true}

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

// Summarizer makes tree lines. M2 brings a model; tests use a fake. Each
// result should fit in limit bytes; a longer one is clipped.
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
	// Summarizer makes tree lines. A writer needs one.
	Summarizer Summarizer
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
}

// Chat is one open foreman chat. It is not safe for concurrent use: the
// caller runs one call at a time.
type Chat struct {
	cfg      Config
	readOnly bool
	lock     *os.File

	msgs  []msgRef
	nodes map[NodeID]Node

	// pending are the nodes still to build, in build order.
	pending []NodeID

	view viewState

	// broken is set when a failed append could not be cut back off the
	// log. The chat then refuses appends; the next Open repairs or moves
	// the history.
	broken error

	// notice is set when Open moved a broken history aside.
	notice string
}

func (c *Chat) mainDir() string  { return filepath.Join(c.cfg.Dir, "main") }
func (c *Chat) treeDir() string  { return filepath.Join(c.cfg.Dir, "tree") }
func (c *Chat) viewPath() string { return filepath.Join(c.cfg.Dir, "view.json") }

// Open opens the chat in cfg.Dir for writing. It takes the chat's lock,
// drops a line a crash left half written, builds the nodes that are
// missing, and brings the saved view up to the end of the log.
//
// coppice must always start. If the history cannot be read (a bad line, a
// missing or doubled id, a view outside the log, an unreadable file), Open
// moves the whole dir aside to <dir>.broken-<UTC time>, never deletes it,
// starts a new empty chat, and says so in Notice.
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
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(cfg.Dir, target); err != nil {
		return "", fmt.Errorf("the history could not be moved aside: %w", err)
	}
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
	if cfg.Summarizer == nil {
		return nil, errors.New("foreman: a writer needs a summarizer")
	}
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
	c.queueMissing()
	berr := c.compact(ctx)
	start := c.view.Count
	for t := start; t < int64(len(c.msgs)); t++ {
		c.viewStep(t)
	}
	if start < int64(len(c.msgs)) {
		if err := c.saveView(); err != nil {
			lock.Close()
			return nil, err
		}
	}
	if berr != nil {
		return c, fmt.Errorf("foreman: some nodes are not built yet: %w", berr)
	}
	return c, nil
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
	return c, nil
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
		if vf, err = c.readViewFile(); err != nil {
			return err
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
			return err
		}
	}
	return c.applyView(vf)
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
	}
	var all []found
	for _, f := range files {
		path := filepath.Join(c.mainDir(), f)
		err := c.readLines(path, func(off int64, line []byte) error {
			var m Message
			if err := json.Unmarshal(line, &m); err != nil {
				return err
			}
			if limit < 0 || m.I < limit {
				all = append(all, found{m.I, msgRef{file: path, off: off, n: len(line), date: m.Date}})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Slice(all, func(a, b int) bool { return all[a].i < all[b].i })
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
	files, err := dayFiles(c.treeDir())
	if err != nil {
		return err
	}
	for _, f := range files {
		err := c.readLines(filepath.Join(c.treeDir(), f), func(_ int64, line []byte) error {
			var n Node
			if err := json.Unmarshal(line, &n); err != nil {
				return err
			}
			if n.L < 0 || n.L > maxLevel || n.I < 0 || n.I > (int64(1)<<(maxLevel-n.L)) {
				return fmt.Errorf("node %d/%d is not a node", n.L, n.I)
			}
			id := n.ID()
			if id.Last() >= int64(len(c.msgs)) {
				if c.readOnly {
					return nil
				}
				return fmt.Errorf("node %s is outside the log", id.Name())
			}
			c.nodes[id] = n
			return nil
		})
		if err != nil {
			return err
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

// compact builds the pending nodes in order. It stops at the first error
// and keeps the rest for the next call.
func (c *Chat) compact(ctx context.Context) error {
	for len(c.pending) > 0 {
		id := c.pending[0]
		if _, ok := c.nodes[id]; !ok {
			if err := c.build(ctx, id); err != nil {
				return err
			}
		}
		c.pending = c.pending[1:]
	}
	return nil
}

// build makes one node and appends it to today's tree file.
func (c *Chat) build(ctx context.Context, id NodeID) error {
	limit := c.cfg.NodeBytes
	var text string
	if id.L == 0 {
		m, err := c.Message(id.I)
		if err != nil {
			return err
		}
		text = m.Text
		if len(text) > limit {
			if text, err = c.cfg.Summarizer.Compress(ctx, m, limit); err != nil {
				return err
			}
		}
	} else {
		lid, rid := id.Children()
		l, lok := c.nodes[lid]
		r, rok := c.nodes[rid]
		if !lok || !rok {
			return fmt.Errorf("foreman: node %s has an unbuilt half", id.Name())
		}
		if len(l.Text)+1+len(r.Text) <= limit {
			text = l.Text + "\n" + r.Text
		} else {
			var err error
			if text, err = c.cfg.Summarizer.Merge(ctx, id, l.Text, r.Text, limit); err != nil {
				return err
			}
		}
	}
	text = clip(strings.ToValidUTF8(text, "�"), limit)
	n := Node{L: id.L, I: id.I, Text: text, Size: len(text)}
	line, err := encodeLine(n)
	if err != nil {
		return err
	}
	day := c.cfg.Now().UTC().Format("2006-01-02")
	if _, err := appendLine(filepath.Join(c.treeDir(), day+".jsonl"), line); err != nil {
		return err
	}
	c.nodes[id] = n
	return nil
}

// Append logs text as one message of kind, or several in a row when it is
// longer than SplitBytes, dated now. See AppendAt.
func (c *Chat) Append(ctx context.Context, kind, text string) ([]int64, error) {
	return c.AppendAt(ctx, kind, text, time.Time{})
}

// AppendAt logs text as one or more messages of kind with the given date
// (now if zero). The text is logged word for word, split when it is long;
// the log is the owner's private data, protected by file modes. Each line
// is on disk, fsynced, before the next step. Then the new nodes are built
// and the view takes the new lines. A summarizer error leaves the messages
// logged and their nodes queued, and is returned.
//
// A failed write is cut back off the log and its id is not used. When a
// split text fails part way, the parts already logged stay, take their
// place in the view, and their ids are returned with the error.
func (c *Chat) AppendAt(ctx context.Context, kind, text string, date time.Time) ([]int64, error) {
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
	for _, part := range split(text, c.cfg.SplitBytes) {
		i := int64(len(c.msgs))
		m := Message{I: i, Kind: kind, Text: part, Size: len(part), Date: stamp}
		line, err := encodeLine(m)
		if err == nil {
			var off int64
			if off, err = appendLine(path, line); err == nil {
				c.msgs = append(c.msgs, msgRef{file: path, off: off, n: len(line) - 1, date: stamp})
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
	berr := c.compact(ctx)
	for _, i := range ids {
		c.viewStep(i)
	}
	if err := c.saveView(); err != nil {
		return ids, err
	}
	if werr != nil {
		return ids, werr
	}
	if berr != nil {
		return ids, fmt.Errorf("foreman: logged, but some nodes are not built yet: %w", berr)
	}
	return ids, nil
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

// Zoom opens a node, the zoom(id, n) tool. For n = 1 it is the message
// itself. For a larger node it is the node's two halves as view lines.
func (c *Chat) Zoom(id, n int64) (string, error) {
	node, err := NodeOf(id, n)
	if err != nil {
		return "", err
	}
	if node.Last() >= int64(len(c.msgs)) {
		return "", fmt.Errorf("foreman: node %s: %w; the chat has %d messages", node.Name(), errNotNode, len(c.msgs))
	}
	if node.L == 0 {
		m, err := c.Message(id)
		if err != nil {
			return "", err
		}
		return m.Text, nil
	}
	l, r := node.Children()
	return c.line(l) + c.line(r), nil
}
