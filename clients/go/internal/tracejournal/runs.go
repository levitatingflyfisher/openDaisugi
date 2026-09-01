package tracejournal

import (
	"errors"
	"fmt"
	"os"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pyyaml"
)

// This file is the supervised run's side of the journal: Journal.log_run,
// append_receipt, receipts_for_run and write_refinement.

// Run is what log_run reads off a RunSession.
type Run struct {
	Task string
	// Env and Plan are model_dump() values; Verification and Session are
	// JSON dumps (the session as asdict writes it).
	Env, Plan, Verification, Session *pyjson.Object
	RunID, Status                    string
	// FailedStepID is the first step whose status is "failed".
	FailedStepID    *string
	TotalDurationMs float64
}

// RunBody is the YAML body log_run writes.
func RunBody(r Run, traceID, createdAt string) (string, error) {
	payload := pyjson.NewObject().Set("id", traceID).Set("created_at", createdAt).Set("task", r.Task).
		Set("envelope", JSONMode(r.Env)).Set("plan", JSONMode(r.Plan)).Set("result", r.Verification).
		Set("run", r.Session)
	text, why := pyyaml.SafeDump(payload)
	if why != nil {
		return "", fmt.Errorf("%w: %s", ErrUnreadable, why.Why)
	}
	return text, nil
}

// LogRun is Journal.log_run: the YAML body first, then the index row with
// the run's columns. A failed insert removes the body.
func (j *Journal) LogRun(r Run, traceID, createdAt string) error {
	text, err := RunBody(r, traceID, createdAt)
	if err != nil {
		return err
	}
	path := j.TracePath(traceID)
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		return err
	}
	var sig any
	if s, ok := StructureSignature(r.Plan); ok {
		sig = s
	}
	okInt := 0
	if r.Verification.Value("ok") == true {
		okInt = 1
	}
	violations := []any{}
	for _, v := range asList(r.Verification.Value("violations")) {
		violations = append(violations, v)
	}
	var failed any
	if r.FailedStepID != nil {
		failed = *r.FailedStepID
	}
	_, err = j.db.Exec("INSERT INTO traces "+
		"(id, created_at, task, plan_id, envelope_id, ok, duration_ms, "+
		" violations_json, run_id, run_status, failed_step_id, "+
		" total_duration_ms, structure_signature) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		traceID, createdAt, r.Task, r.Plan.Value("id"), r.Env.Value("id"), okInt,
		floatOf(r.Verification.Value("duration_ms")), pyjson.Dumps(violations, true),
		r.RunID, r.Status, failed, r.TotalDurationMs, sig)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// Receipt is models.Receipt as append_receipt writes it.
type Receipt struct {
	StepID, RunID string
	Timestamp     float64
	// Evidence is the evidence dict; EvidenceJSON its json.dumps text.
	Evidence      *pyjson.Object
	EvidenceHash  string
	VerifyResult  bool
	VerifyDetails string
	ModelID       *string
	EffectClass   string
	Reversibility string
	ReversalJSON  *string
}

// AppendReceipt is Journal.append_receipt: INSERT OR REPLACE on (run_id,
// step_id).
func (j *Journal) AppendReceipt(r Receipt) error {
	vr := 0
	if r.VerifyResult {
		vr = 1
	}
	var model, reversal any
	if r.ModelID != nil {
		model = *r.ModelID
	}
	if r.ReversalJSON != nil {
		reversal = *r.ReversalJSON
	}
	_, err := j.db.Exec("INSERT OR REPLACE INTO receipts "+
		"(run_id, step_id, timestamp, evidence_hash, verify_result, "+
		"verify_details, evidence_json, model_id, "+
		"effect_class, reversibility, reversal_json) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.RunID, r.StepID, r.Timestamp, r.EvidenceHash, vr, r.VerifyDetails,
		pyjson.Dumps(r.Evidence, true), model, r.EffectClass, r.Reversibility, reversal)
	return err
}

// ReceiptSteps is the step ids of receipts_for_run(run_id).
func (j *Journal) ReceiptSteps(runID string) (map[string]bool, error) {
	rows, err := j.db.Query("SELECT step_id FROM receipts WHERE run_id = ? ORDER BY timestamp ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

// WriteRefinement is Journal.write_refinement: best effort, a failure is
// dropped as the oracle logs and drops it.
func (j *Journal) WriteRefinement(sessionID, recordJSON string, cacheKey any) {
	_, _ = j.db.Exec("INSERT INTO refinement_log (session_id, record_json, inserted_at, cache_key) "+
		"VALUES (?, ?, ?, ?)", sessionID, recordJSON, Now(), cacheKey)
}

// Now is time.time().
func Now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// ReceiptRow is one row receipts_for_run reads, as the Receipt model
// holds it.
type ReceiptRow struct {
	// Evidence is the evidence object as JSON text reads it.
	Evidence      *pyjson.Object
	StepID, RunID string
	Timestamp     float64
	EvidenceHash  string
	VerifyResult  bool
	VerifyDetails string
	ModelID       any // a str or None
	// EffectClass and Reversibility are a str or None; Reversal is the
	// reversal handle as JSON text reads it, or nil.
	EffectClass   any
	Reversibility any
	Reversal      *pyjson.Object
}

// ErrReceiptRow is a receipts row the Receipt model would not read the
// way this binary reads it.
var ErrReceiptRow = errors.New("a receipts row this binary does not read as a Receipt")

// Receipts is receipts_for_run(run_id): the run's receipts, oldest first.
func (j *Journal) Receipts(runID string) ([]ReceiptRow, error) {
	rows, err := j.db.Query("SELECT step_id, run_id, timestamp, evidence_json, evidence_hash, "+
		"verify_result, verify_details, model_id, "+
		"effect_class, reversibility, reversal_json FROM receipts WHERE run_id = ? "+
		"ORDER BY timestamp ASC", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReceiptRow
	for rows.Next() {
		var step, run, ts, evidence, hash, vr, details, model, effect, rev, reversal any
		if err := rows.Scan(&step, &run, &ts, &evidence, &hash, &vr, &details, &model, &effect, &rev,
			&reversal); err != nil {
			return nil, err
		}
		r := ReceiptRow{ModelID: model, EffectClass: effect, Reversibility: rev}
		var ok bool
		if r.StepID, ok = step.(string); !ok {
			return nil, ErrReceiptRow
		}
		if r.RunID, ok = run.(string); !ok {
			return nil, ErrReceiptRow
		}
		switch x := ts.(type) {
		case float64:
			r.Timestamp = x
		case int64:
			r.Timestamp = float64(x)
		default:
			return nil, ErrReceiptRow
		}
		if r.EvidenceHash, ok = hash.(string); !ok {
			return nil, ErrReceiptRow
		}
		n, ok := vr.(int64)
		if !ok {
			return nil, ErrReceiptRow
		}
		r.VerifyResult = n != 0
		if r.VerifyDetails, ok = details.(string); !ok {
			return nil, ErrReceiptRow
		}
		if model != nil {
			if _, ok := model.(string); !ok {
				return nil, ErrReceiptRow
			}
		}
		text, ok := evidence.(string)
		if !ok {
			return nil, ErrReceiptRow
		}
		if v, err := pyjson.Loads(text); err != nil {
			return nil, ErrReceiptRow
		} else if o, isObj := v.(*pyjson.Object); !isObj {
			return nil, ErrReceiptRow
		} else {
			r.Evidence = o
		}
		for _, v := range []any{effect, rev} {
			if v != nil {
				if _, ok := v.(string); !ok {
					return nil, ErrReceiptRow
				}
			}
		}
		if reversal != nil && reversal != "" {
			// A reversal handle: the model reads it; this binary checks
			// only that it is a JSON object, the form the supervisor writes.
			t, ok := reversal.(string)
			if !ok {
				return nil, ErrReceiptRow
			}
			v, err := pyjson.Loads(t)
			if err != nil {
				return nil, ErrReceiptRow
			}
			o, isObj := v.(*pyjson.Object)
			if !isObj {
				return nil, ErrReceiptRow
			}
			r.Reversal = o
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
