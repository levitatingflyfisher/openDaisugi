//go:build !unix

package foreman

import "os"

// lockDir opens the lock file. This platform has no flock, so the one
// writer rule is not enforced here.
func lockDir(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode)
}
