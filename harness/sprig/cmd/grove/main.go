// Command grove runs and supervises a FLEET of sprig agents at once. On a real
// terminal it's an interactive cockpit — j/k to move, a/d to approve/deny the
// agent the envelope flagged, q to quit. Piped, it prints the live minimap and
// then the answers, so it also drops into scripts. The many-agent pain point is
// status visibility (harness-meta §3); grove answers "who needs me now" on one
// screen via state fusion. One small dep (golang.org/x/term) for raw keys.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	sprig "github.com/opendaisugi/sprig"
	fleet "github.com/opendaisugi/sprig/fleet"
	"golang.org/x/term"
)

func main() {
	gateOn := false
	gateCmd := sprig.DefaultGateCmd()
	var tasks []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--gate":
			gateOn = true
		case args[i] == "--gate-cmd" && i+1 < len(args):
			gateCmd = args[i+1]
			i++
		case args[i] == "--help" || args[i] == "-h":
			usage()
			return
		default:
			tasks = append(tasks, args[i])
		}
	}
	if len(tasks) == 0 {
		usage()
		os.Exit(1)
	}

	f := fleet.New()
	for i, t := range tasks {
		f.Add(fmt.Sprintf("a%d", i+1), t)
	}
	var gate sprig.Gate = sprig.AllowAll{}
	if gateOn {
		// EscalatingGate: safe calls run; a refused call pauses that agent and
		// waits for you to approve/deny from the cockpit.
		gate = nil // set per-job below
	}

	done := make(chan struct{})
	go func() {
		f.Run(func(job *fleet.Job) *sprig.Agent {
			g := gate
			if gateOn {
				g = fleet.EscalatingGate{Base: sprig.DaisugiGate{Command: strings.Fields(gateCmd), Timeout: 10 * time.Second}, Fleet: f, JobID: job.ID}
			}
			return &sprig.Agent{Model: sprig.NewClaudeCodeModel(), Exec: sprig.NewExecutor(sprig.DefaultTools(), g), MaxTurns: 20}
		})
		close(done)
	}()

	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		interactive(f, done)
	} else {
		piped(f, done)
	}
}

// interactive: raw single-key cockpit — j/k move, a/d rule, q quit.
func interactive(f *fleet.Fleet, done <-chan struct{}) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		piped(f, done)
		return
	}
	defer term.Restore(fd, old)

	keys := make(chan byte, 16)
	go func() {
		buf := make([]byte, 1)
		for {
			if n, _ := os.Stdin.Read(buf); n > 0 {
				keys <- buf[0]
			}
		}
	}()

	c := &fleet.Cockpit{F: f}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	draw := func() {
		frame := strings.ReplaceAll(fleet.RenderCockpit(f.Snapshot(), c.Sel, true), "\n", "\r\n")
		fmt.Print("\x1b[H\x1b[2J" + frame)
	}
	for {
		draw()
		select {
		case <-done:
			term.Restore(fd, old)
			answers(f)
			return
		case k := <-keys:
			if c.Key(k) {
				return // q
			}
		case <-ticker.C:
		}
	}
}

// piped: no terminal — print the live minimap, then the answers to stdout.
func piped(f *fleet.Fleet, done <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		fmt.Print(fleet.RenderMinimap(f.Snapshot(), false))
		select {
		case <-done:
			fmt.Print(fleet.RenderMinimap(f.Snapshot(), false))
			answers(f)
			return
		case <-ticker.C:
		}
	}
}

func answers(f *fleet.Fleet) {
	for _, j := range f.Snapshot() {
		fmt.Printf("\n=== %s: %s ===\n%s\n", j.ID, j.State, j.Answer)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: grove [--gate] "task 1" "task 2" ...`)
	fmt.Fprintln(os.Stderr, "run many sprig agents at once; on a terminal, j/k move · a/d approve/deny · q quit.")
	fmt.Fprintln(os.Stderr, `example: grove "add a test for parse_url" "fix the --json flag"`)
}
