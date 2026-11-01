package pando

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBackupRelsHoldEveryPartOfEachStoreAndNeverTheLock(t *testing.T) {
	got := BackupRels([]string{"home", "work"})
	want := []string{
		"home/main", "home/tree", "home/view.json", "home/import.json", "home/children",
		"work/main", "work/tree", "work/view.json", "work/import.json", "work/children",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BackupRels = %v, want %v", got, want)
	}
	for _, r := range got {
		if filepath.Base(r) == "lock" {
			t.Fatalf("a lock in the list: %s", r)
		}
	}
}

func TestBackupStoresAreTheStoresOnDiskAndTheOnesAnArchiveNames(t *testing.T) {
	d := t.TempDir()
	for _, s := range []string{"work", "notes-2"} {
		if err := os.MkdirAll(filepath.Join(d, "pando", s, "main"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Not stores: a moved-aside copy, a migrate temp dir, a bad name.
	for _, s := range []string{"work.broken-20261101T000000Z", ".work.migrating-20261101T000000Z", "Bad"} {
		if err := os.MkdirAll(filepath.Join(d, "pando", s, "main"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	archived := []string{"home/main/2026-10-24.jsonl", "home/children/w1/main/x.jsonl", "../etc/x", "Bad/main/x", "lone"}
	got := BackupStores(filepath.Join(d, "pando"), archived)
	want := []string{"home", "notes-2", "work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BackupStores = %v, want %v", got, want)
	}
}
