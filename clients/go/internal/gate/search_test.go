package gate

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func bashAt(cmd string, cwd any) string {
	m := map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_input": map[string]string{"command": cmd}}
	if cwd != nil {
		m["cwd"] = cwd
	}
	p, _ := json.Marshal(m)
	return string(p)
}

// Each reaches the default data directory's secrets from /work or from
// the home directory, the same set as tests/test_gate_search_rule.py.
func TestASearchThatReachesTheSecretsIsRefused(t *testing.T) {
	root, env := setupAllowAll(t)
	const home = "/home/user"
	for _, c := range [][2]string{
		{"grep -r -m 1 token", home},
		{"rg -t py token", home},
		{"rg -g '*' token", home},
		{"grep -r -A 2 token", home},
		{"rg -e token", home},
		{"echo $'it\\'s' ; rg token ~", "/work"},
		{"grep -d recurse token ~", "/work"},
		{"grep --directories=recurse token ~", "/work"},
		{"grep --recur token ~", "/work"},
		{"grep --deref token ~", "/work"},
		{"grep -2 token ~", "/work"},
		{"grep --depth=3 token ~", "/work"},
		{"zgrep -r token ~", "/work"},
		{"ugrep token ~", "/work"},
		{"ug token", home},
		{"git grep --no-index token", home},
		{"git -C ~ grep --no-index token", "/work"},
		{"rg token /*", "/work"},
		{"rg --hidden token ~/.*", "/work"},
		{"rg --hidden token ~/.o*", "/work"},
		{"rg token /hom?/user", "/work"},
		{"rg token /[h]ome/user", "/work"},
		{"rg token {/home/user,/x}", "/work"},
		{"find /* -type f -exec cat", "/work"},
		{"find -- ~ -type f -exec cat", "/work"},
		{"find -O3 ~ -type f -exec cat", "/work"},
		{"find -D stat ~ -type f -exec cat", "/work"},
		{"cd - && rg token", "/work"},
		{"cd $HOME/x/.. && rg token", "/work"},
		{"rg token $PWD", "/work"},
		{"H=~; rg token $H", "/work"},
		{"rg token ~+", "/work"},
		{"env rg token ~", "/work"},
		{"timeout 5 rg token ~", "/work"},
		{"/usr/bin/rg token ~", "/work"},
		{"echo ~ | xargs rg token", "/work"},
		{"bash -c 'rg token ~'", "/work"},
		{"rg token -- ~", "/work"},
		{"rg --files ~", "/work"},
		{"grep -r token .", "user"},
	} {
		res := run(t, root, env, "enforce", bashAt(c[0], c[1]))
		if !res.Native {
			continue // handed to Python, which the corpus compares
		}
		if res.Exit != 2 || !strings.Contains(res.Stderr, searchRefusal) {
			t.Errorf("%q from %s: want the search refusal, got %+v", c[0], c[1], res)
		}
	}
}

func TestANarrowSearchIsLeftToTheEnvelope(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, c := range [][2]string{
		{"rg token src", "/work"},
		{"rg -t py token src", "/work"},
		{"grep -r token /work/src", "/home/user"},
		{"grep -rn token src lib", "/home/user"},
		{"rg -e token /work/src", "/home/user"},
		{"grep token ~/notes.txt", "/work"},
		{"find ~ -name x", "/work"},
		{"rg 'foo$' src", "/work"},
		{"rg token /work/*", "/work"},
		{"ls ~/.opendaisugi/*", "/work"},
	} {
		if res := run(t, root, env, "enforce", bashAt(c[0], c[1])); strings.Contains(res.Stderr, searchRefusal) {
			t.Errorf("%q from %s: the search refusal fired: %+v", c[0], c[1], res)
		}
	}
}

func TestAGlobThatNamesASecretFileIsRefusedByThePaneRule(t *testing.T) {
	root, env := setupAllowAll(t)
	for _, cmd := range []string{
		"cat ~/.opendaisugi/*/*/*",
		"cat ~/.opendaisugi/coppice/web/*",
		"cat ~/.opendaisugi/coppice/web/ca/*.key",
		"cp ~/.opendaisugi/coppice/voice/tok?n /work/x",
	} {
		if res := run(t, root, env, "enforce", bashAt(cmd, "/work")); res.Exit != 2 || !strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%q: want the pane refusal, got %+v", cmd, res)
		}
	}
}

func TestAGrepWithBothFilePathAndPathChecksBoth(t *testing.T) {
	root, env := setupAllowAll(t)
	grep := func(path string) string {
		p, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Grep", "cwd": "/work",
			"tool_input": map[string]string{"pattern": ".", "file_path": "/work", "path": path}})
		return string(p)
	}
	if res := run(t, root, env, "enforce", grep("/home/user")); !strings.Contains(res.Stderr, searchRefusal) {
		t.Errorf("home: got %+v", res)
	}
	if res := run(t, root, env, "enforce", grep(testDataDir+"/web")); !strings.Contains(res.Stderr, paneRefusal) {
		t.Errorf("web dir: got %+v", res)
	}
	noCwd, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Grep",
		"tool_input": map[string]string{"pattern": "."}})
	if res := run(t, root, env, "enforce", string(noCwd)); !strings.Contains(res.Stderr, searchRefusal) {
		t.Errorf("no cwd: got %+v", res)
	}
}

func TestRoundTwoRootsAreRefusedByTheSearchRule(t *testing.T) {
	root, env := setupAllowAll(t)
	many := "{"
	for i := 1; i <= 64; i++ {
		many += "/a" + strconv.Itoa(i) + ","
	}
	many += "/home/user}"
	for _, c := range [][2]string{
		{"rg token /home/use{r..r}", "/work"},
		{"rg token /{a..z}ome/user", "/work"},
		{"rg token /home/use{01..03}", "/work"},
		{"rg token /home/{a..1}", "/work"},
		{"rg token " + many, "/work"},
		{"rg token /home/us{1..100}", "/work"},
		{"rg token {1..100}", "/home/user"},
		{`sh -c "sh -c 'sh -c \"rg token ~\"'"`, "/work"},
	} {
		res := run(t, root, env, "enforce", bashAt(c[0], c[1]))
		if !res.Native {
			continue
		}
		if res.Exit != 2 || !strings.Contains(res.Stderr, searchRefusal) {
			t.Errorf("%q: want the search refusal, got %+v", c[0], res)
		}
	}
	if res := run(t, root, env, "enforce", bashAt("cat ~/.opendaisugi/coppice/web/toke{m..o}", "/work")); res.Native && !strings.Contains(res.Stderr, paneRefusal) {
		t.Errorf("sequence to a secret file: got %+v", res)
	}
	for _, cmd := range []string{"rg token /work/src{1..3}", "rg token /home/{0..9..2}", "echo {1..100}", "cat /work/f{1..100}"} {
		if res := run(t, root, env, "enforce", bashAt(cmd, "/work")); strings.Contains(res.Stderr, searchRefusal) || strings.Contains(res.Stderr, paneRefusal) {
			t.Errorf("%q: refused: %+v", cmd, res)
		}
	}
}

func TestSequenceBracesExpandLikeBash(t *testing.T) {
	for in, want := range map[string]string{
		"a{1..3}":      "a1 a2 a3",
		"{c..a}":       "c b a",
		"{1..9..4}":    "1 5 9",
		"{5..1..-2}":   "5 3 1",
		"x{a,b}{1..2}": "xa1 xa2 xb1 xb2",
		"{}":           "{}",
	} {
		var out []string
		if !(&runner{}).braceExpand(in, &out) || strings.Join(out, " ") != want {
			t.Errorf("%s: got %v", in, out)
		}
	}
	var out []string
	if (&runner{}).braceExpand("{01..03}", &out) {
		t.Error("a zero-padded sequence was expanded")
	}
}
