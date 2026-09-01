// Package rank is opendaisugi.rank: which of N attempts at one task is
// best, and how sure we are. Elimination, quality tiers, Bradley-Terry over
// the judges' votes that hold in both orders with a weak prior and a
// seeded bootstrap, the owner's answers as order constraints, and cost
// last. The arithmetic follows the oracle step by step (RK-R-5, RK-R-6),
// so the three languages agree on every float.
package rank

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"daisugi-verify/internal/pyjson"
)

const (
	MaxAttempts      = 8
	DefaultThreshold = 0.9
	DefaultPrior     = 1.0
	DefaultBootstrap = 1000
	MaxBootstrap     = 10000
	maxIter          = 500
	tol              = 1e-10
)

var (
	rankingID = regexp.MustCompile(`^[A-Za-z0-9._:#-]{1,96}$`)
	attemptID = regexp.MustCompile(`^[A-Za-z0-9._#-]{1,32}$`)
	costKeys  = []string{"tokens", "wall_ms", "diff_lines"}
)

// Error is rank.RankError: an attempts file that does not read.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, a ...any) { panic(&Error{fmt.Sprintf(format, a...)}) }

// R9 is round(x, 9).
func R9(x float64) float64 { return pyjson.Round(x, 9) }

// Attempt is rank.Attempt.
type Attempt struct {
	ID, ContentHash, Author  string
	EdgeProof                *string
	StartedByParent, RanPlan bool
	Verify                   *pyjson.Object // ok, violations
	verifyOK                 bool
	verifyViolations         int64
	hasGate                  bool
	gateOverride             int64
	Tests                    []*pyjson.Object // name, required, result
	Features                 []*pyjson.Object // name, required, green
	Cost                     *pyjson.Object   // tokens, wall_ms, diff_lines, estimated
	cost                     map[string]int64
	Where                    any
	hasWhere                 bool
	Summary                  string
}

// Comparison is rank.Comparison.
type Comparison struct{ A, B, AHash, BHash, Shown, Outcome, Judge, PairID string }

func (c Comparison) canon() *pyjson.Object {
	return pyjson.NewObject().Set("a", c.A).Set("a_hash", c.AHash).Set("b", c.B).Set("b_hash", c.BHash).
		Set("judge", c.Judge).Set("outcome", c.Outcome).Set("pair_id", c.PairID).Set("shown", c.Shown)
}

// Policy is rank.Policy.
type Policy struct {
	Threshold, Prior float64
	Bootstrap        int
	Judges           []string
}

// Pair is an owner constraint: Win before Lose.
type Pair struct{ Win, Lose string }

// Ranking is rank.Ranking.
type Ranking struct {
	ID, Task, Project string
	Attempts          []*Attempt
	Comparisons       []Comparison // nil when the file has none
	HasComparisons    bool
	Owner             []Pair
	HasOwner          bool
	Policy            Policy
}

func isInt(v any) (*big.Int, bool) {
	x, ok := v.(pyjson.Int)
	if !ok {
		return nil, false
	}
	n, ok := new(big.Int).SetString(x.Text, 10)
	return n, ok
}

var limit53 = new(big.Int).Lsh(big.NewInt(1), 53)

func count(v any, what string) int64 {
	n, ok := isInt(v)
	if !ok || n.Sign() < 0 || n.Cmp(limit53) > 0 {
		fail("%s must be a whole number from 0 to 2**53", what)
	}
	return n.Int64()
}

func text(v any, what string) string {
	s, ok := v.(string)
	if !ok {
		fail("%s must be a string", what)
	}
	return s
}

func flag(v any, what string) bool {
	b, ok := v.(bool)
	if !ok {
		fail("%s must be true or false", what)
	}
	return b
}

func obj(v any, what string) *pyjson.Object {
	o, ok := v.(*pyjson.Object)
	if !ok {
		fail("%s must be an object", what)
	}
	return o
}

func list(v any, what string) []any {
	l, ok := v.([]any)
	if !ok {
		fail("%s must be a list", what)
	}
	return l
}

// Finite is a JSON number as a finite float (int or float, never a bool).
func Finite(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case pyjson.Int:
		g, err := strconv.ParseFloat(x.Text, 64)
		if err != nil && !math.IsInf(g, 0) {
			return 0, false
		}
		f = g
	case pyjson.Float:
		f = float64(x)
	case float64:
		f = x
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func number(v any, what string) float64 {
	switch v.(type) {
	case pyjson.Int, pyjson.Float, float64:
	default:
		fail("%s must be a number", what)
	}
	f, ok := Finite(v)
	if !ok {
		fail("%s must be a finite number", what)
	}
	return f
}

// getDef is o.get(k, def): def when absent, the value (nil for null) when present.
func getDef(o *pyjson.Object, k string, def any) any {
	v, ok := o.Get(k)
	if !ok {
		return def
	}
	return v
}

func inStrs(v any, set ...string) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	for _, x := range set {
		if s == x {
			return s, true
		}
	}
	return "", false
}

func parseAttempt(raw any, i int) *Attempt {
	o := obj(raw, fmt.Sprintf("attempts[%d]", i))
	aid, ok := o.Value("id").(string)
	if !ok || !attemptID.MatchString(aid) {
		fail("attempts[%d].id must be 1 to 32 of A-Z a-z 0-9 . _ # -", i)
	}
	w := "attempt " + aid
	h, ok := o.Value("content_hash").(string)
	if !ok || h == "" {
		fail("%s: content_hash must be a string that is not empty", w)
	}
	a := &Attempt{ID: aid, ContentHash: h, cost: map[string]int64{}}
	if v := o.Value("author"); v != nil {
		a.Author = text(v, w+": author")
	}
	if ep := o.Value("edge_proof"); ep != nil {
		s, ok := inStrs(ep, "ok", "failed")
		if !ok {
			fail(`%s: edge_proof must be "ok", "failed" or absent`, w)
		}
		a.EdgeProof = &s
	}
	if v := o.Value("started_by_parent"); v != nil {
		a.StartedByParent = flag(v, w+": started_by_parent")
	}
	if v := o.Value("ran_plan"); v != nil {
		a.RanPlan = flag(v, w+": ran_plan")
	}
	if v := o.Value("verify"); v != nil {
		vo := obj(v, w+": verify")
		a.verifyOK = flag(vo.Value("ok"), w+": verify.ok")
		a.verifyViolations = count(getDef(vo, "violations", pyjson.Int{Text: "0"}), w+": verify.violations")
		a.Verify = pyjson.NewObject().Set("ok", a.verifyOK).Set("violations", int(a.verifyViolations))
	}
	if v := o.Value("gate"); v != nil {
		g := obj(v, w+": gate")
		count(getDef(g, "denies", pyjson.Int{Text: "0"}), w+": gate.denies")
		a.gateOverride = count(getDef(g, "override_allows", pyjson.Int{Text: "0"}), w+": gate.override_allows")
		a.hasGate = true
	}
	for j, t := range list(getDef(o, "tests", []any{}), w+": tests") {
		to := obj(t, fmt.Sprintf("%s: tests[%d]", w, j))
		res, ok := inStrs(to.Value("result"), "pass", "fail", "not_run")
		if !ok {
			fail(`%s: tests[%d].result must be "pass", "fail" or "not_run"`, w, j)
		}
		name := text(to.Value("name"), fmt.Sprintf("%s: tests[%d].name", w, j))
		req := flag(getDef(to, "required", false), fmt.Sprintf("%s: tests[%d].required", w, j))
		a.Tests = append(a.Tests, pyjson.NewObject().Set("name", name).Set("required", req).Set("result", res))
	}
	for j, f := range list(getDef(o, "features", []any{}), w+": features") {
		fo := obj(f, fmt.Sprintf("%s: features[%d]", w, j))
		name := text(fo.Value("name"), fmt.Sprintf("%s: features[%d].name", w, j))
		req := flag(getDef(fo, "required", false), fmt.Sprintf("%s: features[%d].required", w, j))
		green := flag(fo.Value("green"), fmt.Sprintf("%s: features[%d].green", w, j))
		a.Features = append(a.Features, pyjson.NewObject().Set("name", name).Set("required", req).Set("green", green))
	}
	cost := obj(getDef(o, "cost", pyjson.NewObject()), w+": cost")
	a.Cost = pyjson.NewObject()
	for _, k := range costKeys {
		if v := cost.Value(k); v != nil {
			n := count(v, w+": cost."+k)
			a.cost[k] = n
			a.Cost.Set(k, int(n))
		}
	}
	est := list(getDef(cost, "estimated", []any{}), w+": cost.estimated")
	for _, k := range est {
		if _, ok := inStrs(k, costKeys...); !ok {
			fail("%s: cost.estimated names tokens, wall_ms or diff_lines only", w)
		}
	}
	if len(est) > 0 {
		var names []any
		for _, k := range costKeys {
			for _, e := range est {
				if e.(string) == k {
					names = append(names, k)
					break
				}
			}
		}
		a.Cost.Set("estimated", names)
	}
	a.Where, a.hasWhere = o.Value("where"), o.Value("where") != nil
	if pv := o.Value("previews"); pv != nil {
		po := obj(pv, w+": previews")
		if s := po.Value("summary"); s != nil {
			a.Summary = text(s, w+": previews.summary")
		}
	}
	return a
}

func parseComparison(raw any, i int) Comparison {
	w := fmt.Sprintf("comparisons[%d]", i)
	o := obj(raw, w)
	shown, ok := inStrs(o.Value("shown"), "ab", "ba")
	if !ok {
		fail(`%s.shown must be "ab" or "ba"`, w)
	}
	outcome, ok := inStrs(o.Value("outcome"), "a", "b", "tie", "skip")
	if !ok {
		fail(`%s.outcome must be "a", "b", "tie" or "skip"`, w)
	}
	c := Comparison{
		A:     text(o.Value("a"), w+".a"),
		B:     text(o.Value("b"), w+".b"),
		AHash: text(o.Value("a_hash"), w+".a_hash"),
		BHash: text(o.Value("b_hash"), w+".b_hash"),
		Shown: shown, Outcome: outcome,
		Judge:  text(o.Value("judge"), w+".judge"),
		PairID: text(o.Value("pair_id"), w+".pair_id"),
	}
	if c.A == c.B {
		fail("%s compares %s with itself", w, c.A)
	}
	return c
}

func parsePolicy(raw any) Policy {
	p := Policy{Threshold: DefaultThreshold, Prior: DefaultPrior, Bootstrap: DefaultBootstrap, Judges: []string{}}
	if raw == nil {
		return p
	}
	o := obj(raw, "policy")
	if v := o.Value("threshold"); v != nil {
		p.Threshold = number(v, "policy.threshold")
		if !(0.5 <= p.Threshold && p.Threshold <= 1.0) {
			fail("policy.threshold must be from 0.5 to 1")
		}
	}
	if v := o.Value("prior"); v != nil {
		p.Prior = number(v, "policy.prior")
		if !(0.01 <= p.Prior && p.Prior <= 100.0) {
			fail("policy.prior must be from 0.01 to 100")
		}
	}
	if v := o.Value("bootstrap"); v != nil {
		n, ok := isInt(v)
		if !ok || n.Cmp(big.NewInt(1)) < 0 || n.Cmp(big.NewInt(MaxBootstrap)) > 0 {
			fail("policy.bootstrap must be a whole number from 1 to %d", MaxBootstrap)
		}
		p.Bootstrap = int(n.Int64())
	}
	if v := o.Value("judges"); v != nil {
		for j, name := range list(v, "policy.judges") {
			p.Judges = append(p.Judges, text(name, fmt.Sprintf("policy.judges[%d]", j)))
		}
	}
	return p
}

// Parse is rank.parse.
func Parse(doc any) (r *Ranking, err error) {
	defer func() {
		if x := recover(); x != nil {
			e, ok := x.(*Error)
			if !ok {
				panic(x)
			}
			r, err = nil, e
		}
	}()
	o := obj(doc, "the attempts file")
	rid, ok := o.Value("ranking_id").(string)
	if !ok || !rankingID.MatchString(rid) {
		fail("ranking_id must be 1 to 96 of A-Z a-z 0-9 . _ : # -")
	}
	r = &Ranking{ID: rid}
	if v := o.Value("task"); v != nil {
		r.Task = text(v, "task")
	}
	if v := o.Value("project"); v != nil {
		r.Project = text(v, "project")
	}
	raws := list(o.Value("attempts"), "attempts")
	if len(raws) < 1 || len(raws) > MaxAttempts {
		fail("attempts must hold 1 to %d attempts", MaxAttempts)
	}
	for i, a := range raws {
		r.Attempts = append(r.Attempts, parseAttempt(a, i))
	}
	seen := map[string]bool{}
	for _, a := range r.Attempts {
		if seen[a.ID] {
			fail("attempt id %s is used twice", a.ID)
		}
		seen[a.ID] = true
	}
	if v := o.Value("comparisons"); v != nil {
		r.HasComparisons = true
		r.Comparisons = []Comparison{}
		for i, c := range list(v, "comparisons") {
			r.Comparisons = append(r.Comparisons, parseComparison(c, i))
		}
	}
	if v := o.Value("owner_constraints"); v != nil {
		r.HasOwner = true
		r.Owner = []Pair{}
		for i, pr := range list(v, "owner_constraints") {
			l, ok := pr.([]any)
			var x, y string
			if ok && len(l) == 2 {
				var ok1, ok2 bool
				x, ok1 = l[0].(string)
				y, ok2 = l[1].(string)
				ok = ok1 && ok2
			} else {
				ok = false
			}
			if !ok {
				fail("owner_constraints[%d] must be a list of two attempt ids", i)
			}
			for _, id := range []string{x, y} {
				if !seen[id] {
					fail("owner_constraints[%d] names no attempt %s", i, id)
				}
			}
			if x == y {
				fail("owner_constraints[%d] puts %s before itself", i, x)
			}
			r.Owner = append(r.Owner, Pair{x, y})
		}
	}
	r.Policy = parsePolicy(o.Value("policy"))
	return r, nil
}

// ParseComparison is rank._comparison, for a journal row.
func ParseComparison(raw any, i int) (c Comparison, err error) {
	defer func() {
		if x := recover(); x != nil {
			e, ok := x.(*Error)
			if !ok {
				panic(x)
			}
			err = e
		}
	}()
	return parseComparison(raw, i), nil
}

// ---------------------------------------------------------------------------
// The fit
// ---------------------------------------------------------------------------

// Eliminate is rank.eliminate.
func Eliminate(a *Attempt) []string {
	var why []string
	if a.EdgeProof != nil && *a.EdgeProof == "failed" {
		why = append(why, "edge proof failed: never ran")
	} else if a.StartedByParent && a.EdgeProof == nil {
		why = append(why, "edge proof missing")
	}
	if a.Verify != nil && !a.verifyOK {
		why = append(why, fmt.Sprintf("verify failed: %d violations", a.verifyViolations))
	} else if a.Verify == nil && a.RanPlan {
		why = append(why, "verify missing")
	}
	for _, t := range a.Tests {
		req, _ := t.Value("required").(bool)
		res, _ := t.Value("result").(string)
		name, _ := t.Value("name").(string)
		if req && res == "fail" {
			why = append(why, "required test failed: "+name)
		} else if req && res == "not_run" {
			why = append(why, "required test not run: "+name)
		}
	}
	for _, f := range a.Features {
		req, _ := f.Value("required").(bool)
		green, _ := f.Value("green").(bool)
		if req && !green {
			name, _ := f.Value("name").(string)
			why = append(why, "required feature not green: "+name)
		}
	}
	return why
}

type qkey struct{ tests, feats int }

func qualityKey(a *Attempt) qkey {
	var k qkey
	for _, t := range a.Tests {
		if req, _ := t.Value("required").(bool); !req && t.Value("result") == "pass" {
			k.tests++
		}
	}
	for _, f := range a.Features {
		if req, _ := f.Value("required").(bool); !req && f.Value("green") == true {
			k.feats++
		}
	}
	return k
}

// Vote is one judge's vote on a pair (I < J by id); Win is I's share.
type Vote struct {
	I, J, Judge string
	Win         float64
}

func verdict(c Comparison) (string, bool) {
	switch c.Outcome {
	case "skip":
		return "", false
	case "tie":
		return "tie", true
	case "a":
		return c.A, true
	}
	return c.B, true
}

func sorted2(a, b string) (string, string) {
	if b < a {
		return b, a
	}
	return a, b
}

// VotesFrom is rank.votes_from.
func VotesFrom(comps []Comparison, byID map[string]*Attempt, warnings *[]string) []Vote {
	type gkey struct{ judge, pair string }
	groups := map[gkey][]Comparison{}
	var order []gkey
	for _, c := range comps {
		missing := ""
		for _, x := range []string{c.A, c.B} {
			if byID[x] == nil {
				missing = x
				break
			}
		}
		if missing != "" {
			*warnings = append(*warnings, fmt.Sprintf("a comparison by %s names no attempt %s; ignored", c.Judge, missing))
			continue
		}
		if c.AHash != byID[c.A].ContentHash || c.BHash != byID[c.B].ContentHash {
			*warnings = append(*warnings, fmt.Sprintf("a comparison by %s on %s and %s binds to a hash that changed; ignored", c.Judge, c.A, c.B))
			continue
		}
		k := gkey{c.Judge, c.PairID}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], c)
	}
	type vkey struct{ judge, i, j string }
	type kept struct {
		pair string
		v    Vote
	}
	keep := map[vkey]kept{}
	var keys []vkey
	for _, k := range order {
		rows := groups[k]
		pi, pj := sorted2(rows[0].A, rows[0].B)
		good := len(rows) == 2
		if good {
			shown := map[string]bool{rows[0].Shown: true, rows[1].Shown: true}
			good = shown["ab"] && shown["ba"]
			for _, r := range rows {
				x, y := sorted2(r.A, r.B)
				if x != pi || y != pj {
					good = false
				}
			}
		}
		if !good {
			*warnings = append(*warnings, fmt.Sprintf("judge %s call %s does not hold the pair once in each order; no vote", k.judge, k.pair))
			continue
		}
		v1, ok1 := verdict(rows[0])
		v2, ok2 := verdict(rows[1])
		if !ok1 || !ok2 {
			continue
		}
		win := 0.5
		if v1 == v2 && v1 != "tie" {
			if v1 == pi {
				win = 1.0
			} else {
				win = 0.0
			}
		}
		vk := vkey{k.judge, pi, pj}
		if old, ok := keep[vk]; ok {
			*warnings = append(*warnings, fmt.Sprintf("judge %s voted more than once on %s and %s; one vote kept", k.judge, pi, pj))
			if k.pair >= old.pair {
				continue
			}
		} else {
			keys = append(keys, vk)
		}
		keep[vk] = kept{k.pair, Vote{I: pi, J: pj, Judge: k.judge, Win: win}}
	}
	sort.Slice(keys, func(a, b int) bool {
		x, y := keys[a], keys[b]
		if x.i != y.i {
			return x.i < y.i
		}
		if x.j != y.j {
			return x.j < y.j
		}
		return x.judge < y.judge
	})
	out := make([]Vote, 0, len(keys))
	for _, k := range keys {
		out = append(out, keep[k].v)
	}
	return out
}

// BTFit is rank.bt_fit: members in id order.
func BTFit(members []string, votes []Vote, prior float64) map[string]float64 {
	idx := map[string]int{}
	for k, m := range members {
		idx[m] = k
	}
	n := len(members)
	wins := make([]float64, n)
	for i := range wins {
		wins[i] = prior / 2.0
	}
	games := make([][]float64, n)
	for i := range games {
		games[i] = make([]float64, n)
	}
	for _, v := range votes {
		a, b := idx[v.I], idx[v.J]
		wins[a] = wins[a] + v.Win
		wins[b] = wins[b] + (1.0 - v.Win)
		games[a][b] = games[a][b] + 1.0
		games[b][a] = games[b][a] + 1.0
	}
	s := make([]float64, n)
	for i := range s {
		s[i] = 1.0
	}
	for it := 0; it < maxIter; it++ {
		next := make([]float64, n)
		for a := 0; a < n; a++ {
			denom := float64(prior / float64(s[a]+1.0))
			for b := 0; b < n; b++ {
				if b != a && games[a][b] != 0.0 {
					denom = denom + float64(games[a][b]/float64(s[a]+s[b]))
				}
			}
			next[a] = wins[a] / denom
		}
		diff := 0.0
		for a := 0; a < n; a++ {
			d := math.Abs(next[a] - s[a])
			if d > diff {
				diff = d
			}
		}
		s = next
		if diff < tol {
			break
		}
	}
	out := map[string]float64{}
	for _, m := range members {
		out[m] = s[idx[m]]
	}
	return out
}

func pRef(s float64) float64 { return s / float64(s+1.0) }

// SplitMix64 is rank.SplitMix64.
type SplitMix64 struct{ state uint64 }

func (g *SplitMix64) Next() uint64 {
	g.state += 0x9E3779B97F4A7C15
	z := g.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func strList(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func objList(os []*pyjson.Object) []any {
	out := make([]any, 0, len(os))
	for _, o := range os {
		out = append(out, o)
	}
	return out
}

// SeedOf is rank.seed_of.
func SeedOf(r *Ranking, comps []Comparison, owner []Pair) string {
	var atts []any
	for _, a := range r.Attempts {
		var ep any
		if a.EdgeProof != nil {
			ep = *a.EdgeProof
		}
		var verify any
		if a.Verify != nil {
			verify = a.Verify
		}
		atts = append(atts, pyjson.NewObject().Set("content_hash", a.ContentHash).Set("cost", a.Cost).
			Set("edge_proof", ep).Set("features", objList(a.Features)).Set("id", a.ID).
			Set("ran_plan", a.RanPlan).Set("started_by_parent", a.StartedByParent).
			Set("tests", objList(a.Tests)).Set("verify", verify))
	}
	cs := []any{}
	for _, c := range comps {
		cs = append(cs, c.canon())
	}
	ow := []any{}
	for _, p := range owner {
		ow = append(ow, []any{p.Win, p.Lose})
	}
	doc := pyjson.NewObject().Set("attempts", atts).Set("comparisons", cs).Set("owner_constraints", ow).
		Set("policy", pyjson.NewObject().Set("bootstrap", r.Policy.Bootstrap).Set("judges", strList(r.Policy.Judges)).
			Set("prior", r.Policy.Prior).Set("threshold", r.Policy.Threshold)).
		Set("ranking_id", r.ID)
	sum := sha256.Sum256([]byte(pyjson.CanonicalASCII(doc)))
	return hex.EncodeToString(sum[:])[:16]
}

func closure(edges []Pair) map[Pair]bool {
	succ := map[string]map[string]bool{}
	for _, e := range edges {
		if succ[e.Win] == nil {
			succ[e.Win] = map[string]bool{}
		}
		succ[e.Win][e.Lose] = true
	}
	keysOf := func(m map[string]bool) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	out := map[Pair]bool{}
	var starts []string
	for k := range succ {
		starts = append(starts, k)
	}
	sort.Strings(starts)
	for _, start := range starts {
		todo := keysOf(succ[start])
		seen := map[string]bool{}
		for len(todo) > 0 {
			x := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if seen[x] {
				continue
			}
			seen[x] = true
			out[Pair{start, x}] = true
			todo = append(todo, keysOf(succ[x])...)
		}
	}
	return out
}

func ownerEdges(pairs []Pair, surv map[string]bool, warnings *[]string) []Pair {
	var edges []Pair
	for _, p := range pairs {
		if !surv[p.Win] || !surv[p.Lose] {
			continue
		}
		dup := false
		for _, e := range edges {
			dup = dup || e == p
		}
		if !dup {
			edges = append(edges, p)
		}
	}
	clo := closure(edges)
	cyc := map[string]bool{}
	for p := range clo {
		if clo[Pair{p.Lose, p.Win}] {
			cyc[p.Win] = true
		}
	}
	if len(cyc) > 0 {
		var names []string
		for k := range cyc {
			names = append(names, k)
		}
		sort.Strings(names)
		*warnings = append(*warnings, fmt.Sprintf("the owner's answers form a cycle among %s; none of them applied", strings.Join(names, ", ")))
		var kept []Pair
		for _, e := range edges {
			if !(cyc[e.Win] && cyc[e.Lose]) {
				kept = append(kept, e)
			}
		}
		edges = kept
	}
	return edges
}

func applyOwner(base []string, edges []Pair) []string {
	preds := map[string][]string{}
	for _, e := range edges {
		preds[e.Lose] = append(preds[e.Lose], e.Win)
	}
	placed := map[string]bool{}
	var out []string
	left := append([]string{}, base...)
	for len(left) > 0 {
		for i, x := range left {
			ok := true
			for _, p := range preds[x] {
				ok = ok && placed[p]
			}
			if ok {
				out = append(out, x)
				placed[x] = true
				left = append(left[:i], left[i+1:]...)
				break
			}
		}
	}
	return out
}

// costKey is rank._cost_key: an unknown cost sorts after every known one.
func costKey(a *Attempt) [6]int64 {
	var k [6]int64
	for i, name := range costKeys {
		v, ok := a.cost[name]
		if !ok {
			k[2*i] = 1
		} else {
			k[2*i+1] = v
		}
	}
	return k
}

func lessKey(a, b [6]int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Join is rank._join.
func Join(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// Fit is rank.fit. comps and owner stand in for the file's own when it has none.
func Fit(r *Ranking, comps []Comparison, owner []Pair, warnings []string) *pyjson.Object {
	warnings = append([]string{}, warnings...)
	if r.HasComparisons {
		comps = r.Comparisons
	}
	if r.HasOwner {
		owner = r.Owner
	}
	byID := map[string]*Attempt{}
	for _, a := range r.Attempts {
		byID[a.ID] = a
	}
	eliminated := []any{}
	var survivors []*Attempt
	for _, a := range r.Attempts {
		why := Eliminate(a)
		if len(why) > 0 {
			eliminated = append(eliminated, pyjson.NewObject().Set("id", a.ID).Set("reasons", strList(why)))
			continue
		}
		survivors = append(survivors, a)
		if a.hasGate && a.gateOverride > 0 {
			warnings = append(warnings, fmt.Sprintf("%s was let out of its envelope %d times by an operator allow; it is kept", a.ID, a.gateOverride))
		}
	}
	seed := SeedOf(r, comps, owner)
	out := pyjson.NewObject().Set("ranking_id", r.ID).Set("status", "none_survived").Set("eliminated", eliminated).
		Set("quality_tiers", []any{}).Set("order", []any{}).Set("decided_by", []any{}).Set("scores", pyjson.NewObject()).
		Set("leader", nil).Set("confidence", nil).Set("label", "no_label").Set("next", nil).Set("stop", "nothing_to_ask").
		Set("owner_constraints", []any{})
	finish := func() *pyjson.Object {
		out.Set("warnings", strList(warnings)).Set("seed", seed)
		return out
	}
	if len(survivors) == 0 {
		return finish()
	}
	surv := map[string]bool{}
	sb := map[string]*Attempt{}
	for _, a := range survivors {
		surv[a.ID] = true
		sb[a.ID] = a
	}
	// Quality tiers, best first.
	var keys []qkey
	for _, a := range survivors {
		k := qualityKey(a)
		dup := false
		for _, x := range keys {
			dup = dup || x == k
		}
		if !dup {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tests != keys[j].tests {
			return keys[i].tests > keys[j].tests
		}
		return keys[i].feats > keys[j].feats
	})
	var tiers [][]string
	tierOf := map[string]int{}
	for t, k := range keys {
		var members []string
		for _, a := range survivors {
			if qualityKey(a) == k {
				members = append(members, a.ID)
			}
		}
		sort.Strings(members)
		for _, m := range members {
			tierOf[m] = t
		}
		tiers = append(tiers, members)
	}
	if len(survivors) > 1 && len(tiers) == 1 && keys[0] == (qkey{}) {
		warnings = append(warnings, "no optional test or feature separates the attempts; only opinion ranks them")
	}
	var votes []Vote
	for _, v := range VotesFrom(comps, byID, &warnings) {
		if surv[v.I] && surv[v.J] && tierOf[v.I] == tierOf[v.J] {
			votes = append(votes, v)
		}
	}
	edges := ownerEdges(owner, surv, &warnings)
	clo := closure(edges)
	prior := r.Policy.Prior
	point := map[string]float64{}
	var tierVotes [][]Vote
	for _, members := range tiers {
		in := map[string]bool{}
		for _, m := range members {
			in[m] = true
		}
		var tv []Vote
		for _, v := range votes {
			if in[v.I] {
				tv = append(tv, v)
			}
		}
		tierVotes = append(tierVotes, tv)
		for m, s := range BTFit(members, tv, prior) {
			point[m] = s
		}
	}
	seedN, _ := strconv.ParseUint(seed, 16, 64)
	rng := &SplitMix64{seedN}
	B := r.Policy.Bootstrap
	samples := map[string][]float64{}
	boots := make([]map[string]float64, 0, B)
	for b := 0; b < B; b++ {
		refit := map[string]float64{}
		for t, members := range tiers {
			tv := tierVotes[t]
			if len(members) < 2 {
				for m, s := range BTFit(members, nil, prior) {
					refit[m] = s
				}
				continue
			}
			var draw []Vote
			for range tv {
				draw = append(draw, tv[rng.Next()%uint64(len(tv))])
			}
			for m, s := range BTFit(members, draw, prior) {
				refit[m] = s
			}
		}
		boots = append(boots, refit)
		for m := range surv {
			samples[m] = append(samples[m], pRef(refit[m]))
		}
	}
	beats := map[Pair]int{}
	for _, members := range tiers {
		for _, x := range members {
			for _, y := range members {
				if x == y {
					continue
				}
				n := 0
				for _, rf := range boots {
					if R9(rf[x]) > R9(rf[y]) {
						n++
					}
				}
				beats[Pair{x, y}] = n
			}
		}
	}
	var base []string
	klass := map[string]int{}
	next := 0
	for _, members := range tiers {
		pref := append([]string{}, members...)
		sort.SliceStable(pref, func(i, j int) bool {
			pi, pj := R9(pRef(point[pref[i]])), R9(pRef(point[pref[j]]))
			if pi != pj {
				return pi > pj
			}
			return pref[i] < pref[j]
		})
		groups := [][]string{{pref[0]}}
		for k := 1; k < len(pref); k++ {
			share := R9(float64(beats[Pair{pref[k-1], pref[k]}]) / float64(B))
			if share >= r.Policy.Threshold {
				groups = append(groups, []string{pref[k]})
			} else {
				groups[len(groups)-1] = append(groups[len(groups)-1], pref[k])
			}
		}
		for _, g := range groups {
			gs := append([]string{}, g...)
			sort.SliceStable(gs, func(i, j int) bool {
				ki, kj := costKey(sb[gs[i]]), costKey(sb[gs[j]])
				if ki != kj {
					return lessKey(ki, kj)
				}
				return gs[i] < gs[j]
			})
			for _, m := range gs {
				base = append(base, m)
				klass[m] = next
			}
			next++
		}
	}
	order := applyOwner(base, edges)
	pos := map[string]int{}
	for k, m := range base {
		pos[m] = k
	}
	isEdge := map[Pair]bool{}
	for _, e := range edges {
		isEdge[e] = true
	}
	decided := []any{}
	for k := 0; k+1 < len(order); k++ {
		x, y := order[k], order[k+1]
		switch {
		case isEdge[Pair{x, y}] || pos[x] > pos[y]:
			decided = append(decided, "owner")
		case tierOf[x] != tierOf[y]:
			decided = append(decided, "quality")
		case klass[x] != klass[y]:
			decided = append(decided, "preference")
		case costKey(sb[x]) != costKey(sb[y]):
			decided = append(decided, "cost")
		default:
			decided = append(decided, "tie")
		}
	}
	loI := (5 * (B - 1)) / 100
	hiI := (95 * (B - 1)) / 100
	scores := pyjson.NewObject()
	for _, m := range order {
		vals := append([]float64{}, samples[m]...)
		sort.Float64s(vals)
		nv := 0
		for _, v := range votes {
			if v.I == m || v.J == m {
				nv++
			}
		}
		scores.Set(m, pyjson.NewObject().Set("strength", R9(pRef(point[m]))).Set("lo", R9(vals[loI])).
			Set("hi", R9(vals[hiI])).Set("votes", nv))
	}
	leader := order[0]
	var rivals []string
	for _, m := range tiers[tierOf[leader]] {
		if m != leader {
			rivals = append(rivals, m)
		}
	}
	confidence := 1.0
	if len(rivals) > 0 {
		n := 0
		for _, rf := range boots {
			all := true
			for _, m := range rivals {
				if !(clo[Pair{leader, m}] || R9(rf[leader]) > R9(rf[m])) {
					all = false
					break
				}
			}
			if all {
				n++
			}
		}
		confidence = R9(float64(n) / float64(B))
	}
	ownerPick := false
	for _, e := range edges {
		ownerPick = ownerPick || e.Win == leader
	}
	tiersOut := []any{}
	for _, t := range tiers {
		tiersOut = append(tiersOut, strList(t))
	}
	oc := []any{}
	for _, e := range edges {
		oc = append(oc, []any{e.Win, e.Lose})
	}
	out.Set("quality_tiers", tiersOut).Set("order", strList(order)).Set("decided_by", decided).Set("scores", scores).
		Set("leader", leader).Set("confidence", confidence).Set("owner_constraints", oc)
	if len(survivors) == 1 {
		out.Set("status", "single").Set("label", leader).Set("stop", "nothing_to_ask")
		return finish()
	}
	if ownerPick || confidence >= r.Policy.Threshold {
		out.Set("status", "ranked").Set("label", leader).Set("stop", "confident")
		return finish()
	}
	out.Set("status", "provisional").Set("label", "no_label")
	voted := map[[3]string]bool{}
	for _, v := range votes {
		voted[[3]string{v.Judge, v.I, v.J}] = true
	}
	type near struct {
		d  float64
		nv int
		m  string
	}
	nearness := func(m string) near {
		p := R9(point[leader] / float64(point[leader]+point[m]))
		nv := 0
		for _, v := range votes {
			if (v.I == leader && v.J == m) || (v.I == m && v.J == leader) {
				nv++
			}
		}
		return near{R9(math.Abs(p - 0.5)), nv, m}
	}
	cands := append([]string{}, rivals...)
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := nearness(cands[i]), nearness(cands[j])
		if a.d != b.d {
			return a.d < b.d
		}
		if a.nv != b.nv {
			return a.nv < b.nv
		}
		return a.m < b.m
	})
	for _, m := range cands {
		i, j := sorted2(leader, m)
		for _, judge := range r.Policy.Judges {
			if !voted[[3]string{judge, i, j}] {
				p := R9(point[leader] / float64(point[leader]+point[m]))
				out.Set("next", pyjson.NewObject().Set("pair", []any{leader, m}).Set("judge", judge).
					Set("why", fmt.Sprintf("P(%s beats %s) is %s, the nearest to even", leader, m, pyjson.FloatRepr(p)))).
					Set("stop", nil)
				return finish()
			}
		}
	}
	out.Set("stop", "judges_exhausted")
	return finish()
}

// Exit is rank.EXIT.
var Exit = map[string]int{"ranked": 0, "single": 0, "provisional": 3, "none_survived": 1}

func fstr(v any) string {
	switch x := v.(type) {
	case float64:
		return pyjson.FloatRepr(x)
	case pyjson.Float:
		return pyjson.FloatRepr(float64(x))
	}
	return fmt.Sprint(v)
}

// FitText is rank.fit_text.
func FitText(res *pyjson.Object) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ranking %s: %s\n", res.Value("ranking_id"), res.Value("status"))
	order, _ := res.Value("order").([]any)
	decided, _ := res.Value("decided_by").([]any)
	scores, _ := res.Value("scores").(*pyjson.Object)
	for k, mv := range order {
		m := mv.(string)
		sc := scores.Value(m).(*pyjson.Object)
		how := ""
		if k > 0 {
			how = fmt.Sprintf(" [%s]", decided[k-1])
		}
		fmt.Fprintf(&b, "  %d. %s: strength %s (%s to %s), %v votes%s\n", k+1, m, fstr(sc.Value("strength")),
			fstr(sc.Value("lo")), fstr(sc.Value("hi")), sc.Value("votes"), how)
	}
	elim, _ := res.Value("eliminated").([]any)
	for _, e := range elim {
		eo := e.(*pyjson.Object)
		var rs []string
		for _, x := range eo.Value("reasons").([]any) {
			rs = append(rs, x.(string))
		}
		fmt.Fprintf(&b, "  out: %s: %s\n", eo.Value("id"), strings.Join(rs, "; "))
	}
	if res.Value("leader") != nil {
		fmt.Fprintf(&b, "Leader: %s (confidence %s); label: %s\n", res.Value("leader"), fstr(res.Value("confidence")), res.Value("label"))
	} else {
		b.WriteString("No attempt survived; nothing is chosen.\n")
	}
	oc, _ := res.Value("owner_constraints").([]any)
	for _, p := range oc {
		pl := p.([]any)
		fmt.Fprintf(&b, "Owner: %s before %s\n", pl[0], pl[1])
	}
	if n, ok := res.Value("next").(*pyjson.Object); ok {
		pair := n.Value("pair").([]any)
		fmt.Fprintf(&b, "Next: judge %s on %s and %s: %s\n", n.Value("judge"), pair[0], pair[1], n.Value("why"))
	} else {
		fmt.Fprintf(&b, "Stop: %s\n", res.Value("stop"))
	}
	ws, _ := res.Value("warnings").([]any)
	for _, w := range ws {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	return b.String()
}
