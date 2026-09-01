// Command gateway-probe is a test instrument for clients/gateway_compare.py
// --fuzz: it runs turn sequences through the gateway pipeline in process,
// the way the proxy runs each turn, and prints what the oracle's fuzz
// driver prints. Each input line is one sequence: a gateway config and
// its turns (the request body, the usage the upstream reported, the
// answer text, whether the fail-open retry served it, the target an
// external router named).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

type seq struct {
	Config struct {
		Mode, Cheap string
		Local       *string
		Capture     bool
		OpenAI      bool `json:"openai"`
		External    *struct {
			RouteID   string `json:"route_id"`
			Capable   string
			Efficient string
			Prices    map[string][2]float64
		}
	}
	Turns []struct {
		Body     string
		Usage    string
		Answer   string
		Original bool
		Served   *string
	}
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<30)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	base := os.Getenv("FUZZ_DIR")
	n := 0
	for in.Scan() {
		var s seq
		if err := json.Unmarshal(in.Bytes(), &s); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		n++
		dir, _ := os.MkdirTemp(base, "probe")
		c := s.Config
		g := gateway.Gateway{CheapModel: c.Cheap, RouterMode: c.Mode,
			JournalPath: filepath.Join(dir, "gateway", "turns.jsonl")}
		if c.Local != nil {
			g.LocalModel = *c.Local
		}
		if c.Capture {
			g.CaptureAnswers, g.AnswersPath = true, filepath.Join(dir, "gateway", "answers.jsonl")
		}
		if c.External != nil {
			pr := gateway.Prices{}
			for k, v := range c.External.Prices {
				pr[k] = gateway.Price{In: v[0], Out: v[1]}
			}
			g.External = &gateway.External{RouteID: c.External.RouteID, CapableTarget: c.External.Capable,
				EfficientTarget: c.External.Efficient, Prices: pr}
		}
		gw, err := gateway.NewGateway(g)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		srv := &gateway.Server{Now: time.Now}
		var results []any
		for _, t := range s.Turns {
			outbound, prepared, stream := gateway.PrepareBytes(gw, []byte(t.Body))
			usage, derr := pyjson.LoadsPy(t.Usage, 9990)
			if derr != nil {
				fmt.Fprintln(os.Stderr, derr)
				os.Exit(1)
			}
			if c.OpenAI {
				usage = gateway.NormalizeOpenAIUsage(usage)
			}
			before := lines(g.JournalPath)
			srv.Record(gw, prepared, usage, t.Original && prepared != nil && prepared.Decision.Downgraded, t.Answer, t.Served)
			after := lines(g.JournalPath)
			recs := []any{}
			for _, ln := range after[len(before):] {
				v, _ := pyjson.LoadsPy(ln, 9990)
				o := v.(*pyjson.Object)
				o2 := pyjson.NewObject()
				for _, k := range o.Keys() {
					switch v := o.Value(k); {
					case k == "created_at":
					case k == "elapsed_ms" && isNum(v):
						// A turn's time differs run to run; only its type is compared.
						o2.Set(k, "num")
					default:
						o2.Set(k, v)
					}
				}
				recs = append(recs, o2)
			}
			ans := lines(g.AnswersPath)
			var last any
			if len(ans) > 0 {
				v, _ := pyjson.LoadsPy(ans[len(ans)-1], 9990)
				o := v.(*pyjson.Object)
				o2 := pyjson.NewObject()
				for _, k := range o.Keys() {
					if k != "created_at" {
						o2.Set(k, o.Value(k))
					}
				}
				last = o2
			}
			r := pyjson.NewObject().Set("outbound", pystr.DecodeReplace(outbound)).Set("prepared", prepared != nil).
				Set("stream", stream).Set("records", recs).Set("answers", len(ans)).Set("last_answer", last)
			results = append(results, r)
		}
		fmt.Fprintln(out, pyjson.Dumps(pyjson.NewObject().Set("results", results), true))
		out.Flush()
		os.RemoveAll(dir)
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func lines(path string) []string {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

func isNum(v any) bool {
	switch v.(type) {
	case pyjson.Float, pyjson.Int:
		return true
	}
	return false
}
