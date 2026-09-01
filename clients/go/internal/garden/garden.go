// Package garden is opendaisugi.gardener: the pruner and the merger that
// keep the pathway store healthy, with the decisions, reasons and store
// writes of the Python oracle.
package garden

import (
	"math"
	"math/big"
	"sort"
	"strconv"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// PruneConfig is gardener.PruneConfig.
type PruneConfig struct {
	MaxIdleDays     float64
	MaxFailureRatio float64
	MinActivations  *big.Int
}

// DefaultPrune is PruneConfig().
func DefaultPrune() PruneConfig {
	return PruneConfig{MaxIdleDays: 30, MaxFailureRatio: 0.5, MinActivations: big.NewInt(5)}
}

// PruneReport is gardener.PruneReport.
type PruneReport struct {
	RemovedIDs []string
	KeptCount  int
	Reasons    map[string]string
}

// MergeConfig is gardener.MergeConfig.
type MergeConfig struct {
	SimilarityThreshold          float64
	RequireCompatiblePermissions bool
}

// DefaultMerge is MergeConfig().
func DefaultMerge() MergeConfig {
	return MergeConfig{SimilarityThreshold: 0.92, RequireCompatiblePermissions: true}
}

// MergeReport is gardener.MergeReport.
type MergeReport struct {
	MergedPairs [][2]string
	KeptIDs     []string
	RemovedIDs  []string
}

// Store is what the passes need of the pathway store.
type Store interface {
	ReadAll(keep func(id string) bool) ([]*pathways.Pathway, error)
	Delete(id string) (bool, error)
	Put(v pathways.PutRow) error
}

// bigInt is a model int as a big.Int.
func bigInt(v any) *big.Int {
	n, _ := new(big.Int).SetString(v.(pyjson.Int).Text, 10)
	return n
}

// trueDiv is Python's int / int: the quotient correctly rounded.
func trueDiv(a, b *big.Int) float64 {
	f, _ := new(big.Rat).SetFrac(a, b).Float64()
	return f
}

// FormatF is Python's format(x, ".Nf"), inf and nan included.
func FormatF(x float64, n int) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	return strconv.FormatFloat(x, 'f', n, 64)
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

// Prune is gardener.prune: first matching reason wins (grace, then a
// failure-dominated ratio, then staleness), rows deleted one by one
// unless dryRun.
func Prune(s Store, cfg PruneConfig, dryRun bool, now float64) (PruneReport, error) {
	rep := PruneReport{Reasons: map[string]string{}}
	idleCutoff := now - cfg.MaxIdleDays*86_400
	all, err := s.ReadAll(func(string) bool { return false })
	if err != nil {
		return rep, err
	}
	for _, p := range all {
		o := p.Obj
		hits, fails := bigInt(o.Value("hit_count")), bigInt(o.Value("failure_count"))
		total := new(big.Int).Add(hits, fails)
		if total.Cmp(cfg.MinActivations) < 0 {
			rep.KeptCount++
			continue
		}
		denom := total
		if denom.Sign() == 0 {
			denom = big.NewInt(1)
		}
		ratio := trueDiv(fails, denom)
		if ratio > cfg.MaxFailureRatio {
			rep.RemovedIDs = append(rep.RemovedIDs, p.ID())
			rep.Reasons[p.ID()] = "failure_dominated (ratio=" + FormatF(ratio, 2) + ")"
			if !dryRun {
				if _, err := s.Delete(p.ID()); err != nil {
					return rep, err
				}
			}
			continue
		}
		// `last_activation_at or distilled_at`: 0.0 is falsy, NaN is not.
		freshness := floatOf(o.Value("last_activation_at"))
		if freshness == 0 {
			freshness = floatOf(o.Value("distilled_at"))
		}
		if freshness != 0 && freshness < idleCutoff {
			rep.RemovedIDs = append(rep.RemovedIDs, p.ID())
			rep.Reasons[p.ID()] = "stale (idle=" + FormatF((now-freshness)/86_400, 1) + "d)"
			if !dryRun {
				if _, err := s.Delete(p.ID()); err != nil {
					return rep, err
				}
			}
			continue
		}
		rep.KeptCount++
	}
	return rep, nil
}

// Cosine is _similarity.cosine_similarity: the zero-norm guard and the
// clamp to [-1, 1].
func Cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
	}
	na, nb = norm(a), norm(b)
	denom := na * nb
	if denom == 0 {
		denom = 1e-9
	}
	return math.Max(-1, math.Min(1, dot/denom))
}

func norm(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func vector(p *pathways.Pathway) []float64 {
	xs := p.Obj.Value("task_embedding").([]any)
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = floatOf(x)
	}
	return out
}

func permissions(p *pathways.Pathway) *pyjson.Object {
	return p.Obj.Value("envelope").(*pyjson.Object).Value("permissions").(*pyjson.Object)
}

// IntersectPermissions is permissions.intersect_permissions: booleans
// ANDed, the four lists intersected and sorted, the two ceilings the
// minimum, every other field its default.
func IntersectPermissions(perms []*pyjson.Object) *pyjson.Object {
	if len(perms) == 1 {
		return perms[0]
	}
	merged := pyjson.NewObject()
	for _, f := range []string{"shell", "network", "shell_allow_decomposition"} {
		all := true
		for _, p := range perms {
			if !pyjson.Truthy(p.Value(f)) {
				all = false
				break
			}
		}
		merged.Set(f, all)
	}
	for _, f := range []string{"shell_allowlist", "file_read", "file_write", "network_hosts"} {
		var inter map[string]bool
		for _, p := range perms {
			set := map[string]bool{}
			for _, x := range p.Value(f).([]any) {
				set[x.(string)] = true
			}
			if inter == nil {
				inter = set
				continue
			}
			for k := range inter {
				if !set[k] {
					delete(inter, k)
				}
			}
		}
		keys := make([]string, 0, len(inter))
		for k := range inter {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		xs := make([]any, len(keys))
		for i, k := range keys {
			xs[i] = k
		}
		merged.Set(f, xs)
	}
	for _, f := range []string{"max_execution_time_s", "max_output_size_mb"} {
		var min *big.Int
		for _, p := range perms {
			n := bigInt(p.Value(f))
			if min == nil || n.Cmp(min) < 0 {
				min = n
			}
		}
		merged.Set(f, pyjson.Int{Text: min.String()})
	}
	out, _ := pmodel.Validate("Permission", pmodel.Permission, merged, pmodel.Python)
	return out.(*pyjson.Object)
}

// PyEqual is Python's == on model_dump() values.
func PyEqual(a, b any) bool {
	switch x := a.(type) {
	case *pyjson.Object:
		y, ok := b.(*pyjson.Object)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for _, k := range x.Keys() {
			yv, ok := y.Get(k)
			if !ok || !PyEqual(x.Value(k), yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !PyEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case string:
		y, ok := b.(string)
		return ok && x == y
	case nil:
		return b == nil
	}
	na, oka := number(a)
	nb, okb := number(b)
	if oka && okb {
		return na.Cmp(nb) == 0
	}
	return false
}

// number is a bool, int or float as an exact rational; NaN and the
// infinities compare by float rules.
func number(v any) (*big.Rat, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewRat(1, 1), true
		}
		return new(big.Rat), true
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok {
			return nil, false
		}
		return new(big.Rat).SetInt(n), true
	case float64, pyjson.Float:
		f := floatOf(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(f), true
	}
	return nil, false
}

func permissionsCompatible(a, b *pyjson.Object) bool {
	merged := IntersectPermissions([]*pyjson.Object{a, b})
	return PyEqual(merged, a) && PyEqual(merged, b)
}

// pickWinner is merger._pick_winner: more hits wins, then the newer
// distillation (a on an equal time).
func pickWinner(a, b *pathways.Pathway) (winner, loser *pathways.Pathway) {
	ha, hb := bigInt(a.Obj.Value("hit_count")), bigInt(b.Obj.Value("hit_count"))
	if c := ha.Cmp(hb); c != 0 {
		if c > 0 {
			return a, b
		}
		return b, a
	}
	if floatOf(a.Obj.Value("distilled_at")) >= floatOf(b.Obj.Value("distilled_at")) {
		return a, b
	}
	return b, a
}

// Merge is gardener.merge: a greedy single pass over every pair in store
// order; the loser's sources and counts fold into the winner, which is
// written again (to the end of the table), and then the loser is deleted.
func Merge(s Store, cfg MergeConfig, dryRun bool) (MergeReport, error) {
	var rep MergeReport
	all, err := s.ReadAll(func(string) bool { return true })
	if err != nil {
		return rep, err
	}
	vecs := make([][]float64, len(all))
	for i, p := range all {
		p.Full()
		vecs[i] = vector(p)
	}
	removed := map[string]bool{}
	for i := range all {
		a := all[i]
		if removed[a.ID()] {
			continue
		}
		for j := i + 1; j < len(all); j++ {
			b := all[j]
			if removed[b.ID()] {
				continue
			}
			if a.Obj.Value("embedding_model") != b.Obj.Value("embedding_model") ||
				a.Obj.Value("embedding_model_version") != b.Obj.Value("embedding_model_version") {
				continue
			}
			if len(vecs[i]) != len(vecs[j]) {
				continue
			}
			sim := Cosine(vecs[i], vecs[j])
			if sim < cfg.SimilarityThreshold {
				continue
			}
			if cfg.RequireCompatiblePermissions && !permissionsCompatible(permissions(a), permissions(b)) {
				continue
			}
			winner, loser := pickWinner(a, b)
			rep.MergedPairs = append(rep.MergedPairs, [2]string{winner.ID(), loser.ID()})
			removed[loser.ID()] = true
			if !dryRun {
				union := map[string]bool{}
				for _, p := range []*pathways.Pathway{winner, loser} {
					for _, x := range p.Obj.Value("source_trace_ids").([]any) {
						union[x.(string)] = true
					}
				}
				keys := make([]string, 0, len(union))
				for k := range union {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				srcs := make([]any, len(keys))
				for k, x := range keys {
					srcs[k] = x
				}
				w := winner.Obj
				w.Set("source_trace_ids", srcs)
				for _, k := range []string{"hit_count", "failure_count"} {
					sum := new(big.Int).Add(bigInt(w.Value(k)), bigInt(loser.Obj.Value(k)))
					w.Set(k, pyjson.Int{Text: sum.String()})
				}
				row, err := winner.PyRow()
				if err != nil {
					return rep, err
				}
				if err := s.Put(row); err != nil {
					return rep, err
				}
				if _, err := s.Delete(loser.ID()); err != nil {
					return rep, err
				}
			}
			if loser.ID() == a.ID() {
				break
			}
		}
	}
	for _, p := range all {
		if removed[p.ID()] {
			rep.RemovedIDs = append(rep.RemovedIDs, p.ID())
		} else {
			rep.KeptIDs = append(rep.KeptIDs, p.ID())
		}
	}
	sort.Strings(rep.RemovedIDs)
	sort.Strings(rep.KeptIDs)
	return rep, nil
}
