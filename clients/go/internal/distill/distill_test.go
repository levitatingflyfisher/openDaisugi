package distill

import (
	"regexp"
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/verify"
)

func obj(t *testing.T, m *pmodel.Model, text string) *pyjson.Object {
	t.Helper()
	v, err := pmodel.ValidateJSON(m.Name, m, text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

var marker = regexp.MustCompile(`\(echo "([^"]+)"\)`)

// A Z3 that answers unknown to every check: a timeout, whatever the
// envelope. The distiller must not store what it could not prove.
func TestZ3TimeoutFailsClosed(t *testing.T) {
	old := verify.InProcessZ3
	verify.InProcessZ3 = func(cmds string) (string, error) {
		return "unknown\n" + marker.FindStringSubmatch(cmds)[1] + "\n", nil
	}
	defer func() { verify.InProcessZ3 = old }()
	env := obj(t, pmodel.Envelope, `{"id":"env_1","generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["make"]}}`)
	plan := obj(t, pmodel.ActionPlan, `{"id":"p","source":"s","task":"t","steps":[{"id":"a","type":"shell","command":"make"}]}`)
	d := &Distiller{Opt: Options{Z3TimeoutMs: 500}}
	ok, why, err := d.verified(plan, env)
	if err != nil || ok || why != "verifier timed out; raise the Z3 timeout" {
		t.Fatalf("verified: %v %q %v", ok, why, err)
	}
	score, failing, err := d.validateEnvelope(env, []*pyjson.Object{plan})
	if err != nil || score != 0 || len(failing) != 1 {
		t.Fatalf("validate: %v %v %v", score, failing, err)
	}
}
