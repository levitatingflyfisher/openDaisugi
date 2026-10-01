package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"syscall"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// The config beside the gate root: gate.resolve_gate_mode and
// gate._configured_verifier_client each call config.load_config on
// root.parent / "config.yaml", and each turns any exception into its
// default.

type loadedConfig struct{ mode, client, llmBackend, dialectPin string }

// loadConfig is config.load_config(path) for the two fields the gate
// reads. It raises what load_config raises. A file in YAML the port does
// not read is not guessed at: the call is denied undecided.
func loadConfig(p string) loadedConfig {
	if !exists(p) {
		return loadedConfig{mode: "audit", client: "python"}
	}
	v, exc, why := pyyaml.Load(readText(p))
	if why != nil {
		unported("config.yaml holds YAML the port does not read (" + why.Why + ")")
	}
	if exc != nil {
		panic(exc)
	}
	// `yaml.safe_load(...) or {}`, then Config(**known): the model ignores
	// every key that is not a field.
	raw, ok := v.(*pyjson.Object)
	if !ok {
		if v != nil {
			unported("config.yaml is not a mapping")
		}
		raw = pyjson.NewObject()
	}
	out, verr := pmodel.Validate("Config", pmodel.Config, raw, pmodel.Python)
	if verr != nil {
		panic(pystr.NewException("ValidationError", verr.String()))
	}
	o := out.(*pyjson.Object)
	backend, _ := o.Value("llm_backend").(string)
	pin, _ := o.Value("dialect_enforce").(string)
	return loadedConfig{mode: o.Value("gate_mode").(string), client: o.Value("verifier_client").(string),
		llmBackend: backend, dialectPin: pin}
}

func (r *runner) configPath() string {
	return pathJoin(pathParent(r.root), "config.yaml")
}

// resolveMode is gate.resolve_gate_mode: --mode wins; else config's
// gate_mode when it is audit or enforce; else audit.
func (r *runner) resolveMode(explicit *string) string {
	if explicit != nil {
		return *explicit
	}
	var c loadedConfig
	if exc := catch(func() { c = loadConfig(r.configPath()) }); exc != nil {
		return "audit"
	}
	if c.mode == "audit" || c.mode == "enforce" {
		return c.mode
	}
	return "audit"
}

// agentModeMaxBytes is gate.AGENT_MODE_MAX_BYTES: a larger marker is not
// read at all.
const agentModeMaxBytes = 64

// effectiveMode is gate.effective_gate_mode: mode, except that an enforce
// hook becomes audit when the calling pane's marker says watching
// (F11-R-1). The marker never raises a mode.
func (r *runner) effectiveMode(mode string) string {
	if mode == "enforce" && r.agentModeLowered() {
		return "audit"
	}
	return mode
}

// geteuid is os.Geteuid, a variable so a test can name another owner.
var geteuid = os.Geteuid

// agentModeLowered is gate.agent_mode_lowered: true when
// <root>/agents/<floor>/<pane>.mode is a regular file this user owns, with
// one link, writable by no group and no other user, of at most
// agentModeMaxBytes bytes, that holds exactly "watching" with at most one
// newline after it. The pane and floor are COPPICE_PANE and COPPICE_SOCK,
// or a resident call's caller pane and socket, and only an id of
// coppice's shape is read (F11-R-2, F11-R-6). No symlink is followed below
// the gate root, and the marker is opened without blocking (F11-R-7).
// Anything else, an error included, is false.
func (r *runner) agentModeLowered() bool {
	var pane, sock *string
	if r.resident {
		pane, sock = r.caller.Pane, r.caller.Sock
	} else {
		if v, ok := r.env["COPPICE_PANE"]; ok {
			pane = &v
		}
		if v, ok := r.env["COPPICE_SOCK"]; ok {
			sock = &v
		}
	}
	if !validCoppicePane(pane) || sock == nil || *sock == "" {
		return false
	}
	floor, ok := agentModeFloor(*sock)
	if !ok {
		return false
	}
	agents := pathJoin(r.root, "agents")
	if strings.ContainsRune(agents, 0) {
		return false
	}
	enc, eerr := pystr.FSEncode(agents)
	if eerr != nil {
		return false
	}
	// F11-R-7: agents/ and the floor's directory are opened as
	// directories without following a symlink, and the marker by its
	// name in the floor's directory.
	const dirFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	afd, err := syscall.Open(string(enc), dirFlags, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(afd)
	ffd, err := syscall.Openat(afd, floor, dirFlags, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(ffd)
	fd, err := syscall.Openat(ffd, *pane+".mode", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		int(st.Uid) != geteuid() || st.Nlink != 1 || st.Mode&0o022 != 0 || st.Size > agentModeMaxBytes {
		return false
	}
	buf := make([]byte, agentModeMaxBytes+1)
	n, err := syscall.Read(fd, buf)
	if err != nil || n < 0 {
		return false
	}
	got := string(buf[:n])
	return got == "watching" || got == "watching\n"
}

// agentModeFloor is gate.agent_mode_floor: the first 16 hex digits of the
// SHA-256 of the floor's socket path as the OS spells it (F11-R-6). false
// when the path cannot be encoded.
func agentModeFloor(sock string) (string, bool) {
	enc, err := pystr.FSEncode(sock)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:])[:16], true
}

// verifierClient is gate._configured_verifier_client's client: python
// when the config names none or cannot be read.
func (r *runner) verifierClient() string {
	var c loadedConfig
	if exc := catch(func() { c = loadConfig(r.configPath()) }); exc != nil {
		return "python"
	}
	if c.client == "" {
		return "python"
	}
	return c.client
}

// dialectPin is gate._configured_dialect_pin: the dialect hash config.yaml
// pins for enforcement, or nil (audit) when it names none or cannot be
// read.
func (r *runner) dialectPin() *string {
	var c loadedConfig
	if exc := catch(func() { c = loadConfig(r.configPath()) }); exc != nil {
		return nil
	}
	if c.dialectPin == "" {
		return nil
	}
	return &c.dialectPin
}

// internalError is _evaluate_record's fail-closed reason for msg. It
// names the configured client, read again, when that is not python.
func (r *runner) internalError(msg string) string {
	chosen := r.verifierClient()
	if chosen == "python" {
		return "gate internal error (denied fail-closed): " + msg + "."
	}
	return "gate internal error with verifier_client=" + chosen + " (denied fail-closed): " + msg +
		". Set verifier_client: python in config.yaml to rule out the client."
}
