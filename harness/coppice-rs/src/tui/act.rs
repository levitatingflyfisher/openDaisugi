//! The client act id of a sentence to the foreman (FM-R-65). The twin of
//! Go's tui/act.go.
//!
//! The server uses it to log the sentence once and to queue one turn,
//! however many times it arrives.

use std::io::Read;

use super::run::Floor;

/// A fresh random version 4 UUID, or "" when the system has no entropy: the
/// act is then left out and the sentence behaves as it did before ids
/// existed.
pub fn new_act() -> String {
    let mut b = [0u8; 16];
    let ok = std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut b))
        .is_ok();
    if !ok {
        return String::new();
    }
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    let h: String = b.iter().map(|x| format!("{x:02x}")).collect();
    format!(
        "{}-{}-{}-{}-{}",
        &h[0..8],
        &h[8..12],
        &h[12..16],
        &h[16..20],
        &h[20..32]
    )
}

impl Floor<'_> {
    /// The act id of the sentence being sent. A sentence whose send failed
    /// and that is sent again keeps its id; the next sentence gets a new
    /// one.
    pub fn act_for(&mut self, line: &str) -> String {
        if self.act.0 != line || self.act.1.is_empty() {
            self.act = (line.to_string(), new_act());
        }
        self.act.1.clone()
    }

    /// Forgets the id once the server has answered.
    pub fn act_done(&mut self) {
        self.act = Default::default();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_act_is_a_version_4_uuid() {
        let re = regex::Regex::new(
            r"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$",
        )
        .unwrap();
        let (a, b) = (new_act(), new_act());
        assert!(re.is_match(&a) && a != b, "{a} {b}");
    }
}
