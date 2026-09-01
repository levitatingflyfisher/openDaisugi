package sprig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stub writes a tiny shell script standing in for `daisugi gate`, so we test the
// out-of-process boundary without needing a real openDaisugi + Z3 install.
func stub(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gate.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDaisugiGateAllowsOnExitZero(t *testing.T) {
	g := DaisugiGate{Command: []string{stub(t, "exit 0")}, Timeout: 2 * time.Second}
	if v := g.Check(ToolCall{Name: "bash", Input: map[string]any{"cmd": "ls"}}); !v.Allow {
		t.Fatalf("exit 0 must allow, got %+v", v)
	}
}

func TestDaisugiGateDeniesOnNonZeroAndSurfacesReason(t *testing.T) {
	g := DaisugiGate{Command: []string{stub(t, "echo 'compound shell rejected' 1>&2; exit 2")}, Timeout: 2 * time.Second}
	v := g.Check(ToolCall{Name: "bash", Input: map[string]any{"cmd": "a && b"}})
	if v.Allow {
		t.Fatal("exit 2 must deny")
	}
	if v.Reason == "" {
		t.Fatal("a denial must carry the gate's reason")
	}
}

func TestDaisugiGateFailsClosedOnTimeout(t *testing.T) {
	// P3: a hung gate must DENY, not hang the agent or fall open.
	g := DaisugiGate{Command: []string{stub(t, "sleep 5")}, Timeout: 150 * time.Millisecond}
	if v := g.Check(ToolCall{Name: "bash"}); v.Allow {
		t.Fatal("a hung gate must fail closed (deny)")
	}
}

func TestDaisugiGateFailsClosedOnMissingCommand(t *testing.T) {
	// A missing/broken gate binary must DENY, never silently allow.
	g := DaisugiGate{Command: []string{"/no/such/gate/binary"}, Timeout: time.Second}
	if v := g.Check(ToolCall{Name: "bash"}); v.Allow {
		t.Fatal("a missing gate must fail closed (deny)")
	}
}

func TestDaisugiGateNoCommandFailsClosed(t *testing.T) {
	if v := (DaisugiGate{}).Check(ToolCall{Name: "bash"}); v.Allow {
		t.Fatal("no configured command must fail closed")
	}
}

// DefaultGateCmd is what every sprig entry point (cli.go and each cmd/
// binary: grove, sprig-hook, sprig-mcp, weave) uses as its --gate-cmd
// default. There is no Python fallback: it always names the Go daisugi
// CLI's own gate.
func TestDefaultGateCmdIsDaisugiGateCheckEnforce(t *testing.T) {
	got := DefaultGateCmd()
	want := "daisugi gate check --mode enforce"
	if got != want {
		t.Fatalf("DefaultGateCmd() = %q, want %q", got, want)
	}
	if strings.Contains(got, "python") {
		t.Fatalf("DefaultGateCmd() must never name python: %q", got)
	}
}

// When the default's bare "daisugi" does not resolve on PATH, the denial
// must say so plainly and point at how to install it, not surface a raw
// exec error. A custom PATH (an empty temp dir) stands in for "not
// installed" without touching the real PATH or the real daisugi.
func TestDaisugiGateNamesDaisugiWhenMissingFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	g := DaisugiGate{Command: strings.Fields(DefaultGateCmd()), Timeout: time.Second}
	v := g.Check(ToolCall{Name: "bash", Input: map[string]any{"cmd": "ls"}})
	if v.Allow {
		t.Fatal("a missing daisugi binary must fail closed (deny)")
	}
	want := "daisugi is not on PATH, so every tool call is denied. " +
		"Install it: scripts/install.sh, or see the README quick start."
	if v.Reason != want {
		t.Fatalf("reason = %q, want %q", v.Reason, want)
	}
}

// A missing command that is NOT the bare "daisugi" name (a path, or a
// custom --gate-cmd override naming some other binary) keeps the generic
// fail-closed reason — the specific daisugi message must not leak onto an
// unrelated missing command.
func TestDaisugiGateMissingOtherCommandKeepsGenericReason(t *testing.T) {
	g := DaisugiGate{Command: []string{"/no/such/gate/binary"}, Timeout: time.Second}
	v := g.Check(ToolCall{Name: "bash"})
	if v.Allow {
		t.Fatal("a missing gate must fail closed (deny)")
	}
	if strings.Contains(v.Reason, "daisugi is not on PATH") {
		t.Fatalf("a non-daisugi missing command must not get the daisugi-specific reason: %q", v.Reason)
	}
}

func TestGateReasonStripsImportNoise(t *testing.T) {
	// `python -m opendaisugi.gate` prints a RuntimeWarning before its verdict; the
	// model should see the verdict, not the noise.
	stderr := "<frozen runpy>:128: RuntimeWarning: 'opendaisugi.gate' found in sys.modules...\n" +
		"openDaisugi gate: DENIED — permissions: requires shell but envelope forbids it"
	got := gateReason(stderr, "")
	if !strings.HasPrefix(got, "openDaisugi gate: DENIED") {
		t.Fatalf("reason should be the gate's verdict line, got %q", got)
	}
	if strings.Contains(got, "RuntimeWarning") {
		t.Fatalf("reason must not carry import noise: %q", got)
	}
}

func TestGateToolNameSpeaksTheEnvelopeVocabulary(t *testing.T) {
	// openDaisugi's envelope classifier denies unknown tool names by default and
	// does NOT recognize sprig's lowercase read/write/edit/bash. Claude's native
	// names (Write/Bash, from the hook path) are already recognized and pass through.
	for in, want := range map[string]string{
		"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash",
		"Write": "Write", "Bash": "Bash",
		"mcp__sprig__x": "mcp__sprig__x",
	} {
		if got := gateToolName(in); got != want {
			t.Fatalf("gateToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGateVocabularyCoversEveryTool(t *testing.T) {
	// A tool with no translation reaches the openDaisugi gate under its lowercase
	// name and is denied unknown-by-default — a silent, confusing outage. Fail at
	// build time instead: every DefaultTools() name must have a gateVocabulary entry.
	for name := range DefaultTools() {
		if _, ok := gateVocabulary[name]; !ok {
			t.Fatalf("tool %q has no gateVocabulary entry — the envelope will deny it by default; add %q to gateVocabulary", name, name)
		}
	}
}

func TestDaisugiGateSendsTranslatedToolNameToTheEnvelope(t *testing.T) {
	// End to end: sprig's lowercase 'write' must reach the gate as 'Write' or the
	// real envelope refuses every call. This stub allows ONLY on the translated name.
	g := DaisugiGate{Command: []string{stub(t, `grep -q '"tool_name":"Write"' && exit 0 || exit 1`)}, Timeout: 2 * time.Second}
	if v := g.Check(ToolCall{Name: "write", Input: map[string]any{"path": "/tmp/x"}}); !v.Allow {
		t.Fatalf("sprig 'write' must reach the gate as 'Write', got %+v", v)
	}
}

// The gate places a relative path from the call's working directory, so
// every payload names sprig's own. The stub allows only when stdin
// carries the absolute cwd.
func TestDaisugiGateSendsItsWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "payload.json")
	g := DaisugiGate{Command: []string{stub(t, "cat > "+out)}, Timeout: 2 * time.Second}
	if v := g.Check(ToolCall{Name: "write", Input: map[string]any{"path": "fizz.py"}}); !v.Allow {
		t.Fatalf("stub gate should allow, got %+v", v)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cwd":"`+wd+`"`) {
		t.Fatalf("payload does not name the working directory %s: %s", wd, b)
	}
}

// A gate that knows the session names it on every payload, so daisugi's
// journal files each run under its own id.
func TestDaisugiGateSendsItsSessionID(t *testing.T) {
	out := filepath.Join(t.TempDir(), "payload.json")
	g := DaisugiGate{Command: []string{stub(t, "cat > "+out)}, Timeout: 2 * time.Second, SessionID: "run-7"}
	g.Check(ToolCall{Name: "bash", Input: map[string]any{"cmd": "ls"}})
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), `"session_id":"run-7"`) {
		t.Fatalf("payload does not name the session: %s", b)
	}
}
