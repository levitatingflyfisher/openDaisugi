//! `opendaisugi.rank_judge`: one model comparison of two attempts, in both
//! orders. A reply that is not one line of A, B or tie, a transport error and
//! a non-2xx status are no vote. The judge comes from another model family
//! than every author in the pair (W3K-1), and a dropped judge is not asked
//! (RK-5). The Go client's `internal/rank/judge.go` is the reference.

use std::collections::{BTreeMap, HashMap};
use std::io::Write;
use std::process::{Command, Stdio};

use crate::agentprof::Profiles;
use crate::rank::{self, Attempt, Pair, Ranking};
use crate::warden::py::text::{decode_utf8_replace, strip};
use crate::warden::pyjson::{canonical_json_ascii, dumps, Object, Value};
use crate::warden::sha256::hexdigest;

pub const DIFF_CAP: usize = 256 * 1024;

pub const JUDGE_SYSTEM_TEXT: &str = "You compare two answers to one task. Decide which answer does the task better. Length, polish and confidence do not count. Reply with one line: A, B or tie.";

/// `rank_judge.prompt`: the same bytes in every port.
pub fn prompt(task: &str, first: &str, second: &str) -> Vec<(String, String)> {
    let user = format!(
        "Task:\n{task}\n\nAnswer A:\n{first}\n\nAnswer B:\n{second}\n\nWhich answer does the task better? Reply with one line: A, B or tie."
    );
    vec![("system".into(), JUDGE_SYSTEM_TEXT.into()), ("user".into(), user)]
}

/// `rank_judge.artifact`: the text a judge reads for one attempt.
pub fn artifact(a: &Attempt, warnings: Option<&mut Vec<String>>) -> String {
    if let Some(Value::Obj(w)) = &a.where_ {
        if w.value("kind").as_str() == Some("output") {
            if let Some(t) = w.value("text").as_str() {
                return t.to_string();
            }
        }
        if w.value("kind").as_str() == Some("worktree") {
            // The base and the repository come from the attempt the parent
            // wrote, never from the worktree, which its child can rewrite.
            if let (Some(p), Some(base), Some(repo)) = (w.value("path").as_str(), w.value("base").as_str(), w.value("repo").as_str()) {
                if !crate::branch_git::usable(base, repo) {
                    // fall through to the warning
                } else if let Some(raw) =
                    crate::branch_git::diff(p, base, repo, &[], true, std::time::Duration::from_secs(60))
                {
                    let cut = raw.len() > DIFF_CAP;
                    let mut text = decode_utf8_replace(&raw[..raw.len().min(DIFF_CAP)]);
                    if cut {
                        text.push_str("\n[cut at 256 KiB]");
                    }
                    return text;
                }
            }
        }
    }
    if let Some(w) = warnings {
        w.push(format!("attempt {} has no artifact a judge can read", a.id));
    }
    String::new()
}

/// `rank_judge.parse_verdict`: "a", "b" or "tie", else None.
pub fn parse_verdict(text: &str) -> Option<&'static str> {
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    for line in text.split('\n') {
        let line = strip(line);
        if line.is_empty() {
            continue;
        }
        let l = strip(line.trim_end_matches('.')).to_lowercase();
        return match l.as_str() {
            "a" => Some("a"),
            "b" => Some("b"),
            "tie" => Some("tie"),
            _ => None,
        };
    }
    None
}

/// One model call: wire model, base URL, key, messages -> text, or the
/// exception class name and message.
pub type CallFn<'a> = &'a dyn Fn(&str, &str, &str, &[(String, String)]) -> Result<String, (String, String)>;

/// What `rank_judge.judge_once` returns.
pub struct JudgeResult {
    pub recorded: bool,
    pub pair: Option<Vec<String>>,
    pub judge: Option<String>,
    pub outcomes: Option<Vec<String>>,
    pub why: String,
    pub reason: String,
    pub fit: Option<Object>,
}

fn nothing(why: String, fit: Option<Object>, reason: &str) -> JudgeResult {
    JudgeResult { recorded: false, pair: None, judge: None, outcomes: None, why, reason: reason.into(), fit }
}

fn r2_of(r: &Ranking, comps: Vec<rank::Comparison>, owner: Vec<Pair>) -> Ranking {
    Ranking {
        id: r.id.clone(),
        task: r.task.clone(),
        project: r.project.clone(),
        attempts: r.attempts.clone(),
        comparisons: Some(comps),
        owner: Some(owner),
        policy: r.policy.clone(),
    }
}

/// `rank_judge._fit_for`: the fit with the file's votes, the journal's votes
/// and the owner's answers. Also the number of votes so far.
fn fit_for(r: &Ranking, data_dir: &str, mut warnings: Vec<String>) -> (Object, usize) {
    let mut comps: Vec<rank::Comparison> = r.comparisons.clone().unwrap_or_default();
    let (journal, bad) = rank::journal_comparisons(data_dir, &r.id);
    comps.extend(journal);
    if bad > 0 {
        warnings.push(format!("{bad} rows of the comparison journal did not read; skipped"));
    }
    let owner: Vec<Pair> = match &r.owner {
        Some(o) => o.clone(),
        None => {
            let by_id: HashMap<String, &Attempt> = r.attempts.iter().map(|a| (a.id.clone(), a)).collect();
            rank::owner_answers(data_dir, &r.id, &by_id, &mut warnings)
        }
    };
    let r2 = r2_of(r, comps.clone(), owner.clone());
    let mut weights: Option<BTreeMap<String, f64>> = None;
    if r.comparisons.as_ref().map_or(true, |c| c.is_empty()) {
        let mut set: std::collections::BTreeSet<String> = r.policy.judges.iter().cloned().collect();
        set.extend(comps.iter().map(|c| c.judge.clone()));
        if !set.is_empty() {
            let judges: Vec<String> = set.into_iter().collect();
            weights = Some(rank::weights_for(data_dir, &judges, &rank::seed_of(&r2, &comps, &owner)).0);
        }
    }
    let n = comps.len();
    (rank::fit_w(&r2, vec![], vec![], warnings, weights.as_ref()), n)
}

/// The fit of an attempts document, as `rank fit` reads it (`fit_doc`).
pub fn fit_doc(r: &Ranking, data_dir: &str) -> Object {
    fit_for(r, data_dir, vec![]).0
}

fn s_of(v: &Value) -> String {
    v.as_str().unwrap_or("").to_string()
}

fn append_comparisons(data_dir: &str, rows: &[Object]) -> std::io::Result<()> {
    if rows.is_empty() {
        return Ok(());
    }
    let d = rank::rankings_dir(data_dir);
    std::fs::create_dir_all(&d)?;
    let mut f = std::fs::OpenOptions::new().append(true).create(true).open(format!("{d}/comparisons.jsonl"))?;
    let mut b = String::new();
    for r in rows {
        b.push_str(&dumps(&Value::Obj(r.clone()), true));
        b.push('\n');
    }
    f.write_all(b.as_bytes())?;
    f.sync_all()
}

/// `rank_judge.judge_once`.
pub fn judge_once(
    parse: &dyn Fn() -> Result<Ranking, String>,
    data_dir: &str,
    profiles: &Profiles,
    getenv: &dyn Fn(&str) -> Option<String>,
    judge: Option<&str>,
    now: f64,
    call: CallFn,
) -> Result<JudgeResult, String> {
    let mut r = parse()?;
    if let Some(j) = judge {
        r.policy.judges = vec![j.to_string()];
    }
    if r.policy.judges.is_empty() {
        return Ok(nothing("policy.judges names no judge.".into(), None, "nothing"));
    }
    let trust = rank::judge_trust(data_dir, rank::TRUST_FLOOR, rank::TRUST_MIN_CARDS);
    let (dropped, kept): (Vec<String>, Vec<String>) =
        r.policy.judges.iter().cloned().partition(|j| trust.get(j).is_some_and(|t| t.dropped));
    r.policy.judges = kept;
    let mut warnings: Vec<String> = vec![];
    let (res, n_so_far) = fit_for(&r, data_dir, warnings.clone());
    let Some(nxt) = res.value("next").as_obj().cloned() else {
        if !dropped.is_empty() && res.value("stop").as_str() == Some("judges_exhausted") {
            let j = &dropped[0];
            return Ok(nothing(format!("judge {j} is dropped: {}", trust[j].why), Some(res), "nothing"));
        }
        let stop = match res.value("stop") {
            Value::Null => "None".to_string(),
            v => s_of(v),
        };
        return Ok(nothing(format!("nothing to judge: {stop}"), Some(res), "nothing"));
    };
    let entry = s_of(nxt.value("judge"));
    let (first_id, second_id) = match nxt.value("pair") {
        Value::List(p) => (s_of(&p[0]), s_of(&p[1])),
        _ => return Err("the next pair is not a pair".into()),
    };
    let by_id: HashMap<String, &Attempt> = r.attempts.iter().map(|a| (a.id.clone(), a)).collect();
    let (fa, fb) = (by_id[&first_id], by_id[&second_id]);
    let (prof_name, model) = match entry.split_once('/') {
        Some((n, m)) => (n.to_string(), Some(m.to_string())),
        None => (entry.clone(), None),
    };
    let profile = match profiles.lookup(Some(&prof_name)) {
        Ok(p) => p,
        Err(why) => return Ok(nothing(format!("judge {entry}: {why}"), Some(res), "nothing")),
    };
    if profile.wire == "claude-code" {
        return Ok(nothing(
            format!("judge {entry}: profile {prof_name} uses the claude-code wire; a judge needs the anthropic or openai wire."),
            Some(res),
            "nothing",
        ));
    }
    let model = model.or_else(|| profile.model.clone()).unwrap_or_default();
    if model.is_empty() {
        return Ok(nothing(format!("judge {entry}: no model; name PROFILE/MODEL."), Some(res), "nothing"));
    }
    let fam = rank::family_of(&model);
    for a in [fa, fb] {
        if strip(&a.author).is_empty() {
            return Ok(nothing(
                format!("attempt {} has no author, so the other-family rule cannot hold.", a.id),
                Some(res),
                "nothing",
            ));
        }
        if rank::family_of(&a.author) == fam {
            return Ok(nothing(
                format!("judge {entry} is of the same family ({fam}) as the author of {}.", a.id),
                Some(res),
                "nothing",
            ));
        }
    }
    let ta = artifact(fa, Some(&mut warnings));
    let tb = artifact(fb, Some(&mut warnings));
    if ta.is_empty() && tb.is_empty() {
        return Ok(nothing("no attempt has an artifact a judge can read.".into(), Some(res), "nothing"));
    }
    if ta.is_empty() || tb.is_empty() {
        return Ok(nothing("an attempt has no artifact a judge can read.".into(), Some(res), "nothing"));
    }
    let texts: HashMap<&str, &str> = [(fa.id.as_str(), ta.as_str()), (fb.id.as_str(), tb.as_str())].into_iter().collect();
    let wire_model = format!("{}{}", if profile.wire == "anthropic" { "anthropic/" } else { "openai/" }, model);
    let key = match (&profile.key, &profile.key_env) {
        (Some(k), _) => k.clone(),
        (None, Some(e)) if !e.is_empty() => getenv(e).unwrap_or_default(),
        _ => String::new(),
    };
    let base_url = profile.base_url.clone().unwrap_or_default();
    let ordered = [(fa, fb), (fb, fa)];
    let mut verdicts: Vec<&'static str> = vec![];
    for (one, two) in ordered {
        let msgs = prompt(&r.task, texts[one.id.as_str()], texts[two.id.as_str()]);
        let text = match call(&wire_model, &base_url, &key, &msgs) {
            Ok(t) => t,
            Err((name, msg)) => {
                return Ok(nothing(
                    format!("the judge call failed ({name}: {msg}); no vote recorded."),
                    Some(res),
                    "failed",
                ))
            }
        };
        match parse_verdict(&text) {
            Some(v) => verdicts.push(v),
            None => {
                return Ok(nothing(
                    "the judge reply was not one line of A, B or tie; no vote recorded.".into(),
                    Some(res),
                    "unparsable",
                ))
            }
        }
    }
    let canon = canonical_json_ascii(&Value::List(vec![
        r.id.as_str().into(),
        first_id.as_str().into(),
        second_id.as_str().into(),
        entry.as_str().into(),
        Value::Int(n_so_far.to_string()),
    ]));
    let pair_id = format!("pj_{}", &hexdigest(canon.as_bytes())[..12]);
    let mut rows = vec![];
    let mut outcomes = vec![];
    for (k, ((one, two), v)) in ordered.iter().zip(&verdicts).enumerate() {
        let mut row = Object::new();
        row.set("ranking_id", r.id.as_str()).set("a", one.id.as_str()).set("b", two.id.as_str());
        row.set("a_hash", one.content_hash.as_str()).set("b_hash", two.content_hash.as_str());
        row.set("shown", if k == 0 { "ab" } else { "ba" }).set("outcome", *v).set("judge", entry.as_str());
        row.set("pair_id", pair_id.as_str());
        row.set(
            "lengths",
            Value::List(vec![
                Value::Int(texts[one.id.as_str()].chars().count().to_string()),
                Value::Int(texts[two.id.as_str()].chars().count().to_string()),
            ]),
        );
        row.set("ts", now);
        rows.push(row);
        outcomes.push(match *v {
            "a" => one.id.clone(),
            "b" => two.id.clone(),
            _ => "tie".to_string(),
        });
    }
    append_comparisons(data_dir, &rows).map_err(|e| e.to_string())?;
    let (after, _) = fit_for(&r, data_dir, vec![]);
    Ok(JudgeResult {
        recorded: true,
        pair: Some(vec![first_id.clone(), second_id.clone()]),
        judge: Some(entry.clone()),
        outcomes: Some(outcomes),
        why: format!("recorded two calls of {entry} on {first_id} and {second_id}."),
        reason: "recorded".into(),
        fit: Some(after),
    })
}

/// Parse an attempts document as `rank_judge.judge_once` does on each call.
pub fn parse_doc(doc: &Value) -> Result<Ranking, String> {
    rank::parse(doc)
}

#[cfg(test)]
mod tests {
    use super::*;

    // The digest is the oracle's (tests/test_rank_judge.py): the three
    // ports send one request body.
    #[test]
    fn the_prompt_bytes_are_pinned() {
        let msgs = prompt("t", "x", "y");
        let l: Vec<Value> = msgs
            .iter()
            .map(|(r, c)| {
                let mut o = Object::new();
                o.set("content", c.as_str()).set("role", r.as_str());
                Value::Obj(o)
            })
            .collect();
        let text = canonical_json_ascii(&Value::List(l));
        assert_eq!(hexdigest(text.as_bytes()), "f90271f4cb148385a65dcf40cf1c6b078777f6b8c0ed1bb4f76108d64328cc65");
    }

    #[test]
    fn verdicts_read_one_line() {
        for (i, w) in [("A", Some("a")), (" b.\nreason", Some("b")), ("Tie", Some("tie")), ("A or B", None), ("", None), ("\n\nB", Some("b"))] {
            assert_eq!(parse_verdict(i), w, "{i:?}");
        }
    }
}
