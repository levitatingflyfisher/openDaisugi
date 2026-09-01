package cli

import (
	"math"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/delegate"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// router_report: the router's weekly measure and the delegate rule and
// worker in force, for router status.

const maxWeeks = 8

// intLimit is router_report._INT_LIMIT.
var intLimit = new(big.Int).Lsh(big.NewInt(1), 53)

// measureInt is router_report._int.
func measureInt(v any) int64 {
	i, ok := v.(pyjson.Int)
	if !ok {
		return 0
	}
	n, ok := new(big.Int).SetString(i.Text, 10)
	if !ok || n.CmpAbs(intLimit) > 0 {
		return 0
	}
	return n.Int64()
}

// measureNum is router_report._num: ok false for no finite float.
func measureNum(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case pyjson.Float:
		f = float64(x)
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok {
			return 0, false
		}
		f, _ = new(big.Float).SetInt(n).Float64()
	default:
		return 0, false
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

func measureFloat(v any) float64 {
	f, _ := measureNum(v)
	return f
}

// readJSONL is router_report._read_jsonl: each line that reads as a JSON
// object.
func readJSONL(path string) []*pyjson.Object {
	enc, e := pystr.FSEncode(path)
	if e != nil || strings.ContainsRune(path, 0) {
		return nil
	}
	raw, err := os.ReadFile(string(enc))
	if err != nil {
		return nil
	}
	var out []*pyjson.Object
	// read_text: universal newlines, so a lone \r ends a line too.
	text := strings.ReplaceAll(strings.ReplaceAll(pystr.DecodeReplace(raw), "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		if pystr.Strip(line) == "" {
			continue
		}
		v, err := pyjson.Loads(line)
		if err != nil {
			continue
		}
		if o, ok := v.(*pyjson.Object); ok {
			out = append(out, o)
		}
	}
	return out
}

var isoStamp = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})Z$`)

var monthDays = [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func isoWeekOf(t time.Time) string {
	y, w := t.ISOWeek()
	return pad4(y) + "-W" + pad2(w)
}

func pad4(n int) string { return strings.Repeat("0", 4-len(strconv.Itoa(n))) + strconv.Itoa(n) }
func pad2(n int) string { return strings.Repeat("0", 2-len(strconv.Itoa(n))) + strconv.Itoa(n) }

// weekOfISO is router_report.week_of_iso: "" where Python has None.
func weekOfISO(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	m := isoStamp.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	n := make([]int, 6)
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	y, mo, d := n[0], n[1], n[2]
	if y < 1 || mo < 1 || mo > 12 || n[3] > 23 || n[4] > 59 || n[5] > 59 {
		return ""
	}
	days := monthDays[mo]
	if mo == 2 && (y%4 == 0 && (y%100 != 0 || y%400 == 0)) {
		days = 29
	}
	if d < 1 || d > days {
		return ""
	}
	return isoWeekOf(time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC))
}

// weekOfEpoch is router_report.week_of_epoch: "" where Python has None.
func weekOfEpoch(v any) string {
	const first, last = -62135596800, 253402300799
	var sec int64
	switch x := v.(type) {
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok || n.Cmp(big.NewInt(first)) < 0 || n.Cmp(big.NewInt(last)) > 0 {
			return ""
		}
		sec = n.Int64()
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return ""
		}
		fl := math.Floor(f)
		if fl < first || fl > last {
			return ""
		}
		sec = int64(fl)
	default:
		return ""
	}
	return isoWeekOf(time.Unix(sec, 0).UTC())
}

// week is one row of router_report.weekly.
type week struct {
	name                                          string
	turns, turnsDown, turnSaved, turnsEst         int64
	turnDollars, turnMS                           float64
	redirects, notRedirected, delegations, ok     int64
	delegateMS                                    float64
	quotes, dropped, workerTokens, workerUnpriced int64
	workerDollars                                 float64
	kept                                          int64
	keptDollars                                   float64
	pass, fail, unknown, tokensSaved              int64
	dollarsSaved                                  float64
	estimated                                     bool
}

func (w *week) object() *pyjson.Object {
	i := func(n int64) pyjson.Int { return pyjson.Int{Text: strconv.FormatInt(n, 10)} }
	f := func(x float64) pyjson.Float { return pyjson.Float(x) }
	return pyjson.NewObject().Set("week", w.name).
		Set("turns", i(w.turns)).Set("turns_downgraded", i(w.turnsDown)).
		Set("turn_tokens_saved", i(w.turnSaved)).Set("turn_dollars_saved", f(w.turnDollars)).
		Set("turns_estimated", i(w.turnsEst)).Set("turn_elapsed_ms", f(w.turnMS)).
		Set("escalations", i(0)).Set("redirects", i(w.redirects)).Set("not_redirected", i(w.notRedirected)).
		Set("delegations", i(w.delegations)).Set("delegate_ok", i(w.ok)).
		Set("delegate_elapsed_ms", f(w.delegateMS)).Set("quotes", i(w.quotes)).Set("dropped", i(w.dropped)).
		Set("worker_tokens", i(w.workerTokens)).Set("worker_dollars", f(w.workerDollars)).
		Set("worker_unpriced", i(w.workerUnpriced)).Set("delegate_tokens_kept", i(w.kept)).
		Set("delegate_dollars_kept", f(w.keptDollars)).Set("task_pass", i(w.pass)).
		Set("task_fail", i(w.fail)).Set("task_unknown", i(w.unknown)).
		Set("tokens_saved", i(w.tokensSaved)).Set("dollars_saved", f(w.dollarsSaved)).
		Set("estimated", w.estimated)
}

// graftEvents is router_report.read_graft_events.
func graftEvents(dataDir string) []*pyjson.Object {
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
		for _, rec := range readJSONL(dir + "/" + n) {
			if _, ok := rec.Value("graft").(*pyjson.Object); ok {
				out = append(out, rec)
			}
		}
	}
	return out
}

func filepath2(parts ...string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		if strings.HasSuffix(out, "/") {
			out += p
		} else {
			out += "/" + p
		}
	}
	return out
}

// weekly is router_report.weekly.
func weekly(dataDir string) []*week {
	weeks := map[string]*week{}
	row := func(name string) *week {
		if w, ok := weeks[name]; ok {
			return w
		}
		w := &week{name: name}
		weeks[name] = w
		return w
	}
	for _, t := range readJSONL(filepath2(dataDir, "gateway", "turns.jsonl")) {
		name := weekOfISO(t.Value("created_at"))
		if name == "" {
			continue
		}
		r := row(name)
		r.turns++
		if t.Value("downgraded") == true {
			r.turnsDown++
		}
		r.turnSaved += measureInt(t.Value("frontier_tokens_saved"))
		r.turnDollars += measureFloat(t.Value("counterfactual_dollars")) - measureFloat(t.Value("actual_dollars"))
		if t.Value("estimated") == true {
			r.turnsEst++
		}
		r.turnMS += measureFloat(t.Value("elapsed_ms"))
	}
	for _, e := range graftEvents(dataDir) {
		name := weekOfEpoch(e.Value("at"))
		if name == "" {
			continue
		}
		r := row(name)
		if e.Value("graft").(*pyjson.Object).Value("applied") == true {
			r.redirects++
		} else {
			r.notRedirected++
		}
	}
	for _, d := range readJSONL(delegate.JournalPath(dataDir)) {
		name := weekOfISO(d.Value("at"))
		if name == "" {
			continue
		}
		r := row(name)
		r.delegations++
		ok := d.Value("ok") == true
		if ok {
			r.ok++
			r.delegateMS += measureFloat(d.Value("elapsed_ms"))
		}
		r.quotes += measureInt(d.Value("quotes"))
		r.dropped += measureInt(d.Value("dropped"))
		r.workerTokens += measureInt(d.Value("worker_input_tokens")) + measureInt(d.Value("worker_output_tokens"))
		if ok {
			if wd, has := measureNum(d.Value("worker_dollars")); has {
				r.workerDollars += wd
			} else {
				r.workerUnpriced++
			}
		}
		r.kept += measureInt(d.Value("frontier_tokens_kept"))
		r.keptDollars += measureFloat(d.Value("frontier_dollars_kept"))
		switch d.Value("task_ok") {
		case true:
			r.pass++
		case false:
			r.fail++
		default:
			r.unknown++
		}
	}
	names := make([]string, 0, len(weeks))
	for n := range weeks {
		names = append(names, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > maxWeeks {
		names = names[:maxWeeks]
	}
	out := make([]*week, len(names))
	for i, n := range names {
		r := weeks[n]
		r.tokensSaved = r.turnSaved + r.kept
		r.dollarsSaved = r.turnDollars + r.keptDollars - r.workerDollars
		r.estimated = r.turnsEst > 0 || r.ok > 0
		out[i] = r
	}
	return out
}

// groupInt is Python's f"{n:,}".
func groupInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fixed is Python's f"{x:.{prec}f}".
func fixed(x float64, prec int) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	return strconv.FormatFloat(x, 'f', prec, 64)
}

// weekLine is router_report.week_line.
func weekLine(r *week) string {
	est := ""
	if r.estimated {
		est = " (estimated)"
	}
	refused := r.delegations - r.ok
	unpriced := ""
	if r.workerUnpriced != 0 {
		unpriced = ", " + strconv.FormatInt(r.workerUnpriced, 10) + " unpriced"
	}
	each := ""
	if r.ok != 0 {
		each = ", " + fixed(r.delegateMS/1000/float64(r.ok), 1) + "s each"
	}
	d := func(n int64) string { return strconv.FormatInt(n, 10) }
	return r.name + ": tokens saved " + groupInt(r.tokensSaved) + est +
		", billed saved $" + fixed(r.dollarsSaved, 4) + est + "; " +
		"turns " + d(r.turns) + " (" + d(r.turnsDown) + " routed cheaper), escalations 0; " +
		"delegations " + d(r.delegations) + " (" + d(r.ok) + " ok" + each + ", " + d(refused) + " refused), " +
		"quotes " + d(r.quotes) + " kept, " + d(r.dropped) + " dropped, " +
		"worker " + groupInt(r.workerTokens) + " tokens $" + fixed(r.workerDollars, 4) + unpriced + "; " +
		"reads redirected " + d(r.redirects) + ", not redirected " + d(r.notRedirected) + "; " +
		"tasks " + d(r.pass) + " passed, " + d(r.fail) + " failed, " + d(r.unknown) + " unknown"
}

// delegateState is router_report.delegate_state: the JSON object and the
// text lines.
func (e *Env) delegateState(dataDir string) (*pyjson.Object, []string, error) {
	root := filepath2(dataDir, "gate")
	rules, bad, err := delegate.LoadRules(root)
	if err != nil {
		return nil, nil, err
	}
	var rule *delegate.Rule
	for _, r := range rules {
		if r.Acting() {
			rule = r
			break
		}
	}
	var rt delegate.Route
	env, ok := delegate.LoadDefaultEnvelope(root)
	if !ok {
		rt = delegate.Route{Reason: "no worker: the gate's default envelope cannot be read"}
	} else {
		rt, err = delegate.RouteDelegate(dataDir, env, rule != nil && rule.AllowRemote, e.lookup)
		if err != nil {
			return nil, nil, err
		}
	}
	o := pyjson.NewObject()
	lines := []string{"delegate (large reads):"}
	if rule == nil {
		o.Set("rule", nil)
		lines = append(lines, "  rule: none. Reads are not redirected (a rule file in <gate root>/grafts).")
	} else {
		o.Set("rule", pyjson.NewObject().Set("file", rule.File).Set("id", rule.ID).Set("version", rule.Version).
			Set("state", rule.State).Set("file_lines_over", rule.MinLines).Set("allow_remote", rule.AllowRemote))
		verb := "would go to (audit: not denied)"
		if rule.State == "active" {
			verb = "go to"
		}
		lines = append(lines, "  rule: "+rule.ID+" v"+rule.Version.Text+" ("+rule.File+"), "+rule.State+
			": reads over "+rule.MinLines.Text+" lines "+verb+" the delegate tool")
	}
	unused := []any{}
	for _, r := range rules {
		if r == rule {
			continue
		}
		why := "its state is " + r.State
		if r.Acting() {
			why = "an earlier rule file is in force"
		}
		unused = append(unused, pyjson.NewObject().Set("file", r.File).Set("id", r.ID).Set("state", r.State).Set("why", why))
		lines = append(lines, "  rule "+r.ID+" ("+r.File+") is not in force: "+why)
	}
	o.Set("unused_rules", unused)
	badList := []any{}
	for _, b := range bad {
		badList = append(badList, pyjson.NewObject().Set("file", b.File).Set("why", b.Why))
		lines = append(lines, "  rule file "+b.File+" is not used: "+b.Why)
	}
	o.Set("bad_rules", badList)
	if rt.OK {
		o.Set("worker", rt.AsObject())
		lines = append(lines, "  worker: "+rt.Model+" ("+rt.Tier+", "+rt.Host+")")
	} else {
		o.Set("worker", nil)
		lines = append(lines, "  worker: none. "+rt.Reason+".")
		if rule != nil {
			lines = append(lines, "  With no worker, reads over the threshold go through the normal gate.")
		}
	}
	o.Set("worker_reason", rt.Reason)
	return o, lines, nil
}
