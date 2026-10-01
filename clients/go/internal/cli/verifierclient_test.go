package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F14: an old config.yaml that names a verifier client this box lacks
// gets one clear line from gate status, and the command still succeeds.
func TestGateStatusSaysWhenTheNamedVerifierClientIsMissing(t *testing.T) {
	for _, c := range []struct {
		client string
		want   string
	}{
		{"python", ""},
		{"rust", "verifier_client: rust is not installed here"},
		{"cobol", "verifier_client: cobol names no client"},
	} {
		home := t.TempDir()
		root := filepath.Join(home, ".opendaisugi", "gate")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := "verifier_client: " + c.client + "\ngate_mode: enforce\n"
		if err := os.WriteFile(filepath.Join(home, ".opendaisugi", "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, errs := run(t, home, "", "gate", "status", "--root", root)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", c.client, code, errs)
		}
		if c.want == "" {
			if strings.Contains(errs, "verifier_client") {
				t.Errorf("%s: unexpected warning: %s", c.client, errs)
			}
			continue
		}
		if !strings.Contains(errs, c.want) || strings.Count(errs, "verifier_client") != 2 {
			t.Errorf("%s: want one line with %q, got %q", c.client, c.want, errs)
		}
	}
}
