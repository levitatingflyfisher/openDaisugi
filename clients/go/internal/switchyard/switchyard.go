// Package switchyard runs NVIDIA NeMo Switchyard as the gateway's
// external model chooser: opendaisugi.router_switchyard. It finds the
// switchyard-server binary, writes its TOML deployment file, starts and
// stops it as a managed child on loopback, and probes its health. The
// gateway stays the meter.
package switchyard

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/netproxy"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Facts pinned from NVIDIA-NeMo/Switchyard v0.3.0, as the oracle pins them.
const (
	BinaryName   = "switchyard-server"
	PinnedTag    = "v0.3.0"
	InstallCmd   = "cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard --tag " + PinnedTag + " switchyard-server"
	HealthPath   = "/health"
	DefaultHost  = "127.0.0.1"
	DefaultPort  = 4000
	ForwardLogin = "forwards your own login, as the gateway does"
	NoCredential = "no credential: a local host"
	anthropicAPI = "https://api.anthropic.com"
	defaultLocal = "http://127.0.0.1:11434"
)

// LookPath is shutil.which over a PATH value.
func LookPath(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		if syscall.Access(p, 1) != nil {
			continue
		}
		return p
	}
	return ""
}

// PrerequisiteProblem is check_prerequisite: "" when the binary is found.
func PrerequisiteProblem(binary string) string {
	if binary != "" {
		return ""
	}
	return BinaryName + " is not on PATH. The Switchyard proxy is a Rust binary, not a pip package. " +
		"It needs a Rust toolchain: https://rust-lang.org/tools/install. Install it with: " + InstallCmd
}

// Version is binary_version: `switchyard-server --version`, stripped, or
// "" when it will not run.
func Version(binary string) string {
	if binary == "" {
		return ""
	}
	cmd := exec.Command(binary, "--version")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return ""
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return ""
	}
	return pystr.Strip(pystr.DecodeReplace(out))
}

// Targets is SwitchyardTargets.
type Targets struct {
	CapableID, EfficientID            string
	EfficientFormat, EfficientBaseURL string
	CapableFormat, CapableBaseURL     string
	EfficientLocal                    bool
	APIKeyEnv                         string // "" for none
}

// Config is the part of config.yaml the router reads.
type Config struct {
	RouteID, CapableModel string
	EfficientModel        *string
	APIKeyEnv             *string
	LLMBaseURL            *string
	LLMHostKind           *string
}

func openAIRoot(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/v1") {
		return b
	}
	return b + "/v1"
}

// TargetsFromConfig is targets_from_config; nil when no efficient model
// is set.
func TargetsFromConfig(c Config) *Targets {
	if c.EfficientModel == nil || *c.EfficientModel == "" {
		return nil
	}
	eff := *c.EfficientModel
	key := ""
	if c.APIKeyEnv != nil {
		key = *c.APIKeyEnv
	}
	t := &Targets{CapableID: c.CapableModel, EfficientID: eff, CapableFormat: "anthropic_messages",
		CapableBaseURL: anthropicAPI, EfficientLocal: true, APIKeyEnv: key}
	if strings.HasPrefix(eff, "claude-") {
		t.EfficientFormat, t.EfficientBaseURL, t.EfficientLocal = "anthropic_messages", anthropicAPI, false
		return t
	}
	base, kind := defaultLocal, "ollama"
	if c.LLMBaseURL != nil && *c.LLMBaseURL != "" {
		base = *c.LLMBaseURL
		kind = ""
		if c.LLMHostKind != nil {
			kind = *c.LLMHostKind
		}
	}
	if kind == "anthropic" {
		t.EfficientFormat, t.EfficientBaseURL = "anthropic_messages", strings.TrimRight(base, "/")
	} else {
		t.EfficientFormat, t.EfficientBaseURL = "openai_chat", openAIRoot(base)
	}
	return t
}

func tomlStr(s string) string { return pyjson.Dumps(s, true) }

func keyMode(env string) string { return "uses " + env + ": billed per token to that key" }

// Auth is how each tier authenticates upstream.
type Auth struct{ Capable, Efficient string }

// AuthModes is client_auth_modes.
func AuthModes(t *Targets) Auth {
	cloud := ForwardLogin
	if t.APIKeyEnv != "" {
		cloud = keyMode(t.APIKeyEnv)
	}
	eff := cloud
	if t.EfficientLocal {
		eff = NoCredential
	}
	return Auth{cloud, eff}
}

func clientLines(name, format, base, auth string, none bool) []string {
	lines := []string{"[llm_clients." + name + "]", "format = " + tomlStr(format), "base_url = " + tomlStr(base)}
	switch {
	case none:
	case auth == "forward":
		lines = append(lines, "forward_auth = true")
	default:
		lines = append(lines, "api_key_env = "+tomlStr(auth))
	}
	return append(lines, "")
}

// Render is render_switchyard_toml with the default picker and
// threshold; keyPresent checks a named key variable.
func Render(t *Targets, routeID string, keyPresent func(string) bool) (string, error) {
	if routeID == "" {
		return "", errors.New("the route id must not be empty")
	}
	if t.CapableID == t.EfficientID {
		return "", fmt.Errorf("the capable and efficient tiers name the same model %s. Pick a different efficient model.",
			pystr.Repr(t.CapableID))
	}
	if t.APIKeyEnv != "" && !keyPresent(t.APIKeyEnv) {
		return "", fmt.Errorf("switchyard_api_key_env names %s, which is not set. Export it before the gateway starts, "+
			"or clear the field to forward your login.", t.APIKeyEnv)
	}
	cloud := "forward"
	if t.APIKeyEnv != "" {
		cloud = t.APIKeyEnv
	}
	lines := []string{"schema_version = 1", ""}
	lines = append(lines, clientLines("capable", t.CapableFormat, t.CapableBaseURL, cloud, false)...)
	lines = append(lines, clientLines("efficient", t.EfficientFormat, t.EfficientBaseURL, cloud, t.EfficientLocal)...)
	lines = append(lines, "[targets.capable]", "id = "+tomlStr(t.CapableID), `llm_client = "capable"`, "")
	lines = append(lines, "[targets.efficient]", "id = "+tomlStr(t.EfficientID), `llm_client = "efficient"`, "")
	lines = append(lines, "[routes.daisugi]", "id = "+tomlStr(routeID), `type = "stage_router"`)
	lines = append(lines, `capable_target = "capable"`, `efficient_target = "efficient"`)
	lines = append(lines, `picker = "efficient_first"`, "confidence_threshold = 0.5")
	return strings.Join(lines, "\n") + "\n", nil
}

// WriteConfig is write_switchyard_config: <dir>/<name>, mode 0600.
func WriteConfig(dir, text, name string) (string, error) {
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, os.Chmod(path, 0o600)
}

// MeterError is SwitchyardConfigError.
type MeterError struct{ Msg string }

func (e *MeterError) Error() string { return e.Msg }

var tierKeys = [][2]string{{"capable_target", "efficient_target"}, {"strong_target", "weak_target"}}

func readTOML(path string) (*Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTOML(string(raw))
}

func findRoute(doc *Table, routeID string) *Table {
	routes, _ := doc.Vals["routes"].(*Table)
	if routes == nil {
		return nil
	}
	for _, k := range routes.Keys {
		if r, ok := routes.Vals[k].(*Table); ok {
			if id, ok := r.Vals["id"].(string); ok && id == routeID {
				return r
			}
		}
	}
	return nil
}

// RouteTargets is route_targets_from_toml: the (capable, efficient) ids
// of the route. A file outside this reader's TOML is ErrTOML.
func RouteTargets(path, routeID string) (string, string, error) {
	doc, err := readTOML(path)
	if err != nil {
		if errors.Is(err, ErrTOML) {
			return "", "", err
		}
		return "", "", &MeterError{fmt.Sprintf("cannot read %s: %s", path, pyOSError(err))}
	}
	_, routesOK := doc.Vals["routes"].(*Table)
	_, targetsOK := doc.Vals["targets"].(*Table)
	if !routesOK || !targetsOK {
		return "", "", &MeterError{fmt.Sprintf("%s has no route with id %s", path, pystr.Repr(routeID))}
	}
	route := findRoute(doc, routeID)
	if route == nil {
		return "", "", &MeterError{fmt.Sprintf("%s has no route with id %s", path, pystr.Repr(routeID))}
	}
	var pair [2]string
	found := false
	for _, tk := range tierKeys {
		_, a := route.Vals[tk[0]]
		_, b := route.Vals[tk[1]]
		if a && b {
			pair, found = tk, true
			break
		}
	}
	if !found {
		return "", "", &MeterError{fmt.Sprintf("route %s in %s does not name two tiers. The gateway meters "+
			"capable_target and efficient_target, or strong_target and weak_target.", pystr.Repr(routeID), path)}
	}
	targets := doc.Vals["targets"].(*Table)
	var ids [2]string
	for i, key := range pair {
		name := route.Vals[key]
		var target *Table
		if s, ok := name.(string); ok {
			target, _ = targets.Vals[s].(*Table)
		}
		id, ok := "", false
		if target != nil {
			id, ok = target.Vals["id"].(string)
		}
		if !ok {
			return "", "", &MeterError{fmt.Sprintf("route %s in %s names target %s, which is missing",
				pystr.Repr(routeID), path, pyRepr(name))}
		}
		ids[i] = id
	}
	if ids[0] == ids[1] {
		return "", "", &MeterError{fmt.Sprintf("route %s in %s uses the model %s for both tiers",
			pystr.Repr(routeID), path, pystr.Repr(ids[0]))}
	}
	return ids[0], ids[1], nil
}

// pyRepr is repr() of a TOML value.
func pyRepr(v any) string {
	switch x := v.(type) {
	case string:
		return pystr.Repr(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pyjson.FloatRepr(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Table:
		parts := make([]string, len(x.Keys))
		for i, k := range x.Keys {
			parts[i] = pystr.Repr(k) + ": " + pyRepr(x.Vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "None"
}

// pyOSError is str(OSError) for a read that failed.
func pyOSError(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		var errno syscall.Errno
		if errors.As(pe.Err, &errno) {
			return fmt.Sprintf("[Errno %d] %s: %s", int(errno), strerror(errno), pystr.Repr(pe.Path))
		}
	}
	return err.Error()
}

func strerror(e syscall.Errno) string {
	s := e.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// PyOSError is str(exc) for an OSError from reading a file.
func PyOSError(err error) string { return pyOSError(err) }

// AuthFromTOML is client_auth_from_toml; nil when the file or the route
// cannot be read.
func AuthFromTOML(path, routeID string) *Auth {
	doc, err := readTOML(path)
	if err != nil {
		return nil
	}
	routes, ok := doc.Vals["routes"].(*Table)
	if !ok {
		return nil
	}
	var route *Table
	for _, k := range routes.Keys {
		r, ok := routes.Vals[k].(*Table)
		if !ok {
			return nil
		}
		if id, has := r.Vals["id"]; has && id == any(routeID) {
			route = r
			break
		}
	}
	if route == nil {
		return nil
	}
	clients, has := doc.Vals["llm_clients"]
	if !has {
		clients = newTable()
	}
	ct, ok := clients.(*Table)
	if !ok {
		return nil
	}
	var pair [2]string
	found := false
	for _, tk := range tierKeys {
		_, a := route.Vals[tk[0]]
		_, b := route.Vals[tk[1]]
		if a && b {
			pair, found = tk, true
			break
		}
	}
	if !found {
		return nil
	}
	targets, ok := doc.Vals["targets"].(*Table)
	if !ok {
		return nil
	}
	modes := [2]string{}
	for i, key := range pair {
		name, ok := route.Vals[key].(string)
		if !ok {
			return nil
		}
		target, ok := targets.Vals[name].(*Table)
		if !ok {
			return nil
		}
		cname, ok := target.Vals["llm_client"].(string)
		if !ok {
			return nil
		}
		var client *Table
		if c, has := ct.Vals[cname]; has {
			if client, ok = c.(*Table); !ok {
				return nil
			}
		} else {
			client = newTable()
		}
		if env, has := client.Vals["api_key_env"]; has && truthy(env) {
			modes[i] = keyMode(pyStr(env))
		} else if client.Vals["forward_auth"] == true {
			modes[i] = ForwardLogin
		} else {
			modes[i] = "no credential"
		}
	}
	return &Auth{modes[0], modes[1]}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case *Table:
		return len(x.Keys) > 0
	}
	return true
}

// pyStr is str() of a TOML value.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// Files are one child's per-port files.
type Files struct{ Config, Log, RoutingLog string }

// ChildFiles is child_files_for.
func ChildFiles(dataDir string, port int) Files {
	gw := filepath.Join(dataDir, "gateway")
	p := strconv.Itoa(port)
	return Files{filepath.Join(gw, "switchyard-"+p+".toml"), filepath.Join(gw, "switchyard-"+p+".log"),
		filepath.Join(gw, "switchyard-routing-"+p+".jsonl")}
}

// StatePath is state_path_for.
func StatePath(dataDir string, port int) string {
	return filepath.Join(dataDir, "gateway", "switchyard-"+strconv.Itoa(port)+".json")
}

// State is a state file as read_state reads it.
type State struct {
	Obj  *pyjson.Object
	PID  int64
	Host string
	Port int64
}

// ReadState is read_state: nil when the file is absent, not JSON, or
// pid, host or port has the wrong type.
func ReadState(path string) *State {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, exc := pystr.DecodeStrict(raw)
	if exc != nil {
		return nil
	}
	v, derr := pyjson.LoadsPy(text, 9990)
	if derr != nil {
		return nil
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil
	}
	pid, ok1 := o.Value("pid").(pyjson.Int)
	port, ok2 := o.Value("port").(pyjson.Int)
	host, ok3 := o.Value("host").(string)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	pn, err1 := strconv.ParseInt(pid.Text, 10, 64)
	qn, err2 := strconv.ParseInt(port.Text, 10, 64)
	if err1 != nil || err2 != nil {
		// A pid or port past 64 bits names no process; read as the oracle
		// reads it, the state is valid but its pid is not running.
		pn, qn = math.MaxInt64, math.MaxInt64
	}
	return &State{Obj: o, PID: pn, Host: host, Port: qn}
}

// StateFile is one listed state file.
type StateFile struct {
	Path  string
	State *State
}

// ListStates is list_states: every gateway/switchyard-*.json, sorted.
func ListStates(dataDir string) []StateFile {
	matches, _ := filepath.Glob(filepath.Join(globEscape(filepath.Join(dataDir, "gateway")), "switchyard-*.json"))
	var out []StateFile
	for _, m := range matches {
		out = append(out, StateFile{m, ReadState(m)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func globEscape(s string) string {
	r := strings.NewReplacer("*", `\*`, "?", `\?`, "[", `\[`, `\`, `\\`)
	return r.Replace(s)
}

// ProbeHealth is probe_health: GET /health answers 200 within 0.5 s,
// through a proxy as urllib.request.urlopen picks one.
func ProbeHealth(host string, port int64) bool {
	rules := netproxy.UrllibFromVars(netproxy.FromEnviron(os.Environ()), true)
	c := &http.Client{Timeout: 500 * time.Millisecond,
		Transport: netproxy.Transport(rules, &http.Transport{DisableCompression: true})}
	req, err := http.NewRequest("GET", "http://"+hostPort(host, port)+HealthPath, nil)
	if err != nil {
		return false
	}
	// urllib's request, as the oracle's probe sends it.
	req.Header.Set("Accept-Encoding", "identity")
	req.Close = true
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func hostPort(host string, port int64) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.FormatInt(port, 10)
}

func procCmdline(pid int64) (string, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.FormatInt(pid, 10) + "/cmdline")
	if err != nil {
		return "", false
	}
	return pystr.Strip(pystr.DecodeReplace([]byte(strings.ReplaceAll(string(raw), "\x00", " ")))), true
}

func killZero(pid int64) bool {
	if pid <= 0 || pid > math.MaxInt32 {
		return false
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ChildIsRunning is child_is_running.
func ChildIsRunning(pid int64) bool {
	if line, ok := procCmdline(pid); ok {
		return strings.Contains(line, BinaryName)
	}
	return killZero(pid)
}

// Stop is stop_switchyard for a state file this process did not start.
func Stop(statePath string) string {
	if _, err := os.Stat(statePath); err != nil {
		return "no switchyard-server state file; nothing to stop"
	}
	st := ReadState(statePath)
	os.Remove(statePath)
	if st == nil {
		return fmt.Sprintf("the state file %s was unreadable; removed it", statePath)
	}
	pid := st.PID
	line, ok := procCmdline(pid)
	if ok && line == "" {
		return fmt.Sprintf("switchyard-server, pid %d, already exited", pid)
	}
	if ok && !strings.Contains(line, BinaryName) {
		return fmt.Sprintf("pid %d is not switchyard-server now; sent no signal and removed the state", pid)
	}
	if pid <= 0 || pid > math.MaxInt32 {
		return fmt.Sprintf("switchyard-server, pid %d, was already stopped", pid)
	}
	if err := syscall.Kill(int(pid), syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return fmt.Sprintf("no permission to signal pid %d; sent no signal and removed the state", pid)
		}
		return fmt.Sprintf("switchyard-server, pid %d, was already stopped", pid)
	}
	return fmt.Sprintf("sent SIGTERM to switchyard-server, pid %d", pid)
}

// Handle is a child that answered its health check.
type Handle struct {
	PID        int
	Host       string
	Port       int
	ConfigPath string
	LogPath    string
	proc       *os.Process
	exited     chan struct{}
}

// BaseURL is the child's URL.
func (h *Handle) BaseURL() string { return "http://" + h.Host + ":" + strconv.Itoa(h.Port) }

func (h *Handle) alive() bool {
	select {
	case <-h.exited:
		return false
	default:
		return true
	}
}

// spawn starts the child in its own session with its output on logPath.
// The child gets SIGTERM from the kernel when the thread that forked it
// ends, so the fork runs on a thread locked for the life of the process.
func spawn(argv []string, logPath string) (*Handle, error) {
	type result struct {
		cmd *exec.Cmd
		err error
	}
	ch := make(chan result)
	go func() {
		runtime.LockOSThread() // never unlocked: this thread lives as long as the process
		var out *os.File
		if logPath != "" {
			os.MkdirAll(filepath.Dir(logPath), 0o777)
			f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
			if err != nil {
				ch <- result{nil, err}
				select {}
			}
			out = f
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = nil
		if out != nil {
			cmd.Stdout, cmd.Stderr = out, out
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGTERM}
		err := cmd.Start()
		if out != nil {
			out.Close()
		}
		ch <- result{cmd, err}
		select {}
	}()
	r := <-ch
	if r.err != nil {
		return nil, r.err
	}
	h := &Handle{PID: r.cmd.Process.Pid, proc: r.cmd.Process, exited: make(chan struct{})}
	go func() {
		r.cmd.Wait()
		close(h.exited)
	}()
	return h, nil
}

func logTail(logPath string) string {
	if logPath == "" {
		return ""
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	var lines []string
	for _, ln := range pystr.Splitlines(pystr.DecodeReplace(raw)) {
		if pystr.Strip(ln) != "" {
			lines = append(lines, "    "+ln)
		}
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return strings.Join(lines, "\n")
}

// StartOptions are start_switchyard's arguments.
type StartOptions struct {
	ConfigPath, Binary, RoutingLog, LogPath, StatePath, RouteID string
	Port                                                        int
	Auth                                                        *Auth
	Wait, KillGrace                                             time.Duration
}

// Start is start_switchyard: the child on 127.0.0.1, healthy, or nil and
// the reason.
func Start(o StartOptions) (*Handle, string) {
	host := DefaultHost
	resolved := o.Binary
	if resolved == "" {
		resolved = BinaryName
	}
	if o.Wait == 0 {
		o.Wait = 10 * time.Second
	}
	if o.KillGrace == 0 {
		o.KillGrace = 2 * time.Second
	}
	port := int64(o.Port)
	if ProbeHealth(host, port) {
		return nil, fmt.Sprintf("something already answers on http://%s:%d%s. If it is a switchyard-server a gateway "+
			"started earlier, stop it with `daisugi router stop`. Else pick another port with --switchyard-port.",
			host, o.Port, HealthPath)
	}
	argv := []string{resolved, "--config", o.ConfigPath, "--host", host, "--port", strconv.Itoa(o.Port)}
	if o.RoutingLog != "" {
		argv = append(argv, "--routing-log-file", o.RoutingLog)
	}
	shown := strings.Join(argv, " ")
	dryRun := fmt.Sprintf("%s --config %s --dry-run", resolved, o.ConfigPath)
	h, err := spawn(argv, o.LogPath)
	if err != nil {
		return nil, fmt.Sprintf("could not start `%s`: %s", shown, pyOSError(err))
	}
	h.Host, h.Port, h.ConfigPath, h.LogPath = host, o.Port, o.ConfigPath, o.LogPath
	t0 := time.Now()
	for {
		if ProbeHealth(host, port) {
			if o.StatePath != "" {
				writeState(o.StatePath, h, o.RouteID, o.Auth)
			}
			return h, fmt.Sprintf("started `%s`, pid %d, healthy on %s", shown, h.PID, h.BaseURL())
		}
		if !h.alive() {
			tail := logTail(o.LogPath)
			where := ""
			if tail != "" {
				where = fmt.Sprintf(" Its log is %s:\n%s", o.LogPath, tail)
			}
			return nil, fmt.Sprintf("`%s`, pid %d, exited before it answered.%s\n  Check the config with: %s",
				shown, h.PID, where, dryRun)
		}
		if time.Since(t0) >= o.Wait {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.proc.Signal(syscall.SIGTERM)
	grace := time.Now().Add(o.KillGrace)
	for h.alive() && time.Now().Before(grace) {
		time.Sleep(50 * time.Millisecond)
	}
	if h.alive() {
		h.proc.Kill()
	}
	where := ""
	if o.LogPath != "" {
		where = fmt.Sprintf(" Its log is %s.", o.LogPath)
	}
	return nil, fmt.Sprintf("`%s`, pid %d, did not answer %s in %ss, so it was stopped.%s\n  Check the config with: %s",
		shown, h.PID, HealthPath, pyG(o.Wait.Seconds()), where, dryRun)
}

func pyG(x float64) string {
	return strconv.FormatFloat(x, 'g', -1, 64)
}

func writeState(path string, h *Handle, routeID string, auth *Auth) {
	os.MkdirAll(filepath.Dir(path), 0o777)
	o := pyjson.NewObject().Set("pid", h.PID).Set("host", h.Host).Set("port", h.Port).Set("config_path", h.ConfigPath)
	if h.LogPath != "" {
		o.Set("log_path", h.LogPath)
	} else {
		o.Set("log_path", nil)
	}
	if routeID != "" {
		o.Set("route_id", routeID)
	} else {
		o.Set("route_id", nil)
	}
	if auth != nil {
		o.Set("auth", pyjson.NewObject().Set("capable", auth.Capable).Set("efficient", auth.Efficient))
	} else {
		o.Set("auth", nil)
	}
	os.WriteFile(path, []byte(pyjson.Dumps(o, true)), 0o666)
}

// StopOwn is stop_own_child with a five-second wait: the child this
// gateway started, by its own handle. The state file goes only while it
// still names this pid.
func (h *Handle) StopOwn(statePath string) string {
	if st := ReadState(statePath); st != nil && st.PID == int64(h.PID) {
		os.Remove(statePath)
	}
	if !h.alive() {
		// The oracle has not reaped a child that ended on its own: its
		// signal reaches the zombie, and the wait then returns at once.
		return fmt.Sprintf("sent SIGTERM to switchyard-server, pid %d", h.PID)
	}
	if err := h.proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Sprintf("sent SIGTERM to switchyard-server, pid %d", h.PID)
	}
	select {
	case <-h.exited:
	case <-time.After(5 * time.Second):
		return fmt.Sprintf("sent SIGTERM to switchyard-server, pid %d; it still drains", h.PID)
	}
	return fmt.Sprintf("sent SIGTERM to switchyard-server, pid %d", h.PID)
}
