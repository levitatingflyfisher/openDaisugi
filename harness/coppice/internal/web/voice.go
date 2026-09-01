package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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

// voiceRetry is the words every voice message ends with, beside the page's
// Retry button.
const voiceRetry = "press Retry"

// voiceTarget is where a clip goes and the bearer it carries.
type voiceTarget struct {
	url, token string
}

// voiceState is what the page learns about voice: ready, the coppice
// server's state word, and one line that says why voice cannot run and
// how to fix it, empty when it is ready.
type voiceState struct {
	Ready   bool   `json:"ready"`
	State   string `json:"state"`
	Message string `json:"message"`
	// Action is "retry" when the message has a Retry beside it.
	Action string `json:"action,omitempty"`
	// Command is a shell line that fixes it, for the page to type and not
	// run. Away is true when the fix is on the computer that runs coppice,
	// so Retry cannot help.
	Command string `json:"command,omitempty"`
	Away    bool   `json:"away,omitempty"`
}

// offBox is what voice says when --voice-url names another machine and
// no token file of its own: the web sign-in token never leaves this
// machine.
const offBox = "Voice is off: --voice-url names another machine, and coppice sends it no web token. " +
	"Start the web server again with --voice-token-file naming that server's token file."

// loopbackURL reports whether raw names localhost or a loopback address.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// voiceNow is where voice stands: the state for the page, and where a clip
// goes when voice is ready. --voice-url wins over the coppice server; the
// bearer is then the --voice-token-file token, or the web token, which is
// only sent to this machine.
func (s *Server) voiceNow(r *http.Request, verb string) (voiceState, *voiceTarget, error) {
	if s.opts.VoiceURL == "" {
		st, t := s.askVoice(r, verb)
		return st, t, nil
	}
	if s.opts.VoiceTokenFile != "" {
		raw, err := os.ReadFile(s.opts.VoiceTokenFile)
		if err != nil {
			return voiceState{State: "off", Away: true,
				Message: fmt.Sprintf("Voice is off: the voice token file %s cannot be read.", s.opts.VoiceTokenFile)}, nil, nil
		}
		return voiceState{Ready: true, State: "ready"},
			&voiceTarget{url: s.opts.VoiceURL, token: strings.TrimSpace(string(raw))}, nil
	}
	if !loopbackURL(s.opts.VoiceURL) {
		return voiceState{State: "off", Away: true, Message: offBox}, nil, nil
	}
	tok, err := s.opts.Tokens.Load()
	if err != nil {
		return voiceState{}, nil, err
	}
	return voiceState{Ready: true, State: "ready"}, &voiceTarget{url: s.opts.VoiceURL, token: tok}, nil
}

// voiceMessage joins the coppice server's reason and fix into one line
// that ends with Retry.
func voiceMessage(reason, fix string) string {
	if fix == "" {
		return reason + " Press Retry."
	}
	return reason + " " + fix + ", then " + voiceRetry + "."
}

// askVoice runs verb, voice.status or voice.start, on the coppice server.
// It returns the state for the page, and the target when voice is ready.
func (s *Server) askVoice(r *http.Request, verb string) (voiceState, *voiceTarget) {
	msg, err := Call(r.Context(), s.opts.Dial, map[string]any{"cmd": verb})
	if err != nil {
		var refused *RefusedError
		if errors.As(err, &refused) {
			return voiceState{State: "down", Action: "retry",
				Message: voiceMessage("This coppice server is too old to start voice.",
					"Stop it and start it again with this coppice")}, nil
		}
		return voiceState{State: "down", Action: "retry",
			Message: voiceMessage("The coppice server did not answer.", "")}, nil
	}
	res, _ := msg["result"].(map[string]any)
	str := func(k string) string { v, _ := res[k].(string); return v }
	ready, _ := res["ready"].(bool)
	st := voiceState{Ready: ready, State: str("state"), Command: str("command")}
	if st.State == "off" {
		// Voice is off by the coppice server's own config. Retry cannot
		// change that; the fix is on the box.
		st.Message, st.Away = str("reason")+" "+str("fix")+".", true
		return st, nil
	}
	if !ready {
		st.Message, st.Action = voiceMessage(str("reason"), str("fix")), "retry"
		return st, nil
	}
	tok := ""
	if f := str("token_file"); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return voiceState{State: "down", Action: "retry",
				Message: voiceMessage("Voice's token file cannot be read.", "Start voice again")}, nil
		}
		tok = strings.TrimSpace(string(raw))
	}
	return st, &voiceTarget{url: str("url"), token: tok}
}

// handleVoiceStatus tells the page whether the mic can work, and if not,
// why and how to fix it.
func (s *Server) handleVoiceStatus(w http.ResponseWriter, r *http.Request) {
	st, _, _ := s.voiceNow(r, "voice.status")
	writeJSON(w, http.StatusOK, st)
}

// handleVoiceRetry is the page's Retry: it asks the coppice server to
// start voice, and answers what voice is now.
func (s *Server) handleVoiceRetry(w http.ResponseWriter, r *http.Request) {
	st, _, _ := s.voiceNow(r, "voice.start")
	writeJSON(w, http.StatusOK, st)
}

// handleVoiceTranscribe forwards a recorded clip to the voice server and
// relays its reply. The voice server is Options.VoiceURL when that is
// set, with the web token as the bearer, since daisugi voice serve reads
// the web token file by default. Otherwise it is the one the coppice
// server names in voice.status, with the token from the file it names.
// The guard already required a good bearer token to reach here, so the
// request is trusted; the bearer this handler sends onward never comes
// from whatever the phone happened to send.
func (s *Server) handleVoiceTranscribe(w http.ResponseWriter, r *http.Request) {
	st, target, tokErr := s.voiceNow(r, "voice.status")
	if tokErr != nil {
		io.Copy(io.Discard, io.LimitReader(r.Body, MaxVoiceClipBytes))
		writeErr(w, http.StatusInternalServerError, "No web token. Run coppice web token, then try again.")
		return
	}
	if target == nil {
		// Draining the body before answering lets the client finish its
		// write and read the reply over the same connection, rather than
		// having the connection close out from under a clip still in
		// flight. The clip is discarded, up to the same cap a working
		// voice server would enforce.
		io.Copy(io.Discard, io.LimitReader(r.Body, MaxVoiceClipBytes))
		reply := map[string]any{"error": st.Message, "state": st.State}
		if st.Action != "" {
			reply["action"] = st.Action
		}
		if st.Command != "" {
			reply["command"] = st.Command
		}
		if st.Away {
			reply["away"] = true
		}
		writeJSON(w, http.StatusServiceUnavailable, reply)
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

	// A managed voice server comes and goes with the coppice server, so a
	// failure to reach it offers Retry. A --voice-url server is the
	// owner's to start.
	managed := s.opts.VoiceURL == ""

	dest := strings.TrimRight(target.url, "/") + "/transcribe"
	if r.URL.RawQuery != "" {
		dest += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, dest, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError,
			fmt.Sprintf("The request to the voice server at %s could not be built.", target.url))
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upReq.Header.Set("Content-Type", ct)
	}
	upReq.Header.Set("Authorization", "Bearer "+target.token)

	resp, err := voiceClient.Do(upReq)
	if err != nil {
		if managed {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"state": "down", "action": "retry",
				"error": voiceMessage(fmt.Sprintf("Could not reach the voice server at %s.", target.url), "")})
			return
		}
		writeErr(w, http.StatusBadGateway,
			fmt.Sprintf("Could not reach the voice server at %s. Run daisugi voice serve first.", target.url))
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
