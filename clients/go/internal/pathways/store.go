// Package pathways is the pathway store of the Python oracle
// (opendaisugi/pathway_store.py): the same SQLite schema, migrations and
// rows, so the Go binary and the Python CLI read and write one file.
package pathways

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3" // the SQLite driver, the amalgamation linked in
)

// driverName is go-sqlite3 with memory-mapped reads on every connection:
// they halve the time a scan of every stored vector takes. The file on
// disk is the same.
const driverName = "sqlite3_pathways"

func init() {
	sql.Register(driverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(c *sqlite3.SQLiteConn) error {
			_, err := c.Exec("PRAGMA mmap_size=1073741824", nil)
			return err
		},
	})
}

// readers is how many connections read the table at once.
const readers = 4

// schema is _SCHEMA, byte for byte: SQLite keeps this text in
// sqlite_master, and the Python CLI reads the same file.
const schema = `
CREATE TABLE IF NOT EXISTS pathways (
    id TEXT PRIMARY KEY,
    task_description TEXT NOT NULL,
    task_embedding_json TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    plan_template_json TEXT NOT NULL,
    source_trace_ids_json TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    hit_count INTEGER NOT NULL DEFAULT 0,
    distilled_at REAL NOT NULL,
    embedding_model TEXT NOT NULL DEFAULT '',
    embedding_model_version TEXT NOT NULL DEFAULT '',
    last_activation_at REAL NOT NULL DEFAULT 0.0,
    failure_count INTEGER NOT NULL DEFAULT 0,
    structure_signature TEXT,
    parameters_json TEXT NOT NULL DEFAULT '[]'
);
`

// additiveColumns are the columns added after the first schema, each
// with its ALTER, in the oracle's order.
var additiveColumns = [][2]string{
	{"embedding_model", "ALTER TABLE pathways ADD COLUMN embedding_model TEXT NOT NULL DEFAULT ''"},
	{"embedding_model_version", "ALTER TABLE pathways ADD COLUMN embedding_model_version TEXT NOT NULL DEFAULT ''"},
	{"last_activation_at", "ALTER TABLE pathways ADD COLUMN last_activation_at REAL NOT NULL DEFAULT 0.0"},
	{"failure_count", "ALTER TABLE pathways ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0"},
	{"structure_signature", "ALTER TABLE pathways ADD COLUMN structure_signature TEXT"},
	{"parameters_json", "ALTER TABLE pathways ADD COLUMN parameters_json TEXT NOT NULL DEFAULT '[]'"},
}

// droppedColumns are removed on open when present; a failed drop is
// ignored, as the oracle ignores it.
var droppedColumns = []string{"pitfalls_json", "validation_score"}

// ErrPath is a database path the driver would misread: its DSN syntax
// takes a '?' as the start of options.
var ErrPath = errors.New("the pathway database path holds a '?'")

// Store is PathwayStore over one database file.
type Store struct {
	db   *sql.DB
	Path string
	// readOnly is a store opened for the checks: not migrated. noTable
	// is a file with no pathways table; missing holds the additive
	// columns it lacks.
	readOnly bool
	noTable  bool
	missing  []int
}

// OpenReadOnly opens an existing store for the checks a command makes
// before it writes: nothing is made or migrated, and the file takes no
// write. Rows read as Python reads them after the migration: an absent
// additive column holds its default, a dropped column is gone, and a
// file with no table holds no rows. It carries ReadAll, All and Get.
func OpenReadOnly(dbPath string) (*Store, error) {
	if strings.ContainsRune(dbPath, '?') {
		return nil, ErrPath
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	// A URI, so that mode=ro holds; its path escapes '#', '%' and the rest.
	db, err := sql.Open(driverName, (&url.URL{Scheme: "file", Path: abs}).String()+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(readers + 1)
	db.SetMaxIdleConns(readers + 1)
	s := &Store{db: db, Path: dbPath, readOnly: true}
	cols, err := s.columns()
	if err != nil {
		db.Close()
		return nil, err
	}
	s.noTable = len(cols) == 0
	for i, c := range additiveColumns {
		if !cols[c[0]] {
			s.missing = append(s.missing, i)
		}
	}
	return s, nil
}

// additiveDefaults are the values the ALTERs of additiveColumns give an
// existing row, as the driver reads them.
var additiveDefaults = []any{"", "", float64(0), int64(0), nil, "[]"}

// asMigrated makes a row of a read-only store read as it would after the
// migration.
func (s *Store) asMigrated(r Row) {
	if !s.readOnly {
		return
	}
	for _, i := range s.missing {
		r[additiveColumns[i][0]] = additiveDefaults[i]
	}
	for _, c := range droppedColumns {
		delete(r, c)
	}
}

// Open is PathwayStore(db_path): the parent directories made, the table
// created when absent, and the additive and dropped columns applied.
func Open(dbPath string) (*Store, error) {
	if strings.ContainsRune(dbPath, '?') {
		return nil, ErrPath
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o777); err != nil {
		return nil, err
	}
	// Python's sqlite3.connect takes the path as a file name, never as a
	// URI; the driver would read a leading "file:" as one.
	dsn := dbPath
	if strings.HasPrefix(dsn, "file:") {
		dsn = "./" + dsn
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	// The readers, and one connection holding the scan's read lock.
	db.SetMaxOpenConns(readers + 1)
	db.SetMaxIdleConns(readers + 1)
	s := &Store{db: db, Path: dbPath}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	existing, err := s.columns()
	if err != nil {
		return err
	}
	for _, c := range additiveColumns {
		if !existing[c[0]] {
			if _, err := s.db.Exec(c[1]); err != nil {
				return err
			}
		}
	}
	for _, c := range droppedColumns {
		if existing[c] {
			_, _ = s.db.Exec("ALTER TABLE pathways DROP COLUMN " + c)
		}
	}
	return nil
}

func (s *Store) columns() (map[string]bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(pathways)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// Row is one row of the table: each column by name, as the driver read
// it (int64, float64, string, []byte or nil).
type Row map[string]any

// Str is a TEXT column, or "" and false when it holds another type.
func (r Row) Str(col string) (string, bool) {
	v, ok := r[col].(string)
	return v, ok
}

func (s *Store) query(q string, args ...any) ([]Row, error) {
	if s.noTable {
		return nil, nil
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			r[c] = vals[i]
		}
		s.asMigrated(r)
		out = append(out, r)
	}
	return out, rows.Err()
}

// All is _load_all_rows: SELECT * in the table's own order.
func (s *Store) All() ([]Row, error) { return s.query("SELECT * FROM pathways") }

// Get is the row with this id, or nil.
func (s *Store) Get(id string) (Row, error) {
	rows, err := s.query("SELECT * FROM pathways WHERE id = ?", id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Delete is PathwayStore.delete: true when a row went.
func (s *Store) Delete(id string) (bool, error) {
	res, err := s.db.Exec("DELETE FROM pathways WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Stats is PathwayStore.stats: the row count and SUM(hit_count), each
// 0 when NULL. total_hits keeps SQLite's type (an int, or a float when a
// row holds a REAL).
func (s *Store) Stats() (count int64, totalHits any, err error) {
	var sum any
	if err = s.db.QueryRow("SELECT COUNT(*), SUM(hit_count) FROM pathways").Scan(&count, &sum); err != nil {
		return 0, nil, err
	}
	if sum == nil {
		sum = int64(0)
	}
	if f, ok := sum.(float64); ok && f == 0 {
		sum = int64(0)
	}
	return count, sum, nil
}

// Put is PathwayStore.put: INSERT OR REPLACE of every column, each
// already in its stored text or number form.
func (s *Store) Put(v PutRow) error {
	_, err := s.db.Exec(insertSQL, v.args()...)
	return err
}

const insertSQL = "INSERT OR REPLACE INTO pathways " +
	"(id, task_description, task_embedding_json, envelope_json, " +
	"plan_template_json, source_trace_ids_json, " +
	"version, hit_count, distilled_at, " +
	"embedding_model, embedding_model_version, " +
	"last_activation_at, failure_count, " +
	"structure_signature, parameters_json) " +
	"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

func (v PutRow) args() []any {
	return []any{v.ID, v.Task, v.Embedding, v.Envelope, v.Plan, v.Traces, v.Version, v.Hits, v.DistilledAt,
		v.Model, v.ModelVersion, v.LastActivation, v.Failures, v.Signature, v.Parameters}
}

// PutRow is one row to write. Version, Hits and Failures are int64 (or
// a *big.Int text the driver cannot bind, which Put refuses).
type PutRow struct {
	ID, Task, Embedding, Envelope, Plan, Traces string
	Version, Hits, Failures                     int64
	DistilledAt, LastActivation                 float64
	Model, ModelVersion                         string
	Signature                                   any // string or nil
	Parameters                                  string
}

func (r PutRow) String() string { return fmt.Sprintf("pathway %s", r.ID) }

// scanParts reads cols of every row in table (rowid) order, split into
// rowid ranges that separate connections read at once. fn gets each
// range's rows in order; part is the range's index.
//
// The whole scan reads one state of the table: a read transaction on its
// own connection holds SQLite's shared lock from the first read to the
// last, so no writer can commit between the ranges.
func (s *Store) scanParts(cols string, fn func(part int, rows *sql.Rows) error) (parts int, err error) {
	if s.noTable {
		return 0, nil
	}
	ctx := context.Background()
	guard, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer guard.Close()
	if _, err := guard.ExecContext(ctx, "BEGIN"); err != nil {
		return 0, err
	}
	defer guard.ExecContext(ctx, "ROLLBACK") //nolint:errcheck // a read transaction has nothing to keep
	var lo, hi sql.NullInt64
	if err := guard.QueryRowContext(ctx, "SELECT min(rowid), max(rowid) FROM pathways").Scan(&lo, &hi); err != nil {
		return 0, err
	}
	if !lo.Valid {
		return 0, nil
	}
	span := hi.Int64 - lo.Int64 + 1
	parts = readers
	if span < int64(parts)*64 || span > 1<<40 {
		parts = 1
	}
	errs := make([]error, parts)
	done := make(chan int, parts)
	for k := 0; k < parts; k++ {
		a := lo.Int64 + span*int64(k)/int64(parts)
		b := lo.Int64 + span*int64(k+1)/int64(parts) - 1
		if k == parts-1 {
			b = hi.Int64
		}
		go func(k int, a, b int64) {
			defer func() { done <- k }()
			rows, err := s.db.Query("SELECT "+cols+" FROM pathways WHERE rowid BETWEEN ? AND ? ORDER BY rowid", a, b)
			if err != nil {
				errs[k] = err
				return
			}
			defer rows.Close()
			if err := fn(k, rows); err != nil {
				errs[k] = err
				return
			}
			errs[k] = rows.Err()
		}(k, a, b)
	}
	for k := 0; k < parts; k++ {
		<-done
	}
	for _, e := range errs {
		if e != nil {
			return parts, e
		}
	}
	return parts, nil
}

// UpdateEmbedding is reembed_stale's UPDATE of one row.
func (s *Store) UpdateEmbedding(id, embedding, model, version string) error {
	_, err := s.db.Exec("UPDATE pathways SET task_embedding_json = ?, embedding_model = ?, "+
		"embedding_model_version = ? WHERE id = ?", embedding, model, version, id)
	return err
}
