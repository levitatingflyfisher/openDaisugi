//go:build linux

package server

import "github.com/opendaisugi/coppice/internal/gatefs"

// readMarker reads one marker as the gate does (F11-R-7, F11-R-9, GR-R-1):
// the gate root, agents/ and the floor's directory opened as directories
// without following a symlink, each owned by this user and writable by no
// group and no other user, the marker opened by name in the floor's
// directory without following a symlink and without blocking, and only a
// regular file this user owns, with one link, writable by no group and no
// other user, of at most markerMax bytes. ok is false for anything else.
// refusal is "" when something is plainly missing and the gate's one line
// (`<path>: <why> (fix: ...)`) when the gate would refuse a path.
func readMarker(root, floor, name string) (data []byte, ok bool, refusal string) {
	return gatefs.Read(root, []string{modeDir, floor}, name, markerMax)
}
