package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"daisugi-verify/envdefault"
	"github.com/opendaisugi/coppice/internal/config"
)

// One default permission per project (owner ruling 2026-11-06). A project
// whose dir sits under a trusted root gets the default envelope the first
// time the steward spawns into it, with no card. A proposal that fits the
// default is written with no card too. Only a proposal that asks for more,
// or a project outside every trusted root, makes a Needs-you card.

// trustedRoots are the dirs from [steward] trusted_roots and the pinned
// projects. A config that does not read trusts nothing, so every project
// cards.
func trustedRoots() []string {
	cfg, _, err := config.Load()
	if err != nil {
		return nil
	}
	roots, err := cfg.TrustedRoots()
	if err != nil {
		return nil
	}
	return roots
}

// trustedDir says whether dir sits under a trusted root.
func trustedDir(dir string) bool { return envdefault.UnderRoot(dir, trustedRoots()) }

// fenceOffLine is the card's line for a project whose workers would run
// with no fence: the deny list is then the only rule between a worker and
// the owner's keys (review r9, finding 5a).
const fenceOffLine = "the fence is off: a worker here would run with your home dir and keys in reach, so the deny list is the only guard"

// workerFenced says whether a worker in dir would run under the fence.
func (s *Server) workerFenced(dir string) bool {
	w, err := s.fenceCmd("", dir, nil, false)
	return err == nil && w != nil
}

// notTrustedLine is the card's line for a project outside every trusted root.
const notTrustedLine = "the project is outside the trusted roots ([steward] trusted_roots, default ~/code, and the pinned projects)"

// projectEnvelopeText is the operator's envelope file for a project: the
// envelope bytes as given, the budget, the dir it was written for and who
// granted it.
func projectEnvelopeText(env []byte, tokens, turns, helpWait int, dir, grantedBy string) []byte {
	var buf bytes.Buffer
	buf.WriteString("{\n  \"envelope\": ")
	buf.Write(bytes.TrimSpace(env))
	fmt.Fprintf(&buf, ",\n  \"tokens\": %d,\n  \"turns\": %d", tokens, turns)
	if helpWait > 0 {
		fmt.Fprintf(&buf, ",\n  \"help_wait_s\": %d", helpWait)
	}
	if dir != "" {
		d, _ := json.Marshal(dir)
		fmt.Fprintf(&buf, ",\n  \"dir\": %s", d)
	}
	if grantedBy != "" {
		g, _ := json.Marshal(grantedBy)
		fmt.Fprintf(&buf, ",\n  \"granted_by\": %s", g)
	}
	buf.WriteString("\n}\n")
	return buf.Bytes()
}

// projectEnvelopeFor reads the envelope file of the project name in dir. A
// project with no file that sits under a trusted root gets the default
// envelope written first. One outside every trusted root gets a card for
// the default envelope, and the spawn is refused with a line that says so.
func (s *Server) projectEnvelopeFor(name, dir string) (*projectEnvelope, error) {
	path := filepath.Join(envelopeDir(), name+".json")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		why := ""
		switch {
		case !trustedDir(dir):
			why = dir + " is outside the trusted roots"
		case !s.workerFenced(dir):
			why = "the fence is off, so the default is not granted by itself"
		}
		if why != "" {
			if perr := s.proposeDefault(name, dir); perr != nil {
				return nil, fmt.Errorf("no envelope for the project %s, and the card for one could not be made: %v. Write one at %s", name, perr, path)
			}
			return nil, fmt.Errorf("no envelope for the project %s: %s, so the owner has a card for the default envelope. Spawn again once it is approved", name, why)
		}
		text := projectEnvelopeText(envdefault.Envelope(dir), 0, 0, 0, dir, envdefault.GrantedBy)
		if werr := writeFileAtomic(path, text, 0o600); werr != nil {
			return nil, fmt.Errorf("cannot write the default envelope for %s: %v", name, werr)
		}
	}
	pe, err := readProjectEnvelope(path, name)
	if err != nil {
		return nil, err
	}
	// The default is granted on the condition that workers run fenced. A
	// fence turned off later (the warden disarmed, fence = "off", a kernel
	// without Landlock 9) does not inherit the grant (review r9 fix 1, 1).
	if pe.GrantedBy == envdefault.GrantedBy && !s.workerFenced(dir) {
		if perr := s.proposeDefault(name, dir); perr != nil {
			return nil, fmt.Errorf("the fence is off, and the card could not be made: %v", perr)
		}
		return nil, fmt.Errorf("the fence is off, and the envelope of %s was granted by default on the condition that workers run fenced. Approve the card on the floor to run it bare", name)
	}
	// The file is keyed by the dir's base name: a file written for another
	// dir of the same name is not this project's.
	if pe.Dir != "" && filepath.Clean(pe.Dir) != filepath.Clean(dir) {
		return nil, fmt.Errorf("the envelope file %s was written for %s, not %s. Move it aside to give %s its own", path, pe.Dir, dir, dir)
	}
	return pe, nil
}

// proposeDefault puts the default envelope of dir on the floor as a card,
// unless a proposal for the project waits already.
func (s *Server) proposeDefault(name, dir string) error {
	if _, err := os.Stat(s.proposalPath(name)); err == nil {
		return nil
	}
	p := envelopeProposal{Project: name, Dir: dir, Envelope: json.RawMessage(envdefault.Envelope(dir)),
		By: "default", At: proposalStamp(s.clock())}
	b, _ := json.Marshal(p)
	return writeFileAtomic(s.proposalPath(name), b, 0o600)
}

// ownerEnvelope says whether the project's envelope file exists and was
// not written by the default grant: the owner approved it or wrote it. A
// proposal never replaces such a file without a card, since even a
// narrower envelope can drop what the owner set, such as a budget.
func ownerEnvelope(name string) bool {
	b, err := os.ReadFile(filepath.Join(envelopeDir(), name+".json"))
	if err != nil {
		return false
	}
	var f struct {
		GrantedBy string `json:"granted_by"`
	}
	_ = json.Unmarshal(b, &f)
	return f.GrantedBy != envdefault.GrantedBy
}

// ownerEnvelopeLine is the card's line for a proposal that would replace
// the owner's envelope.
const ownerEnvelopeLine = "it replaces an envelope the owner approved or wrote"

// widerLines is each way a proposal asks for more than the default
// envelope of its project; empty means it fits.
func (s *Server) widerLines(p envelopeProposal) []string {
	var out []string
	if !trustedDir(p.Dir) {
		out = append(out, notTrustedLine)
	}
	if !s.workerFenced(p.Dir) {
		out = append(out, fenceOffLine)
	}
	if ownerEnvelope(p.Project) {
		out = append(out, ownerEnvelopeLine)
	}
	out = append(out, envdefault.Wider(p.Envelope, p.Tokens, p.Turns, p.Allow, p.Dir)...)
	if p.OwnInvariants > 0 {
		out = append(out, fmt.Sprintf("its own invariants (%d): the owner reads them; the deny list is always the default's", p.OwnInvariants))
	}
	return out
}
