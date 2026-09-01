package pyjson

import (
	"sort"
	"strings"
)

// Canonical is json.dumps(v, sort_keys=True, separators=(",", ":"),
// ensure_ascii=False), conformance.canonical_json. Keys sort by code
// point, which is the byte order of their UTF-8 form.
func Canonical(v any) string {
	var b strings.Builder
	canonical(&b, v, false)
	return b.String()
}

// CanonicalASCII is json.dumps(v, sort_keys=True, separators=(",", ":")),
// with ensure_ascii left on: what the signing code hashes and signs.
func CanonicalASCII(v any) string {
	var b strings.Builder
	canonical(&b, v, true)
	return b.String()
}

func canonical(b *strings.Builder, v any, ascii bool) {
	switch x := v.(type) {
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			canonical(b, e, ascii)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, s, ascii)
		}
		b.WriteByte(']')
	case *Object:
		keys := append([]string{}, x.Keys()...)
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k, ascii)
			b.WriteByte(':')
			canonical(b, x.Value(k), ascii)
		}
		b.WriteByte('}')
	default:
		encode(b, v, ascii)
	}
}
