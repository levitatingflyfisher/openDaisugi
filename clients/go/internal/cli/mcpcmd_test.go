package cli

import (
	"strings"
	"testing"

	"daisugi-verify/internal/verify"
)

const mcpInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",` +
	`"capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"

// A Z3 check that does not finish makes verify_plan answer not ok, with
// the z3 violation (fail closed), where the oracle's lenient verify keeps
// it as a warning.
func TestMCPVerifyPlanZ3UnknownFailsClosed(t *testing.T) {
	old := verifyPlan
	defer func() { verifyPlan = old }()
	verifyPlan = func(verify.ActionPlan, verify.Envelope, verify.VerifyOptions) verify.VerifyResultGo {
		return verify.VerifyResultGo{OK: true, Timeouts: []string{"Z3 returned unknown"}}
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"verify_plan","arguments":` +
		`{"plan":{"source":"s","task":"t","steps":[{"id":"a","type":"shell","command":"ls"}]},` +
		`"envelope":{"generated_by":"g","task":"t","permissions":{"shell":true,"shell_allowlist":["ls"]}}}}}` + "\n"
	code, out, errs := run(t, t.TempDir(), mcpInit+call, "mcp", "serve")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"ok":false`) || !strings.Contains(last, `"stage":"z3"`) ||
		!strings.Contains(last, "Z3 returned unknown") {
		t.Fatal(last)
	}
}

// A request this binary does not answer the oracle's way gets a JSON-RPC
// error that says so, and the server goes on.
func TestMCPUnportedMethodIsRefused(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"x://y"}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n"
	code, out, _ := run(t, t.TempDir(), mcpInit+req, "mcp", "serve")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.Contains(lines[1], `"code":-32000`) ||
		!strings.Contains(lines[1], "is not in this binary yet.") || lines[2] != `{"jsonrpc":"2.0","id":3,"result":{}}` {
		t.Fatalf("exit %d: %q", code, out)
	}
}
