package server

import (
	"fmt"

	"github.com/opendaisugi/coppice/internal/proto"
)

// PluginSpec is what an enabled plugin holds: the verbs it may run and the
// event kinds it may subscribe to.
type PluginSpec struct {
	Needs   []string
	Listens []string
}

// pluginRefusal is the refusal for a verb a plugin did not ask for.
func pluginRefusal(id, verb string) string {
	return fmt.Sprintf("plugin %s did not ask for %s in its manifest", id, verb)
}

// readVerbs only read. A plugin that runs one makes no note.
var readVerbs = map[string]bool{
	"pane.list": true, "session.list": true, "pane.read": true, "task.list": true,
	"agent.list": true, "agent.get": true, "server.status": true, "floor.notes": true,
	"pane.explain": true, "floor.facts": true,
}

// eventVerbs manage a subscription. A plugin holds them when it listens to
// any event kind.
var eventVerbs = map[string]bool{"events.pause": true, "events.resume": true}

// SetPlugins records the enabled plugins. A plugin connection whose id is
// not here holds no verb at all.
func (s *Server) SetPlugins(specs map[string]PluginSpec) {
	s.plugMu.Lock()
	defer s.plugMu.Unlock()
	s.plugSpecs = map[string]PluginSpec{}
	for id, sp := range specs {
		s.plugSpecs[id] = PluginSpec{
			Needs:   append([]string{}, sp.Needs...),
			Listens: append([]string{}, sp.Listens...),
		}
	}
}

func (s *Server) pluginSpec(id string) (PluginSpec, bool) {
	s.plugMu.RLock()
	defer s.plugMu.RUnlock()
	sp, ok := s.plugSpecs[id]
	return sp, ok
}

// LaunchPlugin runs start, which starts the process of plugin id and
// returns its pid, and records the pid. It holds the lock that placing a
// new connection takes, so the policy is placed as a plugin even when it
// dials before start returns.
func (s *Server) LaunchPlugin(id string, start func() (int, error)) (int, error) {
	s.plugMu.Lock()
	defer s.plugMu.Unlock()
	pid, err := start()
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, fmt.Errorf("plugin %s started with no pid", id)
	}
	s.plugPIDs[pid] = id
	delete(s.plugExited, pid)
	return pid, nil
}

// PluginExited marks the pid of a policy that exited. A process that left
// the policy's group but stayed in its session is still that plugin, so
// the pid is kept until the session is empty. Linux never gives a pid to a
// new process while it names a live session, so a kept pid cannot come
// to name another process.
func (s *Server) PluginExited(pid int) {
	s.plugMu.Lock()
	defer s.plugMu.Unlock()
	if _, ok := s.plugPIDs[pid]; !ok {
		return
	}
	s.plugExited[pid] = true
	s.prunePlugins()
}

// prunePlugins forgets each exited policy whose session is empty. The
// caller holds plugMu for writing. A session the server cannot scan counts
// as not empty, so its pid stays a plugin.
func (s *Server) prunePlugins() {
	if len(s.plugExited) == 0 {
		return
	}
	pids, err := s.listPIDs()
	if err != nil {
		return
	}
	live := map[int]bool{}
	for _, pid := range pids {
		if _, sid, err := s.procStat(pid); err == nil && s.plugExited[sid] {
			live[sid] = true
		}
	}
	for pid := range s.plugExited {
		if !live[pid] {
			delete(s.plugExited, pid)
			delete(s.plugPIDs, pid)
		}
	}
}

// pluginGuard refuses a verb plugin id did not ask for. ok is false with
// the refusal when it refuses.
func (s *Server) pluginGuard(id string, r *proto.Request) (proto.Response, bool) {
	sp, known := s.pluginSpec(id)
	if !known {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized,
			fmt.Sprintf("plugin %s is not enabled. It holds no verb.", id)), false
	}
	// A plugin is not a pane, and a false state could open a blocked pane
	// to typed keys.
	if r.Cmd == "pane.report_state" || r.Cmd == "pane.report_child" {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, "a plugin reports no state. "+PaneRefusal), false
	}
	listens := map[string]bool{}
	for _, k := range sp.Listens {
		listens[k] = true
	}
	if r.Cmd == "events.subscribe" {
		kinds, ok := r.StrSlice("kinds")
		if !ok || len(kinds) == 0 {
			kinds = []string{"state", "layout"}
		}
		for _, k := range kinds {
			if !listens[k] {
				return proto.ErrResp(r.ID, proto.ErrUnauthorized,
					fmt.Sprintf("plugin %s does not listen to %s in its manifest", id, k)), false
			}
		}
		return proto.Response{}, true
	}
	if eventVerbs[r.Cmd] {
		if len(listens) == 0 {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, pluginRefusal(id, r.Cmd)), false
		}
		return proto.Response{}, true
	}
	for _, v := range sp.Needs {
		if v == r.Cmd {
			return proto.Response{}, true
		}
	}
	return proto.ErrResp(r.ID, proto.ErrUnauthorized, pluginRefusal(id, r.Cmd)), false
}
