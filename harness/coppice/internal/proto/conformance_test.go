package proto_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/server"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// corpusDir holds the protocol corpus. Its README.md states the format.
var corpusDir = filepath.Join("..", "..", "testdata", "protocol")

// TestConformanceCorpusReplays sends every corpus file to a real server over
// its unix socket and checks each reply against the expected line. A corpus
// with no files fails, because a test that sends nothing proves nothing.
func TestConformanceCorpusReplays(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(corpusDir, "*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no corpus files, the test would prove nothing")
	}
	s := newConformanceServer(t)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			replayCorpus(t, s, f)
		})
	}
}

// TestProtocolDocumentCarriesTheVersion keeps PROTOCOL.md and proto.Version
// on one number. Two answers to "which protocol is this" is worse than one.
func TestProtocolDocumentCarriesTheVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "Version: " + jsonText(proto.Version)
	if !strings.Contains(string(b), want) {
		t.Fatalf("PROTOCOL.md does not carry %q", want)
	}
}

// TestCorpusReadmeStatesTheFormat fails when the corpus format has no
// written rules. The Python replay copies them from that file.
func TestCorpusReadmeStatesTheFormat(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(corpusDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"*"`, `"$id"`, `"$name"`, "#skip"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("testdata/protocol/README.md does not explain %s", want)
		}
	}
}

// conformanceServer is one real Server on a unix socket. Each corpus file
// opens its own connection to it.
type conformanceServer struct {
	socket string
}

func newConformanceServer(t *testing.T) *conformanceServer {
	t.Helper()
	// The corpus starts pty panes, which render through libghostty. Without
	// it this test skips with the same reason every pane test gives.
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	// A unix socket path is limited to about 100 bytes. A long TMPDIR gets
	// a plain refusal here, not a confusing bind error from the kernel.
	if len(sock) > 100 {
		t.Fatalf("socket path %q is over 100 bytes. Set TMPDIR to a short directory.", sock)
	}
	s, err := server.New(server.Config{SocketPath: sock, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	return &conformanceServer{socket: sock}
}

// replayCorpus plays one file on one connection. Odd lines are requests, even
// lines are expected replies. The matching rules live in conformance_match.go
// and are stated in testdata/protocol/README.md.
func replayCorpus(t *testing.T, s *conformanceServer, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", s.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	m := proto.NewMatcher()
	var lines [][]byte
	for _, l := range bytes.Split(raw, []byte("\n")) {
		l = bytes.TrimSpace(l)
		if len(l) == 0 {
			continue
		}
		if bytes.HasPrefix(l, []byte("#")) && !bytes.HasPrefix(l, []byte(proto.SkipMarker)) {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines)%2 != 0 {
		t.Fatalf("%s has %d lines. Every request needs one expected line.", path, len(lines))
	}
	skipped := 0
	for i := 0; i+1 < len(lines); i += 2 {
		reqLine, expLine := lines[i], lines[i+1]
		caseNo := i/2 + 1
		if bytes.HasPrefix(expLine, []byte(proto.SkipMarker)) {
			skipped++
			t.Logf("%s case %d: skipped, a listed gap: %s", filepath.Base(path), caseNo,
				strings.TrimSpace(strings.TrimPrefix(string(expLine), proto.SkipMarker)))
			continue
		}
		var req any
		if err := json.Unmarshal(reqLine, &req); err != nil {
			t.Fatalf("%s case %d: request line is not JSON: %v", path, caseNo, err)
		}
		req, err = m.Substitute(req)
		if err != nil {
			t.Fatalf("%s case %d: %v", path, caseNo, err)
		}
		reqID := proto.RequestID(req)
		out, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(append(out, '\n')); err != nil {
			t.Fatalf("%s case %d: write: %v", path, caseNo, err)
		}
		actual := readResponse(t, conn, reader, path, caseNo)
		var expected any
		if err := json.Unmarshal(expLine, &expected); err != nil {
			t.Fatalf("%s case %d: expected line is not JSON: %v", path, caseNo, err)
		}
		if err := m.Match(expected, actual, reqID); err != nil {
			t.Errorf("%s case %d: %v\nrequest:  %s\nexpected: %s\nactual:   %s",
				filepath.Base(path), caseNo, err, out, expLine, jsonText(actual))
		}
	}
	if skipped > 0 {
		t.Logf("%s: %d cases skipped. A skip is a listed gap, never a pass.", filepath.Base(path), skipped)
	}
}

// readResponse reads lines until one carries an id key. A line with no id is
// an event or a notification and is skipped.
func readResponse(t *testing.T, conn net.Conn, r *bufio.Reader, path string, caseNo int) any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("%s case %d: no response: %v", path, caseNo, err)
		}
		var v any
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatalf("%s case %d: server sent a line that is not JSON: %q", path, caseNo, line)
		}
		if proto.HasID(v) {
			return v
		}
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
