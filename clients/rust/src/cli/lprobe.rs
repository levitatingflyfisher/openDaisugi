//! The test instrument `l-probe` runs for clients/l_compare.py: one query
//! (argv[1], JSON) of the stage-L library parts no command reaches (the
//! deed ledger, the strata store, batch runs, pathway bundles, the signing
//! primitives), answered on stdout as clients/l_probe_oracle.py answers
//! it. It is not shipped. The Go client's `cli/lprobe.go` is the
//! reference.

use std::collections::HashMap;

use std::collections::BTreeMap;

use crate::batch;
use crate::bundle;
use crate::deeds;
use crate::gate::pyjson::{dumps, loads, Object, Value};
use crate::lerr::{LErr, PyError, LR};
use crate::pathways::pathway::json_mode;
use crate::pathways::pmodel::{validate_model, Id, Mode};
use crate::signing;
use crate::strata;
use crate::supervise::{self, approval::DefaultApproval, Executor, Session, Supervisor};
use crate::tracejournal::Journal;

/// `{"error": <type>, "msg": <text>}` for an exception the oracle raises.
fn py_err(e: &LErr) -> Value {
    let (t, m) = e.py();
    Value::Obj(Object::new().with("error", t).with("msg", m))
}

fn py_error(e: &PyError) -> Value {
    py_err(&LErr::Py(e.clone()))
}

/// `v` with every object's keys sorted, for `json.dumps(sort_keys=True)`.
pub fn sort_keys(v: &Value) -> Value {
    match v {
        Value::Obj(o) => {
            let mut keys: Vec<&String> = o.keys().iter().collect();
            keys.sort();
            let mut out = Object::new();
            for k in keys {
                out.set(k, sort_keys(o.value(k)));
            }
            Value::Obj(out)
        }
        Value::List(l) => Value::List(l.iter().map(sort_keys).collect()),
        other => other.clone(),
    }
}

pub(super) fn strs(v: &Value) -> Vec<String> {
    match v {
        Value::List(l) => l
            .iter()
            .map(|x| x.as_str().unwrap_or("").to_string())
            .collect(),
        _ => vec![],
    }
}

fn list_of(v: &Value) -> &[Value] {
    match v {
        Value::List(l) => l,
        _ => &[],
    }
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Vec<u8> {
    (0..s.len() / 2)
        .filter_map(|i| u8::from_str_radix(s.get(2 * i..2 * i + 2)?, 16).ok())
        .collect()
}

/// The probe's world: the process environment.
pub struct Probe {
    pub environ: HashMap<String, String>,
}

impl Probe {
    fn op(&self, q: &Object) -> Result<Value, LErr> {
        match q.value("op").as_str().unwrap_or("") {
            "b64" => Ok(Value::List(
                strs(q.value("inputs"))
                    .iter()
                    .map(|s| match signing::b64decode(s) {
                        Ok(b) => Value::Obj(Object::new().with("hex", hex(&b))),
                        Err(e) => py_error(&e),
                    })
                    .collect(),
            )),
            "sign" => Ok(Value::List(
                list_of(q.value("cases"))
                    .iter()
                    .map(|c| {
                        let c = c.as_obj().cloned().unwrap_or_default();
                        let payload = c.value("payload").as_str().unwrap_or("");
                        match signing::sign_bytes(
                            payload.as_bytes(),
                            c.value("priv").as_str().unwrap_or(""),
                        ) {
                            Ok(s) => Value::Obj(Object::new().with("sig", s)),
                            Err(e) => py_error(&e),
                        }
                    })
                    .collect(),
            )),
            "verify" => Ok(Value::List(
                list_of(q.value("cases"))
                    .iter()
                    .map(|c| {
                        let c = c.as_obj().cloned().unwrap_or_default();
                        let payload = match c.value("hex").as_str() {
                            Some(h) => unhex(h),
                            None => c
                                .value("payload")
                                .as_str()
                                .unwrap_or("")
                                .as_bytes()
                                .to_vec(),
                        };
                        let ok = signing::verify_bytes(
                            &payload,
                            c.value("sig").as_str().unwrap_or(""),
                            c.value("pub").as_str().unwrap_or(""),
                        );
                        Value::Obj(Object::new().with("ok", ok))
                    })
                    .collect(),
            )),
            other => self.op_more(other, q),
        }
    }
}

/// Runs the probe on this process's argv and environment; the exit code.
pub fn main() -> i32 {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.len() != 1 {
        eprintln!("usage: l-probe QUERY_JSON");
        return 2;
    }
    let q = match loads(&args[0]) {
        Ok(Value::Obj(o)) => o,
        _ => {
            eprintln!("l-probe: the query does not read");
            return 2;
        }
    };
    let p = Probe {
        environ: std::env::vars().collect(),
    };
    let out = match p.op(&q) {
        Ok(v) => v,
        Err(LErr::Unread(why)) => {
            eprintln!("l-probe: {why}. Nothing was changed.");
            return 2;
        }
        Err(e) => py_err(&e),
    };
    println!("{}", dumps(&sort_keys(&out), false));
    0
}

fn model(id: Id, v: &Value) -> LR<Object> {
    match validate_model(id, v, Mode::Python)? {
        Value::Obj(o) => Ok(o),
        _ => unreachable!("a model validates to an object"),
    }
}

fn int_of(v: &Value) -> Option<i64> {
    match v {
        Value::Int(t) => t.parse().ok(),
        _ => None,
    }
}

fn objs(xs: Vec<Object>) -> Value {
    Value::List(xs.into_iter().map(Value::Obj).collect())
}

/// A stratum reference: an index into the ids emitted so far (negative
/// from the end), or an id.
fn ref_of(ids: &[String], v: &Value) -> String {
    match v {
        Value::Int(t) => {
            let mut n: i64 = t.parse().unwrap_or(i64::MAX);
            if n < 0 {
                n += ids.len() as i64;
            }
            usize::try_from(n)
                .ok()
                .and_then(|i| ids.get(i))
                .cloned()
                .unwrap_or_default()
        }
        Value::Str(s) => s.clone(),
        _ => String::new(),
    }
}

fn or<'a>(o: &'a Object, k: &str, def: &'a Value) -> &'a Value {
    o.get(k).unwrap_or(def)
}

impl Probe {
    fn op_more(&self, op: &str, q: &Object) -> LR<Value> {
        match op {
            "contract" => probe_contract(q),
            "to_bundle" => {
                let pw = model(Id::CompiledPathway, q.value("pathway"))?;
                let publisher = q.value("publisher").as_str().unwrap_or("");
                let at = q.value("published_at");
                let b = bundle::to_bundle(
                    &pw,
                    publisher,
                    at,
                    q.value("priv").as_str(),
                    q.value("pub").as_str(),
                )?;
                Ok(Value::Obj(
                    Object::new()
                        .with("bundle", b)
                        .with("hash", bundle::hash(&pw, publisher, at)),
                ))
            }
            "from_bundle" => {
                let b = bundle::validate(q.value("bundle"))?;
                let trusted: Option<Vec<String>> = match q.value("trusted") {
                    Value::Null => None,
                    v => Some(strs(v)),
                };
                let require = q.get("require_signed").map(|r| r.truthy()).unwrap_or(true);
                let pw = bundle::from_bundle(&b, trusted.as_deref(), require)?;
                Ok(Value::Obj(Object::new().with("pathway", pw)))
            }
            "rollback" | "touched" => {
                let j = Journal::open(q.value("data_dir").as_str().unwrap_or(""))?;
                let run = q.value("run_id").as_str().unwrap_or("");
                if op == "rollback" {
                    return Ok(Value::Obj(deeds::rollback(&j, run)?.dump()));
                }
                Ok(Value::List(
                    deeds::touched(&j, run)?
                        .into_iter()
                        .map(|t| {
                            Value::List(vec![
                                Value::Str(t.path),
                                Value::Obj(
                                    Object::new()
                                        .with("pre_existed", t.pre_existed)
                                        .with("pre_content", t.pre_content),
                                ),
                            ])
                        })
                        .collect(),
                ))
            }
            "reverse" => {
                let h = deeds::parse_handle(q.value("handle"))?;
                deeds::apply(&h)?;
                Ok(Value::Obj(Object::new().with("ok", true)))
            }
            "strata" => probe_strata(q),
            "batch" => self.probe_batch(q),
            _ => Err(PyError::new("KeyError", crate::gate::py::text::repr(op)).into()),
        }
    }

    fn probe_batch(&self, q: &Object) -> LR<Value> {
        let d = batch::validate(q.value("decl"))?;
        let env = model(Id::Envelope, q.value("envelope"))?;
        let j = Journal::open(q.value("data_dir").as_str().unwrap_or(""))?;
        let environ = self.environ.clone();
        let mut runner = |plan: &Object| -> LR<Session> {
            let pre = super::runcmd::prepare(plan, &env, crate::pathways::verify::verify)
                .map_err(LErr::Unread)?;
            let mut executors: BTreeMap<String, Box<dyn Executor>> = BTreeMap::new();
            for (k, ex) in supervise::default_executors(environ.clone()) {
                executors.insert(k, ex);
            }
            let approval = Box::new(DefaultApproval {
                env: environ.clone(),
                terminal: false,
            });
            let mut sup = Supervisor::new(executors, approval, Some(&j));
            let s = sup.run(plan, &env, pre.verification);
            if let Some(e) = sup.log_err.take() {
                return Err(LErr::Unread(e));
            }
            Ok(s)
        };
        let fw = strs(
            env.value("permissions")
                .as_obj()
                .map(|p| p.value("file_write"))
                .unwrap_or(&Value::Null),
        );
        let r = batch::run(&d, &fw, &mut runner, int_of(q.value("sample_k")))?;
        let mut out = r.dump();
        if q.value("undo").truthy() {
            out.set("undo", batch::rollback_result(&r)?.dump());
        }
        if let Value::Obj(kw) = q.value("ledger_kwargs") {
            let num = |k: &str| int_of(kw.value(k));
            let l = batch::within_instance(
                &d,
                num("output_tokens_saved").unwrap_or(0),
                num("calls_saved").unwrap_or(0),
                num("tokens_per_call"),
                num("spec_input_injected"),
            );
            out.set("ledger_kwargs", batch::two_ledgers(&l));
        }
        Ok(Value::Obj(out))
    }
}

fn probe_contract(q: &Object) -> LR<Value> {
    let mut c = model(Id::Contract, q.value("contract"))?;
    let mut out = Object::new().with(
        "canonical",
        String::from_utf8_lossy(&signing::canonical_contract(&c)).into_owned(),
    );
    if let Value::Str(private) = q.value("priv") {
        match signing::sign_contract(&c, private) {
            Ok(sig) => {
                out.set("signature", sig.as_str());
                c.set("signature", sig);
                c.set("signer", q.value("signer").clone());
            }
            Err(e) => {
                out.set("signature", py_error(&e));
            }
        }
    }
    let trust = q.value("trust").as_obj().cloned().unwrap_or_default();
    let mut raw = Object::new();
    for (n, pk) in trust.iter() {
        raw.set(n, signing::verify_contract(&c, pk));
    }
    out.set("verify_raw", raw);
    // A scratch file beside the working directory, never in /tmp.
    let dir = format!("l-probe-{}", std::process::id());
    std::fs::create_dir_all(&dir).map_err(|e| LErr::io(e, &dir))?;
    let result = (|| -> LR<()> {
        let path = format!("{dir}/trusted_signers.json");
        let mut reg = signing::Registry::load(&path)?;
        for (n, pk) in trust.iter() {
            reg.add(n, pk.as_str().unwrap_or(""));
        }
        reg.save().map_err(|e| LErr::io(e, &path))?;
        let text = std::fs::read_to_string(&path).map_err(|e| LErr::io(e, &path))?;
        out.set("registry_file", text);
        let mut reg = signing::Registry::load(&path)?;
        out.set(
            "names",
            Value::List(reg.names().into_iter().map(Value::Str).collect()),
        );
        let want = strs(q.value("names"));
        out.set("registry_verify", reg.verify_named(&c, &want));
        let rm = strs(q.value("remove"));
        if !rm.is_empty() {
            let removed: Vec<Value> = rm.iter().map(|n| Value::Bool(reg.remove(n))).collect();
            out.set("removed", Value::List(removed));
            out.set("registry_verify_after", reg.verify_named(&c, &want));
        }
        Ok(())
    })();
    let _ = std::fs::remove_dir_all(&dir);
    result?;
    Ok(Value::Obj(out))
}

fn probe_strata(q: &Object) -> LR<Value> {
    let mut store = strata::Store::default();
    let mut env = match q.get("envelope") {
        Some(v) => Some(model(Id::Envelope, v)?),
        None => None,
    };
    let mut ids: Vec<String> = vec![];
    let mut out = vec![];
    let null = Value::Null;
    for sv in list_of(q.value("steps")) {
        let st = sv.as_obj().cloned().unwrap_or_default();
        let res = (|| -> LR<Value> {
            match st.value("do").as_str().unwrap_or("") {
                "emit" => {
                    let x = store.emit(
                        st.value("kind"),
                        st.value("content"),
                        or(&st, "provenance", &Value::Str(String::new())),
                        or(&st, "status", &Value::Str("open".into())),
                        or(&st, "tags", &null),
                        or(&st, "pinned", &Value::Bool(false)),
                    )?;
                    ids.push(x.value("id").as_str().unwrap_or("").to_string());
                    Ok(Value::Obj(x))
                }
                "set_status" => Ok(Value::Obj(
                    store.set_status(&ref_of(&ids, st.value("ref")), st.value("status"))?,
                )),
                "get" => Ok(store
                    .get(&ref_of(&ids, st.value("ref")))
                    .cloned()
                    .map(Value::Obj)
                    .unwrap_or(Value::Null)),
                "repage" => Ok(Value::Obj(store.repage(&ref_of(&ids, st.value("ref")))?)),
                "by_kind" => Ok(objs(store.by_kind(st.value("kind").as_str().unwrap_or("")))),
                "all" => Ok(objs(store.all())),
                "reconstruct" => {
                    let budget = int_of(st.value("budget"));
                    let query = st.value("query").as_str().unwrap_or("");
                    Ok(Value::Obj(store.reconstruct(
                        budget,
                        &strs(st.value("tags")),
                        query,
                    )))
                }
                "to_json" => Ok(Value::Str(store.to_json())),
                "roundtrip" => {
                    store = strata::Store::from_json(&store.to_json())?;
                    Ok(Value::Str("ok".into()))
                }
                "from_json" => {
                    store = strata::Store::from_json(st.value("text").as_str().unwrap_or(""))?;
                    ids = store
                        .all()
                        .iter()
                        .map(|x| x.value("id").as_str().unwrap_or("").to_string())
                        .collect();
                    Ok(objs(store.all()))
                }
                "promote" => {
                    let inv = match st.get("add_invariant") {
                        Some(v) => Some(model(Id::Invariant, v)?),
                        None => None,
                    };
                    let cand = match st.get("candidate") {
                        Some(v) => Some(model(Id::Envelope, v)?),
                        None => None,
                    };
                    let wit = match st.get("deny_witness") {
                        Some(v) => Some(model(Id::ActionPlan, v)?),
                        None => None,
                    };
                    let id = ref_of(&ids, st.value("ref"));
                    let Some(target) = store.get(&id).cloned() else {
                        return Err(PyError::new(
                            "AttributeError",
                            "'NoneType' object has no attribute 'kind'",
                        )
                        .into());
                    };
                    let Some(cur) = env.clone() else {
                        return Err(PyError::new(
                            "AttributeError",
                            "'NoneType' object has no attribute 'model_copy'",
                        )
                        .into());
                    };
                    let r = strata::promote(
                        &cur,
                        &target,
                        inv.as_ref(),
                        &strs(st.value("remove_file_write")),
                        cand,
                        wit.as_ref(),
                    )?;
                    if r.ok {
                        store.set_status(&id, &Value::Str("promoted".into()))?;
                        env = Some(r.envelope.clone());
                    }
                    let status = store
                        .get(&id)
                        .map(|x| x.value("status").clone())
                        .unwrap_or(Value::Null);
                    let vs: Vec<Value> = r
                        .violations
                        .iter()
                        .map(|m| {
                            Value::List(vec![
                                Value::Str("inheritance".into()),
                                Value::Str(m.clone()),
                            ])
                        })
                        .collect();
                    Ok(Value::Obj(
                        Object::new()
                            .with("ok", r.ok)
                            .with("reason", r.reason)
                            .with("enforcement_proven", r.enforcement_proven)
                            .with("violations", Value::List(vs))
                            .with("envelope", json_mode(&Value::Obj(r.envelope)))
                            .with("status", status),
                    ))
                }
                "ledger" => {
                    let f = st.value("fields").as_obj().cloned().unwrap_or_default();
                    let get = |k: &str| int_of(f.value(k)).unwrap_or(0);
                    let (without, with) = (
                        get("output_tokens_without_store"),
                        get("output_tokens_with_store"),
                    );
                    Ok(Value::Obj(
                        Object::new()
                            .with("output_tokens_without_store", without)
                            .with("output_tokens_with_store", with)
                            .with("rederived_facts", get("rederived_facts"))
                            .with("reexplored_branches", get("reexplored_branches"))
                            .with("evidence_not_proof", true)
                            .with(
                                "note",
                                "Store-on vs store-off output-token delta. Labelled evidence, not proof; the magnitudes \
                                 are model-dependent and the at-scale numbers are deferred to a local model (Stage 4's \
                                 dependency).",
                            )
                            .with("tokens_saved", without - with),
                    ))
                }
                other => Ok(Value::Obj(
                    Object::new()
                        .with("error", "unknown step")
                        .with("msg", other),
                )),
            }
        })();
        match res {
            Ok(v) => out.push(v),
            Err(LErr::Unread(w)) => return Err(LErr::Unread(w)),
            Err(e) => out.push(py_err(&e)),
        }
    }
    Ok(Value::List(out))
}
