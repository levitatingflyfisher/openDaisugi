package install

import (
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
	"daisugi-verify/internal/verify"
	"daisugi-verify/internal/wardenroot"
)

// This file is install's four default layers (skill, MCP, capture and
// instructions) and their reverse, as install.py writes them, and the
// plan of every layer in Python's apply order.

//go:embed assets/skill assets/openclaw_plugin
var layerAssets embed.FS

// SkillName is install._SKILL_NAME.
const SkillName = "opendaisugi-checklist"

// Layer names as the plan prints them.
const (
	LayerSkill        = "skill"
	LayerMCP          = "mcp"
	LayerCapture      = "capture"
	LayerInstructions = "instructions"
)

// mdMarker is install._CLAUDE_MD_MARKER.
const mdMarker = "<!-- opendaisugi-managed -->"

// mdBlock is install._CLAUDE_MD_BLOCK.
const mdBlock = mdMarker + `
## openDaisugi — automatic pathway routing

Before planning any task with 3 or more steps, call the ` + "`find_pathway`" + `
MCP tool first. If similarity ≥ 0.85, use the returned cached plan via
` + "`run_plan`" + ` instead of re-planning from scratch. When a cached pathway is
used, say so: "Using cached opendaisugi pathway (similarity X.XX) —
skipping re-plan."

A job you will repeat goes through ` + "`verify_plan`" + `, then ` + "`run_plan`" + `. Make
one action in each tool call.

If no pathway matches, proceed normally.
` + mdMarker + "\n"

// codexMCPRe is install._CODEX_MCP_RE: the block an install wrote, with any
// command (a bare daisugi from an older install, or an absolute path).
var codexMCPRe = regexp.MustCompile(`\n\[mcp_servers\.opendaisugi\]\ncommand = "((?:[^"\\\n]|\\.)*)"\nargs = \["mcp", "serve"\]\n`)

// codexMCPBlock is install._codex_mcp_block.
func codexMCPBlock(self string) string {
	return "\n[mcp_servers.opendaisugi]\ncommand = " + pyjson.Dumps(self, true) + "\nargs = [\"mcp\", \"serve\"]\n"
}

// recordCommand is install._record_command: `<self> hook record <args>`.
func recordCommand(self, args string) string {
	return verify.ShlexQuote(self) + " hook record " + args
}

// leftAsIs is install.LEFT_AS_IS: a note that says a hook was not rewritten
// contains this phrase.
const leftAsIs = "was left as it is"

// LeftAsIs is whether a note says a hook was left as it is.
func LeftAsIs(note string) bool { return strings.Contains(note, leftAsIs) }

// plainRecord is install._PLAIN_RECORD: one plain command, a program word
// (bare, or one single-quoted path) and plain words. No operator,
// redirection, quote, expansion, newline or NAME=val prefix can match.
var plainRecord = regexp.MustCompile("^([ \\t]*)('[^']*'|[^ \\t\\n\\r\\f\\v'\"\\\\;|&<>()$`]+)" +
	"((?:[ \\t]+[A-Za-z0-9_./:=@%+,-]+)*[ \\t]*)$")

// rebindRecordCommand is install._rebind_record_command: a daisugi hook
// record command pointed at self, and whether it had to be left alone. Only
// one plain command is rewritten, and only its program word; every other
// byte stays. A command with an operator, a redirection, an env or NAME=val
// prefix, a quote or an expansion comes back as it was with left true,
// unless its program is already self. Any other command comes back as it
// was with left false.
func rebindRecordCommand(command, self string) (out string, left bool) {
	programs := config.RecordHookPrograms(command)
	if len(programs) == 0 {
		return command, false
	}
	if m := plainRecord.FindStringSubmatch(command); m != nil {
		word := m[2]
		if strings.HasPrefix(word, "'") {
			word = word[1 : len(word)-1]
		}
		tail := strings.Fields(m[3])
		if config.IsEntryWord(word) && len(tail) >= 2 && tail[0] == "hook" && tail[1] == "record" {
			if word == self {
				return command, false
			}
			if strings.Contains(word, "/") && !strings.Contains(self, "/") {
				// Install found no path to write. A bare word is less than
				// the path the hook has now: keep the path and say so.
				return command, true
			}
			return m[1] + verify.ShlexQuote(self) + m[3], false
		}
	}
	for _, p := range programs {
		if p != self {
			return command, true
		}
	}
	return command, false
}

// keepsPath is install._keeps_path: install would write a bare word over a
// command that names a path. It never does.
func keepsPath(old any, want any) bool {
	o, ok1 := old.(string)
	w, ok2 := want.(string)
	return ok1 && ok2 && strings.Contains(o, "/") && !strings.Contains(w, "/")
}

// leftMCPNote is install._left_mcp_note.
func leftMCPNote(p, old, want string) string {
	return "The MCP command `" + old + "` in " + p + " " + leftAsIs + ": install found no daisugi on PATH, " +
		"so the only word it could write is `" + want + "`, and the entry already names a path."
}

// leftHookNote is install._left_hook_note.
func leftHookNote(runtime, command, self string) string {
	if m := plainRecord.FindStringSubmatch(command); m != nil && strings.Contains(strings.Trim(m[2], "'"), "/") && !strings.Contains(self, "/") {
		return "[" + runtime + "] The hook `" + command + "` " + leftAsIs + ": install found no daisugi on PATH, " +
			"so the only word it could write is `" + self + "`, and the hook already names a path."
	}
	return "[" + runtime + "] The hook `" + command + "` " + leftAsIs + ": it is not one plain command, so " +
		"install did not rewrite it. If the agent cannot find daisugi, make its first word " +
		verify.ShlexQuote(self) + "."
}

// rebindHookEntries is install._rebind_hook_entries: every record hook in a
// list of {"hooks": [...]} entries (or flat {"command": ...} entries)
// pointed at self. It says whether one changed, and gives a note for each
// hook it left as it is.
func rebindHookEntries(entries []any, self, runtime string) (changed bool, notes []string) {
	for _, e := range entries {
		eo, ok := e.(*pyjson.Object)
		if !ok {
			continue
		}
		var holders []any
		if _, flat := eo.Get("command"); flat {
			holders = []any{eo}
		} else {
			holders = asList(eo.Value("hooks"))
		}
		for _, h := range holders {
			ho, ok := h.(*pyjson.Object)
			if !ok {
				continue
			}
			cmd, isStr := ho.Value("command").(string)
			if isStr && config.IsRecordHook(cmd, "") {
				n, left := rebindRecordCommand(cmd, self)
				if left {
					notes = append(notes, leftHookNote(runtime, cmd, self))
				}
				if n != cmd {
					ho.Set("command", n)
					changed = true
				}
			}
		}
	}
	return changed, notes
}

// withNotes puts the notes of left hooks on the edit of a file, or on an
// edit that only carries them when nothing is written.
func withNotes(e *Edit, p string, notes []string) *Edit {
	if len(notes) == 0 {
		return e
	}
	text := strings.Join(notes, "\n")
	if e == nil {
		return &Edit{Path: p, NoteOnly: true, Note: text}
	}
	if e.Note != "" {
		text = e.Note + "\n" + text
	}
	e.Note = text
	return e
}

// AssetFile is one file of an embedded tree.
type AssetFile struct{ Rel, Text string }

// SkillFiles is every file under an embedded directory, by its path below
// it, in name order.
func SkillFiles(dir string) []AssetFile {
	var out []AssetFile
	_ = fs.WalkDir(layerAssets, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := layerAssets.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, AssetFile{Rel: strings.TrimPrefix(p, dir+"/"), Text: string(b)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// SkillText is the SKILL.md install --print-skill prints.
func SkillText() string {
	b, _ := layerAssets.ReadFile("assets/skill/" + SkillName + "/SKILL.md")
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// AgentsSkillTarget is install._agents_skill_target.
func AgentsSkillTarget(home, fallback string) string {
	agents := wardenroot.Join(home, ".agents/skills")
	if exists(agents) || !exists(wardenroot.Join(home, fallback)) {
		return wardenroot.Join(agents, SkillName)
	}
	return wardenroot.Join(wardenroot.Join(home, fallback), SkillName)
}

// Opts are the layers an install writes beyond the four default ones.
type Opts struct {
	Router, Enforce, Ask bool
	URL                  string
	// Report is the floor report the warden layer wires ("", "herdr" or
	// "coppice").
	Report string
	// Only names the layers to write (--only); nil means the default five.
	// The router layer is Router, not an entry here.
	Only map[string]bool
}

// Has is whether the layer is selected.
func (o Opts) Has(layer string) bool { return o.Only == nil || o.Only[layer] }

// skillTarget is where a runtime's skill goes.
func skillTarget(rt Runtime, home string) string {
	switch rt.Key {
	case "claude":
		// Claude Code reads only ~/.claude/skills: a skill in
		// ~/.agents/skills is not in its skills list (checked live on
		// 2.1.319). Codex reads ~/.agents/skills.
		return wardenroot.Join(home, ".claude/skills/"+SkillName)
	case "codex":
		return AgentsSkillTarget(home, ".codex/skills")
	case "hermes":
		return wardenroot.Join(home, ".hermes/skills/opendaisugi/"+SkillName)
	}
	return wardenroot.Join(home, ".openclaw/workspace/skills/"+SkillName)
}

// PlanAllSteps is Runtime.plan for the default layers and the opt-in ones:
// the supported steps in layer order, then the gap notes.
func PlanAllSteps(rt Runtime, home string, o Opts) []Step {
	j := func(rel string) string { return wardenroot.Join(home, rel) }
	var steps []Step
	add := func(layer, desc, target string) {
		steps = append(steps, Step{Layer: layer, Description: desc, Target: target, Supported: true})
	}
	// layer adds a step only for a selected layer.
	layer := func(l, desc, target string) {
		if o.Has(l) {
			add(l, desc, target)
		}
	}
	layer(LayerSkill, "Copy opendaisugi-checklist skill", skillTarget(rt, home))
	switch rt.Key {
	case "claude":
		layer(LayerMCP, `Register MCP server "opendaisugi"`, j(".claude.json"))
		layer(LayerCapture, "Add PreToolUse capture hook", j(".claude/settings.json"))
		layer(LayerInstructions, pathwayGuidanceTitle(j(".claude/CLAUDE.md")), j(".claude/CLAUDE.md"))
	case "codex":
		layer(LayerMCP, "Register opendaisugi MCP server", j(".codex/config.toml"))
		layer(LayerInstructions, pathwayGuidanceTitle(j(".codex/AGENTS.md")), j(".codex/AGENTS.md"))
	case "hermes":
		layer(LayerMCP, "Register opendaisugi MCP server", j(".hermes/config.yaml"))
		layer(LayerCapture, "Add pre_tool_call capture hook", j(".hermes/config.yaml"))
		layer(LayerInstructions, pathwayGuidanceTitle(j(".hermes/SOUL.md")), j(".hermes/SOUL.md"))
	case "openclaw":
		layer(LayerMCP, "Register opendaisugi MCP server", j(".openclaw/openclaw.json"))
		layer(LayerCapture, "Install before_tool_call capture plugin", j(".openclaw/extensions/opendaisugi"))
		layer(LayerInstructions, pathwayGuidanceTitle(j(".openclaw/workspace/AGENTS.md")), j(".openclaw/workspace/AGENTS.md"))
	}
	rest := PlanSteps(rt, home, o.Router, o.Enforce, o.Ask, o.URL)
	var gaps []Step
	for _, s := range rest {
		if s.Layer == LayerWarden && !o.Has(LayerWarden) {
			continue
		}
		if !s.Supported {
			gaps = append(gaps, s)
			continue
		}
		steps = append(steps, s)
		if s.Layer == LayerWarden && rt.Key == "claude" {
			if o.Report == "herdr" {
				add(LayerWarden, "Add Stop + Notification floor-report hooks (Herdr)", j(".claude/settings.json"))
			}
			if o.Report == "herdr" || o.Report == "coppice" {
				add(LayerWarden, "Add SubagentStart + SubagentStop hooks (subagent rows)", j(".claude/settings.json"))
			}
		}
	}
	// The pando layer comes after the warden and the router, as it is
	// written last (the Python oracle's order).
	if o.hasPando() {
		steps = append(steps, pandoPlanSteps(rt, home)...)
	}
	return append(steps, gaps...)
}

// priorOf is the last edit of p in the change, whose content later edits
// of p are planned on.
func priorOf(ch *Change, p string) *Edit {
	var prior *Edit
	for _, e := range ch.Edits {
		if e != nil && e.Path == p && e.Kind == "" && e.Warning == "" && !e.NoteOnly {
			prior = e
		}
	}
	return prior
}

// textOf is path.read_text() after the earlier edits of this run.
func textOf(p string, prior *Edit) (string, bool, error) {
	if prior != nil {
		return prior.Content, true, nil
	}
	return readText(p)
}

// PlanSkill is _link_skill as this binary does it: the skill's files,
// copied from the binary (there is no package directory to link to).
func PlanSkill(target string) *Edit {
	return &Edit{Path: target, Kind: "skill", Listed: true}
}

// PlanMCP is _patch_mcp: entry set at key path in a JSON (or JSON5) file
// unless one is there, the file left alone when it does not parse.
func PlanMCP(p string, json5 bool, keys []string, entry *pyjson.Object, what string, prior *Edit) (*Edit, error) {
	return planMCPNamed("opendaisugi", p, json5, keys, entry, what, prior)
}

// planMCPNamed is PlanMCP for the server called name.
func planMCPNamed(name, p string, json5 bool, keys []string, entry *pyjson.Object, what string, prior *Edit) (*Edit, error) {
	text, existed, err := textOf(p, prior)
	if err != nil {
		return nil, err
	}
	var cfg any = pyjson.NewObject()
	if existed {
		v, lerr := pyjson.Loads(text)
		if lerr != nil && json5 && !errors.Is(lerr, pyjson.ErrUnsupported) {
			v, lerr = pyjson.Loads(stripJSON5(text))
		}
		if lerr != nil {
			if errors.Is(lerr, pyjson.ErrUnsupported) {
				return nil, ErrUnsupported
			}
			return &Edit{Path: p, Warning: p + " is not valid; skipping " + what + " to avoid overwriting user state. " +
				"Fix the file and re-run `daisugi install`."}, nil
		}
		cfg = v
	}
	leaf := cfg
	for _, k := range keys {
		o, ok := leaf.(*pyjson.Object)
		if !ok {
			return nil, failf("'%s' object has no attribute 'setdefault'", pyType(leaf))
		}
		next, has := o.Get(k)
		if !has {
			next = pyjson.NewObject()
			o.Set(k, next)
		}
		leaf = next
	}
	lo, ok := leaf.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported // `in` and item assignment on another type
	}
	if old, has := lo.Get(name); has {
		// An entry from an older install names a bare daisugi. Point it at
		// the absolute program; every other key of it stays as it is.
		want := entry.Value("command")
		oo, isObj := old.(*pyjson.Object)
		if !isObj || !config.IsEntryWord(oo.Value("command")) || oo.Value("command") == want {
			return nil, nil
		}
		if keepsPath(oo.Value("command"), want) {
			return withNotes(nil, p, []string{leftMCPNote(p, oo.Value("command").(string), want.(string))}), nil
		}
		oo.Set("command", want)
	} else {
		lo.Set(name, entry)
	}
	e := &Edit{Path: p, Existed: existed}
	if existed && json5 && json5Comment.MatchString(text) {
		e.PreWarning = p + " contains JSON5 comments which will not survive the rewrite (the writer emits plain " +
			"JSON). The pre-write backup at " + p + ".bak* preserves the original text — restore from it if you " +
			"need the comments back. Tracked as M7 in REVIEW_FINDINGS.md."
	}
	e.Content = dumpSettings(cfg)
	return e, nil
}

func mcpEntry(withType bool, self string) *pyjson.Object {
	o := pyjson.NewObject()
	if withType {
		o.Set("type", "stdio")
	}
	return o.Set("command", self).Set("args", []any{"mcp", "serve"})
}

// settingsOf reads a Claude settings.json for a capture or report edit:
// nil and a warning edit when it does not parse.
func settingsOf(p string, prior *Edit, warning string) (*pyjson.Object, bool, *Edit, error) {
	v, st, err := jsonOf(p, prior)
	if err != nil {
		return nil, false, nil, err
	}
	if st == readBad {
		return nil, false, &Edit{Path: p, Warning: warning}, nil
	}
	existed := st != readMissing
	if !existed {
		v = pyjson.NewObject()
	}
	s, ok := v.(*pyjson.Object)
	if !ok {
		return nil, false, nil, failf("'%s' object has no attribute 'setdefault'", pyType(v))
	}
	return s, existed, nil, nil
}

// eventList is hooks.setdefault(event, []) as a list.
func eventList(s *pyjson.Object, event string) (*pyjson.Object, []any, error) {
	hv, has := s.Get("hooks")
	if !has {
		hv = pyjson.NewObject()
		s.Set("hooks", hv)
	}
	hooks, ok := hv.(*pyjson.Object)
	if !ok {
		return nil, nil, failf("'%s' object has no attribute 'setdefault'", pyType(hv))
	}
	ev, has := hooks.Get(event)
	if !has {
		ev = []any{}
		hooks.Set(event, ev)
	}
	list, ok := ev.([]any)
	if !ok {
		return nil, nil, ErrUnsupported
	}
	return hooks, list, nil
}

// commandsOf is {h["command"] for e in entries for h in e.get("hooks", [])
// if h.get("type") == "command"}.
func commandsOf(entries []any) ([]any, error) {
	hs, err := commandHooks(entries)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, h := range hs {
		c, has := h.Get("command")
		if !has {
			return nil, failf("'command'") // KeyError
		}
		switch c.(type) {
		case []any, *pyjson.Object:
			return nil, failf("unhashable type: '%s'", pyType(c))
		}
		out = append(out, c)
	}
	return out, nil
}

// PlanClaudeCapture is _patch_claude_settings.
func PlanClaudeCapture(p string, prior *Edit, self string) (*Edit, error) {
	s, existed, warn, err := settingsOf(p, prior, p+" is not valid JSON; skipping hook registration to avoid "+
		"overwriting your Claude Code settings (permissions/env). Fix the file and re-run `daisugi install`.")
	if err != nil || warn != nil {
		return warn, err
	}
	hooks, pre, err := eventList(s, "PreToolUse")
	if err != nil {
		return nil, err
	}
	cmds, err := commandsOf(pre)
	if err != nil {
		return nil, err
	}
	changed := true
	var notes []string
	for _, c := range cmds {
		if config.IsRecordHook(c, "") {
			changed = false
		}
	}
	if changed {
		hooks.Set("PreToolUse", append(pre, pyjson.NewObject().
			Set("matcher", "Bash|Edit|Write|Read|Glob|Grep|WebFetch|WebSearch").
			Set("hooks", []any{pyjson.NewObject().Set("type", "command").Set("command", recordCommand(self, "--format claude"))})))
	} else {
		c, n := rebindHookEntries(pre, self, "Claude Code")
		changed = c || changed
		notes = append(notes, n...)
	}
	// The v0.27.1 print-skill SessionStart hook is taken out.
	if ss, ok := hooks.Value("SessionStart").([]any); ok && len(ss) > 0 {
		found := false
		for _, e := range ss {
			eo, ok := e.(*pyjson.Object)
			if !ok {
				return nil, ErrUnsupported
			}
			for _, h := range asList(eo.Value("hooks")) {
				if ho, ok := h.(*pyjson.Object); ok && ho.Value("command") == "daisugi install --print-skill" {
					found = true
				}
			}
		}
		if found {
			var kept []any
			for _, e := range ss {
				eo := e.(*pyjson.Object)
				var left []any
				for _, h := range asList(eo.Value("hooks")) {
					ho, ok := h.(*pyjson.Object)
					if !ok {
						return nil, ErrUnsupported
					}
					if ho.Value("command") != "daisugi install --print-skill" {
						left = append(left, h)
					}
				}
				if left == nil {
					left = []any{}
				}
				eo.Set("hooks", left)
				if len(left) > 0 {
					kept = append(kept, e)
				}
			}
			if len(kept) == 0 {
				hooks.Delete("SessionStart")
			} else {
				hooks.Set("SessionStart", kept)
			}
			changed = true
		}
	}
	if !changed {
		return withNotes(nil, p, notes), nil
	}
	return withNotes(&Edit{Path: p, Existed: existed, Content: dumpSettings(s)}, p, notes), nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// PlanReportHooks is _patch_claude_report_hooks (herdr: Stop and
// Notification) or _patch_claude_subagent_hooks.
func PlanReportHooks(p string, prior *Edit, subagent bool, self string) (*Edit, error) {
	what := "floor-report hook"
	if subagent {
		what = "subagent hook"
	}
	s, existed, warn, err := settingsOf(p, prior, p+" is not valid JSON; skipping "+what+" registration to avoid "+
		"overwriting your Claude Code settings. Fix the file and re-run `daisugi install`.")
	if err != nil || warn != nil {
		return warn, err
	}
	type ev struct{ name, flag string }
	evs := []ev{{"Stop", "--event stop"}, {"Notification", "--event notification"}}
	if subagent {
		evs = []ev{{"SubagentStart", "--event subagent_start"}, {"SubagentStop", "--event subagent_stop"}}
	}
	changed := false
	var notes []string
	for _, e := range evs {
		hooks, list, err := eventList(s, e.name)
		if err != nil {
			return nil, err
		}
		hs, err := commandHooks(list)
		if err != nil {
			return nil, err
		}
		word := e.flag[len("--event "):]
		have := false
		for _, h := range hs {
			if config.IsRecordHook(h.Value("command"), word) {
				have = true
			}
		}
		if !have {
			entry := pyjson.NewObject().Set("hooks", []any{pyjson.NewObject().Set("type", "command").
				Set("command", recordCommand(self, "--format claude "+e.flag))})
			hooks.Set(e.name, append(list, entry))
			changed = true
		} else {
			c, n := rebindHookEntries(list, self, "Claude Code")
			changed = c || changed
			notes = append(notes, n...)
		}
	}
	if !changed {
		return withNotes(nil, p, notes), nil
	}
	return withNotes(&Edit{Path: p, Existed: existed, Content: dumpSettings(s)}, p, notes), nil
}

// markerStarts are the places the marker stands alone on its line (LF or
// CRLF ends). A marker inside a line of prose is not one.
func markerStarts(text string) []int {
	var out []int
	from := 0
	for {
		i := strings.Index(text[from:], mdMarker)
		if i < 0 {
			return out
		}
		i += from
		after := i + len(mdMarker)
		atStart := i == 0 || text[i-1] == '\n'
		atEnd := after == len(text) || text[after] == '\n' || strings.HasPrefix(text[after:], "\r\n")
		if atStart && atEnd {
			out = append(out, i)
		}
		from = after
	}
}

// managedSpan is the [start, end) of the marker-bounded block in text, or
// ok false. The first two markers that stand alone on a line bound it; one
// alone is not a block.
func managedSpan(text string) (start, end int, ok bool) {
	st := markerStarts(text)
	if len(st) < 2 {
		return 0, 0, false
	}
	return st[0], st[1] + len(mdMarker), true
}

// blockFor is the current block with the line ends of the block it replaces.
func blockFor(spanText string) string {
	cur := strings.TrimRight(mdBlock, "\n")
	if strings.Contains(spanText, "\r\n") {
		return strings.ReplaceAll(cur, "\n", "\r\n")
	}
	return cur
}

// pathwayGuidanceTitle is the plan line for the instruction layer: what the
// install does to the file. Update when the file holds an older managed
// block, a keep line when the current block is there already, else Append.
// A file that cannot be read reads as no block.
func pathwayGuidanceTitle(p string) string {
	text, _, err := readText(p)
	if err == nil {
		if s, e, ok := managedSpan(text); ok {
			if text[s:e] != blockFor(text[s:e]) {
				return "Update pathway guidance"
			}
			return "Keep pathway guidance (already current)"
		}
	}
	return "Append pathway guidance"
}

// PlanInstructions is _patch_instructions: the managed block appended
// once; an older block is replaced in place and the rest of the file is
// kept byte for byte, line ends included. The block takes the line ends of
// the one it replaces. It makes the file's directory, and keeps no backup.
func PlanInstructions(p string) (*Edit, error) {
	text, _, err := readText(p)
	if err != nil {
		return nil, err
	}
	if s, e, ok := managedSpan(text); ok {
		cur := blockFor(text[s:e])
		if text[s:e] == cur {
			return nil, nil
		}
		return &Edit{Path: p, Kind: "md", Content: text[:s] + cur + text[e:]}, nil
	}
	if len(markerStarts(text)) > 0 {
		return nil, nil
	}
	sep := ""
	if text != "" {
		sep = "\n\n"
	}
	return &Edit{Path: p, Kind: "md", Content: strings.TrimRight(text, "\n") + sep + mdBlock}, nil
}

// PlanHermesSoul is _patch_hermes_soul: the managed block in Hermes's
// SOUL.md. A missing SOUL.md is left alone, with a warning, because a file
// made here would hold only the block and replace Hermes's built-in identity.
func PlanHermesSoul(p string) (*Edit, error) {
	if _, err := os.Stat(p); err != nil {
		return &Edit{Path: p, Warning: p + " does not exist yet; skipping the Hermes instructions. " +
			"Start Hermes once, then re-run `daisugi install`."}, nil
	}
	return PlanInstructions(p)
}

// PlanCodexMCP is _patch_codex_config.
func PlanCodexMCP(p, self string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, err
	}
	block := codexMCPBlock(self)
	if strings.Contains(text, "[mcp_servers.opendaisugi]") {
		// An older install wrote a bare daisugi. That block is replaced by
		// the absolute one; a block the user edited is left as it is.
		loc := codexMCPRe.FindStringSubmatchIndex(text)
		if loc == nil || text[loc[0]:loc[1]] == block {
			return nil, nil
		}
		old, lerr := pyjson.Loads(`"` + text[loc[2]:loc[3]] + `"`)
		if lerr != nil || !config.IsEntryWord(old) {
			return nil, nil
		}
		if keepsPath(old, self) {
			return withNotes(nil, p, []string{leftMCPNote(p, old.(string), self)}), nil
		}
		return &Edit{Path: p, Existed: existed, Content: text[:loc[0]] + block + text[loc[1]:]}, nil
	}
	sep := ""
	if text != "" {
		sep = "\n"
	}
	return &Edit{Path: p, Existed: existed, Content: strings.TrimRight(text, "\n") + sep + block}, nil
}

// yamlOf is yaml.safe_load(path.read_text()) or {} for a Hermes config: a
// warning edit when it does not parse.
func yamlOf(p string) (*pyjson.Object, bool, *pyjson.Object, error) {
	text, existed, err := readText(p)
	if err != nil || !existed {
		return nil, existed, nil, err
	}
	return yamlOfText(text)
}

// yamlOfPrior is yamlOf after the earlier edits of this run: the text of
// the last edit of p when there is one.
func yamlOfPrior(p string, prior *Edit) (*pyjson.Object, bool, *pyjson.Object, error) {
	if prior == nil {
		return yamlOf(p)
	}
	return yamlOfText(prior.Content)
}

func yamlOfText(text string) (*pyjson.Object, bool, *pyjson.Object, error) {
	// read_text reads with universal newlines.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	v, exc, why := pyyaml.Load(text)
	if why != nil || !pyyaml.Plain(v) {
		return nil, true, nil, ErrUnsupported
	}
	if exc != nil {
		if exc.Type == "ValueError" {
			// Not a yaml.YAMLError: the oracle's except does not catch it.
			return nil, true, nil, ErrUnsupported
		}
		return nil, true, nil, errYAML
	}
	if !pyjson.Truthy(v) {
		return pyjson.NewObject(), true, nil, nil
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, true, nil, ErrUnsupported
	}
	return o, true, o, nil
}

var errYAML = errors.New("not valid YAML")

func yamlText(o *pyjson.Object) (string, error) {
	t, why := pyyaml.SafeDump(o)
	if why != nil {
		return "", ErrUnsupported
	}
	return t, nil
}

// PlanHermesConfig is _patch_hermes_config.
func PlanHermesConfig(p string, withMCP, withCapture bool, self string) (*Edit, error) {
	cfg, existed, _, err := yamlOf(p)
	if errors.Is(err, errYAML) {
		return &Edit{Path: p, Warning: p + " is not valid YAML; skipping to avoid overwriting your Hermes config. " +
			"Fix the file and re-run `daisugi install`."}, nil
	}
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = pyjson.NewObject()
	}
	changed := false
	var notes []string
	if withMCP {
		mv, has := cfg.Get("mcp_servers")
		if !has {
			mv = pyjson.NewObject()
			cfg.Set("mcp_servers", mv)
		}
		mcp, ok := mv.(*pyjson.Object)
		if !ok {
			return nil, ErrUnsupported
		}
		if old, has := mcp.Get("opendaisugi"); !has {
			mcp.Set("opendaisugi", mcpEntry(false, self))
			changed = true
		} else if oo, isObj := old.(*pyjson.Object); isObj && config.IsEntryWord(oo.Value("command")) && oo.Value("command") != self {
			if keepsPath(oo.Value("command"), self) {
				notes = append(notes, leftMCPNote(p, oo.Value("command").(string), self))
			} else {
				oo.Set("command", self)
				changed = true
			}
		}
	}
	if withCapture {
		hooks, list, err := eventList(cfg, "pre_tool_call")
		if err != nil {
			return nil, err
		}
		cmd := recordCommand(self, "--format hermes")
		have := false
		known := false
		for _, h := range list {
			if ho, ok := h.(*pyjson.Object); ok {
				if ho.Value("command") == cmd {
					have = true
				}
				if config.IsRecordHook(ho.Value("command"), "") {
					known = true
				}
			}
		}
		if !have {
			if known {
				c, n := rebindHookEntries(list, self, "Hermes")
				changed = c || changed
				notes = append(notes, n...)
			} else {
				hooks.Set("pre_tool_call", append(list, pyjson.NewObject().Set("matcher", ".*").Set("command", cmd).
					Set("timeout", pyjson.Int{Text: "10"})))
				changed = true
			}
		}
	}
	if !changed {
		return withNotes(nil, p, notes), nil
	}
	text, err := yamlText(cfg)
	if err != nil {
		return nil, err
	}
	return withNotes(&Edit{Path: p, Existed: existed, Content: text}, p, notes), nil
}

// PlanOpenClawPlugin is _install_openclaw_plugin.
func PlanOpenClawPlugin(home string) *Edit {
	return &Edit{Path: wardenroot.Join(home, ".openclaw/extensions/opendaisugi"), Kind: "plugin", Listed: true}
}

// missingDir is the error a write into a directory that is not there
// raises, as Python words it.
func missingDir(p string) error {
	if !isDir(filepath.Dir(p)) {
		return failf("[Errno 2] No such file or directory: %s", pyjson.Repr(p))
	}
	return nil
}

// PlanInstall is Runtime.apply(home, layers, ...) decided without
// writing: every edit in Python's order. A step Python raises on fails
// the runtime, with the edits before it kept (Partial).
func PlanInstall(rt Runtime, home, self, root string, o Opts) (*Change, error) {
	ch := &Change{Runtime: rt}
	j := func(rel string) string { return wardenroot.Join(home, rel) }
	switch rt.Key {
	case "codex":
		ch.Mkdirs = []string{j(".codex")}
	case "hermes":
		ch.Mkdirs = []string{j(".hermes")}
	case "openclaw":
		ch.Mkdirs = []string{j(".openclaw/workspace")}
	}
	type step func() (*Edit, error)
	var steps []step
	add := func(s step) { steps = append(steps, s) }
	settings := j(".claude/settings.json")
	if rt.Key == "claude" {
		// Claude Code makes ~/.claude on its first run. Install never
		// makes it: with none, the runtime fails before any write.
		add(func() (*Edit, error) {
			if !isDir(j(".claude")) {
				return nil, failf("Claude Code has not run yet (%s is missing)", j(".claude"))
			}
			return nil, nil
		})
	}
	if o.Has(LayerSkill) {
		add(func() (*Edit, error) { return PlanSkill(skillTarget(rt, home)), nil })
		if rt.Key == "claude" {
			// An older install put Claude's skill in ~/.agents/skills. With
			// no Codex there, that copy is an orphan: take it out.
			add(func() (*Edit, error) {
				if isDir(j(".codex")) {
					return nil, nil
				}
				return PlanRemove(j(".agents/skills/" + SkillName)), nil
			})
		}
	}
	mode := "audit"
	if o.Enforce {
		mode = "enforce"
	}
	switch rt.Key {
	case "claude":
		if o.Has(LayerMCP) {
			add(func() (*Edit, error) {
				return PlanMCP(j(".claude.json"), false, []string{"mcpServers"}, mcpEntry(true, self), "MCP registration",
					priorOf(ch, j(".claude.json")))
			})
		}
		if o.Has(LayerCapture) {
			add(func() (*Edit, error) {
				e, err := PlanClaudeCapture(settings, priorOf(ch, settings), self)
				if err == nil && e != nil && e.Warning == "" {
					err = missingDir(settings)
				}
				return e, err
			})
		}
		if o.Has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanInstructions(j(".claude/CLAUDE.md")) })
		}
		if o.Has(LayerWarden) {
			add(func() (*Edit, error) {
				if err := CheckWardenRoot(root); err != nil {
					return nil, err
				}
				entry := HookEntry(self, HookOptions{Mode: mode, Root: root, Ask: o.Ask})
				e, err := PlanClaudeWardenOn(settings, entry, priorOf(ch, settings))
				if err == nil && e != nil && e.Warning == "" {
					err = missingDir(settings)
				}
				return e, err
			})
			if o.Report == "herdr" {
				add(func() (*Edit, error) { return PlanReportHooks(settings, priorOf(ch, settings), false, self) })
			}
			if o.Report == "herdr" || o.Report == "coppice" {
				add(func() (*Edit, error) { return PlanReportHooks(settings, priorOf(ch, settings), true, self) })
			}
		}
		if o.Router {
			add(func() (*Edit, error) {
				e, err := PlanClaudeBaseURL(settings, o.URL, priorOf(ch, settings))
				if err == nil && e != nil && e.Warning == "" {
					err = missingDir(settings)
				}
				return e, err
			})
		}
	case "codex":
		if o.Has(LayerMCP) {
			add(func() (*Edit, error) { return PlanCodexMCP(j(".codex/config.toml"), self) })
		}
		if o.Has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanInstructions(j(".codex/AGENTS.md")) })
		}
		if o.Has(LayerWarden) {
			add(func() (*Edit, error) {
				if err := CheckWardenRoot(root); err != nil {
					return nil, err
				}
				return PlanCodexWarden(j(".codex/hooks.json"), HookEntry(self, HookOptions{Mode: mode, Root: root}))
			})
		}
		if o.Router {
			add(func() (*Edit, error) {
				return PlanCodexBaseURLOn(j(".codex/config.toml"), o.URL, priorOf(ch, j(".codex/config.toml")))
			})
		}
	case "hermes":
		if o.Has(LayerMCP) || o.Has(LayerCapture) {
			add(func() (*Edit, error) {
				return PlanHermesConfig(j(".hermes/config.yaml"), o.Has(LayerMCP), o.Has(LayerCapture), self)
			})
		}
		if o.Has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanHermesSoul(j(".hermes/SOUL.md")) })
		}
	case "openclaw":
		cfg := j(".openclaw/openclaw.json")
		if o.Has(LayerMCP) {
			add(func() (*Edit, error) {
				return PlanMCP(cfg, true, []string{"mcp", "servers"}, mcpEntry(false, self), "MCP registration", priorOf(ch, cfg))
			})
		}
		if o.Has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanInstructions(j(".openclaw/workspace/AGENTS.md")) })
		}
		if o.Has(LayerCapture) {
			add(func() (*Edit, error) { return PlanOpenClawPlugin(home), nil })
		}
		if o.Router {
			add(func() (*Edit, error) {
				return PlanMCP(cfg, true, []string{"models", "providers"},
					pyjson.NewObject().Set("baseUrl", o.URL).Set("api", "anthropic-messages"),
					"router provider registration", priorOf(ch, cfg))
			})
		}
	}
	if o.hasPando() {
		for _, ps := range pandoInstallSteps(rt, home, self, ch) {
			steps = append(steps, step(ps))
		}
	}
	for _, s := range steps {
		e, err := s()
		if isFail(err) {
			ch.Failed, ch.Why, ch.Partial = true, err.Error(), true
			return ch, nil
		}
		if err != nil {
			return nil, err
		}
		if e != nil {
			ch.Edits = append(ch.Edits, e)
		}
	}
	return ch, nil
}

// clearPath is skill_paths._clear: a symlink or file unlinked, a
// directory removed with what is in it. It is true when something went.
func clearPath(p string) (bool, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return false, nil
	}
	if st.IsDir() {
		return true, os.RemoveAll(p)
	}
	return true, os.Remove(p)
}

// sameSkill is whether target already holds this binary's skill, file for
// file, with nothing else in it.
func sameSkill(target string) bool {
	if st, err := os.Lstat(target); err != nil || !st.IsDir() {
		return false
	}
	want := map[string]string{}
	for _, f := range SkillFiles("assets/skill/" + SkillName) {
		want[f.Rel] = f.Text
	}
	n := 0
	ok := true
	_ = filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err != nil || !ok {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			ok = false
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(target, p)
		b, rerr := os.ReadFile(p)
		if rerr != nil || want[rel] != string(b) {
			ok = false
		}
		n++
		return nil
	})
	return ok && n == len(want)
}

// applyLayerEdit performs the edits only this file plans.
func applyLayerEdit(e *Edit, backed map[string]bool) (string, error) {
	switch e.Kind {
	case "skill":
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o777); err != nil {
			return "", err
		}
		if sameSkill(e.Path) {
			return "", nil
		}
		if _, err := clearPath(e.Path); err != nil {
			return "", err
		}
		for _, f := range SkillFiles("assets/skill/" + SkillName) {
			p := filepath.Join(e.Path, f.Rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(f.Text), 0o666); err != nil {
				return "", err
			}
		}
		return "", nil
	case "plugin":
		if err := os.MkdirAll(e.Path, 0o777); err != nil {
			return "", err
		}
		for _, f := range SkillFiles("assets/openclaw_plugin") {
			p := filepath.Join(e.Path, f.Rel)
			if isSymlink(p) {
				if err := os.Remove(p); err != nil {
					return "", err
				}
			}
			if _, err := wardenroot.WriteFile(p, f.Text); err != nil {
				return "", err
			}
		}
		return "", nil
	case "md":
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o777); err != nil {
			return "", err
		}
		return wardenroot.WriteFile(e.Path, e.Content)
	case "remove":
		_, err := clearPath(e.Path)
		return "", err
	case "pando-plugin", "pando-unit", "pando-unit-remove":
		return "", applyPandoEdit(e, backed)
	case "md-unpatch":
		if e.Undo && emptyContent(e.Content) && isRegular(e.Path) {
			return "", os.Remove(e.Path) // only daisugi's block was left: no backup
		}
		if err := backupOnce(e.Path, backed); err != nil {
			return "", err
		}
		return wardenroot.WriteFile(e.Path, e.Content)
	}
	return "", errors.New("unknown edit kind " + e.Kind)
}

// PlanRemove is a _clear of p for the reverse, or nil when nothing is
// there.
func PlanRemove(p string) *Edit {
	if _, err := os.Lstat(p); err != nil {
		return nil
	}
	return &Edit{Path: p, Kind: "remove", Listed: true}
}

// PlanUnpatchInstructions is _unpatch_instructions.
func PlanUnpatchInstructions(p string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, ErrUnsupported
	}
	if !existed {
		return nil, nil
	}
	start, end, ok := managedSpan(text)
	if !ok {
		return nil, nil
	}
	cleaned := strings.TrimRight(text[:start]+text[end:], "\n")
	if cleaned != "" {
		cleaned += "\n"
	}
	return &Edit{Path: p, Kind: "md-unpatch", Content: cleaned, Listed: true}, nil
}

// PlanPopMCP is _pop_json_mcp.
func PlanPopMCP(p, key string) (*Edit, error) { return planPopMCPNamed("opendaisugi", p, key, nil) }

// planPopMCPNamed is PlanPopMCP for the server called name, on an earlier
// edit of the same file when prior is set.
func planPopMCPNamed(name, p, key string, prior *Edit) (*Edit, error) {
	var v any
	var st readState
	var err error
	if prior != nil && prior.Content != "" {
		v, err = pyjson.Loads(prior.Content)
		st = readOK
	} else {
		v, st, err = readJSON(p)
	}
	if err != nil {
		if isFail(err) {
			return nil, ErrUnsupported
		}
		return nil, err
	}
	if st != readOK {
		return nil, nil
	}
	cfg, ok := v.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	mv, has := cfg.Get(key)
	if !has {
		return nil, nil
	}
	m, ok := mv.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	if _, has := m.Get(name); !has {
		return nil, nil
	}
	m.Delete(name)
	if m.Len() == 0 {
		cfg.Delete(key)
	}
	return &Edit{Path: p, Existed: true, Content: dumpSettings(cfg)}, nil
}

// PlanCodexMCPRemove is the config.toml half of CodexRuntime.reverse.
func PlanCodexMCPRemove(p string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, ErrUnsupported
	}
	if !existed || !codexMCPRe.MatchString(text) {
		return nil, nil
	}
	cleaned := strings.TrimRight(codexMCPRe.ReplaceAllString(text, ""), "\n")
	if cleaned != "" {
		cleaned += "\n"
	}
	return &Edit{Path: p, Existed: true, Content: cleaned}, nil
}

// PlanHermesReverse is the config.yaml half of HermesRuntime.reverse.
func PlanHermesReverse(p string, withMCP, withCapture bool) (*Edit, error) {
	cfg, existed, _, err := yamlOf(p)
	if !existed || errors.Is(err, errYAML) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnsupported
	}
	changed := false
	if servers, ok := cfg.Value("mcp_servers").(*pyjson.Object); ok && withMCP {
		if _, has := servers.Get("opendaisugi"); has {
			servers.Delete("opendaisugi")
			if servers.Len() == 0 {
				cfg.Delete("mcp_servers")
			}
			changed = true
		}
	}
	if hooks, ok := cfg.Value("hooks").(*pyjson.Object); ok && withCapture {
		if pre, ok := hooks.Value("pre_tool_call").([]any); ok {
			var kept []any
			for _, h := range pre {
				if ho, ok := h.(*pyjson.Object); ok && config.IsRecordHook(ho.Value("command"), "") {
					continue
				}
				kept = append(kept, h)
			}
			if len(kept) != len(pre) {
				changed = true
				if len(kept) > 0 {
					hooks.Set("pre_tool_call", kept)
				} else {
					hooks.Delete("pre_tool_call")
				}
				if hooks.Len() == 0 {
					cfg.Delete("hooks")
				}
			}
		}
	}
	if !changed {
		return nil, nil
	}
	text, err := yamlText(cfg)
	if err != nil {
		return nil, err
	}
	return &Edit{Path: p, Existed: true, Content: text}, nil
}

// PlanOpenClawUnconfig is the openclaw.json half of OpenClawRuntime.reverse:
// the MCP server and the router provider taken out in one rewrite.
func PlanOpenClawUnconfig(p string, withMCP, withBaseURL bool) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, ErrUnsupported
	}
	if !existed {
		return nil, nil
	}
	v, lerr := pyjson.Loads(text)
	if lerr != nil {
		if v, lerr = pyjson.Loads(stripJSON5(text)); lerr != nil {
			if errors.Is(lerr, pyjson.ErrUnsupported) {
				return nil, ErrUnsupported
			}
			return nil, nil
		}
	}
	cfg, ok := v.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	changed := false
	for _, keys := range [][2]string{{"mcp", "servers"}, {"models", "providers"}} {
		if (keys[0] == "mcp" && !withMCP) || (keys[0] == "models" && !withBaseURL) {
			continue
		}
		top, has := cfg.Get(keys[0])
		if !has {
			continue
		}
		to, ok := top.(*pyjson.Object)
		if !ok {
			return nil, ErrUnsupported
		}
		inner, has := to.Get(keys[1])
		if !has {
			continue
		}
		io, ok := inner.(*pyjson.Object)
		if !ok {
			return nil, ErrUnsupported
		}
		if _, has := io.Get("opendaisugi"); !has {
			continue
		}
		io.Delete("opendaisugi")
		if io.Len() == 0 {
			to.Delete(keys[1])
			if to.Len() == 0 {
				cfg.Delete(keys[0])
			}
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return &Edit{Path: p, Existed: true, Content: dumpSettings(cfg)}, nil
}

// PlanReverseAll is Runtime.reverse: every managed change reversed, in
// Python's order.
func PlanReverseAll(rt Runtime, home string, gone map[string]bool, only map[string]bool) (*Change, error) {
	j := func(rel string) string { return wardenroot.Join(home, rel) }
	ch := &Change{Runtime: rt}
	// has is whether a layer is reversed: all of them, or the ones named.
	has := func(layer string) bool { return only == nil || only[layer] }
	switch rt.Key {
	case "claude", "codex":
		if !has(LayerWarden) {
			break
		}
		hooksFile := j(".claude/settings.json")
		if rt.Key == "codex" {
			hooksFile = j(".codex/hooks.json")
		}
		if why := unknownWardenHook(hooksFile); why != "" {
			ch.Failed, ch.Why = true, why
			return ch, nil
		}
	}
	var steps []func() (*Edit, error)
	add := func(s func() (*Edit, error)) { steps = append(steps, s) }
	// A path an earlier runtime's reverse removed is gone by the time
	// this one runs, as Python's runtimes reverse one after another.
	remove := func(p string) *Edit {
		if gone[p] {
			return nil
		}
		e := PlanRemove(p)
		if e != nil {
			gone[p] = true
		}
		return e
	}
	removeBoth := func(fallback string) {
		add(func() (*Edit, error) { return remove(j(".agents/skills/" + SkillName)), nil })
		add(func() (*Edit, error) { return remove(wardenroot.Join(j(fallback), SkillName)), nil })
	}
	settings := j(".claude/settings.json")
	switch rt.Key {
	case "claude":
		if has(LayerSkill) {
			// ~/.agents/skills is Codex's when Codex is there; else a copy
			// in it is an older Claude install's.
			if isDir(j(".codex")) {
				add(func() (*Edit, error) { return remove(j(".claude/skills/" + SkillName)), nil })
			} else {
				removeBoth(".claude/skills")
			}
		}
		if has(LayerMCP) {
			add(func() (*Edit, error) { return PlanPopMCP(j(".claude.json"), "mcpServers") })
		}
		if has(LayerCapture) {
			add(func() (*Edit, error) {
				return popHook(settings, isAnyRecordHook, []string{"PreToolUse"}, priorOf(ch, settings))
			})
		}
		if has(LayerWarden) {
			add(func() (*Edit, error) {
				return popHook(settings, config.IsWardenHook, []string{"PreToolUse"}, priorOf(ch, settings))
			})
			add(func() (*Edit, error) {
				return popHook(settings, isAnyRecordHook, []string{"Stop", "Notification", "SubagentStart", "SubagentStop"},
					priorOf(ch, settings))
			})
		}
		if has(LayerBaseURL) {
			add(func() (*Edit, error) { return PlanPopEnvKey(settings, "ANTHROPIC_BASE_URL", priorOf(ch, settings)) })
		}
		if has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanUnpatchInstructions(j(".claude/CLAUDE.md")) })
		}
	case "codex":
		if has(LayerSkill) {
			removeBoth(".codex/skills")
		}
		if has(LayerMCP) {
			add(func() (*Edit, error) { return PlanCodexMCPRemove(j(".codex/config.toml")) })
		}
		if has(LayerWarden) {
			add(func() (*Edit, error) {
				return popHook(j(".codex/hooks.json"), config.IsWardenHook, []string{"PreToolUse"}, nil)
			})
		}
		if has(LayerBaseURL) {
			add(func() (*Edit, error) {
				return PlanCodexUnpatchOn(j(".codex/config.toml"), priorOf(ch, j(".codex/config.toml")))
			})
		}
		if has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanUnpatchInstructions(j(".codex/AGENTS.md")) })
		}
	case "hermes":
		if has(LayerSkill) {
			add(func() (*Edit, error) { return remove(j(".hermes/skills/opendaisugi/" + SkillName)), nil })
		}
		if has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanUnpatchInstructions(j(".hermes/SOUL.md")) })
		}
		if has(LayerMCP) || has(LayerCapture) {
			add(func() (*Edit, error) {
				return PlanHermesReverse(j(".hermes/config.yaml"), has(LayerMCP), has(LayerCapture))
			})
		}
	case "openclaw":
		if has(LayerSkill) {
			add(func() (*Edit, error) { return remove(j(".openclaw/workspace/skills/" + SkillName)), nil })
		}
		if has(LayerMCP) || has(LayerBaseURL) {
			add(func() (*Edit, error) {
				return PlanOpenClawUnconfig(j(".openclaw/openclaw.json"), has(LayerMCP), has(LayerBaseURL))
			})
		}
		if has(LayerCapture) {
			add(func() (*Edit, error) { return remove(j(".openclaw/extensions/opendaisugi")), nil })
		}
		if has(LayerInstructions) {
			add(func() (*Edit, error) { return PlanUnpatchInstructions(j(".openclaw/workspace/AGENTS.md")) })
		}
	}
	if only == nil || only[LayerPando] {
		steps = append(steps, pandoReverseSteps(rt, home, ch, gone)...)
	}
	for _, s := range steps {
		e, err := s()
		if e != nil {
			e.Undo, e.Home = true, home
		}
		if isFail(err) {
			// reverse() would raise, and uninstall would print the
			// exception's text, which this binary cannot reproduce.
			return nil, ErrUnsupported
		}
		if err != nil {
			return nil, err
		}
		if e != nil {
			ch.Edits = append(ch.Edits, e)
		}
	}
	return ch, nil
}

// popHook is PlanPopHook with a hook file that is not JSON left alone.
func popHook(p string, match func(any) bool, events []string, prior *Edit) (*Edit, error) {
	return PlanPopHook(p, match, events, prior)
}

// pyReprPath is repr(str(path)).
func pyReprPath(p string) string { return pystr.Repr(p) }
