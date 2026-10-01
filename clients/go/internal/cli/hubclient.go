package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
)

// hubRaise is an exception huggingface_hub (or Python) raises out of
// `models pin`: the CLI does not catch it, so Python prints a traceback
// whose first exception is Class and exits 1.
type hubRaise struct{ Class, Msg string }

func (r *hubRaise) Error() string { return r.Class + ": " + r.Msg }

// pyTraceback is an uncaught exception: the traceback's head, the
// exception, exit 1.
func (e *Env) pyTraceback(class, msg string) error {
	e.errf("Traceback (most recent call last): ...\n%s: %s\n", class, msg)
	return exit(1)
}

// hubErr prints what Python prints for err: the traceback of a hubRaise,
// else this binary's refusal.
func (e *Env) hubErr(cmd string, err error) error {
	var r *hubRaise
	if errors.As(err, &r) {
		return e.pyTraceback(r.Class, r.Msg)
	}
	var p *pulledPartly
	if errors.As(err, &p) {
		e.errf("daisugi %s: %s. The Hugging Face cache %s may hold folders and a lock file from this pull.\n", cmd, p.why, p.cache)
		return exit(2)
	}
	return e.refuse(cmd, err)
}

// pulledPartly is a refusal after --pull wrote to the cache: its words do
// not say that nothing changed.
type pulledPartly struct{ why, cache string }

func (p *pulledPartly) Error() string { return p.why }

// connErr is what a failed request raises in Python. A refused connection
// is httpcore's ConnectError, the first exception of Python's traceback;
// any other failure (a name that does not resolve, TLS, a connection that
// breaks or times out) huggingface_hub names in httpx's words, which this
// binary does not carry, so it refuses.
func connErr(err error) error {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return &hubRaise{"httpcore.ConnectError", "[Errno 111] Connection refused"}
	}
	return errString("the Hub could not be reached, a failure huggingface_hub names in httpx's words, which this binary does not carry")
}

// hubResp is one answer: its status, headers and body.
type hubResp struct {
	status int
	header http.Header
	body   []byte
	url    string
}

// do is one request as huggingface_hub's httpx session sends it, its
// body read whole.
func (h *hubClient) do(method, url string, extra map[string]string) (*hubResp, error) {
	resp, err := h.send(method, url, extra)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, connErr(err)
	}
	return &hubResp{resp.StatusCode, resp.Header, b, url}, nil
}

// send is one request, its body not read; a failure is connErr's.
func (h *hubClient) send(method, url string, extra map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, errString("the Hub URL does not parse")
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "opendaisugi")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return nil, connErr(err)
	}
	return resp, nil
}

// raiseForStatus is hf_raise_for_status: a status of 400 or more raises
// HfHubHTTPError from httpx's HTTPStatusError, which the traceback names
// first. A redirect is not one this binary follows the oracle's way.
func raiseForStatus(r *hubResp) error {
	switch {
	case r.status >= 400:
		return &hubRaise{"httpx.HTTPStatusError", fmt.Sprintf("HTTP %d for url '%s'", r.status, r.url)}
	case r.status >= 300:
		return fmt.Errorf("the Hub answered HTTP %d, a redirect this binary does not follow", r.status)
	}
	return nil
}

// jsonOf is response.json().
func jsonOf(r *hubResp) (any, error) {
	text, derr := pystr.DecodeStrict(r.body)
	if derr != nil {
		return nil, errString("the Hub's answer is not UTF-8")
	}
	v, err := pyjson.LoadsStrict(text)
	if err != nil {
		return nil, &hubRaise{"json.decoder.JSONDecodeError", err.Error()}
	}
	return v, nil
}

// getJSON is session.get, hf_raise_for_status and response.json().
func (h *hubClient) getJSON(url string) (any, error) {
	r, err := h.do("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if err := raiseForStatus(r); err != nil {
		return nil, err
	}
	return jsonOf(r)
}

var linkRE = regexp.MustCompile(`<([^>]*)>([^,]*)`)

// nextPage is response.links["next"]["url"], or "".
func nextPage(h http.Header) string {
	for _, v := range h.Values("Link") {
		for _, m := range linkRE.FindAllStringSubmatch(v, -1) {
			for _, param := range strings.Split(m[2], ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.TrimSpace(k) == "rel" && strings.Trim(strings.TrimSpace(val), `"' `) == "next" {
					return strings.TrimSpace(m[1])
				}
			}
		}
	}
	return ""
}

// backoffGet is http_backoff("GET", url): a 429 or 5xx answer is asked
// again up to five times, waiting 1, 2, 4, 8 and 8 seconds (or the
// Retry-After seconds plus one), and each wait is logged on stderr by
// huggingface_hub's logger, as Python prints it.
func (h *hubClient) backoffGet(url string, e *Env) (*hubResp, error) {
	const maxRetries = 5
	sleep := 1
	for tries := 1; ; tries++ {
		r, err := h.do("GET", url, nil)
		if err != nil {
			// http_backoff retries a network error and logs it in httpx's
			// words, which this binary does not carry.
			return nil, errString("the Hub could not be reached for a next page, which huggingface_hub retries")
		}
		switch r.status {
		case 429, 500, 502, 503, 504:
		default:
			return r, nil
		}
		e.errf("HTTP Error %d thrown while requesting GET %s\n", r.status, url)
		if tries > maxRetries {
			return r, nil
		}
		reset := -1
		if r.status == 429 && r.header.Get("ratelimit") != "" {
			return nil, errString("the Hub answered with ratelimit headers, which this binary does not read")
		}
		v := strings.TrimSpace(r.header.Get("Retry-After"))
		if !isASCII(v) || (isASCIIDigits(v) && len(v) > 9) {
			// str.isdigit() reads digits of every script, and a wait this
			// long is not one this binary sleeps.
			return nil, errString("the Hub answered with a Retry-After this binary does not read")
		}
		if isASCIIDigits(v) {
			reset, _ = strconv.Atoi(v)
		}
		var wait time.Duration
		if reset >= 0 {
			e.errf("Rate limited. Waiting %s.0s before retry [Retry %d/%d].\n", strconv.Itoa(reset+1), tries, maxRetries)
			wait = time.Duration(reset+1) * time.Second
		} else {
			e.errf("Retrying in %ds [Retry %d/%d].\n", sleep, tries, maxRetries)
			wait = time.Duration(sleep) * time.Second
		}
		time.Sleep(wait)
		sleep = min(8, sleep*2)
	}
}

func isASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// treeFiles is list_repo_files: the first page of the tree with
// session.get, then each next page the Link header names with
// http_backoff, and the path of every file entry.
func (h *hubClient) treeFiles(url string, e *Env) ([]string, error) {
	r, err := h.do("GET", url, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for {
		if err := raiseForStatus(r); err != nil {
			return nil, err
		}
		v, err := jsonOf(r)
		if err != nil {
			return nil, err
		}
		files, err := treePage(v)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
		next := nextPage(r.header)
		if next == "" {
			return out, nil
		}
		if !strings.HasPrefix(next, "http://") && !strings.HasPrefix(next, "https://") {
			return nil, errString("the Hub's next page is not an absolute URL")
		}
		if r, err = h.backoffGet(next, e); err != nil {
			return nil, err
		}
	}
}

// treePage is one page of list_repo_tree, read as RepoFile(**entry) or
// RepoFolder(**entry): iterating the JSON (a dict iterates its keys, a
// str its characters), entry["type"], then the class's own fields.
func treePage(v any) ([]string, error) {
	var items []any
	switch x := v.(type) {
	case []any:
		items = x
	case *pyjson.Object:
		for _, k := range x.Keys() {
			items = append(items, k)
		}
	case string:
		for _, r := range x {
			items = append(items, string(r))
		}
	default:
		return nil, &hubRaise{"TypeError", "the tree is not iterable"}
	}
	var out []string
	for _, it := range items {
		o, ok := it.(*pyjson.Object)
		if !ok {
			return nil, &hubRaise{"TypeError", "a tree entry is not subscriptable by 'type'"}
		}
		kind, has := o.Get("type")
		if !has {
			return nil, &hubRaise{"KeyError", "'type'"}
		}
		if _, has := o.Get("self"); has {
			return nil, &hubRaise{"TypeError", "__init__() got multiple values for argument 'self'"}
		}
		need := []string{"path", "oid"}
		if kind == "file" {
			need = []string{"path", "size", "oid"}
		}
		for _, k := range need {
			if _, has := o.Get(k); !has {
				return nil, &hubRaise{"KeyError", "'" + k + "'"}
			}
		}
		lc := o.Value("lastCommit")
		if !pyjson.Truthy(lc) {
			lc = o.Value("last_commit")
		}
		if pyjson.Truthy(lc) {
			return nil, errString("a tree entry carries its last commit, which this binary does not read")
		}
		if kind != "file" {
			continue
		}
		if lfs := o.Value("lfs"); lfs != nil {
			lo, ok := lfs.(*pyjson.Object)
			if !ok {
				return nil, errString("a tree entry's lfs is not an object")
			}
			for _, k := range []string{"size", "oid", "pointerSize"} {
				if _, has := lo.Get(k); !has {
					return nil, &hubRaise{"KeyError", "'" + k + "'"}
				}
			}
		}
		if o.Value("securityFileStatus") != nil {
			return nil, errString("a tree entry carries a security status, which this binary does not read")
		}
		path, ok := o.Value("path").(string)
		if !ok {
			return nil, errString("a tree entry's path is not text")
		}
		out = append(out, path)
	}
	return out, nil
}

// hubCacheDir is constants.HF_HUB_CACHE: HF_HUB_CACHE, else
// HUGGINGFACE_HUB_CACHE, else <HF_HOME>/hub, HF_HOME defaulting to
// <XDG_CACHE_HOME or ~/.cache>/huggingface; "~" expanded. A "$" (which
// expandvars reads) is not modelled.
func (e *Env) hubCacheDir() (string, error) {
	expand := func(p string) (string, error) {
		if strings.Contains(p, "$") {
			return "", errString("a Hugging Face cache path with $, which expandvars reads")
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			return e.home + p[1:], nil
		}
		if strings.HasPrefix(p, "~") {
			return "", errString("a Hugging Face cache path with ~user")
		}
		return p, nil
	}
	if v, ok := e.env["HF_HUB_CACHE"]; ok {
		return expand(v)
	}
	if v, ok := e.env["HUGGINGFACE_HUB_CACHE"]; ok {
		return expand(v)
	}
	home, ok := e.env["HF_HOME"]
	if !ok {
		cache, ok := e.env["XDG_CACHE_HOME"]
		if !ok {
			cache = filepath.Join(e.home, ".cache")
		}
		home = filepath.Join(cache, "huggingface")
	}
	h, err := expand(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "hub"), nil
}

// pyQuote is urllib.parse.quote(s): unreserved characters and "/" kept,
// everything else %XX of its UTF-8.
func pyQuote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// normalizeEtag is _normalize_etag: a weak "W/" prefix and the quotes
// dropped.
func normalizeEtag(s string) string {
	return strings.Trim(strings.TrimLeft(s, "W/"), `"`)
}

const cachedirTag = "Signature: 8a477f597d28d172789f06886806bc55\n" +
	"# This file is a cache directory tag created by huggingface_hub.\n" +
	"# For information about cache directory tags, see:\n" +
	"#\thttps://bford.info/cachedir/\n"

var commitHashRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// hubPull is hf_hub_download(repo_id, filename, revision) into the cache:
// the HEAD of the file's resolve URL for its commit, etag and size, then
// the blob downloaded to a temporary file and moved into blobs/<etag>,
// the snapshot's relative symlink to it, refs/<revision> when the
// revision is not the commit, the lock file and CACHEDIR.TAG, as
// huggingface_hub lays them out. What it does not model (a redirect, Xet
// storage, a Hub answer it would retry or fall back on) is refused.
func (e *Env) hubPull(h *hubClient, endpoint, repo, filename, revision string) (string, error) {
	cache, err := e.hubCacheDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(cache) {
		cache = filepath.Join(e.cwd(), cache)
	}
	for _, part := range strings.Split(filename, "/") {
		if part == ".." || part == "" {
			return "", errString("a file name huggingface_hub rejects")
		}
	}
	storage := filepath.Join(cache, "models--"+strings.ReplaceAll(repo, "/", "--"))
	rel := filepath.Join(strings.Split(filename, "/")...)
	pointer := filepath.Join(storage, "snapshots", revision, rel)
	if commitHashRE.MatchString(revision) {
		if _, err := os.Stat(pointer); err == nil {
			return pointer, nil
		}
	}
	url := endpoint + "/" + repo + "/resolve/" + pyQuote(revision) + "/" + pyQuote(filename)
	r, err := h.do("HEAD", url, map[string]string{"Accept-Encoding": "identity"})
	if err != nil {
		return "", errString("the Hub could not be reached for the file, where huggingface_hub falls back to the cache")
	}
	switch {
	case r.status == 404 && r.header.Get("X-Error-Code") == "EntryNotFound" && r.header.Get("X-Repo-Commit") == "":
		return "", raiseForStatus(r)
	case r.status >= 300:
		return "", fmt.Errorf("the Hub answered the file's HEAD with HTTP %d, which this binary does not follow", r.status)
	}
	for k := range r.header {
		if strings.HasPrefix(strings.ToLower(k), "x-xet-") {
			return "", errString("the file is in Xet storage, which this binary does not read")
		}
	}
	commit := r.header.Get("X-Repo-Commit")
	etag := r.header.Get("X-Linked-Etag")
	if etag == "" {
		etag = r.header.Get("ETag")
	}
	etag = normalizeEtag(etag)
	sizeText := r.header.Get("X-Linked-Size")
	if sizeText == "" {
		sizeText = r.header.Get("Content-Length")
	}
	size, serr := strconv.ParseInt(sizeText, 10, 64)
	dots := func(v string) bool { return v == "." || v == ".." }
	if commit == "" || etag == "" || serr != nil || strings.ContainsAny(etag, "/\x00") || strings.Contains(commit, "/") ||
		dots(etag) || dots(commit) {
		return "", errString("the file's HEAD lacks a commit, an etag or a size, where huggingface_hub falls back to the cache")
	}
	blob := filepath.Join(storage, "blobs", etag)
	pointer = filepath.Join(storage, "snapshots", commit, rel)
	writeRef := func() error {
		if revision == commit {
			return nil
		}
		ref := filepath.Join(storage, "refs", revision)
		if err := os.MkdirAll(filepath.Dir(ref), 0o777); err != nil {
			return err
		}
		if old, err := os.ReadFile(ref); err == nil && string(old) == commit {
			return nil
		}
		tmp := ref + "." + randHex(4) + ".tmp"
		if err := os.WriteFile(tmp, []byte(commit), 0o666); err != nil {
			return err
		}
		return os.Rename(tmp, ref)
	}
	if _, err := os.Stat(pointer); err == nil {
		_ = writeRef()
		return pointer, nil
	}
	partly := func(err error) error { return &pulledPartly{err.Error(), cache} }
	for _, d := range []string{filepath.Dir(blob), filepath.Dir(pointer)} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			return "", partly(err)
		}
	}
	if tag := filepath.Join(cache, "CACHEDIR.TAG"); !exists(tag) {
		_ = os.WriteFile(tag, []byte(cachedirTag), 0o666)
	}
	if err := writeRef(); err != nil {
		return "", partly(err)
	}
	lock := filepath.Join(cache, ".locks", "models--"+strings.ReplaceAll(repo, "/", "--"), etag+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o777); err != nil {
		return "", partly(err)
	}
	if f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o664); err == nil {
		f.Close()
		_ = os.Chmod(lock, 0o664)
	}
	if !exists(blob) {
		if err := e.hubDownload(h, url, blob, size, filepath.Base(filename)); err != nil {
			var r *hubRaise
			if errors.As(err, &r) {
				return "", err
			}
			return "", partly(err)
		}
	}
	if !exists(pointer) {
		target, err := filepath.Rel(filepath.Dir(pointer), blob)
		if err != nil {
			return "", partly(err)
		}
		_ = os.Remove(pointer)
		if err := os.Symlink(target, pointer); err != nil {
			return "", partly(err)
		}
	}
	return pointer, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// hubDownload is http_get into a process-unique <blob>.<hex>.incomplete
// file, written as the body arrives (a file of gigabytes costs a buffer),
// checked against the HEAD's size, then moved to the blob with the mode a
// new file gets; the temporary file is removed on any failure, as
// _download_to_tmp_and_move does. Progress goes to stderr only on a
// terminal.
func (e *Env) hubDownload(h *hubClient, url, blob string, size int64, name string) error {
	tmp := blob + "." + randHex(4) + ".incomplete"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	n, resp, err := h.download(url, f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode >= 400:
		return &hubRaise{"httpx.HTTPStatusError", fmt.Sprintf("HTTP %d for url '%s'", resp.StatusCode, url)}
	case resp.StatusCode != 200:
		return fmt.Errorf("the download answered HTTP %d, which this binary does not follow", resp.StatusCode)
	case resp.Header.Get("Content-Encoding") != "" && !strings.EqualFold(resp.Header.Get("Content-Encoding"), "identity"):
		return errString("the download came compressed")
	}
	if n != size {
		shown := url
		if m := headerFilenameRE.FindStringSubmatch(resp.Header.Get("Content-Disposition")); m != nil {
			shown = m[1]
		}
		if r := []rune(shown); len(r) > 40 {
			shown = "(\u2026)" + string(r[len(r)-40:])
		}
		return &hubRaise{"OSError", fmt.Sprintf("Consistency check failed: file should be of size %d but has size %d (%s).\n"+
			"This is usually due to network issues while downloading the file. Please retry with `force_download=True`.", size, n, shown)}
	}
	if f, ok := e.Stderr.(*os.File); ok && supervise.IsTerminal(f) {
		e.errf("%s: %d/%d bytes\n", name, n, size)
	}
	return os.Rename(tmp, blob)
}

var headerFilenameRE = regexp.MustCompile(`filename="(.*?)";`)

// download is the download's GET, a 200 body copied to out as it
// arrives: the bytes written and the answer (its body closed).
func (h *hubClient) download(url string, out io.Writer) (int64, *http.Response, error) {
	resp, err := h.send("GET", url, map[string]string{"Accept-Encoding": "identity"})
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, resp, nil
	}
	var werr error
	n, err := io.Copy(writerFunc(func(b []byte) (int, error) {
		k, err := out.Write(b)
		werr = err
		return k, err
	}), resp.Body)
	switch {
	case werr != nil:
		return n, nil, werr
	case err != nil:
		return n, nil, errString("the download broke off, where huggingface_hub resumes it")
	}
	return n, resp, nil
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(b []byte) (int, error) { return w(b) }

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
