package cli

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tracejournal"
)

// tierTokens is accounting._ESTIMATED_TOKENS_PER_CALL.
var tierTokens = map[string]int64{"tier0": 0, "tier1": 2000, "tier2": 4500}

// classifyTier is accounting.classify_tier.
func classifyTier(generatedBy string) string {
	switch {
	case strings.HasPrefix(generatedBy, "compiled-pathway:"):
		return "tier0"
	case strings.HasPrefix(generatedBy, "tier1:"):
		return "tier1"
	}
	return "tier2"
}

// tiersStats is `daisugi tiers stats`: per-tier call counts, estimated
// tokens and the pathway hit rate over the journal (accounting.tier_stats).
func (e *Env) tiersStats(args []string) error {
	const cmd = "tiers stats"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH"},
		{names: []string{"--days"}, value: true, metavar: "INTEGER", help: "Rollup window (days)."},
		{names: []string{"--json"}, help: "Emit stats as JSON."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Show per-tier call counts, estimated tokens, and pathway hit rate.", opts)
	}
	days, err := clickInt(p, "--days", 30)
	if err != nil {
		return e.usage(cmd, err)
	}
	j, err := e.openJournal(cmd, e.dataDir(p))
	if err != nil {
		return err
	}
	defer j.Close()
	cutoff := float64(time.Now().UnixNano())/1e9 - float64(days)*86400
	rows, err := j.ListSuccessful(&cutoff)
	if err != nil {
		if errors.Is(err, tracejournal.ErrUnreadable) {
			return e.refuse(cmd, err)
		}
		return e.failPy(cmd, err)
	}
	byTier := map[string]int64{"tier0": 0, "tier1": 0, "tier2": 0}
	providers := pyjson.NewObject()
	var total int64
	for _, row := range rows {
		if row.TraceID == "" {
			continue
		}
		rec, err := j.LoadTrace(row.TraceID)
		if err != nil {
			var le *tracejournal.LoadError
			if errors.As(err, &le) {
				continue // tier_stats skips a trace it cannot load
			}
			if errors.Is(err, tracejournal.ErrUnreadable) {
				return e.refuse(cmd, err)
			}
			return e.failPy(cmd, err)
		}
		if ts, ok := isoTimestamp(row.CreatedAt); ok && ts < cutoff {
			continue
		}
		gb, _ := rec.Envelope.Value("generated_by").(string)
		tier := classifyTier(gb)
		byTier[tier]++
		total++
		if tier == "tier1" {
			name := strings.TrimPrefix(gb, "tier1:")
			if name == "" {
				name = "unknown"
			}
			n := int64(0)
			if v, ok := providers.Get(name); ok {
				n = v.(int64)
			}
			providers.Set(name, n+1)
		}
	}
	tokens := map[string]int64{}
	var tokensTotal int64
	for _, t := range []string{"tier0", "tier1", "tier2"} {
		tokens[t] = byTier[t] * tierTokens[t]
		tokensTotal += tokens[t]
	}
	rate := 0.0
	if total > 0 {
		rate = float64(byTier["tier0"]) / float64(total)
	}
	if p.flag("--json") {
		tierObj := func(m map[string]int64) *pyjson.Object {
			o := pyjson.NewObject()
			for _, t := range []string{"tier0", "tier1", "tier2"} {
				o.Set(t, pyjson.Int{Text: itoa64(m[t])})
			}
			return o
		}
		prov := pyjson.NewObject()
		for _, k := range providers.Keys() {
			prov.Set(k, pyjson.Int{Text: itoa64(providers.Value(k).(int64))})
		}
		o := pyjson.NewObject().
			Set("window_days", pyjson.Int{Text: itoa64(days)}).
			Set("total", pyjson.Int{Text: itoa64(total)}).
			Set("by_tier", tierObj(byTier)).
			Set("by_tier1_provider", prov).
			Set("estimated_tokens", tierObj(tokens)).
			Set("estimated_tokens_total", pyjson.Int{Text: itoa64(tokensTotal)}).
			Set("pathway_hit_rate", rate)
		e.echo("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "window: last %dd\n", days)
	fmt.Fprintf(&b, "total traces: %d\n", total)
	for _, t := range []string{"tier0", "tier1", "tier2"} {
		fmt.Fprintf(&b, "  %s: %d call(s)  ~%s est tokens\n", t, byTier[t], groupThousands(tokens[t]))
	}
	fmt.Fprintf(&b, "estimated tokens total: ~%s\n", groupThousands(tokensTotal))
	fmt.Fprintf(&b, "pathway hit rate: %s%%\n", pyFixed(rate*100, 1))
	if providers.Len() > 0 {
		b.WriteString("tier1 breakdown:\n")
		keys := append([]string(nil), providers.Keys()...)
		// sorted(items, key=-count): stable, so a tie keeps insertion order.
		sort.SliceStable(keys, func(a, c int) bool {
			return providers.Value(keys[a]).(int64) > providers.Value(keys[c]).(int64)
		})
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %d\n", k, providers.Value(k).(int64))
		}
	}
	e.echo("%s", b.String())
	return nil
}

// groupThousands is format(n, ","): digits grouped by threes with commas.
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// isoTimestamp is accounting._row_ts on a created_at string:
// datetime.fromisoformat (a trailing Z as +00:00) and .timestamp(), a
// naive time read in the local zone. The forms read: a date, then T or a
// space and HH:MM, :SS and a fraction of 1 to 6 digits, then an offset
// +HH:MM. ok is false for any other text.
func isoTimestamp(raw string) (float64, bool) {
	if strings.HasSuffix(raw, "Z") {
		raw = raw[:len(raw)-1] + "+00:00"
	}
	m := isoForm.FindStringSubmatch(raw)
	if m == nil {
		return 0, false
	}
	atoi := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	y, mo, d := atoi(m[1]), atoi(m[2]), atoi(m[3])
	h, mi, sec := atoi(m[4]), atoi(m[5]), atoi(m[6])
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sec > 59 {
		return 0, false
	}
	frac := 0.0
	if m[7] != "" {
		frac, _ = strconv.ParseFloat("0."+m[7], 64)
	}
	if m[8] == "" {
		t := time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.Local)
		return float64(t.Unix()) + frac, true
	}
	off := atoi(m[9])*3600 + atoi(m[10])*60
	if m[8] == "-" {
		off = -off
	}
	t := time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.UTC)
	return float64(t.Unix()-int64(off)) + frac, true
}

var isoForm = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,6}))?)?(?:([+-])(\d{2}):(\d{2}))?)?$`)
