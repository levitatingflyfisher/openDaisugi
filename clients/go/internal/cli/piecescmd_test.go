package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bare home: every piece is off, the router too (it needs the gateway).
func TestPiecesOnABareHome(t *testing.T) {
	home := t.TempDir()
	code, out, errs := run(t, home, "", "modules", "--pieces", "--data-dir", filepath.Join(home, "dd"))
	if code != 0 {
		t.Fatalf("exit %d, %s", code, errs)
	}
	for _, want := range []string{
		"gate                  off   no gate hook found",
		"router                off   gateway_router: rules; it runs only when the gateway is on",
		"memory and pathways   off   MCP server not registered",
		"floor (coppice)       off   no server answers",
		"loop (sprig)          off   sprig is not on PATH",
		"on:   daisugi install --gate",
		"off:  coppice server stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, "  on:   ") != 6 || strings.Count(out, "  off:  ") != 6 {
		t.Errorf("want six blocks:\n%s", out)
	}
}

// A gate hook and an MCP entry turn two pieces on; --json has the same six keys.
func TestPiecesSeeAHookAndAnMCPEntry(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := `{"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "daisugi hook --mode audit"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(hook), 0o644); err != nil {
		t.Fatal(err)
	}
	mcp := `{"mcpServers": {"opendaisugi": {"command": "daisugi"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, home, "", "modules", "--pieces", "--json", "--data-dir", filepath.Join(home, "dd"))
	if code != 0 {
		t.Fatalf("exit %d, %s", code, errs)
	}
	for _, want := range []string{`"key": "gate"`, `"detail": "hook installed for claude-code"`, `"detail": "MCP server registered"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
