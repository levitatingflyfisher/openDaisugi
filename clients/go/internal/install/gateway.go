package install

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
)

// This file is the BASE_URL layer of install (ADR-0013): each harness
// pointed at the local token-saving gateway, and the reverse.

// DefaultBaseURL is DEFAULT_GATEWAY_BASE_URL.
const DefaultBaseURL = "http://127.0.0.1:8787"

// HermesGap is the gateway gap Hermes reports.
const HermesGap = "Not wired: gateway speaks Anthropic Messages; this harness's custom endpoint expects OpenAI " +
	"— not wired (needs an OpenAI-wire adapter)"

// Layer names as the plan prints them.
const (
	LayerGate    = "gate"
	LayerBaseURL = "base_url"
)

// PlanSteps is Runtime.plan(home, layers) for the gate and base_url
// layers: the supported steps in layer order, then the gap notes.
func PlanSteps(rt Runtime, home string, gate, gateway, enforce, ask bool, url string) []Step {
	var steps, gaps []Step
	if gate {
		for _, s := range Plan(rt, home, enforce, ask) {
			s.Layer = LayerGate
			if s.Supported {
				steps = append(steps, s)
			} else {
				gaps = append(gaps, s)
			}
		}
	}
	if gateway {
		switch rt.Key {
		case "claude":
			steps = append(steps, Step{Layer: LayerBaseURL, Description: "Point ANTHROPIC_BASE_URL at " + url,
				Target: gateroot.Join(home, ".claude/settings.json"), Supported: true})
		case "codex":
			steps = append(steps, Step{Layer: LayerBaseURL, Description: "Register gateway model provider (" + url +
				"/v1, wire_api=chat) and select it", Target: gateroot.Join(home, ".codex/config.toml"), Supported: true})
		case "openclaw":
			steps = append(steps, Step{Layer: LayerBaseURL, Description: "Register opendaisugi gateway provider " +
				"(anthropic-messages) at " + url + " — select it as the active provider manually",
				Target: gateroot.Join(home, ".openclaw/openclaw.json"), Supported: true})
		case "hermes":
			gaps = append(gaps, Step{Layer: LayerBaseURL, Description: HermesGap})
		}
	}
	return append(steps, gaps...)
}

// PlanApplyGateway is PlanApply with the base_url layer after the gate
// layer. A base_url edit Python's apply raises on fails the runtime after
// the gate edit before it was written: Partial keeps that edit.
func PlanApplyGateway(rt Runtime, home, self, root string, gate, gateway, enforce, ask bool, url string) (*Change, error) {
	ch := &Change{Runtime: rt}
	if gate {
		var err error
		if ch, err = PlanApply(rt, home, self, root, enforce, ask); err != nil || ch.Failed {
			return ch, err
		}
	} else {
		switch rt.Key {
		case "codex":
			ch.Mkdirs = []string{gateroot.Join(home, ".codex")}
		case "hermes":
			ch.Mkdirs = []string{gateroot.Join(home, ".hermes")}
		case "openclaw":
			ch.Mkdirs = []string{gateroot.Join(home, ".openclaw/workspace")}
		}
	}
	if !gateway {
		return ch, nil
	}
	var prior *Edit
	for _, e := range ch.Edits {
		prior = e
	}
	var e *Edit
	var err error
	switch rt.Key {
	case "claude":
		p := gateroot.Join(home, ".claude/settings.json")
		if prior == nil || prior.Path != p || prior.Warning != "" {
			prior = nil
		}
		e, err = PlanClaudeBaseURL(p, url, prior)
		if err == nil && e != nil && e.Warning == "" && !isDir(gateroot.Join(home, ".claude")) {
			err = failf("[Errno 2] No such file or directory: %s", pyjson.Repr(p))
		}
	case "codex":
		e, err = PlanCodexBaseURL(gateroot.Join(home, ".codex/config.toml"), url)
	case "openclaw":
		e, err = PlanOpenClawBaseURL(gateroot.Join(home, ".openclaw/openclaw.json"), url)
	}
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
	return ch, nil
}

// jsonOf is the content an edit is planned on: an earlier edit of the
// same file in this run, or the file.
func jsonOf(p string, prior *Edit) (any, readState, error) {
	if prior != nil && prior.Content != "" {
		v, err := pyjson.Loads(prior.Content)
		if err != nil {
			return nil, readOK, ErrUnsupported
		}
		return v, readOK, nil
	}
	return readJSON(p)
}

// PlanClaudeBaseURL is _patch_claude_base_url.
func PlanClaudeBaseURL(settingsPath, url string, prior *Edit) (*Edit, error) {
	v, st, err := jsonOf(settingsPath, prior)
	if err != nil {
		return nil, err
	}
	if st == readBad {
		return &Edit{Path: settingsPath, Warning: settingsPath + " is not valid JSON; skipping ANTHROPIC_BASE_URL " +
			"to avoid overwriting your Claude Code settings (permissions/env). Fix the file and re-run `daisugi install`."}, nil
	}
	existed := st != readMissing
	if !existed {
		v = pyjson.NewObject()
	}
	s, ok := v.(*pyjson.Object)
	if !ok {
		return nil, failf("'%s' object has no attribute 'setdefault'", pyType(v))
	}
	envV, has := s.Get("env")
	if !has {
		envV = pyjson.NewObject()
		s.Set("env", envV)
	}
	env, ok := envV.(*pyjson.Object)
	if !ok {
		return nil, failf("'%s' object has no attribute 'get'", pyType(envV))
	}
	if cur, has := env.Get("ANTHROPIC_BASE_URL"); has && cur == any(url) {
		return nil, nil
	}
	env.Set("ANTHROPIC_BASE_URL", url)
	return &Edit{Path: settingsPath, Existed: existed, Content: dumpSettings(s)}, nil
}

// pyType is type(v).__name__ for a decoded JSON value.
func pyType(v any) string {
	switch v.(type) {
	case *pyjson.Object:
		return "dict"
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	case nil:
		return "NoneType"
	case pyjson.Float, float64:
		return "float"
	}
	return "int"
}

const codexSelect = `model_provider = "opendaisugi"`

func codexBlock(url string) string {
	return "\n[model_providers.opendaisugi]\nname = \"openDaisugi gateway\"\nbase_url = \"" + url + "/v1\"\nwire_api = \"chat\"\n"
}

// readText is path.read_text(): "" and false when the file is absent; a
// file that is not UTF-8 fails the runtime.
func readText(p string) (string, bool, error) {
	if _, err := os.Stat(p); err != nil {
		return "", false, nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", true, failf("%v", err)
	}
	if !utf8.Valid(raw) {
		return "", true, failf("%s is not valid UTF-8", p)
	}
	return string(raw), true, nil
}

// PlanCodexBaseURL is _patch_codex_base_url: the provider table appended
// and the selector prepended, both once.
func PlanCodexBaseURL(p, url string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, err
	}
	if strings.Contains(text, "[model_providers.opendaisugi]") {
		return nil, nil
	}
	if !strings.Contains(text, codexSelect) {
		text = codexSelect + "\n" + text
	}
	return &Edit{Path: p, Existed: existed, Content: strings.TrimRight(text, "\n") + "\n" + codexBlock(url)}, nil
}

var json5TrailingComma = regexp.MustCompile(`,([\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]*[}\]])`)

// stripJSON5 is _strip_json5_comments.
func stripJSON5(text string) string {
	rs := []rune(text)
	var out []rune
	n := len(rs)
	inStr := false
	find := func(sub string, from int) int {
		if from > n {
			return -1
		}
		i := strings.Index(string(rs[from:]), sub)
		if i < 0 {
			return -1
		}
		return from + len([]rune(string(rs[from:])[:i]))
	}
	for i := 0; i < n; {
		c := rs[i]
		if inStr {
			out = append(out, c)
			if c == '\\' && i+1 < n {
				out = append(out, rs[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inStr = false
			}
			i++
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			i++
			continue
		}
		if c == '/' && i+1 < n && rs[i+1] == '/' {
			if j := find("\n", i); j == -1 {
				i = n
			} else {
				i = j
			}
			continue
		}
		if c == '/' && i+1 < n && rs[i+1] == '*' {
			if j := find("*/", i+2); j == -1 {
				i = n
			} else {
				i = j + 2
			}
			continue
		}
		out = append(out, c)
		i++
	}
	return json5TrailingComma.ReplaceAllString(string(out), "$1")
}

var json5Comment = regexp.MustCompile(`//|/\*`)

// PlanOpenClawBaseURL is _patch_openclaw_base_url: _patch_mcp with the
// provider block under models.providers.opendaisugi.
func PlanOpenClawBaseURL(p, url string) (*Edit, error) {
	warnBad := p + " is not valid; skipping gateway provider registration to avoid overwriting user state. " +
		"Fix the file and re-run `daisugi install`."
	text, existed, err := readText(p)
	if err != nil {
		return nil, err
	}
	var cfg any = pyjson.NewObject()
	if existed {
		v, lerr := pyjson.Loads(text)
		if lerr != nil {
			if errors.Is(lerr, pyjson.ErrUnsupported) {
				return nil, ErrUnsupported
			}
			v, lerr = pyjson.Loads(stripJSON5(text))
			if lerr != nil {
				if errors.Is(lerr, pyjson.ErrUnsupported) {
					return nil, ErrUnsupported
				}
				return &Edit{Path: p, Warning: warnBad}, nil
			}
		}
		cfg = v
	}
	leaf := cfg
	for _, k := range []string{"models", "providers"} {
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
		// `"opendaisugi" in leaf` then leaf[...] = entry: only a dict takes it.
		return nil, failf("'%s' object does not support item assignment", pyType(leaf))
	}
	if _, has := lo.Get("opendaisugi"); has {
		return nil, nil
	}
	e := &Edit{Path: p, Existed: existed}
	if existed && json5Comment.MatchString(text) {
		e.PreWarning = p + " contains JSON5 comments which will not survive the rewrite (the writer emits plain " +
			"JSON). The pre-write backup at " + p + ".bak* preserves the original text — restore from it if you " +
			"need the comments back. Tracked as M7 in REVIEW_FINDINGS.md."
	}
	lo.Set("opendaisugi", pyjson.NewObject().Set("baseUrl", url).Set("api", "anthropic-messages"))
	e.Content = dumpSettings(cfg)
	return e, nil
}

// PlanPopEnvKey is _pop_json_env_key.
func PlanPopEnvKey(p, key string, prior *Edit) (*Edit, error) {
	v, st, err := jsonOf(p, prior)
	if err != nil {
		return nil, err
	}
	if st != readOK {
		return nil, nil
	}
	s, ok := v.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	env, ok := s.Value("env").(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	if _, has := env.Get(key); !has {
		return nil, nil
	}
	env.Delete(key)
	if env.Len() == 0 {
		s.Delete("env")
	}
	return &Edit{Path: p, Existed: true, Content: dumpSettings(s)}, nil
}

var codexProviderTable = regexp.MustCompile(`\n?\[model_providers\.opendaisugi\][^\[]*`)

// PlanCodexUnpatch is _unpatch_codex_base_url.
func PlanCodexUnpatch(p string) (*Edit, error) {
	text, existed, err := readText(p)
	if err != nil {
		return nil, ErrUnsupported
	}
	if !existed || (!strings.Contains(text, "[model_providers.opendaisugi]") && !strings.Contains(text, codexSelect)) {
		return nil, nil
	}
	if loc := codexProviderTable.FindStringIndex(text); loc != nil {
		text = text[:loc[0]] + "\n" + text[loc[1]:]
	}
	text = strings.Replace(text, codexSelect+"\n", "", 1)
	cleaned := strings.Trim(text, "\n")
	if cleaned != "" {
		cleaned += "\n"
	}
	return &Edit{Path: p, Existed: true, Content: cleaned}, nil
}

// PlanOpenClawUnprovider is the provider half of OpenClaw's reverse.
func PlanOpenClawUnprovider(p string) (*Edit, error) {
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
	models, has := cfg.Get("models")
	if !has {
		return nil, nil
	}
	mo, ok := models.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	provs, has := mo.Get("providers")
	if !has {
		return nil, nil
	}
	po, ok := provs.(*pyjson.Object)
	if !ok {
		return nil, ErrUnsupported
	}
	if _, has := po.Get("opendaisugi"); !has {
		return nil, nil
	}
	po.Delete("opendaisugi")
	if po.Len() == 0 {
		mo.Delete("providers")
		if mo.Len() == 0 {
			cfg.Delete("models")
		}
	}
	return &Edit{Path: p, Existed: true, Content: dumpSettings(cfg)}, nil
}

// PlanReverseGateway is the base_url half of Runtime.reverse, after the
// gate half's edits (prior) of the same files.
func PlanReverseGateway(rt Runtime, home string, ch *Change) error {
	var e *Edit
	var err error
	switch rt.Key {
	case "claude":
		p := gateroot.Join(home, ".claude/settings.json")
		var prior *Edit
		for _, x := range ch.Edits {
			if x.Path == p {
				prior = x
			}
		}
		e, err = PlanPopEnvKey(p, "ANTHROPIC_BASE_URL", prior)
	case "codex":
		e, err = PlanCodexUnpatch(gateroot.Join(home, ".codex/config.toml"))
	case "openclaw":
		e, err = PlanOpenClawUnprovider(gateroot.Join(home, ".openclaw/openclaw.json"))
	}
	if err != nil {
		return err
	}
	if e != nil {
		ch.Edits = append(ch.Edits, e)
	}
	return nil
}
