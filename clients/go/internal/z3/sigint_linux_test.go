package z3

import (
	"syscall"
	"unsafe"
)

const saOnstack = 0x08000000

type kernelSigaction struct {
	handler  uintptr
	flags    uint64
	restorer uintptr
	mask     uint64
}

// sigintFlags reads the process's SIGINT sa_flags without changing them.
func sigintFlags() uint64 {
	var old kernelSigaction
	syscall.Syscall6(syscall.SYS_RT_SIGACTION, 2, 0, uintptr(unsafe.Pointer(&old)), 8, 0, 0)
	return old.flags
}
