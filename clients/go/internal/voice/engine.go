package voice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"daisugi-verify/internal/pystr"
)

// The pinned facts of voice/pins.py.
var FasterWhisperModelNames = []string{"distil-large-v3", "large-v2", "large-v3", "medium", "medium.en",
	"small", "small.en", "tiny", "tiny.en", "turbo", "base", "base.en"}

const (
	FasterWhisperTestModel = "tiny.en"
	WhisperCppBinary       = "whisper-cli"
	WhisperCppAutoLanguage = "auto"
	WhisperCppTimeoutS     = 120.0
	FixtureMeanMaxWER      = 0.15
)

// FixtureNames are the three speech fixtures, in pins order.
var FixtureNames = []string{"fixture_a.wav", "fixture_b.wav", "fixture_c.wav"}

var (
	FixtureTranscripts = map[string]string{
		"fixture_a.wav": "the quick brown fox jumps over the lazy dog",
		"fixture_b.wav": "please remember to buy milk after work",
		"fixture_c.wav": "what is the weather like today",
	}
	FixtureObservedWER = map[string]float64{"fixture_a.wav": 0.111, "fixture_b.wav": 0.0, "fixture_c.wav": 0.0}
	FixtureMaxWER      = map[string]float64{"fixture_a.wav": 0.15, "fixture_b.wav": 0.10, "fixture_c.wav": 0.10}
)

// EngineError is a speech engine that cannot be built (Kind
// "unavailable", EngineUnavailable, exit 3 from voice serve) or a
// voice_engine this binary does not know (Kind "unknown", UnknownEngine,
// exit 1).
type EngineError struct {
	Kind string
	Msg  string
}

func (e *EngineError) Error() string { return e.Msg }

// Class is the oracle's exception class name for the error. Kind
// "loading" is a resident engine that is loading its model (EngineLoading,
// a 503 engine_loading from the server).
func (e *EngineError) Class() string {
	switch e.Kind {
	case "unknown":
		return "UnknownEngine"
	case "loading":
		return "EngineLoading"
	}
	return "EngineUnavailable"
}

// WhisperCppArgs is engines.whisper_cpp_args.
func WhisperCppArgs(binary, model, wavPath string, language *string) []string {
	lang := WhisperCppAutoLanguage
	if language != nil && *language != "" {
		lang = *language
	}
	return []string{binary, "-m", model, "-f", wavPath, "-l", lang, "-nt", "-np"}
}

// WhisperCppText is engines.whisper_cpp_text: each line stripped, blank
// lines dropped, joined by one space. Lines and spaces are Python's.
func WhisperCppText(stdout string) string {
	var parts []string
	for _, line := range pystr.Splitlines(stdout) {
		if t := pystr.Strip(line); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// Transcript is engines.Transcript.
type Transcript struct {
	Text      string
	DurationS float64
	RTF       float64
}

// Engine is an external speech engine: whisper-cli (whisper.cpp), run
// once per clip, or a resident child (moonshine-cli or parakeet-cli) that
// loads its model once.
type Engine struct {
	Name   string
	binary string
	model  string
	// Arch is the Moonshine architecture; empty for the others.
	Arch string
	// Argv is the resident child's command line; nil for whisper.cpp.
	Argv []string
	res  *Resident
}

func (e *Engine) residentSetup(argv []string, loadS, clipS float64) {
	e.Argv = argv
	e.res = NewResident(ResidentConfig{Name: e.Name, Binary: e.binaryName(), Argv: argv,
		LoadTimeoutS: loadS, ClipTimeoutS: clipS})
}

// Start starts a resident engine's child and waits for its model. It does
// nothing for whisper.cpp.
func (e *Engine) Start() *EngineError {
	if e.res == nil {
		return nil
	}
	return e.res.Start()
}

// Stop stops a resident engine's child.
func (e *Engine) Stop() {
	if e.res != nil {
		e.res.Stop()
	}
}

// WaitReady waits while a resident child loads (for the probe).
func (e *Engine) WaitReady(timeoutS float64) string {
	if e.res == nil {
		return "ready"
	}
	return e.res.WaitReady(timeoutS)
}

// Model is the model file or directory the engine runs, as str(Path).
func (e *Engine) Model() string { return e.model }

// NewWhisperCpp is engines.WhisperCppEngine(model).
func NewWhisperCpp(model string) (*Engine, *EngineError) {
	bin := Which(WhisperCppBinary)
	if bin == "" {
		return nil, &EngineError{"unavailable", fmt.Sprintf("%s is not on PATH. Build whisper.cpp and put %s on PATH, then try again.",
			WhisperCppBinary, WhisperCppBinary)}
	}
	if st, err := os.Stat(model); err != nil || !st.Mode().IsRegular() {
		return nil, &EngineError{"unavailable", fmt.Sprintf("whisper.cpp model file missing: %s. Download a ggml model, then set voice_model to its path.",
			pyPathStr(model))}
	}
	return &Engine{Name: "whisper.cpp", binary: bin, model: model}, nil
}

// pyPathStr is str(Path(p)): repeated slashes and "." parts dropped, a
// trailing slash dropped.
func pyPathStr(p string) string {
	if p == "" {
		return "."
	}
	lead := ""
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		lead = "//"
	} else if strings.HasPrefix(p, "/") {
		lead = "/"
	}
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	out := lead + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}

// ValidEngineNames is engines.VALID_ENGINE_NAMES.
const ValidEngineNames = "faster-whisper, moonshine, parakeet, whisper.cpp"

// The Config defaults, which save_config writes without a choice.
const (
	DefaultEngine = "faster-whisper"
	DefaultModel  = "tiny.en"
)

// PortFasterWhisper is what this binary says where a config names
// faster-whisper, which only the Python build can load (ruling VO-1).
const PortFasterWhisper = "faster-whisper needs the Python build of daisugi. " +
	"Set voice_engine: moonshine to use Moonshine instead."

// InstalledEngines is engines.installed_engines for this binary: the
// resident programs on PATH; faster-whisper never imports here.
func InstalledEngines() map[string]bool {
	return map[string]bool{"parakeet": Which(ParakeetBinary) != "", "moonshine": Which(MoonshineBinary) != ""}
}

// ResolveEngine is engines.resolve_engine: the engine and model a config
// means. With the voice settings unset or at their defaults, the engine
// the hardware picks, after one line through env's Say.
func ResolveEngine(engine, model string, env ModelEnv) (string, string) {
	if engine == DefaultEngine && model == DefaultModel {
		e, m, line := ChooseEngine(env.DetectHardware(), InstalledEngines(), false,
			ParakeetUsable(ParakeetDefaultModel, env, Machine()))
		env.say(line)
		return e, m
	}
	if engine == "moonshine" && model == DefaultModel {
		return "moonshine", MoonshineDefaultModel
	}
	if engine == "parakeet" && model == DefaultModel {
		return "parakeet", ParakeetDefaultModel
	}
	return engine, model
}

// PickEngine is engines.pick_engine for this binary: whisper.cpp,
// Moonshine and Parakeet run here; faster-whisper is a Python package
// (ruling VO-1). The engine is not started.
func PickEngine(engine, model string, env ModelEnv) (*Engine, *EngineError) {
	engine, model = ResolveEngine(engine, model, env)
	switch engine {
	case "whisper.cpp":
		return NewWhisperCpp(pyPathStr(model))
	case "moonshine":
		return NewMoonshine(model, env)
	case "parakeet":
		return NewParakeet(model, env)
	case "faster-whisper":
		return nil, &EngineError{"unavailable", PortFasterWhisper}
	}
	return nil, &EngineError{"unknown", fmt.Sprintf("Unknown voice_engine %s. Valid names: %s.",
		pystr.Repr(engine), ValidEngineNames)}
}

func (e *Engine) binaryName() string {
	switch e.Name {
	case "moonshine":
		return MoonshineBinary
	case "parakeet":
		return ParakeetBinary
	}
	return WhisperCppBinary
}

// Transcribe runs the engine on one 16 kHz mono WAV. A resident engine
// sends it to its child (its models are English and take no language);
// whisper.cpp runs once through a temp file that is removed after, and a
// failed run is an error naming the exit code and the last stderr line.
func (e *Engine) Transcribe(wav []byte, language *string) (*Transcript, error) {
	duration, aerr := WavDurationS(wav)
	if aerr != nil {
		return nil, aerr
	}
	if e.res != nil {
		start := time.Now()
		raw, err := e.res.TranscribeText(wav)
		if err != nil {
			return nil, err
		}
		elapsed := time.Since(start).Seconds()
		rtf := 0.0
		if duration > 0 {
			rtf = elapsed / duration
		}
		return &Transcript{Text: WhisperCppText(raw), DurationS: duration, RTF: rtf}, nil
	}
	f, err := os.CreateTemp("", "daisugi-voice-*.wav")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(wav); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(WhisperCppTimeoutS*float64(time.Second)))
	defer cancel()
	args := WhisperCppArgs(e.binary, e.model, path, language)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start).Seconds()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s timed out", e.binaryName())
	}
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			return nil, runErr
		}
		lines := pystr.Splitlines(pystr.Strip(pystr.DecodeReplace(errb.Bytes())))
		last := ""
		if len(lines) > 0 {
			last = pystr.Slice(pystr.Strip(lines[len(lines)-1]), 0, 200)
		}
		return nil, fmt.Errorf("%s exited %d: %s", e.binaryName(), ee.ExitCode(), last)
	}
	text := WhisperCppText(pystr.DecodeReplace(out.Bytes()))
	rtf := 0.0
	if duration > 0 {
		rtf = elapsed / duration
	}
	return &Transcript{Text: text, DurationS: duration, RTF: rtf}, nil
}
