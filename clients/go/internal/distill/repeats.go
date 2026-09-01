package distill

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/garden"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is gateway_journal.load, gateway_cluster.cluster_repeats and
// gateway_distill.rank_reuse_candidates: the ranked reuse worklist
// `distill-repeats` prints.

// Turn is the part of a GatewayTurnRecord the worklist reads.
type Turn struct {
	Signature, Task string
	Tokens          *big.Int
	Dollars         float64
}

var turnFields = []string{"created_at", "signature", "task", "tier", "requested_model", "model", "difficulty",
	"downgraded", "estimated", "input_tokens", "output_tokens", "frontier_tokens_saved", "actual_dollars",
	"counterfactual_dollars", "cache_read_tokens", "cache_creation_tokens"}

var requiredTurnFields = turnFields[:14]

// LoadTurns is GatewayJournal(path).load(): each line a record, a line
// that is not JSON or not a record skipped (and counted). A record whose
// values the worklist cannot add up the Python way is ErrUnreadable.
func LoadTurns(path string) (turns []Turn, skipped int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	if !utf8.Valid(raw) {
		return nil, 0, fmt.Errorf("%w: the gateway journal is not UTF-8", ErrUnreadable)
	}
	for _, line := range pystr.Splitlines(string(raw)) {
		line = pystr.Strip(line)
		if line == "" {
			continue
		}
		v, derr := pyjson.LoadsPy(line, 900)
		if derr != nil {
			skipped++
			continue
		}
		o, ok := v.(*pyjson.Object)
		if !ok {
			skipped++
			continue
		}
		missing := false
		for _, f := range requiredTurnFields {
			if _, has := o.Get(f); !has {
				missing = true
			}
		}
		if missing {
			skipped++
			continue
		}
		t, err := turnOf(o)
		if err != nil {
			return nil, 0, err
		}
		turns = append(turns, t)
	}
	return turns, skipped, nil
}

func turnOf(o *pyjson.Object) (Turn, error) {
	bad := func(what string) (Turn, error) {
		return Turn{}, fmt.Errorf("%w: a gateway turn's %s is not the type the worklist adds up", ErrUnreadable, what)
	}
	sig, ok1 := o.Value("signature").(string)
	task, ok2 := o.Value("task").(string)
	if !ok1 || !ok2 {
		return bad("signature or task")
	}
	tokens := new(big.Int)
	for _, f := range []string{"input_tokens", "cache_read_tokens", "cache_creation_tokens", "output_tokens"} {
		v, has := o.Get(f)
		if !has {
			continue
		}
		switch x := v.(type) {
		case pyjson.Int:
			n, _ := new(big.Int).SetString(x.Text, 10)
			tokens.Add(tokens, n)
		case bool:
			if x {
				tokens.Add(tokens, big.NewInt(1))
			}
		default:
			return bad(f)
		}
	}
	var dollars float64
	switch x := o.Value("actual_dollars").(type) {
	case pyjson.Float:
		dollars = float64(x)
	case pyjson.Int:
		f, _ := new(big.Float).SetInt(mustInt(x)).Float64()
		dollars = f
	default:
		return bad("actual_dollars")
	}
	return Turn{Signature: sig, Task: task, Tokens: tokens, Dollars: dollars}, nil
}

func mustInt(x pyjson.Int) *big.Int {
	n, _ := new(big.Int).SetString(x.Text, 10)
	return n
}

// Candidate is a ReuseCandidate with its cluster.
type Candidate struct {
	Representative string
	Count          int
	Tokens         *big.Int
	Dollars        float64
	Reusable       bool
}

// RankReuse is rank_reuse_candidates(records, pathway_store=find): the
// repeat clusters of the signed turns, ranked by tokens then dollars.
func RankReuse(turns []Turn, emb pathways.Embedder, threshold float64, find func(task string) bool) []Candidate {
	var signed []Turn
	for _, t := range turns {
		if t.Signature != "" {
			signed = append(signed, t)
		}
	}
	if len(signed) == 0 {
		return nil
	}
	counts := map[string]int{}
	taskBySig := map[string]string{}
	var distinct []string
	for _, t := range signed {
		if counts[t.Signature] == 0 {
			distinct = append(distinct, t.Signature)
		}
		counts[t.Signature]++
		taskBySig[t.Signature] = t.Task
	}
	vec := map[string][]float64{}
	for _, s := range distinct {
		vec[s] = emb.Encode(NormalizeTask(taskBySig[s]))
	}
	var members [][]string
	var centroids [][]float64
	for _, s := range distinct {
		v := vec[s]
		best, bestSim := -1, -1.0
		for i, c := range centroids {
			if sim := garden.Cosine(v, c); sim > bestSim {
				best, bestSim = i, sim
			}
		}
		if best >= 0 && bestSim >= threshold {
			members[best] = append(members[best], s)
			c := make([]float64, len(v))
			for _, m := range members[best] {
				for j, x := range vec[m] {
					c[j] += x
				}
			}
			for j := range c {
				c[j] /= float64(len(members[best]))
			}
			centroids[best] = c
		} else {
			members = append(members, []string{s})
			centroids = append(centroids, append([]float64{}, v...))
		}
	}
	type cl struct {
		rep   string
		sigs  []string
		count int
	}
	var clusters []cl
	for _, ms := range members {
		total := 0
		for _, s := range ms {
			total += counts[s]
		}
		if total <= 1 {
			continue
		}
		rep := ms[0]
		for _, s := range ms[1:] {
			if counts[s] > counts[rep] {
				rep = s
			}
		}
		clusters = append(clusters, cl{taskBySig[rep], ms, total})
	}
	sort.SliceStable(clusters, func(i, j int) bool { return clusters[i].count > clusters[j].count })
	bySig := map[string][]Turn{}
	for _, t := range turns {
		if t.Signature != "" {
			bySig[t.Signature] = append(bySig[t.Signature], t)
		}
	}
	var out []Candidate
	for _, c := range clusters {
		tokens := new(big.Int)
		dollars := 0.0
		for _, s := range c.sigs {
			for _, t := range bySig[s] {
				tokens.Add(tokens, t.Tokens)
			}
		}
		for _, s := range c.sigs {
			for _, t := range bySig[s] {
				dollars += t.Dollars
			}
		}
		out = append(out, Candidate{Representative: c.rep, Count: c.count, Tokens: tokens, Dollars: dollars,
			Reusable: find != nil && find(c.rep)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := out[i].Tokens.Cmp(out[j].Tokens); c != 0 {
			return c > 0
		}
		return out[i].Dollars > out[j].Dollars
	})
	return out
}

// Row is one printed worklist line.
func (c Candidate) Row(rank int) string {
	task := strings.Join(pystr.Split(c.Representative), " ")
	if pystr.Len(task) > 70 {
		task = pystr.Slice(task, 0, 67) + "..."
	}
	reusable := "no"
	if c.Reusable {
		reusable = "yes"
	}
	return fmt.Sprintf("%4d  %4d  %10s  $%8s  %9s  %s", rank, c.Count, commas(c.Tokens), dollarText(c.Dollars), reusable, task)
}

// commas is format(n, ",").
func commas(n *big.Int) string {
	s := n.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func dollarText(x float64) string {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return garden.FormatF(x, 2)
	}
	return fmt.Sprintf("%.2f", x)
}
