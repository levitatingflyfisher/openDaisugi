package sprig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// SpawnTool is sprig's spawn tool: start a child agent through the tree
// command (daisugi tree run), wait for it and return its answer. The
// gate sees it as mcp__sprig__spawn, so only an envelope whose
// mcp_allowlist names sprig/spawn lets it run. It is offered only with
// --tree-cmd.
//
// The parent is always sprig's own --session, never a value from the
// input, so a child cannot be hung under a wider ancestor. sprig's own
// use so far goes as --parent-tokens-used and --parent-turns-used; the
// resolved agent profiles (--spawn-keys-fd) go on a fresh pipe as the
// command's descriptor 3. A child's branch use counts toward sprig's
// --max-tokens. Children run one at a time.
type SpawnTool struct {
	Command []string // the tree command, split at whitespace
	Session string   // sprig's own session: the parent of every child
	Keys    []byte   // the resolved profiles with keys; nil sends none
	// OwnUse is sprig's own tokens and model turns so far, without its
	// children's.
	OwnUse func() (tokens, turns int)
	// DefaultTools is the child's tools when the input names none: the
	// parent's own wall, in host names.
	DefaultTools []string

	mu     sync.Mutex
	branch int
}

func (*SpawnTool) Name() string { return "spawn" }

// BranchTokens is the tokens the children used, their branches included.
func (s *SpawnTool) BranchTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.branch
}

// hostToolNames maps sprig's tool names to the host names an agentic
// step and tree run use.
var hostToolNames = map[string]string{
	"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash", "spawn": "Spawn",
}

// HostTools is a wall in host names.
func HostTools(wall []string) []string {
	out := []string{}
	for _, n := range wall {
		if h, ok := hostToolNames[n]; ok {
			out = append(out, h)
		}
	}
	return out
}

// count reads a whole number from 0 to 2**53 in a tool input.
func count(in map[string]any, key string) (int, bool, error) {
	v, ok := in[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, isNum := v.(float64)
	if !isNum || f != math.Trunc(f) || f < 0 || f > 1<<53 {
		return 0, false, fmt.Errorf("spawn: %s must be a whole number from 0 to 2**53", key)
	}
	return int(f), true, nil
}

// text reads an optional text field of a tool input.
func text(in map[string]any, key string) (string, error) {
	v, ok := in[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("spawn: %s must be text", key)
	}
	return s, nil
}

// spawnReply is what tree run prints, read field by field from a plain
// decode: a field of another type reads as absent.
type spawnReply struct {
	Status     string
	Answer     string
	TokensUsed int
	HasTokens  bool
	Reasons    []string
	AskID      string
}

func readSpawnReply(data []byte) (spawnReply, bool) {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return spawnReply{}, false
	}
	o, isObj := v.(map[string]any)
	if !isObj {
		return spawnReply{}, false
	}
	var r spawnReply
	r.Status, _ = o["status"].(string)
	r.Answer, _ = o["answer"].(string)
	r.AskID, _ = o["ask_id"].(string)
	if f, ok := o["tokens_used"].(float64); ok && f == math.Trunc(f) && f >= 0 && f <= 1<<53 {
		r.TokensUsed, r.HasTokens = int(f), true
	}
	if list, ok := o["reasons"].([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok {
				r.Reasons = append(r.Reasons, s)
			}
		}
	}
	return r, r.Status != ""
}

func (s *SpawnTool) Run(in map[string]any) (string, error) {
	out, err := s.run(in)
	// Every key the bundle holds reads [key] in the result: a child's
	// reasons or the tree command's stderr may echo one.
	for _, k := range bundleKeys(s.Keys) {
		out = Redact(out, k)
		if err != nil {
			err = errors.New(Redact(err.Error(), k))
		}
	}
	return out, err
}

// bundleKeys is every non-empty key of a profile bundle, in the order of
// the profile names.
func bundleKeys(bundle []byte) []string {
	var doc map[string]map[string]any
	if json.Unmarshal(bundle, &doc) != nil {
		return nil
	}
	names := make([]string, 0, len(doc))
	for n := range doc {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if k, ok := doc[n]["key"].(string); ok && k != "" {
			out = append(out, k)
		}
	}
	return out
}

func (s *SpawnTool) run(in map[string]any) (string, error) {
	task, err := text(in, "task")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("spawn: task must be text")
	}
	env, isObj := in["envelope"].(map[string]any)
	if !isObj {
		return "", fmt.Errorf("spawn: envelope must be an object")
	}
	profile, err := text(in, "profile")
	if err != nil {
		return "", err
	}
	if profile == "" {
		profile = "default"
	}
	model, err := text(in, "model")
	if err != nil {
		return "", err
	}
	tools := s.DefaultTools
	if raw, ok := in["tools"]; ok && raw != nil {
		list, isList := raw.([]any)
		if !isList {
			return "", fmt.Errorf("spawn: tools must be a list of tool names")
		}
		tools = []string{}
		for _, x := range list {
			name, isStr := x.(string)
			if !isStr {
				return "", fmt.Errorf("spawn: tools must be a list of tool names")
			}
			if h, ok := hostToolNames[name]; ok {
				name = h
			}
			tools = append(tools, name)
		}
	}
	if tools == nil {
		tools = []string{}
	}
	tokens, hasTokens, err := count(in, "tokens")
	if err != nil {
		return "", err
	}
	turns, hasTurns, err := count(in, "turns")
	if err != nil {
		return "", err
	}
	maxTurns, hasMaxTurns, err := count(in, "max_turns")
	if err != nil {
		return "", err
	}

	args := append([]string{}, s.Command[1:]...)
	args = append(args, "--parent", s.Session, "--profile", profile)
	if model != "" {
		args = append(args, "--model", model)
	}
	if hasTokens {
		args = append(args, "--tokens", strconv.Itoa(tokens))
	}
	if hasTurns {
		args = append(args, "--turns", strconv.Itoa(turns))
	}
	if s.OwnUse != nil {
		ownTokens, ownTurns := s.OwnUse()
		args = append(args, "--parent-tokens-used", strconv.Itoa(ownTokens),
			"--parent-turns-used", strconv.Itoa(ownTurns))
	}
	if s.Keys != nil {
		args = append(args, "--keys-fd", "3")
	}
	args = append(args, "-")

	req := map[string]any{"task": task, "envelope": env, "tools": tools}
	if hasMaxTurns {
		req["max_turns"] = maxTurns
	}
	stdin, _ := json.Marshal(req)

	cmd := exec.Command(s.Command[0], args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = ChildEnv("")
	out, errb := &cappedBuffer{}, &cappedBuffer{}
	cmd.Stdout, cmd.Stderr = out, errb
	// The tree command dies with sprig: it stays in sprig's group, and its
	// parent-death signal is SIGTERM.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if s.Keys != nil {
		r, w, perr := os.Pipe()
		if perr != nil {
			return "", fmt.Errorf("spawn: cannot make a pipe: %w", perr)
		}
		cmd.ExtraFiles = []*os.File{r}
		go func() {
			w.Write(s.Keys)
			w.Close()
		}()
		defer r.Close()
	}
	runErr := runLocked(cmd, out, errb)
	if out.over || errb.over {
		return "", fmt.Errorf("spawn: the tree command wrote more than 16 MiB")
	}
	reply, ok := readSpawnReply(bytes.TrimSpace(out.Bytes()))
	if !ok {
		reason := gateReason(errb.String(), "")
		if reason == "" && runErr != nil {
			reason = runErr.Error()
		}
		return "", fmt.Errorf("spawn: the tree command failed: %s", reason)
	}
	s.mu.Lock()
	switch {
	case reply.HasTokens:
		s.branch = satAdd(s.branch, reply.TokensUsed)
	case reply.Status == "ran" || reply.Status == "failed":
		// A child with no report is charged its whole reservation.
		if hasTokens {
			s.branch = satAdd(s.branch, tokens)
		}
	}
	s.mu.Unlock()
	switch reply.Status {
	case "ran":
		return reply.Answer, nil
	case "asked":
		return "sent to the operator as ask " + reply.AskID, nil
	case "refused":
		return "refused: " + strings.Join(reply.Reasons, "; "), nil
	}
	return "failed: " + strings.Join(reply.Reasons, "; "), nil
}

// maxTreeOutput is the most the tree command may write on stdout, and on
// stderr.
const maxTreeOutput = 16 << 20

// cappedBuffer keeps at most maxTreeOutput bytes and notes when more came.
// A write past the cap fails, so the copy to it stops.
// It holds its bytes.Buffer in a field, not embedded: an embedded one
// would give it ReadFrom, which io.Copy calls instead of Write.
type cappedBuffer struct {
	buf  bytes.Buffer
	over bool
	kill func()
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > maxTreeOutput {
		b.over = true
		if b.kill != nil {
			b.kill()
		}
		return 0, errTooMuch
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) String() string { return b.buf.String() }

var errTooMuch = errors.New("more than 16 MiB")

// runLocked runs cmd with the OS thread locked from the start to the
// wait, since the parent-death signal follows the thread that started
// it. Output past the cap kills the command.
func runLocked(cmd *exec.Cmd, bufs ...*cappedBuffer) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, b := range bufs {
		b.kill = func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}
