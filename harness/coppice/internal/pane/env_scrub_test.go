package pane

import (
	"strings"
	"testing"
)

// A server started from inside an agent session passes that session's
// markers to every pane it starts. A pane must not inherit another
// session's identity or its messaging token, and a harness would treat
// itself as a child session. Settings the owner chose still pass.
func TestBuildEnvDropsInheritedSessionMarkers(t *testing.T) {
	base := []string{
		"PATH=/bin", "CLAUDECODE=1", "CLAUDE_PID=42",
		"CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_CODE_SESSION_ID=abc",
		"CLAUDE_CODE_MESSAGING_SOCKET=/run/x", "CLAUDE_CODE_MESSAGING_TOKEN=secret",
		"CLAUDE_CODE_BRIDGE_SESSION_ID=b", "CLAUDE_CODE_SESSION_ATTENDED=1",
		"CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_EXECPATH=/x/claude",
		"HERDR_PANE_ID=7", "HERDR_PANE=7",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000", "CLAUDE_EFFORT=high",
	}
	env := strings.Join(BuildEnv(base, nil, "/s", "w1:p1", ""), "\n")
	for _, gone := range []string{"CLAUDECODE=", "CLAUDE_PID=", "CHILD_SESSION", "SESSION_ID",
		"MESSAGING", "BRIDGE", "SESSION_ATTENDED", "ENTRYPOINT", "EXECPATH", "HERDR_PANE"} {
		if strings.Contains(env, gone) {
			t.Errorf("inherited %s reached the pane:\n%s", gone, env)
		}
	}
	for _, kept := range []string{"PATH=/bin", "CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000", "CLAUDE_EFFORT=high", "COPPICE_PANE=w1:p1"} {
		if !strings.Contains(env, kept) {
			t.Errorf("%s did not reach the pane:\n%s", kept, env)
		}
	}
}

// A pane's own env may still set any of them on purpose.
func TestBuildEnvKeepsAnExplicitPaneValue(t *testing.T) {
	env := strings.Join(BuildEnv([]string{"CLAUDECODE=1"}, map[string]string{"CLAUDECODE": "2"}, "/s", "p", ""), "\n")
	if !strings.Contains(env, "CLAUDECODE=2") {
		t.Fatalf("explicit value lost:\n%s", env)
	}
}
