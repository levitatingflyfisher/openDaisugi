//! The tokens the gateway says it saved today, for the floor header. The
//! figure is the gateway's own, read from its turn journal and summed over
//! the turns that name a pane of this floor; the floor never estimates one.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Seek, SeekFrom};
use std::path::PathBuf;
use std::time::SystemTime;

use serde_json::Value;

use super::Server;

/// The most bytes of the turn journal one read takes, from its end.
const SAVED_TAIL_CAP: u64 = 8 << 20;

/// What the floor last read: the frontier tokens saved today for each pane
/// the records name. Read again when the size, the time or the local day
/// changed.
#[derive(Default)]
pub struct SavedCache {
    size: u64,
    mod_time: Option<SystemTime>,
    day: (i32, i32, i32),
    by: Option<HashMap<String, i64>>,
}

/// Unix seconds of an RFC 3339 time, or None when the text is not one.
/// This is the form the gateway writes: 2026-10-14T09:30:00.123456+00:00.
fn parse_rfc3339(s: &str) -> Option<f64> {
    let b = s.as_bytes();
    if b.len() < 20 || !s.is_ascii() {
        return None;
    }
    let num = |from: usize, to: usize| -> Option<i64> {
        let t = &s[from..to];
        t.bytes()
            .all(|c| c.is_ascii_digit())
            .then(|| t.parse().ok())?
    };
    if b[4] != b'-' || b[7] != b'-' || b[10] != b'T' || b[13] != b':' || b[16] != b':' {
        return None;
    }
    let (y, mo, d) = (num(0, 4)?, num(5, 7)?, num(8, 10)?);
    let (h, mi, sec) = (num(11, 13)?, num(14, 16)?, num(17, 19)?);
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || h > 23 || mi > 59 || sec > 59 {
        return None;
    }
    let mut i = 19;
    let mut frac = 0.0;
    if b[i] == b'.' {
        let start = i + 1;
        i = start;
        while i < b.len() && b[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            return None;
        }
        frac = format!("0.{}", &s[start..i]).parse().ok()?;
    }
    let off = match b.get(i)? {
        b'Z' if i + 1 == b.len() => 0,
        sign @ (b'+' | b'-') if i + 6 == b.len() && b[i + 3] == b':' => {
            let (oh, om) = (num(i + 1, i + 3)?, num(i + 4, i + 6)?);
            if oh > 23 || om > 59 {
                return None;
            }
            let o = oh * 3600 + om * 60;
            if *sign == b'+' {
                o
            } else {
                -o
            }
        }
        _ => return None,
    };
    // Howard Hinnant's days-from-civil.
    let y2 = if mo <= 2 { y - 1 } else { y };
    let era = y2.div_euclid(400);
    let yoe = y2 - era * 400;
    let doy = (153 * (if mo > 2 { mo - 3 } else { mo + 9 }) + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146097 + doe - 719468;
    Some((days * 86400 + h * 3600 + mi * 60 + sec - off) as f64 + frac)
}

/// Unix seconds of an RFC 3339 time, rounded down.
pub(crate) fn parse_rfc3339_secs(s: &str) -> Option<i64> {
    parse_rfc3339(s).map(|f| f.floor() as i64)
}

/// Sums frontier_tokens_saved for each pane over the records of the last
/// SAVED_TAIL_CAP bytes of path whose created_at falls on the local day of
/// now. A line that is not a record, or names no pane, is skipped. A
/// record that names a floor counts only when it is this one: pane ids
/// start at w1:p1 on every floor, and floors share a gateway journal.
fn read_saved_today(
    path: &PathBuf,
    now: f64,
    floor: &str,
) -> std::io::Result<HashMap<String, i64>> {
    let mut f = std::fs::File::open(path)?;
    let size = f.metadata()?.len();
    let cut = size > SAVED_TAIL_CAP;
    if cut {
        f.seek(SeekFrom::Start(size - SAVED_TAIL_CAP))?;
    }
    let mut r = BufReader::with_capacity(1 << 16, f);
    let mut line = Vec::new();
    if cut {
        // The first line is likely half a line.
        if r.read_until(b'\n', &mut line)? == 0 {
            return Ok(HashMap::new());
        }
    }
    let today = super::facts::local_date(now);
    let mut by: HashMap<String, i64> = HashMap::new();
    loop {
        line.clear();
        if r.read_until(b'\n', &mut line)? == 0 {
            return Ok(by);
        }
        let text = String::from_utf8_lossy(&line);
        let text = text.trim();
        if text.is_empty() {
            continue;
        }
        let Ok(v) = serde_json::from_str::<Value>(text) else {
            continue;
        };
        let (Some(saved), Some(at), Some(pane)) = (
            v.get("frontier_tokens_saved").and_then(|x| x.as_i64()),
            v.get("created_at")
                .and_then(|x| x.as_str())
                .and_then(parse_rfc3339),
            v.get("pane")
                .and_then(|x| x.as_str())
                .filter(|p| !p.is_empty()),
        ) else {
            continue;
        };
        let other_floor = v
            .get("floor")
            .and_then(|x| x.as_str())
            .is_some_and(|f| !f.is_empty() && f != floor);
        if !other_floor && super::facts::local_date(at) == today {
            *by.entry(pane.to_string()).or_default() += saved;
        }
    }
}

impl Server {
    /// The frontier tokens the gateway says it saved on today's turns,
    /// local time, that name a pane of this floor. None when the gateway
    /// has no such figure: no journal, one that cannot be read, or no turn
    /// today that names a pane here. A turn that names no pane counts for
    /// no pane.
    pub(crate) fn saved_today(&self, now: f64) -> Option<i64> {
        let path = self
            .gate_root()
            .parent()?
            .join("gateway")
            .join("turns.jsonl");
        let md = std::fs::metadata(&path).ok()?;
        if !md.is_file() {
            return None;
        }
        let day = super::facts::local_date(now);
        let by = {
            let mut c = self.saved.lock().unwrap_or_else(|e| e.into_inner());
            if c.by.is_none()
                || c.size != md.len()
                || c.mod_time != md.modified().ok()
                || c.day != day
            {
                match read_saved_today(&path, now, &self.floor_key()) {
                    Ok(by) => {
                        *c = SavedCache {
                            size: md.len(),
                            mod_time: md.modified().ok(),
                            day,
                            by: Some(by),
                        };
                    }
                    Err(_) => {
                        *c = SavedCache::default();
                        return None;
                    }
                }
            }
            c.by.clone().unwrap_or_default()
        };
        let mut tokens = 0;
        let mut found = false;
        for p in self.tree.panes() {
            if let Some(v) = by.get(&p.id) {
                tokens += v;
                found = true;
            }
        }
        found.then_some(tokens)
    }
}

#[cfg(test)]
mod tests {
    use super::parse_rfc3339;
    use crate::server::{Config, Server};

    fn stamp(secs: i64) -> String {
        let (y, mo, d, h, mi, s) = crate::web::der::civil(secs);
        format!("{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}.000000+00:00")
    }

    /// One turn: when, frontier tokens saved, and the pane it names or "".
    type Turn = (i64, i64, &'static str);

    fn turns(dir: &std::path::Path, rows: &[Turn]) {
        let g = dir.join("gateway");
        std::fs::create_dir_all(&g).unwrap();
        let mut text = String::new();
        for (at, saved, pane) in rows {
            let pane = if pane.is_empty() {
                String::new()
            } else {
                format!(",\"pane\":\"{pane}\"")
            };
            text.push_str(&format!(
                "{{\"created_at\":\"{}\",\"frontier_tokens_saved\":{saved},\"model\":\"m\"{pane}}}\n",
                stamp(*at)
            ));
        }
        std::fs::write(g.join("turns.jsonl"), text).unwrap();
    }

    #[test]
    fn no_gateway_figure_for_a_pane_here_means_no_number_and_its_turns_today_sum() {
        let dir = std::env::temp_dir().join(format!("coppice-rs-saved-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let s = Server::new(Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.join("data"),
            start_dir: dir.clone(),
            voice: Default::default(),
            foreman: Default::default(),
        });
        let ws = s.tree.create_workspace("w", "/");
        let tab = s.tree.create_tab(&ws.id, "t").unwrap();
        let pane = s
            .tree
            .create_pane(&ws.id, &tab.id, Default::default())
            .unwrap();
        let here: &'static str = Box::leak(pane.id.clone().into_boxed_str());
        let now = Server::now_seconds();
        let t = now as i64;
        assert_eq!(s.saved_today(now), None, "no turn file");
        turns(&dir, &[]);
        assert_eq!(s.saved_today(now), None, "an empty turn file");
        let old = t - 3 * 86400;
        turns(&dir, &[(old, 900, here)]);
        assert_eq!(s.saved_today(now), None, "only an old turn");
        turns(&dir, &[(t, 900, "")]);
        assert_eq!(s.saved_today(now), None, "a turn that names no pane");
        turns(&dir, &[(t, 900, "w7:p7")]);
        assert_eq!(s.saved_today(now), None, "a pane this floor does not have");
        turns(
            &dir,
            &[
                (old, 900, here),
                (t, 120, here),
                (t, 30, here),
                (t, 500, "w7:p7"),
                (t, 70, ""),
            ],
        );
        assert_eq!(s.saved_today(now), Some(150));
        turns(&dir, &[(t, 120, here), (t, 30, here), (t, 50, here)]);
        assert_eq!(s.saved_today(now), Some(200), "a new turn shows");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A record that names a floor counts only on that floor: pane ids
    /// start at w1:p1 on every floor, and floors share a gateway journal.
    #[test]
    fn a_record_of_another_floor_counts_for_no_pane() {
        let dir =
            std::env::temp_dir().join(format!("coppice-rs-savedfloor-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let s = Server::new(Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.join("data"),
            start_dir: dir.clone(),
            voice: Default::default(),
            foreman: Default::default(),
        });
        let ws = s.tree.create_workspace("w", "/");
        let tab = s.tree.create_tab(&ws.id, "t").unwrap();
        let pane = s
            .tree
            .create_pane(&ws.id, &tab.id, Default::default())
            .unwrap();
        let now = Server::now_seconds();
        let at = stamp(now as i64);
        let g = dir.join("gateway");
        std::fs::create_dir_all(&g).unwrap();
        let line = |saved: i64, floor: &str| {
            let f = if floor.is_empty() {
                String::new()
            } else {
                format!(",\"floor\":\"{floor}\"")
            };
            format!(
                "{{\"created_at\":\"{at}\",\"frontier_tokens_saved\":{saved},\"model\":\"m\",\"pane\":\"{}\"{f}}}\n",
                pane.id
            )
        };
        let text = format!(
            "{}{}{}",
            line(100, &s.floor_key()),
            line(7000, "0000000000000000"),
            line(20, "")
        );
        std::fs::write(g.join("turns.jsonl"), text).unwrap();
        assert_eq!(s.saved_today(now), Some(120));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_gateway_time_form_parses() {
        assert_eq!(parse_rfc3339("1970-01-02T00:00:00+00:00"), Some(86400.0));
        assert_eq!(parse_rfc3339("1970-01-01T01:00:00+01:00"), Some(0.0));
        assert_eq!(parse_rfc3339("1970-01-01T00:00:01.5Z"), Some(1.5));
        assert_eq!(parse_rfc3339("2026-10-14T09:30:00"), None);
        assert_eq!(parse_rfc3339("2026-10-14 09:30:00Z"), None);
        assert_eq!(parse_rfc3339("x"), None);
    }
}
