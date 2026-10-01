package voice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func statusEnv(t *testing.T) ModelEnv {
	home := t.TempDir()
	return ModelEnv{Lookup: func(k string) (string, bool) {
		if k == "XDG_CACHE_HOME" {
			return home + "/cache", true
		}
		return "", false
	}, Home: home}
}

func sparse(t *testing.T, path string, size int64, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	then := time.Now().Add(-age)
	if err := os.Chtimes(path, then, then); err != nil {
		t.Fatal(err)
	}
}

func TestStatusKindIsGo(t *testing.T) {
	st := VoiceStatus("moonshine", "tiny", statusEnv(t), time.Now())
	if !strings.Contains(st.JSON(), `"kind": "go"`) {
		t.Fatalf("kind: %s", st.JSON())
	}
	if st.Fetched || st.Fetch != nil {
		t.Fatalf("empty cache: %+v", st)
	}
}

func TestStatusFreshPartIsAFetchAndStaleIsNot(t *testing.T) {
	env := statusEnv(t)
	dir := MoonshineDir("tiny", env)
	sparse(t, dir+"/encoder.ort.part-ab12", 500, 0)
	st := VoiceStatus("moonshine", "tiny", env, time.Now())
	if st.Fetch == nil || st.Fetch.Done != 500 {
		t.Fatalf("fresh part: %+v", st.Fetch)
	}
	st = VoiceStatus("moonshine", "tiny", env, time.Now().Add(time.Hour))
	if st.Fetch != nil {
		t.Fatalf("stale part: %+v", st.Fetch)
	}
}

func TestStatusFetchedWhenEverySizeMatches(t *testing.T) {
	env := statusEnv(t)
	for _, f := range MoonshineModels["tiny"] {
		sparse(t, MoonshineDir("tiny", env)+"/"+f.Name, f.Size, 0)
	}
	st := VoiceStatus("moonshine", "tiny", env, time.Now())
	if !st.Fetched || st.Fetch != nil {
		t.Fatalf("%+v", st)
	}
}
