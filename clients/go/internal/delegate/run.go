package delegate

import (
	"os"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Messages is delegate.worker_messages.
func Messages(question, name, text string) []llm.Message {
	user := "Question: " + question + "\n\nFile name: " + name + "\n\n<file>\n" + text + "\n</file>"
	return []llm.Message{{Role: "system", Content: WorkerSystem}, {Role: "user", Content: user}}
}

// stripFence is delegate._strip_fence.
func stripFence(text string) string {
	t := pystr.Strip(text)
	if strings.HasPrefix(t, "```") {
		nl := strings.Index(t, "\n")
		if nl < 0 {
			return t
		}
		t = pystr.RStrip(t[nl+1:])
		t = strings.TrimSuffix(t, "```")
	}
	return pystr.Strip(t)
}

// Answer is delegate.WorkerAnswer.
type Answer struct {
	Answer  string
	Quotes  []string
	Dropped int
	Cut     bool
}

// CheckReply is delegate.check_reply: the answer with its checked quotes,
// or why the reply is not one. unsupported is true for a reply pyjson
// does not model.
func CheckReply(text, fileText string) (a *Answer, why string, unsupported bool) {
	v, err := pyjson.LoadsPy(stripFence(text), 900)
	if err != nil {
		return nil, "the worker's reply is not the JSON object asked for", err.TooDeep
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, "the worker's reply is not the JSON object asked for", false
	}
	answer, ok := obj.Value("answer").(string)
	if !ok {
		return nil, "the worker's reply has no answer string", false
	}
	var quotes []any
	if raw, has := obj.Get("quotes"); has {
		q, ok := raw.([]any)
		if !ok {
			return nil, "the worker's reply has quotes that are not a list", false
		}
		quotes = q
	}
	if len(quotes) > MaxQuotes {
		quotes = quotes[:MaxQuotes]
	}
	out := &Answer{Quotes: []string{}}
	for _, raw := range quotes {
		q, isStr := raw.(string)
		if !isStr || pystr.Strip(q) == "" || pystr.Len(q) > MaxQuoteChars || !strings.Contains(fileText, q) {
			out.Dropped++
			continue
		}
		seen := false
		for _, k := range out.Quotes {
			if k == q {
				seen = true
				break
			}
		}
		if !seen {
			out.Quotes = append(out.Quotes, q)
		}
	}
	out.Cut = pystr.Len(answer) > MaxAnswerChars
	if out.Cut {
		answer = pystr.Slice(answer, 0, MaxAnswerChars)
	}
	out.Answer = answer
	return out, "", false
}

// prices is gateway._PRICES_PER_MTOK.
var prices = map[string][2]float64{
	"claude-opus-4-8":  {15.0, 75.0},
	"claude-sonnet-5":  {3.0, 15.0},
	"claude-haiku-4-5": {1.0, 5.0},
}

// PriceWorker is delegate.price_worker: nil for no price.
func PriceWorker(rt Route, in, out *int64) any {
	if rt.Tier == "local" {
		return pyjson.Float(0.0)
	}
	name := rt.Model
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	p, ok := prices[name]
	if !ok || in == nil || out == nil {
		return nil
	}
	return pyjson.Float((float64(*in)*p[0] + float64(*out)*p[1]) / 1_000_000)
}

func tokens(n int) int { return (n + 3) / 4 }

// record is DelegationRecord, key for key.
type record struct {
	at, mode, path, reason                           string
	ok, hasReason                                    bool
	fileBytes, fileLines                             *int
	workerModel, workerTier, workerHost, routeReason *string
	workerIn, workerOut                              *int64
	workerDollars                                    any
	elapsedMS                                        float64
	quotes, dropped                                  int
	kept                                             *int
	keptDollars                                      any
	notEstimated                                     bool
	form                                             *string
	applied                                          any
}

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return pyjson.Int{Text: strconv.Itoa(*p)}
}

func int64OrNil(p *int64) any {
	if p == nil {
		return nil
	}
	return pyjson.Int{Text: strconv.FormatInt(*p, 10)}
}

func strPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func (r *record) object() *pyjson.Object {
	var reason any
	if r.hasReason {
		reason = r.reason
	}
	return pyjson.NewObject().
		Set("at", r.at).Set("mode", r.mode).Set("ok", r.ok).Set("reason", reason).Set("path", r.path).
		Set("file_bytes", intOrNil(r.fileBytes)).Set("file_lines", intOrNil(r.fileLines)).
		Set("worker_model", strPtr(r.workerModel)).Set("worker_tier", strPtr(r.workerTier)).
		Set("worker_host", strPtr(r.workerHost)).Set("route_reason", strPtr(r.routeReason)).
		Set("worker_input_tokens", int64OrNil(r.workerIn)).Set("worker_output_tokens", int64OrNil(r.workerOut)).
		Set("worker_dollars", r.workerDollars).
		Set("elapsed_ms", pyjson.Float(pyjson.Round(r.elapsedMS, 3))).
		Set("quotes", pyjson.Int{Text: strconv.Itoa(r.quotes)}).
		Set("dropped", pyjson.Int{Text: strconv.Itoa(r.dropped)}).
		Set("frontier_tokens_kept", intOrNil(r.kept)).Set("frontier_dollars_kept", r.keptDollars).
		Set("estimated", !r.notEstimated).Set("task_ok", nil).Set("kind", "delegate").
		Set("form", strPtr(r.form)).Set("applied", r.applied)
}

// JournalPath is delegate.journal_path.
func JournalPath(dataDir string) string { return join(join(dataDir, "router"), "delegations.jsonl") }

// appendRecord is delegate.append_record: best-effort.
func appendRecord(dataDir string, r *record) {
	path := JournalPath(dataDir)
	enc, e := pystr.FSEncode(path)
	if e != nil || strings.ContainsRune(path, 0) {
		return
	}
	dir, _ := pystr.FSEncode(join(dataDir, "router"))
	if os.MkdirAll(string(dir), 0o777) != nil {
		return
	}
	_, statErr := os.Stat(string(enc))
	f, err := os.OpenFile(string(enc), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	_, _ = f.WriteString(pyjson.Dumps(r.object(), true) + "\n")
	f.Close()
	if os.IsNotExist(statErr) {
		_ = os.Chmod(string(enc), 0o600)
	}
}

// result is delegate.Result as the tool returns it.
func result(ok bool, mode, path string, reason any, worker any, lines any, a *Answer, minLines string) *pyjson.Object {
	o := pyjson.NewObject().Set("ok", ok).Set("mode", mode).Set("path", path).Set("reason", reason).
		Set("worker", worker).Set("lines", lines)
	if a == nil {
		return withDraft(o.Set("answer", nil).Set("answer_cut", false).Set("quotes", []any{}).
			Set("dropped", pyjson.Int{Text: "0"}).Set("untrusted", nil).Set("exact_text", nil), nil)
	}
	qs := make([]any, len(a.Quotes))
	for i, q := range a.Quotes {
		qs[i] = q
	}
	return withDraft(o.Set("answer", a.Answer).Set("answer_cut", a.Cut).Set("quotes", qs).
		Set("dropped", pyjson.Int{Text: strconv.Itoa(a.Dropped)}).Set("untrusted", UntrustedNote).
		Set("exact_text", ExactTextNote(minLines)), nil)
}

// withDraft sets Result's draft fields: null, or the draft's.
func withDraft(o *pyjson.Object, d *Draft) *pyjson.Object {
	if d == nil {
		return o.Set("form", nil).Set("applies", nil).Set("apply_reason", nil).Set("draft", nil)
	}
	var why any
	if d.Why != "" {
		why = d.Why
	}
	info := ""
	if d.Form == "diff" {
		info = "diff"
	}
	return o.Set("form", d.Form).Set("applies", d.Applies).Set("apply_reason", why).Set("draft", Fence(d.Text, info))
}

// draftResult is delegate.Result for a code write's draft.
func draftResult(mode, path string, worker, lines any, d *Draft) *pyjson.Object {
	o := pyjson.NewObject().Set("ok", true).Set("mode", mode).Set("path", path).Set("reason", nil).
		Set("worker", worker).Set("lines", lines).
		Set("answer", nil).Set("answer_cut", false).Set("quotes", []any{}).
		Set("dropped", pyjson.Int{Text: "0"}).Set("untrusted", DraftNote).Set("exact_text", nil)
	return withDraft(o, d)
}

// Worker makes the worker call: llm_client.complete with a base URL.
type Worker func(model, baseURL string, msgs []llm.Message, o llm.BodyOpts, timeout float64) (*llm.Reply, error)

// Unsupported is input this port does not answer the oracle's way.
type Unsupported struct{ Why string }

func (u *Unsupported) Error() string { return u.Why }

// LoadDefaultEnvelope is gate.load_envelope(None, root): the default envelope,
// nil when none is registered, or ok false when it cannot be read.
func LoadDefaultEnvelope(root string) (env *Envelope, ok bool) {
	path := join(join(root, "envelopes"), "default.json")
	enc, e := pystr.FSEncode(path)
	if e != nil || strings.ContainsRune(path, 0) {
		return nil, true
	}
	if _, err := os.Stat(string(enc)); err != nil {
		if os.IsNotExist(err) {
			return nil, true
		}
		return nil, false
	}
	raw, err := os.ReadFile(string(enc))
	if err != nil {
		return nil, false
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return nil, false
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, text)
	if verr != nil {
		return nil, false
	}
	o := v.(*pyjson.Object)
	perms := o.Value("permissions").(*pyjson.Object)
	out := &Envelope{Stakes: o.Value("stakes").(string), Network: perms.Value("network").(bool)}
	for _, h := range perms.Value("network_hosts").([]any) {
		out.NetworkHosts = append(out.NetworkHosts, h.(string))
	}
	return out, true
}

// Run is delegate.run_delegate for arguments FastMCP has validated as
// strings. It returns the tool's result object; an *Unsupported error
// hands the call back.
func Run(path, question, mode, dataDir string, getenv func(string) (string, bool), call Worker) (*pyjson.Object, error) {
	t0 := time.Now()
	rec := &record{at: NowISO(), mode: mode, path: path,
		workerDollars: nil, keptDollars: nil}
	shown := path
	writing := mode == "code_write"
	rec.notEstimated = writing
	refuse := func(reason string, worker, lines any) (*pyjson.Object, error) {
		rec.reason, rec.hasReason = reason, true
		rec.elapsedMS = float64(time.Since(t0).Nanoseconds()) / 1e6
		appendRecord(dataDir, rec)
		return result(false, mode, shown, reason, worker, lines, nil, ""), nil
	}
	if mode != "bulk_read" && mode != "code_write" {
		return refuse("mode "+pystr.Repr(mode)+" is not built; the modes are bulk_read and code_write", nil, nil)
	}
	if pystr.Strip(question) == "" {
		return refuse("the question is empty", nil, nil)
	}
	if !strings.HasPrefix(path, "/") {
		return refuse("the path must be an absolute path", nil, nil)
	}
	norm := normpath(path)
	rec.path, shown = norm, norm
	root := join(dataDir, "gate")
	rule, err := ActingRule(root)
	if err != nil {
		return nil, &Unsupported{"a graft rule file the port's JSON reader does not model"}
	}
	minLines := "350"
	allowRemote := false
	if rule != nil {
		minLines, allowRemote = rule.MinLines.Text, rule.AllowRemote
	}
	env, ok := LoadDefaultEnvelope(root)
	if !ok {
		return refuse("the gate's default envelope cannot be read", nil, nil)
	}
	if env != nil && env.Stakes == "physical" {
		return refuse("the delegate is refused under physical stakes", nil, nil)
	}
	var m *Measure
	var lines any
	if !writing || Lexists(norm) {
		got, why := MeasureFileFollow(norm, !writing)
		if got == nil {
			return refuse("the file cannot be delegated: "+why, nil, nil)
		}
		m = got
		rec.fileBytes, rec.fileLines = &m.Size, &m.Lines
		lines = pyjson.Int{Text: strconv.Itoa(m.Lines)}
	}
	rt, err := RouteDelegate(dataDir, env, allowRemote, getenv)
	if err != nil {
		return nil, &Unsupported{"a local_tier1.json the port's JSON reader does not model"}
	}
	if rt.Model != "" {
		rec.workerModel = &rt.Model
	}
	if rt.Tier != "" {
		rec.workerTier = &rt.Tier
	}
	if rt.Host != "" {
		rec.workerHost = &rt.Host
	}
	reason := rt.Reason
	rec.routeReason = &reason
	if !rt.OK {
		return refuse(rt.Reason, nil, lines)
	}
	worker := rt.AsObject()
	base := ""
	if rt.BaseURL != nil {
		base = *rt.BaseURL
	}
	var fileText *string
	if m != nil {
		fileText = &m.Text
	}
	var msgs []llm.Message
	opts := llm.BodyOpts{MaxTokens: WorkerMaxTokens, JSONObject: true}
	if writing {
		msgs = WriterMessages(question, basename(norm), fileText)
		opts.MaxTokens = WriterMaxTokens
	} else {
		msgs = Messages(question, basename(norm), m.Text)
	}
	reply, err := call(rt.Model, base, msgs, opts, WorkerTimeoutS)
	if err != nil {
		var le *llm.Error
		if e, isLLM := err.(*llm.Error); isLLM {
			le = e
		}
		if le == nil {
			return nil, &Unsupported{"a worker call this binary does not make the oracle's way (" + err.Error() + ")"}
		}
		return refuse("the worker failed: "+le.Msg, worker, lines)
	}
	rec.workerIn, rec.workerOut = reply.InputTokens, reply.OutputTokens
	rec.workerDollars = PriceWorker(rt, reply.InputTokens, reply.OutputTokens)
	if writing {
		d, why, unsupported := CheckDraft(reply.Text, fileText)
		if unsupported {
			return nil, &Unsupported{"a worker reply nested deeper than this binary reads"}
		}
		if d != nil && pystr.HasSurrogate(d.Text) {
			return nil, &Unsupported{"a worker draft that holds a lone surrogate"}
		}
		if d == nil {
			return refuse(why, worker, lines)
		}
		rec.ok = true
		rec.form, rec.applied = &d.Form, d.Applies
		rec.elapsedMS = float64(time.Since(t0).Nanoseconds()) / 1e6
		appendRecord(dataDir, rec)
		return draftResult(mode, shown, worker, lines, d), nil
	}
	a, why, unsupported := CheckReply(reply.Text, m.Text)
	if unsupported {
		return nil, &Unsupported{"a worker reply nested deeper than this binary reads"}
	}
	if a != nil && pystr.HasSurrogate(a.Answer) {
		return nil, &Unsupported{"a worker answer that holds a lone surrogate"}
	}
	if a == nil {
		return refuse(why, worker, lines)
	}
	returned := len(a.Answer)
	for _, q := range a.Quotes {
		returned += len(q)
	}
	kept := tokens(m.Size) - tokens(returned)
	if kept < 0 {
		kept = 0
	}
	rec.ok = true
	rec.quotes, rec.dropped = len(a.Quotes), a.Dropped
	rec.kept = &kept
	rec.keptDollars = pyjson.Float(float64(kept) * FrontierPerMTok * CacheWriteMult / 1_000_000)
	rec.elapsedMS = float64(time.Since(t0).Nanoseconds()) / 1e6
	appendRecord(dataDir, rec)
	return result(true, mode, shown, nil, worker, lines, a, minLines), nil
}

// normpath is posixpath.normpath.
func normpath(p string) string {
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if c != ".." || (initial == 0 && len(out) == 0) || (len(out) > 0 && out[len(out)-1] == "..") {
			out = append(out, c)
		} else if len(out) > 0 {
			out = out[:len(out)-1]
		}
	}
	s := strings.Repeat("/", initial) + strings.Join(out, "/")
	if s == "" {
		return "."
	}
	return s
}

// basename is os.path.basename.
func basename(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

// StripFence is delegate._strip_fence.
func StripFence(text string) string { return stripFence(text) }

// NowISO is delegate.now_iso.
func NowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }
