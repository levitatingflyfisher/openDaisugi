package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/textwidth"
	"github.com/opendaisugi/coppice/internal/web"
)

// PaneRefusal is the one message a pane connection gets for a verb that
// only the operator may run.
const PaneRefusal = "a pane can propose. It cannot allow."

// ForemanRefusal is the message a pane or a plugin gets for a verb that
// names a foreman.
const ForemanRefusal = "only the operator names a foreman."

// PluginRefusal is the one message a plugin connection gets for an allow
// verb.
const PluginRefusal = "a plugin can propose. It cannot allow."

// peerFacts is what the kernel says about the process on the other end of
// a socket connection. checked is false for a connection that did not come
// through Serve on Linux, a test pipe or a system with no peer pid. Then
// the hello alone decides the role.
type peerFacts struct {
	checked bool
	// pane is true when the peer is a pane process or a descendant of one.
	pane bool
	// paneID names the pty pane the peer runs in, or "" for a headless
	// pane, which the walk cannot name.
	paneID string
	// unknown is true when the peer could not be placed. It counts as a
	// pane for the allow verbs only.
	unknown bool
	// plugin names the policy the peer is, or runs under, when the server
	// started that policy. Such a peer holds only its plugin's verbs.
	plugin string
	// self is true when the peer is the server process itself, which is
	// where the phone server runs.
	self bool
}

// maxWalk caps the parent walk. A real process tree is never this deep.
const maxWalk = 256

// classifyPeer places pid. self is the server's pid, and leader says
// whether the server leads its own session. panes maps each pty pane's
// child pid to its pane id, and plugins each policy's pid to its plugin
// id. stat returns a pid's parent and session.
//
// The peer is a plugin when a pid on its parent chain is a policy the
// server started, or when its session is a policy's session. A policy is
// a child of the server, so the walk checks for it before it reaches the
// server itself.
//
// The peer is a pane when a pid on its parent chain, itself included, is a
// pane's child, or is the server with the peer below it. It is also a pane
// when its session is a pty pane's session, which catches a double fork
// that left the parent chain, or the server's own session when the server
// leads it. The checks stop a process that stays in the pane's tree or in
// its session. A process a daemon starts for the pane, through setsid -f,
// systemd-run, ssh or a tmux server, is outside both and reads as the
// operator. The server itself is the operator: the phone server runs
// inside it.
func classifyPeer(pid, self int, leader bool, panes, plugins map[int]string,
	stat func(int) (ppid, sid int, err error)) peerFacts {
	if pid == self {
		return peerFacts{checked: true, self: true}
	}
	_, sid, err := stat(pid)
	if err != nil {
		return peerFacts{checked: true, unknown: true}
	}
	cur := pid
	for i := 0; i < maxWalk && cur > 1; i++ {
		if id, ok := panes[cur]; ok {
			return peerFacts{checked: true, pane: true, paneID: id}
		}
		if id, ok := plugins[cur]; ok {
			return peerFacts{checked: true, plugin: id}
		}
		if cur == self {
			return peerFacts{checked: true, pane: true}
		}
		ppid, _, err := stat(cur)
		if err != nil {
			return peerFacts{checked: true, unknown: true}
		}
		cur = ppid
	}
	if id, ok := panes[sid]; ok {
		return peerFacts{checked: true, pane: true, paneID: id}
	}
	if id, ok := plugins[sid]; ok {
		return peerFacts{checked: true, plugin: id}
	}
	if leader && sid == self {
		return peerFacts{checked: true, pane: true}
	}
	return peerFacts{checked: true}
}

// panePIDs maps the child pid of every live pty pane, and every root pid a
// headless pane's adapter names, to its pane id.
func (s *Server) panePIDs() map[int]string {
	out := map[int]string{}
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	for id, lp := range s.live {
		pid := 0
		switch {
		case lp.PTY != nil:
			pid = lp.PTY.Pid()
		case lp.Adapter != nil:
			if p, ok := lp.Adapter.(pane.Pider); ok {
				for _, root := range p.Pids() {
					if root > 0 {
						out[root] = id
					}
				}
			}
		}
		if pid > 0 {
			out[pid] = id
		}
	}
	return out
}

// role is what a connection is: a pane, and which one, and whether it may
// run the allow verbs.
type role struct {
	pane    bool
	paneID  string
	noAllow bool
	// unknown is true when the kernel could not place the peer.
	unknown bool
	// plugin names the plugin the connection is, or "".
	plugin string
}

// roleOf reads a client's role. A kernel pane is a pane whatever its hello
// said. A hello that names a pane makes any connection a pane. A peer the
// kernel could not place may not allow.
func (c *Client) roleOf() role {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := role{}
	if c.facts.pane {
		r.pane, r.paneID = true, c.facts.paneID
	}
	if c.helloRole == "pane" {
		r.pane = true
	}
	// A kernel pane with no id is a headless pane the walk could not name.
	// Its hello may not borrow another pane's label.
	if r.pane && r.paneID == "" && !c.facts.pane {
		r.paneID = c.helloPane
	}
	r.plugin = c.facts.plugin
	if r.plugin == "" {
		r.plugin = c.helloPlugin
	}
	r.unknown = c.facts.unknown
	r.noAllow = r.pane || r.unknown || r.plugin != ""
	return r
}

// checkName refuses a hello that would change the connection's name. A
// name, once set, may be repeated but not changed. A connection whose name
// came from its token takes no name from a later hello at all, so a
// browser cannot name itself on the phone server's connection.
func (c *Client) checkName(r *proto.Request, name, nameFrom string) *proto.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nameFixed && (name != "" || nameFrom != "") && (name != c.name || nameFrom != "token") {
		resp := proto.ErrResp(r.ID, proto.ErrBadRequest, "this connection takes its name from its token.")
		return &resp
	}
	if c.name != "" && name != "" && name != c.name {
		resp := proto.ErrResp(r.ID, proto.ErrBadRequest, "this connection is "+c.name+".")
		return &resp
	}
	return nil
}

// setName records a name a hello gave, under c.mu. A name that says it
// came from a token counts as a token name only from the server process
// itself, where the phone server runs. From any other process it is a
// socket name. A token hello with no name fixes the connection as
// unnamed.
func (c *Client) setName(name, nameFrom string) {
	if nameFrom == "token" {
		c.nameFixed = true
	}
	if name == "" || c.name != "" {
		return
	}
	c.name = name
	c.nameFrom = "socket"
	if nameFrom == "token" && c.facts.self {
		c.nameFrom = "token"
	}
}

// Who is the name of the connection and how the server knows it. A pane
// is pane:<id> from pane, a plugin is plugin:<id> from plugin, whatever
// either said in a hello. An operator connection has the name its hello
// gave, from token or socket, or no name from none. It reads the role
// each time, so a connection that becomes a pane is that pane.
func (c *Client) Who() (name, from string) {
	ro := c.roleOf()
	switch {
	case ro.plugin != "":
		return "plugin:" + ro.plugin, "plugin"
	case ro.pane && ro.paneID != "":
		return "pane:" + ro.paneID, "pane"
	case ro.pane:
		return "pane", "pane"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.name == "" {
		return "", "none"
	}
	return c.name, c.nameFrom
}

// handleHello records the role and the pane a client says it has. It runs
// in the read loop, before the next line is read, so the verbs after it
// see it. A connection that said it is a pane stays a pane: a later hello
// that says operator changes nothing.
func (s *Server) handleHello(c *Client, r *proto.Request) proto.Response {
	roleName, _ := r.Str("role")
	if roleName == "" {
		roleName = "operator"
	}
	if roleName != "operator" && roleName != "pane" && roleName != "plugin" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "role must be operator, pane or plugin")
	}
	pluginID := ""
	if roleName == "plugin" {
		pluginID, _ = r.Str("plugin")
		if pluginID == "" {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, "role plugin needs plugin, the plugin id.")
		}
	}
	paneID, _ := r.Str("pane")
	name, _ := r.Str("name")
	nameFrom, _ := r.Str("name_from")
	if nameFrom != "" && nameFrom != "token" && nameFrom != "socket" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "name_from must be token or socket.")
	}
	if name != "" {
		if err := web.CheckName(name); err != nil {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, err.Error())
		}
	}
	if bad := c.checkName(r, name, nameFrom); bad != nil {
		return *bad
	}
	placed, bad := s.placeNamedPeers(r)
	if bad != nil {
		return *bad
	}
	// A hello can name the plugin a connection is. It cannot change it,
	// and it cannot name one the server does not run.
	if cur := c.roleOf().plugin; cur != "" && pluginID != "" && pluginID != cur {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "this connection is plugin "+cur+".")
	}
	if pluginID != "" && (c.roleOf().pane || placed.pane || roleName == "pane") {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, "a pane is not a plugin.")
	}
	if pluginID != "" {
		if _, ok := s.pluginSpec(pluginID); !ok {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, "no plugin "+pluginID+" is enabled.")
		}
	}
	c.mu.Lock()
	if pluginID != "" && c.helloPlugin == "" {
		c.helloPlugin = pluginID
	}
	if c.facts.plugin == "" {
		c.facts.plugin = placed.plugin
	}
	c.facts.pane = c.facts.pane || placed.pane
	if c.facts.paneID == "" {
		c.facts.paneID = placed.paneID
	}
	c.facts.unknown = c.facts.unknown || placed.unknown
	if c.helloRole != "pane" {
		c.helloRole = roleName
	}
	if paneID != "" {
		c.helloPane = paneID
	}
	c.setName(name, nameFrom)
	c.mu.Unlock()
	got := c.roleOf()
	res := map[string]any{"role": "operator", "allow": !got.noAllow}
	if who, _ := c.Who(); who != "" {
		res["name"] = who
	}
	if got.plugin != "" {
		res["role"] = "plugin"
		res["plugin"] = got.plugin
		spec, _ := s.pluginSpec(got.plugin)
		res["needs"] = append([]string{}, spec.Needs...)
		return proto.OKResp(r.ID, res)
	}
	if got.pane {
		res["role"] = "pane"
		if got.paneID != "" {
			res["pane"] = got.paneID
		}
	}
	return proto.OKResp(r.ID, res)
}

// placeNamedPeers places the peers a hello names. A web server names the
// local process it serves as peer_pids, or says peer_unknown when it could
// not find one. The names can only take rights away: a pid that is a pane,
// or one that cannot be placed, makes the connection that pane or unknown.
// An empty list is unknown. A system with no peer pids cannot place any
// pid, so every named pid is unknown there.
func (s *Server) placeNamedPeers(r *proto.Request) (peerFacts, *proto.Response) {
	var out peerFacts
	if u, _ := r.Bool("peer_unknown"); u {
		out.unknown = true
	}
	raw, ok := r.Raw("peer_pids")
	if !ok {
		return out, nil
	}
	var pids []int
	if err := json.Unmarshal(raw, &pids); err != nil {
		resp := proto.ErrResp(r.ID, proto.ErrBadRequest, "peer_pids must be a list of pids")
		return out, &resp
	}
	if len(pids) == 0 || !peerPIDSupported {
		out.unknown = true
		return out, nil
	}
	panes := s.panePIDs()
	leader := leadsSession()
	s.plugMu.Lock()
	defer s.plugMu.Unlock()
	s.prunePlugins()
	for _, pid := range pids {
		f := classifyPeer(pid, os.Getpid(), leader, panes, s.plugPIDs, s.procStat)
		if out.plugin == "" {
			out.plugin = f.plugin
		}
		out.pane = out.pane || f.pane
		if out.paneID == "" {
			out.paneID = f.paneID
		}
		out.unknown = out.unknown || f.unknown
	}
	return out, nil
}

// allowVerbs are the verbs only the operator may run, with one exception:
// a foreman may deny an ask it holds. See foremanDeny.
var allowVerbs = map[string]bool{"agent.allow": true, "agent.deny": true}

// TalkRefusal is the message a pane or a plugin gets for floor.talk. The
// owner's words go to the foreman, and a pane's words are not the owner's.
const TalkRefusal = "only the operator talks to the floor's foreman."

// operatorVerbs are verbs only the operator may run, with no exception,
// each with its refusal.
var operatorVerbs = map[string]string{
	"task.set_foreman": ForemanRefusal, "floor.talk": TalkRefusal, "floor.foreman": ForemanRefusal,
	"pane.trust": TrustRefusal,
}

// ForemanLabelRefusal is the message a pane or a plugin gets for a pane
// label that would read as the floor's foreman.
const ForemanLabelRefusal = "only the operator names a pane foreman."

// labelVerbs are the verbs that give a pane a label.
var labelVerbs = map[string]bool{"pane.create": true, "pane.split": true, "pane.fork": true, "pane.rename": true}

// labelMax is how many runes a pane label keeps.
const labelMax = 64

// cleanLabel is label with its control characters and its Unicode
// format characters (zero-width and bidi marks) dropped, cut to labelMax
// runes, and trimmed. Every screen draws labels, so none may carry an
// escape code, look like another label, or run on without end.
func cleanLabel(label string) string {
	label = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, label)
	return strings.TrimSpace(textwidth.Printable(strings.TrimSpace(label), labelMax))
}

// harnessMax is how many characters a harness name may have.
const harnessMax = 32

// validHarness reports whether name has the shape of a harness name in
// coppice.toml: a letter or digit, then letters, digits, '.', '_' or '-',
// at most harnessMax in all.
func validHarness(name string) bool {
	if name == "" || len(name) > harnessMax {
		return false
	}
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok && (i == 0 || (r != '.' && r != '_' && r != '-')) {
			return false
		}
	}
	return true
}

// LabelTakenRefusal is the message a pane or a plugin gets for a label
// another live pane has. The operator may share a label; an agent may
// not pose as another.
const LabelTakenRefusal = "another live pane has that label. Only the operator gives two panes one name."

// isForemanLabel reports whether label, once cleaned, reads as foreman.
func isForemanLabel(label string) bool {
	return strings.EqualFold(cleanLabel(label), DefaultForemanLabel)
}

// roleOrOperator is c's role, or the operator's for a call the server
// makes itself with no client.
func roleOrOperator(c *Client) role {
	if c == nil {
		return role{}
	}
	return c.roleOf()
}

// RenameRefusal is the message a pane or a plugin gets for renaming a
// pane that is not its own. People find their agents by label, so one
// agent may not relabel another.
const RenameRefusal = "a pane may rename only itself. The operator renames any pane."

// takesForemanLabel reports whether r gives a pane the foreman's label.
// The server finds the foreman by its id, but people read the label, so a
// pane may not dress up as the foreman.
func takesForemanLabel(r *proto.Request) bool {
	if !labelVerbs[r.Cmd] {
		return false
	}
	label, _ := r.Str("label")
	return isForemanLabel(label)
}

// resumesForemanLabel reports whether r is a pane.resume of a record
// labelled foreman. A resume keeps the record's label.
func (s *Server) resumesForemanLabel(r *proto.Request) bool {
	if r.Cmd != "pane.resume" {
		return false
	}
	id, _ := r.Str("pane")
	rec, ok := s.tree.Pane(id)
	return ok && isForemanLabel(rec.Label)
}

// foremanDeny reports whether a connection with role ro may run the deny
// in r: it is a pane the kernel placed or named, it is no plugin, and it
// is the foreman that holds that exact ask of that exact pane now.
func (s *Server) foremanDeny(ro role, r *proto.Request) bool {
	if r.Cmd != "agent.deny" || !ro.pane || ro.unknown || ro.plugin != "" || ro.paneID == "" {
		return false
	}
	pane, _ := r.Str("pane")
	ask, _ := r.Str("ask")
	ask = s.resolveHold(pane, strings.TrimSpace(ask))
	return pane != "" && ask != "" && s.holder(pane, ask) == ro.paneID
}

// quietVerbs are verbs a pane runs that make no note: the hello, the
// reports a gate hook sends on every tool call, the event verbs, and the
// note verbs themselves.
var quietVerbs = map[string]bool{
	"hello": true, "pane.report_state": true, "pane.report_child": true,
	"events.subscribe": true, "events.pause": true, "events.resume": true,
	"floor.note": true, "floor.notes": true, "floor.facts": true,
}

// typingVerbs put keys or text into a pane. A pane may not use them on a
// pane that waits on its harness's own question, since a key there is an
// answer.
var typingVerbs = map[string]bool{
	"pane.send_keys": true, "pane.send_text": true, "pane.run": true, "agent.prompt": true,
}

// guard refuses a verb the connection's role may not run. ok is false with
// the refusal when it refuses.
func (s *Server) guard(c *Client, r *proto.Request) (proto.Response, bool) {
	ro := c.roleOf()
	if msg, ok := operatorVerbs[r.Cmd]; ok && ro.noAllow {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, msg), false
	}
	if ro.noAllow && (takesForemanLabel(r) || s.resumesForemanLabel(r)) {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, ForemanLabelRefusal), false
	}
	if r.Cmd == "pane.rename" && ro.noAllow {
		if pane, _ := r.Str("pane"); ro.paneID == "" || pane != ro.paneID {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, RenameRefusal), false
		}
	}
	// A move can put a task under a foreman the operator did not choose,
	// so a pane, or a peer the server cannot place, may not move one.
	if r.Cmd == "task.move" && (ro.pane || ro.unknown) {
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, PaneRefusal), false
	}
	if allowVerbs[r.Cmd] && ro.noAllow && !s.foremanDeny(ro, r) {
		if ro.plugin != "" {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, PluginRefusal), false
		}
		return proto.ErrResp(r.ID, proto.ErrUnauthorized, PaneRefusal), false
	}
	if ro.plugin != "" {
		if resp, ok := s.pluginGuard(ro.plugin, r); !ok {
			return resp, false
		}
	}
	// A pane reports state and subagents only for itself, so it cannot
	// clear another pane's blocked state and then type into it. A pane
	// process the server cannot name reports about no pane at all.
	if (r.Cmd == "pane.report_state" || r.Cmd == "pane.report_child") && ro.pane {
		if id, _ := r.Str("pane"); ro.paneID == "" || id != ro.paneID {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, PaneRefusal), false
		}
	}
	// A prompt to a gated pane gets the handler's own refusal, which names
	// the pane and says to answer its question first. See ready.go.
	if typingVerbs[r.Cmd] && ro.noAllow && !(isPromptRequest(r) && s.gated(paneOf(r))) {
		id, _ := r.Str("pane")
		if eff, ok := s.effectiveState(id); ok && eff.State == proto.StateBlocked {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, PaneRefusal), false
		}
	}
	if r.Cmd == "pane.report_state" && ro.noAllow {
		var ev struct {
			Source string `json:"source"`
		}
		raw, _ := r.Raw("event")
		if json.Unmarshal(raw, &ev) != nil || ev.Source == proto.SrcOperator {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, PaneRefusal), false
		}
	}
	return proto.Response{}, true
}

// noteCut is how many runes of a text value a verb note keeps.
const noteCut = 60

// verbNote is the note text for one verb a pane ran: the pane's label, the
// mark, the verb, then the values of label, pane, harness and text, two
// spaces apart. The text is cut at noteCut runes.
func verbNote(label, verb string, params map[string]string) string {
	parts := []string{verb}
	for _, k := range []string{"label", "pane", "harness", "text"} {
		v := strings.Join(strings.Fields(params[k]), " ")
		if v == "" {
			continue
		}
		if k == "text" && utf8.RuneCountInString(v) > noteCut {
			v = string([]rune(v)[:noteCut])
		}
		parts = append(parts, v)
	}
	return label + " › " + strings.Join(parts, "  ")
}

// labelOf is the label of pane id, or the id when it has none, or "pane"
// when the id is empty.
func (s *Server) labelOf(id string) string {
	if id == "" {
		return "pane"
	}
	if p, ok := s.tree.Pane(id); ok && p.Label != "" {
		return p.Label
	}
	return id
}

// noteVerb records a note for a verb a pane connection runs. It does
// nothing for the operator and for the quiet verbs.
func (s *Server) noteVerb(c *Client, r *proto.Request) {
	if quietVerbs[r.Cmd] {
		return
	}
	ro := c.roleOf()
	if ro.plugin != "" {
		if readVerbs[r.Cmd] {
			return
		}
		params := map[string]string{}
		for _, k := range []string{"pane", "text"} {
			if v, ok := r.Str(k); ok {
				params[k] = v
			}
		}
		id, _ := r.Str("pane")
		s.Note(verbNote(ro.plugin, r.Cmd, params), id)
		return
	}
	if !ro.pane {
		return
	}
	params := map[string]string{}
	for _, k := range []string{"label", "pane", "harness", "text"} {
		if v, ok := r.Str(k); ok {
			params[k] = v
		}
	}
	s.Note(verbNote(s.labelOf(ro.paneID), r.Cmd, params), ro.paneID)
}

// noteMax is how many runes a note or a subagent label keeps. Control
// runes are dropped first, so neither can write escape codes to a floor.
const noteMax = 200

// notesKept is how many notes floor.notes keeps.
const notesKept = 200

// note is one line the floor prints in dim.
// Pane is the pane the note comes from. To, when set, is the pane the note
// is for, apart from its author.
type note struct {
	Event string  `json:"event"`
	Text  string  `json:"text"`
	Pane  string  `json:"pane,omitempty"`
	To    string  `json:"to,omitempty"`
	TS    float64 `json:"ts"`
}

// Note records one note, keeps the last notesKept, and sends it to every
// client that subscribed to notes.
func (s *Server) Note(text, paneID string) {
	s.NoteTo(text, paneID, "")
}

// NoteTo is Note with an addressee: a note from pane paneID for pane to.
func (s *Server) NoteTo(text, paneID, to string) {
	n := note{Event: "note", Text: textwidth.Printable(text, noteMax), Pane: paneID, To: to, TS: nowSeconds()}
	s.notesMu.Lock()
	s.notes = append(s.notes, n)
	if len(s.notes) > notesKept {
		s.notes = append([]note(nil), s.notes[len(s.notes)-notesKept:]...)
	}
	s.notesMu.Unlock()
	s.Broadcast("note", paneID, n)
}

// RegisterFloorCommands wires up hello, the note verbs, and the verbs that
// answer an ask.
func (s *Server) RegisterFloorCommands() {
	_ = s.Handle("hello", s.handleHello)
	_ = s.Handle("floor.note", s.handleNote)
	_ = s.Handle("floor.notes", s.handleNotes)
	_ = s.Handle("floor.facts", s.handleFloorFacts)
	_ = s.Handle("agent.allow", s.handleAnswer("allow"))
	_ = s.Handle("agent.deny", s.handleAnswer("deny"))
	_ = s.Handle("pane.report_child", s.handleReportChild)
}

// handleNote records a note. From a pane connection the note names that
// pane and leads with its label.
func (s *Server) handleNote(c *Client, r *proto.Request) proto.Response {
	text, _ := r.Str("text")
	text = strings.TrimSpace(text)
	if text == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "floor.note needs text.")
	}
	paneID, _ := r.Str("pane")
	if ro := c.roleOf(); ro.plugin != "" {
		paneID = ""
		text = ro.plugin + " › " + text
	} else if ro.pane {
		paneID = ro.paneID
		text = s.labelOf(paneID) + " › " + text
	}
	s.Note(text, paneID)
	return proto.OKResp(r.ID, map[string]any{"text": textwidth.Printable(text, noteMax)})
}

// handleNotes returns the kept notes, oldest first.
func (s *Server) handleNotes(c *Client, r *proto.Request) proto.Response {
	s.notesMu.Lock()
	out := append([]note{}, s.notes...)
	s.notesMu.Unlock()
	return proto.OKResp(r.ID, map[string]any{"notes": out})
}

// handleAnswer answers the gate's ask with decision, through the same file
// the phone writes. guard lets through only the operator, and a foreman's
// deny of the ask it holds. The foreman check runs again here, so a hold
// that ended between the two leaves the ask alone.
func (s *Server) handleAnswer(decision string) Handler {
	return func(c *Client, r *proto.Request) proto.Response {
		verb := "agent." + decision
		ro := c.roleOf()
		foreman := ro.noAllow
		if foreman && !s.foremanDeny(ro, r) {
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, PaneRefusal)
		}
		id, ok := r.Str("pane")
		if !ok || id == "" {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, verb+" needs pane. Run: coppice pane list")
		}
		if _, ok := s.tree.Pane(id); !ok {
			return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
				fmt.Sprintf("no pane %q. Run: coppice pane list", id))
		}
		ask, _ := r.Str("ask")
		ask = s.resolveHold(id, strings.TrimSpace(ask))
		if ask == "" {
			return proto.ErrResp(r.ID, proto.ErrBadRequest, verb+" needs ask, the id of the ask to answer.")
		}
		// The ask must be the one this pane holds now, so an answer can
		// never land on a pane the operator did not name.
		tier, held := s.heldAsk(id, ask)
		if !held {
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("pane %s does not hold ask %q. Run: coppice agent get %s", id, ask, id))
		}
		reason, _ := r.Str("reason")
		switch {
		case foreman:
			reason = "denied by the foreman " + s.labelOf(ro.paneID)
		case reason == "":
			reason = "answered from coppice by the operator"
		}
		confirm, _ := r.Str("confirm")
		scope, _ := r.Str("scope")
		by, from := c.Who()
		var err error
		if an := s.harnessAsker(id, ask); an != nil {
			// The harness holds this ask itself, not the gate. The same
			// allow rule applies: a permanent ask needs the pane name.
			if decision == "allow" {
				err = web.CheckAllow(tier, s.labelOf(id), confirm, scope)
			}
			if err == nil {
				if aerr := an.Answer(ask, decision == "allow", reason); aerr != nil {
					return proto.ErrResp(r.ID, proto.ErrAdapter, aerr.Error())
				}
			}
		} else {
			err = web.AskChannel{Root: s.cfg.GateRoot}.Answer(web.Reply{
				ToolUseID: ask, Decision: decision, Reason: reason,
				Scope: scope, Confirm: confirm, Tier: tier, Name: s.labelOf(id),
				By: by, WhoFrom: from,
			})
		}
		if err == nil {
			s.forgetAsk(id, ask)
			s.endHold(id, ask)
		}
		var needsName *web.NeedsNameError
		switch {
		case errors.Is(err, web.ErrNoAsk):
			return proto.ErrResp(r.ID, proto.ErrBadRequest,
				fmt.Sprintf("no pending ask %q. The gate timed out, or someone answered it.", ask))
		case errors.Is(err, web.ErrBadScope):
			return proto.ErrResp(r.ID, proto.ErrBadRequest, "scope must be once or task.")
		case errors.As(err, &needsName):
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, needsName.Error())
		case errors.Is(err, web.ErrPermanentTask):
			return proto.ErrResp(r.ID, proto.ErrUnauthorized, err.Error()+" with the pane name.")
		case err != nil:
			return proto.ErrResp(r.ID, proto.ErrInternal, "cannot write the answer: "+err.Error())
		}
		return proto.OKResp(r.ID, map[string]any{"pane": id, "ask": ask, "decision": decision})
	}
}

// harnessAsker returns the live headless harness of pane id when that
// harness holds ask itself, else nil.
func (s *Server) harnessAsker(id, ask string) pane.Answerer {
	lp, ok := s.Live(id)
	if !ok || lp.Adapter == nil {
		return nil
	}
	an, ok := lp.Adapter.(pane.Answerer)
	if !ok || !an.OwnsAsk(ask) {
		return nil
	}
	return an
}

// notedAsk is one ask a pane reported: when it expires, and its tier.
type notedAsk struct {
	deadline float64
	tier     string
}

// noteAsk records an ask a pane reported, with its deadline, so the
// operator can answer it while it is live even when a later ask of the
// same pane is the one its state shows.
func (s *Server) noteAsk(paneID string, ask *proto.Ask) {
	if ask == nil || ask.ID == "" {
		return
	}
	s.asksMu.Lock()
	defer s.asksMu.Unlock()
	m := s.asks[paneID]
	if m == nil {
		m = map[string]notedAsk{}
		s.asks[paneID] = m
	}
	m[ask.ID] = notedAsk{deadline: ask.Deadline, tier: proto.NormalTier(ask.Tier)}
}

// heldAsk reports whether pane paneID holds the live ask id: one it
// reported whose deadline has not passed, and the tier it reported with
// it. Expired asks are dropped here.
func (s *Server) heldAsk(paneID, id string) (string, bool) {
	now := nowSeconds()
	s.asksMu.Lock()
	defer s.asksMu.Unlock()
	m := s.asks[paneID]
	for k, a := range m {
		if now >= a.deadline {
			delete(m, k)
		}
	}
	a, ok := m[id]
	return a.tier, ok
}

// forgetAsk drops one ask of a pane, once it is answered.
func (s *Server) forgetAsk(paneID, id string) {
	s.asksMu.Lock()
	defer s.asksMu.Unlock()
	delete(s.asks[paneID], id)
}

// forgetAsks drops every ask of a pane that closed, ends its hold, and
// sends every ask it held as a foreman to the operator.
func (s *Server) forgetAsks(paneID string) {
	s.endHolds(paneID)
	s.asksMu.Lock()
	defer s.asksMu.Unlock()
	delete(s.asks, paneID)
}
