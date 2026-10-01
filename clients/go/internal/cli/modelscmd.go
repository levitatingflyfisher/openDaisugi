package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"daisugi-verify/internal/catalog"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/voice"
)

var modelsHelp = `Usage: daisugi models [OPTIONS] COMMAND [ARGS]...

  The models the garden can use: a curated list, a search, and your choice.

Options:
  --help  Show this message and exit.

Commands:
  list    List the curated models, the default for this box, and the one in use.
  search  Search the Hugging Face API for models, filtered by size and license.
  use     Record the model the garden uses. Any id, path or GGUF file is accepted.
  pin     Resolve a repo to a file pinned to its commit; --pull downloads it.
`

// modelsCmd is the `models` group (cli.models_app).
func (e *Env) modelsCmd(args []string) error {
	if len(args) == 0 {
		e.out("%s", modelsHelp)
		return exit(2)
	}
	if args[0] == "--help" {
		e.out("%s", modelsHelp)
		return nil
	}
	switch args[0] {
	case "list":
		return e.modelsList(args[1:])
	case "search":
		return e.modelsSearch(args[1:])
	case "use":
		return e.modelsUse(args[1:])
	case "pin":
		return e.modelsPin(args[1:])
	}
	e.errf("Usage: daisugi models [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi models --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

// modelsHardware is detect_voice_hardware: OPENDAISUGI_VOICE_HARDWARE
// when it is set, else the probe tiers setup uses.
func (e *Env) modelsHardware() voice.VoiceHardware {
	if raw := e.env[voice.HardwareEnv]; raw != "" {
		if hw, ok := voice.ParseHardwareEnv(raw); ok {
			return hw
		}
	}
	return e.voiceHardware()
}

// inUse is model_catalog.in_use.
func inUse(dataDir string) string {
	raw, err := os.ReadFile(gateroot.Join(dataDir, catalog.ChoiceFile))
	if err != nil {
		return ""
	}
	text, derr := pystr.DecodeStrict(raw)
	if derr != nil {
		return ""
	}
	v, jerr := pyjson.LoadsPy(text, 900)
	if jerr != nil {
		return ""
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return ""
	}
	s, ok := o.Value("model").(string)
	if !ok || pystr.Strip(s) == "" {
		return ""
	}
	return s
}

func (e *Env) modelsList(args []string) error {
	const cmd = "models list"
	opts := []opt{dataDirOpt, {names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "List the curated models, the default for this box, and the one in use.", opts)
	}
	cat, err := catalog.Load()
	if err != nil {
		return e.fail(cmd, err)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	hw := e.modelsHardware()
	def := cat.Default(hw.RAMGB, hw.VRAMGB)
	chosen := inUse(dataDir)
	if p.flag("--json") {
		var ram, use any
		if hw.RAMGB != nil {
			ram = floatAny(*hw.RAMGB)
		}
		if chosen != "" {
			use = chosen
		}
		ms := make([]any, len(cat.Models))
		for i, m := range cat.Models {
			ms[i] = m.Object()
		}
		payload := pyjson.NewObject().
			Set("hardware", pyjson.NewObject().Set("ram_gb", ram).Set("vram_gb", floatAny(hw.VRAMGB)).
				Set("class", cat.Class(hw.RAMGB, hw.VRAMGB))).
			Set("default", def).Set("in_use", use).Set("models", ms)
		e.out("%s\n", pyjson.DumpsIndent(payload, 2, true))
		return nil
	}
	e.out("%s\n", cat.HardwareLine(hw.RAMGB, hw.VRAMGB))
	if chosen != "" {
		e.out("In use: %s (recorded by daisugi models use).\n", chosen)
	} else {
		e.out("In use: the default.\n")
	}
	e.out("\n")
	for _, ln := range catalog.TableLines(cat.Models) {
		e.out("%s\n", ln)
	}
	e.out("\nAny other model works too: daisugi models search QUERY, then daisugi models use ID.\n")
	return nil
}

func (e *Env) modelsUse(args []string) error {
	const cmd = "models use"
	opts := []opt{dataDirOpt}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "ID", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " ID", "Record the model the garden uses. Any id, path or GGUF file is accepted.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "ID", &usageError{"Missing argument 'ID'."})
	}
	id := p.args[0]
	if pystr.Strip(id) == "" {
		return e.fail3("A model id cannot be blank.", "models use records the id it is given, as given.",
			"run: daisugi models list", 2)
	}
	dataDir := gateroot.PathStr(p.str("--data-dir", e.dataHome()))
	if err := os.MkdirAll(dataDir, 0o777); err != nil {
		return e.failPy(cmd, err)
	}
	path := gateroot.Join(dataDir, catalog.ChoiceFile)
	body := pyjson.DumpsIndent(pyjson.NewObject().Set("model", id), 2, true) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o666); err != nil {
		return e.failPy(cmd, err)
	}
	e.out("The garden now uses %s (recorded in %s).\n", id, path)
	return nil
}

// errOffline and errStatus are model_catalog.Offline and HTTPStatus.
type errOffline struct{}

func (errOffline) Error() string { return "offline" }

type errStatus struct{ code int }

func (s errStatus) Error() string { return fmt.Sprintf("HTTP %d", s.code) }

// hfGet is model_catalog.fetch: a GET as urllib makes it, through the
// proxies urllib would use.
func (e *Env) hfGet(url string) (string, error) {
	rules := netproxy.UrllibFromVars(netproxy.FromEnviron(e.Environ), true)
	base := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{Transport: netproxy.Transport(rules, base), Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", errOffline{}
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "opendaisugi")
	resp, err := client.Do(req)
	if err != nil {
		return "", errOffline{}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", errStatus{resp.StatusCode}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errOffline{}
	}
	text, derr := pystr.DecodeStrict(body)
	if derr != nil {
		return "", catalog.ErrBadAnswer
	}
	return text, nil
}

func (e *Env) modelsSearch(args []string) error {
	const cmd = "models search"
	opts := []opt{
		{names: []string{"--max-params"}, value: true, metavar: "FLOAT", help: "Largest size to show, in billions of " +
			"parameters; 0 shows any size. Default: 8 on a capable box, 2 on a small one."},
		{names: []string{"--license"}, value: true, multiple: true, metavar: "TEXT", help: "Show only this license (repeatable), such as apache-2.0."},
		{names: []string{"--limit"}, value: true, metavar: "INTEGER RANGE", help: "Most results to show."},
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "QUERY", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " QUERY", "Search the Hugging Face API for models, filtered by size and license.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "QUERY", &usageError{"Missing argument 'QUERY'."})
	}
	query := p.args[0]
	maxB, err := clickFloat(p, "--max-params", 0)
	if err != nil {
		return e.usageArgs(cmd, "QUERY", err)
	}
	limit := int64(20)
	if p.has("--limit") {
		raw := p.str("--limit", "")
		n, ok := pyInt(raw)
		if !ok {
			return e.usageArgs(cmd, "QUERY", &usageError{fmt.Sprintf("Invalid value for '--limit': %s is not a valid int range.", pystr.Repr(raw))})
		}
		if n < 1 || n > 100 {
			return e.usageArgs(cmd, "QUERY", &usageError{fmt.Sprintf("Invalid value for '--limit': %d is not in the range 1<=x<=100.", n)})
		}
		limit = n
	}
	cat, err := catalog.Load()
	if err != nil {
		return e.fail(cmd, err)
	}
	if !p.has("--max-params") {
		hw := e.modelsHardware()
		maxB = cat.SearchMax(hw.RAMGB, hw.VRAMGB)
	}
	lics := p.vals["--license"]
	where := catalog.Endpoint(e.env)
	var rows []catalog.Row
	var serr error
	if catalog.OfflineEnv(e.env) {
		serr = errOffline{}
	} else {
		var text string
		if text, serr = e.hfGet(where + catalog.SearchTarget(query)); serr == nil {
			rows, serr = cat.ParseListing(text)
		}
	}
	switch x := serr.(type) {
	case nil:
	case errOffline:
		if catalog.OfflineEnv(e.env) {
			e.errf("Offline: HF_HUB_OFFLINE is set, so the Hugging Face API was not asked.\n")
		} else {
			e.errf("Offline: the Hugging Face API at %s could not be reached.\n", where)
		}
		return exit(1)
	case errStatus:
		e.errf("The Hugging Face API at %s answered HTTP %d.\n", where, x.code)
		return exit(1)
	default:
		e.errf("The Hugging Face API at %s did not answer with a model list.\n", where)
		return exit(1)
	}
	shown := catalog.Filter(rows, maxB, lics, int(limit))
	if p.flag("--json") {
		res := make([]any, len(shown))
		for i, r := range shown {
			res[i] = r.Object()
		}
		ls := make([]any, len(lics))
		for i, l := range lics {
			ls[i] = l
		}
		payload := pyjson.NewObject().Set("query", query).Set("endpoint", where).
			Set("max_params_b", floatAny(maxB)).Set("licenses", ls).Set("results", res)
		e.out("%s\n", pyjson.DumpsIndent(payload, 2, true))
		return nil
	}
	size := "up to " + catalog.PyG(maxB) + "B parameters"
	if maxB <= 0 {
		size = "any size"
	}
	lic := "any license"
	if len(lics) > 0 {
		lic = "license " + strings.Join(lics, " or ")
	}
	e.out("Search: \"%s\" on %s, %s, %s.\n\n", query, where, size, lic)
	if len(shown) == 0 {
		e.out("No model matches.\n")
		return nil
	}
	for _, ln := range catalog.TableLines(shown) {
		e.out("%s\n", ln)
	}
	e.out("\nRecord one for the garden: daisugi models use ID\n")
	return nil
}
