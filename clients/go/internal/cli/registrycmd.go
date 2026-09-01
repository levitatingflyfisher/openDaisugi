package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/registry"
	"daisugi-verify/internal/signing"
)

// This file is `daisugi registry init|pull|publish|status|pull-and-tend`
// (opendaisugi/git_pathway_store.py and the CLI over it): a team's shared
// pathway registry as a git repository of signed bundles.

const registryHelp = `Usage: daisugi registry [OPTIONS] COMMAND [ARGS]...

  The shared pathway registry: a git repository of signed pathway bundles.

Commands:
  init           Clone a registry repo to a local directory.
  pull           git pull and materialize new pathway bundles into the local cache.
  publish        Sign + commit + push a local pathway as a bundle to the registry.
  status         Show the local clone's diagnostic info.
  pull-and-tend  Cron-friendly: pull new bundles from the registry, then run tend.
`

func (e *Env) registryCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", registryHelp)
		return nil
	}
	switch args[0] {
	case "init":
		return e.registryInit(args[1:])
	case "pull":
		return e.registryPull(args[1:])
	case "publish":
		return e.registryPublish(args[1:])
	case "status":
		return e.registryStatus(args[1:])
	case "pull-and-tend":
		return e.registryPullAndTend(args[1:])
	}
	e.errf("Usage: daisugi registry [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi registry --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

func (e *Env) defaultRegistry() string { return gateroot.Join(e.home, ".opendaisugi/registry") }

var repoPathOpt = opt{names: []string{"--repo-path"}, value: true, metavar: "PATH", help: "The registry's local clone."}

func (e *Env) registryInit(args []string) error {
	const cmd = "registry init"
	opts := []opt{{names: []string{"--clone-to"}, value: true, metavar: "PATH", help: "Local clone directory."}}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "GIT_URL", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " GIT_URL", "Clone a registry repo to a local directory.", opts)
	}
	if len(p.args) == 0 {
		return e.usageArgs(cmd, "GIT_URL", &usageError{"Missing argument 'git_url'."})
	}
	url := p.args[0]
	cloneTo := gateroot.PathStr(p.str("--clone-to", e.defaultRegistry()))
	// git runs a command for an ext:: or fd:: URL at clone time.
	if strings.HasPrefix(url, "ext::") || strings.HasPrefix(url, "fd::") {
		e.errf("refusing registry URL %s: the git ext::/fd:: transports execute a command and are not allowed.\n",
			pystr.Repr(url))
		return exit(2)
	}
	if exists(cloneTo) && exists(gateroot.Join(cloneTo, ".git")) {
		e.out("already cloned at %s\n", cloneTo)
		return nil
	}
	if err := mkdirParentsPy(gateroot.PathStr(filepath.Dir(cloneTo))); err != nil {
		return e.raise(cmd, err)
	}
	e.out("cloning %s → %s\n", url, cloneTo)
	git, err := lookPath("git", e.env["PATH"])
	if err != nil {
		return e.raise(cmd, &signing.PyError{Type: "FileNotFoundError", Msg: "[Errno 2] No such file or directory: 'git'"})
	}
	c := exec.Command(git, "clone", url, cloneTo)
	c.Args[0] = "git"
	// The allowed protocols are enforced in git itself too: submodules,
	// and any transport not caught above.
	// No inherited GIT_* variable is passed on.
	c.Env = append(registry.Scrubbed(e.Environ), "GIT_ALLOW_PROTOCOL=https:http:ssh:git:file")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, e.Stdout, e.Stderr
	if err := c.Run(); err != nil {
		var x *exec.ExitError
		if errors.As(err, &x) {
			return e.raise(cmd, registry.CalledProcessError([]string{"git", "clone", url, cloneTo}, x.ExitCode()))
		}
		return e.raise(cmd, err)
	}
	e.out("clone ready at %s\n", cloneTo)
	return nil
}

func (e *Env) openRegistry(cmd, repo string, o registry.Options) (*registry.Store, error) {
	o.Environ = e.Environ
	s, err := registry.Open(repo, o)
	if err != nil {
		return nil, e.raise(cmd, err)
	}
	return s, nil
}

func (e *Env) registryPull(args []string) error {
	const cmd = "registry pull"
	opts := []opt{repoPathOpt, {names: []string{"--require-signed"}, neg: "--allow-unsigned",
		help: "Refuse bundles without a valid signature from a trusted signer."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "git pull and materialize new pathway bundles into the local cache.", opts)
	}
	require := !p.flagSet("--require-signed") || p.flag("--require-signed")
	s, err := e.openRegistry(cmd, p.str("--repo-path", e.defaultRegistry()), registry.Options{RequireSigned: require})
	if err != nil {
		return err
	}
	defer s.Close()
	n, err := s.Pull()
	if err != nil {
		return e.raise(cmd, err)
	}
	e.out("pulled; %d new pathway(s) cached\n", n)
	return nil
}

func (e *Env) registryPublish(args []string) error {
	const cmd = "registry publish"
	opts := []opt{
		repoPathOpt,
		{names: []string{"--private-key"}, value: true, metavar: "PATH", help: "Path to a base64 ed25519 private key file."},
		{names: []string{"--public-key"}, value: true, metavar: "PATH", help: "Path to a base64 ed25519 public key file."},
		{names: []string{"--publisher"}, value: true, metavar: "TEXT", help: "Human-readable publisher id stamped on the bundle."},
		{names: []string{"--push"}, neg: "--no-push", help: "Push after the commit."},
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "PATHWAY_ID", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " PATHWAY_ID", "Sign + commit + push a local pathway as a bundle to the registry.", opts)
	}
	if len(p.args) == 0 {
		return e.usageArgs(cmd, "PATHWAY_ID", &usageError{"Missing argument 'pathway_id'."})
	}
	for _, o := range []string{"--private-key", "--public-key"} {
		if !p.has(o) {
			return e.usageArgs(cmd, "PATHWAY_ID", &usageError{"Missing option '" + o + "'."})
		}
	}
	id := p.args[0]
	priv, err := readTextStrip(gateroot.PathStr(p.str("--private-key", "")))
	if err != nil {
		return e.raise(cmd, err)
	}
	pub, err := readTextStrip(gateroot.PathStr(p.str("--public-key", "")))
	if err != nil {
		return e.raise(cmd, err)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", gateroot.Join(e.home, ".opendaisugi")))
	local, err := pathways.Open(gateroot.Join(dataDir, "pathways.db"))
	if err != nil {
		return e.raise(cmd, err)
	}
	all, err := local.ReadAll(func(x string) bool { return x == id })
	local.Close()
	if err != nil {
		return e.raise(cmd, err)
	}
	var pw *pathways.Pathway
	for _, x := range all {
		if x.ID() == id {
			pw = x.Full()
			break
		}
	}
	if pw == nil {
		e.errf("error: no pathway %s in local store\n", id)
		return exit(1)
	}
	publisher := p.str("--publisher", "opendaisugi-instance")
	s, err := e.openRegistry(cmd, p.str("--repo-path", e.defaultRegistry()),
		registry.Options{Priv: &priv, Pub: &pub, Publisher: publisher, RequireSigned: true})
	if err != nil {
		return err
	}
	defer s.Close()
	push := !p.flagSet("--push") || p.flag("--push")
	now := float64(time.Now().UnixNano()) / 1e9
	hash, err := s.Publish(pw, now, push)
	if err != nil {
		return e.raise(cmd, err)
	}
	e.out("%s\n", hash)
	return nil
}

func (e *Env) registryStatus(args []string) error {
	const cmd = "registry status"
	opts := []opt{repoPathOpt, {names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Show the local clone's diagnostic info.", opts)
	}
	s, err := e.openRegistry(cmd, p.str("--repo-path", e.defaultRegistry()), registry.Options{RequireSigned: true})
	if err != nil {
		return err
	}
	defer s.Close()
	st, err := s.Status()
	if err != nil {
		return e.raise(cmd, err)
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.Dumps(st, true))
		return nil
	}
	for _, k := range st.Keys() {
		e.out("  %s: %s\n", k, pmodelStr(st.Value(k)))
	}
	return nil
}

// pmodelStr is str(v) for a status value: a str as it is, else its repr.
func pmodelStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pmodel.Repr(v)
}

func (e *Env) registryPullAndTend(args []string) error {
	const cmd = "registry pull-and-tend"
	opts := []opt{repoPathOpt, {names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Cron-friendly: pull new bundles from the registry, then run tend.", opts)
	}
	s, err := e.openRegistry(cmd, p.str("--repo-path", e.defaultRegistry()), registry.Options{RequireSigned: true})
	if err != nil {
		return err
	}
	n, err := s.Pull()
	s.Close()
	if err != nil {
		return e.raise(cmd, err)
	}
	e.out("pulled; %d new pathway(s) cached\n", n)
	dataDir := gateroot.PathStr(p.str("--data-dir", gateroot.Join(e.home, ".opendaisugi")))
	rep, err := e.runTendQuiet(dataDir)
	if err != nil {
		return err
	}
	if rep != nil {
		e.out("tend: created=%d updated=%d skipped=%d\n", rep.Created, rep.Updated, rep.Skipped)
	}
	return nil
}
