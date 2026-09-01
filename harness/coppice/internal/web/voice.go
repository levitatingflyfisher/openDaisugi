package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxVoiceClipBytes bounds one uploaded clip before it is forwarded to the
// voice server. A phone recording runs a few seconds to a minute, so this
// leaves room for that while still refusing a runaway upload before it
// reaches the voice server at all.
const MaxVoiceClipBytes = 8 << 20

// voiceClient talks to daisugi voice serve. One clip transcribes in one
// call; the timeout leaves room for a slow CPU-only model on a full-length
// clip without letting a dead server hang the request forever.
var voiceClient = &http.Client{Timeout: 120 * time.Second}

// handleVoiceTranscribe forwards a recorded clip to the voice server named
// by Options.VoiceURL and relays its reply. The guard already required a
// good bearer token to reach here, so the request is trusted; the bearer
// this handler sends onward comes from its own token store, not from
// whatever the phone happened to send.
func (s *Server) handleVoiceTranscribe(w http.ResponseWriter, r *http.Request) {
	if s.opts.VoiceURL == "" {
		// Draining the body before answering lets the client finish its
		// write and read the 409 back over the same connection, rather than
		// having the connection close out from under a clip still in
		// flight. Nothing here is decoded or forwarded; the clip is simply
		// discarded, up to the same cap a configured server would enforce.
		io.Copy(io.Discard, io.LimitReader(r.Body, MaxVoiceClipBytes))
		writeErr(w, http.StatusConflict,
			"Voice is not set up. Run daisugi voice serve, then start coppice web serve again with --voice-url.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxVoiceClipBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("The clip is larger than %d bytes. Record a shorter clip.", MaxVoiceClipBytes))
			return
		}
		writeErr(w, http.StatusBadRequest, "The clip could not be read. Record it again.")
		return
	}

	tok, err := s.opts.Tokens.Load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError,
			"No web token. Run coppice web token, then try again.")
		return
	}

	target := strings.TrimRight(s.opts.VoiceURL, "/") + "/transcribe"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError,
			fmt.Sprintf("The request to the voice server at %s could not be built.", s.opts.VoiceURL))
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upReq.Header.Set("Content-Type", ct)
	}
	upReq.Header.Set("Authorization", "Bearer "+tok)

	resp, err := voiceClient.Do(upReq)
	if err != nil {
		writeErr(w, http.StatusBadGateway,
			fmt.Sprintf("Could not reach the voice server at %s. Run daisugi voice serve first.", s.opts.VoiceURL))
		return
	}
	defer resp.Body.Close()
	relayVoiceReply(w, resp)
}

// relayVoiceReply writes the voice server's status and body back to the
// phone. The voice server's own error shape carries a short code under
// "error" and the sentence a person reads under "message"; this package's
// own convention treats "error" itself as the sentence, so a reply that
// carries a message gets that sentence copied into "error" before it goes
// out, keeping every failure one sentence that says what to do next. The
// short code is kept too, under "code", for a caller that later wants the
// machine-readable form. Nothing is rewritten, and the bytes go out exactly
// as they arrived, when there is no "message" to move: a successful
// transcribe reply carries none, and this is what keeps its numbers
// byte-identical instead of round-tripping through float64. A body that is
// not a JSON object relays exactly as it arrived, under the voice server's
// own Content-Type.
func relayVoiceReply(w http.ResponseWriter, resp *http.Response) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "The voice server's reply could not be read.")
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) == nil {
		if msg, ok := body["message"].(string); ok && msg != "" {
			if code, exists := body["error"]; exists {
				body["code"] = code
			}
			body["error"] = msg
			if rewritten, err := json.Marshal(body); err == nil {
				raw = rewritten
			}
		}
		w.Header().Set("Content-Type", "application/json")
	} else if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(raw)
}
