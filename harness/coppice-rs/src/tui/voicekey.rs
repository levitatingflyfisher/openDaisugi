//! The talk key: it records a clip, sends it to the voice server the
//! coppice server runs, and types the text into the selected agent's input
//! line without Enter.

use std::time::{Duration, Instant};

use crate::config;

use super::keys::{read_key_from, Key};
use super::model::Model;
use super::run::{Async, Floor, FloorErr};
use super::speak::{
    SpeakAct, SpeakInput, DEVICE_QUERY, KITTY_POP, KITTY_QUERY, KITTY_RELEASE, KITTY_REPEAT,
};
use super::voice::{
    clean_transcript, find_recorder, start_clip, transcribe, voice_ready, Clip, VoiceTarget,
    MIN_CLIP_BYTES, NO_RECORDER,
};

/// The longest keys go to the talk key while the floor waits for the
/// terminal's answer to a device query.
const DEVICE_WAIT: Duration = Duration::from_secs(1);

/// What one clip came to.
pub struct VoiceResult {
    pub pane: String,
    pub text: String,
    pub line: String,
}

/// What the check before a clip found.
pub struct VoiceCheck {
    pub pane: String,
    pub vt: Option<VoiceTarget>,
    pub why: String,
}

impl Model {
    /// The byte that records a clip.
    pub fn talk_key(&self) -> u8 {
        if self.talk == 0 {
            return config::DEFAULT_TALK;
        }
        self.talk
    }

    /// The talk key as the footer names it.
    pub fn talk_name(&self) -> String {
        config::key_name(self.talk_key())
    }
}

impl Floor<'_> {
    /// Whether keys still go to the talk key while the floor waits for a
    /// device answer.
    pub fn fenced(&self) -> bool {
        self.await_device && matches!(self.keys_to_talk, Some(t) if Instant::now() < t)
    }

    /// Whether byte b goes to the talk key.
    pub fn talk_wants(&self, b: u8) -> bool {
        self.sp.recording || self.fenced() || b == self.m.talk_key()
    }

    fn speak_input_of(&self, k: &Key) -> Option<SpeakInput> {
        let talk = self.m.talk_key();
        match k {
            Key::Byte(b) => {
                if *b == talk {
                    return Some(SpeakInput::TalkByte);
                }
                if *b == 0x03 && self.sp.recording {
                    return Some(SpeakInput::Cancel);
                }
                None
            }
            Key::Esc => self.sp.recording.then_some(SpeakInput::Cancel),
            Key::Device => Some(SpeakInput::DeviceAnswer),
            Key::Kitty(kk) => {
                if kk.query {
                    return Some(SpeakInput::KittyAnswer);
                }
                if super::speak::is_talk_code(talk, kk.code) {
                    return Some(match kk.event {
                        KITTY_REPEAT => SpeakInput::KittyRepeat,
                        KITTY_RELEASE => SpeakInput::KittyRelease,
                        _ => SpeakInput::KittyPress,
                    });
                }
                if kk.event != KITTY_RELEASE
                    && (kk.code == 27 || (kk.code == b'c' as i64 && (kk.mods - 1) & 4 != 0))
                    && self.sp.recording
                {
                    return Some(SpeakInput::Cancel);
                }
                None
            }
            _ => None,
        }
    }

    /// Takes byte b for the talk key. Ok(true) when the floor should
    /// close.
    pub fn speak_key(&mut self, b: u8) -> Result<bool, FloorErr> {
        let (ks, closed) = read_key_from(self, b);
        for k in ks {
            let Some(input) = self.speak_input_of(&k) else {
                if self.sp.recording
                    || !self.m.typing.is_empty()
                    || matches!(k, Key::Kitty(_))
                    || self.fenced()
                {
                    continue;
                }
                let quit = self.handle(k)?;
                if quit {
                    return Ok(true);
                }
                continue;
            };
            if input == SpeakInput::DeviceAnswer {
                self.await_device = false;
            }
            if input == SpeakInput::TalkByte && !self.sp.recording {
                self.check_voice();
                continue;
            }
            let act = self.sp.feed(input, Instant::now());
            self.apply_speak(act);
        }
        self.render()?;
        Ok(closed)
    }

    /// Checks, on the side, that there is an agent to type into, a voice
    /// server, and a recorder.
    fn check_voice(&mut self) {
        if self.checking {
            return;
        }
        let mut pane = self.m.typing.clone();
        if pane.is_empty() {
            if let Some(r) = self.m.selected() {
                pane = if r.parent.is_empty() { r.id } else { r.parent };
            }
        }
        if pane.is_empty() {
            self.m.voice = format!("Select an agent, then press {} again.", self.m.talk_name());
            return;
        }
        self.checking = true;
        self.m.voice = "Checking voice…".into();
        let socket = self.o.socket.clone();
        let tx = self.async_tx.clone();
        std::thread::spawn(move || {
            let (mut vt, mut why) = voice_ready(&socket);
            if vt.is_some() && find_recorder("").is_none() {
                vt = None;
                why = NO_RECORDER.into();
            }
            let _ = tx.send(Async::VoiceChecked(VoiceCheck { pane, vt, why }));
        });
    }

    /// Starts the clip the check allowed, or says why not.
    pub fn voice_check_done(&mut self, c: VoiceCheck) {
        self.checking = false;
        let Some(vt) = c.vt else {
            self.m.voice = format!("{}, then press {} again.", c.why, self.m.talk_name());
            return;
        };
        self.rec_for = c.pane;
        self.rec_to = Some(vt);
        let act = self.sp.feed(SpeakInput::TalkByte, Instant::now());
        self.apply_speak(act);
    }

    fn apply_speak(&mut self, act: SpeakAct) {
        if !act.write.is_empty() {
            let mut w = act.write.clone();
            if w == KITTY_QUERY || w == KITTY_POP {
                w.push_str(DEVICE_QUERY);
                self.await_device = true;
                self.keys_to_talk = Some(Instant::now() + DEVICE_WAIT);
            }
            self.write_out(&w);
        }
        if act.start {
            match start_clip(&self.o.data_dir) {
                Ok(c) => self.rec = Some(c),
                Err(e) => {
                    let e = e.strip_suffix('.').unwrap_or(&e).to_string();
                    self.m.voice = format!("{e}, then press {} again.", self.m.talk_name());
                    let w = self.sp.feed(SpeakInput::Cancel, Instant::now()).write;
                    self.apply_speak(SpeakAct {
                        write: w,
                        ..Default::default()
                    });
                    return;
                }
            }
        } else if act.stop {
            let c = self.rec.take();
            let (pane, vt) = (self.rec_for.clone(), self.rec_to.clone());
            self.hearing += 1;
            self.m.voice = "Listening to the clip…".into();
            let tx = self.async_tx.clone();
            std::thread::spawn(move || {
                let r = heard(c, pane, vt.unwrap_or_default());
                let _ = tx.send(Async::VoiceDone(r));
            });
            return;
        } else if act.cancel {
            if let Some(c) = self.rec.take() {
                std::thread::spawn(move || c.discard());
            }
            self.m.voice = "The clip was thrown away.".into();
            return;
        }
        if self.sp.recording {
            self.m.voice = self.sp.speak_line(&self.m.talk_name());
        }
    }

    /// Puts a clip's text in its agent's input line, without Enter.
    pub fn voice_heard(&mut self, r: VoiceResult) -> Result<(), FloorErr> {
        self.hearing -= 1;
        if !r.line.is_empty() {
            self.m.voice = r.line;
            return Ok(());
        }
        if !self.m.has(&r.pane) {
            self.m.voice = "The agent the clip was for is gone.".into();
            return Ok(());
        }
        if self.m.is_headless(&r.pane) {
            self.headless_insert(&r.pane, &r.text);
            self.m.voice = format!(
                "Typed into {}. Read it, then press Enter there.",
                self.m.label_of(&r.pane)
            );
            return Ok(());
        }
        self.queue_text(&r.pane, r.text.as_bytes());
        self.m.voice = format!(
            "Typed into {}. Read it, then press Enter there.",
            self.m.label_of(&r.pane)
        );
        self.pump_text()
    }

    /// Throws away a clip that still records and pops the kitty flags.
    pub fn stop_voice(&mut self) {
        if let Some(c) = self.rec.take() {
            c.discard();
        }
        let w = self.sp.close();
        if !w.is_empty() {
            self.write_out(&w);
        }
    }
}

/// Stops the recording, sends it to the voice server, and says what it
/// came to.
fn heard(c: Option<Clip>, pane: String, vt: VoiceTarget) -> VoiceResult {
    let res = |line: String| VoiceResult {
        pane: pane.clone(),
        text: String::new(),
        line,
    };
    let wav = match c {
        Some(c) => c.stop(),
        None => Err("no clip".into()),
    };
    let wav = match wav {
        Err(e) => return res(format!("The clip could not be read: {e}.")),
        Ok(w) => w,
    };
    if wav.len() < MIN_CLIP_BYTES {
        return res("The clip was too short. Speak a little longer.".into());
    }
    let (text, err) = match transcribe(&vt.url, &vt.token, &wav) {
        Ok(t) => (t, None),
        Err(e) => (String::new(), Some(e)),
    };
    let text = clean_transcript(&text);
    if let Some(e) = err {
        let e = e.strip_suffix('.').unwrap_or(&e).to_string();
        return res(format!("{e}. Record the clip again."));
    }
    if text.is_empty() {
        return res("No speech was heard. Speak closer to the microphone.".into());
    }
    VoiceResult {
        pane,
        text,
        line: String::new(),
    }
}
