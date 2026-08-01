package sprig

import "testing"

// A tool that records whether it actually ran — so we can prove fail-closed:
// a denied call must NEVER reach the tool.
type spyTool struct {
	ran bool
	out string
}

func (s *spyTool) Name() string { return "spy" }
func (s *spyTool) Run(map[string]any) (string, error) {
	s.ran = true
	return s.out, nil
}

func TestDeniedCallIsNeverExecuted(t *testing.T) {
	spy := &spyTool{out: "did work"}
	ex := NewExecutor(map[string]Tool{"spy": spy}, DenyAll{Reason: "nope"})

	res := ex.Execute(ToolCall{Name: "spy"})

	if spy.ran {
		t.Fatal("fail-closed violated: a denied tool call still executed")
	}
	if res.Allowed {
		t.Fatal("expected a refusal, got allowed")
	}
	if res.Reason != "nope" {
		t.Fatalf("want the gate's reason surfaced, got %q", res.Reason)
	}
}

func TestAllowedCallRuns(t *testing.T) {
	spy := &spyTool{out: "did work"}
	ex := NewExecutor(map[string]Tool{"spy": spy}, AllowAll{})

	res := ex.Execute(ToolCall{Name: "spy"})

	if !spy.ran {
		t.Fatal("an allowed call did not run the tool")
	}
	if !res.Allowed || res.Output != "did work" {
		t.Fatalf("want allowed with output, got %+v", res)
	}
}

func TestUnknownToolIsRefusedNotRun(t *testing.T) {
	// An unrecognized tool must be refused before anything happens — the harness
	// can never be tricked into running a tool it does not know.
	ex := NewExecutor(map[string]Tool{}, AllowAll{})

	res := ex.Execute(ToolCall{Name: "ghost"})

	if res.Allowed {
		t.Fatal("unknown tool must be refused")
	}
	if res.Reason == "" {
		t.Fatal("a refusal must carry a reason")
	}
}
