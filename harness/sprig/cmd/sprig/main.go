// Command sprig is a minimal, automatable coding-agent harness with a fail-closed
// gate. This is the thin binary; the harness lives in package sprig.
package main

import (
	"os"
	"runtime/debug"

	sprig "github.com/opendaisugi/sprig"
)

// version is set at link time (-ldflags "-X main.version=0.44.0"):
// scripts/release.sh sets the release version, and scripts/install.sh the
// git describe of the checkout. A build that sets none reports the commit
// Go stamps into it (see buildVersion).
var version = ""

// buildVersion is version, else the VCS revision go build records
// (short, with -dirty for a tree with changes), else "unknown".
func buildVersion() string {
	if version != "" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

func main() {
	cli := &sprig.CLI{
		Version: buildVersion(),
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
