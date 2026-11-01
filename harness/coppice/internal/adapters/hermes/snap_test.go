package hermes

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// snapshot lists every file in dir with its size and mod time.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		fi, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s %d %d", e.Name(), fi.Size(), fi.ModTime().UnixNano()))
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}
