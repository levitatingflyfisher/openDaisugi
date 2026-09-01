package gateway

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is gateway_journal.GatewayJournal.load and summarize, and
// gateway_report's calibration report and target-share table.

// ErrJournal marks a journal this binary does not read the way the
// oracle does: a record whose values are not the types the gateway
// writes, or a line json.loads raises on (ruled GW-8).
var ErrJournal = errors.New("the gateway journal holds a record this binary does not read")

// ErrUnreadable marks, beside ErrJournal, a journal the oracle raises on
// as it reads it: bytes that are not UTF-8, or a line json.loads raises
// on with other than JSONDecodeError.
var ErrUnreadable = errors.New("the gateway journal cannot be read")

var journalRequired = []string{"created_at", "signature", "task", "tier", "requested_model", "model", "difficulty",
	"downgraded", "estimated", "input_tokens", "output_tokens", "frontier_tokens_saved", "actual_dollars",
	"counterfactual_dollars"}

// JRecord is a loaded GatewayTurnRecord.
type JRecord struct {
	Signature, Task, Tier string
	Model                 any
	Downgraded            bool
	In, Out, CR, CC       *big.Int
	Saved                 *big.Int
	Actual, CF            float64
}

// LoadJournal is GatewayJournal(path).load().
func LoadJournal(path string) ([]JRecord, error) {
	objs, err := loadRecords(path, journalRequired)
	if err != nil {
		if errors.Is(err, errUnreadable) {
			return nil, fmt.Errorf("%w: %w: a line json.loads raises on", ErrJournal, ErrUnreadable)
		}
		return nil, err
	}
	out := make([]JRecord, 0, len(objs))
	for _, o := range objs {
		var r JRecord
		var ok1, ok2, ok3, ok4 bool
		r.Signature, ok1 = o.Value("signature").(string)
		r.Task, ok2 = o.Value("task").(string)
		r.Tier, ok3 = o.Value("tier").(string)
		r.Downgraded, ok4 = o.Value("downgraded").(bool)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return nil, fmt.Errorf("%w: signature, task, tier or downgraded", ErrJournal)
		}
		r.Model = o.Value("model")
		ints := []**big.Int{&r.In, &r.Out, &r.Saved, &r.CR, &r.CC}
		for i, k := range []string{"input_tokens", "output_tokens", "frontier_tokens_saved", "cache_read_tokens",
			"cache_creation_tokens"} {
			v, has := o.Get(k)
			if !has {
				*ints[i] = new(big.Int)
				continue
			}
			if !isPyInt(v) {
				return nil, fmt.Errorf("%w: %s is not an int", ErrJournal, k)
			}
			*ints[i], _ = pyInt(v)
		}
		a, ok1 := o.Value("actual_dollars").(pyjson.Float)
		c, ok2 := o.Value("counterfactual_dollars").(pyjson.Float)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%w: a dollar figure is not a float", ErrJournal)
		}
		r.Actual, r.CF = float64(a), float64(c)
		out = append(out, r)
	}
	return out, nil
}

// Summary is GatewaySummary, less the repeat groups.
type Summary struct {
	Turns, Downgraded, LocalTurns int
	FrontierIn, FrontierOut       *big.Int
	FrontierSaved                 *big.Int
	DollarsSaved, Blended         float64
	CacheRead, CacheCreation      *big.Int
	CacheHitRate                  float64
	totalActual, totalCF          float64
}

// Summarize is gateway_journal.summarize.
func Summarize(rs []JRecord) Summary {
	s := Summary{Turns: len(rs), FrontierIn: new(big.Int), FrontierOut: new(big.Int), CacheRead: new(big.Int),
		CacheCreation: new(big.Int)}
	var actual, cf []float64
	all := new(big.Int)
	for _, r := range rs {
		if r.Downgraded {
			s.Downgraded++
			s.FrontierIn.Add(s.FrontierIn, r.In).Add(s.FrontierIn, r.CR).Add(s.FrontierIn, r.CC)
			s.FrontierOut.Add(s.FrontierOut, r.Out)
		}
		actual = append(actual, r.Actual)
		cf = append(cf, r.CF)
		s.CacheRead.Add(s.CacheRead, r.CR)
		s.CacheCreation.Add(s.CacheCreation, r.CC)
		all.Add(all, r.In).Add(all, r.CR).Add(all, r.CC)
		if r.Tier == "tier1-local" {
			s.LocalTurns++
		}
	}
	s.totalActual, s.totalCF = pyjson.SumFloats(actual), pyjson.SumFloats(cf)
	s.DollarsSaved = s.totalCF - s.totalActual
	s.Blended = 1.0
	if s.totalActual > 0 {
		s.Blended = s.totalCF / s.totalActual
	}
	if all.Sign() != 0 {
		s.CacheHitRate = trueDiv(s.CacheRead, all)
	}
	s.FrontierSaved = new(big.Int).Add(s.FrontierIn, s.FrontierOut)
	return s
}

// Candidate is the part of a reuse candidate the report reads.
type Candidate struct {
	Count   int
	Tokens  *big.Int
	Dollars float64
}

// Report is CalibrationReport.
type Report struct {
	Summary
	RepeatClusters     int
	RecoverableTokens  *big.Int
	RecoverableDollars float64
	CombinedSaved      *big.Int
	CombinedMultiplier float64
}

// BuildReport is build_report, given the ranked reuse candidates.
func BuildReport(rs []JRecord, cands []Candidate) Report {
	s := Summarize(rs)
	rep := Report{Summary: s, RepeatClusters: len(cands), RecoverableTokens: new(big.Int)}
	for _, c := range cands {
		if c.Count <= 1 {
			continue
		}
		n := big.NewInt(int64(c.Count))
		num := new(big.Int).Mul(c.Tokens, big.NewInt(int64(c.Count-1)))
		f := trueDiv(num, n)
		t, _ := new(big.Float).SetFloat64(math.Trunc(f)).Int(nil)
		rep.RecoverableTokens.Add(rep.RecoverableTokens, t)
		rep.RecoverableDollars += float64(c.Dollars*float64(c.Count-1)) / float64(c.Count)
	}
	rep.CombinedSaved = new(big.Int).Add(s.FrontierSaved, rep.RecoverableTokens)
	rep.CombinedMultiplier = 1.0
	if s.totalActual > 0 {
		d := s.totalActual - rep.RecoverableDollars
		if 1e-9 > d {
			d = 1e-9
		}
		rep.CombinedMultiplier = s.totalCF / d
	}
	return rep
}

// Share is TargetShare.
type Share struct {
	Model          string
	Turns          int
	Share          float64
	In, Out, Saved *big.Int
}

// ExternalRecords is the switchyard-tier records; their model must be a
// str, as the gateway writes it.
func ExternalRecords(rs []JRecord) ([]JRecord, error) {
	var out []JRecord
	for _, r := range rs {
		if r.Tier != ExternalTier {
			continue
		}
		if _, ok := r.Model.(string); !ok {
			return nil, fmt.Errorf("%w: a switchyard turn's model is not a str", ErrJournal)
		}
		out = append(out, r)
	}
	return out, nil
}

// ShareTable is build_target_share_table.
func ShareTable(rs []JRecord) ([]Share, error) {
	ext, err := ExternalRecords(rs)
	if err != nil || len(ext) == 0 {
		return nil, err
	}
	by := map[string]*Share{}
	var order []string
	for _, r := range ext {
		m := r.Model.(string)
		sh, ok := by[m]
		if !ok {
			sh = &Share{Model: m, In: new(big.Int), Out: new(big.Int), Saved: new(big.Int)}
			by[m] = sh
			order = append(order, m)
		}
		sh.Turns++
		sh.In.Add(sh.In, r.In)
		sh.Out.Add(sh.Out, r.Out)
		sh.Saved.Add(sh.Saved, r.Saved)
	}
	out := make([]Share, 0, len(order))
	for _, m := range order {
		sh := by[m]
		sh.Share = float64(sh.Turns) / float64(len(ext))
		out = append(out, *sh)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Turns != out[j].Turns {
			return out[i].Turns > out[j].Turns
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// padL and padR pad by code points, as format() does.
func padR(s string, w int) string {
	if n := pystr.Len(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func padL(s string, w int) string {
	if n := pystr.Len(s); n < w {
		return strings.Repeat(" ", w-n) + s
	}
	return s
}

// Commas is format(n, ",").
func Commas(n *big.Int) string {
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

// FormatF is format(x, ".Nf").
func FormatF(x float64, n int) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	return fmt.Sprintf("%.*f", n, x)
}

// Percent is format(x, ".1%").
func Percent(x float64) string { return FormatF(x*100, 1) + "%" }

// FormatShareTable is format_target_share_table.
func FormatShareTable(table []Share) []string {
	w := len("target")
	for _, r := range table {
		w = max(w, pystr.Len(r.Model))
	}
	lines := []string{"  " + padR("target", w) + "  " + padL("turns", 6) + "  " + padL("share", 6) + "  " +
		padL("input", 10) + "  " + padL("output", 9) + "  " + padL("saved", 10)}
	for _, r := range table {
		lines = append(lines, "  "+padR(r.Model, w)+"  "+padL(fmt.Sprint(r.Turns), 6)+"  "+padL(Percent(r.Share), 6)+
			"  "+padL(Commas(r.In), 10)+"  "+padL(Commas(r.Out), 9)+"  "+padL(Commas(r.Saved), 10))
	}
	return lines
}

// PadRepr is format(repr(s), "62") and the like: repr padded to w.
func PadRepr(s string, w int) string { return padR(pystr.Repr(s), w) }
