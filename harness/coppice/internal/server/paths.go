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

// CLISocketPath is the socket a coppice command dials when no --socket flag
// names one. Inside a pane (COPPICE_PANE set, which only the server does)
// it is the pane's own COPPICE_SOCK, so a steward on a server with a
// non-default socket reaches that server. Outside a pane the environment
// does not choose the server: a reviewer's stray COPPICE_SOCK must not
// point a command at a live one. A process the pane's agent starts has the
// pane's env, and so the pane's own server; that is the right one.
func CLISocketPath() string {
	if os.Getenv("COPPICE_PANE") != "" {
		if s := os.Getenv("COPPICE_SOCK"); s != "" {
			return s
		}
	}
	return SocketPath()
}

// CLIDataDir is DataDir, or inside a pane the pane's COPPICE_DATA_DIR. See
// CLISocketPath.
func CLIDataDir() string {
	if os.Getenv("COPPICE_PANE") != "" {
		if d := os.Getenv("COPPICE_DATA_DIR"); d != "" {
			return d
		}
	}
	return DataDir()
}
