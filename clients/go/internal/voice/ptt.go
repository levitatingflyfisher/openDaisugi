package voice

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// ServerError is ptt.VoiceServerError: a one-line sentence for a person.
// Busy is true when the server is still turning an earlier clip into text
// (409): the clip is good and can go out again later.
type ServerError struct {
	Msg  string
	Busy bool
	// Status is the HTTP status the server answered, or 0 when it did not
	// answer.
	Status int
}

func (e *ServerError) Error() string { return e.Msg }

// Stream is ptt.RecordStream.
type Stream interface {
	Start() error
	Stop() error
	// Read is the next chunk and whether more is waiting.
	Read(frames int) ([]byte, bool, error)
}

// Client is ptt.VoiceClient.
type Client interface {
	Transcribe(wav []byte) (any, error)
	Deliver(pane, text, mode string) (any, error)
}

// Result is one press and release: the text and the delivery answer.
type Result struct {
	Text  any
	Reply any
}

// pyStr is str(v) for the JSON values a server answer holds.
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case pyjson.Int:
		return x.Text
	case pyjson.Float:
		return pyjson.FloatRepr(float64(x))
	case float64:
		return pyjson.FloatRepr(x)
	}
	return pyjson.Dumps(v, false)
}

// truthy is Python truth for a JSON value.
func truthy(v any) bool { return pyjson.Truthy(v) }

// get is dict.get(k, def) on a JSON object; any other value raises
// AttributeError in the oracle, which ends the session.
func get(v any, k string, def any) (any, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errors.New("'" + pyTypeName(v) + "' object has no attribute 'get'")
	}
	if x, has := o.Get(k); has {
		return x, nil
	}
	return def, nil
}

func pyTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case pyjson.Int:
		return "int"
	}
	return "float"
}

// pyEq is == between two JSON values from one answer.
func pyEq(a, b any) bool { return pyjson.Dumps(a, true) == pyjson.Dumps(b, true) }

func skipped(reason string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("delivered", "skipped")
	o.Set("reason", reason)
	return o
}

// RecordAndSend is ptt.record_and_send: the frames as a 16 kHz WAV,
// transcribed, then delivered in preview mode.
func RecordAndSend(frames [][]byte, pane string, client Client, sampleRate int64, print func(string)) (*Result, error) {
	if len(frames) == 0 {
		print("No audio was captured. Record for longer before tapping space again.")
		return &Result{"", skipped("no audio was captured")}, nil
	}
	wav, aerr := writeWavPCM(bytes.Join(frames, nil), sampleRate)
	if aerr != nil {
		return nil, aerr
	}
	result, err := client.Transcribe(wav)
	if err != nil {
		return nil, err
	}
	text, err := get(result, "text", "")
	if err != nil {
		return nil, err
	}
	raw, _ := get(result, "raw_text", text)
	if !pyEq(raw, text) {
		print("Heard: " + pyStr(raw))
	}
	if !truthy(text) {
		print("No speech was heard. Record for longer before tapping space again.")
		return &Result{text, skipped("no speech was heard")}, nil
	}
	print(pyStr(text))
	reply, err := client.Deliver(pane, pyStr(text), "preview")
	if err != nil {
		return nil, err
	}
	word, err := get(reply, "delivered", "?")
	if err != nil {
		return nil, err
	}
	line := pyStr(word)
	if reason, _ := get(reply, "reason", nil); truthy(reason) {
		line = line + ". " + pyStr(reason)
	}
	print(line)
	return &Result{text, reply}, nil
}

// PartialClient is a Client that can ask for the text of a recording so
// far. seq numbers the partials of one recording; the answer carries the
// seq the server read.
type PartialClient interface {
	Partial(wav []byte, seq int) (string, int, error)
}

// Snapshotter is a Stream that can give the audio recorded so far without
// stopping.
type Snapshotter interface {
	Snapshot() []byte
}

// LiveTextOld is the line under the prompt when the voice server is too
// old to show the text while the owner talks.
const LiveTextOld = "Live text needs a newer voice server."

// LiveListening is the line under the prompt while it records, before any
// words.
const LiveListening = "Listening…"

// LiveEvery is how often push to talk asks for the text so far.
const LiveEvery = 1500 * time.Millisecond

// Live is how push to talk shows the text while the owner talks. Show
// draws one line in place; "" clears it. Ticks starts the clock for one
// recording and answers its channel and the function that stops it. A
// Live with no Show shows nothing.
type Live struct {
	Show  func(string)
	Ticks func() (<-chan time.Time, func())
}

// liveLoop asks for the text so far on each tick until stop closes. One
// partial is in flight at a time. A reply older than the one shown is
// dropped. A 404 is a server too old for partials: the line says so once
// and the loop ends. A refused token ends it and clears the line, so no
// "Listening…" stays up that will never change. Any other failure says
// nothing.
func liveLoop(pc PartialClient, snap Snapshotter, live Live, stop <-chan struct{}) {
	ticks, halt := live.Ticks()
	defer halt()
	seq, shown := 0, 0
	for {
		select {
		case <-stop:
			return
		case <-ticks:
		}
		pcm := snap.Snapshot()
		if len(pcm) == 0 {
			continue
		}
		wav, aerr := writeWavPCM(pcm, 16000)
		if aerr != nil {
			continue
		}
		seq++
		text, n, err := pc.Partial(wav, seq)
		select {
		case <-stop:
			return
		default:
		}
		if err != nil {
			var se *ServerError
			if errors.As(err, &se) && se.Status == 404 {
				live.Show(LiveTextOld)
				return
			}
			// A refused token ends the partials and clears the line: each
			// refusal counts toward the server's ban, and the clip must
			// still meet the sentence that names the fix.
			if se != nil && (se.Status == 401 || se.Status == 403 || se.Status == 429) {
				live.Show("")
				return
			}
			continue
		}
		if n < shown {
			continue
		}
		shown = n
		if text == "" {
			text = LiveListening
		}
		live.Show(text)
	}
}

// RunPTT is ptt.run_ptt: space starts a recording and stops it, q quits.
// keys yields one key at a time, and false at the end. A cycle the server
// fails prints its sentence and the session goes on.
func RunPTT(pane string, client Client, keys func() (string, bool), open func() (Stream, error),
	chunkFrames int, print func(string)) ([]Result, error) {
	return RunPTTLive(pane, client, keys, open, chunkFrames, print, Live{})
}

// RunPTTLive is RunPTT that shows the text so far while the owner talks,
// when live has a Show, the client can ask for partials and the stream
// can give its audio so far. The line clears before the final text
// prints.
func RunPTTLive(pane string, client Client, keys func() (string, bool), open func() (Stream, error),
	chunkFrames int, print func(string), live Live) ([]Result, error) {
	var results []Result
	recording := false
	var stream Stream
	var frames [][]byte
	// kept is a clip the busy server refused. It goes out with the next one.
	var kept [][]byte
	// stopLive ends the live line of the recording now running and clears
	// it. It does not wait for a partial still out: the clip goes at once,
	// and the guard in show keeps a late answer off the screen. join waits
	// for the loop to end, before the next recording and at the end.
	stopLive := func() {}
	join := func() {}
	startLive := func() {
		pc, ok := client.(PartialClient)
		snap, ok2 := stream.(Snapshotter)
		if live.Show == nil || !ok || !ok2 {
			return
		}
		join()
		if live.Ticks == nil {
			live.Ticks = func() (<-chan time.Time, func()) {
				t := time.NewTicker(LiveEvery)
				return t.C, t.Stop
			}
		}
		var mu sync.Mutex
		over := false
		guarded := live
		guarded.Show = func(line string) {
			mu.Lock()
			defer mu.Unlock()
			if !over {
				live.Show(line)
			}
		}
		live.Show(LiveListening)
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			liveLoop(pc, snap, guarded, stop)
		}()
		stopLive = func() {
			mu.Lock()
			over = true
			live.Show("")
			mu.Unlock()
			close(stop)
			stopLive = func() {}
		}
		join = func() {
			<-done
			join = func() {}
		}
	}
	defer func() { stopLive(); join() }()
	for {
		ch, ok := keys()
		if !ok {
			break
		}
		if ch == "q" {
			stopLive()
			if recording && stream != nil {
				if err := stream.Stop(); err != nil {
					return results, err
				}
			}
			if len(kept) > 0 {
				print("A kept recording was not sent.")
			}
			break
		}
		if ch == " " && !recording {
			recording = true
			frames = nil
			s, err := open()
			if err != nil {
				return results, err
			}
			stream = s
			if err := stream.Start(); err != nil {
				return results, err
			}
			startLive()
		} else if ch == " " && recording {
			recording = false
			// Read first: it stops the recorder, so the clip ends where the
			// owner tapped. Then the live line ends, without waiting for a
			// partial still out, and the clip goes.
			for more := true; more; {
				chunk, m, err := stream.Read(chunkFrames)
				if err != nil {
					return results, err
				}
				more = m
				if len(chunk) > 0 {
					frames = append(frames, chunk)
				}
			}
			if err := stream.Stop(); err != nil {
				return results, err
			}
			stopLive()
			frames = append(append([][]byte(nil), kept...), frames...)
			kept = nil
			r, err := RecordAndSend(frames, pane, client, 16000, print)
			var se *ServerError
			if errors.As(err, &se) {
				print(se.Msg)
				kept = frames
				print("Your recording is kept and goes out with the next one.")
				continue
			}
			if err != nil {
				return results, err
			}
			results = append(results, *r)
		}
	}
	return results, nil
}

// TailLine is the end of text that fits in width columns less one, so the
// line never wraps and a carriage return redraws it. Control characters
// become spaces, so a server's text cannot drive the terminal. A cut start
// is marked with an ellipsis and begins on a word.
func TailLine(text string, width int) string {
	var b strings.Builder
	space := false
	for _, r := range text {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == ' ' {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	rs := []rune(strings.TrimRight(b.String(), " "))
	max := width - 1
	if max < 2 {
		max = 2
	}
	if len(rs) <= max {
		return string(rs)
	}
	tail := rs[len(rs)-(max-1):]
	for i, r := range tail {
		if r == ' ' && i < len(tail)-1 {
			tail = tail[i+1:]
			break
		}
	}
	return "…" + string(tail)
}

// TokenFile is server.default_token_file.
func TokenFile(dataDir string) string { return filepath.Join(dataDir, "coppice", "web", "token") }

// ArmedDir is server.default_armed_dir.
func ArmedDir(dataDir string) string { return filepath.Join(dataDir, "voice", "armed") }

// ReadToken is server._read_token: the stripped text, or "" with ok false
// when the file is missing, unreadable or blank. A file that is not UTF-8
// is err: the oracle raises there.
func ReadToken(path string) (tok string, ok bool, err error) {
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return "", false, nil
	}
	text, xerr := pystr.DecodeStrict(raw)
	if xerr != nil {
		return "", false, errors.New(xerr.Msg)
	}
	t := pystr.Strip(text)
	return t, t != "", nil
}

// HTTPClient is ptt.HttpVoiceClient.
type HTTPClient struct {
	url   string
	token string
	http  *http.Client
	// quick waits a short time: a partial is worth its text only while the
	// owner still talks.
	quick *http.Client
}

// NewHTTPClient reads the token file under dataDir, as the oracle does,
// and refuses with a sentence when it cannot.
func NewHTTPClient(serverURL, dataDir string, timeout time.Duration) (*HTTPClient, error) {
	tf := TokenFile(dataDir)
	tok, ok, err := ReadToken(tf)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &ServerError{Msg: fmt.Sprintf("No token file at %s. Run coppice web token on the box, or pass --token-file.", tf)}
	}
	tr := &http.Transport{Proxy: nil}
	return &HTTPClient{url: strings.TrimRight(serverURL, "/"), token: tok,
		http: &http.Client{Timeout: timeout, Transport: tr}, quick: &http.Client{Timeout: 10 * time.Second, Transport: tr}}, nil
}

func (c *HTTPClient) post(path, ctype string, body []byte, deliver bool) (any, error) {
	return c.postWith(c.http, path, ctype, body, deliver)
}

func (c *HTTPClient) postWith(hc *http.Client, path, ctype string, body []byte, deliver bool) (any, error) {
	req, err := http.NewRequest("POST", c.url+path, bytes.NewReader(body))
	if err != nil {
		return nil, c.unreachable()
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := hc.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, &ServerError{Msg: noAnswerMsg}
		}
		return nil, c.unreachable()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, &ServerError{Msg: noAnswerMsg}
		}
		return nil, c.unreachable()
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		v, derr := loadsBytes(raw)
		if derr != nil {
			return nil, &ServerError{Msg: notJSONMsg}
		}
		return v, nil
	}
	var payload *pyjson.Object
	if v, derr := loadsBytes(raw); derr == nil {
		payload, _ = v.(*pyjson.Object)
	}
	if deliver && payload != nil {
		if _, has := payload.Get("delivered"); has {
			return payload, nil
		}
	}
	msg := fmt.Sprintf("The voice server answered %d.", resp.StatusCode)
	if payload != nil {
		m, _ := payload.Get("message")
		if !truthy(m) {
			m, _ = payload.Get("error")
		}
		if truthy(m) {
			msg = pyStr(m)
		}
	}
	busy := false
	if payload != nil && resp.StatusCode == 409 {
		e, _ := payload.Get("error")
		busy = pyEq(e, "busy")
	}
	return nil, &ServerError{Msg: msg, Busy: busy, Status: resp.StatusCode}
}

// The two replies a clip meets when the server is slow or sends no JSON. Both
// are server errors, so a push-to-talk clip that meets them is kept.
const (
	noAnswerMsg = "The voice server did not answer in time."
	notJSONMsg  = "The voice server sent an answer that is not JSON."
)

func (c *HTTPClient) unreachable() error {
	return &ServerError{Msg: fmt.Sprintf("Could not reach the voice server at %s. Run daisugi voice serve first.", c.url)}
}

// Transcribe posts a WAV to /transcribe.
func (c *HTTPClient) Transcribe(wav []byte) (any, error) {
	return c.post("/transcribe", "audio/wav", wav, false)
}

// Partial posts the recording so far to /partial and answers its text
// and the seq the server read.
func (c *HTTPClient) Partial(wav []byte, seq int) (string, int, error) {
	v, err := c.postWith(c.quick, "/partial?seq="+strconv.Itoa(seq), "audio/wav", wav, false)
	if err != nil {
		return "", 0, err
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return "", 0, &ServerError{Msg: notJSONMsg}
	}
	text, _ := o.Get("text")
	t, _ := text.(string)
	n := seq
	if raw, has := o.Get("seq"); has {
		if i, ok := raw.(pyjson.Int); ok {
			if x, err := strconv.Atoi(i.Text); err == nil {
				n = x
			}
		}
	}
	return t, n, nil
}

// Deliver posts a pane, a text and a mode to /deliver. A refused delivery,
// answered 403 with a delivered field, is a normal answer.
func (c *HTTPClient) Deliver(pane, text, mode string) (any, error) {
	o := pyjson.NewObject()
	o.Set("pane", pane)
	o.Set("text", text)
	o.Set("mode", mode)
	body, xerr := pystr.EncodeUTF8(pyjson.Dumps(o, true))
	if xerr != nil {
		return nil, errors.New(xerr.Msg)
	}
	return c.post("/deliver", "application/json", body, true)
}

// loadsBytes is json.loads(bytes) for a UTF-8 body.
func loadsBytes(raw []byte) (any, error) {
	text, err := decodeJSONBytes(raw)
	if err != nil {
		return nil, err
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		return nil, errors.New(derr.Msg)
	}
	return v, nil
}
