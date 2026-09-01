package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"daisugi-verify/internal/batch"
	"daisugi-verify/internal/bundle"
	"daisugi-verify/internal/deeds"
	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/registry"
	"daisugi-verify/internal/signing"
	"daisugi-verify/internal/strata"
	"daisugi-verify/internal/supervise"
	"daisugi-verify/internal/tracejournal"
)

// LProbe is the test instrument cmd/l-probe runs for clients/l_compare.py:
// one query (Args[0], JSON) of the library parts no command reaches (the
// deed ledger, the strata store, batch runs, pathway bundles, the signing
// primitives), answered on stdout as clients/l_probe_oracle.py answers it.
// It is not shipped.
func LProbe(e *Env) int {
	e.env = map[string]string{}
	for _, kv := range e.Environ {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				e.env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	if len(e.Args) != 1 {
		e.errf("usage: l-probe QUERY_JSON\n")
		return 2
	}
	v, err := pyjson.Loads(e.Args[0])
	if err != nil {
		e.errf("l-probe: the query does not read: %v\n", err)
		return 2
	}
	q := v.(*pyjson.Object)
	out, err := e.probeOp(q)
	var un *registry.Unread
	if errors.As(err, &un) {
		e.errf("l-probe: %v. Nothing was changed.\n", err)
		return 2
	}
	if err != nil {
		out = pyErr(err)
	}
	e.out("%s\n", pyjson.Dumps(sortKeys(out), false))
	return 0
}

// pyErr is the probe's {"error": <type>, "msg": <text>} for an exception
// the oracle raises.
func pyErr(err error) *pyjson.Object {
	var pe *signing.PyError
	var ve *pmodel.ValidationError
	var ee *tracejournal.PyError
	var ge *envgen.PyError
	switch {
	case errors.As(err, &pe):
		return pyjson.NewObject().Set("error", pe.Type).Set("msg", pe.Msg)
	case errors.As(err, &ve):
		return pyjson.NewObject().Set("error", "pydantic_core._pydantic_core.ValidationError").Set("msg", "")
	case errors.As(err, &ee):
		return pyjson.NewObject().Set("error", ee.Type).Set("msg", ee.Msg)
	case errors.As(err, &ge):
		return pyjson.NewObject().Set("error", ge.Class).Set("msg", ge.Msg)
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		if name, text, ok := supervise.PyOSError(pathErr.Err, pathErr.Path); ok {
			return pyjson.NewObject().Set("error", name).Set("msg", text)
		}
	}
	return pyjson.NewObject().Set("error", "Unported").Set("msg", err.Error())
}

func strsOf(v any) []string {
	var out []string
	xs, _ := v.([]any)
	for _, x := range xs {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func objList(xs []*pyjson.Object) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func (e *Env) probeOp(q *pyjson.Object) (any, error) {
	switch q.Value("op") {
	case "b64":
		var out []any
		for _, s := range strsOf(q.Value("inputs")) {
			b, err := signing.B64Decode(s)
			if err != nil {
				out = append(out, pyErr(err))
				continue
			}
			out = append(out, pyjson.NewObject().Set("hex", hex.EncodeToString(b)))
		}
		return out, nil
	case "sign":
		var out []any
		for _, c := range q.Value("cases").([]any) {
			o := c.(*pyjson.Object)
			sig, err := signing.SignBytes([]byte(o.Value("payload").(string)), o.Value("priv").(string))
			if err != nil {
				out = append(out, pyErr(err))
				continue
			}
			out = append(out, pyjson.NewObject().Set("sig", sig))
		}
		return out, nil
	case "verify":
		var out []any
		for _, c := range q.Value("cases").([]any) {
			o := c.(*pyjson.Object)
			var payload []byte
			if h, ok := o.Value("hex").(string); ok {
				payload, _ = hex.DecodeString(h)
			} else {
				payload = []byte(o.Value("payload").(string))
			}
			ok := signing.VerifyBytes(payload, o.Value("sig").(string), o.Value("pub").(string))
			out = append(out, pyjson.NewObject().Set("ok", ok))
		}
		return out, nil
	case "contract":
		return probeContract(q)
	case "to_bundle":
		pw, verr := pmodel.Validate("CompiledPathway", pmodel.CompiledPathway, q.Value("pathway"), pmodel.Python)
		if verr != nil {
			return nil, verr
		}
		var priv, pub *string
		if s, ok := q.Value("priv").(string); ok {
			priv = &s
		}
		if s, ok := q.Value("pub").(string); ok {
			pub = &s
		}
		publisher := q.Value("publisher").(string)
		at := q.Value("published_at")
		b, perr := bundle.ToBundle(pw.(*pyjson.Object), publisher, at, priv, pub)
		if perr != nil {
			return nil, perr
		}
		return pyjson.NewObject().Set("bundle", b).Set("hash", bundle.Hash(pw.(*pyjson.Object), publisher, at)), nil
	case "from_bundle":
		b, verr := bundle.Validate(q.Value("bundle"))
		if verr != nil {
			return nil, verr
		}
		var trusted []string
		if t, ok := q.Value("trusted").([]any); ok {
			trusted = strsOf(t)
			if trusted == nil {
				trusted = []string{}
			}
		}
		require := true
		if r, ok := q.Get("require_signed"); ok {
			require = pyjson.Truthy(r)
		}
		pw, perr := bundle.FromBundle(b, trusted, require)
		if perr != nil {
			return nil, perr
		}
		return pyjson.NewObject().Set("pathway", pw), nil
	case "rollback", "touched":
		j, err := tracejournal.Open(q.Value("data_dir").(string))
		if err != nil {
			return nil, err
		}
		defer j.Close()
		run := q.Value("run_id").(string)
		if q.Value("op") == "rollback" {
			r, err := deeds.Rollback(j, run)
			if err != nil {
				return nil, err
			}
			return r.Dump(), nil
		}
		ts, err := deeds.Touched(j, run)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, t := range ts {
			var c any
			if t.PreContent != nil {
				c = *t.PreContent
			}
			out = append(out, []any{t.Path, pyjson.NewObject().Set("pre_existed", t.PreExisted).Set("pre_content", c)})
		}
		return out, nil
	case "reverse":
		h, verr := deeds.ParseHandle(q.Value("handle"))
		if verr != nil {
			return nil, verr
		}
		if err := deeds.Apply(h); err != nil {
			return nil, err
		}
		return pyjson.NewObject().Set("ok", true), nil
	case "strata":
		return probeStrata(q)
	case "batch":
		return e.probeBatch(q)
	}
	return nil, &signing.PyError{Type: "KeyError", Msg: fmt.Sprint(q.Value("op"))}
}

func probeContract(q *pyjson.Object) (any, error) {
	cv, verr := pmodel.Validate("Contract", signing.ContractModel, q.Value("contract"), pmodel.Python)
	if verr != nil {
		return nil, verr
	}
	c := cv.(*pyjson.Object)
	out := pyjson.NewObject().Set("canonical", string(signing.CanonicalContract(c)))
	if priv, ok := q.Value("priv").(string); ok {
		sig, err := signing.SignContract(c, priv)
		if err != nil {
			out.Set("signature", pyErr(err))
		} else {
			out.Set("signature", sig)
			c.Set("signature", sig).Set("signer", q.Value("signer"))
		}
	}
	trust := q.Value("trust").(*pyjson.Object)
	raw := pyjson.NewObject()
	for _, n := range trust.Keys() {
		raw.Set(n, signing.VerifyContract(c, trust.Value(n)))
	}
	out.Set("verify_raw", raw)
	dir, err := os.MkdirTemp("", "l-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := dir + "/trusted_signers.json"
	reg, err := signing.LoadRegistry(path)
	if err != nil {
		return nil, err
	}
	for _, n := range trust.Keys() {
		reg.Add(n, trust.Value(n).(string))
	}
	if err := reg.Save(); err != nil {
		return nil, err
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out.Set("registry_file", string(text))
	if reg, err = signing.LoadRegistry(path); err != nil {
		return nil, err
	}
	names := []any{}
	for _, n := range reg.Names() {
		names = append(names, n)
	}
	out.Set("names", names)
	want := strsOf(q.Value("names"))
	out.Set("registry_verify", reg.VerifyNamed(c, want))
	if rm, ok := q.Value("remove").([]any); ok && len(rm) > 0 {
		var removed []any
		for _, n := range strsOf(rm) {
			removed = append(removed, reg.Remove(n))
		}
		out.Set("removed", removed)
		out.Set("registry_verify_after", reg.VerifyNamed(c, want))
	}
	return out, nil
}

func refOf(ids []string, v any) string {
	switch x := v.(type) {
	case pyjson.Int:
		var n int
		fmt.Sscan(x.Text, &n)
		if n < 0 {
			n += len(ids)
		}
		if n >= 0 && n < len(ids) {
			return ids[n]
		}
		return ""
	case string:
		return x
	}
	return ""
}

func snapshot(v any) any {
	switch x := v.(type) {
	case *pyjson.Object:
		o := pyjson.NewObject()
		for _, k := range x.Keys() {
			o.Set(k, snapshot(x.Value(k)))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = snapshot(e)
		}
		return out
	}
	return v
}

func valOr(o *pyjson.Object, k string, def any) any {
	if v, ok := o.Get(k); ok {
		return v
	}
	return def
}

func probeStrata(q *pyjson.Object) (any, error) {
	store := &strata.Store{}
	var env *pyjson.Object
	if ev, ok := q.Get("envelope"); ok {
		v, verr := pmodel.Validate("Envelope", pmodel.Envelope, ev, pmodel.Python)
		if verr != nil {
			return nil, verr
		}
		env = v.(*pyjson.Object)
	}
	var ids []string
	out := []any{}
	for _, sv := range q.Value("steps").([]any) {
		st := sv.(*pyjson.Object)
		res, err := func() (any, error) {
			switch st.Value("do") {
			case "emit":
				tags := valOr(st, "tags", nil)
				x, err := store.Emit(st.Value("kind"), st.Value("content"), valOr(st, "provenance", ""),
					valOr(st, "status", "open"), tags, valOr(st, "pinned", false))
				if err != nil {
					return nil, err
				}
				ids = append(ids, x.Value("id").(string))
				return x, nil
			case "set_status":
				return store.SetStatus(refOf(ids, st.Value("ref")), st.Value("status"))
			case "get":
				x := store.Get(refOf(ids, st.Value("ref")))
				if x == nil {
					return nil, nil
				}
				return x, nil
			case "repage":
				return store.Repage(refOf(ids, st.Value("ref")))
			case "by_kind":
				return objList(store.ByKind(st.Value("kind").(string))), nil
			case "all":
				return objList(store.All()), nil
			case "reconstruct":
				var budget *int64
				if b, ok := st.Value("budget").(pyjson.Int); ok {
					var n int64
					fmt.Sscan(b.Text, &n)
					budget = &n
				}
				query, _ := st.Value("query").(string)
				return store.Reconstruct(budget, strsOf(st.Value("tags")), query), nil
			case "to_json":
				return store.ToJSON(), nil
			case "roundtrip":
				s2, err := strata.FromJSON(store.ToJSON())
				if err != nil {
					return nil, err
				}
				store = s2
				return "ok", nil
			case "from_json":
				s2, err := strata.FromJSON(st.Value("text").(string))
				if err != nil {
					return nil, err
				}
				store = s2
				ids = nil
				for _, x := range store.All() {
					ids = append(ids, x.Value("id").(string))
				}
				return objList(store.All()), nil
			case "promote":
				var inv, cand, wit *pyjson.Object
				if v, ok := st.Get("add_invariant"); ok {
					x, verr := pmodel.Validate("Invariant", pmodel.Invariant, v, pmodel.Python)
					if verr != nil {
						return nil, verr
					}
					inv = x.(*pyjson.Object)
				}
				if v, ok := st.Get("candidate"); ok {
					x, verr := pmodel.Validate("Envelope", pmodel.Envelope, v, pmodel.Python)
					if verr != nil {
						return nil, verr
					}
					cand = x.(*pyjson.Object)
				}
				if v, ok := st.Get("deny_witness"); ok {
					x, verr := pmodel.Validate("ActionPlan", pmodel.ActionPlan, v, pmodel.Python)
					if verr != nil {
						return nil, verr
					}
					wit = x.(*pyjson.Object)
				}
				target := store.Get(refOf(ids, st.Value("ref")))
				var remove []string
				if r, ok := st.Value("remove_file_write").([]any); ok {
					remove = strsOf(r)
				}
				r, err := strata.Promote(env, target, inv, remove, cand, wit)
				if err != nil {
					return nil, err
				}
				if r.OK {
					env = r.Envelope
				}
				vs := []any{}
				for _, m := range r.Violations {
					vs = append(vs, []any{"inheritance", m})
				}
				return pyjson.NewObject().Set("ok", r.OK).Set("reason", r.Reason).
					Set("enforcement_proven", r.EnforcementProven).Set("violations", vs).
					Set("envelope", pathways.JSONMode(r.Envelope)).Set("status", target.Value("status")), nil
			case "ledger":
				f := st.Value("fields").(*pyjson.Object)
				get := func(k string) int64 {
					var n int64
					if v, ok := f.Value(k).(pyjson.Int); ok {
						fmt.Sscan(v.Text, &n)
					}
					return n
				}
				without, with := get("output_tokens_without_store"), get("output_tokens_with_store")
				return pyjson.NewObject().
					Set("output_tokens_without_store", pyjson.Int{Text: fmt.Sprint(without)}).
					Set("output_tokens_with_store", pyjson.Int{Text: fmt.Sprint(with)}).
					Set("rederived_facts", pyjson.Int{Text: fmt.Sprint(get("rederived_facts"))}).
					Set("reexplored_branches", pyjson.Int{Text: fmt.Sprint(get("reexplored_branches"))}).
					Set("evidence_not_proof", true).
					Set("note", "Store-on vs store-off output-token delta. Labelled evidence, not proof; the "+
						"magnitudes are model-dependent and the at-scale numbers are deferred to a local model "+
						"(Stage 4's dependency).").
					Set("tokens_saved", pyjson.Int{Text: fmt.Sprint(without - with)}), nil
			}
			return pyjson.NewObject().Set("error", "unknown step").Set("msg", st.Value("do")), nil
		}()
		if err != nil {
			var un *registry.Unread
			if errors.As(err, &un) {
				return nil, err
			}
			res = pyErr(err)
		}
		// The oracle dumps each result when the step runs; a later step
		// that changes a stratum does not change what was printed.
		out = append(out, snapshot(res))
	}
	return out, nil
}

func (e *Env) probeBatch(q *pyjson.Object) (any, error) {
	d, verr := batch.Validate(q.Value("decl"))
	if verr != nil {
		return nil, verr
	}
	ev, verr := pmodel.Validate("Envelope", pmodel.Envelope, q.Value("envelope"), pmodel.Python)
	if verr != nil {
		return nil, verr
	}
	env := ev.(*pyjson.Object)
	j, err := tracejournal.Open(q.Value("data_dir").(string))
	if err != nil {
		return nil, err
	}
	defer j.Close()
	runner := func(plan *pyjson.Object) (*supervise.Session, error) {
		pre, why := prepare(plan, env)
		if why != "" {
			return nil, &registry.Unread{Why: why}
		}
		executors := supervise.DefaultExecutors()
		executors["shell"] = supervise.Shell{Environ: e.Environ}
		sup := &supervise.Supervisor{Executors: executors, Journal: j, Z3TimeoutMs: 500, StepTimeoutS: 30,
			MaxOutputBytes: 10 * 1024 * 1024,
			Approval:       supervise.Default{Getenv: e.lookup, Stdin: e.Stdin, Stdout: e.Stdout, Terminal: e.terminal}}
		s := sup.Run(pre.plan, pre.env, pre.venv, pre.verification)
		if sup.LogErr != nil {
			return nil, sup.LogErr
		}
		return s, nil
	}
	var k *int64
	if v, ok := q.Value("sample_k").(pyjson.Int); ok {
		var n int64
		fmt.Sscan(v.Text, &n)
		k = &n
	}
	perms := env.Value("permissions").(*pyjson.Object)
	r, err := batch.Run(d, strsOf(perms.Value("file_write")), runner, k)
	if err != nil {
		return nil, err
	}
	out := r.Dump()
	if pyjson.Truthy(q.Value("undo")) {
		rep, err := batch.RollbackResult(r)
		if err != nil {
			return nil, err
		}
		out.Set("undo", rep.Dump())
	}
	if kw, ok := q.Value("ledger_kwargs").(*pyjson.Object); ok {
		num := func(k string) *int64 {
			v, ok := kw.Value(k).(pyjson.Int)
			if !ok {
				return nil
			}
			var n int64
			fmt.Sscan(v.Text, &n)
			return &n
		}
		zero := func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		}
		out.Set("ledger_kwargs", batch.TwoLedgers(batch.WithinInstance(d, zero(num("output_tokens_saved")),
			zero(num("calls_saved")), num("tokens_per_call"), num("spec_input_injected"))))
	}
	return out, nil
}
