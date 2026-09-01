package gate

import (
	"net"
	"os"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is the payload half of `daisugi hook record`:
// hook.record_and_contract and hook.record_lifecycle_event. Both always
// return the host's allow contract. Every write is best-effort, as in
// Python: an error, or an input this port does not model, skips the
// write and never changes the contract.

// blockingNotifications is hook._BLOCKING_NOTIFICATIONS.
var blockingNotifications = set("permission_prompt", "elicitation_dialog", "elicitation_url_dialog",
	"agent_needs_input")

// subagentEvents is hook._SUBAGENT_EVENTS: the child state each event
// reports.
var subagentEvents = map[string]string{"subagent_start": "working", "subagent_stop": "done"}

// HookRecord runs `daisugi hook record` on raw, the hook's stdin, and
// returns the contract the host reads on stdout. event is one of the five
// events the command takes; capturesRoot is the --captures-root text.
func HookRecord(raw []byte, capturesRoot, format, event string, env map[string]string) string {
	r := &runner{env: env, fmt: format}
	switch event {
	case "stop", "notification", "subagent_start", "subagent_stop":
		swallow(func() { r.lifecycle(raw, event, pathJoin(pathParent(capturesRoot), "sessions")) })
	default:
		swallow(func() { r.recordCall(raw, capturesRoot) })
	}
	return stdoutFor(format, false, "")
}

// swallow runs fn and swallows whatever it raises or panics with: the
// oracle's `except Exception: pass` around a hook's writes.
func swallow(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// hookPayload is `json.loads(raw.decode("utf-8", "replace"))` when the
// text is not blank, else {}. ok is false when json.loads raises.
func hookPayload(raw []byte) (v any, ok bool) {
	text := pystr.DecodeReplace(raw)
	if pyStrip(text) == "" {
		return pyjson.NewObject(), true
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, false
	}
	return v, true
}

// recordCall is record_and_contract's write: hook.record_call on a
// truthy payload, which classifies the call as the claude format does.
func (r *runner) recordCall(raw []byte, root string) {
	v, ok := hookPayload(raw)
	if !ok || !pyjson.Truthy(v) {
		return
	}
	p, isObj := v.(*pyjson.Object)
	if !isObj {
		// payload.get raises AttributeError.
		return
	}
	rec, ok := r.payloadToRecord(p, "claude")
	if !ok {
		return
	}
	if mkdirPrivateDir(root) != nil {
		return
	}
	rec.obj.Set("captured_at", pyjson.Float(pyTime(time.Now())))
	file := pathJoin(root, rec.obj.Value("session_id").(string)+".jsonl")
	_ = appendLine(file, dumpsLine(rec.obj, true), true)
}

// lifecycle is record_lifecycle_event's writes.
func (r *runner) lifecycle(raw []byte, event, sessionsRoot string) {
	v, ok := hookPayload(raw)
	if !ok {
		v = pyjson.NewObject()
	}
	if state, sub := subagentEvents[event]; sub {
		r.reportSubagent(v, state)
		return
	}
	p, isObj := v.(*pyjson.Object)
	if !isObj || !pyjson.Truthy(p.Value("session_id")) {
		return
	}
	sid := r.safeSession(p.Value("session_id"))
	var state, detail string
	var ask *pyjson.Object
	if event == "stop" {
		state, detail = "idle", "session stop"
	} else {
		message := pyStrOr(p.Value("message"))
		notifType := p.Value("notification_type")
		var isPermission bool
		if pyjson.Truthy(notifType) {
			nt, isStr := notifType.(string)
			switch notifType.(type) {
			case []any, *pyjson.Object:
				// `in` a frozenset raises TypeError on an unhashable value:
				// nothing is reported.
				return
			}
			isPermission = isStr && blockingNotifications[nt]
		} else {
			isPermission = strings.Contains(pystr.Lower(message), "permission")
		}
		if isPermission {
			state = "blocked"
			tool := "notification"
			if pyjson.Truthy(notifType) {
				tool = pyStrOf(notifType)
			}
			ask = pyjson.NewObject().
				Set("id", "harness").
				Set("tool", tool).
				Set("summary", message).
				Set("deadline", pyjson.Float(pyTime(time.Now())+90))
			detail = pyHead("notification: "+message, 200)
		} else {
			state = "idle"
			detail = "notification"
			if message != "" {
				detail = pyHead("notification: "+message, 200)
			}
		}
	}
	ev := pyjson.NewObject().
		Set("v", 1).
		Set("ts", pyjson.Float(pyTime(time.Now()))).
		Set("session_id", sid).
		Set("harness_session_id", p.Value("session_id")).
		Set("harness", harnessOf(r.fmt)).
		Set("pane", nil).
		Set("state", state).
		Set("source", "headless").
		Set("detail", pyHead(detail, 200))
	if ask != nil {
		ev.Set("ask", ask)
	}
	if tp, ok := p.Value("transcript_path").(string); ok && tp != "" && isabs(tp) {
		ev.Set("transcript_path", tp)
	}
	r.reportState(ev)
	swallow(func() { r.lifecycleTree(sessionsRoot, sid, p, ev) })
}

// lifecycleTree is SessionTree.open_or_create(...).append("state", ev):
// the session file made with its header when it is new, then the state
// entry under the tree's head.
func (r *runner) lifecycleTree(root, sid string, p, ev *pyjson.Object) {
	file := pathJoin(root, safeSessionID(sid)+".jsonl")
	var parent any
	if exists(file) {
		parent = readHead(file)
	} else {
		if mkdirPrivateDir(root) != nil {
			return
		}
		header := pyjson.NewObject().
			Set("type", "session").
			Set("id", safeSessionID(sid)).
			Set("ts", pyjson.Float(pyTime(time.Now()))).
			Set("v", 1).
			Set("harness", harnessOf(r.fmt)).
			Set("cwd", pyStrOr(p.Value("cwd"))).
			Set("harnessSessionId", p.Value("session_id")).
			Set("transcriptPath", p.Value("transcript_path")).
			Set("parentSession", nil).
			Set("parentEntry", nil).
			Set("cacheKey", nil)
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			if !os.IsExist(err) {
				return
			}
			// Another writer made it first: open_or_create opens it.
			parent = readHead(file)
		} else {
			line := dumpsLine(header, false)
			if pystr.HasSurrogate(line) {
				// The utf-8 encoder raises before anything is written, and
				// before create's chmod.
				f.Close()
				return
			}
			_, werr := f.WriteString(line)
			f.Close()
			if werr != nil {
				return
			}
			_ = os.Chmod(file, 0o600)
		}
	}
	entry := pyjson.NewObject().
		Set("type", "state").
		Set("id", randomHex(4)).
		Set("parentId", parent).
		Set("ts", pyjson.Float(pyTime(time.Now())))
	for _, k := range ev.Keys() {
		switch k {
		case "type", "id", "parentId", "ts":
			continue
		}
		entry.Set(k, ev.Value(k))
	}
	_ = appendText(file, dumpsLine(entry, false))
}

// reportSubagent is hook._report_subagent: a child row for coppice when
// the payload names an agent id.
func (r *runner) reportSubagent(v any, state string) {
	p, isObj := v.(*pyjson.Object)
	if !isObj {
		return
	}
	id, isStr := p.Value("agent_id").(string)
	if !isStr || id == "" {
		return
	}
	label, _ := p.Value("agent_type").(string)
	r.reportChild(id, state, label)
}

// reportChild is _state_report.report_child: one pane.report_child line
// to coppice inside a 0.2 s budget. Only coppice shows subagents.
func (r *runner) reportChild(child, state, label string) {
	sock, pane := r.env["COPPICE_SOCK"], r.env["COPPICE_PANE"]
	if sock == "" || pane == "" || child == "" {
		return
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	conn, err := net.DialTimeout("unix", sock, time.Until(deadline))
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	req := pyjson.NewObject().Set("id", "c").Set("cmd", "pane.report_child").Set("pane", pane).
		Set("child", child).Set("state", state).Set("label", pyHead(label, 200))
	if _, err := conn.Write([]byte(pyjson.Dumps(req, true) + "\n")); err != nil {
		return
	}
	buf := make([]byte, 65536)
	_, _ = conn.Read(buf)
}

// mkdirPrivateDir is Path.mkdir(parents=True, exist_ok=True, mode=0o700)
// then os.chmod(path, 0o700): a path that exists and is no directory
// raises FileExistsError before the chmod.
func mkdirPrivateDir(d string) error {
	if st, err := os.Stat(d); err == nil && !st.IsDir() {
		return os.ErrExist
	}
	return mkdirPrivate(d)
}
