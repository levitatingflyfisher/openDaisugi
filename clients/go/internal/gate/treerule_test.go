package gate

import (
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

func TestTheTreeVerbsReadPerCommand(t *testing.T) {
	r := &runner{}
	for _, line := range []string{
		"daisugi gate register env.json",
		"daisugi --root /g gate --x register e.json",
		"daisugi tree spawn e.json --parent top --session kid",
		"daisugi tree status; daisugi gate register e.json",
		"dai\\sugi tr'ee' spa\"wn\"",
		"echo daisugi tree end",
		"daisugi gate init --force",
		"daisugi start claude",
		"daisugi-py start",
		"npm test && daisugi start",
		"daisugi status && npm start (",
	} {
		if !r.runsVerb(line, treeVerbs) {
			t.Errorf("not a hit: %q", line)
		}
	}
	for _, line := range []string{
		"daisugi tree check p.json c.json",
		"daisugi tree status --json",
		"daisugi register gate",
		"daisugi trees spawn",
		"daisugi tree check end.json root.json",
		"daisugi registry init /r",
		"npm start",
		"daisugi status && npm start",
		"daisugi help start",
	} {
		if r.runsVerb(line, treeVerbs) {
			t.Errorf("a hit: %q", line)
		}
	}
}

func TestTheGateVerbsAreTheOperators(t *testing.T) {
	r := &runner{}
	for _, line := range []string{
		"daisugi gate disarm",
		"daisugi gate arm",
		"daisugi gate serve",
		"daisugi install --uninstall",
		"daisugi graft remove",
		"uv run --no-sync python -m opendaisugi.cli gate disarm",
		"sh -c 'daisugi gate disarm'",
		"x=$(daisugi gate disarm)",
		"daisugi gate disarm; (",
	} {
		if !r.runsVerb(line, gateVerbs) {
			t.Errorf("not a hit: %q", line)
		}
	}
	for _, line := range []string{
		"daisugi gate status",
		"daisugi graft status",
		"daisugi status && npm install",
		"daisugi pathways show disarm",
		"grep 'gate disarm' daisugi.log",
	} {
		if r.runsVerb(line, gateVerbs) {
			t.Errorf("a hit: %q", line)
		}
	}
}

func TestTheDeadlineReadsTheValidatedFloat(t *testing.T) {
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope,
		`{"generated_by": "t", "task": "t", "deadline": 4000000000, "permissions": {}}`)
	if verr != nil {
		t.Fatal(verr.String())
	}
	e := envelopeFrom(v.(*pyjson.Object))
	if d, ok := e.deadline(); !ok || d != 4e9 {
		t.Fatalf("got %v %v", d, ok)
	}
	r := &runner{env: map[string]string{nowEnv: "4000000001"}}
	if r.gateNow() <= 4e9 {
		t.Fatal("the pin must move the clock forward")
	}
	r = &runner{env: map[string]string{nowEnv: "1"}}
	if r.gateNow() < 1e9 {
		t.Fatal("a pin never moves the clock back")
	}
}
