//go:build linux

package web

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// localPeerPIDs finds the processes on this box that hold the client end of
// a TCP connection. remote is the peer as this server sees it, and local is
// this server's end. local is false for a peer on another host. For a
// local peer, err is set when no process could be found.
//
// The client end is the row in /proc/net/tcp or tcp6 whose local address
// is our remote and whose remote address is our local. Its inode names a
// socket, and the processes that hold it have a /proc/<pid>/fd link to
// socket:[inode].
func localPeerPIDs(remote, local net.Addr) (pids []int, isLocal bool, err error) {
	r, ok1 := remote.(*net.TCPAddr)
	l, ok2 := local.(*net.TCPAddr)
	if !ok1 || !ok2 {
		return nil, true, errNotPlaced
	}
	if !isLocalIP(r.IP) {
		return nil, false, nil
	}
	inodes := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		scanTCP(f, r, l, inodes)
	}
	if len(inodes) == 0 {
		return nil, true, errNotPlaced
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, true, errNotPlaced
	}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", p.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", p.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] {
				pids = append(pids, pid)
				break
			}
		}
	}
	if len(pids) == 0 {
		return nil, true, errNotPlaced
	}
	return pids, true, nil
}

// scanTCP adds to inodes the inode of every row in file whose local
// address is r and whose remote address is l.
func scanTCP(file string, r, l *net.TCPAddr, inodes map[string]bool) {
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // the header line
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		lip, lport, err1 := parseProcAddr(fields[1])
		rip, rport, err2 := parseProcAddr(fields[2])
		if err1 != nil || err2 != nil {
			continue
		}
		if lport == r.Port && rport == l.Port && lip.Equal(r.IP) && rip.Equal(l.IP) && fields[9] != "0" {
			inodes[fields[9]] = true
		}
	}
}
