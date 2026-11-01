package pando

import (
	"daisugi-verify/internal/broken"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// DefaultStore is the store a command uses when it names none.
const DefaultStore = "work"

// ErrStoreAll is the answer to --store all (PD-R-2): no command reads two
// stores.
var ErrStoreAll = errors.New("one store at a time: pando stores are separate on purpose.")

var storeRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidStore says whether name is a store name (PD-R-1): a letter, then
// letters, digits and dashes, 32 at most. "work" and "home" are the two
// named stores.
func ValidStore(name string) bool { return storeRe.MatchString(name) }

// StoreDir is the dir of one store under a data home: <dataDir>/pando/<store>.
func StoreDir(dataDir, store string) (string, error) {
	if store == "all" {
		return "", ErrStoreAll
	}
	if !ValidStore(store) {
		return "", fmt.Errorf("bad store name %q: use a-z, 0-9 and -, 1 to 32 letters, starting with a letter", store)
	}
	if err := noLink(dataDir, "pando", store); err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "pando", store), nil
}

// noLink checks each part of base/parts... in turn, below base, and refuses
// a symbolic link: a link in a store or child path could reach another
// store. base itself may be a link (a data home moved to another disk). A
// part that does not exist ends the check.
func noLink(base string, parts ...string) error {
	p := base
	for _, part := range parts {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("pando: %s is a symbolic link; a store path may hold none", p)
		}
	}
	return nil
}

// storeLinks refuses a store dir whose parts are links: main, tree, the
// view, the import queue and the children dir.
func storeLinks(dir string) error {
	for _, part := range []string{"main", "tree", "view.json", ImportFile, ChildrenDir, "lock"} {
		if err := noLink(dir, part); err != nil {
			return err
		}
	}
	return nil
}

// ListStores names the stores under a data home: the dirs of
// <dataDir>/pando that hold a main dir. They come sorted.
func ListStores(dataDir string) ([]string, error) {
	return storesIn(filepath.Join(dataDir, "pando"))
}

// storesIn is ListStores on the pando dir itself.
func storesIn(pandoDir string) ([]string, error) {
	ents, err := os.ReadDir(pandoDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !ValidStore(e.Name()) {
			continue
		}
		if st, err := os.Stat(filepath.Join(pandoDir, e.Name(), "main")); err == nil && st.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func pathJoin(a, b string) string { return filepath.Join(a, b) }

// Clip, EncodeLine, AppendLine and Flat are the file helpers of the store,
// shared with the chair, whose own logs (turns, calls) are written the same
// way: one JSON value per line, appended with an fsync.
func Clip(s string, n int) string { return clip(s, n) }

// EncodeLine is one JSON value and a newline, with no HTML escapes.
func EncodeLine(v any) ([]byte, error) { return encodeLine(v) }

// AppendLine appends one line to a file and syncs it; it returns the offset
// the line starts at.
func AppendLine(path string, line []byte) (int64, error) { return appendLine(path, line) }

// Flat puts a text on one line.
func Flat(s string) string { return flat(s) }

// FreeName is base, or base-2, base-3 ... the first name nothing has.
func FreeName(base string) string { return freeName(base) }

// SetNoSync turns the fsync of appends off or on. Only tests of other
// packages call it, since they append hundreds of lines and do not test
// durability.
func SetNoSync(off bool) { noSync = off }

// SetBrokenKeep sets how many .broken-<time> copies of one file the store
// keeps: a tree file's bad lines, a view.json that did not read. 0 keeps
// all; the default is 5. coppice sets it from its broken_keep setting. A
// whole history moved aside is never removed, whatever this says.
func SetBrokenKeep(n int) { broken.SetKeep(n) }

// BrokenKeep is the count SetBrokenKeep set. 0 means keep all.
func BrokenKeep() int { return broken.Keep() }
