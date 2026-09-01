package proto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// MaxLine caps one wire line. A client that sends more is refused rather than
// allowed to grow the server's heap without bound.
const MaxLine = 1 << 20 // 1 MiB

type Decoder struct {
	r *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns one line without its newline. An over-long line is an error and
// the decoder is finished, because the stream is no longer framed.
func (d *Decoder) Next() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := d.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > MaxLine {
			return nil, fmt.Errorf("line is over %d bytes, send a smaller request", MaxLine)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// Encoder writes one JSON value per line. Send is safe for concurrent use: the
// frame pump, the state pump and the request handler all write to one client.
type Encoder struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: bufio.NewWriterSize(w, 64<<10)} }

func (e *Encoder) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return e.SendBytes(b)
}

// SendBytes writes one already-encoded line and its newline under the same
// mutex Send uses. EncodeResponse and EncodeEvent produce its input.
func (e *Encoder) SendBytes(b []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(b); err != nil {
		return err
	}
	if err := e.w.WriteByte('\n'); err != nil {
		return err
	}
	return e.w.Flush()
}
