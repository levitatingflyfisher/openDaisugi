// Command fake-upstream is a test instrument for gateway_compare.py
// --speed: an Anthropic-shaped upstream on 127.0.0.1 that answers every
// request at once with one fixed buffered message, so a timing measures
// the gateway and not the upstream.
package main

import (
	"io"
	"net/http"
	"os"
)

const reply = `{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-4-8", ` +
	`"content": [{"type": "text", "text": "ok"}], "usage": {"input_tokens": 10, "output_tokens": 2}}`

func main() {
	if len(os.Args) != 2 {
		os.Stderr.WriteString("usage: fake-upstream PORT\n")
		os.Exit(2)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, reply)
	})
	if err := http.ListenAndServe("127.0.0.1:"+os.Args[1], h); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}
