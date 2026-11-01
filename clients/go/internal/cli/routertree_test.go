package cli

import (
	"strings"
	"testing"
)

// The router is one group with five verbs; the commands it replaces are
// gone, with no alias.
func TestRouterTree(t *testing.T) {
	home := t.TempDir()
	code, out, _ := run(t, home, "", "router", "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, verb := range []string{"serve", "report", "status", "label", "stop"} {
		if !strings.Contains(out, "\n  "+verb+" ") {
			t.Errorf("router --help does not list %s:\n%s", verb, out)
		}
	}
	for _, old := range []string{"gateway", "gateway-report"} { // names-ok: the old commands are gone
		code, _, errs := run(t, home, "", old, "--help")
		if code != 2 || !strings.Contains(errs, "No such command") && !strings.Contains(errs, "is not in this binary yet") {
			t.Errorf("%s: exit %d %q", old, code, errs)
		}
	}
}

// install takes --router and --routing-mode, and not the old flags.
func TestInstallTakesRouterAndRoutingMode(t *testing.T) {
	home := t.TempDir()
	for _, old := range []string{"--gateway", "--gate"} { // names-ok: the old flags are gone
		code, _, _ := run(t, home, "", "install", old, "--dry-run", "--yes")
		if code != 2 {
			t.Errorf("%s: exit %d, want 2", old, code)
		}
	}
}
