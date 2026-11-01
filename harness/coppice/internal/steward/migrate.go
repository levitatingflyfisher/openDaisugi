package steward

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"daisugi-verify/pando"
)

// AdoptedMark is the file in the chair's dir that says the chair files of a
// moved dir were copied in.
const AdoptedMark = "adopted-from-moved"

// OldStore is the old log dir (LogDir) when it still holds a chat that
// `daisugi pando migrate` has not moved, and the new store holds none. Else
// "".
func OldStore(logDir, storeDir string) string {
	if st, err := os.Stat(filepath.Join(logDir, "main")); err != nil || !st.IsDir() {
		return ""
	}
	if filepath.Clean(logDir) == filepath.Clean(storeDir) {
		return ""
	}
	if st, err := os.Stat(filepath.Join(storeDir, "main")); err == nil && st.IsDir() {
		return ""
	}
	return logDir
}

// AdoptChairFiles copies the chair's files out of the dir `daisugi pando
// migrate` renamed (LogDir + ".moved-<stamp>") into logDir, once: turns,
// calls, the failover state, the queue and the workers' logs. It copies and
// never deletes. It returns one line to show, or "" when it did nothing.
func AdoptChairFiles(logDir string) string {
	if _, err := os.Stat(filepath.Join(logDir, AdoptedMark)); err == nil {
		return ""
	}
	moved := pando.MovedSibling(logDir)
	if moved == "" {
		return ""
	}
	ents, err := os.ReadDir(moved)
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return ""
	}
	skip := map[string]bool{"main": true, "tree": true, "view.json": true, pando.ImportFile: true, "lock": true,
		"import.lock": true, "turns": true, "tools.sock": true}
	var copied []string
	for _, e := range ents {
		if skip[e.Name()] || strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(logDir, e.Name())); err == nil {
			continue
		}
		if err := copyTree(filepath.Join(moved, e.Name()), filepath.Join(logDir, e.Name())); err == nil {
			copied = append(copied, e.Name())
		}
	}
	sort.Strings(copied)
	_ = os.WriteFile(filepath.Join(logDir, AdoptedMark), []byte(moved+"\n"), 0o600)
	if len(copied) == 0 {
		return ""
	}
	return fmt.Sprintf("Copied the steward's own files (%s) from %s. Nothing there was deleted.", strings.Join(copied, ", "), moved)
}

func copyTree(from, to string) error {
	st, err := os.Lstat(from)
	if err != nil {
		return err
	}
	switch {
	case st.IsDir():
		if err := os.MkdirAll(to, 0o700); err != nil {
			return err
		}
		ents, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if err := copyTree(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case st.Mode().IsRegular():
		b, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o600)
	}
	return nil
}
