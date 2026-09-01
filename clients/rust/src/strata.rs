//! The oracle's rationale-durability ledger (`opendaisugi/strata.py`): a
//! typed store of facts, hypotheses, constraints and goals kept outside
//! the transcript, a lossy reconstruction a harness calls after a
//! compaction, and the one path from the store to authority,
//! `promote_constraint`, which may only tighten the envelope. The Go
//! client's `internal/strata` is the reference.

use crate::envgen::inherit::verify_inheritance;
use crate::gate::py::text::{lower, repr};
use crate::gate::pyjson::{Object, Value};
use crate::lerr::{LErr, PyError, LR};
use crate::pathways::pathway::dump_json;
use crate::pathways::pmodel::{validate_json, validate_model, Id, Mode};

/// `StrataStore`. Each stratum is its model dump, in field order; a status
/// set later is kept as given (pydantic does not validate an assignment).
#[derive(Debug, Clone, Default)]
pub struct Store {
    strata: Vec<Object>,
    seq: i64,
}

fn seq_of(x: &Object) -> i64 {
    match x.value("seq") {
        Value::Int(t) => t.parse().unwrap_or(0),
        _ => 0,
    }
}

fn key_error(id: &str) -> LErr {
    PyError::new("KeyError", repr(id)).into()
}

impl Store {
    /// `emit()`: the sequence number is taken first, so an emit whose
    /// stratum does not validate still uses one.
    pub fn emit(
        &mut self,
        kind: &Value,
        content: &Value,
        provenance: &Value,
        status: &Value,
        tags: &Value,
        pinned: &Value,
    ) -> LR<Object> {
        self.seq += 1;
        // list(tags or []): a str gives its characters.
        let tag_list = match tags {
            Value::List(t) => Value::List(t.clone()),
            Value::Str(s) => Value::List(s.chars().map(|c| Value::Str(c.to_string())).collect()),
            _ => Value::List(vec![]),
        };
        let input = Object::new()
            .with("kind", kind.clone())
            .with("content", content.clone())
            .with("provenance", provenance.clone())
            .with("status", status.clone())
            .with("tags", tag_list)
            .with("pinned", pinned.clone())
            .with("seq", Value::Int(self.seq.to_string()));
        let Value::Obj(o) = validate_model(Id::Stratum, &Value::Obj(input), Mode::Python)? else {
            unreachable!("a model validates to an object")
        };
        self.strata.push(o.clone());
        Ok(o)
    }

    fn index(&self, id: &str) -> Option<usize> {
        self.strata
            .iter()
            .position(|x| x.value("id").as_str() == Some(id))
    }

    /// `get()`: the first stratum with the id.
    pub fn get(&self, id: &str) -> Option<&Object> {
        self.index(id).map(|i| &self.strata[i])
    }

    /// `set_status()`: KeyError for an unknown id.
    pub fn set_status(&mut self, id: &str, status: &Value) -> LR<Object> {
        let i = self.index(id).ok_or_else(|| key_error(id))?;
        self.strata[i].set("status", status.clone());
        Ok(self.strata[i].clone())
    }

    /// `repage()`: the stratum verbatim, or KeyError.
    pub fn repage(&self, id: &str) -> LR<Object> {
        self.get(id).cloned().ok_or_else(|| key_error(id))
    }

    /// `all()`.
    pub fn all(&self) -> Vec<Object> {
        self.strata.clone()
    }

    /// `by_kind()`.
    pub fn by_kind(&self, kind: &str) -> Vec<Object> {
        self.strata
            .iter()
            .filter(|x| x.value("kind").as_str() == Some(kind))
            .cloned()
            .collect()
    }

    /// `reconstruct_context()`: the pinned strata and open constraints
    /// always, then the most relevant rest up to `budget` (None is no
    /// budget), shown in the order they were found. The
    /// ReconstructedContext dump.
    pub fn reconstruct(&self, budget: Option<i64>, tags: &[String], query: &str) -> Object {
        let (pinned, mut cands): (Vec<&Object>, Vec<&Object>) =
            self.strata.iter().partition(|x| always_include(x));
        cands.sort_by(|a, b| relevance(b, tags, query).cmp(&relevance(a, tags, query)));
        let (selected, dropped) = match budget {
            None => (cands.clone(), vec![]),
            Some(b) => {
                let room = (b - pinned.len() as i64).clamp(0, cands.len() as i64) as usize;
                (cands[..room].to_vec(), cands[room..].to_vec())
            }
        };
        let mut chosen: Vec<&Object> = pinned.iter().chain(selected.iter()).copied().collect();
        chosen.sort_by_key(|x| seq_of(x));
        let mut note = format!(
            "Reconstruction is lossy: {} stratum(s) dropped — each a fact the agent will re-derive unless re-paged. {} \
             pinned/constraint stratum(s) always retained.",
            dropped.len(),
            pinned.len()
        );
        if let Some(b) = budget {
            if chosen.len() as i64 > b {
                note.push_str(&format!(
                    " Pinned/constraint strata ({}) exceed the budget ({b}); budget is a floor, not a ceiling — they \
                     are never dropped.",
                    pinned.len()
                ));
            }
        }
        let objs =
            |xs: &[&Object]| Value::List(xs.iter().map(|x| Value::Obj((*x).clone())).collect());
        Object::new()
            .with("strata", objs(&chosen))
            .with("pinned", objs(&pinned))
            .with(
                "dropped_ids",
                Value::List(dropped.iter().map(|x| x.value("id").clone()).collect()),
            )
            .with("note", note)
    }

    /// `to_json()`: the store state as pydantic's compact JSON.
    pub fn to_json(&self) -> String {
        dump_json(&Value::Obj(
            Object::new()
                .with("seq", Value::Int(self.seq.to_string()))
                .with(
                    "strata",
                    Value::List(self.strata.iter().cloned().map(Value::Obj).collect()),
                ),
        ))
    }

    /// `StrataStore.from_json(text)`.
    pub fn from_json(text: &str) -> LR<Store> {
        let Value::Obj(o) = validate_json(Id::StoreState, text)? else {
            unreachable!("a model validates to an object")
        };
        let seq = match o.value("seq") {
            Value::Int(t) => t.parse().unwrap_or(0),
            _ => 0,
        };
        let strata = match o.value("strata") {
            Value::List(l) => l.iter().filter_map(|x| x.as_obj().cloned()).collect(),
            _ => vec![],
        };
        Ok(Store { strata, seq })
    }
}

fn always_include(x: &Object) -> bool {
    if matches!(x.value("pinned"), Value::Bool(true)) {
        return true;
    }
    x.value("kind").as_str() == Some("constraint")
        && matches!(x.value("status").as_str(), Some("open") | Some("promoted"))
}

/// `_relevance`: tag hits, a query hit, the ruled-out penalty, then the
/// sequence number; higher is better.
fn relevance(x: &Object, tags: &[String], query: &str) -> (i64, i64, i64, i64) {
    let mut hits = 0;
    if !tags.is_empty() {
        let mut own: Vec<&str> = match x.value("tags") {
            Value::List(l) => l.iter().filter_map(|t| t.as_str()).collect(),
            _ => vec![],
        };
        own.sort();
        own.dedup();
        hits = own.iter().filter(|t| tags.iter().any(|g| g == *t)).count() as i64;
    }
    let content = x.value("content").as_str().unwrap_or("");
    let q = i64::from(!query.is_empty() && lower(content).contains(&lower(query)));
    let pen = if x.value("status").as_str() == Some("ruled_out") {
        -1
    } else {
        0
    };
    (hits, q, pen, seq_of(x))
}

/// `PromotionResult`.
#[derive(Debug, Clone)]
pub struct Promotion {
    pub ok: bool,
    pub envelope: Object,
    /// The inheritance messages.
    pub violations: Vec<String>,
    pub reason: String,
    pub enforcement_proven: bool,
}

fn refused(env: &Object, reason: &str) -> Promotion {
    Promotion {
        ok: false,
        envelope: env.clone(),
        violations: vec![],
        reason: reason.into(),
        enforcement_proven: false,
    }
}

/// `promote_constraint`: `env` and `candidate` are validated Envelope
/// dumps; `add_invariant` a validated Invariant dump; `witness` a
/// validated ActionPlan dump. On success the caller sets the stratum's
/// status to "promoted".
pub fn promote(
    env: &Object,
    c: &Object,
    add_invariant: Option<&Object>,
    remove_file_write: &[String],
    candidate: Option<Object>,
    witness: Option<&Object>,
) -> LR<Promotion> {
    let kind = c.value("kind").as_str().unwrap_or("");
    if kind != "constraint" {
        return Ok(refused(
            env,
            &format!(
                "only a 'constraint' stratum may touch authority; got kind '{kind}' — facts, hypotheses and goals \
                 inform reasoning, never gate actions"
            ),
        ));
    }
    if c.value("status").as_str() == Some("promoted") {
        return Ok(refused(
            env,
            "constraint is already promoted; re-promoting against the original envelope would build a second \
             tightening that drops the first",
        ));
    }
    let cand = candidate.unwrap_or_else(|| tightened(env, add_invariant, remove_file_write));
    let loosening = verify_inheritance(&cand, env);
    if !loosening.is_empty() {
        let mut r = refused(
            env,
            "promotion would loosen the envelope; a captured constraint may only tighten",
        );
        r.violations = loosening;
        return Ok(r);
    }
    if verify_inheritance(env, &cand).is_empty() {
        return Ok(refused(
            env,
            "promotion has no enforceable effect (candidate equals the current envelope)",
        ));
    }
    let mut proven = false;
    if let Some(w) = witness {
        if !denies(w, &cand)? {
            return Ok(refused(
                env,
                "promoted constraint does not actually deny its witness — an unenforced (soft/uncompiled) \
                 constraint is refused, not accepted",
            ));
        }
        proven = true;
    }
    Ok(Promotion {
        ok: true,
        envelope: cand,
        violations: vec![],
        reason: "tightened".into(),
        enforcement_proven: proven,
    })
}

/// `not verify(witness, candidate).ok`. A Z3 check that did not finish
/// counts as not denied, so a promotion is never taken as proven on a
/// check that gave no answer.
fn denies(witness: &Object, candidate: &Object) -> LR<bool> {
    let r = crate::pathways::verify::verify(
        &Value::Obj(witness.clone()),
        &Value::Obj(candidate.clone()),
        None,
        500,
    )?;
    if !r.timeouts.is_empty() {
        return Ok(false);
    }
    Ok(!r.violations.is_empty())
}

fn tightened(env: &Object, inv: Option<&Object>, remove: &[String]) -> Object {
    let mut cand = env.clone();
    if let Some(i) = inv {
        let mut invs = match cand.value("invariants") {
            Value::List(l) => l.clone(),
            _ => vec![],
        };
        invs.push(Value::Obj(i.clone()));
        cand.set("invariants", Value::List(invs));
    }
    if !remove.is_empty() {
        let mut perms = cand
            .value("permissions")
            .as_obj()
            .cloned()
            .unwrap_or_default();
        let kept: Vec<Value> = match perms.value("file_write") {
            Value::List(l) => l
                .iter()
                .filter(|g| !remove.iter().any(|r| Some(r.as_str()) == g.as_str()))
                .cloned()
                .collect(),
            _ => vec![],
        };
        perms.set("file_write", Value::List(kept));
        cand.set("permissions", Value::Obj(perms));
    }
    cand
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s(x: &str) -> Value {
        Value::Str(x.into())
    }

    fn emit(
        st: &mut Store,
        kind: &str,
        content: &str,
        status: &str,
        tags: &[&str],
        pinned: bool,
    ) -> Object {
        let tags = Value::List(tags.iter().map(|t| s(t)).collect());
        st.emit(
            &s(kind),
            &s(content),
            &s(""),
            &s(status),
            &tags,
            &Value::Bool(pinned),
        )
        .unwrap()
    }

    #[test]
    fn reconstruct_keeps_pinned_and_constraints_and_fills_by_relevance() {
        let mut st = Store::default();
        emit(&mut st, "fact", "old fact", "open", &[], false);
        emit(
            &mut st,
            "constraint",
            "never touch prod",
            "open",
            &[],
            false,
        );
        emit(
            &mut st,
            "fact",
            "the DB is Postgres",
            "open",
            &["db"],
            false,
        );
        emit(&mut st, "hypothesis", "cache bug", "ruled_out", &[], false);
        let r = st.reconstruct(Some(2), &["db".into()], "");
        let got: Vec<&str> = match r.value("strata") {
            Value::List(l) => l
                .iter()
                .map(|x| x.as_obj().unwrap().value("content").as_str().unwrap())
                .collect(),
            _ => vec![],
        };
        assert_eq!(got, ["never touch prod", "the DB is Postgres"]);
        assert!(r
            .value("note")
            .as_str()
            .unwrap()
            .starts_with("Reconstruction is lossy: 2 stratum(s) dropped"));
        let all = st.reconstruct(None, &[], "");
        assert!(matches!(all.value("dropped_ids"), Value::List(l) if l.is_empty()));
    }

    #[test]
    fn a_store_round_trips_through_its_json() {
        let mut st = Store::default();
        emit(&mut st, "goal", "ship ☃", "open", &["a"], true);
        let back = Store::from_json(&st.to_json()).unwrap();
        assert_eq!(back.to_json(), st.to_json());
        assert!(Store::from_json("{").is_err());
    }

    #[test]
    fn an_emit_that_does_not_validate_still_takes_a_number() {
        let mut st = Store::default();
        assert!(st
            .emit(
                &s("rumor"),
                &s("x"),
                &s(""),
                &s("open"),
                &Value::Null,
                &Value::Bool(false)
            )
            .is_err());
        let o = emit(&mut st, "fact", "y", "open", &[], false);
        assert_eq!(o.value("seq"), &Value::Int("2".into()));
        assert!(st.set_status("nope", &s("resolved")).is_err());
    }

    #[test]
    fn only_a_constraint_that_tightens_is_promoted() {
        let env = match validate_model(
            Id::Envelope,
            &crate::gate::pyjson::loads(r#"{"generated_by": "t", "task": "t", "permissions": {"file_write": ["/a/**", "/b/**"]}}"#).unwrap(),
            Mode::Python,
        )
        .unwrap()
        {
            Value::Obj(o) => o,
            _ => unreachable!(),
        };
        let mut st = Store::default();
        let fact = emit(&mut st, "fact", "f", "open", &[], false);
        assert!(!promote(&env, &fact, None, &[], None, None).unwrap().ok);
        let c = emit(&mut st, "constraint", "no /b", "open", &[], false);
        let r = promote(&env, &c, None, &["/b/**".into()], None, None).unwrap();
        assert!(r.ok, "{}", r.reason);
        let same = promote(&env, &c, None, &[], None, None).unwrap();
        assert!(!same.ok);
        assert!(same.reason.contains("no enforceable effect"));
    }
}
