//go:build linux

package server

import "syscall"

// readMarker reads one marker as the gate does (F11-R-7, F11-R-9):
// agents/ and the floor's directory opened as directories without
// following a symlink, the marker opened by name in the floor's directory
// without following a symlink and without blocking, and only a regular
// file this user owns, with one link, writable by no group and no other
// user, of at most markerMax bytes. ok is false for anything else.
func readMarker(agents, floor, name string) (data []byte, ok bool) {
	const dirFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	afd, err := syscall.Open(agents, dirFlags, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.Close(afd)
	ffd, err := syscall.Openat(afd, floor, dirFlags, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.Close(ffd)
	fd, err := syscall.Openat(ffd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		int(st.Uid) != syscall.Geteuid() || st.Nlink != 1 || st.Mode&0o022 != 0 || st.Size > markerMax {
		return nil, false
	}
	buf := make([]byte, markerMax+1)
	n, err := syscall.Read(fd, buf)
	if err != nil || n < 0 {
		return nil, false
	}
	return buf[:n], true
}
