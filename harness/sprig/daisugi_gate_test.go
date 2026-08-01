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
