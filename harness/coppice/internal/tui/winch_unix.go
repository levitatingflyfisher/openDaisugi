//go:build unix

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize returns a channel that gets SIGWINCH, the signal a
// terminal sends when its size changes, and a func that stops it.
func notifyResize() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGWINCH)
	return c, func() { signal.Stop(c) }
}
