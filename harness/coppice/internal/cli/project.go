package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/opendaisugi/coppice/internal/config"
)

// runProject is coppice project add|list|rm. add and rm edit coppice.toml
// directly, on this machine, with no socket involved. list dials the
// server, since its answer also carries the directories panes have
// recently started in, not just the pinned ones this file names.
func (c *CLI) runProject(remote string, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(c.Err, "project needs a verb: add, list or rm")
		return 1
	}
	sub, rest := argv[0], argv[1:]
	if (sub == "add" || sub == "rm") && remote != "" {
		fmt.Fprintf(c.Err, "coppice project %s edits the local coppice.toml; it is not wired for "+
			"--remote yet. SSH into the host and run coppice project %s there.\n", sub, sub)
		return 1
	}
	switch sub {
	case "add":
		return c.projectAdd(rest)
	case "rm":
		return c.projectRemove(rest)
	case "list":
		return c.runVerb(remote, append([]string{"project", "list"}, rest...))
	default:
		fmt.Fprintf(c.Err, "unknown project verb %q. Known verbs: add, list, rm\n", sub)
		return 1
	}
}

func (c *CLI) projectAdd(argv []string) int {
	if len(argv) != 1 {
		fmt.Fprintln(c.Err, "project add needs one directory")
		return 1
	}
	abs, err := filepath.Abs(argv[0])
	if err != nil {
		fmt.Fprintf(c.Err, "cannot resolve %q: %v\n", argv[0], err)
		return 1
	}
	fi, statErr := os.Stat(abs)
	if statErr != nil {
		fmt.Fprintf(c.Err, "%s does not exist\n", abs)
		return 1
	}
	if !fi.IsDir() {
		fmt.Fprintf(c.Err, "%s is not a directory\n", abs)
		return 1
	}
	resolved := resolveSymlinks(abs)
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read %s: %v\n", config.Path(), err)
		return 1
	}
	for _, p := range cfg.Projects {
		if p == resolved {
			fmt.Fprintf(c.Out, "%s is already a project\n", resolved)
			return 0
		}
	}
	cfg.Projects = append(cfg.Projects, resolved)
	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(c.Err, "cannot write %s: %v\n", config.Path(), err)
		return 1
	}
	fmt.Fprintf(c.Out, "added %s\n", resolved)
	return 0
}

// resolveSymlinks is path with every symlink in it followed, so the same
// directory pinned once through a symlink and once through its real path
// is stored, and matched, as one path rather than two. A path that cannot
// be resolved - already gone, say - is returned unchanged rather than
// failing an operation that has already confirmed the caller meant this
// exact directory.
func resolveSymlinks(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

func (c *CLI) projectRemove(argv []string) int {
	if len(argv) != 1 {
		fmt.Fprintln(c.Err, "project rm needs one directory")
		return 1
	}
	abs, err := filepath.Abs(argv[0])
	if err != nil {
		fmt.Fprintf(c.Err, "cannot resolve %q: %v\n", argv[0], err)
		return 1
	}
	resolved := resolveSymlinks(abs)
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintf(c.Err, "cannot read %s: %v\n", config.Path(), err)
		return 1
	}
	out := make([]string, 0, len(cfg.Projects))
	found := false
	for _, p := range cfg.Projects {
		if p == resolved {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		fmt.Fprintf(c.Err, "%s is not a project. Run: coppice project list\n", resolved)
		return 1
	}
	cfg.Projects = out
	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(c.Err, "cannot write %s: %v\n", config.Path(), err)
		return 1
	}
	fmt.Fprintf(c.Out, "removed %s\n", abs)
	return 0
}
