// Command sprig-hook is a Claude Code PreToolUse hook that gates the driver's
// OWN native tools with a sprig.Gate. This is Design D: point a driver's
// PreToolUse hook at this binary and every read/write/edit/bash it attempts is
// verified against the openDaisugi envelope before it runs, fail-closed.
//
// Wire it in settings.json, e.g.:
//
//	{"hooks":{"PreToolUse":[{"matcher":".*","hooks":[
//	  {"type":"command","command":"sprig-hook --gate-cmd 'daisugi gate --mode enforce'"}]}]}}
//
// It reads the {tool_name, tool_input} payload on stdin and prints the decision.
package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	sprig "github.com/opendaisugi/sprig"
	"github.com/opendaisugi/sprig/hook"
)

func main() {
	gateCmd := "python -m opendaisugi.gate --mode enforce"
	// Default 30s (not 10): the gate is import-dominated (Z3) and a cold first call
	// under load can take several seconds; fail-closed still holds on a real hang.
	// It's a flag, not a constant, because 30s is right for a cold box and wasteful
	// on a warm one — the caller decides.
	gateTimeout := 30 * time.Second
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--gate-cmd" && i+1 < len(args):
			gateCmd = args[i+1]
			i++
		case args[i] == "--gate-timeout" && i+1 < len(args):
			if s, err := strconv.Atoi(args[i+1]); err == nil && s > 0 {
				gateTimeout = time.Duration(s) * time.Second
			}
			i++
		}
	}

	input, _ := io.ReadAll(os.Stdin)
	gate := sprig.DaisugiGate{Command: strings.Fields(gateCmd), Timeout: gateTimeout}
	out, allow := hook.Decide(input, gate)
	os.Stdout.Write(out)
	if !allow {
		// Exit 2 is an UNCONDITIONAL block. A JSON deny alone is a permission-flow
		// decision, which --permission-mode bypassPermissions skips; exit 2 does not.
		os.Exit(2)
	}
}
