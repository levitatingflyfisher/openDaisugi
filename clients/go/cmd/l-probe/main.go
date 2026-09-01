// Command l-probe is a test instrument for clients/l_compare.py: one query
// of the library parts no command reaches (the deed ledger, the strata
// store, batch runs, pathway bundles and the signing primitives),
// answered as JSON on stdout. It is not shipped.
package main

import (
	"os"

	"daisugi-verify/internal/cli"
)

func main() {
	os.Exit(cli.LProbe(&cli.Env{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Environ: os.Environ(),
	}))
}
