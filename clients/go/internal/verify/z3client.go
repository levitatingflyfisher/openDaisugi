package verify

import (
	"bufio"
	"fmt"
	"strings"
	"sync"
)

// Z3Client runs the Full profile's SMT-LIB2 queries in the Z3 this binary
// links (clients/go/internal/z3), through Z3's own command interpreter:
// the same text `z3 -in` would read, with no second program. The
// interpreter keeps its state for the whole run, and each query is
// isolated with (push 1)/(pop 1), so declarations never leak between
// logically unrelated checks.
type Z3Client struct {
	// eval runs the commands and returns what they print.
	eval func(string) (string, error)
	mu   sync.Mutex
}

// z3QueryCounter gives every query a unique resync marker — see CheckSat.
var z3QueryCounter uint64

// CheckSat sends `smt2` (declarations + assertions, no leading/trailing
// check-sat) wrapped in a push/pop scope with the given timeout, then reads
// Z3's response to "(check-sat)": "sat", "unsat", or "unknown".
//
// Z3's interpreter does NOT stop on a malformed command: two
// `declare-const`s of the same name in one scope print an `(error ...)`
// line and then KEEP GOING. So each query ends with a unique `(echo "...")`
// marker, and every line before the marker other than the answer (an
// `(error ...)` one included) is surfaced as part of the returned error.
func (c *Z3Client) CheckSat(smt2 string, timeoutMs int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	z3QueryCounter++
	marker := fmt.Sprintf("DAISUGI-MARK-%d", z3QueryCounter)

	var b strings.Builder
	fmt.Fprintf(&b, "(push 1)\n(set-option :timeout %d)\n", timeoutMs)
	b.WriteString(smt2)
	b.WriteString("\n(check-sat)\n")
	fmt.Fprintf(&b, "(pop 1)\n(echo %q)\n", marker)
	out, err := c.eval(b.String())
	if err != nil {
		// A failed command can leave the scope pushed; start clean.
		_, _ = c.eval("(reset)")
		return "", fmt.Errorf("z3: %w", err)
	}
	stdout := bufio.NewReader(strings.NewReader(out))

	var result string
	var stray []string
	for {
		line, err := stdout.ReadString('\n')
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
			return "", fmt.Errorf("reading from z3 (never saw marker %s): %w", marker, err)
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

// smtQuoteString escapes a Go string for SMT-LIB2 string-literal syntax:
// double the embedded double-quotes (SMT-LIB2's own escape), everything
// else passes through as UTF-8 (Z3's String sort is Unicode code points).
func smtQuoteString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
