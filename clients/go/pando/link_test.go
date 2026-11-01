package pando

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reviewer's probe: a link planted as a child store, a link as a store
// and a link as a store's main dir must not reach another store.
func TestASymlinkInAStorePathIsRefused(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "home", "a private home line")
	seedStore(t, data, "work", "a work line")
	work, _ := StoreDir(data, "work")
	if err := os.MkdirAll(filepath.Join(work, ChildrenDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../home", filepath.Join(work, ChildrenDir, "evil")); err != nil {
		t.Fatal(err)
	}
	code, out, errs := cli(t, "", "zoom", "--child", "evil", "0", "1", "--data-dir", data)
	if code == 0 || strings.Contains(out, "private home") || !strings.Contains(errs, "link") {
		t.Fatalf("child link: %d %q %q", code, out, errs)
	}
	if _, err := ChildDir(work, "evil"); err == nil {
		t.Fatal("ChildDir must refuse a link")
	}
	if _, err := OpenChild(ctx, work, "evil", Config{}); err == nil {
		t.Fatal("OpenChild must refuse a link")
	}

	// A store that is a link.
	if err := os.Symlink("home", filepath.Join(data, "pando", "other")); err != nil {
		t.Fatal(err)
	}
	code, out, errs = cli(t, "", "zoom", "0", "1", "--store", "other", "--data-dir", data)
	if code == 0 || strings.Contains(out, "private home") {
		t.Fatalf("store link: %d %q %q", code, out, errs)
	}
	if _, err := StoreDir(data, "other"); err == nil {
		t.Fatal("StoreDir must refuse a link")
	}

	// A main dir that is a link.
	side := filepath.Join(data, "pando", "side")
	if err := os.MkdirAll(side, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../home/main", filepath.Join(side, "main")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(side); err == nil {
		t.Fatal("a main dir that is a link must be refused")
	}
	if _, err := Open(ctx, Config{Dir: side}); err == nil {
		t.Fatal("a writer must refuse a main dir that is a link")
	}
}

// A day file in main/ that links into another store is never read, and a
// writer never appends through it.
func TestALinkedDayFileIsNeitherReadNorWrittenThrough(t *testing.T) {
	data := t.TempDir()
	home := seedStore(t, data, "home", "a private home line")
	days, _ := filepath.Glob(filepath.Join(home, "main", "*.jsonl"))
	before, _ := os.ReadFile(days[0])
	work, _ := StoreDir(data, "work")
	if err := os.MkdirAll(filepath.Join(work, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(days[0], filepath.Join(work, "main", filepath.Base(days[0]))); err != nil {
		t.Fatal(err)
	}
	if r, err := OpenReadOnly(work); err != nil || r.Count() != 0 {
		t.Fatalf("a reader read a linked day file: %v", err)
	}
	w, err := Open(ctx, Config{Dir: work, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	w.Append(ctx, KindUser, "written into work")
	w.Close()
	if after, _ := os.ReadFile(days[0]); string(after) != string(before) {
		t.Fatal("a writer appended through the link into the home store")
	}
}
