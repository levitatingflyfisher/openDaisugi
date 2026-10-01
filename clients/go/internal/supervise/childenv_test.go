package supervise

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"daisugi-verify/internal/agentprof"
)

// Every child gets a scrubbed environment (MX-R-3): no key variable, and
// for a sprig child no ambient key or redirect.
func TestEveryChildGetsAScrubbedEnvironment(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin", "HOME=/home/x", "ANTHROPIC_API_KEY=k", "ANTHROPIC_AUTH_TOKEN=k",
		"CLAUDE_CODE_OAUTH_TOKEN=k", "OPENAI_API_KEY=k", "XAI_API_KEY=k", "GITHUB_TOKEN=k",
		"MY_KEY_VAR=k", "ANTHROPIC_BASE_URL=http://127.0.0.1:1", "SPRIG_BACKEND=api",
		"SPRIG_MODEL=m", "TOKENIZERS_PARALLELISM=false",
	}
	names := func(env []string) []string {
		var out []string
		for _, kv := range env {
			n, _, _ := strings.Cut(kv, "=")
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	var empty agentprof.Profiles
	if got := names(childEnv(environ, empty, true)); !reflect.DeepEqual(got, []string{"HOME", "MY_KEY_VAR", "PATH", "TOKENIZERS_PARALLELISM"}) {
		t.Fatalf("sprig: %v", got)
	}
	want := []string{"ANTHROPIC_BASE_URL", "HOME", "MY_KEY_VAR", "PATH", "SPRIG_BACKEND", "SPRIG_MODEL", "TOKENIZERS_PARALLELISM"}
	if got := names(childEnv(environ, empty, false)); !reflect.DeepEqual(got, want) {
		t.Fatalf("claude: %v", got)
	}
}
