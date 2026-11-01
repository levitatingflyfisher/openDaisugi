package rank

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Judge trust (RK-5) and judge weights (W3K-2, owner ruling A3): the Go side
// of rank.family_of, kendall_tau_b, cyclic_magnitude, beta_sample,
// judge_weights, judge_trust and weights_for. Every sum is in the oracle's
// order and every multiply that could fuse is wrapped in float64().

// Trust floor and the least answered cards before a judge can be dropped.
const (
	TrustFloor    = 0.5
	TrustMinCards = 5
)

var familyMerge = map[string]string{"o1": "gpt", "o3": "gpt", "o4": "gpt", "chatgpt": "gpt", "gemma": "gemini",
	"haiku": "claude", "sonnet": "claude", "opus": "claude", "fable": "claude"}

// providerWord is true for 1 or more of a-z, 0-9 and _.
func providerWord(w string) bool {
	for i := 0; i < len(w); i++ {
		c := w[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return w != ""
}

// FamilyOf is rank.family_of (W3K-1).
func FamilyOf(model string) string {
	word := strings.ToLower(pystr.Strip(model))
	// One leading provider/ is dropped when something follows it.
	if i := strings.IndexByte(word, '/'); i > 0 && i < len(word)-1 && providerWord(word[:i]) {
		word = word[i+1:]
	}
	if i := strings.IndexAny(word, "-./:"); i >= 0 {
		word = word[:i]
	}
	if f, ok := familyMerge[word]; ok {
		return f
	}
	j := 0
	for j < len(word) && word[j] >= 'a' && word[j] <= 'z' {
		j++
	}
	letters := word[:j]
	if f, ok := familyMerge[letters]; ok {
		return f
	}
	return letters
}

func sign(x int) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

func positions(order [][]string) map[string]int {
	pos := map[string]int{}
	for k, g := range order {
		for _, x := range g {
			pos[x] = k
		}
	}
	return pos
}

// KendallTauB is rank.kendall_tau_b. Each element of an order is a tie
// group. The second result is false for under 2 shared items or when one
// order ties every pair.
func KendallTauB(a, b [][]string) (float64, bool) {
	pa, pb := positions(a), positions(b)
	var items []string
	for x := range pa {
		if _, ok := pb[x]; ok {
			items = append(items, x)
		}
	}
	sort.Strings(items)
	n := len(items)
	if n < 2 {
		return 0, false
	}
	conc, disc, ta, tb := 0, 0, 0, 0
	for x := 0; x < n; x++ {
		for y := x + 1; y < n; y++ {
			sa := sign(pa[items[x]] - pa[items[y]])
			sb := sign(pb[items[x]] - pb[items[y]])
			if sa == 0 {
				ta++
			}
			if sb == 0 {
				tb++
			}
			if sa != 0 && sb != 0 {
				if sa == sb {
					conc++
				} else {
					disc++
				}
			}
		}
	}
	n0 := n * (n - 1) / 2
	denom := (n0 - ta) * (n0 - tb)
	if denom == 0 {
		return 0, false
	}
	return R9(float64(conc-disc) / math.Sqrt(float64(denom))), true
}

// CyclicMagnitude is rank.cyclic_magnitude: the share of one judge's voted
// pairs that lie on a cycle of majority directions. The second result is
// false when the votes cover under 3 attempts or close no loop.
func CyclicMagnitude(votes []Vote) (float64, bool) {
	type edge struct{ lo, hi string }
	flow := map[edge]float64{}
	for _, v := range votes {
		f := float64(2.0*v.Win) - 1.0
		if v.I < v.J {
			flow[edge{v.I, v.J}] += f
		} else {
			flow[edge{v.J, v.I}] += -f
		}
	}
	seen := map[string]bool{}
	for e := range flow {
		seen[e.lo], seen[e.hi] = true, true
	}
	var nodes []string
	for x := range seen {
		nodes = append(nodes, x)
	}
	sort.Strings(nodes)
	if len(nodes) < 3 {
		return 0, false
	}
	edges := make([]edge, 0, len(flow))
	for e := range flow {
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].lo != edges[j].lo {
			return edges[i].lo < edges[j].lo
		}
		return edges[i].hi < edges[j].hi
	})
	parent := map[string]string{}
	for _, x := range nodes {
		parent[x] = x
	}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != x {
			x = parent[x]
		}
		return x
	}
	for _, e := range edges {
		parent[find(e.lo)] = find(e.hi)
	}
	roots := map[string]bool{}
	for _, x := range nodes {
		roots[find(x)] = true
	}
	if len(flow)-len(nodes)+len(roots) <= 0 {
		return 0, false
	}
	idx := map[string]int{}
	for k, x := range nodes {
		idx[x] = k
	}
	n := len(nodes)
	reach := make([][]bool, n)
	for i := range reach {
		reach[i] = make([]bool, n)
	}
	type arc struct{ u, v string }
	var arcs []arc
	for _, e := range edges {
		f := flow[e]
		if f > 0 {
			arcs = append(arcs, arc{e.lo, e.hi})
		} else if f < 0 {
			arcs = append(arcs, arc{e.hi, e.lo})
		}
	}
	for _, a := range arcs {
		reach[idx[a.u]][idx[a.v]] = true
	}
	for k := 0; k < n; k++ {
		for i := 0; i < n; i++ {
			if reach[i][k] {
				for j := 0; j < n; j++ {
					if reach[k][j] {
						reach[i][j] = true
					}
				}
			}
		}
	}
	on := 0
	for _, a := range arcs {
		if reach[idx[a.v]][idx[a.u]] {
			on++
		}
	}
	return R9(float64(on) / float64(len(flow))), true
}

// BetaSample is rank.beta_sample: the alpha-th smallest of alpha+beta-1
// uniforms.
func BetaSample(alpha, beta int, rng *SplitMix64) float64 {
	us := make([]float64, alpha+beta-1)
	for i := range us {
		us[i] = float64(rng.Next()>>11) / 9007199254740992.0
	}
	sort.Float64s(us)
	return R9(us[alpha-1])
}

// Trust is one judge's record (rank.judge_trust).
type Trust struct {
	Cards             int
	Agree             float64
	HasAgree          bool
	Agreed, Disagreed int
	Tau               float64
	HasTau            bool
	CyclicRanking     []string // ranking ids, sorted
	Cyclic            map[string]float64
	CyclicKnown       map[string]bool
	Dropped           bool
	Why               string
}

// JudgeWeights is rank.judge_weights: judges in name order, a dropped judge
// has no entry.
func JudgeWeights(trust map[string]*Trust, seedHex string) map[string]float64 {
	out := map[string]float64{}
	var names []string
	for j := range trust {
		names = append(names, j)
	}
	sort.Strings(names)
	for _, j := range names {
		t := trust[j]
		if t.Dropped {
			continue
		}
		sum := sha256.Sum256([]byte(seedHex + j))
		seed, _ := strconv.ParseUint(hex.EncodeToString(sum[:])[:16], 16, 64)
		out[j] = R9(float64(2.0 * BetaSample(1+t.Agreed, 1+t.Disagreed, &SplitMix64{seed})))
	}
	return out
}

func weightsObj(w map[string]float64) *pyjson.Object {
	var names []string
	for j := range w {
		names = append(names, j)
	}
	sort.Strings(names)
	o := pyjson.NewObject()
	for _, j := range names {
		o.Set(j, w[j])
	}
	return o
}

// WeightsObj is the weights as the result carries them (judge names sorted).
func WeightsObj(w map[string]float64) *pyjson.Object { return weightsObj(w) }

func pairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "\x00" + b
}

// answerPairs is rank._answer_pairs.
func answerPairs(card *Card) []Pair {
	if card.Answer == nil {
		return nil
	}
	var surv []string
	for _, s := range card.survivors() {
		surv = append(surv, s.Value("id").(string))
	}
	chosen := card.chosen()
	if card.Answer.Value("event") == "confirmed" {
		var out []Pair
		for _, x := range surv {
			if x != chosen {
				out = append(out, Pair{chosen, x})
			}
		}
		return out
	}
	pick := card.Answer.Value("pick").(string)
	found := false
	for _, x := range surv {
		found = found || x == pick
	}
	if !found || pick == chosen {
		return nil
	}
	return []Pair{{pick, chosen}}
}

func groupScores(score map[string]float64) [][]string {
	seen := map[float64]bool{}
	var vals []float64
	for _, v := range score {
		if !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
	var out [][]string
	for _, sc := range vals {
		var g []string
		for k, v := range score {
			if v == sc {
				g = append(g, k)
			}
		}
		sort.Strings(g)
		out = append(out, g)
	}
	return out
}

// JudgeTrust is rank.judge_trust. It returns the judges' names in order and
// their records. Reads only.
func JudgeTrust(dataDir string, floor float64, minCards int) ([]string, map[string]*Trust) {
	rows, _ := readRows(filepath.Join(RankingsDir(dataDir), "comparisons.jsonl"))
	byRank := map[string][]Comparison{}
	for _, row := range rows {
		rid, ok := row.Value("ranking_id").(string)
		if !ok {
			continue
		}
		c, err := ParseComparison(row, 0)
		if err != nil {
			continue
		}
		byRank[rid] = append(byRank[rid], c)
	}
	answered := map[string]*Card{}
	for _, card := range ReadCards(dataDir) {
		if card.Answer != nil {
			rid := card.Opened.Value("ranking_id").(string)
			if _, ok := answered[rid]; !ok {
				answered[rid] = card
			}
		}
	}
	type stat struct {
		cards, good, agreed, disagreed int
		taus                           []float64
		cyclic                         map[string]float64
		cyclicKnown                    map[string]bool
	}
	stats := map[string]*stat{}
	st := func(j string) *stat {
		if stats[j] == nil {
			stats[j] = &stat{cyclic: map[string]float64{}, cyclicKnown: map[string]bool{}}
		}
		return stats[j]
	}
	var rids []string
	for rid := range byRank {
		rids = append(rids, rid)
	}
	sort.Strings(rids)
	for _, rid := range rids {
		comps := byRank[rid]
		card := answered[rid]
		byID := map[string]*Attempt{}
		if card != nil {
			for _, s := range card.survivors() {
				id := s.Value("id").(string)
				byID[id] = &Attempt{ID: id, ContentHash: s.Value("content_hash").(string)}
			}
		} else {
			for _, c := range comps {
				if byID[c.A] == nil {
					byID[c.A] = &Attempt{ID: c.A, ContentHash: c.AHash}
				}
				if byID[c.B] == nil {
					byID[c.B] = &Attempt{ID: c.B, ContentHash: c.BHash}
				}
			}
		}
		var scratch []string
		votes := VotesFrom(comps, byID, &scratch)
		judgeSet := map[string]bool{}
		for _, v := range votes {
			judgeSet[v.Judge] = true
		}
		var judges []string
		for j := range judgeSet {
			judges = append(judges, j)
		}
		sort.Strings(judges)
		mineOf := func(j string) []Vote {
			var m []Vote
			for _, v := range votes {
				if v.Judge == j {
					m = append(m, v)
				}
			}
			return m
		}
		for _, j := range judges {
			if m, ok := CyclicMagnitude(mineOf(j)); ok {
				st(j).cyclic[rid], st(j).cyclicKnown[rid] = m, true
			} else {
				st(j).cyclicKnown[rid] = false
			}
		}
		if card != nil {
			pairs := answerPairs(card)
			fixed := map[string]Pair{}
			for _, p := range pairs {
				fixed[pairKey(p.Win, p.Lose)] = p
			}
			var ownerOrder []string
			if f, ok := card.Opened.Value("facts").(*pyjson.Object); ok {
				if ord, ok := f.Value("order").([]any); ok {
					for _, x := range ord {
						if s, ok := x.(string); ok {
							ownerOrder = append(ownerOrder, s)
						}
					}
				}
			}
			var survIDs []string
			for _, s := range card.survivors() {
				survIDs = append(survIDs, s.Value("id").(string))
			}
			if len(ownerOrder) == 0 {
				ownerOrder = survIDs
			}
			if card.Answer.Value("event") == "overridden" {
				pick := card.Answer.Value("pick").(string)
				next := []string{pick}
				for _, x := range ownerOrder {
					if x != pick {
						next = append(next, x)
					}
				}
				ownerOrder = next
			}
			ownerGroups := make([][]string, 0, len(ownerOrder))
			for _, x := range ownerOrder {
				ownerGroups = append(ownerGroups, []string{x})
			}
			for _, j := range judges {
				mine := mineOf(j)
				ag, dis := 0, 0
				for _, v := range mine {
					p, ok := fixed[pairKey(v.I, v.J)]
					if !ok || v.Win == 0.5 {
						continue
					}
					winner := v.J
					if v.Win == 1.0 {
						winner = v.I
					}
					if winner == p.Win {
						ag++
					} else {
						dis++
					}
				}
				if ag+dis > 0 {
					t := st(j)
					t.cards++
					if ag > dis {
						t.good++
					}
					t.agreed += ag
					t.disagreed += dis
				}
				score := map[string]float64{}
				for _, x := range survIDs {
					score[x] = 0.0
				}
				for _, v := range mine {
					_, a := score[v.I]
					_, b := score[v.J]
					if a && b {
						score[v.I] += v.Win
						score[v.J] += 1.0 - v.Win
					}
				}
				if tau, ok := KendallTauB(groupScores(score), ownerGroups); ok && len(mine) > 0 {
					st(j).taus = append(st(j).taus, tau)
				}
			}
		} else {
			pairVotes := map[[2]string][]Vote{}
			var keys [][2]string
			for _, v := range votes {
				if v.Win != 0.5 {
					k := [2]string{v.I, v.J}
					if _, ok := pairVotes[k]; !ok {
						keys = append(keys, k)
					}
					pairVotes[k] = append(pairVotes[k], v)
				}
			}
			sort.Slice(keys, func(a, b int) bool {
				if keys[a][0] != keys[b][0] {
					return keys[a][0] < keys[b][0]
				}
				return keys[a][1] < keys[b][1]
			})
			for _, k := range keys {
				pv := pairVotes[k]
				for _, v := range pv {
					others := 0
					first := 0
					for _, o := range pv {
						if o.Judge != v.Judge {
							others++
							if o.Win == 1.0 {
								first++
							}
						}
					}
					second := others - first
					if others == 0 || first == second {
						continue
					}
					if (v.Win == 1.0) == (first > second) {
						st(v.Judge).agreed++
					} else {
						st(v.Judge).disagreed++
					}
				}
			}
		}
	}
	var names []string
	for j := range stats {
		names = append(names, j)
	}
	sort.Strings(names)
	out := map[string]*Trust{}
	for _, j := range names {
		t := stats[j]
		tr := &Trust{Cards: t.cards, Agreed: t.agreed, Disagreed: t.disagreed,
			Cyclic: t.cyclic, CyclicKnown: t.cyclicKnown}
		if t.cards > 0 {
			tr.Agree, tr.HasAgree = R9(float64(t.good)/float64(t.cards)), true
		}
		tr.Dropped = t.cards >= minCards && tr.HasAgree && tr.Agree < floor
		if tr.Dropped {
			tr.Why = fmt.Sprintf("agreement %s over %d answered cards", pyjson.FloatRepr(tr.Agree), t.cards)
		}
		if len(t.taus) > 0 {
			sum := 0.0
			for _, x := range t.taus {
				sum = sum + x
			}
			tr.Tau, tr.HasTau = R9(sum/float64(len(t.taus))), true
		}
		for rid := range t.cyclicKnown {
			tr.CyclicRanking = append(tr.CyclicRanking, rid)
		}
		sort.Strings(tr.CyclicRanking)
		out[j] = tr
	}
	return names, out
}

// WeightsFor is rank.weights_for: the weights a fit uses for judges, and the
// trust behind them. A judge with no history gets the prior; a dropped judge
// gets none.
func WeightsFor(dataDir string, judges []string, seedHex string) (map[string]float64, map[string]*Trust) {
	_, trust := JudgeTrust(dataDir, TrustFloor, TrustMinCards)
	need := map[string]*Trust{}
	for _, j := range judges {
		if t, ok := trust[j]; ok {
			need[j] = t
		} else {
			need[j] = &Trust{}
		}
	}
	return JudgeWeights(need, seedHex), trust
}

// TrustDoc is the `rank judges --json` document.
func TrustDoc(names []string, trust map[string]*Trust) *pyjson.Object {
	judges := pyjson.NewObject()
	for _, j := range names {
		t := trust[j]
		var agree, tau any
		if t.HasAgree {
			agree = t.Agree
		}
		if t.HasTau {
			tau = t.Tau
		}
		cyc := pyjson.NewObject()
		for _, rid := range t.CyclicRanking {
			if t.CyclicKnown[rid] {
				cyc.Set(rid, t.Cyclic[rid])
			} else {
				cyc.Set(rid, nil)
			}
		}
		judges.Set(j, pyjson.NewObject().Set("cards", t.Cards).Set("agree", agree).Set("agreed", t.Agreed).
			Set("disagreed", t.Disagreed).Set("tau", tau).Set("cyclic", cyc).Set("dropped", t.Dropped).
			Set("why", t.Why))
	}
	return judges
}

// TrustText is rank.judges_text.
func TrustText(names []string, trust map[string]*Trust, weights map[string]float64) string {
	if len(names) == 0 {
		return "No judge has voted yet.\n"
	}
	var b strings.Builder
	for _, j := range names {
		t := trust[j]
		agree := "none yet"
		if t.HasAgree {
			agree = pyjson.FloatRepr(t.Agree)
		}
		w := "none (dropped)"
		if v, ok := weights[j]; ok {
			w = pyjson.FloatRepr(v)
		}
		fmt.Fprintf(&b, "%s: agreement %s over %d answered cards; agreed %d, disagreed %d; weight %s\n",
			j, agree, t.Cards, t.Agreed, t.Disagreed, w)
		if t.Dropped {
			fmt.Fprintf(&b, "  dropped: %s\n", t.Why)
		}
		if t.HasTau {
			fmt.Fprintf(&b, "  tau-b with the owner's orders: %s\n", pyjson.FloatRepr(t.Tau))
		}
	}
	return b.String()
}
