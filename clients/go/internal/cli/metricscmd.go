package cli

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// This file is opendaisugi.exporter: the numbers the dashboard shows,
// in the Prometheus text exposition format (0.0.4), printed once or
// served at /metrics.

const promContentType = "text/plain; version=0.0.4; charset=utf-8"

type promSample struct {
	labels string // `status="passed"`, or "" for none
	value  string
}

func promMetric(lines []string, name, typ, help string, samples ...promSample) []string {
	lines = append(lines, "# HELP "+name+" "+help, "# TYPE "+name+" "+typ)
	for _, s := range samples {
		l := name
		if s.labels != "" {
			l += "{" + s.labels + "}"
		}
		lines = append(lines, l+" "+s.value)
	}
	return lines
}

func one(v string) promSample { return promSample{"", v} }

var errPromHitsNone = errors.New("TypeError: float() argument must be a string or a real number, not 'NoneType'")

// renderPrometheus is exporter.render_prometheus over the raw readings.
func renderPrometheus(raw rawMetrics) (string, error) {
	i64 := func(n int64) string { return strconv.FormatInt(n, 10) }
	var lines []string
	lines = promMetric(lines, "daisugi_up", "gauge", "1 if the daisugi exporter responded.", one("1"))
	if raw.journalTotal != nil {
		lines = promMetric(lines, "daisugi_journal_traces", "gauge",
			"Verified traces recorded in the journal, by outcome.",
			promSample{`status="passed"`, i64(*raw.journalPassed)},
			promSample{`status="failed"`, i64(*raw.journalFailed)},
			promSample{`status="total"`, i64(*raw.journalTotal)})
	}
	if raw.pathwayCount != nil {
		if raw.pathwayHits == nil {
			return "", errPromHitsNone
		}
		lines = promMetric(lines, "daisugi_pathways", "gauge", "Distilled pathways stored.", one(i64(*raw.pathwayCount)))
		lines = promMetric(lines, "daisugi_pathway_reuse_hits_total", "counter", "Lifetime pathway reuse hits.",
			one(raw.pathwayHits.String()))
	}
	if g := raw.gateway; g != nil {
		lines = promMetric(lines, "daisugi_gateway_turns_total", "counter", "Gateway turns routed.", one(strconv.Itoa(g.Turns)))
		lines = promMetric(lines, "daisugi_gateway_downgraded_turns_total", "counter",
			"Gateway turns routed to a cheaper model than requested.", one(strconv.Itoa(g.Downgraded)))
		lines = promMetric(lines, "daisugi_gateway_frontier_tokens_saved_total", "counter",
			"Tokens kept off the frontier model by routing.", one(new(big.Int).Set(g.FrontierSaved).String()))
		lines = promMetric(lines, "daisugi_gateway_dollars_saved", "gauge",
			"Estimated dollars saved by routing (best-effort).", one(pyReprFloat(g.DollarsSaved)))
		lines = promMetric(lines, "daisugi_gateway_blended_multiplier", "gauge",
			"Blended cost multiplier versus all-frontier routing.", one(pyReprFloat(g.Blended)))
		lines = promMetric(lines, "daisugi_gateway_cache_hit_rate", "gauge",
			"Share of input tokens served from cache (0..1).", one(pyReprFloat(g.CacheHitRate)))
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// prometheusText reads the stores and renders them. A gateway record of
// other types is refused (GW-8) before any store is opened.
func (e *Env) prometheusText(cmd, dataDir string) (string, error) {
	recs, err := e.gatewayRecords(cmd, dataDir)
	if err != nil {
		return "", err
	}
	text, err := renderPrometheus(e.readRaw(dataDir, recs))
	if err != nil {
		return "", e.fail(cmd, err)
	}
	return text, nil
}

// pyErrorPage is http.server's DEFAULT_ERROR_MESSAGE, filled as
// send_error fills it.
func pyErrorPage(code int, message, explain string) string {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	return "<!DOCTYPE HTML>\n<html lang=\"en\">\n    <head>\n        <meta charset=\"utf-8\">\n" +
		"        <title>Error response</title>\n    </head>\n    <body>\n        <h1>Error response</h1>\n" +
		fmt.Sprintf("        <p>Error code: %d</p>\n        <p>Message: %s.</p>\n", code, esc(message)) +
		fmt.Sprintf("        <p>Error code explanation: %d - %s.</p>\n", code, esc(explain)) +
		"    </body>\n</html>\n"
}

// metricsHandler is exporter.make_metrics_server's handler: GET of a
// path that is "" or "/metrics" once its trailing slashes go answers the
// exposition; any other GET is a 404 with no body; any other method is
// the 501 BaseHTTPRequestHandler sends.
func (e *Env) metricsHandler(dataDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			body := pyErrorPage(501, fmt.Sprintf("Unsupported method ('%s')", r.Method),
				"Server does not support this operation")
			w.Header().Set("Content-Type", "text/html;charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(501)
			if r.Method != "HEAD" {
				_, _ = w.Write([]byte(body))
			}
			return
		}
		path := strings.TrimRight(r.RequestURI, "/")
		if path != "" && path != "/metrics" {
			w.WriteHeader(404)
			return
		}
		recs, err := loadGatewayRecords(dataDir)
		if err != nil {
			// A record this binary does not read the oracle's way: no
			// numbers rather than other numbers (K4-6).
			e.errf("daisugi metrics: %v\n", err)
			w.WriteHeader(500)
			return
		}
		text, err := renderPrometheus(e.readRaw(dataDir, recs))
		if err != nil {
			e.errf("daisugi metrics: %v\n", err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", promContentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(text)))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(text))
	})
}

// serveMetrics is exporter.serve_metrics: the endpoint on host:port
// until Ctrl-C, which ends it with a newline and exit 0.
func (e *Env) serveMetrics(dataDir, host string, port int64) error {
	e.out("serving Prometheus metrics at http://%s:%d/metrics (Ctrl-C to stop)\n", host, port)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.FormatInt(port, 10)))
	if err != nil {
		var no syscall.Errno
		if errors.As(err, &no) {
			e.errf("daisugi metrics: OSError: [Errno %d] %s\n", int(no), pyStrerror(no))
		} else {
			e.errf("daisugi metrics: OSError: %v\n", err)
		}
		return exit(1)
	}
	srv := &http.Server{Handler: e.metricsHandler(dataDir)}
	srv.SetKeepAlivesEnabled(false)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case <-sig:
		_ = srv.Close()
		e.out("\n")
		return nil
	case err := <-done:
		return e.fail("metrics", err)
	}
}

// pyStrerror is strerror(no) with its first letter upper case, as
// Python words an OSError.
func pyStrerror(no syscall.Errno) string {
	msg := no.Error()
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	return msg
}

// metricsCmd is `daisugi metrics`.
func (e *Env) metricsCmd(args []string) error {
	const cmd = "metrics"
	opts := []opt{
		dataDirOpt,
		{names: []string{"--serve"}, help: "Serve /metrics over HTTP for a Prometheus scraper."},
		{names: []string{"--host"}, value: true, metavar: "TEXT", help: "Bind host for --serve."},
		{names: []string{"--port"}, value: true, metavar: "INTEGER", help: "Bind port for --serve."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Prometheus metrics: exposition text on stdout, or an HTTP /metrics endpoint.", opts)
	}
	port, err := clickInt(p, "--port", 9188)
	if err != nil {
		return e.usage(cmd, err)
	}
	dataDir := e.dataDir(p)
	if p.flag("--serve") {
		return e.serveMetrics(dataDir, p.str("--host", "127.0.0.1"), port)
	}
	text, err := e.prometheusText(cmd, dataDir)
	if err != nil {
		return err
	}
	e.out("%s", text)
	return nil
}
