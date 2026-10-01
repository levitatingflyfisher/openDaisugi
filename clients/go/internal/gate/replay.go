package gate

import (
	"errors"
	"fmt"
	"time"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// ReplayCrash is an error the oracle's replay_captures raises out of the
// command: the CLI then prints a traceback and exits 1. Exc is the last
// line of that traceback.
type ReplayCrash struct{ Exc string }

func (c *ReplayCrash) Error() string { return c.Exc }

// Replay is gate.replay_captures: each captured call in the file at
// capsPath, decided in audit terms against envObj, as the audit log
// record the report reads. envObj is the validated envelope,
// model_dump(mode="json"). A refusal (why != "") is a call the port
// cannot decide the way the oracle does; nothing is written either way.
func Replay(capsPath string, envObj *pyjson.Object, environ map[string]string) (recs []*pyjson.Object, crash *ReplayCrash, why string) {
	defer func() {
		if p := recover(); p != nil {
			recs, crash = nil, nil
			if d, ok := p.(unportedCall); ok {
				why = d.reason
				return
			}
			if exc, ok := p.(*pystr.Exception); ok {
				crash = &ReplayCrash{Exc: exc.Type + ": " + exc.Msg}
				return
			}
			why = fmt.Sprintf("unexpected: %v", p)
		}
	}()
	r := &runner{env: environ, t0: time.Now()}
	r.checkEnv()
	r.defaultRoot = pathJoin(r.dataHome(), "gate")
	r.root = r.defaultRoot
	r.mode = "audit"
	r.fmt = "claude"
	r.verifyTimeoutS = 10
	// replay_captures runs at frame 4 of the CLI: typer's main, the
	// command and replay_captures sit under evaluate_record.
	r.depth = 4
	env := envelopeFrom(envObj)
	for _, line := range splitlines(readText(capsPath)) {
		if pyStrip(line) == "" {
			continue
		}
		c, err := pyjson.Loads(line)
		if err != nil {
			if errors.Is(err, pyjson.ErrUnsupported) {
				unported("a captured line holds JSON this binary does not read")
			}
			continue
		}
		cap, ok := c.(*pyjson.Object)
		if !ok {
			// _evaluate_record reads record.get before its try block.
			noAttr(c, "get")
		}
		r.t0 = time.Now()
		d := r.replayOne(cap, env)
		rec := pyjson.NewObject()
		rec.Set("at", cap.Value("captured_at"))
		rec.Set("session_id", cap.Value("session_id"))
		rec.Set("tool_name", d.ToolName)
		rec.Set("step_type", d.StepType)
		rec.Set("detail", d.Detail)
		rec.Set("mode", "audit")
		rec.Set("allow", d.Allow)
		rec.Set("would_deny", d.WouldDeny)
		rec.Set("reason", d.Reason)
		rec.Set("elapsed_ms", pyjson.Round(d.ElapsedMS, 3))
		if len(d.WordAudit) > 0 {
			wa := make([]any, len(d.WordAudit))
			for i, w := range d.WordAudit {
				wa[i] = w
			}
			rec.Set("word_audit", wa)
		}
		recs = append(recs, rec)
	}
	return recs, nil, ""
}

// replayOne is _evaluate_record on a capture record read from a file.
func (r *runner) replayOne(cap *pyjson.Object, env *envelope) *decision {
	toolName := cap.Value("tool_name")
	stepAny, hasStep := cap.Get("step_type")
	detail := ""
	for _, k := range []string{"command", "path", "url"} {
		if v := cap.Value(k); pyjson.Truthy(v) {
			detail = pyStrOf(v)
			break
		}
	}
	deny := func(reason string) *decision {
		d := r.deny(reason)
		d.ToolName, d.StepType, d.Detail = toolName, stepAny, detail
		return d
	}
	if !hasStep {
		// r["step_type"] raises KeyError inside the try.
		return deny(r.internalError("'step_type'"))
	}
	step, _ := stepAny.(string)
	switch step {
	case "shell", "file_read", "file_write", "network", "mcp":
	default:
		return deny("could not synthesize a step for tool " + pyValueRepr(toolName))
	}
	rec := &record{StepType: step, obj: pyjson.NewObject()}
	if s, ok := toolName.(string); ok {
		rec.ToolName = s
	}
	rec.CommandRaw = cap.Value("command")
	rec.PathRaw = cap.Value("path")
	rec.URLRaw = cap.Value("url")
	rec.Command, rec.Path, rec.URL = pyStrOr(rec.CommandRaw), pyStrOr(rec.PathRaw), pyStrOr(rec.URLRaw)
	if step == "file_read" || step == "file_write" {
		if s, ok := rec.PathRaw.(string); ok && s != "" {
			rec.SearchPaths = []string{s}
		}
	}
	if step == "mcp" {
		var errs []pmodel.Err
		server, tool, args := cap.Value("mcp_server"), cap.Value("mcp_tool"), cap.Value("arguments")
		for _, f := range []struct {
			name string
			v    any
		}{{"server", server}, {"tool", tool}} {
			if !pyjson.Truthy(f.v) {
				continue
			}
			if _, ok := f.v.(string); !ok {
				errs = append(errs, pmodel.Err{Loc: []any{f.name}, Type: "string_type",
					Msg: "Input should be a valid string", Input: f.v})
			}
		}
		argObj, isObj := args.(*pyjson.Object)
		if pyjson.Truthy(args) && !isObj {
			errs = append(errs, pmodel.Err{Loc: []any{"arguments"}, Type: "dict_type",
				Msg: "Input should be a valid dictionary", Input: args})
		}
		if len(errs) > 0 {
			e := &pmodel.ValidationError{Title: "MCPStep", Errs: errs}
			return deny(r.internalError(e.String()))
		}
		rec.MCPServer, rec.MCPTool = pyStrOr(server), pyStrOr(tool)
		if argObj == nil {
			argObj = pyjson.NewObject()
		}
		rec.Arguments = argObj
	}
	d := r.evaluateRecord(rec, env, nil, nil)
	d.ToolName, d.StepType, d.Detail = toolName, stepAny, detail
	return d
}
