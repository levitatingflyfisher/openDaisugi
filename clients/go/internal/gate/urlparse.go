package gate

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"daisugi-verify/internal/pystr"
)

// This file is urllib.parse.urlsplit of Python 3.12.12 and the hostname
// of its result, which the permission stage reads for a network step.
// urlsplit raises ValueError on some netlocs; that is a *pystr.Exception
// panic here.

const whatwgC0OrSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

func isSchemeChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.'
}

// urlSplit is urlsplit(url): the scheme and the netloc.
func urlSplit(url string) (scheme, netloc string) {
	url = strings.TrimLeft(url, whatwgC0OrSpace)
	url = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(url)
	rs := pystr.Runes(url)
	i := -1
	for k, r := range rs {
		if r == ':' {
			i = k
			break
		}
	}
	if i > 0 && rs[0] < 128 && isAlphaByte(byte(rs[0])) {
		ok := true
		for _, c := range rs[:i] {
			if !isSchemeChar(c) {
				ok = false
				break
			}
		}
		if ok {
			scheme = pystr.Lower(pystr.FromRunes(rs[:i]))
			rs = rs[i+1:]
		}
	}
	if len(rs) >= 2 && rs[0] == '/' && rs[1] == '/' {
		delim := len(rs)
		for _, c := range []rune{'/', '?', '#'} {
			for k := 2; k < len(rs); k++ {
				if rs[k] == c {
					if k < delim {
						delim = k
					}
					break
				}
			}
		}
		netloc = pystr.FromRunes(rs[2:delim])
		open, close := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
		if open != close {
			panic(pystr.NewException("ValueError", "Invalid IPv6 URL"))
		}
		if open && close {
			checkBracketedNetloc(netloc)
		}
	}
	checkNetloc(netloc)
	return scheme, netloc
}

// checkNetloc is urllib.parse._checknetloc.
func checkNetloc(netloc string) {
	if netloc == "" || isASCII(netloc) {
		return
	}
	n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	n2 := norm.NFKC.String(n)
	if n == n2 {
		return
	}
	if strings.ContainsAny(n2, "/?#@:") {
		panic(pystr.NewException("ValueError", "netloc '"+netloc+"' contains invalid characters under NFKC normalization"))
	}
}

func partition(s, sep string) (string, bool, string) {
	a, b, ok := strings.Cut(s, sep)
	return a, ok, b
}

func rpartitionTail(s, sep string) string {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[i+len(sep):]
	}
	return s
}

// checkBracketedNetloc is urllib.parse._check_bracketed_netloc.
func checkBracketedNetloc(netloc string) {
	hostAndPort := rpartitionTail(netloc, "@")
	before, open, bracketed := partition(hostAndPort, "[")
	var hostname string
	if open {
		if before != "" {
			panic(pystr.NewException("ValueError", "Invalid IPv6 URL"))
		}
		var port string
		hostname, _, port = partition(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			panic(pystr.NewException("ValueError", "Invalid IPv6 URL"))
		}
	} else {
		hostname, _, _ = partition(hostAndPort, ":")
	}
	checkBracketedHost(hostname)
}

// checkBracketedHost is urllib.parse._check_bracketed_host.
func checkBracketedHost(hostname string) {
	if strings.HasPrefix(hostname, "v") {
		if pyMatch("url._ipvfuture", hostname) == nil {
			panic(pystr.NewException("ValueError", "IPvFuture address is invalid"))
		}
		return
	}
	if isIPv4(hostname) {
		panic(pystr.NewException("ValueError", "An IPv4 address cannot be in brackets"))
	}
	if !isIPv6(hostname) {
		panic(pystr.NewException("ValueError", pystr.Repr(hostname)+" does not appear to be an IPv4 or IPv6 address"))
	}
}

// isIPv4 is ipaddress.IPv4Address(s) not raising.
func isIPv4(s string) bool {
	if s == "" || strings.Contains(s, "/") {
		return false
	}
	octets := strings.Split(s, ".")
	if len(octets) != 4 {
		return false
	}
	for _, o := range octets {
		if o == "" || len(o) > 3 {
			return false
		}
		for i := 0; i < len(o); i++ {
			if o[i] < '0' || o[i] > '9' {
				return false
			}
		}
		if o != "0" && o[0] == '0' {
			return false
		}
		v := 0
		for i := 0; i < len(o); i++ {
			v = v*10 + int(o[i]-'0')
		}
		if v > 255 {
			return false
		}
	}
	return true
}

// isIPv6 is ipaddress.IPv6Address(s) not raising.
func isIPv6(s string) bool {
	if strings.Contains(s, "/") {
		return false
	}
	addr, sep, scope := partition(s, "%")
	if sep && (scope == "" || strings.Contains(scope, "%")) {
		return false
	}
	s = addr
	if s == "" || pystr.Len(s) > 45 {
		return false
	}
	const hextets = 8
	parts := strings.SplitN(s, ":", hextets+2)
	if len(parts) < 3 {
		return false
	}
	if strings.Contains(parts[len(parts)-1], ".") {
		v4 := parts[len(parts)-1]
		if !isIPv4(v4) {
			return false
		}
		parts = append(parts[:len(parts)-1], "0", "0")
	}
	if len(parts) > hextets+1 {
		return false
	}
	skip := -1
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			if skip >= 0 {
				return false
			}
			skip = i
		}
	}
	var hi, lo int
	if skip >= 0 {
		hi, lo = skip, len(parts)-skip-1
		if parts[0] == "" {
			hi--
			if hi != 0 {
				return false
			}
		}
		if parts[len(parts)-1] == "" {
			lo--
			if lo != 0 {
				return false
			}
		}
		if hextets-(hi+lo) < 1 {
			return false
		}
	} else {
		if len(parts) != hextets || parts[0] == "" || parts[len(parts)-1] == "" {
			return false
		}
		hi, lo = len(parts), 0
	}
	hex := func(h string) bool {
		if h == "" || len(h) > 4 {
			return false
		}
		for i := 0; i < len(h); i++ {
			c := h[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
		return true
	}
	for i := 0; i < hi; i++ {
		if !hex(parts[i]) {
			return false
		}
	}
	for i := len(parts) - lo; i < len(parts); i++ {
		if !hex(parts[i]) {
			return false
		}
	}
	return true
}

// urlHostname is SplitResult.hostname: the host of the netloc, lowered
// but for an IPv6 zone, or "" for None.
func urlHostname(netloc string) string {
	hostinfo := rpartitionTail(netloc, "@")
	_, open, bracketed := partition(hostinfo, "[")
	var hostname string
	if open {
		hostname, _, _ = partition(bracketed, "]")
	} else {
		hostname, _, _ = partition(hostinfo, ":")
	}
	if hostname == "" {
		return ""
	}
	h, pct, zone := partition(hostname, "%")
	if pct {
		return pystr.Lower(h) + "%" + zone
	}
	return pystr.Lower(h)
}
