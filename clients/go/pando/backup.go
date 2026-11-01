package pando

import (
	"sort"
	"strings"
)

// BackupParts are the parts of a store that a backup holds: the log, the
// tree, the view, the import queue and the child stores. The lock is never
// one of them.
var BackupParts = []string{"main", "tree", "view.json", ImportFile, ChildrenDir}

// BackupRels is every path a backup holds for these stores, relative to the
// pando dir: <store>/<part> for each store and part. `daisugi backup` and
// `coppice backup` both use it, so the two hold the same files.
func BackupRels(stores []string) []string {
	var out []string
	for _, s := range stores {
		for _, p := range BackupParts {
			out = append(out, s+"/"+p)
		}
	}
	return out
}

// BackupStores is the stores a backup or a restore covers: each store in
// pandoDir (a dir with a main dir and a good name), and each good store
// name that is the first part of an archived path (relative to the pando
// dir). A restore passes the archive's paths, so a store the target does
// not have yet is put back; any other path stays outside the list, and the
// restore refuses it. The list comes sorted.
func BackupStores(pandoDir string, archived []string) []string {
	seen := map[string]bool{}
	on, _ := storesIn(pandoDir)
	for _, s := range on {
		seen[s] = true
	}
	for _, p := range archived {
		first, rest, ok := strings.Cut(p, "/")
		if ok && rest != "" && ValidStore(first) {
			seen[first] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
