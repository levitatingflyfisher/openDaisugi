package web

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	shipped "github.com/opendaisugi/coppice/plugins"
)

// treeView is the shipped tree view, served from the files the binary
// carries.
func treeView(t *testing.T) View {
	t.Helper()
	sub, err := fs.Sub(shipped.Files(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	return View{ID: "tree", Title: "Tree", Page: "index.html", FS: sub}
}

func getWith(t *testing.T, url, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// A view page loads the way the shell does. An iframe cannot carry a
// bearer header, so the page itself holds no data. The data it reads
// comes through the API, behind the same token as every other route.
func TestAViewPageIsServedUnderPlugins(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d, Views: []View{treeView(t)}})
	resp, body := getWith(t, ts.URL+"/plugins/tree/", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "tree.js") {
		t.Fatalf("GET /plugins/tree/ returned %d:\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type %q", ct)
	}
	assertSecurityHeaders(t, resp, "/plugins/tree/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("a view must be framable by the floor page, CSP is %q", csp)
	}
	js, body := getWith(t, ts.URL+"/plugins/tree/tree.js", "")
	if js.StatusCode != 200 || !strings.Contains(js.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("GET tree.js returned %d %q:\n%s", js.StatusCode, js.Header.Get("Content-Type"), body)
	}
}

func TestTheShellStillRefusesToBeFramed(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d, Views: []View{treeView(t)}})
	resp, _ := getWith(t, ts.URL+"/", "")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP on the shell is %q", csp)
	}
}

func TestOnlyAViewsOwnFilesAreServed(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	view := View{ID: "v", Title: "V", Page: "index.html", FS: fstest.MapFS{
		"index.html":  {Data: []byte("<p>v</p>")},
		"_hidden.txt": {Data: []byte("no")},
		"sub/a.js":    {Data: []byte("a")},
	}}
	ts, _, _ := newTestServer(t, Options{Dial: d, Views: []View{view}})
	for _, p := range []string{
		"/plugins/nope/", "/plugins/v/missing.js", "/plugins/v/_hidden.txt",
		"/plugins/v/..%2F..%2Findex.html", "/plugins/v/sub/",
	} {
		resp, body := getWith(t, ts.URL+p, "")
		if resp.StatusCode == 200 {
			t.Errorf("GET %s returned 200:\n%s", p, body)
		}
	}
	resp, body := getWith(t, ts.URL+"/plugins/v/sub/a.js", "")
	if resp.StatusCode != 200 || body != "a" {
		t.Errorf("GET sub/a.js returned %d %q", resp.StatusCode, body)
	}
}

func TestAPIViewsListsEachViewWithItsTitle(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, Views: []View{treeView(t)}})
	resp, body := getWith(t, ts.URL+"/api/views", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got struct {
		Views []map[string]any `json:"views"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Views) != 1 || got.Views[0]["id"] != "tree" || got.Views[0]["title"] != "Tree" || got.Views[0]["ring"] != false {
		t.Fatalf("%s", body)
	}
}

func TestAPIViewsIsAnEmptyListWithNoViews(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	_, body := getWith(t, ts.URL+"/api/views", tok)
	if strings.TrimSpace(body) != `{"views":[]}` {
		t.Fatalf("%s", body)
	}
}

func TestAPITasksProxiesTaskList(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		if req["cmd"] != "task.list" {
			return []map[string]any{{"id": req["id"], "ok": false}}
		}
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
			"tasks": []any{map[string]any{"id": "t1", "label": "auth"}},
		}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d})
	resp, body := getWith(t, ts.URL+"/api/tasks", tok)
	if resp.StatusCode != 200 || !strings.Contains(body, `"t1"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// A view loaded with no token gets the refusal every other API route
// gives, word for word.
func TestTheViewAPIRefusesAMissingTokenLikeEveryRoute(t *testing.T) {
	// Each request gets its own server, so no address meets the ban.
	serve := func() string {
		_, d := newFakeServer(t, echoOK)
		ts, _, _ := newTestServer(t, Options{Dial: d, Views: []View{treeView(t)}})
		return ts.URL
	}
	_, want := getWith(t, serve()+"/api/panes", "")
	for _, p := range []string{"/api/views", "/api/tasks"} {
		resp, body := getWith(t, serve()+p, "")
		if resp.StatusCode != http.StatusUnauthorized || body != want {
			t.Errorf("GET %s returned %d %q, want 401 %q", p, resp.StatusCode, body, want)
		}
	}
}

func TestLocalURLNamesTheFloorPageOnThisBox(t *testing.T) {
	cases := []struct {
		c    Config
		want string
	}{
		{Config{}, ""},
		{Config{Enabled: true}, "https://127.0.0.1:8443"},
		{Config{Enabled: true, Listen: "0.0.0.0:9000"}, "https://127.0.0.1:9000"},
		{Config{Enabled: true, Listen: ":8080", TLS: "off"}, "http://127.0.0.1:8080"},
		{Config{Enabled: true, Listen: "[::1]:7000"}, "https://[::1]:7000"},
		{Config{Enabled: true, Listen: "[::]:7000"}, "https://127.0.0.1:7000"},
		{Config{Enabled: true, Listen: "100.64.1.2:8443"}, "https://100.64.1.2:8443"},
		{Config{Enabled: true, Listen: "nonsense"}, ""},
	}
	for _, c := range cases {
		if got := LocalURL(c.c); got != c.want {
			t.Errorf("LocalURL(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

// A view runs in an opaque origin. The page gets a sandbox policy, and its
// scripts carry an open CORS header, since the frame asks for them with
// Origin null. They are static and hold no data.
func TestAViewRunsSandboxedWithScriptsOnly(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d, Views: []View{treeView(t)}})
	for _, p := range []string{"/plugins/tree/", "/plugins/tree/tree.js"} {
		resp, _ := getWith(t, ts.URL+p, "")
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox allow-scripts") || strings.Contains(csp, "allow-same-origin") {
			t.Errorf("%s: CSP %q", p, csp)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: no open CORS header", p)
		}
	}
	resp, _ := getWith(t, ts.URL+"/", "")
	if strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("the shell is sandboxed")
	}
}

// What a sandboxed view page can send: Origin null. Its fetch to the API
// and its websocket open are refused, even with a token it should not
// have, and a refusal of that kind is no ban strike on the floor.
func TestAViewPageCannotReachTheAPIOrTheSocket(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, Views: []View{treeView(t)}})
	send := func(method, path, token string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Origin", "null")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/tasks"}, {"GET", "/api/panes"}, {"GET", "/api/views"}, {"GET", "/api/events"},
		{"POST", "/api/ask/answer"}, {"GET", "/ws"},
	} {
		for _, token := range []string{"", tok} {
			if resp := send(c.method, c.path, token); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s with Origin null and token %v: %d, want 403", c.method, c.path, token != "", resp.StatusCode)
			}
		}
	}
	if st := getStatus(t, ts.URL+"/api/panes", tok); st != 200 {
		t.Fatalf("the floor's own token was banned after refused view requests: %d", st)
	}
}

// A sandboxed view can load /api/panes as an image or as a no-cors fetch.
// Those send no Origin and no token. A missing token guesses nothing, so it
// earns no strike, and the floor page on the same address keeps working.
func TestAMissingTokenEarnsNoStrike(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}})
	for i := 0; i < BanFailures+2; i++ {
		for _, p := range []string{"/api/panes", "/api/tasks", "/ws"} {
			if st := getStatus(t, ts.URL+p, ""); st != http.StatusUnauthorized {
				t.Fatalf("GET %s with no token: %d, want 401", p, st)
			}
		}
		req, _ := http.NewRequest("POST", ts.URL+"/api/ask/answer", strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("POST answer with no token: %d", resp.StatusCode)
		}
	}
	if st := getStatus(t, ts.URL+"/api/panes", tok); st != 200 {
		t.Fatalf("the floor's token after tokenless requests: %d, want 200", st)
	}
}

// A wrong token is still a guess, and three of them ban the address.
func TestAWrongTokenStillEarnsAStrike(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	for i := 0; i < BanFailures; i++ {
		getStatus(t, ts.URL+"/api/panes", "wrong")
	}
	if st := getStatus(t, ts.URL+"/api/panes", tok); st != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong tokens: %d, want 429", BanFailures, st)
	}
}

// sharedLib is the shipped library the views import.
func sharedLib(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(shipped.Files(), "_lib")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// Every view imports ../_lib/view.js. The server serves the library's
// JavaScript under the view policy and nothing else from that directory.
func TestTheViewLibraryIsServedAndNothingElseBesideIt(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	lib := fstest.MapFS{
		"view.js":         {Data: []byte("export const x = 1;")},
		"floor_client.py": {Data: []byte("secret = 1")},
		"_tests/a.js":     {Data: []byte("no")},
		"sub/b.js":        {Data: []byte("no")},
		".hidden.js":      {Data: []byte("no")},
	}
	ts, _, _ := newTestServer(t, Options{Dial: d, Views: []View{treeView(t)}, ViewLib: lib})
	resp, body := getWith(t, ts.URL+"/plugins/_lib/view.js", "")
	if resp.StatusCode != 200 || body != "export const x = 1;" {
		t.Fatalf("GET view.js returned %d %q", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Errorf("content type %q", resp.Header.Get("Content-Type"))
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts") || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("view.js is not served under the view policy: CSP %q", csp)
	}
	for _, p := range []string{
		"/plugins/_lib/floor_client.py", "/plugins/_lib/_tests/a.js", "/plugins/_lib/sub/b.js",
		"/plugins/_lib/.hidden.js", "/plugins/_lib/", "/plugins/_lib/missing.js",
		"/plugins/_lib/..%2Ftree%2Findex.html",
	} {
		if resp, body := getWith(t, ts.URL+p, ""); resp.StatusCode == 200 {
			t.Errorf("GET %s returned 200:\n%s", p, body)
		}
	}
}

func TestNoViewLibraryMeansNotFound(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d})
	if resp, _ := getWith(t, ts.URL+"/plugins/_lib/view.js", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET view.js with no library: %d", resp.StatusCode)
	}
}

// The shipped library carries view.js, so a shipped view can load it.
func TestTheShippedLibraryCarriesViewJS(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d, ViewLib: sharedLib(t)})
	if resp, _ := getWith(t, ts.URL+"/plugins/_lib/view.js", ""); resp.StatusCode != 200 {
		t.Fatalf("GET the shipped view.js: %d", resp.StatusCode)
	}
}
