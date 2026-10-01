package voice

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A download that was killed leaves dest.part-<random>. The next download
// of that file removes the ones older than a day, and only those: a
// younger one may belong to another download that runs.
func TestFetchSweepsOnlyStalePartFiles(t *testing.T) {
	body := []byte("model bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	sum := sha256.Sum256(body)
	dir := t.TempDir()
	dest := filepath.Join(dir, "m.gguf")
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("m.gguf.part-111", 25*time.Hour)
	fresh := write("m.gguf.part-222", time.Hour)
	other := write("n.gguf.part-333", 48*time.Hour)
	env := ModelEnv{Lookup: func(string) (string, bool) { return "", false }, Home: "/nonexistent"}
	size := int64(len(body))
	if err := FetchFile(srv.URL+"/m.gguf", hex.EncodeToString(sum[:]), dest, &size, env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the stale orphan stays")
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(p), err)
		}
	}
}
