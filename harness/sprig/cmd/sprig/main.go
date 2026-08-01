// Command sprig is a minimal, automatable coding-agent harness with a fail-closed
// gate. This is the thin binary; the harness lives in package sprig.
package main

import (
	"os"

	sprig "github.com/opendaisugi/sprig"
)

var version = "0.0.1"

func main() {
	cli := &sprig.CLI{
		Version: version,
		// Default backend: claude -p (subscription, no API key — respects the
		// exposure freeze). SPRIG_BACKEND=api opts into the direct Messages API
		// (Design E): a minimal prompt, but it needs ANTHROPIC_API_KEY.
		NewModel: func() (sprig.Model, error) {
			if os.Getenv("SPRIG_BACKEND") == "api" {
				return sprig.NewAPIModel()
			}
			return sprig.NewClaudeCodeModel(), nil
		},
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
