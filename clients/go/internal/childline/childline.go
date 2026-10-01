// Package childline is the line protocol both resident children share:
// the voice engine (internal/voice) and a pack's worker (internal/pack).
// A request is a frame; each answer is one JSON object per line; a parent
// that sees a child end names how it ended. It is below both packages, so
// the pack client does not import the voice package. The one definition
// is src/opendaisugi/childline.py.
package childline

import (
	"encoding/binary"
	"fmt"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Frame is one request: the body's length, big-endian, then the body.
func Frame(body []byte) []byte {
	out := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

// ParseLine is childline.parse_line: the line as a JSON object, or nil
// when it is not UTF-8 or not one object.
func ParseLine(raw []byte) *pyjson.Object {
	text, err := pystr.DecodeStrict(raw)
	if err != nil {
		return nil
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		return nil
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// ExitReason is childline.exit_reason for a child that ended with rc.
func ExitReason(rc int, tail []string) string {
	what := fmt.Sprintf("exited %d", rc)
	if rc < 0 {
		what = fmt.Sprintf("killed by signal %d", -rc)
	}
	if len(tail) > 0 {
		if last := pystr.Slice(pystr.Strip(tail[len(tail)-1]), 0, 200); last != "" {
			return what + ": " + last
		}
	}
	return what
}
