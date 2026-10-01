package server

import (
	"os"
	"strconv"
	"syscall"
)

// closeInheritedOnExec sets close-on-exec on every descriptor above 2 the
// process holds. A server started from a shell or a CI runner can inherit
// descriptors without that flag, and Go's exec passes such a descriptor on
// to every child, a child that carries a key on fd 3 included. Descriptors
// Go opened itself already carry the flag, so setting it again changes
// nothing for them.
func closeInheritedOnExec() {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < 3 {
			continue
		}
		syscall.CloseOnExec(fd)
	}
}
