//go:build linux

package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
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

// peerPIDSupported is true where the kernel names a socket peer's pid.
const peerPIDSupported = true

var errUnreadablePID = errors.New("cannot read the peer pid of this connection")

// peerPID reads the pid from SO_PEERCRED.
func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, errUnreadablePID
	}
	var ucred *syscall.Ucred
	var serr error
	cerr := raw.Control(func(fd uintptr) {
		ucred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cerr != nil || serr != nil || ucred == nil || ucred.Pid <= 0 {
		return 0, errUnreadablePID
	}
	return int(ucred.Pid), nil
}

// procStat reads a pid's parent and session from /proc/<pid>/stat. The
// command name sits in parentheses and may hold spaces, so the fields are
// read after the last closing parenthesis.
func procStat(pid int) (ppid, sid int, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, fmt.Errorf("cannot read /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:])
	// state ppid pgrp session
	if len(f) < 4 {
		return 0, 0, fmt.Errorf("cannot read /proc/%d/stat", pid)
	}
	ppid, err1 := strconv.Atoi(f[1])
	sid, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("cannot read /proc/%d/stat", pid)
	}
	return ppid, sid, nil
}

// leadsSession reports whether this process leads its own session.
func leadsSession() bool {
	_, sid, err := procStat(os.Getpid())
	return err == nil && sid == os.Getpid()
}

// listPIDs lists every pid under /proc.
func listPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, pid)
		}
	}
	return out, nil
}
