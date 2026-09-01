package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/token/check", s.guard(s.handleTokenCheck))
	s.mux.HandleFunc("GET /api/panes", s.guard(s.handlePanes))
	s.mux.HandleFunc("POST /api/ask/answer", s.guard(s.handleAskAnswer))
	s.mux.HandleFunc("POST /api/push/test", s.guard(s.handlePushTest))
	s.mux.HandleFunc("POST /api/voice/transcribe", s.guard(s.handleVoiceTranscribe))
}

// handleTokenCheck answers only for a good token. The guard already checked
// it, so reaching this line is the answer.
func (s *Server) handleTokenCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePanes is the roster's first paint, before the websocket is open.
//
// A refusal is never flattened into an empty list. "No panes yet" and
// "coppice-server refused" are different facts, and painting the first over
// the second would send an operator hunting a bug that is not there.
func (s *Server) handlePanes(w http.ResponseWriter, r *http.Request) {
	msg, err := Call(r.Context(), s.opts.Dial, map[string]any{"cmd": "pane.list"})
	var refused *RefusedError
	switch {
	case errors.As(err, &refused):
		s.opts.Log.Warn("web: pane.list refused", "code", refused.Code, "message", refused.Message)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "server refused: " + refused.Message,
			"code":  refused.Code,
		})
		return
	case err != nil:
		s.opts.Log.Warn("web: pane.list failed", "err", err)
		writeErr(w, http.StatusBadGateway, "coppice-server did not answer. Run coppice server status.")
		return
	}
	// An ok:true reply whose result is not an object is not "no panes
	// yet". It is a reply the phone cannot read, and it gets the same
	// refusal treatment as an ok:false one, never a painted-over empty
	// roster.
	result, isObject := msg["result"].(map[string]any)
	if !isObject {
		s.opts.Log.Warn("web: pane.list answered ok with a result the phone cannot read")
		writeErr(w, http.StatusBadGateway, "coppice-server sent a reply the phone could not read. Run coppice server status.")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAskAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToolUseID string `json:"tool_use_id"`
		Decision  string `json:"decision"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "The request body is not JSON.")
		return
	}
	if body.Reason == "" {
		body.Reason = "answered from the phone"
	}
	err := s.opts.Gate.Answer(body.ToolUseID, body.Decision, body.Reason)
	switch {
	case errors.Is(err, ErrBadDecision):
		writeErr(w, http.StatusBadRequest, "Send decision allow or deny.")
	case errors.Is(err, ErrNoAsk):
		writeErr(w, http.StatusConflict, "That ask is gone. The gate timed out, or another client answered it.")
	case err != nil:
		s.opts.Log.Error("web: could not write the answer", "err", err)
		writeErr(w, http.StatusInternalServerError, "The answer could not be written. Check that the gate directory exists.")
	default:
		s.opts.Log.Info("web: answered an ask", "decision", body.Decision, "tool_use_id", body.ToolUseID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
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
