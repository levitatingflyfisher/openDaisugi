// Package distill is the oracle's distiller (opendaisugi/distiller.py):
// successful journal traces clustered by the active matcher, each cluster
// intersected, salvaged or generalized by a model, verified, and stored as
// a compiled pathway.
//
// Every stored pathway is verified with the linked Z3 first and the
// distiller fails closed: a violation, a Z3 error or a Z3 check that did
// not finish drops the cluster.
package distill

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"daisugi-verify/internal/garden"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/verify"
)

// Store is what the distiller needs of the pathway store.
type Store interface {
	All() ([]pathways.Row, error)
	ReadAll(keep func(id string) bool) ([]*pathways.Pathway, error)
	UpdateEmbedding(id, embedding, model, version string) error
	PutPathway(p *pathways.Pathway) error
}

// DiskStore is a pathway store file as the distiller writes it.
type DiskStore struct{ *pathways.Store }

// PutPathway is PathwayStore.put.
func (s DiskStore) PutPathway(p *pathways.Pathway) error {
	row, err := p.PyRow()
	if err != nil {
		return err
	}
	return s.Put(row)
}

// MemStore is PathwayStore(":memory:"), the store of a dry run.
type MemStore struct{ ps []*pathways.Pathway }

// All has no stale rows: only this run's pathways are in it.
func (m *MemStore) All() ([]pathways.Row, error) { return nil, nil }

// ReadAll is list_all.
func (m *MemStore) ReadAll(func(string) bool) ([]*pathways.Pathway, error) { return m.ps, nil }

// UpdateEmbedding is never reached: nothing in a new store is stale.
func (m *MemStore) UpdateEmbedding(string, string, string, string) error { return nil }

// PutPathway is INSERT OR REPLACE by id.
func (m *MemStore) PutPathway(p *pathways.Pathway) error {
	for i, q := range m.ps {
		if q.ID() == p.ID() {
			m.ps = append(m.ps[:i], m.ps[i+1:]...)
			break
		}
	}
	m.ps = append(m.ps, p)
	return nil
}

// Options are the Distiller's settings.
type Options struct {
	Model           string
	MinTraces       int64
	LookbackDays    int64
	Threshold       float64
	ValidationSplit float64
	StructureWeight float64
	Now             func() float64
	// Identity is the active matcher's provenance stamp, which rows are
	// stamped with and checked against.
	Identity string
	// Z3TimeoutMs is verify()'s default.
	Z3TimeoutMs int
}

// Report is TendReport.
type Report struct {
	Created, Updated, Skipped int
	Pathways                  []string
	Warnings                  []string
}

// Distiller holds one tend run's inputs.
type Distiller struct {
	J       *tracejournal.Journal
	S       Store
	M       *pathways.Matcher
	LLM     *llm.Client
	Opt     Options
	records map[string]*tracejournal.Record
	loadErr map[string]error
	refs    map[string][]*pyjson.Object
}

// ErrUnreadable wraps journal content the binary refuses to read.
var ErrUnreadable = tracejournal.ErrUnreadable

var preamble = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^[ \t]*Base directory for this skill:[^\n]*\n?`),
	regexp.MustCompile(`(?m)^[ \t]*###[ \t]+Skill:[^\n]*\n?`),
	regexp.MustCompile(`(?m)^[ \t]*Path:[ \t]+(?:plugin|bundled):[^\n]*\n?`),
	regexp.MustCompile(`<command-name>[^<]*</command-name>|<command-message>[^<]*</command-message>|<command-args>[^<]*</command-args>`),
}

// NormalizeTask is _normalize_task_for_embedding.
func NormalizeTask(task string) string {
	s := task
	for _, re := range preamble {
		s = re.ReplaceAllString(s, "")
	}
	s = pystr.Strip(s)
	if s == "" {
		return task
	}
	return s
}

// tokenHex is secrets.token_hex(4).
func tokenHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Prepare reads, before anything is written, every trace body and
// refinement record the run may need, so that one this binary cannot read
// refuses the run with nothing changed. Load errors Python meets are kept
// and reported where Python reports them.
func (d *Distiller) Prepare(traces []tracejournal.Distillable) error {
	d.records, d.loadErr, d.refs = map[string]*tracejournal.Record{}, map[string]error{}, map[string][]*pyjson.Object{}
	for _, t := range traces {
		rec, err := d.J.LoadTrace(t.TraceID)
		var le *tracejournal.LoadError
		switch {
		case err == nil:
			d.records[t.TraceID] = rec
		case errors.As(err, &le):
			d.loadErr[t.TraceID] = le
		default:
			return err
		}
		if run, ok := t.RunID.(string); ok && run != "" {
			if _, seen := d.refs[run]; !seen {
				recs, err := d.J.Refinements(run)
				if err != nil {
					return err
				}
				d.refs[run] = recs
			}
		}
	}
	return nil
}

// ErrNoEmbedder is a run that needs the embedder of a matcher this
// binary does not carry; the command checks for it before it writes.
var ErrNoEmbedder = errors.New("the matcher's embedder is not in this binary")

// embed is the matcher's encode over the normalized task texts.
func (d *Distiller) embed(texts []string, normalize bool) ([][]float64, error) {
	if d.M == nil {
		return nil, ErrNoEmbedder
	}
	emb, err := d.M.Embedder()
	if err != nil {
		return nil, err
	}
	out := make([][]float64, len(texts))
	for i, t := range texts {
		if normalize {
			t = NormalizeTask(t)
		}
		out[i] = emb.Encode(t)
	}
	return out, nil
}

// reembedStale is PathwayStore.reembed_stale with the distiller's
// embedder.
func (d *Distiller) reembedStale() (int, error) {
	rows, err := d.S.All()
	if err != nil {
		return 0, err
	}
	var stale []pathways.Row
	for _, r := range rows {
		if r["embedding_model"] != d.Opt.Identity || r["embedding_model_version"] != pathways.EmbeddingModelVersion {
			stale = append(stale, r)
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}
	tasks := make([]string, len(stale))
	for i, r := range stale {
		s, ok := r["task_description"].(string)
		if !ok {
			return 0, fmt.Errorf("%w: a task_description is not text", pathways.ErrUnreadable)
		}
		tasks[i] = s
	}
	vecs, err := d.embed(tasks, true)
	if err != nil {
		// Logged on the opendaisugi logger, which prints nothing by
		// default.
		return 0, nil
	}
	for i, r := range stale {
		xs := make([]any, len(vecs[i]))
		for k, x := range vecs[i] {
			xs[k] = x
		}
		if err := d.S.UpdateEmbedding(r["id"].(string), pyjson.Dumps(xs, true), d.Opt.Identity, pathways.EmbeddingModelVersion); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

type cluster struct {
	members  []int
	centroid []float64
}

func norm(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

// meanRows is vecs[members].mean(axis=0): each column summed row by row,
// then divided by the count.
func meanRows(vecs [][]float64, members []int) []float64 {
	out := make([]float64, len(vecs[members[0]]))
	for _, m := range members {
		for j, x := range vecs[m] {
			out[j] += x
		}
	}
	n := float64(len(members))
	for j := range out {
		out[j] /= n
	}
	return out
}

// clusterWithCentroids is _cluster_with_centroids.
func clusterWithCentroids(vecs [][]float64, threshold float64) []cluster {
	var cs []cluster
	for i, v := range vecs {
		vn := norm(v)
		if vn == 0 {
			vn = 1e-9
		}
		best, bestSim := -1, -1.0
		for ci, c := range cs {
			cn := norm(c.centroid)
			if cn == 0 {
				cn = 1e-9
			}
			var dot float64
			for j := range v {
				dot += (v[j] / vn) * (c.centroid[j] / cn)
			}
			if dot > bestSim {
				bestSim, best = dot, ci
			}
		}
		if bestSim >= threshold {
			cs[best].members = append(cs[best].members, i)
			cs[best].centroid = meanRows(vecs, cs[best].members)
		} else {
			cs = append(cs, cluster{members: []int{i}, centroid: append([]float64{}, v...)})
		}
	}
	return cs
}

// Tend is Distiller.tend.
func (d *Distiller) Tend(traces []tracejournal.Distillable) (Report, error) {
	rep := Report{Pathways: []string{}}
	n, err := d.reembedStale()
	if err != nil {
		return rep, err
	}
	if n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("re-embedded %d pathways under the current embedder.", n))
	}
	if int64(len(traces)) < d.Opt.MinTraces {
		rep.Skipped = len(traces)
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("tend: only %d successful trace(s) in the last %d days, "+
			"below min_traces=%d; no pathways distilled.", len(traces), d.Opt.LookbackDays, d.Opt.MinTraces))
		return rep, nil
	}
	tasks := make([]string, len(traces))
	sigs := make([]string, len(traces))
	for i, t := range traces {
		tasks[i] = t.Task
		if s, ok := t.Signature.(string); ok {
			sigs[i] = s
		}
	}
	taskVecs, err := d.embed(tasks, true)
	if err != nil {
		rep.Skipped = len(traces)
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("tend: %v Built the verified journal but distilled 0 pathways.", err))
		return rep, nil
	}
	vecs := taskVecs
	if d.Opt.StructureWeight > 0 {
		structVecs, err := d.embed(sigs, false)
		if err != nil {
			return rep, err
		}
		tw := math.Pow(1-d.Opt.StructureWeight, 0.5)
		sw := math.Pow(d.Opt.StructureWeight, 0.5)
		vecs = make([][]float64, len(traces))
		for i := range traces {
			row := make([]float64, 0, len(taskVecs[i])+len(structVecs[i]))
			for _, x := range taskVecs[i] {
				row = append(row, x*tw)
			}
			for _, x := range structVecs[i] {
				row = append(row, x*sw)
			}
			vecs[i] = row
		}
	}
	for _, c := range clusterWithCentroids(vecs, d.Opt.Threshold) {
		if int64(len(c.members)) < d.Opt.MinTraces {
			rep.Skipped++
			continue
		}
		ct := make([]tracejournal.Distillable, len(c.members))
		for k, i := range c.members {
			ct[k] = traces[i]
		}
		existing, err := d.findExistingCovering(ct)
		if err != nil {
			return rep, err
		}
		if existing != nil && !hasNewTraces(existing, ct) {
			rep.Skipped++
			continue
		}
		centroid := meanRows(taskVecs, c.members)
		p, err := d.distillCluster(ct, centroid, &rep.Warnings)
		if err != nil {
			return rep, err
		}
		if p == nil {
			rep.Skipped++
			continue
		}
		if err := d.S.PutPathway(p); err != nil {
			return rep, err
		}
		rep.Pathways = append(rep.Pathways, p.ID())
		if existing == nil {
			rep.Created++
		} else {
			rep.Updated++
		}
	}
	return rep, nil
}

func sources(p *pathways.Pathway) []any { return p.Obj.Value("source_trace_ids").([]any) }

func (d *Distiller) findExistingCovering(ct []tracejournal.Distillable) (*pathways.Pathway, error) {
	all, err := d.S.ReadAll(func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, t := range ct {
		ids[t.TraceID] = true
	}
	for _, p := range all {
		for _, s := range sources(p) {
			if ids[s.(string)] {
				return p, nil
			}
		}
	}
	return nil, nil
}

func hasNewTraces(p *pathways.Pathway, ct []tracejournal.Distillable) bool {
	known := map[string]bool{}
	for _, s := range sources(p) {
		known[s.(string)] = true
	}
	for _, t := range ct {
		if !known[t.TraceID] {
			return true
		}
	}
	return false
}

func (d *Distiller) loadRecords(ts []tracejournal.Distillable, warnings *[]string) []*tracejournal.Record {
	var out []*tracejournal.Record
	for _, t := range ts {
		if r, ok := d.records[t.TraceID]; ok {
			out = append(out, r)
			continue
		}
		*warnings = append(*warnings, fmt.Sprintf("load_trace(%s) failed: %v", t.TraceID, d.loadErr[t.TraceID]))
	}
	return out
}

// verified is one verify() as the distiller reads it: ok, the reason it
// is not (the first violation, or a Z3 check that did not finish).
func (d *Distiller) verified(plan, env *pyjson.Object) (bool, string, error) {
	p, err := verify.ParsePlan([]byte(pathways.DumpJSON(plan)))
	if err != nil {
		return false, "", fmt.Errorf("%w: %v", pathways.ErrUnreadable, err)
	}
	e, err := verify.ParseEnvelope([]byte(pathways.DumpJSON(env)))
	if err != nil {
		return false, "", fmt.Errorf("%w: %v", pathways.ErrUnreadable, err)
	}
	res := verify.Verify(p, e, verify.VerifyOptions{Z3TimeoutMs: d.Opt.Z3TimeoutMs})
	if len(res.Timeouts) > 0 {
		return false, "verifier timed out; raise the Z3 timeout", nil
	}
	if res.OK {
		return true, "", nil
	}
	why := "unknown"
	if len(res.Violations) > 0 {
		why = res.Violations[0].Message
	}
	return false, why, nil
}

func (d *Distiller) validateEnvelope(env *pyjson.Object, plans []*pyjson.Object) (float64, []*pyjson.Object, error) {
	if len(plans) == 0 {
		return 0, nil, nil
	}
	var failing []*pyjson.Object
	passed := 0
	for _, p := range plans {
		ok, _, err := d.verified(p, env)
		if err != nil {
			return 0, nil, err
		}
		if ok {
			passed++
		} else {
			failing = append(failing, p)
		}
	}
	return float64(passed) / float64(len(plans)), failing, nil
}

// GeneralizedTemplate is distiller.GeneralizedTemplate.
var GeneralizedTemplate = &pmodel.Model{Name: "GeneralizedTemplate", Fields: []pmodel.Field{
	{Name: "task_description", Schema: pmodel.Str{}, Required: true},
	{Name: "plan_template", Schema: pmodel.ActionPlanReply, Required: true},
}}

const generalizeSystem = "You are a planner distilling multiple successful runs into a reusable template.\n" +
	"Produce a generalized task description and a concrete plan template.\n" +
	"The plan template must be a valid ActionPlan with REAL (not placeholder) values —\n" +
	"concrete paths, commands, URLs; use the cluster's most representative values.\n" +
	"At reuse, only the fields that varied across the cluster are re-bound (a typed,\n" +
	"re-verified data bind, ADR-0008); the rest run as-is. No <placeholder> tokens."

const improveSystem = "You are tightening an envelope that was too restrictive for some valid plans.\n" +
	"Widen ONLY the specific permissions that blocked the failing plans.\n" +
	"Do not loosen anything else. Return a revised Envelope."

const maxPitfalls = 20

func (d *Distiller) distillCluster(ct []tracejournal.Distillable, centroid []float64, warnings *[]string) (*pathways.Pathway, error) {
	split := int(float64(len(ct)) * d.Opt.ValidationSplit)
	if split < 1 {
		split = 1
	}
	if split > len(ct) {
		split = len(ct)
	}
	train, test := ct[:split], ct[split:]
	trainRecs := d.loadRecords(train, warnings)
	var testRecs []*tracejournal.Record
	if len(test) > 0 {
		testRecs = d.loadRecords(test, warnings)
	}
	if len(trainRecs) == 0 {
		*warnings = append(*warnings, "cluster skipped: no loadable train traces after errors")
		return nil, nil
	}
	perms := make([]*pyjson.Object, len(trainRecs))
	for i, r := range trainRecs {
		perms[i] = r.Envelope.Value("permissions").(*pyjson.Object)
	}
	rep := trainRecs[len(trainRecs)-1]
	env := copyObj(rep.Envelope)
	env.Set("permissions", garden.IntersectPermissions(perms))
	env.Set("generated_by", "distilled")

	var allPlans []*pyjson.Object
	for _, r := range append(append([]*tracejournal.Record{}, trainRecs...), testRecs...) {
		allPlans = append(allPlans, r.Plan)
	}
	divergent, salvageParams, derr := PlanDivergence(allPlans)
	if derr != nil {
		*warnings = append(*warnings, "divergence analysis failed (continuing frozen): "+derr.Error())
		divergent, salvageParams = nil, []any{}
	}
	workspace, hasWorkspace := "", false
	if len(divergent) > 0 {
		workspace, hasWorkspace = SalvageWorkspace(stringList(env.Value("permissions").(*pyjson.Object).Value("file_read")))
		if !hasWorkspace {
			*warnings = append(*warnings, "delegated salvage skipped: no file_read glob gives the leaf a workspace; "+
				"falling back to frozen generalization")
		}
	}
	if hasWorkspace {
		salvaged, err := BuildDelegatedTemplate(rep.Plan, divergent, allPlans, workspace)
		if err != nil {
			return nil, err
		}
		ok, why, err := d.verified(salvaged, env)
		if err != nil {
			return nil, err
		}
		if ok {
			var sig any
			if s, ok := tracejournal.StructureSignature(salvaged); ok {
				sig = s
			}
			return d.pathway(ct[len(ct)-1].Task, centroid, env, salvaged, ct, sig, salvageParams), nil
		}
		*warnings = append(*warnings, "delegated salvage template failed verification ("+why+
			") — falling back to frozen generalization")
	}
	var pitfalls []string
	seen := map[string]bool{}
	for _, t := range ct {
		run, ok := t.RunID.(string)
		if !ok || run == "" {
			continue
		}
		// _extract_pitfalls per run, then the global dedupe.
		for _, rec := range d.refs[run] {
			for _, v := range rec.Value("violations").([]any) {
				vo := v.(*pyjson.Object)
				msg := "[" + vo.Value("stage").(string) + "] " + vo.Value("message").(string)
				if !seen[msg] {
					seen[msg] = true
					pitfalls = append(pitfalls, msg)
				}
			}
		}
	}
	if len(pitfalls) > maxPitfalls {
		pitfalls = append(pitfalls[:maxPitfalls:maxPitfalls],
			fmt.Sprintf("... (%d more pitfall(s) truncated)", len(pitfalls)-maxPitfalls))
	}
	block := "(none recorded)"
	if len(pitfalls) > 0 {
		lines := make([]string, len(pitfalls))
		for i, p := range pitfalls {
			lines[i] = "- " + p
		}
		block = strings.Join(lines, "\n")
	}
	user := "Representative plan:\n" + pathways.DumpJSONIndent(rep.Plan, 2) + "\n\n" +
		"Envelope constraints:\n" + pathways.DumpJSONIndent(env, 2) + "\n\n" +
		"Known pitfalls from past rejections:\n" + block + "\n\n" +
		"Produce: a generalized task_description (broad enough for similar tasks) " +
		"and a plan_template with concrete representative values."
	gen, err := d.LLM.Structured(llm.Call{Model: d.Opt.Model, System: generalizeSystem, User: user,
		Response: llm.Schema{Name: "GeneralizedTemplate", Model: GeneralizedTemplate}, MaxRetries: 2})
	if err != nil {
		var le *llm.Error
		if !errors.As(err, &le) {
			return nil, err
		}
		*warnings = append(*warnings, "cluster generalization failed: "+le.Msg)
		return nil, nil
	}
	var testPlans []*pyjson.Object
	for _, r := range testRecs {
		testPlans = append(testPlans, r.Plan)
	}
	score, failing, err := d.validateEnvelope(env, testPlans)
	if err != nil {
		return nil, err
	}
	if score < 0.5 && len(failing) > 0 {
		blocks := make([]string, len(failing))
		for i, p := range failing {
			blocks[i] = "Plan " + p.Value("id").(string) + ":\n" + pathways.DumpJSONIndent(p, 2)
		}
		iu := "Current envelope (too tight):\n" + pathways.DumpJSONIndent(env, 2) + "\n\n" +
			"Plans that should have passed but were rejected:\n" + strings.Join(blocks, "\n\n") + "\n\n" +
			"Return a revised envelope that passes these plans while keeping\nall other permissions tight."
		improved, err := d.LLM.Structured(llm.Call{Model: d.Opt.Model, System: improveSystem, User: iu,
			Response: llm.Schema{Name: "Envelope", Model: pmodel.Envelope}, MaxRetries: 2})
		if err != nil {
			var le *llm.Error
			if !errors.As(err, &le) {
				return nil, err
			}
			*warnings = append(*warnings, "cluster improvement pass failed: "+le.Msg)
		} else {
			newScore, _, err := d.validateEnvelope(improved, testPlans)
			if err != nil {
				return nil, err
			}
			if newScore > score {
				env, score = improved, newScore
			} else {
				*warnings = append(*warnings, fmt.Sprintf("cluster improvement pass did not increase score (%s → %s)",
					garden.FormatF(score, 2), garden.FormatF(newScore, 2)))
			}
		}
	}
	template := gen.Value("plan_template").(*pyjson.Object)
	ok, why, err := d.verified(template, env)
	if err != nil {
		return nil, err
	}
	if !ok {
		*warnings = append(*warnings, "distilled plan_template does not verify against its envelope ("+why+"); dropping cluster")
		return nil, nil
	}
	var sig any
	if s, ok := tracejournal.StructureSignature(template); ok {
		sig = s
	}
	params, perr := DiffPlansForParameters(allPlans)
	if perr == nil {
		params, perr = RekeyToTemplate(params, template)
	}
	if perr != nil {
		*warnings = append(*warnings, "parameter diff failed (kept frozen): "+perr.Error())
		params = []any{}
	}
	return d.pathway(gen.Value("task_description").(string), centroid, env, template, ct, sig, params), nil
}

func copyObj(o *pyjson.Object) *pyjson.Object {
	c := pyjson.NewObject()
	for _, k := range o.Keys() {
		c.Set(k, o.Value(k))
	}
	return c
}

func (d *Distiller) pathway(task string, centroid []float64, env, plan *pyjson.Object,
	ct []tracejournal.Distillable, sig any, params []any) *pathways.Pathway {
	emb := make([]any, len(centroid))
	for i, x := range centroid {
		emb[i] = x
	}
	srcs := make([]any, len(ct))
	for i, t := range ct {
		srcs[i] = t.TraceID
	}
	o := pyjson.NewObject().Set("id", "pathway_"+tokenHex()).Set("task_description", task).
		Set("task_embedding", emb).Set("embedding_model", d.Opt.Identity).
		Set("embedding_model_version", pathways.EmbeddingModelVersion).
		Set("envelope", env).Set("plan_template", plan).Set("source_trace_ids", srcs).
		Set("version", pyjson.Int{Text: "1"}).Set("hit_count", pyjson.Int{Text: "0"}).
		Set("distilled_at", d.Opt.Now()).Set("last_activation_at", 0.0).
		Set("failure_count", pyjson.Int{Text: "0"}).Set("activation_count", pyjson.Int{Text: "0"}).
		Set("structure_signature", sig).Set("parameters", params)
	return &pathways.Pathway{Obj: o}
}
