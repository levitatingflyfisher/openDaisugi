package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"strings"
)

// The directive has no all: prefix on purpose. Go leaves out entries whose
// name starts with _ or ., which is what keeps static/_tests and
// static/_icongen out of the binary.
//
//go:embed static
var staticFS embed.FS

// StaticFiles is the shipped PWA, rooted at static/.
func StaticFiles() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// csp keeps every script and style in a file. Nothing inline means a stray
// injected script has nowhere to run, which matters on a page that holds the
// operator's token. Nothing the shell ships uses a data: image, so img-src
// names only the origin itself.
const csp = "default-src 'self'; connect-src 'self'; img-src 'self'; " +
	"script-src 'self'; style-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// StaticHandler serves the shell. There is no token on this path: the shell
// has to load before the operator has pasted one. The security headers
// come from Server.Handler's wrapper now, not from here, so this stays
// correct for a caller who never wraps it: it just serves files, and
// refuses a path under /_ in case a future caller ever points a broader
// pattern at it than RegisterStatic does today.
func StaticHandler() (http.Handler, error) {
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		return nil, err
	}
	files := http.FileServerFS(StaticFiles())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	}), nil
}

// RegisterStatic serves the shell on the exact paths it owns: the root path
// and one path per embedded file, each walked from the same embed the
// handler serves from. A request for any other path, or the right path
// with the wrong method, never reaches this handler. That is what lets a
// wrong verb on an API path get the mux's own refusal instead of a file
// server's 404: registering the shell on every path, including the API's
// own, would answer for paths this handler does not own and hide the
// refusal the guarded routes are supposed to give.
func RegisterStatic(mux *http.ServeMux) error {
	handler, err := StaticHandler()
	if err != nil {
		return err
	}
	mux.Handle("GET /{$}", handler)
	return fs.WalkDir(StaticFiles(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		mux.Handle("GET /"+path, handler)
		return nil
	})
}
