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

// peerPIDSupported is false here: the hello alone decides a connection's
// role on a system with no peer pid.
const peerPIDSupported = false

func peerPID(*net.UnixConn) (int, error) { return 0, errors.New("no peer pid on this system") }

func procStat(int) (int, int, error) { return 0, 0, errors.New("no /proc on this system") }

func leadsSession() bool { return false }

func listPIDs() ([]int, error) { return nil, errors.New("no /proc on this system") }
