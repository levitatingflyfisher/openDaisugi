package tui

import (
	"crypto/rand"
	"fmt"
)

// newAct is a fresh client act id for one sentence to the foreman (FM-R-65):
// a random version 4 UUID. The server uses it to log the sentence once and
// to queue one turn, however many times it arrives.
func newAct() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// No entropy: the act is left out, and the sentence behaves as it
		// did before ids existed.
		return ""
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// actFor is the act id of the sentence being sent. A sentence whose send
// failed and that is sent again keeps its id; the next sentence gets a new
// one.
func (f *floor) actFor(line string) string {
	if f.act.line != line || f.act.id == "" {
		f.act.line, f.act.id = line, newAct()
	}
	return f.act.id
}

// actDone forgets the id once the server has answered.
func (f *floor) actDone() { f.act.line, f.act.id = "", "" }
