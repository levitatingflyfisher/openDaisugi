package server

import (
	"bufio"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// outsideTTL is how long the floor trusts what it read about the world
// outside coppice: the gateway address and router from the config files,
// and whether the gateway answers.
const outsideTTL = 5 * time.Second

// gatewayDialTimeout bounds the check that the gateway answers.
const gatewayDialTimeout = 300 * time.Millisecond

// outsideCache holds, under Server.outsideMu, what the floor last read
// about the gateway.
type outsideCache struct {
	readAt  time.Time
	gateway string
	router  string

	dialAt  time.Time
	dialFor string
	answers bool
}

// gatewayFacts returns the gateway base URL and what routes behind it,
// "gateway" or "switchyard". The URL comes from coppice.toml's gateway
// key, or daisugi's default address when the key is absent. It is empty
// when there is no gateway or the file cannot be read.
//
// The router comes from gateway_router in daisugi's config.yaml beside
// the gate root. The gateway reads that value only when it starts, and a
// --router flag overrides it, so this is what the gateway was set to
// run, not proof of what it runs now.
func (s *Server) gatewayFacts() (gateway, router string) {
	s.outsideMu.Lock()
	defer s.outsideMu.Unlock()
	now := time.Now()
	if !s.outside.readAt.IsZero() && now.Sub(s.outside.readAt) < outsideTTL {
		return s.outside.gateway, s.outside.router
	}
	gateway = ""
	if cfg, _, err := config.Load(); err == nil {
		gateway = cfg.GatewayURL()
	}
	router = "gateway"
	if daisugiRouter(filepath.Join(filepath.Dir(s.cfg.GateRoot), "config.yaml")) == "switchyard" {
		router = "switchyard"
	}
	s.outside.readAt, s.outside.gateway, s.outside.router = now, gateway, router
	return gateway, router
}

// daisugiRouter reads the top-level gateway_router key from daisugi's
// config.yaml. It reads only that one line shape, so it needs no YAML
// parser. A missing file or key is "".
func daisugiRouter(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		rest, ok := strings.CutPrefix(line, "gateway_router:")
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

// routerOf says who routes a pane's model calls: "gateway" or
// "switchyard" when its ANTHROPIC_BASE_URL or OPENAI_BASE_URL is the
// gateway, "direct" when either names anything else. It is "" when
// neither is set. With no gateway to compare with, it is "direct" only
// for a vendor's own API address, and "" for any other.
func (s *Server) routerOf(p layout.Pane) string {
	var bases []string
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"} {
		if v := strings.TrimSpace(s.paneEnv(p, k)); v != "" {
			bases = append(bases, v)
		}
	}
	if len(bases) == 0 {
		return ""
	}
	gateway, router := s.gatewayFacts()
	if gateway == "" {
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
		if sameEndpoint(b, gateway) {
			return router
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
// compared: the gateway serves the OpenAI wire under /v1 of the same
// address.
func sameEndpoint(a, b string) bool {
	ea, oka := endpoint(a)
	eb, okb := endpoint(b)
	return oka && okb && ea == eb
}

// gatewayAnswers reports whether something accepts a connection at the
// gateway's address. It dials only a loopback address, never a remote
// one, and remembers the answer for outsideTTL. known is false when there
// is no gateway or its address is not loopback.
//
// It opens a TCP connection and closes it without a request: every path
// the gateway serves but one is sent on to the model provider, so an HTTP
// request here could leave the machine.
func (s *Server) gatewayAnswers(gateway string) (answers, known bool) {
	u, err := url.Parse(gateway)
	if err != nil || u.Host == "" || !isLoopbackName(strings.ToLower(u.Hostname())) {
		return false, false
	}
	e, ok := endpoint(gateway)
	if !ok {
		return false, false
	}
	port := e[strings.LastIndexByte(e, ':')+1:]
	s.outsideMu.Lock()
	if s.outside.dialFor == gateway && time.Since(s.outside.dialAt) < outsideTTL {
		a := s.outside.answers
		s.outsideMu.Unlock()
		return a, true
	}
	s.outsideMu.Unlock()
	host := u.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), gatewayDialTimeout)
	answers = err == nil
	if conn != nil {
		_ = conn.Close()
	}
	s.outsideMu.Lock()
	s.outside.dialAt, s.outside.dialFor, s.outside.answers = time.Now(), gateway, answers
	s.outsideMu.Unlock()
	return answers, true
}

// disarmed reports whether daisugi's disarm marker is in the gate root.
// While it is there, an installed gate allows every call.
func (s *Server) disarmed() bool {
	_, err := os.Lstat(filepath.Join(s.cfg.GateRoot, "DISARMED"))
	return err == nil
}

// modeOff is the daisugi mode of a harness pane that no gate hook has
// reported for.
const modeOff = "off"

// stackOf is a pane.list row's stack object: loop, model, router and
// daisugi, each only when known. It is nil when nothing is known.
func (s *Server) stackOf(p layout.Pane, v factsView) map[string]any {
	st := map[string]any{}
	if p.Harness != "" {
		st["loop"] = p.Harness
	}
	if v.model != "" {
		st["model"] = v.model
	}
	if r := s.routerOf(p); r != "" {
		st["router"] = r
	}
	if p.Harness != "" && !p.Closed {
		mode := v.mode
		if mode == "" {
			mode = modeOff
		}
		st["daisugi"] = map[string]any{"mode": mode, "armed": !s.disarmed()}
	}
	if len(st) == 0 {
		return nil
	}
	return st
}

// addFacts puts stack, gate and tokens on one pane.list row, each only
// when known.
func (s *Server) addFacts(row map[string]any, p layout.Pane) {
	v := s.viewFacts(p)
	if st := s.stackOf(p, v); st != nil {
		row["stack"] = st
	}
	if v.verdict != nil {
		row["gate"] = v.verdict
	}
	if v.tokens != nil {
		row["tokens"] = *v.tokens
	}
}

// hookFiles are the harness settings a daisugi gate hook is written to,
// under the home: Claude Code's and Codex's.
var hookFiles = []string{".claude/settings.json", ".codex/hooks.json"}

// gateInstalled reports whether any harness settings file under the home
// names the daisugi gate. Every gate hook command daisugi writes, Python,
// Go or Rust, holds "opendaisugi.gate". It says nothing of any one pane:
// that is the pane's own mode.
func (s *Server) gateInstalled() bool {
	if s.cfg.HookHome == "" {
		return false
	}
	for _, f := range hookFiles {
		raw, err := os.ReadFile(filepath.Join(s.cfg.HookHome, f))
		if err == nil && strings.Contains(string(raw), "opendaisugi.gate") {
			return true
		}
	}
	return false
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
				// A foreman hears this ask first, so it is working, the
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
	res := map[string]any{
		"daisugi": map[string]any{
			"mode": mode, "armed": !s.disarmed(),
			"enforcing": counts["enforcing"], "watching": counts["watching"], "off": counts[modeOff],
			"installed": s.gateInstalled(),
		},
		"working": working, "needing_you": needing, "tokens_today": today,
	}
	// The floor's foreman, when the one the server tracks runs now.
	s.talkMu.Lock()
	fid := s.trackedForeman()
	s.talkMu.Unlock()
	if live, _ := s.foremanState(fid); live {
		res["foreman"] = map[string]any{"pane": fid, "label": s.labelOf(fid)}
	}
	if gateway, _ := s.gatewayFacts(); gateway != "" {
		gw := map[string]any{"url": gateway}
		if answers, known := s.gatewayAnswers(gateway); known {
			gw["answers"] = answers
		}
		res["gateway"] = gw
	}
	return proto.OKResp(r.ID, res)
}
