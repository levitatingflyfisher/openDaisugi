//! The engine a box with no voice choice gets, by its hardware (ruling
//! VO-17): `engines.choose_engine`.

use super::moonshine::{MOONSHINE_BINARY, MOONSHINE_DEFAULT_MODEL};
use super::parakeet::{PARAKEET_BINARY, PARAKEET_DEFAULT_MODEL};
use super::resident::py_g;

pub const PARAKEET_MIN_RAM_GB: f64 = 8.0;
pub const PARAKEET_MIN_CPUS: i64 = 4;
pub const MOONSHINE_MIN_RAM_GB: f64 = 2.0;
/// A test sets this to "RAM_GB,CPUS,VRAM_GB" (RAM_GB may be empty), so no
/// test or case reads the real box. The voice engine choice and the model
/// catalog's default read it; nothing else does.
pub const HARDWARE_ENV: &str = "OPENDAISUGI_VOICE_HARDWARE";

/// `engines.VoiceHardware`.
#[derive(Clone, Debug)]
pub struct VoiceHardware {
    pub ram_gb: Option<f64>,
    pub cpus: i64,
    pub vram_gb: f64,
}

/// `engines.hardware_order`: the tiers this hardware can run, best first.
pub fn hardware_order(hw: &VoiceHardware) -> Vec<&'static str> {
    match hw.ram_gb {
        Some(r) if r >= PARAKEET_MIN_RAM_GB && hw.cpus >= PARAKEET_MIN_CPUS => vec!["parakeet", "moonshine", "tiny"],
        Some(r) if r >= MOONSHINE_MIN_RAM_GB => vec!["moonshine", "tiny"],
        _ => vec!["tiny"],
    }
}

fn hardware_text(hw: &VoiceHardware) -> String {
    let ram = match hw.ram_gb {
        Some(r) => format!("{} GB of RAM", py_g(r)),
        None => "an unknown amount of RAM".into(),
    };
    let cores = if hw.cpus == 1 { "1 core".to_string() } else { format!("{} cores", hw.cpus) };
    let mut text = format!("{ram} and {cores}");
    if hw.vram_gb > 0.0 {
        text += &format!(", and a GPU with {} GB (no GPU engine is built yet)", py_g(hw.vram_gb));
    }
    text
}

fn label(p: (&str, &str)) -> &'static str {
    match p.0 {
        "parakeet" => "Parakeet v2",
        "moonshine" => "Moonshine small",
        _ => "faster-whisper tiny.en",
    }
}

/// `engines.choose_engine`: the engine, model and one line. `installed`
/// holds the engines whose program is on PATH or whose package imports;
/// `fw` is whether faster-whisper imports (never, in this binary).
/// `parakeet_ok` is false when this machine's Parakeet Q4_K file is neither
/// pinned nor on disk (`parakeet::parakeet_usable`); the tier is then skipped.
pub fn choose_engine(
    hw: &VoiceHardware,
    installed: &[&str],
    fw: bool,
    parakeet_ok: bool,
) -> (&'static str, &'static str, String) {
    let mut picks: Vec<(&'static str, &'static str)> = vec![];
    for tier in hardware_order(hw) {
        let p = if tier == "parakeet" {
            if !parakeet_ok {
                continue;
            }
            ("parakeet", PARAKEET_DEFAULT_MODEL)
        } else if tier == "moonshine" || !fw {
            ("moonshine", MOONSHINE_DEFAULT_MODEL)
        } else {
            ("faster-whisper", "tiny.en")
        };
        if !picks.contains(&p) {
            picks.push(p);
        }
    }
    let chosen = picks.iter().copied().find(|p| installed.contains(&p.0)).unwrap_or(picks[0]);
    let mut line =
        format!("No voice engine is set, so voice uses {}: this box has {}.", label(chosen), hardware_text(hw));
    if chosen != picks[0] {
        let needs = if picks[0].0 == "parakeet" { PARAKEET_BINARY } else { MOONSHINE_BINARY };
        line += &format!(" {} would come first, but {needs} is not on PATH.", label(picks[0]));
    }
    (chosen.0, chosen.1, line)
}

/// Reads HARDWARE_ENV's "RAM_GB,CPUS,VRAM_GB"; None when it is unset or
/// not in that form.
pub fn parse_hardware_env(raw: &str) -> Option<VoiceHardware> {
    let parts: Vec<&str> = raw.split(',').collect();
    if parts.len() != 3 {
        return None;
    }
    let ram_gb = if parts[0].is_empty() { None } else { Some(super::pynum::py_float(parts[0])?) };
    let cpus: i64 = parts[1].trim().parse().ok()?;
    let vram_gb = super::pynum::py_float(parts[2])?;
    Some(VoiceHardware { ram_gb, cpus, vram_gb })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_desktop_gets_parakeet_and_the_line_says_why() {
        let desk = VoiceHardware { ram_gb: Some(15.5), cpus: 4, vram_gb: 0.0 };
        let (e, m, line) = choose_engine(&desk, &["parakeet", "moonshine"], false, true);
        assert_eq!((e, m), ("parakeet", "v2"));
        assert_eq!(line, "No voice engine is set, so voice uses Parakeet v2: this box has 15.5 GB of RAM and 4 cores.");
        let (_, _, line) = choose_engine(&desk, &["moonshine"], false, true);
        assert!(line.ends_with("Parakeet v2 would come first, but parakeet-cli is not on PATH."));
        let (e, _, line) = choose_engine(&VoiceHardware { ram_gb: None, cpus: 1, vram_gb: 0.0 }, &[], false, true);
        assert_eq!(e, "moonshine");
        assert!(line.contains("an unknown amount of RAM and 1 core."));
        assert_eq!(hardware_order(&VoiceHardware { ram_gb: Some(7.9), cpus: 8, vram_gb: 0.0 }), vec!["moonshine", "tiny"]);
    }

    #[test]
    fn a_machine_that_cannot_run_parakeet_skips_its_tier() {
        let desk = VoiceHardware { ram_gb: Some(15.5), cpus: 4, vram_gb: 0.0 };
        let (e, m, line) = choose_engine(&desk, &["parakeet", "moonshine"], false, false);
        assert_eq!((e, m), ("moonshine", "small"));
        assert!(!line.contains("Parakeet"));
    }
}
