package web

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDaisugi writes a script that records its argv, one call per line, and
// prints a canned reply chosen by the first two words. It points the web
// package at the script and at a data dir, and returns the argv log path.
func fakeDaisugi(t *testing.T, replies map[string]string, exit map[string]int) (log, data string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "argv.log")
	data = filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\necho \"$*\" >> " + log + "\nkey=\"$1 $2\"\ncase \"$key\" in\n")
	for k, v := range replies {
		code := exit[k]
		sb.WriteString("\"" + k + "\") cat <<'EOT'\n" + v + "\nEOT\n")
		if code != 0 {
			sb.WriteString("echo 'There is no choice here.' >&2\nexit " + string(rune('0'+code)) + " ;;\n")
		} else {
			sb.WriteString(";;\n")
		}
	}
	sb.WriteString("*) echo '{}' ;;\nesac\n")
	bin := filepath.Join(dir, "daisugi")
	if err := os.WriteFile(bin, []byte(sb.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_DAISUGI_BIN", bin)
	t.Setenv("COPPICE_DAISUGI_DATA_DIR", data)
	return log, data
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func post(t *testing.T, url, tok, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.String()
}

const queueJSON = `{"cards":[{"choice_id":"ch_1","headline":"Kept a over b."}],"decayed":0}`

func TestCardsRouteRunsRankQueueAndNeverWrites(t *testing.T) {
	log, data := fakeDaisugi(t, map[string]string{"rank queue": queueJSON}, nil)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := getWith(t, ts.URL+"/api/cards?sort=impact", tok)
	if resp.StatusCode != 200 || !strings.Contains(body, `"ch_1"`) || !strings.Contains(body, `"source":"daisugi rank queue"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"data_dir":"`+data+`"`) {
		t.Errorf("the reply does not name the data dir: %s", body)
	}
	got := calls(t, log)
	want := "rank queue --json --sort impact --data-dir " + data
	if len(got) != 1 || got[0] != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if ents, _ := os.ReadDir(data); len(ents) != 0 {
		t.Errorf("the route wrote into the data dir: %v", ents)
	}
	if resp, _ := getWith(t, ts.URL+"/api/cards?sort=bogus", tok); resp.StatusCode != 400 {
		t.Errorf("a bad sort answered %d, want 400", resp.StatusCode)
	}
}

func TestCardsRouteSaysWhyTheChildFailed(t *testing.T) {
	t.Setenv("COPPICE_DAISUGI_BIN", "/nonexistent/daisugi")
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := getWith(t, ts.URL+"/api/cards", tok)
	if resp.StatusCode != 502 || !strings.Contains(body, "daisugi rank queue failed:") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestCardsAnswerRefusesAPane(t *testing.T) {
	log, _ := fakeDaisugi(t, map[string]string{"rank record": `{"recorded":true}`}, nil)
	old := peerOf
	peerOf = func(*http.Request) map[string]any { return map[string]any{"peer_pids": []int{4242}} }
	t.Cleanup(func() { peerOf = old })
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"allow": false}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := post(t, ts.URL+"/api/cards/answer", tok, `{"choice_id":"ch_1","outcome":"drop"}`)
	if resp.StatusCode != 403 || !strings.Contains(body, PaneRefusal) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(calls(t, log)) != 0 {
		t.Errorf("daisugi ran for a pane: %v", calls(t, log))
	}
	resp, _ = post(t, ts.URL+"/api/tree/answer", tok, `{"ask_id":"a1","decision":"allow"}`)
	if resp.StatusCode != 403 || len(calls(t, log)) != 0 {
		t.Errorf("the tree answer let a pane through: %d %v", resp.StatusCode, calls(t, log))
	}
}

func TestCardsAnswerRecordsADropAndASwitchThenPrunes(t *testing.T) {
	log, data := fakeDaisugi(t, map[string]string{
		"rank record":         `{"recorded":true}`,
		"tree prune-branches": `{"removed":["s1"]}`,
	}, nil)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := post(t, ts.URL+"/api/cards/answer", tok, `{"choice_id":"ch_1","outcome":"drop"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"pruned"`) || !strings.Contains(body, "s1") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	resp, _ = post(t, ts.URL+"/api/cards/answer", tok, `{"choice_id":"ch_1","outcome":"switch","pick":"b"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("switch status %d", resp.StatusCode)
	}
	got := calls(t, log)
	want := []string{
		"rank record drop ch_1 --json --data-dir " + data,
		"tree prune-branches --json --data-dir " + data,
		"rank record pick ch_1 b --json --data-dir " + data,
		"tree prune-branches --json --data-dir " + data,
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("argv = %q, want %q", got, want)
	}
	// A bad answer never reaches daisugi.
	for _, b := range []string{
		`{"choice_id":"ch_1","outcome":"burn"}`,
		`{"choice_id":"ch_1","outcome":"switch"}`,
		`{"choice_id":"--apply","outcome":"drop"}`,
		`{"choice_id":"ch_1","outcome":"switch","pick":"-x"}`,
	} {
		if resp, _ := post(t, ts.URL+"/api/cards/answer", tok, b); resp.StatusCode != 400 {
			t.Errorf("%s answered %d, want 400", b, resp.StatusCode)
		}
	}
	if len(calls(t, log)) != len(want) {
		t.Errorf("a bad answer reached daisugi: %v", calls(t, log))
	}
}

func TestCardsAnswerOfAnUnknownCardIs404(t *testing.T) {
	fakeDaisugi(t, map[string]string{"rank record": ``}, map[string]int{"rank record": 1})
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := post(t, ts.URL+"/api/cards/answer", tok, `{"choice_id":"nope","outcome":"drop"}`)
	if resp.StatusCode != 404 || !strings.Contains(body, "There is no choice") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestCardsAnswerRefusesALockScreenDenyToken(t *testing.T) {
	log, _ := fakeDaisugi(t, nil, nil)
	url, _, deny, _, _ := denyFixture(t)
	resp, _ := post(t, url+"/api/cards/answer", deny, `{"choice_id":"ch_1","outcome":"drop"}`)
	if resp.StatusCode != 403 || len(calls(t, log)) != 0 {
		t.Fatalf("a deny token answered a card: %d %v", resp.StatusCode, calls(t, log))
	}
}

func TestCardsDiffReadsOnlyUnderTheFloorsWorktrees(t *testing.T) {
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=a@b", "-c", "user.name=t"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	run(repo, "add", "-A")
	run(repo, "commit", "-q", "-m", "x")
	wt := filepath.Join(t.TempDir(), "wt")
	run(repo, "worktree", "add", "-q", "--detach", wt)
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("two\n"), 0o644)
	outside := t.TempDir()
	head, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	base := strings.TrimSpace(string(head))
	show := `{"choices":[{"event":"opened","facts":{"order":["a","b","c"],"where":{` +
		`"a":{"kind":"worktree","path":"` + wt + `","base":"` + base + `","repo":"` + repo + `/.git"},` +
		`"b":{"kind":"worktree","path":"` + outside + `"},` +
		`"c":{"kind":"output","text":"SECRETTEXT"}}}}]}`
	log, data := fakeDaisugi(t, map[string]string{"rank show": show}, nil)
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
			"tasks": []any{map[string]any{"id": "t1", "worktree": wt + "/.."}}}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := getWith(t, ts.URL+"/api/cards/ch_1/diff", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "-one") || !strings.Contains(body, "+two") {
		t.Errorf("the worktree diff is missing: %s", body)
	}
	if !strings.Contains(body, "outside the floor's worktrees") {
		t.Errorf("a path outside the roots was not refused: %s", body)
	}
	if strings.Contains(body, "SECRETTEXT") {
		t.Errorf("an output attempt leaked into the diff: %s", body)
	}
	got := calls(t, log)
	if len(got) != 1 || got[0] != "rank show --choice ch_1 --json --data-dir "+data {
		t.Errorf("argv = %q", got)
	}
}

// branchFixture is a repository with one worktree that holds an edit.
func branchFixture(t *testing.T) (repo, wt, base string) {
	t.Helper()
	repo = t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=a@b", "-c", "user.name=t"}, args...)...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	run(repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644)
	run(repo, "add", "-A")
	run(repo, "commit", "-q", "-m", "x")
	wt = filepath.Join(t.TempDir(), "wt")
	run(repo, "worktree", "add", "-q", "--detach", wt)
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("two\n"), 0o644)
	return repo, wt, strings.TrimSpace(run(repo, "rev-parse", "HEAD"))
}

func diffBody(t *testing.T, wt, place string) string {
	t.Helper()
	show := `{"choices":[{"event":"opened","facts":{"order":["a"],"where":{"a":` + place + `}}}]}`
	fakeDaisugi(t, map[string]string{"rank show": show}, nil)
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
			"tasks": []any{map[string]any{"id": "t1", "worktree": wt + "/.."}}}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := getWith(t, ts.URL+"/api/cards/ch_1/diff", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	return body
}

func TestCardsDiffNeedsTheBaseDaisugiRecordedAndNeverReadsAnote(t *testing.T) {
	repo, wt, base := branchFixture(t)
	gd := strings.TrimSpace(func() string {
		o, _ := exec.Command("git", "-C", wt, "rev-parse", "--path-format=absolute", "--git-dir").Output()
		return string(o)
	}())
	target := filepath.Join(t.TempDir(), "pwned")
	os.WriteFile(filepath.Join(gd, "daisugi-base"), []byte("--output="+target+"\n"), 0o644)
	place := func(b string) string {
		return `{"kind":"worktree","path":"` + wt + `","base":"` + b + `","repo":"` + repo + `/.git"}`
	}
	if body := diffBody(t, wt, place(base)); !strings.Contains(body, "+two") {
		t.Errorf("the recorded base gave no diff: %s", body)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the base note wrote a file")
	}
	// A base that is not an object id is never given to git.
	body := diffBody(t, wt, place("--output="+target))
	if !strings.Contains(body, "recorded no base commit") {
		t.Errorf("a dash base was not refused: %s", body)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a dash base wrote a file")
	}
	// A worktree with no recorded base or repository has no diff.
	if body := diffBody(t, wt, `{"kind":"worktree","path":"`+wt+`"}`); !strings.Contains(body, "recorded no base commit") {
		t.Errorf("a place with no base was read: %s", body)
	}
}

func TestCardsDiffRefusesAWorktreeThatLeftItsRepository(t *testing.T) {
	repo, wt, base := branchFixture(t)
	other, owt, _ := branchFixture(t)
	ogd, _ := exec.Command("git", "-C", owt, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+strings.TrimSpace(string(ogd))+"\n"), 0o644)
	_ = other
	body := diffBody(t, wt, `{"kind":"worktree","path":"`+wt+`","base":"`+base+`","repo":"`+repo+`/.git"}`)
	if !strings.Contains(body, "no longer belongs to its repository") || strings.Contains(body, "+two") {
		t.Errorf("a redirected .git was read: %s", body)
	}
}

// twoDirs is a fake daisugi that holds ch_1 in the main data dir and ch_1
// again and ch_2 in the work dir, and says "no choice" for a card a dir
// does not hold.
func twoDirs(t *testing.T) (log, main, work string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "argv.log")
	main, work = filepath.Join(dir, "main"), filepath.Join(dir, "work")
	os.Mkdir(main, 0o700)
	os.Mkdir(work, 0o700)
	script := "#!/bin/sh\necho \"$*\" >> " + log + `
case "$*" in
"rank queue"*"--data-dir ` + main + `") echo '{"cards":[{"choice_id":"ch_1"}],"decayed":1}' ;;
"rank queue"*"--data-dir ` + work + `") echo '{"cards":[{"choice_id":"ch_1"},{"choice_id":"ch_2"}],"decayed":2}' ;;
"rank record"*ch_2*"--data-dir ` + main + `") echo 'There is no choice ch_2.' >&2; exit 1 ;;
"rank record"*"--data-dir ` + work + `") echo '{"recorded":true}' ;;
"tree prune-branches"*"--data-dir ` + work + `") echo '{"removed":["n-x"]}' ;;
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

func TestCardsShowTheStewardsCardsOnceAndAnswerInTheirOwnDir(t *testing.T) {
	log, main, work := twoDirs(t)
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, ExtraDataDirs: []string{work, main, filepath.Join(work, "missing")}})
	resp, body := getWith(t, ts.URL+"/api/cards", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if strings.Count(body, `"choice_id":"ch_1"`) != 1 || strings.Count(body, `"choice_id":"ch_2"`) != 1 {
		t.Errorf("a card is missing or twice: %s", body)
	}
	if !strings.Contains(body, `"data_dir":"`+work+`"`) || !strings.Contains(body, `"decayed":3`) {
		t.Errorf("the cards do not name their dir, or the decayed count is not summed: %s", body)
	}
	resp, body = post(t, ts.URL+"/api/cards/answer", tok, `{"choice_id":"ch_2","outcome":"drop"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "n-x") {
		t.Fatalf("the steward's card was not answered and pruned in its dir: %d %s", resp.StatusCode, body)
	}
	for _, c := range calls(t, log) {
		if strings.HasPrefix(c, "tree prune-branches") && !strings.HasSuffix(c, work) {
			t.Errorf("pruned the wrong dir: %s", c)
		}
	}
}

func TestCardsDiffRefusesADotGitAtTheMainOrASiblingsGitDir(t *testing.T) {
	repo, wt, base := branchFixture(t)
	sibling := filepath.Join(t.TempDir(), "sib")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "--detach", sibling).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	sgdRaw, _ := exec.Command("git", "-C", sibling, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	sgd := strings.TrimSpace(string(sgdRaw))
	main := filepath.Join(repo, ".git")
	place := `{"kind":"worktree","path":"` + wt + `","base":"` + base + `","repo":"` + main + `"}`
	idx := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	for name, set := range map[string]func(){
		"main file":    func() { os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+main+"\n"), 0o644) },
		"main symlink": func() { os.Remove(filepath.Join(wt, ".git")); os.Symlink(main, filepath.Join(wt, ".git")) },
		"sibling": func() {
			os.Remove(filepath.Join(wt, ".git"))
			os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+sgd+"\n"), 0o644)
		},
	} {
		os.Remove(filepath.Join(wt, ".git"))
		set()
		beforeMain, beforeSib := idx(filepath.Join(main, "index")), idx(filepath.Join(sgd, "index"))
		body := diffBody(t, wt, place)
		if !strings.Contains(body, "no longer belongs to its repository") || strings.Contains(body, "+two") {
			t.Errorf("%s: a redirected .git was read: %s", name, body)
		}
		if idx(filepath.Join(main, "index")) != beforeMain || idx(filepath.Join(sgd, "index")) != beforeSib {
			t.Errorf("%s: an index was written", name)
		}
	}
}

func TestCardsDiffDoesNotRunAConfigTheChildWrote(t *testing.T) {
	repo, wt, base := branchFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(t.TempDir(), "hook.sh")
	os.WriteFile(hook, []byte("#!/bin/sh\necho x >> "+marker+"\n"), 0o755)
	if out, err := exec.Command("git", "-C", repo, "config", "core.fsmonitor", hook).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	exec.Command("git", "-C", wt, "status").Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: an unguarded git did not run the child's fsmonitor")
	}
	os.Remove(marker)
	place := `{"kind":"worktree","path":"` + wt + `","base":"` + base + `","repo":"` + repo + `/.git"}`
	if body := diffBody(t, wt, place); !strings.Contains(body, "+two") {
		t.Errorf("the honest worktree gave no diff: %s", body)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the child's fsmonitor ran under the floor's diff")
	}
}

func TestCardsDiffRunsNoProgramOfANestedRepositoryTheChildMade(t *testing.T) {
	repo, wt, base := branchFixture(t)
	root := t.TempDir()
	marker := filepath.Join(root, "ran-nested")
	script := filepath.Join(root, "clean.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+marker+"\ncat\n"), 0o755)
	g := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=a@b", "-c", "user.name=t"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	sub := filepath.Join(wt, "sub")
	os.MkdirAll(sub, 0o755)
	g(sub, "init", "-q")
	os.WriteFile(filepath.Join(sub, "s.txt"), []byte("aaaa\n"), 0o644)
	os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("* filter=x\n"), 0o644)
	g(sub, "add", "-A")
	g(sub, "commit", "-qm", "n")
	g(sub, "config", "filter.x.clean", script)
	g(wt, "add", "sub")
	touch := func(text string, sec int64) {
		f := filepath.Join(sub, "s.txt")
		os.WriteFile(f, []byte(text), 0o644)
		os.Chtimes(f, time.Unix(sec, 0), time.Unix(sec, 0))
	}
	touch("bbbb\n", 1)
	exec.Command("git", "-C", wt, "diff", "--name-only", base).Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: an unguarded diff did not run the nested filter")
	}
	os.Remove(marker)
	touch("cccc\n", 7)
	place := `{"kind":"worktree","path":"` + wt + `","base":"` + base + `","repo":"` + repo + `/.git"}`
	if body := diffBody(t, wt, place); !strings.Contains(body, "+two") {
		t.Errorf("the honest worktree gave no diff: %s", body)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the nested repository's filter ran under the floor's diff")
	}
}

func TestCardsDiffRefusesAWorktreeWithAConfigWorktree(t *testing.T) {
	repo, wt, base := branchFixture(t)
	exec.Command("git", "-C", repo, "config", "extensions.worktreeConfig", "true").Run()
	root := t.TempDir()
	marker := filepath.Join(root, "ran-wtconfig")
	script := filepath.Join(root, "clean.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+marker+"\ncat\n"), 0o755)
	gd, _ := exec.Command("git", "-C", wt, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	os.WriteFile(filepath.Join(strings.TrimSpace(string(gd)), "config.worktree"), []byte("[filter \"x\"]\n\tclean = "+script+"\n"), 0o644)
	os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("* filter=x\n"), 0o644)
	os.Chtimes(filepath.Join(wt, "a.txt"), time.Unix(3, 0), time.Unix(3, 0))
	exec.Command("git", "-C", wt, "diff", "HEAD").Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the control failed: git did not run the child's config.worktree")
	}
	os.Remove(marker)
	place := `{"kind":"worktree","path":"` + wt + `","base":"` + base + `","repo":"` + repo + `/.git"}`
	body := diffBody(t, wt, place)
	if !strings.Contains(body, "no longer belongs to its repository") || strings.Contains(body, "+two") {
		t.Errorf("a worktree with a config.worktree was read: %s", body)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the child's config.worktree ran under the floor's diff")
	}
}

func TestCardsDiffReadsAChildThatCommittedIntoItsOwnStore(t *testing.T) {
	repo, wt, base := branchFixture(t)
	gd, _ := exec.Command("git", "-C", wt, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	own := filepath.Join(strings.TrimSpace(string(gd)), "objects")
	os.MkdirAll(own, 0o700)
	env := append(os.Environ(), "GIT_OBJECT_DIRECTORY="+own, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(repo, ".git", "objects"))
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.email=a@b", "-c", "user.name=t", "commit", "-qm", "c"}} {
		c := exec.Command("git", append([]string{"-C", wt}, args...)...)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("three\n"), 0o644)
	place := `{"kind":"worktree","path":"` + wt + `","base":"` + base + `","repo":"` + repo + `/.git"}`
	// Until daisugi folds the child's store away, the floor reads nothing.
	if body := diffBody(t, wt, place); !strings.Contains(body, "no longer belongs to its repository") {
		t.Errorf("a worktree with an unfolded store was read: %s", body)
	}
	// The fold daisugi runs once the child is gone (branch_git.fold).
	admin := strings.TrimSpace(string(gd))
	for _, args := range [][]string{
		{"--git-dir=" + repo + "/.git", "-c", "core.logAllRefUpdates=false", "update-ref", "--no-deref", "worktrees/" + filepath.Base(admin) + "/HEAD", base},
		{"--git-dir=" + repo + "/.git", "read-tree", base},
	} {
		c := exec.Command("git", args...)
		c.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(admin, "index"))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.RemoveAll(filepath.Join(admin, "logs"))
	os.RemoveAll(own)
	if body := diffBody(t, wt, place); !strings.Contains(body, "+three") {
		t.Errorf("a child that committed gave no diff after the fold: %s", body)
	}
}
