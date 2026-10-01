package voice

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
)

// StatusKind is what this build calls itself in voice status.
const StatusKind = "go"

// StatusFreshS: a partial download not written for this long is a fetch
// that stopped (ruling VO-R-1).
const StatusFreshS = 30.0

// Fetch is the progress of a model download.
type Fetch struct{ Done, Total int64 }

// Status is status.voice_status: the engine, its model, and a fetch.
type Status struct {
	Engine, Model string
	ModelBytes    int64
	Fetched       bool
	Fetch         *Fetch
}

type statusFile struct {
	name string
	size int64
}

func fileSize(p string) (int64, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	return st.Size(), true
}

// partSizes is the largest partial file of path, and whether any is fresh.
func partSizes(path string, now time.Time) (int64, bool) {
	dir, base := filepath.Split(path)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var best int64
	fresh := false
	for _, e := range ents {
		n := e.Name()
		if n != base+".part" && !strings.HasPrefix(n, base+".part-") {
			continue
		}
		st, err := os.Stat(dir + n)
		if err != nil {
			continue
		}
		if st.Size() > best {
			best = st.Size()
		}
		if now.Sub(st.ModTime()).Seconds() <= StatusFreshS {
			fresh = true
		}
	}
	return best, fresh
}

func statusProgress(dir string, files []statusFile, now time.Time, also []string) *Fetch {
	var done, total int64
	active := false
	for _, f := range files {
		total += f.size
		if n, ok := fileSize(dir + "/" + f.name); ok && n == f.size {
			done += f.size
			continue
		}
		part, fresh := partSizes(dir+"/"+f.name, now)
		if part > f.size {
			part = f.size
		}
		done += part
		active = active || fresh
	}
	for _, a := range also {
		if _, fresh := partSizes(dir+"/"+a, now); fresh {
			active = true
		}
	}
	if !active {
		return nil
	}
	return &Fetch{done, total}
}

func pathBytes(p string) (int64, bool) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, false
	}
	if st.Mode().IsRegular() {
		return st.Size(), true
	}
	if !st.IsDir() {
		return 0, false
	}
	ents, _ := os.ReadDir(p)
	var total int64
	for _, e := range ents {
		if i, err := e.Info(); err == nil && i.Mode().IsRegular() {
			total += i.Size()
		}
	}
	return total, true
}

// VoiceStatus is status.voice_status for this binary. now is the clock.
func VoiceStatus(engine, model string, env ModelEnv, now time.Time) Status {
	env.Say = nil
	engine, model = ResolveEngine(engine, model, env)
	out := Status{Engine: engine, Model: model}
	_, curatedMoon := MoonshineModels[model]
	pk, curatedPara := ParakeetModels[model]
	switch {
	case engine == "moonshine" && curatedMoon:
		dir := MoonshineDir(model, env)
		var pinned []statusFile
		out.Fetched = true
		for _, f := range MoonshineModels[model] {
			pinned = append(pinned, statusFile{f.Name, f.Size})
			out.ModelBytes += f.Size
			if n, ok := fileSize(dir + "/" + f.Name); !ok || n != f.Size {
				out.Fetched = false
			}
		}
		out.Fetch = statusProgress(dir, pinned, now, nil)
	case engine == "parakeet" && curatedPara:
		dir := ParakeetDir(model, env)
		out.ModelBytes = pk.Quantized.Size
		n, ok := fileSize(dir + "/" + pk.Quantized.Name)
		out.Fetched = ok && n == pk.Quantized.Size
		if !out.Fetched {
			out.Fetch = statusProgress(dir, []statusFile{{pk.Source.Name, pk.Source.Size}}, now, []string{pk.Quantized.Name})
		}
	case engine == "moonshine" || engine == "parakeet" || engine == "whisper.cpp":
		out.ModelBytes, out.Fetched = pathBytes(pyPathStr(model))
	}
	return out
}

// StatusMB is the status line's megabytes: 10**6, rounded half up.
func StatusMB(n int64) int64 { return (n + 500_000) / 1_000_000 }

// Describe is status.describe: the one line voice status prints without --json.
func (s Status) Describe() string {
	if s.Fetch != nil {
		return "Getting the " + s.Engine + " model, " + i64s(StatusMB(s.Fetch.Done)) + " of " + i64s(StatusMB(s.Fetch.Total)) + " MB."
	}
	what := "The " + s.Engine + " " + s.Model + " model"
	if s.Fetched {
		return what + " is on this box (" + i64s(StatusMB(s.ModelBytes)) + " MB)."
	}
	return what + " is not on this box yet."
}

func i64s(n int64) string { return strconv.FormatInt(n, 10) }

func pyBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// JSON is json.dumps of the status dict, in the oracle's key order.
func (s Status) JSON() string {
	out := `{"engine": ` + pyjson.Dumps(s.Engine, true) + `, "model": ` + pyjson.Dumps(s.Model, true) +
		`, "model_bytes": ` + i64s(s.ModelBytes) + `, "fetched": ` + pyBool(s.Fetched) +
		`, "kind": "` + StatusKind + `"`
	if s.Fetch != nil {
		out += `, "fetch": {"done": ` + i64s(s.Fetch.Done) + `, "total": ` + i64s(s.Fetch.Total) + `}`
	}
	return out + "}"
}
