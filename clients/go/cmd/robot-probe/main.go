//go:build mujoco

// Command robot-probe is a test instrument for clients/robotics_compare.py:
// the robotics executors on the robotics cases, one result line per case.
// It is not shipped.
package main

import (
	"os"

	"daisugi-verify/internal/cli"
)

func main() {
	os.Exit(cli.RobotProbe(&cli.Env{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Environ: os.Environ(),
	}))
}
