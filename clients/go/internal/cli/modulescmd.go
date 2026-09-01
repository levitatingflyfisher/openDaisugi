package cli

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/switchyard"
)

// This file is opendaisugi.modules: the module map `daisugi modules`
// draws, and the stages `daisugi dashboard` shares with it.

// The state of one module within a stage.
const (
	modActive    = "active"
	modAvailable = "available"
	modPossible  = "possible"
)

// wModule is modules.Module.
type wModule struct{ name, state, note string }

// wStage is modules.Stage.
type wStage struct {
	key, title, role string
	modules          []wModule
}

// The glyph sets of console.BOX and console.ASCII_BOX this view uses.
type glyphSet struct {
	h, v, tl, tr, bl, br, on, avail, off, flow, down string
}

var (
	boxGlyphs   = glyphSet{"─", "│", "┌", "┐", "└", "┘", "●", "○", "·", "▸", "▼"}
	asciiGlyphs = glyphSet{"-", "|", "+", "+", "+", "+", "*", "o", ".", ">", "v"}
)

func (g glyphSet) state(s string) string {
	switch s {
	case modActive:
		return g.on
	case modAvailable:
		return g.avail
	}
	return g.off
}

// stageEffect is swap.STAGE_EFFECT: how a choice at each stage takes
// effect.
var stageEffect = map[string]string{
	"harness": "cfg", "gate": "cfg", "verifier": "live", "shell": "live", "envelope": "cfg",
	"backend": "live", "matcher": "live", "router": "live", "distill": "live", "stores": "planned",
	"floor_report": "cfg", "floor_backend": "live", "voice_engine": "cfg",
}

// The extras this binary carries, answered as a base install of the
// oracle with only them would answer (K4-1): potion and the bash grammar
// are in it; the MiniLM and int8 matchers and the voice engines are not.
const (
	carriesMiniLM = false
	carriesPotion = true
	carriesInt8   = false
	int8Reason    = "needs opendaisugi[int8]"
)

// hookSettings is what modules._claude_hook_installed reads from
// ~/.claude/settings.json: the command of every hook, by event. unread
// is a file that is missing or does not parse, which holds no hook.
type hookSettings struct {
	unread bool
	cmds   map[string][]string
}

// installed is _claude_hook_installed(kind, event=event).
func (h hookSettings) installed(kind, event string) bool {
	for _, c := range h.cmds[event] {
		if strings.Contains(c, kind) {
			return true
		}
	}
	return false
}

var errHookShape = errors.New("~/.claude/settings.json holds hooks in a shape the oracle raises on, " +
	"which this binary does not read yet")

// readHookSettings reads ~/.claude/settings.json as the three checks the
// map makes read it (PreToolUse, Stop and Notification). A file that
// cannot be read or parsed holds no hook, as in Python. A shape the
// oracle's .get or `in` raises on is an error: the map refuses it
// (K4-4).
func readHookSettings(path string) (hookSettings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return hookSettings{unread: true}, nil
	}
	text, exc := pystr.DecodeStrict(raw)
	if exc != nil {
		return hookSettings{unread: true}, nil
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		if derr.TooDeep {
			return hookSettings{}, errHookShape
		}
		return hookSettings{unread: true}, nil
	}
	data, ok := v.(*pyjson.Object)
	if !ok {
		return hookSettings{}, errHookShape
	}
	out := hookSettings{cmds: map[string][]string{}}
	hv, has := data.Get("hooks")
	if !has {
		return out, nil
	}
	hooks, ok := hv.(*pyjson.Object)
	if !ok {
		return hookSettings{}, errHookShape
	}
	for _, event := range []string{"PreToolUse", "Stop", "Notification"} {
		ev, has := hooks.Get(event)
		if !has {
			continue
		}
		entries, ok := ev.([]any)
		if !ok {
			return hookSettings{}, errHookShape
		}
		for _, en := range entries {
			eo, ok := en.(*pyjson.Object)
			if !ok {
				return hookSettings{}, errHookShape
			}
			hl, has := eo.Get("hooks")
			if !has {
				continue
			}
			list, ok := hl.([]any)
			if !ok {
				return hookSettings{}, errHookShape
			}
			for _, h := range list {
				ho, ok := h.(*pyjson.Object)
				if !ok {
					return hookSettings{}, errHookShape
				}
				c, has := ho.Get("command")
				if !has {
					out.cmds[event] = append(out.cmds[event], "")
					continue
				}
				s, ok := c.(string)
				if !ok {
					return hookSettings{}, errHookShape
				}
				out.cmds[event] = append(out.cmds[event], s)
			}
		}
	}
	return out, nil
}

// isDir is Path.is_dir(), following a link.
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// opencodePluginPath is install.opencode_plugin_path.
func (e *Env) opencodePluginPath() string {
	base := gateroot.Join(e.home, ".config")
	if xdg := e.env["XDG_CONFIG_HOME"]; xdg != "" && strings.HasPrefix(xdg, "/") {
		base = gateroot.PathStr(xdg)
	}
	return gateroot.Join(base, "opencode/plugins/daisugi-gate.ts")
}

// coppiceSocketPresent is modules._coppice_socket_present: a socket this
// user owns at the coppice server path, not followed through a link.
func (e *Env) coppiceSocketPresent() bool {
	base := gateroot.Join(e.home, ".opendaisugi/coppice")
	if rt := e.env["XDG_RUNTIME_DIR"]; rt != "" {
		base = gateroot.Join(gateroot.PathStr(rt), "coppice")
	}
	st, err := os.Lstat(filepath.Join(base, "server.sock"))
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return st.Mode()&os.ModeSocket != 0 && ok && int(sys.Uid) == os.Getuid()
}

// urlNetloc is urllib.parse.urlparse(url).netloc: the part after "//"
// up to the first "/", "?" or "#", once a scheme is split off.
func urlNetloc(u string) string {
	rest := u
	if i := strings.IndexByte(u, ':'); i > 0 {
		scheme := u[:i]
		ok := isASCIILetter(scheme[0])
		for j := 1; j < len(scheme) && ok; j++ {
			c := scheme[j]
			ok = isASCIILetter(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
		}
		if ok {
			rest = u[i+1:]
		}
	}
	if !strings.HasPrefix(rest, "//") {
		return ""
	}
	rest = rest[2:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// wiringPre is what the map reads before its first write, so that an
// input this binary refuses changes nothing.
type wiringPre struct {
	homeCfg, dataCfg config.Config
	dataErr          error
	hooks            hookSettings
	roots            []transcriptRoot
}

// wiringRead reads, in the oracle's order, everything detect_stages
// reads before gather_status reads the stores. A refusal here changes
// nothing; an error the oracle raises is returned as the command's end.
func (e *Env) wiringRead(cmd, dataDir string) (*wiringPre, error) {
	pre := &wiringPre{}
	// gather_status first asks the matcher's threshold, which loads the
	// config in the default data directory.
	hcfg, herr := config.Load(gateroot.Join(e.home, ".opendaisugi/config.yaml"))
	if herr != nil {
		return nil, e.configLoadErr(cmd, herr)
	}
	pre.homeCfg = hcfg
	if _, ok := matcherThresholds[hcfg.MatcherModel]; !ok {
		e.errf("matcher_model=%s is not a built embedder.\n", pystr.Repr(hcfg.MatcherModel))
		return nil, exit(1)
	}
	pre.dataCfg, pre.dataErr = config.Load(gateroot.Join(dataDir, "config.yaml"))
	if pre.dataErr != nil && !errors.Is(pre.dataErr, config.ErrInvalid) {
		var attr *config.AttributeError
		if !errors.As(pre.dataErr, &attr) {
			return nil, e.refuse(cmd, fmt.Errorf("the config file is not one this binary reads: %w", pre.dataErr))
		}
	}
	h, err := readHookSettings(gateroot.Join(e.home, ".claude/settings.json"))
	if err != nil {
		return nil, e.refuse(cmd, err)
	}
	pre.hooks = h
	roots, err := e.transcriptRoots()
	if err != nil {
		return nil, e.refuse(cmd, err)
	}
	pre.roots = roots
	return pre, nil
}

// storeCounts is the part of gather_status the map shows: the traces in
// the journal and the pathways in the store, zero when either is missing
// or cannot be read. Both are read read-only.
func (e *Env) storeCounts(dataDir string) (traces, pws int64) {
	db := filepath.Join(dataDir, "pathways.db")
	if _, err := os.Stat(db); err == nil {
		// int(count) is kept when int(hits) raises, as in Python.
		if c, _, ok := readPathwayStats(db); ok {
			pws = c
		}
	}
	rep := &status{dataDir: dataDir}
	e.statusJournal(rep)
	return rep.total, pws
}

// detectStages is modules.detect_stages: the wiring snapshot for dataDir.
func (e *Env) detectStages(cmd, dataDir string) ([]wStage, error) {
	pre, err := e.wiringRead(cmd, dataDir)
	if err != nil {
		return nil, err
	}
	journalTotal, pathwayCount := e.storeCounts(dataDir)
	if pre.dataErr != nil {
		return nil, e.configLoadErr(cmd, pre.dataErr)
	}
	cfg := pre.dataCfg
	which := func(name string) bool {
		_, err := install.LookPath(e.env)(name)
		return err == nil
	}
	present := map[string]bool{}
	for _, r := range pre.roots {
		if exists(r.root) {
			present[r.harness] = true
		}
	}
	piInstalled := exists(gateroot.Join(e.home, ".pi/agent/extensions/daisugi-gate/index.ts"))
	piPresent := isDir(gateroot.Join(e.home, ".pi"))
	ocPath := e.opencodePluginPath()
	ocInstalled := exists(ocPath)
	ocPresent := which("opencode") || isDir(gateroot.Parent(gateroot.Parent(ocPath)))
	harness := func(name, hid string) wModule {
		if present[hid] {
			return wModule{name, modActive, "transcripts found"}
		}
		return wModule{name, modPossible, "not detected on this box"}
	}
	backend := pystr.Strip(e.env["OPENDAISUGI_LLM_BACKEND"])
	gatewayOn := e.env["OPENDAISUGI_GATEWAY_BASE_URL"] != "" || pre.hooks.installed("gateway", "PreToolUse")
	matcherSel := cfg.MatcherModel
	switchyardRow := switchyardModule(cfg.GatewayRouter, which("switchyard-server"))
	herdrStopOn := pre.hooks.installed("--event stop", "Stop")
	herdrNotifOn := pre.hooks.installed("--event notification", "Notification")
	herdrHooksOn := herdrStopOn && herdrNotifOn
	herdrHooksPartial := herdrStopOn != herdrNotifOn
	herdrBinary := which("herdr")
	coppiceSocket := e.coppiceSocketPresent()
	coppiceOnPath := which("coppice")
	tmuxOnPath := which("tmux")
	var herdrState, herdrNote string
	switch {
	case herdrHooksOn:
		herdrState, herdrNote = modActive, "hooks installed; Herdr CLI contract unverified"
	case herdrHooksPartial:
		herdrState, herdrNote = modAvailable, "one of two hooks installed; run `daisugi install --gate --report herdr`"
	case herdrBinary:
		herdrState, herdrNote = modAvailable, "run `daisugi install --gate --report herdr`"
	default:
		herdrState, herdrNote = modPossible, "install Herdr first"
	}
	// effective_matcher: the package-absence fallback lands on lexical
	// for MiniLM and int8 in the oracle; this binary does not fall back
	// (K4-2), so lexical runs only when it is chosen.
	lexicalEffective := matcherSel == "lexical"
	embState := func(key string, installed bool) string {
		if matcherSel == key && installed {
			return modActive
		}
		if installed {
			return modAvailable
		}
		return modPossible
	}
	lexicalState := modAvailable
	if lexicalEffective {
		lexicalState = modActive
	}
	const lexicalDesc = "keyword floor — no model, no download"
	named := backend
	if named == "" && cfg.LLMBackend != nil {
		named = pystr.Strip(*cfg.LLMBackend)
	}
	autoPick := "api"
	if e.env["ANTHROPIC_API_KEY"] == "" && e.env["ANTHROPIC_AUTH_TOKEN"] == "" && which("claude") {
		autoPick = "claude-code"
	}
	pick := func(on bool) string {
		if on {
			return modActive
		}
		return modAvailable
	}
	auto := wModule{"auto", modActive, "pick what runs here: " + autoPick}
	if named != "" {
		auto = wModule{"auto", modAvailable, "pick what runs here"}
	}
	backendModules := []wModule{
		auto,
		{"claude-code", pick(named == "claude-code"), "no API key"},
		{"anthropic-api", pick(named == "api" || named == "anthropic"), "BYOK"},
		{"llamafile / local", pick(named == "llamafile"), "base_url, offline"},
		{"ollama", pick(named == "ollama"), "if running"},
	}
	if cfg.LLMBaseURL != nil && *cfg.LLMBaseURL != "" {
		u := *cfg.LLMBaseURL
		netloc := urlNetloc(u)
		if netloc == "" {
			netloc = u
		}
		model := "model unset"
		if cfg.LLMHostModel != nil && *cfg.LLMHostModel != "" {
			model = *cfg.LLMHostModel
		}
		ctx := "context unknown"
		if w := cfg.LLMContextWindow; w != nil && w.Sign() != 0 {
			ctx = new(big.Int).Div(w, big.NewInt(1024)).String() + "k"
		}
		kind := "None"
		if cfg.LLMHostKind != nil {
			kind = *cfg.LLMHostKind
		}
		backendModules = append(backendModules, wModule{
			fmt.Sprintf("%s (%s, %s, %s)", netloc, kind, model, ctx), modAvailable,
			"recorded. Export the env lines from `tiers setup --remote` to use it"})
	}
	voiceState := func(key string, installed bool) string {
		if cfg.VoiceEngine == key && installed {
			return modActive
		}
		if installed {
			return modAvailable
		}
		return modPossible
	}
	gateHook := pre.hooks.installed("daisugi hook", "PreToolUse")
	cond := func(b bool, yes, no string) string {
		if b {
			return yes
		}
		return no
	}
	piState := modPossible
	piNote := "not detected on this box"
	switch {
	case piInstalled:
		piState, piNote = modActive, "gate extension installed"
	case piPresent:
		piState, piNote = modAvailable, "pi detected. Run `daisugi install --harness pi`"
	}
	ocState := modPossible
	ocNote := "not detected on this box"
	switch {
	case ocInstalled:
		ocState, ocNote = modActive, "gate plugin installed: in-process deny-only hook; fail-closed"
	case ocPresent:
		ocState, ocNote = modAvailable, "opencode detected. Run `daisugi install --harness opencode`"
	}
	envelopeLLM := wModule{"llm-generated", modPossible, "set OPENDAISUGI_LLM_BACKEND"}
	if backend != "" {
		envelopeLLM = wModule{"llm-generated", modAvailable, "backend=" + backend}
	}
	coppiceState, coppiceNote := modPossible, "build it in harness/coppice"
	switch {
	case coppiceSocket:
		coppiceState, coppiceNote = modActive, "server running"
	case coppiceOnPath:
		coppiceState, coppiceNote = modAvailable, "built, run `coppice server start`"
	}
	return []wStage{
		{"harness", "harness (agent host)", "the agent whose tool-calls we sit in front of", []wModule{
			harness("claude-code", "claude-code"),
			harness("codex", "codex"),
			{"pi", piState, piNote},
			{"opencode", ocState, ocNote},
			{"hermes", modPossible, "adapter designed, not built"},
			{"openclaw", modPossible, "adapter designed, not built"},
			{"<any via AGENTS.md>", modPossible, "vendor-neutral hook contract"},
		}},
		{"gate", "gate (call-time)", "checks each proposed action BEFORE it runs; fail-closed", []wModule{
			{"claude PreToolUse", cond(gateHook, modActive, modPossible), cond(gateHook, "installed", "run `daisugi install`")},
			{"codex hooks.json", modAvailable, "fail-open class — soft gate"},
			{"pi tool_call (in-process)", cond(piInstalled, modActive, modPossible),
				cond(piInstalled, "native block, no exit-2 convention, no fail-open outer timeout",
					"run `daisugi install --harness pi`")},
			{"opencode tool.execute.before, in-process", cond(ocInstalled, modActive, modPossible),
				cond(ocInstalled, opencodeGateNote, "run `daisugi install --harness opencode`")},
			{"audit / off", modAvailable, "config: gate mode"},
		}},
		{"verifier", "verifier (the checker)", "proves the action stays inside the envelope (SMT-backed)", []wModule{
			{"python (in-process)", modActive, "the oracle; always runs"},
			// No checkout: no compiled client is built (K4-1).
			{"rust", modPossible, "cd clients/rust && cargo build --release"},
			{"go (mvdan)", modPossible, "cd clients/go && scripts/native.sh && scripts/build.sh"},
			{"typescript", modPossible, "cd clients/ts && npm install && npm run build"},
			{"lean (proven core)", modPossible, "cd clients/lean && lake build"},
		}},
		{"shell", "shell decomposition", "splits `a && b` into heads so each is checked (ADR-0010/14)", []wModule{
			// The bash grammar is linked into this binary.
			{"tree-sitter-bash", modActive, "installed"},
			{"reject-compound", modAvailable, "the fail-closed default when off"},
		}},
		{"envelope", "envelope source", "where the 'what is allowed' spec comes from", []wModule{
			{"evidence-inferred", modActive, "ADR-0016 — from observed steps, zero-LLM"},
			envelopeLLM,
		}},
		{"backend", "model backend", "the LLM for envelope generation / planning (Tier-1)", backendModules},
		{"matcher", "matcher / embedder (pathway reuse)", "finds a stored pathway to reuse instead of re-planning", []wModule{
			{"all-MiniLM-L6-v2", embState("all-MiniLM-L6-v2", carriesMiniLM), "needs opendaisugi[search]"},
			{"potion static", embState("potion", carriesPotion), "torch-free, numpy-only, ~30MB, offline"},
			{"int8 / fp16 onnx", embState("int8", carriesInt8), int8Reason},
			{"lexical / intent", lexicalState, lexicalDesc},
		}},
		{"router", "token router / gateway", "routes turns to cheap vs frontier models to save tokens", []wModule{
			{"daisugi gateway", cond(gatewayOn, modActive, modPossible), cond(gatewayOn, "base_url wired", "daisugi install --gateway")},
			switchyardRow,
			{"off (direct)", modAvailable, "default; start routing with `daisugi gateway`"},
		}},
		{"distill", "distillation (the gardener)", "clusters verified traces into reusable pathways (`tend`)", []wModule{
			{"sentence-transformers", embState("all-MiniLM-L6-v2", carriesMiniLM), "needs [search]"},
			{"potion (torch-free)", embState("potion", carriesPotion), "numpy-only clustering"},
			{"int8 (onnx, no torch)", embState("int8", carriesInt8), int8Reason},
			{"lexical (no model)", lexicalState, lexicalDesc},
			{"no-embedder", modAvailable, "journal only, 0 pathways (graceful)"},
		}},
		{"stores", "stores (local-first)", "where trust and pathways live — on your disk", []wModule{
			{"journal (sqlite)", modActive, fmt.Sprintf("%d traces", journalTotal)},
			{"pathway store (sqlite)", modActive, fmt.Sprintf("%d pathways", pathwayCount)},
			{"git-backed store", modAvailable, "shareable pathway registry"},
		}},
		{"floor_report", "floor report (state → a pane host)", "tells the pane host: idle, working, blocked, done", []wModule{
			{"herdr", herdrState, herdrNote},
			{"coppice", modPossible, "the report path is not wired yet"},
			{"none", cond(!herdrHooksOn, modActive, modAvailable), "no floor listening"},
		}},
		{"floor_backend", "pane backend", "drives the harness inside a real pane: coppice, herdr, or tmux", []wModule{
			{"coppice", coppiceState, coppiceNote},
			{"herdr", cond(herdrBinary, modAvailable, modPossible), cond(herdrBinary, "installed", "install herdr from herdr.dev")},
			{"tmux", cond(tmuxOnPath, modAvailable, modPossible), cond(tmuxOnPath, "installed", "install tmux 3.2 or newer")},
		}},
		{"voice_engine", "voice engine (speech to text)", "turns a recorded clip into text for the voice bridge", []wModule{
			{"faster-whisper", voiceState("faster-whisper", false), "needs opendaisugi[voice]"},
			{"parakeet", modPossible, "needs opendaisugi[voice-parakeet]"},
		}},
	}, nil
}

const opencodeGateNote = "in-process deny-only hook; fail-closed on every call. apply_patch is " +
	"checked path by path. Writes to OpenCode's config, to .opencode plugin and tool " +
	"directories and to any opencode.json are hard-denied. Every ask is permanent: the gate has no fixed " +
	"project root for OpenCode. A coppice pane will not start without the " +
	"plugin or with --pure. Outside coppice, OpenCode runs ungated if the " +
	"plugin fails to load or with --pure. Plugins under OPENCODE_CONFIG_DIR " +
	"are not guarded"

// switchyardModule is modules._switchyard_module.
func switchyardModule(router string, present bool) wModule {
	selected := router == "switchyard"
	switch {
	case selected && present:
		return wModule{"NeMo Switchyard", modActive, "selected; `daisugi gateway` starts it. The gateway row says " +
			"if turns flow. See `daisugi router status`"}
	case selected:
		return wModule{"NeMo Switchyard", modPossible, "selected, but the binary is missing: " + switchyard.InstallCmd}
	case present:
		return wModule{"NeMo Switchyard", modAvailable,
			"binary found: daisugi install --gateway --router switchyard --efficient-model <id>"}
	}
	return wModule{"NeMo Switchyard", modPossible, "needs the binary: " + switchyard.InstallCmd}
}

// pyLen is len() of a str; pySlice is s[:n] by code point; pyLjust is
// s.ljust(n).
func pyCut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func pyLjust(s string, n int) string {
	if l := pystr.Len(s); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}

// boxLine is `f"{v} {text[:inner].ljust(inner)} {v}"`.
func boxLine(g glyphSet, text string, inner int) string {
	return g.v + " " + pyLjust(pyCut(text, inner), inner) + " " + g.v
}

// moduleRows is the module row(s) of a stage, wrapped at inner.
func moduleRows(g glyphSet, st wStage, inner int) []string {
	var out []string
	line := "  "
	for _, m := range st.modules {
		chunk := g.state(m.state) + " " + m.name + "   "
		if pystr.Len(line)+pystr.Len(chunk) > inner {
			out = append(out, boxLine(g, line, inner))
			line = "  "
		}
		line += chunk
	}
	if pystr.Strip(line) != "" {
		out = append(out, boxLine(g, line, inner))
	}
	return out
}

const wiringWidth = 66

// renderWiring is modules.render_wiring.
func renderWiring(dataDir string, stages []wStage, g glyphSet) string {
	width := wiringWidth
	inner := width - 4
	out := []string{"openDaisugi — module wiring   (data dir: " + dataDir + ")", "", "  a task from your agent"}
	for _, st := range stages {
		out = append(out, "        "+g.v, "        "+g.down)
		tag := ""
		if eff := stageEffect[st.key]; eff != "" {
			tag = " [" + eff + "] "
		}
		head := g.tl + g.h + " " + st.title + " "
		head = head + strings.Repeat(g.h, max(0, width-pystr.Len(head)-pystr.Len(tag)-1)) + tag + g.tr
		out = append(out, head, boxLine(g, st.role, inner))
		out = append(out, moduleRows(g, st, inner)...)
		out = append(out, g.bl+strings.Repeat(g.h, width-2)+g.br)
	}
	out = append(out, "        "+g.v, "        "+g.down, "  verified action runs  (or falls back / is refused)", "")
	out = append(out, fmt.Sprintf("legend:  %s active   %s available (swap in)   %s possible / planned", g.on, g.avail, g.off))
	out = append(out, "         [live]    = takes effect now")
	out = append(out, "         [cfg]     = a real choice, but needs a restart/reinstall")
	out = append(out, "         [planned] = you can record it, but nothing reads it yet")
	n := map[string]int{}
	for _, st := range stages {
		n[stageEffect[st.key]]++
	}
	out = append(out, fmt.Sprintf("         %d live · %d need a restart/reinstall · %d planned (not wired yet).",
		n["live"], n["cfg"], n["planned"]))
	return strings.Join(out, "\n")
}

// stageObject is dataclasses.asdict(stage).
func stageObject(st wStage) *pyjson.Object {
	mods := make([]*pyjson.Object, 0, len(st.modules))
	for _, m := range st.modules {
		mods = append(mods, pyjson.NewObject().Set("name", m.name).Set("state", m.state).Set("note", m.note))
	}
	return pyjson.NewObject().Set("key", st.key).Set("title", st.title).Set("role", st.role).Set("modules", mods)
}

// wiringJSON is modules.wiring_json.
func wiringJSON(stages []wStage) string {
	out := make([]*pyjson.Object, 0, len(stages))
	for _, st := range stages {
		out = append(out, stageObject(st))
	}
	return pyjson.DumpsIndent(out, 2, true)
}

// modulesCmd is `daisugi modules`.
func (e *Env) modulesCmd(args []string) error {
	const cmd = "modules"
	opts := []opt{dataDirOpt, jsonOpt}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Show the module wiring: what's active, available to swap in, or an open slot.", opts)
	}
	dataDir := e.dataDir(p)
	stages, err := e.detectStages(cmd, dataDir)
	if err != nil {
		return err
	}
	if p.flag("--json") {
		e.out("%s\n", wiringJSON(stages))
		return nil
	}
	e.out("%s\n", renderWiring(dataDir, stages, e.glyphs()))
	return nil
}
