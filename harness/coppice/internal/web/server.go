package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Options is everything the phone server needs. Dial is the only door to
// coppice-server; Tokens is the only wall in front of it. VoiceURL is a
// daisugi voice serve the owner started, for the record buttons; empty
// means the coppice server says where voice is, in voice.status.
type Options struct {
	Dial     Dialer
	Tokens   TokenStore
	Gate     AskChannel
	Push     *Publisher
	VoiceURL string
	// VoiceTokenFile is the token file VoiceURL's server checks. With none,
	// the web token goes to VoiceURL only when it is on this machine.
	VoiceTokenFile string
	// Views are the view plugins served under /plugins.
	Views []View
	// ViewLib is the library the views share, served at /plugins/_lib/.
	// Only its JavaScript files are served.
	ViewLib fs.FS
	// Events is the ring of recent state events that /api/events reads.
	// Serve makes one and feeds it. With none, the route refuses.
	Events *EventRing
	Log    *slog.Logger
	Now    func() time.Time
}

// Server is the HTTP surface for the websocket that carries coppice's own
// protocol to the phone.
type Server struct {
	opts   Options
	bans   *Banlist
	mux    *http.ServeMux
	wsLive atomic.Int64
}

func New(opts Options) (*Server, error) {
	if opts.Dial == nil {
		return nil, errors.New("web: no dialer. Pass Options.Dial")
	}
	if opts.Tokens.Path == "" {
		return nil, errors.New("web: no token store. Pass Options.Tokens")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, bans: NewBanlist(opts.Now), mux: http.NewServeMux()}
	s.mux.HandleFunc("/ws", s.guard(s.handleWS))
	s.routes()
	s.viewRoutes()
	if err := RegisterStatic(s.mux); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler wraps the mux so every response this server ever sends, the
// mux's own 404 or 405 included, carries the same security headers. A
// refusal is a response too, and the page holding the operator's token
// deserves the same protection whether the answer is yes or no.
func (s *Server) Handler() http.Handler { return securityHeaders(s.mux) }

// securityHeaders sets the four headers every response carries, then
// hands the request to next. Setting them here, once, ahead of routing,
// is what makes them stick even when no registered pattern matches the
// path or the method: the mux answers those itself, before any handler in
// this package would otherwise get the chance to set anything.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// liveWS is how many /ws handlers are running right now. A test drives this
// to zero to prove a closed browser connection actually ends its handler
// goroutine instead of leaving it running.
func (s *Server) liveWS() int64 { return s.wsLive.Load() }

// clientAddr is the host part of RemoteAddr. No supported path puts a proxy
// in front of this server, so a forwarding header is never trusted. Honoring
// one would let a caller pick its own ban bucket.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// guard fails closed. A banned address, a missing token, or a wrong token
// never reaches the handler, and every refusal of a token is one log line.
// Only a wrong token is a strike toward a ban. A missing token guesses
// nothing, and a view on the floor page's address can send one with an
// image load, so it must not be able to ban that address.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refuseView(w, r) {
			return
		}
		addr := clientAddr(r)
		if s.bans.Banned(addr) {
			s.opts.Log.Warn("web: refused a banned address", "addr", addr, "path", r.URL.Path)
			writeErr(w, http.StatusTooManyRequests, "Too many bad tokens. Wait one minute.")
			return
		}
		tok := bearerFrom(r)
		if tok == "" {
			refuseNoToken(w)
			return
		}
		name, ok := s.opts.Tokens.Who(tok)
		if !ok {
			s.bans.Fail(addr)
			s.opts.Log.Warn("web: refused a bad token", "addr", addr, "path", r.URL.Path)
			writeErr(w, http.StatusUnauthorized, "Bad token. Run coppice web token to see the current one.")
			return
		}
		next(w, withName(r, name))
	}
}

// nameKey marks a request with the name its token carries. The value is
// "" for the operator's own token.
type nameKey struct{}

// withName returns r marked with name.
func withName(r *http.Request, name string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), nameKey{}, name))
}

// nameOf is the name the request's token carries, or "" for none.
func nameOf(r *http.Request) string {
	name, _ := r.Context().Value(nameKey{}).(string)
	return name
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// ViewRefusal is what a request from a view frame gets on every guarded
// route.
const ViewRefusal = "A view holds no token. It gets its data from the floor page."

// refuseView refuses a request whose Origin is null, which is what a
// sandboxed view frame sends. Such a request is never the floor page's
// own, so it is refused whatever token it carries. It is no ban strike:
// the floor page shares the address and must not be locked out by a view.
func refuseView(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != "null" {
		return false
	}
	writeErr(w, http.StatusForbidden, ViewRefusal)
	return true
}

// refuseNoToken answers a request that carries no token. It is no strike.
func refuseNoToken(w http.ResponseWriter) {
	writeErr(w, http.StatusUnauthorized, "Bad token. Run coppice web token to see the current one.")
}
