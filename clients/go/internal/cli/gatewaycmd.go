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
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pystr"
)

var routers = []string{"rules", "switchyard", "off"}

var upstreamKinds = []string{"anthropic", "ollama", "openai-compatible", "anthropic-compatible"}

// kindToBackend is model_host._KIND_TO_BACKEND.
var kindToBackend = map[string]string{"ollama": "ollama", "openai": "openai-compatible", "anthropic": "anthropic-compatible"}

func gatewayOpts() []opt {
	return []opt{
		{names: []string{"--host"}, value: true, metavar: "TEXT", help: "Bind address."},
		{names: []string{"--port"}, value: true, metavar: "INTEGER", help: "Bind port."},
		{names: []string{"--upstream"}, value: true, metavar: "TEXT", help: "Where saved turns go."},
		{names: []string{"--cheap-model"}, value: true, metavar: "TEXT", help: "Model an easy turn is routed onto."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory (holds the turn journal)."},
		{names: []string{"--capture-answers"}, neg: "--no-capture-answers",
			help: "Persist each turn's raw response text to <data-dir>/gateway/answers.jsonl (opt-in)."},
		{names: []string{"--local-model"}, value: true, metavar: "TEXT", help: "A qualified local model id (ADR-0015)."},
		{names: []string{"--openai-upstream"}, value: true, metavar: "TEXT", help: "Upstream for OpenAI-wire requests."},
		{names: []string{"--openai-cheap-model"}, value: true, metavar: "TEXT",
			help: "Model an easy OpenAI-wire turn is routed onto; empty disables routing on that wire."},
		{names: []string{"--upstream-kind"}, value: true, metavar: "TEXT",
			help: "The wire --upstream speaks: anthropic, ollama, openai-compatible, anthropic-compatible."},
		{names: []string{"--router"}, value: true, metavar: "TEXT", help: "Who picks the model: rules, switchyard or off."},
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

func (e *Env) gatewayCmd(args []string) error {
	const cmd = "gateway"
	opts := gatewayOpts()
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Run the token-saving gateway: a local proxy any harness points at via base_url.", opts)
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
		return e.notYet("daisugi gateway with this proxy setting (" + r.Unported + ")")
	}
	host := p.str("--host", "127.0.0.1")
	upstream := p.str("--upstream", "https://api.anthropic.com")
	cheap := p.str("--cheap-model", "claude-haiku-4-5")
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	capture := p.flag("--capture-answers")
	localFlag := p.str("--local-model", "")
	openaiUp := p.str("--openai-upstream", "https://api.openai.com")
	openaiCheap := p.str("--openai-cheap-model", "gpt-5-mini")
	cfgPath := filepath.Join(dataDir, "config.yaml")

	router := p.str("--router", "")
	if !p.has("--router") {
		cfg, err := e.loadCfg(cmd, cfgPath)
		if err != nil {
			return err
		}
		router = cfg.GatewayRouter
	}
	if !contains(routers, router) {
		e.errf("unknown --router %s.\nthe gateway knows three choosers.\nchoose one of: %s\n",
			pystr.Repr(router), strings.Join(routers, ", "))
		return exit(1)
	}
	shownLocal := localFlag
	if shownLocal == "" {
		cfg, err := e.loadCfg(cmd, cfgPath)
		if err != nil {
			return err
		}
		if cfg.GatewayLocalModel != nil {
			shownLocal = *cfg.GatewayLocalModel
		}
	}
	kind := p.str("--upstream-kind", "")
	if p.has("--upstream-kind") && !contains(upstreamKinds, kind) {
		e.errf("unknown --upstream-kind %s.\nthe gateway only knows the wires it can shim count_tokens for.\n"+
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
	mode := map[string]string{"rules": gateway.ModeRules, "switchyard": gateway.ModeExternal, "off": gateway.ModeOff}[router]
	var ext *gateway.External
	var child *switchyardChild
	if router == "switchyard" {
		ext, upstream, child, err = e.startSwitchyardForGateway(dataDir, p.str("--switchyard-config", ""), p.has("--switchyard-config"), syPort)
		if err != nil {
			return err
		}
		kind = "anthropic"
	}
	e.out("opendaisugi gateway  →  %s\n", upstream)
	e.out("  router: %s\n", router)
	if shownLocal != "" && router == "rules" {
		e.out("  local rung: easy turns go to %s\n", shownLocal)
	}
	e.out("  listening on http://%s:%d  (journal: %s/gateway/turns.jsonl)\n", host, port, dataDir)
	e.out("  config reload: on change to %s/config.yaml, on SIGHUP, or POST http://%s:%d/_reload from this machine\n",
		dataDir, host, port)
	e.out("  point your harness at it:  ANTHROPIC_BASE_URL=http://%s:%d\n", host, port)
	if openaiCheap != "" {
		e.out("  OpenAI wire: …/chat/completions → %s (easy turns → %s)\n", openaiUp, openaiCheap)
	}
	if capture {
		e.out("  capturing answers to:      %s/gateway/answers.jsonl\n", dataDir)
	}
	build := func(localModel, cheapModel, mode string, ext *gateway.External) (*gateway.Gateway, error) {
		g := gateway.Gateway{CheapModel: cheapModel, LocalModel: localModel, RouterMode: mode, External: ext,
			JournalPath: filepath.Join(dataDir, "gateway", "turns.jsonl")}
		if capture {
			g.CaptureAnswers = true
			g.AnswersPath = filepath.Join(dataDir, "gateway", "answers.jsonl")
		}
		return gateway.NewGateway(g)
	}
	rebuild := func() (*gateway.Gateway, error) {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			if errors.Is(err, config.ErrUnsupported) {
				e.errf("daisugi gateway: %s is not one this binary reads; keeping the running router.\n", cfgPath)
			}
			return nil, err
		}
		local := localFlag
		if local == "" && cfg.GatewayLocalModel != nil {
			local = *cfg.GatewayLocalModel
		}
		return build(local, cheap, mode, ext)
	}
	reloader := &gateway.Reloader{Path: cfgPath, Rebuild: rebuild, MinInterval: 2 * time.Second}
	anthropic := reloader.Current()
	if anthropic == nil {
		if anthropic, err = build(localFlag, cheap, mode, ext); err != nil {
			return e.fail(cmd, err)
		}
	}
	var openai *gateway.Gateway
	if openaiCheap != "" {
		if openai, err = build(localFlag, openaiCheap, gateway.ModeRules, nil); err != nil {
			return e.fail(cmd, err)
		}
	}
	srv := &gateway.Server{Anthropic: anthropic, Reloader: reloader, OpenAI: openai, UpstreamBase: upstream,
		OpenAIBase: openaiUp, UpstreamKind: kind, Client: gateway.NewClientWith(proxies)}
	code := e.serveGateway(srv, reloader, host, int(port))
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

// serveGateway binds, serves until SIGINT or SIGTERM, drains, and
// returns the exit code: 0 after SIGINT, -SIGTERM after SIGTERM.
func (e *Env) serveGateway(srv *gateway.Server, reloader *gateway.Reloader, host string, port int) int {
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
