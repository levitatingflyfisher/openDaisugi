// Package supervise is the oracle's supervised run
// (opendaisugi/supervisor.py and what it calls): the plan verified before
// it runs, each step verified again, approved, handed to its executor, the
// completed step checked against the envelope's postconditions, a receipt
// written for every step that ran, the receipts checked at the end, and
// the run written to the journal.
//
// Steps and plans are model_dump() values (*pyjson.Object), the same
// values the oracle's pydantic models dump, so the journal holds what the
// oracle writes.
package supervise

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// Run statuses (run_session.RunStatus).
const (
	Pending   = "pending"
	Rejected  = "rejected"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Aborted   = "aborted"
	Halted    = "halted_by_simplex"
)

// ExecResult is executor.ExecutorResult.
type ExecResult struct {
	RC         int
	Stdout     string
	DurationMs float64
	TimedOut   bool
	// Model, Tokens and CostUSD are set by a model-backed executor.
	Model   *string
	Tokens  *int64
	CostUSD *float64
	// Reversibility and Reversal are the deed ledger's verdict, set by an
	// executor that mutated (or refused to mutate) something.
	Reversibility *string
	Reversal      *pyjson.Object
}

// Outcome is run_session.StepOutcome.
type Outcome struct {
	StepID        string
	Status        string
	ApprovedBy    *string
	RC            *int
	Stdout        string
	DurationMs    float64
	StartedAt     string
	Error         *string
	ModelID       *string
	Reversibility *string
	Reversal      *pyjson.Object
}

// Dump is dataclasses.asdict(outcome), the reversal as its model dump.
func (o Outcome) Dump() *pyjson.Object { return o.dump(false) }

// dump with asStr writes the reversal as json.dumps(default=str) writes
// the model asdict leaves in place: str(model).
func (o Outcome) dump(asStr bool) *pyjson.Object {
	var rc any
	if o.RC != nil {
		rc = PyInt(*o.RC)
	}
	var rev any
	if o.Reversal != nil {
		rev = o.Reversal
		if asStr {
			parts := make([]string, 0, len(o.Reversal.Keys()))
			for _, k := range o.Reversal.Keys() {
				parts = append(parts, k+"="+pmodel.Repr(o.Reversal.Value(k)))
			}
			rev = strings.Join(parts, " ")
		}
	}
	return pyjson.NewObject().Set("step_id", o.StepID).Set("status", o.Status).
		Set("approved_by", sp(o.ApprovedBy)).Set("rc", rc).Set("stdout", o.Stdout).
		Set("duration_ms", o.DurationMs).Set("started_at", o.StartedAt).Set("error", sp(o.Error)).
		Set("model_id", sp(o.ModelID)).Set("reversibility", sp(o.Reversibility)).Set("reversal", rev)
}

// Session is run_session.RunSession.
type Session struct {
	ID              string
	EnvelopeID      string
	PlanID          string
	Status          string
	Verification    *pyjson.Object
	Steps           []Outcome
	StartedAt       string
	EndedAt         *string
	TraceID         *string
	IntegrityPassed *bool
	FailedStepID    *string
}

// Dump is the session as log_run writes it and `run --json` prints it:
// asdict with the status as its value and the verification's JSON dump.
func (s *Session) Dump() *pyjson.Object { return s.dump(false) }

// JSON is _serialize_session: the session `run --json` prints.
func (s *Session) JSON() *pyjson.Object { return s.dump(true) }

func (s *Session) dump(asStr bool) *pyjson.Object {
	steps := make([]any, len(s.Steps))
	for i, o := range s.Steps {
		steps[i] = o.dump(asStr)
	}
	var integrity any
	if s.IntegrityPassed != nil {
		integrity = *s.IntegrityPassed
	}
	return pyjson.NewObject().Set("id", s.ID).Set("envelope_id", s.EnvelopeID).Set("plan_id", s.PlanID).
		Set("status", s.Status).Set("verification", s.Verification).Set("steps", steps).
		Set("started_at", s.StartedAt).Set("ended_at", sp(s.EndedAt)).Set("trace_id", sp(s.TraceID)).
		Set("integrity_passed", integrity).Set("failed_step_id", sp(s.FailedStepID))
}

func sp(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func strp(s string) *string { return &s }

// PyInt is an int as a JSON and YAML value.
func PyInt(n int) pyjson.Int { return pyjson.Int{Text: strconv.Itoa(n)} }

// EvidenceHash is models.compute_evidence_hash: the SHA-256 of
// json.dumps(evidence, sort_keys=True, separators=(",", ":"), default=str).
func EvidenceHash(evidence *pyjson.Object) string {
	var b strings.Builder
	canonicalASCII(&b, evidence)
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}

func canonicalASCII(b *strings.Builder, v any) {
	switch x := v.(type) {
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			canonicalASCII(b, e)
		}
		b.WriteByte(']')
	case *pyjson.Object:
		keys := append([]string{}, x.Keys()...)
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(pyjson.Dumps(k, true))
			b.WriteByte(':')
			canonicalASCII(b, x.Value(k))
		}
		b.WriteByte('}')
	default:
		b.WriteString(pyjson.Dumps(v, true))
	}
}

// str is a step field that holds a string ("" when absent).
func str(step *pyjson.Object, key string) string {
	s, _ := step.Value(key).(string)
	return s
}
