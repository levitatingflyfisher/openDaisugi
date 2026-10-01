// Package registry is the oracle's git-backed pathway registry
// (opendaisugi/git_pathway_store.py): a local clone of a shared git
// repository whose pathways/ directory holds signed bundles, with the
// local SQLite pathway store as its cache.
//
// git runs as a child process, as the oracle runs it: `git -C <repo> ...`
// with this process's environment less every GIT_* variable, a ceiling at
// the directory above the clone, and its output captured.
package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/bundle"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
	"daisugi-verify/internal/signing"
)

// PathwaysSubdir and TrustedSignersFile are the layout names.
const (
	PathwaysSubdir     = "pathways"
	TrustedSignersFile = "trusted-signers.json"
)

// ErrUnread is input this binary cannot read the way the oracle reads it
// (a bundle file in YAML the port does not model). The command refuses it
// before anything is written.
var ErrUnread = errors.New("unread")

// Unread wraps ErrUnread with the reason.
type Unread struct{ Why string }

func (u *Unread) Error() string { return u.Why }
func (u *Unread) Unwrap() error { return ErrUnread }

// Git runs git the way the oracle's _git does.
type Git struct {
	Environ []string
}

// Scrubbed is environ without any GIT_* variable (the oracle's git_env):
// an inherited GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_CONFIG_* or
// GIT_SSH_COMMAND must not point git at another repository or change
// what it runs.
func Scrubbed(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// ceiling is str(Path(repo).resolve().parent): git looks for a repository
// in repo itself and never in a directory above it.
func ceiling(repo string) string {
	p, err := filepath.Abs(repo)
	if err != nil {
		p = repo
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Dir(p)
}

// Result is a finished git run.
type Result struct {
	Code           int
	Stdout, Stderr string
}

// Run is subprocess.run(["git", "-C", repo, *args], capture_output=True).
// A missing git is FileNotFoundError, as in the oracle.
func (g Git) Run(repo string, args ...string) (Result, error) {
	path, err := lookPath("git", g.env("PATH"))
	if err != nil {
		return Result{}, &signing.PyError{Type: "FileNotFoundError", Msg: "[Errno 2] No such file or directory: 'git'"}
	}
	cmd := exec.Command(path, append([]string{"-C", repo}, args...)...)
	cmd.Args[0] = "git"
	cmd.Env = append(Scrubbed(g.Environ), "GIT_CEILING_DIRECTORIES="+ceiling(repo))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code := 0
	if err != nil {
		var x *exec.ExitError
		if !errors.As(err, &x) {
			return Result{}, err
		}
		code = x.ExitCode()
	}
	return Result{Code: code, Stdout: out.String(), Stderr: errb.String()}, nil
}

func (g Git) env(k string) string {
	for _, kv := range g.Environ {
		if strings.HasPrefix(kv, k+"=") {
			return kv[len(k)+1:]
		}
	}
	return ""
}

func lookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// CalledProcessError is the oracle's CalledProcessError for a git run
// with check=True.
func CalledProcessError(args []string, code int) *signing.PyError {
	return &signing.PyError{Type: "subprocess.CalledProcessError",
		Msg: fmt.Sprintf("Command '%s' returned non-zero exit status %d.", pystr.ReprList(args), code)}
}

// Store is GitPathwayStore.
type Store struct {
	RepoPath      string // as pathlib prints it
	Priv, Pub     *string
	Publisher     string
	RequireSigned bool
	OfflineOK     bool
	Git           Git

	cache      *pathways.Store
	trustPath  string
	putPathway func(p *pathways.Pathway) error
}

// Options are GitPathwayStore's keyword arguments.
type Options struct {
	Priv, Pub     *string
	Publisher     string
	RequireSigned bool
	Environ       []string
}

// Open is GitPathwayStore(repo_path=...): the repository must exist; the
// cache (repo/.cache/pathways.db) is made, and the bundles already in the
// clone are put in it. The count put is dropped, as the constructor drops
// it.
func Open(repoPath string, o Options) (*Store, error) {
	s := &Store{RepoPath: gateroot.PathStr(repoPath), Priv: o.Priv, Pub: o.Pub, Publisher: o.Publisher,
		RequireSigned: o.RequireSigned, OfflineOK: true, Git: Git{Environ: o.Environ}}
	if s.Publisher == "" {
		s.Publisher = "opendaisugi-instance"
	}
	if _, err := os.Stat(s.RepoPath); err != nil {
		return nil, &signing.PyError{Type: "ValueError", Msg: fmt.Sprintf(
			"GitPathwayStore: repo_path %s does not exist; clone it first with `daisugi registry init`", s.RepoPath)}
	}
	// The bundles are read before the cache is made, so YAML this binary
	// cannot read stops the command before it writes anything.
	found, err := s.readBundles()
	if err != nil {
		return nil, err
	}
	cacheDir := gateroot.Join(s.RepoPath, ".cache")
	if err := mkdirParents(cacheDir); err != nil {
		return nil, err
	}
	st, err := pathways.Open(gateroot.Join(cacheDir, "pathways.db"))
	if err != nil {
		return nil, err
	}
	s.cache = st
	s.trustPath = gateroot.Join(cacheDir, TrustedSignersFile)
	s.putPathway = st.PutPathway
	if _, err := s.materialize(found); err != nil {
		st.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the cache.
func (s *Store) Close() error {
	if s.cache == nil {
		return nil
	}
	return s.cache.Close()
}

// mkdirParents is Path.mkdir(parents=True, exist_ok=True).
func mkdirParents(d string) error {
	if err := os.MkdirAll(d, 0o777); err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && errors.Is(err, os.ErrExist) {
			return &signing.PyError{Type: "FileExistsError", Msg: fmt.Sprintf("[Errno 17] File exists: '%s'", d)}
		}
		return err
	}
	return nil
}

func (s *Store) pathwaysDir() string { return gateroot.Join(s.RepoPath, PathwaysSubdir) }

// TrustedSigners is _load_trusted_signers: the distinct str values of the
// local anchor file, or none when it is absent, not JSON, or not an
// object. The in-repo trusted-signers.json is never read.
func (s *Store) TrustedSigners() ([]string, error) {
	raw, err := os.ReadFile(s.trustPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, &Unread{s.trustPath + " is not UTF-8"}
	}
	v, derr := pyjson.LoadsPy(string(raw), 900)
	if derr != nil {
		return nil, nil
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, k := range o.Keys() {
		if str, ok := o.Value(k).(string); ok && !seen[str] {
			seen[str] = true
			out = append(out, str)
		}
	}
	return out, nil
}

// yamlNames is sorted(pathways_dir.glob("*.yaml")): every entry whose name
// ends in .yaml, dot names included.
func (s *Store) yamlNames() ([]string, error) {
	st, err := os.Stat(s.pathwaysDir())
	if err != nil || !st.IsDir() {
		return nil, nil
	}
	ents, err := os.ReadDir(s.pathwaysDir())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// found is one bundle file read: its validated dump, or nil when the
// oracle skips it (unparseable or invalid).
type found struct {
	name   string
	bundle *pyjson.Object
}

func (s *Store) readBundles() ([]found, error) {
	if _, err := os.Stat(s.pathwaysDir()); err != nil {
		return nil, nil
	}
	names, err := s.yamlNames()
	if err != nil {
		return nil, err
	}
	var out []found
	for _, n := range names {
		p := gateroot.Join(s.pathwaysDir(), n)
		raw, err := os.ReadFile(p)
		if err != nil || !utf8.Valid(raw) {
			// read_text raises (a directory, bytes that are not UTF-8),
			// which the oracle catches and skips.
			out = append(out, found{name: n})
			continue
		}
		text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
		v, exc, why := pyyaml.Load(text)
		if why != nil || (exc == nil && !pyyaml.Plain(v)) {
			// A value this binary does not model is skipped, as the oracle
			// skips a file it cannot parse: a bundle is never admitted on
			// a reading the oracle might not share (L-3).
			out = append(out, found{name: n})
			continue
		}
		if exc != nil {
			out = append(out, found{name: n})
			continue
		}
		b, verr := bundle.Validate(v)
		if verr != nil {
			out = append(out, found{name: n})
			continue
		}
		out = append(out, found{name: n, bundle: b})
	}
	return out, nil
}

// materialize is _materialize_local_bundles over bundles already read.
func (s *Store) materialize(bundles []found) (int, error) {
	if _, err := os.Stat(s.pathwaysDir()); err != nil {
		return 0, nil
	}
	trusted, err := s.TrustedSigners()
	if err != nil {
		return 0, err
	}
	existing, err := s.cache.ReadAll(nil)
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, p := range existing {
		have[p.ID()] = true
	}
	var trustArg []string
	if s.RequireSigned {
		trustArg = trusted
		if trustArg == nil {
			trustArg = []string{}
		}
	}
	n := 0
	for _, f := range bundles {
		if f.bundle == nil {
			continue
		}
		pw, perr := bundle.FromBundle(f.bundle, trustArg, s.RequireSigned)
		if perr != nil {
			continue
		}
		p := &pathways.Pathway{Obj: pw}
		if have[p.ID()] {
			continue
		}
		if err := s.putPathway(p); err != nil {
			return n, err
		}
		have[p.ID()] = true
		n++
	}
	return n, nil
}

// Pull is pull(): `git pull --ff-only` (a failure tolerated, the store
// being offline-tolerant), then the bundles now in the clone put in the
// cache. It returns how many were new.
func (s *Store) Pull() (int, error) {
	if _, err := s.Git.Run(s.RepoPath, "pull", "--ff-only"); err != nil {
		return 0, err
	}
	found, err := s.readBundles()
	if err != nil {
		return 0, err
	}
	return s.materialize(found)
}

// Publish is publish(pathway, push=push): the bundle signed and written
// to pathways/<hash>.yaml, added, committed, pushed when asked (a push
// failure tolerated), and the pathway put in the cache. It returns the
// bundle hash.
func (s *Store) Publish(p *pathways.Pathway, publishedAt float64, push bool) (string, error) {
	if s.Priv == nil {
		return "", &signing.PyError{Type: "ValueError", Msg: "GitPathwayStore.publish requires private_key_b64 at " +
			"construction; the bundle must be signed."}
	}
	b, perr := bundle.ToBundle(p.Obj, s.Publisher, publishedAt, s.Priv, s.Pub)
	if perr != nil {
		return "", perr
	}
	text, why := pyyaml.SafeDump(b)
	if why != nil {
		return "", &Unread{"the bundle holds a value this binary does not write as YAML (" + why.Why + ")"}
	}
	if err := mkdirParents(s.pathwaysDir()); err != nil {
		return "", err
	}
	hash := b.Value("bundle_hash").(string)
	rel := PathwaysSubdir + "/" + hash + ".yaml"
	if err := os.WriteFile(gateroot.Join(s.RepoPath, rel), []byte(text), 0o666); err != nil {
		return "", err
	}
	if r, err := s.Git.Run(s.RepoPath, "add", rel); err != nil {
		return "", err
	} else if r.Code != 0 {
		return "", CalledProcessError([]string{"git", "-C", s.RepoPath, "add", rel}, r.Code)
	}
	msg := fmt.Sprintf("publish pathway %s (bundle %s)", p.ID(), hash[:12])
	if r, err := s.Git.Run(s.RepoPath, "commit", "-m", msg); err != nil {
		return "", err
	} else if r.Code != 0 {
		return "", CalledProcessError([]string{"git", "-C", s.RepoPath, "commit", "-m", msg}, r.Code)
	}
	if push {
		if _, err := s.Git.Run(s.RepoPath, "push"); err != nil {
			return "", err
		}
	}
	if err := s.putPathway(p); err != nil {
		return "", err
	}
	return hash, nil
}

// Status is status(): the registry's diagnostic fields, in order.
func (s *Store) Status() (*pyjson.Object, error) {
	commit := ""
	r, err := s.Git.Run(s.RepoPath, "rev-parse", "HEAD")
	var pe *signing.PyError
	switch {
	case errors.As(err, &pe) && pe.Type == "FileNotFoundError":
		commit = "(git binary missing)"
	case err != nil:
		return nil, err
	default:
		commit = pystr.Strip(r.Stdout)
	}
	names, err := s.yamlNames()
	if err != nil {
		return nil, err
	}
	cached, err := s.cache.ReadAll(nil)
	if err != nil {
		return nil, err
	}
	trusted, err := s.TrustedSigners()
	if err != nil {
		return nil, err
	}
	return pyjson.NewObject().
		Set("repo_path", s.RepoPath).
		Set("head_commit", commit).
		Set("bundle_files", len(names)).
		Set("cached_pathways", len(cached)).
		Set("trusted_signers", len(trusted)).
		Set("publisher", s.Publisher).
		Set("signing_configured", s.Priv != nil), nil
}
