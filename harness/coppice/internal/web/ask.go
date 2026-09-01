package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// ErrNoAsk means the gate is not holding an ask under that id any more.
	ErrNoAsk = errors.New("no pending ask")
	// ErrBadDecision means the caller sent something that is not a decision.
	ErrBadDecision = errors.New("decision must be allow or deny")
	// ErrBadScope means the caller sent a scope other than once or task.
	ErrBadScope = errors.New("scope must be once or task")
	// ErrPermanentTask means the caller asked to allow a permanent ask for
	// the whole task. A permanent ask is allowed once, by name, or not at
	// all.
	ErrPermanentTask = errors.New("a permanent ask cannot be allowed for the task. Allow it once")
)

// The tiers an ask can carry. They match proto.TierUndoable and
// proto.TierPermanent.
const (
	TierUndoable  = "undoable"
	TierPermanent = "permanent"
)

// NeedsNameError refuses an allow on a permanent ask that does not carry
// the pane name. Name is what the operator must type.
type NeedsNameError struct{ Name string }

func (e *NeedsNameError) Error() string {
	return "this cannot be undone. Type the pane name to allow: " + e.Name
}

// Reply is one answer to one ask. Tier is the tier the caller holds for
// the ask, from the pane's state. Name is the pane's name: its label, or
// its id when it has none. Confirm is what the operator typed. Scope is
// "", "once" or "task". By names who gave the answer, and WhoFrom says how
// that name is known: token, socket, pane or plugin. An empty By writes
// local from none.
type Reply struct {
	ToolUseID string
	Decision  string
	Reason    string
	Scope     string
	Confirm   string
	Tier      string
	Name      string
	By        string
	WhoFrom   string
}

// worstTier is undoable only when both tiers say undoable. A missing or
// unknown tier on either side makes the ask permanent.
func worstTier(a, b string) string {
	if a == TierUndoable && b == TierUndoable {
		return TierUndoable
	}
	return TierPermanent
}

// CheckAllow is the permanent rule. A permanent ask needs confirm equal to
// name, and name may not be empty. A permanent ask may not be allowed for
// the task. An undoable ask passes.
func CheckAllow(tier, name, confirm, scope string) error {
	if tier == TierUndoable {
		return nil
	}
	if scope == "task" {
		return ErrPermanentTask
	}
	confirm = strings.TrimSpace(confirm)
	if name == "" || confirm == "" || confirm != name {
		return &NeedsNameError{Name: name}
	}
	return nil
}

var unsafeIDChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// SafeID is the Go port of src/opendaisugi/ask.py's _safe, in its order:
// replace every character outside [A-Za-z0-9._-] with "_", strip leading and
// trailing dots, cut to 128 characters, then fall back to "none". The two
// must agree exactly, because the file stem is the only thing joining an
// answer to its ask. testdata/safe_id_cases.json holds both languages to it.
func SafeID(raw string) string {
	s := unsafeIDChars.ReplaceAllString(raw, "_")
	s = strings.Trim(s, ".")
	if len(s) > 128 {
		s = s[:128]
	}
	if s == "" {
		return "none"
	}
	return s
}

// AskChannel writes into the gate's ask directory. The gate has no command
// for answering an ask over the wire, so the phone uses the same file
// protocol the terminal view uses.
//
// Root is the gate directory the caller resolved. This package never reads
// the home directory or an environment variable to find it.
type AskChannel struct{ Root string }

// Answer records one decision, echoing the nonce from the ask it answers.
// It is the one door every answer goes through: the coppice socket's
// agent.allow and agent.deny, and the phone's answer route. The nonce it
// reads is whichever ask is live under the tool_use_id at the moment this
// runs, not necessarily the one the phone had on screen when the operator
// tapped. A retried tool_use_id can carry a newer ask by the time the tap
// lands, and the decision then answers that later ask.
//
// It fails closed, and in this order. An unknown decision, or an allow
// with an unknown scope, writes nothing. A missing ask, or an ask with no nonce, writes nothing, because
// the gate retires both files the instant it sees any answer, so an answer
// it will reject would burn the ask cycle for no reason. Then an allow
// meets the permanent rule. The tier is the worst of r.Tier and the tier
// the gate wrote in the ask file, so the ask is undoable only when both
// say so. A deny passes every check but the ask-exists one, and its scope
// is ignored.
func (a AskChannel) Answer(r Reply) error {
	if r.Decision != "allow" && r.Decision != "deny" {
		return ErrBadDecision
	}
	if r.Decision == "allow" && r.Scope != "" && r.Scope != "once" && r.Scope != "task" {
		return ErrBadScope
	}
	if a.Root == "" {
		return errors.New("web: no gate root. Options.Gate was never set")
	}
	id := SafeID(r.ToolUseID)
	raw, err := os.ReadFile(filepath.Join(a.Root, "asks", id+".json"))
	if err != nil {
		return ErrNoAsk
	}
	var ask struct {
		Nonce string `json:"nonce"`
		Tier  any    `json:"tier"`
	}
	if json.Unmarshal(raw, &ask) != nil || ask.Nonce == "" {
		return ErrNoAsk
	}
	if r.Decision == "allow" {
		fileTier, _ := ask.Tier.(string)
		if err := CheckAllow(worstTier(r.Tier, fileTier), r.Name, r.Confirm, r.Scope); err != nil {
			return err
		}
	}
	scope := r.Scope
	if scope == "" || r.Decision == "deny" {
		scope = "once"
	}
	by, from := r.By, r.WhoFrom
	if by == "" || from == "" {
		by, from = "local", "none"
	}
	body, err := json.Marshal(map[string]any{
		"toolUseId":    r.ToolUseID,
		"decision":     r.Decision,
		"reason":       r.Reason,
		"updatedInput": nil,
		"nonce":        ask.Nonce,
		"scope":        scope,
		"by":           by,
		"whoFrom":      from,
	})
	if err != nil {
		return err
	}
	dir := filepath.Join(a.Root, "answers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".answer-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Rename so a reader never sees half a file. Answer already read the ask
	// before writing anything, so the answer's mtime is not earlier than the
	// ask's. The gate's wait_answer rejects an answer older than the ask it
	// claims to answer.
	return os.Rename(tmp.Name(), filepath.Join(dir, id+".json"))
}
