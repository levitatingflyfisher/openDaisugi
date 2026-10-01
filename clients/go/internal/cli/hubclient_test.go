package cli

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testHub() *hubClient {
	e := &Env{}
	return e.newHubClient()
}

// A redirect is not followed: the 3xx comes back and is refused (RF-7).
func TestHubDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			w.Write([]byte("[]"))
			return
		}
		http.Redirect(w, r, "/moved", http.StatusFound)
	}))
	defer srv.Close()
	r, err := testHub().do("GET", srv.URL+"/tree", nil)
	if err != nil || r.status != 302 {
		t.Fatalf("status %v, err %v", r, err)
	}
	var raise *hubRaise
	if err := raiseForStatus(r); err == nil || errors.As(err, &raise) {
		t.Fatalf("a redirect was not refused: %v", err)
	}
}

// Only a refused connection is httpcore's ConnectError; a connection that
// closes without an answer is refused, not guessed.
func TestHubNetworkFailures(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	_, err := testHub().do("GET", "http://"+addr+"/x", nil)
	var raise *hubRaise
	if !errors.As(err, &raise) || raise.Class != "httpcore.ConnectError" {
		t.Fatalf("refused: %v", err)
	}
	l, _ = net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, err = testHub().do("GET", "http://"+l.Addr().String()+"/x", nil)
	if err == nil || errors.As(err, &raise) {
		t.Fatalf("a closed connection: %v", err)
	}
}

// The download writes the body out as it comes; a 404 is not written.
func TestHubDownloadStreams(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 300_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Write(big)
	}))
	defer srv.Close()
	var out bytes.Buffer
	n, resp, err := testHub().download(srv.URL+"/f", &out)
	if err != nil || n != 300_000 || out.Len() != 300_000 || resp.StatusCode != 200 {
		t.Fatalf("n %d, err %v", n, err)
	}
	out.Reset()
	n, resp, err = testHub().download(srv.URL+"/missing", &out)
	if err != nil || n != 0 || out.Len() != 0 || resp.StatusCode != 404 {
		t.Fatalf("404: n %d, err %v", n, err)
	}
}

// A Retry-After in another script's digits, or too long to sleep, is
// refused.
func TestHubRetryAfterRefused(t *testing.T) {
	for _, v := range []string{"١", "99999999999999999999"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", v)
			w.WriteHeader(429)
		}))
		var log strings.Builder
		e := &Env{Stderr: &log}
		_, err := testHub().backoffGet(srv.URL+"/p", e)
		srv.Close()
		var raise *hubRaise
		if err == nil || errors.As(err, &raise) || !strings.Contains(err.Error(), "Retry-After") {
			t.Fatalf("%q: %v", v, err)
		}
	}
}
