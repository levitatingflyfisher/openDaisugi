// Command sprig-hook is a Claude Code PreToolUse hook that checks the driver's
// OWN native tools with a sprig.Warden. This is Design D: point a driver's
// PreToolUse hook at this binary and every read/write/edit/bash it attempts is
// verified against the openDaisugi envelope before it runs, fail-closed.
//
// Wire it in settings.json, e.g.:
//
//	{"hooks":{"PreToolUse":[{"matcher":".*","hooks":[
//	  {"type":"command","command":"sprig-hook --warden-cmd 'daisugi warden check'"}]}]}}
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
	wardenCmd := sprig.DefaultWardenCmd()
	// Default 30s (not 10): the warden is import-dominated (Z3) and a cold first call
	// under load can take several seconds; fail-closed still holds on a real hang.
	// It's a flag, not a constant, because 30s is right for a cold box and wasteful
	// on a warm one — the caller decides.
	wardenTimeout := 30 * time.Second
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--warden-cmd" && i+1 < len(args):
			wardenCmd = args[i+1]
			i++
		case args[i] == "--warden-timeout" && i+1 < len(args):
			if s, err := strconv.Atoi(args[i+1]); err == nil && s > 0 {
				wardenTimeout = time.Duration(s) * time.Second
			}
			i++
		}
	}

	input, _ := io.ReadAll(os.Stdin)
	warden := sprig.DaisugiWarden{Command: strings.Fields(wardenCmd), Timeout: wardenTimeout}
	out, allow := hook.Decide(input, warden)
	os.Stdout.Write(out)
	if !allow {
		// Exit 2 is an UNCONDITIONAL block. A JSON deny alone is a permission-flow
		// decision, which --permission-mode bypassPermissions skips; exit 2 does not.
		os.Exit(2)
	}
}
