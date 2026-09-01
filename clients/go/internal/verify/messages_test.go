package verify

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"daisugi-verify/internal/z3"
)

// verify_messages.jsonl is written by clients/pathway_cases.py: plans and
// envelopes, and what the oracle's verify() found, message by message.
const messagesPath = "../../../fixtures/pathways/verify_messages.jsonl"

func TestViolationMessagesMatchTheOracle(t *testing.T) {
	InProcessZ3 = z3.EvalSMTLIB2
	path := messagesPath
	if p := os.Getenv("DAISUGI_VERIFY_MESSAGES"); p != "" {
		path = p // more cases in the same shape, such as the private corpus's
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	n, words := 0, 0
	for sc.Scan() {
		var c struct {
			Plan     string `json:"plan"`
			Envelope string `json:"envelope"`
			Strict   *bool  `json:"strict"`
			Expect   struct {
				OK         bool     `json:"ok"`
				Violations [][3]any `json:"violations"`
			} `json:"expect"`
		}
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		plan, err := ParsePlan(json.RawMessage(c.Plan))
		if err != nil {
			t.Fatal(err)
		}
		env, err := ParseEnvelope(json.RawMessage(c.Envelope))
		if err != nil {
			t.Fatal(err)
		}
		n++
		res := Verify(plan, env, VerifyOptions{Strict: c.Strict, Z3TimeoutMs: 500})
		if res.OK != c.Expect.OK || len(res.Violations) != len(c.Expect.Violations) {
			t.Errorf("plan %s\n got %v %+v\nwant %v %v", c.Plan, res.OK, res.Violations, c.Expect.OK, c.Expect.Violations)
			continue
		}
		for i, v := range res.Violations {
			want := c.Expect.Violations[i]
			step, _ := want[1].(string)
			if v.Stage != want[0] || v.Step != step || v.HasStep != (want[1] != nil) {
				t.Errorf("plan %s: violation %d is %s/%s, oracle %v", c.Plan, i, v.Stage, v.Step, want)
				continue
			}
			words++
			msg := want[2].(string)
			if v.Stage == "delegation" && strings.Contains(msg, "delegation refused") {
				// The oracle names Z3's counterexample; this client proves
				// the same verdict and words its own reason (PW-10).
				continue
			}
			if v.Message != msg {
				t.Errorf("plan %s: message\n got %q\nwant %q", c.Plan, v.Message, msg)
			}
		}
	}
	if n < 300 && path == messagesPath {
		t.Fatalf("only %d cases", n)
	}
	t.Logf("%d cases, %d violations worded", n, words)
}
