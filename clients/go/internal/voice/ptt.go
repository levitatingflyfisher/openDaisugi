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
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// ServerError is ptt.VoiceServerError: a one-line sentence for a person.
type ServerError struct{ Msg string }

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

// RunPTT is ptt.run_ptt: space starts a recording and stops it, q quits.
// keys yields one key at a time, and false at the end. A cycle the server
// fails prints its sentence and the session goes on.
func RunPTT(pane string, client Client, keys func() (string, bool), open func() (Stream, error),
	chunkFrames int, print func(string)) ([]Result, error) {
	var results []Result
	recording := false
	var stream Stream
	var frames [][]byte
	for {
		ch, ok := keys()
		if !ok {
			break
		}
		if ch == "q" {
			if recording && stream != nil {
				if err := stream.Stop(); err != nil {
					return results, err
				}
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
		} else if ch == " " && recording {
			recording = false
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
			r, err := RecordAndSend(frames, pane, client, 16000, print)
			var se *ServerError
			if errors.As(err, &se) {
				print(se.Msg)
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
		return nil, &ServerError{fmt.Sprintf("No token file at %s. Run coppice web token on the box, or pass --token-file.", tf)}
	}
	return &HTTPClient{url: strings.TrimRight(serverURL, "/"), token: tok,
		http: &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}}}, nil
}

func (c *HTTPClient) post(path, ctype string, body []byte, deliver bool) (any, error) {
	req, err := http.NewRequest("POST", c.url+path, bytes.NewReader(body))
	if err != nil {
		return nil, c.unreachable()
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, err
		}
		return nil, c.unreachable()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		v, derr := loadsBytes(raw)
		if derr != nil {
			return nil, derr
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
	return nil, &ServerError{msg}
}

func (c *HTTPClient) unreachable() error {
	return &ServerError{fmt.Sprintf("Could not reach the voice server at %s. Run daisugi voice serve first.", c.url)}
}

// Transcribe posts a WAV to /transcribe.
func (c *HTTPClient) Transcribe(wav []byte) (any, error) {
	return c.post("/transcribe", "audio/wav", wav, false)
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
