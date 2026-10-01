//! The rail keys, one table that also prints the footer; the decoder that
//! turns bytes into keystrokes; and mouse reports.

use std::time::Duration;

use super::model::Model;
use super::speak::{parse_kitty_csi, KittyKey};

/// What one rail key does.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Action {
    GoIn,
    Window,
    New,
    NewIn,
    Rename,
    Stop,
    NextNeed,
    Peek,
    Talk,
    Up,
    None,
}

/// One rail key: its name as the footer shows it, what it does, and the
/// word the footer prints beside it.
#[derive(Clone, Debug)]
pub struct Binding {
    pub keys: &'static str,
    pub action: Action,
    pub help: &'static str,
}

/// The rail keys, in the order the footer shows them.
pub fn table() -> Vec<Binding> {
    let b = |keys, action, help| Binding { keys, action, help };
    vec![
        b("Enter", Action::GoIn, "go in"),
        b("1-9", Action::Window, "to window"),
        b("n", Action::New, "new"),
        b("N", Action::NewIn, "new in project"),
        b("r", Action::Rename, "rename"),
        b("ctrl-w", Action::Stop, "stop"),
        b("shift-tab", Action::NextNeed, "next need"),
        b("Space", Action::Peek, "peek or fill"),
        b("ctrl-t", Action::Talk, "talk"),
        b("Esc", Action::Up, "up"),
    ]
}

/// The footer text: every binding's keys and help, two spaces apart.
#[cfg(test)]
pub fn keys_text() -> String {
    join_keys(&table())
}

pub fn join_keys(t: &[Binding]) -> String {
    t.iter()
        .map(|b| format!("{} {}", b.keys, b.help))
        .collect::<Vec<_>>()
        .join("  ")
}

/// One decoded keystroke.
#[derive(Clone, Debug, PartialEq)]
pub enum Key {
    Byte(u8),
    Up,
    Down,
    Esc,
    ShiftTab,
    Mouse(Click),
    /// A CSI u report.
    Kitty(KittyKey),
    /// The terminal's answer to the device query CSI c.
    Device,
    Left,
    Right,
}

/// The action one keystroke binds to on the rail.
pub fn bind(k: &Key) -> Action {
    match k {
        Key::ShiftTab => Action::NextNeed,
        Key::Esc => Action::Up,
        Key::Byte(b) => match *b {
            0x14 => Action::Talk,
            0x17 => Action::Stop,
            b' ' => Action::Peek,
            b'\r' | b'\n' => Action::GoIn,
            b'n' => Action::New,
            b'N' => Action::NewIn,
            b'r' => Action::Rename,
            b'1'..=b'9' => Action::Window,
            _ => Action::None,
        },
        _ => Action::None,
    }
}

/// One mouse report in SGR 1006 form.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Click {
    pub button: i64,
    pub x: i64,
    pub y: i64,
    pub release: bool,
}

pub const SGR_MOUSE_PREFIX: &[u8] = b"\x1b[<";

/// Parses one SGR 1006 mouse report.
pub fn parse_sgr(b: &[u8]) -> Option<Click> {
    if !b.starts_with(SGR_MOUSE_PREFIX) || b.len() < SGR_MOUSE_PREFIX.len() + 1 {
        return None;
    }
    let fin = b[b.len() - 1];
    if fin != b'M' && fin != b'm' {
        return None;
    }
    let body = &b[SGR_MOUSE_PREFIX.len()..b.len() - 1];
    let parts: Vec<&[u8]> = body.split(|c| *c == b';').collect();
    if parts.len() != 3 {
        return None;
    }
    let mut nums = [0i64; 3];
    for (i, p) in parts.iter().enumerate() {
        let n = go_atoi(p)?;
        if n < 0 {
            return None;
        }
        nums[i] = n;
    }
    Some(Click {
        button: nums[0],
        x: nums[1],
        y: nums[2],
        release: fin == b'm',
    })
}

/// Go's strconv.Atoi on bytes: an optional sign, then decimal digits,
/// within 64 bits.
pub fn go_atoi(p: &[u8]) -> Option<i64> {
    let s = std::str::from_utf8(p).ok()?;
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    s.parse::<i64>().ok()
}

/// The cursor-order index of the roster row drawn on screen row y.
pub fn hit_row(m: &Model, y: i64) -> Option<i64> {
    if y < 1 || y > m.row_at.len() as i64 || m.row_at[(y - 1) as usize] < 0 {
        return None;
    }
    Some(m.row_at[(y - 1) as usize])
}

/// The shown slot drawn under screen column x.
pub fn hit_tile(m: &Model, x: i64) -> Option<i64> {
    if x < 1 || x > m.tile_at.len() as i64 || m.tile_at[(x - 1) as usize] < 0 {
        return None;
    }
    Some(m.tile_at[(x - 1) as usize])
}

/// Whether the tiles are drawn on screen row y.
pub fn on_tiles(m: &Model, y: i64) -> bool {
    y >= 1 && y <= m.tile_y.len() as i64 && m.tile_y[(y - 1) as usize]
}

/// How long a bare ESC waits for the rest of a sequence.
pub const ESC_WAIT: Duration = Duration::from_millis(50);
/// The most bytes a mouse report may take.
pub const MOUSE_SEQ_MAX: usize = 32;

/// The next key byte within a wait: Some(Some(b)) for a byte,
/// Some(None) when the keys closed, None when no byte came in time.
pub trait GetByte {
    fn get(&mut self, wait: Duration) -> Option<Option<u8>>;
}

/// Turns the byte b and, for ESC, what follows it into keystrokes. closed
/// is true when the keys closed during the wait.
pub fn read_key_from(get: &mut dyn GetByte, b: u8) -> (Vec<Key>, bool) {
    if b != 0x1b {
        return (vec![Key::Byte(b)], false);
    }
    let c = match get.get(ESC_WAIT) {
        None => return (vec![Key::Esc], false),
        Some(None) => return (vec![Key::Esc], true),
        Some(Some(c)) => c,
    };
    if c != b'[' && c != b'O' {
        return (vec![Key::Esc, Key::Byte(c)], false);
    }
    let mut seq = vec![0x1b, c];
    loop {
        let d = match get.get(ESC_WAIT) {
            None => return (vec![], false),
            Some(None) => return (vec![], true),
            Some(Some(d)) => d,
        };
        seq.push(d);
        if seq.len() == 3 && d == b'<' {
            continue;
        }
        if &seq[..3] == SGR_MOUSE_PREFIX {
            if seq.len() > MOUSE_SEQ_MAX {
                return (vec![], false);
            }
            if d == b'M' || d == b'm' {
                if let Some(click) = parse_sgr(&seq) {
                    return (vec![Key::Mouse(click)], false);
                }
                return (vec![], false);
            }
            continue;
        }
        if (b'@'..=b'~').contains(&d) {
            match d {
                b'A' => return (vec![Key::Up], false),
                b'B' => return (vec![Key::Down], false),
                b'C' => return (vec![Key::Right], false),
                b'D' => return (vec![Key::Left], false),
                b'Z' => return (vec![Key::ShiftTab], false),
                b'u' => {
                    if let Some(k) = parse_kitty_csi(&seq) {
                        return (vec![Key::Kitty(k)], false);
                    }
                }
                b'c' if seq[2] == b'?' => return (vec![Key::Device], false),
                _ => {}
            }
            return (vec![], false);
        }
    }
}

#[cfg(test)]
pub mod tests {
    use super::*;
    use std::collections::VecDeque;

    /// The bytes of a string, then nothing: a getter for tests.
    pub struct Feed(pub VecDeque<u8>);

    impl GetByte for Feed {
        fn get(&mut self, _wait: Duration) -> Option<Option<u8>> {
            self.0.pop_front().map(Some)
        }
    }

    pub fn feed(s: &[u8]) -> Feed {
        Feed(s.iter().copied().collect())
    }

    fn bind_seq(seq: &[u8]) -> Action {
        let mut f = feed(&seq[1..]);
        let (ks, closed) = read_key_from(&mut f, seq[0]);
        assert!(!closed, "{seq:?} closed the keys");
        let mut got = Action::None;
        for k in &ks {
            let a = bind(k);
            if a != Action::None {
                got = a;
            }
        }
        got
    }

    #[test]
    fn the_rail_keys_map_and_nothing_else_does() {
        let cases: &[(&[u8], Action)] = &[
            (b"\x14", Action::Talk),
            (b"\x1b[Z", Action::NextNeed),
            (b"\x17", Action::Stop),
            (b" ", Action::Peek),
            (b"\r", Action::GoIn),
            (b"\n", Action::GoIn),
            (b"n", Action::New),
            (b"N", Action::NewIn),
            (b"r", Action::Rename),
            (b"1", Action::Window),
            (b"9", Action::Window),
        ];
        for (seq, want) in cases {
            assert_eq!(bind_seq(seq), *want, "{seq:?}");
        }
        for b in b"qpcdxt?o0\x01\x03\x7f" {
            assert_eq!(bind_seq(&[*b]), Action::None, "{b} must be text");
        }
        for seq in [&b"\x1b[A"[..], b"\x1b[B", b"\x1b[<0;3;4M"] {
            assert_eq!(bind_seq(seq), Action::None, "{seq:?}");
        }
    }

    #[test]
    fn lone_esc_is_up_after_the_wait() {
        assert_eq!(bind_seq(b"\x1b"), Action::Up);
    }

    #[test]
    fn footer_comes_from_the_table() {
        let f = keys_text();
        for b in table() {
            assert!(f.contains(&format!("{} {}", b.keys, b.help)));
        }
        assert_eq!(table().len(), 10);
        assert!(!f.contains("q quit") && !f.contains("t open"));
    }

    #[test]
    fn every_binding_in_the_table_has_an_action() {
        let mut seen = std::collections::HashSet::new();
        for b in table() {
            assert!(b.action != Action::None && !b.keys.is_empty() && !b.help.is_empty());
            assert!(seen.insert(b.action), "action {:?} bound twice", b.action);
        }
    }
}
