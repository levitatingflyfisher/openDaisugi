package pystr

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// textChunk is the number of bytes io.TextIOWrapper asks for at a time.
const textChunk = 8192

// DecodeStream is a file opened with open(path, encoding="utf-8") and read
// line by line. Bytes that are all UTF-8 give the whole text and no error.
//
// Otherwise the reader decodes the file 8192 bytes at a time, and each
// decode call sees the bytes of an incomplete code point it held back from
// the call before, then the new chunk. The first call that meets bytes
// that are not UTF-8 raises UnicodeDecodeError, and the position in its
// text counts from the start of the bytes that call saw. The text returned
// with the error is what the reader gave out before it: the whole lines of
// the text decoded by the calls before (a CR at its end is held back, as
// it may be half of a CRLF).
func DecodeStream(b []byte) (string, *Exception) {
	if utf8.Valid(b) {
		return string(b), nil
	}
	done := 0 // the bytes decoded by the calls before
	for start := 0; ; start += textChunk {
		end := min(start+textChunk, len(b))
		final := start >= len(b)
		buf := b[done:end]
		i := 0
		for i < len(buf) {
			n, complete, badStart := utf8Subpart(buf, i)
			if complete {
				i += n
				continue
			}
			short := !badStart && i+n >= len(buf)
			if short && !final {
				break // held back for the next call
			}
			reason := "invalid continuation byte"
			switch {
			case badStart:
				reason = "invalid start byte"
			case short:
				reason = "unexpected end of data"
			}
			var msg string
			if n == 1 {
				msg = fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", buf[i], i, reason)
			} else {
				msg = fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", i, i+n-1, reason)
			}
			return wholeLines(string(b[:done])), NewException("UnicodeDecodeError", msg)
		}
		done += i
		if final {
			// Every byte decoded: utf8.Valid says this cannot be.
			return string(b), nil
		}
	}
}

// wholeLines is the part of text a line reader hands out before it needs
// more: every line that ends in LF, CR or CRLF, less a CR at the very end.
func wholeLines(text string) string {
	text = strings.TrimSuffix(text, "\r")
	if i := strings.LastIndexAny(text, "\r\n"); i >= 0 {
		return text[:i+1]
	}
	return ""
}
