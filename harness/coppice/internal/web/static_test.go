package web

import (
	"encoding/json"
	"image"
	_ "image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The shell has to load before the operator has pasted anything, so this is
// the one part of the surface with no token on it.
func TestTheAppShellLoadsWithoutAToken(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / returned %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "screen-roster") {
		t.Fatalf("index.html has no roster screen:\n%s", body)
	}
}

func TestTheManifestAndServiceWorkerAreServedCorrectly(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d})
	resp, err := http.Get(ts.URL + "/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/manifest+json") {
		t.Errorf("manifest content type is %q", ct)
	}
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	if m["start_url"] != "/" || m["display"] != "standalone" {
		t.Errorf("manifest is %v", m)
	}

	sw, err := http.Get(ts.URL + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Body.Close()
	if sw.StatusCode != 200 {
		t.Fatalf("GET /sw.js returned %d, so the worker cannot take the root scope", sw.StatusCode)
	}
}

func TestStaticSetsNoCacheNoSniffAndAContentSecurityPolicy(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, _ := newTestServer(t, Options{Dial: d})
	resp, err := http.Get(ts.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control is %q", resp.Header.Get("Cache-Control"))
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options is %q", resp.Header.Get("X-Content-Type-Options"))
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "base-uri 'none'", "img-src 'self';"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP allows inline script or style: %q", csp)
	}
}

// The failure this names: an inline handler or style would force
// unsafe-inline into the policy, and the policy is the reason a stray script
// cannot read the operator's token. This walks every .html file the embed
// carries, not just index.html, so it stays true as more screens are added.
func TestNoStaticFileUsesInlineScriptOrStyle(t *testing.T) {
	onAttr := regexp.MustCompile(`\bon[a-z]+\s*=`)
	scriptTag := regexp.MustCompile(`<script\b[^>]*>`)
	err := fs.WalkDir(StaticFiles(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		raw, err := fs.ReadFile(StaticFiles(), path)
		if err != nil {
			return err
		}
		html := string(raw)
		if m := onAttr.FindString(html); m != "" {
			t.Errorf("%s contains %q, which the CSP forbids", path, m)
		}
		for _, bad := range []string{"<style", "srcdoc=", "javascript:"} {
			if strings.Contains(html, bad) {
				t.Errorf("%s contains %q, which the CSP forbids", path, bad)
			}
		}
		for _, tag := range scriptTag.FindAllString(html, -1) {
			if !strings.Contains(tag, "src=") {
				t.Errorf("%s has a <script> tag with no src, which the CSP forbids: %q", path, tag)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEveryFileTheShellNeedsIsEmbedded(t *testing.T) {
	for _, name := range []string{
		"index.html", "app.css", "app.js", "grid.js", "chips.js", "roster.js", "pane.js",
		"tiles.js", "floor.js", "windows.js", "keys.js", "rail.js", "dock.js", "newpane.js", "settings.js", "record.js", "messages.js", "stackbar.js", "overlay.js", "sw.js", "sw-policy.js",
		"manifest.webmanifest", "icons/icon-192.png", "icons/icon-512.png",
	} {
		if _, err := fs.Stat(StaticFiles(), name); err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		}
	}
}

// The failure this names: mountPane's own wiring throws when one of its
// other buttons is missing from the page, but record.js's mountRecord
// carries no such guard, so deleting <button id="record"> from index.html
// would silently drop the feature from the shipped page with every Go and
// Node test in this suite still green.
func TestIndexHTMLCarriesTheComposeRowButtons(t *testing.T) {
	raw, err := fs.ReadFile(StaticFiles(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, id := range []string{`id="compose"`, `id="text"`, `id="send-enter"`, `id="record"`, `id="keys-toggle"`, `id="sheet-edge"`, `id="sheet-back"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html is missing %s", id)
		}
	}
}

func TestTheTestsDirectoryIsNotShipped(t *testing.T) {
	if _, err := fs.Stat(StaticFiles(), "_tests"); err == nil {
		t.Fatal("the JS unit tests were embedded into the binary")
	}
	if _, err := fs.Stat(StaticFiles(), "_icongen"); err == nil {
		t.Fatal("the icon generator was embedded into the binary")
	}
}

// parsedShell reads the SHELL array out of sw-policy.js as text, since Go
// cannot run the module itself, and returns it as a set.
func parsedShell(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := fs.ReadFile(StaticFiles(), "sw-policy.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`SHELL\s*=\s*\[([^\]]*)\]`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("sw-policy.js has no SHELL array")
	}
	got := map[string]bool{}
	for _, tok := range strings.Split(m[1], ",") {
		path := strings.Trim(strings.TrimSpace(tok), "'\"")
		if path != "" {
			got[path] = true
		}
	}
	return got
}

// embeddedShellPaths is what SHELL should hold: every embedded file, each
// as its own served path, except sw.js, which the browser fetches on its
// own to register the worker and never needs from the cache, and
// index.html, which is served at / instead.
func embeddedShellPaths(t *testing.T) map[string]bool {
	t.Helper()
	want := map[string]bool{"/": true}
	err := fs.WalkDir(StaticFiles(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == "sw.js" || path == "index.html" {
			return nil
		}
		want["/"+path] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return want
}

// The failure this names: caches.addAll rejects the whole install if one
// entry in SHELL cannot be fetched, so a wrong or stray name in
// sw-policy.js would leave the phone with no offline shell at all. The
// other direction matters just as much: a file the shell needs, dropped
// from SHELL by an edit that never touched this file, breaks offline
// support just as silently, and an empty SHELL passes a one directional
// check with nothing left to walk. Comparing both sets, in both
// directions, catches an emptied list, a dropped entry, and an added
// entry all at once. Go cannot run sw-policy.js, so this reads SHELL as
// text and checks it against the same embed the server answers from.
func TestServiceWorkerShellListMatchesTheEmbedExactly(t *testing.T) {
	got := parsedShell(t)
	want := embeddedShellPaths(t)
	for path := range want {
		if !got[path] {
			t.Errorf("SHELL is missing %q, which the embed carries and the shell needs", path)
		}
	}
	for path := range got {
		if !want[path] {
			t.Errorf("SHELL names %q, which the embed does not carry or the shell should not cache", path)
		}
	}
}

// The failure this names: grid.js's ATTR is copied from PINS.md by hand,
// so a later bump to the frame cell attrs row, or a typo in grid.js, would
// have the phone paint the wrong style for a bold, faint, or inverse cell
// and nothing here would say so. This checks grid.js against the same
// PINS.md row pins_test.go checks against vt.go, in the same shape as the
// SHELL test above.
func TestGridAttrMatchesThePinnedBits(t *testing.T) {
	pins, err := os.ReadFile("../../PINS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pins), "bold 1, faint 2, italic 4, underline 8, blink 16, inverse 32, strike 64") {
		t.Fatal("PINS.md no longer records the seven attrs bits this test checks grid.js against")
	}

	raw, err := fs.ReadFile(StaticFiles(), "grid.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`ATTR\s*=\s*\{([^}]*)\}`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("grid.js has no ATTR object")
	}
	for name, bit := range map[string]string{
		"BOLD": "1", "FAINT": "2", "ITALIC": "4", "UNDERLINE": "8",
		"BLINK": "16", "INVERSE": "32", "STRIKE": "64",
	} {
		re := regexp.MustCompile(name + `\s*:\s*` + bit + `\b`)
		if !re.MatchString(m[1]) {
			t.Errorf("grid.js ATTR does not set %s to %s", name, bit)
		}
	}
}

// The failure this names: serving the shell on every path, including the
// API's own, turns a wrong verb on a real route into a 404 that says
// "no such route" when the route is there and the verb is wrong. It also
// turns an unmapped directory into a generated listing. Both are checked
// against the real mux RegisterStatic builds, not a handler in isolation.
func TestStaticRoutesLeaveAPIMethodMismatchesToTheAPI(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})

	req, _ := http.NewRequest("POST", ts.URL+"/api/panes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/panes returned %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow header is %q, want %q", allow, "GET, HEAD")
	}

	dir, err := http.Get(ts.URL + "/icons/")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Body.Close()
	if dir.StatusCode != http.StatusNotFound {
		t.Errorf("GET /icons/ returned %d, want %d, not a generated listing", dir.StatusCode, http.StatusNotFound)
	}

	for path := range embeddedShellPaths(t) {
		r, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d, want 200", path, r.StatusCode)
		}
	}
}

// assertSecurityHeaders fails the test if any of the four headers every
// response carries is missing, whatever status code the response answered
// with.
func assertSecurityHeaders(t *testing.T, resp *http.Response, where string) {
	t.Helper()
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("%s: Cache-Control is %q", where, resp.Header.Get("Cache-Control"))
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options is %q", where, resp.Header.Get("X-Content-Type-Options"))
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("%s: Content-Security-Policy is empty", where)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("%s: Referrer-Policy is %q", where, resp.Header.Get("Referrer-Policy"))
	}
}

// The failure this names: once RegisterStatic answers only for the paths
// it owns, a request the mux refuses on its own, a 404 for an unmatched
// path or a 405 for the wrong method, carried none of the headers every
// other response carries. The mux answers those before any handler in
// this package runs, so only a wrapper around the whole mux reaches them.
func TestEveryResponseCarriesTheSecurityHeadersEvenAMuxRefusal(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	ts, _, tok := newTestServer(t, Options{Dial: d})

	notFound, err := http.Get(ts.URL + "/_tests/x.mjs")
	if err != nil {
		t.Fatal(err)
	}
	defer notFound.Body.Close()
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /_tests/x.mjs returned %d, want 404", notFound.StatusCode)
	}
	assertSecurityHeaders(t, notFound, "GET /_tests/x.mjs, 404")

	req, _ := http.NewRequest("POST", ts.URL+"/api/panes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	mismatch, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer mismatch.Body.Close()
	if mismatch.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/panes returned %d, want 405", mismatch.StatusCode)
	}
	if allow := mismatch.Header.Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow header is %q, want %q", allow, "GET, HEAD")
	}
	assertSecurityHeaders(t, mismatch, "POST /api/panes, 405")
}

func TestTheIconsAreRealPNGsAtTheDeclaredSizes(t *testing.T) {
	for name, want := range map[string]int{"icons/icon-192.png": 192, "icons/icon-512.png": 512} {
		f, err := StaticFiles().Open(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cfg, format, err := image.DecodeConfig(f)
		f.Close()
		if err != nil || format != "png" {
			t.Fatalf("%s is not a PNG: %v", name, err)
		}
		if cfg.Width != want || cfg.Height != want {
			t.Errorf("%s is %dx%d, want %dx%d", name, cfg.Width, cfg.Height, want, want)
		}
	}
}
