package sqlpath

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// A '?', '#', '%' or a leading "file:" in a path is part of the file name,
// as in Python's sqlite3.connect.
func TestDSNKeepsTheFileName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a?b.db", "c#d%25e.db", "file:f.db?mode=ro"} {
		p := filepath.Join(dir, name)
		dsn, err := DSN(p, "")
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		db.Close()
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
