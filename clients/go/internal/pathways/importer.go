package pathways

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
	"daisugi-verify/internal/z3"
)

func init() {
	// Import re-verifies in the Z3 this binary links, never a z3 on PATH.
	verify.InProcessZ3 = z3.EvalSMTLIB2
}

// ImportError is portability.PathwayImportError: a stable code and a
// message, printed as "[CODE] message".
type ImportError struct{ Code, Msg string }

func (e *ImportError) Error() string { return "[" + e.Code + "] " + e.Msg }

// IsSkill is what parse_bundle checks first: text that, with leading
// white space dropped, starts with "---" is skill markdown.
func IsSkill(text string) bool { return strings.HasPrefix(lstrip(text), "---") }

// lstrip is str.lstrip(): Python's white space, Unicode included.
func lstrip(s string) string {
	for i, r := range s {
		if !pystr.IsSpace(r) {
			return s[i:]
		}
	}
	return ""
}

// ParseBundle is portability.parse_bundle for the JSON form: the bundle
// read and its pathway validated as CompiledPathway.model_validate does.
// A skill file is the caller's to route (IsSkill); fromSkill is the
// daisugi mapping of one already read.
func ParseBundle(text, source string) (*Pathway, error) {
	text = lstrip(text)
	if !strings.HasPrefix(text, "{") {
		return nil, &ImportError{"SCHEMA_INCOMPATIBLE", "input is neither JSON nor skill markdown with YAML frontmatter"}
	}
	bundle, err := pyjson.Loads(text)
	if err != nil {
		return nil, &Invalid{"json.decoder.JSONDecodeError: " + err.Error()}
	}
	return pathwayFromBundle(bundle, source)
}

func pathwayFromBundle(bundle any, source string) (*Pathway, error) {
	o, ok := bundle.(*pyjson.Object)
	if !ok {
		return nil, &Invalid{"AttributeError: the bundle has no .get"}
	}
	if sv, present := o.Get("schema_version"); present {
		newer, err := greaterThanOne(sv)
		if err != nil {
			return nil, err
		}
		if newer {
			return nil, &ImportError{"SCHEMA_INCOMPATIBLE", fmt.Sprintf(
				"bundle schema_version=%s is newer than this library (%d)", pyRepr(sv), BundleSchemaVersion)}
		}
	}
	raw := o.Value("pathway")
	if raw == nil {
		return nil, &ImportError{"SCHEMA_INCOMPATIBLE", "bundle is missing 'pathway' key (source=" + source + ")"}
	}
	out, verr := pmodel.Validate("CompiledPathway", pmodel.CompiledPathway, raw, pmodel.Python)
	if verr != nil {
		for _, e := range verr.Errs {
			if e.Type == pmodel.UnreadableStep {
				return nil, fmt.Errorf("%w: a plan step is a string", ErrUnreadable)
			}
		}
		// parse_bundle's refusal: a NaN or an infinity in the envelope or
		// plan (models.non_finite_error) is one of these.
		return nil, &ImportError{"SCHEMA_INCOMPATIBLE", "the pathway is not valid: " + verr.String()}
	}
	return &Pathway{Obj: out.(*pyjson.Object)}, nil
}

// greaterThanOne is `v > BUNDLE_SCHEMA_VERSION` for what json gives:
// numbers and bools compare, anything else raises TypeError.
func greaterThanOne(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return false, nil
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		return ok && n.Cmp(big.NewInt(BundleSchemaVersion)) > 0, nil
	case pyjson.Float:
		return float64(x) > BundleSchemaVersion, nil
	}
	return false, &Invalid{"TypeError: '>' not supported for schema_version"}
}

// pyRepr is str() of a schema_version value for the message.
func pyRepr(v any) string {
	switch x := v.(type) {
	case pyjson.Int:
		return x.Text
	case pyjson.Float:
		return pyjson.FloatRepr(float64(x))
	}
	return fmt.Sprint(v)
}

// Verify is verify(plan_template, envelope, z3_timeout_ms) as import uses
// it: nil when the plan verifies, else the ImportError import raises. A Z3
// check that answered unknown refuses the import first
// (VERIFICATION_TIMEOUT): an envelope Z3 could not check is not admitted.
func Verify(p *Pathway, timeoutMs int) (*ImportError, error) {
	plan, err := verify.ParsePlan(json.RawMessage(DumpJSON(p.Obj.Value("plan_template"))))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	env, err := verify.ParseEnvelope(json.RawMessage(DumpJSON(p.Obj.Value("envelope"))))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	res := verify.Verify(plan, env, verify.VerifyOptions{Z3TimeoutMs: timeoutMs})
	if len(res.Timeouts) > 0 {
		return &ImportError{"VERIFICATION_TIMEOUT",
			"verifier timed out (" + strings.Join(res.Timeouts, "; ") + "); raise --z3-timeout-ms"}, nil
	}
	if res.OK {
		return nil, nil
	}
	parts := make([]string, len(res.Violations))
	for i, v := range res.Violations {
		parts[i] = "[" + v.Stage + "] " + v.Message
	}
	return &ImportError{"VERIFICATION_FAILED",
		"plan template does not verify against declared envelope: " + strings.Join(parts, "; ")}, nil
}

// serializationDepth is the deepest nesting of non-empty containers
// (models, dicts and lists, the root model included) pydantic-core
// serializes; one more raises "Circular reference detected (depth
// exceeded)".
const serializationDepth = 257

// Storable is portability._check_storable: a pathway the store cannot
// write and then read back is refused before anything is deleted or
// written, with the oracle's reason.
func Storable(p *Pathway) error {
	refuse := func(why string) error {
		return &ImportError{"UNSTORABLE", "pathway " + pystr.Repr(p.ID()) + " cannot be stored and read back: " + why}
	}
	for _, k := range []string{"id", "task_description", "embedding_model", "embedding_model_version", "structure_signature"} {
		if v, ok := p.Obj.Value(k).(string); ok && pystr.HasSurrogate(v) {
			return refuse("a text holds a lone surrogate")
		}
	}
	for _, k := range []string{"version", "hit_count", "failure_count"} {
		n, ok := new(big.Int).SetString(p.Obj.Value(k).(pyjson.Int).Text, 10)
		if !ok || !n.IsInt64() {
			return refuse("an integer is past 64 bits")
		}
	}
	for _, k := range []string{"distilled_at", "last_activation_at"} {
		if math.IsNaN(p.Obj.Value(k).(float64)) {
			return refuse("a time is NaN")
		}
	}
	for _, m := range []struct {
		key   string
		model *pmodel.Model
	}{{"envelope", pmodel.Envelope}, {"plan_template", pmodel.ActionPlan}} {
		v := p.Obj.Value(m.key)
		// model_dump_json raises past pydantic-core's serialization depth
		// and on a lone surrogate in a value (one in a key is written as
		// three U+FFFD, see keyText).
		if hasSurrogate(v) {
			return refuse("a text holds a lone surrogate")
		}
		if depth(v) > serializationDepth {
			return refuse("it nests deeper than it can be written")
		}
		if _, verr := pmodel.ValidateJSON(m.model.Name, m.model, DumpJSON(v)); verr != nil {
			return refuse("it nests deeper than it can be read back")
		}
	}
	return nil
}

// depth is how deep pydantic-core's serializer recurses into v: one
// level for each container it has to enter, so an empty list or dict,
// which has nothing to enter, counts as a leaf.
func depth(v any) int {
	switch x := v.(type) {
	case []any:
		if len(x) == 0 {
			return 0
		}
		d := 0
		for _, e := range x {
			d = max(d, depth(e))
		}
		return d + 1
	case *pyjson.Object:
		if x.Len() == 0 {
			return 0
		}
		d := 0
		for _, k := range x.Keys() {
			d = max(d, depth(x.Value(k)))
		}
		return d + 1
	}
	return 0
}

func hasSurrogate(v any) bool {
	switch x := v.(type) {
	case string:
		return pystr.HasSurrogate(x)
	case []any:
		for _, e := range x {
			if hasSurrogate(e) {
				return true
			}
		}
	case *pyjson.Object:
		// A key is not checked: pydantic writes a surrogate in a key as
		// its three bytes, each read back as U+FFFD (DumpJSON does too).
		for _, k := range x.Keys() {
			if hasSurrogate(x.Value(k)) {
				return true
			}
		}
	}
	return false
}

// PutPathway is PathwayStore.put(pathway): each column as the oracle
// writes it.
func (s *Store) PutPathway(p *Pathway) error {
	row, err := p.Row()
	if err != nil {
		return err
	}
	return s.Put(row)
}

// ReplacePathway is import's overwrite: the row that had this id deleted
// and the new one written, in one transaction, and only once the new row
// is known to be writable. The oracle deletes first and can then fail,
// losing the old row; this keeps it. It reports whether a row went.
func (s *Store) ReplacePathway(p *Pathway) (existed bool, err error) {
	row, err := p.Row()
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM pathways WHERE id = ?", row.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(insertSQL, row.args()...); err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// Row is the row put() writes. It refuses what Storable refuses.
func (p *Pathway) Row() (PutRow, error) {
	if err := Storable(p); err != nil {
		return PutRow{}, err
	}
	return p.PyRow()
}

// PyRow is the row put() writes, failing only where Python's sqlite3
// fails to bind a column. A caller that is not import (the gardener)
// meets the oracle's own exception rather than import's refusal.
func (p *Pathway) PyRow() (PutRow, error) {
	o := p.Obj
	// sqlite3 binds the columns in order and raises on the first it
	// cannot bind: a text with a lone surrogate, an int past 64 bits.
	var ints = map[string]int64{}
	for _, k := range []string{"id", "task_description", "version", "hit_count", "embedding_model",
		"embedding_model_version", "failure_count", "structure_signature"} {
		switch v := o.Value(k).(type) {
		case string:
			if pystr.HasSurrogate(v) {
				return PutRow{}, &Invalid{"UnicodeEncodeError: 'utf-8' codec can't encode a surrogate in " + k}
			}
		case pyjson.Int:
			n, ok := new(big.Int).SetString(v.Text, 10)
			if !ok || !n.IsInt64() {
				return PutRow{}, &Invalid{"OverflowError: Python int too large to convert to SQLite INTEGER"}
			}
			ints[k] = n.Int64()
		}
	}
	cols := [3]int64{ints["version"], ints["hit_count"], ints["failure_count"]}
	for _, k := range []string{"distilled_at", "last_activation_at"} {
		if f := o.Value(k).(float64); math.IsNaN(f) {
			// sqlite3 binds NaN as NULL, which the NOT NULL column refuses.
			return PutRow{}, &Invalid{"sqlite3.IntegrityError: NOT NULL constraint failed: pathways." + k}
		}
	}
	params := []any{}
	for _, x := range o.Value("parameters").([]any) {
		params = append(params, x)
	}
	var sig any
	if v := o.Value("structure_signature"); v != nil {
		sig = v.(string)
	}
	return PutRow{
		ID:             p.ID(),
		Task:           p.Task(),
		Embedding:      pyjson.Dumps(o.Value("task_embedding"), true),
		Envelope:       DumpJSON(o.Value("envelope")),
		Plan:           DumpJSON(o.Value("plan_template")),
		Traces:         pyjson.Dumps(o.Value("source_trace_ids"), true),
		Version:        cols[0],
		Hits:           cols[1],
		DistilledAt:    o.Value("distilled_at").(float64),
		Model:          o.Value("embedding_model").(string),
		ModelVersion:   o.Value("embedding_model_version").(string),
		LastActivation: o.Value("last_activation_at").(float64),
		Failures:       cols[2],
		Signature:      sig,
		Parameters:     pyjson.Dumps(params, true),
	}, nil
}
