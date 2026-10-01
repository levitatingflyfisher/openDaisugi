package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pyjson"
)

// opendaisugi.pack.manage: list, status, remove, install, bundle, run.
// The lines printed are Python's, byte for byte.

// Ctx is manage.Ctx. Env is the environment of every child process and of
// the proxy choice; nil is this process's.
type Ctx struct {
	DataDir string
	Cat     *Catalog
	Out     func(string)
	Err     func(string)
	Env     []string
	// SystemDir holds the system packs ("" none).
	SystemDir string
}

// SystemPacks is manage.SYSTEM_PACKS; SystemEnv names another.
const (
	SystemPacks = "/usr/lib/opendaisugi/packs"
	SystemEnv   = "OPENDAISUGI_SYSTEM_PACKS"
)

func (c *Ctx) environ() []string {
	if c.Env == nil {
		return os.Environ()
	}
	return c.Env
}

// workerSources are the files every pack's worker/ holds, in order.
var workerSources = [][2]string{
	{WorkerFile, "worker.py"},
	{"lora_train.py", "lora_train.py"},
	{"vla_oracle.py", "vla_oracle.py"},
}

var pipFlags = []string{
	"--isolated",
	"--disable-pip-version-check",
	"--no-input",
	"--no-cache-dir",
	"--quiet",
	"--require-hashes",
	"--only-binary=:all:",
}

// PackDir is manage.pack_dir.
func PackDir(c *Ctx, name string) string { return filepath.Join(c.DataDir, "packs", name) }

func shaBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func shaFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func thisPlatform() string {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64", "386": "i686"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	return arch + "-" + runtime.GOOS
}

func lexists(p string) bool { _, err := os.Lstat(p); return err == nil }

func isDir(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

func isFile(p string) bool { st, err := os.Stat(p); return err == nil && st.Mode().IsRegular() }

// ReadManifest is manage.read_manifest.
func ReadManifest(d string) *pyjson.Object {
	b, err := os.ReadFile(filepath.Join(d, "manifest.json"))
	if err != nil {
		return nil
	}
	v, err := pyjson.Loads(string(b))
	if err != nil {
		return nil
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// FindPack is manage.find_pack: the data dir's pack, else the system's.
func FindPack(c *Ctx, name string) (dir string, system, ok bool) {
	if d := PackDir(c, name); ReadManifest(d) != nil {
		return d, false, true
	}
	if c.SystemDir != "" {
		if sd := filepath.Join(c.SystemDir, name); ReadManifest(sd) != nil {
			return sd, true, true
		}
	}
	return "", false, false
}

func lookup(c *Ctx, name string, build bool) *Pack {
	p := c.Cat.Find(name)
	if p == nil {
		c.Err(fmt.Sprintf("No pack named %s.", name))
		c.Err("See: daisugi pack list")
		return nil
	}
	if build && p.GPU {
		c.Err(fmt.Sprintf("The pack %s needs a GPU build, which this release does not carry.", name))
		return nil
	}
	if build && thisPlatform() != c.Cat.Platform {
		c.Err(fmt.Sprintf("The packs of this release are for %s only.", c.Cat.Platform))
		return nil
	}
	return p
}

func unknownCode(c *Ctx, name string) int {
	if c.Cat.Find(name) == nil {
		return 2
	}
	return 1
}

// ---------------------------------------------------------------------------
// list, status, remove
// ---------------------------------------------------------------------------

// List is manage.list_packs.
func List(c *Ctx) int {
	for _, p := range c.Cat.Packs {
		state := "not installed"
		if p.GPU {
			state = "needs a GPU"
		} else if _, system, ok := FindPack(c, p.Name); ok {
			state = "installed"
			if system {
				state = "system"
			}
		}
		c.Out(fmt.Sprintf("%-14s%-15s%s", p.Name, state, p.Summary))
	}
	c.Out("")
	c.Out("Install one: daisugi pack install NAME")
	return 0
}

func objStr(o *pyjson.Object, k string) string {
	if o == nil {
		return ""
	}
	s, _ := o.Value(k).(string)
	return s
}

func objObj(o *pyjson.Object, k string) *pyjson.Object {
	if o == nil {
		return nil
	}
	v, _ := o.Value(k).(*pyjson.Object)
	return v
}

func problems(c *Ctx, p *Pack, d string, m *pyjson.Object, system bool) []string {
	var found []string
	if _, err := os.Stat(filepath.Join(d, "venv", "bin", "python")); err != nil {
		found = append(found, "the virtual environment has no python")
	}
	if w := objObj(m, "worker"); w != nil {
		for _, fname := range w.Keys() {
			want, _ := w.Value(fname).(string)
			f := filepath.Join(d, "worker", fname)
			got, err := shaFile(f)
			if !isFile(f) || err != nil || got != want {
				found = append(found, fmt.Sprintf("the worker file %s was changed", fname))
			}
		}
	}
	if proto, ok := m.Get("protocol"); !ok || proto != Protocol {
		found = append(found, fmt.Sprintf("the worker speaks %s, not %s", pyStr(proto), Protocol))
	}
	if system {
		return found
	}
	lock, _ := c.Cat.LockText(*p)
	if objStr(m, "lock_sha256") != shaBytes([]byte(lock)) {
		found = append(found, "this binary pins another lock")
	}
	if objStr(objObj(m, "python"), "sha256") != c.Cat.Python.Sha256 {
		found = append(found, "this binary pins another Python")
	}
	return found
}

// pyStr is Python's str() of a JSON value in an f-string, for the one
// message that shows a manifest value as it is.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	default:
		return pyjson.Dumps(x, true)
	}
}

// Status is manage.status; name "" is every installed pack.
func Status(c *Ctx, name string) int {
	var names []string
	if name != "" {
		if lookup(c, name, false) == nil {
			return 2
		}
		names = []string{name}
	} else {
		for _, p := range c.Cat.Packs {
			if _, _, ok := FindPack(c, p.Name); !p.GPU && ok {
				names = append(names, p.Name)
			}
		}
		if len(names) == 0 {
			c.Out("No pack is installed. See: daisugi pack list")
			return 0
		}
	}
	bad := false
	for _, n := range names {
		p := c.Cat.Find(n)
		d, system, ok := FindPack(c, n)
		if p.GPU || !ok {
			c.Out(fmt.Sprintf("%s: not installed", n))
			bad = true
			continue
		}
		m := ReadManifest(d)
		if system {
			c.Out(fmt.Sprintf("%s: provided by the system in %s", n, d))
			found := problems(c, p, d, m, true)
			for _, pr := range found {
				c.Out("  problem: " + pr)
			}
			if len(found) > 0 {
				c.Out("  fix: reinstall the system package that provides it")
				bad = true
			} else {
				c.Out("  ok")
			}
			continue
		}
		py := objObj(m, "python")
		pkgs, _ := m.Value("packages").([]any)
		c.Out(fmt.Sprintf("%s: installed in %s", n, d))
		c.Out(fmt.Sprintf("  python %s (%s)", objStr(py, "version"), objStr(py, "file")))
		sha := objStr(m, "lock_sha256")
		if len(sha) > 12 {
			sha = sha[:12]
		}
		c.Out(fmt.Sprintf("  %d packages from %s (sha256 %s)", len(pkgs), objStr(m, "lock"), sha))
		found := problems(c, p, d, m, false)
		for _, pr := range found {
			c.Out("  problem: " + pr)
		}
		if len(found) > 0 {
			c.Out(fmt.Sprintf("  fix: daisugi pack install %s --force", n))
			bad = true
		} else {
			c.Out("  ok")
		}
	}
	if bad {
		return 1
	}
	return 0
}

// Remove is manage.remove.
func Remove(c *Ctx, name string) int {
	if c.Cat.Find(name) == nil {
		lookup(c, name, false)
		return 2
	}
	d := PackDir(c, name)
	if !lexists(d) {
		c.Out(fmt.Sprintf("Nothing to remove: the pack %s is not installed.", name))
		return 0
	}
	if err := os.RemoveAll(d); err != nil {
		c.Err(fmt.Sprintf("Could not remove %s.", d))
		return 1
	}
	c.Out(fmt.Sprintf("Removed the pack %s (%s).", name, d))
	return 0
}

// ---------------------------------------------------------------------------
// The steps install and bundle share
// ---------------------------------------------------------------------------

func fetch(c *Ctx, url string, size int64) ([]byte, error) {
	rules := netproxy.UrllibFromVars(netproxy.FromEnviron(c.environ()), true)
	base := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 60 * time.Second}).DialContext,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	client := &http.Client{Transport: netproxy.Transport(rules, base)}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, stopf("Could not fetch %s: no answer.", url)
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, stopf("Could not fetch %s: no answer.", url)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, stopf("Could not fetch %s: HTTP %d.", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, stopf("Could not fetch %s: no answer.", url)
	}
	if int64(len(body)) > size {
		return nil, stopf("Could not fetch %s: larger than its pinned size of %d bytes.", url, size)
	}
	return body, nil
}

func checkHash(label, got, want string) error {
	if got != want {
		return &stop{[]string{
			fmt.Sprintf("Hash mismatch: %s has sha256 %s, the pin is %s.", label, got, want),
			"Nothing was installed.",
		}}
	}
	return nil
}

// pythonTarball is manage.python_tarball; src "" fetches it.
func pythonTarball(c *Ctx, src, label string) ([]byte, error) {
	py := c.Cat.Python
	var data []byte
	if src != "" {
		f := filepath.Join(src, py.File)
		if !isFile(f) {
			return nil, stopf("The bundle %s has no %s.", label, py.File)
		}
		var err error
		if data, err = os.ReadFile(f); err != nil {
			return nil, err
		}
	} else {
		c.Out(fmt.Sprintf("Fetching %s ...", py.File))
		var err error
		if data, err = fetch(c, py.URL, py.Size); err != nil {
			return nil, err
		}
	}
	if err := checkHash(py.File, shaBytes(data), py.Sha256); err != nil {
		return nil, err
	}
	c.Out(fmt.Sprintf("Checked %s (sha256 %s).", py.File, py.Sha256[:12]))
	return data, nil
}

func unpackPython(c *Ctx, data []byte, dest string) error {
	py := c.Cat.Python
	if err := unpackTarGz(data, dest, py.File); err != nil {
		return err
	}
	if !isFile(filepath.Join(dest, "python", "bin", "python3")) {
		return stopf("%s has no python/bin/python3.", py.File)
	}
	c.Out(fmt.Sprintf("Unpacked Python %s.", py.Version))
	return nil
}

func checkWheels(reqs []Req, wheels string) error {
	var files []string
	if isDir(wheels) {
		ents, _ := os.ReadDir(wheels)
		for _, e := range ents {
			files = append(files, e.Name())
		}
		sort.Strings(files)
	}
	keyed := map[[2]string][]string{}
	for _, f := range files {
		if n, v, ok := WheelKey(f); ok {
			keyed[[2]string{n, v}] = append(keyed[[2]string{n, v}], f)
		}
	}
	for _, r := range reqs {
		found := keyed[[2]string{r.Name, r.Version}]
		if len(found) == 0 {
			return stopf("The bundle has no wheel for %s==%s.", r.Name, r.Version)
		}
		for _, f := range found {
			got, err := shaFile(filepath.Join(wheels, f))
			if err != nil {
				return err
			}
			if !r.pins(got) {
				return &stop{[]string{
					fmt.Sprintf("Hash mismatch: wheels/%s has sha256 %s, which the lock does not pin.", f, got),
					"Nothing was installed.",
				}}
			}
		}
	}
	return nil
}

func indexFlags(p *Pack) []string {
	flags := []string{"--index-url", p.IndexURL}
	for _, e := range p.ExtraIndexURLs {
		flags = append(flags, "--extra-index-url", e)
	}
	return flags
}

func runQuiet(c *Ctx, argv []string, what string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = c.Env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return stopf("%s: cannot start %s.", what, argv[0])
	}
	if err == nil {
		return nil
	}
	var lines []string
	for _, ln := range strings.Split(strings.ToValidUTF8(errb.String()+out.String(), "�"), "\n") {
		ln = strings.TrimSuffix(ln, "\r")
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	s := &stop{[]string{fmt.Sprintf("%s (exit %d):", what, ee.ExitCode())}}
	for _, ln := range lines {
		s.lines = append(s.lines, "  "+ln)
	}
	return s
}

func openBundle(offline, scratch string) (string, error) {
	if isDir(offline) {
		return offline, nil
	}
	if isFile(offline) {
		if err := UstarExtract(offline, scratch); err != nil {
			return "", stopf("The bundle %s is not readable: %s.", offline, err.Error())
		}
		return scratch, nil
	}
	return "", stopf("The bundle %s is not a directory or a .tar file.", offline)
}

func printStop(c *Ctx, err error) {
	var s *stop
	if errors.As(err, &s) {
		for _, ln := range s.lines {
			c.Err(ln)
		}
		return
	}
	c.Err(err.Error())
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

// Install is manage.install; offline "" fetches from the network.
func Install(c *Ctx, name string, offline *string, force bool) int {
	p := lookup(c, name, true)
	if p == nil {
		return unknownCode(c, name)
	}
	lock, err := c.Cat.LockText(*p)
	if err != nil {
		c.Err(fmt.Sprintf("The lock %s is not usable: %v.", p.Lock, err))
		return 1
	}
	lockSha := shaBytes([]byte(lock))
	d := PackDir(c, name)
	if m := ReadManifest(d); m != nil && !force {
		if objStr(m, "lock_sha256") == lockSha && len(problems(c, p, d, m, false)) == 0 {
			c.Out(fmt.Sprintf("The pack %s is already installed in %s.", name, d))
			return 0
		}
		c.Out(fmt.Sprintf("The pack %s is installed from another pin; installing it again.", name))
	}
	reqs, err := ParseLock(lock)
	if err != nil {
		c.Err(fmt.Sprintf("The lock %s is not usable: %v.", p.Lock, err))
		return 1
	}
	if lexists(d) {
		os.RemoveAll(d)
	}
	if err := os.MkdirAll(d, 0o777); err != nil {
		c.Err(err.Error())
		return 1
	}
	if err := installSteps(c, p, d, lock, lockSha, reqs, offline); err != nil {
		printStop(c, err)
		os.RemoveAll(d)
		return 1
	}
	c.Out(fmt.Sprintf("Installed the pack %s. Run a job: daisugi pack run %s JOB", name, name))
	return 0
}

func installSteps(c *Ctx, p *Pack, d, lock, lockSha string, reqs []Req, offline *string) error {
	name := p.Name
	py := c.Cat.Python
	c.Out(fmt.Sprintf("Installing the pack %s in %s.", name, d))
	src, label := "", ""
	if offline != nil {
		var err error
		if src, err = openBundle(*offline, filepath.Join(d, "bundle")); err != nil {
			return err
		}
		label = *offline
	}
	data, err := pythonTarball(c, src, label)
	if err != nil {
		return err
	}
	if err := unpackPython(c, data, d); err != nil {
		return err
	}
	data = nil
	if err := runQuiet(c, []string{filepath.Join(d, "python", "bin", "python3"), "-m", "venv", filepath.Join(d, "venv")},
		"Could not make the virtual environment"); err != nil {
		return err
	}
	c.Out("Made the virtual environment.")
	if err := os.WriteFile(filepath.Join(d, "lock.txt"), []byte(lock), 0o666); err != nil {
		return err
	}
	pip := append([]string{filepath.Join(d, "venv", "bin", "python"), "-m", "pip", "install"}, pipFlags...)
	pip = append(pip, "-r", filepath.Join(d, "lock.txt"))
	if src != "" {
		if err := checkWheels(reqs, filepath.Join(src, "wheels")); err != nil {
			return err
		}
		pip = append(pip, "--no-index", "--find-links", filepath.Join(src, "wheels"))
	} else {
		pip = append(pip, indexFlags(p)...)
	}
	c.Out(fmt.Sprintf("Installing %d packages from %s ...", len(reqs), p.Lock))
	if err := runQuiet(c, pip, "pip could not install the lock"); err != nil {
		return err
	}
	c.Out(fmt.Sprintf("Installed %d packages.", len(reqs)))
	if src != "" && src == filepath.Join(d, "bundle") {
		os.RemoveAll(src)
	}
	w := filepath.Join(d, "worker")
	if err := os.Mkdir(w, 0o777); err != nil {
		return err
	}
	shas := pyjson.NewObject()
	for _, ws := range workerSources {
		body := mustAsset(ws[1])
		if err := os.WriteFile(filepath.Join(w, ws[0]), []byte(body), 0o666); err != nil {
			return err
		}
		shas.Set(ws[0], shaBytes([]byte(body)))
	}
	check := make([]any, len(p.Check))
	for i, m := range p.Check {
		check[i] = m
	}
	info := pyjson.DumpsIndent(pyjson.NewObject().Set("name", name).Set("check", check), 2, true) + "\n"
	if err := os.WriteFile(filepath.Join(w, "pack.json"), []byte(info), 0o666); err != nil {
		return err
	}
	shas.Set("pack.json", shaBytes([]byte(info)))
	got := RunJob(d, name, "selftest", nil, func(string) {}, c.Env)
	if got.Code != 0 {
		return stopf("The self-test failed: %s", got.Error)
	}
	var parts []string
	if imports := objObj(got.Result, "imports"); imports != nil {
		for _, k := range imports.Keys() {
			parts = append(parts, strings.TrimSpace(k+" "+pyStr(imports.Value(k))))
		}
	}
	c.Out("Self-test: " + strings.Join(parts, ", ") + ".")
	pkgs := make([]any, len(reqs))
	for i, r := range reqs {
		pkgs[i] = r.Name + "==" + r.Version
	}
	source := "index"
	if src != "" {
		source = "bundle"
	}
	manifest := pyjson.NewObject().
		Set("pack", name).
		Set("protocol", Protocol).
		Set("python", pyjson.NewObject().Set("version", py.Version).Set("file", py.File).Set("sha256", py.Sha256)).
		Set("lock", p.Lock).
		Set("lock_sha256", lockSha).
		Set("packages", pkgs).
		Set("source", source).
		Set("worker", shas)
	return os.WriteFile(filepath.Join(d, "manifest.json"), []byte(pyjson.DumpsIndent(manifest, 2, true)+"\n"), 0o666)
}

// ---------------------------------------------------------------------------
// bundle
// ---------------------------------------------------------------------------

// Bundle is manage.bundle.
func Bundle(c *Ctx, name, out string) int {
	p := lookup(c, name, true)
	if p == nil {
		return unknownCode(c, name)
	}
	lock, err := c.Cat.LockText(*p)
	if err != nil {
		c.Err(fmt.Sprintf("The lock %s is not usable: %v.", p.Lock, err))
		return 1
	}
	reqs, err := ParseLock(lock)
	if err != nil {
		c.Err(fmt.Sprintf("The lock %s is not usable: %v.", p.Lock, err))
		return 1
	}
	asTar := strings.HasSuffix(filepath.Base(out), ".tar")
	work := out
	if asTar {
		work = filepath.Join(filepath.Dir(out), filepath.Base(out)+".partial")
	}
	if lexists(out) || (asTar && lexists(work)) {
		c.Err(fmt.Sprintf("%s already exists. Give a new path.", out))
		return 1
	}
	if err := os.MkdirAll(work, 0o777); err != nil {
		c.Err(err.Error())
		return 1
	}
	err = bundleSteps(c, p, lock, reqs, work)
	if err == nil && asTar {
		names := []string{c.Cat.Python.File}
		sources := []string{filepath.Join(work, c.Cat.Python.File)}
		ents, _ := os.ReadDir(filepath.Join(work, "wheels"))
		for _, e := range ents {
			names = append(names, "wheels/"+e.Name())
			sources = append(sources, filepath.Join(work, "wheels", e.Name()))
		}
		if werr := UstarWriteFile(out, names, sources); werr != nil {
			err = stopf("Could not write %s: %s.", out, werr.Error())
		} else {
			os.RemoveAll(work)
		}
	}
	if err != nil {
		printStop(c, err)
		os.RemoveAll(work)
		if asTar && lexists(out) {
			os.Remove(out)
		}
		return 1
	}
	c.Out(fmt.Sprintf("Bundled the pack %s in %s: Python %s and %d wheels.", name, out, c.Cat.Python.Version, len(reqs)))
	c.Out(fmt.Sprintf("Install it with no network: daisugi pack install %s --offline %s", name, out))
	return 0
}

func bundleSteps(c *Ctx, p *Pack, lock string, reqs []Req, work string) error {
	py := c.Cat.Python
	data, err := pythonTarball(c, "", "")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, py.File), data, 0o666); err != nil {
		return err
	}
	tmp := filepath.Join(work, ".python")
	if err := os.Mkdir(tmp, 0o777); err != nil {
		return err
	}
	if err := unpackPython(c, data, tmp); err != nil {
		return err
	}
	data = nil
	if err := os.WriteFile(filepath.Join(tmp, "lock.txt"), []byte(lock), 0o666); err != nil {
		return err
	}
	pip := append([]string{filepath.Join(tmp, "python", "bin", "python3"), "-m", "pip", "download"}, pipFlags...)
	pip = append(pip, "--no-deps", "-d", filepath.Join(work, "wheels"), "-r", filepath.Join(tmp, "lock.txt"))
	pip = append(pip, indexFlags(p)...)
	c.Out(fmt.Sprintf("Fetching %d wheels from %s ...", len(reqs), p.Lock))
	if err := runQuiet(c, pip, "pip could not fetch the lock's wheels"); err != nil {
		return err
	}
	os.RemoveAll(tmp)
	if err := checkWheels(reqs, filepath.Join(work, "wheels")); err != nil {
		return err
	}
	c.Out(fmt.Sprintf("Checked %d wheels against %s.", len(reqs), p.Lock))
	return nil
}

// ---------------------------------------------------------------------------
// run, and the trainer through the train pack
// ---------------------------------------------------------------------------

func installedOrSay(c *Ctx, name string) string {
	d, _, ok := FindPack(c, name)
	if !ok {
		c.Err(fmt.Sprintf("The pack %s is not installed.", name))
		c.Err(fmt.Sprintf("Install it: daisugi pack install %s", name))
		return ""
	}
	return d
}

// Run is manage.run.
func Run(c *Ctx, name, job string, args []string) int {
	if lookup(c, name, false) == nil {
		return 2
	}
	d := installedOrSay(c, name)
	if d == "" {
		return 1
	}
	got := RunJob(d, name, job, args, c.Err, c.Env)
	if got.Code != 0 {
		c.Err(got.Error)
		return got.Code
	}
	c.Out(pyjson.DumpsIndent(got.Result, 2, true))
	return 0
}

// TrainPack is manage.TRAIN_PACK.
const TrainPack = "train"

// LoraTrain is manage.lora_train.
func LoraTrain(c *Ctx, args []string) int {
	d := installedOrSay(c, TrainPack)
	if d == "" {
		return 1
	}
	got := RunJob(d, TrainPack, "train", args, c.Err, c.Env)
	if got.Code != 0 {
		c.Err(got.Error)
		return got.Code
	}
	c.Out(fmt.Sprintf("The adapter is in %s.", pyStr(got.Result.Value("adapter"))))
	return 0
}
