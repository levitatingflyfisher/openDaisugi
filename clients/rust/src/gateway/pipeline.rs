//! `gateway_pipeline.Gateway`: prepare before the upstream call, finish
//! after it.

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex};

use super::answers::{append_line, capture_answer};
use super::knn::{self, Knn};
use super::model::Choice;
use super::meter::{default_prices, measure, record_turn, strictly_cheaper, Price, Prices, Record};
use super::pyops::{get, loads_bytes, PyR, Raise};
use super::route::{
    conversation_key, estimate_difficulty, estimate_prefix_tokens, latest_user_text, new_user_text, route_turn, Decision,
    DEFAULT_CHEAP_MODEL, STICKY_PREFIX_THRESHOLD,
};
use crate::gate::py::text;
use crate::gate::pyjson::{self, Object, Value};

pub const MODE_RULES: &str = "rules";
pub const MODE_EXTERNAL: &str = "external";
pub const MODE_OFF: &str = "off";
pub const MODE_KNN: &str = "knn";
pub const MODE_MODEL: &str = "model";
pub const EXTERNAL_TIER: &str = "tier-switchyard";
pub const OFF_TIER: &str = "tier-off";
/// The model an external turn is booked under when the router did not
/// name a clear target.
pub const UNKNOWN_TARGET: &str = "unknown";
const MAX_SESSIONS: usize = 4096;
/// The reason the router model was not asked.
pub const NOT_ASKED: &str = "not asked";

/// Orders the journal and answer-file writes of every gateway. It is not held
/// across the embedder or the outcome table.
static FILE_LOCK: Mutex<()> = Mutex::new(());

/// The sticky table: conversation key to the last routed model, first in
/// first out past MAX_SESSIONS.
#[derive(Debug, Default)]
struct Sessions {
    keys: VecDeque<String>,
    map: HashMap<String, Value>,
}

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
    /// The knn router, for MODE_KNN.
    pub knn: Option<Arc<Knn>>,
    /// The model that picks (MODE_MODEL) and the model that grades; "" for none.
    pub router_model: String,
    pub grader_model: String,
    /// "" when journalling is off.
    pub journal_path: String,
    /// "" unless answers are captured.
    pub answers_path: String,
    pub capture_answers: bool,
    pub max_answers: usize,
}

/// `gateway_pipeline.Gateway`. A clone shares the sticky table, so the proxy can
/// prepare a turn from a clone without holding the gateway lock.
#[derive(Clone)]
pub struct Gateway {
    pub cheap_model: String,
    pub local_model: String,
    pub prices: Prices,
    pub router_mode: String,
    pub external: Option<External>,
    pub knn: Option<Arc<Knn>>,
    pub router_model: String,
    pub grader_model: String,
    pub journal_path: String,
    pub answers_path: String,
    pub capture_answers: bool,
    pub max_answers: usize,
    sessions: Arc<Mutex<Sessions>>,
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
    /// The coppice pane and floor the turn came from, or empty. The proxy
    /// takes them from the request path; `finish` stamps the record.
    pub pane: String,
    pub floor: String,
    /// knn mode only: the task vector, the embedder that made it and the
    /// conversation key. `finish` books them; a turn with no vector is not
    /// booked.
    pub knn_vec: Option<Vec<f32>>,
    pub knn_embedder: String,
    pub conversation: String,
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
            MODE_KNN => {
                if s.knn.is_none() {
                    return Err("router_mode 'knn' needs a KnnRouter".into());
                }
            }
            MODE_MODEL => {
                if s.knn.is_none() || s.router_model.is_empty() {
                    return Err("router_mode 'model' needs a KnnRouter and a router_model".into());
                }
            }
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
                    "router_mode must be one of rules, external, off, knn, model, not {}",
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
            knn: s.knn,
            router_model: s.router_model,
            grader_model: s.grader_model,
            journal_path: s.journal_path,
            answers_path: s.answers_path,
            capture_answers: s.capture_answers,
            max_answers: if s.max_answers == 0 { 1000 } else { s.max_answers },
            sessions: Arc::new(Mutex::new(Sessions::default())),
        })
    }

    /// `Gateway.prepare`. An error is a raise: the proxy then forwards the
    /// original bytes untouched.
    pub fn prepare(&self, body: &Value) -> PyR<Prepared> {
        self.prepare_with(body, None)
    }

    /// `Gateway.prepare(body, choice)`: in model mode, `choice` is the router
    /// model's answer.
    pub fn prepare_with(&self, body: &Value, choice: Option<&Choice>) -> PyR<Prepared> {
        let started = std::time::Instant::now();
        let mut p = self.prepare_inner(body, choice)?;
        p.started = Some(started);
        Ok(p)
    }

    fn prepare_inner(&self, body: &Value, choice: Option<&Choice>) -> PyR<Prepared> {
        match self.router_mode.as_str() {
            MODE_EXTERNAL => return self.prepare_passing(body, true),
            MODE_OFF => return self.prepare_passing(body, false),
            MODE_KNN => return self.prepare_knn(body, None),
            MODE_MODEL => {
                let none = Choice { name: String::new(), why: NOT_ASKED.into() };
                return self.prepare_knn(body, Some(choice.unwrap_or(&none)));
            }
            _ => {}
        }
        let task = latest_user_text(body)?;
        let ask = new_user_text(body)?;
        let key = conversation_key(body)?;
        let sticky = self.sticky_of(&key);
        let d = route_turn(body, &self.cheap_model, &self.local_model, sticky.as_ref())?;
        self.remember(key, sticky.is_some(), d.model.clone());
        let Value::Obj(o) = body else { return Err(Raise::Raise) };
        let mut out = o.clone();
        if d.downgraded {
            out.set("model", d.model.clone());
        }
        Ok(Prepared {
            decision: d,
            outbound: out,
            task,
            ask,
            started: None,
            pane: String::new(),
            floor: String::new(),
            knn_vec: None,
            knn_embedder: String::new(),
            conversation: String::new(),
        })
    }

    fn sticky_of(&self, key: &str) -> Option<Value> {
        self.sessions.lock().ok()?.map.get(key).cloned()
    }

    fn remember(&self, key: String, had: bool, model: Value) {
        let Ok(mut s) = self.sessions.lock() else { return };
        if !had && !s.map.contains_key(&key) {
            if s.keys.len() >= MAX_SESSIONS {
                if let Some(old) = s.keys.pop_front() {
                    s.map.remove(&old);
                }
            }
            s.keys.push_back(key.clone());
        }
        s.map.insert(key, model);
    }

    /// `Gateway._prepare_knn`: route by the outcomes of similar past turns.
    /// With too few known neighbours, or no vector for the task, the rules
    /// router decides and the reason says why.
    fn prepare_knn(&self, body: &Value, choice: Option<&Choice>) -> PyR<Prepared> {
        let task = latest_user_text(body)?;
        let ask = new_user_text(body)?;
        let key = conversation_key(body)?;
        let sticky = self.sticky_of(&key);
        let had = sticky.is_some();
        let requested = get(body, "model")?.cloned().unwrap_or_else(|| Value::Str(String::new()));
        let rules = |gw: &Gateway| route_turn(body, &gw.cheap_model, &gw.local_model, sticky.as_ref());
        let fallback = |gw: &Gateway, prefix: &str| -> PyR<Decision> {
            let mut d = rules(gw)?;
            d.reason = format!("{prefix}; rules router: {}", d.reason);
            Ok(d)
        };
        let mut vec: Option<Vec<f32>> = None;
        let mut identity = String::new();
        let d = if text::strip(&task).is_empty() {
            rules(self)?
        } else if !matches!(&requested, Value::Str(s) if !s.is_empty()) {
            fallback(self, "knn: the request names no model")?
        } else {
            let Value::Str(req) = &requested else { return Err(Raise::Raise) };
            let k = self.knn.clone().ok_or(Raise::Raise)?;
            // In model mode the vector only books the turn, and a continuation is not booked.
            let embedded = if choice.is_none() || !text::strip(&ask).is_empty() { Some((k.embed)(&task)) } else { None };
            let (v, id) = match embedded {
                Some(Ok((v, id))) => (Some(v), id),
                _ => (None, String::new()),
            };
            vec = v;
            identity = id;
            if let Some(c) = choice {
                self.model_decision(body, c, sticky.as_ref(), &task, &ask, req)?
            } else if let Some(v) = &vec {
                let rows = k.store.recent(&identity, knn::MAX_ROWS).unwrap_or_default();
                let cands = knn::candidate_models(req, &self.cheap_model, &self.local_model, &self.prices);
                let kd = knn::decide(v, &rows, req, &cands, &self.prices, k.threshold, knn::K_NEIGHBOURS, knn::MIN_KNOWN);
                if kd.fallback {
                    fallback(self, &kd.reason)?
                } else {
                    self.knn_decision(&kd, body, sticky.as_ref(), &task, req)?
                }
            } else {
                fallback(self, "knn: could not embed the task")?
            }
        };
        self.remember(key.clone(), had, d.model.clone());
        let Value::Obj(o) = body else { return Err(Raise::Raise) };
        let mut out = o.clone();
        if d.downgraded {
            out.set("model", d.model.clone());
        }
        Ok(Prepared {
            decision: d,
            outbound: out,
            task,
            ask,
            started: None,
            pane: String::new(),
            floor: String::new(),
            knn_vec: vec,
            knn_embedder: identity,
            conversation: key,
        })
    }

    /// `Gateway._prepare_model`'s decision: the router model picked `c.name`,
    /// or failed with `c.why`. A name outside the list goes to the rules
    /// router. A continuation stays on the conversation's last model.
    fn model_decision(&self, body: &Value, c: &Choice, sticky: Option<&Value>, task: &str, ask: &str, requested: &str) -> PyR<Decision> {
        let cands = knn::candidate_models(requested, &self.cheap_model, &self.local_model, &self.prices);
        let (mut picked, mut why) = (c.name.clone(), c.why.clone());
        let mut how = format!("model: {picked} picked by {} from {}", self.router_model, cands.join(", "));
        let last = match sticky {
            Some(Value::Str(s)) if cands.contains(s) => Some(s.clone()),
            _ => None,
        };
        if let (true, Some(last)) = (picked.is_empty() && why == NOT_ASKED && text::strip(ask).is_empty(), last) {
            how = format!("model: {last} kept from the last turn of this conversation");
            picked = last;
        } else if !picked.is_empty() && !cands.contains(&picked) {
            picked = String::new();
            why = "the router model named a model that is not allowed".into();
        }
        if picked.is_empty() {
            let mut d = route_turn(body, &self.cheap_model, &self.local_model, sticky)?;
            d.reason = format!("model: {why}; rules router: {}", d.reason);
            return Ok(d);
        }
        let kd = knn::Decision { model: picked, fallback: false, reason: how, neighbours: vec![], known: 0, scores: vec![] };
        self.knn_decision(&kd, body, sticky, task, requested)
    }

    /// `Gateway._knn_decision`: a Decision from a knn choice, with ADR-0015
    /// stickiness on top.
    fn knn_decision(&self, kd: &knn::Decision, body: &Value, sticky: Option<&Value>, task: &str, requested: &str) -> PyR<Decision> {
        let difficulty = estimate_difficulty(task);
        let model = kd.model.as_str();
        let req = Value::Str(requested.into());
        if model == requested {
            return Ok(Decision {
                tier: "tier2-frontier".into(),
                model: req.clone(),
                requested_model: req,
                difficulty,
                downgraded: false,
                reason: kd.reason.clone(),
            });
        }
        let is_local = !self.local_model.is_empty() && model == self.local_model;
        let sticky_is_model = matches!(sticky, Some(Value::Str(s)) if s == model);
        if !is_local && !sticky_is_model {
            let prefix = estimate_prefix_tokens(body)?;
            if prefix >= STICKY_PREFIX_THRESHOLD {
                return Ok(Decision {
                    tier: "tier2-frontier".into(),
                    model: req.clone(),
                    requested_model: req,
                    difficulty,
                    downgraded: false,
                    reason: format!(
                        "{}; sticky: ~{prefix} prefix tokens cached against the requested model, so it stays",
                        kd.reason
                    ),
                });
            }
        }
        Ok(Decision {
            tier: if is_local { "tier1-local" } else { "tier1-cheap" }.into(),
            model: Value::Str(model.into()),
            requested_model: req,
            difficulty,
            downgraded: true,
            reason: kd.reason.clone(),
        })
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
        Ok(Prepared {
            decision: d,
            outbound: out,
            task,
            ask,
            started: None,
            pane: String::new(),
            floor: String::new(),
            knn_vec: None,
            knn_embedder: String::new(),
            conversation: String::new(),
        })
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

    /// `Gateway.pick_request`: the request that asks the router model to
    /// pick, and the allowed names. None when the turn has no new text (a
    /// continuation), names no model, or has no cheaper rung to pick.
    pub fn pick_request(&self, body: &Value) -> Option<(Vec<u8>, Vec<String>)> {
        let task = latest_user_text(body).ok()?;
        let ask = new_user_text(body).ok()?;
        let Some(Value::Str(req)) = get(body, "model").ok()? else { return None };
        if text::strip(&ask).is_empty() || text::strip(&task).is_empty() || req.is_empty() {
            return None;
        }
        let cands = knn::candidate_models(req, &self.cheap_model, &self.local_model, &self.prices);
        if cands.len() < 2 {
            return None;
        }
        Some((super::model::picker_body(&self.router_model, &task, &cands), cands))
    }

    /// `Gateway._book_knn`: book one knn turn in the outcome table. Best
    /// effort: it never breaks the turn.
    fn book_knn(&self, p: &Prepared, d: &Decision, s: &super::meter::Saving, elapsed: Option<f64>, failed: bool, now_ns: i128) -> Option<String> {
        let k = self.knn.as_ref()?;
        let vec = p.knn_vec.as_ref()?;
        if text::strip(&p.ask).is_empty() {
            return None;
        }
        let Value::Str(model) = &d.model else { return None };
        k.record(&p.conversation, &p.task, vec, &p.knn_embedder, model, s.actual.dollars, elapsed, failed, now_ns as f64 / 1e9)
            .ok()
    }

    /// `Gateway.finish`: measure the turn, append its record, and capture
    /// the answer when capture is on. An error is a raise the proxy
    /// swallows: the turn was served, the line is not written.
    pub fn finish(&self, p: &Prepared, usage: &Value, answer: &str, failed: bool, now_ns: i128) -> PyR<Record> {
        let mut d = p.decision.clone();
        if d.downgraded && !strictly_cheaper(&self.prices, &d.model, &d.requested_model)? {
            d.downgraded = false;
            d.reason.push_str("; the price table shows no saving, so none is booked");
        }
        let s = measure(&self.prices, &d, usage)?;
        let mut rec = record_turn(&d, &s, &p.task, &p.ask, now_ns.div_euclid(1_000_000_000) as i64)?;
        rec.elapsed_ms = p.started.map(|t| pyjson::round(t.elapsed().as_secs_f64() * 1000.0, 3));
        rec.pane = p.pane.clone();
        rec.floor = p.floor.clone();
        if self.router_mode == MODE_KNN || self.router_mode == MODE_MODEL {
            rec.reason = d.reason.clone();
            rec.turn_id = self.book_knn(p, &d, &s, rec.elapsed_ms, failed, now_ns).unwrap_or_default();
        }
        let _files = FILE_LOCK.lock().unwrap_or_else(|e| e.into_inner());
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
pub fn prepare_bytes(gw: &Gateway, body: &[u8]) -> (Vec<u8>, Option<Prepared>, bool) {
    prepare_bytes_with(gw, body, None)
}

/// `_prepare(gateway, body, choice)`.
pub fn prepare_bytes_with(gw: &Gateway, body: &[u8], choice: Option<&Choice>) -> (Vec<u8>, Option<Prepared>, bool) {
    let v = match loads_bytes(body) {
        Ok(v) => v,
        Err(_) => return (body.to_vec(), None, false),
    };
    let p = match gw.prepare_with(&v, choice) {
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
#[allow(clippy::too_many_arguments)]
pub fn record(gw: &Gateway, p: &Prepared, usage: &Value, original: bool, answer: &str, served: Option<&str>, failed: bool, now_ns: i128) -> Option<Record> {
    let mut eff = p.clone();
    if gw.router_mode == MODE_EXTERNAL {
        match gw.external_decision(p, served) {
            Ok(d) => eff.decision = d,
            Err(_) => return None,
        }
    } else if original {
        let d = &mut eff.decision;
        d.model = d.requested_model.clone();
        d.tier = "tier2-frontier".into();
        d.downgraded = false;
        d.reason = "downgrade rejected by upstream 4xx; served the original model".into();
    }
    gw.finish(&eff, usage, answer, failed, now_ns).ok()
}

/// The time now in nanoseconds since the epoch.
pub fn now_ns() -> i128 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_nanos() as i128).unwrap_or(0)
}
