// Command coppice is the client and the server in one binary. The first
// invocation can bring up a background server; every later one talks to it.
package main

import (
	"fmt"
	"os"

	"github.com/opendaisugi/coppice/internal/cli"
	"github.com/opendaisugi/coppice/internal/datahome"
	"github.com/opendaisugi/coppice/internal/server"

	// Registering every adapter. Each package's init puts itself in the
	// registry, including the slots not yet built - see internal/adapters.
	_ "github.com/opendaisugi/coppice/internal/adapters/all"
)

func main() {
	// F11: an old ~/.opendaisugi/coppice that holds data is kept. Say so
	// once per floor start, before the screen is taken.
	if len(os.Args) == 1 {
		if note := datahome.Legacy(); note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
	}
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
