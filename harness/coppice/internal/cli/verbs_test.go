package cli

import (
	"strings"
	"testing"
)

func TestSendTextWithNoPositionalSendsABareEnter(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "send-text", []string{"w1:p1"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["text"] != "" {
		t.Fatalf("params = %v, want text \"\"", params)
	}
}

// pane.run's command text is keyed "line" on the wire. The request's own
// top-level "cmd" already names the verb; a second "cmd" in the same JSON
// object would silently replace it.
func TestPaneRunSendsTheLineUnderLineNotCmd(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "run", []string{"w1:p1", "ls", "-la"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["line"] != "ls -la" {
		t.Fatalf("params = %v, want line \"ls -la\"", params)
	}
	if _, ok := params["cmd"]; ok {
		t.Fatalf("params = %v, must not carry cmd: cmd is the wire verb", params)
	}
}

func TestTimeoutFlagMapsToTimeoutMS(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "wait-output", []string{"w1:p1", "--contains", "$", "--timeout", "500"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["timeout_ms"] != 500 {
		t.Fatalf("params = %v, want timeout_ms 500", params)
	}
}

func TestUnknownFlagNamesItselfAndTheVerb(t *testing.T) {
	_, _, errMsg := parseVerbArgs("pane", "list", []string{"--bogus", "x"})
	if !strings.Contains(errMsg, "--bogus") || !strings.Contains(errMsg, "pane list") {
		t.Fatalf("error %q does not name the flag and the verb", errMsg)
	}
}

func TestPaneResizeSendsIntegerColsAndRows(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "resize", []string{"w1:p1", "--cols", "80", "--rows", "24"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["cols"] != 80 || params["rows"] != 24 {
		t.Fatalf("params = %v, want integer cols and rows", params)
	}
	if params["pane"] != "w1:p1" {
		t.Fatalf("params = %v, want pane w1:p1", params)
	}
}

func TestAgentPromptSetsWaitTrueOnTheFlag(t *testing.T) {
	params, _, errMsg := parseVerbArgs("agent", "prompt", []string{"w1:p1", "--wait", "go", "on"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["wait"] != true {
		t.Fatalf("params = %v, want wait true", params)
	}
	if params["text"] != "go on" {
		t.Fatalf("params = %v, want text \"go on\"", params)
	}
}

func TestVerbNeedingAPaneRefusesWithNoPositional(t *testing.T) {
	_, _, errMsg := parseVerbArgs("pane", "close", nil)
	if !strings.Contains(errMsg, "pane") {
		t.Fatalf("error %q does not ask for a pane id", errMsg)
	}
}

func TestPaneListEndedFlagMapsToEndedTrue(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "list", []string{"--ended"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["ended"] != true {
		t.Fatalf("params = %v, want ended true", params)
	}
}

// pane forget takes a bare pane id, or --ended with none. Neither the CLI
// nor parseVerbArgs enforces "one of the two" - the server refuses with
// bad_request when it sees neither, the same way it already does for
// pane.create's cwd.
func TestPaneForgetTakesAPositionalPaneID(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "forget", []string{"w1:p3"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["pane"] != "w1:p3" {
		t.Fatalf("params = %v, want pane w1:p3", params)
	}
	if _, ok := params["ended"]; ok {
		t.Fatalf("params = %v, must not set ended when a pane id was given", params)
	}
}

func TestPaneForgetEndedFlagNeedsNoPositional(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "forget", []string{"--ended"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["ended"] != true {
		t.Fatalf("params = %v, want ended true", params)
	}
	if socketCommand("pane", "forget") != "pane.forget" {
		t.Fatalf("socketCommand = %q, want pane.forget", socketCommand("pane", "forget"))
	}
}

func TestPaneResumeTakesAPositionalPaneID(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "resume", []string{"w1:p3"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["pane"] != "w1:p3" {
		t.Fatalf("params = %v, want pane w1:p3", params)
	}
	if socketCommand("pane", "resume") != "pane.resume" {
		t.Fatalf("socketCommand = %q, want pane.resume", socketCommand("pane", "resume"))
	}
}

func TestPaneResumeRefusesWithNoPositional(t *testing.T) {
	_, _, errMsg := parseVerbArgs("pane", "resume", nil)
	if !strings.Contains(errMsg, "pane") {
		t.Fatalf("error %q does not ask for a pane id", errMsg)
	}
}

func TestPaneRenameJoinsTheTrailingWordsIntoOneLabel(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "rename", []string{"w1:p3", "new", "name"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["pane"] != "w1:p3" || params["label"] != "new name" {
		t.Fatalf("params = %v, want pane w1:p3 and label \"new name\"", params)
	}
	if socketCommand("pane", "rename") != "pane.rename" {
		t.Fatalf("socketCommand = %q, want pane.rename", socketCommand("pane", "rename"))
	}
}

func TestStatusTextFormatsCountsNotesAndWarnings(t *testing.T) {
	res := map[string]any{
		"pid": 123.0, "socket": "/tmp/s.sock", "panes": 2.0, "panes_live": 1.0, "uptime_s": 5.0,
		"restart_note": "note text",
		"restore": map[string]any{
			"panes": 2.0, "already_closed": 0.0, "resumed": 1.0, "marked_done": 1.0, "marked_unknown": 0.0,
			"notes": []any{"w1:p1 resumed harness session abc."},
		},
		"detection_warnings": []any{"the override directory is broken"},
	}
	text := statusText(res)
	for _, want := range []string{
		"123", "/tmp/s.sock", "note text", "resumed",
		"w1:p1 resumed harness session abc.",
		"detection warnings", "the override directory is broken",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status text %q missing %q", text, want)
		}
	}
}

// A bool flag followed by a bare number is almost certainly a mistyped
// value-taking flag, not free text: coppice agent prompt w1:p1 --wait 5000
// used to set wait: true and silently join "5000" into the prompt text.
func TestBoolFlagFollowedByABareNumberIsRefused(t *testing.T) {
	_, _, errMsg := parseVerbArgs("agent", "prompt", []string{"w1:p1", "--wait", "5000"})
	if !strings.Contains(errMsg, "--wait") || !strings.Contains(errMsg, "--timeout 5000") {
		t.Fatalf("error %q does not suggest --timeout 5000", errMsg)
	}
}

// A bool flag followed by ordinary text is unaffected: this is exactly the
// wait-then-prompt-text shape prompt is meant to support.
func TestBoolFlagFollowedByTextIsUnaffected(t *testing.T) {
	params, _, errMsg := parseVerbArgs("agent", "prompt", []string{"w1:p1", "--wait", "go", "on"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["wait"] != true || params["text"] != "go on" {
		t.Fatalf("params = %v, want wait true and text \"go on\"", params)
	}
}

func TestTaskCreateFlagsMapToTheWire(t *testing.T) {
	params, _, errMsg := parseVerbArgs("task", "create",
		[]string{"--label", "gate-refactor", "--parent", "t1", "--cwd", "/repo", "--worktree", "--model", "opus"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["label"] != "gate-refactor" || params["parent"] != "t1" || params["cwd"] != "/repo" ||
		params["worktree"] != true || params["model"] != "opus" {
		t.Fatalf("params = %v", params)
	}
	params, _, errMsg = parseVerbArgs("task", "create", []string{"--label", "x"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if _, ok := params["worktree"]; ok {
		t.Fatalf("worktree sent without the flag: %v", params)
	}
}

func TestTaskCloseAndMoveTakeATaskPositional(t *testing.T) {
	params, _, errMsg := parseVerbArgs("task", "close", []string{"t1", "--keep-worktree"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["task"] != "t1" || params["keep_worktree"] != true {
		t.Fatalf("params = %v", params)
	}
	if _, ok := params["pane"]; ok {
		t.Fatalf("a task verb sent pane: %v", params)
	}
	params, _, errMsg = parseVerbArgs("task", "move", []string{"t2", "--parent", "t1"})
	if errMsg != "" {
		t.Fatalf("unexpected error %q", errMsg)
	}
	if params["task"] != "t2" || params["parent"] != "t1" {
		t.Fatalf("params = %v", params)
	}
	_, _, errMsg = parseVerbArgs("task", "close", nil)
	if !strings.Contains(errMsg, "task id") || !strings.Contains(errMsg, "coppice task list") {
		t.Fatalf("error %q does not teach the task id", errMsg)
	}
	if socketCommand("task", "list") != "task.list" || pluralKey("task", "list") != "tasks" {
		t.Fatal("task list is not wired to task.list and its tasks key")
	}
}

func TestFloorNoteJoinsItsText(t *testing.T) {
	params, _, errMsg := parseVerbArgs("floor", "note", []string{"docs", "pane", "spawned"})
	if errMsg != "" || params["text"] != "docs pane spawned" {
		t.Fatalf("%v %q", params, errMsg)
	}
	if socketCommand("floor", "note") != "floor.note" {
		t.Fatal("wire verb")
	}
}

func TestFloorTalkJoinsItsText(t *testing.T) {
	params, _, errMsg := parseVerbArgs("floor", "talk", []string{"start", "an", "agent", "in", "trellis"})
	if errMsg != "" || params["text"] != "start an agent in trellis" {
		t.Fatalf("%v %q", params, errMsg)
	}
	if socketCommand("floor", "talk") != "floor.talk" {
		t.Fatal("wire verb")
	}
}

func TestPaneForkTakesAPaneAndALabel(t *testing.T) {
	params, _, errMsg := parseVerbArgs("pane", "fork", []string{"w1:p1", "--label", "try two"})
	if errMsg != "" || params["pane"] != "w1:p1" || params["label"] != "try two" {
		t.Fatalf("%v %q", params, errMsg)
	}
	if socketCommand("pane", "fork") != "pane.fork" {
		t.Fatal("wire verb")
	}
}

func TestAgentAllowTakesAPaneAnAskAndAReason(t *testing.T) {
	params, _, errMsg := parseVerbArgs("agent", "allow", []string{"w1:p1", "ask-3", "--reason", "looks fine"})
	if errMsg != "" || params["pane"] != "w1:p1" || params["ask"] != "ask-3" || params["reason"] != "looks fine" {
		t.Fatalf("%v %q", params, errMsg)
	}
	if socketCommand("agent", "deny") != "agent.deny" {
		t.Fatal("wire verb")
	}
}
