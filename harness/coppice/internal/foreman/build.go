package foreman

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The tree is built apart from the log, one node at a time, in the order
// the nodes were queued. AppendAt logs a message and queues its nodes. The
// front pending node is built next: a line that fits in NodeBytes needs no
// model and is written at once; any other is handed out as a Job, and its
// result comes back through Commit. The view takes message t once every
// node queued for t is written.
//
// The chat is not safe for concurrent use. The caller holds one lock
// around Next, Commit, FailErr and every other call; the model call runs
// outside it. Drain does this with the lock it is given.

// Job is one tree line that needs the model.
type Job struct {
	ID NodeID
	// Msg is the message to compress, for a level-0 node.
	Msg Message
	// Left and Right are the two lines to merge, for a parent.
	Left, Right string
	// Limit is the most bytes the line may have.
	Limit int
}

// A node whose call fails is not asked again at once. Its first failure
// waits 30 s, its second 2 min, and every later one 5 min. An append asks
// again at once, since the owner is there. Nothing behind the failed node
// is built until it is.
var retrySteps = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// retryState is what the chat remembers of the front node after its call
// failed.
type retryState struct {
	id    NodeID
	at    time.Time
	fails int
	err   error
}

// heldLine is a finished line waiting for its write to succeed.
type heldLine struct {
	id   NodeID
	text string
}

// finish is a line as it is written: valid UTF-8, at most limit bytes.
func finish(text string, limit int) string {
	return clip(strings.ToValidUTF8(text, "�"), limit)
}

// prepare looks at the front node. It returns the line for a node that
// needs no model, or the job for one that does.
func (c *Chat) prepare(id NodeID) (job Job, text string, needsModel bool, err error) {
	limit := c.cfg.NodeBytes
	if id.L == 0 {
		m, err := c.Message(id.I)
		if err != nil {
			return Job{}, "", false, err
		}
		if len(m.Text) <= limit {
			return Job{}, finish(m.Text, limit), false, nil
		}
		return Job{ID: id, Msg: m, Limit: limit}, "", true, nil
	}
	lid, rid := id.Children()
	l, lok := c.nodes[lid]
	r, rok := c.nodes[rid]
	if !lok || !rok {
		return Job{}, "", false, fmt.Errorf("node %s has an input that is not built", id.Name())
	}
	if len(l.Text)+1+len(r.Text) <= limit {
		return Job{}, finish(l.Text+"\n"+r.Text, limit), false, nil
	}
	return Job{ID: id, Left: l.Text, Right: r.Text, Limit: limit}, "", true, nil
}

// Next builds every node at the front that needs no model, steps the view,
// and returns the job for the first node that does. ok is false when
// nothing is pending, or when the front node waits for a retry; in that
// case err is the error that put it to wait.
func (c *Chat) Next() (job Job, ok bool, err error) {
	for len(c.pending) > 0 {
		id := c.pending[0]
		if w := c.wait; w != nil && w.id == id && c.cfg.Now().Before(w.at) {
			err = w.err
			break
		}
		if h := c.held; h != nil && h.id == id {
			if werr := c.write(id, h.text); werr != nil {
				err = werr
				break
			}
			c.held = nil
			continue
		}
		j, text, needsModel, perr := c.prepare(id)
		if perr != nil {
			err = fmt.Errorf("foreman: %w", perr)
			break
		}
		if needsModel {
			job, ok = j, true
			break
		}
		if werr := c.write(id, text); werr != nil {
			err = werr
			break
		}
	}
	if aerr := c.advance(); aerr != nil && err == nil {
		err = aerr
	}
	return job, ok && err == nil, err
}

// Commit writes the line a job got, then builds what follows that needs no
// model and steps the view.
func (c *Chat) Commit(j Job, text string) error {
	if len(c.pending) > 0 && c.pending[0] == j.ID {
		// Only this job's own retry state ends here: a live job that moved
		// ahead of a failed import job must not reset that job's backoff.
		if c.wait != nil && c.wait.id == j.ID {
			c.wait = nil
		}
		text = finish(text, j.Limit)
		if err := c.write(j.ID, text); err != nil {
			c.held = &heldLine{id: j.ID, text: text}
			return err
		}
	}
	return c.settle()
}

// FailErr records that the call for a job failed with err. Next hands the
// job out again only after its wait (retrySteps).
func (c *Chat) FailErr(j Job, err error) {
	w := c.wait
	if w == nil || w.id != j.ID {
		w = &retryState{id: j.ID}
		c.wait = w
	}
	w.fails++
	step := w.fails - 1
	if step >= len(retrySteps) {
		step = len(retrySteps) - 1
	}
	w.at, w.err = c.cfg.Now().Add(retrySteps[step]), err
}

// RetryNow ends the wait, so the next Next hands the failed node out
// again. An append does this. The count of failures stays, so a node that
// fails again waits the next step and not the first.
func (c *Chat) RetryNow() {
	if c.wait != nil {
		c.wait.at = time.Time{}
	}
}

// RetryIn is how long until the failed front node may be asked again. ok
// is false when no node waits for a retry.
func (c *Chat) RetryIn() (d time.Duration, ok bool) {
	if c.wait == nil || len(c.pending) == 0 || c.pending[0] != c.wait.id {
		return 0, false
	}
	d = c.wait.at.Sub(c.cfg.Now())
	if d < 0 {
		d = 0
	}
	return d, true
}

// settle builds the nodes at the front that need no model and steps the
// view.
func (c *Chat) settle() error {
	_, _, err := c.Next()
	return err
}

// write appends the front pending node to the tree file.
func (c *Chat) write(id NodeID, text string) error {
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
	c.pending = c.pending[1:]
	return nil
}

// chain is the nodes queued for message i: its own and each parent it
// completes, a binary carry up the tree.
func chain(i int64) []NodeID {
	id := NodeID{L: 0, I: i}
	var out []NodeID
	for {
		out = append(out, id)
		if id.I%2 == 0 || id.L >= maxLevel {
			return out
		}
		id = id.Parent()
	}
}

// complete reports whether every node queued for message i is written.
func (c *Chat) complete(i int64) bool {
	for _, id := range chain(i) {
		if _, ok := c.nodes[id]; !ok && !c.skip[id] {
			return false
		}
	}
	return true
}

// advance steps the view over each message in order while its nodes are
// all written, and saves the view when it moved.
func (c *Chat) advance() error {
	if !c.stepView() {
		return nil
	}
	return c.saveView()
}

// stepView steps the view over each message in order while its nodes are
// all written. It reports whether the view moved.
func (c *Chat) stepView() bool {
	moved := false
	for c.view.Count < int64(len(c.msgs)) && c.complete(c.view.Count) {
		c.viewStep(c.view.Count)
		moved = true
	}
	return moved
}

// replayView rebuilds the view in memory from the tree, from an empty view.
// It writes nothing.
func (c *Chat) replayView() {
	c.view = viewState{}
	c.stepView()
}

// Tail is the messages the view does not cover yet, word for word, oldest
// first: the ones after the view's count.
func (c *Chat) Tail() ([]Message, error) {
	var out []Message
	for i := c.view.Count; i < int64(len(c.msgs)); i++ {
		m, err := c.Message(i)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// runJob asks s for a job's line.
func runJob(ctx context.Context, s Summarizer, j Job) (string, error) {
	if j.ID.L == 0 {
		return s.Compress(ctx, j.Msg, j.Limit)
	}
	return s.Merge(ctx, j.ID, j.Left, j.Right, j.Limit)
}

// importNode says whether node id is an import node: the message that
// completes it is a past message. A node that merges imported lines into a
// live message is a live node.
func (c *Chat) importNode(id NodeID) bool {
	last := (id.I+1)<<uint(id.L) - 1
	return last >= 0 && last < int64(len(c.msgs)) && c.msgs[last].past
}

// prioritize moves the live nodes whose inputs are built ahead of the first
// import node, so a live message is compacted before any import job that
// was queued earlier (FM-R-72). A live node that waits for an import node
// as its input stays behind it. The order among live nodes, and among the
// rest, does not change.
func (c *Chat) prioritize() {
	if c.held != nil || len(c.pending) < 2 {
		return
	}
	first := -1
	for k, id := range c.pending {
		if c.importNode(id) {
			first = k
			break
		}
	}
	if first < 0 {
		return
	}
	var live, rest []NodeID
	for _, id := range c.pending[first:] {
		ready := id.L == 0
		if !ready {
			l, r := id.Children()
			_, lok := c.nodes[l]
			_, rok := c.nodes[r]
			ready = lok && rok
		}
		if ready && !c.importNode(id) {
			live = append(live, id)
		} else {
			rest = append(rest, id)
		}
	}
	if len(live) == 0 {
		return
	}
	out := append([]NodeID(nil), c.pending[:first]...)
	out = append(out, live...)
	c.pending = append(out, rest...)
}

// Drain builds the pending nodes with s. See DrainGated.
func (c *Chat) Drain(ctx context.Context, s Summarizer, mu sync.Locker) error {
	return c.DrainGated(ctx, s, mu, nil)
}

// DrainGated builds the pending nodes with s, one at a time, until none can
// be built now. It takes mu (nil for none) around each step on the chat and
// none around a model call. A live node always goes before an import node.
// When hold is set and says true, no model call starts for an import node
// and Drain returns nil with the node pending; hold runs outside mu. A node
// whose call fails waits (retrySteps) and stays pending, and nothing behind
// it is built. Drain returns that error, or, when the node still waits, the
// error that put it to wait. It starts no call for a node that waits, so a
// caller that drains again at once does not repeat a failed call.
func (c *Chat) DrainGated(ctx context.Context, s Summarizer, mu sync.Locker, hold func() bool) error {
	lock, unlock := func() {}, func() {}
	if mu != nil {
		lock, unlock = mu.Lock, mu.Unlock
	}
	for {
		lock()
		c.prioritize()
		job, ok, err := c.Next()
		imp := ok && err == nil && c.importNode(job.ID)
		unlock()
		if err != nil || !ok {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if imp && hold != nil && hold() {
			return nil
		}
		text, err := runJob(ctx, s, job)
		lock()
		if err != nil {
			if ctx.Err() == nil {
				c.FailErr(job, err)
			}
			unlock()
			return err
		}
		err = c.Commit(job, text)
		unlock()
		if err != nil {
			return err
		}
	}
}
