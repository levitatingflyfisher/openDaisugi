package server

import (
	"maps"
	"slices"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/steward"

	"daisugi-verify/pando"
)

// pandoAgentEnv is env with the two Pando opt-out variables set. A pane that
// a pane, a plugin or an unplaced peer asks for (an agent, never the
// operator) is a worker: it gets no memory view and none of its chat is
// logged into the owner's work. The variables ride on the layout record, so
// a resume, a restore and a fork keep them.
//
// Edge: the owner who types `coppice pane create` in a shell inside a
// coppice pane is a pane-role caller too, and the new pane is marked. Chats
// the owner starts from the floor, the phone or a plain terminal are not.
func (s *Server) pandoAgentEnv(env map[string]string) map[string]string {
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out[pando.SkipVar] = "1"
	out[pando.SkipListVar] = pando.SkipListIn(steward.StoreRoot(s.cfg.DataDir))
	return out
}

// listedClaudeArgv is a marked claude pty pane's argv with a minted
// --session-id after the program, and that id. The id is on the skip list
// before the process runs, so no follow poll can take the first line of its
// transcript. The saved argv never holds the flag: a restart mints a new id,
// and a resume (which has its own --resume) is left alone. sid is "" when
// the pane is not such a pane or the list cannot be written (its hooks list
// the session then).
func (s *Server) listedClaudeArgv(rec layout.Pane) (argv []string, sid string) {
	if rec.Kind != layout.KindPTY || rec.Harness != "claude" || len(rec.Argv) == 0 ||
		!pando.SkipOn(func(k string) string { return rec.Env[k] }) {
		return rec.Argv, ""
	}
	for _, a := range rec.Argv[1:] {
		if slices.Contains([]string{"--session-id", "--resume", "-r", "--continue", "-c", "--fork-session"}, a) {
			return rec.Argv, ""
		}
	}
	id, err := pando.NewSkippedSession(steward.StoreRoot(s.cfg.DataDir))
	if err != nil {
		return rec.Argv, ""
	}
	out := append([]string{rec.Argv[0], "--session-id", id}, rec.Argv[1:]...)
	return out, id
}
