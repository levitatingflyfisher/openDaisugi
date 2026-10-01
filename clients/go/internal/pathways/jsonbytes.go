package pathways

import (
	"encoding/binary"
	"unicode/utf8"

	"daisugi-verify/internal/pystr"
)

// JSONText is the text json.loads reads from bytes: json.detect_encoding
// picks UTF-8 (with or without a BOM), UTF-16 or UTF-32 from the first
// bytes, and the bytes are decoded with 'surrogatepass', so a lone
// surrogate is kept. A decode that fails is Python's UnicodeDecodeError.
func JSONText(b []byte) (string, error) {
	fail := &Invalid{"UnicodeDecodeError: the bytes do not decode in the encoding json detects"}
	at := func(i int) byte { return b[i] }
	switch {
	case len(b) >= 4 && (string(b[:4]) == "\x00\x00\xfe\xff" || string(b[:4]) == "\xff\xfe\x00\x00"):
		return utf32(b[4:], b[0] == 0xff, fail)
	case len(b) >= 2 && (string(b[:2]) == "\xfe\xff" || string(b[:2]) == "\xff\xfe"):
		return utf16(b[2:], b[0] == 0xff, fail)
	case len(b) >= 3 && string(b[:3]) == "\xef\xbb\xbf":
		return utf8Pass(b[3:], fail)
	case len(b) >= 4:
		if at(0) == 0 {
			if at(1) != 0 {
				return utf16(b, false, fail)
			}
			return utf32(b, false, fail)
		}
		if at(1) == 0 {
			if at(2) != 0 || at(3) != 0 {
				return utf16(b, true, fail)
			}
			return utf32(b, true, fail)
		}
	case len(b) == 2:
		if at(0) == 0 {
			return utf16(b, false, fail)
		}
		if at(1) == 0 {
			return utf16(b, true, fail)
		}
	}
	return utf8Pass(b, fail)
}

// utf8Pass decodes UTF-8 with 'surrogatepass': an encoded surrogate
// (ED A0..BF xx) is kept, which is how this port holds one.
func utf8Pass(b []byte, fail error) (string, error) {
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 {
			if i+2 < len(b) && b[i] == 0xED && b[i+1] >= 0xA0 && b[i+1] <= 0xBF && b[i+2] >= 0x80 && b[i+2] <= 0xBF {
				i += 3
				continue
			}
			return "", fail
		}
		i += n
	}
	return string(b), nil
}

func utf16(b []byte, le bool, fail error) (string, error) {
	if len(b)%2 != 0 {
		return "", fail // truncated data
	}
	var out []byte
	for i := 0; i < len(b); i += 2 {
		var u uint16
		if le {
			u = binary.LittleEndian.Uint16(b[i:])
		} else {
			u = binary.BigEndian.Uint16(b[i:])
		}
		r := rune(u)
		if u >= 0xD800 && u < 0xDC00 && i+3 < len(b) {
			var v uint16
			if le {
				v = binary.LittleEndian.Uint16(b[i+2:])
			} else {
				v = binary.BigEndian.Uint16(b[i+2:])
			}
			if v >= 0xDC00 && v < 0xE000 {
				r = 0x10000 + (rune(u)-0xD800)<<10 + (rune(v) - 0xDC00)
				i += 2
			}
		}
		out = pystr.AppendRune(out, r)
	}
	return string(out), nil
}

func utf32(b []byte, le bool, fail error) (string, error) {
	if len(b)%4 != 0 {
		return "", fail // truncated data
	}
	var out []byte
	for i := 0; i < len(b); i += 4 {
		var u uint32
		if le {
			u = binary.LittleEndian.Uint32(b[i:])
		} else {
			u = binary.BigEndian.Uint32(b[i:])
		}
		if u > 0x10FFFF {
			return "", fail // code point not in range(0x110000)
		}
		out = pystr.AppendRune(out, rune(u))
	}
	return string(out), nil
}
