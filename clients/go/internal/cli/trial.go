package cli

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"daisugi-verify/internal/delegate"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// The promotion meter's constants (router_report.py).
const (
	minLabeled         = 3
	maxTranscriptBytes = 64 * 1024 * 1024
	maxNoteChars       = 500
)

// errTooDeep is a transcript line nested deeper than this binary reads.
var errTooDeep = errors.New("a transcript line nested deeper than this binary reads")

// errTooMany is a token sum past 2**53, which this binary does not sum the
// way Python does.
var errTooMany = errors.New("a token count past 2**53 in sum, which this binary does not add")

const sumLimit = int64(1) << 53

func labelsPath(dataDir string) string { return filepath2(dataDir, "router", "labels.jsonl") }

// sessionOK is router_report.session_ok.
func sessionOK(s string) bool { return gateroot.SafeSessionID(s) == s }

// readLabels is router_report.read_labels: the last good row wins.
func readLabels(dataDir string) map[string]string {
	out := map[string]string{}
	for _, row := range readJSONL(labelsPath(dataDir)) {
		s, ok := row.Value("session").(string)
		o, ok2 := row.Value("outcome").(string)
		if ok && ok2 && sessionOK(s) && (o == "pass" || o == "fail") {
			out[s] = o
		}
	}
	return out
}

// countOf is router_report._count: an integer from 0 to 2**53, else 0.
func countOf(v any) int64 {
	n := measureInt(v)
	if n < 0 {
		return 0
	}
	return n
}

// readCapped is router_report._read_capped.
func readCapped(path string) []byte {
	if strings.ContainsRune(path, 0) {
		return nil
	}
	enc, e := pystr.FSEncode(path)
	if e != nil {
		return nil
	}
	fd, err := syscall.Open(string(enc), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Size > maxTranscriptBytes {
		return nil
	}
	var out []byte
	buf := make([]byte, 1<<20)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return nil
		}
		if n == 0 {
			break
		}
		out = append(out, buf[:n]...)
		if len(out) > maxTranscriptBytes {
			return nil
		}
	}
	if out == nil {
		out = []byte{}
	}
	return out
}

// cost is a session's billed cost.
type cost struct {
	dollars   float64
	quota     int64
	estimated bool
}

// sessionCost is router_report.session_cost: nil when the transcript
// cannot be read.
func sessionCost(path string) (*cost, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, nil
	}
	data := readCapped(path)
	if data == nil {
		return nil, nil
	}
	type message struct {
		model string
		usage *pyjson.Object
	}
	var order []string
	msgs := map[string]message{}
	for n, line := range strings.Split(pystr.DecodeReplace(data), "\n") {
		if pystr.Strip(line) == "" {
			continue
		}
		v, err := pyjson.LoadsPy(line, 900)
		if err != nil {
			if err.TooDeep {
				return nil, errTooDeep
			}
			continue
		}
		row, ok := v.(*pyjson.Object)
		if !ok {
			continue
		}
		if t, _ := row.Value("type").(string); t != "assistant" {
			continue
		}
		msg, ok := row.Value("message").(*pyjson.Object)
		if !ok {
			continue
		}
		usage, ok := msg.Value("usage").(*pyjson.Object)
		if !ok {
			continue
		}
		key := "line:" + strconv.Itoa(n)
		if id, ok := msg.Value("id").(string); ok {
			key = "id:" + id
		}
		model, _ := msg.Value("model").(string)
		if _, seen := msgs[key]; !seen {
			order = append(order, key)
		}
		msgs[key] = message{model, usage}
	}
	c := &cost{}
	for _, k := range order {
		m := msgs[k]
		in, cr := countOf(m.usage.Value("input_tokens")), countOf(m.usage.Value("cache_read_input_tokens"))
		cw, out := countOf(m.usage.Value("cache_creation_input_tokens")), countOf(m.usage.Value("output_tokens"))
		d, known := gateway.PriceMessage(m.model, in, out, cr, cw)
		if !known {
			c.estimated = true
		}
		c.dollars += d
		c.quota += in + cr + cw + out
		if in > sumLimit || cr > sumLimit || cw > sumLimit || out > sumLimit || c.quota > sumLimit {
			return nil, errTooMany
		}
	}
	return c, nil
}

// readAudit is router_report._read_audit: every audit record, by file
// name then line.
func readAudit(dataDir string) []*pyjson.Object {
	dir := filepath2(dataDir, "gate", "audit")
	enc, e := pystr.FSEncode(dir)
	if e != nil {
		return nil
	}
	f, err := os.Open(string(enc))
	if err != nil {
		return nil
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil
	}
	var names []string
	for _, ent := range ents {
		n := pystr.FSDecode([]byte(ent.Name()))
		if strings.HasSuffix(n, ".jsonl") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []*pyjson.Object
	for _, n := range names {
		out = append(out, readJSONL(dir+"/"+n)...)
	}
	return out
}

// armRow is one arm of the trial table.
type armRow struct {
	sessions, labeled, passed, failed, unlabeled, costUnknown int64
	dollars                                                   float64
	quota                                                     int64
	estimated                                                 bool
	rate, perSuccess, quotaPerSuccess                         *float64
}

func (r *armRow) object() *pyjson.Object {
	i := func(n int64) pyjson.Int { return pyjson.Int{Text: strconv.FormatInt(n, 10)} }
	opt := func(f *float64) any {
		if f == nil {
			return nil
		}
		return pyjson.Float(*f)
	}
	return pyjson.NewObject().Set("sessions", i(r.sessions)).Set("labeled", i(r.labeled)).
		Set("passed", i(r.passed)).Set("failed", i(r.failed)).Set("unlabeled", i(r.unlabeled)).
		Set("cost_unknown", i(r.costUnknown)).Set("billed_dollars", pyjson.Float(r.dollars)).
		Set("quota_tokens", i(r.quota)).Set("estimated", r.estimated).Set("success_rate", opt(r.rate)).
		Set("dollars_per_success", opt(r.perSuccess)).Set("quota_per_success", opt(r.quotaPerSuccess))
}

// trial is router_report.trial_state.
type trial struct {
	rule                 *delegate.Rule
	graft, control       *armRow
	conflicting          int64
	verdict, verdictText string
}

func (t *trial) object() *pyjson.Object {
	return pyjson.NewObject().Set("rule", t.rule.ID).Set("version", t.rule.Version).Set("state", t.rule.State).
		Set("seed", pyjson.Int{Text: strconv.FormatInt(t.rule.Seed, 10)}).
		Set("min_labeled", pyjson.Int{Text: strconv.Itoa(minLabeled)}).
		Set("arms", pyjson.NewObject().Set("graft", t.graft.object()).Set("control", t.control.object())).
		Set("conflicting", pyjson.Int{Text: strconv.FormatInt(t.conflicting, 10)}).
		Set("verdict", t.verdict).Set("verdict_text", t.verdictText)
}

// trialState is router_report.trial_state: nil when no rule in audit or
// trial is in force.
func trialState(dataDir string) (*trial, error) {
	rules, _, err := delegate.LoadRules(filepath2(dataDir, "gate"))
	if err != nil {
		return nil, err
	}
	var rule *delegate.Rule
	for _, r := range rules {
		if r.Acting() {
			rule = r
			break
		}
	}
	if rule == nil || (rule.State != "audit" && rule.State != "trial") {
		return nil, nil
	}
	arms := map[string]map[string]bool{}
	transcripts := map[string]string{}
	for _, rec := range readAudit(dataDir) {
		session, ok := rec.Value("session_id").(string)
		if !ok {
			continue
		}
		if tp, ok := rec.Value("transcript_path").(string); ok && strings.HasPrefix(tp, "/") {
			transcripts[session] = tp
		}
		g, ok := rec.Value("graft").(*pyjson.Object)
		if !ok {
			continue
		}
		id, _ := g.Value("rule_id").(string)
		arm, _ := g.Value("arm").(string)
		ver, isInt := g.Value("version").(pyjson.Int)
		if id != rule.ID || (arm != "graft" && arm != "control") {
			continue
		}
		if !isInt || strconv.FormatInt(countOf(ver), 10) != rule.Version.Text {
			continue
		}
		if arms[session] == nil {
			arms[session] = map[string]bool{}
		}
		arms[session][arm] = true
	}
	labels := readLabels(dataDir)
	t := &trial{rule: rule, graft: &armRow{}, control: &armRow{}}
	sessions := make([]string, 0, len(arms))
	for s := range arms {
		sessions = append(sessions, s)
	}
	sort.Strings(sessions)
	for _, s := range sessions {
		if len(arms[s]) > 1 {
			t.conflicting++
			continue
		}
		r := t.control
		if arms[s]["graft"] {
			r = t.graft
		}
		r.sessions++
		outcome, has := labels[s]
		if !has {
			r.unlabeled++
			continue
		}
		r.labeled++
		if outcome == "pass" {
			r.passed++
		} else {
			r.failed++
		}
		var c *cost
		if tp, ok := transcripts[s]; ok {
			c, err = sessionCost(tp)
			if err != nil {
				return nil, err
			}
		}
		if c == nil {
			r.costUnknown++
			continue
		}
		r.dollars += c.dollars
		r.quota += c.quota
		if r.quota > sumLimit {
			return nil, errTooMany
		}
		if c.estimated {
			r.estimated = true
		}
	}
	if rule.AllowRemote {
		t.graft.estimated = true
	}
	for _, r := range []*armRow{t.graft, t.control} {
		if r.labeled > 0 {
			v := float64(r.passed) / float64(r.labeled)
			r.rate = &v
		}
		if r.passed > 0 && r.costUnknown == 0 {
			d := r.dollars / float64(r.passed)
			q := float64(r.quota) / float64(r.passed)
			r.perSuccess, r.quotaPerSuccess = &d, &q
		}
	}
	t.verdict, t.verdictText = verdictOf(rule, t.graft, t.control)
	return t, nil
}

// verdictOf is router_report._verdict.
func verdictOf(rule *delegate.Rule, g, c *armRow) (string, string) {
	if rule.State == "audit" {
		return "none", "nothing: the rule is in audit, so both arms only record. " +
			"Set its state to trial to compare them."
	}
	if g.labeled < minLabeled || c.labeled < minLabeled {
		return "wait", "wait: each arm needs " + strconv.Itoa(minLabeled) + " labeled sessions (graft " +
			strconv.FormatInt(g.labeled, 10) + ", control " + strconv.FormatInt(c.labeled, 10) + ")."
	}
	if *g.rate < *c.rate {
		return "retire", "retire rule " + rule.ID + ": the graft arm's success rate " + fixed(*g.rate, 2) +
			" is below the control arm's " + fixed(*c.rate, 2) + "."
	}
	if g.perSuccess == nil || c.perSuccess == nil {
		return "wait", "wait: a labeled session's billed cost is unknown, or an arm has no success."
	}
	if *g.perSuccess < *c.perSuccess {
		return "promote", "promote: set rule " + rule.ID + " to active (billed cost per success $" +
			fixed(*g.perSuccess, 4) + " against $" + fixed(*c.perSuccess, 4) + ")."
	}
	return "keep", "keep the trial: billed cost per success $" + fixed(*g.perSuccess, 4) +
		" is not below $" + fixed(*c.perSuccess, 4) + "."
}

func armLine(name string, r *armRow) string {
	est := ""
	if r.estimated {
		est = " (estimated)"
	}
	per := "per success: unknown"
	if r.perSuccess != nil {
		per = "per success $" + fixed(*r.perSuccess, 4) + est + ", " + fixed(*r.quotaPerSuccess, 0) + " quota tokens"
	}
	unknown := ""
	if r.costUnknown != 0 {
		unknown = ", " + strconv.FormatInt(r.costUnknown, 10) + " with no cost"
	}
	d := func(n int64) string { return strconv.FormatInt(n, 10) }
	return "  " + name + " arm: " + d(r.sessions) + " sessions, " + d(r.labeled) + " labeled (" + d(r.passed) +
		" passed, " + d(r.failed) + " failed), " + d(r.unlabeled) + " unlabeled" + unknown + "; billed $" +
		fixed(r.dollars, 4) + est + ", " + groupInt(r.quota) + " quota tokens; " + per
}

// trialLines is router_report.trial_lines.
func trialLines(t *trial) []string {
	out := []string{"trial (rule " + t.rule.ID + " v" + t.rule.Version.Text + ", " + t.rule.State + ", seed " +
		strconv.FormatInt(t.rule.Seed, 10) + "): promotion is the operator's; nothing is changed here."}
	out = append(out, armLine("graft", t.graft), armLine("control", t.control))
	if t.conflicting != 0 {
		out = append(out, "  "+strconv.FormatInt(t.conflicting, 10)+" sessions with both arms recorded are left out.")
	}
	return append(out, "  promotion would: "+t.verdictText)
}

// routerLabel is `daisugi router label`.
func (e *Env) routerLabel(args []string) error {
	const cmd = "router label"
	opts := []opt{
		{names: []string{"--note"}, value: true, metavar: "TEXT", help: "Why, in a few words."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--json"}, help: "Print the row as JSON."},
	}
	p, err := parseArgs(args, opts, 2)
	if err != nil {
		return e.usageArgs(cmd, "SESSION OUTCOME", err)
	}
	if p.help {
		return e.cmdHelp(cmd, "SESSION OUTCOME", "Record whether a session's task succeeded: the outcome a graft trial counts.", opts)
	}
	if len(p.args) < 2 {
		missing := "session"
		if len(p.args) == 1 {
			missing = "outcome"
		}
		return e.usageArgs(cmd, "SESSION OUTCOME", &usageError{"Missing argument '" + missing + "'."})
	}
	session, outcome := p.args[0], p.args[1]
	if outcome != "pass" && outcome != "fail" {
		e.errf("Error: OUTCOME must be pass or fail.\n")
		return exit(2)
	}
	if !sessionOK(session) {
		e.errf("Error: SESSION must be a session id as the gate names it: 1 to 128 of A-Z a-z 0-9 . _ -, with no dot at either end.\n")
		return exit(2)
	}
	var note any
	if p.has("--note") {
		n := p.str("--note", "")
		if pystr.Len(n) > maxNoteChars {
			e.errf("Error: --note must be at most %d characters.\n", maxNoteChars)
			return exit(2)
		}
		note = n
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	row := pyjson.NewObject().Set("at", delegate.NowISO()).Set("session", session).Set("outcome", outcome).Set("note", note)
	path := labelsPath(dataDir)
	if !appendLabel(dataDir, path, pyjson.Dumps(row, true)+"\n") {
		e.errf("Error: the label could not be written to %s.\n", path)
		return exit(1)
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.Dumps(row, true))
		return nil
	}
	e.out("labeled %s: %s\n", session, outcome)
	return nil
}

// appendLabel is router_report.write_label's file work: false when it
// cannot be written.
func appendLabel(dataDir, path, line string) bool {
	dir, e := pystr.FSEncode(filepath2(dataDir, "router"))
	enc, e2 := pystr.FSEncode(path)
	if e != nil || e2 != nil || strings.ContainsRune(path, 0) {
		return false
	}
	if os.MkdirAll(string(dir), 0o777) != nil {
		return false
	}
	_, statErr := os.Stat(string(enc))
	f, err := os.OpenFile(string(enc), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return false
	}
	_, werr := f.WriteString(line)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return false
	}
	if os.IsNotExist(statErr) {
		if os.Chmod(string(enc), 0o600) != nil {
			return false
		}
	}
	return true
}
