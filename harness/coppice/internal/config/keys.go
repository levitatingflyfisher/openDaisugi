package config

import (
	"errors"
	"fmt"
	"strings"
)

// Keys is the [keys] table: the two keys coppice takes for itself inside
// a pane. Leave names the key that goes back to the rail, as ctrl-space,
// ctrl-] or ctrl-a. Empty means DefaultLeave.
type Keys struct {
	Leave string `toml:"leave"`
	// Talk names the key that records a voice clip. Empty means
	// DefaultTalk.
	Talk string `toml:"talk,omitempty"`
}

// DefaultLeave is ctrl-space, byte 0x00, the key that leaves a pane when
// the file names no other key. Claude Code binds nothing to it, and one
// hand can press it.
const DefaultLeave byte = 0x00

// keyNameError is the one line ParseKey refuses with.
var keyNameError = errors.New("the leave key is one ctrl key, like ctrl-]")

// ParseKey reads a key name and returns its byte. It accepts ctrl-space
// and ctrl-@, which are both 0x00, ctrl-a to ctrl-z, and ctrl-\, ctrl-],
// ctrl-^ and ctrl-_, with a plus or a minus after ctrl, in any case, with
// spaces around it. Four ctrl keys are
// refused: ctrl-[ is ESC, the byte every escape sequence starts with, and
// ctrl-i, ctrl-j, and ctrl-m are Tab and Enter, keys every harness reads.
// Every other name is refused with the same line, which shows the shape.
func ParseKey(name string) (byte, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	rest, ok := strings.CutPrefix(name, "ctrl-")
	if !ok {
		rest, ok = strings.CutPrefix(name, "ctrl+")
	}
	if ok && (rest == "space" || rest == "@") {
		return 0x00, nil
	}
	if !ok || len(rest) != 1 {
		return 0, keyNameError
	}
	var b byte
	switch c := rest[0]; {
	case c >= 'a' && c <= 'z':
		b = c - 'a' + 1
	case c >= '[' && c <= '_':
		b = c - '[' + 0x1b
	default:
		return 0, keyNameError
	}
	switch b {
	case 0x09, 0x0a, 0x0d, 0x1b:
		return 0, keyNameError
	}
	return b, nil
}

// KeyName is the name ParseKey reads for byte b. 0x00 is ctrl-space. A
// byte outside the ctrl range gives a question mark.
func KeyName(b byte) string {
	switch {
	case b == 0x00:
		return "ctrl-space"
	case b >= 0x01 && b <= 0x1a:
		return "ctrl-" + string(rune('a'+b-1))
	case b >= 0x1b && b <= 0x1f:
		return "ctrl-" + string(rune('['+b-0x1b))
	}
	return "?"
}

// LeaveKey is the byte that leaves a pane. An empty [keys] leave gives
// DefaultLeave. A value ParseKey refuses is an error that names the file,
// so the run stops before a pane is ever attached with a key nobody can
// press.
func (c Config) LeaveKey() (byte, error) {
	if strings.TrimSpace(c.Keys.Leave) == "" {
		return DefaultLeave, nil
	}
	b, err := ParseKey(c.Keys.Leave)
	if err != nil {
		return 0, fmt.Errorf("bad [keys] leave %q in %s: %w", c.Keys.Leave, Path(), err)
	}
	return b, nil
}
