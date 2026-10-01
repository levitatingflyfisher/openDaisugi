package server

import (
	"os"
	"path/filepath"

	"github.com/opendaisugi/coppice/internal/datahome"
)

// SocketPath is master spec 3.3: the runtime dir when the session has one,
// otherwise a private directory under the data home (internal/datahome).
func SocketPath() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "coppice", "server.sock")
	}
	return filepath.Join(datahome.Default(), "coppice", "server.sock")
}

// DataDir holds the layout file and anything else that must survive a restart.
// It is never the runtime dir, which the system clears on logout. It sits
// under the data home (internal/datahome).
func DataDir() string {
	return filepath.Join(datahome.Default(), "coppice")
}
