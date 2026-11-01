package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const treeJSON = `{"nodes":[{"session":"s1","tokens_used":10,"tokens_reserved":100}],"asks":[{"ask_id":"a1"}],"skipped":0}`

func TestTreeRouteRunsTreeStatus(t *testing.T) {
	log, data := fakeDaisugi(t, map[string]string{"tree status": treeJSON}, nil)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := getWith(t, ts.URL+"/api/tree", tok)
	if resp.StatusCode != 200 || !strings.Contains(body, `"a1"`) || !strings.Contains(body, `"source":"daisugi tree status"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	got := calls(t, log)
	if len(got) != 1 || got[0] != "tree status --json --data-dir "+data {
		t.Errorf("argv = %q", got)
	}
}

func TestTreeAnswerRunsAllowAndDenyAndChecksTheInput(t *testing.T) {
	log, data := fakeDaisugi(t, map[string]string{"tree answer": `{"answered":true}`}, nil)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	for _, dec := range []string{"allow", "deny"} {
		resp, body := post(t, ts.URL+"/api/tree/answer", tok, `{"ask_id":"a1","decision":"`+dec+`","reason":"x"}`)
		if resp.StatusCode != 200 || !strings.Contains(body, "answered") {
			t.Fatalf("%s: status %d: %s", dec, resp.StatusCode, body)
		}
	}
	for _, b := range []string{`{"ask_id":"a1","decision":"maybe"}`, `{"ask_id":"-x","decision":"allow"}`, `{"decision":"allow"}`} {
		if resp, _ := post(t, ts.URL+"/api/tree/answer", tok, b); resp.StatusCode != 400 {
			t.Errorf("%s answered %d, want 400", b, resp.StatusCode)
		}
	}
	want := "tree answer a1 allow --data-dir " + data
	if got := calls(t, log); len(got) != 2 || got[0] != want {
		t.Errorf("argv = %q", got)
	}
}

func twoTreeDirs(t *testing.T) (log, main, work string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "argv.log")
	main, work = filepath.Join(dir, "main"), filepath.Join(dir, "work")
	os.Mkdir(main, 0o700)
	os.Mkdir(work, 0o700)
	script := "#!/bin/sh\necho \"$*\" >> " + log + `
case "$*" in
"tree status"*"--data-dir ` + main + `") echo '{"nodes":[{"session":"s1"}],"asks":[{"ask_id":"a1"}],"skipped":1}' ;;
"tree status"*"--data-dir ` + work + `") echo '{"nodes":[{"session":"s1"},{"session":"n-w"}],"asks":[{"ask_id":"a2"}],"skipped":2}' ;;
"tree answer a2"*"--data-dir ` + main + `") echo 'There is no ask a2.' >&2; exit 1 ;;
"tree answer a2"*"--data-dir ` + work + `") echo '{"answered":true}' ;;
"tree answer zz"*) echo 'There is no ask zz.' >&2; exit 1 ;;
*) echo '{}' ;;
esac
`
	bin := filepath.Join(dir, "daisugi")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_DAISUGI_BIN", bin)
	t.Setenv("COPPICE_DAISUGI_DATA_DIR", main)
	return log, main, work
}

// A steward branch run puts its ask in the steward's work dir. The floor
// shows it and carries the answer to the dir that holds it.
func TestTreeShowsAndAnswersTheStewardsAsk(t *testing.T) {
	log, main, work := twoTreeDirs(t)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, ExtraDataDirs: []string{work, main, filepath.Join(work, "missing")}})
	resp, body := getWith(t, ts.URL+"/api/tree", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"ask_id":"a1"`) || !strings.Contains(body, `"ask_id":"a2"`) ||
		strings.Count(body, `"session":"s1"`) != 1 || !strings.Contains(body, `"session":"n-w"`) {
		t.Errorf("an ask or a node is missing, or doubled: %s", body)
	}
	if !strings.Contains(body, `"data_dir":"`+work+`"`) || !strings.Contains(body, `"skipped":3`) {
		t.Errorf("the entries do not name their dir, or skipped is not summed: %s", body)
	}
	resp, body = post(t, ts.URL+"/api/tree/answer", tok, `{"ask_id":"a2","decision":"allow"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "answered") {
		t.Fatalf("the steward's ask was not answered: %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, ts.URL+"/api/tree/answer", tok, `{"ask_id":"zz","decision":"allow"}`)
	if resp.StatusCode != 404 {
		t.Errorf("an unknown ask answered %d: %s", resp.StatusCode, body)
	}
	_ = log
}
