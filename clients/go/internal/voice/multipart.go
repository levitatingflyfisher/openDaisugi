package voice

import (
	"bytes"
	"errors"
	"strings"

	"daisugi-verify/internal/pystr"
)

// Latin1 is bytes.decode("latin-1"): each byte one code point.
func Latin1(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		s.WriteRune(rune(c))
	}
	return s.String()
}

// ExtractMultipartAudio is server._extract_multipart_audio: the first part
// that names a Content-Disposition and has a blank line, and that part's
// own Content-Type (the last one given), or application/octet-stream.
// contentType is the header as Python reads it, latin-1 decoded.
func ExtractMultipartAudio(body []byte, contentType string) ([]byte, string, error) {
	const marker = "boundary="
	idx := strings.Index(contentType, marker)
	if idx == -1 {
		return nil, "", errors.New("multipart request is missing a boundary")
	}
	boundary := strings.Trim(pystr.Strip(contentType[idx+len(marker):]), `"`)
	sep := append([]byte("--"), []byte(boundary)...)
	for _, part := range bytes.Split(body, sep) {
		if !bytes.Contains(part, []byte("Content-Disposition")) {
			continue
		}
		end := bytes.Index(part, []byte("\r\n\r\n"))
		if end == -1 {
			continue
		}
		headers := Latin1(part[:end])
		payload := part[end+4:]
		payload = bytes.TrimSuffix(payload, []byte("\r\n"))
		partType := "application/octet-stream"
		for _, line := range pystr.Splitlines(headers) {
			if strings.HasPrefix(asciiLower(line), "content-type:") {
				_, v, _ := strings.Cut(line, ":")
				partType = pystr.Strip(v)
			}
		}
		return payload, partType, nil
	}
	return nil, "", errors.New("multipart request has no file part")
}

// asciiLower lowers ASCII letters only. Within latin-1 text no other
// character lowers to an ASCII letter, so a prefix test on it is
// str.lower().startswith().
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
