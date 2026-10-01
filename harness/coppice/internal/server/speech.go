package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// speech.status and speech.say: the foreman answers aloud. The page speaks
// with the browser's own voice unless [voice] speak_url names a speech
// server with an OpenAI-style POST /v1/audio/speech. speech.say asks that
// server for one sentence and answers the audio, base64 in the reply. The
// web server turns that reply into a binary frame on the page's socket.

// SpeechRefusal is what a pane or a plugin gets for the speech verbs. The
// speech server may take a key, and the audio is the owner's.
const SpeechRefusal = "only the operator reads aloud."

// The limits of one sentence. A server that takes longer than speechLimit,
// or sends more than speechMaxAudio bytes, has failed: the page then
// speaks that reply with the browser's voice.
const (
	speechMaxAudio = 2 << 20
	speechMaxText  = 2000
)

// speechLimit is how long the speech server has for one sentence.
// A test shortens it.
var speechLimit = 10 * time.Second

// Defaults for a speech server that runs Kokoro-FastAPI. A server that is
// not that one is asked for its own model and voice in coppice.toml.
const (
	speechDefaultModel = "kokoro"
	speechDefaultVoice = "af_heart"
)

// speechEndpoint is the address of POST /v1/audio/speech for a speak_url.
// An address with a user name, a password, a query or a fragment is refused.
// A url that already ends with /audio/speech is used as it is; one that
// ends with /v1 gets /audio/speech; any other gets /v1/audio/speech.
func speechEndpoint(raw string) (string, *url.URL, bool) {
	raw = strings.TrimSpace(raw)
	// No user name, password, query or fragment: a key never lives in
	// coppice.toml, and what the page shows of this address is all of it.
	if strings.ContainsAny(raw, "?#@") {
		return "", nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || !plainHost(u.Hostname()) || bracketedNonIP(u) {
		return "", nil, false
	}
	p := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(p, "/audio/speech"):
	case strings.HasSuffix(p, "/v1"):
		p += "/audio/speech"
	default:
		p += "/v1/audio/speech"
	}
	u.Path = p
	u.RawPath = ""
	return u.String(), u, true
}

// plainHost is true for a host of letters, digits, dots and hyphens, or an
// IP address with no zone, as the Rust server reads one: nothing a name
// lookup should not see.
func plainHost(h string) bool {
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.Zone() == ""
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// bracketedNonIP is true for a host in brackets that is not an IP address.
func bracketedNonIP(u *url.URL) bool {
	if !strings.HasPrefix(u.Host, "[") {
		return false
	}
	_, err := netip.ParseAddr(u.Hostname())
	return err != nil
}

// speechShown is the address the page shows for the speech server: the
// scheme, host and path.
func speechShown(u *url.URL) string {
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
}

func speechOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}

func (s *Server) handleSpeechStatus(_ *Client, r *proto.Request) proto.Response {
	v := s.cfg.Voice
	res := map[string]any{"mode": "browser"}
	if strings.TrimSpace(v.SpeakURL) == "" {
		return proto.OKResp(r.ID, res)
	}
	_, u, ok := speechEndpoint(v.SpeakURL)
	if !ok {
		res["line"] = "[voice] speak_url is not an http:// or https:// address with no user name, password or query, so the browser's own voice speaks. Fix speak_url in coppice.toml, then start the coppice server again."
		return proto.OKResp(r.ID, res)
	}
	res["mode"] = "server"
	res["url"] = speechShown(u)
	res["model"], res["voice"] = speechOr(v.SpeakModel, speechDefaultModel), speechOr(v.SpeakVoice, speechDefaultVoice)
	if v.SpeakProfile != "" {
		res["profile"] = v.SpeakProfile
	}
	return proto.OKResp(r.ID, res)
}

// speechClient never follows a redirect: a redirect is a failure to say,
// and the key must not follow it to another host.
func speechClient() *http.Client {
	return &http.Client{
		// No proxy from the environment: the Rust server uses none, and a
		// key must not go through one. One connection for one sentence.
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		Timeout:       speechLimit,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func slowLine(host string) string {
	span := fmt.Sprintf("%d ms", speechLimit/time.Millisecond)
	if speechLimit%time.Second == 0 {
		span = fmt.Sprintf("%d seconds", speechLimit/time.Second)
	}
	return fmt.Sprintf("The speech server at %s took longer than %s. Check that it runs and is not busy.", host, span)
}

func (s *Server) handleSpeechSay(_ *Client, r *proto.Request) proto.Response {
	text, _ := r.Str("text")
	text = strings.TrimSpace(text)
	if text == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "speech.say needs the text to speak.")
	}
	if len(text) > speechMaxText {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, fmt.Sprintf("That text is over %d bytes. Send one sentence at a time.", speechMaxText))
	}
	v := s.cfg.Voice
	endpoint, u, ok := speechEndpoint(v.SpeakURL)
	if !ok {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "No speech server is set. Set [voice] speak_url in coppice.toml, then start the coppice server again.")
	}
	host := u.Host
	var key string
	if v.SpeakProfile != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		table, err := s.operatorBundle(ctx)
		cancel()
		if err != nil {
			return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The agent profiles could not be read, so the speech server at %s gets no key: %s.", host, strings.TrimSuffix(err.Error(), ".")))
		}
		e, found := table[v.SpeakProfile]
		if !found {
			return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The speak profile %q is not in the operator's agent profiles (a key variable it names may be unset). Fix [voice] speak_profile in coppice.toml.", v.SpeakProfile))
		}
		if e.Key != nil {
			key = *e.Key
		}
		if key != "" && !keyMayGo(u) {
			return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The speech server at %s is reached over plain http, so the key of the profile %q is not sent. Use https, or an address on this box or the tailnet (127.0.0.1, 100.64.0.0/10, fd7a:115c:a1e0::/48, *.ts.net), in speak_url in coppice.toml.", host, v.SpeakProfile))
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model": speechOr(v.SpeakModel, speechDefaultModel), "voice": speechOr(v.SpeakVoice, speechDefaultVoice),
		"input": text, "response_format": "mp3",
	})
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, "[voice] speak_url is not an address the server can send to. Fix it in coppice.toml.")
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := speechClient().Do(req)
	if err != nil {
		if isTimeout(err) {
			return proto.ErrResp(r.ID, proto.ErrTimeout, slowLine(host))
		}
		return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The speech server at %s did not answer. Check that it runs and that speak_url in coppice.toml names it.", host))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The speech server at %s answered %d. Check [voice] speak_model, speak_voice and speak_profile in coppice.toml.", host, resp.StatusCode))
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, speechMaxAudio+1))
	switch {
	case err != nil && isTimeout(err):
		return proto.ErrResp(r.ID, proto.ErrTimeout, slowLine(host))
	case err != nil:
		return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The speech server at %s stopped sending audio. Check that it runs.", host))
	case len(audio) > speechMaxAudio:
		return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The speech server at %s sent more than %d MiB of audio for one sentence. Use a shorter sentence or another server.", host, speechMaxAudio>>20))
	case len(audio) == 0:
		return proto.ErrResp(r.ID, proto.ErrAdapter, fmt.Sprintf("The speech server at %s sent no audio. Check [voice] speak_model and speak_voice in coppice.toml.", host))
	}
	return proto.OKResp(r.ID, map[string]any{
		"type": speechType(resp.Header.Get("Content-Type")), "bytes": len(audio),
		"audio": base64.StdEncoding.EncodeToString(audio),
	})
}

// speechType is the media type the speech server named, with no
// parameters, or audio/mpeg when it named none that looks like audio.
func speechType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	if strings.HasPrefix(ct, "audio/") && len(ct) <= 40 && !strings.ContainsAny(ct, " \"'<>") {
		return ct
	}
	return "audio/mpeg"
}

// tailnetV4 and tailnetV6 are the address ranges Tailscale hands out.
var (
	tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// keyMayGo reports whether a key may be sent to u: over https, or over
// plain http to this box or to a tailnet address or name. The operator sets
// speak_url, so a name that ends in .ts.net is trusted by that suffix: no
// lookup checks it is on the tailnet. An address with a zone is refused.
func keyMayGo(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || tailnetV4.Contains(ip) || tailnetV6.Contains(ip)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// RegisterSpeechCommands wires up speech.status and speech.say.
func (s *Server) RegisterSpeechCommands() {
	_ = s.Handle("speech.status", s.handleSpeechStatus)
	_ = s.Handle("speech.say", s.handleSpeechSay)
}
