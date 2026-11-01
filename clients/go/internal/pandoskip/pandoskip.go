// Package pandoskip is the opt-out of the Pando live feed. The owner talks
// to some agents directly, and those chats go into the memory. A worker the
// steward started, and a model call that daisugi or coppice makes for its
// own work, are not the owner's chats: they get no view and are never
// logged. A process that is not a chat of the owner's has DAISUGI_PANDO_SKIP
// set, and so has every process it starts.
package pandoskip

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Var is the environment variable the hooks and the Hermes plugin check.
const Var = "DAISUGI_PANDO_SKIP"

// On is whether the value of Var says to skip. "" and "0" mean no.
func On(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "0"
}

// Environ is environ with Var set to 1. A nil environ is this process's own
// environment, as exec treats nil.
func Environ(environ []string) []string {
	if environ == nil {
		environ = os.Environ()
	}
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if strings.HasPrefix(kv, Var+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, Var+"=1")
}

// BranchVar names the Pando branch of a long-running agent or of a worker
// that works for one (round 8b): a path of labels under the store, such as
// "trellis" or "trellis/ui". The process sees that branch and everything
// below it, never the store above it or a sibling branch. With Var set too,
// the process is a short worker: it gets read-only search and zoom on the
// branch, no view, and nothing it says is logged by its hooks.
const BranchVar = "DAISUGI_PANDO_BRANCH"

// WithBranch is environ with BranchVar set to path ("" removes it).
func WithBranch(environ []string, path string) []string {
	if environ == nil {
		environ = os.Environ()
	}
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, BranchVar+"=") {
			out = append(out, kv)
		}
	}
	if path == "" {
		return out
	}
	return append(out, BranchVar+"="+path)
}

// ListVar names the skip list file (<data>/pando/skip.sessions). A spawner
// that sets it makes every `claude -p` call of its child tree list its
// session before it starts (SessionArgs). A process without it passes no
// session flag, and its hooks list the session instead.
const ListVar = "DAISUGI_PANDO_SKIPLIST"

// Lookup is the value of name in environ, or in this process's environment
// when environ is nil.
func Lookup(environ []string, name string) string {
	if environ == nil {
		return os.Getenv(name)
	}
	v := ""
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == name {
			v = val // the last one wins, as exec does
		}
	}
	return v
}

// WithList is environ with ListVar set to path (and nothing else changed).
func WithList(environ []string, path string) []string {
	if environ == nil {
		environ = os.Environ()
	}
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, ListVar+"=") {
			out = append(out, kv)
		}
	}
	return append(out, ListVar+"="+path)
}

// SessionArgs is the flags for one `claude -p` call: when environ names a
// skip list, a fresh session id is written to it and passed as --session-id.
// The list holds the id before the child runs, so no follow poll can take the
// first line of the transcript. nil when there is no list or it cannot be
// written (the child's hooks then list the session, a few ms later).
func SessionArgs(environ []string) []string {
	path := Lookup(environ, ListVar)
	if path == "" || !On(Lookup(environ, Var)) {
		return nil
	}
	id, err := NewUUID()
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	_, werr := f.WriteString(id + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		return nil
	}
	return []string{"--session-id", id}
}

// NewUUID is a version 4 uuid, the form `claude --session-id` takes.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
