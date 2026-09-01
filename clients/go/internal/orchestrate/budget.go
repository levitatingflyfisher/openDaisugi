package orchestrate

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"daisugi-verify/internal/pyjson"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// approxUSDPerMTok is budget.APPROX_USD_PER_MTOK, in its order.
var approxUSDPerMTok = []struct {
	key   string
	price float64
}{{"opus", 22.0}, {"sonnet", 6.0}, {"haiku", 1.5}}

func pricePerMTok(model string) float64 {
	m := strings.ToLower(model)
	for _, p := range approxUSDPerMTok {
		if strings.Contains(m, p.key) {
			return p.price
		}
	}
	return 0
}

type stepCost struct {
	stepID, model string
	tokens        int64
	cost          *float64
}

// Tracker is budget.BudgetTracker.
type Tracker struct {
	Total  *int64
	Strict bool
	mu     sync.Mutex
	spent  int64
	costs  []stepCost
}

// Remaining is BudgetTracker.remaining: +Inf when unlimited.
func (t *Tracker) Remaining() float64 {
	if t.Total == nil {
		return math.Inf(1)
	}
	return math.Max(0, float64(*t.Total-t.spent))
}

// Exhausted is BudgetTracker.exhausted.
func (t *Tracker) Exhausted() bool { return t.Total != nil && t.Remaining() <= 0 }

// ErrBudgetExceeded is budget.BudgetExceeded.
type ErrBudgetExceeded struct{ Msg string }

func (e *ErrBudgetExceeded) Error() string { return e.Msg }

// Record is BudgetTracker.record: the spend counted first, then in strict
// mode an overrun raised.
func (t *Tracker) Record(stepID, model string, tokens int64, cost *float64) error {
	if tokens < 0 {
		return fmt.Errorf("token count must be non-negative, got %d", tokens)
	}
	t.mu.Lock()
	t.spent += tokens
	t.costs = append(t.costs, stepCost{stepID, model, tokens, cost})
	over := t.Strict && t.Total != nil && t.spent > *t.Total
	spent := t.spent
	t.mu.Unlock()
	if over {
		return &ErrBudgetExceeded{fmt.Sprintf("recording %d tokens for step '%s' pushed spend to %d > budget %d",
			tokens, stepID, spent, *t.Total)}
	}
	return nil
}

// Report is budget.BudgetReport.
type Report struct {
	Total     *int64
	Spent     int64
	Remaining *int64
	StepCount int
	ByModel   *pyjson.Object // model -> Int
	byModel   []string
	byTokens  map[string]int64
	Approx    any // Int 0 with no spend, else a float
	ApproxF   float64
	Measured  *float64
}

// Report is BudgetTracker.report.
func (t *Tracker) Report() Report {
	r := Report{Total: t.Total, Spent: t.spent, StepCount: len(t.costs), byTokens: map[string]int64{}}
	if t.Total != nil {
		rem := int64(t.Remaining())
		r.Remaining = &rem
	}
	for _, c := range t.costs {
		if _, seen := r.byTokens[c.model]; !seen {
			r.byModel = append(r.byModel, c.model)
		}
		r.byTokens[c.model] += c.tokens
	}
	r.ByModel = pyjson.NewObject()
	for _, m := range r.byModel {
		r.ByModel.Set(m, pyjson.Int{Text: itoa(r.byTokens[m])})
	}
	if len(r.byModel) == 0 {
		// sum() of nothing is the int 0, and round keeps it an int.
		r.Approx = pyjson.Int{Text: "0"}
	} else {
		var terms []float64
		for _, m := range r.byModel {
			terms = append(terms, pricePerMTok(m)*float64(r.byTokens[m])/1_000_000)
		}
		r.ApproxF = pyjson.Round(pyjson.SumFloats(terms), 4)
		r.Approx = r.ApproxF
	}
	var measured []float64
	for _, c := range t.costs {
		if c.cost != nil {
			measured = append(measured, *c.cost)
		}
	}
	if len(measured) > 0 {
		m := pyjson.Round(pyjson.SumFloats(measured), 6)
		r.Measured = &m
	}
	return r
}

// Dump is dataclasses.asdict(report).
func (r Report) Dump() *pyjson.Object {
	var total, rem, measured any
	if r.Total != nil {
		total = pyjson.Int{Text: itoa(*r.Total)}
	}
	if r.Remaining != nil {
		rem = pyjson.Int{Text: itoa(*r.Remaining)}
	}
	if r.Measured != nil {
		measured = *r.Measured
	}
	return pyjson.NewObject().Set("total", total).Set("spent", pyjson.Int{Text: itoa(r.Spent)}).
		Set("remaining", rem).Set("step_count", pyjson.Int{Text: strconv.Itoa(r.StepCount)}).
		Set("by_model", r.ByModel).Set("approx_cost_usd", r.Approx).Set("measured_cost_usd", measured)
}
