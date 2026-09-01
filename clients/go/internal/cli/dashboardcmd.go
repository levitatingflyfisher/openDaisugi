package cli

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tracejournal"
)

// This file is opendaisugi.dashboard: the live floor, the module map
// with a gauge per stage read from the local stores.

// isTerminal is stream.isatty() for a file.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// stdoutTTY is sys.stdout.isatty().
func (e *Env) stdoutTTY() bool {
	f, ok := e.Stdout.(*os.File)
	return ok && isTerminal(f)
}

// glyphs is console.glyphs(): box drawing on a terminal, ASCII when
// stdout is not one or under --plain.
func (e *Env) glyphs() glyphSet {
	if e.plain || !e.stdoutTTY() {
		return asciiGlyphs
	}
	return boxGlyphs
}

// readPathwayStats is PathwayStore(db).stats() read as int(count) and
// int(total_hits). hitsOK is false when int() of the hits raises (a sum
// that is inf); ok is false when the store cannot be read at all.
func readPathwayStats(db string) (count int64, hits *big.Int, ok bool) {
	s, err := pathways.Open(db)
	if err != nil {
		return 0, nil, false
	}
	defer s.Close()
	c, h, err := s.Stats()
	if err != nil {
		return 0, nil, false
	}
	switch x := h.(type) {
	case int64:
		return c, big.NewInt(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return c, nil, true
		}
		n, _ := big.NewFloat(math.Trunc(x)).Int(nil)
		return c, n, true
	}
	return c, nil, true
}

// rawMetrics is dashboard.RawMetrics: nil where a store is absent or
// cannot be read.
type rawMetrics struct {
	journalTotal, journalPassed, journalFailed *int64
	pathwayCount                               *int64
	pathwayHits                                *big.Int
	gateway                                    *gateway.Summary
}

// loadGatewayRecords reads the gateway's turn journal. A record of
// other types (GW-8) is an error; a file that cannot be read or decoded
// is no records, as the oracle's read_raw swallows the error.
func loadGatewayRecords(dataDir string) ([]gateway.JRecord, error) {
	recs, err := gateway.LoadJournal(filepath.Join(dataDir, "gateway", "turns.jsonl"))
	if err != nil {
		if errors.Is(err, gateway.ErrUnreadable) || !errors.Is(err, gateway.ErrJournal) {
			return nil, nil
		}
		return nil, err
	}
	return recs, nil
}

// gatewayRecords is loadGatewayRecords before anything is written: a
// record of other types is refused, as the report commands refuse it.
func (e *Env) gatewayRecords(cmd, dataDir string) ([]gateway.JRecord, error) {
	recs, err := loadGatewayRecords(dataDir)
	if err != nil {
		return nil, e.refuse(cmd, err)
	}
	return recs, nil
}

// readRaw is dashboard.read_raw, given the gateway records read first.
func (e *Env) readRaw(dataDir string, recs []gateway.JRecord) rawMetrics {
	var raw rawMetrics
	if exists(filepath.Join(dataDir, "journal", "index.db")) {
		if j, err := tracejournal.OpenReadOnly(dataDir); err == nil {
			t, p, f, _, err := j.Stats()
			j.Close()
			if err == nil {
				raw.journalTotal, raw.journalPassed, raw.journalFailed = &t, &p, &f
			}
		}
	}
	if db := filepath.Join(dataDir, "pathways.db"); exists(db) {
		if c, h, ok := readPathwayStats(db); ok {
			raw.pathwayCount, raw.pathwayHits = &c, h
		}
	}
	if len(recs) > 0 {
		s := gateway.Summarize(recs)
		raw.gateway = &s
	}
	return raw
}

// gaugeT is dashboard.Gauge.
type gaugeT struct {
	label, value, detail string
	fraction             *float64
}

func blankGauge(label, why string) gaugeT { return gaugeT{label, "—", why, nil} }

func ratio(num, denom int64) *float64 {
	if denom == 0 {
		return nil
	}
	f := float64(num) / float64(denom)
	return &f
}

// groupedFixed is format(x, ",.<n>f").
func groupedFixed(x float64, n int) string {
	s := pyFixed(x, n)
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return s
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	ip, fp, _ := strings.Cut(s, ".")
	n2, _ := new(big.Int).SetString(ip, 10)
	out := commas(n2)
	if fp != "" {
		out += "." + fp
	}
	if neg {
		return "-" + out
	}
	return out
}

// commas is format(n, ",").
func commas(n *big.Int) string {
	s := n.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
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

func commas64(n int64) string { return commas(big.NewInt(n)) }

// percent0 is format(x, ".0%").
func percent0(x float64) string { return pyFixed(x*100, 0) + "%" }

var errHitsNone = errors.New("TypeError: unsupported format string passed to NoneType.__format__")

// collectMetrics is dashboard.collect_metrics.
func collectMetrics(raw rawMetrics) (map[string][]gaugeT, error) {
	var stores gaugeT
	if raw.journalTotal == nil {
		stores = blankGauge("traces", "no journal yet — run `daisugi onboard`")
	} else {
		stores = gaugeT{"traces", commas64(*raw.journalTotal),
			commas64(*raw.journalPassed) + " passed · " + commas64(*raw.journalFailed) + " failed",
			ratio(*raw.journalPassed, *raw.journalTotal)}
	}
	var matcher, distill gaugeT
	if raw.pathwayCount == nil {
		matcher = blankGauge("reuse hits", "no pathways yet — run `daisugi tend`")
		distill = blankGauge("pathways", "no pathways yet — run `daisugi tend`")
	} else {
		if raw.pathwayHits == nil {
			return nil, errHitsNone
		}
		matcher = gaugeT{"reuse hits", commas(raw.pathwayHits), commas64(*raw.pathwayCount) + " pathway(s) stored", nil}
		distill = gaugeT{"pathways", commas64(*raw.pathwayCount), commas(raw.pathwayHits) + " lifetime reuse hit(s)", nil}
	}
	var router gaugeT
	if g := raw.gateway; g == nil {
		router = blankGauge("tokens saved", "no gateway turns recorded yet")
	} else {
		router = gaugeT{"tokens saved", commas(g.FrontierSaved),
			fmt.Sprintf("$%s · %sx · cache %s · %d/%d downgraded", groupedFixed(g.DollarsSaved, 2),
				pyFixed(g.Blended, 2), percent0(g.CacheHitRate), g.Downgraded, g.Turns),
			ratio(int64(g.Downgraded), int64(g.Turns))}
	}
	return map[string][]gaugeT{
		"harness":  {{"detected", "see map", "active harnesses marked ● above", nil}},
		"gate":     {blankGauge("throughput", "no passive counter yet")},
		"verifier": {blankGauge("latency", "benchmark-only — run `daisugi guard-cost`")},
		"matcher":  {matcher},
		"distill":  {distill},
		"router":   {router},
		"stores":   {stores},
	}, nil
}

// renderDashboard is dashboard.render_dashboard: one frame. pulse < 0
// lights no stage.
func renderDashboard(dataDir string, stages []wStage, metrics map[string][]gaugeT, pulse int, g glyphSet) string {
	width := wiringWidth
	inner := width - 4
	out := []string{"openDaisugi — live floor   (data dir: " + dataDir + ")", "", "  a task from your agent"}
	n := len(stages)
	for i, st := range stages {
		out = append(out, "        "+g.v)
		if pulse >= 0 && pulse%n == i {
			out = append(out, "        "+g.down+" "+g.on)
		} else {
			out = append(out, "        "+g.down)
		}
		head := g.tl + g.h + " " + st.title + " "
		head = head + strings.Repeat(g.h, max(0, width-len([]rune(head))-1)) + g.tr
		out = append(out, head, boxLine(g, st.role, inner))
		out = append(out, moduleRows(g, st, inner)...)
		for _, gg := range metrics[st.key] {
			text := g.flow + " " + gg.label + " " + gg.value
			if gg.detail != "" {
				text += "  (" + gg.detail + ")"
			}
			out = append(out, boxLine(g, "  "+text, inner))
		}
		out = append(out, g.bl+strings.Repeat(g.h, width-2)+g.br)
	}
	out = append(out, "        "+g.v, "        "+g.down, "  verified action runs  (or falls back / is refused)", "")
	out = append(out, fmt.Sprintf("legend:  %s active   %s available   %s possible    %s live gauge (read-only)",
		g.on, g.avail, g.off, g.flow))
	return strings.Join(out, "\n")
}

// dashboardJSON is dashboard.dashboard_json: the wiring JSON with each
// stage's gauges added.
func dashboardJSON(stages []wStage, metrics map[string][]gaugeT) string {
	out := make([]*pyjson.Object, 0, len(stages))
	for _, st := range stages {
		gs := make([]*pyjson.Object, 0)
		for _, gg := range metrics[st.key] {
			var frac any
			if gg.fraction != nil {
				frac = *gg.fraction
			}
			gs = append(gs, pyjson.NewObject().Set("label", gg.label).Set("value", gg.value).
				Set("detail", gg.detail).Set("fraction", frac))
		}
		out = append(out, stageObject(st).Set("metrics", gs))
	}
	return pyjson.DumpsIndent(out, 2, true)
}

// dashboardFrame reads the stores and draws one frame, in the oracle's
// order: the map first (which makes the journal), then the gauges.
func (e *Env) dashboardFrame(cmd, dataDir string) (string, error) {
	recs, err := e.gatewayRecords(cmd, dataDir)
	if err != nil {
		return "", err
	}
	stages, err := e.detectStages(cmd, dataDir)
	if err != nil {
		return "", err
	}
	metrics, err := collectMetrics(e.readRaw(dataDir, recs))
	if err != nil {
		return "", e.fail(cmd, err)
	}
	return renderDashboard(dataDir, stages, metrics, -1, e.glyphs()), nil
}

// runLive is dashboard.run_live: one frame when stdout is not a
// terminal; on a terminal, a frame every interval seconds, the map read
// once and the gauges on each tick, until Ctrl-C. interrupted is what
// Ctrl-C ends the command with.
func (e *Env) runLive(cmd, dataDir string, interval float64, interrupted func() error) error {
	if !e.stdoutTTY() {
		frame, err := e.dashboardFrame(cmd, dataDir)
		if err != nil {
			return err
		}
		e.out("%s\n", frame)
		return nil
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	recs, err := e.gatewayRecords(cmd, dataDir)
	if err != nil {
		return err
	}
	stages, err := e.detectStages(cmd, dataDir)
	if err != nil {
		return err
	}
	g := e.glyphs()
	for i := 0; ; i++ {
		if i > 0 {
			if recs, err = e.gatewayRecords(cmd, dataDir); err != nil {
				return err
			}
		}
		metrics, err := collectMetrics(e.readRaw(dataDir, recs))
		if err != nil {
			return e.fail(cmd, err)
		}
		if !e.plain {
			e.out("\x1b[H\x1b[2J")
		}
		e.out("%s\n", renderDashboard(dataDir, stages, metrics, i, g))
		if math.IsNaN(interval) || interval < 0 || math.IsInf(interval, 0) {
			e.errf("daisugi %s: ValueError: sleep length must be a finite number of seconds, zero or more\n", cmd)
			return exit(1)
		}
		select {
		case <-sig:
			return interrupted()
		case <-time.After(time.Duration(interval * float64(time.Second))):
		}
	}
}

// dashboardCmd is `daisugi dashboard`.
func (e *Env) dashboardCmd(args []string) error {
	const cmd = "dashboard"
	opts := []opt{
		dataDirOpt,
		{names: []string{"--once"}, help: "Render one frame and exit (no live loop)."},
		{names: []string{"--json"}, help: "Emit live wiring as JSON (superset of `modules --json`)."},
		{names: []string{"--interval"}, value: true, metavar: "FLOAT", help: "Seconds between live refreshes."},
		{names: []string{"--tui"}, help: "Interactive Textual GUI; not in this binary yet."},
		{names: []string{"--serve"}, help: "The Textual GUI in a browser; not in this binary yet."},
		{names: []string{"--host"}, value: true, metavar: "TEXT", help: "Bind host for --serve."},
		{names: []string{"--port"}, value: true, metavar: "INTEGER", help: "Bind port for --serve."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "The live 'factory floor': the module map with real throughput gauges.", opts)
	}
	interval, err := clickFloat(p, "--interval", 2.0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if _, err := clickInt(p, "--port", 8000); err != nil {
		return e.usage(cmd, err)
	}
	dataDir := e.dataDir(p)
	if p.flag("--json") {
		recs, err := e.gatewayRecords(cmd, dataDir)
		if err != nil {
			return err
		}
		stages, err := e.detectStages(cmd, dataDir)
		if err != nil {
			return err
		}
		metrics, err := collectMetrics(e.readRaw(dataDir, recs))
		if err != nil {
			return e.fail(cmd, err)
		}
		e.out("%s\n", dashboardJSON(stages, metrics))
		return nil
	}
	if p.flag("--once") {
		frame, err := e.dashboardFrame(cmd, dataDir)
		if err != nil {
			return err
		}
		e.out("%s\n", frame)
		return nil
	}
	if p.flag("--serve") {
		return e.notYet("daisugi dashboard --serve (the Textual GUI)")
	}
	if p.flag("--tui") {
		return e.notYet("daisugi dashboard --tui (the Textual GUI)")
	}
	return e.runLive(cmd, dataDir, interval, func() error {
		e.out("\n")
		return nil
	})
}

// pyReprFloat is repr(float(x)).
func pyReprFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return pyjson.FloatRepr(f)
}
