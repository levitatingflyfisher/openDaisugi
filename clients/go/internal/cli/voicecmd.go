package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/voice"
)

const voiceHelp = `Usage: daisugi voice [OPTIONS] COMMAND [ARGS]...

  The voice bridge. Record anywhere, transcribe on this box, land the text
  in a pane.

Options:
  --help  Show this message and exit.

Commands:
  serve   Run the voice bridge. It answers GET /health, POST /transcribe, and POST /deliver.
  ptt     Laptop push-to-talk. Tap space to start recording. Tap it again to stop and send.
  arm     Grant PANE direct send for a time window. Without this, delivered text only previews.
  disarm  Revoke PANE's direct-send grant. Delivered text goes back to preview only.
`

func (e *Env) voiceCmd(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", voiceHelp)
		if len(args) == 0 {
			return exit(2)
		}
		return nil
	}
	switch args[0] {
	case "serve":
		return e.voiceServe(args[1:])
	case "ptt":
		return e.voicePTT(args[1:])
	case "arm":
		return e.voiceArm(args[1:])
	case "disarm":
		return e.voiceDisarm(args[1:])
	}
	e.errf("Usage: daisugi voice [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi voice --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

var voiceDataOpt = opt{names: []string{"--data-dir"}, value: true, metavar: "PATH", help: "Daisugi data directory."}
var voiceJSONOpt = opt{names: []string{"--json"}, help: "Machine-readable JSON output."}

// voiceParse reads one voice verb's arguments: exactly want positional
// arguments, named in the usage line.
func (e *Env) voiceParse(verb, argName string, args []string, opts []opt, want int, help string) (*parsed, error) {
	p, err := parseArgs(args, opts, want)
	if err == nil && !p.help && len(p.args) < want {
		err = &usageError{"Missing argument '" + argName + "'."}
	}
	if err != nil {
		var u *usageError
		if errors.As(err, &u) {
			usage := "daisugi voice " + verb + " [OPTIONS]"
			if argName != "" {
				usage += " " + argName
			}
			e.errf("Usage: %s\nTry 'daisugi voice %s --help' for help.\n\nError: %s\n", usage, verb, u.msg)
			return nil, exit(2)
		}
		return nil, err
	}
	if p.help {
		e.out("%s", help)
		return nil, exit(0)
	}
	return p, nil
}

func (e *Env) voiceDataDir(p *parsed) string {
	return gateroot.PathStr(p.str("--data-dir", filepath.Join(e.home, ".opendaisugi")))
}

func (e *Env) voiceConfig(cmd, dataDir string) (config.Config, error) {
	cfg, err := config.Load(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		return cfg, e.refuse(cmd, fmt.Errorf("%s/config.yaml does not load: %v", dataDir, err))
	}
	return cfg, nil
}

const voiceArmHelp = `Usage: daisugi voice arm [OPTIONS] PANE

  Grant PANE direct send for a time window. Without this, delivered text
  only previews.

Arguments:
  PANE  The pane id to grant direct-send to.  [required]

Options:
  --for TEXT       How long, for example 30m or 2h.  [default: 30m]
  --data-dir PATH  Daisugi data directory.
  --json           Machine-readable JSON output.
  --help           Show this message and exit.
`

// armMinutes is cli._validated_arm_minutes: a number of minutes, or one
// that ends in m or h, finite, above zero and at most 7 days.
func armMinutes(spec string) (float64, string) {
	s := pystr.Lower(pystr.Strip(spec))
	var m float64
	var ok bool
	switch {
	case strings.HasSuffix(s, "m"):
		m, ok = voice.PyFloat(s[:len(s)-1])
	case strings.HasSuffix(s, "h"):
		m, ok = voice.PyFloat(s[:len(s)-1])
		m *= 60.0
	default:
		m, ok = voice.PyFloat(s)
	}
	if !ok {
		return 0, fmt.Sprintf("--for must be a number of minutes, or end with m or h, for example 30m or 2h. Got %s.", pystr.Repr(spec))
	}
	if math.IsInf(m, 0) || math.IsNaN(m) || !(m > 0 && m <= 7*24*60.0) {
		return 0, fmt.Sprintf("--for must be a positive number of minutes, up to 7 days. Got %s.", pystr.Repr(spec))
	}
	return m, ""
}

func nowFloat() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (e *Env) voiceArm(args []string) error {
	opts := []opt{{names: []string{"--for"}, value: true, metavar: "TEXT"}, voiceDataOpt, voiceJSONOpt}
	p, err := e.voiceParse("arm", "PANE", args, opts, 1, voiceArmHelp)
	if err != nil {
		return err
	}
	pane := p.args[0]
	minutes, msg := armMinutes(p.str("--for", "30m"))
	if msg != "" {
		e.errf("%s\n", msg)
		return exit(1)
	}
	dataDir := e.voiceDataDir(p)
	if _, err := e.voiceConfig("voice arm", dataDir); err != nil {
		return err
	}
	armedDir := voice.ArmedDir(dataDir)
	entry, err := voice.Arm(pane, minutes, armedDir, nowFloat())
	if err != nil {
		e.errf("Could not write the grant under %s. Check the directory, or pass --data-dir.\n", armedDir)
		return exit(1)
	}
	if p.flag("--json") {
		e.out("{\"pane\": %s, \"expires_at\": %s}\n", pyjson.Dumps(pane, true), pyFloatRepr(entry.ExpiresAt))
		return nil
	}
	until := time.Unix(0, int64(entry.ExpiresAt*1e9)).Local().Format("15:04:05")
	e.out("%s armed for %s minutes, until %s.\n", pane, strconv.FormatFloat(minutes, 'f', 0, 64), until)
	return nil
}

const voiceDisarmHelp = `Usage: daisugi voice disarm [OPTIONS] PANE

  Revoke PANE's direct-send grant. Delivered text goes back to preview only.

Arguments:
  PANE  The pane id to revoke direct-send from.  [required]

Options:
  --data-dir PATH  Daisugi data directory.
  --json           Machine-readable JSON output.
  --help           Show this message and exit.
`

func (e *Env) voiceDisarm(args []string) error {
	p, err := e.voiceParse("disarm", "PANE", args, []opt{voiceDataOpt, voiceJSONOpt}, 1, voiceDisarmHelp)
	if err != nil {
		return err
	}
	pane := p.args[0]
	dataDir := e.voiceDataDir(p)
	if _, err := e.voiceConfig("voice disarm", dataDir); err != nil {
		return err
	}
	armedDir := voice.ArmedDir(dataDir)
	removed, err := voice.Disarm(pane, armedDir)
	if err != nil {
		e.errf("Could not remove the grant under %s. Check the directory, or pass --data-dir.\n", armedDir)
		return exit(1)
	}
	switch {
	case p.flag("--json"):
		b := "false"
		if removed {
			b = "true"
		}
		e.out("{\"pane\": %s, \"removed\": %s}\n", pyjson.Dumps(pane, true), b)
	case removed:
		e.out("%s disarmed. The grant is gone.\n", pane)
	default:
		e.out("%s had no grant. Nothing changed.\n", pane)
	}
	return nil
}

const voicePTTHelp = `Usage: daisugi voice ptt [OPTIONS] PANE

  Laptop push-to-talk. Tap space to start recording. Tap it again to stop
  and send.

Arguments:
  PANE  The pane id to deliver text to.  [required]

Options:
  --server TEXT    The voice server URL. Defaults to voice_server_url in config.
  --data-dir PATH  Daisugi data directory.
  --help           Show this message and exit.
`

func (e *Env) voicePTT(args []string) error {
	opts := []opt{{names: []string{"--server"}, value: true, metavar: "TEXT"}, voiceDataOpt}
	p, err := e.voiceParse("ptt", "PANE", args, opts, 1, voicePTTHelp)
	if err != nil {
		return err
	}
	pane := p.args[0]
	dataDir := e.voiceDataDir(p)
	cfg, err := e.voiceConfig("voice ptt", dataDir)
	if err != nil {
		return err
	}
	serverURL := p.str("--server", "")
	if serverURL == "" {
		serverURL = cfg.VoiceServerURL
	}
	if !voice.CheckServerURL(serverURL) {
		e.errf("--server must be an http or https URL with a host. Got %s.\n", pystr.Repr(serverURL))
		return exit(1)
	}
	print := func(s string) { e.out("%s\n", s) }
	client, err := voice.NewHTTPClient(serverURL, dataDir, 30*time.Second)
	var se *voice.ServerError
	if errors.As(err, &se) {
		print(se.Msg)
		return nil
	}
	if err != nil {
		return e.fail("voice ptt", err)
	}
	print(fmt.Sprintf("Push to talk on %s through %s. Tap space to start recording. Tap it again to stop and send. q to quit.",
		pane, serverURL))
	restore := rawTerminal(e.Stdin)
	defer restore()
	in := bufio.NewReader(e.Stdin)
	keys := func() (string, bool) {
		r, _, err := in.ReadRune()
		if err != nil || r == utf8.RuneError {
			return "", false
		}
		return string(r), true
	}
	_, err = voice.RunPTT(pane, client, keys, openRecorder, 1600, print)
	if err != nil {
		restore()
		e.errf("%v\n", err)
		return exit(1)
	}
	return nil
}

// rawTerminal puts stdin in raw mode when it is a terminal, as main_loop
// does, and returns the function that puts it back.
func rawTerminal(stdin io.Reader) func() {
	f, ok := stdin.(*os.File)
	if !ok {
		return func() {}
	}
	fd := f.Fd()
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return func() {}
	}
	raw := old
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&raw)))
	done := false
	return func() {
		if !done {
			done = true
			syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
		}
	}
}

// recorder records from the default capture device through arecord, the
// ALSA recorder: the Python build uses PortAudio through sounddevice,
// which this binary does not carry (ruling VO-5). The whole press is
// returned by the first Read.
type recorder struct {
	cmd *os.Process
	out *os.File
	buf chan []byte
}

func openRecorder() (voice.Stream, error) {
	bin := voice.Which("arecord")
	if bin == "" {
		return nil, errors.New("arecord is not on PATH. Install alsa-utils for push to talk, or run the Python daisugi")
	}
	return &recorder{}, nil
}

func (r *recorder) Start() error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	attr := &os.ProcAttr{Files: []*os.File{nil, pw, nil}}
	proc, err := os.StartProcess(voice.Which("arecord"), []string{"arecord", "-q", "-t", "raw", "-f", "S16_LE", "-r", "16000", "-c", "1"}, attr)
	pw.Close()
	if err != nil {
		pr.Close()
		return err
	}
	r.cmd, r.out, r.buf = proc, pr, make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(pr)
		r.buf <- b
	}()
	return nil
}

func (r *recorder) Read(int) ([]byte, bool, error) {
	if r.cmd == nil {
		return nil, false, nil
	}
	r.cmd.Signal(os.Interrupt)
	r.cmd.Wait()
	b := <-r.buf
	r.out.Close()
	r.cmd = nil
	return b, false, nil
}

func (r *recorder) Stop() error {
	if r.cmd != nil {
		r.cmd.Kill()
		r.cmd.Wait()
		r.cmd = nil
	}
	return nil
}

const voiceServeHelp = `Usage: daisugi voice serve [OPTIONS]

  Run the voice bridge. It answers GET /health, POST /transcribe, and POST
  /deliver.

Options:
  --host TEXT        Bind address.  [default: 127.0.0.1]
  --port INTEGER     Bind port.  [default: 7477]
  --listen TEXT      host:port for the tailnet, for example 0.0.0.0:7477. Needs a token file.
  --token-file PATH  Defaults to the coppice web token file.
  --tls-cert PATH    TLS certificate file.
  --tls-key PATH     TLS private key file.
  --data-dir PATH    Daisugi data directory.
  --help             Show this message and exit.
`

func (e *Env) voiceServe(args []string) error {
	opts := []opt{
		{names: []string{"--host"}, value: true}, {names: []string{"--port"}, value: true},
		{names: []string{"--listen"}, value: true}, {names: []string{"--token-file"}, value: true},
		{names: []string{"--tls-cert"}, value: true}, {names: []string{"--tls-key"}, value: true},
		voiceDataOpt,
	}
	p, err := e.voiceParse("serve", "", args, opts, 0, voiceServeHelp)
	if err != nil {
		return err
	}
	host := p.str("--host", "127.0.0.1")
	portText := p.str("--port", "7477")
	pn := voice.PyInt(portText)
	if pn == nil || !pn.IsInt64() {
		e.errf("Usage: daisugi voice serve [OPTIONS]\nTry 'daisugi voice serve --help' for help.\n\nError: Invalid value for '--port': %s is not a valid integer.\n",
			pystr.Repr(portText))
		return exit(2)
	}
	port := int(pn.Int64())
	if p.has("--listen") && p.str("--listen", "") != "" {
		listen := p.str("--listen", "")
		h, n, ok := voice.ParseListen(listen)
		if !ok {
			e.errf("--listen must be host:port, for example 0.0.0.0:7477. Got %s.\n", pystr.Repr(listen))
			return exit(1)
		}
		host, port = h, n
	}
	dataDir := e.voiceDataDir(p)
	cfg, err := e.voiceConfig("voice serve", dataDir)
	if err != nil {
		return err
	}
	tokenFile := voice.TokenFile(dataDir)
	if p.has("--token-file") {
		tokenFile = gateroot.PathStr(p.str("--token-file", ""))
	}
	var cert, key *string
	if p.has("--tls-cert") {
		s := gateroot.PathStr(p.str("--tls-cert", ""))
		cert = &s
	}
	if p.has("--tls-key") {
		s := gateroot.PathStr(p.str("--tls-key", ""))
		key = &s
	}
	scheme := "http"
	if cert != nil {
		scheme = "https"
	}
	lookup := func(k string) (string, bool) { v, ok := e.env[k]; return v, ok }
	menv := voice.ModelEnv{Lookup: lookup, Home: e.home, Environ: e.Environ,
		Say: func(s string) { e.errf("%s\n", s) }, Hardware: e.voiceHardware}
	engine, eerr := voice.PickEngine(cfg.VoiceEngine, cfg.VoiceModel, menv)
	if eerr != nil {
		e.errf("%s\n", eerr.Msg)
		if eerr.Kind == "unknown" {
			return exit(1)
		}
		return exit(3)
	}
	// A resident engine starts here and loads its model before the bind,
	// and stops with the server however it ends.
	if serr := engine.Start(); serr != nil {
		e.errf("%s\n", serr.Msg)
		return exit(3)
	}
	defer engine.Stop()
	getenv := func(k string) (string, bool) { v, ok := e.env[k]; return v, ok }
	scfg := voice.ServerConfig{
		DataDir: dataDir, Home: e.home, TokenFile: tokenFile, ArmedDir: voice.ArmedDir(dataDir),
		Engine: engine,
		Cleanup: voice.CleanupConfig{On: cfg.VoiceCleanup, Model: cfg.VoiceCleanupModel,
			BaseURL: cfg.VoiceCleanupBaseURL, DataDir: dataDir},
		Floor: voice.FloorConfig{Backend: cfg.FloorBackend, CoppiceSocket: cfg.CoppiceSocket},
		LLM: llm.New(llm.Env{Getenv: getenv, Environ: e.Environ, Home: e.home,
			Stdout: io.Discard, Stderr: io.Discard}),
	}
	srv, err := voice.Listen(host, port, scfg, cert, key)
	if err != nil {
		e.errf("%s\n", err.Error())
		return exit(voice.ExitCode(err))
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	go func() {
		<-sigs
		srv.Close()
	}()
	e.out("opendaisugi voice listening on %s://%s:%d\n", scheme, host, port)
	if f, ok := e.Stdout.(*os.File); ok {
		f.Sync()
	}
	srv.Serve()
	e.out("\n")
	return nil
}

// voiceHardware is the probe tiers setup uses, for the engine a box with
// no voice choice gets (ruling VO-17).
func (e *Env) voiceHardware() voice.VoiceHardware {
	vram, _ := e.gpuProbe()
	if math.IsNaN(vram) {
		vram = 0
	}
	return voice.VoiceHardware{RAMGB: ramGB(), CPUs: cpuCount(), VRAMGB: vram}
}
