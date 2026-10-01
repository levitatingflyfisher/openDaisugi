package cli

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// The Hub's default endpoint, as huggingface_hub's constants name it.
const hubDefaultEndpoint = "https://huggingface.co"

// The one date form huggingface_hub's parse_datetime is sure to read.
var hubDate = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(\.[0-9]+)?Z$`)

// modelsPin is `daisugi models pin REPO`: model_registry.resolve_pinned
// through the Hub API, as huggingface_hub's list_repo_files and model_info
// ask it, and --pull's hf_hub_download into the cache (RF-7).
func (e *Env) modelsPin(args []string) error {
	const cmd = "models pin"
	opts := []opt{
		{names: []string{"--suffix"}, value: true, metavar: "TEXT", help: "File suffix to resolve (.gguf or .llamafile)."},
		{names: []string{"--pull"}, help: "Download the resolved file (pinned to its commit)."},
		{names: []string{"--json"}, help: "Machine-readable JSON output."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "REPO", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " REPO", "Resolve a repo to a file pinned to its commit; --pull downloads it.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "REPO", &usageError{"Missing argument 'repo'."})
	}
	repo, suffix := p.args[0], p.str("--suffix", ".gguf")
	// validate_repo_id runs before any request, then the offline check.
	if why := hubRepoInvalid(repo); why != "" {
		return e.pyTraceback("huggingface_hub.errors.HFValidationError", why)
	}
	endpoint := hubDefaultEndpoint
	if v, ok := e.env["HF_ENDPOINT"]; ok {
		endpoint = strings.TrimRight(v, "/")
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return e.refuse(cmd, errString("HF_ENDPOINT is not an http or https URL"))
	}
	treeURL := endpoint + "/api/models/" + repo + "/tree/main?recursive=true&expand=false"
	if hubOffline(e.env) {
		return e.pyTraceback("huggingface_hub.errors.OfflineModeIsEnabled", "Cannot reach "+treeURL+
			": offline mode is enabled. To disable it, please unset the `HF_HUB_OFFLINE` environment variable.")
	}
	hub := e.newHubClient()
	files, err := hub.treeFiles(treeURL, e)
	if err != nil {
		return e.hubErr(cmd, err)
	}
	var matches []string
	for _, f := range files {
		if strings.HasSuffix(f, suffix) {
			matches = append(matches, f)
		}
	}
	if len(matches) == 0 {
		e.errf("no %s file in %s (saw %d files)\n", pystr.Repr(suffix), pystr.Repr(repo), len(files))
		return exit(2)
	}
	sort.Strings(matches)
	body, err := hub.getJSON(endpoint + "/api/models/" + repo)
	if err != nil {
		return e.hubErr(cmd, err)
	}
	sha, err := modelInfoSHA(body)
	if err != nil {
		return e.hubErr(cmd, err)
	}
	var pulled any
	if p.flag("--pull") {
		rev, ok := sha.(string)
		if !ok {
			return e.refuse(cmd, errString("--pull with a model info that has no sha"))
		}
		path, err := e.hubPull(hub, endpoint, repo, matches[0], rev)
		if err != nil {
			return e.hubErr(cmd, err)
		}
		pulled = path
	}
	if p.flag("--json") {
		o := pyjson.NewObject().Set("repo_id", repo).Set("filename", matches[0]).Set("revision", sha).
			Set("downloaded_path", pulled)
		e.out("%s\n", pyjson.DumpsIndent(o, 2, true))
		return nil
	}
	rev := "None"
	if s, ok := sha.(string); ok {
		rev = s
	}
	e.out("repo:     %s\n", repo)
	e.out("file:     %s\n", matches[0])
	e.out("revision: %s   (immutable commit — reproducible)\n", rev)
	if path, ok := pulled.(string); ok && path != "" {
		e.out("pulled:   %s\n", path)
	} else {
		e.out("\nDownload it (pinned):  daisugi models pin %s --suffix %s --pull\n", repo, suffix)
	}
	return nil
}

// hubRepoInvalid is validate_repo_id: "" when the id passes, else the
// HFValidationError's text. Python's \w is a letter or number of any
// script (str.isalnum) or "_".
func hubRepoInvalid(repo string) string {
	if strings.Count(repo, "/") > 1 {
		return "Repo id must be in the form 'repo_name' or 'namespace/repo_name': '" + repo + "'. Use `repo_type` argument if needed."
	}
	word := func(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }
	part := func(s string, max int) bool {
		rs := []rune(s)
		if len(rs) == 0 || (max > 0 && len(rs) > max) || !word(rs[0]) || !word(rs[len(rs)-1]) {
			return false
		}
		for _, r := range rs {
			if !word(r) && r != '-' && r != '.' {
				return false
			}
		}
		return true
	}
	ns, name, has := strings.Cut(repo, "/")
	ok := part(repo, 96)
	if has {
		ok = part(ns, 0) && part(name, 96)
	}
	if !ok {
		return "Repo id must use alphanumeric chars, '-', '_' or '.'. The name cannot start or end with '-' or '.' and the maximum length is 96: '" + repo + "'."
	}
	if strings.Contains(repo, "--") || strings.Contains(repo, "..") {
		return "Cannot have -- or .. in repo_id: '" + repo + "'."
	}
	if strings.HasSuffix(repo, ".git") {
		return "Repo_id cannot end by '.git': '" + repo + "'."
	}
	return ""
}

// hubOffline is huggingface_hub's HF_HUB_OFFLINE: any value it reads as
// true makes every request raise.
func hubOffline(env map[string]string) bool {
	switch strings.ToUpper(env["HF_HUB_OFFLINE"]) {
	case "1", "ON", "YES", "TRUE":
		return true
	}
	return false
}

type hubClient struct {
	c     *http.Client
	token string
}

// newHubClient sends requests as huggingface_hub's httpx session does,
// through the proxies httpx would use, with the token it would send.
func (e *Env) newHubClient() *hubClient {
	rules := netproxy.HttpxFromVars(netproxy.FromEnviron(e.Environ), true)
	base := &http.Transport{DialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext}
	// A redirect is not followed: the answer comes back as it is, and is
	// refused (RF-7), as the Rust port's client does.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hubClient{c: &http.Client{Transport: netproxy.Transport(rules, base), CheckRedirect: noRedirect}, token: e.hubToken()}
}

// hubToken is get_token: HF_TOKEN, else the token file.
func (e *Env) hubToken() string {
	for _, k := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"} {
		if t := strings.TrimSpace(e.env[k]); t != "" {
			return t
		}
	}
	path := e.env["HF_TOKEN_PATH"]
	if path == "" {
		home := e.env["HF_HOME"]
		if home == "" {
			cache := e.env["XDG_CACHE_HOME"]
			if cache == "" {
				cache = filepath.Join(e.home, ".cache")
			}
			home = filepath.Join(cache, "huggingface")
		}
		path = filepath.Join(home, "token")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// modelInfoSHA is ModelInfo(**data).sha, for a body ModelInfo is sure to
// read. The value is a string or nil.
func modelInfoSHA(v any) (any, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errString("the model info is not an object")
	}
	if _, has := o.Get("id"); !has {
		return nil, &hubRaise{"KeyError", "'id'"}
	}
	if _, has := o.Get("self"); has {
		return nil, &hubRaise{"TypeError", "ModelInfo.__init__() got multiple values for argument 'self'"}
	}
	either := func(a, b string) any {
		if x := o.Value(a); pyjson.Truthy(x) {
			return x
		}
		return o.Value(b)
	}
	for _, d := range []any{either("lastModified", "last_modified"), either("createdAt", "created_at")} {
		if pyjson.Truthy(d) && !hubDateOK(d) {
			return nil, errString("a model info date is not in the form this binary reads")
		}
	}
	switch m := o.Value("inferenceProviderMapping").(type) {
	case nil:
	case []any:
		if len(m) > 0 {
			return nil, errString("the model info maps inference providers, which this binary does not read")
		}
	case *pyjson.Object:
		if m.Len() > 0 {
			return nil, errString("the model info maps inference providers, which this binary does not read")
		}
	default:
		return nil, errString("huggingface_hub raises on this inferenceProviderMapping")
	}
	if card, ok := either("cardData", "card_data").(*pyjson.Object); ok {
		for _, k := range []string{"model-index", "eval_results"} {
			if pyjson.Truthy(card.Value(k)) {
				return nil, errString("the model card holds eval results, which this binary does not read")
			}
		}
		for _, k := range []string{"self", "ignore_metadata_errors"} {
			if _, has := card.Get(k); has {
				return nil, fmt.Errorf("the model card holds the key %s", k)
			}
		}
		switch card.Value("tags").(type) {
		case nil, string, []any:
		default:
			return nil, errString("the model card's tags are not a list")
		}
	}
	if ti := either("transformersInfo", "transformers_info"); pyjson.Truthy(ti) {
		to, ok := ti.(*pyjson.Object)
		if !ok {
			return nil, errString("transformersInfo is not an object")
		}
		if _, has := to.Get("auto_model"); !has {
			return nil, errString("transformersInfo has no auto_model")
		}
		for _, k := range to.Keys() {
			switch k {
			case "auto_model", "custom_class", "pipeline_tag", "processor":
			default:
				return nil, fmt.Errorf("transformersInfo holds the key %s", k)
			}
		}
	}
	if sib := o.Value("siblings"); sib != nil {
		list, ok := sib.([]any)
		if !ok {
			return nil, errString("siblings is not a list")
		}
		for _, s := range list {
			so, ok := s.(*pyjson.Object)
			if !ok {
				return nil, errString("a sibling is not an object")
			}
			if _, has := so.Get("rfilename"); !has {
				return nil, errString("a sibling has no rfilename")
			}
			if lfs := so.Value("lfs"); pyjson.Truthy(lfs) {
				lo, ok := lfs.(*pyjson.Object)
				if !ok {
					return nil, errString("a sibling's lfs is not an object")
				}
				for _, k := range []string{"size", "sha256", "pointerSize"} {
					if _, has := lo.Get(k); !has {
						return nil, fmt.Errorf("a sibling's lfs has no %s", k)
					}
				}
			}
		}
	}
	if st := o.Value("safetensors"); pyjson.Truthy(st) {
		so, ok := st.(*pyjson.Object)
		if !ok {
			return nil, errString("safetensors is not an object")
		}
		for _, k := range []string{"parameters", "total"} {
			if _, has := so.Get(k); !has {
				return nil, fmt.Errorf("safetensors has no %s", k)
			}
		}
	}
	if pyjson.Truthy(o.Value("evalResults")) {
		return nil, errString("the model info holds eval results, which this binary does not read")
	}
	switch s := o.Value("sha").(type) {
	case nil, string:
		return s, nil
	}
	return nil, errString("the model info's sha is not text")
}

// hubDateOK is parse_datetime reading d without raising, for the one form
// the Hub writes.
func hubDateOK(d any) bool {
	s, ok := d.(string)
	if !ok {
		return false
	}
	m := hubDate.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	_, err := time.Parse("2006-01-02T15:04:05", s[:19])
	return err == nil
}
