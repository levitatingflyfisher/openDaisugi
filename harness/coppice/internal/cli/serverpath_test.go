package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAServerWithoutDaisugiOrTheHarnessOnPathWarnsAndNamesThePath(t *testing.T) {
	none := func(string) (string, error) { return "", errors.New("no") }
	got := pathWarnings(none, "/usr/bin:/bin", "daisugi", "claude")
	if len(got) != 2 {
		t.Fatalf("warnings = %v", got)
	}
	for _, w := range got {
		if !strings.Contains(w, "/usr/bin:/bin") || !strings.Contains(w, "coppice server install") {
			t.Errorf("warning lacks the PATH or the fix: %s", w)
		}
	}
	all := func(n string) (string, error) { return "/x/" + n, nil }
	if got := pathWarnings(all, "/usr/bin", "daisugi", "claude"); len(got) != 0 {
		t.Errorf("warned with everything found: %v", got)
	}
}

func TestTheServerUnitCarriesThePathWithLocalBin(t *testing.T) {
	u, _ := serverUnit("/opt/coppice", "/usr/bin:/bin", "/home/u", []string{"--socket", "/run/s k"})
	for _, want := range []string{
		`Environment="PATH=/home/u/.local/bin:/usr/bin:/bin"`,
		`ExecStart=/opt/coppice --socket "/run/s k" server start --foreground`,
		"Restart=on-failure", "WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if u2, _ := serverUnit("/c", "/home/u/.local/bin:/bin", "/home/u", nil); strings.Count(u2, "/home/u/.local/bin") != 1 {
		t.Errorf("local bin doubled:\n%s", u2)
	}
}

func TestTheServerUnitEscapesPercentAndRefusesQuoteAndNewline(t *testing.T) {
	u, err := serverUnit("/opt/co%ppice", "/a%h/bin:/bin", "/home/u", []string{"--socket", "/run/100%/s k"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, `/a%%h/bin`) || !strings.Contains(u, `/opt/co%%ppice`) || !strings.Contains(u, `/run/100%%/s k`) {
		t.Errorf("percent not doubled:\n%s", u)
	}
	u, err = serverUnit("/opt/co$HOME", "/bin", "/home/u", []string{"--data-dir", "/d/$X y"})
	if err != nil || !strings.Contains(u, "ExecStart=/opt/co$$HOME --data-dir \"/d/$$X y\" server") {
		t.Errorf("dollar not doubled in ExecStart (%v):\n%s", err, u)
	}
	for _, bad := range []string{"/a\"b:/bin", "/a\n/b"} {
		if _, err := serverUnit("/c", bad, "/home/u", nil); err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Errorf("PATH %q: %v", bad, err)
		}
	}
}

func TestServerInstallWritesTheUnitUnderTheConfigHome(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	var out, errb strings.Builder
	c := &CLI{Socket: "/run/u/coppice/server.sock", DataDir: "/d", Out: &out, Err: &errb}
	if code := c.serverInstall(false); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, err := os.ReadFile(filepath.Join(cfg, "systemd", "user", "coppice-server.service"))
	if err != nil || !strings.Contains(string(b), "server start --foreground") {
		t.Fatalf("unit: %v %s", err, b)
	}
	if !strings.Contains(out.String(), "systemctl --user") {
		t.Errorf("no next step: %s", out.String())
	}
}
