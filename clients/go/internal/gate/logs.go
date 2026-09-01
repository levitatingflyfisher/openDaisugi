package gate

import (
	"errors"
	"net"
	"os"

	"time"
	"unicode/utf8"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// plan is every write one call makes, computed before any of them runs.
type plan struct {
	result Result

	shadowDir  string
	shadowFile string
	shadowRec  *pyjson.Object

	treeDir    string
	treeFile   string
	treeHeader *pyjson.Object // nil when the tree file exists
	treeHead   any
	toolCall   *pyjson.Object
	verdict    *pyjson.Object

	captureDir  string
	captureFile string
	captureRec  *pyjson.Object

	event     *pyjson.Object // the PaneStateEvent, ts set on send
	stateTree bool

	checkpoint *checkpointDue
}

func pyTime(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func dumpsLine(o *pyjson.Object, ascii bool) string { return pyjson.Dumps(o, ascii) + "\n" }

// shadow is gate._log_shadow's record.
func (pl *plan) shadow(r *runner, sid string, d *decision, payloadSession any, join *pyjson.Object) {
	pl.shadowDir = pathJoin(r.root, "shadow")
	pl.shadowFile = pathJoin(pl.shadowDir, sid+".jsonl")
	vs := make([]any, len(d.Violations))
	for i, v := range d.Violations {
		vs[i] = v.obj()
	}
	rec := pyjson.NewObject().
		Set("at", nil).
		Set("session_id", sid).
		Set("payload_session_id", payloadSession).
		Set("tool_name", d.ToolName).
		Set("step_type", d.StepType).
		Set("detail", d.Detail).
		Set("mode", r.mode).
		Set("allow", d.Allow).
		Set("would_deny", d.WouldDeny).
		Set("reason", d.Reason).
		Set("elapsed_ms", pyjson.Float(pyjson.Round(d.ElapsedMS, 3))).
		Set("clause", d.clause()).
		Set("violations", vs).
		Set("envelope_id", d.EnvelopeID).
		Set("plan_id", d.PlanID).
		Set("ask", d.Ask).
		Set("tier", d.Tier)
	if d.Ask {
		// An answered ask always names who gave it. No name is local.
		key := "denied_by"
		if d.Allow {
			key = "allowed_by"
		}
		by, from := d.AnsweredBy, d.WhoFrom
		if !pyjson.Truthy(by) {
			by = "local"
		}
		if !pyjson.Truthy(from) {
			from = "none"
		}
		rec.Set(key, by).Set("who_from", from)
	}
	if join != nil {
		for _, k := range join.Keys() {
			rec.Set(k, join.Value(k))
		}
	}
	pl.shadowRec = rec
}

func harnessSessionID(p *pyjson.Object) any {
	if s, ok := p.Value("session_id").(string); ok {
		return s
	}
	return nil
}

// cwdText is str(payload.get("cwd") or "").
func cwdText(p *pyjson.Object) string { return pyStrOr(p.Value("cwd")) }

// tree is gate._log_tree: a header when the session is new, then the
// tool_call entry and its verdict.
func (pl *plan) tree(r *runner, sid string, p *pyjson.Object, d *decision) {
	pl.treeDir = pathJoin(pathParent(r.root), "sessions")
	pl.treeFile = pathJoin(pl.treeDir, safeSessionID(sid)+".jsonl")
	if exists(pl.treeFile) {
		pl.treeHead = readHead(pl.treeFile)
	} else {
		pl.treeHeader = r.treeHeader(sid, p)
	}
	name := d.ToolName
	if !pyjson.Truthy(name) {
		name = p.Value("tool_name")
	}
	callID := randomHex(4)
	pl.toolCall = pyjson.NewObject().
		Set("type", "tool_call").
		Set("id", callID).
		Set("parentId", pl.treeHead).
		Set("ts", nil).
		Set("toolUseId", p.Value("tool_use_id")).
		Set("name", name).
		Set("stepType", d.StepType).
		Set("detail", d.Detail).
		Set("agentId", p.Value("agent_id")).
		Set("agentType", p.Value("agent_type"))
	decisionWord := "deny"
	if d.Allow {
		decisionWord = "allow"
	}
	pl.verdict = pyjson.NewObject().
		Set("type", "verdict").
		Set("id", randomHex(4)).
		Set("parentId", callID).
		Set("ts", nil).
		Set("toolUseId", p.Value("tool_use_id")).
		Set("decision", decisionWord).
		Set("wouldDeny", d.WouldDeny).
		Set("mode", r.mode).
		Set("reason", d.Reason).
		Set("clause", d.clause()).
		Set("counterexample", d.counterexample()).
		Set("envelopeId", d.EnvelopeID).
		Set("planId", d.PlanID).
		Set("latencyMs", pyjson.Float(pyjson.Round(d.ElapsedMS, 3))).
		Set("answeredBy", answeredBy(d)).
		Set("tier", d.Tier)
}

func answeredBy(d *decision) any {
	if d.Ask {
		return "operator"
	}
	return nil
}

// treeHeader is the session entry SessionTree.create writes for a call.
func (r *runner) treeHeader(sid string, p *pyjson.Object) *pyjson.Object {
	return pyjson.NewObject().
		Set("type", "session").
		Set("id", safeSessionID(sid)).
		Set("ts", nil).
		Set("v", 1).
		Set("harness", harnessOf(r.fmt)).
		Set("cwd", cwdText(p)).
		Set("harnessSessionId", harnessSessionID(p)).
		Set("transcriptPath", p.Value("transcript_path")).
		Set("parentSession", nil).
		Set("parentEntry", nil).
		Set("cacheKey", nil)
}

// entryTypes is session_tree.ENTRY_TYPES; noMove is _NO_MOVE.
var entryTypes = set("session", "prompt", "assistant", "tool_call", "verdict", "tool_result", "checkpoint",
	"compaction", "branch_summary", "label", "note", "head", "state")
var noMove = set("label", "head", "session", "state")

// readHead is SessionTree._compute_head: scan from the end for the last
// head entry's leafId or the last moving entry's id.
func readHead(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil {
		unported("the session tree cannot be read")
	}
	text := string(raw)
	if !utf8.ValidString(text) {
		unported("the session tree is not valid UTF-8")
	}
	lines := splitlines(text)
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if pyStrip(line) == "" {
			continue
		}
		v, err := pyjson.Loads(line)
		if err == pyjson.ErrUnsupported {
			unported("the session tree holds JSON the port does not model")
		}
		if err != nil {
			continue
		}
		row, ok := v.(*pyjson.Object)
		if !ok {
			continue
		}
		t := row.Value("type")
		ts, isStr := t.(string)
		if !isStr {
			switch t.(type) {
			case nil, bool, pyjson.Int, pyjson.Float:
				continue
			}
			unported("a session tree entry type that Python cannot hash")
		}
		if !entryTypes[ts] {
			continue
		}
		if ts == "head" {
			return row.Value("leafId")
		}
		if !noMove[ts] && pyjson.Truthy(row.Value("id")) {
			return row.Value("id")
		}
	}
	return nil
}

// splitlines is Python's str.splitlines().
func splitlines(s string) []string {
	var out []string
	start := 0
	i := 0
	for i < len(s) {
		r, size := rune(s[i]), 1
		if s[i] >= 0x80 {
			r, size = utf8.DecodeRuneInString(s[i:])
		}
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			i += size
			start = i
			continue
		case '\r':
			out = append(out, s[start:i])
			i++
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// capture is hook.record_call's line.
func (pl *plan) capture(r *runner, rec *record) {
	pl.captureDir = *r.captures
	pl.captureFile = pathJoin(pl.captureDir, rec.obj.Value("session_id").(string)+".jsonl")
	pl.captureRec = rec.obj
}

func (r *runner) paneFromEnv() any {
	for _, k := range []string{"COPPICE_PANE", "HERDR_PANE_ID", "HERDR_PANE", "TMUX_PANE"} {
		if v := r.env[k]; v != "" {
			return v
		}
	}
	return nil
}

// state is gate._maybe_report_state: the 'working' event for coppice and
// its copy in the session tree.
func (pl *plan) state(r *runner, sid string, p *pyjson.Object, d *decision) {
	detail := "verdict=allow"
	if !d.Allow {
		detail = "verdict=deny clause=" + d.clause()
	}
	decisionWord := "deny"
	if d.Allow {
		decisionWord = "allow"
	}
	clause := ""
	if d.WouldDeny {
		clause = d.clause()
	}
	pl.event = r.buildEvent(sid, p, "working", pyHead(detail, 200), nil, decisionWord, toolWord(d), clause)
	pl.stateTree = true
}

// toolWord is str(decision.tool_name or "unknown").
func toolWord(d *decision) string {
	if !pyjson.Truthy(d.ToolName) {
		return "unknown"
	}
	return pyStrOf(d.ToolName)
}

// buildEvent is _state_report.build_event for a call the gate reports:
// the transcript path (an absolute string only), the verdict and the mode
// word the floor shows. ts is set when the event is sent.
func (r *runner) buildEvent(sid string, p *pyjson.Object, state, detail string, ask *pyjson.Object,
	decisionWord, tool, clause string) *pyjson.Object {
	ev := pyjson.NewObject().
		Set("v", 1).
		Set("ts", nil).
		Set("session_id", sid).
		Set("harness_session_id", harnessSessionID(p)).
		Set("harness", harnessOf(r.fmt)).
		Set("pane", r.paneFromEnv()).
		Set("state", state).
		Set("source", "gate").
		Set("detail", pyHead(detail, 200))
	if ask != nil {
		ev.Set("ask", ask)
	}
	if tp, ok := p.Value("transcript_path").(string); ok && tp != "" && isabs(tp) {
		ev.Set("transcript_path", tp)
	}
	ev.Set("verdict", pyjson.NewObject().
		Set("decision", decisionWord).
		Set("tool", pyHead(tool, 200)).
		Set("clause", pyHead(clause, 200)))
	floorMode := "watching"
	if r.mode == "enforce" {
		floorMode = "enforcing"
	}
	ev.Set("mode", floorMode)
	return ev
}

// reportBlocked is gate._report_blocked: the 'blocked' event goes out the
// moment an ask is posted, and into the session tree, each best-effort.
func (r *runner) reportBlocked(p *pyjson.Object, d *decision, sessionID any, tid string, deadline float64) {
	sid := r.safeSession(sessionID)
	ask := pyjson.NewObject().
		Set("id", tid).
		Set("tool", toolWord(d)).
		Set("summary", d.Detail).
		Set("deadline", pyjson.Float(deadline)).
		Set("tier", d.Tier)
	ev := r.buildEvent(sid, p, "blocked", pyHead("awaiting operator: "+d.Reason, 200), ask, "ask", toolWord(d), d.clause())
	ev.Set("ts", pyjson.Float(pyTime(time.Now())))
	r.reportState(ev)
	_ = catch(func() {
		file := pathJoin(pathJoin(pathParent(r.root), "sessions"), safeSessionID(sid)+".jsonl")
		var parent any
		if exists(file) {
			parent = readHeadQuiet(file)
		} else {
			dir := pathParent(file)
			if mkdirPrivate(dir) != nil {
				return
			}
			header := r.treeHeader(sid, p).Set("ts", pyjson.Float(pyTime(time.Now())))
			if appendText(file, dumpsLine(header, false)) != nil {
				return
			}
			_ = os.Chmod(file, 0o600)
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
	})
}

// mkdirPrivate is gate._mkdir_private.
func mkdirPrivate(d string) error {
	if parent := pathParent(d); parent != d {
		_ = os.MkdirAll(parent, 0o777)
	}
	if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	_ = os.Chmod(d, 0o700)
	return nil
}

func appendLine(path, line string, private bool) error {
	_, statErr := os.Stat(path)
	fresh := os.IsNotExist(statErr)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line)
	cerr := f.Close()
	if fresh && private {
		_ = os.Chmod(path, 0o600)
	}
	if werr != nil {
		return werr
	}
	return cerr
}

// apply makes the writes in the Python gate's order. Each is best-effort,
// as in Python: a failed write never changes the verdict.
func (pl *plan) apply(r *runner) {
	if pl.shadowRec != nil {
		if mkdirPrivate(pl.shadowDir) == nil {
			pl.shadowRec.Set("at", pyjson.Float(pyTime(time.Now())))
			_ = appendLine(pl.shadowFile, dumpsLine(pl.shadowRec, true), true)
		}
	}
	treeOK := false
	if pl.toolCall != nil {
		treeOK = pl.writeTree()
	}
	if pl.checkpoint != nil {
		r.takeCheckpoint(pl.checkpoint)
	}
	if pl.captureRec != nil {
		if err := mkdirPrivate(pl.captureDir); err == nil {
			pl.captureRec.Set("captured_at", pyjson.Float(pyTime(time.Now())))
			_ = appendLine(pl.captureFile, dumpsLine(pl.captureRec, true), true)
		}
	}
	if pl.event != nil {
		pl.event.Set("ts", pyjson.Float(pyTime(time.Now())))
		r.reportState(pl.event)
		parent, ok := any(nil), false
		if pl.stateTree {
			if treeOK {
				parent, ok = pl.verdict.Value("id"), true
			} else {
				// _report_and_append_state opens the tree again when
				// _log_tree failed, and appends under its head.
				parent, ok = pl.reopenTree()
			}
		}
		if ok {
			entry := pyjson.NewObject().
				Set("type", "state").
				Set("id", randomHex(4)).
				Set("parentId", parent).
				Set("ts", pyjson.Float(pyTime(time.Now())))
			for _, k := range pl.event.Keys() {
				switch k {
				case "type", "id", "parentId", "ts":
					continue
				}
				entry.Set(k, pl.event.Value(k))
			}
			_ = appendText(pl.treeFile, dumpsLine(entry, false))
		}
	}
}

func (pl *plan) writeTree() bool {
	if pl.treeHeader != nil {
		if mkdirPrivate(pl.treeDir) != nil {
			return false
		}
		pl.treeHeader.Set("ts", pyjson.Float(pyTime(time.Now())))
		f, err := os.OpenFile(pl.treeFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			if !os.IsExist(err) {
				return false
			}
			// Another writer made it first: open it, as Python's
			// open_or_create does, and parent on its head.
			pl.toolCall.Set("parentId", readHeadQuiet(pl.treeFile))
		} else {
			line := dumpsLine(pl.treeHeader, false)
			if pystr.HasSurrogate(line) {
				// The utf-8 encoder raises before anything is written.
				f.Close()
				_ = os.Chmod(pl.treeFile, 0o600)
				return false
			}
			_, werr := f.WriteString(line)
			f.Close()
			_ = os.Chmod(pl.treeFile, 0o600)
			if werr != nil {
				return false
			}
		}
	}
	pl.toolCall.Set("ts", pyjson.Float(pyTime(time.Now())))
	if appendText(pl.treeFile, dumpsLine(pl.toolCall, false)) != nil {
		return false
	}
	pl.verdict.Set("ts", pyjson.Float(pyTime(time.Now())))
	return appendText(pl.treeFile, dumpsLine(pl.verdict, false)) == nil
}

// appendText is a write through a file opened with encoding="utf-8": a
// lone surrogate makes the encoder raise before anything is written.
func appendText(path, line string) error {
	if pystr.HasSurrogate(line) {
		return errSurrogate
	}
	return appendLine(path, line, false)
}

var errSurrogate = errors.New("'utf-8' codec can't encode a surrogate")

// reopenTree is SessionTree.open_or_create on the call's tree: the head
// of the file as it stands, after a header when the file is new.
func (pl *plan) reopenTree() (any, bool) {
	if !lexists(pl.treeFile) {
		if pl.treeHeader == nil || mkdirPrivate(pl.treeDir) != nil {
			return nil, false
		}
		f, err := os.OpenFile(pl.treeFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			return nil, false
		}
		pl.treeHeader.Set("ts", pyjson.Float(pyTime(time.Now())))
		line := dumpsLine(pl.treeHeader, false)
		if pystr.HasSurrogate(line) {
			f.Close()
			_ = os.Chmod(pl.treeFile, 0o600)
			return nil, false
		}
		_, werr := f.WriteString(line)
		f.Close()
		_ = os.Chmod(pl.treeFile, 0o600)
		if werr != nil {
			return nil, false
		}
		return nil, true
	}
	return readHeadQuiet(pl.treeFile), true
}

func readHeadQuiet(path string) (head any) {
	defer func() {
		if recover() != nil {
			head = nil
		}
	}()
	return readHead(path)
}

// reportState is _state_report.report_state's coppice path, inside the
// same 0.2 s total budget.
func (r *runner) reportState(ev *pyjson.Object) {
	deadline := time.Now().Add(200 * time.Millisecond)
	sock, pane := r.env["COPPICE_SOCK"], r.env["COPPICE_PANE"]
	if sock == "" || pane == "" {
		for _, k := range []string{"HERDR_PANE_ID", "HERDR_PANE"} {
			if v := r.env[k]; v != "" {
				r.sendHerdrReport(v, ev, deadline)
				return
			}
		}
		return
	}
	conn, err := net.DialTimeout("unix", sock, time.Until(deadline))
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	req := pyjson.NewObject().Set("id", "r").Set("cmd", "pane.report_state").Set("pane", pane).Set("event", ev)
	if _, err := conn.Write([]byte(pyjson.Dumps(req, true) + "\n")); err != nil {
		return
	}
	buf := make([]byte, 65536)
	_, _ = conn.Read(buf)
}
