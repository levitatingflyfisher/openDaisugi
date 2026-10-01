package web

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func speechReply(req map[string]any) []map[string]any {
	switch req["cmd"] {
	case "pane.audio":
		// Not a speech.say: it must never become a frame.
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
			"type": "audio/mpeg", "bytes": 3, "audio": "AAAA"}}}
	case "speech.say":
		if req["text"] == "fail" {
			return []map[string]any{{"id": req["id"], "ok": false, "error": map[string]any{"code": "adapter_error", "message": "The speech server at x did not answer."}}}
		}
		audio := []byte("ID3 fake audio")
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
			"type": "audio/mpeg", "bytes": len(audio), "audio": base64.StdEncoding.EncodeToString(audio)}}}
	}
	return echoOK(req)
}

func readOne(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return typ, data
}

// A speech.say answer reaches the page as one binary frame with a header
// and the audio bytes. A failure and every other reply stay text.
func TestWSSpeechReplyIsABinaryFrame(t *testing.T) {
	_, d := newFakeServer(t, speechReply)
	ts, _, tok := newTestServer(t, Options{Dial: d})
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	c, _, err := websocket.Dial(context.Background(), wsAddr(ts.URL), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	send := func(line string) {
		if err := c.Write(context.Background(), websocket.MessageText, []byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	send(`{"id":"w1","cmd":"speech.say","text":"Hello."}`)
	typ, data := readOne(t, c)
	if typ != websocket.MessageBinary {
		t.Fatalf("type = %v, body %s", typ, data)
	}
	n := binary.BigEndian.Uint32(data[:4])
	var head map[string]any
	if err := json.Unmarshal(data[4:4+n], &head); err != nil {
		t.Fatal(err)
	}
	if head["event"] != "speech" || head["id"] != "w1" || head["type"] != "audio/mpeg" || len(head) != 3 {
		t.Fatalf("head = %v", head)
	}
	if string(data[4+n:]) != "ID3 fake audio" {
		t.Fatalf("audio = %q", data[4+n:])
	}

	send(`{"id":"w2","cmd":"speech.say","text":"fail"}`)
	typ, data = readOne(t, c)
	if typ != websocket.MessageText {
		t.Fatalf("a failure was %v", typ)
	}
	var msg map[string]any
	_ = json.Unmarshal(data, &msg)
	if msg["id"] != "w2" || msg["ok"] != false {
		t.Fatalf("failure = %s", data)
	}

	send(`{"id":"w3","cmd":"pane.list"}`)
	if typ, data = readOne(t, c); typ != websocket.MessageText {
		t.Fatalf("pane.list was %v %s", typ, data)
	}
	// A reply that looks like audio, to a verb that is not speech.say, is text.
	send(`{"id":"w4","cmd":"pane.audio"}`)
	if typ, data = readOne(t, c); typ != websocket.MessageText {
		t.Fatalf("pane.audio was %v %s", typ, data)
	}
}

func TestSpeechFrameLeavesOtherLinesAlone(t *testing.T) {
	for _, line := range []string{
		`{"id":"1","ok":true,"result":{"text":"the audio\":\"x"}}`,
		`{"id":"1","ok":true,"result":{"audio":"!!!","bytes":3}}`,
		`{"id":"1","ok":true,"result":{"audio":"AAAA","bytes":9,"type":"audio/mpeg"}}`,
		`{"id":7,"ok":true,"result":{"audio":"AAAA","bytes":3,"type":"audio/mpeg"}}`,
		`{"id":"1","ok":false,"result":{"audio":"AAAA","bytes":3,"type":"audio/mpeg"}}`,
		`not json "audio":"`,
	} {
		ids := &speechIDs{}
		ids.note([]byte(`{"id":"1","cmd":"speech.say","text":"x"}`))
		if f, ok := speechFrame([]byte(line), ids); ok {
			t.Fatalf("%s became a frame %q", line, f)
		}
	}
	// The id must be one a speech.say request used, once.
	line := `{"id":"1","ok":true,"result":{"audio":"AAAA","bytes":3,"type":"audio/mpeg"}}`
	if _, ok := speechFrame([]byte(line), &speechIDs{}); ok {
		t.Fatal("an answer to no speech.say became a frame")
	}
	ids := &speechIDs{}
	ids.note([]byte(`{"id":"1","cmd":"pane.list"}`))
	if _, ok := speechFrame([]byte(line), ids); ok {
		t.Fatal("an answer to another verb became a frame")
	}
	ids.note([]byte(`{"id":"1","cmd":"speech.say","text":"x"}`))
	if _, ok := speechFrame([]byte(line), ids); !ok {
		t.Fatal("an answer to speech.say did not")
	}
	if _, ok := speechFrame([]byte(line), ids); ok {
		t.Fatal("one request gave two frames")
	}
}
