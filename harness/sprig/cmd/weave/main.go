// Command weave runs a workflow file — a small JSON graph of steps, each a sprig
// agent task, each checked by the envelope. Control flow is deterministic (zero
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
	wardenOn := true
	wardenCmd := sprig.DefaultWardenCmd()
	jsonOut := false
	var file string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "run":
			// optional verb
		case args[i] == "--warden" || args[i] == "--warden=true":
			wardenOn = true
		case args[i] == "--warden=false":
			wardenOn = false
		case args[i] == "--warden-cmd" && i+1 < len(args):
			wardenCmd = args[i+1]
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

	var warden sprig.Warden = sprig.AllowAll{}
	if wardenOn {
		warden = sprig.DaisugiWarden{Command: strings.Fields(wardenCmd), Timeout: 10 * time.Second}
	} else {
		fmt.Fprintln(os.Stderr, sprig.WardenOffNotice("weave"))
	}
	fmt.Fprintf(os.Stderr, "weave: running %q (%d steps)\n", wf.Name, len(wf.Steps))

	results, err := wf.Run(func(weave.Step) *sprig.Agent {
		return &sprig.Agent{Model: sprig.NewClaudeCodeModel(), Exec: sprig.NewExecutor(sprig.DefaultTools(), warden), MaxTurns: 20}
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
	fmt.Fprintln(os.Stderr, "usage: weave run <workflow.json> [--warden=false] [--warden-cmd \"<cmd>\"] [--json]")
	fmt.Fprintln(os.Stderr, "run a JSON graph of checked agent steps in dependency order.")
	fmt.Fprintln(os.Stderr, "the warden checks every tool call by default; --warden=false turns it off.")
}
