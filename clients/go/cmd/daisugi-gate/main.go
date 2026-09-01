// Command daisugi-gate answers one tool-call hook the way
// `python -m opendaisugi.gate` does, with the same flags, stdin, stdout,
// stderr, exit code and log lines. It never runs Python. A call it cannot
// decide is denied with a reason.
//
// The call is decided in a child process of this binary. Whatever ends
// that child abnormally (a fatal runtime error, a signal, a hang past the
// deadline) comes back to the host as the format's deny contract, never
// as the exit code or empty stdout a host reads as allow.
//
// DAISUGI_GATE_TRACE names a file that gets one JSON line per call saying
// whether the gate decided it (native), denied it undecided (unported),
// or denied it after the child ended abnormally (crash), and why. It
// never changes stdout, stderr or the exit code.
package main

import (
	"encoding/json"
	"io"
	"os"
	"runtime/debug"

	"daisugi-verify/internal/gate"
)

func main() {
	if f := gate.ChildPipe(os.Getenv(gate.ChildEnv)); f != nil {
		child(f)
		return
	}
	stdin, err := io.ReadAll(io.LimitReader(os.Stdin, gate.MaxPayloadBytes+1))
	if err != nil {
		stdin = nil
	}
	var res gate.Result
	if exe, err := os.Executable(); err != nil {
		res = gate.DenyResult(gate.DenyFormat(os.Args[1:]),
			"the Go gate cannot find its own binary ("+err.Error()+"), so it denies this call")
		res.Crashed = true
	} else {
		res = gate.RunGuarded(exe, os.Args[1:], stdin, gate.ChildEnviron(os.Environ()), gate.ChildDeadline(os.Args[1:]))
	}
	if trace := os.Getenv("DAISUGI_GATE_TRACE"); trace != "" {
		writeTrace(trace, res)
	}
	os.Stdout.WriteString(res.Stdout)
	os.Stderr.WriteString(res.Stderr)
	os.Exit(res.Exit)
}

// child decides the call and writes one frame for the parent, on the
// frame pipe. A runaway
// recursion stops at a quarter of the default stack limit.
func child(frame *os.File) {
	debug.SetMaxStack(256 << 20)
	stdin, err := io.ReadAll(io.LimitReader(os.Stdin, gate.MaxPayloadBytes+1))
	if err != nil {
		stdin = nil
	}
	frame.Write(gate.ChildFrame(os.Args[1:], stdin, os.Environ()))
	frame.Close()
	deny := gate.ChildDeny(os.Args[1:])
	os.Stdout.WriteString(deny.Stdout)
	os.Stderr.WriteString(deny.Stderr)
	os.Exit(deny.Exit)
}

func writeTrace(path string, res gate.Result) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	kind := "native"
	switch {
	case res.Crashed:
		kind = "crash"
	case !res.Native:
		kind = "unported"
	}
	line, _ := json.Marshal(map[string]any{"path": kind, "why": res.Why, "exit": res.Exit})
	f.Write(append(line, '\n'))
}
