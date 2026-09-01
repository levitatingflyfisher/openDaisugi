package server

import (
	"errors"
	"fmt"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/worktree"
)

// RegisterTaskCommands wires up the task verbs. New calls it beside
// RegisterPaneCommands.
func (s *Server) RegisterTaskCommands() {
	_ = s.Handle("task.create", s.handleTaskCreate)
	_ = s.Handle("task.list", s.handleTaskList)
	_ = s.Handle("task.close", s.handleTaskClose)
	_ = s.Handle("task.move", s.handleTaskMove)
	_ = s.Handle("task.set_foreman", s.handleSetForeman)
}

// handleSetForeman names the pane that hears a task's asks first, for the
// task and every descendant that names no nearer one. An empty pane clears
// it. guard lets only the operator run it.
func (s *Server) handleSetForeman(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("task")
	if id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"task.set_foreman needs task. Run: coppice task list")
	}
	if _, ok := s.tree.Task(id); !ok {
		return noTask(r.ID, id)
	}
	pane, _ := r.Str("pane")
	if pane != "" {
		p, ok := s.tree.Pane(pane)
		if !ok {
			return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
				fmt.Sprintf("no pane %q. Run: coppice pane list", pane))
		}
		if p.Closed {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("pane %s is closed. A foreman must be open.", pane))
		}
	}
	if err := s.tree.SetForeman(id, pane); err != nil {
		return noTask(r.ID, id)
	}
	s.saveLayout()
	s.recheckHolds()
	return proto.OKResp(r.ID, map[string]any{"task": id, "foreman": pane})
}

// keepWorktreeHint is the sentence every refusal about a worktree ends
// with: the two ways out of a tree coppice will not delete.
const keepWorktreeHint = "Commit them, or pass keep_worktree: true."

// noTask is the refusal every verb gives for an id that names no task.
func noTask(reqID, id string) proto.Response {
	return proto.ErrResp(reqID, proto.ErrBadRequest,
		fmt.Sprintf("no task %s. Run: coppice task list", id))
}

// handleTaskCreate records a task. With worktree true it first finds the
// repo that holds cwd and adds a worktree named after the label beside it.
// The worktree is made before the record, so a failed add leaves nothing.
func (s *Server) handleTaskCreate(_ *Client, r *proto.Request) proto.Response {
	label, _ := r.Str("label")
	label = cleanLabel(label)
	if label == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"task.create needs label. Pass the task's name.")
	}
	parent, _ := r.Str("parent")
	if parent != "" {
		if _, ok := s.tree.Task(parent); !ok {
			return noTask(r.ID, parent)
		}
	}
	cwd, _ := r.Str("cwd")
	model, _ := r.Str("model")
	wantTree, _ := r.Bool("worktree")
	path := ""
	repo := ""
	if wantTree {
		if cwd == "" {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				"task.create with worktree needs cwd. Pass a directory inside the repo.")
		}
		top, err := worktree.Toplevel(cwd)
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("worktree needs a git repo. %s is not inside one.", cwd))
		}
		repo = top
		path, err = worktree.Add(repo, label)
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
		}
	}
	rec, err := s.tree.CreateTask(layout.Task{
		Label: label, Parent: parent, Cwd: cwd, Worktree: path, Model: model,
	})
	if err != nil {
		if path != "" {
			_ = worktree.Remove(repo, label, false)
		}
		if errors.Is(err, layout.ErrNoTask) {
			return noTask(r.ID, parent)
		}
		return proto.ErrResp(r.ID, proto.ErrInternal, err.Error())
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{"task": rec.ID, "worktree": path})
}

// taskRow is one task.list row. ahead is present only when the task has a
// worktree and git could count its commits past the upstream.
func (s *Server) taskRow(tk layout.Task, paneState func(string) string) map[string]any {
	panes := []string{}
	for _, p := range s.tree.Panes() {
		if p.TaskID == tk.ID {
			panes = append(panes, p.ID)
		}
	}
	row := map[string]any{
		"id": tk.ID, "label": tk.Label, "parent": tk.Parent, "cwd": tk.Cwd,
		"worktree": tk.Worktree, "model": tk.Model, "foreman": tk.Foreman,
		"state": layout.TaskState(tk, s.tree, paneState),
		"panes": panes,
	}
	if tk.Worktree != "" {
		if ahead, _, _, err := worktree.Status(tk.Worktree); err == nil {
			row["ahead"] = ahead
		}
	}
	return row
}

// RootLabel is the first label of every path: the floor itself, above
// every task.
const RootLabel = "floor"

// scopeOf reads the scope a list verb names. With no scope, keep is nil
// and path is the root alone. With a scope, keep holds the task and every
// descendant, and path holds the labels from the root down to the task. A
// scope that names no task is a refusal.
func (s *Server) scopeOf(r *proto.Request) (keep map[string]bool, path []string, resp *proto.Response) {
	path = []string{RootLabel}
	id, _ := r.Str("scope")
	if id == "" {
		return nil, path, nil
	}
	sub, err := s.tree.Subtree(id)
	if err != nil {
		bad := noTask(r.ID, id)
		return nil, nil, &bad
	}
	keep = map[string]bool{}
	for _, tk := range sub {
		keep[tk.ID] = true
	}
	var chain []string
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		seen[cur] = true
		tk, ok := s.tree.Task(cur)
		if !ok {
			break
		}
		label := tk.Label
		if label == "" {
			label = tk.ID
		}
		chain = append([]string{label}, chain...)
		cur = tk.Parent
	}
	return keep, append(path, chain...), nil
}

// handleTaskList lists the tasks with their folded state. A blocked pane
// whose ask a foreman holds folds as working: the task waits on its
// foreman, not on the operator.
func (s *Server) handleTaskList(_ *Client, r *proto.Request) proto.Response {
	keep, path, bad := s.scopeOf(r)
	if bad != nil {
		return *bad
	}
	paneState := func(id string) string {
		ev, ok := s.effectiveState(id)
		if !ok {
			return proto.StateUnknown
		}
		if ev.State == proto.StateBlocked && s.heldFor(id) != nil {
			return proto.StateWorking
		}
		return ev.State
	}
	out := []map[string]any{}
	for _, tk := range s.tree.Tasks() {
		if keep != nil && !keep[tk.ID] {
			continue
		}
		out = append(out, s.taskRow(tk, paneState))
	}
	return proto.OKResp(r.ID, map[string]any{"tasks": out, "path": path})
}

// handleTaskClose closes a task and everything under it. Every worktree in
// the subtree is checked for uncommitted changes before anything happens.
// Then the panes close, then the worktrees go, then the records. Branches
// are never deleted.
//
// The steps run under no lock of their own, and the server runs each
// request on its own goroutine. Two concurrent closes of one task both
// pass the dirty check; the first removes the worktrees and the records,
// and the second fails at the removal with an internal error, or at
// CloseTask with the no-task refusal, and changes nothing more.
func (s *Server) handleTaskClose(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("task")
	if id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"task.close needs task. Run: coppice task list")
	}
	subtree, err := s.tree.Subtree(id)
	if err != nil {
		return noTask(r.ID, id)
	}
	keep, _ := r.Bool("keep_worktree")
	if !keep {
		for _, tk := range subtree {
			if tk.Worktree == "" {
				continue
			}
			dirty, err := worktree.Dirty(tk.Worktree)
			if err != nil {
				return proto.ErrResp(r.ID, proto.ErrBadRequest,
					fmt.Sprintf("task %s has a worktree at %s that git cannot read: %v. Pass keep_worktree: true to leave it.",
						id, tk.Worktree, err))
			}
			if dirty {
				return proto.ErrResp(r.ID, proto.ErrBadRequest,
					fmt.Sprintf("task %s has uncommitted changes in %s. %s", id, tk.Worktree, keepWorktreeHint))
			}
		}
	}
	gone := map[string]bool{}
	for _, tk := range subtree {
		gone[tk.ID] = true
	}
	closed := []string{}
	for _, p := range s.tree.Panes() {
		if !gone[p.TaskID] || p.Closed {
			continue
		}
		s.closeRecord(p)
		closed = append(closed, p.ID)
	}
	if !keep {
		for _, tk := range subtree {
			if tk.Worktree == "" {
				continue
			}
			if hook := s.beforeWorktreeRemove; hook != nil {
				hook(tk.Worktree)
			}
			repo, err := worktree.Repo(tk.Worktree)
			if err == nil {
				err = worktree.Remove(repo, tk.Label, false)
			}
			if err != nil {
				s.saveLayout()
				return proto.ErrResp(r.ID, proto.ErrInternal,
					fmt.Sprintf("task %s: its panes closed, but git kept the worktree at %s: %v. %s",
						id, tk.Worktree, err, keepWorktreeHint))
			}
		}
	}
	if err := s.tree.CloseTask(id); err != nil {
		s.saveLayout()
		return noTask(r.ID, id)
	}
	s.saveLayout()
	return proto.OKResp(r.ID, map[string]any{"task": id, "closed": true, "panes": closed})
}

func (s *Server) handleTaskMove(_ *Client, r *proto.Request) proto.Response {
	id, _ := r.Str("task")
	if id == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			"task.move needs task. Run: coppice task list")
	}
	parent, _ := r.Str("parent")
	if err := s.tree.MoveTask(id, parent); err != nil {
		if errors.Is(err, layout.ErrCycle) {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("task %s cannot move under %s: that is itself or one of its own descendants.", id, parent))
		}
		missing := id
		if _, ok := s.tree.Task(id); ok {
			missing = parent
		}
		return noTask(r.ID, missing)
	}
	s.saveLayout()
	s.recheckHolds()
	return proto.OKResp(r.ID, map[string]any{"task": id, "parent": parent})
}
