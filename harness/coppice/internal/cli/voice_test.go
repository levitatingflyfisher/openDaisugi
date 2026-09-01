package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

func TestVoiceConfigReadsTheVoiceTable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := voiceConfig(); got.Off || got.URL != "" {
		t.Fatalf("no file: %+v, want voice started by the server", got)
	}
	off := false
	if err := config.Save(config.Config{Voice: config.Voice{Enabled: &off, URL: "http://127.0.0.1:9",
		TokenFile: "/t", Args: []string{"--data-dir", "/d"}}}); err != nil {
		t.Fatal(err)
	}
	got := voiceConfig()
	if !got.Off || got.URL != "http://127.0.0.1:9" || got.TokenFile != "/t" || strings.Join(got.Args, " ") != "--data-dir /d" {
		t.Fatalf("voiceConfig() = %+v", got)
	}
	if err := os.WriteFile(config.Path(), []byte("not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	got = voiceConfig()
	if !got.Off || !strings.Contains(got.OffReason, config.Path()) || got.OffFix == "" {
		t.Fatalf("a file that does not read must turn voice off and say why: %+v", got)
	}
}
