//go:build linux

package server

import (
	"errors"
	"net"
	"syscall"
)

var errUnreadableUID = errors.New("cannot read the peer uid of this connection")

// peerUID reads SO_PEERCRED. A connection whose credentials we cannot read is
// refused, so an unexpected socket type can never be treated as trusted.
func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, errUnreadableUID
	}
	var ucred *syscall.Ucred
	var serr error
	cerr := raw.Control(func(fd uintptr) {
		ucred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cerr != nil || serr != nil || ucred == nil {
		return 0, errUnreadableUID
	}
	return ucred.Uid, nil
}
