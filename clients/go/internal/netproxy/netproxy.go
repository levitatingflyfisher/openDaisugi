// Package netproxy reads proxies from the environment as the oracle's HTTP
// clients read them, and opens connections through them.
//
// Two rule sets, since the oracle has two kinds of client:
//
//   - Httpx: httpx 0.28 with trust_env, as the gateway's upstream client
//     and the model client (llm_client.py, the gate's llm_check) use it.
//     HTTP_PROXY, HTTPS_PROXY and ALL_PROXY, and NO_PROXY as httpx turns
//     each entry into a URL pattern. There is no bypass for loopback: a
//     request to 127.0.0.1 goes through the proxy unless NO_PROXY names it.
//     (net/http's ProxyFromEnvironment differs: it skips loopback, reads
//     upper case first, and has no ALL_PROXY.)
//   - Urllib: urllib.request.urlopen, as the potion download and the
//     Switchyard health probe use it. No ALL_PROXY; NO_PROXY as a list of
//     DNS suffixes; an https request always goes through an HTTP CONNECT
//     tunnel, whatever the proxy URL's scheme.
//
// Both read the variables as urllib.request.getproxies does: any
// <scheme>_proxy in any case, a name that ends in lower-case _proxy
// winning, and HTTP_PROXY dropped when REQUEST_METHOD is set.
package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SocksText is httpx's error when a proxy URL names SOCKS and the socksio
// package is not installed (it is not, in the oracle's environment).
const SocksText = "Using SOCKS proxy, but the 'socksio' package is not installed. " +
	"Make sure to install httpx using `pip install httpx[socks]`."

// Var is one environment variable.
type Var struct{ Name, Value string }

// FromEnviron splits "NAME=value" entries, in their order.
func FromEnviron(environ []string) []Var {
	out := make([]Var, 0, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out = append(out, Var{k, v})
		}
	}
	return out
}

// OrderedFromMap lists a map's variables in this process's environment
// order, and says whether that is the order Python's os.environ would
// give: true when every *_proxy variable of the map is in the process
// environment with the same value (the map was read from it). A variable
// the process does not hold comes after, in name order.
func OrderedFromMap(env map[string]string) ([]Var, bool) {
	var out []Var
	taken := map[string]bool{}
	for _, v := range FromEnviron(os.Environ()) {
		if val, ok := env[v.Name]; ok && val == v.Value && !taken[v.Name] {
			out = append(out, v)
			taken[v.Name] = true
		}
	}
	var rest []Var
	for k, v := range env {
		if !taken[k] {
			rest = append(rest, Var{k, v})
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	ordered := true
	for _, v := range rest {
		if len(v.Name) > 5 && strings.HasSuffix(strings.ToLower(v.Name), "_proxy") {
			ordered = false
		}
	}
	return append(out, rest...), ordered
}

// FromMap lists a map's variables, in no order.
func FromMap(env map[string]string) []Var {
	out := make([]Var, 0, len(env))
	for k, v := range env {
		out = append(out, Var{k, v})
	}
	return out
}

type kv struct{ k, v string }

func get(p []kv, k string) (string, bool) {
	for _, e := range p {
		if e.k == k {
			return e.v, true
		}
	}
	return "", false
}

func set(p []kv, k, v string) []kv {
	for i := range p {
		if p[i].k == k {
			p[i].v = v
			return p
		}
	}
	return append(p, kv{k, v})
}

func del(p []kv, k string) []kv {
	out := p[:0]
	for _, e := range p {
		if e.k != k {
			out = append(out, e)
		}
	}
	return out
}

// getproxies is urllib.request.getproxies_environment. When vars are not
// in the environment's order, two names that differ only in case and hold
// different values make the answer depend on that order: an error.
func getproxies(vars []Var, ordered bool) ([]kv, error) {
	var out []kv
	type ent struct{ name, value, pname string }
	var env []ent
	var seen1, seen2 []kv
	for _, e := range vars {
		r := []rune(e.Name)
		if len(r) > 5 && r[len(r)-6] == '_' && strings.ToLower(string(r[len(r)-5:])) == "proxy" {
			pname := strings.ToLower(string(r[:len(r)-6]))
			env = append(env, ent{e.Name, e.Value, pname})
			if e.Value != "" {
				if !ordered && !strings.HasSuffix(e.Name, "_proxy") {
					for _, s := range seen1 {
						if s.k == pname && s.v != e.Value {
							return nil, fmt.Errorf("%s and another %s_proxy with a different value", e.Name, pname)
						}
					}
					seen1 = append(seen1, kv{pname, e.Value})
				}
				out = set(out, pname, e.Value)
			}
		}
	}
	for _, e := range vars {
		if e.Name == "REQUEST_METHOD" {
			out = del(out, "http")
			break
		}
	}
	for _, e := range env {
		if !strings.HasSuffix(e.name, "_proxy") {
			continue
		}
		if !ordered {
			for _, s := range seen2 {
				if s.k == e.pname && s.v != e.value {
					return nil, fmt.Errorf("%s and another %s_proxy with a different value", e.name, e.pname)
				}
			}
			seen2 = append(seen2, kv{e.pname, e.value})
		}
		if e.value != "" {
			out = set(out, e.pname, e.value)
		} else {
			out = del(out, e.pname)
		}
	}
	return out, nil
}

// pyStrip is Python's str.strip() with no argument.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// Proxy is the first hop when it is a proxy.
type Proxy struct {
	// TLS to the proxy itself (a proxy URL of the https scheme).
	TLS  bool
	Host string
	Port int
	// User and Pass are the unquoted credentials; Auth says whether a
	// Proxy-Authorization header is sent.
	User, Pass string
	Auth       bool
}

// AuthValue is the Proxy-Authorization value, or "".
func (p Proxy) AuthValue() string {
	if !p.Auth {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(p.User+":"+p.Pass))
}

// Tunnel is how a CONNECT is written.
type Tunnel int

const (
	// TunnelHttpx is httpcore's CONNECT: Host then Accept: */* then the
	// credentials; any 2xx opens the tunnel; an IPv6 target without
	// brackets.
	TunnelHttpx Tunnel = iota
	// TunnelUrllib is http.client's CONNECT: the credentials then Host;
	// only 200 opens the tunnel; an IPv6 target in brackets.
	TunnelUrllib
)

// Kind is what a request goes through.
type Kind int

const (
	Direct Kind = iota
	// Forward sends an HTTP request in absolute form to the proxy.
	Forward
	// Through opens a CONNECT tunnel through the proxy, then TLS.
	Through
)

// Route is the way to an origin.
type Route struct {
	Kind   Kind
	Proxy  Proxy
	Tunnel Tunnel
}

// Refusal says why a request cannot be sent.
type Refusal struct {
	// Fails is httpx's error text when it cannot build its client (a SOCKS
	// or unknown proxy scheme): every request fails with it.
	Fails string
	// Unported names a shape of variable this binary does not model.
	Unported string
	// Invalid is httpx.InvalidURL's text: the client cannot be built, and
	// the error is not one the callers' except clauses name.
	Invalid string
}

func (r *Refusal) Error() string {
	if r.Fails != "" {
		return r.Fails
	}
	if r.Invalid != "" {
		return r.Invalid
	}
	return "a proxy setting this binary does not read as the oracle does: " + r.Unported
}

// Origin is where a request goes: TLS or not, the host as the URL has it
// (an IPv6 address without brackets), the port, and whether the URL wrote
// the port.
type Origin struct {
	TLS          bool
	Host         string
	Port         int
	ExplicitPort bool
}

// ---------------------------------------------------------------------------
// httpx
// ---------------------------------------------------------------------------

type pattern struct {
	scheme, host string
	port         int // 0: any
}

func (p pattern) priority() [3]int {
	pp := 1
	if p.port != 0 {
		pp = 0
	}
	return [3]int{pp, -utf8.RuneCountInString(p.host), -utf8.RuneCountInString(p.scheme)}
}

func suffixed(host, d string) bool {
	return len(host) > len(d)+1 && strings.HasSuffix(host, d) && host[len(host)-len(d)-1] == '.'
}

func (p pattern) matches(scheme, host string, port int) bool {
	if p.scheme != "" && p.scheme != scheme {
		return false
	}
	if p.host != "" {
		var ok bool
		switch {
		case strings.HasPrefix(p.host, "*."):
			ok = suffixed(host, p.host[2:])
		case strings.HasPrefix(p.host, "*"):
			ok = host == p.host[1:] || suffixed(host, p.host[1:])
		default:
			ok = host == p.host
		}
		if !ok {
			return false
		}
	}
	if p.port != 0 && p.port != port {
		return false
	}
	return true
}

func defaultPort(scheme string) int {
	switch scheme {
	case "http", "ws":
		return 80
	case "https", "wss":
		return 443
	case "ftp":
		return 21
	}
	return 0
}

type purl struct {
	scheme, host string
	userinfo     *string
	port         int // 0: none or the scheme's default
	rest         string
}

func isSchemeChar(r rune) bool {
	return r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("+-.", r))
}

// parseURL cuts a URL as httpx parses it (parseHttpx): an *InvalidURL
// when httpx raises, an *unportedURL when this binary does not model it.
func parseURL(s string) (purl, error) {
	u, err := parseHttpx(s)
	if err != nil {
		return purl{}, err
	}
	out := purl{scheme: u.scheme, host: u.host, rest: u.rest()}
	if u.port > 0 {
		out.port = u.port
	}
	if u.userinfo != "" {
		ui := u.userinfo
		out.userinfo = &ui
	}
	return out, nil
}

// failsOr is the refusal a parse error gives: httpx's InvalidURL fails
// every request, as the client cannot be built; anything else is not
// modelled.
func failsOr(err error, why string) *Httpx {
	if inv, ok := err.(*InvalidURL); ok {
		return &Httpx{refusal: &Refusal{Invalid: inv.Msg}}
	}
	return unported(why + ": " + err.Error())
}

func isIPv4(h string) bool {
	a, err := netip.ParseAddr(strings.SplitN(h, "/", 2)[0])
	return err == nil && a.Is4()
}

func isIPv6(h string) bool {
	a, err := netip.ParseAddr(strings.SplitN(h, "/", 2)[0])
	return err == nil && a.Is6() && a.Zone() == ""
}

// Httpx is the routing httpx's Client builds from the environment.
type Httpx struct {
	mounts  []mount
	refusal *Refusal
}

type mount struct {
	pat   pattern
	proxy *Proxy
}

// HttpxFromVars reads the variables; ordered says they are in the
// environment's order.
func HttpxFromVars(vars []Var, ordered bool) *Httpx {
	p, err := getproxies(vars, ordered)
	if err != nil {
		return &Httpx{refusal: &Refusal{Unported: err.Error()}}
	}
	return httpxFrom(p)
}

func unported(why string) *Httpx { return &Httpx{refusal: &Refusal{Unported: why}} }

// httpxFrom is httpx._utils.get_environment_proxies, then the mounts.
func httpxFrom(p []kv) *Httpx {
	type key struct {
		k string
		v *string
	}
	var keys []key
	put := func(k string, v *string) {
		for i := range keys {
			if keys[i].k == k {
				keys[i].v = v
				return
			}
		}
		keys = append(keys, key{k, v})
	}
	for _, scheme := range []string{"http", "https", "all"} {
		if v, ok := get(p, scheme); ok && v != "" {
			if !strings.Contains(v, "://") {
				v = "http://" + v
			}
			vv := v
			put(scheme+"://", &vv)
		}
	}
	no, _ := get(p, "no")
	for _, entry := range strings.Split(no, ",") {
		h := pyStrip(entry)
		switch {
		case h == "*":
			return &Httpx{}
		case h == "":
			continue
		case strings.Contains(h, "://"):
			put(h, nil)
		case isIPv4(h):
			put("all://"+h, nil)
		case isIPv6(h):
			put("all://["+h+"]", nil)
		case strings.ToLower(h) == "localhost":
			put("all://"+h, nil)
		default:
			put("all://*"+h, nil)
		}
	}
	type parsed struct {
		k string
		u *purl
		v string
	}
	var ps []parsed
	for _, e := range keys {
		if e.v == nil {
			ps = append(ps, parsed{k: e.k})
			continue
		}
		u, err := parseURL(*e.v)
		if err != nil {
			return failsOr(err, "a proxy URL")
		}
		switch u.scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			if t, ok := unknownSchemeText(u); ok {
				return &Httpx{refusal: &Refusal{Fails: t}}
			}
			return unported(fmt.Sprintf("the proxy URL %q", *e.v))
		}
		ps = append(ps, parsed{e.k, &u, *e.v})
	}
	var mounts []mount
	for _, e := range ps {
		pat, err := toPattern(e.k)
		if err != nil {
			return failsOr(err, "a NO_PROXY entry")
		}
		var proxy *Proxy
		if e.u != nil {
			u := e.u
			if strings.HasPrefix(u.scheme, "socks5") {
				return &Httpx{refusal: &Refusal{Fails: SocksText}}
			}
			if u.host == "" || strings.Contains(u.host, "%") {
				// No host, or one httpx sends percent-encoded: where the
				// connection fails is not modelled.
				return unported(fmt.Sprintf("the proxy URL %q", e.v))
			}
			pr := &Proxy{TLS: u.scheme == "https", Host: u.host, Port: u.port}
			if pr.Port == 0 {
				pr.Port = 80
				if pr.TLS {
					pr.Port = 443
				}
			}
			if u.userinfo != nil {
				user, pass, _ := strings.Cut(*u.userinfo, ":")
				pr.User, pr.Pass = Unquote(user), Unquote(pass)
				pr.Auth = pr.User != "" || pr.Pass != ""
			}
			proxy = pr
		}
		mounts = append(mounts, mount{pat, proxy})
	}
	sort.SliceStable(mounts, func(i, j int) bool {
		a, b := mounts[i].pat.priority(), mounts[j].pat.priority()
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
	return &Httpx{mounts: mounts}
}

func toPattern(key string) (pattern, error) {
	if key != "" && !strings.Contains(key, ":") {
		return pattern{}, &unportedURL{fmt.Sprintf("the proxy key %q", key)}
	}
	u, err := parseURL(key)
	if err != nil {
		return pattern{}, err
	}
	// URLPattern reads the scheme, the host and the port, nothing else.
	p := pattern{scheme: u.scheme, host: u.host, port: u.port}
	if p.scheme == "all" {
		p.scheme = ""
	}
	if p.host == "*" {
		p.host = ""
	}
	if strings.Contains(p.host, "xn--") {
		// URLPattern's host is the IDNA-decoded one: its length orders
		// the mounts.
		return pattern{}, &unportedURL{fmt.Sprintf("the IDNA host in %q", key)}
	}
	return p, nil
}

func unknownSchemeText(u purl) (string, bool) {
	if u.userinfo != nil {
		return "", false
	}
	for _, r := range u.rest {
		if !(r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/-._~", r))) {
			return "", false
		}
	}
	host := u.host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := ""
	if u.port != 0 {
		port = ":" + strconv.Itoa(u.port)
	}
	return fmt.Sprintf("Unknown scheme for proxy URL URL('%s://%s%s%s')", u.scheme, host, port, u.rest), true
}

// Refusal says whether the client cannot be built at all.
func (h *Httpx) Refusal() *Refusal { return h.refusal }

// Route is Client._transport_for_url: the first mount whose pattern
// matches.
func (h *Httpx) Route(o Origin) (Route, error) {
	if h.refusal != nil {
		return Route{}, h.refusal
	}
	scheme := "http"
	if o.TLS {
		scheme = "https"
	}
	// encode_host lower-cases a name, and keeps an IPv6 address as
	// written.
	host := o.Host
	if !strings.Contains(host, ":") {
		host = strings.ToLower(host)
	}
	port := o.Port
	if port == defaultPort(scheme) {
		port = 0
	}
	for _, m := range h.mounts {
		if !m.pat.matches(scheme, host, port) {
			continue
		}
		if m.proxy == nil {
			return Route{}, nil
		}
		for _, r := range host {
			if r >= 128 {
				return Route{}, &Refusal{Unported: fmt.Sprintf("the host %q through a proxy", o.Host)}
			}
		}
		if o.TLS {
			return Route{Kind: Through, Proxy: *m.proxy, Tunnel: TunnelHttpx}, nil
		}
		return Route{Kind: Forward, Proxy: *m.proxy}, nil
	}
	return Route{}, nil
}

// ---------------------------------------------------------------------------
// urllib
// ---------------------------------------------------------------------------

// Urllib is the routing urllib.request.urlopen does with the default
// opener.
type Urllib struct {
	proxies []kv
	refusal *Refusal
}

// UrllibFromVars reads the variables.
func UrllibFromVars(vars []Var, ordered bool) *Urllib {
	p, err := getproxies(vars, ordered)
	if err != nil {
		return &Urllib{refusal: &Refusal{Unported: err.Error()}}
	}
	return &Urllib{proxies: p}
}

// bypass is proxy_bypass_environment(host): host is the URL's netloc
// host, with its port when the URL wrote one.
func (u *Urllib) bypass(host string) bool {
	no, ok := get(u.proxies, "no")
	if !ok {
		return false
	}
	if no == "*" {
		return true
	}
	host = strings.ToLower(host)
	hostonly := host
	if i := strings.LastIndex(host, ":"); i >= 0 && strings.IndexFunc(host[i+1:], func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		hostonly = host[:i]
	}
	for _, name := range strings.Split(no, ",") {
		name = pyStrip(name)
		if name == "" {
			continue
		}
		name = strings.ToLower(strings.TrimLeft(name, "."))
		if hostonly == name || host == name {
			return true
		}
		if strings.HasSuffix(hostonly, "."+name) || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}

// Route is what urlopen does for o.
func (u *Urllib) Route(o Origin) (Route, error) {
	if u.refusal != nil {
		return Route{}, u.refusal
	}
	scheme := "http"
	if o.TLS {
		scheme = "https"
	}
	value, ok := get(u.proxies, scheme)
	if !ok {
		return Route{}, nil
	}
	netloc := o.Host
	if strings.Contains(netloc, ":") {
		netloc = "[" + netloc + "]"
	}
	if o.ExplicitPort {
		netloc += ":" + strconv.Itoa(o.Port)
	}
	if u.bypass(netloc) {
		return Route{}, nil
	}
	no := &Refusal{Unported: fmt.Sprintf("the proxy %s_proxy=%q", scheme, value)}
	pscheme, user, pass, hostport, ok := parseProxy(value)
	if !ok {
		return Route{}, no
	}
	hostport = Unquote(hostport)
	for _, r := range o.Host + hostport {
		if r >= 128 {
			return Route{}, no
		}
	}
	def := 80
	if o.TLS {
		def = 443
	}
	var host string
	port := def
	if strings.HasPrefix(hostport, "[") {
		h, p, ok := strings.Cut(hostport[1:], "]")
		if !ok {
			return Route{}, no
		}
		host = h
		if p != "" && p != ":" {
			if !strings.HasPrefix(p, ":") {
				return Route{}, no
			}
			n, err := strconv.ParseUint(p[1:], 10, 16)
			if err != nil {
				return Route{}, no
			}
			port = int(n)
		}
	} else if i := strings.LastIndex(hostport, ":"); i >= 0 {
		host = hostport[:i]
		if p := hostport[i+1:]; p != "" {
			n, err := strconv.ParseUint(p, 10, 16)
			if err != nil {
				return Route{}, no
			}
			port = int(n)
		}
	} else {
		host = hostport
	}
	if host == "" {
		return Route{}, no
	}
	pr := Proxy{Host: host, Port: port}
	if user != nil && pass != nil && *user != "" && *pass != "" {
		pr.User, pr.Pass, pr.Auth = Unquote(*user), Unquote(*pass), true
	}
	if o.TLS {
		return Route{Kind: Through, Proxy: pr, Tunnel: TunnelUrllib}, nil
	}
	if pscheme == nil || *pscheme == "http" {
		return Route{Kind: Forward, Proxy: pr}, nil
	}
	return Route{}, no
}

// parseProxy is urllib.request._parse_proxy.
func parseProxy(proxy string) (scheme, user, pass *string, hostport string, ok bool) {
	rest := proxy
	if s, r, found := strings.Cut(proxy, ":"); found && s != "" &&
		strings.IndexFunc(s, func(c rune) bool { return !isSchemeChar(c) }) < 0 {
		l := strings.ToLower(s)
		scheme = &l
		rest = r
	}
	authority := proxy
	if !strings.HasPrefix(rest, "/") {
		scheme = nil
	} else {
		r, found := strings.CutPrefix(rest, "//")
		if !found {
			return nil, nil, nil, "", false
		}
		end := -1
		if at := strings.Index(r, "@"); at >= 0 {
			if i := strings.Index(r[at:], "/"); i >= 0 {
				end = at + i
			}
		} else {
			end = strings.Index(r, "/")
		}
		if end < 0 {
			end = len(r)
		}
		authority = r[:end]
	}
	hostport = authority
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		ui := authority[:i]
		hostport = authority[i+1:]
		if a, b, found := strings.Cut(ui, ":"); found {
			user, pass = &a, &b
		} else {
			user = &ui
		}
	}
	return scheme, user, pass, hostport, true
}

// Unquote is urllib.parse.unquote: each %XX escape to its byte, the bytes
// read as UTF-8 with replacement.
func Unquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	var pending []byte
	flush := func() {
		if len(pending) > 0 {
			out.WriteString(strings.ToValidUTF8(string(pending), "\uFFFD"))
			pending = pending[:0]
		}
	}
	hex := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			if h, ok := hex(s[i+1]); ok {
				if l, ok := hex(s[i+2]); ok {
					pending = append(pending, h*16+l)
					i += 3
					continue
				}
			}
		}
		flush()
		out.WriteByte(s[i])
		i++
	}
	flush()
	return out.String()
}

// ---------------------------------------------------------------------------
// The connector
// ---------------------------------------------------------------------------

// StatusError is a proxy's refusal of a CONNECT.
type StatusError struct {
	Code   int
	Reason string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Reason) }

// ConnectLine is the CONNECT request as each client writes it.
func ConnectLine(o Origin, p *Proxy, style Tunnel) string {
	host := strings.ToLower(o.Host)
	port := strconv.Itoa(o.Port)
	auth := ""
	if v := p.AuthValue(); v != "" {
		auth = "Proxy-Authorization: " + v + "\r\n"
	}
	if style == TunnelHttpx {
		target := host + ":" + port
		return "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nAccept: */*\r\n" + auth + "\r\n"
	}
	wrapped := host
	if strings.Contains(host, ":") {
		wrapped = "[" + host + "]"
	}
	return "CONNECT " + wrapped + ":" + port + " HTTP/1.1\r\n" + auth + "Host: " + host + ":" + port + "\r\n\r\n"
}

// Dialer opens connections through a route.
type Dialer struct {
	// Net dials TCP; nil uses a plain net.Dialer.
	Net func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLS is the config for the origin's TLS (its ServerName is set per
	// connection); nil uses the system's roots.
	TLS *tls.Config
}

func (d *Dialer) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if d.Net != nil {
		return d.Net(ctx, "tcp", addr)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func handshake(ctx context.Context, c net.Conn, host string, base *tls.Config) (net.Conn, error) {
	cfg := &tls.Config{}
	if base != nil {
		cfg = base.Clone()
	}
	cfg.ServerName = host
	cfg.NextProtos = []string{"http/1.1"}
	t := tls.Client(c, cfg)
	if err := t.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return t, nil
}

// Open opens a connection to o by r: TCP to the first hop, TLS to a proxy
// of the https scheme, the CONNECT for a tunnel, then the origin's TLS
// when withTLS. For a Forward route the caller sends the request in
// absolute form.
func (d *Dialer) Open(ctx context.Context, r Route, o Origin, withTLS bool) (net.Conn, error) {
	host, port := o.Host, o.Port
	if r.Kind != Direct {
		host, port = r.Proxy.Host, r.Proxy.Port
	}
	c, err := d.dial(ctx, host, port)
	if err != nil {
		return nil, err
	}
	if r.Kind != Direct && r.Proxy.TLS {
		if c, err = handshake(ctx, c, r.Proxy.Host, nil); err != nil {
			return nil, err
		}
	}
	if r.Kind == Through {
		if dl, ok := ctx.Deadline(); ok {
			c.SetDeadline(dl)
		}
		if _, err := c.Write([]byte(ConnectLine(o, &r.Proxy, r.Tunnel))); err != nil {
			c.Close()
			return nil, err
		}
		code, reason, err := readConnectAnswer(c)
		if err != nil {
			c.Close()
			return nil, err
		}
		ok := code >= 200 && code <= 299
		if r.Tunnel != TunnelHttpx {
			ok = code == 200
		}
		if !ok {
			c.Close()
			return nil, &StatusError{code, reason}
		}
		c.SetDeadline(time.Time{})
	}
	if withTLS && o.TLS {
		return handshake(ctx, c, strings.ToLower(o.Host), d.TLS)
	}
	return c, nil
}

// readConnectAnswer reads the answer's head a byte at a time, so nothing
// of the tunnel is read.
func readConnectAnswer(c net.Conn) (int, string, error) {
	var head []byte
	b := make([]byte, 1)
	for !strings.HasSuffix(string(head), "\r\n\r\n") && !strings.HasSuffix(string(head), "\n\n") {
		n, err := c.Read(b)
		if n == 1 {
			head = append(head, b[0])
		}
		if err != nil {
			return 0, "", err
		}
		if len(head) > 1<<16 {
			return 0, "", errors.New("the proxy's answer is too long")
		}
	}
	line, _, _ := bufio.NewReader(strings.NewReader(string(head))).ReadLine()
	parts := strings.SplitN(string(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, "", errors.New("the proxy answered something that is not HTTP")
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", errors.New("the proxy answered something that is not HTTP")
	}
	reason := ""
	if len(parts) == 3 {
		reason = strings.TrimSpace(parts[2])
	}
	return code, reason, nil
}

// ProxyURL is the proxy as net/http's Transport.Proxy wants it, with
// credentials only when a Proxy-Authorization is sent.
func (p *Proxy) ProxyURL() *url.URL {
	u := &url.URL{Scheme: "http", Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port))}
	if p.TLS {
		u.Scheme = "https"
	}
	if p.Auth {
		u.User = url.UserPassword(p.User, p.Pass)
	}
	return u
}

// Router is either rule set.
type Router interface {
	Route(Origin) (Route, error)
	// tlsProxies lists the host:port of each proxy reached over TLS.
	tlsProxies() map[string]bool
}

func (h *Httpx) tlsProxies() map[string]bool {
	out := map[string]bool{}
	for _, m := range h.mounts {
		if m.proxy != nil && m.proxy.TLS {
			out[net.JoinHostPort(m.proxy.Host, strconv.Itoa(m.proxy.Port))] = true
		}
	}
	return out
}

// urllib never speaks TLS to a proxy.
func (u *Urllib) tlsProxies() map[string]bool { return nil }

func originOf(u *url.URL) Origin {
	tlsOn := u.Scheme == "https"
	port := 80
	if tlsOn {
		port = 443
	}
	explicit := false
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
			explicit = true
		}
	}
	return Origin{TLS: tlsOn, Host: u.Hostname(), Port: port, ExplicitPort: explicit}
}

// Transport sets base's Proxy, DialContext and DialTLSContext so every
// request goes the way rt routes it: an http request to a proxy by
// net/http's own forward proxying, an https one through this package's
// CONNECT (net/http's own adds a User-Agent and brackets an IPv6 target).
// Setting DialTLSContext leaves the client on HTTP/1.1, as httpx is.
func Transport(rt Router, base *http.Transport) *http.Transport {
	d := &Dialer{Net: base.DialContext, TLS: base.TLSClientConfig}
	viaTLS := rt.tlsProxies()
	base.Proxy = func(req *http.Request) (*url.URL, error) {
		o := originOf(req.URL)
		r, err := rt.Route(o)
		if err != nil {
			return nil, err
		}
		if r.Kind == Forward {
			return r.Proxy.ProxyURL(), nil
		}
		return nil, nil
	}
	base.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, _ := strconv.Atoi(p)
		o := Origin{TLS: true, Host: h, Port: port, ExplicitPort: true}
		if viaTLS[addr] {
			// net/http forwards a plain request to a proxy of the https
			// scheme and dials the proxy here.
			c, err := d.dial(ctx, h, port)
			if err != nil {
				return nil, err
			}
			return handshake(ctx, c, h, nil)
		}
		r, err := rt.Route(o)
		if err != nil {
			return nil, err
		}
		return d.Open(ctx, r, o, true)
	}
	return base
}
