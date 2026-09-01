package gate

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// loadOutcome runs loadConfig on text and reports what load_config would
// give: the two fields, "exc" for a raise, or "unported".
func loadOutcome(t *testing.T, text string) (string, loadedConfig) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var c loadedConfig
	kind := "ok"
	func() {
		defer func() {
			if x := recover(); x != nil {
				if _, isUnported := x.(unportedCall); isUnported {
					kind = "unported"
					return
				}
				panic(x)
			}
		}()
		if exc := catch(func() { c = loadConfig(p) }); exc != nil {
			kind = "exc"
		}
	}()
	return kind, c
}

func TestConfigLoadsAsLoadConfigDoes(t *testing.T) {
	for text, want := range map[string]loadedConfig{
		"verifier_client: go\n":                   {"shadow", "go", ""},
		"gate_mode: enforce\n# c\n":               {"enforce", "python", ""},
		"gate_mode: 'enforce'  # quoted\n":        {"enforce", "python", ""},
		"floor:\n  backend: tmux\ngate_mode: x\n": {"x", "python", ""},
		"": {"shadow", "python", ""},
	} {
		kind, got := loadOutcome(t, text)
		if kind != "ok" || got != want {
			t.Errorf("%q: %s %+v, want %+v", text, kind, got, want)
		}
	}
	// A bool where a str belongs, an empty data_dir, a bad floor: Config
	// raises, so the gate falls back.
	for _, text := range []string{"gate_mode: on\n", "data_dir:\n", "floor: 3\n", "max_task_chars: 1.5\n"} {
		if kind, _ := loadOutcome(t, text); kind != "exc" {
			t.Errorf("%q: %s, want a raise", text, kind)
		}
	}
	if kind, _ := loadOutcome(t, "gate_mode: [enforce]\n"); kind != "unported" {
		t.Errorf("a flow sequence was read")
	}
}

// TestConfigAgainstOracle reads the pyyaml oracle's config answers from
// DAISUGI_PYYAML_ORACLE (internal/pyyaml/testdata/oracle.py).
func TestConfigAgainstOracle(t *testing.T) {
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
	n, skipped, bad := 0, 0, 0
	for sc.Scan() {
		var row struct {
			Text   string            `json:"text"`
			Config map[string]string `json:"config"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		kind, got := loadOutcome(t, row.Text)
		switch {
		case kind == "unported":
			skipped++
		case kind == "exc":
			if _, raised := row.Config["exc"]; !raised {
				bad++
				t.Errorf("%q: raised, oracle gave %v", row.Text, row.Config)
			}
		default:
			if row.Config["gate_mode"] != got.mode || row.Config["verifier_client"] != got.client {
				bad++
				t.Errorf("%q: got %+v, oracle gave %v", row.Text, got, row.Config)
			}
		}
		if bad > 20 {
			t.FailNow()
		}
	}
	t.Logf("%d configs, %d outside the subset, %d differ", n, skipped, bad)
}
