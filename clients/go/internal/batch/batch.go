// Package batch is the oracle's within-instance batch compilation
// (opendaisugi/batch.py): an agent declares a program, its items, the
// write footprint and an acceptance check; the whole write set is proved
// inside the envelope and the footprint before any item runs, programs
// that could leave an irreversible effect are refused, the acceptance is
// checked on a sample that is then undone from the deed ledger, and every
// item runs under the supervisor with per-element rollback.
package batch

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"daisugi-verify/internal/deeds"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/verify"
)

// Model is batch.BatchDeclaration.
var Model = &pmodel.Model{Name: "BatchDeclaration", Fields: []pmodel.Field{
	{Name: "program", Schema: pmodel.ActionPlan, Required: true},
	{Name: "parameters", Schema: pmodel.List{Elem: pmodel.PathwayParameter}, Default: func() any { return []any{} }},
	{Name: "items", Schema: pmodel.List{Elem: pmodel.Dict{Val: pmodel.Str{}}}, Required: true},
	{Name: "footprint", Schema: pmodel.List{Elem: pmodel.Str{}}, Default: func() any { return []any{} }},
	{Name: "acceptance", Schema: pmodel.Nullable{Inner: pmodel.Postcondition}, Default: func() any { return nil }},
	{Name: "sample_k", Schema: pmodel.Int{}, Default: func() any { return pyjson.Int{Text: "2"} }},
}}

var batchable = map[string]bool{"file_write": true, "file_read": true, "network": true}

// Decl is a validated BatchDeclaration dump.
type Decl struct{ O *pyjson.Object }

func (d Decl) steps() []*pyjson.Object {
	var out []*pyjson.Object
	for _, s := range d.O.Value("program").(*pyjson.Object).Value("steps").([]any) {
		out = append(out, s.(*pyjson.Object))
	}
	return out
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Classification is BatchClassification.
type Classification struct {
	Batchable    bool
	NonBatchable []*pyjson.Object
}

// Dump is asdict.
func (c *Classification) Dump() *pyjson.Object {
	nb := make([]any, len(c.NonBatchable))
	for i, x := range c.NonBatchable {
		nb[i] = x
	}
	return pyjson.NewObject().Set("batchable", c.Batchable).Set("non_batchable", nb)
}

// Classify is classify_declaration.
func Classify(d Decl) *Classification {
	c := &Classification{NonBatchable: []*pyjson.Object{}}
	for _, s := range d.steps() {
		t, _ := s.Value("type").(string)
		if !batchable[t] {
			c.NonBatchable = append(c.NonBatchable, pyjson.NewObject().Set("id", s.Value("id")).Set("type", t))
		}
	}
	c.Batchable = len(c.NonBatchable) == 0
	return c
}

// NonBatchableKinds is ", ".join(sorted({type ...})).
func (c *Classification) NonBatchableKinds() string {
	seen := map[string]bool{}
	var kinds []string
	for _, x := range c.NonBatchable {
		t := x.Value("type").(string)
		if !seen[t] {
			seen[t] = true
			kinds = append(kinds, t)
		}
	}
	sortStrings(kinds)
	return strings.Join(kinds, ", ")
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// Resolve is resolve_items: each item's bound plan, or nil where the
// binding fails (its index recorded). A ValueError from the binding is a
// failure; any other exception is returned, as the oracle raises it.
func Resolve(d Decl) (plans []*pyjson.Object, bad []int, err error) {
	var params []*pyjson.Object
	for _, p := range d.O.Value("parameters").([]any) {
		params = append(params, p.(*pyjson.Object))
	}
	program := d.O.Value("program").(*pyjson.Object)
	for i, it := range d.O.Value("items").([]any) {
		plan, e := envgen.ApplyBindings(program, params, it.(*pyjson.Object))
		if e != nil {
			var pe *envgen.PyError
			if !errors.As(e, &pe) || pe.Class != "ValueError" {
				return nil, nil, e
			}
			plan = nil
		}
		if plan == nil {
			bad = append(bad, i)
		}
		plans = append(plans, plan)
	}
	return plans, bad, nil
}

func writesOf(plan *pyjson.Object) []string {
	var out []string
	for _, s := range plan.Value("steps").([]any) {
		st := s.(*pyjson.Object)
		if st.Value("type") == "file_write" {
			out = append(out, st.Value("path").(string))
		}
	}
	return out
}

// Proof is FootprintProof.
type Proof struct {
	OK            bool
	Writes        []string
	OutOfEnvelope []string
	UnderDeclared []string
	BadBindings   []int
	Reason        string
	Vacuous       bool
}

func anyList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// Dump is asdict.
func (p *Proof) Dump() *pyjson.Object {
	bad := make([]any, len(p.BadBindings))
	for i, b := range p.BadBindings {
		bad[i] = b
	}
	return pyjson.NewObject().Set("ok", p.OK).Set("resolved_writes", anyList(p.Writes)).
		Set("out_of_envelope", anyList(p.OutOfEnvelope)).Set("under_declared", anyList(p.UnderDeclared)).
		Set("bad_bindings", bad).Set("reason", p.Reason).Set("vacuous", p.Vacuous)
}

func intList(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func strListRepr(xs []string) string {
	if len(xs) == 0 {
		return "[]"
	}
	return pystr.ReprList(xs)
}

// Prove is prove_footprint: every write each item resolves to checked
// with the runtime gate's own path matcher against the envelope's
// file_write globs and the declared footprint. envFileWrite is the
// envelope's file_write list.
func Prove(d Decl, envFileWrite []string) (*Proof, error) {
	plans, bad, err := Resolve(d)
	if err != nil {
		return nil, err
	}
	foot := strs(d.O.Value("footprint"))
	p := &Proof{Writes: []string{}, OutOfEnvelope: []string{}, UnderDeclared: []string{}, BadBindings: bad}
	if p.BadBindings == nil {
		p.BadBindings = []int{}
	}
	for _, plan := range plans {
		if plan == nil {
			continue
		}
		for _, w := range writesOf(plan) {
			p.Writes = append(p.Writes, w)
			if !verify.PathMatchesAny(w, envFileWrite) {
				p.OutOfEnvelope = append(p.OutOfEnvelope, w)
			}
			if !verify.PathMatchesAny(w, foot) {
				p.UnderDeclared = append(p.UnderDeclared, w)
			}
		}
	}
	p.OK = len(bad) == 0 && len(p.OutOfEnvelope) == 0 && len(p.UnderDeclared) == 0
	var reasons []string
	if len(bad) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d item(s) failed to bind (unbound hole or head change): %s", len(bad), intList(bad)))
	}
	if len(p.OutOfEnvelope) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d write(s) outside the envelope: %s", len(p.OutOfEnvelope), strListRepr(p.OutOfEnvelope)))
	}
	if len(p.UnderDeclared) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d write(s) outside the declared footprint F: %s", len(p.UnderDeclared),
			strListRepr(p.UnderDeclared)))
	}
	p.Reason = strings.Join(reasons, "; ")
	p.Vacuous = len(p.Writes) == 0
	return p, nil
}

// WouldBeReversible is would_be_reversible: a symlink target is not a
// reason to refuse; otherwise the executor's own pre-image capture says.
func WouldBeReversible(path string) bool {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return true
	}
	parent := supervise.PyDirname(path)
	if parent == "" {
		parent = "."
	}
	_, ok := supervise.CapturePreImage(path, parent)
	return ok
}

// Irreversible is [w for w in writes if not would_be_reversible(w)].
func Irreversible(writes []string) []string {
	out := []string{}
	for _, w := range writes {
		if !WouldBeReversible(w) {
			out = append(out, w)
		}
	}
	return out
}

// DumpJSON is decl.model_dump_json().
func (d Decl) DumpJSON() string { return pathways.DumpJSON(d.O) }

// Validate is BatchDeclaration.model_validate(v) (Python mode).
func Validate(v any) (Decl, *pmodel.ValidationError) {
	out, err := pmodel.Validate("BatchDeclaration", Model, v, pmodel.Python)
	if err != nil {
		return Decl{}, err
	}
	return Decl{O: out.(*pyjson.Object)}, nil
}

// ValidateJSON is BatchDeclaration.model_validate_json(text).
func ValidateJSON(text string) (Decl, *pmodel.ValidationError) {
	out, err := pmodel.ValidateJSON("BatchDeclaration", Model, text)
	if err != nil {
		return Decl{}, err
	}
	return Decl{O: out.(*pyjson.Object)}, nil
}

// -- the net-token meter ------------------------------------------------------

// DefaultTokensPerCall is _DEFAULT_TOKENS_PER_CALL.
const DefaultTokensPerCall = 2000

// Ledger is NetTokenLedger.
type Ledger struct {
	Label, Baseline                                string
	OutputSaved, CallsSaved, PerCall, SpecInjected int64
}

// Dump is model_dump(mode="json"), the computed fields last.
func (l Ledger) Dump() *pyjson.Object {
	net := l.OutputSaved + l.CallsSaved*l.PerCall - l.SpecInjected
	return pyjson.NewObject().Set("label", l.Label).Set("baseline", l.Baseline).
		Set("output_tokens_saved", pyInt(l.OutputSaved)).Set("calls_saved", pyInt(l.CallsSaved)).
		Set("tokens_per_call", pyInt(l.PerCall)).Set("spec_input_injected", pyInt(l.SpecInjected)).
		Set("evidence_not_proof", true).Set("net", pyInt(net)).Set("net_positive", net > 0)
}

func pyInt(n int64) pyjson.Int { return pyjson.Int{Text: fmt.Sprint(n)} }

// WithinInstance is NetTokenLedger.within_instance. perCall and spec nil
// take the defaults: 2000, and the declaration's JSON length / 4 (at
// least 1).
func WithinInstance(d Decl, outputSaved, callsSaved int64, perCall, spec *int64) Ledger {
	l := Ledger{Label: "within-instance",
		Baseline:    "honest-script (a competent agent already scripts the bulk job)",
		OutputSaved: outputSaved, CallsSaved: callsSaved, PerCall: DefaultTokensPerCall}
	if perCall != nil {
		l.PerCall = *perCall
	}
	if spec != nil {
		l.SpecInjected = *spec
	} else {
		n := int64(pystr.Len(d.DumpJSON()) / 4)
		if n < 1 {
			n = 1
		}
		l.SpecInjected = n
	}
	return l
}

// TwoLedgers is TwoLedgerReport's dump with no cross-instance ledger.
func TwoLedgers(within Ledger) *pyjson.Object {
	return pyjson.NewObject().Set("within_instance", within.Dump()).Set("cross_instance", nil).
		Set("note", "Two ledgers reported separately and never merged (roadmap Stage 9). The within-instance "+
			"win is the proven blast radius; the cross-instance (persistence + generalization) win is Stage 4's "+
			"at-scale question.")
}

// -- execution ----------------------------------------------------------------

// Runner runs one bound plan under the supervisor (Supervisor.run).
type Runner func(plan *pyjson.Object) (*supervise.Session, error)

// Result is BatchResult.
type Result struct {
	Status         string
	Reason         string
	Classification *Classification
	Proof          *Proof
	SampleOK       bool
	Executed       int
	Reversals      []*pyjson.Object
	Rollback       *deeds.Report
	Ledger         *pyjson.Object
}

// Dump is the probe's view of the result: each part as asdict or
// model_dump writes it.
func (r *Result) Dump() *pyjson.Object {
	var cls, proof, rb any
	if r.Classification != nil {
		cls = r.Classification.Dump()
	}
	if r.Proof != nil {
		proof = r.Proof.Dump()
	}
	if r.Rollback != nil {
		rb = r.Rollback.Dump()
	}
	revs := make([]any, len(r.Reversals))
	for i, h := range r.Reversals {
		revs[i] = h
	}
	var ledger any
	if r.Ledger != nil {
		ledger = r.Ledger
	}
	return pyjson.NewObject().Set("status", r.Status).Set("reason", r.Reason).Set("classification", cls).
		Set("proof", proof).Set("sample_ok", r.SampleOK).Set("executed", r.Executed).Set("reversals", revs).
		Set("rollback", rb).Set("ledger", ledger)
}

func handlesOf(s *supervise.Session) []*pyjson.Object {
	var out []*pyjson.Object
	for _, o := range s.Steps {
		if o.Reversal != nil {
			out = append(out, o.Reversal)
		}
	}
	return out
}

// rollback is _rollback: newest first; an undo that fails is reported as
// skipped with the error, never as undone.
func rollback(handles []*pyjson.Object) (*deeds.Report, error) {
	rep := &deeds.Report{Undone: []string{}, Skipped: []*pyjson.Object{}}
	for i := len(handles) - 1; i >= 0; i-- {
		h, verr := deeds.ParseHandle(handles[i])
		if verr != nil {
			return nil, verr
		}
		if err := deeds.Apply(h); err != nil {
			_, text, ok := supervise.PyOSError(err, h.Path)
			if !ok {
				return nil, err
			}
			rep.Skipped = append(rep.Skipped, pyjson.NewObject().Set("path", h.Path).Set("reason", "rollback failed: "+text))
			continue
		}
		rep.Undone = append(rep.Undone, h.Path)
	}
	return rep, nil
}

func acceptanceHolds(q *pyjson.Object, s *supervise.Session, plan *pyjson.Object) bool {
	for _, o := range s.Steps {
		if o.Status != supervise.Succeeded {
			return false
		}
	}
	if q == nil {
		return true
	}
	switch q.Value("type") {
	case "succeeded", "ran":
		return true
	case "rc_zero", "rc0":
		for _, o := range s.Steps {
			if o.RC != nil && *o.RC != 0 {
				return false
			}
		}
		return true
	case "file_exists", "file_nonempty":
		var paths []string
		if p, ok := q.Value("path").(string); ok && p != "" {
			paths = []string{p}
		} else {
			paths = writesOf(plan)
		}
		for _, p := range paths {
			if p == "" {
				return false
			}
			st, err := os.Stat(p)
			if err != nil {
				return false
			}
			if q.Value("type") == "file_nonempty" && st.Size() == 0 {
				return false
			}
		}
		return true
	}
	return false
}

// Run is run_batch. sampleK nil takes the declaration's sample_k.
func Run(d Decl, envFileWrite []string, run Runner, sampleK *int64) (*Result, error) {
	cls := Classify(d)
	if !cls.Batchable {
		return &Result{Status: "rejected", Classification: cls,
			Reason: "program contains non-batchable step kind(s): " + cls.NonBatchableKinds()}, nil
	}
	proof, err := Prove(d, envFileWrite)
	if err != nil {
		return nil, err
	}
	if !proof.OK {
		return &Result{Status: "rejected", Classification: cls, Proof: proof,
			Reason: "footprint not provable: " + proof.Reason}, nil
	}
	if irr := Irreversible(proof.Writes); len(irr) > 0 {
		return &Result{Status: "rejected", Classification: cls, Proof: proof,
			Reason: "targets whose write would be irreversible cannot enter a batch: " + strListRepr(irr)}, nil
	}
	resolved, _, err := Resolve(d)
	if err != nil {
		return nil, err
	}
	var plans []*pyjson.Object
	for _, p := range resolved {
		if p != nil {
			plans = append(plans, p)
		}
	}
	var k int64
	if sampleK != nil {
		k = *sampleK
	} else {
		fmt.Sscan(d.O.Value("sample_k").(pyjson.Int).Text, &k)
	}
	n := int64(len(plans))
	if k < 0 {
		k += n
		if k < 0 {
			k = 0
		}
	}
	if k > n {
		k = n
	}
	acc, _ := d.O.Value("acceptance").(*pyjson.Object)
	var sample []*pyjson.Object
	sampleOK := true
	for _, plan := range plans[:k] {
		s, err := run(plan)
		if err != nil {
			return nil, err
		}
		sample = append(sample, handlesOf(s)...)
		if s.Status != supervise.Succeeded || !acceptanceHolds(acc, s, plan) {
			sampleOK = false
			break
		}
	}
	sampleReport, err := rollback(sample)
	if err != nil {
		return nil, err
	}
	if !sampleOK {
		return &Result{Status: "rejected", Classification: cls, Proof: proof, Rollback: sampleReport,
			Reason: "acceptance postcondition Q failed on the sampled fork"}, nil
	}
	var done []*pyjson.Object
	executed := 0
	for _, plan := range plans {
		s, err := run(plan)
		if err != nil {
			return nil, err
		}
		if s.Status != supervise.Succeeded {
			rep, err := rollback(done)
			if err != nil {
				return nil, err
			}
			return &Result{Status: "halted", Classification: cls, Proof: proof, SampleOK: true, Executed: executed,
				Rollback: rep, Reason: fmt.Sprintf("element did not succeed (%s); rolled back and halted", s.Status)}, nil
		}
		var irreversible []supervise.Outcome
		for _, o := range s.Steps {
			if o.Reversibility != nil && *o.Reversibility == "irreversible" {
				irreversible = append(irreversible, o)
			}
		}
		if len(irreversible) > 0 {
			rep, err := rollback(append(append([]*pyjson.Object{}, done...), handlesOf(s)...))
			if err != nil {
				return nil, err
			}
			for _, o := range irreversible {
				rep.Skipped = append(rep.Skipped, pyjson.NewObject().Set("step_id", o.StepID).
					Set("path", stepPath(plan, o.StepID)).Set("reason", "irreversible"))
			}
			return &Result{Status: "halted", Classification: cls, Proof: proof, SampleOK: true, Executed: executed,
				Rollback: rep, Reason: "an element produced an irreversible deed; halted before the rest"}, nil
		}
		done = append(done, handlesOf(s)...)
		executed++
	}
	return &Result{Status: "succeeded", Classification: cls, Proof: proof, SampleOK: true, Executed: executed,
		Reversals: done, Ledger: TwoLedgers(WithinInstance(d, 0, 0, nil, nil))}, nil
}

func stepPath(plan *pyjson.Object, id string) string {
	for _, s := range plan.Value("steps").([]any) {
		st := s.(*pyjson.Object)
		if st.Value("id") == id {
			p, _ := st.Value("path").(string)
			return p
		}
	}
	return ""
}

// RollbackResult is rollback_result: the rollback already made, or the
// succeeded batch undone now from its handles.
func RollbackResult(r *Result) (*deeds.Report, error) {
	if r.Rollback != nil {
		return r.Rollback, nil
	}
	return rollback(r.Reversals)
}
