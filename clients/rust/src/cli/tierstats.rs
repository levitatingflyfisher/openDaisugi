//! `daisugi tiers stats`: per-tier call counts, estimated tokens and the
//! pathway hit rate over the journal (`accounting.tier_stats`). The Go
//! client's `cli/tierstats.go` is the twin.

use super::statuscmd::py_fixed;
use super::{parse_args, Env, Opt, Res};
use crate::gate::pyjson::{dumps_indent, Object, Value};

const TIERS: [&str; 3] = ["tier0", "tier1", "tier2"];

/// `accounting._ESTIMATED_TOKENS_PER_CALL`.
fn tier_tokens(tier: &str) -> i128 {
    match tier {
        "tier1" => 2000,
        "tier2" => 4500,
        _ => 0,
    }
}

/// `accounting.classify_tier`.
fn classify_tier(generated_by: &str) -> &'static str {
    if generated_by.starts_with("compiled-pathway:") {
        "tier0"
    } else if generated_by.starts_with("tier1:") {
        "tier1"
    } else {
        "tier2"
    }
}

/// `format(n, ",")`.
fn group_thousands(n: i128) -> String {
    let s = n.unsigned_abs().to_string();
    let mut out = String::new();
    for (i, c) in s.chars().enumerate() {
        if i > 0 && (s.len() - i) % 3 == 0 {
            out.push(',');
        }
        out.push(c);
    }
    if n < 0 {
        format!("-{out}")
    } else {
        out
    }
}

/// Days from 1970-01-01 to a civil date (proleptic Gregorian).
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

/// `accounting._row_ts` on a created_at string: `datetime.fromisoformat`
/// (a trailing Z as +00:00) and `.timestamp()`, a naive time read in the
/// local zone. The forms read: a date, then `T` or a space and HH:MM,
/// :SS and a fraction, then an offset. None for any other text.
fn iso_timestamp(raw: &str) -> Option<f64> {
    let raw = match raw.strip_suffix('Z') {
        Some(r) => format!("{r}+00:00"),
        None => raw.to_string(),
    };
    let b = raw.as_bytes();
    let num = |s: &str| -> Option<i64> {
        (!s.is_empty() && s.bytes().all(|c| c.is_ascii_digit())).then(|| s.parse().ok()).flatten()
    };
    if b.len() < 10 || b[4] != b'-' || b[7] != b'-' {
        return None;
    }
    let (y, mo, d) = (num(&raw[0..4])?, num(&raw[5..7])?, num(&raw[8..10])?);
    if !(1..=12).contains(&mo) || d < 1 || d > 31 {
        return None;
    }
    let mut rest = &raw[10..];
    let (mut h, mut mi, mut sec, mut frac) = (0i64, 0i64, 0i64, 0f64);
    if !rest.is_empty() {
        if !(rest.starts_with('T') || rest.starts_with(' ')) || rest.len() < 6 || rest.as_bytes()[3] != b':' {
            return None;
        }
        h = num(&rest[1..3])?;
        mi = num(&rest[4..6])?;
        rest = &rest[6..];
        if rest.starts_with(':') {
            sec = num(rest.get(1..3)?)?;
            rest = &rest[3..];
            if let Some(r) = rest.strip_prefix('.') {
                let n = r.bytes().take_while(|c| c.is_ascii_digit()).count();
                if !(1..=6).contains(&n) {
                    return None;
                }
                frac = format!("0.{}", &r[..n]).parse().ok()?;
                rest = &r[n..];
            }
        }
        if h > 23 || mi > 59 || sec > 59 {
            return None;
        }
    }
    let wall = days_from_civil(y, mo, d) * 86400 + h * 3600 + mi * 60 + sec;
    if rest.is_empty() {
        // A naive time is local time.
        let mut tm: libc::tm = unsafe { std::mem::zeroed() };
        tm.tm_year = (y - 1900) as i32;
        tm.tm_mon = (mo - 1) as i32;
        tm.tm_mday = d as i32;
        tm.tm_hour = h as i32;
        tm.tm_min = mi as i32;
        tm.tm_sec = sec as i32;
        tm.tm_isdst = -1;
        let t = unsafe { libc::mktime(&mut tm) };
        return (t != -1).then(|| t as f64 + frac);
    }
    let sign = match rest.as_bytes()[0] {
        b'+' => 1,
        b'-' => -1,
        _ => return None,
    };
    let off = &rest[1..];
    if off.len() != 5 || off.as_bytes()[2] != b':' {
        return None;
    }
    let (oh, om) = (num(&off[0..2])?, num(&off[3..5])?);
    Some((wall - sign * (oh * 3600 + om * 60)) as f64 + frac)
}

impl Env {
    pub(super) fn tiers_stats(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tiers stats";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", ""),
            Opt::val(&["--days"], "INTEGER", "Rollup window (days)."),
            Opt::flag(&["--json"], "Emit stats as JSON."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Show per-tier call counts, estimated tokens, and pathway hit rate.", &opts);
        }
        let days = self.click_int(CMD, &p, "--days", 30)?;
        let dir = self.data_dir_of(&p);
        let j = self.open_journal(CMD, &dir)?;
        let now =
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0);
        let cutoff = now - days as f64 * 86400.0;
        let rows = j.list_successful(Some(cutoff)).map_err(|e| self.pw_err(CMD, e))?;
        let mut by_tier = [0i128; 3];
        let mut providers: Vec<(String, i128)> = vec![];
        let mut total: i128 = 0;
        for row in rows {
            if row.trace_id.is_empty() {
                continue;
            }
            let rec = match j.load_trace(&row.trace_id).map_err(|e| self.pw_err(CMD, e))? {
                Ok(r) => r,
                // tier_stats skips a trace it cannot load.
                Err(_) => continue,
            };
            if iso_timestamp(&row.created_at).is_some_and(|ts| ts < cutoff) {
                continue;
            }
            let gb = rec.envelope.value("generated_by").as_str().unwrap_or("").to_string();
            let tier = classify_tier(&gb);
            by_tier[TIERS.iter().position(|t| *t == tier).unwrap_or(2)] += 1;
            total += 1;
            if tier == "tier1" {
                let name = match &gb["tier1:".len()..] {
                    "" => "unknown",
                    n => n,
                };
                match providers.iter_mut().find(|(k, _)| k == name) {
                    Some((_, n)) => *n += 1,
                    None => providers.push((name.to_string(), 1)),
                }
            }
        }
        let tokens: Vec<i128> = TIERS.iter().enumerate().map(|(i, t)| by_tier[i] * tier_tokens(t)).collect();
        let tokens_total: i128 = tokens.iter().sum();
        let rate = if total > 0 { by_tier[0] as f64 / total as f64 } else { 0.0 };
        if p.flag("--json") {
            let tier_obj = |m: &[i128]| {
                let mut o = Object::new();
                for (i, t) in TIERS.iter().enumerate() {
                    o.set(t, Value::Int(m[i].to_string()));
                }
                o
            };
            let mut prov = Object::new();
            for (k, n) in &providers {
                prov.set(k, Value::Int(n.to_string()));
            }
            let o = Object::new()
                .with("window_days", Value::Int(days.to_string()))
                .with("total", Value::Int(total.to_string()))
                .with("by_tier", tier_obj(&by_tier))
                .with("by_tier1_provider", prov)
                .with("estimated_tokens", tier_obj(&tokens))
                .with("estimated_tokens_total", Value::Int(tokens_total.to_string()))
                .with("pathway_hit_rate", rate);
            self.echo(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        let mut b = format!("window: last {days}d\ntotal traces: {total}\n");
        for (i, t) in TIERS.iter().enumerate() {
            b.push_str(&format!("  {t}: {} call(s)  ~{} est tokens\n", by_tier[i], group_thousands(tokens[i])));
        }
        b.push_str(&format!("estimated tokens total: ~{}\n", group_thousands(tokens_total)));
        b.push_str(&format!("pathway hit rate: {}%\n", py_fixed(rate * 100.0, 1)));
        if !providers.is_empty() {
            b.push_str("tier1 breakdown:\n");
            // sorted(items, key=-count): stable, so a tie keeps insertion order.
            let mut sorted = providers.clone();
            sorted.sort_by(|a, c| c.1.cmp(&a.1));
            for (k, n) in sorted {
                b.push_str(&format!("  {k}: {n}\n"));
            }
        }
        self.echo(&b);
        Ok(())
    }
}
