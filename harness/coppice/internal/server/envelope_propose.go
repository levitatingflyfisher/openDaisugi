package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/envdefault"
	"github.com/opendaisugi/coppice/internal/proto"
)

// A project envelope is the operator's file, <config dir>/envelopes/<name>.json,
// and only the operator writes it. The steward may propose one. The server
// checks the proposal, keeps it as a pending card, and writes the file only
// when the owner approves. A steward is a pane, and a pane never gets to
// approve (operatorVerbs).

// ProposalRefusal is the message a pane or a plugin gets for approving,
// declining or listing envelope proposals.
const ProposalRefusal = "only the operator approves an envelope. A steward proposes one with envelope.propose."

// proposalMaxBytes caps a proposed envelope.
const proposalMaxBytes = 64 << 10

// envelopeProposal is one pending proposal. Envelope is kept byte for byte:
// approve writes exactly these bytes.
type envelopeProposal struct {
	Project   string          `json:"project"`
	Dir       string          `json:"dir"`
	Envelope  json.RawMessage `json:"envelope"`
	Tokens    int             `json:"tokens"`
	Turns     int             `json:"turns"`
	HelpWaitS int             `json:"help_wait_s,omitempty"`
	// Allow are the denied verbs (envdefault.Denied) the proposal asks to
	// run. The envelope carries the deny invariants of every other one.
	Allow []string `json:"allow,omitempty"`
	// OwnInvariants counts the invariants the proposal brought that are not
	// the canonical denies, before WithDeny dropped any of a deny type. A
	// proposal with any is for the owner to read (review r9, finding 2).
	OwnInvariants int    `json:"own_invariants,omitempty"`
	By            string `json:"by,omitempty"`
	At            string `json:"at"`
}

// proposalStamp names one proposal: the time to the nanosecond and a random
// nonce, so a replacement in the same second still has a new stamp. The
// card carries it, and approve must send it back.
func proposalStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z") + "-" + randHex(4)
}

func (s *Server) proposalDir() string { return filepath.Join(s.cfg.DataDir, "envelope-proposals") }

func (s *Server) proposalPath(name string) string {
	return filepath.Join(s.proposalDir(), name+".json")
}

// RegisterEnvelopeCommands wires up the envelope verbs.
func (s *Server) RegisterEnvelopeCommands() {
	_ = s.Handle("envelope.propose", s.handleEnvelopePropose)
	_ = s.Handle("envelope.pending", s.handleEnvelopePending)
	_ = s.Handle("envelope.approve", s.handleEnvelopeApprove)
	_ = s.Handle("envelope.decline", s.handleEnvelopeDecline)
}

// checkEnvelopeFile asks daisugi whether the file is a valid envelope: a
// valid envelope fits inside itself (tree check FILE FILE).
func checkEnvelopeFile(path string) error {
	out, err := runDaisugiTask("tree", "check", path, path)
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return errors.New(reasonsOf(out))
	}
	return fmt.Errorf("cannot run daisugi to check the envelope: %v", err)
}

func (s *Server) handleEnvelopePropose(c *Client, r *proto.Request) proto.Response {
	bad := func(msg string) proto.Response { return proto.ErrResp(r.ID, proto.ErrBadRequest, msg) }
	project, _ := r.Str("project")
	if project == "" {
		return bad("envelope.propose needs project. Pass the project's name or path.")
	}
	dir, err := s.resolveProject(project)
	if err != nil {
		return bad(err.Error())
	}
	name := filepath.Base(dir)
	raw, ok := r.Raw("envelope")
	if !ok {
		return bad("envelope.propose needs envelope: the envelope object.")
	}
	env := bytes.TrimSpace(raw)
	if len(env) == 0 || env[0] != '{' || len(env) > proposalMaxBytes || !json.Valid(env) {
		return bad("envelope must be a JSON object under 64 KiB.")
	}
	if _, has := r.Raw("checks"); has {
		return bad("a proposal carries no checks. Checks are commands, and only the operator writes them into the file.")
	}
	var allow []string
	if raw, has := r.Raw("allow"); has && string(bytes.TrimSpace(raw)) != "null" {
		if json.Unmarshal(raw, &allow) != nil {
			return bad("allow must be a list of denied verbs, such as [\"git push\"].")
		}
	}
	if err := envdefault.CheckAllow(allow); err != nil {
		return bad(err.Error())
	}
	// Every envelope the steward proposes carries the deny invariants of
	// the verbs it does not ask to run. The card shows these bytes, and
	// approve writes them as they are.
	own := envdefault.OwnInvariants(env)
	denied, derr := envdefault.WithDeny(env, allow)
	if derr != nil {
		return bad(derr.Error())
	}
	env = denied
	tokens, _ := r.Int("tokens")
	turns, _ := r.Int("turns")
	help, _ := r.Int("help_wait_s")
	if tokens < 0 || turns < 0 || help < 0 {
		return bad("tokens, turns and help_wait_s are not negative.")
	}
	// The check reads a file; it is a candidate in the proposal dir, never
	// the operator's envelope dir.
	if err := os.MkdirAll(s.proposalDir(), 0o700); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot make "+s.proposalDir()+": "+err.Error())
	}
	cand := filepath.Join(s.proposalDir(), "candidate-"+randHex(6)+".json")
	if err := os.WriteFile(cand, env, 0o600); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot write the candidate: "+err.Error())
	}
	cerr := checkEnvelopeFile(cand)
	_ = os.Remove(cand)
	if cerr != nil {
		return bad("the envelope does not pass daisugi's check: " + cerr.Error())
	}
	p := envelopeProposal{Project: name, Dir: dir, Envelope: json.RawMessage(env), Tokens: tokens, Turns: turns,
		HelpWaitS: help, Allow: allow, OwnInvariants: own, At: proposalStamp(s.clock())}
	if ro := roleOrOperator(c); ro.pane {
		p.By = ro.paneID
	}
	// A proposal that fits the default of a trusted project is the default
	// permission: it is written now, with no card (owner ruling 2026-11-06).
	if len(s.widerLines(p)) == 0 {
		// A proposal that fits is granted the default itself, never its own
		// bytes: nothing it wrote reaches the file without a card (review
		// r9, findings 1 and 2). help_wait_s is not a permission and rides
		// along unchecked.
		text := projectEnvelopeText(envdefault.Envelope(dir), 0, 0, help, dir, envdefault.GrantedBy)
		if err := writeFileAtomic(filepath.Join(envelopeDir(), name+".json"), text, 0o600); err != nil {
			return proto.ErrResp(r.ID, proto.ErrInternal, "cannot write the envelope: "+err.Error())
		}
		_ = os.Remove(s.proposalPath(name))
		return proto.OKResp(r.ID, map[string]any{"pending": false, "approved": name, "granted_by": envdefault.GrantedBy,
			"path": filepath.Join(envelopeDir(), name+".json"),
			"note": "It fits the project's default permission, so it is written with no card."})
	}
	b, _ := json.Marshal(p)
	// A second proposal for the project replaces the pending one.
	if err := writeFileAtomic(s.proposalPath(name), b, 0o600); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot keep the proposal: "+err.Error())
	}
	return proto.OKResp(r.ID, map[string]any{"pending": true, "project": name,
		"note": "The owner sees this on the floor and the phone. Nothing is written until the owner approves."})
}

// writeFileAtomic writes b to path through a temp file in the same dir.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// readProposals reads every pending proposal, oldest first.
func (s *Server) readProposals() []envelopeProposal {
	ents, _ := os.ReadDir(s.proposalDir())
	var out []envelopeProposal
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, "candidate-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.proposalDir(), n))
		if err != nil {
			continue
		}
		var p envelopeProposal
		if json.Unmarshal(b, &p) != nil || p.Project == "" || p.Project+".json" != n {
			continue
		}
		// The file is on disk, so it is not trusted: the envelope is an
		// object, and no checks ride along.
		var top map[string]json.RawMessage
		if json.Unmarshal(b, &top) != nil {
			continue
		}
		if _, has := top["checks"]; has {
			continue
		}
		if e := bytes.TrimSpace(p.Envelope); len(e) == 0 || e[0] != '{' {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// proposalCard is what the floor and the phone show for one proposal.
func (s *Server) proposalCard(p envelopeProposal) map[string]any {
	var env struct {
		Task        string `json:"task"`
		Stakes      string `json:"stakes"`
		Permissions struct {
			Read      []string `json:"file_read"`
			Write     []string `json:"file_write"`
			Network   bool     `json:"network"`
			Hosts     []string `json:"network_hosts"`
			Shell     bool     `json:"shell"`
			Allowlist []string `json:"shell_allowlist"`
			MCP       []string `json:"mcp_allowlist"`
			Decompose bool     `json:"shell_allow_decomposition"`
			Custom    []string `json:"custom_step_allowlist"`
			Rules     []struct {
				Tool string `json:"tool"`
			} `json:"mcp_rules"`
			TimeS *float64 `json:"max_execution_time_s"`
			OutMB *float64 `json:"max_output_size_mb"`
		} `json:"permissions"`
		Interp string `json:"shell_interpreter_policy"`
	}
	_ = json.Unmarshal(p.Envelope, &env)
	nz := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	var also []string
	if env.Permissions.Decompose {
		also = append(also, "compound shell commands allowed, each command checked")
	}
	if len(env.Permissions.Custom) > 0 {
		also = append(also, "custom steps: "+strings.Join(env.Permissions.Custom, ", "))
	}
	if len(env.Permissions.Rules) > 0 {
		var tools []string
		for _, r := range env.Permissions.Rules {
			tools = append(tools, r.Tool)
		}
		also = append(also, "tool argument rules for: "+strings.Join(tools, ", "))
	}
	num := func(v *float64, def float64) string { return strconv.FormatFloat(numOr(v, def), 'f', -1, 64) }
	also = append(also, "limits: "+num(env.Permissions.TimeS, 30)+" s per step, "+num(env.Permissions.OutMB, 10)+" MB output")
	if env.Interp != "" && env.Interp != "surface" {
		also = append(also, "shell interpreters: "+env.Interp)
	}
	_, statErr := os.Stat(filepath.Join(envelopeDir(), p.Project+".json"))
	return map[string]any{
		"project": p.Project, "dir": p.Dir, "task": env.Task, "stakes": env.Stakes,
		"read": nz(env.Permissions.Read), "write": nz(env.Permissions.Write),
		"run":   map[string]any{"shell": env.Permissions.Shell, "allow": nz(env.Permissions.Allowlist), "mcp": nz(env.Permissions.MCP)},
		"reach": map[string]any{"network": env.Permissions.Network, "hosts": nz(env.Permissions.Hosts)},
		"also":  also, "tokens": p.Tokens, "turns": p.Turns, "by": p.By, "at": p.At, "replaces": statErr == nil,
		"wider": nz(s.widerLines(p)), "denied": nz(stillDenied(p.Allow)),
	}
}

func numOr(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func (s *Server) handleEnvelopePending(c *Client, r *proto.Request) proto.Response {
	if c.tokenNamed() {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, ConfigRefusal)
	}
	cards := []map[string]any{}
	for _, p := range s.readProposals() {
		cards = append(cards, s.proposalCard(p))
	}
	return proto.OKResp(r.ID, map[string]any{"proposals": cards})
}

// proposalFor reads the pending proposal named by the request.
func (s *Server) proposalFor(r *proto.Request) (envelopeProposal, string, *proto.Response) {
	name, _ := r.Str("project")
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		e := proto.ErrResp(r.ID, proto.ErrBadRequest, r.Cmd+" needs project: the name on the card.")
		return envelopeProposal{}, "", &e
	}
	for _, p := range s.readProposals() {
		if p.Project == name {
			return p, name, nil
		}
	}
	e := proto.ErrResp(r.ID, proto.ErrBadRequest, "no pending envelope for "+name+". It may be approved, declined or replaced already.")
	return envelopeProposal{}, "", &e
}

func (s *Server) handleEnvelopeApprove(c *Client, r *proto.Request) proto.Response {
	if c.tokenNamed() {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, ConfigRefusal)
	}
	p, name, errResp := s.proposalFor(r)
	if errResp != nil {
		return *errResp
	}
	// The owner approves what the card showed. A replaced proposal has a
	// new "at", so approve always names the one it saw.
	at, ok := r.Str("at")
	if !ok || at == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "envelope.approve needs at: the stamp on the card. Look at the pending list first.")
	}
	if at != p.At {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "the proposal for "+name+" changed after you looked. Look again.")
	}
	// The file was checked when it came in. It sat on disk since, so check
	// the stored bytes again before they become the operator's file.
	if err := os.MkdirAll(s.proposalDir(), 0o700); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot make "+s.proposalDir()+": "+err.Error())
	}
	cand := filepath.Join(s.proposalDir(), "candidate-"+randHex(6)+".json")
	if err := os.WriteFile(cand, bytes.TrimSpace(p.Envelope), 0o600); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot write the candidate: "+err.Error())
	}
	cerr := checkEnvelopeFile(cand)
	_ = os.Remove(cand)
	if cerr != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "the stored envelope does not pass daisugi's check: "+cerr.Error())
	}
	// The envelope bytes go in as they were proposed.
	b := projectEnvelopeText(p.Envelope, p.Tokens, p.Turns, p.HelpWaitS, p.Dir, envdefault.GrantedByOwner)
	if err := writeFileAtomic(filepath.Join(envelopeDir(), name+".json"), b, 0o600); err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal, "cannot write the envelope: "+err.Error())
	}
	_ = os.Remove(s.proposalPath(name))
	return proto.OKResp(r.ID, map[string]any{"approved": name, "path": filepath.Join(envelopeDir(), name+".json")})
}

func (s *Server) handleEnvelopeDecline(c *Client, r *proto.Request) proto.Response {
	if c.tokenNamed() {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, ConfigRefusal)
	}
	_, name, errResp := s.proposalFor(r)
	if errResp != nil {
		return *errResp
	}
	_ = os.Remove(s.proposalPath(name))
	return proto.OKResp(r.ID, map[string]any{"declined": name})
}

// stillDenied are the denied verbs a proposal leaves denied.
func stillDenied(allow []string) []string {
	skip := map[string]bool{}
	for _, a := range allow {
		skip[a] = true
	}
	var out []string
	for _, n := range envdefault.VerbNames() {
		if !skip[n] {
			out = append(out, n)
		}
	}
	return out
}
