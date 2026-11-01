package web

import (
	"bytes"
	"context"
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
// voice server. The owner dictates long prompts, and the voice server takes
// 20 minutes of audio, so this leaves room for 20 minutes as an
// uncompressed 16 kHz wav (about 38 MB) while still refusing a runaway
// upload before it reaches the voice server at all.
const MaxVoiceClipBytes = 64 << 20

// voiceClient talks to daisugi voice serve. One clip transcribes in one
// call, and a long clip is cut into windows that the engine takes one at a
// time. The wait is as long as the voice server can run: whisper.cpp takes
// up to 120 s for each of 40 windows of a 20 minute clip (80 minutes), and
// cleanup adds a few minutes. A shorter wait threw the finished text away
// and left the server busy for the next clip. A dead server still cannot
// hang the request forever.
const voiceWait = 90 * time.Minute

var voiceClient = &http.Client{Timeout: voiceWait}

// isTimeout is true when err is the client's wait running out.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

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
	// Fetch is {done, total} in bytes while voice, starting, gets its
	// speech model.
	Fetch map[string]any `json:"fetch,omitempty"`
	// MaxSeconds is the longest clip the voice server takes. The page reads
	// its cap from here, so it keeps no second number.
	MaxSeconds float64 `json:"max_seconds"`
	// Partial is true when the voice server shows the text while the owner
	// talks (it answers /partial), and PartialEngine names the engine that
	// reads it.
	Partial       bool   `json:"partial"`
	PartialEngine string `json:"partial_engine,omitempty"`
	// PartialKnown is true when the voice server answered the probe, so
	// Partial false means a server with no live text, not one that could
	// not be asked.
	PartialKnown bool `json:"partial_known,omitempty"`
}

// VoiceMaxClipSeconds is the cap used until the voice server says its own
// (a voice server built before /health named it). The voice server owns the
// number: askVoiceCap reads it from /health.
const VoiceMaxClipSeconds = 1200.0

// voiceInfo is what the voice server at target says about itself.
type voiceInfo struct {
	MaxSeconds    float64 `json:"max_seconds"`
	Partial       bool    `json:"partial"`
	PartialEngine string  `json:"partial_engine"`
	// Known is true when /status or /health answered.
	Known bool `json:"-"`
}

// askVoiceInfo reads the voice server's /status, and falls back to /health for a voice server built before /status. The cap
// falls back to VoiceMaxClipSeconds when neither names one, and live text
// is off unless /status says it is on.
func askVoiceInfo(ctx context.Context, target *voiceTarget) voiceInfo {
	off := voiceInfo{MaxSeconds: VoiceMaxClipSeconds}
	if target == nil {
		return off
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Neither probe sends the token: a wrong one would count toward the
	// voice server's ban, and the clip after it would meet "wait 60
	// seconds" instead of the sentence that names the fix.
	get := func(path string) (voiceInfo, bool) {
		var info voiceInfo
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.url, "/")+path, nil)
		if err != nil {
			return info, false
		}
		resp, err := voiceClient.Do(req)
		if err != nil {
			return info, false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return info, false
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info) != nil || info.MaxSeconds <= 0 {
			return info, false
		}
		return info, true
	}
	if info, ok := get("/status"); ok {
		if !info.Partial {
			info.PartialEngine = ""
		}
		info.Known = true
		return info
	}
	if info, ok := get("/health"); ok {
		return voiceInfo{MaxSeconds: info.MaxSeconds, Known: true}
	}
	return off
}

// withInfo copies what the voice server says about itself into st.
func withInfo(st voiceState, info voiceInfo) voiceState {
	st.MaxSeconds, st.Partial, st.PartialEngine = info.MaxSeconds, info.Partial, info.PartialEngine
	st.PartialKnown = info.Known
	return st
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
	if f, ok := res["fetch"].(map[string]any); ok && st.State == "starting" {
		st.Fetch = f
	}
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
	st, target, _ := s.voiceNow(r, "voice.status")
	writeJSON(w, http.StatusOK, withInfo(st, askVoiceInfo(r.Context(), target)))
}

// handleVoiceRetry is the page's Retry: it asks the coppice server to
// start voice, and answers what voice is now.
func (s *Server) handleVoiceRetry(w http.ResponseWriter, r *http.Request) {
	st, target, _ := s.voiceNow(r, "voice.start")
	writeJSON(w, http.StatusOK, withInfo(st, askVoiceInfo(r.Context(), target)))
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
	s.forwardVoice(w, r, "/transcribe")
}

// voicePartialClient waits a short time: a partial is the text so far,
// worth showing only while the owner still talks. The page sends a newer
// one soon after.
var voicePartialClient = &http.Client{Timeout: 15 * time.Second}

// handleVoicePartial forwards the recording so far to the voice server's
// /partial and relays the text so far. Unlike a clip, it follows the
// page: a page that goes away frees the voice server at once. An older
// voice server answers 404, which reaches the page as it is.
func (s *Server) handleVoicePartial(w http.ResponseWriter, r *http.Request) {
	s.forwardVoice(w, r, "/partial")
}

// forwardVoice sends the clip in r to the voice server's path, /transcribe
// or /partial, and relays the reply.
func (s *Server) forwardVoice(w http.ResponseWriter, r *http.Request, path string) {
	partial := path == "/partial"
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

	dest := strings.TrimRight(target.url, "/") + path
	if r.URL.RawQuery != "" {
		dest += "?" + r.URL.RawQuery
	}
	// The upstream call for a clip does not follow the page. A page that
	// reloads or loses its connection drops this request, but the voice
	// server keeps its job and answers 409 to a second clip until it ends.
	// Waiting for its answer here, as the Rust server does, keeps both
	// servers the same. A partial follows the page.
	ctx := context.WithoutCancel(r.Context())
	client := voiceClient
	if partial {
		ctx, client = r.Context(), voicePartialClient
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, dest, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError,
			fmt.Sprintf("The request to the voice server at %s could not be built.", target.url))
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upReq.Header.Set("Content-Type", ct)
	}
	upReq.Header.Set("Authorization", "Bearer "+target.token)

	// Whether a job is already running is the voice server's to say: it
	// answers 409, and relayVoiceReply passes that on.
	resp, err := client.Do(upReq)
	if err != nil {
		if partial && isTimeout(err) {
			writeErr(w, http.StatusGatewayTimeout, "The text so far did not come back in time. The next one is on its way.")
			return
		}
		if isTimeout(err) {
			// The voice server did not see the cancel and may still be
			// working. Say that this was a wait that ran out, not a dead
			// server, so the owner does not start the same job twice.
			writeErr(w, http.StatusGatewayTimeout, fmt.Sprintf(
				"The clip took longer than %d minutes to turn into text. The voice server may still be working on it. Wait a few minutes before sending it again.",
				int(voiceClient.Timeout/time.Minute)))
			return
		}
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
