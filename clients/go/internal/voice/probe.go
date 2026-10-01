package voice

import (
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Probe answers one query of clients/voice_cases.py on stdin with one JSON
// object on stdout, as clients/voice_probe_oracle.py answers it for the
// oracle. It is a test instrument, not shipped.
func Probe(stdin io.Reader, stdout io.Writer) int {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return 2
	}
	text, xerr := pystr.DecodeStrict(raw)
	if xerr != nil {
		return 2
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		return 2
	}
	q, ok := v.(*pyjson.Object)
	if !ok {
		return 2
	}
	p := &prober{q: q, root: os.Getenv("VOICE_PROBE_DIR")}
	ans, err := p.answer()
	if err != nil {
		io.WriteString(os.Stderr, err.Error()+"\n")
		return 1
	}
	io.WriteString(stdout, pyjson.Dumps(ans, false)+"\n")
	return 0
}

type prober struct {
	q    *pyjson.Object
	root string
}

func (p *prober) str(k string) string {
	v, _ := p.q.Get(k)
	s, _ := v.(string)
	return s
}

func (p *prober) optStr(k string) *string {
	v, _ := p.q.Get(k)
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func (p *prober) num(k string) float64 {
	v, _ := p.q.Get(k)
	switch x := v.(type) {
	case pyjson.Int:
		f, _ := new(big.Float).SetString(x.Text)
		r, _ := f.Float64()
		return r
	case pyjson.Float:
		return float64(x)
	}
	return 0
}

func (p *prober) bytes(k string) []byte {
	b, _ := base64.StdEncoding.DecodeString(p.str(k))
	return b
}

func b64s(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func errAnswer(class, msg string) *pyjson.Object { return obj("error", class, "message", msg) }

func (p *prober) answer() (*pyjson.Object, error) {
	switch op := p.str("op"); op {
	case "pins":
		return pinsAnswer(), nil
	case "prereq":
		wcpp, moon, para := Which(WhisperCppBinary) != "", Which(MoonshineBinary) != "", Which(ParakeetBinary) != ""
		return obj("faster_whisper_available", false, "ffmpeg_available", Which("ffmpeg") != "",
			"espeak_available", Which("espeak-ng") != "" || Which("espeak") != "",
			"whisper_cpp_available", wcpp, "moonshine_available", moon, "parakeet_available", para,
			"ok", wcpp || moon || para), nil
	case "engine_args":
		args := WhisperCppArgs(p.str("binary"), pyPathStr(p.str("model")), p.str("wav_path"), p.optStr("language"))
		return obj("args", strList(args)), nil
	case "engine_text":
		return obj("text", WhisperCppText(p.str("stdout"))), nil
	case "pick_engine":
		if v := p.optStr("moonshine_base_url"); v != nil {
			os.Setenv(MoonshineBaseURLEnv, *v)
		}
		if v := p.optStr("xdg_cache_home"); v != nil {
			os.Setenv("XDG_CACHE_HOME", *v)
		}
		if v := p.optStr("parakeet_base_url"); v != nil {
			os.Setenv(ParakeetBaseURLEnv, *v)
		}
		if v := p.optStr("hardware"); v != nil {
			os.Setenv(HardwareEnv, *v)
		}
		engine, model := "faster-whisper", "tiny.en"
		eng, mod := p.optStr("voice_engine"), p.optStr("voice_model")
		if eng != nil {
			engine = *eng
		}
		if mod != nil {
			model = *mod
		}
		var said []any
		env := ProcessModelEnv()
		env.Say = func(s string) { said = append(said, s) }
		e, err := PickEngine(engine, model, env)
		var ans *pyjson.Object
		if err != nil {
			ans = errAnswer(err.Class(), err.Msg)
		} else {
			ans = obj("engine", e.Name)
			if e.Name == "moonshine" {
				ans.Set("model", e.Model())
				ans.Set("arch", e.Arch)
			}
			if e.Name == "parakeet" {
				ans.Set("model", e.Model())
			}
			if e.Argv != nil {
				ans.Set("argv", strList(e.Argv))
			}
		}
		if said != nil {
			ans.Set("said", said)
		}
		return ans, nil
	case "moonshine_args":
		return obj("args", strList(MoonshineArgs(p.str("binary"), pyPathStr(p.str("model")), p.str("arch")))), nil
	case "parakeet_quant_refusal":
		if why := ParakeetQuantRefusal(p.str("name"), p.str("arch")); why != "" {
			return obj("refusal", why), nil
		}
		return obj("refusal", nil), nil
	case "parakeet_args":
		return obj("args", strList(ParakeetArgs(p.str("binary"), pyPathStr(p.str("model"))))), nil
	case "resident":
		return p.resident(), nil
	case "choose_engine":
		var hw VoiceHardware
		if v, _ := p.q.Get("ram_gb"); v != nil {
			r := p.num("ram_gb")
			hw.RAMGB = &r
		}
		hw.CPUs, hw.VRAMGB = int(p.num("cpus")), p.num("vram_gb")
		installed := map[string]bool{}
		if v, _ := p.q.Get("installed"); v != nil {
			for _, x := range v.([]any) {
				installed[x.(string)] = true
			}
		}
		fw, _ := p.q.Value("faster_whisper").(bool)
		// Usable on a pinned arch, or where a file that matches the pin is on disk.
		arch := "x86_64"
		if v, ok := p.q.Value("arch").(string); ok {
			arch = v
		}
		onDisk, _ := p.q.Value("q4k_on_disk").(bool)
		ok := ParakeetQuantRefusal(ParakeetDefaultModel, arch) == "" || onDisk
		e, m, line := ChooseEngine(hw, installed, fw, ok)
		return obj("order", strList(HardwareOrder(hw)), "engine", e, "model", m, "line", line), nil
	case "resident_line":
		o := ParseResidentLine(p.bytes("line_b64"))
		var kind any
		if k := ReplyKind(o); k != "" {
			kind = k
		}
		return obj("ready", IsReadyLine(o), "kind", kind), nil
	case "fetch_file":
		var size *int64
		if v, _ := p.q.Get("size"); v != nil {
			if i, ok := v.(pyjson.Int); ok {
				n, _ := new(big.Int).SetString(i.Text, 10)
				x := n.Int64()
				size = &x
			}
		}
		dest := pyPathStr(p.str("dest"))
		err := FetchFile(p.str("url"), p.str("sha256"), dest, size, ProcessModelEnv())
		var ans *pyjson.Object
		var fe *FetchError
		switch {
		case err == nil:
			ans = obj("ok", true)
		case errors.As(err, &fe) && fe.Verify:
			ans = obj("error", "verify")
		default:
			ans = obj("error", "download")
		}
		files := pyjson.NewObject()
		listing := fetchListing(filepath.Dir(dest))
		names := make([]string, 0, len(listing))
		for n := range listing {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			files.Set(n, listing[n])
		}
		ans.Set("files", files)
		return ans, nil
	case "transcribe":
		e, eerr := NewWhisperCpp(pyPathStr(p.str("model")))
		if eerr != nil {
			return errAnswer(eerr.Class(), eerr.Msg), nil
		}
		t, err := e.Transcribe(p.bytes("wav_b64"), p.optStr("language"))
		if err != nil {
			return errAnswer("RuntimeError", err.Error()), nil
		}
		seg := obj("start", 0.0, "end", t.DurationS, "text", t.Text)
		return obj("text", t.Text, "segments", []any{seg}, "duration_s", t.DurationS), nil
	case "wav_duration":
		d, err := WavDurationS(p.bytes("wav_b64"))
		if err != nil {
			return obj("error", "unreadable"), nil
		}
		return obj("seconds", d), nil
	case "to_wav":
		out, err := ToWav16kMono(p.bytes("raw_b64"))
		if err != nil {
			if err.BadAudio() {
				return obj("error", "bad_audio"), nil
			}
			return obj("error", "internal"), nil
		}
		return obj("wav_b64", b64s(out)), nil
	case "multipart":
		payload, partType, err := ExtractMultipartAudio(p.bytes("body_b64"), p.str("content_type"))
		if err != nil {
			return errAnswer("ValueError", err.Error()), nil
		}
		return obj("payload_b64", b64s(payload), "part_type", partType), nil
	case "content_length":
		req := &request{}
		if v, _ := p.q.Get("value"); v != nil {
			req.names, req.values = []string{"Content-Length"}, []string{p.str("value")}
		}
		n := contentLength(req)
		if n == nil {
			return obj("length", nil), nil
		}
		return obj("length", pyjson.Int{Text: n.String()}), nil
	case "is_loopback":
		return obj("loopback", IsLoopback(p.str("address"))), nil
	case "armed_name":
		return obj("name", ArmedName(p.str("pane_key"))), nil
	case "arm":
		if err := p.lay(); err != nil {
			return nil, err
		}
		nowRaw, _ := p.q.Get("now")
		e, err := ArmRaw(p.str("pane_key"), p.num("minutes"), filepath.Join(p.root, "armed"), p.num("now"), nowRaw)
		var ans *pyjson.Object
		if err != nil {
			ans = obj("error", PyOSErrorClass(err))
		} else {
			ans = obj("entry", obj("pane_key", e.PaneKey, "armed_at", pyNum(p.q, "now", e.ArmedAt), "expires_at", e.ExpiresAt))
		}
		ans.Set("tree", treeOf(p.root))
		return ans, nil
	case "disarm":
		if err := p.lay(); err != nil {
			return nil, err
		}
		removed, err := Disarm(p.str("pane_key"), filepath.Join(p.root, "armed"))
		var ans *pyjson.Object
		if err != nil {
			ans = obj("error", PyOSErrorClass(err))
		} else {
			ans = obj("removed", removed)
		}
		ans.Set("tree", treeOf(p.root))
		return ans, nil
	case "is_armed":
		if err := p.lay(); err != nil {
			return nil, err
		}
		return obj("armed", IsArmed(p.str("pane_key"), filepath.Join(p.root, "armed"), p.num("now"))), nil
	case "deliver":
		if err := p.lay(); err != nil {
			return nil, err
		}
		var calls []any
		res, err := Deliver(p.str("pane"), p.str("text"), p.str("mode"), filepath.Join(p.root, "armed"), p.optStr("pane_key"),
			p.num("now"), func(text string) error {
				calls = append(calls, "factory", "send "+p.str("pane")+" "+text)
				return nil
			})
		if err != nil {
			return nil, err
		}
		var reason any
		if res.Reason != nil {
			reason = *res.Reason
		}
		if calls == nil {
			calls = []any{}
		}
		return obj("delivered", res.Delivered, "reason", reason, "calls", calls), nil
	case "cleanup":
		return p.cleanup()
	case "ptt":
		return p.ptt()
	case "client":
		return p.client()
	default:
		return nil, errors.New("unknown op " + op)
	}
}

// pyNum keeps an int query value an int, as Python's arithmetic keeps it.
func pyNum(q *pyjson.Object, k string, f float64) any {
	if v, _ := q.Get(k); v != nil {
		if i, ok := v.(pyjson.Int); ok {
			return i
		}
	}
	return f
}

// resident is op_resident: a resident engine built, started, run through
// the query's steps, then stopped.
func (p *prober) resident() *pyjson.Object {
	env := ProcessModelEnv()
	if v, _ := p.q.Get("load_timeout_s"); v != nil {
		env.LoadTimeoutS = p.num("load_timeout_s")
	}
	if v, _ := p.q.Get("clip_timeout_s"); v != nil {
		env.ClipTimeoutS = p.num("clip_timeout_s")
	}
	var e *Engine
	var eerr *EngineError
	if p.str("engine") == "moonshine" {
		e, eerr = NewMoonshine(p.str("model"), env)
	} else {
		e, eerr = NewParakeet(p.str("model"), env)
	}
	if eerr == nil {
		eerr = e.Start()
	}
	if eerr != nil {
		return obj("start", errAnswer(eerr.Class(), eerr.Msg))
	}
	defer e.Stop()
	steps := []any{}
	v, _ := p.q.Get("steps")
	list, _ := v.([]any)
	for _, raw := range list {
		step, _ := raw.(*pyjson.Object)
		if _, ok := step.Get("wait"); ok {
			sp := &prober{q: step}
			steps = append(steps, obj("state", e.WaitReady(sp.num("wait"))))
			continue
		}
		clip, _ := base64.StdEncoding.DecodeString(step.Value("clip").(string))
		t, err := e.Transcribe(clip, nil)
		if err != nil {
			var ee *EngineError
			if errors.As(err, &ee) {
				steps = append(steps, errAnswer(ee.Class(), ee.Msg))
			} else {
				steps = append(steps, errAnswer("RuntimeError", err.Error()))
			}
			continue
		}
		steps = append(steps, obj("text", t.Text, "duration_s", t.DurationS))
	}
	return obj("start", "ok", "steps", steps)
}

func strList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func pinsAnswer() *pyjson.Object {
	names := append([]string(nil), FasterWhisperModelNames...)
	sort.Strings(names)
	models, sizes := pyjson.NewObject(), pyjson.NewObject()
	for _, name := range MoonshineModelNames {
		var files []any
		for _, f := range MoonshineModels[name] {
			files = append(files, []any{f.Name, f.SHA256, pyjson.Int{Text: strconv.FormatInt(f.Size, 10)}})
		}
		models.Set(name, files)
		sizes.Set(name, pyjson.Int{Text: strconv.FormatInt(MoonshineSizeMB(name), 10)})
	}
	pmodels := pyjson.NewObject()
	pfile := func(f ParakeetFile) []any {
		return []any{f.Name, f.SHA256, pyjson.Int{Text: strconv.FormatInt(f.Size, 10)}}
	}
	for _, name := range ParakeetModelNames {
		m := ParakeetModels[name]
		pmodels.Set(name, obj("dir", m.Dir, "source", pfile(m.Source), "quantized", pfile(m.Quantized)))
	}
	tr, ow, mw := pyjson.NewObject(), pyjson.NewObject(), pyjson.NewObject()
	for _, n := range FixtureNames {
		tr.Set(n, FixtureTranscripts[n])
		ow.Set(n, FixtureObservedWER[n])
		mw.Set(n, FixtureMaxWER[n])
	}
	return obj("faster_whisper_model_names", strList(names), "faster_whisper_test_model", FasterWhisperTestModel,
		"fixture_transcripts", tr, "fixture_observed_wer", ow, "fixture_max_wer", mw,
		"fixture_mean_max_wer", FixtureMeanMaxWER, "whisper_cpp_binary", WhisperCppBinary,
		"whisper_cpp_auto_language", WhisperCppAutoLanguage, "whisper_cpp_timeout_s", WhisperCppTimeoutS,
		"faster_whisper_revisions", fasterWhisperRevisionsObj(),
		"faster_whisper_sizes", obj("tiny.en", pyjson.Int{Text: strconv.FormatInt(FasterWhisperSizes["tiny.en"], 10)}),
		"moonshine_binary", MoonshineBinary, "moonshine_timeout_s", MoonshineTimeoutS,
		"moonshine_default_model", MoonshineDefaultModel, "moonshine_dir_arch", MoonshineDirArch,
		"moonshine_base_url", MoonshineBaseURL, "moonshine_revision", MoonshineRevision,
		"moonshine_models", models, "moonshine_size_mb", sizes,
		"parakeet_binary", ParakeetBinary, "parakeet_quantize_binary", ParakeetQuantizeBinary,
		"parakeet_default_model", ParakeetDefaultModel, "parakeet_base_url", ParakeetBaseURL,
		"parakeet_quant", ParakeetQuant, "parakeet_quant_arches", strList(ParakeetQuantArches), "parakeet_models", pmodels,
		"valid_engine_names", ValidEngineNames,
		"hardware", obj("parakeet_min_ram_gb", ParakeetMinRAMGB, "parakeet_min_cpus", pyjson.Int{Text: strconv.Itoa(ParakeetMinCPUs)},
			"moonshine_min_ram_gb", MoonshineMinRAMGB),
		"resident", obj("protocol", ResidentProtocol, "max_frame_bytes", pyjson.Int{Text: strconv.Itoa(MaxFrameBytes)},
			"load_timeout_s", LoadTimeoutS, "clip_timeout_s", ClipTimeoutS, "stop_grace_s", StopGraceS),
		"cleanup_prompt", CleanupPrompt)
}

// lay writes the query's files under the probe directory, as _lay does.
func (p *prober) lay() error {
	v, _ := p.q.Get("files")
	files, _ := v.(*pyjson.Object)
	if files == nil {
		return nil
	}
	for _, rel := range files.Keys() {
		spec, _ := files.Value(rel).(*pyjson.Object)
		path := filepath.Join(p.root, rel)
		mode := func(def int64) os.FileMode {
			if m, ok := spec.Value("mode").(pyjson.Int); ok {
				n, _ := new(big.Int).SetString(m.Text, 10)
				return os.FileMode(n.Int64())
			}
			return os.FileMode(def)
		}
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(path, 0o777); err != nil {
				return err
			}
			if err := os.Chmod(path, mode(0o700)); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		var data []byte
		if b, ok := spec.Value("b64").(string); ok {
			data, _ = base64.StdEncoding.DecodeString(b)
		} else {
			t, _ := spec.Value("text").(string)
			data = []byte(t)
		}
		if err := os.WriteFile(path, data, 0o666); err != nil {
			return err
		}
		if err := os.Chmod(path, mode(0o600)); err != nil {
			return err
		}
	}
	return nil
}

// treeOf is _tree: every path under root with its mode, and a file's text
// or, when it is not UTF-8, its base64.
func treeOf(root string) *pyjson.Object {
	out := pyjson.NewObject()
	var paths []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	for _, path := range paths {
		rel, _ := filepath.Rel(root, path)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		mode := pyjson.Int{Text: big.NewInt(int64(st.Mode().Perm())).String()}
		if st.IsDir() {
			out.Set(rel+"/", obj("mode", mode))
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if t, xerr := pystr.DecodeStrict(raw); xerr == nil && !pystr.HasSurrogate(t) {
			out.Set(rel, obj("mode", mode, "text", t))
		} else {
			out.Set(rel, obj("mode", mode, "b64", b64s(raw)))
		}
	}
	return out
}

// PyOSErrorClass is the class Python raises for a file error.
func PyOSErrorClass(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOENT:
			return "FileNotFoundError"
		case syscall.EEXIST:
			return "FileExistsError"
		case syscall.ENOTDIR:
			return "NotADirectoryError"
		case syscall.EISDIR:
			return "IsADirectoryError"
		case syscall.EACCES, syscall.EPERM:
			return "PermissionError"
		}
	}
	if errors.Is(err, fs.ErrExist) {
		return "FileExistsError"
	}
	return "OSError"
}

type scriptedTransport struct {
	spec  *pyjson.Object
	calls []any
}

func (t *scriptedTransport) Complete(model, system, user string) (string, *big.Int, *big.Int, error) {
	t.calls = append(t.calls, obj("model", model, "system", system, "user", user))
	if r, _ := t.spec.Get("raise"); truthy(r) {
		return "", nil, nil, errors.New("the transport failed")
	}
	text, _ := t.spec.Value("text").(string)
	in, out := big.NewInt(0), big.NewInt(0)
	if u, ok := t.spec.Value("usage").(*pyjson.Object); ok {
		if v, ok := u.Value("input_tokens").(pyjson.Int); ok {
			in.SetString(v.Text, 10)
		}
		if v, ok := u.Value("output_tokens").(pyjson.Int); ok {
			out.SetString(v.Text, 10)
		}
	}
	return text, in, out, nil
}

func (p *prober) cleanup() (*pyjson.Object, error) {
	if err := p.lay(); err != nil {
		return nil, err
	}
	cfgv, _ := p.q.Value("config").(*pyjson.Object)
	cfg := CleanupConfig{DataDir: filepath.Join(p.root, "data")}
	if cfgv != nil {
		cfg.On = truthy(cfgv.Value("voice_cleanup"))
		if m, ok := cfgv.Value("voice_cleanup_model").(string); ok {
			cfg.Model = &m
		}
	}
	spec, _ := p.q.Value("transport").(*pyjson.Object)
	tr := &scriptedTransport{spec: spec}
	res, err := CleanTranscript(p.str("text"), cfg, tr, time.Now())
	if err != nil {
		return nil, err
	}
	var journal []any
	raw, rerr := os.ReadFile(filepath.Join(cfg.DataDir, "gateway", "turns.jsonl"))
	if rerr == nil {
		for _, ln := range pystr.Splitlines(string(raw)) {
			v, derr := pyjson.LoadsPy(ln, 900)
			if derr != nil {
				return nil, errors.New(derr.Msg)
			}
			if o, ok := v.(*pyjson.Object); ok {
				c, _ := o.Get("created_at")
				if truthy(c) {
					c = "<created_at>"
				}
				o.Set("created_at", c)
			}
			journal = append(journal, v)
		}
	}
	if journal == nil {
		journal = []any{}
	}
	calls := tr.calls
	if calls == nil {
		calls = []any{}
	}
	var reason any
	if res.Reason != nil {
		reason = *res.Reason
	}
	return obj("text", res.Text, "cleaned", res.Cleaned, "reason", reason, "calls", calls, "journal", journal), nil
}

// scriptedStream and scriptedClient play back a ptt query.
type scriptedStream struct {
	chunks []any
	events *[]any
}

func (s *scriptedStream) Start() error { *s.events = append(*s.events, "start"); return nil }
func (s *scriptedStream) Stop() error  { *s.events = append(*s.events, "stop"); return nil }
func (s *scriptedStream) Read(frames int) ([]byte, bool, error) {
	*s.events = append(*s.events, "read "+itoa(frames))
	if len(s.chunks) == 0 {
		return nil, false, nil
	}
	c, _ := s.chunks[0].([]any)
	s.chunks = s.chunks[1:]
	b, _ := base64.StdEncoding.DecodeString(c[0].(string))
	return b, truthy(c[1]), nil
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

type scriptedClient struct {
	transcribes, delivers []any
	events                *[]any
}

func next(list *[]any) *pyjson.Object {
	a, _ := (*list)[0].(*pyjson.Object)
	*list = (*list)[1:]
	return a
}

func (c *scriptedClient) Transcribe(wav []byte) (any, error) {
	*c.events = append(*c.events, obj("transcribe", b64s(wav)))
	a := next(&c.transcribes)
	if e, has := a.Get("error"); has {
		return nil, &ServerError{pyStr(e)}
	}
	return a.Value("reply"), nil
}

func (c *scriptedClient) Deliver(pane, text, mode string) (any, error) {
	*c.events = append(*c.events, obj("deliver", []any{pane, text, mode}))
	a := next(&c.delivers)
	if e, has := a.Get("error"); has {
		return nil, &ServerError{pyStr(e)}
	}
	return a.Value("reply"), nil
}

func (p *prober) list(k string) []any {
	v, _ := p.q.Value(k).([]any)
	return append([]any(nil), v...)
}

func (p *prober) ptt() (*pyjson.Object, error) {
	events := []any{}
	printed := []any{}
	streams := p.list("streams")
	client := &scriptedClient{transcribes: p.list("transcribe"), delivers: p.list("deliver"), events: &events}
	keys := pystr.Runes(p.str("keys"))
	i := 0
	nextKey := func() (string, bool) {
		if i >= len(keys) {
			return "", false
		}
		i++
		return string(keys[i-1]), true
	}
	open := func() (Stream, error) {
		events = append(events, "open")
		var chunks []any
		if len(streams) > 0 {
			chunks, _ = streams[0].([]any)
			streams = streams[1:]
		}
		return &scriptedStream{chunks: append([]any(nil), chunks...), events: &events}, nil
	}
	frames := 1600
	if v, ok := p.q.Value("chunk_frames").(pyjson.Int); ok {
		n, _ := new(big.Int).SetString(v.Text, 10)
		frames = int(n.Int64())
	}
	results, err := RunPTT(p.str("pane"), client, nextKey, open, frames, func(s string) { printed = append(printed, s) })
	if err != nil {
		return nil, err
	}
	rs := []any{}
	for _, r := range results {
		rs = append(rs, []any{r.Text, r.Reply})
	}
	return obj("results", rs, "printed", printed, "events", events), nil
}

func (p *prober) client() (*pyjson.Object, error) {
	if err := p.lay(); err != nil {
		return nil, err
	}
	c, err := NewHTTPClient(p.str("url"), filepath.Join(p.root, "data"), 5*time.Second)
	var reply any
	if err == nil {
		if p.str("call") == "transcribe" {
			reply, err = c.Transcribe(p.bytes("wav_b64"))
		} else {
			reply, err = c.Deliver(p.str("pane"), p.str("text"), p.str("mode"))
		}
	}
	var se *ServerError
	if errors.As(err, &se) {
		return errAnswer("VoiceServerError", se.Msg), nil
	}
	if err != nil {
		return nil, err
	}
	return obj("reply", reply), nil
}

// fasterWhisperRevisionsObj is pins.FASTER_WHISPER_REVISIONS in pins order.
func fasterWhisperRevisionsObj() *pyjson.Object {
	o := pyjson.NewObject()
	for _, k := range FasterWhisperRevisionOrder {
		o.Set(k, FasterWhisperRevisions[k])
	}
	return o
}
