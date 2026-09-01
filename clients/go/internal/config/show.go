package config

import (
	"errors"
	"os"
	"sort"

	"daisugi-verify/internal/pyjson"
)

// Row is config.ResolvedField: one setting as the running code sees it,
// and where it came from.
type Row struct{ Key, Value, Source string }

// AttributeError is load_config on a file whose top level is truthy but
// not a mapping: `raw.items()` raises. The text names the type as Python
// does ('list' object has no attribute 'items').
type AttributeError struct{ Type string }

func (e *AttributeError) Error() string {
	return "AttributeError: '" + e.Type + "' object has no attribute 'items'"
}

func kindName(k Kind) string {
	switch k {
	case Bool:
		return "bool"
	case Int:
		return "int"
	case Float:
		return "float"
	case Str:
		return "str"
	case Seq:
		return "list"
	}
	return "dict"
}

// pyStr is str() of a value dumped holds.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case pyjson.Int:
		return x.Text
	case string:
		return x
	}
	return ""
}

// Rows is the Config part of resolved_config(path): every field in
// model_fields order with str() of its value and "file" or "default", a
// nested group as its leaves; and unknown_config_keys(path), sorted. A
// file load_config raises on is ErrInvalid, or an *AttributeError; a file
// outside the modeled YAML is ErrUnsupported. cfg is load_config(path).
func Rows(path, home string) (rows []Row, unknown []string, cfg Config, err error) {
	doc := &Doc{Vals: map[string]Value{}}
	raw, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		d, perr := Parse(string(raw))
		var top *TopLevelError
		if errors.As(perr, &top) {
			if _, lerr := Load(path); lerr != nil {
				if errors.Is(lerr, ErrInvalid) {
					return nil, nil, Config{}, &AttributeError{kindName(top.V.Kind)}
				}
				return nil, nil, Config{}, lerr
			}
			// A falsy scalar: an empty config, and no raw keys.
		} else if perr != nil {
			return nil, nil, Config{}, perr
		} else {
			doc = d
		}
	case os.IsNotExist(rerr):
	default:
		return nil, nil, Config{}, ErrUnsupported
	}
	if cfg, err = Load(path); err != nil {
		var top *TopLevelError
		if !errors.As(err, &top) {
			return nil, nil, Config{}, err
		}
	}
	if HasNonStrKeys(path) {
		return nil, nil, Config{}, ErrUnsupported
	}
	defs := defaults(home)
	for _, f := range Fields {
		v, present := doc.Vals[f.Name]
		if f.Type == tFloor {
			var sub map[string]Value
			if present {
				if v.NonStrKeys > 0 {
					return nil, nil, Config{}, ErrUnsupported
				}
				sub = v.Map
			}
			def := defs["floor"].(*pyjson.Object)
			for _, ff := range FloorFields {
				val, source := pyStr(def.Value(ff.Name)), "default"
				if sv, has := sub[ff.Name]; has {
					d, derr := dumped(sv, ff.Type)
					if derr != nil {
						return nil, nil, Config{}, derr
					}
					val, source = pyStr(d), "file"
				}
				rows = append(rows, Row{"floor." + ff.Name, val, source})
			}
			if present && v.Kind == Map {
				for _, k := range v.Keys {
					if !floorField(k) {
						unknown = append(unknown, "floor."+k)
					}
				}
			}
			continue
		}
		val, source := defs[f.Name], "default"
		if present {
			d, derr := dumped(v, f.Type)
			if derr != nil {
				return nil, nil, Config{}, derr
			}
			val, source = d, "file"
		}
		s := pyStr(val)
		if f.Name == "llm_backend" && val == nil {
			s = "auto"
		}
		rows = append(rows, Row{f.Name, s, source})
	}
	for _, k := range doc.Keys {
		if !configField(k) {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return rows, unknown, cfg, nil
}

func configField(k string) bool {
	for _, f := range Fields {
		if f.Name == k {
			return true
		}
	}
	return false
}

func floorField(k string) bool {
	for _, f := range FloorFields {
		if f.Name == k {
			return true
		}
	}
	return false
}

// HasNonStrKeys is whether the file's top level has a key that is not a
// string: unknown_config_keys would name it by its str(), which this
// package does not keep.
func HasNonStrKeys(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	v, err := ParseYAML(string(raw))
	return err == nil && v.Kind == Map && v.NonStrKeys > 0
}
