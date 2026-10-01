// Package envgen is the oracle's envelope generation (envelope.py): the
// Tier-0 pathway lookup, the Tier-1 slot (tier1.py), the envelope cache
// (envelope_cache.py), the Tier-2 model ladder with refinement hints, and
// the checks around them (self-consistency, inheritance). It also carries
// pathway_bind (bind a typed pathway's holes with one model call, then
// re-verify) and compose (pathways as skill steps).
//
// An envelope is the model_dump() value of models.Envelope, a
// *pyjson.Object as internal/pmodel validates it.
package envgen

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/sqlpath"
)

// KeyArgs are the inputs make_cache_key hashes. Context and Parent nil
// are None; Tier1 nil leaves the tier1 line out, so a Tier-2 key is the
// key older releases wrote.
type KeyArgs struct {
	Task      string
	Context   *string
	Model     string
	Parent    *string
	Summarize bool
	Thinking  string
	Tier1     *string
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CacheKey is make_cache_key: the SHA-256 of the lines, not normalized.
func CacheKey(a KeyArgs) string {
	sum := "0"
	if a.Summarize {
		sum = "1"
	}
	lines := []string{
		"task:" + a.Task,
		"context:" + orEmpty(a.Context),
		"model:" + a.Model,
		"parent:" + orEmpty(a.Parent),
		"summarize:" + sum,
		"thinking:" + a.Thinking,
	}
	if a.Tier1 != nil {
		lines = append(lines, "tier1:"+*a.Tier1)
	}
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

const cacheSchema = `
CREATE TABLE IF NOT EXISTS envelope_cache (
    cache_key TEXT PRIMARY KEY,
    prompt_version TEXT NOT NULL,
    envelope_json TEXT NOT NULL,
    inserted_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_version ON envelope_cache(prompt_version);
`

// Cache is EnvelopeCache: one SQLite file, rows of another prompt version
// evicted when it is opened.
type Cache struct {
	path    string
	version string
	// Evicted is how many rows the open evicted.
	Evicted int64
}

func (c *Cache) open() (*sql.DB, error) {
	dsn, err := sqlpath.DSN(c.path, "")
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite3", dsn)
}

// OpenCache is EnvelopeCache(path, prompt_version=PromptVersion).
func OpenCache(path string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	c := &Cache{path: path, version: PromptVersion}
	db, err := c.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.Exec(cacheSchema); err != nil {
		return nil, err
	}
	res, err := db.Exec("DELETE FROM envelope_cache WHERE prompt_version != ?", c.version)
	if err != nil {
		return nil, err
	}
	c.Evicted, _ = res.RowsAffected()
	return c, nil
}

// ErrCacheRow is a cached envelope that does not validate: get raises
// pydantic's ValidationError, which the caller does not catch.
type ErrCacheRow struct{ Msg string }

func (e *ErrCacheRow) Error() string { return e.Msg }

// Get is EnvelopeCache.get for a key: the envelope, or nil on a miss.
func (c *Cache) Get(key string) (*pyjson.Object, error) {
	db, err := c.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var text any
	err = db.QueryRow("SELECT envelope_json FROM envelope_cache WHERE cache_key = ?", key).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s string
	switch x := text.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return nil, &ErrCacheRow{"a cached envelope is not text"}
	}
	out, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, s)
	if verr != nil {
		return nil, &ErrCacheRow{verr.String()}
	}
	return out.(*pyjson.Object), nil
}

// Put is EnvelopeCache.put: best effort, a failed write is dropped.
func (c *Cache) Put(env *pyjson.Object, key string) {
	db, err := c.open()
	if err != nil {
		return
	}
	defer db.Close()
	now := float64(time.Now().UnixNano()) / 1e9
	_, _ = db.Exec("INSERT OR REPLACE INTO envelope_cache (cache_key, prompt_version, envelope_json, inserted_at) VALUES (?, ?, ?, ?)",
		key, c.version, pathways.DumpJSON(env), now)
}

// InsertedAt is get_inserted_at: the row's inserted_at, ok false on a miss.
func (c *Cache) InsertedAt(key string) (float64, bool, error) {
	db, err := c.open()
	if err != nil {
		return 0, false, err
	}
	defer db.Close()
	var v float64
	err = db.QueryRow("SELECT inserted_at FROM envelope_cache WHERE cache_key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// Invalidate is EnvelopeCache.invalidate: best effort.
func (c *Cache) Invalidate(key string) bool {
	db, err := c.open()
	if err != nil {
		return false
	}
	defer db.Close()
	res, err := db.Exec("DELETE FROM envelope_cache WHERE cache_key = ?", key)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}
