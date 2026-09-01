package proto

import (
	"strings"
	"testing"
)

// TestValidateAgreesWithThePythonOracleOnEveryRule transcribes, row for row,
// the parametrize table in
// tests/floor/test_events.py::test_layer_validator_and_dataclass_agree_on_every_rule
// (the base row there is _row(): v=1, ts=1000.0, session_id="s1",
// harness="claude-code", state="idle", source="manifest", detail=""). That
// test pins opendaisugi.floor.events.PaneStateEvent.from_json and
// opendaisugi._state_report._validate_hook_report_row to the same verdict on
// every row; this test pins ParseStateEvent to the same verdicts on the same
// JSON, so all three implementations of master spec section 3.1 agree. It
// does not shell out to Python: the rows are copied by hand, not generated.
func TestValidateAgreesWithThePythonOracleOnEveryRule(t *testing.T) {
	longDetail := strings.Repeat("x", 201)

	rows := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name:    "the base row is valid",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":""}`,
			wantErr: false,
		},
		{
			name: "a gate blocked event with a well-formed ask is valid",
			json: `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"blocked","source":"gate","detail":"",` +
				`"ask":{"id":"t1","tool":"Bash","summary":"ls","deadline":1090.0}}`,
			wantErr: false,
		},
		{
			name:    "an unknown state is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"napping","source":"manifest","detail":""}`,
			wantErr: true,
		},
		{
			name:    "an unknown source is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"ouija","detail":""}`,
			wantErr: true,
		},
		{
			name:    "manifest may not say done",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"done","source":"manifest","detail":""}`,
			wantErr: true,
		},
		{
			name:    "gate may not say done",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"done","source":"gate","detail":""}`,
			wantErr: true,
		},
		{
			name:    "operator may not say done",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"done","source":"operator","detail":""}`,
			wantErr: true,
		},
		{
			name:    "process may say done",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"done","source":"process","detail":""}`,
			wantErr: false,
		},
		{
			name:    "headless may say done",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"done","source":"headless","detail":""}`,
			wantErr: false,
		},
		{
			name:    "detail over 200 chars is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":"` + longDetail + `"}`,
			wantErr: true,
		},
		{
			name: "an ask on a non-blocked event is rejected",
			json: `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"working","source":"gate","detail":"",` +
				`"ask":{"id":"t1","tool":"Bash","summary":"ls","deadline":1090.0}}`,
			wantErr: true,
		},
		{
			name:    "an unsupported schema version is rejected",
			json:    `{"v":2,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":""}`,
			wantErr: true,
		},
		{
			name:    "a string ts is rejected",
			json:    `{"v":1,"ts":"abc","session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":""}`,
			wantErr: true,
		},
		{
			name:    "a null ts is rejected",
			json:    `{"v":1,"ts":null,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":""}`,
			wantErr: true,
		},
		{
			name:    "a non-object ask is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"blocked","source":"gate","detail":"","ask":5}`,
			wantErr: true,
		},
		{
			name:    "a missing required field is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","state":"idle","source":"manifest","detail":""}`,
			wantErr: true,
		},
		{
			name:    "a non-string session_id is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":5,"harness":"claude-code","state":"idle","source":"manifest","detail":""}`,
			wantErr: true,
		},
		// A present, non-string pane or harness_session_id is rejected the
		// same as a present, non-string session_id above. Null stays valid:
		// it means "not reported".
		{
			name:    "a non-string pane is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":"","pane":5}`,
			wantErr: true,
		},
		{
			name:    "a non-string harness_session_id is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":"","harness_session_id":5}`,
			wantErr: true,
		},
		{
			name:    "a null pane is valid",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":"","pane":null}`,
			wantErr: false,
		},
		{
			name:    "a null harness_session_id is valid",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"idle","source":"manifest","detail":"","harness_session_id":null}`,
			wantErr: false,
		},
		{
			name:    "a gate blocked event with no ask is rejected",
			json:    `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"blocked","source":"gate","detail":""}`,
			wantErr: true,
		},
		{
			name: "a non-numeric ask deadline is rejected",
			json: `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"blocked","source":"gate","detail":"",` +
				`"ask":{"id":"t1","tool":"Bash","summary":"ls","deadline":"soon"}}`,
			wantErr: true,
		},
		{
			name: "a null ask deadline is rejected",
			json: `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"blocked","source":"gate","detail":"",` +
				`"ask":{"id":"t1","tool":"Bash","summary":"ls","deadline":null}}`,
			wantErr: true,
		},
		{
			name: "a non-string ask id is rejected",
			json: `{"v":1,"ts":1000.0,"session_id":"s1","harness":"claude-code","state":"blocked","source":"gate","detail":"",` +
				`"ask":{"id":5,"tool":"Bash","summary":"ls","deadline":1090.0}}`,
			wantErr: true,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := ParseStateEvent([]byte(row.json))
			if gotErr := err != nil; gotErr != row.wantErr {
				t.Fatalf("ParseStateEvent(%s) error = %v, want error = %v", row.json, err, row.wantErr)
			}
		})
	}
}
