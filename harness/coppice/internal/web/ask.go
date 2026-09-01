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
)

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
// The nonce it reads is whichever ask is live under toolUseID at the
// moment this runs, not necessarily the one the phone had on screen when
// the operator tapped. A retried tool_use_id can carry a newer ask by the
// time the tap lands, and the decision then answers that later ask.
//
// It fails closed in three ways, and each matters. An unknown decision
// writes nothing. A missing ask writes nothing, because the gate retires
// both files the instant it sees any answer, so an answer it will reject
// would burn the ask cycle for no reason. An ask with no nonce writes
// nothing, for the same reason.
func (a AskChannel) Answer(toolUseID, decision, reason string) error {
	if decision != "allow" && decision != "deny" {
		return ErrBadDecision
	}
	if a.Root == "" {
		return errors.New("web: no gate root. Options.Gate was never set")
	}
	id := SafeID(toolUseID)
	raw, err := os.ReadFile(filepath.Join(a.Root, "asks", id+".json"))
	if err != nil {
		return ErrNoAsk
	}
	var ask struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(raw, &ask) != nil || ask.Nonce == "" {
		return ErrNoAsk
	}
	body, err := json.Marshal(map[string]any{
		"toolUseId":    toolUseID,
		"decision":     decision,
		"reason":       reason,
		"updatedInput": nil,
		"nonce":        ask.Nonce,
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
