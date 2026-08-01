package verify

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Z3Client is a single persistent "z3 -in" subprocess (docs/spec/
// conformance.md: "emitting SMT-LIB2 text and invoking a solver binary...
// never by binding a solver API"). One process for the whole run — not one
// per query — is what keeps the Full-profile tail from dominating the
// bench; each query is isolated with (push 1)/(pop 1) so declarations never
// leak between logically-unrelated checks.
type Z3Client struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	closed bool
}

// NewZ3Client starts "z3 -in". Returns an error if the z3 binary isn't on
// PATH — callers treat that as "Full profile unavailable" (fail closed,
// per PORTING-NOTES: an opted-in-but-unusable capability rejects rather
// than silently passing).
func NewZ3Client() (*Z3Client, error) {
	cmd := exec.Command("z3", "-in")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting z3 -in: %w", err)
	}
	return &Z3Client{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// z3QueryCounter gives every query a unique resync marker — see CheckSat.
var z3QueryCounter uint64

// CheckSat sends `smt2` (declarations + assertions, no leading/trailing
// check-sat) wrapped in a push/pop scope with the given timeout, then reads
// z3's response to "(check-sat)": "sat", "unsat", or "unknown".
//
// z3 -in does NOT abort a session on a malformed command — e.g. two
// `declare-const`s of the same name in one scope print an `(error ...)`
// line and then KEEP GOING, so a single buggy query can silently shift the
// stdout read cursor by one line for the rest of the process's life,
// corrupting every later, otherwise-correct query. Guard against that
// class of bug structurally: emit a unique `(echo "...")` marker after
// each query and read (and discard) any interleaved lines — including
// `(error ...)` ones, surfaced as part of the returned error — until that
// marker reappears, so the stream is always resynchronized before the next
// query starts.
func (c *Z3Client) CheckSat(smt2 string, timeoutMs int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", fmt.Errorf("z3 client closed")
	}
	z3QueryCounter++
	marker := fmt.Sprintf("DAISUGI-MARK-%d", z3QueryCounter)

	var b strings.Builder
	fmt.Fprintf(&b, "(push 1)\n(set-option :timeout %d)\n", timeoutMs)
	b.WriteString(smt2)
	b.WriteString("\n(check-sat)\n")
	fmt.Fprintf(&b, "(pop 1)\n(echo %q)\n", marker)
	if _, err := io.WriteString(c.stdin, b.String()); err != nil {
		return "", fmt.Errorf("writing to z3: %w", err)
	}

	var result string
	var stray []string
	for {
		line, err := c.stdout.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed == marker {
			break
		}
		switch trimmed {
		case "sat", "unsat", "unknown":
			result = trimmed
		case "":
			// blank line — ignore
		default:
			stray = append(stray, trimmed)
		}
		if err != nil {
			return "", fmt.Errorf("reading from z3 (stream desynced, never saw marker %s): %w", marker, err)
		}
	}
	if result == "" {
		return "", fmt.Errorf("z3 produced no sat/unsat/unknown line (stray output: %v)", stray)
	}
	if len(stray) > 0 {
		return "", fmt.Errorf("z3 emitted unexpected output alongside %q: %v", result, stray)
	}
	return result, nil
}

func (c *Z3Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.stdin.Close()
	c.cmd.Wait()
}

// smtQuoteString escapes a Go string for SMT-LIB2 string-literal syntax:
// double the embedded double-quotes (SMT-LIB2's own escape), everything
// else passes through as UTF-8 (Z3's String sort is Unicode code points).
func smtQuoteString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
