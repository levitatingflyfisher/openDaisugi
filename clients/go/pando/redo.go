package pando

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// A node whose model call gave no line is written as a clipped copy of its
// source (FM-R-64). Redo builds such nodes again once the model is fixed.
//
// A rebuilt node is written as a new line for the same node. The tree loads
// the last line of a node, so the new line replaces the old one and nothing
// is cut from a file. A parent's line was made from the lines of its two
// children: either both joined (when they fit), or a model merge. When a
// child is written again, the parent is stale, so it is made again too, with
// or without the model. A parent is stale when its line was written before a
// line of one of its children.

// stale says whether a built node was written before one of its children.
func (c *Chat) stale(id NodeID) bool {
	if id.L == 0 {
		return false
	}
	l, r := id.Children()
	mine := c.seq[id]
	return c.seq[l] > mine || c.seq[r] > mine
}

// RedoSet says which nodes a redo run builds again. Clipped picks the nodes
// whose text is a clipped fallback, the nodes stale after one of their
// children, and every built ancestor of those. FromLevel, when HasLevel is
// set, picks every built node at that level and above. Both together pick the
// union.
type RedoSet struct {
	Clipped   bool
	FromLevel int
	HasLevel  bool
}

// inLevel says whether id is picked by the level rule.
func (r RedoSet) inLevel(id NodeID) bool { return r.HasLevel && id.L >= r.FromLevel }

// RedoFile is the marker of a redo run that has not finished, in the tree
// directory of a store. It holds the count of node lines the tree had when
// the run began (after), the profile that run used (model) and what it
// picked (set). A run with the same set and the same profile skips a node
// that holds a line after that count and is not clipped: an earlier run
// built it. A run with another set or profile replaces the marker. A run
// that finishes removes it.
const RedoFile = "redo.json"

type redoMarker struct {
	After int    `json:"after"`
	Model string `json:"model"`
	Set   string `json:"set"`
}

// Key names what a redo run picks, for the marker.
func (r RedoSet) Key() string {
	k := ""
	if r.Clipped {
		k = "clipped"
	}
	if r.HasLevel {
		if k != "" {
			k += "+"
		}
		k += fmt.Sprintf("level:%d", r.FromLevel)
	}
	return k
}

// redoModelKey is the profile name a marker holds: the default profile has
// two spellings.
func redoModelKey(profile string) string {
	if profile == "" {
		return "default"
	}
	return profile
}

// redoResume reads the marker of this store. It says where the earlier run
// began when the marker is there and is for this set and this profile.
func (c *Chat) redoResume(set RedoSet) (after int, ok bool) {
	b, err := os.ReadFile(filepath.Join(c.treeDir(), RedoFile))
	if err != nil {
		return 0, false
	}
	var m redoMarker
	if json.Unmarshal(b, &m) != nil || m.Set != set.Key() || m.Model != redoModelKey(c.redoModel) {
		return 0, false
	}
	return m.After, true
}

// redoBegin writes the marker of a run that picks nodes, unless the marker of
// this same run is there.
func (c *Chat) redoBegin(set RedoSet) error {
	if _, ok := c.redoResume(set); ok {
		return nil
	}
	line, err := encodeLine(redoMarker{After: c.nseq, Model: redoModelKey(c.redoModel), Set: set.Key()})
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(c.treeDir(), RedoFile), line)
}

// redoEnd removes the marker of a store, when it belongs to this run: it is
// for the same set and the same profile. A marker of another redo stays.
func redoEnd(dir string, set RedoSet, profile string) {
	path := filepath.Join(dir, "tree", RedoFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m redoMarker
	if json.Unmarshal(b, &m) != nil || m.Set != set.Key() || m.Model != redoModelKey(profile) {
		return
	}
	_ = os.Remove(path)
}

// RedoNodes names the built nodes redo would write again for set. Lowest
// level first, then oldest. A node an earlier run of the same redo built is
// left out (see RedoFile).
func (c *Chat) RedoNodes(set RedoSet) []NodeID {
	picked := map[NodeID]bool{}
	for id, n := range c.nodes {
		if set.inLevel(id) || (set.Clipped && (isClipped(n.Text) || c.stale(id))) {
			picked[id] = true
		}
	}
	if set.Clipped {
		for id := range picked {
			for p := id.Parent(); p.L <= maxLevel; p = p.Parent() {
				if _, built := c.nodes[p]; !built {
					break
				}
				picked[p] = true
			}
		}
	}
	after, resumed := c.redoResume(set)
	out := make([]NodeID, 0, len(picked))
	for id := range picked {
		if resumed && c.seq[id] > after && !isClipped(c.nodes[id].Text) {
			continue
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].L != out[j].L {
			return out[i].L < out[j].L
		}
		return out[i].I < out[j].I
	})
	return out
}

// RedoPlan counts the model calls a redo run of set can make, at most. A
// level-0 node needs one when its message does not fit a line. A parent needs
// one unless its two children are not built again and their lines join into
// one line: a child that is built again has a new line that is not known
// before the run, so the plan takes the call as needed.
func (c *Chat) RedoPlan(set RedoSet) (nodes []NodeID, calls int) {
	nodes = c.RedoNodes(set)
	picked := map[NodeID]bool{}
	for _, id := range nodes {
		picked[id] = true
	}
	for _, id := range nodes {
		if id.L == 0 {
			if m, err := c.Message(id.I); err != nil || len(m.Text) > c.cfg.NodeBytes {
				calls++
			}
			continue
		}
		l, r := id.Children()
		if picked[l] || picked[r] {
			calls++
			continue
		}
		if _, _, needs, err := c.prepare(id); err != nil || needs {
			calls++
		}
	}
	return nodes, calls
}

// rewrite writes a new line for a node that is built already.
func (c *Chat) rewrite(id NodeID, text string) error {
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
	c.markWritten(id)
	return nil
}

// redoCalls builds again the nodes RedoNodes names for set, lowest level
// first, with s. max caps the model calls of this store; a node made without the model
// does not count. A node whose new text is the same clipped text it had is
// left. Counts go into run.
func redoCalls(ctx context.Context, c *Chat, s Summarizer, set RedoSet, max int, run *tendRun) error {
	start := run.spent
	c.redoModel = run.model
	if len(c.RedoNodes(set)) > 0 {
		if err := c.redoBegin(set); err != nil {
			return err
		}
	}
	for _, id := range c.RedoNodes(set) {
		if max > 0 && run.spent-start >= max {
			break
		}
		t0 := time.Now()
		old := c.nodes[id].Text
		stale := c.stale(id)
		// An ancestor is made again only when a child was written again,
		// unless the level rule picked it.
		if !stale && !isClipped(old) && !set.inLevel(id) {
			continue
		}
		job, text, needsModel, err := c.prepare(id)
		if err != nil {
			return err
		}
		if needsModel {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			got, rerr := run.job(ctx, s, job)
			if rerr != nil {
				return rerr
			}
			run.calls++
			text = finish(got, job.Limit)
			if isClipped(text) {
				run.clipped++
			}
		}
		if text != old || stale {
			if err := c.rewrite(id, text); err != nil {
				return err
			}
			run.built++
			run.report(id, time.Since(t0))
		}
		if run.stopOnClips && run.calls == ClipStopAfter && run.clipped == run.calls {
			return ErrAllClipped
		}
	}
	return nil
}

// redoChildren is redoCalls over the stores below a store: its children
// and its branches with theirs (SubStores).
func redoChildren(ctx context.Context, storeDir string, s Summarizer, set RedoSet, max int, c func(dir string) (*Chat, error), run *tendRun) error {
	start := run.spent
	for _, ss := range SubStores(storeDir) {
		left := 0
		if max > 0 {
			if left = max - (run.spent - start); left <= 0 {
				break
			}
		}
		d := ss.Dir
		ch, err := c(d)
		if err == ErrLocked {
			run.locked = append(run.locked, d)
			continue
		}
		if err != nil {
			return err
		}
		err = redoCalls(ctx, ch, s, set, left, run)
		ch.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
