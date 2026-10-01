// Command envelope-probe is a test instrument for clients/k1_compare.py:
// it runs one query of internal/envgen (or internal/recall's bind path)
// and prints the result as one JSON line, the way the oracle's script in
// clients/k1_cases.py prints its own. The query is argv[1]; its "kind" is
// generate, inherit, bind, compose or recall. A path this binary does not
// take prints {"unported": reason}.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/datahome"
	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/recall"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

type query struct {
	Kind         string          `json:"kind"`
	Task         string          `json:"task"`
	Context      *string         `json:"context"`
	Summarize    bool            `json:"summarize"`
	Stakes       string          `json:"stakes"`
	LowStakes    json.RawMessage `json:"low_stakes"`
	Model        json.RawMessage `json:"model"`
	Thinking     string          `json:"thinking"`
	MaxRetries   *int            `json:"max_retries"`
	MaxTaskChars *int            `json:"max_task_chars"`
	Cache        *string         `json:"cache"`
	Pathways     *string         `json:"pathways"`
	Threshold    *float64        `json:"threshold"`
	Journal      *string         `json:"journal"`
	Tier1        json.RawMessage `json:"tier1"`
	Parent       json.RawMessage `json:"parent"`
	Child        json.RawMessage `json:"child"`
	DB           string          `json:"db"`
	ID           string          `json:"id"`
	Envelope     json.RawMessage `json:"envelope"`
	Z3           int             `json:"z3_timeout_ms"`
	SkillIDs     []string        `json:"skill_ids"`
	Executors    []string        `json:"executors"`
	Plan         json.RawMessage `json:"plan"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: envelope-probe QUERY-JSON")
		os.Exit(2)
	}
	var q query
	if err := json.Unmarshal([]byte(os.Args[1]), &q); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	home := os.Getenv("HOME")
	c := llm.New(llm.Env{Getenv: os.LookupEnv, Environ: os.Environ(), Home: home, Stdout: os.Stderr, Stderr: os.Stderr})
	var out *pyjson.Object
	var err error
	switch q.Kind {
	case "generate":
		out, err = generate(q, c, home)
	case "inherit":
		out, err = inherit(q)
	case "bind":
		out, err = bind(q, c)
	case "compose":
		out, err = compose(q)
	case "recall":
		out, err = doRecall(q, c, home)
	default:
		err = fmt.Errorf("unknown kind %q", q.Kind)
	}
	if errors.Is(err, envgen.ErrUnported) {
		out = pyjson.NewObject().Set("unported", err.Error())
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "envelope-probe:", err)
		os.Exit(1)
	}
	fmt.Println(pyjson.Dumps(out, true))
}

func validate(title string, m *pmodel.Model, raw json.RawMessage) (*pyjson.Object, error) {
	v, verr := pmodel.ValidateJSON(title, m, string(raw))
	if verr != nil {
		return nil, fmt.Errorf("%s: %s", title, verr.String())
	}
	return v.(*pyjson.Object), nil
}

func null(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func matcherKey(home string) (string, error) {
	cfg, err := config.Load(filepath.Join(datahome.Dir(os.Getenv, home, datahome.Exists), "config.yaml"))
	if err != nil {
		return "", err
	}
	return cfg.MatcherModel, nil
}

func pyErr(err error) (*pyjson.Object, error) {
	var pe *envgen.PyError
	var ie *envgen.InheritanceError
	var ce *envgen.ErrCacheRow
	switch {
	case errors.As(err, &pe):
		return pyjson.NewObject().Set("error", pe.Class).Set("message", pe.Msg), nil
	case errors.As(err, &ie):
		return pyjson.NewObject().Set("error", "EnvelopeInheritanceError").Set("message", ie.Error()), nil
	case errors.As(err, &ce):
		return pyjson.NewObject().Set("error", "ValidationError").Set("message", ""), nil
	}
	return nil, err
}

func generate(q query, c *llm.Client, home string) (*pyjson.Object, error) {
	o := envgen.Options{Task: q.Task, Context: q.Context, Summarize: q.Summarize, Stakes: q.Stakes,
		Thinking: q.Thinking, Threshold: q.Threshold, LLM: c, MaxRetries: 3, MaxTaskChars: 4000}
	if q.MaxRetries != nil {
		o.MaxRetries = *q.MaxRetries
	}
	if q.MaxTaskChars != nil {
		o.MaxTaskChars = *q.MaxTaskChars
	}
	if !null(q.Model) {
		var s string
		if json.Unmarshal(q.Model, &s) == nil {
			o.Models, o.Single = []string{s}, true
		} else if err := json.Unmarshal(q.Model, &o.Models); err != nil {
			return nil, err
		} else if o.Models == nil {
			o.Models = []string{}
		}
	}
	var err error
	if !null(q.LowStakes) {
		if o.LowStakes, err = validate("Envelope", pmodel.Envelope, q.LowStakes); err != nil {
			return nil, err
		}
	}
	if !null(q.Parent) {
		if o.Parent, err = validate("Envelope", pmodel.Envelope, q.Parent); err != nil {
			return nil, err
		}
	}
	if q.Cache != nil {
		if o.Cache, err = envgen.OpenCache(*q.Cache); err != nil {
			return nil, err
		}
	}
	if q.Pathways != nil {
		s, err := pathways.Open(*q.Pathways)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		o.Store = s
		if o.MatcherKey, err = matcherKey(home); err != nil {
			return nil, err
		}
		o.Potion = potion.Env{Lookup: os.LookupEnv, Environ: os.Environ(), Home: home, Notice: func(string) {}}
	}
	if q.Journal != nil {
		j, err := tracejournal.Open(*q.Journal)
		if err != nil {
			return nil, err
		}
		defer j.Close()
		o.Journal = j
	}
	if !null(q.Tier1) {
		var dir string
		if json.Unmarshal(q.Tier1, &dir) == nil {
			t, err := envgen.LoadConfiguredTier1(dir)
			if err != nil {
				out, err := pyErr(err)
				if out != nil {
					out.Set("warnings", []any{})
				}
				return out, err
			}
			o.Tier1 = t
		} else {
			var t struct {
				Model   string  `json:"model"`
				BaseURL *string `json:"base_url"`
				BaseEnv string  `json:"base_url_env"`
				APIKey  *string `json:"api_key"`
				Name    string  `json:"name"`
			}
			if err := json.Unmarshal(q.Tier1, &t); err != nil {
				return nil, err
			}
			if t.BaseEnv != "" {
				// The fake server's address, which only the environment knows.
				b := os.Getenv(t.BaseEnv)
				t.BaseURL = &b
			}
			o.Tier1 = envgen.NewTier1(t.Model, t.BaseURL, t.APIKey, t.Name)
		}
	}
	r, err := envgen.Generate(o)
	warnings := []any{}
	if r.FindWarning != "" {
		warnings = append(warnings, r.FindWarning)
	}
	if err != nil {
		out, err := pyErr(err)
		if out != nil {
			out.Set("warnings", warnings)
		}
		return out, err
	}
	return pyjson.NewObject().Set("envelope", r.Envelope).Set("warnings", warnings), nil
}

func inherit(q query) (*pyjson.Object, error) {
	child, err := validate("Envelope", pmodel.Envelope, q.Child)
	if err != nil {
		return nil, err
	}
	parent, err := validate("Envelope", pmodel.Envelope, q.Parent)
	if err != nil {
		return nil, err
	}
	msgs := []any{}
	for _, m := range envgen.VerifyInheritance(child, parent) {
		msgs = append(msgs, m)
	}
	return pyjson.NewObject().Set("violations", msgs), nil
}

func bind(q query, c *llm.Client) (*pyjson.Object, error) {
	s, err := pathways.OpenReadOnly(q.DB)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	row, err := s.Get(q.ID)
	if err != nil || row == nil {
		return nil, fmt.Errorf("no pathway %s: %v", q.ID, err)
	}
	p, err := pathways.FromRow(row)
	if err != nil {
		return nil, err
	}
	env, err := verify.ParseEnvelope(q.Envelope)
	if err != nil {
		return nil, err
	}
	model := envgen.DefaultModel
	if !null(q.Model) {
		_ = json.Unmarshal(q.Model, &model)
	}
	return pyjson.NewObject().Set("plan", envgen.Bind(c, p.Obj, q.Task, env, model, q.Z3)), nil
}

// echo is the probe's executor: "<type>:<the step's capability field>".
type echo struct{}

func (echo) Run(step *pyjson.Object, _, _ int) (string, error) {
	t, _ := step.Value("type").(string)
	f := map[string]string{"shell": "command", "file_read": "path", "file_write": "path", "network": "url"}[t]
	v, _ := step.Value(f).(string)
	return t + ":" + v, nil
}

func sortedKeys[V any](m map[string]V) []any {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	out := make([]any, len(ks))
	for i, k := range ks {
		out[i] = k
	}
	return out
}

func compose(q query) (*pyjson.Object, error) {
	s, err := pathways.OpenReadOnly(q.DB)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	contracts, err := envgen.ContractEnvelopes(s)
	if err != nil {
		return nil, err
	}
	executors := map[string]envgen.Executor{}
	for _, t := range q.Executors {
		executors[t] = echo{}
	}
	handlers, err := envgen.HandlersFor(s, q.SkillIDs, executors)
	if err != nil {
		return nil, err
	}
	runs := pyjson.NewObject()
	for _, k := range sortedKeys(handlers) {
		id := k.(string)
		o, err := handlers[id](nil)
		if err != nil {
			var pe *envgen.PyError
			if errors.As(err, &pe) {
				runs.Set(id, pyjson.NewObject().Set("error", pe.Class).Set("message", pe.Msg))
				continue
			}
			return nil, err
		}
		runs.Set(id, o)
	}
	// The plan, each skill step carrying its pathway's contract.
	pv, perr := pyjson.LoadsPy(string(q.Plan), 900)
	if perr != nil {
		return nil, perr
	}
	plan := pv.(*pyjson.Object)
	for _, st := range tracejournal.Steps(plan) {
		if st.Value("type") == "skill" {
			if env, ok := contracts[st.Value("skill_id").(string)]; ok {
				st.Set("contract_envelope", env)
			}
		}
	}
	p, err := verify.ParsePlan(json.RawMessage(pathways.DumpJSON(plan)))
	if err != nil {
		return nil, err
	}
	env, err := verify.ParseEnvelope(q.Envelope)
	if err != nil {
		return nil, err
	}
	res := verify.Verify(p, env, verify.VerifyOptions{Z3TimeoutMs: q.Z3})
	vs := []any{}
	for _, v := range res.Violations {
		var msg any
		if v.Stage != "delegation" {
			// A refused delegation is worded by each side (PW-12).
			msg = v.Message
		}
		vs = append(vs, []any{v.Stage, msg})
	}
	return pyjson.NewObject().Set("contracts", sortedKeys(contracts)).Set("handlers", sortedKeys(handlers)).
		Set("runs", runs).Set("ok", res.OK && len(res.Timeouts) == 0).Set("violations", vs), nil
}

func doRecall(q query, c *llm.Client, home string) (*pyjson.Object, error) {
	key, err := matcherKey(home)
	if err != nil {
		return nil, err
	}
	s, err := pathways.OpenReadOnly(q.DB)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	pe := potion.Env{Lookup: os.LookupEnv, Environ: os.Environ(), Home: home, Notice: func(string) {}}
	r, err := recall.Recall(s, key, pe, q.Task, q.Envelope, q.Z3, c, recall.DefaultModel)
	if err != nil {
		return nil, err
	}
	out := pyjson.NewObject().Set("hit", r.Hit)
	if r.Reason == "" {
		out.Set("reason", nil)
	} else {
		out.Set("reason", r.Reason)
	}
	out.Set("plan", r.Plan)
	if r.Provenance != nil {
		p := r.Provenance
		out.Set("provenance", pyjson.NewObject().Set("pathway_id", p.PathwayID).Set("similarity", p.Similarity).
			Set("tier", p.Tier).Set("source_trace_count", p.SourceTraceCount).Set("distilled_at", p.DistilledAt).
			Set("hit_count", p.HitCount))
	} else {
		out.Set("provenance", nil)
	}
	return out, nil
}
