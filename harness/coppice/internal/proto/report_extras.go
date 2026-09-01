package proto

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// The modes a gate hook reports. The daisugi config value shadow is
// reported as watching, and enforce as enforcing.
const (
	ModeEnforcing = "enforcing"
	ModeWatching  = "watching"
)

// Verdict is the gate's decision on one tool call, as the gate hook
// reports it. Decision is allow, deny or ask. In watching mode the
// decision is what the harness was told, allow, and Clause names what an
// enforcing gate would have denied.
type Verdict struct {
	Decision string `json:"decision"`
	Tool     string `json:"tool"`
	Clause   string `json:"clause"`
}

var validDecisions = map[string]bool{"allow": true, "deny": true, "ask": true}

// ReportExtras are the optional fields a gate hook sends inside the event
// of pane.report_state, beside the PaneStateEvent fields. They are kept
// apart from PaneStateEvent on purpose: the event is stored and sent to
// every client, and a transcript path must never reach a client.
type ReportExtras struct {
	// TranscriptPath is the harness's own transcript file. It is absolute.
	TranscriptPath string
	Verdict        *Verdict
	// Mode is ModeEnforcing, ModeWatching or empty.
	Mode string
}

// ParseReportExtras reads the optional fields from one event object.
// Absent or JSON null means not sent. A present value of the wrong type,
// a relative transcript path, an unknown mode or an unknown decision is
// an error: a bad field refuses the report rather than being guessed at.
func ParseReportExtras(raw []byte) (ReportExtras, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		return ReportExtras{}, fmt.Errorf("state event is not a JSON object: %w", err)
	}
	var x ReportExtras
	tp, err := optionalStringFromRow(row, "transcript_path")
	if err != nil {
		return ReportExtras{}, err
	}
	if tp != nil {
		if !filepath.IsAbs(*tp) {
			return ReportExtras{}, fmt.Errorf("transcript_path must be an absolute path")
		}
		x.TranscriptPath = filepath.Clean(*tp)
	}
	mode, err := optionalStringFromRow(row, "mode")
	if err != nil {
		return ReportExtras{}, err
	}
	if mode != nil {
		if *mode != ModeEnforcing && *mode != ModeWatching {
			return ReportExtras{}, fmt.Errorf("mode %q is not enforcing or watching", *mode)
		}
		x.Mode = *mode
	}
	if rv, ok := row["verdict"]; ok && !isJSONNull(rv) {
		v, err := parseVerdict(rv)
		if err != nil {
			return ReportExtras{}, err
		}
		x.Verdict = v
	}
	return x, nil
}

func parseVerdict(raw json.RawMessage) (*Verdict, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("verdict must be a JSON object")
	}
	rd, ok := obj["decision"]
	if !ok {
		return nil, fmt.Errorf("verdict is missing decision")
	}
	decision, err := stringFromRaw(rd, "verdict decision")
	if err != nil {
		return nil, err
	}
	if !validDecisions[decision] {
		return nil, fmt.Errorf("verdict decision %q is not allow, deny or ask", decision)
	}
	tool, err := optionalStringFromRow(obj, "tool")
	if err != nil {
		return nil, fmt.Errorf("verdict tool must be a string")
	}
	clause, err := optionalStringFromRow(obj, "clause")
	if err != nil {
		return nil, fmt.Errorf("verdict clause must be a string")
	}
	v := &Verdict{Decision: decision}
	if tool != nil {
		v.Tool = cutRunes(*tool, DetailMax)
	}
	if clause != nil {
		v.Clause = cutRunes(*clause, DetailMax)
	}
	return v, nil
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
