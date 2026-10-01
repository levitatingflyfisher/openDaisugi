//! The talk key's state. On a terminal that speaks the kitty keyboard
//! protocol the owner holds the key while speaking; anywhere else one press
//! starts the clip and a second press stops it. The floor asks the
//! terminal with CSI ? u on the first press, and pushes the flags only
//! while a clip records.

use std::time::{Duration, Instant};

use super::keys::go_atoi;

pub const KITTY_QUERY: &str = "\x1b[?u";
pub const KITTY_PUSH: &str = "\x1b[>11u";
pub const KITTY_POP: &str = "\x1b[<u";

pub const KITTY_PRESS: i64 = 1;
pub const KITTY_REPEAT: i64 = 2;
pub const KITTY_RELEASE: i64 = 3;

/// One CSI u report, or with query true, the answer to CSI ? u.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct KittyKey {
    pub code: i64,
    pub mods: i64,
    pub event: i64,
    pub query: bool,
}

/// Reads a whole sequence that starts with ESC [ and ends with u.
pub fn parse_kitty_csi(seq: &[u8]) -> Option<KittyKey> {
    if seq.len() < 4 || seq[0] != 0x1b || seq[1] != b'[' || seq[seq.len() - 1] != b'u' {
        return None;
    }
    let body = &seq[2..seq.len() - 1];
    if let Some(rest) = body.strip_prefix(b"?") {
        go_atoi(rest)?;
        return Some(KittyKey {
            query: true,
            ..Default::default()
        });
    }
    let fields: Vec<&[u8]> = body.split(|c| *c == b';').collect();
    let first = fields[0].split(|c| *c == b':').next().unwrap_or(&[]);
    let code = go_atoi(first)?;
    let mut k = KittyKey {
        code,
        mods: 1,
        event: KITTY_PRESS,
        query: false,
    };
    if fields.len() > 1 && !fields[1].is_empty() {
        let mut parts = fields[1].splitn(2, |c| *c == b':');
        let p0 = parts.next().unwrap_or(&[]);
        let p1 = parts.next();
        if !p0.is_empty() {
            k.mods = go_atoi(p0)?;
        }
        if let Some(p1) = p1 {
            k.event = go_atoi(p1)?;
        }
    }
    Some(k)
}

/// The key codes the kitty protocol reports for the ctrl byte b.
pub fn kitty_codes(b: u8) -> Vec<i64> {
    match b {
        0x00 => vec![b' ' as i64],
        0x01..=0x1a => vec![(b'a' + b - 1) as i64],
        0x1c => vec![b'\\' as i64],
        0x1d => vec![b']' as i64],
        0x1e => vec![b'^' as i64, b'6' as i64],
        0x1f => vec![b'_' as i64, b'-' as i64],
        _ => vec![],
    }
}

/// Whether a kitty key code is the talk key b.
pub fn is_talk_code(b: u8, code: i64) -> bool {
    kitty_codes(b).contains(&code)
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum KittySupport {
    #[default]
    Unknown,
    Yes,
    No,
}

/// One thing the owner or the terminal did while the talk key matters.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SpeakInput {
    TalkByte,
    KittyPress,
    KittyRepeat,
    KittyRelease,
    KittyAnswer,
    Cancel,
    DeviceAnswer,
}

/// CSI c, primary device attributes.
pub const DEVICE_QUERY: &str = "\x1b[c";

/// What the floor does after one input.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SpeakAct {
    pub start: bool,
    pub stop: bool,
    pub cancel: bool,
    pub write: String,
}

/// The talk key's state. It is pure: the floor feeds it inputs and does
/// what it returns.
#[derive(Clone, Debug, Default)]
pub struct Speaker {
    pub recording: bool,
    pub started: Option<Instant>,
    pub last: Option<Instant>,
    pub hold: bool,
    pub kitty: KittySupport,
    pub queried: bool,
}

pub const REPEAT_GAP: Duration = Duration::from_millis(150);
pub const MIN_TOGGLE: Duration = Duration::from_millis(700);

fn since(now: Instant, t: Option<Instant>) -> Duration {
    match t {
        Some(t) => now.saturating_duration_since(t),
        // Go's zero time is long ago.
        None => Duration::MAX,
    }
}

impl Speaker {
    /// Takes one input at now and says what to do.
    pub fn feed(&mut self, input: SpeakInput, now: Instant) -> SpeakAct {
        match input {
            SpeakInput::KittyAnswer => {
                self.kitty = KittySupport::Yes;
                if self.recording && !self.hold {
                    self.hold = true;
                    return SpeakAct {
                        write: KITTY_PUSH.into(),
                        ..Default::default()
                    };
                }
                SpeakAct::default()
            }
            SpeakInput::TalkByte => {
                let last = self.last;
                self.last = Some(now);
                if !self.recording {
                    self.started = Some(now);
                    return self.begin();
                }
                if self.hold {
                    return SpeakAct::default();
                }
                if since(now, last) < REPEAT_GAP || since(now, self.started) < MIN_TOGGLE {
                    return SpeakAct::default();
                }
                self.end(false)
            }
            SpeakInput::KittyPress | SpeakInput::KittyRelease => {
                if self.recording && self.hold {
                    return self.end(false);
                }
                SpeakAct::default()
            }
            SpeakInput::Cancel => {
                if self.recording {
                    return self.end(true);
                }
                SpeakAct::default()
            }
            SpeakInput::DeviceAnswer => {
                if self.kitty == KittySupport::Unknown && self.queried {
                    self.kitty = KittySupport::No;
                }
                SpeakAct::default()
            }
            SpeakInput::KittyRepeat => SpeakAct::default(),
        }
    }

    fn begin(&mut self) -> SpeakAct {
        self.recording = true;
        let mut act = SpeakAct {
            start: true,
            ..Default::default()
        };
        if self.kitty == KittySupport::Yes {
            self.hold = true;
            act.write = KITTY_PUSH.into();
        } else if self.kitty == KittySupport::Unknown && !self.queried {
            self.queried = true;
            act.write = KITTY_QUERY.into();
        }
        act
    }

    fn end(&mut self, cancel: bool) -> SpeakAct {
        let mut act = SpeakAct {
            stop: !cancel,
            cancel,
            ..Default::default()
        };
        if self.hold {
            act.write = KITTY_POP.into();
        }
        self.recording = false;
        self.hold = false;
        if self.kitty == KittySupport::Unknown && self.queried {
            self.kitty = KittySupport::No;
        }
        act
    }

    /// What the floor writes when it closes.
    pub fn close(&mut self) -> String {
        if self.hold {
            self.hold = false;
            return KITTY_POP.into();
        }
        String::new()
    }

    /// The line the floor shows while a clip records.
    pub fn speak_line(&self, talk: &str) -> String {
        if self.hold {
            return format!(
                "recording… let go of {talk}, or press it again, to stop  Esc throws it away"
            );
        }
        format!("recording… press {talk} to stop  Esc throws it away")
    }
}
