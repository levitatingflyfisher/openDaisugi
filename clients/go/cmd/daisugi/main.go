// Command daisugi is the openDaisugi CLI in one Go binary: the warden
// commands (check, init, register, arm, disarm, status, report, settings,
// proposals, serve), the warden half of install, and the pathway store
// (pathways list, show, stats, delete, export, import), with the flags,
// files and output of the Python CLI.
//
// The CLI never runs Python. `daisugi warden check`, the hook entry an
// installed hook runs, answers every call through internal/warden, decided
// in a child process of this binary so that no abnormal end reads as an
// allow.
package main

import (
	"os"

	"daisugi-verify/internal/cli"
)

func main() {
	// The warden's crash backstop re-runs this binary; with no path to it,
	// warden check decides in this process.
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	os.Exit(cli.Main(&cli.Env{
		Args:      os.Args[1:],
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Environ:   os.Environ(),
		WardenExe: exe,
	}))
}
