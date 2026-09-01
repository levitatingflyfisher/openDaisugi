package pmodel

import "daisugi-verify/internal/pyjson"

// This file holds config.Config and config.FloorConfig
// (opendaisugi/config.py). The gate reads two fields, but load_config
// validates every field, so one bad field anywhere fails the whole load.

// Path is pathlib.Path: a str, the only path-like value a YAML load gives.
type Path struct{}

func (Path) validate(v any, loc []any, mode Mode) (any, []Err) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return nil, one(loc, "path_type", "Input is not a valid path", v)
}

// FloorConfig is config.FloorConfig.
var FloorConfig = &Model{Name: "FloorConfig", Fields: []Field{
	{Name: "backend", Schema: Str{}, Default: constant("auto")},
	{Name: "notify_cmd", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "tmux_socket", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "coppice_socket", Schema: Nullable{Str{}}, Default: constant(nil)},
}}

func floorDefault() any {
	out, _ := FloorConfig.fields(pyjson.NewObject(), nil, Python)
	return out
}

// Config is config.Config. data_dir's default (the home's .opendaisugi)
// is left nil here: the gate never reads it.
var Config = &Model{Name: "Config", Fields: []Field{
	{Name: "model", Schema: Str{}, Default: constant("anthropic/claude-sonnet-4-20250514")},
	{Name: "max_task_chars", Schema: Int{}, Default: constant(pyjson.Int{Text: "4000"})},
	{Name: "z3_timeout_ms", Schema: Int{}, Default: constant(pyjson.Int{Text: "500"})},
	{Name: "data_dir", Schema: Path{}, Default: constant(nil)},
	{Name: "auto_tend", Schema: Nullable{Bool{}}, Default: constant(nil)},
	{Name: "gateway_local_model", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "gateway_router", Schema: Str{}, Default: constant("rules")},
	{Name: "switchyard_route_id", Schema: Str{}, Default: constant("daisugi")},
	{Name: "switchyard_capable_model", Schema: Str{}, Default: constant("claude-sonnet-5")},
	{Name: "switchyard_efficient_model", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "switchyard_api_key_env", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "shell_allow_decomposition", Schema: Bool{}, Default: constant(false)},
	{Name: "gate_mode", Schema: Str{}, Default: constant("shadow")},
	{Name: "gate_ask", Schema: Bool{}, Default: constant(false)},
	{Name: "verifier_client", Schema: Str{}, Default: constant("python")},
	{Name: "matcher_model", Schema: Str{}, Default: constant("all-MiniLM-L6-v2")},
	{Name: "llm_backend", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "llm_base_url", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "llm_host_kind", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "llm_host_model", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "llm_context_window", Schema: Nullable{Int{}}, Default: constant(nil)},
	{Name: "envelope_source", Schema: Str{}, Default: constant("evidence-inferred")},
	{Name: "pathway_store_backend", Schema: Str{}, Default: constant("sqlite")},
	{Name: "floor_report", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "floor", Schema: FloorConfig, Default: floorDefault},
	{Name: "voice_engine", Schema: Str{}, Default: constant("faster-whisper")},
	{Name: "voice_model", Schema: Str{}, Default: constant("tiny.en")},
	{Name: "voice_device", Schema: Str{}, Default: constant("cpu")},
	{Name: "voice_compute_type", Schema: Str{}, Default: constant("int8")},
	{Name: "voice_cleanup", Schema: Bool{}, Default: constant(false)},
	{Name: "voice_cleanup_model", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "voice_cleanup_base_url", Schema: Nullable{Str{}}, Default: constant(nil)},
	{Name: "voice_server_url", Schema: Str{}, Default: constant("http://127.0.0.1:7477")},
}}
