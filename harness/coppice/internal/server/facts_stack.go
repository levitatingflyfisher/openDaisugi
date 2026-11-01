package server

import (
	"bufio"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/web"
)

// outsideTTL is how long the floor trusts what it read about the world
// outside coppice: the router address and route from the config files,
// and whether the router answers.
const outsideTTL = 5 * time.Second

// routerDialTimeout bounds the check that the router answers.
const routerDialTimeout = 300 * time.Millisecond

// outsideCache holds, under Server.outsideMu, what the floor last read
// about the router.
type outsideCache struct {
	readAt time.Time
	router string
	route  string

	dialAt  time.Time
	dialFor string
	answers bool
}

// routerFacts returns the router base URL and what routes behind it,
// "router" or "switchyard". The URL comes from coppice.toml's router
// key, or daisugi's default address when the key is absent. It is empty
// when there is no router or the file cannot be read.
//
// The route comes from routing_mode in daisugi's config.yaml beside
// the warden root. The router reads that value only when it starts, and a
// --routing-mode flag overrides it, so this is what the router was set to
// run, not proof of what it runs now.
func (s *Server) routerFacts() (router, route string) {
	s.outsideMu.Lock()
	defer s.outsideMu.Unlock()
	now := time.Now()
	if !s.outside.readAt.IsZero() && now.Sub(s.outside.readAt) < outsideTTL {
		return s.outside.router, s.outside.route
	}
	router = ""
	if cfg, _, err := config.Load(); err == nil {
		router = cfg.RouterURL()
	}
	route = "router"
	if daisugiRoute(filepath.Join(filepath.Dir(s.cfg.WardenRoot), "config.yaml")) == "switchyard" {
		route = "switchyard"
	}
	s.outside.readAt, s.outside.router, s.outside.route = now, router, route
	return router, route
}

// daisugiRoute reads the top-level routing_mode key from daisugi's
// config.yaml. It reads only that one line shape, so it needs no YAML
// parser. A missing file or key is "".
func daisugiRoute(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		rest, ok := strings.CutPrefix(line, "routing_mode:")
		if !ok {
			continue
		}
		if i := strings.IndexByte(rest, '#'); i >= 0 {
			rest = rest[:i]
		}
		return strings.Trim(strings.TrimSpace(rest), `"'`)
	}
	return ""
}

// paneEnv reads one variable as the pane's process sees it: the pane's
// own env over the server's, which every pane inherits. An empty value
// counts as unset.
func (s *Server) paneEnv(p layout.Pane, key string) string {
	if v, ok := p.Env[key]; ok {
		return v
	}
	return s.getenv(key)
}

// vendorAPIs are the providers' own API addresses. A base URL at one of
// them is direct whatever else is known.
var vendorAPIs = []string{"https://api.anthropic.com", "https://api.openai.com"}

// routeOf says who routes a pane's model calls: "router" or
// "switchyard" when its ANTHROPIC_BASE_URL or OPENAI_BASE_URL is the
// router, "direct" when either names anything else. It is "" when
// neither is set. With no router to compare with, it is "direct" only
// for a vendor's own API address, and "" for any other.
func (s *Server) routeOf(p layout.Pane) string {
	var bases []string
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"} {
		if v := strings.TrimSpace(s.paneEnv(p, k)); v != "" {
			bases = append(bases, v)
		}
	}
	if len(bases) == 0 {
		return ""
	}
	router, route := s.routerFacts()
	if router == "" {
		for _, b := range bases {
			for _, v := range vendorAPIs {
				if sameEndpoint(b, v) {
					return "direct"
				}
			}
		}
		return ""
	}
	for _, b := range bases {
		if sameEndpoint(b, router) {
			return route
		}
	}
	return "direct"
}

// endpoint is a URL's scheme, host and port. Every loopback name is the
// same host, and a missing port is the scheme's own.
func endpoint(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", false
		}
	}
	host := strings.ToLower(u.Hostname())
	if isLoopbackName(host) {
		host = "loopback"
	}
	return scheme + "://" + host + ":" + port, true
}

func isLoopbackName(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameEndpoint compares two base URLs by endpoint only. A path is not
// compared: the router serves the OpenAI wire under /v1 of the same
// address.
func sameEndpoint(a, b string) bool {
	ea, oka := endpoint(a)
	eb, okb := endpoint(b)
	return oka && okb && ea == eb
}

// routerAnswers reports whether something accepts a connection at the
// router's address. It dials only a loopback address, never a remote
// one, and remembers the answer for outsideTTL. known is false when there
// is no router or its address is not loopback.
//
// It opens a TCP connection and closes it without a request: every path
// the router serves but one is sent on to the model provider, so an HTTP
// request here could leave the machine.
func (s *Server) routerAnswers(router string) (answers, known bool) {
	u, err := url.Parse(router)
	if err != nil || u.Host == "" || !isLoopbackName(strings.ToLower(u.Hostname())) {
		return false, false
	}
	e, ok := endpoint(router)
	if !ok {
		return false, false
	}
	port := e[strings.LastIndexByte(e, ':')+1:]
	s.outsideMu.Lock()
	if s.outside.dialFor == router && time.Since(s.outside.dialAt) < outsideTTL {
		a := s.outside.answers
		s.outsideMu.Unlock()
		return a, true
	}
	s.outsideMu.Unlock()
	host := u.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), routerDialTimeout)
	answers = err == nil
	if conn != nil {
		_ = conn.Close()
	}
	s.outsideMu.Lock()
	s.outside.dialAt, s.outside.dialFor, s.outside.answers = time.Now(), router, answers
	s.outsideMu.Unlock()
	return answers, true
}

// disarmed reports whether daisugi's disarm marker is in the warden root.
// While it is there, an installed warden allows every call.
func (s *Server) disarmed() bool {
	_, err := os.Lstat(filepath.Join(s.cfg.WardenRoot, "DISARMED"))
	return err == nil
}

// modeOff is the daisugi mode of a harness pane that no warden hook has
// reported for.
const modeOff = "off"

// stackOf is a pane.list row's stack object: loop, model, route and
// daisugi, each only when known. It is nil when nothing is known.
func (s *Server) stackOf(p layout.Pane, v factsView) map[string]any {
	st := map[string]any{}
	if p.Harness != "" {
		st["loop"] = p.Harness
	}
	if v.model != "" {
		st["model"] = v.model
	}
	if r := s.routeOf(p); r != "" {
		st["route"] = r
	}
	if p.Harness != "" && !p.Closed {
		mode := v.mode
		if mode == "" {
			mode = modeOff
		}
		d := map[string]any{"mode": mode, "armed": !s.disarmed()}
		// The owner set this agent to watching; the warden reads it on the
		// agent's next tool call, so mode may still say enforcing.
		if m, note := s.modeMarker(p.ID); m != "" {
			d["marker"] = m
			if note != "" {
				d["markerNote"] = note
			}
		}
		st["daisugi"] = d
	}
	if len(st) == 0 {
		return nil
	}
	return st
}

// addFacts puts stack, warden and tokens on one pane.list row, each only
// when known.
func (s *Server) addFacts(row map[string]any, p layout.Pane) {
	v := s.viewFacts(p)
	if st := s.stackOf(p, v); st != nil {
		row["stack"] = st
	}
	if v.verdict != nil {
		row["warden"] = v.verdict
	}
	if v.tokens != nil {
		row["tokens"] = *v.tokens
	}
}

// hookFiles are the harness settings a daisugi warden hook is written to,
// under the home: Claude Code's and Codex's.
var hookFiles = []string{".claude/settings.json", ".codex/hooks.json"}

// wardenInstalled reports whether any harness settings file under the home
// names the daisugi warden. Every warden hook command daisugi writes, Python,
// Go or Rust, holds "opendaisugi.warden". It says nothing of any one pane:
// that is the pane's own mode.
func (s *Server) wardenInstalled() bool {
	if s.cfg.HookHome == "" {
		return false
	}
	for _, f := range hookFiles {
		raw, err := os.ReadFile(filepath.Join(s.cfg.HookHome, f))
		if err == nil && strings.Contains(string(raw), "opendaisugi.warden") {
			return true
		}
	}
	return false
}

// hookRootArg finds the --root a daisugi warden hook command names, in
// the raw text of a harness settings file.
var hookRootArg = regexp.MustCompile(`opendaisugi\.warden[^"\n]*?--root(?:=|\s+)([^\s"\\]+)`)

// hookRoot is the warden root an installed warden hook names with --root when
// it is not this server's warden root, or "". Then the warden reads markers
// from another place, so a mode set here does nothing, and the mode row
// says so (F11-R-15). A root that starts with ~/ is read from the home. A
// hook with no --root is taken to use the default root, which is this
// server's when both run with their defaults; that case is not checked.
func (s *Server) hookRoot() string {
	if s.cfg.HookHome == "" {
		return ""
	}
	for _, f := range hookFiles {
		raw, err := os.ReadFile(filepath.Join(s.cfg.HookHome, f))
		if err != nil {
			continue
		}
		for _, m := range hookRootArg.FindAllStringSubmatch(string(raw), -1) {
			root := m[1]
			if strings.HasPrefix(root, "~/") {
				root = filepath.Join(s.cfg.HookHome, root[2:])
			}
			if filepath.Clean(root) != filepath.Clean(s.cfg.WardenRoot) {
				return m[1]
			}
		}
	}
	return ""
}

// phoneCertWarnDays is how close to its end the phone's tailscale
// certificate must be before floor.facts names it. The phone server
// renews it under 30 days, so a certificate this close means renewal
// failed.
const phoneCertWarnDays = 7

// phoneCertDays is the whole days left on the phone server's tailscale
// certificate, and false when there is none to warn about. It reads only
// a phone server that runs now (the pair it names in web/serving.json,
// with a live pid) or a saved one (web.json enabled under --tls
// tailscale: its cert_file, or the default pair when blank). A fact that
// cannot be read is absent, never guessed.
func (s *Server) phoneCertDays(now time.Time) (int, bool) {
	certFile, ok := web.ServingCert(web.ServingPath(s.cfg.DataDir))
	if !ok {
		cfg, err := web.LoadConfig(web.ConfigPath(s.cfg.DataDir))
		if err != nil || !cfg.Enabled || cfg.TLS != string(web.TLSTailscale) {
			return 0, false
		}
		certFile = cfg.CertFile
		if certFile == "" {
			certFile, _ = web.TailscalePaths(s.cfg.DataDir)
		}
	}
	leaf, err := web.LoadLeaf(certFile)
	if err != nil {
		return 0, false
	}
	return web.CertDays(leaf.NotAfter, now), true
}

// modeRank orders the daisugi modes by how much they guard.
var modeRank = map[string]int{modeOff: 0, "watching": 1, "enforcing": 2}

// handleFloorFacts answers floor.facts: the facts a floor shows in its
// header row. The daisugi mode is the least guarded mode among the live
// harness panes, so the header never claims more than every agent has,
// and off when there is none. tokens_today sums the tokens of every pane
// the server saw a report or a token change from today, local time,
// ended panes included, over each such pane's whole total.
func (s *Server) handleFloorFacts(_ *Client, r *proto.Request) proto.Response {
	s.pruneFacts()
	s.refreshProcess()
	now := s.clock()
	ty, tm, td := now.Date()
	counts := map[string]int{modeOff: 0, "watching": 0, "enforcing": 0}
	harnesses, working, needing := 0, 0, 0
	var today Tokens
	for _, p := range s.tree.Panes() {
		v := s.viewFacts(p)
		if v.tokens != nil && !v.seen.IsZero() {
			if y, m, d := v.seen.In(now.Location()).Date(); y == ty && m == tm && d == td {
				today.add(*v.tokens)
			}
		}
		if p.Closed {
			continue
		}
		if eff, ok := s.effectiveState(p.ID); ok {
			switch {
			case eff.State == proto.StateWorking:
				working++
			case eff.State == proto.StateBlocked && s.heldFor(p.ID) != nil:
				// A steward hears this ask first, so it is working, the
				// same as every floor counts it from pane.list.
				working++
			case eff.State == proto.StateBlocked:
				needing++
			}
		}
		if p.Harness != "" {
			mode := v.mode
			if _, ok := modeRank[mode]; !ok {
				mode = modeOff
			}
			counts[mode]++
			harnesses++
		}
	}
	mode := modeOff
	if harnesses > 0 {
		mode = "enforcing"
		for m, n := range counts {
			if n > 0 && modeRank[m] < modeRank[mode] {
				mode = m
			}
		}
	}
	daisugi := map[string]any{
		"mode": mode, "armed": !s.disarmed(),
		"enforcing": counts["enforcing"], "watching": counts["watching"], "off": counts[modeOff],
		"installed": s.wardenInstalled(),
	}
	if root := s.hookRoot(); root != "" {
		daisugi["hook_root"] = root
	}
	res := map[string]any{
		"daisugi": daisugi,
		"working": working, "needing_you": needing, "tokens_today": today,
	}
	// The saving is the router's own figure and nothing else: with none,
	// the reply has no such key.
	if tokens, ok := s.savedToday(now); ok {
		res["saved_today"] = map[string]any{"tokens": tokens}
	}
	// The floor's steward, when the one the server tracks runs now.
	s.talkMu.Lock()
	fid := s.trackedSteward()
	s.talkMu.Unlock()
	if live, _ := s.stewardState(fid); live {
		res["steward"] = map[string]any{"pane": fid, "label": s.labelOf(fid)}
	}
	// The chat steward's last turn: the profile it ran on, once one has run.
	if name, ok := s.lastTurnProfile(); ok {
		res["steward_turn"] = map[string]any{"ran_on": name}
	}
	if days, ok := s.phoneCertDays(now); ok && days < phoneCertWarnDays {
		res["phone_cert_days"] = days
	}
	if router, _ := s.routerFacts(); router != "" {
		gw := map[string]any{"url": router}
		if answers, known := s.routerAnswers(router); known {
			gw["answers"] = answers
		}
		res["router"] = gw
	}
	return proto.OKResp(r.ID, res)
}
