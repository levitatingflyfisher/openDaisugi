package voice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"daisugi-verify/internal/netproxy"
)

// The Moonshine facts of voice/pins.py (ruling VO-13).
const (
	MoonshineBinary       = "moonshine-cli"
	MoonshineTimeoutS     = 120.0
	MoonshineDefaultModel = "small"
	MoonshineDirArch      = "small"
	MoonshineBaseURL      = "https://download.moonshine.ai/model"
	MoonshineBaseURLEnv   = "OPENDAISUGI_MOONSHINE_BASE_URL"
	MoonshineRevision     = "quantized_26_08_21"
)

// MoonshineFile is one pinned file of a curated model.
type MoonshineFile struct {
	Name   string
	SHA256 string
	Size   int64
}

// MoonshineModelNames are the curated models, in pins order.
var MoonshineModelNames = []string{"tiny", "small", "medium"}

// MoonshineModels is pins.MOONSHINE_MODELS.
var MoonshineModels = map[string][]MoonshineFile{
	"tiny": {
		{"adapter.ort", "22ecc949e146c49667fda28d102d4e30749a107dc88a396292aa8f277ef1347c", 1_319_664},
		{"cross_kv.ort", "143a36667b8d05fd9d04e8c337b7ee121f37ef299aea6b3d82bdb3d3401950b4", 1_287_544},
		{"decoder_kv.ort", "8852553f312adb6c9aa4d17418015049b30f412209ee569d336548c0044627de", 32_583_720},
		{"encoder.ort", "a8414e1a5dedf9f2093d7680601dd8a9b0433e7020260eafe0e370ead91134ca", 7_675_440},
		{"frontend.model.ort", "5121b561417b638afce0c6c31b760e37c93cf97f80d9b0031aad1fe7b6f25d61", 23_344},
		{"frontend.weights.ort", "217da24ac6f522ebf02da8ef288e77d1ac68d50d4a6821433182e4fbf4204bbd", 2_093_464},
		{"streaming_config.json", "74fe5ddebd63b17caf59e8a3b18c17547ff7bce1642050edbb1c3962674f8950", 509},
		{"tokenizer.bin", "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d", 249_974},
	},
	"small": {
		{"adapter.ort", "c665f742364febad597cc9ac1e0b341ffbee0e24a1466e2f3bde95e6e4771762", 2_870_368},
		{"cross_kv.ort", "e2d3417144e9514055ebfefe8dcc4c0a55a55adcb8530435844c75c53e352bf6", 5_356_536},
		{"decoder_kv.ort", "1a05465b1dd955858dfcbee039c0020fb5dd982b0f5094c34e61735d518d771b", 81_878_600},
		{"encoder.ort", "2d4d973e91e8aca08c51e7e7efa28a46ab265b63d809d5294d18b86bcd85b993", 44_148_576},
		{"frontend.model.ort", "09b1210ae30dc5f0f3e45f0ebab914c254741323114f53fbbe5ae62cca35058f", 26_944},
		{"frontend.weights.ort", "7ef97521bd4bad3928f5bb6808586f4fcc6e92bd5990394112eed7d4052ec338", 7_769_464},
		{"streaming_config.json", "26f02b6afb22d60871a5efd85c3d38e569cc0ddb6c5eb6e93d3260152ae8a47a", 512},
		{"tokenizer.bin", "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d", 249_974},
	},
	"medium": {
		{"adapter.ort", "3f2a287def57cc094367a0eec3c4f5fc36a32ec420e86764b696920991b20281", 3_651_296},
		{"cross_kv.ort", "642f6e21cd305be79342207c6f9e6b681d469d55bc48c72b27b84846fb71fd1e", 11_643_776},
		{"decoder_kv.ort", "193bb366492b74fc4ad338c6778e8d8eb916aaa11b5aa264f9057f4db7759486", 146_972_408},
		{"encoder.ort", "12915e76ebac7dd287c5ea63965d06103a53ba1ce242a4a34f318f3958c60c37", 94_705_376},
		{"frontend.model.ort", "95768855c70c8251eeecc05fedf69999da1b8ab16f605c9f457fd3354b0ad6b5", 28_720},
		{"frontend.weights.ort", "5ac941f490cbe035b335b99a414cc393d62d4c6f9f2423495b286870d271d709", 11_889_560},
		{"streaming_config.json", "28e83b7a28e91472692a035e0dae3116422ae43aeb2bef5ed822c44ce89b88af", 513},
		{"tokenizer.bin", "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d", 249_974},
	},
}

// FasterWhisperRevisions and FasterWhisperSizes are pins'; only the
// Python build fetches these models.
var (
	// FasterWhisperRevisionOrder is pins order, the order the probe prints.
	FasterWhisperRevisionOrder = []string{"tiny", "tiny.en", "base", "base.en", "small", "small.en", "medium", "medium.en", "large-v2", "large-v3", "distil-large-v3", "turbo"}
	FasterWhisperRevisions     = map[string]string{
		"tiny":            "d90ca5fe260221311c53c58e660288d3deb8d356",
		"tiny.en":         "0d3d19a32d3338f10357c0889762bd8d64bbdeba",
		"base":            "ebe41f70d5b6dfa9166e2c581c45c9c0cfc57b66",
		"base.en":         "3d3d5dee26484f91867d81cb899cfcf72b96be6c",
		"small":           "536b0662742c02347bc0e980a01041f333bce120",
		"small.en":        "d1d751a5f8271d482d14ca55d9e2deeebbae577f",
		"medium":          "08e178d48790749d25932bbc082711ddcfdfbc4f",
		"medium.en":       "a29b04bd15381511a9af671baec01072039215e3",
		"large-v2":        "f0fe81560cb8b68660e564f55dd99207059c092e",
		"large-v3":        "edaa852ec7e145841d8ffdb056a99866b5f0a478",
		"distil-large-v3": "c3058b475261292e64a0412df1d2681c06260fab",
		"turbo":           "0a363e9161cbc7ed1431c9597a8ceaf0c4f78fcf",
	}
	FasterWhisperSizes = map[string]int64{"tiny.en": 78_090_594}
)

// ModelEnv is what model resolution reads from the process: the
// environment, the home directory, where its one line goes, and the
// hardware probe (nil: only HardwareEnv is read).
type ModelEnv struct {
	Lookup   func(string) (string, bool)
	Home     string
	Environ  []string
	Say      func(string)
	Hardware func() VoiceHardware
	// The resident child's timeouts in seconds (0: the defaults). Only the
	// probe sets them.
	LoadTimeoutS, ClipTimeoutS float64
}

// ProcessModelEnv reads this process's own environment and says lines on
// stderr.
func ProcessModelEnv() ModelEnv {
	return ModelEnv{Lookup: os.LookupEnv, Home: os.Getenv("HOME"), Environ: os.Environ(),
		Say: func(s string) { fmt.Fprintln(os.Stderr, s) }}
}

func (m ModelEnv) say(s string) {
	if m.Say != nil {
		m.Say(s)
	}
}

func (m ModelEnv) get(k string) string {
	if m.Lookup == nil {
		return ""
	}
	v, _ := m.Lookup(k)
	return v
}

// MoonshineArgs is engines.moonshine_args: the resident child's command line.
func MoonshineArgs(binary, model, arch string) []string {
	return []string{binary, "-m", model, "-a", arch, "--resident"}
}

// MoonshineDir is models.moonshine_dir: under $XDG_CACHE_HOME when it is
// an absolute path, else under $HOME/.cache.
func MoonshineDir(name string, env ModelEnv) string {
	base := env.get("XDG_CACHE_HOME")
	if !strings.HasPrefix(base, "/") {
		base = env.Home + "/.cache"
	}
	return pyPathStr(base + "/opendaisugi/models/moonshine/" + name + "-streaming-en/" + MoonshineRevision)
}

// MoonshineURL is models.moonshine_url.
func MoonshineURL(name, file string, env ModelEnv) string {
	base := env.get(MoonshineBaseURLEnv)
	if base == "" {
		base = MoonshineBaseURL
	}
	return base + "/" + name + "-streaming-en/" + MoonshineRevision + "/" + file
}

// MoonshineSizeMB is models.moonshine_size_mb.
func MoonshineSizeMB(name string) int64 {
	var total int64
	for _, f := range MoonshineModels[name] {
		total += f.Size
	}
	return (total + 500_000) / 1_000_000
}

// FetchError is a pinned download that failed: Verify when the bytes
// arrived and did not match their digest (_model_fetch's
// FetchVerificationError), else any other failure (its OSError).
type FetchError struct {
	Verify bool
	Err    error
}

func (e *FetchError) Error() string { return e.Err.Error() }

func sha256Of(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", errors.New("not a file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stalePartAge: a temporary download file older than this is the orphan of
// a download that was killed. A younger one may belong to another download
// that runs.
const stalePartAge = 24 * time.Hour

// sweepStaleParts removes the dest.part-* files beside dest that were last
// written more than maxAge ago; a killed download leaves one of up to 1.2 GB.
func sweepStaleParts(dest string, maxAge time.Duration) {
	olds, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), globEscape(filepath.Base(dest))+".part-*"))
	for _, p := range olds {
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() && time.Since(st.ModTime()) > maxAge {
			_ = os.Remove(p)
		}
	}
}

func globEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(s)
}

// FetchFile is _model_fetch.fetch: a file that matches its digest is used
// as it is; any other is removed and downloaded again into a temporary
// file beside it, checked, then renamed into place. A download that stops
// is started again from nothing next time (ruling VO-13: this binary does
// not resume one). size is nil when the caller does not know it.
func FetchFile(url, digest, dest string, size *int64, env ModelEnv) error {
	if got, err := sha256Of(dest); err == nil && got == digest {
		return nil
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &FetchError{Err: err}
	}
	_ = os.Remove(dest)
	sweepStaleParts(dest, stalePartAge)
	rules := netproxy.UrllibFromVars(netproxy.FromEnviron(env.Environ), true)
	base := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{Transport: netproxy.Transport(rules, base)}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return &FetchError{Err: err}
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "opendaisugi")
	resp, err := client.Do(req)
	if err != nil {
		return &FetchError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &FetchError{Err: fmt.Errorf("GET %s: %s", url, resp.Status)}
	}
	out, err := os.CreateTemp(dir, filepath.Base(dest)+".part-*")
	if err != nil {
		return &FetchError{Err: err}
	}
	part := out.Name()
	defer os.Remove(part)
	h := sha256.New()
	var body io.Reader = resp.Body
	if size != nil {
		body = io.LimitReader(resp.Body, *size+1)
	}
	n, err := io.Copy(io.MultiWriter(out, h), body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &FetchError{Err: err}
	}
	if size != nil && n < *size {
		return &FetchError{Err: fmt.Errorf("download of %s stopped at %d of %d bytes", url, n, *size)}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return &FetchError{Verify: true, Err: fmt.Errorf("%s: sha256 %s, not the pinned %s", filepath.Base(dest), got, digest)}
	}
	if err := os.Chmod(part, 0o644); err != nil {
		return &FetchError{Err: err}
	}
	if err := os.Rename(part, dest); err != nil {
		return &FetchError{Err: err}
	}
	return nil
}

// EnsureMoonshine is models.ensure_moonshine: the directory of a curated
// model with every file checked, the stale ones fetched after one line.
func EnsureMoonshine(name string, env ModelEnv) (string, error) {
	dir := MoonshineDir(name, env)
	var stale []MoonshineFile
	for _, f := range MoonshineModels[name] {
		if got, err := sha256Of(dir + "/" + f.Name); err != nil || got != f.SHA256 {
			stale = append(stale, f)
		}
	}
	if len(stale) == 0 {
		return dir, nil
	}
	env.say(fmt.Sprintf("Fetching the Moonshine %s model (%d MB) into %s. This happens once.", name, MoonshineSizeMB(name), dir))
	for _, f := range stale {
		size := f.Size
		if err := FetchFile(MoonshineURL(name, f.Name, env), f.SHA256, dir+"/"+f.Name, &size, env); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func fetchFailed(what string) string {
	return fmt.Sprintf("The %s model could not be fetched. Check the network, then run daisugi voice serve again.", what)
}

// NewMoonshine is engines.MoonshineEngine(model): moonshine-cli on PATH,
// then a curated model fetched once, or a model directory.
func NewMoonshine(model string, env ModelEnv) (*Engine, *EngineError) {
	bin := Which(MoonshineBinary)
	if bin == "" {
		return nil, &EngineError{"unavailable", MoonshineBinary + " is not on PATH. Build it with " +
			"clients/go/scripts/native.sh --moonshine (scripts/install.sh does this " +
			"and puts it on PATH), then try again."}
	}
	if _, curated := MoonshineModels[model]; curated {
		dir, err := EnsureMoonshine(model, env)
		if err != nil {
			var fe *FetchError
			if errors.As(err, &fe) && fe.Verify {
				return nil, &EngineError{"unavailable", fmt.Sprintf("A file of the Moonshine %s model did not match its pinned sha256 "+
					"and was deleted. Run daisugi voice serve again to fetch it again.", model)}
			}
			return nil, &EngineError{"unavailable", fetchFailed("Moonshine " + model)}
		}
		e := &Engine{Name: "moonshine", binary: bin, model: dir, Arch: model}
		e.residentSetup(MoonshineArgs(bin, dir, model), env.LoadTimeoutS, env.ClipTimeoutS)
		return e, nil
	}
	dir := pyPathStr(model)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, &EngineError{"unavailable", fmt.Sprintf("Moonshine model directory missing: %s. Set voice_model to "+
			"tiny, small or medium, or to a Moonshine streaming model directory.", dir)}
	}
	e := &Engine{Name: "moonshine", binary: bin, model: dir, Arch: MoonshineDirArch}
	e.residentSetup(MoonshineArgs(bin, dir, MoonshineDirArch), env.LoadTimeoutS, env.ClipTimeoutS)
	return e, nil
}

// fetchAnswer lists the files in dir with their digests, a partial
// download left out, for the probe.
func fetchListing(dir string) map[string]string {
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.Contains(n, ".part") {
			continue
		}
		if d, err := sha256Of(filepath.Join(dir, n)); err == nil {
			out[n] = d
		}
	}
	return out
}
