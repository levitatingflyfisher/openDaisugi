package tracejournal

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// oldJournal is a journal as an early release left it: user_version 1,
// no run columns, no structure_signature and no later tables. The data
// directory's name holds characters a URI must escape.
func oldJournal(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data dir #1 %20")
	if err := os.MkdirAll(filepath.Join(dir, "journal"), 0o777); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "journal", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE traces (id TEXT PRIMARY KEY, created_at TEXT NOT NULL, task TEXT NOT NULL,
			plan_id TEXT NOT NULL, envelope_id TEXT NOT NULL, ok INTEGER NOT NULL, duration_ms REAL NOT NULL,
			violations_json TEXT NOT NULL)`,
		`INSERT INTO traces VALUES ('b', '2026-01-01T00:00:00Z', 'x', 'p', 'e', 1, 1.0, '[]')`,
		`INSERT INTO traces VALUES ('a', '2026-01-01T00:00:00Z', 'y', 'p', 'e', 1, 1.0, '[]')`,
		`INSERT INTO traces VALUES ('c', '2025-01-01T00:00:00Z', 'z', 'p', 'e', 0, 1.0, '[]')`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		out = append(out, p)
		return nil
	})
	return out
}

func TestOpenReadOnlyReadsAnOldJournalAndWritesNothing(t *testing.T) {
	dir := oldJournal(t)
	before := listing(t, dir)
	raw, _ := os.ReadFile(filepath.Join(dir, "journal", "index.db"))
	j, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := j.ListSuccessful(nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range ts {
		ids = append(ids, x.TraceID)
		if x.RunID != nil || x.RunStatus != nil || x.Signature != nil {
			t.Fatalf("a missing column reads as NULL, got %+v", x)
		}
	}
	// A tie on created_at lists the later insert first.
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("ids %v", ids)
	}
	if recs, err := j.Refinements("run1"); err != nil || len(recs) != 0 {
		t.Fatalf("a missing refinement_log reads as empty: %v %v", recs, err)
	}
	if done, err := j.IsConverted("s"); err != nil || done {
		t.Fatalf("a missing hook_conversions reads as empty: %v %v", done, err)
	}
	if _, err := j.db.Exec("CREATE TABLE x (y)"); err == nil {
		t.Fatal("a read-only journal took a write")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, before) {
		t.Fatalf("files changed:\n%v\n%v", before, got)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "journal", "index.db")); string(after) != string(raw) {
		t.Fatal("the index changed")
	}
}
