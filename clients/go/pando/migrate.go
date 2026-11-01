package pando

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrOpenElsewhere is the answer when the old dir is open in another
// process.
var ErrOpenElsewhere = errors.New("coppice has this chat open; stop it first")

// ErrTargetExists is the answer when the new store already holds a log.
var ErrTargetExists = errors.New("the new store already holds a log; nothing was copied")

// MovedInfix is what the old dir's name gets, then a UTC stamp.
const MovedInfix = ".moved-"

// Report says what Migrate did.
type Report struct {
	Copied   []string // what was copied: main, tree, view.json, import.json
	Kept     []string // the chair's files that stayed in the moved dir
	MovedTo  string   // where the old dir went
	Messages int64
	Already  bool // the dir was moved by an earlier run
}

// MovedSibling is the newest "<from>.moved-*" beside from, or "".
func MovedSibling(from string) string {
	ms, _ := filepath.Glob(from + MovedInfix + "*")
	sort.Strings(ms)
	if len(ms) == 0 {
		return ""
	}
	return ms[len(ms)-1]
}

// MigratingInfix names the temp dir a migrate copies into: a dot, the
// store, this infix, then a UTC stamp. The dot keeps it out of every store
// list, so nothing opens a copy that is not whole.
const MigratingInfix = ".migrating-"

// migrateHook, when set, runs after each file the migrate copies. Tests
// use it to stop a migrate part way.
var migrateHook func(stage string)

// Migrate moves a coppice steward dir into the store <dataDir>/pando/<store>
// (PD-R-6). It copies main/, tree/, view.json and import.json into a temp
// dir beside the store (.<store>.migrating-<stamp>), which it holds the
// store lock in. It opens the copy and checks that it holds the same
// messages and the same view. Only then does one rename put the whole copy
// in place as the store, and the lock it holds is the store's lock from
// then on. Last it renames from to from + ".moved-" + the UTC stamp of now.
// It deletes nothing. A failed or cut migrate leaves no store: the copy
// stays in the temp dir (the error names it) and the old dir stays where
// it is. The chair's files stay in the renamed dir. A second run, with from
// gone and a moved sibling beside it, reports that sibling and does
// nothing.
func Migrate(from, dataDir, store string, now time.Time) (Report, error) {
	dst, err := StoreDir(dataDir, store)
	if err != nil {
		return Report{}, err
	}
	if st, err := os.Stat(filepath.Join(from, "main")); err != nil || !st.IsDir() {
		if m := MovedSibling(from); m != "" {
			return Report{MovedTo: m, Already: true}, nil
		}
		return Report{}, fmt.Errorf("no steward log at %s", from)
	}
	lock, err := lockDir(filepath.Join(from, "lock"))
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return Report{}, ErrOpenElsewhere
		}
		return Report{}, err
	}
	defer lock.Close()
	if st, err := os.Stat(filepath.Join(dst, "main")); err == nil && st.IsDir() {
		return Report{}, ErrTargetExists
	}
	src, err := OpenReadOnly(from)
	if err != nil {
		return Report{}, err
	}
	if err := ensureDir(filepath.Dir(dst)); err != nil {
		return Report{}, err
	}
	stamp := now.UTC().Format("20060102T150405Z")
	tmp := freeName(filepath.Join(filepath.Dir(dst), "."+store+MigratingInfix+stamp))
	if err := ensureDir(tmp); err != nil {
		return Report{}, err
	}
	tlock, err := lockDir(filepath.Join(tmp, "lock"))
	if err != nil {
		return Report{}, err
	}
	defer tlock.Close()
	left := func(err error) (Report, error) {
		return Report{}, fmt.Errorf("%v. No store was made; the partial copy stays in %s and the old dir stays where it is", err, tmp)
	}
	var rep Report
	for _, d := range []string{"main", "tree"} {
		ents, err := os.ReadDir(filepath.Join(from, d))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return left(err)
		}
		if err := ensureDir(filepath.Join(tmp, d)); err != nil {
			return left(err)
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			if err := copyFile(filepath.Join(from, d, e.Name()), filepath.Join(tmp, d, e.Name())); err != nil {
				return left(err)
			}
			if migrateHook != nil {
				migrateHook(d + "/" + e.Name())
			}
		}
		rep.Copied = append(rep.Copied, d)
	}
	for _, f := range []string{"view.json", ImportFile} {
		if _, err := os.Stat(filepath.Join(from, f)); err != nil {
			continue
		}
		if err := copyFile(filepath.Join(from, f), filepath.Join(tmp, f)); err != nil {
			return left(err)
		}
		rep.Copied = append(rep.Copied, f)
	}
	cp, err := OpenReadOnly(tmp)
	if err != nil {
		return left(fmt.Errorf("the copy does not open: %w", err))
	}
	a, _ := src.ViewJSON()
	b, _ := cp.ViewJSON()
	if cp.Count() != src.Count() || !bytes.Equal(a, b) {
		return left(fmt.Errorf("the copy does not match: %d messages, want %d", cp.Count(), src.Count()))
	}
	rep.Messages = cp.Count()
	// Publish. An empty dir in the way goes (rename does not replace one on
	// every system); a dir that holds anything stays, and the rename fails.
	if st, err := os.Lstat(dst); err == nil {
		if !st.IsDir() {
			return left(fmt.Errorf("%s is in the way", dst))
		}
		_ = os.Remove(dst)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return left(fmt.Errorf("the copy could not be put in place: %w", err))
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return Report{}, err
	}
	ents, _ := os.ReadDir(from)
	skip := map[string]bool{"main": true, "tree": true, "view.json": true, ImportFile: true, "lock": true, "import.lock": true}
	for _, e := range ents {
		if !skip[e.Name()] {
			rep.Kept = append(rep.Kept, e.Name())
		}
	}
	sort.Strings(rep.Kept)
	to := from + MovedInfix + stamp
	to = freeName(to)
	if err := os.Rename(from, to); err != nil {
		return Report{}, err
	}
	rep.MovedTo = to
	return rep, nil
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return writeAtomic(to, b)
}

// IsMoved says whether name is the name of a moved dir.
func IsMoved(name string) bool { return strings.Contains(name, MovedInfix) }
