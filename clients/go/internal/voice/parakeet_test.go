package voice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// On a machine with no pinned Q4_K result, nothing is fetched or made:
// one line says so before the 1.2 GB download.
func TestAnUnpinnedMachineRefusesBeforeFetching(t *testing.T) {
	vars := map[string]string{
		"XDG_CACHE_HOME":   t.TempDir(),
		ParakeetBaseURLEnv: "http://127.0.0.1:1/never",
	}
	var said []string
	env := ModelEnv{
		Lookup: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
		Home:   "/nonexistent",
		Say:    func(s string) { said = append(said, s) },
	}
	_, err := ensureParakeetOn("v2", "/nonexistent/q", env, "aarch64")
	var qe *QuantizeError
	if !errors.As(err, &qe) || err.Error() != ParakeetQuantRefusal("v2", "aarch64") {
		t.Fatalf("not refused: %v", err)
	}
	if len(said) != 0 {
		t.Fatalf("said %q", said)
	}
	if ParakeetQuantRefusal("v2", "x86_64") != "" {
		t.Fatal("x86_64 refused")
	}
}

// Usable means the arch is pinned, or a Q4_K file that matches the pin is
// already on disk.
func TestParakeetUsableFollowsArchAndAPinnedFile(t *testing.T) {
	cache := t.TempDir()
	env := ModelEnv{
		Lookup: func(k string) (string, bool) { return cache, k == "XDG_CACHE_HOME" },
		Home:   "/nonexistent",
	}
	if !ParakeetUsable("v2", env, "x86_64") || ParakeetUsable("v2", env, "aarch64") {
		t.Fatal("arch alone decides with no file")
	}
	m := ParakeetModels["v2"]
	out := ParakeetDir("v2", env) + "/" + m.Quantized.Name
	if err := os.MkdirAll(ParakeetDir("v2", env), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("not the pinned file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ParakeetUsable("v2", env, "aarch64") {
		t.Fatal("a file that fails the pin counts")
	}
	sum := sha256.Sum256([]byte("not the pinned file"))
	saved := ParakeetModels["v2"]
	defer func() { ParakeetModels["v2"] = saved }()
	pinned := saved
	pinned.Quantized.SHA256 = hex.EncodeToString(sum[:])
	ParakeetModels["v2"] = pinned
	if !ParakeetUsable("v2", env, "aarch64") {
		t.Fatal("a file that matches the pin is lost")
	}
}
