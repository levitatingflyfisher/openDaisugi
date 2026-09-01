// Package recall is the assured reuse a harness opts into (ADR-0012 §2C,
// §2D): gateway_recall.recall and gateway_answers.recall_answer. Neither
// runs inside the proxy: the oracle's gateway only captures answers, and
// a harness reaches these through its MCP tools. They are here so the Go
// side has the same library, proven on the oracle's cases.
//
// recall fails closed: a reused plan is served only after it verifies
// against the caller's envelope in the linked Z3, and a Z3 check that
// answered unknown is a miss here (the oracle's lenient verify would
// keep it as a warning; GW-11). A typed pathway's holes are bound with
// one model call (envgen.Bind, pathway_bind); with no model, or on any
// failure, it takes the frozen template, as the oracle does.
package recall

import (
	"encoding/json"
	"errors"
	"fmt"

	"daisugi-verify/internal/distill"
	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/garden"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

// Provenance is RecallProvenance.
type Provenance struct {
	PathwayID        string
	Similarity       float64
	Tier             string
	SourceTraceCount int
	DistilledAt      any
	HitCount         any
}

// Result is RecallResult.
type Result struct {
	Hit        bool
	Reason     string
	Plan       any // the plan as model_dump(mode="json") gives it
	Provenance *Provenance
}

// DefaultModel is gateway_recall._DEFAULT_MODEL, the model that binds.
const DefaultModel = "anthropic/claude-sonnet-4-20250514"

// Recall is gateway_recall.recall for a store and the caller's envelope.
// c is the model client that binds a typed pathway (nil: no model).
func Recall(store *pathways.Store, matcherKey string, pe potion.Env, task string, envelope json.RawMessage, z3ms int,
	c *llm.Client, model string) (Result, error) {
	env, err := verify.ParseEnvelope(envelope)
	if err != nil {
		return Result{}, fmt.Errorf("the caller's envelope does not parse: %w", err)
	}
	r, err := store.Find(task, matcherKey, pe, -1)
	if err != nil {
		return Result{}, err
	}
	if r.Match == nil {
		return Result{Reason: "no matching pathway"}, nil
	}
	p := r.Match.Pathway
	tier := "frozen"
	var tmpl any = p.Obj.Value("plan_template")
	if params, ok := p.Obj.Value("parameters").([]any); ok && len(params) > 0 {
		tier = "typed"
		tmpl = envgen.Bind(c, p.Obj, task, env, model, z3ms)
	}
	if _, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(tmpl))); err != nil {
		return Result{}, fmt.Errorf("%w: %v", pathways.ErrUnreadable, err)
	}
	if !envgen.PlanVerifies(tmpl.(*pyjson.Object), env, z3ms) {
		return Result{Reason: "reuse failed verification against your envelope"}, nil
	}
	srcs, _ := p.Obj.Value("source_trace_ids").([]any)
	return Result{Hit: true, Plan: tmpl, Provenance: &Provenance{
		PathwayID: p.ID(), Similarity: r.Match.Similarity, Tier: tier, SourceTraceCount: len(srcs),
		DistilledAt: p.Obj.Value("distilled_at"), HitCount: p.Obj.Value("hit_count")}}, nil
}

// DefaultMaxAge is DEFAULT_ANSWER_MAX_AGE_SECONDS: seven days.
const DefaultMaxAge = 7 * 24 * 3600.0

// AnswerProvenance is gateway_answers.AnswerProvenance.
type AnswerProvenance struct {
	Similarity, AgeSeconds float64
	CreatedAt, GroundHash  any
}

// AnswerResult is gateway_answers.AnswerResult.
type AnswerResult struct {
	Hit        bool
	Reason     string
	Answer     any
	Provenance *AnswerProvenance
}

// ErrAnswers is a store whose values the freshness gates cannot read the
// way the oracle reads them (a task or time that is not the type the
// gateway writes).
var ErrAnswers = errors.New("the answer store holds an entry this binary does not read")

func floatOf(v any) (float64, bool) {
	switch x := v.(type) {
	case pyjson.Float:
		return float64(x), true
	case float64:
		return x, true
	case pyjson.Int:
		var f float64
		_, err := fmt.Sscan(x.Text, &f)
		return f, err == nil
	}
	return 0, false
}

// RecallAnswer is recall_answer: the nearest past answer by the ask, served
// only when it clears confidence, age and ground-shift.
func RecallAnswer(task string, entries []gateway.AnswerEntry, now float64, emb pathways.Embedder, threshold, maxAge float64,
	ground *string) (AnswerResult, error) {
	var cands []gateway.AnswerEntry
	for _, e := range entries {
		if pyjson.Truthy(e.Signature) && pyjson.Truthy(e.Answer) {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return AnswerResult{Reason: "no answers in store"}, nil
	}
	q := emb.Encode(distill.NormalizeTask(task))
	best, bestSim := -1, -1.0
	for i, c := range cands {
		t, ok := c.Task.(string)
		if !ok {
			return AnswerResult{}, ErrAnswers
		}
		if sim := garden.Cosine(q, emb.Encode(distill.NormalizeTask(t))); sim > bestSim {
			best, bestSim = i, sim
		}
	}
	e := cands[best]
	if bestSim < threshold {
		return AnswerResult{Reason: "no sufficiently similar past answer"}, nil
	}
	created, ok := floatOf(e.CreatedAt)
	if !ok {
		return AnswerResult{}, ErrAnswers
	}
	age := now - created
	if age > maxAge {
		return AnswerResult{Reason: "cached answer too old"}, nil
	}
	if ground != nil && e.GroundHash != nil {
		if g, ok := e.GroundHash.(string); !ok || g != *ground {
			return AnswerResult{Reason: "the answer's ground has changed"}, nil
		}
	}
	return AnswerResult{Hit: true, Answer: e.Answer, Provenance: &AnswerProvenance{Similarity: bestSim, AgeSeconds: age,
		CreatedAt: e.CreatedAt, GroundHash: e.GroundHash}}, nil
}
