package llm

import (
	"errors"
	"io"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// The same bytes as tests/test_llm.py::test_request_bodies_are_compact_ascii_json.
func TestBodyBytes(t *testing.T) {
	w, e := ResolveWire("anthropic/m", "", "", env(map[string]string{"ANTHROPIC_API_KEY": "k"}))
	if e != nil {
		t.Fatal(e)
	}
	zero := 0
	msgs := []Message{{"system", "S é"}, {"user", "U 😀"}}
	got := string(Body(w, msgs, BodyOpts{MaxTokens: 200, Temperature: &zero}))
	want := `{"model":"m","max_tokens":200,"system":"S \u00e9","messages":[{"role":"user","content":"U \ud83d\ude00"}],"temperature":0}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	c, _ := ResolveWire("ollama/m", "", "", env(nil))
	got = string(Body(c, msgs, BodyOpts{MaxTokens: -1, JSONObject: true}))
	want = `{"model":"m","messages":[{"role":"system","content":"S \u00e9"},{"role":"user","content":"U \ud83d\ude00"}],"response_format":{"type":"json_object"}}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

// The same URLs as tests/test_llm.py::test_urls.
func TestURLs(t *testing.T) {
	for _, c := range [][3]string{
		{"anthropic/m", "", "https://api.anthropic.com/v1/messages"},
		{"anthropic/m", "http://h:1/", "http://h:1/v1/messages"},
		{"anthropic/m", "http://h:1/v1/messages/", "http://h:1/v1/messages"},
		{"anthropic/m", "http://h:1/V1/MESSAGES", "http://h:1/V1/MESSAGES/v1/messages"},
		{"openai/m", "", "https://api.openai.com/v1/chat/completions"},
		{"openai/m", "http://h:8080/v1", "http://h:8080/v1/chat/completions"},
		{"openai/m", "http://h:8080/v1/chat/completions", "http://h:8080/v1/chat/completions"},
		{"ollama/m", "", "http://localhost:11434/v1/chat/completions"},
		{"ollama/m", "http://h:11434/", "http://h:11434/v1/chat/completions"},
		{"ollama/m", "http://h:11434/v1", "http://h:11434/v1/chat/completions"},
	} {
		w, e := ResolveWire(c[0], c[1], "", env(map[string]string{"ANTHROPIC_API_KEY": "k"}))
		if e != nil || w.URL != c[2] {
			t.Errorf("%v: %v %v", c, w, e)
		}
	}
	if _, e := ResolveWire("gpt-4o", "", "", env(nil)); e == nil ||
		e.Msg != "no model wire for 'gpt-4o': name it anthropic/<model>, openai/<model> or ollama/<model>" {
		t.Fatal(e)
	}
	if _, e := ResolveWire("anthropic/m", "", "", env(map[string]string{"ANTHROPIC_API_KEY": ""})); e == nil || e.Msg != noKeyText {
		t.Fatal(e)
	}
}

func TestClean(t *testing.T) {
	got := Clean("at http://u:secret@h:1/x key sk-abcdefghijklmnopqrstuvwxyz0123")
	if got != "at http://h:1/x key sk-abcd...0123" {
		t.Fatal(got)
	}
}

func TestTimeout(t *testing.T) {
	for v, want := range map[string]float64{" 1.5 ": 1.5, "0": 600, "-1": 600, "nan": 600, "inf": 600, "x": 600} {
		if got := Timeout(env(map[string]string{"OPENDAISUGI_LLM_TIMEOUT": v})); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
}

func TestReadReply(t *testing.T) {
	c, _ := ResolveWire("ollama/m", "", "", env(nil))
	r, e := ReadReply(c, 200, `{"choices":[{"message":{"content":null},"finish_reason":"length"}]}`)
	if e != nil || r.Text != "" || !r.Cut {
		t.Fatal(r, e)
	}
	if _, e := ReadReply(c, 404, `{"error": "x"}`); e == nil || e.Msg != `the model server answered HTTP 404: {"error": "x"}` {
		t.Fatal(e)
	}
	if _, e := ReadReply(c, 200, `{"choices": []}`); e == nil || e.Msg != `the model reply could not be read: {"choices": []}` {
		t.Fatal(e)
	}
}

func TestResultText(t *testing.T) {
	if s, err := resultText(`{"type":"result","is_error":false,"result":"hi"}`); err != nil || s != "hi" {
		t.Fatal(s, err)
	}
	if _, err := resultText(`{"type":"result","is_error":true,"result":"no"}`); err == nil || err.Error() != "claude -p reported is_error: 'no'" {
		t.Fatal(err)
	}
	if _, err := resultText(`{"a":1}`); err == nil || err.Error() != `claude -p stdout was not its JSON envelope: '{"a":1}'` {
		t.Fatal(err)
	}
}

func TestRenamedBackendFailsTheCall(t *testing.T) {
	for _, v := range []string{"litellm", " litellm "} {
		c := New(Env{Getenv: env(map[string]string{"OPENDAISUGI_LLM_BACKEND": v, "ANTHROPIC_API_KEY": "sk-test"}),
			Home: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard})
		if err := c.Check("anthropic/x"); err != nil {
			t.Fatalf("%q: Check refused: %v", v, err)
		}
		_, err := c.Structured(Call{Model: "anthropic/x", User: "go"})
		var le *Error
		if !errors.As(err, &le) || le.Msg != RenamedText {
			t.Fatalf("%q: got %v", v, err)
		}
	}
}

func TestAutoBackendIsAPI(t *testing.T) {
	c := New(Env{Getenv: env(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
		Home: t.TempDir(), LookPath: func(string) (string, error) { return "", errors.New("no") }})
	if b := c.Backend(); b != "api" {
		t.Fatalf("got %q", b)
	}
}
