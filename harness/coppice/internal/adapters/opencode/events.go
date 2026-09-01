package opencode

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// askDeadline is how long an ask stays open for a floor client.
// OpenCode's own prompts carry no timeout. At the deadline the adapter
// rejects the ask, so an ask no one answered never turns into an approval.
var askDeadline = 90 * time.Second

// busEvent is one event of OpenCode's /event stream. PINS.md records the
// shapes of the types the translator reads.
type busEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// parseSSELine decodes one "data: {json}" line. Any other line, such as a
// blank line or a ": comment", and any line that is not one JSON object, is
// not an event.
func parseSSELine(line string) (busEvent, bool) {
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return busEvent{}, false
	}
	var ev busEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev); err != nil || ev.Type == "" {
		return busEvent{}, false
	}
	return ev, true
}

// openAsk is one of OpenCode's own prompts that waits for an answer.
type openAsk struct {
	id       string
	kind     string // "permission" or "question"
	tool     string
	summary  string
	count    int // how many questions a question ask holds
	deadline time.Time
}

// translator turns the events of one OpenCode session, and of the child
// sessions its task tool calls start, into pane events. It keeps each open
// ask by id, so the adapter answers the exact ask the operator names, and
// the tool calls it has already named, so each one is named once.
type translator struct {
	session string
	now     func() time.Time

	mu       sync.Mutex
	children map[string]bool
	asks     []openAsk // oldest first
	tools    map[string]bool
}

func newTranslator(session string, now func() time.Time) *translator {
	return &translator{session: session, now: now, children: map[string]bool{}, tools: map[string]bool{}}
}

// ours reports whether sid is the pane's session or one of its children.
// The caller holds t.mu.
func (t *translator) oursLocked(sid string) bool {
	return sid != "" && (sid == t.session || t.children[sid])
}

// open returns the ids of the open permission asks and the open questions,
// oldest first. An ask past its deadline is not open.
func (t *translator) open() (perms, questions []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, a := range t.asks {
		if now.After(a.deadline) {
			continue
		}
		if a.kind == "permission" {
			perms = append(perms, a.id)
		} else {
			questions = append(questions, a.id)
		}
	}
	return perms, questions
}

// get returns the open ask id, if it is open and not past its deadline.
func (t *translator) get(id string) (openAsk, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range t.asks {
		if a.id == id && !t.now().After(a.deadline) {
			return a, true
		}
	}
	return openAsk{}, false
}

func (t *translator) owns(id string) bool {
	_, ok := t.get(id)
	return ok
}

// remove forgets the ask id.
func (t *translator) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeLocked(id)
}

func (t *translator) removeLocked(id string) {
	out := t.asks[:0]
	for _, a := range t.asks {
		if a.id != id {
			out = append(out, a)
		}
	}
	t.asks = out
}

// expire removes and returns each ask past its deadline.
func (t *translator) expire() []openAsk {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var gone []openAsk
	keep := t.asks[:0]
	for _, a := range t.asks {
		if now.After(a.deadline) {
			gone = append(gone, a)
		} else {
			keep = append(keep, a)
		}
	}
	t.asks = keep
	return gone
}

func (t *translator) blockedOn(a openAsk) pane.Event {
	return pane.Event{
		Kind: pane.EvState, State: pane.StateBlockedStr, Detail: a.summary,
		Ask: &proto.Ask{ID: a.id, Tool: a.tool, Summary: a.summary, Deadline: float64(a.deadline.Unix())},
	}
}

// addLocked opens an ask and returns its blocked event. The caller holds
// t.mu.
func (t *translator) addLocked(a openAsk) []pane.Event {
	a.deadline = t.now().Add(askDeadline)
	t.removeLocked(a.id)
	t.asks = append(t.asks, a)
	return []pane.Event{t.blockedOn(a)}
}

// resolvedLocked forgets the ask id. When another ask is still open the
// pane stays blocked on the newest one, else it works on. The caller holds
// t.mu.
func (t *translator) resolvedLocked(id string) []pane.Event {
	t.removeLocked(id)
	if n := len(t.asks); n > 0 {
		return []pane.Event{t.blockedOn(t.asks[n-1])}
	}
	return state(pane.StateWorkingStr)
}

func state(s string) []pane.Event { return []pane.Event{{Kind: pane.EvState, State: s}} }

// translate maps one bus event to zero or more pane events. Session state
// and text come from the pane's own session only. Asks come from it and
// from its child sessions. Any other event maps to nothing.
func (t *translator) translate(ev busEvent) []pane.Event {
	var scope struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID       string `json:"id"`
			ParentID string `json:"parentID"`
		} `json:"info"`
	}
	if json.Unmarshal(ev.Properties, &scope) != nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if ev.Type == "session.created" {
		if scope.Info.ID != "" && t.oursLocked(scope.Info.ParentID) {
			t.children[scope.Info.ID] = true
		}
		return nil
	}
	if !t.oursLocked(scope.SessionID) {
		return nil
	}
	own := scope.SessionID == t.session
	switch ev.Type {
	case "session.status":
		if !own {
			return nil
		}
		var p struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		}
		if json.Unmarshal(ev.Properties, &p) != nil {
			return nil
		}
		switch p.Status.Type {
		case "busy", "retry":
			return state(pane.StateWorkingStr)
		case "idle":
			t.clearLocked()
			return state(pane.StateIdleStr)
		}
		return nil
	case "session.idle":
		if !own {
			return nil
		}
		t.clearLocked()
		return state(pane.StateIdleStr)
	case "session.error":
		if !own {
			return nil
		}
		var p struct {
			Error struct {
				Name string `json:"name"`
				Data struct {
					Message string `json:"message"`
				} `json:"data"`
			} `json:"error"`
		}
		_ = json.Unmarshal(ev.Properties, &p)
		detail := strings.TrimSpace(p.Error.Name + ": " + p.Error.Data.Message)
		return []pane.Event{{Kind: pane.EvState, State: pane.StateUnknownStr, Detail: "session error " + detail}}
	case "permission.asked":
		var p struct {
			ID         string   `json:"id"`
			Permission string   `json:"permission"`
			Patterns   []string `json:"patterns"`
		}
		if json.Unmarshal(ev.Properties, &p) != nil || p.ID == "" {
			return nil
		}
		return t.addLocked(openAsk{id: p.ID, kind: "permission", tool: p.Permission, summary: strings.Join(p.Patterns, ", ")})
	case "question.asked":
		var p struct {
			ID        string `json:"id"`
			Questions []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		if json.Unmarshal(ev.Properties, &p) != nil || p.ID == "" {
			return nil
		}
		summary := ""
		if len(p.Questions) > 0 {
			summary = p.Questions[0].Question
		}
		return t.addLocked(openAsk{id: p.ID, kind: "question", tool: "question", summary: summary, count: len(p.Questions)})
	case "permission.replied", "question.replied", "question.rejected":
		var p struct {
			RequestID string `json:"requestID"`
		}
		if json.Unmarshal(ev.Properties, &p) != nil {
			return nil
		}
		return t.resolvedLocked(p.RequestID)
	case "message.part.updated":
		if !own {
			return nil
		}
		return t.partLocked(ev.Properties)
	}
	return nil
}

// clearLocked forgets the open asks and the named tool calls. An idle
// session has no open prompt and no running tool, and a call id never
// comes back. The caller holds t.mu.
func (t *translator) clearLocked() {
	t.asks = nil
	t.tools = map[string]bool{}
}

// partLocked maps one message part. The caller holds t.mu. A text part becomes one text event when it
// ends: the server sends the part again with time.end set and the full
// text, so the deltas before it never reach the grid. The echo of the
// user's own prompt has no time and is skipped. A tool part becomes one
// tool event the first time it runs.
func (t *translator) partLocked(raw json.RawMessage) []pane.Event {
	var p struct {
		Part struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			CallID string `json:"callID"`
			Tool   string `json:"tool"`
			Time   *struct {
				End *int64 `json:"end"`
			} `json:"time"`
			State struct {
				Status string          `json:"status"`
				Input  json.RawMessage `json:"input"`
			} `json:"state"`
		} `json:"part"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	switch p.Part.Type {
	case "text":
		if p.Part.Time == nil || p.Part.Time.End == nil || p.Part.Text == "" {
			return nil
		}
		return []pane.Event{{Kind: pane.EvText, Text: p.Part.Text}}
	case "tool":
		if p.Part.State.Status != "running" || p.Part.CallID == "" {
			return nil
		}
		seen := t.tools[p.Part.CallID]
		t.tools[p.Part.CallID] = true
		if seen {
			return nil
		}
		return []pane.Event{{Kind: pane.EvTool, Tool: p.Part.Tool, Detail: p.Part.CallID + " " + string(p.Part.State.Input)}}
	}
	return nil
}
