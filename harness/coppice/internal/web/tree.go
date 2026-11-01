package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// The delegation tree on the floor: the nodes, their budgets and the asks
// that need a person. The tree is daisugi's; the floor shows it and
// carries the operator's answer.

func (s *Server) treeRoutes() {
	s.mux.HandleFunc("GET /api/tree", s.guard(s.handleTree))
	s.mux.HandleFunc("POST /api/tree/answer", s.guardAnswer(s.handleTreeAnswer))
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	// The configured data dir and the steward's work dir: a steward branch
	// run puts its asks in the work dir, and the floor is where the operator
	// looks for them. A node or an ask shows once, whichever dir holds it.
	var doc map[string]any
	seen := map[string]bool{}
	for i, dir := range s.dataSources() {
		d, ok := s.daisugiJSONIn(w, r, i == 0, dir, "daisugi tree status", "tree", "status", "--json")
		if !ok {
			if i == 0 {
				return
			}
			continue
		}
		lists := []struct{ key, id string }{{"nodes", "session"}, {"asks", "ask_id"}}
		got := map[string][]any{}
		for _, list := range lists {
			got[list.key], _ = d[list.key].([]any)
		}
		if doc == nil {
			doc = d
			doc["nodes"], doc["asks"] = []any{}, []any{}
		} else if n, ok := d["skipped"].(float64); ok {
			prev, _ := doc["skipped"].(float64)
			doc["skipped"] = prev + n
		}
		for _, list := range lists {
			items := got[list.key]
			for _, it := range items {
				m, ok := it.(map[string]any)
				if !ok {
					continue
				}
				id, _ := m[list.id].(string)
				if id != "" && seen[list.key+"/"+id] {
					continue
				}
				seen[list.key+"/"+id] = true
				m["data_dir"] = dataLabel(dir)
				doc[list.key] = append(doc[list.key].([]any), m)
			}
		}
	}
	doc["source"] = "daisugi tree status"
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleTreeAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AskID    string `json:"ask_id"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "The request body is not JSON.")
		return
	}
	if !s.operatorOnly(w, r) {
		return
	}
	if !idPattern.MatchString(body.AskID) {
		writeErr(w, http.StatusBadRequest, "ask_id is not an ask id.")
		return
	}
	if body.Decision != "allow" && body.Decision != "deny" {
		writeErr(w, http.StatusBadRequest, "decision is allow or deny.")
		return
	}
	// The ask lives in the dir that holds it: try each, and stop at the one
	// that knows it (exit 1 is "no such ask").
	var stdout, stderr string
	var code int
	var err error
	for _, dir := range s.dataSources() {
		stdout, stderr, code, err = runDaisugi(r.Context(), daisugiArgsIn(dir, "tree", "answer", body.AskID, body.Decision)...)
		if !(err == nil && code == 1) {
			break
		}
	}
	switch {
	case err != nil || code < 0 || code > 1:
		why := firstLine(stderr)
		if err != nil {
			why = err.Error()
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "daisugi tree answer failed: " + why})
		return
	case code == 1:
		writeErr(w, http.StatusNotFound, strings.TrimSpace(stderr))
		return
	}
	reply := map[string]any{}
	if json.Unmarshal([]byte(stdout), &reply) != nil || reply == nil {
		reply = map[string]any{"output": strings.TrimSpace(stdout)}
	}
	writeJSON(w, http.StatusOK, reply)
}
