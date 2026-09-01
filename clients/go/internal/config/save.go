package config

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pyyaml"
)

// This file is save_config(load_config(path).model_copy(update=...)):
// the whole Config, every field as pydantic dumps it in JSON mode, keys
// sorted, written with yaml.safe_dump. A value this package cannot turn
// into the form pydantic would dump is ErrUnsupported: the caller then
// refuses before it writes anything.

// defaults is Config() as model_dump(mode="json") gives it; data_dir is
// filled from the home directory.
func defaults(home string) map[string]any {
	null := any(nil)
	return map[string]any{
		"model": "anthropic/claude-sonnet-4-20250514", "max_task_chars": pyjson.Int{Text: "4000"},
		"z3_timeout_ms": pyjson.Int{Text: "500"}, "data_dir": gateroot.PathStr(filepath.Join(home, ".opendaisugi")),
		"auto_tend": null, "gateway_local_model": null, "gateway_router": "rules", "switchyard_route_id": "daisugi",
		"switchyard_capable_model": "claude-sonnet-5", "switchyard_efficient_model": null,
		"switchyard_api_key_env": null, "shell_allow_decomposition": false, "gate_mode": "audit", "gate_ask": false,
		"dialect_enforce": null,
		"verifier_client": "python", "matcher_model": "lexical", "llm_backend": null, "llm_base_url": null,
		"llm_host_kind": null, "llm_host_model": null, "llm_context_window": null,
		"envelope_source": "evidence-inferred", "pathway_store_backend": "sqlite", "floor_report": null,
		"floor":        floorObject(map[string]any{"backend": "auto", "notify_cmd": null, "tmux_socket": null, "coppice_socket": null}),
		"voice_engine": "faster-whisper", "voice_model": "tiny.en", "voice_device": "cpu", "voice_compute_type": "int8",
		"voice_cleanup": false, "voice_cleanup_model": null, "voice_cleanup_base_url": null,
		"voice_server_url": "http://127.0.0.1:7477",
	}
}

func floorObject(m map[string]any) *pyjson.Object {
	o := pyjson.NewObject()
	for _, k := range []string{"backend", "coppice_socket", "notify_cmd", "tmux_socket"} {
		o.Set(k, m[k])
	}
	return o
}

// dumped is the value pydantic keeps for v in a field of type t.
func dumped(v Value, t fieldType) (any, error) {
	if v.Kind == Null {
		return nil, nil
	}
	switch t {
	case tStr, tOptStr:
		return v.Text, nil
	case tPath:
		return gateroot.PathStr(v.Text), nil
	case tInt, tOptInt:
		switch v.Kind {
		case Int:
			n, ok := new(big.Int).SetString(strings.ReplaceAll(v.Text, "_", ""), 10)
			if !ok {
				return nil, ErrUnsupported
			}
			return pyjson.Int{Text: n.String()}, nil
		case Bool:
			if v.B {
				return pyjson.Int{Text: "1"}, nil
			}
			return pyjson.Int{Text: "0"}, nil
		case Float:
			f, err := strconv.ParseFloat(v.Text, 64)
			if err != nil || f != float64(int64(f)) {
				return nil, ErrUnsupported
			}
			return pyjson.Int{Text: strconv.FormatInt(int64(f), 10)}, nil
		case Str:
			s := strings.ReplaceAll(strings.TrimSpace(v.Text), "_", "")
			if reIntFloat().MatchString(s) {
				s = s[:strings.IndexByte(s, '.')]
			}
			n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "+"), 10)
			if !ok {
				return nil, ErrUnsupported
			}
			return pyjson.Int{Text: n.String()}, nil
		}
	case tBool, tOptBool:
		return boolOf(v), nil
	case tFloor:
		m := map[string]any{"backend": "auto", "notify_cmd": nil, "tmux_socket": nil, "coppice_socket": nil}
		for _, f := range FloorFields {
			if sub, present := v.Map[f.Name]; present {
				x, err := dumped(sub, f.Type)
				if err != nil {
					return nil, err
				}
				m[f.Name] = x
			}
		}
		return floorObject(m), nil
	}
	return nil, ErrUnsupported
}

// Save writes load_config(path).model_copy(update=updates) to path as
// save_config does. updates holds str values, or nil for None.
func Save(path, home string, updates map[string]any) error {
	text, err := Dump(path, home, updates)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	_, err = gateroot.WriteFile(path, text)
	return err
}

// Dump is the text Save would write.
func Dump(path, home string, updates map[string]any) (string, error) {
	vals := defaults(home)
	raw, err := os.ReadFile(path)
	if err == nil {
		doc, perr := Parse(string(raw))
		var top *TopLevelError
		switch {
		case errors.As(perr, &top):
			if _, lerr := Load(path); lerr != nil {
				return "", lerr
			}
		case perr != nil:
			return "", perr
		default:
			if _, err := FromDoc(doc); err != nil {
				return "", err
			}
			for _, f := range Fields {
				v, present := doc.Vals[f.Name]
				if !present {
					continue
				}
				x, err := dumped(v, f.Type)
				if err != nil {
					return "", err
				}
				vals[f.Name] = x
			}
		}
	} else if !os.IsNotExist(err) {
		return "", ErrUnsupported
	}
	for k, v := range updates {
		vals[k] = v
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	o := pyjson.NewObjectCap(len(keys))
	for _, k := range keys {
		o.Set(k, vals[k])
	}
	text, why := pyyaml.SafeDump(o)
	if why != nil {
		return "", ErrUnsupported
	}
	return text, nil
}
