package server

import (
	"os"
	"path/filepath"
)

// SocketPath is master spec 3.3: the runtime dir when the session has one,
// otherwise a private directory in the home directory.
func SocketPath() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "coppice", "server.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".opendaisugi", "coppice", "server.sock")
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "server.sock")
}

// DataDir holds the layout file and anything else that must survive a restart.
// It is never the runtime dir, which the system clears on logout.
func DataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".opendaisugi", "coppice")
	}
	return filepath.Join(home, ".opendaisugi", "coppice")
}
