//! The knn router on the command line: the router the gateway builds, the
//! `route --knn` view, `route feedback`, and the knn part of
//! `router status`. The Go client's `routeknn.go` and `routecmd.go` are the
//! reference.

use std::cell::RefCell;
use std::rc::Rc;
use std::sync::{Arc, Mutex};

use super::gateroot::{join, path_str};
use super::{exit, Env, Res};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{Object, Value};
use crate::gateway::knn::{self, EmbedFn, Knn, OutcomeStore, View};
use crate::gateway::meter::default_prices;
use crate::pathways::find::Embedder;
use crate::pathways::potion;

/// One booked turn in `outcome_summary`'s recent list.
pub(super) struct KnnTurn {
    pub turn_id: String,
    pub model: String,
    pub outcome: String,
}

/// `outcome_summary`: counts by outcome and the last few booked turns,
/// oldest first.
pub(super) struct KnnState {
    pub path: String,
    pub booked: usize,
    pub good: usize,
    pub bad: usize,
    pub unknown: usize,
    pub recent: Vec<KnnTurn>,
}

pub(super) fn knn_summary(path: &str) -> Result<KnnState, String> {
    let rows = OutcomeStore { path: path.into() }.all(knn::MAX_ROWS)?;
    let count = |o: &str| rows.iter().filter(|r| r.outcome == o).count();
    let recent = rows
        .iter()
        .take(10)
        .rev()
        .map(|r| KnnTurn { turn_id: r.turn_id.clone(), model: r.model.clone(), outcome: r.outcome.clone() })
        .collect();
    Ok(KnnState {
        path: path.into(),
        booked: rows.len(),
        good: count(knn::GOOD),
        bad: count(knn::BAD),
        unknown: count(knn::UNKNOWN),
        recent,
    })
}

impl KnnState {
    pub(super) fn object(&self) -> Object {
        let mut o = Object::new();
        o.set("path", self.path.as_str());
        for (k, n) in [("booked", self.booked), ("good", self.good), ("bad", self.bad), ("unknown", self.unknown)] {
            o.set(k, Value::Int(n.to_string()));
        }
        let rs = self
            .recent
            .iter()
            .map(|t| {
                let mut r = Object::new();
                r.set("turn_id", t.turn_id.as_str());
                r.set("model", t.model.as_str());
                r.set("outcome", t.outcome.as_str());
                Value::Obj(r)
            })
            .collect();
        o.set("recent", Value::List(rs));
        o
    }
}

/// The knn key of `route --json`.
pub(super) fn view_object(v: &View) -> Object {
    let mut o = Object::new();
    if !v.err.is_empty() {
        o.set("error", v.err.as_str());
        return o;
    }
    o.set("model", v.model.as_str());
    o.set("fallback", v.fallback);
    o.set("reason", v.reason.as_str());
    o.set("known", Value::Int(v.known.to_string()));
    o.set("threshold", v.threshold);
    let ns = v
        .neighbours
        .iter()
        .map(|n| {
            let mut x = Object::new();
            x.set("turn_id", n.turn_id.as_str());
            x.set("similarity", n.similarity);
            x.set("model", n.model.as_str());
            x.set("outcome", n.outcome.as_str());
            Value::Obj(x)
        })
        .collect();
    o.set("neighbours", Value::List(ns));
    let ss = v
        .scores
        .iter()
        .map(|(m, s)| {
            let mut x = Object::new();
            x.set("model", m.as_str());
            x.set("score", *s);
            Value::Obj(x)
        })
        .collect();
    o.set("scores", Value::List(ss));
    o
}

impl Env {
    /// `build_default_gateway`'s KnnRouter: the outcome table under the data
    /// dir, the active matcher as the embedder, and its threshold. A matcher
    /// this binary cannot run gives a threshold of 1.0 and an embedder that
    /// fails, so every turn falls back to the rules router.
    pub(super) fn knn_router(&self, data_dir: &str) -> Arc<Knn> {
        let notes = Rc::new(RefCell::new(String::new()));
        let (key, identity, threshold, usable) = match self.matcher_pick(&notes) {
            Ok(pick) => match pick.matcher {
                Some(m) => (pick.key, m.identity().to_string(), m.threshold(), true),
                None => (pick.key, String::new(), 1.0, false),
            },
            Err(_) => (String::new(), String::new(), 1.0, false),
        };
        let vars = self.env.clone();
        let home = self.home.clone();
        let loaded: Mutex<Option<Embedder>> = Mutex::new(None);
        let embed: EmbedFn = Box::new(move |text: &str| {
            if !usable {
                return Err("no usable matcher".to_string());
            }
            let mut slot = loaded.lock().map_err(|_| "embedder lock".to_string())?;
            if slot.is_none() {
                *slot = Some(match key.as_str() {
                    "lexical" => Embedder::Lexical,
                    _ => {
                        let pe = potion::Env::from_process(vars.clone(), home.clone());
                        match potion::open(&identity, &pe) {
                            Ok(m) => Embedder::Potion(Box::new(m)),
                            Err(potion::NotAvailable(why)) => return Err(why),
                        }
                    }
                });
            }
            match slot.as_ref() {
                Some(e) => Ok((knn::to_f32(&e.encode(text)), identity.clone())),
                None => Err("no embedder".to_string()),
            }
        });
        Arc::new(Knn { store: OutcomeStore { path: join(&join(data_dir, "gateway"), "routing.db") }, embed, threshold })
    }

    /// `knn_view`: the knn router's choice for a task, read only.
    pub(super) fn knn_view(&self, task: &str, data_dir: &str, requested: &str, cheap: &str) -> Result<View, String> {
        let k = self.knn_router(data_dir);
        match (k.embed)(task) {
            Err(_) => Ok(View { err: "could not embed the task".into(), ..Default::default() }),
            Ok((vec, identity)) => {
                knn::view_of(&vec, &identity, &k.store, requested, cheap, &default_prices(), k.threshold)
            }
        }
    }

    /// `daisugi route feedback <turn-id> good|bad`.
    pub(super) fn route_feedback(&mut self, cmd: &str, rest: &[String], data_dir: &str, json: bool) -> Res {
        if rest.len() != 2 {
            return self.usage(cmd, "Invalid value: usage: daisugi route feedback <turn-id> good|bad".into());
        }
        let (turn_id, outcome) = (&rest[0], &rest[1]);
        if outcome != knn::GOOD && outcome != knn::BAD {
            return self.usage(cmd, format!("Invalid value: the outcome must be good or bad, not {}", repr(outcome)));
        }
        let store = OutcomeStore { path: join(&join(&path_str(data_dir), "gateway"), "routing.db") };
        match store.set_outcome(turn_id, outcome, knn::SOURCE_HUMAN) {
            Err(why) => self.fail(cmd, &why),
            Ok(false) => {
                self.errf(&format!(
                    "no booked turn {}.\nthe knn router books turns in <data-dir>/gateway/routing.db.\n\
                     see the turn ids with: daisugi router status\n",
                    repr(turn_id)
                ));
                exit(1)
            }
            Ok(true) => {
                if json {
                    let mut o = Object::new();
                    o.set("turn_id", turn_id.as_str());
                    o.set("outcome", outcome.as_str());
                    self.out(&format!("{}\n", crate::gate::pyjson::dumps(&Value::Obj(o), true)));
                } else {
                    self.out(&format!("turn {turn_id}: outcome set to {outcome}\n"));
                }
                Ok(())
            }
        }
    }
}
