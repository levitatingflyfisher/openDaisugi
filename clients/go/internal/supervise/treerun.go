package supervise

import (
	"bytes"
	"daisugi-verify/internal/pandoskip"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/fence"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tree"
	"daisugi-verify/internal/verify"
	"daisugi-verify/internal/wardenroot"
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
		// help has no effect in the world and needs no grant: it is how a
		// worker asks its parent (a name in any ASCII case, as the others).
		if isHelpTool(asked) {
			if !slices.Contains(wall, "help") {
				wall = append(wall, "help")
			}
			continue
		}
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

// isHelpTool reports whether an asked tool name is help.
func isHelpTool(asked string) bool {
	return strings.EqualFold(asked, "help") && asked == pystrASCII(asked)
}

// pystrASCII is the name when it is ASCII, else "".
func pystrASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return ""
		}
	}
	return s
}

// WithoutHelp drops help from a tool list: a branch has no parent to ask.
func WithoutHelp(tools []string) []string {
	out := []string{}
	for _, t := range tools {
		if !isHelpTool(t) {
			out = append(out, t)
		}
	}
	return out
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
func sprigStart(self, model, wardenRoot, dataDir, sessDir, session string, wall []string, profile *agentprof.Profile,
	profiled bool, ps agentprof.Profiles, environ []string, maxTurns, maxTokens *big.Int, resume string) ([]string, map[int][]byte, []string) {
	warden := []string{self, "warden", "check", "--mode", "enforce", "--root", wardenRoot, "--session", session,
		"--captures-root", wardenroot.Join(wardenRoot, "captures")}
	argv := []string{"--json", "--warden", "--warden-cmd", strings.Join(warden, " "), "--tools", strings.Join(wall, ","),
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
		treeCmd := []string{self, "tree", "run", "--data-dir", dataDir, "--root", wardenRoot}
		argv = append(argv, "--spawn-keys-fd", strconv.Itoa(SpawnKeysFD), "--tree-cmd", strings.Join(treeCmd, " "))
		fds[SpawnKeysFD] = ps.Bundle()
	}
	argv = append(argv, "--session-dir", sessDir)
	if resume != "" {
		// A resumed worker: the old session's file and history, the new
		// node's session for the warden.
		argv = append(argv, "--resume", resume)
	}
	argv = append(argv, "--session", session)
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
	cmd.Env = pandoskip.Environ(environ) // a worker is not the owner's chat
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
	// timeoutS 0 is no run cap. Output a process outside the group still
	// holds is then read for sprigDrainS after the child exits.
	// The cap is on the timer, not on a select case: the timer sets the
	// verdict and kills, so a run past its cap is reported so whatever the
	// loop below picks first (review r9 fix 1, finding 4).
	var capFired atomic.Bool
	var capTimer *time.Timer
	deadline := make(chan struct{}, 1)
	if timeoutS > 0 {
		capTimer = runCapTimer(time.Duration(timeoutS)*time.Second, func() {
			capFired.Store(true)
			deadline <- struct{}{}
			kill()
		})
	}
	tooBig, exitSeen, ended := false, false, 0
	var drain, grace <-chan time.Time
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
			// The cap stays armed. A child that exited cleanly gets a
			// short grace for output a process outside its group still
			// holds, then ends with what it wrote: the exit is the end of
			// its answer (review r9 fix 2, finding 3). A cap that fires
			// first is a timeout, as before.
			if drain == nil {
				kill()
				switch {
				case timeoutS <= 0 || capFired.Load():
					drain = time.After(sprigDrainS * time.Second)
				case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0:
					grace = time.After(exitGrace)
				}
			}
		case <-grace:
			break loop
		case <-deadline:
			if drain == nil {
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
	// A timer that has fired, or fires as the run ends, is the verdict.
	if capTimer != nil && !capTimer.Stop() {
		capFired.Store(true)
	}
	timedOut := capFired.Load()
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
	// HelpKind and HelpText are set when the child ended by calling help.
	HelpKind, HelpText *string
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
	if n.HelpKind != nil {
		status = "help"
	}
	doc := pyjson.NewObject().Set("status", status).Set("session", opt(n.Session)).Set("answer", opt(n.Answer)).
		Set("tokens_used", big(n.TokensUsed)).Set("turns_used", big(n.TurnsUsed)).Set("reasons", reasons).
		Set("ask_id", opt(n.AskID))
	if n.HelpKind != nil {
		doc.Set("help_kind", opt(n.HelpKind)).Set("help_text", opt(n.HelpText))
	}
	return doc
}

// Starter is what tree_run.run_node needs to start a sprig child.
type Starter struct {
	Self     string // this binary: the warden and the tree command
	TempDir  string // $TMPDIR, as tempfile.gettempdir reads it
	Sprig    string // the sprig binary as named
	Environ  []string
	LookPath func(string) (string, error)
	// Fence, when set, is the `daisugi fence ...` prefix (no "--") sprig
	// starts under (DAISUGI_SPRIG_FENCE, FN-R-25). The tree run itself stays
	// outside the fence and keeps writing its warden root; only sprig and
	// its tools are fenced, and the warden root is no rw grant of theirs.
	Fence []string
}

// FenceSessionsDir is sprig's session dir under the fence: in the data
// dir, outside the warden root, which the fence keeps read only.
const FenceSessionsDir = "sprig-sessions"

// SpawnFenceText is the refusal of a spawn wall under the sprig fence: a
// nested tree run would write the warden root from inside the fence.
const SpawnFenceText = "the spawn tool cannot run under the sprig fence: a child tree run writes the warden root, which the fence keeps read only"

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
	// TimeoutS caps the whole sprig run; 0 is no cap. StepCapS caps each
	// bash call sprig makes (SPRIG_STEP_TIMEOUT_S); 0 is no cap.
	TimeoutS     int
	StepCapS     int
	DefaultModel string
	TimeoutMs    int
	// Reserved means a branch starter proved the edge and reserved this
	// node's budget (tree.SpawnBranches): the node is neither spawned nor
	// ended here. The caller ends it, in branch order, with the use this
	// returns (the node's own, W3K-13).
	Reserved bool
	// Resume is the session of an earlier segment of this worker, which
	// ended with help. Task is then the answer to its question.
	Resume string
}

func profiledName(name *string) bool { return name != nil && *name != agentprof.Default }

// RunNode is tree_run.run_node: start one child under the parent and wait
// for it. A *tree.Error is the tree refusing the command itself.
func (s *Starter) RunNode(dataDir, wardenRoot string, o NodeOpts) (*NodeRun, error) {
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
	var session string
	if o.Reserved {
		session = *o.Session
	} else {
		sp, err := tree.DoSpawnOpts(dataDir, wardenRoot, o.Envelope, o.Parent, o.Session, o.Tokens, o.Turns, o.TimeoutMs,
			tree.SpawnOpts{Profile: profName, Model: &m, ParentTokensUsed: o.ParentTokensUsed, ParentTurnsUsed: o.ParentTurnsUsed})
		if err != nil {
			return nil, err
		}
		session = sp.Session
		if sp.Status != "started" {
			run := &NodeRun{Status: sp.Status, Session: &session, Reasons: sp.Reasons}
			if sp.AskID != "" {
				id := sp.AskID
				run.AskID = &id
			}
			return run, nil
		}
	}
	runTurns := o.Turns
	if o.MaxTurns != nil && (runTurns == nil || o.MaxTurns.Cmp(runTurns) < 0) {
		runTurns = o.MaxTurns
	}
	sessDir := wardenroot.Join(wardenRoot, "sessions")
	if s.Fence != nil {
		sessDir = wardenroot.Join(dataDir, FenceSessionsDir)
		_ = os.MkdirAll(sessDir, 0o700)
	}
	argv, fds, env := sprigStart(s.Self, model, wardenRoot, dataDir, sessDir, session, wall, profile,
		profiledName(o.ProfileName), o.Profiles, s.Environ, runTurns, o.Tokens, o.Resume)
	if env == nil {
		env = s.Environ
	}
	if o.StepCapS > 0 {
		env = withVar(env, "SPRIG_STEP_TIMEOUT_S", strconv.Itoa(o.StepCapS))
	}
	bin, lerr := s.Sprig, error(nil)
	if s.LookPath != nil {
		bin, lerr = s.LookPath(s.Sprig)
	}
	if s.Fence != nil && lerr == nil {
		// sprig under the fence: its session dir read and write, its key
		// pipes kept, the warden root left read only (FN-R-25).
		wrapped := append(append([]string{}, s.Fence[1:]...), "--allow", sessDir+":rw")
		if o.Reserved {
			repo := ""
			if n := tree.ReadTree(dataDir).Nodes[session]; n != nil && n.Repo != nil {
				repo = *n.Repo
			}
			wrapped = BranchFence(wrapped, o.Cwd, session, repo)
			env = BranchEnv(env, session, repo)
		}
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
	tree.EndSubtree(dataDir, wardenRoot, session, false)
	usedTokens, usedTurns := ownTokens, ownTurns
	if !o.Reserved {
		node, err := tree.End(dataDir, wardenRoot, session, ownTokens, ownTurns)
		if err != nil {
			return nil, err
		}
		usedTokens, usedTurns = node.TokensUsed, node.TurnsUsed
	}
	if out.Failed {
		return &NodeRun{Status: "failed", Session: &session, TokensUsed: usedTokens, TurnsUsed: usedTurns,
			Reasons: []string{out.Error}}, nil
	}
	answer := out.Answer
	run := &NodeRun{Status: "started", Session: &session, Answer: &answer, TokensUsed: usedTokens,
		TurnsUsed: usedTurns, Obj: out.Obj}
	if st, _ := out.Obj.Value("status").(string); st == "help" {
		kind, _ := out.Obj.Value("help_kind").(string)
		text, _ := out.Obj.Value("help_text").(string)
		run.HelpKind, run.HelpText = &kind, &text
	}
	return run, nil
}

// reprTools is repr() of a list of tool names.
func reprTools(tools []string) string {
	l := make([]any, len(tools))
	for i, t := range tools {
		l[i] = t
	}
	return pmodel.Repr(l)
}

// BranchFence is tree_run.branch_fence: the fence words for a branch child.
// Its workspace is its own worktree, not the project. The repository's git
// directory is read only, except this branch's own admin directory, so the
// child can use git but cannot write the shared config, the hooks, the shared
// object store (which hold the operator's history) or a sibling's git
// directory. The child writes objects to a store of its own in its admin
// directory (BranchEnv).
func BranchFence(words []string, worktree, session, repo string) []string {
	out := make([]string, 0, len(words)+8)
	swapped := false
	for i := 0; i < len(words); i++ {
		if words[i] == "--workspace" && i+1 < len(words) {
			out = append(out, "--workspace", worktree)
			swapped = true
			i++
			continue
		}
		out = append(out, words[i])
	}
	if !swapped {
		out = append(out, "--workspace", worktree)
	}
	if repo != "" {
		out = append(out, "--allow", repo+":ro", "--allow", repo+"/worktrees/"+session+":rw")
	}
	return out
}

// BranchEnv is tree_run.branch_env: the environment of a fenced branch child.
// Git writes its objects to a store in the branch's own admin directory and
// reads the shared store as an alternate. The parent makes the directory; the
// child cannot make it outside its grant.
func BranchEnv(environ []string, session, repo string) []string {
	if repo == "" || strings.Contains(repo, ":") {
		return environ
	}
	mine := filepath.Join(repo, "worktrees", session, "objects")
	if err := os.MkdirAll(mine, 0o700); err != nil {
		return environ
	}
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GIT_OBJECT_DIRECTORY=") && !strings.HasPrefix(kv, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_OBJECT_DIRECTORY="+mine, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(repo, "objects"))
}

// RealPath is os.path.realpath for an absolute path: the longest part that
// exists has its links resolved, and the rest is kept as written.
func RealPath(p string) string {
	p = fence.Norm(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
	}
}

// FenceCovers is tree_run.fence_covers: the first read-and-write grant of a
// fence prefix that reaches the repository's git directory or its root, or "".
// Such a grant would let the branch child write the shared config and hooks;
// BranchFence narrows only what it adds itself. A grant counts when it holds,
// or lies inside, the git directory (and so the root and what is above it).
//
// Each grant is read as the fence reads it: ~ is home, a relative path is
// joined to base (the fence's workspace, the child's worktree), and symbolic
// links are resolved, because Landlock grants the place a link points to.
func FenceCovers(words []string, envelopeFile, repo, home, base string) string {
	realRepo := RealPath(repo)
	hits := func(p string) bool {
		// The git directory sits inside the root, so a grant on the root, or
		// above it, holds the git directory too.
		p = RealPath(p)
		return fence.Covers(p, realRepo) || fence.Covers(realRepo, p)
	}
	var given []string
	for i := 0; i < len(words); i++ {
		name, val, eq := strings.Cut(words[i], "=")
		if name != "--allow" && name != "--workdir" {
			continue
		}
		if !eq {
			if i+1 >= len(words) {
				continue
			}
			i++
			val = words[i]
		}
		if name == "--workdir" || strings.HasSuffix(val, ":rw") {
			given = append(given, strings.TrimSuffix(val, ":rw"))
		}
	}
	for _, g := range given {
		if hits(fence.Expand(g, home, base)) {
			return g
		}
	}
	if envelopeFile != "" {
		if raw, err := os.ReadFile(envelopeFile); err == nil {
			if doc, perr := pyjson.Loads(string(raw)); perr == nil {
				if o, ok := doc.(*pyjson.Object); ok {
					if perm, ok := o.Value("permissions").(*pyjson.Object); ok {
						o = perm
					}
					if pats, ok := o.Value("file_write").([]any); ok {
						for _, pv := range pats {
							pat, ok := pv.(string)
							if ok && pat != "" && hits(fence.GlobPrefix(pat, home, base)) {
								return pat
							}
						}
					}
				}
			}
		}
	}
	return ""
}

// withVar is environ with name set to value, any earlier value dropped.
func withVar(environ []string, name, value string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return append(out, name+"="+value)
}

// runCapTimer arms the run cap; a test replaces it.
var runCapTimer = time.AfterFunc

// exitGrace is how long a child that exited cleanly waits for a process
// outside its group to let go of its output; a test shortens it.
var exitGrace = 30 * time.Second
