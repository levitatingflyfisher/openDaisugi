package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"daisugi-verify/internal/pystr"
)

// The Parakeet facts of voice/pins.py (rulings VO-16 and VO-17).
const (
	ParakeetBinary         = "parakeet-cli"
	ParakeetQuantizeBinary = "parakeet-quantize"
	ParakeetDefaultModel   = "v2"
	ParakeetBaseURL        = "https://huggingface.co/cstr/parakeet-tdt-0.6b-v2-GGUF/resolve/" + ParakeetRevision
	// ParakeetRevision is pins.PARAKEET_REVISION: the repo commit the F16
	// file is fetched at.
	ParakeetRevision   = "8878172f3c45ba231fac8cfd4aff2542b13bc875"
	ParakeetBaseURLEnv = "OPENDAISUGI_PARAKEET_BASE_URL"
	ParakeetQuant      = "q4_k"
	QuantizeTimeoutS   = 1800.0
)

// ParakeetFile is one pinned file.
type ParakeetFile struct {
	Name   string
	SHA256 string
	Size   int64
}

// ParakeetModel is a curated model: the F16 file fetched and the Q4_K
// file parakeet-quantize makes from it.
type ParakeetModel struct {
	Dir       string
	Source    ParakeetFile
	Quantized ParakeetFile
}

// ParakeetModelNames are the curated models, in pins order.
var ParakeetModelNames = []string{"v2"}

// ParakeetModels is pins.PARAKEET_MODELS.
var ParakeetModels = map[string]ParakeetModel{
	"v2": {
		Dir:       "tdt-0.6b-v2",
		Source:    ParakeetFile{"parakeet-tdt-0.6b-v2.gguf", "c82b001dcb0adecd36f7401e4b77c7257eb352462368b89cf3ee13184206d7f7", 1_236_861_248},
		Quantized: ParakeetFile{"parakeet-tdt-0.6b-v2-q4_k.gguf", "764c4e6738b0b38c53bbfea040f9e07425d6d742df58b18906053085aea46b1c", 396_937_984},
	},
}

// ParakeetArgs is engines.parakeet_args: the resident child's command line.
func ParakeetArgs(binary, model string) []string {
	return []string{binary, "-m", model, "--resident"}
}

// ParakeetDir is models.parakeet_dir.
func ParakeetDir(name string, env ModelEnv) string {
	base := env.get("XDG_CACHE_HOME")
	if !strings.HasPrefix(base, "/") {
		base = env.Home + "/.cache"
	}
	return pyPathStr(base + "/opendaisugi/models/parakeet/" + ParakeetModels[name].Dir)
}

// ParakeetURL is models.parakeet_url.
func ParakeetURL(file string, env ModelEnv) string {
	base := env.get(ParakeetBaseURLEnv)
	if base == "" {
		base = ParakeetBaseURL
	}
	return base + "/" + file
}

func megabytes(n int64) int64 { return (n + 500_000) / 1_000_000 }

// QuantizeError is models.QuantizeError.
type QuantizeError struct{ Msg string }

func (e *QuantizeError) Error() string { return e.Msg }

func isExecFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// ParakeetQuantArches is pins.PARAKEET_QUANT_ARCHES: the machines whose
// parakeet-quantize result is pinned.
var ParakeetQuantArches = []string{"x86_64"}

// Machine is platform.machine() for the architectures Go builds for here.
func Machine() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

// ParakeetQuantRefusal is models.parakeet_quant_refusal: "" when arch's
// Q4_K result is pinned, else the one line that refuses to make it.
func ParakeetQuantRefusal(name, arch string) string {
	for _, a := range ParakeetQuantArches {
		if a == arch {
			return ""
		}
	}
	return fmt.Sprintf("The Parakeet %s model's Q4_K file has no pinned sha256 for %s yet, so it is not made here. "+
		"Set voice_model to the path of a Parakeet GGUF file instead.", name, arch)
}

// ParakeetUsable is models.parakeet_usable: true when EnsureParakeet can
// give this machine the curated model, because its Q4_K result is pinned
// for arch or a file that matches the pin is already on disk.
func ParakeetUsable(name string, env ModelEnv, arch string) bool {
	if ParakeetQuantRefusal(name, arch) == "" {
		return true
	}
	m := ParakeetModels[name]
	got, err := sha256Of(ParakeetDir(name, env) + "/" + m.Quantized.Name)
	return err == nil && got == m.Quantized.SHA256
}

// EnsureParakeet is models.ensure_parakeet: the curated model's Q4_K
// file, made once from the fetched F16 file and checked against its pin.
func EnsureParakeet(name, quantizer string, env ModelEnv) (string, error) {
	return ensureParakeetOn(name, quantizer, env, Machine())
}

func ensureParakeetOn(name, quantizer string, env ModelEnv, arch string) (string, error) {
	m := ParakeetModels[name]
	dir := ParakeetDir(name, env)
	out := dir + "/" + m.Quantized.Name
	if got, err := sha256Of(out); err == nil && got == m.Quantized.SHA256 {
		return out, nil
	}
	// A file that matches the pin is good on any machine; only making one
	// needs a pinned result for this machine.
	if why := ParakeetQuantRefusal(name, arch); why != "" {
		return "", &QuantizeError{why}
	}
	env.say(fmt.Sprintf("Fetching the Parakeet %s model (%d MB) into %s, then making its %d MB Q4_K file. This happens once.",
		name, megabytes(m.Source.Size), dir, megabytes(m.Quantized.Size)))
	src := dir + "/" + m.Source.Name
	size := m.Source.Size
	if err := FetchFile(ParakeetURL(m.Source.Name, env), m.Source.SHA256, src, &size, env); err != nil {
		return "", err
	}
	if !isExecFile(quantizer) {
		return "", &QuantizeError{ParakeetQuantizeBinary + " is not beside " + ParakeetBinary +
			". Build both with clients/go/scripts/native.sh --parakeet, then try again."}
	}
	part := out + ".part"
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(QuantizeTimeoutS*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, quantizer, src, part, ParakeetQuant)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	runErr := cmd.Run()
	if ctx.Err() != nil {
		os.Remove(part)
		return "", &QuantizeError{fmt.Sprintf("%s did not finish in %s seconds.", ParakeetQuantizeBinary, pyG(QuantizeTimeoutS))}
	}
	if runErr != nil {
		os.Remove(part)
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return "", runErr
		}
		lines := pystr.Splitlines(pystr.Strip(pystr.DecodeReplace(errb.Bytes())))
		last := ""
		if len(lines) > 0 {
			last = pystr.Slice(pystr.Strip(lines[len(lines)-1]), 0, 200)
		}
		return "", &QuantizeError{fmt.Sprintf("%s exited %d: %s", ParakeetQuantizeBinary, ee.ExitCode(), last)}
	}
	if got, err := sha256Of(part); err != nil || got != m.Quantized.SHA256 {
		os.Remove(part)
		return "", &QuantizeError{"The Q4_K file " + ParakeetQuantizeBinary + " made did not match its pinned sha256 " +
			"and was deleted. Set voice_model to the path of a Parakeet GGUF file instead."}
	}
	if err := os.Rename(part, out); err != nil {
		return "", err
	}
	os.Remove(src)
	return out, nil
}

// NewParakeet is engines.ParakeetEngine(model): parakeet-cli on PATH, then
// the curated model made once, or a GGUF file.
func NewParakeet(model string, env ModelEnv) (*Engine, *EngineError) {
	bin := Which(ParakeetBinary)
	if bin == "" {
		return nil, &EngineError{"unavailable", ParakeetBinary + " is not on PATH. Build it with " +
			"clients/go/scripts/native.sh --parakeet (scripts/install.sh does this " +
			"and puts it on PATH), then try again."}
	}
	var path string
	if _, curated := ParakeetModels[model]; curated {
		real, err := filepath.EvalSymlinks(bin)
		if err != nil {
			real = bin
		}
		real, _ = filepath.Abs(real)
		p, ferr := EnsureParakeet(model, filepath.Join(filepath.Dir(real), ParakeetQuantizeBinary), env)
		if ferr != nil {
			var fe *FetchError
			var qe *QuantizeError
			switch {
			case errors.As(ferr, &fe) && fe.Verify:
				return nil, &EngineError{"unavailable", fmt.Sprintf("A file of the Parakeet %s model did not match its pinned sha256 "+
					"and was deleted. Run daisugi voice serve again to fetch it again.", model)}
			case errors.As(ferr, &qe):
				return nil, &EngineError{"unavailable", qe.Msg}
			}
			return nil, &EngineError{"unavailable", fetchFailed("Parakeet " + model)}
		}
		path = p
	} else {
		path = pyPathStr(model)
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			return nil, &EngineError{"unavailable", fmt.Sprintf("Parakeet model file missing: %s. Set voice_model to "+
				"%s, or to a Parakeet-TDT GGUF file.", path, ParakeetDefaultModel)}
		}
	}
	e := &Engine{Name: "parakeet", binary: bin, model: path}
	e.residentSetup(ParakeetArgs(bin, path), env.LoadTimeoutS, env.ClipTimeoutS)
	return e, nil
}
