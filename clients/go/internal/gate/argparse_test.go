package gate

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
)

func parseOutcome(argv []string, columns int) (o options, ex *argvExit, unp string) {
	defer func() {
		if p := recover(); p != nil {
			if u, ok := p.(unportedCall); ok {
				unp = u.reason
				return
			}
			panic(p)
		}
	}()
	ex = catchArgvExit(func() { o = parseArgv(argv, columns) })
	return
}

func TestArgparseReadsTheGateFlags(t *testing.T) {
	o, ex, _ := parseOutcome([]string{"--mo", "enforce", "--verify=2.5", "--ask", "--session", "-1"}, 80)
	if ex != nil || *o.mode != "enforce" || *o.verifyTimeout != 2.5 || !o.ask || *o.session != "-1" {
		t.Fatalf("got %+v %+v", o, ex)
	}
	_, ex, _ = parseOutcome([]string{"--as"}, 80)
	want := "usage: opendaisugi.gate [-h] [--mode {shadow,enforce}] [--root ROOT]\n" +
		"                        [--format FMT] [--verify-timeout VERIFY_TIMEOUT]\n" +
		"                        [--captures-root CAPTURES_ROOT] [--session SESSION]\n" +
		"                        [--ask] [--ask-timeout ASK_TIMEOUT] [--checkpoints]\n" +
		"opendaisugi.gate: error: ambiguous option: --as could match --ask, --ask-timeout\n"
	if ex == nil || ex.code != 2 || ex.stderr != want {
		t.Fatalf("got %+v", ex)
	}
	_, ex, _ = parseOutcome([]string{"--help"}, 80)
	if ex == nil || ex.code != 0 || !strings.HasPrefix(ex.stdout, "usage: ") {
		t.Fatalf("got %+v", ex)
	}
}

// TestArgparseAgainstOracle reads testdata/argv_oracle.py's output from
// DAISUGI_ARGV_ORACLE (run with HOME=/home/user).
func TestArgparseAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_ARGV_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_ARGV_ORACLE is not set")
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
			Argv    []string       `json:"argv"`
			Columns string         `json:"columns"`
			NS      map[string]any `json:"ns"`
			Exit    *int           `json:"exit"`
			Stdout  string         `json:"stdout"`
			Stderr  string         `json:"stderr"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		o, ex, unp := parseOutcome(row.Argv, terminalColumns(map[string]string{"COLUMNS": row.Columns}))
		if unp != "" {
			skipped++
			continue
		}
		var got, want string
		if row.Exit != nil {
			b, _ := json.Marshal(map[string]any{"exit": *row.Exit, "stdout": row.Stdout, "stderr": row.Stderr})
			want = string(b)
		} else {
			b, _ := json.Marshal(row.NS)
			want = string(b)
		}
		if ex != nil {
			b, _ := json.Marshal(map[string]any{"exit": ex.code, "stdout": ex.stdout, "stderr": ex.stderr})
			got = string(b)
		} else {
			ns := map[string]any{"mode": o.mode, "root": "/home/user/.opendaisugi/gate", "fmt": "claude",
				"verify_timeout": "10.0", "captures_root": nil, "session": o.session, "ask": o.ask,
				"ask_timeout": "90.0", "checkpoints": o.checkpoints}
			if o.root != nil {
				ns["root"] = pathStr(*o.root)
			}
			if o.format != nil {
				ns["fmt"] = *o.format
			}
			if o.verifyTimeout != nil {
				ns["verify_timeout"] = pmodel.FloatRepr(*o.verifyTimeout)
			}
			if o.askTimeout != nil {
				ns["ask_timeout"] = pmodel.FloatRepr(*o.askTimeout)
			}
			if o.captures != nil {
				ns["captures_root"] = pathStr(*o.captures)
			}
			b, _ := json.Marshal(ns)
			got = string(b)
		}
		if got != want {
			bad++
			if bad < 15 {
				t.Errorf("%q at %q:\n got %s\nwant %s", row.Argv, row.Columns, got, want)
			}
		}
	}
	t.Logf("%d argv lists, %d undecided, %d differ", n, skipped, bad)
}
