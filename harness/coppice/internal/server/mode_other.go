//go:build !linux

package server

// readMarker reads no marker where the server has no openat to read one
// as the gate does: the stack then shows no marker, which is the safe
// way to be wrong (F11-R-9).
func readMarker(agents, floor, name string) ([]byte, bool) {
	return nil, false
}
