package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opendaisugi/coppice/internal/config"
)

// runNew is coppice new [PROJECT] [--no-attach]: the default harness, in a
// project by name or path, or in the caller's own directory, then the same
// attach-or-print-the-id tail as coppice open.
func (c *CLI) runNew(remote string, argv []string) int {
	if remote != "" {
		fmt.Fprintln(c.Err, "coppice new needs a local socket; it is not wired for --remote yet. "+
			"SSH into the host and run coppice new there.")
		return 1
	}
	noAttach := false
	project := ""
	for _, a := range argv {
		if a == "--no-attach" {
			noAttach = true
			continue
		}
		if project != "" {
			fmt.Fprintln(c.Err, "coppice new takes at most one project name or path")
			return 1
		}
		project = a
	}
	cwd, code := c.resolveNewCwd(remote, project)
	if code != 0 {
		return code
	}
	name, code := c.defaultHarnessName()
	if code != 0 {
		return code
	}
	return c.openAt(remote, []string{name}, false, cwd, noAttach)
}

// resolveNewCwd is coppice new's own cwd rule: with no project, the
// caller's own directory, never a project.list guess - the operator asked
// to work right here. With one, project.list's exact base-name match
// first, then its exact path match. Two projects can share a base name -
// different parents, same leaf directory name - so every name match is
// collected before anything is decided: one match wins, more than one is
// refused by name entirely rather than guessing which the caller meant.
func (c *CLI) resolveNewCwd(remote, project string) (string, int) {
	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(c.Err, "cannot read the working directory: %v\n", err)
			return "", 1
		}
		return cwd, 0
	}
	cl, code := c.dial(remote)
	if code != 0 {
		return "", code
	}
	res, err := cl.Do("project.list", map[string]any{})
	if err != nil {
		return "", c.reportDoErr(cl, err)
	}
	_ = cl.Close()
	rows, _ := res["projects"].([]any)
	var byName []string
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if name, _ := m["name"].(string); name == project {
			if path, _ := m["path"].(string); path != "" {
				byName = append(byName, path)
			}
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], 0
	case 0:
		// no name match at all: fall through to a path match, below.
	default:
		fmt.Fprintf(c.Err, "%d projects are named %q: %s. Pass the path instead.\n",
			len(byName), project, strings.Join(byName, ", "))
		return "", 1
	}
	abs, err := filepath.Abs(project)
	if err == nil {
		for _, r := range rows {
			m, _ := r.(map[string]any)
			if path, _ := m["path"].(string); path == abs {
				return path, 0
			}
		}
	}
	fmt.Fprintf(c.Err, "no project named or at %q. Run: coppice project list\n", project)
	return "", 1
}

// defaultHarnessName is the harness coppice new opens when the caller
// named none: coppice.toml's default line. With no config file at all,
// this falls back to the first harness Discover finds on PATH - the same
// fallback open itself uses for a bare word with no config at all. A
// config file that DOES exist but names no default is a different case:
// guessing from PATH here would silently start a harness the operator
// never chose, so this refuses instead, with the exact fix the server
// itself gives pane.create for the same gap.
func (c *CLI) defaultHarnessName() (string, int) {
	cfg, found, err := config.Load()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read %s: %v\n", config.Path(), err)
		return "", 1
	}
	if found {
		if cfg.Default != "" {
			return cfg.Default, 0
		}
		fmt.Fprintf(c.Err, "no default harness in %s. Run coppice open HARNESS once to set one.\n",
			config.Path())
		return "", 1
	}
	disc := config.Discover(exec.LookPath)
	if len(disc) == 0 {
		fmt.Fprintln(c.Err, config.AppOnlyNote(nil))
		return "", 1
	}
	return disc[0].Name, 0
}
