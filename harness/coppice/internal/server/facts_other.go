//go:build !unix

package server

import (
	"errors"
	"os"
)

// openRegular refuses every path outside unix. There is no portable way
// here to open a file without following a symlink, so the floor reads no
// transcript at all rather than one it cannot check.
func openRegular(path string) (*os.File, uint64, uint64, bool, error) {
	return nil, 0, 0, true, errors.New("transcripts are read only on unix")
}
