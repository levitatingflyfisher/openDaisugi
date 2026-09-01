package pathways

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// ErrUnreadable is a row this binary cannot read the way Python does.
// The command refuses rather than guess.
var ErrUnreadable = errors.New("cannot read")

// Invalid is a row Python fails to read: pydantic or json raises, and
// the CLI exits 1 with a traceback.
type Invalid struct{ Msg string }

func (e *Invalid) Error() string { return e.Msg }

// Pathway is a validated CompiledPathway: model_dump() in field order.
type Pathway struct {
	Obj     *pyjson.Object
	embText string
}

// ID is the pathway's id.
func (p *Pathway) ID() string { return p.Obj.Value("id").(string) }

// Task is task_description.
func (p *Pathway) Task() string { return p.Obj.Value("task_description").(string) }

// pyValue is a column value as Python's sqlite3 gives it, in the pyjson
// types pmodel reads.
func pyValue(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case int64:
		return pyjson.Int{Text: strconv.FormatInt(x, 10)}, nil
	case float64:
		return pyjson.Float(x), nil
	case string:
		return x, nil
	}
	// A BLOB is bytes in Python; pydantic's str and float read bytes by
	// rules this port does not carry.
	return nil, fmt.Errorf("%w: a column holds a %T", ErrUnreadable, v)
}

func field(title string, s pmodel.Schema, v any) (any, error) {
	out, verr := pmodel.Validate(title, s, v, pmodel.Python)
	if verr != nil {
		return nil, &Invalid{"pydantic_core._pydantic_core.ValidationError: " + verr.String()}
	}
	return out, nil
}

func loads(text any) (any, error) {
	s, ok := text.(string)
	if !ok {
		// json.loads of an int or None raises TypeError.
		return nil, &Invalid{"TypeError: the JSON object must be str, bytes or bytearray"}
	}
	v, err := pyjson.Loads(s)
	if err != nil {
		return nil, &Invalid{"json.decoder.JSONDecodeError: " + err.Error()}
	}
	return v, nil
}

// FromRow is PathwayStore._row_to_pathway: the JSON columns decoded, the
// envelope and plan read with model_validate_json, and CompiledPathway
// built from them.
func FromRow(r Row) (*Pathway, error) {
	get := func(col string) (any, error) {
		v, present := r[col]
		if !present {
			return nil, fmt.Errorf("%w: the row has no %s column", ErrUnreadable, col)
		}
		return pyValue(v)
	}
	out := pyjson.NewObject()
	for _, col := range []string{"id", "task_description"} {
		v, err := get(col)
		if err != nil {
			return nil, err
		}
		if v, err = field("CompiledPathway", pmodel.Str{}, v); err != nil {
			return nil, err
		}
		out.Set(col, v)
	}
	embText, err := get("task_embedding_json")
	if err != nil {
		return nil, err
	}
	// json.loads and list[float]: checked here, the floats themselves
	// built only when a command prints them (Full).
	embStr, isStr := embText.(string)
	if !isStr {
		if _, err := loads(embText); err != nil {
			return nil, err
		}
	}
	if _, ok := scanEmbedding(embStr); !ok {
		if _, err := loads(embStr); err != nil {
			return nil, err
		}
		return nil, &Invalid{"pydantic_core._pydantic_core.ValidationError: " + "task_embedding is not a list of numbers"}
	}
	out.Set("task_embedding", nil)
	for _, col := range []string{"embedding_model", "embedding_model_version"} {
		v, err := get(col)
		if err != nil {
			return nil, err
		}
		if v, err = field("CompiledPathway", pmodel.Str{}, v); err != nil {
			return nil, err
		}
		out.Set(col, v)
	}
	for _, c := range []struct {
		col, key string
		model    *pmodel.Model
	}{{"envelope_json", "envelope", pmodel.Envelope}, {"plan_template_json", "plan_template", pmodel.ActionPlan}} {
		v, err := get(c.col)
		if err != nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, &Invalid{"pydantic_core._pydantic_core.ValidationError: " + "model_validate_json needs a str"}
		}
		obj, verr := pmodel.ValidateJSON(c.model.Name, c.model, s)
		if verr != nil {
			for _, e := range verr.Errs {
				if e.Type == pmodel.UnreadableStep {
					return nil, fmt.Errorf("%w: a plan step is a string", ErrUnreadable)
				}
			}
			return nil, &Invalid{"pydantic_core._pydantic_core.ValidationError: " + verr.String()}
		}
		out.Set(c.key, obj)
	}
	traces, err := get("source_trace_ids_json")
	if err != nil {
		return nil, err
	}
	if traces, err = loads(traces); err != nil {
		return nil, err
	}
	if traces, err = field("CompiledPathway", pmodel.List{Elem: pmodel.Str{}}, traces); err != nil {
		return nil, err
	}
	out.Set("source_trace_ids", traces)
	for _, c := range []struct {
		col string
		s   pmodel.Schema
	}{{"version", pmodel.Int{}}, {"hit_count", pmodel.Int{}}, {"distilled_at", pmodel.Float{}}} {
		v, err := get(c.col)
		if err != nil {
			return nil, err
		}
		if v, err = field("CompiledPathway", c.s, v); err != nil {
			return nil, err
		}
		out.Set(c.col, v)
	}
	// The later columns: present on every migrated table.
	for _, c := range []struct {
		col string
		s   pmodel.Schema
	}{{"last_activation_at", pmodel.Float{}}, {"failure_count", pmodel.Int{}}} {
		v, err := get(c.col)
		if err != nil {
			return nil, err
		}
		if v, err = field("CompiledPathway", c.s, v); err != nil {
			return nil, err
		}
		out.Set(c.col, v)
	}
	out.Set("activation_count", pyjson.Int{Text: "0"})
	sig, err := get("structure_signature")
	if err != nil {
		return nil, err
	}
	if sig, err = field("CompiledPathway", pmodel.Nullable{Inner: pmodel.Str{}}, sig); err != nil {
		return nil, err
	}
	out.Set("structure_signature", sig)
	params, err := get("parameters_json")
	if err != nil {
		return nil, err
	}
	// `json.loads(p) if p else []`: an empty text or a NULL is no
	// parameters.
	var plist any = []any{}
	if pyjson.Truthy(params) {
		if plist, err = loads(params); err != nil {
			return nil, err
		}
	}
	if plist, err = field("CompiledPathway", pmodel.List{Elem: pmodel.PathwayParameter}, plist); err != nil {
		return nil, err
	}
	out.Set("parameters", plist)
	return &Pathway{Obj: out, embText: embStr}, nil
}

// Full sets task_embedding to its floats, for a command that prints it.
func (p *Pathway) Full() *Pathway {
	if p.Obj.Value("task_embedding") != nil {
		return p
	}
	vec, _ := parseEmbedding(p.embText)
	xs := make([]any, len(vec))
	for i, x := range vec {
		xs[i] = x
	}
	p.Obj.Set("task_embedding", xs)
	return p
}

// pydantic's JSON, written from a model_dump() value.

func pydanticValue(v any) any {
	switch x := v.(type) {
	case float64:
		return pyjson.Raw(floatJSON(x))
	case pyjson.Float:
		return pyjson.Raw(floatJSON(float64(x)))
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = pydanticValue(e)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			out.Set(k, pydanticValue(x.Value(k)))
		}
		return out
	}
	return v
}

// floatJSON is pydantic's text for a float: its own exponent form, and
// null for NaN and the infinities.
func floatJSON(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	s := pyjson.FloatRepr(f)
	if mant, ok := strings.CutSuffix(s, "e-05"); ok {
		sign := ""
		if strings.HasPrefix(mant, "-") {
			sign, mant = "-", mant[1:]
		}
		return sign + "0.0000" + strings.Replace(mant, ".", "", 1)
	}
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		sign := ""
		if exp[0] == '+' || exp[0] == '-' {
			sign, exp = exp[:1], exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		s = mant + "e" + sign + exp
	}
	return s
}

// DumpJSON is model_dump_json(): compact, non-ASCII kept.
func DumpJSON(v any) string {
	var b strings.Builder
	compact(&b, pydanticValue(v))
	return b.String()
}

// DumpJSONIndent is model_dump_json(indent=n).
func DumpJSONIndent(v any, n int) string {
	return pyjson.DumpsIndent(pydanticValue(v), n, false)
}

func compact(b *strings.Builder, v any) {
	switch x := v.(type) {
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			compact(b, e)
		}
		b.WriteByte(']')
	case *pyjson.Object:
		b.WriteByte('{')
		for i, k := range x.Keys() {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(pyjson.Dumps(keyText(k), false))
			b.WriteByte(':')
			compact(b, x.Value(k))
		}
		b.WriteByte('}')
	default:
		b.WriteString(pyjson.Dumps(v, false))
	}
}

// ReadAll is list_all(): every row read as a pathway, in table order.
// Row ranges are read and checked on separate connections at once. keep
// names the pathways whose embedding a command will print; the others
// drop its text once it is checked. The error is the first row's in
// table order, as Python's loop meets it.
func (s *Store) ReadAll(keep func(id string) bool) ([]*Pathway, error) {
	type result struct {
		p   *Pathway
		err error
	}
	var mu sync.Mutex
	byPart := map[int][]result{}
	_, err := s.scanParts("*", func(part int, rows *sql.Rows) error {
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		var out []result
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			r := make(Row, len(cols))
			for i, c := range cols {
				r[c] = vals[i]
			}
			s.asMigrated(r)
			p, err := FromRow(r)
			if err == nil && (keep == nil || !keep(p.ID())) {
				p.embText = ""
			}
			out = append(out, result{p, err})
		}
		mu.Lock()
		byPart[part] = out
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	var all []*Pathway
	for k := 0; k < len(byPart); k++ {
		for _, r := range byPart[k] {
			if r.err != nil {
				return nil, r.err
			}
			all = append(all, r.p)
		}
	}
	return all, nil
}

// keyText is a dict key as pydantic's JSON writes it: a lone surrogate
// becomes its three UTF-8-style bytes, which read back as three U+FFFD.
func keyText(k string) string {
	if !pystr.HasSurrogate(k) {
		return k
	}
	var b strings.Builder
	for i := 0; i < len(k); {
		r, n := pystr.DecodeRune(k[i:])
		if pystr.IsSurrogate(r) {
			b.WriteString("\uFFFD\uFFFD\uFFFD")
		} else {
			b.WriteString(k[i : i+n])
		}
		i += n
	}
	return b.String()
}
