package pathways

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// legacyStore is a store as the first release left it: none of the
// additive columns, and the two columns a later release drops.
func legacyStore(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "pathways.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		"DROP TABLE pathways",
		`CREATE TABLE pathways (id TEXT PRIMARY KEY, task_description TEXT NOT NULL,
		task_embedding_json TEXT NOT NULL, envelope_json TEXT NOT NULL, plan_template_json TEXT NOT NULL,
		source_trace_ids_json TEXT NOT NULL, pitfalls_json TEXT NOT NULL DEFAULT '[]',
		validation_score REAL NOT NULL DEFAULT 0.0, version INTEGER NOT NULL DEFAULT 1,
		hit_count INTEGER NOT NULL DEFAULT 0, distilled_at REAL NOT NULL)`,
		`INSERT INTO pathways (id, task_description, task_embedding_json, envelope_json, plan_template_json,
		source_trace_ids_json, hit_count, distilled_at) VALUES ('pw1', 'build it', '[1.0, 0.0]',
		'{"id": "e", "generated_by": "t", "task": "t", "permissions": {}}',
		'{"id": "p", "source": "s", "task": "t", "steps": []}', '["t1"]', 3, 1700000000.5)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func dumpAll(t *testing.T, s *Store) ([]string, []Row) {
	t.Helper()
	ps, err := s.ReadAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range ps {
		out = append(out, DumpJSON(p.Obj))
	}
	rows, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	return out, rows
}

// A read-only store reads a legacy table as Python reads it after the
// migration, and writes nothing.
func TestOpenReadOnlyReadsALegacyTableAsMigrated(t *testing.T) {
	ro := legacyStore(t, t.TempDir())
	rw := legacyStore(t, t.TempDir())
	raw, _ := os.ReadFile(ro)
	s, err := OpenReadOnly(ro)
	if err != nil {
		t.Fatal(err)
	}
	gotPs, gotRows := dumpAll(t, s)
	if _, err := s.db.Exec("DELETE FROM pathways"); err == nil {
		t.Fatal("a read-only store took a write")
	}
	s.Close()
	m, err := Open(rw)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	wantPs, wantRows := dumpAll(t, m)
	if len(gotPs) != 1 || !reflect.DeepEqual(gotPs, wantPs) {
		t.Fatalf("pathways\n%v\n%v", gotPs, wantPs)
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("rows\n%v\n%v", gotRows, wantRows)
	}
	if after, _ := os.ReadFile(ro); string(after) != string(raw) {
		t.Fatal("the store changed")
	}
	entries, _ := os.ReadDir(filepath.Dir(ro))
	if len(entries) != 1 {
		t.Fatalf("files beside the store: %v", entries)
	}
}

func TestOpenReadOnlyReadsAFileWithNoTableAsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pathways.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE pathways"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r, err := OpenReadOnly(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if ps, rows := dumpAll(t, r); len(ps) != 0 || len(rows) != 0 {
		t.Fatalf("%v %v", ps, rows)
	}
}
