package netproxy

// httpx's URL parser (httpx/_urlparse.py, httpx 0.28) for the URLs a proxy
// setting holds: a proxy URL and a NO_PROXY pattern. It gives the parts
// httpx gives, raises httpx.InvalidURL with httpx's words, and refuses
// (unportedURL) what it does not model: a port Python's int() might read
// another way or a socket cannot take, and an IDNA host outside the
// letters it encodes.

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pystr"
)

// InvalidURL is httpx.InvalidURL: the client cannot be built.
type InvalidURL struct{ Msg string }

func (e *InvalidURL) Error() string { return e.Msg }

// unportedURL is a URL this binary does not read the way httpx does.
type unportedURL struct{ why string }

func (e *unportedURL) Error() string { return e.why }

const (
	unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	subDelims  = "!$&'()*+,;="
)

func safeRange(exclude string) string {
	var b strings.Builder
	for i := 0x20; i < 0x7f; i++ {
		if !strings.ContainsRune(exclude, rune(i)) {
			b.WriteByte(byte(i))
		}
	}
	return b.String()
}

var (
	fragSafe     = safeRange("\x20\x22\x3c\x3e\x60")
	querySafe    = safeRange("\x20\x22\x23\x3c\x3e")
	pathSafe     = safeRange("\x20\x22\x23\x3c\x3e\x3f\x60\x7b\x7d")
	userinfoSafe = safeRange("\x20\x22\x23\x3c\x3e\x3f\x60\x7b\x7d\x2f\x3b\x3d\x40\x5b\x5c\x5d\x5e\x7c")

	urlRE       = regexp.MustCompile(`^(?:(?P<scheme>[a-zA-Z][a-zA-Z0-9+.-]*)?:)?(?://(?P<authority>[^/?#]*))?(?P<path>[^?#]*)(?:\?(?P<query>[^#]*))?(?:#(?P<fragment>(?s:.*)))?`)
	authorityRE = regexp.MustCompile(`^(?:(?P<userinfo>(?s:.*))@)?(?P<host>\[(?s:.*)\]|[^:@]*):?(?P<port>(?s:.*))?`)
	ipv4StyleRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	pctRE       = regexp.MustCompile(`%[A-Fa-f0-9]{2}`)
	portRE      = regexp.MustCompile(`^ *[+-]?[0-9]+(?:_[0-9]+)* *$`)
)

// httpxURL is urlparse(url) for the parts a proxy route reads.
type httpxURL struct {
	scheme, userinfo, host string
	port                   int // -1: none (or the scheme's default)
	path                   string
	query, fragment        *string
}

// rest is the path, query and fragment as str(URL) writes them.
func (u httpxURL) rest() string {
	s := u.path
	if u.query != nil {
		s += "?" + *u.query
	}
	if u.fragment != nil {
		s += "#" + *u.fragment
	}
	return s
}

// parseHttpx is httpx._urlparse.urlparse(s).
func parseHttpx(s string) (httpxURL, error) {
	if utf8.RuneCountInString(s) > 65536 {
		return httpxURL{}, &InvalidURL{"URL too long"}
	}
	idx := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return httpxURL{}, &InvalidURL{fmt.Sprintf(
				"Invalid non-printable ASCII character in URL, %s at position %d.", pystr.Repr(string(r)), idx)}
		}
		idx++
	}
	m := urlRE.FindStringSubmatch(s)
	scheme := m[urlRE.SubexpIndex("scheme")]
	authority := m[urlRE.SubexpIndex("authority")]
	path := m[urlRE.SubexpIndex("path")]
	query := optional(urlRE, s, "query")
	frag := optional(urlRE, s, "fragment")
	am := authorityRE.FindStringSubmatch(authority)
	userinfo := am[authorityRE.SubexpIndex("userinfo")]
	host := am[authorityRE.SubexpIndex("host")]
	portText := am[authorityRE.SubexpIndex("port")]

	out := httpxURL{scheme: strings.ToLower(scheme), userinfo: quoteURL(userinfo, userinfoSafe), port: -1}
	h, err := encodeHost(host)
	if err != nil {
		return httpxURL{}, err
	}
	out.host = h
	if portText != "" {
		// int() reads a decimal digit of any script (Unicode Nd) as its
		// value.
		// An error quotes the port as the URL wrote it.
		digits := asciiDigits(portText)
		if !utf8.ValidString(digits) || !isASCII(digits) {
			return httpxURL{}, &unportedURL{fmt.Sprintf("the port %q", portText)}
		}
		if !portRE.MatchString(digits) {
			return httpxURL{}, &InvalidURL{"Invalid port: " + pystr.Repr(portText)}
		}
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(digits), "_", ""))
		if err != nil || n <= 0 || n > 65535 {
			return httpxURL{}, &unportedURL{fmt.Sprintf("the port %q", portText)}
		}
		// normalize_port looks the default up under the scheme as written.
		if def, ok := map[string]int{"ftp": 21, "http": 80, "https": 443, "ws": 80, "wss": 443}[scheme]; !ok || n != def {
			out.port = n
		}
	}
	hasScheme := out.scheme != ""
	hasAuthority := out.userinfo != "" || out.host != "" || out.port != -1
	if hasAuthority && path != "" && !strings.HasPrefix(path, "/") {
		return httpxURL{}, &InvalidURL{"For absolute URLs, path must be empty or begin with '/'"}
	}
	if !hasScheme && !hasAuthority {
		if strings.HasPrefix(path, "//") {
			return httpxURL{}, &InvalidURL{"Relative URLs cannot have a path starting with '//'"}
		}
		if strings.HasPrefix(path, ":") {
			return httpxURL{}, &InvalidURL{"Relative URLs cannot have a path starting with ':'"}
		}
	}
	if hasScheme || hasAuthority {
		path = normalizePath(path)
	}
	out.path = quoteURL(path, pathSafe)
	if query != nil {
		q := quoteURL(*query, querySafe)
		out.query = &q
	}
	if frag != nil {
		f := quoteURL(*frag, fragSafe)
		out.fragment = &f
	}
	return out, nil
}

// optional is a group that may match the empty text or not take part:
// None in Python when it does not take part.
func optional(re *regexp.Regexp, s, name string) *string {
	loc := re.FindStringSubmatchIndex(s)
	i := re.SubexpIndex(name)
	if loc[2*i] < 0 {
		return nil
	}
	v := s[loc[2*i]:loc[2*i+1]]
	return &v
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// encodeHost is httpx._urlparse.encode_host.
func encodeHost(host string) (string, error) {
	switch {
	case host == "":
		return "", nil
	case ipv4StyleRE.MatchString(host):
		for _, part := range strings.Split(host, ".") {
			n, err := strconv.Atoi(part)
			if err != nil || n > 255 || (len(part) > 1 && part[0] == '0') || len(part) > 3 {
				return "", &InvalidURL{"Invalid IPv4 address: " + pystr.Repr(host)}
			}
		}
		return host, nil
	case strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && len(host) >= 2:
		inner := host[1 : len(host)-1]
		a, err := netip.ParseAddr(inner)
		if err != nil || !a.Is6() {
			if strings.Contains(inner, "%") {
				return "", &unportedURL{"the IPv6 address " + pystr.Repr(host)}
			}
			return "", &InvalidURL{"Invalid IPv6 address: " + pystr.Repr(host)}
		}
		if a.Zone() != "" {
			return "", &unportedURL{"the IPv6 address " + pystr.Repr(host)}
		}
		return inner, nil
	case isASCII(host):
		return quoteURL(strings.ToLower(host), subDelims+"\"`{}%|\\"), nil
	}
	return idnaEncode(host)
}

// normalizePath is httpx._urlparse.normalize_path.
func normalizePath(path string) string {
	if !strings.Contains(path, ".") {
		return path
	}
	comps := strings.Split(path, "/")
	has := false
	for _, c := range comps {
		if c == "." || c == ".." {
			has = true
		}
	}
	if !has {
		return path
	}
	var out []string
	for _, c := range comps {
		switch c {
		case ".":
		case "..":
			if len(out) > 0 && !(len(out) == 1 && out[0] == "") {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, c)
		}
	}
	return strings.Join(out, "/")
}

func percentEncoded(s, safe string) string {
	ok := unreserved + safe
	all := true
	for _, r := range s {
		if !strings.ContainsRune(ok, r) {
			all = false
			break
		}
	}
	if all {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(ok, r) {
			b.WriteRune(r)
			continue
		}
		for _, c := range []byte(string(r)) {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// quoteURL is httpx._urlparse.quote: percent-encoding that keeps the
// '%xx' escapes already there.
func quoteURL(s, safe string) string {
	var b strings.Builder
	cur := 0
	for _, loc := range pctRE.FindAllStringIndex(s, -1) {
		if loc[0] != cur {
			b.WriteString(percentEncoded(s[cur:loc[0]], safe))
		}
		b.WriteString(s[loc[0]:loc[1]])
		cur = loc[1]
	}
	if cur != len(s) {
		b.WriteString(percentEncoded(s[cur:], safe))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// IDNA (the idna package's encode) for the letters this binary models
// ---------------------------------------------------------------------------

// idnaLetter is a code point the idna package takes as PVALID and this
// binary encodes: the lower-case Latin-1 letters.
func idnaLetter(r rune) bool {
	return (r >= 0xdf && r <= 0xff && r != 0xf7)
}

func ldh(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
}

// idnaEncode is idna.encode(host.lower()).decode("ascii"), or the
// InvalidURL httpx raises for an IDNAError.
func idnaEncode(host string) (string, error) {
	bad := &InvalidURL{"Invalid IDNA hostname: " + pystr.Repr(host)}
	s := pystr.Lower(host)
	labels := strings.Split(s, ".")
	if len(labels) > 1 && labels[len(labels)-1] == "" {
		labels = labels[:len(labels)-1]
	}
	var out []string
	for _, label := range labels {
		if label == "" {
			return "", bad
		}
		ascii := isASCII(label)
		for _, r := range label {
			if r < 0x80 {
				if !ldh(r) {
					return "", bad
				}
			} else if !idnaLetter(r) {
				return "", &unportedURL{"the host " + pystr.Repr(host)}
			}
		}
		if strings.HasPrefix(label, "xn--") {
			return "", &unportedURL{"the host " + pystr.Repr(host)}
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", bad
		}
		if rs := []rune(label); len(rs) >= 4 && rs[2] == '-' && rs[3] == '-' {
			return "", bad
		}
		enc := label
		if !ascii {
			enc = "xn--" + punycode(label)
		}
		if len(enc) > 63 {
			return "", bad
		}
		out = append(out, enc)
	}
	res := strings.Join(out, ".")
	if strings.HasSuffix(s, ".") {
		res += "."
	}
	if len(res) > 253+boolInt(strings.HasSuffix(res, ".")) {
		return "", bad
	}
	return res, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// punycode is RFC 3492's encoding of a label.
func punycode(label string) string {
	const (
		base, tmin, tmax, skew, damp = 36, 1, 26, 38, 700
		initialBias, initialN        = 72, 128
	)
	rs := []rune(label)
	var out []byte
	for _, r := range rs {
		if r < 0x80 {
			out = append(out, byte(r))
		}
	}
	b := len(out)
	h := b
	if b > 0 {
		out = append(out, '-')
	}
	digit := func(d int) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	adapt := func(delta, numPoints int, first bool) int {
		if first {
			delta /= damp
		} else {
			delta /= 2
		}
		delta += delta / numPoints
		k := 0
		for delta > ((base-tmin)*tmax)/2 {
			delta /= base - tmin
			k += base
		}
		return k + (base-tmin+1)*delta/(delta+skew)
	}
	n, delta, bias := initialN, 0, initialBias
	for h < len(rs) {
		m := int(^uint(0) >> 1)
		for _, r := range rs {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range rs {
			if int(r) < n {
				delta++
			}
			if int(r) == n {
				q := delta
				for k := base; ; k += base {
					t := k - bias
					if t < tmin {
						t = tmin
					} else if t > tmax {
						t = tmax
					}
					if q < t {
						break
					}
					out = append(out, digit(t+(q-t)%(base-t)))
					q = (q - t) / (base - t)
				}
				out = append(out, digit(q))
				bias = adapt(delta, h+1, h == b)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return string(out)
}

// ndZeros are the digit zeros of Unicode 15.0 (Python 3.12's unicodedata):
// each starts a run of ten Nd characters with the values 0 to 9.
var ndZeros = []rune{0x30, 0x660, 0x6f0, 0x7c0, 0x966, 0x9e6, 0xa66, 0xae6, 0xb66, 0xbe6, 0xc66, 0xce6, 0xd66, 0xde6, 0xe50, 0xed0, 0xf20, 0x1040, 0x1090, 0x17e0, 0x1810, 0x1946, 0x19d0, 0x1a80, 0x1a90, 0x1b50, 0x1bb0, 0x1c40, 0x1c50, 0xa620, 0xa8d0, 0xa900, 0xa9d0, 0xa9f0, 0xaa50, 0xabf0, 0xff10, 0x104a0, 0x10d30, 0x11066, 0x110f0, 0x11136, 0x111d0, 0x112f0, 0x11450, 0x114d0, 0x11650, 0x116c0, 0x11730, 0x118e0, 0x11950, 0x11c50, 0x11d50, 0x11da0, 0x11f50, 0x16a60, 0x16ac0, 0x16b50, 0x1d7ce, 0x1d7d8, 0x1d7e2, 0x1d7ec, 0x1d7f6, 0x1e140, 0x1e2f0, 0x1e4f0, 0x1e950, 0x1fbf0}

// asciiDigits is text with each decimal digit of another script written
// as its ASCII digit, as int() reads it.
func asciiDigits(text string) string {
	if isASCII(text) {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		for _, z := range ndZeros {
			if r >= z && r < z+10 && z != '0' {
				r = '0' + (r - z)
				break
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}
