package gateroot

import (
	"errors"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// DisarmFile is the kill-switch marker's name in the gate root.
const DisarmFile = "DISARMED"

// EnvelopesDir is gate._envelopes_dir.
func EnvelopesDir(root string) string { return Join(root, "envelopes") }

// ShadowDir is gate._shadow_dir.
func ShadowDir(root string) string { return Join(root, "shadow") }

// Disarm is gate.disarm: the root made private and the marker written.
// followed is the file written when the marker path was a symlink.
func Disarm(root string) (marker, followed string, err error) {
	if err := mkdirPrivate(root); err != nil {
		return "", "", err
	}
	marker = Join(root, DisarmFile)
	followed, err = WriteFile(marker, "disarmed by operator\n")
	return marker, followed, err
}

// Arm is gate.arm: the marker removed if Path.exists() sees it.
func Arm(root string) error {
	marker := Join(root, DisarmFile)
	if exists(marker) {
		return os.Remove(marker)
	}
	return nil
}

// IsDisarmed is gate.is_disarmed.
func IsDisarmed(root string) bool { return exists(Join(root, DisarmFile)) }

// ErrName marks a directory entry whose name is not valid UTF-8, which
// Python would carry as surrogate escapes.
var ErrName = errors.New("an envelope file name is not valid UTF-8")

// Envelopes is sorted(e.stem for e in envelopes.glob("*.json")): every
// entry whose name ends in .json, hidden ones and directories included,
// case-sensitively, by its stem.
func Envelopes(root string) ([]string, error) {
	d := EnvelopesDir(root)
	if !exists(d) {
		return []string{}, nil
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		if st, serr := os.Stat(d); serr == nil && !st.IsDir() {
			// pathlib globs a file as an empty directory.
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		name := e.Name()
		if !utf8.ValidString(name) {
			return nil, ErrName
		}
		if strings.HasSuffix(name, ".json") {
			out = append(out, stem(name))
		}
	}
	sort.Strings(out)
	return out, nil
}

// stem is PurePath.stem: the name less its last suffix, where a suffix
// needs a dot that is neither first nor last.
func stem(name string) string {
	i := strings.LastIndex(name, ".")
	if 0 < i && i < len(name)-1 {
		return name[:i]
	}
	return name
}
