// Command sprig-mcp serves sprig's four tools as an MCP server over stdio, so a
// driver like `claude` can call them NATIVELY while sprig's gate still rules
// every call. This is Design C — the pi-claude-bridge pattern: the model drives
// and emits real tool calls; sprig executes them behind the envelope.
//
// Point a driver at it with --mcp-config, e.g.:
//
//	{"mcpServers":{"sprig":{"type":"stdio","command":"sprig-mcp","args":["--gate"]}}}
//	claude -p --mcp-config cfg.json --allowedTools "mcp__sprig__bash" "run: echo hi"
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	sprig "github.com/opendaisugi/sprig"
	"github.com/opendaisugi/sprig/mcp"
)

func main() {
	gateOn := false
	gateCmd := sprig.DefaultGateCmd()
	// Default 30s: the gate is import-dominated (Z3); a cold first call under load
	// can take several seconds. A flag, not a constant, so a warm box can tighten it.
	gateTimeout := 30 * time.Second
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--gate":
			gateOn = true
		case args[i] == "--gate-cmd" && i+1 < len(args):
			gateCmd = args[i+1]
			i++
		case args[i] == "--gate-timeout" && i+1 < len(args):
			if s, err := strconv.Atoi(args[i+1]); err == nil && s > 0 {
				gateTimeout = time.Duration(s) * time.Second
			}
			i++
		case args[i] == "--help" || args[i] == "-h":
			usage()
			return
		}
	}

	var gate sprig.Gate = sprig.AllowAll{}
	if gateOn {
		// The whole point of the bridge: even though CLAUDE drives, every call it
		// makes is verified against the envelope before sprig runs it. Fail-closed
		// still holds on a real hang (see gateTimeout).
		gate = sprig.DaisugiGate{Command: strings.Fields(gateCmd), Timeout: gateTimeout}
	}

	srv := mcp.NewServer(sprig.NewExecutor(sprig.DefaultTools(), gate))
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sprig-mcp:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sprig-mcp [--gate] [--gate-cmd \"<cmd>\"]")
	fmt.Fprintln(os.Stderr, "serve sprig's read/write/edit/bash tools to an MCP driver over stdio.")
	fmt.Fprintln(os.Stderr, "--gate verifies every call against the openDaisugi envelope, fail-closed.")
}
