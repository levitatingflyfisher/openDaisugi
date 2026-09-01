package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/token/check", s.guard(s.handleTokenCheck))
	s.mux.HandleFunc("GET /api/panes", s.guard(s.handlePanes))
	s.mux.HandleFunc("POST /api/ask/answer", s.guardAnswer(s.handleAskAnswer))
	s.mux.HandleFunc("POST /api/push/test", s.guard(s.handlePushTest))
	s.mux.HandleFunc("POST /api/voice/transcribe", s.guard(s.handleVoiceTranscribe))
	s.mux.HandleFunc("GET /api/voice/status", s.guard(s.handleVoiceStatus))
	s.mux.HandleFunc("POST /api/voice/retry", s.guard(s.handleVoiceRetry))
}

// handleTokenCheck answers only for a good token. The guard already checked
// it, so reaching this line is the answer. The reply carries the name the
// token was minted for, "" for the operator's own token.
func (s *Server) handleTokenCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": nameOf(r)})
}

// handlePanes is the roster's first paint, before the websocket is open.
//
// A refusal is never flattened into an empty list. "No panes yet" and
// "coppice-server refused" are different facts, and painting the first over
// the second would send an operator hunting a bug that is not there.
func (s *Server) handlePanes(w http.ResponseWriter, r *http.Request) {
	s.proxyList(w, r, "pane.list")
}

// denyOnlyKey marks a request that came in with a lock-screen deny token
// instead of the operator's token. Its value is the one ask it may deny.
type denyOnlyKey struct{}

// guardAnswer is guard for the answer route, with one more door: a deny
// token from a lock-screen card. Such a request may deny its own ask and
// nothing else. A token that is neither is a strike, the same as guard.
func (s *Server) guardAnswer(next http.HandlerFunc) http.HandlerFunc {
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
		if name, ok := s.opts.Tokens.Who(tok); ok {
			next(w, withName(r, name))
			return
		}
		if ask, ok := s.opts.Push.DenyGrant(tok); ok {
			next(w, r.WithContext(context.WithValue(r.Context(), denyOnlyKey{}, ask)))
			return
		}
		// A tap on an old card is normal use, not a guess at a token.
		if s.opts.Push.DenyRetired(tok) {
			writeErr(w, http.StatusConflict, "That ask is gone. The gate timed out, or another client answered it.")
			return
		}
		s.bans.Fail(addr)
		s.opts.Log.Warn("web: refused a bad token", "addr", addr, "path", r.URL.Path)
		writeErr(w, http.StatusUnauthorized, "Bad token. Run coppice web token to see the current one.")
	}
}

func (s *Server) handleAskAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToolUseID string `json:"tool_use_id"`
		Decision  string `json:"decision"`
		Reason    string `json:"reason"`
		Confirm   string `json:"confirm"`
		Scope     string `json:"scope"`
		Pane      string `json:"pane"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "The request body is not JSON.")
		return
	}
	if body.Reason == "" {
		body.Reason = "answered from the phone"
	}
	// A client on this box is placed first. A pane process, or one that
	// cannot be placed, is refused, the same as on the coppice socket.
	if hello := peerHello(r); hello != nil && !placeAllows(r.Context(), s.opts.Dial, hello) {
		s.opts.Log.Warn("web: refused an answer from a pane", "remote", r.RemoteAddr)
		writeErr(w, http.StatusForbidden, PaneRefusal)
		return
	}
	// A deny token denies its own ask and nothing else. This refusal is
	// not a strike: the token is real, the request only asks too much.
	if only, ok := r.Context().Value(denyOnlyKey{}).(string); ok {
		if body.Decision != "deny" || body.ToolUseID != only {
			writeErr(w, http.StatusForbidden, "This card can only deny its own ask.")
			return
		}
	}
	reply := Reply{
		ToolUseID: body.ToolUseID, Decision: body.Decision, Reason: body.Reason,
		Scope: body.Scope, Confirm: body.Confirm,
	}
	// The token names who answers. The operator's own token names nobody.
	if name := nameOf(r); name != "" {
		reply.By, reply.WhoFrom = name, "token"
	}
	// An allow needs the ask's tier and the pane's name, which only the
	// coppice server knows. The ask must show on a pane now. A deny needs
	// neither.
	if body.Decision == "allow" {
		facts, err := lookupAsk(r.Context(), s.opts.Dial, body.ToolUseID, body.Pane)
		if err == nil && facts.Held {
			writeErr(w, http.StatusConflict, AtTheFloor(facts.Pane))
			return
		}
		switch {
		case errors.Is(err, ErrNoAsk):
			writeErr(w, http.StatusConflict, "That ask is gone. The gate timed out, or another client answered it.")
			return
		case err != nil:
			s.opts.Log.Warn("web: could not read the ask's tier", "err", err)
			writeErr(w, http.StatusBadGateway, "coppice-server did not answer. Run coppice server status.")
			return
		}
		reply.Tier, reply.Name = facts.Tier, facts.Name
	}
	s.answer(w, reply, func() (string, bool) {
		// A deny the gate has no file for may be an ask a harness holds.
		// Say where to answer it, never that it is gone.
		facts, err := lookupAsk(r.Context(), s.opts.Dial, body.ToolUseID, body.Pane)
		return facts.Pane, err == nil && facts.Held
	})
}

// answer writes one reply through the gate's ask channel and turns the
// result into a response.
func (s *Server) answer(w http.ResponseWriter, reply Reply, held func() (string, bool)) {
	err := s.opts.Gate.Answer(reply)
	var needsName *NeedsNameError
	if errors.Is(err, ErrNoAsk) && held != nil {
		if pane, ok := held(); ok {
			writeErr(w, http.StatusConflict, AtTheFloor(pane))
			return
		}
	}
	switch {
	case errors.Is(err, ErrBadDecision):
		writeErr(w, http.StatusBadRequest, "Send decision allow or deny.")
	case errors.Is(err, ErrBadScope):
		writeErr(w, http.StatusBadRequest, "Send scope once or task.")
	case errors.Is(err, ErrNoAsk):
		writeErr(w, http.StatusConflict, "That ask is gone. The gate timed out, or another client answered it.")
	case errors.As(err, &needsName):
		writeErr(w, http.StatusForbidden, needsName.Error())
	case errors.Is(err, ErrPermanentTask):
		writeErr(w, http.StatusForbidden, err.Error()+" with the pane name.")
	case err != nil:
		s.opts.Log.Error("web: could not write the answer", "err", err)
		writeErr(w, http.StatusInternalServerError, "The answer could not be written. Check that the gate directory exists.")
	default:
		s.opts.Push.RetireDeny(reply.ToolUseID)
		s.opts.Log.Info("web: answered an ask", "decision", reply.Decision, "tool_use_id", reply.ToolUseID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// askFacts is what an allow needs to know about an ask from the coppice
// server: its tier and the name of the pane that holds it.
type askFacts struct {
	Pane string
	Tier string
	Name string
	Held bool // a harness holds the ask itself
}

// lookupAsk finds the pane whose state shows ask id, through agent.list.
// When pane is not empty, only that pane counts. It returns ErrNoAsk when
// no pane shows the ask, and any other error when the server did not
// answer or answered with something it cannot read.
func lookupAsk(ctx context.Context, d Dialer, id, pane string) (askFacts, error) {
	if id == "" {
		return askFacts{}, ErrNoAsk
	}
	msg, err := Call(ctx, d, map[string]any{"cmd": "agent.list"})
	if err != nil {
		return askFacts{}, err
	}
	result, ok := msg["result"].(map[string]any)
	if !ok {
		return askFacts{}, errors.New("agent.list answered with no result")
	}
	agents, ok := result["agents"].([]any)
	if !ok {
		return askFacts{}, errors.New("agent.list answered with no agents list")
	}
	for _, raw := range agents {
		row, _ := raw.(map[string]any)
		ask, _ := row["ask"].(map[string]any)
		if ask == nil || row["state"] != "blocked" {
			continue
		}
		askID, _ := ask["id"].(string)
		paneID, _ := row["pane"].(string)
		if askID != id || paneID == "" || (pane != "" && paneID != pane) {
			continue
		}
		tier, _ := ask["tier"].(string)
		name, _ := row["label"].(string)
		if name == "" {
			name = paneID
		}
		holder, _ := ask["holder"].(string)
		return askFacts{Pane: paneID, Tier: tier, Name: name, Held: holder == "harness"}, nil
	}
	return askFacts{}, ErrNoAsk
}

// handlePushTest lets the settings screen prove a configured ntfy topic
// actually works. When push is off it says so and names the flags that
// turn it on, instead of a bare 409.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if s.opts.Push == nil {
		writeErr(w, http.StatusConflict,
			"Push is off. Restart the server with --ntfy URL and --ntfy-topic NAME.")
		return
	}
	if err := s.opts.Push.Test(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, "ntfy did not accept the message. Check the URL and the token.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
