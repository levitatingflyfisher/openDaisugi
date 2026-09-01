package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// recorder is one program that records a 16 kHz mono WAV clip to a file
// until it gets an interrupt.
type recorder struct {
	name string
	args func(path string) []string
}

// recorders are the programs the talk key records with, in the order it
// looks for them on PATH.
var recorders = []recorder{
	{"pw-record", func(p string) []string {
		return []string{"--rate", "16000", "--channels", "1", "--format", "s16", p}
	}},
	{"parecord", func(p string) []string {
		return []string{"--rate=16000", "--channels=1", "--format=s16le", "--file-format=wav", p}
	}},
	{"arecord", func(p string) []string {
		return []string{"-q", "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "wav", p}
	}},
}

// lookPath finds a recorder. Tests replace it.
var lookPath = exec.LookPath

// noRecorder is the line the floor shows when no recorder is on PATH.
const noRecorder = "Voice needs a recorder: pw-record, parecord or arecord. Install pipewire-utils (Fedora), " +
	"pipewire-bin (Debian, Ubuntu), pulseaudio-utils or alsa-utils; check with: command -v pw-record parecord arecord"

// findRecorder returns the path and the arguments of the first recorder
// on PATH, for a clip written to clipPath.
func findRecorder(clipPath string) (string, []string, bool) {
	for _, r := range recorders {
		if p, err := lookPath(r.name); err == nil {
			return p, r.args(clipPath), true
		}
	}
	return "", nil, false
}

// clip is one recording in progress.
type clip struct {
	cmd  *exec.Cmd
	path string
	done chan struct{}
}

// staleClip is how old a clip file must be before a new clip deletes it.
// No clip is recorded and heard for this long.
const staleClip = 10 * time.Minute

// startClip starts the first recorder on PATH, writing to a new file
// under dir/voice. dir is the coppice data directory, never the system
// temp directory, which can be RAM.
func startClip(dir string) (*clip, error) {
	if dir == "" {
		return nil, errors.New("Voice needs the coppice data directory, and this floor has none")
	}
	voiceDir := filepath.Join(dir, "voice")
	if err := os.MkdirAll(voiceDir, 0o700); err != nil {
		return nil, fmt.Errorf("Voice cannot make %s: %v", voiceDir, err)
	}
	// A floor that closed while a clip was being heard leaves that clip
	// behind. Clips older than staleClip are such leftovers.
	if old, _ := filepath.Glob(filepath.Join(voiceDir, "clip-*.wav")); len(old) > 0 {
		for _, p := range old {
			if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > staleClip {
				os.Remove(p)
			}
		}
	}
	f, err := os.CreateTemp(voiceDir, "clip-*.wav")
	if err != nil {
		return nil, fmt.Errorf("Voice cannot write a clip in %s: %v", voiceDir, err)
	}
	path := f.Name()
	f.Close()
	bin, args, ok := findRecorder(path)
	if !ok {
		os.Remove(path)
		return nil, errors.New(noRecorder)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	setRecorderProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("%s did not start: %v", filepath.Base(bin), err)
	}
	c := &clip{cmd: cmd, path: path, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// end interrupts the recorder, so it finishes the WAV header, and kills
// it when it has not ended within two seconds.
func (c *clip) end() {
	_ = c.cmd.Process.Signal(os.Interrupt)
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// stop ends the recording and returns the clip's bytes. The file is gone
// after.
func (c *clip) stop() ([]byte, error) {
	c.end()
	defer os.Remove(c.path)
	return os.ReadFile(c.path)
}

// discard ends the recording and throws the clip away.
func (c *clip) discard() {
	c.end()
	os.Remove(c.path)
}

// minClipBytes is a WAV header and about a quarter second of 16 kHz mono
// 16-bit audio. A shorter clip holds no words.
const minClipBytes = 44 + 16000*2/4

// voiceClient sends one clip. A slow CPU model on a long clip takes a
// while, and a dead server must not hang the floor.
var voiceClient = &http.Client{Timeout: 120 * time.Second}

// transcribe posts wav to the voice server at base with the bearer token
// and returns the text it heard.
func transcribe(base, token string, wav []byte) (string, error) {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/transcribe?cleanup=1",
		bytes.NewReader(wav))
	if err != nil {
		return "", fmt.Errorf("The voice server address %s is not valid", base)
	}
	req.Header.Set("Content-Type", "audio/wav")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := voiceClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Could not reach the voice server at %s", base)
	}
	defer resp.Body.Close()
	var body struct {
		Text    string `json:"text"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSuffix(body.Message, ".")
		if msg == "" {
			msg = fmt.Sprintf("The voice server answered %d", resp.StatusCode)
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(body.Text), nil
}

// cleanTranscript makes a transcript safe to type into a pty. Text typed
// there is keys: a CR or LF is Enter and an ESC starts a sequence that
// drives the agent's screen. CR, LF and Tab become a space. Every other C0
// byte, DEL, ESC, the C1 range U+0080 to U+009F and bytes that are not
// UTF-8 are dropped. Runs of space become one, and the ends are trimmed.
func cleanTranscript(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// voiceTarget is where a clip goes: the voice server's address and the
// token it checks.
type voiceTarget struct {
	url, token string
}

// voiceReady asks the coppice server whether voice can run. When it
// cannot, it asks the server to start voice, and returns the line that
// says why and what fixes it, without the words that say how to try
// again.
func voiceReady(socket string) (*voiceTarget, string) {
	res, err := callWithin(socket, "voice.status", nil, 5*time.Second)
	if err != nil {
		return nil, fmt.Sprintf("Voice status is not known: %v", strings.TrimSuffix(err.Error(), "."))
	}
	if ready, _ := res["ready"].(bool); !ready {
		if st, _ := res["state"].(string); st == "idle" || st == "down" {
			if again, err := callWithin(socket, "voice.start", nil, 5*time.Second); err == nil {
				res = again
			}
		}
		reason, _ := res["reason"].(string)
		fix, _ := res["fix"].(string)
		if fix == "" {
			return nil, strings.TrimSuffix(reason, ".")
		}
		return nil, reason + " " + fix
	}
	url, _ := res["url"].(string)
	tok := ""
	if f, _ := res["token_file"].(string); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, "Voice's token file cannot be read"
		}
		tok = strings.TrimSpace(string(raw))
	}
	return &voiceTarget{url: url, token: tok}, ""
}

// voiceResult is what one clip came to: the text for pane, or the line
// that says why there is none.
type voiceResult struct {
	pane string
	text string
	line string
}
