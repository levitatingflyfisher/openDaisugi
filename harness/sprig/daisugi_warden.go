package sprig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// DefaultWardenCmd is the --warden-cmd default every sprig entry point (cli.go
// and each cmd/ binary) uses: the Go daisugi CLI's own warden. It carries no
// --mode, so the installed mode decides: audit records and blocks nothing,
// enforce denies. There is no Python fallback — daisugi answers this natively,
// so nothing at run time needs Python. When daisugi is not on PATH, this
// string still names it, so Check below fails closed with a clear reason
// instead of silently allowing.
func DefaultWardenCmd() string {
	return "daisugi warden check"
}

// WardenOffNotice is the one line a run prints to stderr when --warden=false
// turns the warden off. who is the binary's name.
func WardenOffNotice(who string) string {
	return who + ": the warden is off (--warden=false): nothing is checked or recorded"
}

// DaisugiWarden is the differentiator: it verifies each proposed tool call against
// an openDaisugi envelope BEFORE it runs, as a separate process. The design
// research settled the clean boundary (AgentSpec, arXiv 2606.26057, P1-P4): an
// OUT-OF-PROCESS control on the only path the agent can route around, fail-closed
// at request and system levels, with an externalized policy. This is exactly
// openDaisugi's PreToolUse-hook contract — so DaisugiWarden just shells out to it.
//
//   - P1 process separation  : the verdict comes from a separate process.
//   - P2 pre-action, only path: the Executor routes EVERY call through Check first.
//   - P3 fail-closed          : nonzero exit, exec error, or timeout all DENY.
//   - P4 externalized policy  : the envelope lives in openDaisugi, not in sprig.
//
// Command is the warden invocation, e.g. []string{"daisugi","warden","check"}
// or []string{"python","-m","opendaisugi.warden"}. It reads a hook payload on stdin
// and signals allow (exit 0) vs deny (nonzero) — openDaisugi's existing contract.
type DaisugiWarden struct {
	Command []string
	Timeout time.Duration
	// SessionID names the run on every payload, so the journal keeps each
	// run apart. Empty sends none.
	SessionID string
}

// wardenVocabulary maps sprig's lowercase tool names to the names openDaisugi's
// envelope classifier (_TOOL_TYPE_MAP) recognizes. The warden denies unknown tool
// names by default, and sprig's read/write/edit/bash are NOT in its map — so a
// sprig call must speak the envelope's vocabulary or every call is refused. Names
// already recognized (e.g. Claude's native "Write" from the hook path) pass
// through unchanged; the envelope's tool_input keys already accept both path and
// file_path, cmd and command, so only the name needs translating.
var wardenVocabulary = map[string]string{
	"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash",
	// spawn is an MCP-style tool to the warden: the envelope admits it only
	// when mcp_allowlist names sprig/spawn.
	"spawn": "mcp__sprig__spawn",
}

func wardenToolName(name string) string {
	if mapped, ok := wardenVocabulary[name]; ok {
		return mapped
	}
	return name
}

func (g DaisugiWarden) Check(call ToolCall) Verdict {
	if len(g.Command) == 0 {
		return Verdict{Allow: false, Reason: "no warden command configured (fail-closed)"}
	}
	body := map[string]any{
		"tool_name":  wardenToolName(call.Name),
		"tool_input": call.Input,
	}
	// sprig's tools run in this process's directory, so a relative path in
	// a call names a file there. The warden places it from this cwd.
	if wd, err := os.Getwd(); err == nil {
		body["cwd"] = wd
	}
	if g.SessionID != "" {
		body["session_id"] = g.SessionID
	}
	payload, _ := json.Marshal(body)

	timeout := g.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, g.Command[0], g.Command[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = ChildEnv("")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// A warden that runs past its time is killed with its whole group.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err := runInGroup(cmd)

	if ctx.Err() == context.DeadlineExceeded {
		return Verdict{Allow: false, Reason: "warden timed out — fail-closed"}
	}
	if err == nil {
		return Verdict{Allow: true} // exit 0 = verified, allowed
	}
	if g.Command[0] == "daisugi" && errors.Is(err, exec.ErrNotFound) {
		// The default names the bare "daisugi" binary; LookPath found nothing
		// on PATH. Say so plainly rather than surfacing a raw exec error —
		// still fail-closed, just with a reason a human can act on.
		return Verdict{Allow: false, Reason: "daisugi is not on PATH, so every tool call is denied. " +
			"Install it: scripts/install.sh, or see the README quick start."}
	}
	// Any nonzero exit or exec error is a refusal. Surface the warden's own words.
	reason := wardenReason(errb.String(), out.String())
	if reason == "" {
		reason = "refused by the warden (" + err.Error() + ")"
	}
	return Verdict{Allow: false, Reason: reason}
}

// wardenReason picks the warden's verdict line out of its output, skipping the
// `python -m` import RuntimeWarning that precedes it. Falls back to the last
// non-empty stderr line, then stdout.
func wardenReason(stderr, stdout string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.Contains(l, "openDaisugi warden:") {
			return l
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return strings.TrimSpace(stdout)
}
