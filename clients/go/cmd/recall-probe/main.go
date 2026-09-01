// Command recall-probe is a test instrument for clients/gateway_compare.py:
// it runs internal/recall on one query per input line and prints the
// result the way the oracle's MCP tools return it. A line is
// {"kind": "recall", "db", "matcher", "task", "envelope", "z3_timeout_ms"}
// or {"kind": "answer", "answers", "matcher", "task", "now",
// "max_age_seconds", "ground_hash"}.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/recall"
)

type query struct {
	Kind, DB, Matcher, Task, Answers string
	Envelope                         json.RawMessage
	Z3                               int      `json:"z3_timeout_ms"`
	Now                              float64  `json:"now"`
	MaxAge                           *float64 `json:"max_age_seconds"`
	Ground                           *string  `json:"ground_hash"`
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<28)
	pe := potion.Env{Lookup: os.LookupEnv, Home: os.Getenv("HOME"), Notice: func(string) {}}
	// The model client that binds a typed pathway's holes, over this
	// process's environment, as the oracle's recall makes one.
	lc := llm.New(llm.Env{Getenv: os.LookupEnv, Environ: os.Environ(), Home: os.Getenv("HOME"),
		Stdout: os.Stderr, Stderr: os.Stderr})
	for in.Scan() {
		var q query
		if err := json.Unmarshal(in.Bytes(), &q); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		m, err := pathways.SelectMatcher(q.Matcher, pe)
		if err != nil || m == nil {
			fmt.Fprintln(os.Stderr, "matcher:", q.Matcher, err)
			os.Exit(1)
		}
		out := pyjson.NewObject()
		switch q.Kind {
		case "recall":
			s, err := pathways.OpenReadOnly(q.DB)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			r, err := recall.Recall(s, m.Key, pe, q.Task, q.Envelope, q.Z3, lc, recall.DefaultModel)
			s.Close()
			if err != nil {
				out.Set("error", err.Error())
				break
			}
			out.Set("hit", r.Hit)
			setStr(out, "reason", r.Reason)
			out.Set("plan", r.Plan)
			if r.Provenance != nil {
				p := r.Provenance
				out.Set("provenance", pyjson.NewObject().Set("pathway_id", p.PathwayID).Set("similarity", p.Similarity).
					Set("tier", p.Tier).Set("source_trace_count", p.SourceTraceCount).Set("distilled_at", p.DistilledAt).
					Set("hit_count", p.HitCount))
			} else {
				out.Set("provenance", nil)
			}
		case "answer":
			entries, err := gateway.LoadAnswers(q.Answers)
			if err != nil {
				out.Set("error", err.Error())
				break
			}
			emb, err := m.Embedder()
			if err != nil {
				out.Set("error", err.Error())
				break
			}
			maxAge := recall.DefaultMaxAge
			if q.MaxAge != nil {
				maxAge = *q.MaxAge
			}
			r, err := recall.RecallAnswer(q.Task, entries, q.Now, emb, m.Threshold, maxAge, q.Ground)
			if err != nil {
				out.Set("error", err.Error())
				break
			}
			out.Set("hit", r.Hit)
			setStr(out, "reason", r.Reason)
			out.Set("answer", r.Answer)
			if r.Provenance != nil {
				p := r.Provenance
				out.Set("provenance", pyjson.NewObject().Set("similarity", p.Similarity).Set("age_seconds", p.AgeSeconds).
					Set("created_at", p.CreatedAt).Set("ground_hash", p.GroundHash))
			} else {
				out.Set("provenance", nil)
			}
		}
		fmt.Println(pyjson.Dumps(out, true))
	}
}

func setStr(o *pyjson.Object, k, v string) {
	if v == "" {
		o.Set(k, nil)
	} else {
		o.Set(k, v)
	}
}
