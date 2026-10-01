package server

import (
	"syscall"
	"testing"
)

// An inherited descriptor without close-on-exec gets the flag, so no
// child of the server sees it.
func TestAnInheritedDescriptorIsClosedOnExec(t *testing.T) {
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(p[0])
	defer syscall.Close(p[1])
	flag := func(fd int) uintptr {
		r, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if e != 0 {
			t.Fatal(e)
		}
		return r & syscall.FD_CLOEXEC
	}
	if flag(p[0]) != 0 {
		t.Skip("syscall.Pipe already set close-on-exec here")
	}
	closeInheritedOnExec()
	if flag(p[0]) == 0 || flag(p[1]) == 0 {
		t.Fatalf("pipe %v still open across exec", p)
	}
}
