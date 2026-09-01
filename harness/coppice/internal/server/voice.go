package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/web"
)

// VoiceConfig says what the server does about voice. The zero value
// starts nothing: voice.status then says voice is not started, and
// voice.start starts it. The command line fills it from coppice.toml.
type VoiceConfig struct {
	// Off is true when coppice.toml says [voice] enabled = false, or
	// when it does not read. OffReason and OffFix then say why and what to
	// do; empty means enabled = false.
	Off       bool
	OffReason string
	OffFix    string
	// URL is a voice server that runs somewhere else. The server starts
	// none of its own when it is set.
	URL string
	// TokenFile is the bearer token file URL's server checks. Empty means
	// the web token file.
	TokenFile string
	// Args are added after the flags the server passes to daisugi voice
	// serve.
	Args []string
}

// Voice states, as voice.status reports them.
const (
	voiceOff      = "off"
	voiceIdle     = "idle"
	voiceStarting = "starting"
	voiceReady    = "ready"
	voiceDown     = "down"
)

// voiceSup starts daisugi voice serve on a loopback port, watches it, and
// stops it with the server. One process runs at most. When the process
// dies after it answered, it is started once more. When it dies again, or
// never answers, voice is down with a reason until voice.start runs.
type voiceSup struct {
	cfg     VoiceConfig
	dataDir string
	// lookPath finds daisugi. health is how long the process may take to
	// answer /health the first time: a first start can download a model.
	lookPath func(string) (string, error)
	health   time.Duration
	every    time.Duration

	mu     sync.Mutex
	state  string
	url    string
	reason string
	fix    string
	// command is the shell line that fixes a down voice, for a client to
	// type and not run, or "".
	command   string
	cmd       *exec.Cmd
	gen       int
	restarted bool
	stopping  bool
	// exited is closed when the running process's Wait returns.
	exited chan struct{}
}

func newVoiceSup(cfg VoiceConfig, dataDir string) *voiceSup {
	if !cfg.Off {
		if reason, fix := refuseVoiceConfig(cfg); reason != "" {
			cfg.Off, cfg.OffReason, cfg.OffFix = true, reason, fix
		}
	}
	v := &voiceSup{cfg: cfg, dataDir: dataDir, lookPath: exec.LookPath,
		health: 3 * time.Minute, every: 250 * time.Millisecond, state: voiceIdle}
	switch {
	case cfg.Off:
		v.state = voiceOff
	case cfg.URL != "":
		v.url = strings.TrimRight(cfg.URL, "/")
	}
	return v
}

// installVoice and upgradeVoice are the shell lines that install
// daisugi with its voice extra. A client may type them in a shell for the
// owner to read and run.
const (
	installVoice = "pip install 'opendaisugi[voice]'"
	upgradeVoice = "pip install --upgrade 'opendaisugi[voice]'"
)

// managedFlags are the flags the server passes to daisugi voice serve
// itself. The voice server listens on loopback and checks the server's
// token, always, so [voice] args may not set them.
var managedFlags = []string{"--host", "--listen", "--port", "--token-file"}

// refuseVoiceConfig says why a [voice] table cannot be used, or "" when it
// can. A url on another machine needs a token_file of its own, so the web
// sign-in token never leaves this machine. args may not set the flags the
// server sets.
func refuseVoiceConfig(cfg VoiceConfig) (reason, fix string) {
	if cfg.URL != "" && cfg.TokenFile == "" && !loopbackURL(cfg.URL) {
		return "Voice is off: [voice] url names another machine, and coppice sends it no web token.",
			"Set [voice] token_file in coppice.toml to that server's token file, then start the coppice server again"
	}
	for _, a := range cfg.Args {
		for _, f := range managedFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Sprintf("Voice is off: [voice] args sets %s, which coppice sets itself.", f),
					"Take " + f + " out of [voice] args in coppice.toml, then start the coppice server again"
			}
		}
	}
	return "", ""
}

// loopbackURL reports whether raw names localhost or a loopback address.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// voiceDir holds the token file and the log of the voice process.
func (v *voiceSup) voiceDir() string { return filepath.Join(v.dataDir, "voice") }

// tokenFile is the file whose token the voice server checks.
func (v *voiceSup) tokenFile() string {
	switch {
	case v.cfg.URL != "" && v.cfg.TokenFile != "":
		return v.cfg.TokenFile
	case v.cfg.URL != "":
		return web.TokenPath(v.dataDir)
	}
	return filepath.Join(v.voiceDir(), "token")
}

// logPath is where the voice process writes its output.
func (v *voiceSup) logPath() string { return filepath.Join(v.voiceDir(), "voice.log") }

// status is what voice.status answers. It never waits on the process.
// For a voice server that runs somewhere else it asks /health, for at
// most one second.
func (v *voiceSup) status() map[string]any {
	if v.cfg.URL != "" && !v.cfg.Off {
		out := map[string]any{"state": voiceReady, "ready": true, "url": v.url,
			"token_file": v.tokenFile(), "managed": false, "reason": "", "fix": "", "command": ""}
		if !answers(v.url, time.Second) {
			out["state"], out["ready"] = voiceDown, false
			out["reason"] = fmt.Sprintf("The voice server at %s does not answer.", v.url)
			out["fix"] = "Start it on that machine"
		}
		return out
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]any{"state": v.state, "ready": v.state == voiceReady, "url": "",
		"token_file": v.tokenFile(), "managed": true, "reason": v.reason, "fix": v.fix,
		"command": v.command}
	if v.state == voiceReady || v.state == voiceStarting {
		out["url"] = v.url
	}
	if v.cmd != nil && v.cmd.Process != nil {
		out["pid"] = v.cmd.Process.Pid
	}
	switch v.state {
	case voiceOff:
		out["reason"] = "Voice is off in coppice.toml."
		out["fix"] = "Remove [voice] enabled = false there and start the coppice server again"
		if v.cfg.OffReason != "" {
			out["reason"], out["fix"] = v.cfg.OffReason, v.cfg.OffFix
		}
	case voiceIdle:
		out["reason"] = "Voice is not started."
		out["fix"] = ""
	case voiceStarting:
		out["reason"] = "Voice is starting. The first start can take a minute."
		out["fix"] = "Wait a moment"
	}
	return out
}

// answers is true when base/health replies 200 within wait.
func answers(base string, wait time.Duration) bool {
	c := &http.Client{Timeout: wait}
	resp, err := c.Get(base + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ensure starts the voice process unless one is starting or ready, or
// voice is off or runs somewhere else. It returns at once. A start after
// voice went down begins fresh, with its one restart back.
func (v *voiceSup) ensure() {
	if v.cfg.URL != "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopping || v.state == voiceOff || v.state == voiceStarting || v.state == voiceReady {
		return
	}
	v.restarted = false
	v.beginLocked(true)
}

// down records why voice cannot run and what fixes it.
func (v *voiceSup) downLocked(reason, fix string) {
	v.state, v.reason, v.fix, v.url, v.cmd, v.command = voiceDown, reason, fix, "", nil, ""
}

// beginLocked marks voice as starting and starts it on its own goroutine.
// probe says whether to check first that daisugi can serve voice.
func (v *voiceSup) beginLocked(probe bool) {
	v.gen++
	v.state, v.reason, v.fix, v.url, v.command = voiceStarting, "", "", "", ""
	go v.start(v.gen, probe, 0)
}

// start runs one attempt. gen names the attempt, so an attempt that a
// later one replaced changes nothing. tries counts starts that lost their
// port to another program.
func (v *voiceSup) start(gen int, probe bool, tries int) {
	failWith := func(reason, fix, command string) {
		v.mu.Lock()
		defer v.mu.Unlock()
		if gen == v.gen && !v.stopping {
			v.downLocked(reason, fix)
			v.command = command
		}
	}
	fail := func(reason, fix string) { failWith(reason, fix, "") }
	bin, err := v.lookPath("daisugi")
	if err != nil {
		failWith("Voice needs daisugi, and daisugi is not on PATH.",
			"Install opendaisugi with its voice extra", installVoice)
		return
	}
	if probe {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := exec.CommandContext(ctx, bin, "voice", "serve", "--help").Run()
		cancel()
		if err != nil {
			failWith("This daisugi cannot serve voice.",
				"Update opendaisugi and install its voice extra", upgradeVoice)
			return
		}
	}
	if err := os.MkdirAll(v.voiceDir(), 0o700); err != nil {
		fail(fmt.Sprintf("Voice cannot make %s: %v.", v.voiceDir(), err), "Check that the coppice data directory is writable")
		return
	}
	tok := web.TokenStore{Path: v.tokenFile()}
	if _, err := tok.Load(); err != nil {
		if _, err := tok.Mint(); err != nil {
			fail(fmt.Sprintf("Voice has no token file: %v.", err), "Check that the coppice data directory is writable")
			return
		}
	}
	port, err := freePort()
	if err != nil {
		fail(fmt.Sprintf("Voice found no free loopback port: %v.", err), "")
		return
	}
	logf, err := os.OpenFile(v.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fail(fmt.Sprintf("Voice cannot write its log: %v.", err), "Check that the coppice data directory is writable")
		return
	}
	args := append([]string{"voice", "serve", "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--token-file", v.tokenFile()}, v.cfg.Args...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	setVoiceProcAttrs(cmd)
	url := "http://127.0.0.1:" + strconv.Itoa(port)

	v.mu.Lock()
	if gen != v.gen || v.stopping {
		v.mu.Unlock()
		logf.Close()
		return
	}
	fmt.Fprintf(logf, "coppice: starting daisugi %s\n", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		logf.Close()
		v.downLocked(fmt.Sprintf("daisugi voice serve did not start: %v.", err), "")
		v.mu.Unlock()
		return
	}
	v.cmd, v.url = cmd, url
	exited := make(chan struct{})
	v.exited = exited
	v.mu.Unlock()

	go func() {
		err := cmd.Wait()
		logf.Close()
		close(exited)
		v.onExit(gen, cmd, err, tries)
	}()
	v.waitHealthy(gen, url, exited)
}

// waitHealthy polls /health until it answers, the process exits, or the
// health wait runs out. A process that never answers is stopped.
func (v *voiceSup) waitHealthy(gen int, url string, exited <-chan struct{}) {
	deadline := time.Now().Add(v.health)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return
		case <-time.After(v.every):
		}
		if answers(url, time.Second) {
			v.mu.Lock()
			if gen == v.gen && v.state == voiceStarting {
				v.state = voiceReady
			}
			v.mu.Unlock()
			return
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if gen != v.gen || v.state != voiceStarting {
		return
	}
	cmd := v.cmd
	v.gen++
	v.downLocked("Voice did not answer in time.", "See "+v.logPath()+"")
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// onExit decides what a process that ended means. A stop meant it. A
// process that was ready starts once more. A process that lost its port
// before it bound starts again on a new one, once. Anything else is
// voice down, with the reason read from its log.
func (v *voiceSup) onExit(gen int, cmd *exec.Cmd, err error, tries int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopping || gen != v.gen || v.cmd != cmd {
		return
	}
	was := v.state
	last := lastLine(v.logPath())
	code := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	switch {
	case was == voiceReady && !v.restarted:
		v.restarted = true
		v.beginLocked(false)
	case was == voiceReady:
		v.downLocked("Voice stopped twice.", "See "+v.logPath()+"")
	case strings.Contains(last, "not installed"):
		v.downLocked("Voice needs daisugi's voice extra.",
			"Install it with "+installVoice)
		v.command = installVoice
	case strings.Contains(strings.ToLower(last), "address already in use") && tries == 0:
		v.gen++
		v.state = voiceStarting
		go v.start(v.gen, false, tries+1)
	default:
		reason := fmt.Sprintf("Voice stopped as it started (exit %d).", code)
		if last != "" {
			reason = fmt.Sprintf("Voice stopped as it started: %s", strings.TrimSuffix(last, "."))
			reason += "."
		}
		v.downLocked(reason, "See "+v.logPath()+"")
	}
}

// stop ends the voice process: an interrupt first, so it closes its
// socket, then a kill when it has not ended within two seconds. Nothing
// starts after a stop.
func (v *voiceSup) stop() {
	v.mu.Lock()
	v.stopping = true
	cmd, exited := v.cmd, v.exited
	v.mu.Unlock()
	if cmd == nil || cmd.Process == nil || exited == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}

// freePort asks the kernel for a free loopback port. The probe closes
// before daisugi binds, so another program can take the port in between;
// onExit then tries a new port once.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// lastLine is the last line of path that is not empty and that coppice
// did not write itself, or "".
func lastLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// Only the end of the file matters, and a long log need not be read.
	if st, err := f.Stat(); err == nil && st.Size() > 8192 {
		_, _ = f.Seek(st.Size()-8192, 0)
	}
	last := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "coppice: starting") {
			// Only the latest run's lines say why it stopped.
			last = ""
			continue
		}
		if line != "" && !strings.HasPrefix(line, "coppice: ") {
			last = line
		}
	}
	return last
}

// RegisterVoiceCommands wires up voice.status and voice.start.
func (s *Server) RegisterVoiceCommands() {
	_ = s.Handle("voice.status", func(_ *Client, r *proto.Request) proto.Response {
		return proto.OKResp(r.ID, s.voice.status())
	})
	_ = s.Handle("voice.start", func(_ *Client, r *proto.Request) proto.Response {
		s.voice.ensure()
		return proto.OKResp(r.ID, s.voice.status())
	})
}

// StartVoice starts the voice server when the config asks for one. The
// command line calls it once the socket listens. It returns at once.
func (s *Server) StartVoice() { s.voice.ensure() }
