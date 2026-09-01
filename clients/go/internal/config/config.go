package config

import (
	"daisugi-verify/internal/lazyre"
	"errors"
	"os"
	"strconv"
	"strings"
)

// fieldType is how pydantic validates one Config field.
type fieldType int

const (
	tStr fieldType = iota
	tOptStr
	tInt
	tOptInt
	tBool
	tOptBool
	tPath
	tFloor
)

// Fields is opendaisugi.config.Config.model_fields, in order, with each
// field's type. testdata/config_fields.json holds the list Python reports;
// a test keeps the two equal, since a field Python validates and this
// table does not know would be read as valid here.
var Fields = []struct {
	Name string
	Type fieldType
}{
	{"model", tStr},
	{"max_task_chars", tInt},
	{"z3_timeout_ms", tInt},
	{"data_dir", tPath},
	{"auto_tend", tOptBool},
	{"gateway_local_model", tOptStr},
	{"gateway_router", tStr},
	{"switchyard_route_id", tStr},
	{"switchyard_capable_model", tStr},
	{"switchyard_efficient_model", tOptStr},
	{"switchyard_api_key_env", tOptStr},
	{"shell_allow_decomposition", tBool},
	{"gate_mode", tStr},
	{"gate_ask", tBool},
	{"verifier_client", tStr},
	{"matcher_model", tStr},
	{"llm_backend", tOptStr},
	{"llm_base_url", tOptStr},
	{"llm_host_kind", tOptStr},
	{"llm_host_model", tOptStr},
	{"llm_context_window", tOptInt},
	{"envelope_source", tStr},
	{"pathway_store_backend", tStr},
	{"floor_report", tOptStr},
	{"floor", tFloor},
	{"voice_engine", tStr},
	{"voice_model", tStr},
	{"voice_device", tStr},
	{"voice_compute_type", tStr},
	{"voice_cleanup", tBool},
	{"voice_cleanup_model", tOptStr},
	{"voice_cleanup_base_url", tOptStr},
	{"voice_server_url", tStr},
}

// FloorFields is FloorConfig.model_fields.
var FloorFields = []struct {
	Name string
	Type fieldType
}{
	{"backend", tStr},
	{"notify_cmd", tOptStr},
	{"tmux_socket", tOptStr},
	{"coppice_socket", tOptStr},
}

// Config holds the fields the gate commands read.
type Config struct {
	GateMode                string
	ShellAllowDecomposition bool
	VerifierClient          string
	MatcherModel            string
	// LLMBackend is llm_backend, nil when unset or null.
	LLMBackend *string
	// AutoTend is auto_tend, nil when unset or null.
	AutoTend *bool
}

// Default is Config() with every default.
func Default() Config {
	return Config{GateMode: "shadow", VerifierClient: "python", MatcherModel: "all-MiniLM-L6-v2"}
}

// verdict of one value against one field type.
type verdict int

const (
	ok verdict = iota
	invalid
	unsure
)

var (
	reIntStr     = lazyre.New(`^[+-]?[0-9](?:_?[0-9])*$`)
	reIntFloat   = lazyre.New(`^[+-]?[0-9]+\.0+$`)
	reNumberish  = lazyre.New(`^[0-9+\-_.eE \t\n\r\f\v]*$`)
	boolStrTrue  = map[string]bool{"true": true, "yes": true, "on": true, "t": true, "y": true, "1": true}
	boolStrFalse = map[string]bool{"false": true, "no": true, "off": true, "f": true, "n": true, "0": true}
)

// intStr is pydantic's int from a string: surrounding whitespace dropped,
// underscores between digits allowed, and a float text with a zero
// fraction read as its integer.
func intStr(s string) verdict {
	t := strings.TrimSpace(s)
	if reIntStr().MatchString(t) || reIntFloat().MatchString(t) {
		return ok
	}
	if !reNumberish().MatchString(s) {
		return invalid
	}
	return unsure
}

// boolOf is pydantic's bool of a value check has passed.
func boolOf(v Value) bool {
	switch v.Kind {
	case Bool:
		return v.B
	case Int:
		return v.Text == "1"
	case Float:
		f, _ := strconv.ParseFloat(v.Text, 64)
		return f == 1
	case Str:
		return boolStrTrue[strings.ToLower(v.Text)]
	}
	return false
}

// check is pydantic's lax validation of one value for one field type,
// with the rules measured against pydantic in clients/cli_cases.py.
func check(v Value, t fieldType) verdict {
	optional := t == tOptStr || t == tOptInt || t == tOptBool
	if v.Kind == Null {
		if optional {
			return ok
		}
		return invalid
	}
	switch t {
	case tStr, tOptStr, tPath:
		if v.Kind == Str {
			return ok
		}
		return invalid
	case tInt, tOptInt:
		switch v.Kind {
		case Int, Bool:
			return ok
		case Float:
			f, err := strconv.ParseFloat(v.Text, 64)
			if err != nil {
				return unsure
			}
			if f == float64(int64(f)) {
				return ok
			}
			return invalid
		case Str:
			return intStr(v.Text)
		}
		return invalid
	case tBool, tOptBool:
		switch v.Kind {
		case Bool:
			return ok
		case Int:
			if v.Text == "0" || v.Text == "1" {
				return ok
			}
			return invalid
		case Float:
			f, err := strconv.ParseFloat(v.Text, 64)
			if err == nil && (f == 0 || f == 1) {
				return ok
			}
			return invalid
		case Str:
			l := strings.ToLower(v.Text)
			if boolStrTrue[l] || boolStrFalse[l] {
				return ok
			}
			return invalid
		}
		return invalid
	case tFloor:
		if v.Kind != Map {
			return invalid
		}
		if v.NonStrKeys > 0 {
			return unsure
		}
		worst := ok
		for _, f := range FloorFields {
			if sub, present := v.Map[f.Name]; present {
				if r := check(sub, f.Type); r > worst {
					worst = r
				}
			}
		}
		return worst
	}
	return unsure
}

// FromDoc validates a parsed file as Config(**raw) does.
func FromDoc(doc *Doc) (Config, error) {
	cfg := Default()
	anyInvalid, anyUnsure := false, false
	for _, f := range Fields {
		v, present := doc.Vals[f.Name]
		if !present {
			continue
		}
		switch check(v, f.Type) {
		case invalid:
			anyInvalid = true
		case unsure:
			anyUnsure = true
		}
	}
	// One invalid field makes pydantic raise, whatever the others hold.
	if anyInvalid {
		return Config{}, ErrInvalid
	}
	if anyUnsure {
		return Config{}, ErrUnsupported
	}
	if v, present := doc.Vals["gate_mode"]; present {
		cfg.GateMode = v.Text
	}
	if v, present := doc.Vals["verifier_client"]; present {
		cfg.VerifierClient = v.Text
	}
	if v, present := doc.Vals["matcher_model"]; present {
		cfg.MatcherModel = v.Text
	}
	if v, present := doc.Vals["shell_allow_decomposition"]; present {
		cfg.ShellAllowDecomposition = boolOf(v)
	}
	if v, present := doc.Vals["llm_backend"]; present && v.Kind == Str {
		s := v.Text
		cfg.LLMBackend = &s
	}
	if v, present := doc.Vals["auto_tend"]; present && v.Kind != Null {
		b := boolOf(v)
		cfg.AutoTend = &b
	}
	return cfg, nil
}

// Load is load_config(path): the defaults when the file is absent.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Path.exists() is false, a dangling symlink included.
			return Default(), nil
		}
		return Config{}, ErrUnsupported
	}
	doc, err := Parse(string(raw))
	var top *TopLevelError
	if errors.As(err, &top) {
		// `yaml.safe_load(...) or {}`: a falsy scalar is an empty
		// config; anything else has no .items() and raises.
		v := top.V
		falsy := (v.Kind == Bool && !v.B) || (v.Kind == Int && strings.TrimLeft(strings.TrimPrefix(v.Text, "-"), "0") == "") ||
			(v.Kind == Str && v.Text == "") || (v.Kind == Seq && len(v.Items) == 0)
		if v.Kind == Float {
			f, _ := strconv.ParseFloat(v.Text, 64)
			falsy = f == 0
		}
		if falsy {
			return Default(), nil
		}
		return Config{}, ErrInvalid
	}
	if err != nil {
		return Config{}, err
	}
	return FromDoc(doc)
}

// GateMode is gate.resolve_gate_mode(None, root=...): the gate_mode of
// the config.yaml beside the gate root, or shadow when that file does
// not validate or names another mode. err is ErrUnsupported only.
func GateMode(configPath string) (string, error) {
	cfg, err := Load(configPath)
	if errors.Is(err, ErrInvalid) {
		return "shadow", nil
	}
	if err != nil {
		return "", err
	}
	if cfg.GateMode == "shadow" || cfg.GateMode == "enforce" {
		return cfg.GateMode, nil
	}
	return "shadow", nil
}
