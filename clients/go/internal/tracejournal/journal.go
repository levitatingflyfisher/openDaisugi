// Package tracejournal is the trace journal of the Python oracle
// (opendaisugi/journal.py): YAML trace bodies under
// <data_dir>/journal/traces and the SQLite index <data_dir>/journal/index.db,
// with the same schema, migrations and rows, so the Go binary and the
// Python CLI read and write one journal. It carries what the distiller and
// the capture conversion use.
package tracejournal

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3" // the SQLite driver, the amalgamation linked in

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// schema is journal._SCHEMA, byte for byte: SQLite keeps each CREATE text
// in sqlite_master, and the Python CLI reads the same file.
const schema = `
CREATE TABLE IF NOT EXISTS traces (
    id TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    task TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    envelope_id TEXT NOT NULL,
    ok INTEGER NOT NULL,
    duration_ms REAL NOT NULL,
    violations_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS refinement_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    record_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_refinement_session ON refinement_log(session_id);
CREATE TABLE IF NOT EXISTS receipts (
    run_id TEXT NOT NULL,
    step_id TEXT NOT NULL,
    timestamp REAL NOT NULL,
    evidence_hash TEXT NOT NULL,
    verify_result INTEGER NOT NULL,
    verify_details TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '{}',
    model_id TEXT,
    PRIMARY KEY (run_id, step_id)
);
CREATE INDEX IF NOT EXISTS idx_receipts_run ON receipts(run_id);
CREATE TABLE IF NOT EXISTS hook_conversions (
    session_id TEXT PRIMARY KEY,
    trace_id TEXT NOT NULL,
    converted_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS provenance_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    detail_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
`

// ErrPath is a database path the driver would misread.
var ErrPath = errors.New("the journal path holds a '?'")

// ErrUnreadable marks journal content this binary cannot read the way
// Python does. A command refuses before it writes anything.
var ErrUnreadable = errors.New("cannot read")

// Journal is journal.Journal over one data directory.
type Journal struct {
	db        *sql.DB
	DataDir   string
	TracesDir string
	// readOnly is a journal opened for the checks: not migrated, so a
	// column or table a later release added may be absent. has names what
	// is there ("table" and "table.column").
	readOnly bool
	has      map[string]bool
}

// Open is Journal(data_dir=...): the traces directory made, the index
// created, and the migrations of each user_version run in order, each
// ALTER that fails (the column is there) ignored.
func Open(dataDir string) (*Journal, error) {
	traces := filepath.Join(dataDir, "journal", "traces")
	dbPath := filepath.Join(dataDir, "journal", "index.db")
	if strings.ContainsRune(dbPath, '?') {
		return nil, ErrPath
	}
	if err := os.MkdirAll(traces, 0o777); err != nil {
		return nil, err
	}
	dsn := dbPath
	if strings.HasPrefix(dsn, "file:") {
		dsn = "./" + dsn
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	j := &Journal{db: db, DataDir: dataDir, TracesDir: traces}
	if err := j.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return j, nil
}

// OpenReadOnly opens an existing journal for the checks a command makes
// before it writes: nothing is made or migrated, and the index takes no
// write. What a later migration would add reads as Python reads it after
// that migration: an absent column as NULL, an absent table as empty.
func OpenReadOnly(dataDir string) (*Journal, error) {
	dbPath := filepath.Join(dataDir, "journal", "index.db")
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
	dsn := (&url.URL{Scheme: "file", Path: abs}).String() + "?mode=ro"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	j := &Journal{db: db, DataDir: dataDir, TracesDir: filepath.Join(dataDir, "journal", "traces"),
		readOnly: true, has: map[string]bool{}}
	if err := j.readShape(); err != nil {
		db.Close()
		return nil, err
	}
	return j, nil
}

// readShape records the tables and columns the index holds.
func (j *Journal) readShape() error {
	rows, err := j.db.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range tables {
		j.has[t] = true
		cols, err := j.db.Query("SELECT name FROM pragma_table_info(?)", t)
		if err != nil {
			return err
		}
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				cols.Close()
				return err
			}
			j.has[t+"."+c] = true
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			return err
		}
	}
	return nil
}

// hasTable is whether a migrated journal would read the table from disk.
func (j *Journal) hasTable(t string) bool { return !j.readOnly || j.has[t] }

// col is a traces column, or NULL for one this journal lacks.
func (j *Journal) col(c string) string {
	if !j.readOnly || j.has["traces."+c] {
		return c
	}
	return "NULL"
}

// Close releases the index.
func (j *Journal) Close() error { return j.db.Close() }

func (j *Journal) migrate() error {
	if _, err := j.db.Exec(schema); err != nil {
		return err
	}
	var version int
	if err := j.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	try := func(q string) { _, _ = j.db.Exec(q) }
	must := func(q string) error { _, err := j.db.Exec(q); return err }
	if version < 2 {
		for _, c := range []string{"run_id TEXT", "run_status TEXT", "failed_step_id TEXT", "total_duration_ms REAL"} {
			try("ALTER TABLE traces ADD COLUMN " + c)
		}
		if err := must("PRAGMA user_version = 2"); err != nil {
			return err
		}
	}
	if version < 3 {
		try("ALTER TABLE refinement_log ADD COLUMN cache_key TEXT")
		if err := must("CREATE INDEX IF NOT EXISTS idx_refinement_cache_key ON refinement_log(cache_key)"); err != nil {
			return err
		}
		if err := must("PRAGMA user_version = 3"); err != nil {
			return err
		}
	}
	if version < 4 {
		try("ALTER TABLE receipts ADD COLUMN model_id TEXT")
		if err := must("PRAGMA user_version = 4"); err != nil {
			return err
		}
	}
	if version < 5 {
		try("ALTER TABLE traces ADD COLUMN structure_signature TEXT")
		if err := must("CREATE INDEX IF NOT EXISTS idx_traces_structure ON traces(structure_signature)"); err != nil {
			return err
		}
		if err := must("PRAGMA user_version = 5"); err != nil {
			return err
		}
	}
	if version < 6 {
		try("CREATE TABLE IF NOT EXISTS provenance_log (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT, " +
			"detail_json TEXT NOT NULL, " +
			"inserted_at REAL NOT NULL" +
			")")
		if err := must("PRAGMA user_version = 6"); err != nil {
			return err
		}
	}
	if version < 7 {
		for _, c := range []string{"effect_class TEXT", "reversibility TEXT", "reversal_json TEXT"} {
			try("ALTER TABLE receipts ADD COLUMN " + c)
		}
		if err := must("PRAGMA user_version = 7"); err != nil {
			return err
		}
	}
	return nil
}

// Distillable is journal.DistillableTrace.
type Distillable struct {
	TraceID, Task, EnvelopeID, PlanID, CreatedAt string
	RunID, RunStatus, Signature                  any // string or nil
}

// SinceISO is the text list_successful_traces compares created_at with:
// datetime.fromtimestamp(since, tz=utc).isoformat() with Z for +00:00,
// microseconds shown only when not zero.
func SinceISO(since float64) string {
	sec, frac := math.Modf(since)
	us := math.RoundToEven(frac * 1e6)
	s := int64(sec)
	if us >= 1e6 {
		s++
		us -= 1e6
	} else if us < 0 {
		s--
		us += 1e6
	}
	t := time.Unix(s, 0).UTC()
	out := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
	if us != 0 {
		out += fmt.Sprintf(".%06d", int64(us))
	}
	return out + "Z"
}

// ListSuccessful is list_successful_traces: succeeded runs and verified
// imports, newest first. since nil reads them all.
func (j *Journal) ListSuccessful(since *float64) ([]Distillable, error) {
	if !j.hasTable("traces") {
		return nil, nil
	}
	runStatus := j.col("run_status")
	q := "SELECT id, task, envelope_id, plan_id, " + j.col("run_id") + ", " + runStatus + ", created_at, " +
		j.col("structure_signature") + " " +
		"FROM traces WHERE (" + runStatus + " = 'succeeded' OR (" + runStatus + " IS NULL AND ok = 1))"
	var args []any
	if since != nil {
		q += " AND created_at >= ?"
		args = append(args, SinceISO(*since))
	}
	// rowid breaks a tie: created_at has one-second steps, and the later
	// insert is the newer trace.
	q += " ORDER BY created_at DESC, rowid DESC"
	rows, err := j.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Distillable
	for rows.Next() {
		var v [8]any
		if err := rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7]); err != nil {
			return nil, err
		}
		str := func(x any) (string, bool) { s, ok := x.(string); return s, ok }
		d := Distillable{RunID: v[4], RunStatus: v[5], Signature: v[7]}
		var ok [5]bool
		d.TraceID, ok[0] = str(v[0])
		d.Task, ok[1] = str(v[1])
		d.EnvelopeID, ok[2] = str(v[2])
		d.PlanID, ok[3] = str(v[3])
		d.CreatedAt, ok[4] = str(v[6])
		for _, o := range ok {
			if !o {
				return nil, fmt.Errorf("%w: a trace row holds a value that is not text", ErrUnreadable)
			}
		}
		for _, x := range []any{v[4], v[5], v[7]} {
			if _, isStr := x.(string); x != nil && !isStr {
				return nil, fmt.Errorf("%w: a trace row holds a value that is not text", ErrUnreadable)
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Record is journal.TraceRecord: the envelope, plan and result as
// model_dump() values.
type Record struct {
	ID, CreatedAt, Task any
	Envelope, Plan      *pyjson.Object
	Result              *pyjson.Object
}

// LoadError is a load_trace that raises in Python: the text is str(exc).
type LoadError struct{ Type, Msg string }

func (e *LoadError) Error() string { return e.Msg }

// TracePath is the YAML body of a trace.
func (j *Journal) TracePath(id string) string { return filepath.Join(j.TracesDir, id+".yaml") }

// LoadTrace is load_trace: the YAML read with safe_load and each part
// validated. A body not in the form yaml.safe_dump writes is ErrUnreadable.
func (j *Journal) LoadTrace(id string) (*Record, error) {
	path := j.TracePath(id)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &LoadError{"FileNotFoundError", "No trace with id " + pystr.Repr(id) + " at " + path}
	}
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: the trace %s is not UTF-8", ErrUnreadable, id)
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
	v, why := pyyaml.LoadDumped(text)
	if why != nil {
		return nil, fmt.Errorf("%w: the trace %s: %s", ErrUnreadable, id, why.Why)
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("%w: the trace %s is not a mapping", ErrUnreadable, id)
	}
	rec := &Record{}
	for _, k := range []string{"id", "created_at", "task", "envelope", "plan", "result"} {
		if _, has := o.Get(k); !has {
			return nil, &LoadError{"KeyError", pystr.Repr(k)}
		}
	}
	rec.ID, rec.CreatedAt, rec.Task = o.Value("id"), o.Value("created_at"), o.Value("task")
	for _, part := range []struct {
		key   string
		model *pmodel.Model
		dst   **pyjson.Object
	}{{"envelope", pmodel.Envelope, &rec.Envelope}, {"plan", pmodel.ActionPlan, &rec.Plan},
		{"result", pmodel.VerificationResult, &rec.Result}} {
		in, isObj := o.Value(part.key).(*pyjson.Object)
		if !isObj {
			return nil, fmt.Errorf("%w: the trace %s: %s is not a mapping", ErrUnreadable, id, part.key)
		}
		// Model(**raw[...]): keyword arguments, so every key is a str.
		out, verr := part.model.ValidateObject(in, pmodel.Python)
		if verr != nil {
			for _, e := range verr.Errs {
				if e.Type == pmodel.UnreadableStep {
					return nil, fmt.Errorf("%w: the trace %s holds a plan step given as a string", ErrUnreadable, id)
				}
			}
			return nil, &LoadError{"ValidationError", verr.String()}
		}
		*part.dst = out
	}
	return rec, nil
}

// Refinements is get_refinements(session_id): each record_json read with
// RefinementRecord.model_validate_json, in insertion order. A record this
// binary does not fully read is ErrUnreadable.
func (j *Journal) Refinements(session string) ([]*pyjson.Object, error) {
	if !j.hasTable("refinement_log") {
		return nil, nil
	}
	rows, err := j.db.Query("SELECT record_json FROM refinement_log WHERE session_id = ? ORDER BY inserted_at ASC", session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pyjson.Object
	for rows.Next() {
		var text any
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		s, ok := text.(string)
		if !ok {
			return nil, fmt.Errorf("%w: a refinement record is not text", ErrUnreadable)
		}
		rec, verr := pmodel.ValidateJSON("RefinementRecord", RefinementRecord, s)
		if verr != nil {
			return nil, fmt.Errorf("%w: a refinement record of %s: %v", ErrUnreadable, session, verr)
		}
		out = append(out, rec.(*pyjson.Object))
	}
	return out, rows.Err()
}

// IsConverted is is_session_converted.
func (j *Journal) IsConverted(session string) (bool, error) {
	if !j.hasTable("hook_conversions") {
		return false, nil
	}
	var one int
	err := j.db.QueryRow("SELECT 1 FROM hook_conversions WHERE session_id = ? LIMIT 1", session).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// MarkConverted is mark_session_converted.
func (j *Journal) MarkConverted(session, traceID string, at float64) error {
	_, err := j.db.Exec("INSERT OR REPLACE INTO hook_conversions (session_id, trace_id, converted_at) VALUES (?, ?, ?)",
		session, traceID, at)
	return err
}

// TraceBody is a trace's YAML body, as Log writes it: the JSON-mode dumps
// of the envelope, plan and result through yaml.safe_dump. A value the
// dumper does not write is ErrUnreadable.
func TraceBody(task string, env, plan, result *pyjson.Object, traceID, createdAt string) (string, error) {
	payload := pyjson.NewObject().Set("id", traceID).Set("created_at", createdAt).Set("task", task).
		Set("envelope", JSONMode(env)).Set("plan", JSONMode(plan)).Set("result", JSONMode(result))
	text, why := pyyaml.SafeDump(payload)
	if why != nil {
		return "", fmt.Errorf("%w: %s", ErrUnreadable, why.Why)
	}
	return text, nil
}

// Log is Journal.log: the YAML body written first, then the index row.
// A failed insert removes the body. env, plan and result are model_dump()
// values; the YAML holds their JSON-mode dumps.
func (j *Journal) Log(task string, env, plan, result *pyjson.Object, traceID, createdAt string) error {
	text, err := TraceBody(task, env, plan, result, traceID, createdAt)
	if err != nil {
		return err
	}
	path := j.TracePath(traceID)
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		return err
	}
	var sig any
	if s, ok := StructureSignature(plan); ok {
		sig = s
	}
	violations := []any{}
	for _, v := range result.Value("violations").([]any) {
		violations = append(violations, JSONMode(v))
	}
	okInt := 0
	if result.Value("ok") == true {
		okInt = 1
	}
	_, err = j.db.Exec("INSERT INTO traces "+
		"(id, created_at, task, plan_id, envelope_id, ok, duration_ms, "+
		" violations_json, structure_signature) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		traceID, createdAt, task, plan.Value("id"), env.Value("id"), okInt,
		floatOf(result.Value("duration_ms")), pyjson.Dumps(violations, true), sig)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func floatOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case pyjson.Float:
		return float64(x)
	}
	return 0
}

// JSONMode is model_dump(mode="json") of a model_dump() value: NaN and
// the infinities as None.
func JSONMode(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
	case pyjson.Float:
		if f := float64(x); math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return float64(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = JSONMode(e)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			out.Set(k, JSONMode(x.Value(k)))
		}
		return out
	}
	return v
}

// envelopePromptVersion is envelope.ENVELOPE_PROMPT_VERSION.
const envelopePromptVersion = "2026-04-18"

// EnsureEnvelopeCache is EnvelopeCache(path, prompt_version=...), which
// the facade builds for every command that makes one: the parent made, the
// schema created, and rows of another prompt version evicted.
func EnsureEnvelopeCache(path string) error {
	if strings.ContainsRune(path, '?') {
		return ErrPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	dsn := path
	if strings.HasPrefix(dsn, "file:") {
		dsn = "./" + dsn
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS envelope_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_version ON envelope_cache(prompt_version);
`); err != nil {
		return err
	}
	_, err = db.Exec("DELETE FROM envelope_cache WHERE prompt_version != ?", envelopePromptVersion)
	return err
}
