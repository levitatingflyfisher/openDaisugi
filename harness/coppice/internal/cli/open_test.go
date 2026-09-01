package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

// openCLI runs coppice with argv on a pipe stdin against sock.
func openCLI(t *testing.T, sock, dir string, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	c := &CLI{Version: "test", Socket: sock, DataDir: dir, In: strings.NewReader(""), Out: &out, Err: &errb}
	code := c.Run(argv)
	return code, out.String(), errb.String()
}

// saveFakeh writes a config whose one harness, fakeh, runs sh -c 'sleep 30'.
func saveFakeh(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
}

// paneRows lists the panes over the socket as rows.
func paneRows(t *testing.T, sock, dir string) []map[string]any {
	t.Helper()
	code, out, errb := runCLI(t, sock, dir, "pane", "list", "--json")
	if code != 0 {
		t.Fatalf("pane list exit %d: %s", code, errb)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	raw, _ := res["panes"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		rows = append(rows, m)
	}
	return rows
}

func assertOpened(t *testing.T, code int, out, errb string) {
	t.Helper()
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s %s", code, out, errb)
	}
	if !strings.HasPrefix(out, "opened ") || !strings.Contains(out, "p1") ||
		!strings.Contains(out, "attach needs a terminal.") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestBareHarnessNameOpensAPaneOnAPipe(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, out, errb := openCLI(t, sock, dir, "fakeh")
	assertOpened(t, code, out, errb)
	rows := paneRows(t, sock, dir)
	if len(rows) != 1 || rows[0]["harness"] != "fakeh" {
		t.Fatalf("panes = %v", rows)
	}
	cmd, _ := rows[0]["cmd"].([]any)
	if len(cmd) != 3 || cmd[0] != "sh" {
		t.Fatalf("cmd = %v", cmd)
	}
}

func TestOpenLongFormOpensAPaneOnAPipe(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, out, errb := openCLI(t, sock, dir, "open", "fakeh")
	assertOpened(t, code, out, errb)
	if rows := paneRows(t, sock, dir); len(rows) != 1 {
		t.Fatalf("panes = %v", rows)
	}
}

func TestOpenPassesExtraArgsToTheHarness(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, out, errb := openCLI(t, sock, dir, "fakeh", "--flag", "x")
	assertOpened(t, code, out, errb)
	rows := paneRows(t, sock, dir)
	cmd, _ := rows[0]["cmd"].([]any)
	if len(cmd) != 5 || cmd[3] != "--flag" || cmd[4] != "x" {
		t.Fatalf("cmd = %v", cmd)
	}
}

func TestBareNameWithNoConfigAndNothingOnPathNamesTheApps(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	emptyPath(t)
	code, out, errb := openCLI(t, sock, dir, "fakeh")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s %s", code, out, errb)
	}
	if !strings.Contains(errb, "apps") || !strings.Contains(errb, "pane list") {
		t.Fatalf("stderr does not name the apps and the usage: %q", errb)
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config was written with nothing found")
	}
}

func TestBareNameMissingFromTheConfigListsTheHarnesses(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, out, errb := openCLI(t, sock, dir, "nope")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s %s", code, out, errb)
	}
	for _, want := range []string{`unknown command "nope"`, "Harnesses in", config.Path(), "fakeh", "pane list"} {
		if !strings.Contains(errb, want) {
			t.Fatalf("stderr lacks %q: %q", want, errb)
		}
	}
	if rows := paneRows(t, sock, dir); len(rows) != 0 {
		t.Fatalf("a pane was opened: %v", rows)
	}
}

// The long form with an unknown name says the same line, without usage.
func TestOpenLongFormMissingFromTheConfigListsTheHarnesses(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, _, errb := openCLI(t, sock, dir, "open", "nope")
	if code != 1 || !strings.Contains(errb, "Harnesses in") || !strings.Contains(errb, "fakeh") {
		t.Fatalf("exit %d: %q", code, errb)
	}
}

func TestBareNameWithNoConfigFoundOnPathWritesTheConfigAndOpens(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeHarnessOnPath(t, "claude")
	code, out, errb := openCLI(t, sock, dir, "claude")
	if code != 0 || !strings.Contains(out, "opened") {
		t.Fatalf("exit %d: %q %q", code, out, errb)
	}
	b, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatalf("no config written: %v", err)
	}
	if !strings.Contains(string(b), `default = "claude"`) {
		t.Fatalf("config = %q", b)
	}
	rows := paneRows(t, sock, dir)
	if len(rows) != 1 || rows[0]["harness"] != "claude" {
		t.Fatalf("panes = %v", rows)
	}
}

// With no config and a PATH that holds a harness, a word that is not on
// PATH is an unknown command with the usage, the same as before.
func TestBareUnknownWordWithNoConfigKeepsTheUsage(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeHarnessOnPath(t, "claude")
	code, _, errb := openCLI(t, sock, dir, "teleport")
	if code != 1 || !strings.Contains(errb, `unknown command "teleport"`) || !strings.Contains(errb, "pane list") {
		t.Fatalf("exit %d: %q", code, errb)
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config was written for an unknown word")
	}
}

// The long form with no config and a name not on PATH names what is.
func TestOpenLongFormNotOnPathNamesWhatIs(t *testing.T) {
	sock, dir := liveServer(t)
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeHarnessOnPath(t, "claude")
	code, _, errb := openCLI(t, sock, dir, "open", "nope")
	if code != 1 || !strings.Contains(errb, "nope is not on PATH. Harnesses found: claude.") {
		t.Fatalf("exit %d: %q", code, errb)
	}
}

func TestOpenWithNoNameTeaches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, _, errb := openCLI(t, dir+"/none.sock", dir, "open")
	if code != 1 || !strings.Contains(errb, "open needs a harness name") {
		t.Fatalf("exit %d: %q", code, errb)
	}
}

func TestOpenWithRemoteIsRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	code, _, errb := openCLI(t, dir+"/none.sock", dir, "--remote", "ssh://host", "fakeh")
	if code != 1 || !strings.Contains(errb, "not wired for --remote") {
		t.Fatalf("exit %d: %q", code, errb)
	}
	assertNoAutostartArtifacts(t, dir)
}

// The --remote refusal comes before the config is read and before PATH
// is searched, so it is the one reason the user sees, and nothing is
// written.
func TestOpenWithRemoteIsRefusedBeforeTheConfigIsRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	emptyPath(t)
	code, _, errb := openCLI(t, dir+"/none.sock", dir, "--remote", "ssh://host", "fakeh")
	if code != 1 || !strings.Contains(errb, "not wired for --remote") {
		t.Fatalf("exit %d: %q", code, errb)
	}
	if strings.Contains(errb, "apps") {
		t.Fatalf("PATH was searched before the --remote refusal: %q", errb)
	}
	if _, ok, _ := config.Load(); ok {
		t.Fatal("a config was written under --remote")
	}
	assertNoAutostartArtifacts(t, dir)
}

func TestABrokenConfigStopsOpen(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	saveFakeh(t)
	if err := os.WriteFile(config.Path(), []byte("default = [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := openCLI(t, dir+"/none.sock", dir, "fakeh")
	if code != 1 || !strings.Contains(errb, config.Path()) {
		t.Fatalf("exit %d: %q", code, errb)
	}
}

func TestUsageNamesTheShellForms(t *testing.T) {
	lines := strings.Split(usage, "\n")
	if len(lines) < 5 {
		t.Fatalf("usage is short: %q", usage)
	}
	if lines[3] != "  coppice claude [ARGS...]                 open that harness here and attach" {
		t.Fatalf("usage line 4 = %q", lines[3])
	}
	if lines[4] != "  coppice open HARNESS [ARGS...]           the long form" {
		t.Fatalf("usage line 5 = %q", lines[4])
	}
}
