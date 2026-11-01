package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opendaisugi/coppice/internal/config"
)

// pathWarnings are the lines a starting server prints when it cannot find
// daisugi or the default harness on its own PATH. A server started over
// ssh, or by a service manager, often has PATH=/usr/local/bin:/usr/bin:/bin
// and finds neither, and a later `coppice open` says only that it found no
// default harness. Each line names the PATH the server has.
func pathWarnings(lookPath func(string) (string, error), pathEnv string, daisugi, harness string) []string {
	var out []string
	miss := func(what, name string) {
		out = append(out, fmt.Sprintf("%s %q is not on this server's PATH (%s). Start the server from a shell that has it, or run: coppice server install",
			what, name, pathEnv))
	}
	if _, err := lookPath(daisugi); err != nil {
		miss("daisugi", daisugi)
	}
	if harness != "" {
		if _, err := lookPath(harness); err != nil {
			miss("the default harness", harness)
		}
	}
	return out
}

// serverUnit is the systemd user unit for the server: the PATH it was
// installed from (with ~/.local/bin added), so the server finds daisugi and
// the harnesses in a session no login shell started.
//
// systemd reads % in a unit as a specifier, so % is written as %%. It also
// expands $VAR in ExecStart, so $ is written as $$ there. A PATH
// holding a double quote or a newline cannot sit inside Environment="...",
// so the unit is refused.
func serverUnit(exe, pathEnv, home string, args []string) (string, error) {
	if strings.ContainsAny(pathEnv, "\"\n\r") {
		return "", fmt.Errorf("PATH holds a double quote or a newline, which a systemd unit cannot carry; fix PATH and run coppice server install again")
	}
	parts := strings.Split(pathEnv, ":")
	local := filepath.Join(home, ".local", "bin")
	seen := false
	for _, p := range parts {
		if p == local {
			seen = true
		}
	}
	if !seen {
		parts = append([]string{local}, parts...)
	}
	argv := append([]string{exe}, args...)
	argv = append(argv, "server", "start", "--foreground")
	for i, a := range argv {
		a = strings.ReplaceAll(strings.ReplaceAll(a, "%", "%%"), "$", "$$")
		argv[i] = a
		if strings.ContainsAny(a, " \t\"'\\") {
			argv[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
		}
	}
	return "[Unit]\nDescription=coppice server\n\n[Service]\n" +
		"Environment=\"PATH=" + strings.ReplaceAll(strings.Join(parts, ":"), "%", "%%") + "\"\n" +
		"ExecStart=" + strings.Join(argv, " ") + "\n" +
		"Restart=on-failure\n\n[Install]\nWantedBy=default.target\n", nil
}

// serverInstall writes ~/.config/systemd/user/coppice-server.service (under
// XDG_CONFIG_HOME) and says how to start it. It runs no systemctl.
func (c *CLI) serverInstall(asJSON bool) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(c.Err, "cannot find the coppice binary:", err)
		return 1
	}
	if r, e := filepath.EvalSymlinks(exe); e == nil {
		exe = r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(c.Err, "cannot find the home dir:", err)
		return 1
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	dir := filepath.Join(cfg, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	var args []string
	if c.Socket != "" {
		args = append(args, "--socket", c.Socket)
	}
	if c.DataDir != "" {
		args = append(args, "--data-dir", c.DataDir)
	}
	path := filepath.Join(dir, "coppice-server.service")
	unit, err := serverUnit(exe, os.Getenv("PATH"), home, args)
	if err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(c.Err, err)
		return 1
	}
	fmt.Fprintf(c.Out, "wrote %s\nStart it: systemctl --user daemon-reload && systemctl --user enable --now coppice-server\n", path)
	return 0
}

// warnPath prints the pathWarnings of this process, once, to stderr (the
// server log, for a detached server).
func (c *CLI) warnPath() {
	harness := ""
	if cfg, found, err := config.Load(); err == nil && found && cfg.Default != "" {
		harness = cfg.Harness[cfg.Default].Command
		if harness == "" {
			harness = cfg.Default
		}
	}
	bin := os.Getenv("COPPICE_DAISUGI_BIN")
	if bin == "" {
		bin = "daisugi"
	}
	for _, w := range pathWarnings(exec.LookPath, os.Getenv("PATH"), bin, harness) {
		fmt.Fprintln(c.Err, "warning:", w)
	}
}
