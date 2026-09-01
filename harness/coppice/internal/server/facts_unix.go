//go:build unix

package server

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openRegular opens path for reading only when it is a regular file. It
// never follows a symlink at the last step (O_NOFOLLOW), and it never
// waits on a FIFO (O_NONBLOCK). The type check runs on the open file, so a
// path swapped between a check and the open cannot slip past it. refuse is
// true when the path exists and is not a regular file, or is a symlink:
// such a path is never read.
func openRegular(path string) (f *os.File, dev, ino uint64, refuse bool, err error) {
	f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, 0, false, err
		}
		return nil, 0, 0, true, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, 0, false, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, 0, 0, true, fmt.Errorf("%s is not a regular file", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		dev, ino = uint64(st.Dev), uint64(st.Ino)
	}
	return f, dev, ino, false, nil
}
