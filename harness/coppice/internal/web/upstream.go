package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// MaxLineBytes bounds one reply line read from the server. A full 120x40
// frame is roughly 150 KB of JSON, so bufio.Scanner's 64 KB default would
// split frames and hand the browser half a line. 4 MiB leaves room for a
// scrollback read as well.
const MaxLineBytes = 4 << 20

// MaxRequestBytes bounds one outbound request line, counting the newline
// this package appends. It mirrors proto.MaxLine, the server's own inbound
// line limit: a line at or over it is refused here, before anything reaches
// the wire, rather than written and then silently dropped by the server's
// decoder with no reply at all.
const MaxRequestBytes = 1 << 20

// Dialer is the only thing this package knows about coppice-server. Tests
// supply a fake; the running binary supplies a unix socket.
type Dialer interface {
	Dial(ctx context.Context) (io.ReadWriteCloser, error)
}

// UnixDialer dials the coppice-server socket at Path. The path itself comes
// from the caller; this package never guesses it.
type UnixDialer struct{ Path string }

func (d UnixDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	var dl net.Dialer
	return dl.DialContext(ctx, "unix", d.Path)
}

// Session is one client's connection to coppice-server. One websocket owns
// exactly one Session, which is what makes a browser client the same kind of
// client as a terminal one. Send guards its write with a mutex, so two
// callers sending at once cannot interleave their lines on the wire.
type Session struct {
	up     io.ReadWriteCloser
	lines  chan []byte
	done   chan struct{}
	mu     sync.Mutex
	err    error
	once   sync.Once
	sendMu sync.Mutex
}

// Open dials the server and starts reading its replies in the background.
func Open(ctx context.Context, d Dialer) (*Session, error) {
	up, err := d.Dial(ctx)
	if err != nil {
		return nil, err
	}
	s := &Session{up: up, lines: make(chan []byte, 64), done: make(chan struct{})}
	go s.read()
	return s, nil
}

func (s *Session) read() {
	defer close(s.lines)
	sc := bufio.NewScanner(s.up)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		select {
		case s.lines <- line:
		case <-s.done:
			return
		}
	}
	s.setErr(sc.Err())
}

func (s *Session) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Err is the reader's first error, if any. Close unblocks a pending read by
// closing the connection, so Err is normally non-nil once Close has run: that
// is the reader noticing its own connection closed, not a fault.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Send writes one JSONL line. The newline is added here; the caller passes
// the object bytes exactly as they should reach the server. A line at or
// over MaxRequestBytes is refused before anything is written, rather than
// sent and then dropped by the server with no reply.
func (s *Session) Send(line []byte) error {
	out := make([]byte, 0, len(line)+1)
	out = append(out, line...)
	out = append(out, '\n')
	if len(out) >= MaxRequestBytes {
		return fmt.Errorf("that request line is %d bytes, at or over the server's %d byte limit", len(out), MaxRequestBytes)
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	_, err := s.up.Write(out)
	return err
}

// Lines yields one element per JSONL line from the server, newline
// stripped. It closes when the connection ends.
func (s *Session) Lines() <-chan []byte { return s.lines }

// Close ends the session. It is safe to call more than once.
func (s *Session) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.up.Close()
	})
	return err
}

var callSeq atomic.Uint64

// Call runs one request over a fresh connection and returns the reply that
// echoes its id. Every /api handler runs rarely compared to the websocket,
// so a connection per call is the right trade for that path.
func Call(ctx context.Context, d Dialer, req map[string]any) (map[string]any, error) {
	s, err := Open(ctx, d)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	id := "api-" + strconv.FormatUint(callSeq.Add(1), 10)
	out := make(map[string]any, len(req)+1)
	for k, v := range req {
		out[k] = v
	}
	out["id"] = id
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if err := s.Send(body); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("coppice-server did not answer in 5 s")
		case line, ok := <-s.Lines():
			if !ok {
				if err := s.Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("coppice-server closed the connection")
			}
			var msg map[string]any
			if json.Unmarshal(line, &msg) != nil {
				continue
			}
			if msg["id"] != id {
				continue
			}
			if okField, _ := msg["ok"].(bool); !okField {
				return nil, refusal(msg)
			}
			return msg, nil
		}
	}
}

// RefusedError is a coppice-server refusal, carried out to the caller with
// its closed enum code intact.
type RefusedError struct {
	Code    string
	Message string
}

func (e *RefusedError) Error() string {
	if e.Code == "" {
		return "coppice-server refused the request"
	}
	return e.Code + ": " + e.Message
}

// refusal turns {"ok":false,"error":{"code","message"}} into an error.
// Returning that reply as a success is how a floor ends up showing an empty
// roster on top of a real failure, and this package never guesses like that.
func refusal(msg map[string]any) error {
	body, _ := msg["error"].(map[string]any)
	code, _ := body["code"].(string)
	message, _ := body["message"].(string)
	if code == "" && message == "" {
		return &RefusedError{Code: "internal", Message: "coppice-server refused and said nothing"}
	}
	return &RefusedError{Code: code, Message: message}
}
