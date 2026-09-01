//go:build !linux

package server

import (
	"errors"
	"net"
)

var errUnreadableUID = errors.New("cannot read the peer uid of this connection")

// peerUID has no portable implementation outside Linux. Returning an error
// refuses every connection, which is the fail-closed direction. Master spec
// section 8 puts other platforms out of scope for now.
func peerUID(*net.UnixConn) (uint32, error) { return 0, errUnreadableUID }
