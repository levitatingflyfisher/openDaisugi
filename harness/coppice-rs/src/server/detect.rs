//! The manifest tick and pane.explain: what the floor reads on a harness
//! pane's screen. The tick is the source of last resort. A pane held back
//! by a fresher source, a gate ask or done is not read at all.

use std::sync::Arc;
use std::thread;
use std::time::Duration;

use serde_json::{json, Value};

use super::panes::LivePane;
use super::{Client, Server};
use crate::detect::{self, Input, Verdict};
use crate::gojson;
use crate::layout;
use crate::pane::GRID_CLOSED;
use crate::proto::{self, Request, Response};
use crate::state::{self, Event};

/// How often a pty pane with no better source is read.
pub const MANIFEST_TICK: Duration = Duration::from_millis(500);

/// How long an ordinary higher source holds the tick back, on the
/// server's receive clock.
pub const GATE_QUIET: f64 = 2.0;

/// The rule that sees Claude Code's folder trust screen.
pub const TRUST_RULE: &str = "coppice_first_run_trust";

/// The detail of a state event the tick sends: the rule's words when it
/// has some, then the rule and the region.
pub fn manifest_detail(v: &Verdict) -> String {
    let ids = format!("rule={} region={}", v.rule_id, v.region);
    if v.rule_id == TRUST_RULE {
        return format!("asks to trust this folder ({ids})");
    }
    ids
}

/// A unix time as Go's RFC 3339 in UTC, to the second.
pub fn rfc3339_utc(secs: i64) -> String {
    let days = secs.div_euclid(86400);
    let rem = secs.rem_euclid(86400);
    // Howard Hinnant's civil_from_days.
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z",
        rem / 3600,
        rem % 3600 / 60,
        rem % 60
    )
}

fn pane_closed(r: &Request, id: &str) -> Response {
    proto::err_resp(
        &r.id,
        proto::ERR_PANE_CLOSED,
        &format!("pane {id} has closed. Create a new one with: coppice pane create"),
    )
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle("pane.explain", Arc::new(|s, c, r| s.handle_explain(c, r)));
    let sw = s.clone();
    thread::spawn(move || sw.run_manifest_tick());
}

impl Server {
    /// The manifests the tick and the prompt rules read.
    pub fn detect_set(&self) -> Option<Arc<detect::Set>> {
        self.detect_set
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// A pane's detection text, title and progress.
    pub fn detection_input(lp: &LivePane) -> Result<(Input, String), String> {
        let text = lp.grid.read("detection")?;
        Ok((
            Input {
                screen: text.clone(),
                osc_title: lp.grid.title(),
                osc_progress: lp.grid.progress(),
            },
            text,
        ))
    }

    /// Why the tick would not read this pane's screen now, or "".
    pub fn tick_skip_reason(&self, pane: &str, now: f64) -> String {
        if let Some(rec) = self.tree.pane(pane) {
            if rec.kind != layout::KIND_PTY {
                return "skipped: headless pane, not scanned".into();
            }
        }
        let Some(ev) = self.states.current(pane) else {
            return String::new();
        };
        if ev.state == state::STATE_DONE {
            return "skipped: done".into();
        }
        if ev.state == state::STATE_BLOCKED && ev.source == state::SRC_GATE {
            if let Some(a) = &ev.ask {
                if now < a.deadline {
                    return format!(
                        "skipped: gate blocked on an ask until {}",
                        rfc3339_utc(a.deadline as i64)
                    );
                }
            }
        }
        if ev.source != state::SRC_MANIFEST {
            if let Some(received) = self.states.received(pane) {
                if now - received < GATE_QUIET {
                    return format!(
                        "skipped: {} spoke {}s ago",
                        ev.source,
                        (now - received) as i64
                    );
                }
            }
        }
        String::new()
    }

    fn run_manifest_tick(self: Arc<Server>) {
        loop {
            if self.tick_stop.wait(Some(MANIFEST_TICK)) || self.is_closed() {
                return;
            }
            if let Some(set) = self.detect_set() {
                self.scan_once(&set);
            }
        }
    }

    /// One pass of the tick over every pane in the tree.
    fn scan_once(&self, set: &detect::Set) {
        if self.is_closed() {
            return;
        }
        let now = Server::now_seconds();
        for rec in self.tree.panes() {
            if rec.closed || rec.kind != layout::KIND_PTY || rec.harness.is_empty() {
                continue;
            }
            let Some(c) = set.get(&rec.harness) else {
                continue;
            };
            if !self.tick_skip_reason(&rec.id, now).is_empty() {
                continue;
            }
            let Some(lp) = self.live(&rec.id) else {
                continue;
            };
            let Ok((input, _)) = Server::detection_input(&lp) else {
                continue;
            };
            let v = c.evaluate(&input);
            if !v.matched || v.skip {
                continue;
            }
            self.apply_state(
                &rec.id,
                Event {
                    v: 1,
                    ts: now,
                    session_id: rec.id.clone(),
                    harness: rec.harness.clone(),
                    pane: Some(rec.id.clone()),
                    state: v.state.clone(),
                    source: state::SRC_MANIFEST.into(),
                    detail: manifest_detail(&v),
                    ..Default::default()
                },
            );
        }
    }

    /// pane.explain: the merged state a reader sees beside the manifest's
    /// own opinion of the screen, every rule the evaluator visited, and
    /// what the tick would do now.
    fn handle_explain(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let id = r.s("pane");
        if self.live(&id).is_none() {
            return pane_closed(r, &id);
        }
        let (input, text) = match Server::detection_input(&lp) {
            Ok(x) => x,
            Err(e) if e == GRID_CLOSED => return pane_closed(r, &id),
            Err(e) => return proto::err_resp(&r.id, proto::ERR_INTERNAL, &e),
        };
        let Some(rec) = self.tree.pane(&id) else {
            return pane_closed(r, &id);
        };
        let harness = rec.harness.clone();
        let now = Server::now_seconds();
        let mut out: Vec<(&str, Value)> = vec![
            ("pane", json!(id)),
            ("harness", json!(harness)),
            ("detection_text", json!(text)),
            ("osc_title", json!(input.osc_title)),
            ("osc_progress", json!(input.osc_progress)),
            ("detection_warnings", json!(self.detection_warnings())),
        ];
        if let Some(cur) = self.states.current(&id) {
            let eff = state::effective_state(&cur, now);
            out.push(("effective_state", json!(eff.state)));
            out.push(("effective_source", json!(eff.source)));
        }
        if let Some(received) = self.states.received(&id) {
            out.push((
                "received_age_s",
                gojson::float(((now - received) * 10.0).round() / 10.0),
            ));
        }
        if harness.is_empty() {
            out.push(("tick", json!("skipped: no harness")));
            out.push((
                "note",
                json!("this pane has no harness, so no manifest applies. Create it with --harness NAME to enable screen detection."),
            ));
            return proto::ok_resp(&r.id, gojson::map(out));
        }
        let set = self.detect_set();
        let Some(c) = set.as_ref().and_then(|s| s.get(&harness)) else {
            out.push((
                "note",
                json!(format!(
                    "no manifest for harness {}. Run: coppice agent list",
                    crate::sys::go_quote(&harness)
                )),
            ));
            return proto::ok_resp(&r.id, gojson::map(out));
        };
        let v = c.evaluate(&input);
        let source = set.as_ref().map(|s| s.source(&v.agent)).unwrap_or_default();
        out.push(("agent", json!(v.agent)));
        out.push(("source_file", json!(source)));
        out.push(("matched", json!(v.matched)));
        out.push(("rule_id", json!(v.rule_id)));
        out.push(("priority", json!(v.priority)));
        out.push(("region", json!(v.region)));
        out.push(("manifest_state", json!(v.state)));
        out.push(("skip_state_update", json!(v.skip)));
        out.push((
            "evaluated",
            Value::Array(v.evaluated.iter().map(|e| e.to_value()).collect()),
        ));
        if !v.matched {
            out.push((
                "note",
                json!("no rule matched, so no state is claimed. The floor shows unknown rather than guessing idle."),
            ));
        }
        let reason = self.tick_skip_reason(&rec.id, now);
        let tick = if !reason.is_empty() {
            reason
        } else if !v.matched {
            "skipped: no rule matched".into()
        } else if v.skip {
            "skipped: rule sets skip_state_update".into()
        } else {
            "applied".into()
        };
        out.push(("tick", json!(tick)));
        proto::ok_resp(&r.id, gojson::map(out))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rfc3339_matches_go() {
        assert_eq!(rfc3339_utc(0), "1970-01-01T00:00:00Z");
        assert_eq!(rfc3339_utc(4102444800), "2100-01-01T00:00:00Z");
        assert_eq!(rfc3339_utc(1757300090), "2025-09-08T02:54:50Z");
    }
}
