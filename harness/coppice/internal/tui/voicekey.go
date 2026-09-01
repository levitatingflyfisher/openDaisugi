package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
)

// The talk key records a clip, sends it to the voice server the coppice
// server runs, and types the text into the selected agent's input line
// without Enter, so the owner reads it before it goes.

// deviceWait is the longest keys go to the talk key while the floor waits
// for the terminal's answer to a device query. Every terminal answers in
// milliseconds; this only ends the wait for one that never does.
const deviceWait = time.Second

// fenced reports whether keys still go to the talk key because the
// terminal has not answered the last device query.
func (f *floor) fenced() bool {
	return f.awaitDevice && time.Now().Before(f.keysToTalk)
}

// talkKey is the byte that records a clip.
func (m *Model) talkKey() byte {
	if m.Talk == 0 {
		return config.DefaultTalk
	}
	return m.Talk
}

// talkName is the talk key as the footer and the lines name it.
func (m *Model) talkName() string { return config.KeyName(m.talkKey()) }

// talkWants reports whether byte b goes to the talk key: while a clip
// records, until the terminal answers the device query the floor sent, and
// the talk key itself.
func (f *floor) talkWants(b byte) bool {
	return f.sp.recording || f.fenced() || b == f.m.talkKey()
}

// speakInputOf reads one decoded key as a talk key input. ok is false for
// every other key.
func (f *floor) speakInputOf(k key) (speakInput, bool) {
	talk := f.m.talkKey()
	switch k.kind {
	case keyByte:
		switch {
		case k.b == talk:
			return inTalkByte, true
		case k.b == 0x03 && f.sp.recording:
			return inCancel, true
		}
	case keyEsc:
		if f.sp.recording {
			return inCancel, true
		}
	case keyDevice:
		return inDeviceAnswer, true
	case keyKitty:
		kk := k.kitty
		switch {
		case kk.query:
			return inKittyAnswer, true
		case isTalkCode(talk, kk.code):
			switch kk.event {
			case kittyRepeat:
				return inKittyRepeat, true
			case kittyRelease:
				return inKittyRelease, true
			}
			return inKittyPress, true
		case kk.event != kittyRelease && (kk.code == 27 || kk.code == 'c' && (kk.mods-1)&4 != 0):
			if f.sp.recording {
				return inCancel, true
			}
		}
	}
	return 0, false
}

// speakKey takes byte b for the talk key. A key that is not a talk key
// input is dropped while a clip records or while a window has the keys,
// and goes to the rail otherwise. end is true when the floor should
// close.
func (f *floor) speakKey(b byte) (end bool, err error) {
	ks, closed := readKeyFrom(f.get, b)
	for _, k := range ks {
		in, ok := f.speakInputOf(k)
		if !ok {
			if f.sp.recording || f.m.Typing != "" || k.kind == keyKitty || f.fenced() {
				continue
			}
			quit, err := f.handle(k)
			if err != nil || quit {
				return quit, err
			}
			continue
		}
		if in == inDeviceAnswer {
			f.awaitDevice = false
		}
		if in == inTalkByte && !f.sp.recording {
			// A clip starts only once the check on the side says it can.
			f.checkVoice()
			continue
		}
		f.applySpeak(f.sp.feed(in, time.Now()))
	}
	if err := f.render(); err != nil {
		return false, err
	}
	return closed, nil
}

// askVoice asks the coppice server whether voice can run. Tests replace
// it.
var askVoice = voiceReady

// voiceCheck is what the check before a clip found: the pane the clip is
// for and the voice server, or the line that says why there is no clip.
type voiceCheck struct {
	pane string
	vt   *voiceTarget
	why  string
}

// checkVoice checks, before a clip starts, that there is an agent to type
// into, a voice server, and a recorder. Asking the server can take
// seconds, so it runs on the side, and the answer comes back on
// voiceChecked. A second press while it runs does nothing.
func (f *floor) checkVoice() {
	m := f.m
	if f.checking {
		return
	}
	pane := m.Typing
	if pane == "" {
		if r, ok := m.Selected(); ok {
			pane = r.ID
			if r.Parent != "" {
				pane = r.Parent
			}
		}
	}
	if pane == "" {
		m.Voice = "Select an agent, then press " + m.talkName() + " again."
		return
	}
	f.checking = true
	m.Voice = "Checking voice…"
	socket := f.o.Socket
	go func() {
		vt, why := askVoice(socket)
		if vt != nil {
			if _, _, ok := findRecorder(""); !ok {
				vt, why = nil, noRecorder
			}
		}
		f.voiceChecked <- voiceCheck{pane: pane, vt: vt, why: why}
	}()
}

// voiceCheckDone starts the clip the check allowed, or says why not.
func (f *floor) voiceCheckDone(c voiceCheck) {
	f.checking = false
	if c.vt == nil {
		f.m.Voice = c.why + ", then press " + f.m.talkName() + " again."
		return
	}
	f.recFor, f.recTo = c.pane, c.vt
	f.applySpeak(f.sp.feed(inTalkByte, time.Now()))
}

// applySpeak does what the talk key's state said: write to the terminal,
// start, stop or throw away the clip, and keep the voice line current.
func (f *floor) applySpeak(act speakAct) {
	m := f.m
	if act.write != "" {
		w := act.write
		if w == kittyQuery || w == kittyPop {
			// The device answer comes after everything the terminal sent
			// before it read this, so keys wait for it.
			w += deviceQuery
			f.awaitDevice = true
			f.keysToTalk = time.Now().Add(deviceWait)
		}
		_, _ = io.WriteString(f.o.Out, w)
	}
	switch {
	case act.start:
		c, err := startClip(f.o.DataDir)
		if err != nil {
			m.Voice = strings.TrimSuffix(err.Error(), ".") + ", then press " + m.talkName() + " again."
			f.applySpeak(speakAct{write: f.sp.feed(inCancel, time.Now()).write})
			return
		}
		f.rec = c
	case act.stop:
		c, pane, vt := f.rec, f.recFor, f.recTo
		f.rec = nil
		f.hearing++
		m.Voice = "Listening to the clip…"
		go func() { f.voiceDone <- heard(c, pane, vt) }()
		return
	case act.cancel:
		if f.rec != nil {
			// Stopping a recorder can take two seconds, so it happens on
			// the side.
			go f.rec.discard()
			f.rec = nil
		}
		m.Voice = "The clip was thrown away."
		return
	}
	if f.sp.recording {
		m.Voice = f.sp.speakLine(m.talkName())
	}
}

// heard stops the recording, sends it to the voice server, and says what
// it came to.
func heard(c *clip, pane string, vt *voiceTarget) voiceResult {
	wav, err := c.stop()
	switch {
	case err != nil:
		return voiceResult{pane: pane, line: fmt.Sprintf("The clip could not be read: %v.", err)}
	case len(wav) < minClipBytes:
		return voiceResult{pane: pane, line: "The clip was too short. Speak a little longer."}
	}
	text, err := transcribe(vt.url, vt.token, wav)
	text = cleanTranscript(text)
	switch {
	case err != nil:
		return voiceResult{pane: pane, line: strings.TrimSuffix(err.Error(), ".") + ". Record the clip again."}
	case text == "":
		return voiceResult{pane: pane, line: "No speech was heard. Speak closer to the microphone."}
	}
	return voiceResult{pane: pane, text: text}
}

// voiceHeard puts a clip's text in its agent's input line, without
// Enter, in order with any text typed before it. For a headless agent
// that line is its own window's input line, never the wire: the text
// lands there exactly as a typed key would, and Enter there sends it as
// agent.prompt, the same as any other word its owner types.
func (f *floor) voiceHeard(r voiceResult) error {
	m := f.m
	f.hearing--
	switch {
	case r.line != "":
		m.Voice = r.line
		return nil
	case !m.has(r.pane):
		m.Voice = "The agent the clip was for is gone."
		return nil
	}
	if m.isHeadless(r.pane) {
		f.headlessInsert(r.pane, r.text)
		m.Voice = "Typed into " + m.labelOf(r.pane) + ". Read it, then press Enter there."
		return nil
	}
	f.queueText(r.pane, []byte(r.text))
	m.Voice = "Typed into " + m.labelOf(r.pane) + ". Read it, then press Enter there."
	return f.pumpText()
}

// stopVoice throws away a clip that still records and pops the kitty
// flags, as the floor closes.
func (f *floor) stopVoice() {
	if f.rec != nil {
		f.rec.discard()
		f.rec = nil
	}
	if w := f.sp.close(); w != "" {
		_, _ = io.WriteString(f.o.Out, w)
	}
}
