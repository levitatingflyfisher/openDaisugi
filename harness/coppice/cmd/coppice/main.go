// Command coppice is the client and the server in one binary. The first
// invocation can bring up a background server; every later one talks to it.
package main

import (
	"os"

	"github.com/opendaisugi/coppice/internal/cli"
	"github.com/opendaisugi/coppice/internal/server"

	// Registering every adapter. Each package's init puts itself in the
	// registry, including the slots not yet built - see internal/adapters.
	_ "github.com/opendaisugi/coppice/internal/adapters/all"
)

func main() {
	c := &cli.CLI{
		Version: cli.Version(),
		Socket:  server.SocketPath(),
		DataDir: server.DataDir(),
		In:      os.Stdin,
		Out:     os.Stdout,
		Err:     os.Stderr,
	}
	os.Exit(c.Run(os.Args[1:]))
}
