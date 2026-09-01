// Package strata is the oracle's rationale-durability ledger
// (opendaisugi/strata.py): a typed store of facts, hypotheses, constraints
// and goals kept outside the transcript, a lossy reconstruction a harness
// calls after a compaction, and the one path from the store to authority,
// promote_constraint, which may only tighten the envelope.
package strata

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/signing"
	"daisugi-verify/internal/verify"
)

var kinds = []string{"fact", "hypothesis", "constraint", "goal"}
var statuses = []string{"open", "ruled_out", "resolved", "promoted"}

func newID() any {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "stratum_" + hex.EncodeToString(b)
}

// Model is strata.Stratum.
var Model = &pmodel.Model{Name: "Stratum", Fields: []pmodel.Field{
	{Name: "id", Schema: pmodel.Str{}, Default: newID},
	{Name: "kind", Schema: pmodel.Literal{Choices: kinds}, Required: true},
	{Name: "content", Schema: pmodel.Str{}, Required: true},
	{Name: "provenance", Schema: pmodel.Str{}, Default: func() any { return "" }},
	{Name: "status", Schema: pmodel.Literal{Choices: statuses}, Default: func() any { return "open" }},
	{Name: "tags", Schema: pmodel.List{Elem: pmodel.Str{}}, Default: func() any { return []any{} }},
	{Name: "pinned", Schema: pmodel.Bool{}, Default: func() any { return false }},
	{Name: "seq", Schema: pmodel.Int{}, Default: func() any { return pyjson.Int{Text: "0"} }},
}}

// stateModel is strata._StoreState.
var stateModel = &pmodel.Model{Name: "_StoreState", Fields: []pmodel.Field{
	{Name: "seq", Schema: pmodel.Int{}, Required: true},
	{Name: "strata", Schema: pmodel.List{Elem: Model}, Required: true},
}}

// Store is StrataStore. Each stratum is its model dump, in field order;
// a status set later is kept as given (pydantic does not validate an
// assignment).
type Store struct {
	strata []*pyjson.Object
	seq    int64
}

// ValidationError is pydantic's error for a stratum or a store state.
type ValidationError = pmodel.ValidationError

// Emit is emit(): the sequence number is taken first, so an emit whose
// stratum does not validate still uses one.
func (s *Store) Emit(kind, content, provenance, status any, tags any, pinned any) (*pyjson.Object, error) {
	s.seq++
	var tagList any = []any{}
	switch t := tags.(type) {
	case []any:
		tagList = t
	case string:
		var chars []any
		for _, r := range pystr.Runes(t) {
			chars = append(chars, string(r))
		}
		if chars != nil {
			tagList = chars
		}
	}
	in := pyjson.NewObject().Set("kind", kind).Set("content", content).Set("provenance", provenance).
		Set("status", status).Set("tags", tagList).Set("pinned", pinned).
		Set("seq", pyjson.Int{Text: fmt.Sprint(s.seq)})
	out, err := pmodel.Validate("Stratum", Model, in, pmodel.Python)
	if err != nil {
		return nil, err
	}
	o := out.(*pyjson.Object)
	s.strata = append(s.strata, o)
	return o, nil
}

// Get is get(): the first stratum with the id, or nil.
func (s *Store) Get(id string) *pyjson.Object {
	for _, x := range s.strata {
		if x.Value("id") == id {
			return x
		}
	}
	return nil
}

func keyError(id string) *signing.PyError {
	return &signing.PyError{Type: "KeyError", Msg: pystr.Repr(id)}
}

// SetStatus is set_status(): KeyError for an unknown id.
func (s *Store) SetStatus(id string, status any) (*pyjson.Object, error) {
	x := s.Get(id)
	if x == nil {
		return nil, keyError(id)
	}
	x.Set("status", status)
	return x, nil
}

// Repage is repage(): the stratum verbatim, or KeyError.
func (s *Store) Repage(id string) (*pyjson.Object, error) {
	x := s.Get(id)
	if x == nil {
		return nil, keyError(id)
	}
	return x, nil
}

// All is all().
func (s *Store) All() []*pyjson.Object { return append([]*pyjson.Object{}, s.strata...) }

// ByKind is by_kind().
func (s *Store) ByKind(kind string) []*pyjson.Object {
	var out []*pyjson.Object
	for _, x := range s.strata {
		if x.Value("kind") == kind {
			out = append(out, x)
		}
	}
	return out
}

func seqOf(x *pyjson.Object) int64 {
	var n int64
	fmt.Sscan(x.Value("seq").(pyjson.Int).Text, &n)
	return n
}

func alwaysInclude(x *pyjson.Object) bool {
	if x.Value("pinned") == true {
		return true
	}
	st := x.Value("status")
	return x.Value("kind") == "constraint" && (st == "open" || st == "promoted")
}

// relevance is _relevance: tag hits, a query hit, the ruled-out penalty,
// then the sequence number; higher is better.
func relevance(x *pyjson.Object, tags map[string]bool, query string) [4]int64 {
	var hits int64
	if len(tags) > 0 {
		own := map[string]bool{}
		for _, t := range x.Value("tags").([]any) {
			own[t.(string)] = true
		}
		for t := range own {
			if tags[t] {
				hits++
			}
		}
	}
	var q int64
	if query != "" && strings.Contains(pystr.Lower(x.Value("content").(string)), pystr.Lower(query)) {
		q = 1
	}
	var pen int64
	if x.Value("status") == "ruled_out" {
		pen = -1
	}
	return [4]int64{hits, q, pen, seqOf(x)}
}

func less(a, b [4]int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func dumps(xs []*pyjson.Object) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// Reconstruct is reconstruct_context(): the pinned strata and open
// constraints always, then the most relevant rest up to budget (nil is
// no budget), shown in the order they were found. It returns the
// ReconstructedContext dump.
func (s *Store) Reconstruct(budget *int64, tags []string, query string) *pyjson.Object {
	tagset := map[string]bool{}
	for _, t := range tags {
		tagset[t] = true
	}
	var pinned, cands []*pyjson.Object
	for _, x := range s.strata {
		if alwaysInclude(x) {
			pinned = append(pinned, x)
		} else {
			cands = append(cands, x)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return less(relevance(cands[j], tagset, query), relevance(cands[i], tagset, query))
	})
	selected, dropped := cands, []*pyjson.Object{}
	if budget != nil {
		room := *budget - int64(len(pinned))
		if room < 0 {
			room = 0
		}
		if room > int64(len(cands)) {
			room = int64(len(cands))
		}
		selected, dropped = cands[:room], cands[room:]
	}
	chosen := append(append([]*pyjson.Object{}, pinned...), selected...)
	sort.SliceStable(chosen, func(i, j int) bool { return seqOf(chosen[i]) < seqOf(chosen[j]) })
	note := fmt.Sprintf("Reconstruction is lossy: %d stratum(s) dropped — each a fact the agent will re-derive "+
		"unless re-paged. %d pinned/constraint stratum(s) always retained.", len(dropped), len(pinned))
	if budget != nil && int64(len(chosen)) > *budget {
		note += fmt.Sprintf(" Pinned/constraint strata (%d) exceed the budget (%d); budget is a floor, "+
			"not a ceiling — they are never dropped.", len(pinned), *budget)
	}
	ids := make([]any, len(dropped))
	for i, x := range dropped {
		ids[i] = x.Value("id")
	}
	return pyjson.NewObject().Set("strata", dumps(chosen)).Set("pinned", dumps(pinned)).
		Set("dropped_ids", ids).Set("note", note)
}

// ToJSON is to_json(): the store state as pydantic's compact JSON.
func (s *Store) ToJSON() string {
	return pathways.DumpJSON(pyjson.NewObject().Set("seq", pyjson.Int{Text: fmt.Sprint(s.seq)}).
		Set("strata", dumps(s.strata)))
}

// FromJSON is StrataStore.from_json(text).
func FromJSON(text string) (*Store, error) {
	out, err := pmodel.ValidateJSON("_StoreState", stateModel, text)
	if err != nil {
		return nil, err
	}
	o := out.(*pyjson.Object)
	st := &Store{}
	fmt.Sscan(o.Value("seq").(pyjson.Int).Text, &st.seq)
	for _, x := range o.Value("strata").([]any) {
		st.strata = append(st.strata, x.(*pyjson.Object))
	}
	return st, nil
}

// Promotion is PromotionResult.
type Promotion struct {
	OK                bool
	Envelope          *pyjson.Object
	Violations        []string // inheritance messages
	Reason            string
	EnforcementProven bool
}

// Promote is promote_constraint: env and candidate are validated
// Envelope dumps; addInvariant a validated Invariant dump; witness a
// validated ActionPlan dump. On success the stratum's status becomes
// "promoted".
func Promote(env *pyjson.Object, c *pyjson.Object, addInvariant *pyjson.Object, removeFileWrite []string,
	candidate *pyjson.Object, witness *pyjson.Object) (*Promotion, error) {
	if k := c.Value("kind"); k != "constraint" {
		return &Promotion{Envelope: env, Reason: fmt.Sprintf("only a 'constraint' stratum may touch authority; got kind "+
			"'%v' — facts, hypotheses and goals inform reasoning, never gate actions", k)}, nil
	}
	if c.Value("status") == "promoted" {
		return &Promotion{Envelope: env, Reason: "constraint is already promoted; re-promoting against the original " +
			"envelope would build a second tightening that drops the first"}, nil
	}
	if candidate == nil {
		candidate = tightened(env, addInvariant, removeFileWrite)
	}
	if loosening := envgen.VerifyInheritance(candidate, env); len(loosening) > 0 {
		return &Promotion{Envelope: env, Violations: loosening,
			Reason: "promotion would loosen the envelope; a captured constraint may only tighten"}, nil
	}
	if len(envgen.VerifyInheritance(env, candidate)) == 0 {
		return &Promotion{Envelope: env,
			Reason: "promotion has no enforceable effect (candidate equals the current envelope)"}, nil
	}
	proven := false
	if witness != nil {
		denied, err := denies(witness, candidate)
		if err != nil {
			return nil, err
		}
		if !denied {
			return &Promotion{Envelope: env, Reason: "promoted constraint does not actually deny its witness — an " +
				"unenforced (soft/uncompiled) constraint is refused, not accepted"}, nil
		}
		proven = true
	}
	c.Set("status", "promoted")
	return &Promotion{OK: true, Envelope: candidate, Reason: "tightened", EnforcementProven: proven}, nil
}

// denies is `not verify(witness, candidate).ok`. A Z3 check that did not
// finish counts as not denied, so a promotion is never taken as proven
// on a check that gave no answer.
func denies(witness, candidate *pyjson.Object) (bool, error) {
	venv, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(candidate)))
	if err != nil {
		return false, &signing.PyError{Type: "Unread", Msg: "the candidate envelope does not read: " + err.Error()}
	}
	p, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(witness)))
	if err != nil {
		return false, &signing.PyError{Type: "Unread", Msg: "the witness does not read: " + err.Error()}
	}
	r := verify.Verify(p, venv, verify.VerifyOptions{Z3TimeoutMs: 500})
	if len(r.Timeouts) > 0 {
		return false, nil
	}
	return !r.OK, nil
}

func tightened(env *pyjson.Object, inv *pyjson.Object, remove []string) *pyjson.Object {
	cand := envgen.DeepCopy(env)
	if inv != nil {
		invs := append([]any{}, cand.Value("invariants").([]any)...)
		cand.Set("invariants", append(invs, inv))
	}
	if len(remove) > 0 {
		drop := map[string]bool{}
		for _, g := range remove {
			drop[g] = true
		}
		perms := envgen.DeepCopy(cand.Value("permissions").(*pyjson.Object))
		kept := []any{}
		for _, g := range perms.Value("file_write").([]any) {
			if !drop[g.(string)] {
				kept = append(kept, g)
			}
		}
		perms.Set("file_write", kept)
		cand.Set("permissions", perms)
	}
	return cand
}
