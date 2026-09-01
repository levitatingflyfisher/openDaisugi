package gate

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"daisugi-verify/internal/config"
)

// The CLI's config reader (internal/config, for gate status) and the
// gate's (loadConfig, for gate check) must give the same mode and client
// wherever both decide, or status could report a mode the hook does not
// use. Reads DAISUGI_PYYAML_ORACLE's texts.
func TestConfigReadersAgree(t *testing.T) {
	path := os.Getenv("DAISUGI_PYYAML_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_PYYAML_ORACLE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	dir := t.TempDir()
	n, both, bad := 0, 0, 0
	for sc.Scan() {
		var row struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(row.Text), 0o600); err != nil {
			t.Fatal(err)
		}
		kind, got := loadOutcome(t, row.Text)
		cfg, cerr := config.Load(p)
		if kind == "unported" || errors.Is(cerr, config.ErrUnsupported) {
			continue
		}
		both++
		if (kind == "exc") != (cerr != nil) || (cerr == nil && (cfg.GateMode != got.mode || cfg.VerifierClient != got.client)) {
			bad++
			if bad < 10 {
				t.Errorf("%q: gate %s %+v, config %+v %v", row.Text, kind, got, cfg, cerr)
			}
		}
	}
	t.Logf("%d texts, %d both read, %d differ", n, both, bad)
}
