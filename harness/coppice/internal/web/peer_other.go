//go:build !linux

package web

import "net"

// localPeerPIDs has no /proc to read here. A local peer cannot be placed,
// so it cannot answer an ask through the web server.
func localPeerPIDs(remote, local net.Addr) ([]int, bool, error) {
	r, ok := remote.(*net.TCPAddr)
	if !ok {
		return nil, true, errNotPlaced
	}
	if !isLocalIP(r.IP) {
		return nil, false, nil
	}
	return nil, true, errNotPlaced
}
