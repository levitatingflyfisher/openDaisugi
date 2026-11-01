package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/layout"
)

// An envelope on a task (design-delegation-tree.md, option 3). coppice asks
// daisugi to prove each edge (tree check), to register the envelope under a
// minted session (tree root, tree spawn) and to enforce it (warden check
// --session). It decides nothing itself (ADR-0020).

// TaskRefusal is the message a pane or a plugin gets for making a root task
// with an envelope: only the operator's envelope can be a root.
const TaskRefusal = "only the operator makes a root task."

// OwnTaskRefusal is the message a pinned pane gets for starting a pane
// outside its own task and the tasks below it (W3K-12).
const OwnTaskRefusal = "a pane starts panes only under its own task."

const daisugiTaskTimeout = 60 * time.Second

// pinMu serializes the registration of task sessions, so two panes started
// into one task together mint one session.
var pinMu sync.Mutex

// daisugiDataArgs is --data-dir for the daisugi calls that read or write
// the tree ledger, from COPPICE_DAISUGI_DATA_DIR, or none.
func daisugiDataArgs() []string {
	if d := os.Getenv("COPPICE_DAISUGI_DATA_DIR"); d != "" {
		return []string{"--data-dir", d}
	}
	return nil
}

// runDaisugiTask runs daisugi with args and returns its combined output.
func runDaisugiTask(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daisugiTaskTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, daisugiBin(), args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// reasonsOf is the first lines of daisugi's output, joined, for a refusal.
func reasonsOf(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > 6 {
		keep = keep[:6]
	}
	return strings.Join(keep, " ")
}

// envelopeFile writes env to a private file under the data dir and returns
// its path. The files stay: daisugi reads them again on a resume.
func (s *Server) envelopeFile(taskID string, env json.RawMessage) (string, error) {
	dir := filepath.Join(s.cfg.DataDir, "task-envelopes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, taskID+".json")
	if err := os.WriteFile(path, env, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// envelopeAbove is the nearest task above id (not id itself) that has an
// envelope, or false.
func (s *Server) envelopeAbove(id string) (layout.Task, bool) {
	tk, ok := s.tree.Task(id)
	for seen := 0; ok && tk.Parent != "" && seen < 64; seen++ {
		tk, ok = s.tree.Task(tk.Parent)
		if ok && len(tk.Envelope) > 0 {
			return tk, true
		}
	}
	return layout.Task{}, false
}

// pinnedTask is the task whose envelope binds panes in task id: the task
// itself when it has one, else the nearest one above. False when none does.
func (s *Server) pinnedTask(id string) (layout.Task, bool) {
	tk, ok := s.tree.Task(id)
	if !ok {
		return layout.Task{}, false
	}
	if len(tk.Envelope) > 0 {
		return tk, true
	}
	return s.envelopeAbove(id)
}

// proveEdge asks daisugi whether child fits inside parent. A refusal is
// daisugi's reasons, joined.
func (s *Server) proveEdge(parent layout.Task, child json.RawMessage) error {
	pf, err := s.envelopeFile(parent.ID, parent.Envelope)
	if err != nil {
		return err
	}
	cf := filepath.Join(s.cfg.DataDir, "task-envelopes", "candidate-"+randHex(6)+".json")
	if err := os.WriteFile(cf, child, 0o600); err != nil {
		return err
	}
	defer os.Remove(cf)
	out, err := runDaisugiTask("tree", "check", pf, cf)
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return errors.New(reasonsOf(out))
	}
	return fmt.Errorf("could not run daisugi tree check: %v", err)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// pinRoot is the warden root a pinned pane's hook reads: the configured
// one, else the first armed one.
func (s *Server) pinRoot() string {
	if s.cfg.WardenRoot != "" {
		return s.cfg.WardenRoot
	}
	for _, r := range s.wardenRoots() {
		if wardenArmed(r) {
			return r
		}
	}
	return ""
}

// taskSession is the daisugi session task tk's envelope is registered
// under. It registers it first when no pane has yet: tree root for a task
// with no envelope above it, tree spawn --parent for one with, which proves
// the edge again (TR-R-8). The session is minted t<id>-<16 hex> and kept on
// the task, so every pane of the task shares one budget.
func (s *Server) taskSession(tk layout.Task) (string, error) {
	pinMu.Lock()
	defer pinMu.Unlock()
	return s.taskSessionLocked(tk.ID)
}

func (s *Server) taskSessionLocked(id string) (string, error) {
	tk, ok := s.tree.Task(id)
	if !ok {
		return "", fmt.Errorf("no task %s", id)
	}
	if tk.Session != "" {
		return tk.Session, nil
	}
	root := s.pinRoot()
	if root == "" {
		return "", errors.New("task " + id + " has an envelope, and no warden is armed to enforce it. " +
			"Run daisugi install, or start the server with a warden root.")
	}
	file, err := s.envelopeFile(id, tk.Envelope)
	if err != nil {
		return "", err
	}
	session := id + "-" + randHex(8)
	args := []string{"tree"}
	if up, ok := s.envelopeAbove(id); ok {
		parentSession, err := s.taskSessionLocked(up.ID)
		if err != nil {
			return "", err
		}
		args = append(args, "spawn", file, "--parent", parentSession, "--session", session)
	} else {
		args = append(args, "root", file, "--session", session)
	}
	args = append(args, "--root", root)
	args = append(args, daisugiDataArgs()...)
	if out, err := runDaisugiTask(args...); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("daisugi %s refused task %s's envelope: %s", args[1], id, reasonsOf(out))
		}
		return "", fmt.Errorf("could not run daisugi %s: %v", args[1], err)
	}
	if err := s.tree.SetTaskSession(id, session); err != nil {
		return "", err
	}
	s.markLayoutDirty()
	return session, nil
}

// paneSession is the session a pane about to start under rec is pinned to,
// or "" when its task (and every task above it) has no envelope. An error
// refuses the start: a pane in a task with an envelope never runs unpinned.
func (s *Server) paneSession(rec layout.Pane) (string, error) {
	if rec.TaskID == "" {
		return "", nil
	}
	pt, ok := s.pinnedTask(rec.TaskID)
	if !ok {
		return "", nil
	}
	return s.taskSession(pt)
}

// ownTaskOnly is true when the connection is a pane whose task is pinned
// to an envelope and taskID is neither that task nor one below it. Any
// other connection, and a pane with no pinned task, may start a pane
// anywhere, as before.
func (s *Server) ownTaskOnly(c *Client, taskID string) bool {
	if c == nil {
		return false
	}
	ro := c.roleOf()
	if !ro.pane || ro.paneID == "" {
		return false
	}
	p, ok := s.tree.Pane(ro.paneID)
	if !ok || p.TaskID == "" {
		return false
	}
	if _, pinned := s.pinnedTask(p.TaskID); !pinned {
		return false
	}
	for id, seen := taskID, 0; id != "" && seen < 64; seen++ {
		if id == p.TaskID {
			return false
		}
		tk, ok := s.tree.Task(id)
		if !ok {
			break
		}
		id = tk.Parent
	}
	return true
}
