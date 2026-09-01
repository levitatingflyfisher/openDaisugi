//! `gateway_pipeline.Gateway`: prepare before the upstream call, finish
//! after it.

use std::collections::{HashMap, VecDeque};

use super::answers::{append_line, capture_answer};
use super::meter::{default_prices, measure, record_turn, strictly_cheaper, Price, Prices, Record};
use super::pyops::{get, loads_bytes, PyR, Raise};
use super::route::{latest_user_text, new_user_text, route_turn, conversation_key, Decision, DEFAULT_CHEAP_MODEL};
use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

pub const MODE_RULES: &str = "rules";
pub const MODE_EXTERNAL: &str = "external";
pub const MODE_OFF: &str = "off";
pub const EXTERNAL_TIER: &str = "tier-switchyard";
pub const OFF_TIER: &str = "tier-off";
/// The model an external turn is booked under when the router did not
/// name a clear target.
pub const UNKNOWN_TARGET: &str = "unknown";
const MAX_SESSIONS: usize = 4096;

/// ExternalRouterConfig.
#[derive(Debug, Clone, Default)]
pub struct External {
    pub route_id: String,
    pub capable_target: String,
    pub efficient_target: String,
    pub prices: Prices,
}

/// The settings a Gateway is built from.
#[derive(Debug, Clone, Default)]
pub struct Settings {
    pub cheap_model: String,
    pub local_model: String,
    pub router_mode: String,
    pub external: Option<External>,
    /// "" when journalling is off.
    pub journal_path: String,
    /// "" unless answers are captured.
    pub answers_path: String,
    pub capture_answers: bool,
    pub max_answers: usize,
}

/// `gateway_pipeline.Gateway`.
pub struct Gateway {
    pub cheap_model: String,
    pub local_model: String,
    pub prices: Prices,
    pub router_mode: String,
    pub external: Option<External>,
    pub journal_path: String,
    pub answers_path: String,
    pub capture_answers: bool,
    pub max_answers: usize,
    /// The sticky table: conversation key to the last routed model, first
    /// in first out past MAX_SESSIONS.
    session_keys: VecDeque<String>,
    sessions: HashMap<String, Value>,
}

/// PreparedTurn.
#[derive(Debug, Clone)]
pub struct Prepared {
    pub decision: Decision,
    pub outbound: Object,
    pub task: String,
    pub ask: String,
    /// When `prepare` began; `finish` books the time from here as the
    /// record's elapsed_ms. None books none.
    pub started: Option<std::time::Instant>,
}

impl Gateway {
    /// `Gateway(...)` with `__post_init__`: a local model prices at zero,
    /// and external mode adds its targets' prices.
    pub fn new(s: Settings) -> Result<Gateway, String> {
        let cheap = if s.cheap_model.is_empty() { DEFAULT_CHEAP_MODEL.to_string() } else { s.cheap_model };
        let mode = if s.router_mode.is_empty() { MODE_RULES.to_string() } else { s.router_mode };
        let mut prices = default_prices();
        if !s.local_model.is_empty() {
            prices.entry(s.local_model.clone()).or_insert(Price::default());
        }
        match mode.as_str() {
            MODE_RULES | MODE_OFF => {}
            MODE_EXTERNAL => match &s.external {
                None => return Err("router_mode 'external' needs an ExternalRouterConfig".into()),
                Some(e) => {
                    for (k, v) in &e.prices {
                        prices.insert(k.clone(), *v);
                    }
                }
            },
            _ => {
                return Err(format!(
                    "router_mode must be one of rules, external, off, not {}",
                    crate::gate::py::text::repr(&mode)
                ))
            }
        }
        Ok(Gateway {
            cheap_model: cheap,
            local_model: s.local_model,
            prices,
            router_mode: mode,
            external: s.external,
            journal_path: s.journal_path,
            answers_path: s.answers_path,
            capture_answers: s.capture_answers,
            max_answers: if s.max_answers == 0 { 1000 } else { s.max_answers },
            session_keys: VecDeque::new(),
            sessions: HashMap::new(),
        })
    }

    /// `Gateway.prepare`. An error is a raise: the proxy then forwards the
    /// original bytes untouched.
    pub fn prepare(&mut self, body: &Value) -> PyR<Prepared> {
        let started = std::time::Instant::now();
        let mut p = self.prepare_inner(body)?;
        p.started = Some(started);
        Ok(p)
    }

    fn prepare_inner(&mut self, body: &Value) -> PyR<Prepared> {
        match self.router_mode.as_str() {
            MODE_EXTERNAL => return self.prepare_passing(body, true),
            MODE_OFF => return self.prepare_passing(body, false),
            _ => {}
        }
        let task = latest_user_text(body)?;
        let ask = new_user_text(body)?;
        let key = conversation_key(body)?;
        let sticky = self.sessions.get(&key).cloned();
        let d = route_turn(body, &self.cheap_model, &self.local_model, sticky.as_ref())?;
        if sticky.is_none() {
            if self.session_keys.len() >= MAX_SESSIONS {
                if let Some(old) = self.session_keys.pop_front() {
                    self.sessions.remove(&old);
                }
            }
            self.session_keys.push_back(key.clone());
        }
        self.sessions.insert(key, d.model.clone());
        let Value::Obj(o) = body else { return Err(Raise::Raise) };
        let mut out = o.clone();
        if d.downgraded {
            out.set("model", d.model.clone());
        }
        Ok(Prepared { decision: d, outbound: out, task, ask, started: None })
    }

    /// The external and off modes: the body goes as it came (external
    /// names the route as its model), only metered.
    fn prepare_passing(&self, body: &Value, external: bool) -> PyR<Prepared> {
        let requested = match get(body, "model")? {
            Some(Value::Str(s)) => Value::Str(s.clone()),
            _ => Value::Str(String::new()),
        };
        let Value::Obj(o) = body else { return Err(Raise::Raise) };
        let mut out = o.clone();
        let d = if external {
            let route = self.external.as_ref().map(|e| e.route_id.clone()).unwrap_or_default();
            out.set("model", route.as_str());
            Decision {
                tier: EXTERNAL_TIER.into(),
                model: Value::Str(route.clone()),
                requested_model: requested,
                difficulty: 0.0,
                downgraded: false,
                reason: format!("the external router picks the model; sent as route {}", text::repr(&route)),
            }
        } else {
            Decision {
                tier: OFF_TIER.into(),
                model: requested.clone(),
                requested_model: requested,
                difficulty: 0.0,
                downgraded: false,
                reason: "routing is off; the turn goes unchanged and is only metered".into(),
            }
        };
        let task = latest_user_text(body)?;
        let ask = new_user_text(body)?;
        Ok(Prepared { decision: d, outbound: out, task, ask, started: None })
    }

    /// `_external_decision`: the decision an external turn is booked with
    /// once the response names its target.
    pub fn external_decision(&self, p: &Prepared, served: Option<&str>) -> PyR<Decision> {
        let mut d = p.decision.clone();
        let ext = self.external.as_ref();
        let served = match served {
            Some(s) if ext.is_none_or(|e| s != e.route_id) => s,
            _ => {
                d.model = Value::Str(UNKNOWN_TARGET.into());
                d.downgraded = false;
                d.reason = "the external router did not name a clear target; booked no saving".into();
                return Ok(d);
            }
        };
        let mut down = false;
        if ext.is_some_and(|e| served == e.efficient_target) {
            down = strictly_cheaper(&self.prices, &Value::Str(served.into()), &d.requested_model)?;
        }
        d.model = Value::Str(served.into());
        d.downgraded = down;
        d.reason = if down {
            format!("the external router served the efficient target {served}")
        } else {
            format!("the external router served {served}; booked no saving")
        };
        Ok(d)
    }

    /// `Gateway.finish`: measure the turn, append its record, and capture
    /// the answer when capture is on. An error is a raise the proxy
    /// swallows: the turn was served, the line is not written.
    pub fn finish(&self, p: &Prepared, usage: &Value, answer: &str, now_ns: i128) -> PyR<Record> {
        let mut d = p.decision.clone();
        if d.downgraded && !strictly_cheaper(&self.prices, &d.model, &d.requested_model)? {
            d.downgraded = false;
            d.reason.push_str("; the price table shows no saving, so none is booked");
        }
        let s = measure(&self.prices, &d, usage)?;
        let mut rec = record_turn(&d, &s, &p.task, &p.ask, now_ns.div_euclid(1_000_000_000) as i64)?;
        rec.elapsed_ms = p.started.map(|t| pyjson::round(t.elapsed().as_secs_f64() * 1000.0, 3));
        if !self.journal_path.is_empty() {
            append_line(&self.journal_path, &rec.json()).map_err(|_| Raise::Raise)?;
        }
        if self.capture_answers && !self.answers_path.is_empty() && !answer.is_empty() && !text::strip(&p.ask).is_empty() {
            // Best effort, as the oracle's: a failure never breaks the turn.
            let _ = capture_answer(&self.answers_path, self.max_answers, &p.task, answer, now_ns);
        }
        Ok(rec)
    }
}

/// `_prepare`: the bytes to send, the prepared turn, and whether the client
/// asked for a stream. Any raise forwards the original bytes untouched.
pub fn prepare_bytes(gw: &mut Gateway, body: &[u8]) -> (Vec<u8>, Option<Prepared>, bool) {
    let v = match loads_bytes(body) {
        Ok(v) => v,
        Err(_) => return (body.to_vec(), None, false),
    };
    let p = match gw.prepare(&v) {
        Ok(p) => p,
        Err(_) => return (body.to_vec(), None, false),
    };
    let stream = matches!(&v, Value::Obj(o) if o.get("stream") == Some(&Value::Bool(true)));
    let out = pyjson::dumps(&Value::Obj(p.outbound.clone()), true).into_bytes();
    (out, Some(p), stream)
}

/// The stream flag of a body no gateway routes: `json.loads(body).get(
/// "stream") is True`, false on any raise.
pub fn stream_flag(body: &[u8]) -> bool {
    matches!(loads_bytes(body), Ok(Value::Obj(o)) if o.get("stream") == Some(&Value::Bool(true)))
}

/// `_record`: measure and journal the turn, best effort. `original` is set
/// when the fail-open retry served the requested model after the
/// downgrade was refused.
pub fn record(gw: &Gateway, p: &Prepared, usage: &Value, original: bool, answer: &str, served: Option<&str>, now_ns: i128) {
    let mut eff = p.clone();
    if gw.router_mode == MODE_EXTERNAL {
        match gw.external_decision(p, served) {
            Ok(d) => eff.decision = d,
            Err(_) => return,
        }
    } else if original {
        let d = &mut eff.decision;
        d.model = d.requested_model.clone();
        d.tier = "tier2-frontier".into();
        d.downgraded = false;
        d.reason = "downgrade rejected by upstream 4xx; served the original model".into();
    }
    let _ = gw.finish(&eff, usage, answer, now_ns);
}

/// The time now in nanoseconds since the epoch.
pub fn now_ns() -> i128 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_nanos() as i128).unwrap_or(0)
}
