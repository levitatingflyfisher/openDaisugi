package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"daisugi-verify/internal/pyjson"
)

func TestLoadsBytesDetectsEncodings(t *testing.T) {
	for _, b := range [][]byte{
		[]byte(`{"a": 1}`),
		append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"a": 1}`)...),
		{'{', 0, '"', 0, 'a', 0, '"', 0, ':', 0, '1', 0, '}', 0},
	} {
		v, err := LoadsBytes(b)
		if err != nil {
			t.Fatalf("%q: %v", b, err)
		}
		if o, ok := v.(*pyjson.Object); !ok || o.Len() != 1 {
			t.Fatalf("%q: %#v", b, v)
		}
	}
	if _, err := LoadsBytes([]byte{'"', 0xff, '"'}); err == nil {
		t.Fatal("invalid UTF-8 must not decode")
	}
	if _, err := LoadsBytes([]byte("\"\xed\xa0\x80\"")); err != nil {
		t.Fatalf("an encoded surrogate passes: %v", err)
	}
}

func TestPyIntReadsAsPythonReads(t *testing.T) {
	cases := map[string]string{" 12 ": "12", "1_000": "1000", "-7": "-7", "١٢": "12"}
	for in, want := range cases {
		n, err := pyInt(in)
		if err != nil || n.String() != want {
			t.Errorf("%q: %v %v", in, n, err)
		}
	}
	for _, bad := range []string{"1.5", "_1", "1__0", "", "x"} {
		if _, err := pyInt(bad); err == nil {
			t.Errorf("%q must raise", bad)
		}
	}
}

func TestSumFloatsIsCompensated(t *testing.T) {
	if got := pyjson.SumFloats([]float64{1e100, 1.0, -1e100, 1.0}); got != 2.0 {
		t.Fatalf("got %v", got)
	}
}

func body(t *testing.T, s string) any {
	v, err := LoadsBytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRouteTurnLadder(t *testing.T) {
	easy := body(t, `{"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hi"}]}`)
	d, err := RouteTurn(easy, DefaultCheapModel, "", nil)
	if err != nil || d.Tier != "tier1-cheap" || !d.Downgraded {
		t.Fatalf("%+v %v", d, err)
	}
	d, _ = RouteTurn(easy, DefaultCheapModel, "local", nil)
	if d.Tier != "tier1-local" || d.Model != "local" {
		t.Fatalf("%+v", d)
	}
	hard := body(t, `{"model": "m", "messages": [{"role": "user", "content": "design a schema and prove it"}]}`)
	if d, _ = RouteTurn(hard, DefaultCheapModel, "", nil); d.Tier != "tier2-frontier" || d.Downgraded {
		t.Fatalf("%+v", d)
	}
	big := body(t, `{"model": "m", "system": "`+strings.Repeat("s", 20000)+`", "messages": [{"role": "user", "content": "hi"}]}`)
	if d, _ = RouteTurn(big, DefaultCheapModel, "", nil); d.Tier != "tier2-frontier" {
		t.Fatalf("sticky prefix: %+v", d)
	}
	if d, _ = RouteTurn(big, DefaultCheapModel, "", DefaultCheapModel); d.Tier != "tier1-cheap" {
		t.Fatalf("sticky cheap: %+v", d)
	}
	if _, err := RouteTurn(body(t, `{"messages": [1]}`), DefaultCheapModel, "", nil); err == nil {
		t.Fatal("a message that is not a dict raises")
	}
}

// TestServerStreamsAndJournalsAfterDisconnect: a streamed turn reaches
// the client as the upstream sends it, and a client that leaves does not
// stop the turn from being read to its end and journaled.
func TestServerStreamsAndJournalsAfterDisconnect(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\": \"message_start\", \"message\": {\"usage\": {\"input_tokens\": 5}}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: {\"type\": \"message_delta\", \"usage\": {\"output_tokens\": 2}}\n\n")
	}))
	defer up.Close()
	dir := t.TempDir()
	journal := filepath.Join(dir, "turns.jsonl")
	gw, err := NewGateway(Gateway{JournalPath: journal})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Server{Anthropic: gw, UpstreamBase: up.URL, UpstreamKind: "anthropic", Client: NewClient(nil)})
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	b := `{"model": "claude-opus-4-8", "stream": true, "messages": [{"role": "user", "content": "design a schema"}]}`
	io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: "+strconv.Itoa(len(b))+"\r\n\r\n"+b)
	rd := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	seen := ""
	for !strings.Contains(seen, "message_start") {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("the first event did not arrive before the upstream finished: %v (%q)", err, seen)
		}
		seen += line
	}
	conn.Close()
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(journal); err == nil && strings.Contains(string(raw), `"output_tokens": 2`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the turn was not journaled after the client left")
}
