package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/capture"
	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
	"daisugi-verify/internal/tracejournal"
	"daisugi-verify/internal/transcript"
	"daisugi-verify/internal/verify"
)

// clickFile is click.Path(exists=True, dir_okay=False, readable=True) on
// an argument: the usage error click gives, or nil.
func clickFile(name, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return &usageError{fmt.Sprintf("Invalid value for '%s': File '%s' does not exist.", name, path)}
	}
	if st.IsDir() {
		return &usageError{fmt.Sprintf("Invalid value for '%s': File '%s' is a directory.", name, path)}
	}
	if syscall.Access(path, 4) != nil {
		return &usageError{fmt.Sprintf("Invalid value for '%s': File '%s' is not readable.", name, path)}
	}
	return nil
}

// note is console.note: one line on stderr, silent under -q.
func (e *Env) note(format string, a ...any) {
	if !e.quiet {
		e.errf(format+"\n", a...)
	}
}

// backend is llm.resolve_backend(None).
func (e *Env) backend() string { return e.llmClient().Backend() }

// echoResolved is cli._echo_resolved(DEFAULT_DATA_DIR): the backend, the
// gate's state and the data directory, on stderr.
func (e *Env) echoResolved() error {
	gate, err := e.gateState()
	if err != nil {
		return err
	}
	e.note("backend: %s · gate: %s · data: %s", e.backend(), gate, e.tilde(e.dataHome()))
	return nil
}

// gateState is the gate part of _echo_resolved, which it works out before
// it resolves the backend.
func (e *Env) gateState() (string, error) {
	dataDir := e.dataHome()
	root := gateroot.Join(dataDir, "gate")
	if gateroot.IsDisarmed(root) {
		return "disarmed", nil
	}
	return config.GateMode(gateroot.Join(dataDir, "config.yaml"))
}

// checkLLMFlag is cli._check_llm_flag: a value that names no backend is
// one line, exit 2; the old name gets the line that names api.
func (e *Env) checkLLMFlag(v string) error {
	switch {
	case v == "api" || v == "claude-code":
		return nil
	case v == "litellm":
		e.errf("%s\n", llm.RenamedText)
	default:
		e.errf("Invalid --llm value %s. Must be 'api' or 'claude-code'.\n", pystr.Repr(v))
	}
	return exit(2)
}

// renamedBackend is _echo_resolved on the old backend name: resolve_backend
// raises LLMNotConfigured, which main() prints as one line, exit 1. It
// returns nil when the backend is not the old name.
func (e *Env) renamedBackend(cmd string) error {
	if !llm.Renamed(e.backend()) {
		return nil
	}
	if _, err := e.gateState(); err != nil {
		return e.refuse(cmd, err)
	}
	e.errf("%s\n", llm.RenamedText)
	return exit(1)
}

const splitSystemPrompt = `You split a sequence of agent tool calls into logical sub-tasks.

Given a user message and a numbered list of tool calls, identify coherent sub-tasks.
Each sub-task should represent a focused unit of work.

Return a JSON object with a "subtasks" array. Each subtask has:
- "start_index": first tool call index (inclusive)
- "end_index": last tool call index (inclusive)
- "task": short description of this sub-task (imperative form)

Rules:
- Every tool call must belong to exactly one subtask.
- Subtasks must be contiguous and ordered by index.
- Aim for 5-15 tool calls per subtask.
`

// splitClaude is _call_split_llm on the claude-code backend: `claude -p
// --model=haiku [DAISUGI_CLAUDE_ARGS]` with the prompt on stdin, 60 s,
// in a fresh directory. Any failure is nil: the episode stays whole.
func (e *Env) splitClaude(content string, cwd *string) (any, error) {
	prompt := "[system]\n" + splitSystemPrompt + "\n\n[user]\n" + content
	bin, err := lookPath("claude", e.env["PATH"])
	if err != nil {
		return nil, nil
	}
	args := []string{"-p", "--model=haiku"}
	if raw := pystr.Strip(e.env["DAISUGI_CLAUDE_ARGS"]); raw != "" {
		if extra, err := verify.ShlexSplit(raw); err == nil {
			args = append(args, extra...)
		}
	}
	if *cwd == "" {
		d, err := os.MkdirTemp(e.env["TMPDIR"], "opendaisugi-claude-")
		if err != nil {
			return nil, err
		}
		*cwd = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	c.Dir = *cwd
	c.Env = e.Environ
	c.Stdin = strings.NewReader(prompt)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &bytes.Buffer{}
	if err := c.Run(); err != nil {
		return nil, nil
	}
	if !utf8.Valid(out.Bytes()) {
		return nil, fmt.Errorf("%w: claude wrote text that is not UTF-8", transcript.ErrUnreadable)
	}
	// text=True reads the output with universal newlines.
	text := strings.ReplaceAll(strings.ReplaceAll(out.String(), "\r\n", "\n"), "\r", "\n")
	text = pystr.Strip(text)
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil, nil
	}
	body := text[start : end+1]
	var obj *pyjson.Object
	if v, derr := pyjson.LoadsPy(body, 900); derr == nil {
		obj, _ = v.(*pyjson.Object)
	} else if d := pmodel.DecodeDictText(body); d.Refuse != "" {
		return nil, fmt.Errorf("%w: claude wrote a reply that holds %s", transcript.ErrUnreadable, d.Refuse)
	} else {
		obj = d.Dict
	}
	if obj == nil {
		return nil, nil
	}
	sub, has := obj.Get("subtasks")
	if !has {
		return []any{}, nil
	}
	return sub, nil
}

// splitDefaultModel is journal parse's --model default.
const splitDefaultModel = "anthropic/claude-sonnet-4-20250514"

// splitJSONDepth is well under the nesting where json.loads would raise
// RecursionError; deeper is refused.
const splitJSONDepth = 900

// splitAPI is _call_split_llm on the api backend: one llm_client.complete
// with json_object, the reply's text read by json.loads, and
// body.get("subtasks", []). Unlike the claude-code split, every failure
// raises, so the parse fails: "Parse error: <the exception's text>".
func (e *Env) splitAPI(model, content string) (any, error) {
	reply, err := e.llmClient().Complete(model,
		[]llm.Message{{Role: "system", Content: splitSystemPrompt}, {Role: "user", Content: content}},
		llm.BodyOpts{MaxTokens: -1, JSONObject: true})
	if err != nil {
		var le *llm.Error
		if errors.As(err, &le) {
			return nil, &transcript.ParseError{Msg: le.Msg}
		}
		return nil, err
	}
	v, derr := pyjson.LoadsPy(reply.Text, splitJSONDepth)
	if derr != nil {
		if derr.TooDeep {
			return nil, fmt.Errorf("%w: a split reply nested past what json.loads reads", llm.ErrUnsupported)
		}
		return nil, &transcript.ParseError{Msg: derr.Error()}
	}
	obj, isObj := v.(*pyjson.Object)
	if !isObj {
		return nil, &transcript.ParseError{Msg: "'" + pyTypeName(v) + "' object has no attribute 'get'"}
	}
	sub, has := obj.Get("subtasks")
	if !has {
		return []any{}, nil
	}
	return sub, nil
}

func (e *Env) journalParse(args []string) error {
	const cmd = "journal parse"
	opts := []opt{
		{names: []string{"-o", "--output"}, value: true, metavar: "PATH", help: "Path to write the episodes YAML/JSON file."},
		{names: []string{"--format"}, value: true, metavar: "TEXT", help: "Parser format (default: claude-code)."},
		{names: []string{"--min-tools"}, value: true, metavar: "INTEGER", help: "Merge episodes below this tool-call threshold."},
		{names: []string{"--max-tools"}, value: true, metavar: "INTEGER", help: "LLM-split episodes above this tool-call threshold."},
		{names: []string{"--model"}, value: true, metavar: "TEXT", help: "Model for LLM splitting (rarely needed)."},
		{names: []string{"--json"}, help: "Write JSON instead of YAML."},
		{names: []string{"--llm"}, value: true, metavar: "TEXT", help: "LLM backend: api | claude-code. Default: auto-detect."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " TRANSCRIPT", "Parse an agent transcript into episodes.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "TRANSCRIPT", "TRANSCRIPT")
	}
	path := p.args[0]
	if err := clickFile("transcript", path); err != nil {
		return e.usage(cmd, err)
	}
	minTools, err := clickInt(p, "--min-tools", 3)
	if err != nil {
		return e.usage(cmd, err)
	}
	maxTools, err := clickInt(p, "--max-tools", 30)
	if err != nil {
		return e.usage(cmd, err)
	}
	if !p.has("-o") {
		return e.usage(cmd, &usageError{"Missing option '-o' / '--output'."})
	}
	output := p.str("-o", "")
	if llm := p.str("--llm", ""); p.has("--llm") {
		if err := e.checkLLMFlag(llm); err != nil {
			return err
		}
		e.env["OPENDAISUGI_LLM_BACKEND"] = llm
		e.Environ = append(e.Environ, "OPENDAISUGI_LLM_BACKEND="+llm)
	}
	if err := e.renamedBackend(cmd); err != nil {
		return err
	}
	format := p.str("--format", "claude-code")
	if format != "claude-code" && format != "codex" {
		if err := e.echoResolved(); err != nil {
			return e.refuse(cmd, err)
		}
		e.errf("Unknown parser format: %s. Available: ['claude-code', 'codex']\n", pystr.Repr(format))
		return exit(2)
	}
	// The transcript is read before the note Python prints first, so a
	// refusal is the run's one line. An exception the parse raises is
	// printed after the note, as Python prints it.
	raw, err := os.ReadFile(path)
	if err != nil {
		return e.failPy(cmd, err)
	}
	eps, err := transcript.Read(raw, format)
	var pe *transcript.ParseError
	if err != nil && !errors.As(err, &pe) {
		return e.refuse(cmd, err)
	}
	if pe != nil {
		if err := e.echoResolved(); err != nil {
			return e.refuse(cmd, err)
		}
		e.errf("Parse error: %s\n", pe.Msg)
		return exit(2)
	}
	eps = transcript.MergeSmall(eps, minTools)
	// Splitting asks a model. On the api backend a proxy setting the
	// binary does not read as httpx does is refused before any call.
	model := p.str("--model", splitDefaultModel)
	viaAPI := e.backend() != "claude-code"
	for _, ep := range eps {
		if int64(ep.Steps()) > maxTools && viaAPI {
			if err := e.llmClient().Check(model); err != nil {
				return e.refuse(cmd, err)
			}
			break
		}
	}
	if err := e.echoResolved(); err != nil {
		return e.refuse(cmd, err)
	}
	neutral := ""
	eps, err = transcript.Split(eps, maxTools, func(content string) (any, error) {
		if viaAPI {
			return e.splitAPI(model, content)
		}
		return e.splitClaude(content, &neutral)
	})
	if errors.Is(err, llm.ErrUnsupported) {
		return e.refuse(cmd, err)
	}
	if neutral != "" {
		defer os.RemoveAll(neutral)
	}
	var episodes []any
	if err == nil {
		episodes, err = transcript.Finalize(eps)
	}
	if errors.As(err, &pe) {
		e.errf("Parse error: %s\n", pe.Msg)
		return exit(2)
	}
	if err != nil {
		return e.refuse(cmd, err)
	}
	payload := pyjson.NewObject().Set("source", format).Set("source_file", gateroot.PathStr(path)).
		Set("parsed_at", time.Now().UTC().Format("2006-01-02T15:04:05Z")).Set("episodes", episodes)
	var text string
	if p.flag("--json") {
		text = pyjson.DumpsIndent(payload, 2, true)
	} else {
		var why *pyyaml.Unsupported
		if text, why = pyyaml.SafeDump(payload); why != nil {
			return e.refuse(cmd, why)
		}
	}
	if err := os.WriteFile(output, []byte(text), 0o666); err != nil {
		return e.failPy(cmd, err)
	}
	e.out("Parsed %d episodes to %s\n", len(eps), gateroot.PathStr(output))
	return nil
}

// ingestItem is one episode's outcome (ingest.EpisodeResult), with what
// a real run journals for it.
type ingestItem struct {
	id, task, status  string
	steps, violations int
	errText           string
	// hasErr is set for ERROR: errText is the error.
	hasErr  bool
	traceID string
	env     *pyjson.Object
	plan    *pyjson.Object
	result  *pyjson.Object
	// loadErr is load_trace raising something other than not found: the
	// oracle stops there with a traceback.
	loadErr error
}

// loadEpisodes is yaml.safe_load(episodes_file.read_text()): the value,
// or the yaml.YAMLError the oracle prints.
func loadEpisodes(text string) (any, *pystr.Exception, error) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	v, exc, why := pyyaml.Load(text)
	if why != nil {
		return nil, nil, fmt.Errorf("the episodes file holds YAML this binary does not read (%s)", why.Why)
	}
	if exc != nil {
		if exc.Type == "ValueError" {
			return nil, nil, errors.New("the episodes file makes yaml raise a ValueError, which the oracle does not catch")
		}
		return nil, exc, nil
	}
	if !pyyaml.Plain(v) {
		return nil, nil, errors.New("the episodes file holds a date or a key that is not text")
	}
	return v, nil, nil
}

// traceIDOK is journal._TRACE_ID_RE.
func traceIDOK(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (e *Env) journalIngest(args []string) error {
	const cmd = "journal ingest"
	opts := []opt{journalDataDirOpt,
		{names: []string{"--dry-run"}, help: "Show what would be ingested without LLM calls."},
		decomposeOpt,
		{names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " EPISODES_FILE", "Ingest parsed episodes into the journal.", opts)
	}
	if len(p.args) < 1 {
		return e.missingArg(cmd, "EPISODES_FILE", "EPISODES_FILE")
	}
	path := p.args[0]
	if err := clickFile("episodes_file", path); err != nil {
		return e.usage(cmd, err)
	}
	dry := p.flag("--dry-run")
	if _, set := e.env["OPENDAISUGI_CONFORMANCE_RECORD"]; set && !dry {
		return e.notYet("daisugi journal ingest with OPENDAISUGI_CONFORMANCE_RECORD set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return e.failPy(cmd, err)
	}
	if !utf8.Valid(raw) {
		return e.refuse(cmd, errors.New("the episodes file is not UTF-8"))
	}
	v, yexc, err := loadEpisodes(string(raw))
	if err != nil {
		return e.refuse(cmd, err)
	}
	if yexc != nil {
		e.errf("Invalid YAML: %s\n", yexc.Msg)
		return exit(2)
	}
	obj, isObj := v.(*pyjson.Object)
	if !isObj {
		e.errf("Invalid episodes file: opendaisugi.parsers.ParseResult() argument after ** must be a mapping, not %s\n",
			pyTypeName(v))
		return exit(2)
	}
	pr, verr := transcriptValidate(obj)
	if verr != nil {
		e.errf("Invalid episodes file: %s\n", verr.String())
		return exit(2)
	}
	dataDir := e.dataDir(p)
	decompose := p.flag("--allow-shell-decomposition")
	if !p.flagSet("--allow-shell-decomposition") {
		cfg, err := config.Load(gateroot.Join(dataDir, "config.yaml"))
		if err != nil {
			return e.configLoadErr(cmd, err)
		}
		decompose = cfg.ShellAllowDecomposition
	}
	sourceFile := pr.Value("source_file").(string)
	sum := sha256Hex(sourceFile)[:8]
	episodes := pr.Value("episodes").([]any)
	// Everything is worked out before the first write, so input this
	// binary cannot journal the way Python does changes nothing. A trace
	// already ingested is found by its YAML body, as load_trace finds it.
	traces := &tracejournal.Journal{TracesDir: gateroot.Join(dataDir, "journal/traces")}
	items := make([]*ingestItem, 0, len(episodes))
	for _, x := range episodes {
		ep := x.(*pyjson.Object)
		it := &ingestItem{id: ep.Value("id").(string), task: ep.Value("task").(string)}
		steps := ep.Value("steps").([]any)
		it.steps = len(steps)
		it.traceID = "import-" + sum + "-" + it.id
		items = append(items, it)
		_, lerr := traces.LoadTrace(it.traceID)
		var le *tracejournal.LoadError
		switch {
		case lerr == nil:
			it.status = "SKIP"
			continue
		case errors.As(lerr, &le) && le.Type == "FileNotFoundError":
		case errors.As(lerr, &le):
			// load_trace raises: the oracle stops at this episode.
			it.loadErr = lerr
		default:
			return e.loadErr(cmd, lerr)
		}
		if it.loadErr != nil {
			break
		}
		if err := e.ingestOne(cmd, it, steps, decompose, dry); err != nil {
			return err
		}
	}
	j, err := e.openJournal(cmd, dataDir)
	if err != nil {
		return err
	}
	defer j.Close()
	for _, it := range items {
		if it.loadErr != nil {
			// load_trace raised: the oracle's asyncio.run ends with it.
			return e.loadErr(cmd, it.loadErr)
		}
		if dry || it.status != "" && it.status != "OK" && it.status != "FAIL" {
			continue
		}
		if !traceIDOK(it.traceID) {
			it.status, it.hasErr, it.violations = "ERROR", true, 0
			it.errText = fmt.Sprintf("Invalid trace_id %s: must contain only alphanumeric characters, hyphens, "+
				"underscores, and dots", pystr.Repr(it.traceID))
			continue
		}
		created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		if err := j.Log(it.task, it.env, it.plan, it.result, it.traceID, created); err != nil {
			if errors.Is(err, tracejournal.ErrUnreadable) {
				return e.refuse(cmd, err)
			}
			it.status, it.hasErr, it.violations = "ERROR", true, 0
			it.errText = err.Error()
		}
	}
	return e.ingestReport(items, path, dry, p.flag("--json"))
}

// ingestOne is _process_episode up to the journal write: the envelope
// inferred from the episode's steps, the plan verified against it.
func (e *Env) ingestOne(cmd string, it *ingestItem, steps []any, decompose, dry bool) error {
	var records []*pyjson.Object
	for _, x := range steps {
		s := x.(*pyjson.Object)
		switch s.Value("type") {
		case "shell":
			records = append(records, pyjson.NewObject().Set("step_type", "shell").Set("command", s.Value("command")))
		case "file_read":
			records = append(records, pyjson.NewObject().Set("step_type", "file_read").Set("path", s.Value("path")))
		case "file_write":
			records = append(records, pyjson.NewObject().Set("step_type", "file_write").Set("path", s.Value("path")))
		case "network":
			records = append(records, pyjson.NewObject().Set("step_type", "network").Set("url", s.Value("url")))
		case "mcp":
			records = append(records, pyjson.NewObject().Set("step_type", "mcp").Set("mcp_server", s.Value("server")).
				Set("mcp_tool", s.Value("tool")))
		}
	}
	env, err := capture.InferEnvelope(records, it.task, decompose)
	if err != nil {
		return e.refuse(cmd, err)
	}
	planID, err := randHex8()
	if err != nil {
		return e.failPy(cmd, err)
	}
	plan := pyjson.NewObject().Set("id", "plan_"+planID).Set("source", "claude-code-import").
		Set("task", it.task).Set("steps", steps)
	vp, err := verify.ParsePlan([]byte(pathways.DumpJSON(plan)))
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("the plan of %s: %v", it.id, err))
	}
	ve, err := verify.ParseEnvelope([]byte(pathways.DumpJSON(env)))
	if err != nil {
		return e.refuse(cmd, fmt.Errorf("the envelope of %s: %v", it.id, err))
	}
	res, ok, n, err := journalResult(vp, ve, env, plan, it.task, "episode "+it.id)
	it.violations = n
	it.status = "FAIL"
	if ok {
		it.status = "OK"
	}
	if dry {
		return nil
	}
	if err != nil {
		return e.refuse(cmd, err)
	}
	it.env, it.plan, it.result = env, plan, res
	return nil
}

// journalResult is verify(plan, envelope) as a trace stores its result:
// the result, whether it is ok, and the number of violations. err is set
// when the result holds a violation detail or a warning this binary does
// not word, or when the trace body of task is one it cannot write; what
// names the input in that error.
func journalResult(vp verify.ActionPlan, ve verify.Envelope, env, plan *pyjson.Object, task, what string) (*pyjson.Object,
	bool, int, error) {
	t0 := time.Now()
	res := verify.Verify(vp, ve, verify.VerifyOptions{Z3TimeoutMs: 500})
	ms := float64(time.Since(t0).Nanoseconds()) / 1e6
	n := len(res.Violations)
	violations := []any{}
	for _, v := range res.Violations {
		if !v.Known {
			return nil, res.OK, n, fmt.Errorf("%s would be journaled with a violation (%s), whose detail this "+
				"binary does not write yet", what, v.Message)
		}
		var rem any
		if v.Remediation != nil {
			rem = *v.Remediation
		}
		violations = append(violations, pyjson.NewObject().Set("stage", v.Stage).Set("message", v.Message).
			Set("detail", v.Detail).Set("suggested_remediation", rem))
	}
	if res.WarningsUnmodeled {
		return nil, res.OK, n, fmt.Errorf("%s may carry a verifier warning this binary does not word yet", what)
	}
	warnings := []any{}
	for _, w := range res.Warnings {
		warnings = append(warnings, w)
	}
	result := pyjson.NewObject().Set("ok", res.OK).Set("violations", violations).Set("warnings", warnings).
		Set("envelope_id", env.Value("id")).Set("plan_id", plan.Value("id")).Set("duration_ms", ms).
		Set("client", "python").Set("fallback", nil).Set("client_verdict", nil)
	// The body must be one the binary can write.
	if _, err := tracejournal.TraceBody(task, env, plan, result, "2000-01-01-00000000", "2000-01-01T00:00:00Z"); err != nil {
		return nil, res.OK, n, err
	}
	return result, res.OK, n, nil
}

func (e *Env) ingestReport(items []*ingestItem, path string, dry, asJSON bool) error {
	var passed, failed, skipped, errored int
	for _, it := range items {
		switch it.status {
		case "OK":
			passed++
		case "FAIL":
			failed++
		case "SKIP":
			skipped++
		case "ERROR":
			errored++
		}
	}
	if asJSON {
		eps := make([]any, len(items))
		for i, it := range items {
			var errV any
			if it.hasErr {
				errV = it.errText
			}
			eps[i] = pyjson.NewObject().Set("episode_id", it.id).Set("task", it.task).Set("status", it.status).
				Set("steps", it.steps).Set("violations", it.violations).Set("error", errV)
		}
		o := pyjson.NewObject().Set("total", len(items)).Set("passed", passed).Set("failed", failed).
			Set("skipped", skipped).Set("errored", errored).Set("episodes", eps)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
	} else {
		for _, it := range items {
			detail := fmt.Sprintf("%d steps", it.steps)
			if it.violations > 0 {
				detail += fmt.Sprintf(", %d violations", it.violations)
			}
			if it.hasErr {
				detail = it.errText
			}
			e.out("%s  %s \"%s\" (%s)\n", it.id, it.status+strings.Repeat(" ", max(0, 7-len(it.status))), it.task, detail)
		}
		e.out("\n")
		verb, note, pass, fail := "Ingested", "", "passed", "failed"
		if dry {
			verb, note, pass, fail = "Previewed", " (dry run — nothing written)", "would pass", "would fail"
		}
		name := gateroot.PathStr(path)
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		e.out("%s %d episodes from %s%s\n", verb, len(items), name, note)
		e.out("  %d %s verification\n", passed, pass)
		e.out("  %d %s verification\n", failed, fail)
		e.out("  %d skipped (already in journal)\n", skipped)
		if errored > 0 {
			e.out("  %d errored\n", errored)
		}
	}
	if errored > 0 {
		return exit(1)
	}
	return nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func transcriptValidate(o *pyjson.Object) (*pyjson.Object, *pmodel.ValidationError) {
	return transcript.Validate(o)
}

// pyTypeName is type(v).__name__ of a JSON value.
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case pyjson.Int:
		return "int"
	case float64, pyjson.Float:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	}
	return "dict"
}
