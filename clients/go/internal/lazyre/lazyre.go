// Package lazyre compiles a package-level regexp on its first use. Every
// gate call starts two processes of the binary, and each would compile
// every package's patterns at start, used or not.
package lazyre

import (
	"regexp"
	"sync"
)

// New returns a function that compiles pattern the first time it is
// called and returns the same *regexp.Regexp after. A bad pattern panics
// there, as regexp.MustCompile does.
func New(pattern string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(pattern) })
}
