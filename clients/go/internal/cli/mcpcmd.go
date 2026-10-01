package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"daisugi-verify/internal/envgen"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/mcpwire"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
)

// refusalCode is the JSON-RPC error code of a request this binary does
// not answer the oracle's way (MCP-3).
const refusalCode = -32000

var mcpHelp = `Usage: daisugi mcp [OPTIONS] COMMAND [ARGS]...

  Serve openDaisugi tools over MCP stdio.

Options:
  --help  Show this message and exit.

Commands:
  serve  Serve openDaisugi tools over MCP stdio.
`

func (e *Env) mcpCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", mcpHelp)
		return nil
	}
	if args[0] == "serve" {
		return e.mcpServe(args[1:])
	}
	e.errf("Usage: daisugi mcp [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi mcp --help' for help.\n\n"+
		"Error: No such command '%s'.\n", args[0])
	return exit(2)
}

// mcpServer is the state of one `daisugi mcp serve`: the facade's data
// directory and model, and the stores it opens on first use.
type mcpServer struct {
	e           *Env
	dataDir     string
	model       string
	initialized bool
	store       *pathways.Store
	journal     *tracejournal.Journal
	cache       *envgen.Cache
	out         io.Writer
	// stale prints the stale-embeddings warning once per server, as
	// Python does once per process.
	stale *staleWarner
}

func (e *Env) mcpServe(args []string) error {
	const cmd = "mcp serve"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: ""},
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "Model used for envelope generation."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Serve openDaisugi tools over MCP stdio.", opts)
	}
	s := &mcpServer{e: e, out: e.Stdout,
		dataDir: gateroot.PathStr(p.str("--data-dir", e.dataHome())),
		model:   p.str("--model", envgen.DefaultModel)}
	// Daisugi(model=..., data_dir=...) makes the envelope cache.
	if s.cache, err = envgen.OpenCache(filepath.Join(s.dataDir, "envelope_cache.db")); err != nil {
		return e.fail(cmd, err)
	}
	defer s.close()
	lr := mcpwire.NewLineReader(e.Stdin)
	for {
		line, ok := lr.Next()
		if !ok {
			return nil
		}
		s.handle(line)
	}
}

func (s *mcpServer) close() {
	if s.store != nil {
		s.store.Close()
	}
	if s.journal != nil {
		s.journal.Close()
	}
}

func (s *mcpServer) write(line string) {
	fmt.Fprintf(s.out, "%s\n", line)
	if f, ok := s.out.(*os.File); ok {
		_ = f.Sync()
	}
}

// refuse answers a request this binary does not answer the oracle's way.
func (s *mcpServer) refuse(id any, what string) {
	s.write(mcpwire.ErrorReply(id, refusalCode, what+" is not in this binary yet.", nil))
}

func (s *mcpServer) handle(line string) {
	m := mcpwire.Classify(line)
	switch m.Kind {
	case mcpwire.Invalid:
		s.write(mcpwire.ErrorNotification)
		return
	case mcpwire.Notification:
		if m.Method == "notifications/initialized" {
			s.initialized = true
		}
		return
	}
	switch mcpwire.Check(m) {
	case "invalid":
		s.write(mcpwire.InvalidParams(m.ID))
		return
	case "unported":
		s.refuse(m.ID, "daisugi mcp: the request "+m.Method+" in this form")
		return
	}
	if m.Method != "initialize" && m.Method != "ping" && !s.initialized {
		s.write(mcpwire.InvalidParams(m.ID))
		return
	}
	switch m.Method {
	case "initialize":
		s.write(mcpwire.Reply(m.ID, mcpwire.InitializeResult(mcpwire.Protocol(m.Params.Value("protocolVersion")))))
		s.initialized = true
	case "ping":
		s.write(mcpwire.Reply(m.ID, "{}"))
	case "tools/list":
		s.write(mcpwire.Reply(m.ID, mcpwire.ToolsList))
	case "resources/list":
		s.write(mcpwire.Reply(m.ID, `{"resources":[]}`))
	case "resources/templates/list":
		s.write(mcpwire.Reply(m.ID, `{"resourceTemplates":[]}`))
	case "prompts/list":
		s.write(mcpwire.Reply(m.ID, `{"prompts":[]}`))
	case "prompts/get":
		s.write(mcpwire.ErrorReply(m.ID, 0, "Unknown prompt: "+m.Params.Value("name").(string), nil))
	case "logging/setLevel", "resources/subscribe", "resources/unsubscribe", "completion/complete",
		"tasks/get", "tasks/result", "tasks/list", "tasks/cancel":
		// The oracle's server registers no handler for these.
		s.write(mcpwire.ErrorReply(m.ID, -32601, "Method not found", nil))
	case "resources/read":
		uri, _ := mcpwire.NormURI(m.Params.Value("uri").(string))
		s.write(mcpwire.ErrorReply(m.ID, 0, "Unknown resource: "+uri, nil))
	case "tools/call":
		name := m.Params.Value("name").(string)
		args, _ := m.Params.Value("arguments").(*pyjson.Object)
		if args == nil {
			args = pyjson.NewObject()
		}
		result, refusal := s.safeCall(name, args)
		if refusal != "" {
			s.refuse(m.ID, "daisugi mcp: "+name+" with "+refusal)
			return
		}
		s.write(mcpwire.Reply(m.ID, result))
	}
}

// safeCall is call, with a panic taken as a refusal: the server answers
// the request and goes on, as the oracle's does.
func (s *mcpServer) safeCall(name string, args *pyjson.Object) (result, why string) {
	defer func() {
		if r := recover(); r != nil {
			result, why = "", fmt.Sprintf("input this binary does not handle (%v)", r)
		}
	}()
	return s.call(name, args)
}

// toolError is an exception a tool raised: FastMCP writes its text.
type toolError struct{ text string }

func (t *toolError) Error() string { return t.text }

// refusal is input this binary does not answer the oracle's way.
type refusal struct{ why string }

func (r *refusal) Error() string { return r.why }

func refusef(format string, a ...any) error { return &refusal{fmt.Sprintf(format, a...)} }

// listTools are the tools whose return is a list or None: FastMCP wraps
// it as {"result": ...}.
var listTools = map[string]bool{"find_pathway": true, "list_pathways": true, "receipts_for_run": true,
	"recent_runs": true}

// call is FastMCP's call_tool for one tool: the result's JSON, or a
// refusal.
func (s *mcpServer) call(name string, args *pyjson.Object) (string, string) {
	model, known := mcpwire.Args[name]
	if !known {
		return mcpwire.ToolResult([]string{"Unknown tool: " + name}, nil, true), ""
	}
	pre, ok := mcpwire.PreParse(model, args)
	if !ok {
		return "", "an argument nested deeper than this binary reads"
	}
	if pystr.HasSurrogate(pyjson.Dumps(pre, false)) {
		return "", "an argument that holds a lone surrogate"
	}
	v, verr := pmodel.Validate(model.Name, model, pre, pmodel.Python)
	if verr != nil {
		return mcpwire.ToolResult([]string{"Error executing tool " + name + ": " + verr.String()}, nil, true), ""
	}
	a := v.(*pyjson.Object)
	var out any
	var err error
	switch name {
	case "envelope_for":
		out, err = s.envelopeFor(a)
	case "find_pathway":
		out, err = s.findPathway(a)
	case "recall":
		out, err = s.recall(a)
	case "recall_answer":
		out, err = s.recallAnswer(a)
	case "verify_plan":
		out, err = s.verifyPlan(a)
	case "verify_completed_step":
		out, err = s.verifyCompletedStep(a)
	case "list_pathways":
		out, err = s.listPathways()
	case "pathway_stats":
		out, err = s.pathwayStats()
	case "run_plan":
		out, err = s.runPlan(a)
	case "receipts_for_run":
		out, err = s.receiptsForRun(a)
	case "recent_runs":
		out, err = s.recentRuns(a)
	case "delegate":
		out, err = s.delegate(a)
	}
	var te *toolError
	var re *refusal
	switch {
	case errors.As(err, &te):
		return mcpwire.ToolResult([]string{"Error executing tool " + name + ": " + te.text}, nil, true), ""
	case errors.As(err, &re):
		return "", re.why
	case err != nil:
		return "", "an error this binary does not word (" + err.Error() + ")"
	}
	// _convert_to_content: a list is one text block per item, None none.
	var texts []string
	switch x := out.(type) {
	case nil:
	case []any:
		for _, item := range x {
			texts = append(texts, mcpwire.Indent(item))
		}
	default:
		texts = []string{mcpwire.Indent(out)}
	}
	structured := mcpwire.JSONMode(out)
	if listTools[name] {
		structured = pyjson.NewObject().Set("result", structured)
	}
	return mcpwire.ToolResult(texts, structured, false), ""
}

// pathwayStore is the facade's lazy PathwayStore(data_dir/pathways.db).
func (s *mcpServer) pathwayStore() (*pathways.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	st, err := pathways.Open(filepath.Join(s.dataDir, "pathways.db"))
	if err != nil {
		return nil, refusef("a pathway store this binary does not open (%v)", err)
	}
	s.store = st
	return st, nil
}

// theJournal is the facade's lazy Journal(data_dir).
func (s *mcpServer) theJournal() (*tracejournal.Journal, error) {
	if s.journal != nil {
		return s.journal, nil
	}
	j, err := tracejournal.Open(s.dataDir)
	if err != nil {
		return nil, refusef("a journal this binary does not open (%v)", err)
	}
	s.journal = j
	return j, nil
}

func (s *mcpServer) pathwayStats() (any, error) {
	st, err := s.pathwayStore()
	if err != nil {
		return nil, err
	}
	n, hits, err := st.Stats()
	if err != nil {
		return nil, refusef("a pathway store this binary does not read (%v)", err)
	}
	return pyjson.NewObject().Set("count", pyjson.Int{Text: strconv.FormatInt(n, 10)}).Set("total_hits", hits), nil
}

func (s *mcpServer) listPathways() (any, error) {
	st, err := s.pathwayStore()
	if err != nil {
		return nil, err
	}
	all, err := st.ReadAll(func(string) bool { return false })
	if err != nil {
		return nil, refusef("a pathway store this binary does not read (%v)", err)
	}
	out := []any{}
	for _, p := range all {
		out = append(out, pyjson.NewObject().Set("id", p.ID()).Set("task_description", p.Task()).
			Set("hit_count", p.Obj.Value("hit_count")).Set("distilled_at", p.Obj.Value("distilled_at")))
	}
	return out, nil
}

func intArg(a *pyjson.Object, k string) (int64, bool) {
	n, ok := a.Value(k).(pyjson.Int)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(n.Text, 10, 64)
	return v, err == nil
}

func (s *mcpServer) recentRuns(a *pyjson.Object) (any, error) {
	limit, ok := intArg(a, "limit")
	if !ok || limit != int64(int(limit)) {
		return nil, refusef("a limit outside what SQLite takes")
	}
	j, err := s.theJournal()
	if err != nil {
		return nil, err
	}
	rows, err := j.ListRecent(int(limit))
	if err != nil {
		return nil, refusef("a journal this binary does not read (%v)", err)
	}
	out := []any{}
	for _, r := range rows {
		id, ok1 := r.ID.(string)
		task, ok2 := r.Task.(string)
		created, ok3 := r.CreatedAt.(string)
		okv, ok4 := r.OK.(int64)
		dur, ok5 := sqlFloat(r.DurationMs)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			return nil, refusef("a journal row this binary does not read as a Trace")
		}
		if _, ok := r.PlanID.(string); !ok {
			return nil, refusef("a journal row this binary does not read as a Trace")
		}
		if _, ok := r.EnvelopeID.(string); !ok {
			return nil, refusef("a journal row this binary does not read as a Trace")
		}
		if v, ok := r.Violations.(string); !ok || v != "[]" {
			return nil, refusef("a journal row with violations, which this binary does not read as a Trace yet")
		}
		out = append(out, pyjson.NewObject().Set("run_id", id).Set("task", task).Set("ok", okv != 0).
			Set("duration_ms", dur).Set("created_at", created))
	}
	return out, nil
}

// sqlFloat is a REAL or INTEGER column read as a float field.
func sqlFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func (s *mcpServer) receiptsForRun(a *pyjson.Object) (any, error) {
	runID := a.Value("run_id").(string)
	j, err := s.theJournal()
	if err != nil {
		return nil, err
	}
	rows, err := j.Receipts(runID)
	if err != nil {
		return nil, refusef("a journal this binary does not read (%v)", err)
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, pyjson.NewObject().Set("step_id", r.StepID).Set("run_id", r.RunID).
			Set("timestamp", r.Timestamp).Set("evidence_hash", r.EvidenceHash).Set("verify_result", r.VerifyResult).
			Set("verify_details", r.VerifyDetails).Set("model_id", r.ModelID))
	}
	return out, nil
}

// validationText is str(ValidationError) for a model the tool validates.
func validate(title string, m *pmodel.Model, v any) (*pyjson.Object, error) {
	out, verr := pmodel.Validate(title, m, v, pmodel.Python)
	if verr != nil {
		if why := verr.Unreadable(); why != "" {
			return nil, refusef("%s", why)
		}
		return nil, &toolError{verr.String()}
	}
	return out.(*pyjson.Object), nil
}

// short is the first line of an error, for a refusal's text.
func short(err error) string {
	return strings.SplitN(err.Error(), "\n", 2)[0]
}
