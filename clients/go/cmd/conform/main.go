// Command conform speaks the openDaisugi conformance wire protocol
// (docs/spec/conformance.md): one case JSON per line on stdin, one verdict
// JSON per line on stdout, flushed per line, logs to stderr only.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"daisugi-verify/internal/verify"
)

// Corpus lines reach ~1MB (long shell transcripts). bufio.Scanner's default
// 64KB buffer truncates those silently; use bufio.Reader.ReadString instead,
// which has no such cap.
const outputBufSize = 1 << 20

func main() {
	in := bufio.NewReaderSize(os.Stdin, outputBufSize)
	out := bufio.NewWriterSize(os.Stdout, outputBufSize)
	defer out.Flush()

	for {
		line, err := in.ReadString('\n')
		trimmed := line
		if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\n' {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if len(trimmed) > 0 {
			handleLine(out, trimmed)
			out.Flush()
		}
		if err != nil {
			break // EOF or read error — nothing more to answer.
		}
	}
}

func handleLine(out *bufio.Writer, line string) {
	var c verify.Case
	if err := json.Unmarshal([]byte(line), &c); err != nil {
		emitError(out, "", fmt.Sprintf("malformed case JSON: %v", err))
		return
	}
	if c.V > 1 {
		emitError(out, c.ID, fmt.Sprintf("unsupported conformance version v=%d (client speaks v<=1)", c.V))
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// A panic anywhere in the parser/verifier on one of 13k arbitrary
			// transcript strings must not kill the whole run: one case
			// becomes an error verdict (counted as a mismatch), the stream
			// continues.
			emitError(out, c.ID, fmt.Sprintf("panic: %v", r))
		}
	}()
	switch c.Kind {
	case "verify":
		handleVerify(out, c)
	case "decompose":
		handleDecompose(out, c)
	default:
		emitError(out, c.ID, fmt.Sprintf("unknown kind %q", c.Kind))
	}
}

func handleDecompose(out *bufio.Writer, c verify.Case) {
	d := verify.DecomposeCommand(c.Command)
	v := verify.DecomposeVerdict{ID: c.ID, OK: d.OK}
	if d.OK {
		v.Heads = d.Heads
		v.Commands = d.Commands
		v.Reads = verify.SortedCopy(d.Reads)
		v.Writes = verify.SortedCopy(d.Writes)
	}
	writeJSON(out, v)
}

func handleVerify(out *bufio.Writer, c verify.Case) {
	plan, err := verify.ParsePlan(c.Plan)
	if err != nil {
		emitError(out, c.ID, err.Error())
		return
	}
	env, err := verify.ParseEnvelope(c.Envelope)
	if err != nil {
		emitError(out, c.ID, err.Error())
		return
	}
	res := verify.Verify(plan, env, verify.VerifyOptions{
		Strict:      c.Options.Strict,
		Z3TimeoutMs: c.Options.Z3Timeout(),
	})
	writeJSON(out, verify.VerifyVerdict{
		ID:         c.ID,
		OK:         res.OK,
		Violations: verify.ToWireViolations(res.Violations),
	})
}

func emitError(out *bufio.Writer, id, msg string) {
	writeJSON(out, verify.ErrorVerdict{ID: id, Error: msg})
}

func writeJSON(out *bufio.Writer, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		// Marshaling our own verdict struct should never fail; if it does,
		// still emit SOMETHING so the stream doesn't silently lose a line.
		fmt.Fprintf(os.Stderr, "conform: marshal error: %v\n", err)
		out.WriteString(`{"error":"internal marshal failure"}` + "\n")
		return
	}
	out.Write(b)
	out.WriteByte('\n')
}
