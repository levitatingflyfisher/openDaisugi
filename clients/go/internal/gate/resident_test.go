package gate

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"daisugi-verify/internal/pyjson"
)

func residentVectors(t *testing.T) (b64 [][]any, argp [][]any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/resident_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		B64  [][]any `json:"b64decode"`
		Argp [][]any `json:"hook_report_argv"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.B64, v.Argp
}

func TestB64DecodeLenientAgreesWithPython(t *testing.T) {
	vs, _ := residentVectors(t)
	for _, v := range vs {
		text, want, ok := v[0].(string), v[1].(string), v[2].(bool)
		got, gotOK := b64decodeLenient(text)
		if gotOK != ok || (ok && hex.EncodeToString(got) != want) {
			t.Errorf("b64decode(%q) = %x, %v; Python %s, %v", text, got, gotOK, want, ok)
		}
	}
}

func TestHookReportArgvAgreesWithPython(t *testing.T) {
	_, vs := residentVectors(t)
	for _, v := range vs {
		var argv []string
		for _, a := range v[0].([]any) {
			argv = append(argv, a.(string))
		}
		ok := v[3].(bool)
		pane, root, gotOK := parseHookReportArgv(argv)
		if gotOK != ok {
			t.Errorf("%q: ok %v, Python %v", argv, gotOK, ok)
			continue
		}
		if !ok {
			continue
		}
		if w, _ := v[1].(string); (pane == nil) != (v[1] == nil) || (pane != nil && *pane != w) {
			t.Errorf("%q: pane %v, Python %v", argv, pane, v[1])
		}
		// The parser stores root as a Path.
		if w, _ := v[2].(string); (root == nil) != (v[2] == nil) || (root != nil && *root != pathStr(w)) {
			t.Errorf("%q: root %v, Python %v", argv, root, v[2])
		}
	}
}

func TestParseRequest(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		argv []string
		in   string
	}{
		{"plain", `{"v": 1, "argv": ["--format", "pi"], "stdin_b64": "e30="}` + "\n", true, []string{"--format", "pi"}, "{}"},
		{"no stdin", `{"argv": []}`, true, nil, ""},
		{"argv string", `{"argv": "ab"}`, true, []string{"a", "b"}, ""},
		{"argv object", `{"argv": {"--x": 1, "y": 2}}`, true, []string{"--x", "y"}, ""},
		{"argv scalars", `{"argv": [true, null, 1, 1.5, -0.0, 1e100, [1, "a"], {"k": null}]}`, true,
			[]string{"True", "None", "1", "1.5", "-0.0", "1e+100", "[1, 'a']", "{'k': None}"}, ""},
		{"argv number", `{"argv": 5}`, false, nil, ""},
		{"argv null", `{"argv": null}`, false, nil, ""},
		{"no argv", `{"v": 1}`, false, nil, ""},
		{"list", `[1]`, false, nil, ""},
		{"stdin number", `{"argv": [], "stdin_b64": 5}`, false, nil, ""},
		{"stdin null", `{"argv": [], "stdin_b64": null}`, false, nil, ""},
		{"stdin non ascii", `{"argv": [], "stdin_b64": "é"}`, false, nil, ""},
		{"stdin bad padding", `{"argv": [], "stdin_b64": "abc"}`, false, nil, ""},
		{"bom", "\xef\xbb\xbf" + `{"argv": []}`, true, nil, ""},
		{"utf16 bom", "\xff\xfe{\x00}\x00", false, nil, ""},
		{"invalid utf8", `{"argv": [], "x": "` + "\xff" + `"}`, false, nil, ""},
		{"encoded surrogate", `{"argv": ["` + "\xed\xb2\x80" + `"]}`, true, []string{"\xed\xb2\x80"}, ""},
		{"nan", `{"argv": [], "x": NaN}`, true, nil, ""},
		{"deep ok", `{"argv": [], "x": ` + strings.Repeat("[", 9991) + strings.Repeat("]", 9991) + `}`, true, nil, ""},
		{"too deep", `{"argv": [], "x": ` + strings.Repeat("[", 9992) + strings.Repeat("]", 9992) + `}`, false, nil, ""},
		{"empty", "", false, nil, ""},
	}
	for _, c := range cases {
		q, ok := ParseRequest([]byte(c.line))
		if ok != c.ok {
			t.Errorf("%s: ok %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if strings.Join(q.Argv, "\x00") != strings.Join(c.argv, "\x00") || string(q.Stdin) != c.in {
			t.Errorf("%s: argv %q stdin %q, want %q %q", c.name, q.Argv, q.Stdin, c.argv, c.in)
		}
	}
}

func TestParseRequestCallerFieldsAreStringsOnly(t *testing.T) {
	q, ok := ParseRequest([]byte(`{"argv": [], "coppice_sock": "/s", "coppice_pane": 3, "herdr_pane": null, "coppice_data_dir": "/d"}`))
	if !ok {
		t.Fatal("not parsed")
	}
	if q.Caller.Sock == nil || *q.Caller.Sock != "/s" || q.Caller.Pane != nil || q.Caller.HerdrPane != nil ||
		q.Caller.DataDir == nil || *q.Caller.DataDir != "/d" {
		t.Errorf("caller %+v", q.Caller)
	}
}

func TestReplyLineIsPythonsJSON(t *testing.T) {
	got := string(ReplyLine("{\"continue\": true}", "é\xed\xb2\x80", 2))
	want := `{"v": 1, "stdout": "{\"continue\": true}", "stderr": "` + "\\u00e9\\udc80" + `", "exit_code": 2}` + "\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if string(BadRequest) != `{"v": 1, "stdout": "", "stderr": "openDaisugi gate: DENIED `+"\\u2014"+` bad request", "exit_code": 2}`+"\n" {
		t.Errorf("bad request %q", BadRequest)
	}
}

func TestRequestRoundTripsThroughTheChild(t *testing.T) {
	sock, pid := "/s", 42
	q := &Request{Argv: []string{"a\x00b", "\xed\xb2\x80", "é"}, Stdin: []byte{0, 0xff, '{'},
		Caller: Caller{Sock: &sock, PeerPid: &pid}}
	back, err := decodeRequest(encodeRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(back.Argv, "|") != strings.Join(q.Argv, "|") || string(back.Stdin) != string(q.Stdin) ||
		back.Caller.Sock == nil || *back.Caller.Sock != "/s" || back.Caller.Pane != nil ||
		back.Caller.PeerPid == nil || *back.Caller.PeerPid != 42 {
		t.Errorf("round trip: %+v", back)
	}
}

func TestChildEnvironDropsTheResidentMarker(t *testing.T) {
	for _, kv := range ChildEnviron([]string{"A=1", ResidentEnv + "=1"}) {
		if strings.HasPrefix(kv, ResidentEnv+"=") {
			t.Fatalf("a host's %s reached the child", ResidentEnv)
		}
	}
}

func TestHelpIsNoReply(t *testing.T) {
	q := &Request{Argv: []string{"--help"}}
	res := RunResident(q, []string{"HOME=/home/user"})
	if !res.Help {
		t.Fatalf("--help: %+v", res)
	}
}

func TestResidentOutcomeTextKeepsSurrogates(t *testing.T) {
	q := &Request{Argv: []string{"--mode", "enforce", "--format", "bogus\xed\xb2\x80", "--root", t.TempDir()},
		Stdin: []byte(`{}`)}
	res := RunResident(q, []string{"HOME=/home/user"})
	if res.Exit != 2 || !strings.Contains(res.OutStderr, "'bogus\\udc80'") || strings.HasSuffix(res.OutStderr, "\n") {
		t.Errorf("%+v", res)
	}
}

func TestValidateHookReportRow(t *testing.T) {
	cases := map[string]string{
		`{"v": 1, "ts": 1, "session_id": "s", "harness": "pi", "state": "idle", "source": "headless"}`:      "",
		`{"ts": 1, "session_id": "s", "harness": "pi", "state": "idle"}`:                                    "event missing required field 'source'",
		`{"v": true, "ts": 1, "session_id": "s", "harness": "pi", "state": "idle", "source": "headless"}`:   "invalid schema version True",
		`{"v": 1, "ts": 1, "session_id": "s", "harness": "pi", "state": "nap", "source": "headless"}`:       "unknown state 'nap'",
		`{"ts": false, "session_id": "s", "harness": "pi", "state": "idle", "source": "headless"}`:          "ts must be a number",
		`{"ts": 1, "session_id": "s", "harness": "pi", "state": "done", "source": "manifest"}`:              "state 'done' may only come from source 'process' or 'headless'",
		`{"ts": 1, "session_id": "s", "harness": "pi", "state": "idle", "source": "headless", "v": 3}`:      "unsupported event schema version 3",
		`{"ts": 1, "session_id": "s", "harness": "pi", "state": "idle", "source": "headless", "ask": null}`: "",
	}
	for text, want := range cases {
		v, err := pyjson.Loads(text)
		if err != nil {
			t.Fatal(err)
		}
		if got := validateHookReportRow(v.(*pyjson.Object)); got != want {
			t.Errorf("%s: %q, want %q", text, got, want)
		}
	}
}
