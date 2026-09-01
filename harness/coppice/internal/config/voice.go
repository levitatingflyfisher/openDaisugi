package config

import (
	"errors"
	"fmt"
	"strings"
)

// Voice is the [voice] table. With no table the coppice server starts
// daisugi voice serve itself when daisugi is on PATH. enabled = false
// turns that off. url names a voice server that runs somewhere else, and
// the coppice server then starts none. token_file is the bearer token
// file that other server checks. With url set and no token_file, the
// web token file is sent, since that is what daisugi voice serve reads by
// default. args are added after the flags the coppice server passes to
// daisugi voice serve, for example ["--data-dir", "/some/dir"].
type Voice struct {
	Enabled   *bool    `toml:"enabled,omitempty"`
	URL       string   `toml:"url,omitempty"`
	TokenFile string   `toml:"token_file,omitempty"`
	Args      []string `toml:"args,omitempty"`
}

// VoiceOn is false only when the file says [voice] enabled = false.
func (c Config) VoiceOn() bool {
	return c.Voice.Enabled == nil || *c.Voice.Enabled
}

// DefaultTalk is ctrl-\, byte 0x1c, the key that records a voice clip
// when the file names no other key. No harness this floor runs binds it,
// and in raw mode it sends no signal.
const DefaultTalk byte = 0x1c

// takenKeys are the ctrl keys the floor itself answers to on the rail:
// ctrl-c quits, ctrl-t opens the talk line, ctrl-w stops an agent. ctrl-space
// is the default leave key, and the floor reads a zero talk key as the
// default talk key.
var takenKeys = map[byte]bool{0x00: true, 0x03: true, 0x14: true, 0x17: true}

// TalkKey is the byte that records a voice clip. An empty [keys] talk
// gives DefaultTalk. A key ParseKey refuses, the leave key, or a key the
// rail already uses is an error that names the file.
func (c Config) TalkKey(leave byte) (byte, error) {
	name := strings.TrimSpace(c.Keys.Talk)
	if name == "" {
		if DefaultTalk == leave {
			return 0, fmt.Errorf("[keys] leave in %s is ctrl-\\, the default talk key. Set [keys] talk to another ctrl key", Path())
		}
		return DefaultTalk, nil
	}
	b, err := ParseKey(name)
	if err == nil && (b == leave || takenKeys[b]) {
		err = errors.New("the talk key is one ctrl key that is not the leave key, ctrl-space, ctrl-c, ctrl-t or ctrl-w")
	}
	if err != nil {
		return 0, fmt.Errorf("bad [keys] talk %q in %s: %w", c.Keys.Talk, Path(), err)
	}
	return b, nil
}
