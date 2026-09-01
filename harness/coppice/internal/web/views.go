package web

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
)

// View is one view plugin the web server serves at /plugins/<id>/. FS
// holds the view's own files and nothing else. Page is its html file.
type View struct {
	ID    string
	Title string
	Page  string
	FS    fs.FS
	// Ring is true for a view that asks for the ring of recent state
	// events.
	Ring bool
}

// viewCSP is csp with two changes: the floor page may frame a view, and a
// view runs in a sandbox with scripts only. It gets an opaque origin, so it
// cannot read the floor page's storage, and every request it sends carries
// Origin null, which the guarded routes refuse. The shell keeps
// frame-ancestors 'none' and no sandbox.
var viewCSP = strings.Replace(csp, "frame-ancestors 'none'", "frame-ancestors 'self'", 1) +
	"; sandbox allow-scripts"

// viewRoutes mounts every view, the library they share, and the view API
// routes.
func (s *Server) viewRoutes() {
	s.mux.HandleFunc("GET /plugins/{id}/{file...}", s.handleViewFile)
	s.mux.HandleFunc("GET /plugins/_lib/{file...}", s.handleViewLib)
	s.mux.HandleFunc("GET /api/views", s.guard(s.handleViews))
	s.mux.HandleFunc("GET /api/tasks", s.guard(s.handleTasks))
	s.mux.HandleFunc("GET /api/events", s.guard(s.handleEvents))
}

func (s *Server) viewByID(id string) (View, bool) {
	for _, v := range s.opts.Views {
		if v.ID == id && v.FS != nil {
			return v, true
		}
	}
	return View{}, false
}

// handleViewFile serves one file of a view. The path must name a regular
// file inside the view. A name that starts with _ or . is never served.
// There is no token on this path, the same as the shell. A view holds no
// token and asks the server for nothing: the floor page posts it the data
// it shows.
func (s *Server) handleViewFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", viewCSP)
	// A sandboxed frame loads its module scripts with Origin null. The
	// files are static and hold no data, so any origin may read them.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	v, ok := s.viewByID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("file")
	if name == "" {
		name = v.Page
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, "_") || strings.HasPrefix(part, ".") {
			http.NotFound(w, r)
			return
		}
	}
	serveFile(w, r, v.FS, name)
}

// serveFile serves the regular file name from files, or answers not
// found.
func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name string) {
	f, err := files.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		http.Error(w, "cannot read the file", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, path.Base(name), st.ModTime(), bytes.NewReader(body))
}

// handleViewLib serves one JavaScript file of the library every view
// imports. It serves a file at the top of the library whose name ends in
// .js and does not start with _ or ., and nothing else. The library also
// holds the policies' Python, which no page needs.
func (s *Server) handleViewLib(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", viewCSP)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	name := r.PathValue("file")
	if s.opts.ViewLib == nil || !fs.ValidPath(name) || strings.Contains(name, "/") ||
		!strings.HasSuffix(name, ".js") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	serveFile(w, r, s.opts.ViewLib, name)
}

// handleViews lists the views the rail can switch to.
func (s *Server) handleViews(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, v := range s.opts.Views {
		out = append(out, map[string]any{"id": v.ID, "title": v.Title, "ring": v.Ring})
	}
	writeJSON(w, http.StatusOK, map[string]any{"views": out})
}

// handleTasks is task.list for a view.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	s.proxyList(w, r, "task.list")
}

// proxyList answers with the result of one list verb. A refusal is never
// flattened into an empty list.
func (s *Server) proxyList(w http.ResponseWriter, r *http.Request, cmd string) {
	msg, err := Call(r.Context(), s.opts.Dial, map[string]any{"cmd": cmd})
	var refused *RefusedError
	switch {
	case errors.As(err, &refused):
		s.opts.Log.Warn("web: "+cmd+" refused", "code", refused.Code, "message", refused.Message)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "server refused: " + refused.Message,
			"code":  refused.Code,
		})
		return
	case err != nil:
		s.opts.Log.Warn("web: "+cmd+" failed", "err", err)
		writeErr(w, http.StatusBadGateway, "coppice-server did not answer. Run coppice server status.")
		return
	}
	result, isObject := msg["result"].(map[string]any)
	if !isObject {
		s.opts.Log.Warn("web: " + cmd + " answered ok with a result the phone cannot read")
		writeErr(w, http.StatusBadGateway, "coppice-server sent a reply the phone could not read. Run coppice server status.")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// LocalURL is the floor page's address on this box for c, or "" when the
// phone server is off or its listen address does not parse. It names the
// listen host, or loopback for a listener on every address.
func LocalURL(c Config) string {
	if !c.Enabled {
		return ""
	}
	listen := c.Listen
	if listen == "" {
		listen = ":8443"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return ""
	}
	scheme := "https"
	if TLSSource(c.TLS) == TLSOff {
		scheme = "http"
	}
	// A listener on every address answers on loopback. A listener on one
	// address answers only there.
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}
