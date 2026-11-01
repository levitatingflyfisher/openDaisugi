package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/router"
	"daisugi-verify/internal/wardenroot"
)

var routers = []string{"rules", "switchyard", "knn", "model", "off"}

var upstreamKinds = []string{"anthropic", "ollama", "openai-compatible", "anthropic-compatible"}

// kindToBackend is model_host._KIND_TO_BACKEND.
var kindToBackend = map[string]string{"ollama": "ollama", "openai": "openai-compatible", "anthropic": "anthropic-compatible"}

func routerOpts() []opt {
	return []opt{
		{names: []string{"--host"}, value: true, metavar: "TEXT", help: "Bind address."},
		{names: []string{"--port"}, value: true, metavar: "INTEGER", help: "Bind port."},
		{names: []string{"--upstream"}, value: true, metavar: "TEXT", help: "Where saved turns go."},
		{names: []string{"--cheap-model"}, value: true, metavar: "TEXT", help: "Model an easy turn is routed onto."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory (holds the turn journal)."},
		{names: []string{"--capture-answers"}, neg: "--no-capture-answers",
			help: "Persist each turn's raw response text to <data-dir>/router/answers.jsonl (opt-in)."},
		{names: []string{"--local-model"}, value: true, metavar: "TEXT", help: "A qualified local model id (ADR-0015)."},
		{names: []string{"--openai-upstream"}, value: true, metavar: "TEXT", help: "Upstream for OpenAI-wire requests."},
		{names: []string{"--openai-cheap-model"}, value: true, metavar: "TEXT",
			help: "Model an easy OpenAI-wire turn is routed onto; empty disables routing on that wire."},
		{names: []string{"--upstream-kind"}, value: true, metavar: "TEXT",
			help: "The wire --upstream speaks: anthropic, ollama, openai-compatible, anthropic-compatible."},
		{names: []string{"--routing-mode"}, value: true, metavar: "TEXT", help: "Who picks the model: rules, switchyard, knn, model or off."},
		{names: []string{"--switchyard-config"}, value: true, metavar: "PATH", help: "Your own Switchyard TOML file."},
		{names: []string{"--switchyard-port"}, value: true, metavar: "INTEGER", help: "The loopback port of the managed switchyard-server."},
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// loadCfg is load_config(path) as a command that has not caught its
// errors meets it: an invalid file ends the run with pydantic's error.
func (e *Env) loadCfg(cmd, path string) (config.Config, error) {
	cfg, err := config.Load(path)
	var yerr *config.YAMLError
	if errors.As(err, &yerr) {
		e.errf("daisugi %s: %s\n", cmd, yerr.Error())
		return cfg, exit(1)
	}
	if errors.Is(err, config.ErrInvalid) {
		e.errf("daisugi %s: pydantic_core._pydantic_core.ValidationError: %s does not validate\n", cmd, path)
		return cfg, exit(1)
	}
	if err != nil {
		return cfg, e.refuse(cmd, fmt.Errorf("%s is not one this binary reads: %w", path, err))
	}
	return cfg, nil
}

func (e *Env) routerServe(args []string) error {
	const cmd = "router serve"
	opts := routerOpts()
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Run the token-saving router: a local proxy any harness points at via base_url.", opts)
	}
	port, err := clickInt(p, "--port", 8787)
	if err != nil {
		return e.usage(cmd, err)
	}
	syPort, err := clickInt(p, "--switchyard-port", 4000)
	if err != nil {
		return e.usage(cmd, err)
	}
	// The upstream calls go through a proxy as httpx picks one. A proxy
	// setting this binary cannot read the same way is refused before
	// anything starts; one httpx cannot build a client from fails each
	// turn, as it does in the oracle.
	proxies := netproxy.HttpxFromVars(netproxy.FromEnviron(e.Environ), true)
	if r := proxies.Refusal(); r != nil && r.Unported != "" {
		return e.notYet("daisugi router serve with this proxy setting (" + r.Unported + ")")
	}
	host := p.str("--host", "127.0.0.1")
	upstream := p.str("--upstream", "https://api.anthropic.com")
	cheap := p.str("--cheap-model", "claude-haiku-4-5")
	dataDir := wardenroot.PathStr(p.str("--data-dir", e.dataHome()))
	capture := p.flag("--capture-answers")
	localFlag := p.str("--local-model", "")
	openaiUp := p.str("--openai-upstream", "https://api.openai.com")
	openaiCheap := p.str("--openai-cheap-model", "gpt-5-mini")
	cfgPath := filepath.Join(dataDir, "config.yaml")

	routingMode := p.str("--routing-mode", "")
	if !p.has("--routing-mode") {
		cfg, err := e.loadCfg(cmd, cfgPath)
		if err != nil {
			return err
		}
		routingMode = cfg.RoutingMode
	}
	if !contains(routers, routingMode) {
		e.errf("unknown --routing-mode %s.\nthe router knows five choosers.\nchoose one of: %s\n",
			pystr.Repr(routingMode), strings.Join(routers, ", "))
		return exit(1)
	}
	shownLocal := localFlag
	if shownLocal == "" {
		cfg, err := e.loadCfg(cmd, cfgPath)
		if err != nil {
			return err
		}
		if cfg.RouterLocalModel != nil {
			shownLocal = *cfg.RouterLocalModel
		}
	}
	kind := p.str("--upstream-kind", "")
	if p.has("--upstream-kind") && !contains(upstreamKinds, kind) {
		e.errf("unknown --upstream-kind %s.\nthe router only knows the wires it can shim count_tokens for.\n"+
			"choose one of: %s\n", pystr.Repr(kind), strings.Join(upstreamKinds, ", "))
		return exit(1)
	}
	if !p.has("--upstream-kind") {
		// The recorded kind counts only when --upstream names that host.
		cfg, err := e.loadCfg(cmd, filepath.Join(e.dataHome(), "config.yaml"))
		if err != nil {
			return err
		}
		kind = "anthropic"
		if cfg.LLMBaseURL != nil && *cfg.LLMBaseURL != "" && cfg.LLMHostKind != nil && *cfg.LLMHostKind != "" &&
			strings.TrimRight(upstream, "/") == strings.TrimRight(*cfg.LLMBaseURL, "/") {
			kind = kindToBackend[*cfg.LLMHostKind]
			if kind == "" {
				kind = "anthropic-compatible"
			}
		}
	}
	mode := map[string]string{"rules": router.ModeRules, "switchyard": router.ModeExternal, "knn": router.ModeKnn, "model": router.ModeModel, "off": router.ModeOff}[routingMode]
	var ext *router.External
	var child *switchyardChild
	if routingMode == "switchyard" {
		ext, upstream, child, err = e.startSwitchyardForRouter(dataDir, p.str("--switchyard-config", ""), p.has("--switchyard-config"), syPort)
		if err != nil {
			return err
		}
		kind = "anthropic"
	}
	e.out("opendaisugi router  →  %s\n", upstream)
	e.out("  routing mode: %s\n", routingMode)
	if shownLocal != "" && routingMode == "rules" {
		e.out("  local rung: easy turns go to %s\n", shownLocal)
	}
	if routingMode == "knn" {
		e.out("  knn outcomes: %s/router/routing.db\n", dataDir)
	}
	cfgNow, err := e.loadCfg(cmd, cfgPath)
	if err != nil {
		return err
	}
	if routingMode == "model" {
		rm := "not set"
		if cfgNow.RoutingModel != nil {
			rm = *cfgNow.RoutingModel
		}
		e.out("  routing model: %s\n", rm)
		e.out("  outcomes: %s/router/routing.db\n", dataDir)
	}
	if router.Off(cfgNow.RouterGrader) != "" {
		e.out("  grader: %s\n", *cfgNow.RouterGrader)
	}
	e.out("  listening on http://%s:%d  (journal: %s/router/turns.jsonl)\n", host, port, dataDir)
	e.out("  config reload: on change to %s/config.yaml, on SIGHUP, or POST http://%s:%d/_reload from this machine\n",
		dataDir, host, port)
	e.out("  point your harness at it:  ANTHROPIC_BASE_URL=http://%s:%d\n", host, port)
	if openaiCheap != "" {
		e.out("  OpenAI wire: …/chat/completions → %s (easy turns → %s)\n", openaiUp, openaiCheap)
	}
	if capture {
		e.out("  capturing answers to:      %s/router/answers.jsonl\n", dataDir)
	}
	build := func(localModel, cheapModel, mode string, ext *router.External, routingModel, graderModel string) (*router.Router, error) {
		g := router.Router{CheapModel: cheapModel, LocalModel: localModel, RoutingMode: mode, External: ext,
			JournalPath: filepath.Join(dataDir, "router", "turns.jsonl")}
		if mode == router.ModeKnn || mode == router.ModeModel {
			g.Knn = e.knnRouter(dataDir)
			g.RoutingModel, g.GraderModel = routingModel, graderModel
		}
		if capture {
			g.CaptureAnswers = true
			g.AnswersPath = filepath.Join(dataDir, "router", "answers.jsonl")
		}
		return router.NewRouter(g)
	}
	rebuild := func() (*router.Router, error) {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			if errors.Is(err, config.ErrUnsupported) {
				e.errf("daisugi router serve: %s is not one this binary reads; keeping the running router.\n", cfgPath)
			}
			return nil, err
		}
		local := localFlag
		if local == "" && cfg.RouterLocalModel != nil {
			local = *cfg.RouterLocalModel
		}
		return build(local, cheap, mode, ext, router.Off(cfg.RoutingModel), router.Off(cfg.RouterGrader))
	}
	reloader := &router.Reloader{Path: cfgPath, Rebuild: rebuild, MinInterval: 2 * time.Second}
	anthropic := reloader.Current()
	if anthropic == nil {
		if anthropic, err = build(localFlag, cheap, mode, ext, router.Off(cfgNow.RoutingModel), router.Off(cfgNow.RouterGrader)); err != nil {
			return e.fail(cmd, err)
		}
	}
	var openai *router.Router
	if openaiCheap != "" {
		if openai, err = build(localFlag, openaiCheap, router.ModeRules, nil, "", ""); err != nil {
			return e.fail(cmd, err)
		}
	}
	srv := &router.Server{Anthropic: anthropic, Reloader: reloader, OpenAI: openai, UpstreamBase: upstream,
		OpenAIBase: openaiUp, UpstreamKind: kind, Client: router.NewClientWith(proxies)}
	code := e.serveRouter(srv, reloader, host, int(port))
	if child != nil {
		// The oracle's exit cleanup. After SIGTERM it runs too, from the
		// oracle's SIGTERM handler, once the server has drained.
		e.out("  switchyard: %s\n", child.stop())
	}
	if code == -int(syscall.SIGTERM) {
		// The oracle's server re-raises SIGTERM once it has drained, and
		// its handler ends the process by that signal after the cleanup.
		signal.Reset(syscall.SIGTERM)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(time.Hour)
	}
	if code != 0 {
		return exit(code)
	}
	return nil
}

// serveRouter binds, serves until SIGINT or SIGTERM, drains, and
// returns the exit code: 0 after SIGINT, -SIGTERM after SIGTERM.
func (e *Env) serveRouter(srv *router.Server, reloader *router.Reloader, host string, port int) int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		e.errf("ERROR:    [Errno %d] error while attempting to bind on address %s: %s\n",
			errnoOf(err), bindAddr(host, port), bindReason(err))
		return 3
	}
	hs := &http.Server{Handler: srv}
	done := make(chan struct{})
	go func() {
		hs.Serve(ln)
		close(done)
	}()
	for s := range sigs {
		switch s {
		case syscall.SIGHUP:
			reloader.Reload()
		case syscall.SIGINT, syscall.SIGTERM:
			hs.Shutdown(context.Background())
			<-done
			if s == syscall.SIGTERM {
				return -int(syscall.SIGTERM)
			}
			return 0
		}
	}
	return 0
}

func errnoOf(err error) int {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return int(errno)
	}
	return 0
}

func bindAddr(host string, port int) string {
	return fmt.Sprintf("(%s, %d)", pystr.Repr(host), port)
}

func bindReason(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return strings.ToLower(errno.Error())
	}
	return err.Error()
}

// knnRouter is build_default_router's KnnRouter: the outcome table under
// the data dir, the active matcher as the embedder, and its threshold. A
// matcher this binary cannot run gives a threshold of 1.0 and an embedder
// that fails, so every turn falls back to the rules router.
func (e *Env) knnRouter(dataDir string) *router.Knn {
	m, notBuilt, merr := e.matcher()
	usable := merr == nil && notBuilt == ""
	threshold := 1.0
	if usable {
		threshold = m.Threshold
	}
	return &router.Knn{
		Store:     &router.OutcomeStore{Path: filepath.Join(dataDir, "router", "routing.db")},
		Threshold: threshold,
		Embed: func(text string) ([]float32, string, error) {
			if !usable {
				return nil, "", errors.New("no usable matcher")
			}
			emb, err := m.Embedder()
			if err != nil {
				return nil, "", err
			}
			return router.ToF32(emb.Encode(text)), m.Identity, nil
		},
	}
}
