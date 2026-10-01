package gateroot

import (
	"errors"
	"os"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"daisugi-verify/internal/statefile"
)

// DisarmFile is the kill-switch marker's name in the gate root.
const DisarmFile = "DISARMED"

// EnvelopesDir is gate._envelopes_dir.
func EnvelopesDir(root string) string { return Join(root, "envelopes") }

// AuditDir is gate._audit_dir.
func AuditDir(root string) string { return Join(root, "audit") }

// LegacyAuditDir is gate._legacy_audit_dir: the log written while audit
// mode was called shadow mode. Only the report reads it.
func LegacyAuditDir(root string) string { return Join(root, "shadow") }

// Disarm is gate.disarm: the marker written through the root's directory fd
// (GR-R-9) and read back with the gate's reader. A link planted at DISARMED
// is refused, and so is a marker the gate would not read, so this never
// reports a kill switch the gate ignores. The error is a
// *statefile.WriteRefused for either.
func Disarm(root string) (marker string, err error) {
	if _, err := statefile.WriteFile(root, nil, DisarmFile, "disarmed by operator\n", false); err != nil {
		return "", err
	}
	if err := statefile.VerifyWritten(root, nil, DisarmFile); err != nil {
		return "", err
	}
	return Join(root, DisarmFile), nil
}

// Arm is gate.arm: the marker removed through the root's directory fd
// (GR-R-9). A refused root is a *statefile.WriteRefused, so this never
// reports an armed gate it did not arm.
func Arm(root string) error {
	_, err := statefile.UnlinkFile(root, nil, DisarmFile)
	return err
}

// IsDisarmed is gate.is_disarmed: DISARMED is a regular file this user
// owns, read through no link (GR-R-1). A refusal reads as armed.
func IsDisarmed(root string) bool {
	fd, ok := statefile.Open(root, nil, DisarmFile, -1)
	if ok {
		syscall.Close(fd)
	}
	return ok
}

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
