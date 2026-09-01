package proto

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	StateIdle    = "idle"
	StateWorking = "working"
	StateBlocked = "blocked"
	StateDone    = "done"
	StateUnknown = "unknown"
)

// Sources, in precedence order. Master spec 3.1, amended 2026-09-13: a live
// pty pane always has a process source. The server's own process tick
// reports working within five seconds of output and idle after that, and
// watchExit reports done on exit. So a live pane is never unknown; that
// state is reserved for a headless pane whose event stream failed to parse.
const (
	SrcOperator = "operator"
	SrcGate     = "gate"
	SrcHeadless = "headless"
	SrcProcess  = "process"
	SrcManifest = "manifest"
)

// DetailMax is master spec 3.1's cap on the free-text detail field.
const DetailMax = 200

var validStates = map[string]bool{
	StateIdle: true, StateWorking: true, StateBlocked: true, StateDone: true, StateUnknown: true,
}

// sourceRanks encodes the precedence in master spec 3.1:
// operator > gate > headless > process > manifest.
var sourceRanks = map[string]int{
	SrcOperator: 5, SrcGate: 4, SrcHeadless: 3, SrcProcess: 2, SrcManifest: 1,
}

// SourceRank is 0 for anything not in the enum, so an unknown source can never
// win a comparison. That is the fail-closed direction.
func SourceRank(src string) int { return sourceRanks[src] }

type Ask struct {
	ID       string  `json:"id"`
	Tool     string  `json:"tool"`
	Summary  string  `json:"summary"`
	Deadline float64 `json:"deadline"`
}

// PaneStateEvent is master spec 3.1, byte for byte. The Python side in
// opendaisugi.floor.events (PaneStateEvent) declares the same shape;
// testdata/events holds the fixtures both sides parse.
//
// HarnessSessionID and Pane are *string, not string: master spec 3.1 lets a
// sender omit them or send JSON null, and a coppice reader must not confuse
// "not reported" with the empty string.
type PaneStateEvent struct {
	V                int     `json:"v"`
	TS               float64 `json:"ts"`
	SessionID        string  `json:"session_id"`
	HarnessSessionID *string `json:"harness_session_id"`
	Harness          string  `json:"harness"`
	Pane             *string `json:"pane"`
	State            string  `json:"state"`
	Source           string  `json:"source"`
	Ask              *Ask    `json:"ask,omitempty"`
	Detail           string  `json:"detail"`
}

// Validate is the Go twin of opendaisugi.floor.events.PaneStateEvent
// __post_init__: it assumes every field already carries the right Go type
// (ParseStateEvent's job) and checks only the cross-field rules from master
// spec section 3.1. It does not check that session_id or harness is
// non-empty, because the Python dataclass does not either - that is
// deliberate, not an oversight: from_json's own required-field check is what
// catches a field that was never sent.
func (e PaneStateEvent) Validate() error {
	if !validStates[e.State] {
		return fmt.Errorf("state %q is not one of idle working blocked done unknown", e.State)
	}
	if SourceRank(e.Source) == 0 {
		return fmt.Errorf("source %q is not one of operator gate headless process manifest", e.Source)
	}
	// Master spec 3.1, amended 2026-09-09: done may only come from process
	// or headless. A gate- or operator-sourced done would show a live agent
	// as finished, so every other source is barred, not only manifest.
	// Matches opendaisugi.floor.events.PaneStateEvent.__post_init__:
	//   if self.state == "done" and self.source not in ("process", "headless"):
	if e.State == StateDone && e.Source != SrcProcess && e.Source != SrcHeadless {
		return fmt.Errorf("state done may only come from source process or headless, got %q", e.Source)
	}
	if utf8.RuneCountInString(e.Detail) > DetailMax {
		return fmt.Errorf("detail is %d chars, the cap is %d",
			utf8.RuneCountInString(e.Detail), DetailMax)
	}
	if e.Ask != nil && e.State != StateBlocked {
		return fmt.Errorf("ask is only valid on a blocked event, state is %q", e.State)
	}
	if e.Source == SrcGate && e.State == StateBlocked && e.Ask == nil {
		return fmt.Errorf("a gate blocked event must carry the ask it is holding")
	}
	if e.V != 1 {
		return fmt.Errorf("unsupported event schema version %d", e.V)
	}
	return nil
}

// ParseStateEvent is the Go twin of
// opendaisugi.floor.events.PaneStateEvent.from_json: it decodes one line,
// checks that every field present has the JSON type master spec 3.1
// requires (a required field absent, or present as the wrong type or as
// JSON null, is rejected here rather than silently zero-valued - Go's
// encoding/json leaves a field at its zero value for a JSON null without
// error, which from_json's own isinstance checks do not allow), then calls
// Validate for the cross-field rules. A bad line is an error, never a
// default event, because a default would be a state nobody reported.
func ParseStateEvent(line []byte) (PaneStateEvent, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(line, &row); err != nil {
		return PaneStateEvent{}, fmt.Errorf("state event is not a JSON object: %w", err)
	}

	required := []string{"session_id", "harness", "state", "source", "ts"}
	var missing []string
	for _, k := range required {
		if _, ok := row[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return PaneStateEvent{}, fmt.Errorf("event is missing required field(s): %s", strings.Join(missing, ", "))
	}

	sessionID, err := stringFromRaw(row["session_id"], "session_id")
	if err != nil {
		return PaneStateEvent{}, err
	}
	harness, err := stringFromRaw(row["harness"], "harness")
	if err != nil {
		return PaneStateEvent{}, err
	}
	state, err := stringFromRaw(row["state"], "state")
	if err != nil {
		return PaneStateEvent{}, err
	}
	source, err := stringFromRaw(row["source"], "source")
	if err != nil {
		return PaneStateEvent{}, err
	}
	ts, err := numberFromRaw(row["ts"], "ts")
	if err != nil {
		return PaneStateEvent{}, err
	}

	detail := ""
	if raw, ok := row["detail"]; ok {
		detail, err = stringFromRaw(raw, "detail")
		if err != nil {
			return PaneStateEvent{}, err
		}
	}

	v := 1
	if raw, ok := row["v"]; ok {
		v, err = intFromRaw(raw)
		if err != nil {
			return PaneStateEvent{}, err
		}
	}

	harnessSessionID, err := optionalStringFromRow(row, "harness_session_id")
	if err != nil {
		return PaneStateEvent{}, err
	}
	pane, err := optionalStringFromRow(row, "pane")
	if err != nil {
		return PaneStateEvent{}, err
	}

	var ask *Ask
	if raw, ok := row["ask"]; ok && !isJSONNull(raw) {
		ask, err = parseAsk(raw)
		if err != nil {
			return PaneStateEvent{}, err
		}
	}

	ev := PaneStateEvent{
		V:                v,
		TS:               ts,
		SessionID:        sessionID,
		HarnessSessionID: harnessSessionID,
		Harness:          harness,
		Pane:             pane,
		State:            state,
		Source:           source,
		Ask:              ask,
		Detail:           detail,
	}
	if err := ev.Validate(); err != nil {
		return PaneStateEvent{}, err
	}
	return ev, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// stringFromRaw requires raw to be present and a JSON string, matching
// PaneStateEvent.from_json's `isinstance(row[key], str)` checks. JSON null is
// rejected rather than silently becoming "", because encoding/json would
// otherwise leave the zero value with no error.
func stringFromRaw(raw json.RawMessage, name string) (string, error) {
	if isJSONNull(raw) {
		return "", fmt.Errorf("%s must be a string", name)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return s, nil
}

// numberFromRaw requires raw to be present and a JSON number, matching
// from_json's `isinstance(ts_raw, bool) or not isinstance(ts_raw, (int,
// float))` check. A JSON bool already fails Go's own float64 unmarshal, so no
// separate bool check is needed here; JSON null is checked explicitly for the
// same reason as stringFromRaw.
func numberFromRaw(raw json.RawMessage, name string) (float64, error) {
	if isJSONNull(raw) {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	return f, nil
}

// intFromRaw parses the optional "v" field. Go's encoding/json already
// refuses a bool or a non-whole-number literal (even "1.0") for an int
// target, which matches from_json's `isinstance(v_raw, bool) or not
// isinstance(v_raw, int)` - Python's json.loads also parses "1.0" as a float,
// so isinstance(1.0, int) is False there too.
func intFromRaw(raw json.RawMessage) (int, error) {
	if isJSONNull(raw) {
		return 0, fmt.Errorf("invalid schema version: null")
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("invalid schema version: %w", err)
	}
	return n, nil
}

// optionalStringFromRow reads harness_session_id or pane: absent or JSON null
// both mean "not reported" (nil), matching from_json's `row.get(key)`; present
// with a non-string value is an error.
func optionalStringFromRow(row map[string]json.RawMessage, key string) (*string, error) {
	raw, ok := row[key]
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s must be a string", key)
	}
	return &s, nil
}

// parseAsk mirrors from_json's ask handling: the ask value must be a JSON
// object, all four fields must be present, id/tool/summary must be strings,
// and deadline must be a number (not a bool, not null).
func parseAsk(raw json.RawMessage) (*Ask, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("ask must be a JSON object")
	}

	required := []string{"id", "tool", "summary", "deadline"}
	var missing []string
	for _, k := range required {
		if _, ok := obj[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("ask is missing field(s): %s", strings.Join(missing, ", "))
	}

	id, err := stringFromRaw(obj["id"], "ask id")
	if err != nil {
		return nil, err
	}
	tool, err := stringFromRaw(obj["tool"], "ask tool")
	if err != nil {
		return nil, err
	}
	summary, err := stringFromRaw(obj["summary"], "ask summary")
	if err != nil {
		return nil, err
	}
	deadline, err := numberFromRaw(obj["deadline"], "ask deadline")
	if err != nil {
		return nil, err
	}
	return &Ask{ID: id, Tool: tool, Summary: summary, Deadline: deadline}, nil
}
