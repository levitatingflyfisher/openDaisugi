package netproxy

import "testing"

// The answers httpx 0.28's urlparse gives (read off the oracle's httpx).
func TestParseHttpxAsHttpx(t *testing.T) {
	type want struct {
		scheme, userinfo, host string
		port                   int
		rest                   string
		invalid                string
	}
	for in, w := range map[string]want{
		"http://127.0.0.1:+8080":      {"http", "", "127.0.0.1", 8080, "", ""},
		"http://127.0.0.1: 8080":      {"http", "", "127.0.0.1", 8080, "", ""},
		"http://127.0.0.1:x":          {invalid: "Invalid port: 'x'"},
		"http://127.0.0.01:1":         {invalid: "Invalid IPv4 address: '127.0.0.01'"},
		"http://[::1:1":               {invalid: "Invalid port: ':1:1'"},
		"http://bü_cher:1":            {invalid: "Invalid IDNA hostname: 'bü_cher'"},
		"http://bücher.localhost:1":   {"http", "", "xn--bcher-kva.localhost", 1, "", ""},
		"http://Bücher.Example/":      {"http", "", "xn--bcher-kva.example", -1, "/", ""},
		"all://*x%y":                  {"all", "", "*x%y", -1, "", ""},
		"all://*a:b":                  {invalid: "Invalid port: 'b'"},
		"all://*bücher.example":       {invalid: "Invalid IDNA hostname: '*bücher.example'"},
		"http://p q:1":                {"http", "", "p%20q", 1, "", ""},
		"HTTP://h:80":                 {"http", "", "h", 80, "", ""},
		"http://h:80":                 {"http", "", "h", -1, "", ""},
		"http://u:p@h/a/./b/../c?q#f": {"http", "u:p", "h", -1, "/a/c?q#f", ""},
		"http://münchen-straße.de":    {"http", "", "xn--mnchen-strae-v9a90b.de", -1, "", ""},
		"http://ab--cd.de":            {"http", "", "ab--cd.de", -1, "", ""},
	} {
		u, err := parseHttpx(in)
		if w.invalid != "" {
			if inv, ok := err.(*InvalidURL); !ok || inv.Msg != w.invalid {
				t.Errorf("%q: %v, want InvalidURL %q", in, err, w.invalid)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if u.scheme != w.scheme || u.userinfo != w.userinfo || u.host != w.host || u.port != w.port || u.rest() != w.rest {
			t.Errorf("%q: got %+v rest %q, want %+v", in, u, u.rest(), w)
		}
	}
	// A port no socket takes, or with a non-ASCII space, is refused.
	for _, in := range []string{"http://h:99999", "http://h:0", "http://h:-1", "http://h:\u300080"} {
		if _, err := parseHttpx(in); err == nil {
			t.Errorf("%q read", in)
		} else if _, ok := err.(*unportedURL); !ok {
			t.Errorf("%q: %v, want a refusal", in, err)
		}
	}
}

// A NO_PROXY entry httpx cannot parse stops the client: every request
// fails with InvalidURL's words.
func TestNoProxyEntryInvalidURL(t *testing.T) {
	h := HttpxFromVars([]Var{{"HTTP_PROXY", "http://127.0.0.1:1"}, {"NO_PROXY", "a:b"}}, true)
	if r := h.Refusal(); r == nil || r.Invalid != "Invalid port: 'b'" {
		t.Fatalf("%+v", r)
	}
	h = HttpxFromVars([]Var{{"HTTP_PROXY", "http://127.0.0.1:1"}, {"NO_PROXY", "x%y"}}, true)
	if r := h.Refusal(); r != nil {
		t.Fatalf("%+v", r)
	}
}

// A digit of another script is its value, as int() reads it.
func TestPortInOtherDigits(t *testing.T) {
	u, err := parseHttpx("http://h:\uff18\uff10\u0668\u0660")
	if err != nil || u.port != 8080 {
		t.Fatalf("%+v %v", u, err)
	}
}

// An error quotes the port as the URL wrote it, as httpx does.
func TestInvalidPortQuotesTheText(t *testing.T) {
	_, err := parseHttpx("http://h:\uff18x")
	if ie, ok := err.(*InvalidURL); !ok || ie.Msg != "Invalid port: '\uff18x'" {
		t.Fatalf("%v", err)
	}
}
