// Command weave runs a workflow file — a small JSON graph of steps, each a sprig
// agent task, each gated by the envelope. Control flow is deterministic (zero
// model tokens); the model only fills each step. clig.dev: results to stdout,
// logs to stderr, real exit codes, --json for scripts.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	sprig "github.com/opendaisugi/sprig"
	weave "github.com/opendaisugi/sprig/weave"
)

func main() {
	args := os.Args[1:]
	gateOn := false
	gateCmd := "python -m opendaisugi.gate --mode enforce"
	jsonOut := false
	var file string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "run":
			// optional verb
		case args[i] == "--gate":
			gateOn = true
		case args[i] == "--gate-cmd" && i+1 < len(args):
			gateCmd = args[i+1]
			i++
		case args[i] == "--json":
			jsonOut = true
		case args[i] == "--help" || args[i] == "-h":
			usage()
			return
		default:
			file = args[i]
		}
	}
	if file == "" {
		usage()
		os.Exit(1)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "weave:", err)
		os.Exit(1)
	}
	wf, err := weave.Parse(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err) // the grammar-is-a-fence errors are actionable
		os.Exit(1)
	}

	var gate sprig.Gate = sprig.AllowAll{}
	if gateOn {
		gate = sprig.DaisugiGate{Command: strings.Fields(gateCmd), Timeout: 10 * time.Second}
	}
	fmt.Fprintf(os.Stderr, "weave: running %q (%d steps)\n", wf.Name, len(wf.Steps))

	results, err := wf.Run(func(weave.Step) *sprig.Agent {
		return &sprig.Agent{Model: sprig.NewClaudeCodeModel(), Exec: sprig.NewExecutor(sprig.DefaultTools(), gate), MaxTurns: 20}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "weave:", err)
		os.Exit(1)
	}

	if jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(results)
	} else {
		for _, r := range results {
			status := "ok"
			if r.Err != nil {
				status = "FAILED: " + r.Err.Error()
			}
			fmt.Printf("=== %s [%s] ===\n%s\n\n", r.StepID, status, r.Output)
		}
	}
	for _, r := range results { // non-zero exit if any step failed
		if r.Err != nil {
			os.Exit(1)
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: weave run <workflow.json> [--gate] [--json]")
	fmt.Fprintln(os.Stderr, "run a JSON graph of gated agent steps in dependency order.")
}
