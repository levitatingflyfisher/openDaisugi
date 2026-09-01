package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"regexp"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is the meter of opendaisugi.gateway (price_turn,
// measure_turn, _strictly_cheaper) and gateway_journal.record_turn.

// Price is (input, output) USD per million tokens.
type Price struct{ In, Out float64 }

// Prices is a price table; its keys are model ids.
type Prices map[string]Price

// DefaultPrices is _PRICES_PER_MTOK.
func DefaultPrices() Prices {
	return Prices{
		"claude-opus-4-8":  {15.0, 75.0},
		"claude-sonnet-5":  {3.0, 15.0},
		"claude-haiku-4-5": {1.0, 5.0},
	}
}

var fallbackPrice = Price{3.0, 15.0}

const (
	cacheReadMult  = 0.1
	cacheWriteMult = 1.25
)

// lookup is prices.get(model, fallback): an unhashable model raises.
func (p Prices) lookup(model any) (Price, error) {
	if !hashable(model) {
		return Price{}, errRaise
	}
	if s, ok := model.(string); ok {
		if pr, has := p[s]; has {
			return pr, nil
		}
	}
	return fallbackPrice, nil
}

// has is `model in prices`: an unhashable model raises.
func (p Prices) has(model any) (bool, error) {
	if !hashable(model) {
		return false, errRaise
	}
	s, ok := model.(string)
	if !ok {
		return false, nil
	}
	_, has := p[s]
	return has, nil
}

// StrictlyCheaper is _strictly_cheaper: both models priced and the
// served one cheaper on input and on output.
func (p Prices) StrictlyCheaper(served, requested any) (bool, error) {
	hs, err := p.has(served)
	if err != nil {
		return false, err
	}
	if !hs {
		return false, nil
	}
	hr, err := p.has(requested)
	if err != nil {
		return false, err
	}
	if !hr {
		return false, nil
	}
	s, r := p[served.(string)], p[requested.(string)]
	return s.In < r.In && s.Out < r.Out, nil
}

// Cost is TurnCost.
type Cost struct {
	Model                             any
	In, Out, CacheRead, CacheCreation *big.Int
	Dollars                           float64
}

// priceTurn is price_turn: every bucket priced, in Python's order of
// operations.
func (p Prices) priceTurn(model any, in, out, cr, cc *big.Int) (Cost, error) {
	pr, err := p.lookup(model)
	if err != nil {
		return Cost{}, err
	}
	fin, err := intToFloat(in)
	if err != nil {
		return Cost{}, err
	}
	fcr, err := intToFloat(cr)
	if err != nil {
		return Cost{}, err
	}
	fcc, err := intToFloat(cc)
	if err != nil {
		return Cost{}, err
	}
	fout, err := intToFloat(out)
	if err != nil {
		return Cost{}, err
	}
	// Each product is rounded on its own, as Python rounds it: the
	// conversions keep the compiler from fusing a multiply and an add.
	a := float64(fin * pr.In)
	b := float64(float64(fcr*pr.In) * cacheReadMult)
	c := float64(float64(fcc*pr.In) * cacheWriteMult)
	d := float64(fout * pr.Out)
	dollars := float64(float64(float64(a+b)+c)+d) / 1_000_000
	return Cost{Model: model, In: in, Out: out, CacheRead: cr, CacheCreation: cc, Dollars: dollars}, nil
}

// Saving is TurnSaving.
type Saving struct {
	Actual, Counterfactual Cost
	Estimated              bool
}

// FrontierTokensSaved is frontier_tokens_saved: the whole turn when the
// served model differs from the counterfactual's.
func (s Saving) FrontierTokensSaved() *big.Int {
	n := new(big.Int)
	if !pyEqual(s.Actual.Model, s.Counterfactual.Model) {
		n.Add(n, s.Actual.In).Add(n, s.Actual.CacheRead).Add(n, s.Actual.CacheCreation).Add(n, s.Actual.Out)
	}
	return n
}

// measure is measure_turn: the usage block's four buckets, int() each.
func (p Prices) measure(d Decision, usage any) (Saving, error) {
	ints := make([]*big.Int, 4)
	for i, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		v, err := get(usage, k, pyjson.Int{Text: "0"})
		if err != nil {
			return Saving{}, err
		}
		n, err := pyInt(v)
		if err != nil {
			return Saving{}, err
		}
		ints[i] = n
	}
	actual, err := p.priceTurn(d.Model, ints[0], ints[1], ints[2], ints[3])
	if err != nil {
		return Saving{}, err
	}
	if !d.Downgraded {
		return Saving{Actual: actual, Counterfactual: actual}, nil
	}
	cf, err := p.priceTurn(d.RequestedModel, ints[0], ints[1], ints[2], ints[3])
	if err != nil {
		return Saving{}, err
	}
	return Saving{Actual: actual, Counterfactual: cf, Estimated: true}, nil
}

var whitespaceRun = regexp.MustCompile(`[\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]+`)

// TurnSignature is turn_signature: SHA-256 of the stripped, lowered,
// whitespace-collapsed ask, 16 hex digits. A lone surrogate raises.
func TurnSignature(task string) (string, error) {
	norm := whitespaceRun.ReplaceAllString(pystr.Lower(pystr.Strip(task)), " ")
	b, exc := pystr.EncodeUTF8(norm)
	if exc != nil {
		return "", errRaise
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16], nil
}

// Record is GatewayTurnRecord, as the journal line holds it.
type Record struct {
	CreatedAt      string
	Signature      string
	Task           string
	Tier           string
	RequestedModel any
	Model          any
	Difficulty     float64
	Downgraded     bool
	Estimated      bool
	In, Out        *big.Int
	FrontierSaved  *big.Int
	Actual         float64
	Counterfactual float64
	CacheRead      *big.Int
	CacheCreation  *big.Int
	// Elapsed is elapsed_ms: the turn's time in the proxy, or nil.
	Elapsed *float64
}

// recordTurn is record_turn with created_at stamped now (UTC seconds).
func recordTurn(d Decision, s Saving, task, ask string, now time.Time) (Record, error) {
	sig := ""
	if pystr.Strip(ask) != "" {
		var err error
		if sig, err = TurnSignature(ask); err != nil {
			return Record{}, err
		}
	}
	return Record{
		CreatedAt: now.UTC().Format("2006-01-02T15:04:05Z"), Signature: sig, Task: task, Tier: d.Tier,
		RequestedModel: d.RequestedModel, Model: d.Model, Difficulty: d.Difficulty, Downgraded: d.Downgraded,
		Estimated: s.Estimated, In: s.Actual.In, Out: s.Actual.Out, FrontierSaved: s.FrontierTokensSaved(),
		Actual: s.Actual.Dollars, Counterfactual: s.Counterfactual.Dollars,
		CacheRead: s.Actual.CacheRead, CacheCreation: s.Actual.CacheCreation,
	}, nil
}

// JSON is json.dumps(asdict(record)).
func (r Record) JSON() string {
	o := pyjson.NewObjectCap(16)
	o.Set("created_at", r.CreatedAt).Set("signature", r.Signature).Set("task", r.Task).Set("tier", r.Tier)
	o.Set("requested_model", r.RequestedModel).Set("model", r.Model).Set("difficulty", r.Difficulty)
	o.Set("downgraded", r.Downgraded).Set("estimated", r.Estimated)
	o.Set("input_tokens", bigInt(r.In)).Set("output_tokens", bigInt(r.Out))
	o.Set("frontier_tokens_saved", bigInt(r.FrontierSaved))
	o.Set("actual_dollars", r.Actual).Set("counterfactual_dollars", r.Counterfactual)
	o.Set("cache_read_tokens", bigInt(r.CacheRead)).Set("cache_creation_tokens", bigInt(r.CacheCreation))
	if r.Elapsed != nil {
		o.Set("elapsed_ms", pyjson.Float(*r.Elapsed))
	} else {
		o.Set("elapsed_ms", nil)
	}
	return pyjson.Dumps(o, true)
}

func bigInt(n *big.Int) pyjson.Int { return pyjson.Int{Text: n.String()} }
