package pathways

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"

	"daisugi-verify/internal/embed/lexical"
	"daisugi-verify/internal/embed/potion"
)

// EmbeddingModelVersion is distiller._EMBEDDING_MODEL_VERSION: the second
// half of a row's provenance stamp.
const EmbeddingModelVersion = "3"

// The config keys of the backends this binary does not carry.
const (
	miniLM = "all-MiniLM-L6-v2"
	int8   = "int8"
)

// ErrNotCarried is a matcher this binary does not carry. It is refused,
// never answered with another matcher.
var ErrNotCarried = errors.New("matcher not in this binary")

// Embedder turns one text into a unit vector.
type Embedder interface {
	Encode(text string) []float64
}

type lexicalEmbedder struct{}

func (lexicalEmbedder) Encode(text string) []float64 { return lexical.Encode(text) }

// Matcher is the backend a matcher_model config key selects.
type Matcher struct {
	Key       string // the config key: lexical or potion
	Identity  string // the provenance stamp
	Threshold float64
	load      func() (Embedder, error)
	emb       Embedder
}

// Embedder loads the backend once.
func (m *Matcher) Embedder() (Embedder, error) {
	if m.emb != nil {
		return m.emb, nil
	}
	e, err := m.load()
	if err != nil {
		return nil, err
	}
	m.emb = e
	return e, nil
}

// SelectMatcher is effective_matcher with active_model_name and
// active_threshold, for a binary that carries lexical and potion.
// MiniLM and int8 are refused with ErrNotCarried. Any other key is a
// matcher nothing builds: Python's find answers None, and so does Find.
func SelectMatcher(key string, pe potion.Env) (*Matcher, error) {
	switch key {
	case "lexical":
		return &Matcher{Key: key, Identity: lexical.Identity, Threshold: lexical.Threshold,
			load: func() (Embedder, error) { return lexicalEmbedder{}, nil }}, nil
	case "potion":
		id := potion.ResolveModel(pe.Lookup)
		return &Matcher{Key: key, Identity: id, Threshold: potion.Threshold,
			load: func() (Embedder, error) {
				m, err := potion.Open(id, pe)
				if err != nil {
					return nil, err
				}
				return m, nil
			}}, nil
	case miniLM, int8:
		need := "torch (sentence-transformers)"
		if key == int8 {
			need = "onnxruntime"
		}
		return nil, fmt.Errorf("%w: matcher_model %q needs %s, which this binary does not carry. "+
			"Set matcher_model: lexical (no model) or potion (no torch) in config.yaml", ErrNotCarried, key, need)
	}
	return nil, nil
}

// Match is PathwayMatch.
type Match struct {
	Pathway    *Pathway
	Similarity float64
}

// FindResult is what find did: the match, or none, and the one-time
// stale warning when it fired.
type FindResult struct {
	Match *Match
	// Warning is the stale-embeddings UserWarning text, or "".
	Warning string
	// Reason says why there is no match when a backend could not run
	// (Python's find says nothing; the probe reports it).
	Reason string
}

// Find is PathwayStore.find(task) with the configured backend.
// threshold < 0 means the backend's own. A MiniLM or int8 config is
// refused with ErrNotCarried; only rows Python would reach are read.
func (s *Store) Find(task string, matcherKey string, pe potion.Env, threshold float64) (FindResult, error) {
	var any int
	if err := s.db.QueryRow("SELECT EXISTS (SELECT 1 FROM pathways)").Scan(&any); err != nil {
		return FindResult{}, err
	}
	if any == 0 {
		return FindResult{}, nil
	}
	m, err := SelectMatcher(matcherKey, pe)
	if err != nil {
		return FindResult{}, err
	}
	if m == nil {
		return FindResult{Reason: fmt.Sprintf("matcher_model %q is not a built embedder", matcherKey)}, nil
	}
	if threshold < 0 {
		threshold = m.Threshold
	}
	emb, err := m.Embedder()
	if err != nil {
		if errors.Is(err, potion.ErrNotAvailable) {
			return FindResult{Reason: err.Error()}, nil
		}
		return FindResult{}, err
	}
	q := emb.Encode(task)
	return s.findIn(q, m.Identity, threshold)
}

// scored is one compatible row after the dimension guard.
type scored struct {
	rowid   int64
	score   float64
	keep    bool
	invalid bool
}

func (s *Store) findIn(q []float64, identity string, threshold float64) (FindResult, error) {
	// numpy's norm of a vector is BLAS ddot, whose order of sums depends
	// on the CPU's kernel; the pairwise sum is the nearer of the orders
	// this binary can know.
	qn := math.Sqrt(toSparse(q).sumsq)
	var mu sync.Mutex
	byPart := map[int][]*scored{}
	counts := map[int]int{}
	_, err := s.scanParts("rowid, embedding_model, embedding_model_version, task_embedding_json",
		func(part int, rows *sql.Rows) error {
			var out []*scored
			n := 0
			for rows.Next() {
				var rowid int64
				var model, version, text any
				if err := rows.Scan(&rowid, &model, &version, &text); err != nil {
					return err
				}
				n++
				ms, ok1 := model.(string)
				vs, ok2 := version.(string)
				if !ok1 || !ok2 {
					return fmt.Errorf("%w: a provenance column is not text", ErrUnreadable)
				}
				if !((ms == "" && vs == "") || (ms == identity && vs == EmbeddingModelVersion)) {
					continue
				}
				sc := &scored{rowid: rowid}
				out = append(out, sc)
				t, isText := text.(string)
				if !isText {
					sc.invalid = true
					continue
				}
				sp, ok := scanEmbedding(t)
				if !ok {
					sc.invalid = true
					continue
				}
				if sp.n != len(q) {
					continue // the dimension guard
				}
				sc.keep = true
				den := math.Sqrt(sp.sumsq) * qn
				if den == 0 {
					den = 1e-9
				}
				sc.score = clip(sp.dot(q) / den)
			}
			mu.Lock()
			byPart[part], counts[part] = out, n
			mu.Unlock()
			return nil
		})
	if err != nil {
		return FindResult{}, err
	}
	total := 0
	var results []*scored
	for k := 0; k < len(byPart); k++ {
		total += counts[k]
		results = append(results, byPart[k]...)
	}
	if len(results) == 0 {
		return FindResult{}, nil
	}
	var res FindResult
	stale := 1.0 - float64(len(results))/float64(total)
	if stale >= 0.10 {
		res.Warning = fmt.Sprintf("%d pathway(s) (%s of store) were embedded under a different model/version "+
			"than the current distiller. They are excluded from find() — semantic comparison across embedding "+
			"spaces would return wrong matches. Run `daisugi tend` to re-embed.",
			total-len(results), percent(stale))
	}
	best := -1
	for i, sc := range results {
		if sc.invalid {
			return res, &Invalid{"a stored embedding is not a list of numbers"}
		}
		if !sc.keep {
			continue
		}
		if best < 0 || sc.score > results[best].score ||
			(math.IsNaN(sc.score) && !math.IsNaN(results[best].score)) {
			best = i
		}
	}
	if best < 0 || results[best].score < threshold {
		return res, nil
	}
	bestScore := results[best].score
	if math.IsNaN(bestScore) {
		return res, fmt.Errorf("%w: a similarity is NaN", ErrUnreadable)
	}
	rs, err := s.query("SELECT * FROM pathways WHERE rowid = ?", results[best].rowid)
	if err != nil {
		return res, err
	}
	if len(rs) != 1 {
		return res, errors.New("the matched row went away")
	}
	p, err := FromRow(rs[0])
	if err != nil {
		return res, err
	}
	res.Match = &Match{Pathway: p, Similarity: bestScore}
	return res, nil
}

func clip(x float64) float64 {
	if x > 1 {
		return 1
	}
	if x < -1 {
		return -1
	}
	return x
}

// percent is Python's f"{x:.0%}".
func percent(x float64) string {
	return fmt.Sprintf("%.0f%%", x*100)
}
