package gate

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

type fakeReq struct {
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// fakeAnthropic answers every POST with status and body, raw, and logs
// what it got as the oracle's fake logs it (Python's http.server folds a
// run of leading slashes in the path into one).
func fakeAnthropic(t *testing.T) (*httptest.Server, func(int, string), func() []fakeReq) {
	var mu sync.Mutex
	status, body := 200, ""
	var log []fakeReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		path := r.RequestURI
		if strings.HasPrefix(path, "//") {
			path = "/" + strings.TrimLeft(path, "/")
		}
		h := map[string]string{}
		for k, v := range r.Header {
			switch k = strings.ToLower(k); k {
			case "host", "content-length", "connection", "accept-encoding":
			default:
				h[k] = v[0]
			}
		}
		mu.Lock()
		log = append(log, fakeReq{Path: path, Headers: h, Body: string(raw)})
		st, b := status, body
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(st)
		io.WriteString(w, b)
	}))
	t.Cleanup(srv.Close)
	set := func(st int, b string) {
		mu.Lock()
		status, body, log = st, b, nil
		mu.Unlock()
	}
	got := func() []fakeReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]fakeReq{}, log...)
	}
	return srv, set, got
}

func TestLLMCheckMissingKey(t *testing.T) {
	r := &runner{env: map[string]string{"PATH": "/usr/bin:/bin", "OPENDAISUGI_LLM_BACKEND": "litellm"}, home: "/home/user"}
	res := r.runLLMCheck("be nice", pyjson.NewObject())
	if !res.errored || res.reason != "error: llm_check call failed: "+llmMissingKey {
		t.Fatalf("got %+v", res)
	}
}

// TestLLMCheckAgainstOracle replays testdata/llm_oracle.py's rows from
// DAISUGI_LLM_ORACLE against a fake Anthropic server: the same requests
// and the same result.
func TestLLMCheckAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_LLM_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_LLM_ORACLE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	srv, set, got := fakeAnthropic(t)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	n, bad := 0, 0
	for sc.Scan() {
		rowV, err := pyjson.Loads(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		row := rowV.(*pyjson.Object)
		var plain struct {
			Status   int       `json:"status"`
			Body     string    `json:"body"`
			Requests []fakeReq `json:"requests"`
			Result   struct {
				Satisfied bool   `json:"satisfied"`
				Reason    string `json:"reason"`
				Errored   bool   `json:"errored"`
			} `json:"result"`
		}
		// The payload may hold NaN, which encoding/json refuses; the rest
		// is plain JSON.
		rest := pyjson.NewObject()
		for _, k := range []string{"status", "body", "requests", "result"} {
			rest.Set(k, row.Value(k))
		}
		if err := json.Unmarshal([]byte(pyjson.Dumps(rest, true)), &plain); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/home/user"}
		envObj := row.Value("env").(*pyjson.Object)
		for _, k := range envObj.Keys() {
			env[k] = strings.ReplaceAll(envObj.Value(k).(string), "{BASE}", srv.URL)
		}
		set(plain.Status, row.Value("body").(string))
		r := &runner{env: env, home: "/home/user"}
		var res llmResult
		why := ""
		func() {
			defer func() {
				if p := recover(); p != nil {
					if u, ok := p.(unportedCall); ok {
						why = u.reason
						return
					}
					panic(p)
				}
			}()
			res = r.runLLMCheck(row.Value("rule").(string), row.Value("payload").(*pyjson.Object))
		}()
		n++
		reqs := got()
		for i := range reqs {
			reqs[i].Body = strings.ReplaceAll(reqs[i].Body, srv.URL, "{BASE}")
		}
		reason := strings.ReplaceAll(res.reason, srv.URL, "{BASE}")
		// encoding/json reads a lone surrogate escape as U+FFFD.
		rr := pystr.Runes(reason)
		for i, c := range rr {
			if pystr.IsSurrogate(c) {
				rr[i] = 0xFFFD
			}
		}
		reason = string(rr)
		gj, _ := json.Marshal(map[string]any{"why": why, "sat": res.satisfied, "reason": reason, "err": res.errored, "reqs": reqs})
		wj, _ := json.Marshal(map[string]any{"why": "", "sat": plain.Result.Satisfied, "reason": plain.Result.Reason,
			"err": plain.Result.Errored, "reqs": plain.Requests})
		if string(gj) != string(wj) {
			bad++
			if bad < 15 {
				t.Errorf("row %d (%d %.60q):\n got %.1500s\nwant %.1500s", n, plain.Status, plain.Body, gj, wj)
			}
		}
	}
	t.Logf("%d rows, %d differ", n, bad)
}
