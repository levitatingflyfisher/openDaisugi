// Command sprig-mcp serves sprig's four tools as an MCP server over stdio, so a
// driver like `claude` can call them NATIVELY while sprig's warden still rules
// every call. This is Design C — the pi-claude-bridge pattern: the model drives
// and emits real tool calls; sprig executes them behind the envelope.
//
// Point a driver at it with --mcp-config, e.g.:
//
//	{"mcpServers":{"sprig":{"type":"stdio","command":"sprig-mcp","args":[]}}}
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
	wardenOn := true
	wardenCmd := sprig.DefaultWardenCmd()
	// Default 30s: the warden is import-dominated (Z3); a cold first call under load
	// can take several seconds. A flag, not a constant, so a warm box can tighten it.
	wardenTimeout := 30 * time.Second
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--warden" || args[i] == "--warden=true":
			wardenOn = true
		case args[i] == "--warden=false":
			wardenOn = false
		case args[i] == "--warden-cmd" && i+1 < len(args):
			wardenCmd = args[i+1]
			i++
		case args[i] == "--warden-timeout" && i+1 < len(args):
			if s, err := strconv.Atoi(args[i+1]); err == nil && s > 0 {
				wardenTimeout = time.Duration(s) * time.Second
			}
			i++
		case args[i] == "--help" || args[i] == "-h":
			usage()
			return
		}
	}

	var warden sprig.Warden = sprig.AllowAll{}
	if wardenOn {
		// The whole point of the bridge: even though CLAUDE drives, every call it
		// makes is verified against the envelope before sprig runs it. Fail-closed
		// still holds on a real hang (see wardenTimeout).
		warden = sprig.DaisugiWarden{Command: strings.Fields(wardenCmd), Timeout: wardenTimeout}
	} else {
		fmt.Fprintln(os.Stderr, sprig.WardenOffNotice("sprig-mcp"))
	}

	srv := mcp.NewServer(sprig.NewExecutor(sprig.DefaultTools(), warden))
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sprig-mcp:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sprig-mcp [--warden=false] [--warden-cmd \"<cmd>\"]")
	fmt.Fprintln(os.Stderr, "serve sprig's read/write/edit/bash tools to an MCP driver over stdio.")
	fmt.Fprintln(os.Stderr, "the warden checks every call by default, fail-closed; --warden=false turns it off.")
}
