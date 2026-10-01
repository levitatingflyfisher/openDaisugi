// Package broken keeps the pile of "<name>.broken-<UTC time>" files small.
// Every place that moves damaged state aside calls Prune with the name it
// just made, and only the newest Keep() per base name stay.
package broken

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync/atomic"
)

// DefaultKeep is how many broken copies of one name stay.
const DefaultKeep = 5

var keep atomic.Int64

func init() { keep.Store(DefaultKeep) }

// SetKeep sets the count to keep. 0 keeps all. A negative value is ignored.
func SetKeep(n int) {
	if n >= 0 {
		keep.Store(int64(n))
	}
}

// Keep is the count to keep. 0 means keep all.
func Keep() int { return int(keep.Load()) }

var nameRe = regexp.MustCompile(`^(.*)\.broken-(\d{8}T\d{6}Z)(?:-(\d+))?$`)

type item struct {
	path  string
	stamp string
	n     int
}

// Prune removes the oldest broken copies of the base name that moved is a
// copy of, so the newest Keep() stay. It never removes moved itself. It is
// best effort: an error leaves the file in place.
func Prune(moved string) {
	k := Keep()
	if k == 0 {
		return
	}
	m := nameRe.FindStringSubmatch(filepath.Base(moved))
	if m == nil {
		return
	}
	dir := filepath.Dir(moved)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var all []item
	for _, e := range ents {
		em := nameRe.FindStringSubmatch(e.Name())
		if em == nil || em[1] != m[1] {
			continue
		}
		n := 1
		if em[3] != "" {
			n, _ = strconv.Atoi(em[3])
		}
		all = append(all, item{filepath.Join(dir, e.Name()), em[2], n})
	}
	if len(all) <= k {
		return
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].stamp != all[j].stamp {
			return all[i].stamp > all[j].stamp
		}
		return all[i].n > all[j].n
	})
	for _, it := range all[k:] {
		if it.path != moved {
			_ = os.RemoveAll(it.path)
		}
	}
}
