package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	// WSSubprotocol is the only name the server ever negotiates.
	WSSubprotocol = "daisugi.v1"
	// WSBearerPrefix carries the token in a second subprotocol offer,
	// because the browser WebSocket API cannot set request headers.
	WSBearerPrefix = "daisugi.bearer."
)

// bearerFrom reads the token from Authorization, then from a
// Sec-WebSocket-Protocol offer. Terminal clients use the header; browsers
// use the offer.
func bearerFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	for _, raw := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, WSBearerPrefix) {
				return strings.TrimPrefix(part, WSBearerPrefix)
			}
		}
	}
	return ""
}

// handleWS gives one browser one socket connection and copies JSONL lines
// through untouched in both directions. Nothing here parses a request: the
// browser speaks the same protocol a terminal speaks, so parity is the
// shape of the code rather than a claim about it.
//
// Subprotocols lists only WSSubprotocol, so the handshake response never
// echoes the token back. Accept refuses a cross-origin upgrade by default,
// and that default stays: no other page may drive the operator's panes.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{WSSubprotocol},
	})
	if err != nil {
		s.opts.Log.Warn("web: upgrade refused", "err", err)
		return
	}

	s.wsLive.Add(1)
	defer s.wsLive.Add(-1)
	defer c.CloseNow()
	// The limit here is the upstream's own inbound limit, not the reply
	// limit: a browser frame this package cannot forward should never reach
	// the read loop at all. MaxRequestBytes counts the newline Send adds, so
	// the read limit sits one byte under it.
	c.SetReadLimit(MaxRequestBytes - 1)

	sess, err := Open(r.Context(), s.opts.Dial)
	if err != nil {
		s.opts.Log.Warn("web: coppice-server is not reachable", "err", err)
		c.Close(websocket.StatusInternalError, "coppice-server is not reachable")
		return
	}

	// The coppice server hears who this client is before any line from
	// the browser: the name its token carries, and for a client on this
	// box, the local process behind it, so the server's own role check
	// applies to every line this socket forwards. name_from token fixes
	// the name, so a hello from the browser cannot change it. The reply to
	// this hello is dropped below. A hello that cannot be sent closes the
	// socket.
	hello := map[string]any{"name_from": "token"}
	if name := nameOf(r); name != "" {
		hello["name"] = name
	}
	for k, v := range peerHello(r) {
		hello[k] = v
	}
	line, err := helloLine(hello)
	if err == nil {
		err = sess.Send(line)
	}
	if err != nil {
		s.opts.Log.Warn("web: could not name the client upstream", "err", err)
		sess.Close()
		c.Close(websocket.StatusInternalError, "coppice-server is not reachable")
		return
	}
	// No browser line goes upstream until the server accepts the hello. A
	// refused hello leaves the name open, and a browser hello could then
	// fill it, so a refusal, a timeout or a closed upstream ends the socket.
	if reason := awaitHello(r.Context(), sess); reason != "" {
		s.opts.Log.Warn("web: the server did not accept the client's hello", "reason", reason)
		sess.Close()
		c.Close(websocket.StatusInternalError, reason)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for line := range sess.Lines() {
			if err := c.Write(ctx, websocket.MessageText, line); err != nil {
				return
			}
		}
		// sess.Lines closed with no write error, so coppice-server ended the
		// connection. The browser is told why, and cancel makes sure the
		// read loop below returns even if the browser never answers.
		c.Close(websocket.StatusInternalError, "coppice-server closed the connection")
		cancel()
	}()

	// This order matters. cancel unblocks a write stuck on the network,
	// sess.Close closes the upstream socket and so closes sess.Lines, and
	// only once both have run is the forwarding goroutine guaranteed to
	// stop, which is what wg.Wait proves before the handler returns.
	defer wg.Wait()
	defer sess.Close()
	defer cancel()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		if err := sess.Send(data); err != nil {
			s.opts.Log.Warn("web: could not forward a line to coppice-server", "err", err)
			return
		}
	}
}

// helloWait bounds how long a websocket waits for the reply to its hello.
const helloWait = 5 * time.Second

// awaitHello reads the reply to the web hello, the first line the server
// sends on a new upstream. It returns "" when the server accepted it, and
// the close reason when it refused it, did not answer in helloWait, or
// closed the connection.
func awaitHello(ctx context.Context, sess *Session) string {
	t := time.NewTimer(helloWait)
	defer t.Stop()
	for {
		select {
		case line, ok := <-sess.Lines():
			if !ok {
				return "coppice-server closed the connection"
			}
			if !isWebHelloReply(line) {
				continue
			}
			var m struct {
				OK bool `json:"ok"`
			}
			if json.Unmarshal(line, &m) != nil || !m.OK {
				return "coppice-server refused this client"
			}
			return ""
		case <-t.C:
			return "coppice-server did not answer"
		case <-ctx.Done():
			return "coppice-server did not answer"
		}
	}
}
