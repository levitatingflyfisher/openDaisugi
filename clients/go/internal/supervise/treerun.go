package supervise

import (
	"bytes"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tree"
	"daisugi-verify/internal/verify"
)

// KeyFD and SpawnKeysFD are the descriptors a child reads its key and the
// profile bundle from.
const (
	KeyFD       = 3
	SpawnKeysFD = 4
)

// RootSession is agentic_executor.ROOT_SESSION: the root an executor's
// tree mode makes.
const RootSession = "root"

var maxCount = new(big.Int).Lsh(big.NewInt(1), 53)

// toolGranted is verify.agentic_tool_granted on a dumped envelope's
// permissions.
func toolGranted(tool string, perms *pyjson.Object) bool {
	capName, known := agenticToolCapabilities[tool]
	if !known || perms == nil {
		return false
	}
	if tool == "Spawn" {
		var allow []string
		if l, ok := perms.Value("mcp_allowlist").([]any); ok {
			for _, x := range l {
				if s, ok := x.(string); ok {
					allow = append(allow, s)
				}
			}
		}
		return verify.HeadAllowed(verify.SpawnMCPKey, allow)
	}
	return pyjson.Truthy(perms.Value(capName))
}

// SprigWall is agentic_executor.sprig_wall: sprig's --tools for a step's
// host tools, each one the envelope backs and sprig has a tool for, in
// the step's order, once.
func SprigWall(tools []string, envelope *pyjson.Object) []string {
	perms, _ := envelope.Value("permissions").(*pyjson.Object)
	var wall []string
	for _, asked := range tools {
		// An ASCII name in any case maps to its host tool (FN-R-30):
		// coppice sends read/write/edit/bash. Only ASCII is folded, as the
		// oracle folds it.
		t := canonTool(asked)
		name, ok := sprigTools[t]
		if !ok || !toolGranted(t, perms) {
			continue
		}
		dup := false
		for _, w := range wall {
			dup = dup || w == name
		}
		if !dup {
			wall = append(wall, name)
		}
	}
	return wall
}

// canonTool is agentic_executor._CANON_TOOLS: an ASCII name, folded, to
// the host tool of that name, or "".
func canonTool(asked string) string {
	for i := 0; i < len(asked); i++ {
		if asked[i] >= 0x80 {
			return ""
		}
	}
	low := strings.ToLower(asked)
	for host := range sprigTools {
		if strings.ToLower(host) == low {
			return host
		}
	}
	return ""
}

func hasSpace(words []string) bool {
	for _, w := range words {
		if strings.IndexFunc(w, unicode.IsSpace) >= 0 {
			return true
		}
	}
	return false
}

// childEnv is agentic_executor.child_env: the key variables the table
// names dropped, and for a profiled sprig child (or one that may spawn)
// every ambient key and redirect too. nil (inherit) when there is
// nothing to drop.
func childEnv(environ []string, ps agentprof.Profiles, sprig bool) []string {
	drop := map[string]bool{}
	for _, n := range ps.KeyNames() {
		drop[n] = true
	}
	if sprig {
		for _, n := range agentprof.Ambient {
			drop[n] = true
		}
	}
	if environ == nil {
		environ = os.Environ()
	}
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] && !agentprof.IsKeyName(name) {
			out = append(out, kv)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// sprigStart is agentic_executor.sprig_start: the argv (without the
// binary), descriptors and environment of a sprig child. One builder for
// every starter, so a child starts the same whoever starts it.
func sprigStart(self, model, gateRoot, dataDir, sessDir, session string, wall []string, profile *agentprof.Profile,
	profiled bool, ps agentprof.Profiles, environ []string, maxTurns, maxTokens *big.Int) ([]string, map[int][]byte, []string) {
	gate := []string{self, "gate", "check", "--mode", "enforce", "--root", gateRoot, "--session", session,
		"--captures-root", gateroot.Join(gateRoot, "captures")}
	argv := []string{"--json", "--gate", "--gate-cmd", strings.Join(gate, " "), "--tools", strings.Join(wall, ","),
		"--model", model}
	fds := map[int][]byte{}
	spawn := false
	for _, w := range wall {
		spawn = spawn || w == "spawn"
	}
	if profiled {
		argv = append(argv, "--wire", profile.Wire)
		if profile.Wire != "claude-code" {
			base := agentprof.DefaultBase[profile.Wire]
			if profile.BaseURL != nil && *profile.BaseURL != "" {
				base = *profile.BaseURL
			}
			argv = append(argv, "--base-url", base)
		}
	}
	if profile.Key != nil && *profile.Key != "" {
		argv = append(argv, "--key-fd", strconv.Itoa(KeyFD))
		fds[KeyFD] = []byte(*profile.Key)
	}
	if names := ps.KeyNames(); (profiled || spawn) && len(names) > 0 {
		argv = append(argv, "--scrub-env", strings.Join(names, ","))
	}
	if spawn {
		treeCmd := []string{self, "tree", "run", "--data-dir", dataDir, "--root", gateRoot}
		argv = append(argv, "--spawn-keys-fd", strconv.Itoa(SpawnKeysFD), "--tree-cmd", strings.Join(treeCmd, " "))
		fds[SpawnKeysFD] = ps.Bundle()
	}
	argv = append(argv, "--session-dir", sessDir, "--session", session)
	if maxTurns != nil {
		argv = append(argv, "--max-turns", maxTurns.String())
	}
	if maxTokens != nil {
		argv = append(argv, "--max-tokens", maxTokens.String())
	}
	argv = append(argv, "-")
	return argv, fds, childEnv(environ, ps, true)
}

// SprigOutcome is how a sprig child ended: the failure text, or its
// answer and the --json object it printed.
type SprigOutcome struct {
	Error  string
	Failed bool
	Answer string
	Obj    *pyjson.Object
}

// runSprigChild is agentic_executor.run_sprig_child: run a sprig child
// and read its --json reply; every failure is one of the oracle's texts.
func runSprigChild(bin, name string, lookErr error, argv []string, task, cwd string, timeoutS int,
	environ []string, fds map[int][]byte, onStart func(int), branch func() []int) SprigOutcome {
	fail := func(t string) SprigOutcome { return SprigOutcome{Error: t, Failed: true} }
	stdin, perr := pystr.FSEncode(task)
	if perr != nil {
		stdin = []byte(task)
	}
	var rc int
	var out, errb []byte
	var timedOut, tooBig bool
	err := lookErr
	if err == nil {
		rc, out, errb, timedOut, tooBig, err = runGroupFds(bin, argv, cwd, environ, stdin, timeoutS, fds, onStart, branch)
	}
	if err != nil {
		return fail("agentic sub-agent failed: cannot start sprig (" + name + ")")
	}
	if tooBig {
		return fail("agentic sub-agent failed: sprig wrote more than 16 MiB")
	}
	if timedOut {
		return fail(fmt.Sprintf("agentic sub-agent failed: sprig ran past %ds and was killed", timeoutS))
	}
	if rc != 0 {
		return fail(sprigExitText(rc, errb))
	}
	raw := pystr.DecodeReplace(out)
	parsed, derr := pyjson.LoadsPy(raw, 900)
	if derr != nil {
		return fail("agentic sub-agent returned unparseable output: " + pystr.Repr(pystr.Slice(raw, 0, 300)))
	}
	obj, _ := parsed.(*pyjson.Object)
	var answer string
	isStr := false
	if obj != nil {
		answer, isStr = obj.Value("answer").(string)
	}
	if !isStr {
		return fail("agentic sub-agent reply has no answer: " + pystr.Repr(pystr.Slice(raw, 0, 300)))
	}
	return SprigOutcome{Answer: answer, Obj: obj}
}

// runGroupFds is agentic_executor._run_sprig: the child in a process
// group of its own, with descriptors at fixed numbers (each a pipe that
// holds its bytes) and the parent-death signal (SIGTERM, so a sprig can
// kill its tools' groups). The OS thread stays locked from the start to
// the wait, since the signal follows the thread that started the child.
// onStart gets its pid, which is its group. The kill (MX-R-8): when the
// child exits, writes more than outputCap or runs past the timeout,
// branch()'s groups and then its own are killed (killGroups). Output a
// process outside the group still holds is read until the timeout, and
// after a kill for sprigDrainS more.
func runGroupFds(name string, args []string, dir string, environ []string, stdin []byte,
	timeoutS int, fds map[int][]byte, onStart func(int), branch func() []int) (int, []byte, []byte, bool, bool, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdin = bytes.NewReader(stdin)
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, nil, nil, false, false, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return 0, nil, nil, false, false, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	var readers []*os.File
	if len(fds) > 0 {
		top := 0
		for fd := range fds {
			if fd > top {
				top = fd
			}
		}
		cmd.ExtraFiles = make([]*os.File, top-2)
		for fd, data := range fds {
			r, w, err := os.Pipe()
			if err != nil {
				return 0, nil, nil, false, false, err
			}
			cmd.ExtraFiles[fd-3] = r
			readers = append(readers, r)
			go func(w *os.File, data []byte) {
				_, _ = w.Write(data)
				w.Close()
			}(w, data)
		}
	}
	err = cmd.Start()
	for _, r := range readers {
		r.Close()
	}
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return 0, nil, nil, false, false, err
	}
	pgid := cmd.Process.Pid
	if onStart != nil {
		onStart(pgid)
	}
	var out, errb bytes.Buffer
	big := make(chan struct{}, 2)
	eof := make(chan struct{}, 2)
	read := func(r *os.File, buf *bytes.Buffer) {
		chunk := make([]byte, 65536)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				if buf.Len()+n > outputCap {
					big <- struct{}{}
					break
				}
				buf.Write(chunk[:n])
			}
			if err != nil {
				break
			}
		}
		eof <- struct{}{}
	}
	go read(outR, &out)
	go read(errR, &errb)
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	kill := func() {
		var groups []int
		if branch != nil {
			groups = branch()
		}
		killGroups(append(groups, pgid))
	}
	deadline := time.After(time.Duration(timeoutS) * time.Second)
	timedOut, tooBig, exitSeen, ended := false, false, false, 0
	var drain <-chan time.Time
loop:
	for ended < 2 || !exitSeen {
		var exitCase <-chan struct{}
		if !exitSeen {
			exitCase = exited
		}
		select {
		case <-big:
			tooBig = true
			if drain == nil {
				kill()
				drain = time.After(sprigDrainS * time.Second)
			}
		case <-eof:
			ended++
		case <-exitCase:
			exitSeen = true
			if drain == nil {
				kill()
			}
		case <-deadline:
			deadline = nil
			if drain == nil {
				timedOut = true
				kill()
				drain = time.After(sprigDrainS * time.Second)
			}
		case <-drain:
			break loop
		}
	}
	outR.Close()
	errR.Close()
	<-exited
	rc := 0
	if st := cmd.ProcessState; st != nil {
		rc = st.ExitCode()
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			rc = -int(ws.Signal())
		}
	}
	if timedOut && drain != nil && ended < 2 {
		return -1, nil, nil, true, tooBig, nil
	}
	return rc, out.Bytes(), errb.Bytes(), timedOut, tooBig, nil
}

// outputCap is OUTPUT_CAP: the most a child may write on stdout, and on
// stderr.
const outputCap = 16 << 20

// killGrace is KILL_GRACE_S.
const killGrace = 2 * time.Second

// killGroups is agentic_executor.kill_groups: SIGTERM to each group in
// order, then SIGKILL to each one still there after killGrace. Never a
// group of 1 or less, and never this process's own group.
func killGroups(groups []int) {
	mine := syscall.Getpgrp()
	seen := map[int]bool{}
	var targets []int
	for _, g := range groups {
		if g > 1 && g != mine && !seen[g] {
			seen[g] = true
			targets = append(targets, g)
		}
	}
	signalAll := func(sig syscall.Signal) []int {
		var left []int
		for _, g := range targets {
			if syscall.Kill(-g, sig) == nil {
				left = append(left, g)
			}
		}
		return left
	}
	alive := signalAll(syscall.SIGTERM)
	end := time.Now().Add(killGrace)
	for len(alive) > 0 && time.Now().Before(end) {
		var still []int
		for _, g := range alive {
			if syscall.Kill(-g, 0) == nil {
				still = append(still, g)
			}
		}
		alive = still
		if len(alive) > 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	targets = alive
	signalAll(syscall.SIGKILL)
}

// ownUse is tree_run._own_use: the four usage counts added (ints only,
// capped at 2**53, floored at 0), and the model turns.
func ownUse(obj *pyjson.Object) (*big.Int, *big.Int) {
	var tokens, turns *big.Int
	if usage, ok := obj.Value("usage").(*pyjson.Object); ok {
		// A negative count makes the report no report (MX-R-7).
		sum, any, negative := new(big.Int), false, false
		for _, f := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			if n, ok := usage.Value(f).(pyjson.Int); ok {
				v, _ := new(big.Int).SetString(n.Text, 10)
				if v != nil {
					sum.Add(sum, v)
					any = true
					negative = negative || v.Sign() < 0
				}
			}
		}
		if any && !negative {
			tokens = clampCount(sum)
		}
	}
	if n, ok := obj.Value("model_turns").(pyjson.Int); ok {
		if v, okk := new(big.Int).SetString(n.Text, 10); okk {
			turns = clampCount(v)
		}
	}
	return tokens, turns
}

func clampCount(v *big.Int) *big.Int {
	if v.Sign() < 0 {
		return new(big.Int)
	}
	if v.Cmp(maxCount) > 0 {
		return new(big.Int).Set(maxCount)
	}
	return v
}

// NodeRun is tree_run.NodeRun: what happened to one child.
type NodeRun struct {
	Status     string // started, refused, asked or failed
	Session    *string
	Answer     *string
	TokensUsed *big.Int
	TurnsUsed  *big.Int
	Reasons    []string
	AskID      *string
	Obj        *pyjson.Object
}

// Doc is NodeRun.doc.
func (n *NodeRun) Doc() *pyjson.Object {
	status := n.Status
	if status == "started" {
		status = "ran"
	}
	opt := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	big := func(b *big.Int) any {
		if b == nil {
			return nil
		}
		return pyjson.Int{Text: b.String()}
	}
	reasons := []any{}
	for _, r := range n.Reasons {
		reasons = append(reasons, r)
	}
	return pyjson.NewObject().Set("status", status).Set("session", opt(n.Session)).Set("answer", opt(n.Answer)).
		Set("tokens_used", big(n.TokensUsed)).Set("turns_used", big(n.TurnsUsed)).Set("reasons", reasons).
		Set("ask_id", opt(n.AskID))
}

// Starter is what tree_run.run_node needs to start a sprig child.
type Starter struct {
	Self     string // this binary: the gate and the tree command
	TempDir  string // $TMPDIR, as tempfile.gettempdir reads it
	Sprig    string // the sprig binary as named
	Environ  []string
	LookPath func(string) (string, error)
	// Fence, when set, is the `daisugi fence ...` prefix (no "--") sprig
	// starts under (DAISUGI_SPRIG_FENCE, FN-R-25). The tree run itself stays
	// outside the fence and keeps writing its gate root; only sprig and
	// its tools are fenced, and the gate root is no rw grant of theirs.
	Fence []string
}

// FenceSessionsDir is sprig's session dir under the fence: in the data
// dir, outside the gate root, which the fence keeps read only.
const FenceSessionsDir = "sprig-sessions"

// SpawnFenceText is the refusal of a spawn wall under the sprig fence: a
// nested tree run would write the gate root from inside the fence.
const SpawnFenceText = "the spawn tool cannot run under the sprig fence: a child tree run writes the gate root, which the fence keeps read only"

// NodeOpts are run_node's keywords.
type NodeOpts struct {
	Parent                            string
	Session                           *string
	Envelope                          *pyjson.Object
	Profiles                          agentprof.Profiles
	ProfileName                       *string
	Model                             string
	Tokens, Turns                     *big.Int
	ParentTokensUsed, ParentTurnsUsed *big.Int
	Task                              string
	Tools                             []string
	ToolsRepr                         string // repr of the requested tools, for the empty-wall text
	MaxTurns                          *big.Int
	Cwd                               string
	TimeoutS                          int
	DefaultModel                      string
	TimeoutMs                         int
}

func profiledName(name *string) bool { return name != nil && *name != agentprof.Default }

// RunNode is tree_run.run_node: start one child under the parent and wait
// for it. A *tree.Error is the tree refusing the command itself.
func (s *Starter) RunNode(dataDir, gateRoot string, o NodeOpts) (*NodeRun, error) {
	profile, why := o.Profiles.Lookup(o.ProfileName)
	if profile == nil {
		return &NodeRun{Status: "failed", Session: o.Session, Reasons: []string{"agentic sub-agent failed: " + why}}, nil
	}
	model := o.Model
	if model == "" && profile.Model != nil {
		model = *profile.Model
	}
	if model == "" {
		model = o.DefaultModel
	}
	wall := SprigWall(o.Tools, o.Envelope)
	if len(wall) == 0 {
		return &NodeRun{Status: "failed", Session: o.Session, Reasons: []string{
			"no requested tool is backed by the envelope (requested " + o.ToolsRepr + "); nothing to delegate"}}, nil
	}
	spawn := false
	for _, w := range wall {
		spawn = spawn || w == "spawn"
	}
	words := []string{s.Self, s.TempDir}
	if spawn {
		words = append(words, s.Self)
	}
	if hasSpace(words) {
		return &NodeRun{Status: "failed", Session: o.Session, Reasons: []string{SprigSpaceText}}, nil
	}
	if spawn && s.Fence != nil {
		return &NodeRun{Status: "failed", Session: o.Session, Reasons: []string{SpawnFenceText}}, nil
	}
	var profName *string
	if profiledName(o.ProfileName) {
		profName = o.ProfileName
	}
	m := model
	sp, err := tree.DoSpawnOpts(dataDir, gateRoot, o.Envelope, o.Parent, o.Session, o.Tokens, o.Turns, o.TimeoutMs,
		tree.SpawnOpts{Profile: profName, Model: &m, ParentTokensUsed: o.ParentTokensUsed, ParentTurnsUsed: o.ParentTurnsUsed})
	if err != nil {
		return nil, err
	}
	session := sp.Session
	if sp.Status != "started" {
		run := &NodeRun{Status: sp.Status, Session: &session, Reasons: sp.Reasons}
		if sp.AskID != "" {
			id := sp.AskID
			run.AskID = &id
		}
		return run, nil
	}
	runTurns := o.Turns
	if o.MaxTurns != nil && (runTurns == nil || o.MaxTurns.Cmp(runTurns) < 0) {
		runTurns = o.MaxTurns
	}
	sessDir := gateroot.Join(gateRoot, "sessions")
	if s.Fence != nil {
		sessDir = gateroot.Join(dataDir, FenceSessionsDir)
		_ = os.MkdirAll(sessDir, 0o700)
	}
	argv, fds, env := sprigStart(s.Self, model, gateRoot, dataDir, sessDir, session, wall, profile,
		profiledName(o.ProfileName), o.Profiles, s.Environ, runTurns, o.Tokens)
	if env == nil {
		env = s.Environ
	}
	bin, lerr := s.Sprig, error(nil)
	if s.LookPath != nil {
		bin, lerr = s.LookPath(s.Sprig)
	}
	if s.Fence != nil && lerr == nil {
		// sprig under the fence: its session dir read and write, its key
		// pipes kept, the gate root left read only (FN-R-25).
		wrapped := append(append([]string{}, s.Fence[1:]...), "--allow", sessDir+":rw")
		keys := make([]int, 0, len(fds))
		for fd := range fds {
			keys = append(keys, fd)
		}
		sort.Ints(keys)
		for _, fd := range keys {
			wrapped = append(wrapped, "--keep-fd", strconv.Itoa(fd))
		}
		argv = append(append(wrapped, "--", bin), argv...)
		bin = s.Fence[0]
	}
	out := runSprigChild(bin, s.Sprig, lerr, argv, o.Task, o.Cwd, o.TimeoutS, env, fds,
		func(pid int) { _ = tree.RecordGroup(dataDir, session, pid) },
		func() []int { return tree.ReadTree(dataDir).BranchGroups(session) })
	var ownTokens, ownTurns *big.Int
	if !out.Failed {
		ownTokens, ownTurns = ownUse(out.Obj)
	}
	// Nothing under it may outlive it: end its running descendants with no
	// report, leaves first, then the node itself with its own use.
	tree.EndSubtree(dataDir, gateRoot, session, false)
	node, err := tree.End(dataDir, gateRoot, session, ownTokens, ownTurns)
	if err != nil {
		return nil, err
	}
	if out.Failed {
		return &NodeRun{Status: "failed", Session: &session, TokensUsed: node.TokensUsed, TurnsUsed: node.TurnsUsed,
			Reasons: []string{out.Error}}, nil
	}
	answer := out.Answer
	return &NodeRun{Status: "started", Session: &session, Answer: &answer, TokensUsed: node.TokensUsed,
		TurnsUsed: node.TurnsUsed, Obj: out.Obj}, nil
}

// reprTools is repr() of a list of tool names.
func reprTools(tools []string) string {
	l := make([]any, len(tools))
	for i, t := range tools {
		l[i] = t
	}
	return pmodel.Repr(l)
}
