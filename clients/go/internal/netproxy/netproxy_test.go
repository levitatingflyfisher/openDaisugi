package netproxy

import "testing"

func vars(pairs ...string) []Var {
	var out []Var
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Var{pairs[i], pairs[i+1]})
	}
	return out
}

func o(tlsOn bool, host string, port int) Origin { return Origin{TLS: tlsOn, Host: host, Port: port} }

func TestGetproxies(t *testing.T) {
	p, _ := getproxies(vars("http_proxy", "a", "HTTP_PROXY", "b", "HTTPS_PROXY", "c", "no_proxy", ""), true)
	if v, _ := get(p, "http"); v != "a" {
		t.Fatal("lower case wins", p)
	}
	if v, _ := get(p, "https"); v != "c" {
		t.Fatal(p)
	}
	if _, ok := get(p, "no"); ok {
		t.Fatal(p)
	}
	p, _ = getproxies(vars("HTTP_PROXY", "b", "REQUEST_METHOD", "GET"), true)
	if _, ok := get(p, "http"); ok {
		t.Fatal("CGI drops HTTP_PROXY")
	}
	p, _ = getproxies(vars("HTTP_PROXY", "b", "http_proxy", ""), true)
	if _, ok := get(p, "http"); ok {
		t.Fatal("an empty lower-case name removes it")
	}
	if _, err := getproxies(vars("HTTP_PROXY", "b", "Http_Proxy", "c"), false); err == nil {
		t.Fatal("order-dependent without an order")
	}
}

func route(t *testing.T, h *Httpx, org Origin) Route {
	t.Helper()
	r, err := h.Route(org)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHttpxSchemes(t *testing.T) {
	h := HttpxFromVars(vars("HTTP_PROXY", "127.0.0.1:3128", "ALL_PROXY", "http://127.0.0.1:9"), true)
	if r := route(t, h, o(false, "a.localhost", 80)); r.Kind != Forward || r.Proxy.Port != 3128 {
		t.Fatal(r)
	}
	if r := route(t, h, o(true, "a.localhost", 443)); r.Kind != Through || r.Proxy.Port != 9 {
		t.Fatal(r)
	}
	if r := route(t, h, o(false, "127.0.0.1", 8)); r.Kind != Forward {
		t.Fatal("no loopback bypass", r)
	}
}

func TestHttpxNoProxy(t *testing.T) {
	direct := func(no string, tlsOn bool, host string, port int) bool {
		h := HttpxFromVars(vars("ALL_PROXY", "http://p:1", "NO_PROXY", no), true)
		return route(t, h, o(tlsOn, host, port)).Kind == Direct
	}
	cases := []struct {
		no, host string
		tls      bool
		port     int
		want     bool
	}{
		{"example.com", "example.com", true, 443, true},
		{"example.com", "www.example.com", true, 443, true},
		{"example.com", "wwwexample.com", true, 443, false},
		{".example.com", "example.com", true, 443, false},
		{".example.com", "www.example.com", true, 443, true},
		{"*", "x", false, 80, true},
		{"a.b, *, c", "x", false, 80, true},
		{"127.0.0.1", "127.0.0.1", false, 9, true},
		{"127.0.0.1", "127.0.0.2", false, 9, false},
		{"192.168.0.0/16", "192.168.0.0", false, 9, true},
		{"192.168.0.0/16", "192.168.1.1", false, 9, false},
		{"::1", "::1", false, 9, true},
		{"LOCALHOST", "localhost", false, 9, true},
		{"LOCALHOST", "a.localhost", false, 9, false},
		{"example.com:8443", "example.com", true, 8443, true},
		{"example.com:8443", "example.com", true, 443, false},
		{"example.com:443", "example.com", true, 443, false},
		{"http://example.com", "example.com", false, 80, true},
		{"http://example.com", "example.com", true, 443, false},
	}
	for _, c := range cases {
		if got := direct(c.no, c.tls, c.host, c.port); got != c.want {
			t.Errorf("NO_PROXY=%q %s:%d: direct %v", c.no, c.host, c.port, got)
		}
	}
}

func TestHttpxRefusals(t *testing.T) {
	h := HttpxFromVars(vars("HTTPS_PROXY", "socks5://127.0.0.1:9"), true)
	if _, err := h.Route(o(false, "x", 80)); err == nil || err.Error() != SocksText {
		t.Fatal(err)
	}
	h = HttpxFromVars(vars("HTTP_PROXY", "ftp://127.0.0.1:9", "HTTPS_PROXY", "socks5://h:1"), true)
	if _, err := h.Route(o(false, "x", 80)); err == nil || err.Error() != "Unknown scheme for proxy URL URL('ftp://127.0.0.1:9')" {
		t.Fatal(err)
	}
	h = HttpxFromVars(vars("HTTPS_PROXY", "socks5://h:1", "NO_PROXY", "*"), true)
	if r := route(t, h, o(false, "x", 80)); r.Kind != Direct {
		t.Fatal(r)
	}
}

func TestCredentials(t *testing.T) {
	h := HttpxFromVars(vars("HTTP_PROXY", "http://us%65r:p%40ss@127.0.0.1:3"), true)
	r := route(t, h, o(false, "x", 80))
	if r.Proxy.AuthValue() != "Basic dXNlcjpwQHNz" {
		t.Fatal(r.Proxy.AuthValue())
	}
	h = HttpxFromVars(vars("HTTP_PROXY", "http://u:@127.0.0.1:3"), true)
	if v := route(t, h, o(false, "x", 80)).Proxy.AuthValue(); v != "Basic dTo=" {
		t.Fatal(v)
	}
}

func TestConnectLines(t *testing.T) {
	p := &Proxy{Host: "p", Port: 1, User: "u", Pass: "p", Auth: true}
	v6 := o(true, "::1", 8443)
	if got := ConnectLine(v6, p, TunnelHttpx); got != "CONNECT ::1:8443 HTTP/1.1\r\nHost: ::1:8443\r\nAccept: */*\r\nProxy-Authorization: Basic dTpw\r\n\r\n" {
		t.Fatal(got)
	}
	if got := ConnectLine(v6, p, TunnelUrllib); got != "CONNECT [::1]:8443 HTTP/1.1\r\nProxy-Authorization: Basic dTpw\r\nHost: ::1:8443\r\n\r\n" {
		t.Fatal(got)
	}
}

func TestUrllib(t *testing.T) {
	u := func(pairs ...string) *Urllib { return UrllibFromVars(vars(pairs...), true) }
	r, _ := u("ALL_PROXY", "http://p:1").Route(o(true, "x", 443))
	if r.Kind != Direct {
		t.Fatal("no ALL_PROXY in urllib")
	}
	r, _ = u("https_proxy", "socks5://u:p@p:3").Route(o(true, "hf.co", 443))
	if r.Kind != Through || r.Tunnel != TunnelUrllib || r.Proxy.Port != 3 || r.Proxy.AuthValue() != "Basic dTpw" {
		t.Fatal(r)
	}
	r, _ = u("https_proxy", "http://u@p").Route(o(true, "hf.co", 443))
	if r.Proxy.Port != 443 || r.Proxy.Auth {
		t.Fatal(r)
	}
	x := u("https_proxy", "p:3", "no_proxy", ".hf.co")
	for host, want := range map[string]Kind{"hf.co": Direct, "cdn.hf.co": Direct, "xhf.co": Through} {
		if r, _ := x.Route(o(true, host, 443)); r.Kind != want {
			t.Error(host, r)
		}
	}
	if r, _ := u("https_proxy", "p:3", "no_proxy", "a, *").Route(o(true, "hf.co", 443)); r.Kind != Through {
		t.Fatal("only a whole '*' bypasses")
	}
	y := u("http_proxy", "http://p:3", "no_proxy", "127.0.0.1:9")
	if r, _ := y.Route(Origin{Host: "127.0.0.1", Port: 9, ExplicitPort: true}); r.Kind != Direct {
		t.Fatal(r)
	}
	if r, _ := y.Route(Origin{Host: "127.0.0.1", Port: 8, ExplicitPort: true}); r.Kind != Forward {
		t.Fatal(r)
	}
}
