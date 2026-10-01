package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/fence"
	"daisugi-verify/internal/fenceruns"
	"daisugi-verify/internal/gateroot"
)

const fenceHelp = `Usage: daisugi fence [OPTIONS] [--] COMMAND [ARGS]...

  Run any agent harness under a Landlock fence. The kernel then holds the
  line the gate only reads: the harness and every process it starts can
  read and write the workspace, the harness's own directory and the temp
  dir, read the system, and reach nothing else under HOME. ~/.ssh, ~/.aws,
  ~/.gnupg and the other secret directories stay shut, and the gate's own
  state is never writable.

  The temp dir is a private one on disk, made for this run: /tmp is not
  reachable. Unix sockets connect only in the gate root and the coppice
  socket's directory; abstract sockets and signals stay inside the fence.
  The gate hook inside the fence asks the resident gate, which the fence
  starts outside it when the gate is on.

  The fence adds one exec and a few system calls at start and nothing per
  call. It refuses to start the harness when the kernel has no usable
  Landlock (Linux 6.2 or newer with landlock in lsm=), or a Landlock older
  than ABI 9 unless --allow-weak-ipc is given.

  The network is not limited in this version: Landlock limits ports, not
  hosts. A harness under the fence can still reach any host. Nor are
  chmod, chown and other metadata calls.

Options:
  --harness NAME           Whose own paths to allow: claude, codex, pi, opencode,
                           sprig. Default: the command's name.
  --workspace DIR          The directory to read and write. Default: the current one.
  --allow PATH[:rw]        Also allow PATH, read only unless it ends in :rw. Repeatable.
                           Only --allow opens a secret directory. --allow /tmp:rw
                           keeps the shared /tmp as the temp dir.
  --workdir DIR            Also read and write DIR, checked like the workspace.
                           Repeatable.
  --envelope FILE          Read file_read and file_write from this envelope. Default:
                           the gate's default envelope.
  --gate-root DIR          The gate root, when it is not under the data dir.
  --tmpdir DIR             Use DIR (made 0700) as the private temp dir. The caller
                           removes it. Default: a new one under the data dir.
  --coppice-socket PATH    Let the harness connect to the coppice socket at PATH.
  --keep-fd N              Keep descriptor N open in the harness. Repeatable.
  --allow-weak-ipc         Fence the files only on a kernel before Landlock ABI 9.
  --print                  Show the rules this would apply, and run nothing.
  --json                   With --print, write JSON.
  --no-fence               Run COMMAND with no fence.
  --help                   Show this message and exit.
`

// fenceCmd is `daisugi fence`. It reads no config and loads nothing it
// does not need: the cost of the fence is one exec.
func (e *Env) fenceCmd(args []string) error {
	var o fence.Options
	var print, asJSON, noFence, weakIPC bool
	var keep []int
	planFrom := ""
	i := 0
parse:
	for ; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
				return v, nil
			}
			if i+1 >= len(args) {
				return "", &usageError{"Option '" + a + "' requires an argument."}
			}
			i++
			return args[i], nil
		}
		name, _, _ := strings.Cut(a, "=")
		var v string
		var err error
		switch name {
		case "--":
			i++
			break parse
		case "--help":
			e.out("%s", fenceHelp)
			return nil
		case "--print":
			print = true
		case "--json":
			asJSON = true
		case "--no-fence":
			noFence = true
		case "--allow-weak-ipc":
			weakIPC = true
		case "--harness", "--workspace", "--allow", "--envelope", "--gate-root", "--plan-from",
			"--workdir", "--tmpdir", "--coppice-socket", "--keep-fd", "--spawn-gate":
			if v, err = val(); err != nil {
				return e.usage("fence", err)
			}
			switch name {
			case "--harness":
				o.Harness = v
			case "--workspace":
				o.Workspace = v
			case "--allow":
				o.Allow = append(o.Allow, v)
			case "--envelope":
				o.Envelope = v
			case "--gate-root":
				o.GateRoot = v
			case "--plan-from":
				planFrom = v
			case "--spawn-gate":
				// Hidden: start `gate serve` in a session of its own, print
				// its pid and end, so the server's parent is not the harness.
				// Its captures root is fixed here, <root>/captures, sprig's
				// own default, so a fenced run's captures land (FN-R-32).
				e.out("%d\n", e.spawnGateServer(v, gateCapturesArgs(v)...))
				return nil
			case "--workdir":
				o.Workdirs = append(o.Workdirs, v)
			case "--tmpdir":
				o.Tmpdir = v
			case "--coppice-socket":
				o.Sockets = append(o.Sockets, v)
			case "--keep-fd":
				n, err := strconv.Atoi(v)
				if err != nil || n < 3 {
					return e.usage("fence", &usageError{"--keep-fd takes a descriptor number of 3 or more, not " + v + "."})
				}
				keep = append(keep, n)
			}
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return e.usage("fence", &usageError{"No such option: " + a})
			}
			break parse
		}
	}
	o.Cmd = args[i:]

	if planFrom != "" {
		return e.fencePlanFrom(planFrom)
	}
	if len(o.Cmd) == 0 && !print {
		return e.usage("fence", &usageError{"Missing command. Write: daisugi fence -- COMMAND [ARGS]"})
	}
	o.Environ = e.Environ
	cwd, err := os.Getwd()
	if err != nil {
		return e.refuse("fence", err)
	}
	o.Cwd = cwd
	if exe, err := os.Executable(); err == nil {
		o.SelfExe = exe
	}
	// No variable turns the fence off: a process inside a fenced pane sets
	// its children's environment (FN-R-15). Only --no-fence does.
	if noFence {
		if print {
			e.out("fence: off\n")
			return nil
		}
		return e.fenceExec(o.Cmd, nil)
	}
	if u, err := user.Current(); err == nil {
		o.PwHome = u.HomeDir
	}
	o.Pid = os.Getpid()
	in, exe, err := fence.Gather(o)
	if err != nil {
		return e.refuse("fence", errors.New("HOME is unset or not an absolute path"))
	}
	plan := fence.Plan(in)
	abi, kerr := fence.Check(weakIPC)
	if print {
		return e.fencePrint(in, plan, abi, kerr, asJSON)
	}
	if len(plan.Refusals) > 0 {
		for _, r := range plan.Refusals {
			e.errf("daisugi fence: not starting: %s\n", r)
		}
		return exit(1)
	}
	if kerr != nil {
		e.errf("daisugi fence: not starting the harness: %v.\n"+
			"Nothing was run. To run it without a fence, use --no-fence, or set fence = \"off\" in coppice's config.\n", kerr)
		return exit(1)
	}
	if exe == "" {
		e.errf("daisugi fence: %s is not on PATH.\n", o.Cmd[0])
		return exit(127)
	}
	if in.Tmpdir != "" {
		if err := fence.MakeTmpdir(in.Tmpdir, o.Tmpdir == ""); err != nil {
			e.errf("daisugi fence: not starting: the private temp dir: %v\n", err)
			return exit(1)
		}
	}
	fence.EnsureHarnessDirs(in.Harness, in.Home, in.Workspace)
	e.fenceGateServer(in)
	argv := append([]string{o.Cmd[0]}, o.Cmd[1:]...)
	environ := fence.StripEnv(e.Environ)
	if in.Tmpdir != "" {
		environ = append(dropKey(environ, "TMPDIR"), "TMPDIR="+in.Tmpdir)
	}
	environ = append(environ, "DAISUGI_FENCED=landlock")
	_, xerr := fence.Exec(plan.Rules, weakIPC, fence.NewGuard(in), keep, exe, argv, environ)
	var ref *fence.ErrRefused
	if errors.As(xerr, &ref) {
		e.errf("daisugi fence: not starting: %s\n", ref.Why)
		return exit(1)
	}
	e.errf("daisugi fence: cannot start %s: %v\n", o.Cmd[0], xerr)
	return exit(126)
}

func dropKey(environ []string, key string) []string {
	out := environ[:0:0]
	for _, kv := range environ {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// fenceGateServer makes sure the resident gate answers on each gate root
// that is in use, starting it outside the fence (before the ruleset) when
// it does not: inside the fence, the gate hook only asks it (FN-R-12). It
// says on stderr which server it started, and with what pid.
func (e *Env) fenceGateServer(in fence.Input) {
	for _, root := range fence.GateRoots(in) {
		if !fence.GateOn(root) {
			continue
		}
		// This process becomes the harness (exec keeps the pid), and it is
		// still outside the fence: the record binds the workspace for the
		// resident gate's checkpoints (FN-R-21).
		if err := fenceruns.Register(root, os.Getpid(), in.Workspace); err != nil {
			e.errf("daisugi fence: cannot record this run in %s (%v); the resident gate will deny its checkpoints\n", root, err)
		}
		sock := gateroot.Join(root, "gate.sock")
		state := probeGate(sock)
		if state == gateLive {
			continue
		}
		if state == gateStale {
			_ = syscall.Unlink(sock)
		}
		pid := 0
		if self, err := os.Executable(); err == nil {
			if out, err := exec.Command(self, "fence", "--spawn-gate", root).Output(); err == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(out)))
			}
		}
		t0 := time.Now()
		for probeGate(sock) != gateLive && time.Since(t0) < startServerWait {
			time.Sleep(10 * time.Millisecond)
		}
		if probeGate(sock) == gateLive {
			e.errf("daisugi fence: started the resident gate on %s (pid %d)\n", sock, pid)
		} else {
			e.errf("daisugi fence: the resident gate did not start on %s; the gate hook will deny every call. "+
				"Run `daisugi gate serve --root %s` to see why.\n", sock, root)
		}
	}
}

// fenceExec runs the command with no fence.
func (e *Env) fenceExec(cmd []string, extra []string) error {
	path := cmd[0]
	if !strings.Contains(path, "/") {
		found := ""
		for _, d := range strings.Split(e.env["PATH"], ":") {
			if d == "" {
				continue
			}
			p := d + "/" + path
			if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				found = p
				break
			}
		}
		if found == "" {
			e.errf("daisugi fence: %s is not on PATH.\n", cmd[0])
			return exit(127)
		}
		path = found
	}
	err := syscall.Exec(path, cmd, append(append([]string{}, e.Environ...), extra...))
	e.errf("daisugi fence: cannot start %s: %v\n", cmd[0], err)
	return exit(126)
}

type fencePrinted struct {
	Harness     string       `json:"harness"`
	Workspace   string       `json:"workspace"`
	Home        string       `json:"home"`
	Tmpdir      string       `json:"tmpdir"`
	LandlockABI *int         `json:"landlock_abi"`
	Landlock    string       `json:"landlock"`
	Rules       []fence.Rule `json:"rules"`
	Notes       []string     `json:"notes"`
	Refusals    []string     `json:"refusals"`
}

func (e *Env) fencePrint(in fence.Input, plan fence.Output, abi int, kerr error, asJSON bool) error {
	p := fencePrinted{Harness: in.Harness, Workspace: in.Workspace, Home: in.Home, Tmpdir: in.Tmpdir,
		Rules: plan.Rules, Notes: plan.Notes, Refusals: plan.Refusals}
	if kerr == nil {
		p.LandlockABI = &abi
		p.Landlock = "ok"
	} else {
		p.Landlock = kerr.Error()
	}
	if asJSON {
		b, _ := json.MarshalIndent(p, "", "  ")
		e.out("%s\n", b)
		return nil
	}
	h := p.Harness
	if h == "" {
		h = "(none)"
	}
	e.out("harness %s, workspace %s\n", h, p.Workspace)
	if kerr == nil {
		e.out("landlock: ABI %d\n", abi)
	} else {
		e.out("landlock: unusable (%s). The harness would not start.\n", p.Landlock)
	}
	for _, r := range p.Rules {
		e.out("  %-7s %s\n", r.Mode, r.Path)
	}
	for _, n := range p.Notes {
		e.out("note: %s\n", n)
	}
	for _, r := range p.Refusals {
		e.out("refused: %s\n", r)
	}
	e.out("network: not limited (Landlock limits ports, not hosts)\n")
	return nil
}

// fencePlanFrom is the hidden test hook: the oracle's input in, its output
// out, so clients/fence_compare.py can run the golden cases through this
// binary.
func (e *Env) fencePlanFrom(src string) error {
	var r io.Reader = e.Stdin
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return e.refuse("fence", err)
		}
		defer f.Close()
		r = f
	}
	var in fence.Input
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return e.refuse("fence", err)
	}
	b, _ := json.Marshal(fence.Plan(in))
	e.out("%s\n", b)
	return nil
}
