//go:build !unix

package tui

import "os"

// notifyResize returns nil where the system has no SIGWINCH. The floor
// then follows a size change on the next event or poll.
func notifyResize() (<-chan os.Signal, func()) {
	return nil, func() {}
}
