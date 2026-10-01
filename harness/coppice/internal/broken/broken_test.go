package broken

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPruneKeepsNewestFive(t *testing.T) {
	SetKeep(5)
	dir := t.TempDir()
	base := filepath.Join(dir, "view.json")
	var last string
	for i := 1; i <= 8; i++ {
		last = fmt.Sprintf("%s.broken-2026010%dT000000Z", base, i)
		if err := os.WriteFile(last, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "other.broken-20260101T000000Z")
	_ = os.WriteFile(other, nil, 0o600)
	Prune(last)
	ents, _ := filepath.Glob(base + ".broken-*")
	if len(ents) != 5 {
		t.Fatalf("kept %d, want 5: %v", len(ents), ents)
	}
	for i := 4; i <= 8; i++ {
		if _, err := os.Stat(fmt.Sprintf("%s.broken-2026010%dT000000Z", base, i)); err != nil {
			t.Errorf("newest %d was removed", i)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another base name was touched")
	}
}
