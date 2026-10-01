package web

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"sync"
)

// A speech.say reply carries one sentence of audio as base64 inside a
// JSON line. The socket sends it to the page as a binary frame instead, so
// the page gets audio bytes and no 33 percent of text to decode:
//
//	4 bytes   big-endian length N of the header
//	N bytes   {"event":"speech","id":<the request id>,"type":"audio/mpeg"}
//	the rest  the audio
//
// The frame goes only to the socket whose request it answers, since the
// reply comes back on that socket's own upstream session. A speech.say
// that fails stays a text line with the server's error.

// speechIDs are the ids of the speech.say requests one socket sent whose
// answers have not come. Only an answer to one of them is turned into a
// frame, so no other verb can make the socket send audio. The set is
// capped: a request that fails leaves its id until the cap clears it.
type speechIDs struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

const speechIDsMax = 256

// note reads one line the browser sent, and keeps the id when it is a
// speech.say request.
func (s *speechIDs) note(line []byte) {
	if !bytes.Contains(line, []byte("speech.say")) {
		return
	}
	var m struct {
		ID  string `json:"id"`
		Cmd string `json:"cmd"`
	}
	if json.Unmarshal(line, &m) != nil || m.Cmd != "speech.say" || m.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil || len(s.ids) >= speechIDsMax {
		s.ids = map[string]struct{}{}
	}
	s.ids[m.ID] = struct{}{}
}

// take is true once for an id that note kept.
func (s *speechIDs) take(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; !ok {
		return false
	}
	delete(s.ids, id)
	return true
}

// speechMarker is what every audio reply holds, checked before any parse so
// that no other line pays for one.
var speechMarker = []byte(`"audio":"`)

// speechFrame turns a speech.say reply line into the binary frame. ok is
// false for any other line, which goes to the page as it is.
func speechFrame(line []byte, ids *speechIDs) (frame []byte, ok bool) {
	if !bytes.Contains(line, speechMarker) {
		return nil, false
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		OK     bool            `json:"ok"`
		Result struct {
			Audio string `json:"audio"`
			Type  string `json:"type"`
			Bytes *int   `json:"bytes"`
		} `json:"result"`
	}
	if json.Unmarshal(line, &m) != nil || !m.OK || m.Result.Audio == "" || m.Result.Bytes == nil || len(m.ID) == 0 || m.ID[0] != '"' {
		return nil, false
	}
	audio, err := base64.StdEncoding.DecodeString(m.Result.Audio)
	if err != nil || len(audio) != *m.Result.Bytes {
		return nil, false
	}
	var id string
	if json.Unmarshal(m.ID, &id) != nil || !ids.take(id) {
		return nil, false
	}
	typ, _ := json.Marshal(m.Result.Type)
	head := append([]byte(`{"event":"speech","id":`), m.ID...)
	head = append(head, `,"type":`...)
	head = append(head, typ...)
	head = append(head, '}')
	frame = binary.BigEndian.AppendUint32(nil, uint32(len(head)))
	frame = append(frame, head...)
	return append(frame, audio...), true
}
