package cli

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/distill"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/switchyard"
)

// switchyardChild is the child a gateway started, and its state file.
type switchyardChild struct {
	h     *switchyard.Handle
	state string
}

func (c *switchyardChild) stop() string { return c.h.StopOwn(c.state) }

func syConfig(cfg config.Config) switchyard.Config {
	return switchyard.Config{RouteID: cfg.SwitchyardRouteID, CapableModel: cfg.SwitchyardCapableModel,
		EfficientModel: cfg.SwitchyardEfficientModel, APIKeyEnv: cfg.SwitchyardAPIKeyEnv, LLMBaseURL: cfg.LLMBaseURL,
		LLMHostKind: cfg.LLMHostKind}
}

// fail3 is _fail: what, why and fix on stderr, then the exit code.
func (e *Env) fail3(what, why, fix string, code int) error {
	e.errf("%s\n%s\n%s\n", what, why, fix)
	return exit(code)
}

// startSwitchyardForGateway is _start_switchyard_for_gateway: a healthy
// child and the target pair to meter by, or the run ends.
func (e *Env) startSwitchyardForGateway(dataDir, own string, hasOwn bool, port int64) (*gateway.External, string, *switchyardChild, error) {
	binary := switchyard.LookPath(switchyard.BinaryName, e.env["PATH"])
	if problem := switchyard.PrerequisiteProblem(binary); problem != "" {
		e.errf("%s\n", problem)
		return nil, "", nil, exit(3)
	}
	cfg, err := e.loadCfg("gateway", filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		return nil, "", nil, err
	}
	prices := gateway.Prices{}
	files := switchyard.ChildFiles(dataDir, int(port))
	var configPath string
	var auth *switchyard.Auth
	if hasOwn {
		own = gateroot.PathStr(own)
		raw, rerr := os.ReadFile(own)
		text := ""
		if rerr == nil {
			var exc *pystr.Exception
			if text, exc = pystr.DecodeStrict(raw); exc != nil {
				return nil, "", nil, e.fail3(fmt.Sprintf("cannot read %s: %s", own, exc.Msg),
					"the gateway copies your Switchyard config before it starts the child.",
					"check the path you gave to --switchyard-config.", 1)
			}
		} else {
			return nil, "", nil, e.fail3(fmt.Sprintf("cannot read %s: %s", own, switchyard.PyOSError(rerr)),
				"the gateway copies your Switchyard config before it starts the child.",
				"check the path you gave to --switchyard-config.", 1)
		}
		if configPath, err = switchyard.WriteConfig(filepath.Dir(files.Config), text, filepath.Base(files.Config)); err != nil {
			return nil, "", nil, e.fail("gateway", err)
		}
	} else {
		targets := switchyard.TargetsFromConfig(syConfig(cfg))
		if targets == nil {
			return nil, "", nil, e.fail3("no efficient model is set for Switchyard.",
				"the stage router needs a cheaper tier to choose.",
				"run: daisugi install --gateway --router switchyard --efficient-model <id>", 1)
		}
		text, rerr := switchyard.Render(targets, cfg.SwitchyardRouteID, func(name string) bool { return e.env[name] != "" })
		if rerr != nil {
			return nil, "", nil, e.fail3("cannot write the Switchyard config: "+rerr.Error(),
				"the values in config.yaml do not make a valid route.",
				"fix switchyard_* in config.yaml, or run daisugi install again.", 1)
		}
		if configPath, err = switchyard.WriteConfig(filepath.Dir(files.Config), text, filepath.Base(files.Config)); err != nil {
			return nil, "", nil, e.fail("gateway", err)
		}
		a := switchyard.AuthModes(targets)
		auth = &a
		if targets.EfficientLocal {
			prices[targets.EfficientID] = gateway.Price{}
		}
	}
	capable, efficient, err := switchyard.RouteTargets(configPath, cfg.SwitchyardRouteID)
	if err != nil {
		var me *switchyard.MeterError
		if errors.As(err, &me) {
			return nil, "", nil, e.fail3("cannot meter this Switchyard config: "+me.Msg,
				"the gateway books each turn by the two tiers of the route it sends.",
				"name a stage_router route with id switchyard_route_id, or pass a --switchyard-config that has one.", 1)
		}
		return nil, "", nil, e.refuse("gateway", fmt.Errorf("cannot read %s: %w", configPath, err))
	}
	if auth == nil {
		auth = switchyard.AuthFromTOML(configPath, cfg.SwitchyardRouteID)
	}
	statePath := switchyard.StatePath(dataDir, int(port))
	h, msg := switchyard.Start(switchyard.StartOptions{ConfigPath: configPath, Binary: binary, RoutingLog: files.RoutingLog,
		LogPath: files.Log, StatePath: statePath, RouteID: cfg.SwitchyardRouteID, Auth: auth, Port: int(port)})
	e.out("  switchyard: %s\n", msg)
	if h == nil {
		return nil, "", nil, exit(3)
	}
	e.out("  switchyard config: %s\n", configPath)
	e.out("  capable tier: %s   efficient tier: %s\n", capable, efficient)
	if auth != nil {
		e.out("  capable auth: %s\n", auth.Capable)
		e.out("  efficient auth: %s\n", auth.Efficient)
	}
	ext := &gateway.External{RouteID: cfg.SwitchyardRouteID, CapableTarget: capable, EfficientTarget: efficient, Prices: prices}
	return ext, h.BaseURL(), &switchyardChild{h, statePath}, nil
}

const routerHelp = `Usage: daisugi router [OPTIONS] COMMAND [ARGS]...

  The gateway's model chooser: NVIDIA NeMo Switchyard as a managed child,
  or the built-in rules router.

Commands:
  label   Record whether a session's task succeeded: the outcome a graft trial counts.
  status  Show the router choice, the Switchyard binary, each running child, and recent turns.
  stop    Stop every switchyard-server that a gateway started and left running.
`

func (e *Env) router(args []string) error {
	if len(args) == 0 {
		// typer's no_args_is_help: the help, then exit 2.
		e.out("%s", routerHelp)
		return exit(2)
	}
	if args[0] == "--help" {
		e.out("%s", routerHelp)
		return nil
	}
	switch args[0] {
	case "label":
		return e.routerLabel(args[1:])
	case "status":
		return e.routerStatus(args[1:])
	case "stop":
		return e.routerStop(args[1:])
	}
	e.errf("Usage: daisugi router [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi router --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

func (e *Env) loadJournal(cmd, dataDir string) ([]gateway.JRecord, error) {
	rs, err := gateway.LoadJournal(filepath.Join(dataDir, "gateway", "turns.jsonl"))
	if err != nil {
		if errors.Is(err, gateway.ErrJournal) {
			return nil, e.refuse(cmd, err)
		}
		return nil, e.fail(cmd, err)
	}
	return rs, nil
}

func (e *Env) routerStatus(args []string) error {
	const cmd = "router status"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."},
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Show the router choice, the Switchyard binary, each running child, and recent turns.", opts)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
	cfg, err := e.loadCfg(cmd, filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		return err
	}
	binary := switchyard.LookPath(switchyard.BinaryName, e.env["PATH"])
	version := switchyard.Version(binary)
	type child struct {
		obj     *pyjson.Object
		running bool
		auth    *pyjson.Object
		st      *switchyard.State
		path    string
	}
	var children []child
	var unreadable []string
	for _, sf := range switchyard.ListStates(dataDir) {
		if sf.State == nil {
			unreadable = append(unreadable, sf.Path)
			continue
		}
		st := sf.State
		running := switchyard.ChildIsRunning(st.PID)
		healthy := running && switchyard.ProbeHealth(st.Host, st.Port)
		var auth *pyjson.Object
		if a, ok := st.Obj.Value("auth").(*pyjson.Object); ok {
			auth = a
		}
		routeID := st.Obj.Value("route_id")
		if !pyjson.Truthy(routeID) {
			routeID = cfg.SwitchyardRouteID
		}
		o := pyjson.NewObject().Set("pid", st.Obj.Value("pid")).Set("host", st.Host).Set("port", st.Obj.Value("port")).
			Set("running", running).Set("healthy", healthy).Set("config_path", st.Obj.Value("config_path")).
			Set("route_id", routeID)
		if auth != nil {
			o.Set("auth", auth)
		} else {
			o.Set("auth", nil)
		}
		o.Set("state_file", sf.Path)
		children = append(children, child{o, running, auth, st, sf.Path})
	}
	targets := switchyard.TargetsFromConfig(syConfig(cfg))
	var next *switchyard.Auth
	if targets != nil {
		a := switchyard.AuthModes(targets)
		next = &a
	}
	records, err := e.loadJournal(cmd, dataDir)
	if err != nil {
		return err
	}
	recent, err := gateway.ExternalRecords(records)
	if err != nil {
		return e.refuse(cmd, err)
	}
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}
	share, err := gateway.ShareTable(records)
	if err != nil {
		return e.refuse(cmd, err)
	}
	weeks := weekly(dataDir)
	dstate, dlines, err := e.delegateState(dataDir)
	if err != nil {
		return e.refuse(cmd, err)
	}
	tr, err := trialState(dataDir)
	if err != nil {
		return e.refuse(cmd, err)
	}
	if p.flag("--json") {
		o := pyjson.NewObject().Set("router", cfg.GatewayRouter)
		if binary != "" {
			o.Set("binary", binary)
		} else {
			o.Set("binary", nil)
		}
		if version != "" {
			o.Set("version", version)
		} else {
			o.Set("version", nil)
		}
		kids := make([]any, len(children))
		for i, c := range children {
			kids[i] = c.obj
		}
		o.Set("children", kids)
		un := make([]any, len(unreadable))
		for i, u := range unreadable {
			un[i] = u
		}
		o.Set("unreadable_state_files", un)
		if next != nil {
			o.Set("next_start_auth", pyjson.NewObject().Set("capable", next.Capable).Set("efficient", next.Efficient))
		} else {
			o.Set("next_start_auth", nil)
		}
		rows := make([]any, len(share))
		for i, r := range share {
			rows[i] = pyjson.NewObject().Set("model", r.Model).Set("turns", r.Turns).Set("share", r.Share).
				Set("input_tokens", pyjson.Int{Text: r.In.String()}).Set("output_tokens", pyjson.Int{Text: r.Out.String()}).
				Set("frontier_tokens_saved", pyjson.Int{Text: r.Saved.String()})
		}
		o.Set("targets", rows)
		rt := make([]any, len(recent))
		for i, r := range recent {
			rt[i] = pyjson.NewObject().Set("task", r.Task).Set("model", r.Model).Set("downgraded", r.Downgraded)
		}
		o.Set("recent_turns", rt)
		ws := make([]any, len(weeks))
		for i, w := range weeks {
			ws[i] = w.object()
		}
		o.Set("weeks", ws).Set("escalation_built", false).Set("delegate", dstate)
		if tr != nil {
			o.Set("trial", tr.object())
		} else {
			o.Set("trial", nil)
		}
		e.out("%s\n", pyjson.Dumps(o, true))
		return nil
	}
	e.out("configured router: %s\n", cfg.GatewayRouter)
	if binary != "" {
		e.out("  binary:  %s\n", binary)
	} else {
		e.out("  binary:  not found. Install it with: %s\n", switchyard.InstallCmd)
	}
	if version != "" {
		e.out("  version: %s\n", version)
	}
	live := false
	for _, c := range children {
		live = live || c.running
	}
	if live {
		e.out("running router: switchyard\n")
	}
	for _, c := range children {
		if !c.running {
			e.out("  stale state file: %s. Pid %s is not a running switchyard-server. Clear it with: daisugi router stop\n",
				c.path, pyStrOf(c.obj.Value("pid")))
		}
	}
	for _, c := range children {
		if !c.running {
			continue
		}
		status := "not answering"
		if c.obj.Value("healthy") == true {
			status = "healthy"
		}
		e.out("  child:   pid %s on %s:%s, %s, route %s\n", pyStrOf(c.obj.Value("pid")), c.st.Host,
			pyStrOf(c.obj.Value("port")), status, pyStrOf(c.obj.Value("route_id")))
		e.out("    config: %s\n", pyStrOf(c.obj.Value("config_path")))
		if c.auth != nil && c.auth.Len() > 0 {
			capable, ok1 := c.auth.Get("capable")
			if !ok1 {
				e.errf("Traceback (most recent call last): ...\nKeyError: 'capable'\n")
				return exit(1)
			}
			e.out("    capable auth:   %s\n", pyStrOf(capable))
			efficient, ok2 := c.auth.Get("efficient")
			if !ok2 {
				e.errf("Traceback (most recent call last): ...\nKeyError: 'efficient'\n")
				return exit(1)
			}
			e.out("    efficient auth: %s\n", pyStrOf(efficient))
		} else {
			e.out("    auth: not recorded at start\n")
		}
	}
	if !live {
		e.out("  child:   not running. Start it with: daisugi gateway --router switchyard\n")
		if next != nil {
			e.out("  capable auth at next start:   %s\n", next.Capable)
			e.out("  efficient auth at next start: %s\n", next.Efficient)
		}
	}
	for _, u := range unreadable {
		e.out("  unreadable state file: %s. Remove it by hand, or run daisugi router stop.\n", u)
	}
	if len(share) > 0 {
		e.out("  targets:\n")
		for _, ln := range gateway.FormatShareTable(share) {
			e.out("  %s\n", ln)
		}
	}
	e.out("  recent turns, last %d:\n", len(recent))
	for _, r := range recent {
		label := pystr.Slice(strings.Join(pystr.Split(r.Task), " "), 0, 60)
		saved := "no saving"
		if r.Downgraded {
			saved = "saving"
		}
		e.out("    %s  %s  %s\n", gateway.PadRepr(label, 62), r.Model.(string), saved)
	}
	e.out("by week (UTC), newest first; escalation is not built yet:\n")
	if len(weeks) == 0 {
		e.out("  no turns and no delegations recorded\n")
	}
	for _, w := range weeks {
		e.out("  %s\n", weekLine(w))
	}
	for _, ln := range dlines {
		e.out("%s\n", ln)
	}
	if tr != nil {
		for _, ln := range trialLines(tr) {
			e.out("%s\n", ln)
		}
	}
	return nil
}

// pyStrOf is str() of a decoded JSON value.
func pyStrOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyReprOf(v)
}

// pyReprOf is repr() of a decoded JSON value.
func pyReprOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pystr.Repr(x)
	case pyjson.Int:
		return x.Text
	case pyjson.Float:
		return pyFloatRepr(float64(x))
	case float64:
		return pyFloatRepr(x)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyReprOf(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyjson.Object:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			parts = append(parts, pystr.Repr(k)+": "+pyReprOf(x.Value(k)))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func pyFloatRepr(f float64) string {
	switch s := pyjson.FloatRepr(f); s {
	case "NaN":
		return "nan"
	case "Infinity":
		return "inf"
	case "-Infinity":
		return "-inf"
	default:
		return s
	}
}

func (e *Env) routerStop(args []string) error {
	const cmd = "router stop"
	opts := []opt{{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Stop every switchyard-server that a gateway started and left running.", opts)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
	states := switchyard.ListStates(dataDir)
	if len(states) == 0 {
		e.out("no switchyard-server state file; nothing to stop\n")
		return nil
	}
	for _, sf := range states {
		e.out("%s\n", switchyard.Stop(sf.Path))
	}
	return nil
}

func (e *Env) gatewayReport(args []string) error {
	const cmd = "gateway-report"
	opts := []opt{{names: []string{"--data-dir"}, value: true, metavar: "PATH",
		help: "Daisugi data directory (reads <data-dir>/gateway/turns.jsonl)."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Calibrate the gateway on a real recorded day: realized routing + potential reuse.", opts)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
	records, err := e.loadJournal(cmd, dataDir)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		e.out("no turns recorded yet — run `daisugi gateway` to start journaling.\n")
		return nil
	}
	share, err := gateway.ShareTable(records)
	if err != nil {
		return e.refuse(cmd, err)
	}
	if len(share) > 0 {
		e.out("Switchyard targets, measured. Each turn is booked by the target it names:\n")
		for _, ln := range gateway.FormatShareTable(share) {
			e.out("%s\n", ln)
		}
		e.out("\n")
	}
	var cands []gateway.Candidate
	signed := false
	turns := make([]distill.Turn, len(records))
	for i, r := range records {
		signed = signed || r.Signature != ""
		turns[i] = distill.Turn{Signature: r.Signature, Task: r.Task, Dollars: r.Actual}
		t := new(big.Int).Add(r.In, r.CR)
		t.Add(t, r.CC).Add(t, r.Out)
		turns[i].Tokens = t
	}
	if signed {
		m, notBuilt, err := e.matcher()
		if err != nil {
			return e.refuse(cmd, err)
		}
		if notBuilt != "" {
			e.errf("matcher_model=%s is not a built embedder.\n", pystr.Repr(notBuilt))
			return exit(1)
		}
		emb, err := m.Embedder()
		if err != nil {
			e.errf("%v\n", err)
			return exit(2)
		}
		for _, c := range distill.RankReuse(turns, emb, m.Threshold, nil) {
			cands = append(cands, gateway.Candidate{Count: c.Count, Tokens: c.Tokens, Dollars: c.Dollars})
		}
	}
	r := gateway.BuildReport(records, cands)
	f2 := func(x float64) string { return gateway.FormatF(x, 2) }
	e.out("Routing (realized) — measured from turns already run:\n")
	e.out("  turns:                 %d\n", r.Turns)
	e.out("  downgraded turns:      %d\n", r.Downgraded)
	e.out("  frontier tokens saved: %s\n", gateway.Commas(r.FrontierSaved))
	e.out("  dollars saved:         $%s\n", f2(r.DollarsSaved))
	e.out("  blended multiplier:    %sx\n", f2(r.Blended))
	e.out("  local-rung turns:      %d\n", r.LocalTurns)
	e.out("\n")
	e.out("Prompt cache (measured) — the provider's own usage split:\n")
	e.out("  cache read tokens:     %s\n", gateway.Commas(r.CacheRead))
	e.out("  cache write tokens:    %s\n", gateway.Commas(r.CacheCreation))
	e.out("  cache hit rate:        %s of all input\n", gateway.Percent(r.CacheHitRate))
	e.out("\n")
	e.out("Reuse opportunity (ceiling) — NOT measured, assumes perfect fresh reuse:\n")
	e.out("  repeat clusters:       %d\n", r.RepeatClusters)
	e.out("  recoverable tokens:    %s\n", gateway.Commas(r.RecoverableTokens))
	e.out("  recoverable dollars:   $%s\n", f2(r.RecoverableDollars))
	e.out("\n")
	e.out("Combined (ceiling) — realized routing plus the reuse ceiling above:\n")
	e.out("  frontier tokens saved: %s\n", gateway.Commas(r.CombinedSaved))
	e.out("  blended multiplier:    %sx\n", f2(r.CombinedMultiplier))
	e.out("\n")
	e.out("note: the Reuse and Combined figures are a CEILING, assuming every repeat-after-the-first is served " +
		"from a perfect, fresh cache — only Routing above is measured.\n")
	return nil
}
