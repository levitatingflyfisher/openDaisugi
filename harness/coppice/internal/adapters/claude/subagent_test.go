package claude

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/pane"
)

func TestASubagentStartIsAWorkingChild(t *testing.T) {
	p := &proc{}
	evs := p.parse([]byte(`{"session_id":"s","hook_event_name":"SubagentStart","agent_id":"a1","agent_type":"Explore"}`))
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one", evs)
	}
	ev := evs[0]
	if ev.Kind != pane.EvChild || ev.Child != "a1" || ev.State != "working" || ev.Text != "Explore" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestASubagentStopIsADoneChild(t *testing.T) {
	p := &proc{}
	evs := p.parse([]byte(`{"session_id":"s","hook_event_name":"SubagentStop","agent_id":"a1",` +
		`"agent_type":"Explore","agent_transcript_path":"/t.jsonl","stop_hook_active":false}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvChild || evs[0].State != "done" || evs[0].Child != "a1" {
		t.Fatalf("events = %+v", evs)
	}
}

// A subagent hook line with no agent id names no child, and says so.
func TestASubagentLineWithNoAgentIDIsAnError(t *testing.T) {
	p := &proc{}
	evs := p.parse([]byte(`{"hook_event_name":"SubagentStart"}`))
	if len(evs) != 1 || evs[0].Kind != pane.EvError {
		t.Fatalf("events = %+v", evs)
	}
}

func TestPidsIsEmptyWithNoProcess(t *testing.T) {
	var _ pane.Pider = &proc{}
	if len((&proc{}).Pids()) != 0 {
		t.Fatal("a proc with no process named a pid")
	}
}
