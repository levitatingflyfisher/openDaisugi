package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The CA certificate is public by nature, so this one path carries no
// token. Nothing else may ride along with it.
func TestCACertHandlerServesTheCertificateAndNothingElse(t *testing.T) {
	ts := httptest.NewServer(CACertHandler([]byte("-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n")))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-x509-ca-cert" {
		t.Errorf("content type is %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "coppice-ca.crt") {
		t.Errorf("content disposition is %q, so the phone will not save it as a file", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "BEGIN CERTIFICATE") {
		t.Errorf("body was %q", body)
	}

	for _, path := range []string{"/", "/api/panes", "/ca.key"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", path, resp.StatusCode)
		}
	}

	// A traversal has to be written by hand. http.Get resolves "/../ca.key"
	// against the base URL in the client and sends "GET /ca.key", so going
	// through the client would test nothing and claim it had.
	raw := rawRequestPath(t, ts.Listener.Addr().String(), "/../ca.key")
	if !strings.HasPrefix(raw, "HTTP/1.1 404") && !strings.HasPrefix(raw, "HTTP/1.1 400") {
		t.Errorf("a hand-written traversal got %q", strings.SplitN(raw, "\r\n", 2)[0])
	}
}

// rawRequestPath dials the listener and writes the request line itself, so
// the path reaches the handler exactly as typed.
func rawRequestPath(t *testing.T, addr, path string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", path)
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCACertURLPointsAtTheHandOffPort(t *testing.T) {
	if got := CACertURL("192.168.1.20", ":8080"); got != "http://192.168.1.20:8080/ca.crt" {
		t.Fatalf("CACertURL gave %q", got)
	}
	if got := CACertURL("box.tail1234.ts.net", "0.0.0.0:9000"); got != "http://box.tail1234.ts.net:9000/ca.crt" {
		t.Fatalf("CACertURL gave %q", got)
	}
}

// ServeCACert has to actually bind the listener and answer a real request
// over it, not just wire a handler nothing ever reaches. It also has to
// stop, and return nil, once its context ends.
func TestServeCACertBindsAndServesThenStopsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	pem := []byte("-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeCACert(ctx, addr, pem, nil)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/ca.crt")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("ServeCACert never answered a real request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "BEGIN CERTIFICATE") {
		t.Fatalf("served body was %q", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeCACert returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeCACert did not return after its context ended")
	}
}

func TestWriteQRDrawsBlocks(t *testing.T) {
	var sb strings.Builder
	WriteQR(&sb, "http://127.0.0.1:8080/ca.crt")
	out := sb.String()
	if !strings.ContainsAny(out, "█▀▄") {
		t.Fatalf("the QR had no block characters: %q", out)
	}
	if len(strings.Split(strings.TrimRight(out, "\n"), "\n")) < 10 {
		t.Fatal("the QR is too short to be a QR")
	}
}

func qrWidth(text string) int {
	var sb strings.Builder
	WriteQR(&sb, text)
	widest := 0
	for _, line := range strings.Split(strings.TrimRight(sb.String(), "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > widest {
			widest = n
		}
	}
	return widest
}

// The failure this names: a QR wider than the terminal wraps, and a wrapped
// QR is not a QR. Byte count is not the property that matters here; drawn
// width is. The URL code is small. The PEM code is the one at risk, and if
// it ever stops fitting, --qr pem has to go rather than print a scramble.
func TestBothQRCodesFitATerminal(t *testing.T) {
	if w := qrWidth("http://192.168.1.20:8080/ca.crt"); w > MaxQRColumns {
		t.Errorf("the URL QR is %d columns wide, over the %d budget", w, MaxQRColumns)
	}
	d, _ := initCA(t)
	pem, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	// An ECDSA P-256 root is roughly 670 bytes of PEM, which lands near 113
	// columns with the two-module quiet zone on each side.
	if w := qrWidth(string(pem)); w > MaxQRColumns {
		t.Errorf("the CA PEM QR is %d columns wide, over the %d budget. "+
			"Either shrink the certificate or drop --qr pem", w, MaxQRColumns)
	}
}
