package server

import "testing"

// Inside a pane the command dials the pane's own server. Outside one the
// environment does not choose the server.
func TestACommandInAPaneDialsThePanesServer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/u")
	t.Setenv("COPPICE_SOCK", "/odd/server.sock")
	t.Setenv("COPPICE_DATA_DIR", "/odd/data")

	t.Setenv("COPPICE_PANE", "")
	if got := CLISocketPath(); got != "/run/u/coppice/server.sock" {
		t.Errorf("outside a pane the env chose the server: %s", got)
	}
	if got := CLIDataDir(); got != DataDir() {
		t.Errorf("outside a pane the env chose the data dir: %s", got)
	}

	t.Setenv("COPPICE_PANE", "w1:p1")
	if got := CLISocketPath(); got != "/odd/server.sock" {
		t.Errorf("inside a pane: socket %s", got)
	}
	if got := CLIDataDir(); got != "/odd/data" {
		t.Errorf("inside a pane: data dir %s", got)
	}
	t.Setenv("COPPICE_SOCK", "")
	if got := CLISocketPath(); got != "/run/u/coppice/server.sock" {
		t.Errorf("a pane with no COPPICE_SOCK: %s", got)
	}
}
