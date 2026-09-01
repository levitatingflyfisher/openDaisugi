package pathways

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/pyjson"
)

// find.jsonl is written by clients/pathway_cases.py: stores, a
// matcher_model, and what the oracle's PathwayStore.find answered.
const fixtures = "../../../fixtures/pathways"

type findCase struct {
	Name       string            `json:"name"`
	Matcher    string            `json:"matcher"`
	Env        map[string]string `json:"env"`
	Rows       []map[string]any  `json:"rows"`
	RealPotion bool              `json:"real_potion"`
	GoOnly     bool              `json:"go_only"`
	Queries    []struct {
		Task      string   `json:"task"`
		Threshold *float64 `json:"threshold"`
		Expect    struct {
			ID      *string  `json:"id"`
			TieIDs  []string `json:"tie_ids"`
			Score   *float64 `json:"score"`
			Warning string   `json:"warning"`
			Refused bool     `json:"refused"`
		} `json:"expect"`
	} `json:"queries"`
}

// embeddingText is pathway_cases.unsparse: a json.dumps list of floats
// from its length and nonzero entries, or the text as given.
func embeddingText(spec map[string]any) string {
	if t, ok := spec["text"].(string); ok {
		return t
	}
	v := make([]any, int(spec["n"].(float64)))
	for i := range v {
		v[i] = 0.0
	}
	for _, nz := range spec["nz"].([]any) {
		p := nz.([]any)
		v[int(p[0].(float64))] = p[1].(float64)
	}
	return pyjson.Dumps(v, true)
}

// layOut writes rows the way pathway_cases.lay_out_db does, through the
// store's own schema.
func layOut(t *testing.T, path, home string, rows []map[string]any) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var cols, marks []string
		var vals []any
		for k, v := range r {
			cols = append(cols, k)
			marks = append(marks, "?")
			switch x := v.(type) {
			case map[string]any:
				if k == "task_embedding_json" {
					v = embeddingText(x)
				} else {
					t.Fatalf("column %s holds %v", k, x)
				}
			case string:
				v = strings.ReplaceAll(x, "{HOME}", home)
			case float64:
				if x == math.Trunc(x) && k != "distilled_at" && k != "last_activation_at" {
					v = int64(x)
				}
			}
			vals = append(vals, v)
		}
		if _, err := s.db.Exec("INSERT INTO pathways ("+strings.Join(cols, ", ")+") VALUES ("+
			strings.Join(marks, ", ")+")", vals...); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindMatchesOracle(t *testing.T) {
	f, err := os.Open(filepath.Join(fixtures, "find.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	n := 0
	for sc.Scan() {
		var c findCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		if c.RealPotion {
			continue // the real model is never downloaded by a test
		}
		n++
		t.Run(c.Name, func(t *testing.T) {
			home := t.TempDir()
			copyDir(t, filepath.Join(fixtures, "potion-tiny"), filepath.Join(home, "potion-tiny"))
			s := layOut(t, filepath.Join(home, "store.db"), home, c.Rows)
			defer s.Close()
			pe := potion.Env{
				Lookup: func(k string) (string, bool) {
					v, ok := c.Env[k]
					return strings.ReplaceAll(v, "{HOME}", home), ok
				},
				Home: home, NoFetch: true,
			}
			tol := 1e-6
			if c.Matcher == "potion" {
				tol = 1e-5
			}
			for _, q := range c.Queries {
				th := -1.0
				if q.Threshold != nil {
					th = *q.Threshold
				}
				res, err := s.Find(q.Task, c.Matcher, pe, th)
				if q.Expect.Refused {
					if !errors.Is(err, ErrNotCarried) {
						t.Errorf("%q: want a refusal, got %v", q.Task, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%q: %v", q.Task, err)
				}
				var id *string
				if res.Match != nil {
					x := res.Match.Pathway.ID()
					id = &x
				}
				switch {
				case (id == nil) != (q.Expect.ID == nil):
					t.Errorf("%q: match %v, oracle %v", q.Task, deref(id), deref(q.Expect.ID))
				case id != nil && *id != *q.Expect.ID && !contains(q.Expect.TieIDs, *id):
					t.Errorf("%q: match %s, oracle %s", q.Task, *id, *q.Expect.ID)
				case id != nil && math.Abs(res.Match.Similarity-*q.Expect.Score) > tol:
					t.Errorf("%q: score %v, oracle %v", q.Task, res.Match.Similarity, *q.Expect.Score)
				}
				if res.Warning != q.Expect.Warning {
					t.Errorf("%q: warning %q, oracle %q", q.Task, res.Warning, q.Expect.Warning)
				}
			}
		})
	}
	if n < 15 {
		t.Fatalf("only %d find cases ran", n)
	}
}

func deref(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

func TestOpenMigratesALegacyTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "pathways.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE pathways"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE pathways (id TEXT PRIMARY KEY, task_description TEXT NOT NULL,
		task_embedding_json TEXT NOT NULL, envelope_json TEXT NOT NULL, plan_template_json TEXT NOT NULL,
		source_trace_ids_json TEXT NOT NULL, pitfalls_json TEXT NOT NULL DEFAULT '[]',
		validation_score REAL NOT NULL DEFAULT 0.0, version INTEGER NOT NULL DEFAULT 1,
		hit_count INTEGER NOT NULL DEFAULT 0, distilled_at REAL NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(p); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cols, err := s.columns()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range additiveColumns {
		if !cols[c[0]] {
			t.Errorf("column %s was not added", c[0])
		}
	}
	for _, c := range droppedColumns {
		if cols[c] {
			t.Errorf("column %s was not dropped", c)
		}
	}
}

func TestPercentIsPythonFormat(t *testing.T) {
	for x, want := range map[float64]string{0.1: "10%", 0.125: "12%", 0.135: "14%", 1: "100%", 0.2: "20%"} {
		if got := percent(x); got != want {
			t.Errorf("percent(%v) = %s, want %s", x, got, want)
		}
	}
}

func TestScanFastAgreesWithGeneralReader(t *testing.T) {
	for _, s := range []string{
		"[]", "[0.0]", "[1.5]", "[0.0, 0.0, -0.0, 1e-07, 0.25]", "[0.0,0.0]", "[1, 2]", "[01.5]",
		"[1.0, ]", "[, 1.0]", "[1.0  , 2.0]", "[NaN, 1.0]", "[1.0] ", "[\"a\"]", "[0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1.0]",
	} {
		fast, fok := scanFast(s)
		vec, gok := parseEmbedding(s)
		if fok {
			if !gok {
				t.Errorf("%q: the fast path took text the general reader refuses", s)
				continue
			}
			want := toSparse(vec)
			if fast.n != want.n || len(fast.idx) != len(want.idx) || fast.sumsq != want.sumsq {
				t.Errorf("%q: fast %+v, general %+v", s, fast, want)
			}
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
