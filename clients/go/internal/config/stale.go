package config

import "os"

// RenamedKeys are the six config keys the rename moved, longest old name
// first (config.RENAMED_CONFIG_KEYS). The old names are read on purpose.
var RenamedKeys = [][2]string{
	{"gateway_router_model", "routing_model"},     // names-ok: old key
	{"gateway_local_model", "router_local_model"}, // names-ok: old key
	{"gateway_router", "routing_mode"},            // names-ok: old key
	{"gateway_grader", "router_grader"},           // names-ok: old key
	{"gate_mode", "warden_mode"},                  // names-ok: old key
	{"gate_ask", "warden_ask"},                    // names-ok: old key
}

// StaleKeys is config.stale_config_keys: the (old, new) pairs for each
// renamed key still at the top level of the file. A file that does not read
// or is not a mapping names none.
func StaleKeys(path string) [][2]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	doc, err := Parse(string(raw))
	if err != nil {
		return nil
	}
	var out [][2]string
	for _, k := range RenamedKeys {
		if _, ok := doc.Vals[k[0]]; ok {
			out = append(out, k)
		}
	}
	return out
}
