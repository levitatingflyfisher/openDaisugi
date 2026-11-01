package pando

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// ChildrenDir is the dir under a store that holds the transcript trees of
// its workers: <store>/children/<label>/. A child is a small store of its
// own, with the same main/ and tree/ layout and the same 512-byte node
// rule. It is never merged into the parent's tree and has no view in the
// parent's. A child lives under the store that spawned it, so a home child
// is under home and a work child under work.
const ChildrenDir = "children"

var childRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidChild says whether label can name a child store.
func ValidChild(label string) bool { return childRe.MatchString(label) && label != ".." }

// ChildDir is the dir of the child store for a worker label. The children
// dir and the label must not be symbolic links: a link could reach
// another store.
func ChildDir(storeDir, label string) (string, error) {
	if !ValidChild(label) {
		return "", fmt.Errorf("bad worker label %q: use letters, digits, dot, dash and underscore, 1 to 64, starting with a letter or digit", label)
	}
	if err := noLink(storeDir, ChildrenDir, label); err != nil {
		return "", err
	}
	return filepath.Join(storeDir, ChildrenDir, label), nil
}

// ListChildren names the child stores of a store, sorted.
func ListChildren(storeDir string) []string {
	ents, err := os.ReadDir(filepath.Join(storeDir, ChildrenDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !ValidChild(e.Name()) {
			continue
		}
		if st, err := os.Stat(filepath.Join(storeDir, ChildrenDir, e.Name(), "main")); err == nil && st.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// OpenChild opens (and makes) the child store of a worker for writing.
func OpenChild(ctx context.Context, storeDir, label string, cfg Config) (*Chat, error) {
	d, err := ChildDir(storeDir, label)
	if err != nil {
		return nil, err
	}
	cfg.Dir = d
	return Open(ctx, cfg)
}

// TendChild builds the pending nodes of one child store with s. A child
// that another process has open is left for that process (0, nil).
func TendChild(ctx context.Context, storeDir, label string, s Summarizer, max int, now func() time.Time) (int, error) {
	run := &tendRun{}
	err := tendChild(ctx, storeDir, label, s, max, now, run)
	return run.built, err
}

func tendChild(ctx context.Context, storeDir, label string, s Summarizer, max int, now func() time.Time, run *tendRun) error {
	d, err := ChildDir(storeDir, label)
	if err != nil {
		return err
	}
	c, err := Open(ctx, Config{Dir: d, Now: now})
	if err == ErrLocked {
		return nil
	}
	if err != nil {
		return err
	}
	defer c.Close()
	return tendCalls(ctx, c, s, max, run)
}

// TendChildren builds the pending nodes of every store below a store (its
// children, and its branches with theirs)
// with s, the same summarizer and the same rule as the parent. max caps the
// nodes built across all children in this run (0 means all). It returns how
// many nodes it built and the first error.
func TendChildren(ctx context.Context, storeDir string, s Summarizer, max int, now func() time.Time) (int, error) {
	run := &tendRun{}
	err := tendChildren(ctx, storeDir, s, max, now, run)
	return run.built, err
}

// tendChildren is TendChildren that counts into run. max caps the model
// calls of this run across the children, counted from run.spent at the start.
func tendChildren(ctx context.Context, storeDir string, s Summarizer, max int, now func() time.Time, run *tendRun) error {
	start := run.spent
	for _, ss := range SubStores(storeDir) {
		left := 0
		if max > 0 {
			if left = max - (run.spent - start); left <= 0 {
				break
			}
		}
		if err := tendDir(ctx, ss.Dir, s, left, now, run); err != nil {
			return err
		}
	}
	return nil
}

// tendDir builds the pending nodes of the store in d. A store that another
// process has open is left for that process.
func tendDir(ctx context.Context, d string, s Summarizer, max int, now func() time.Time, run *tendRun) error {
	c, err := Open(ctx, Config{Dir: d, Now: now})
	if err == ErrLocked {
		return nil
	}
	if err != nil {
		return err
	}
	defer c.Close()
	return tendCalls(ctx, c, s, max, run)
}

// ChildRoots names the root nodes of a child store, for the work message
// and for zoom: "label:first+count" for each, oldest first.
func (c *Chat) ChildRoots(label string) []string {
	var out []string
	for _, r := range c.Roots() {
		out = append(out, label+":"+r.Name())
	}
	return out
}

// TendDir builds the pending nodes of the store in dir with s: a worker's
// child store under a branch, say. A store another process has open is
// left for that process (0, nil).
func TendDir(ctx context.Context, dir string, s Summarizer, max int, now func() time.Time) (int, error) {
	run := &tendRun{}
	err := tendDir(ctx, dir, s, max, now, run)
	return run.built, err
}
