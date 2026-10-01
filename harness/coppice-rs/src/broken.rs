//! Keeps the pile of "<name>.broken-<UTC time>" files small. Every place that
//! moves damaged state aside calls `prune` with the name it just made, and
//! only the newest `keep()` per base name stay.

use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};

/// How many broken copies of one name stay.
pub const DEFAULT_KEEP: usize = 5;

static KEEP: AtomicUsize = AtomicUsize::new(DEFAULT_KEEP);

/// Sets the count to keep. 0 keeps all.
pub fn set_keep(n: usize) {
    KEEP.store(n, Ordering::Relaxed);
}

/// The count to keep. 0 means keep all.
pub fn keep() -> usize {
    KEEP.load(Ordering::Relaxed)
}

/// Splits "<base>.broken-<YYYYMMDDTHHMMSSZ>[-n]" into (base, stamp, n).
fn parse(name: &str) -> Option<(&str, &str, u64)> {
    let i = name.rfind(".broken-")?;
    let (base, rest) = (&name[..i], &name[i + ".broken-".len()..]);
    if rest.len() < 16 || !rest.is_char_boundary(16) {
        return None;
    }
    let (stamp, tail) = rest.split_at(16);
    let b = stamp.as_bytes();
    let ok = b[8] == b'T'
        && b[15] == b'Z'
        && b[..8].iter().all(u8::is_ascii_digit)
        && b[9..15].iter().all(u8::is_ascii_digit);
    if !ok {
        return None;
    }
    let n = if tail.is_empty() {
        1
    } else {
        tail.strip_prefix('-')?.parse().ok()?
    };
    Some((base, stamp, n))
}

/// Removes the oldest broken copies of the base name that `moved` is a copy
/// of, so the newest `keep()` stay. It never removes `moved` itself. Best
/// effort: an error leaves the file in place.
pub fn prune(moved: &Path) {
    let k = keep();
    if k == 0 {
        return;
    }
    let Some(name) = moved.file_name().map(|n| n.to_string_lossy().into_owned()) else {
        return;
    };
    let Some((base, _, _)) = parse(&name) else {
        return;
    };
    let dir = moved.parent().unwrap_or(Path::new("."));
    let Ok(rd) = std::fs::read_dir(dir) else {
        return;
    };
    let mut all: Vec<(String, u64, std::path::PathBuf)> = Vec::new();
    for e in rd.flatten() {
        let en = e.file_name().to_string_lossy().into_owned();
        if let Some((b, stamp, n)) = parse(&en) {
            if b == base {
                all.push((stamp.to_string(), n, e.path()));
            }
        }
    }
    if all.len() <= k {
        return;
    }
    all.sort_by(|a, b| (&b.0, b.1).cmp(&(&a.0, a.1)));
    for (_, _, p) in all.into_iter().skip(k) {
        if p == moved {
            continue;
        }
        if p.is_dir() && !p.is_symlink() {
            let _ = std::fs::remove_dir_all(&p);
        } else {
            let _ = std::fs::remove_file(&p);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prune_keeps_newest_five() {
        set_keep(DEFAULT_KEEP);
        let dir = std::env::temp_dir().join(format!("broken-prune-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let mut last = dir.clone();
        for i in 1..=8 {
            last = dir.join(format!("view.json.broken-2026010{i}T000000Z"));
            std::fs::write(&last, b"").unwrap();
        }
        let other = dir.join("other.broken-20260101T000000Z");
        std::fs::write(&other, b"").unwrap();
        prune(&last);
        let mut left: Vec<String> = std::fs::read_dir(&dir)
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n.starts_with("view.json"))
            .collect();
        left.sort();
        assert_eq!(left.len(), 5, "{left:?}");
        assert!(left[0].contains("20260104"));
        assert!(other.exists());
        let _ = std::fs::remove_dir_all(&dir);
    }
}
