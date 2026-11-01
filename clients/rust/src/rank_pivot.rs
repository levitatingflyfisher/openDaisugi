//! `opendaisugi.rank_pivot`: switch from the chosen attempt to another, or
//! say why not. `plan` reads and writes nothing; `apply` undoes the later
//! steps the ledger marks reversible, newest first, then applies the
//! alternative's patch inside the chosen worktree. It never runs a permanent
//! step (W3K-4). The Go client's `internal/rank/pivot.go` is the reference.

use std::collections::HashMap;
use std::io::Write;
use std::process::{Command, Stdio};

use rusqlite::{Connection, OpenFlags};

use crate::rank::{self, Card};
use crate::warden::py::text::{decode_utf8_replace, strip};
use crate::warden::pyjson::{loads_py, Object, Value};

struct PRow {
    step: String,
    rev: String,
    at: f64,
    rjson: Option<String>,
}

/// `rank_pivot._rows`: (step, reversibility, timestamp, reversal_json) of a
/// run, read only.
fn pivot_rows(data_dir: &str, run_id: &str) -> Vec<PRow> {
    let path = format!("{data_dir}/journal/index.db");
    if std::fs::metadata(&path).is_err() {
        return vec![];
    }
    let dsn = if path.starts_with("file:") { format!("./{path}") } else { path };
    let flags = OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX;
    let Ok(con) = Connection::open_with_flags(dsn, flags) else { return vec![] };
    let Ok(mut st) = con.prepare(
        "SELECT step_id, reversibility, timestamp, reversal_json FROM receipts WHERE run_id = ? ORDER BY timestamp, step_id",
    ) else {
        return vec![];
    };
    let rows = st.query_map([run_id], |r| {
        let a: rusqlite::types::Value = r.get(0)?;
        let b: rusqlite::types::Value = r.get(1)?;
        let c: rusqlite::types::Value = r.get(2)?;
        let d: rusqlite::types::Value = r.get(3)?;
        Ok((a, b, c, d))
    });
    let Ok(rows) = rows else { return vec![] };
    let mut out = vec![];
    for row in rows {
        let Ok((a, b, c, d)) = row else { return vec![] };
        use rusqlite::types::Value as V;
        let at = match c {
            V::Real(f) => f,
            V::Integer(n) => n as f64,
            _ => continue,
        };
        let text = |v: V| match v {
            V::Text(s) => s,
            V::Integer(n) => n.to_string(),
            V::Blob(b) => String::from_utf8_lossy(&b).into_owned(),
            _ => String::new(),
        };
        let rjson = match d {
            V::Text(s) => Some(s),
            V::Blob(b) => Some(String::from_utf8_lossy(&b).into_owned()),
            _ => None,
        };
        out.push(PRow { step: text(a), rev: text(b), at, rjson });
    }
    out
}

fn where_of(card: &Card, attempt: &str) -> Option<Object> {
    let facts = card.opened.value("facts").as_obj()?;
    let w = facts.value("where").as_obj()?;
    w.value(attempt).as_obj().cloned()
}

/// `rank_pivot.plan`. The error is the oracle's RankError text.
pub fn plan(card: &Card, data_dir: &str, to: &str) -> Result<Object, String> {
    let surv: Vec<String> = card.survivors().into_iter().map(|(id, _)| id).collect();
    let chosen = card.chosen();
    if !surv.iter().any(|s| s == to) || to == chosen {
        let others: Vec<String> = surv.iter().filter(|s| **s != chosen).cloned().collect();
        return Err(format!("{to} is not another attempt that survived; the others are {}.", rank::join(&others)));
    }
    let sc = rank::switch_cost(card, data_dir, Some(to));
    let facts = card.opened.value("facts").as_obj().cloned().unwrap_or_default();
    let run = facts.value("run").as_obj().cloned();
    let mut undo: Vec<Value> = vec![];
    let mut skipped: Vec<Value> = vec![];
    if let Some(run) = run {
        if let (Some(rid), Value::List(down_l)) = (run.value("run_id").as_str(), run.value("downstream")) {
            let down: Vec<&str> = down_l.iter().filter_map(|d| d.as_str()).collect();
            let mut runs = vec![rid.to_string()];
            runs.extend(card.resumed.iter().filter(|r| r.as_str() != rid).cloned());
            let mut later: Vec<(String, PRow)> = vec![];
            for r in &runs {
                for row in pivot_rows(data_dir, r) {
                    if down.contains(&row.step.as_str()) {
                        later.push((r.clone(), row));
                    }
                }
            }
            // Newest first; equal keys keep their order (reverse=True is stable).
            later.sort_by(|a, b| {
                b.1.at.partial_cmp(&a.1.at).unwrap_or(std::cmp::Ordering::Equal).then_with(|| b.1.step.cmp(&a.1.step))
            });
            for (rid, row) in later {
                if row.rev == "reversible" {
                    let mut path = String::new();
                    if let Some(j) = &row.rjson {
                        if let Ok(Value::Obj(o)) = loads_py(j, 900) {
                            match o.get("path") {
                                Some(Value::Str(s)) => path = s.clone(),
                                Some(other) => path = crate::warden::pyjson::dumps(other, true),
                                None => {}
                            }
                        }
                    }
                    let mut u = Object::new();
                    u.set("run_id", rid.as_str()).set("step_id", row.step.as_str()).set("path", path.as_str());
                    undo.push(Value::Obj(u));
                } else if row.rev == "irreversible" {
                    let mut u = Object::new();
                    u.set("run_id", rid.as_str()).set("step_id", row.step.as_str());
                    skipped.push(Value::Obj(u));
                }
            }
        }
    }
    let mut apply_v: Option<Object> = None;
    let (alt, cur) = (where_of(card, to), where_of(card, &chosen));
    if let Some(a) = &alt {
        if a.value("kind").as_str() == Some("output") {
            if let Some(t) = a.value("text").as_str() {
                let mut o = Object::new();
                o.set("kind", "output").set("text", t);
                apply_v = Some(o);
            }
        }
    }
    if apply_v.is_none() {
        if let (Some(a), Some(c)) = (&alt, &cur) {
            if a.value("kind").as_str() == Some("worktree") && c.value("kind").as_str() == Some("worktree") {
                if let (Some(ap), Some(cp), Some(ab), Some(ar), Some(cb), Some(cr)) = (
                    a.value("path").as_str(),
                    c.value("path").as_str(),
                    a.value("base").as_str(),
                    a.value("repo").as_str(),
                    c.value("base").as_str(),
                    c.value("repo").as_str(),
                ) {
                    if crate::branch_git::usable(ab, ar) && crate::branch_git::usable(cb, cr) {
                        let mut o = Object::new();
                        o.set("kind", "worktree_patch").set("from", ap).set("from_base", ab).set("from_repo", ar);
                        o.set("onto", cp).set("onto_base", cb).set("onto_repo", cr);
                        apply_v = Some(o);
                    }
                }
            }
        }
    }
    let follow_up: Option<String> = if sc.cost == rank::FOLLOW_UP {
        Some(format!("Follow-up task: redo the work of choice {} with {to} in place of {chosen}. {}", card.id(), sc.text))
    } else if apply_v.is_none() {
        Some(format!(
            "Follow-up task: apply {to} in place of {chosen} by hand; its place cannot be patched onto the chosen one."
        ))
    } else {
        None
    };
    let mut p = Object::new();
    p.set("choice_id", card.id()).set("to", to).set("chosen", chosen.as_str()).set("cost", sc.cost);
    p.set("text", sc.text.as_str()).set("undo", Value::List(undo)).set("skipped", Value::List(skipped));
    p.set("apply", apply_v.map(Value::Obj).unwrap_or(Value::Null)).set("follow_up", follow_up);
    Ok(p)
}

fn git_run(input: Option<&[u8]>, args: &[&str]) -> (Vec<u8>, String, bool) {
    let mut cmd = crate::branch_git::command();
    cmd.args(args).stdout(Stdio::piped()).stderr(Stdio::piped());
    cmd.stdin(if input.is_some() { Stdio::piped() } else { Stdio::null() });
    let Ok(mut child) = cmd.spawn() else { return (vec![], String::new(), false) };
    if let (Some(data), Some(mut stdin)) = (input, child.stdin.take()) {
        let _ = stdin.write_all(data);
    }
    match child.wait_with_output() {
        Ok(o) => (o.stdout, strip(&decode_utf8_replace(&o.stderr)).to_string(), o.status.success()),
        Err(_) => (vec![], String::new(), false),
    }
}

fn result(applied: bool) -> Object {
    let mut o = Object::new();
    o.set("applied", applied);
    o
}

fn fail(done: &[Value], why: String) -> Object {
    let mut o = result(false);
    o.set("undone", Value::List(done.to_vec())).set("why", why);
    o
}

fn names_of(path: &str, base: &str, repo: &str) -> Vec<String> {
    let Some(out) = crate::branch_git::diff(path, base, repo, &["--name-only", "--no-renames", "-z", "--cached"], false, std::time::Duration::ZERO) else {
        return vec![];
    };
    let mut names: Vec<String> = decode_utf8_replace(&out).split('\0').filter(|f| !f.is_empty()).map(String::from).collect();
    names.sort();
    names
}

/// `rank_pivot._patch`: take the chosen edits out of the chosen worktree and
/// put the alternative's in, both read against their recorded base commits.
/// When the alternative does not apply, every touched path goes back to the
/// base and the chosen edits are applied again, as adopt puts a tree back.
/// None on success.
fn patch(a: &Object, done: &[Value]) -> Option<Object> {
    let s = |k: &str| a.value(k).as_str().unwrap_or("").to_string();
    let (onto, from) = (s("onto"), s("from"));
    let (ob, orp, fb, frp) = (s("onto_base"), s("onto_repo"), s("from_base"), s("from_repo"));
    for (w, repo) in [(&onto, &orp), (&from, &frp)] {
        if !crate::branch_git::owns(w, repo) {
            return Some(fail(done, format!("{w} no longer belongs to {repo}; nothing was changed")));
        }
    }
    for w in [&onto, &from] {
        crate::branch_git::stage(w, false);
    }
    let none = std::time::Duration::ZERO;
    let mine = crate::branch_git::diff(&onto, &ob, &orp, &["--cached"], false, none);
    let theirs = crate::branch_git::diff(&from, &fb, &frp, &["--cached"], false, none);
    let (Some(mine), Some(theirs)) = (mine, theirs) else {
        return Some(fail(done, "git diff failed; nothing was changed".into()));
    };
    let mine_files = names_of(&onto, &ob, &orp);
    let theirs_files = names_of(&from, &fb, &frp);
    let before: Vec<String> =
        theirs_files.iter().filter(|f| std::fs::symlink_metadata(format!("{onto}/{f}")).is_ok()).cloned().collect();
    if !mine.is_empty() {
        let (_, e, ok) = git_run(Some(&mine), &["-C", &onto, "apply", "-R", "--index"]);
        if !ok {
            return Some(fail(done, format!("git apply -R failed, the chosen edits stay: {e}")));
        }
    }
    let (_, e, ok) = git_run(Some(&theirs), &["-C", &onto, "apply", "--3way", "--index"]);
    if ok {
        return None;
    }
    let mut why = format!("git apply failed: {e}");
    let mut touched: Vec<String> = vec![];
    for f in mine_files.iter().chain(theirs_files.iter()) {
        if !touched.contains(f) {
            touched.push(f.clone());
        }
    }
    touched.sort();
    let mut ls: Vec<&str> = vec!["-C", &onto, "ls-tree", "-r", "--name-only", "-z", &ob, "--"];
    ls.extend(touched.iter().map(|f| f.as_str()));
    let (tr, _, _) = git_run(None, &ls);
    let tracked: Vec<String> = decode_utf8_replace(&tr).split('\0').map(String::from).collect();
    let mut rs: Vec<&str> = vec!["-C", &onto, "reset", "-q", "--"];
    rs.extend(touched.iter().map(|f| f.as_str()));
    git_run(None, &rs);
    for f in &touched {
        if tracked.contains(f) {
            git_run(None, &["-C", &onto, "checkout", &ob, "--", f]);
        } else if mine_files.contains(f) || !before.contains(f) {
            // The chosen edit made this file, or the failed merge wrote it
            // (the alternative's new file). A file only the alternative adds
            // may already have been in the tree (an ignored one): it is not
            // ours to remove.
            let _ = std::fs::remove_file(format!("{onto}/{f}"));
        }
    }
    if !mine.is_empty() {
        let (_, be, bok) = git_run(Some(&mine), &["-C", &onto, "apply", "--index"]);
        if !bok {
            why.push_str(&format!("; the chosen edits could not be put back: {be}"));
        } else {
            why.push_str("; the chosen edits are back");
        }
    }
    Some(fail(done, why))
}

/// `rank_pivot.apply`.
pub fn apply(p: &Object, data_dir: &str) -> Object {
    if p.value("cost").as_str() == Some(rank::FOLLOW_UP) || !p.value("follow_up").is_null() {
        let mut o = result(false);
        o.set("follow_up", p.value("follow_up").clone()).set("why", p.value("text").clone());
        return o;
    }
    let mut done: Vec<Value> = vec![];
    let undo = match p.value("undo") {
        Value::List(l) => l.clone(),
        _ => vec![],
    };
    if !undo.is_empty() {
        let j = match crate::tracejournal::Journal::open_read_only(data_dir) {
            Ok(j) => j,
            Err(e) => {
                let mut o = result(false);
                o.set("why", format!("{e:?}"));
                return o;
            }
        };
        let mut by_step: HashMap<(String, String), crate::tracejournal::runs::ReceiptRow> = HashMap::new();
        let mut seen: Vec<String> = vec![];
        for u in &undo {
            let rid = u.as_obj().map(|o| o.value("run_id").as_str().unwrap_or("").to_string()).unwrap_or_default();
            if seen.contains(&rid) {
                continue;
            }
            seen.push(rid.clone());
            match j.receipts(&rid) {
                Ok(rows) => {
                    for r in rows {
                        by_step.insert((r.run_id.clone(), r.step_id.clone()), r);
                    }
                }
                Err(e) => {
                    let mut o = result(false);
                    o.set("why", format!("{e:?}"));
                    return o;
                }
            }
        }
        for uv in &undo {
            let u = uv.as_obj().cloned().unwrap_or_default();
            let step = u.value("step_id").as_str().unwrap_or("").to_string();
            let rid = u.value("run_id").as_str().unwrap_or("").to_string();
            let Some(r) = by_step.get(&(rid, step.clone())) else {
                let mut o = result(false);
                o.set("why", format!("step {step} has no reversal; stopped"));
                return o;
            };
            let (Some(rev), Some("reversible")) = (&r.reversal, r.reversibility.as_deref()) else {
                let mut o = result(false);
                o.set("why", format!("step {step} has no reversal; stopped"));
                return o;
            };
            let h = match crate::deeds::parse_handle(&Value::Obj(rev.clone())) {
                Ok(h) => h,
                Err(e) => {
                    let mut o = result(false);
                    o.set("why", format!("{e:?}"));
                    return o;
                }
            };
            if let Err(e) = crate::deeds::apply(&h) {
                let mut o = result(false);
                o.set("why", format!("{e:?}"));
                return o;
            }
            done.push(step.into());
        }
    }
    let a = p.value("apply").as_obj().cloned();
    if let Some(a) = &a {
        if a.value("kind").as_str() == Some("worktree_patch") {
            if let Some(r) = patch(a, &done) {
                return r;
            }
        }
    }
    let mut o = result(true);
    o.set("undone", Value::List(done)).set("applied_kind", a.map(|a| a.value("kind").clone()).unwrap_or(Value::Null));
    o
}

/// `rank_pivot.plan_text`.
pub fn plan_text(p: &Object) -> String {
    let s = |v: &Value| v.as_str().unwrap_or("").to_string();
    let mut b = format!(
        "Pivot {}: {} to {} ({})\n{}\n",
        s(p.value("choice_id")),
        s(p.value("chosen")),
        s(p.value("to")),
        s(p.value("cost")),
        s(p.value("text"))
    );
    if let Value::List(l) = p.value("undo") {
        for u in l {
            if let Some(o) = u.as_obj() {
                b.push_str(&format!(
                    "  undo step {} of {}: {}\n",
                    s(o.value("step_id")),
                    s(o.value("run_id")),
                    s(o.value("path"))
                ));
            }
        }
    }
    if let Value::List(l) = p.value("skipped") {
        for u in l {
            if let Some(o) = u.as_obj() {
                b.push_str(&format!("  cannot undo step {} of {}\n", s(o.value("step_id")), s(o.value("run_id"))));
            }
        }
    }
    if let Some(a) = p.value("apply").as_obj() {
        if a.value("kind").as_str() == Some("worktree_patch") {
            b.push_str(&format!("  then apply {} onto {}\n", s(a.value("from")), s(a.value("onto"))));
        } else {
            b.push_str("  then use the alternative's output\n");
        }
    }
    if let Some(f) = p.value("follow_up").as_str().filter(|f| !f.is_empty()) {
        b.push_str(&format!("{f}\n"));
    }
    b
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    fn g(dir: &str, args: &[&str]) -> String {
        let out = std::process::Command::new("git")
            .args(["-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"])
            .args(args)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_SYSTEM", "/dev/null")
            .output()
            .expect("git runs");
        assert!(out.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&out.stderr));
        String::from_utf8_lossy(&out.stdout).into_owned()
    }

    /// A repo with a.txt, two detached worktrees at HEAD: (root, repo, wa, wb, base, common).
    fn two(tag: &str) -> (String, String, String, String, String, String) {
        let root = std::env::temp_dir().join(format!("pivot-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        let root = root.display().to_string();
        let repo = format!("{root}/repo");
        std::fs::create_dir_all(&repo).unwrap();
        g(&repo, &["init", "-q", "-b", "main"]);
        std::fs::write(format!("{repo}/a.txt"), "one\n").unwrap();
        g(&repo, &["add", "-A"]);
        g(&repo, &["commit", "-qm", "init"]);
        let (wa, wb) = (format!("{root}/wa"), format!("{root}/wb"));
        g(&repo, &["worktree", "add", "-q", "--detach", &wa, "HEAD"]);
        g(&repo, &["worktree", "add", "-q", "--detach", &wb, "HEAD"]);
        let base = g(&repo, &["rev-parse", "HEAD"]).trim().to_string();
        let common = format!("{repo}/.git");
        (root, repo, wa, wb, base, common)
    }

    fn arg(from: &str, onto: &str, fb: &str, ob: &str, repo: &str) -> Object {
        let mut a = Object::new();
        a.set("kind", "worktree_patch").set("from", from).set("from_base", fb).set("from_repo", repo);
        a.set("onto", onto).set("onto_base", ob).set("onto_repo", repo);
        a
    }

    #[test]
    fn a_config_and_a_hook_the_child_wrote_do_not_run_under_a_pivot() {
        let (root, repo, wa, wb, base, common) = two("hook");
        // A base that is not the other's makes the alternative's patch reach
        // the three-way merge and fail there, so the restore path runs
        // checkout. The commit comes before the config is armed.
        std::fs::write(format!("{repo}/a.txt"), "two\n").unwrap();
        g(&repo, &["commit", "-qam", "second"]);
        let base2 = g(&repo, &["rev-parse", "HEAD"]).trim().to_string();
        let marker = format!("{root}/ran");
        let script = format!("#!/bin/sh\necho x >> {marker}\n");
        let hook = format!("{root}/hook.sh");
        std::fs::write(&hook, &script).unwrap();
        std::fs::set_permissions(&hook, std::fs::Permissions::from_mode(0o755)).unwrap();
        g(&repo, &["config", "core.fsmonitor", &hook]);
        let post = format!("{repo}/.git/hooks/post-checkout");
        std::fs::write(&post, &script).unwrap();
        std::fs::set_permissions(&post, std::fs::Permissions::from_mode(0o755)).unwrap();
        g(&wa, &["status"]);
        g(&wa, &["checkout", "HEAD", "--", "a.txt"]);
        assert!(std::path::Path::new(&marker).exists(), "the control: an unguarded git runs the child's config or hook");
        let _ = std::fs::remove_file(&marker);
        std::fs::write(format!("{wa}/a.txt"), "chosen\n").unwrap();
        std::fs::write(format!("{wb}/a.txt"), "alternative\n").unwrap();
        let _ = patch(&arg(&wb, &wa, &base2, &base, &common), &[]);
        assert!(!std::path::Path::new(&marker).exists(), "the child's config or hook ran under a pivot");
    }

    #[test]
    fn a_dot_git_at_a_siblings_git_dir_is_refused_by_a_pivot() {
        let (_, _, wa, wb, base, common) = two("sibling");
        let sgd = g(&wb, &["rev-parse", "--path-format=absolute", "--git-dir"]).trim().to_string();
        std::fs::write(format!("{wa}/evil.txt"), "evil\n").unwrap();
        std::fs::write(format!("{wa}/.git"), format!("gitdir: {sgd}\n")).unwrap();
        let before = std::fs::read(format!("{sgd}/index")).unwrap();
        let out = patch(&arg(&wb, &wa, &base, &base, &common), &[]).expect("a refusal");
        assert!(out.value("why").as_str().unwrap().contains("no longer belongs"));
        assert_eq!(std::fs::read(format!("{sgd}/index")).unwrap(), before);
    }

    #[test]
    fn a_failed_merge_leaves_no_new_file_of_the_alternative_behind() {
        let (_, repo, wa, wb, base, common) = two("merge");
        std::fs::write(format!("{repo}/a.txt"), "two\n").unwrap();
        g(&repo, &["commit", "-qam", "second"]);
        let base2 = g(&repo, &["rev-parse", "HEAD"]).trim().to_string();
        g(&wb, &["checkout", "-q", "--detach", &base2]);
        std::fs::write(format!("{wa}/a.txt"), "chosen\n").unwrap();
        std::fs::write(format!("{wb}/a.txt"), "alternative\n").unwrap();
        std::fs::write(format!("{wb}/only_b.txt"), "only in b\n").unwrap();
        let out = patch(&arg(&wb, &wa, &base2, &base, &common), &[]).expect("a failure");
        assert!(out.value("why").as_str().unwrap().contains("git apply failed"), "{:?}", out.value("why"));
        assert!(!std::path::Path::new(&format!("{wa}/only_b.txt")).exists());
        assert_eq!(std::fs::read_to_string(format!("{wa}/a.txt")).unwrap(), "chosen\n");
        assert_eq!(g(&wa, &["diff", "--name-only", "--diff-filter=U"]), "");
    }

    #[test]
    fn a_nested_repository_the_child_made_runs_no_program_under_a_pivot() {
        let (root, _, wa, wb, base, common) = two("nested");
        let marker = format!("{root}/ran-nested");
        let script = format!("{root}/clean.sh");
        std::fs::write(&script, format!("#!/bin/sh\necho x >> {marker}\ncat\n")).unwrap();
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        let sub = format!("{wa}/sub");
        std::fs::create_dir_all(&sub).unwrap();
        g(&sub, &["init", "-q"]);
        std::fs::write(format!("{sub}/s.txt"), "aaaa\n").unwrap();
        std::fs::write(format!("{sub}/.gitattributes"), "* filter=x\n").unwrap();
        g(&sub, &["add", "-A"]);
        g(&sub, &["commit", "-qm", "n"]);
        g(&sub, &["config", "filter.x.clean", &script]);
        g(&wa, &["add", "sub"]);
        let touch = |text: &str, sec: u64| {
            let f = format!("{sub}/s.txt");
            std::fs::write(&f, text).unwrap();
            let t = std::time::UNIX_EPOCH + std::time::Duration::from_secs(sec);
            std::fs::File::options().write(true).open(&f).unwrap().set_modified(t).unwrap();
        };
        touch("bbbb\n", 1);
        g(&wa, &["diff", "--name-only", &base]);
        assert!(std::path::Path::new(&marker).exists(), "the control: an unguarded diff runs the nested filter");
        let _ = std::fs::remove_file(&marker);
        touch("cccc\n", 7);
        std::fs::write(format!("{wa}/a.txt"), "chosen\n").unwrap();
        std::fs::write(format!("{wb}/a.txt"), "alternative\n").unwrap();
        assert!(patch(&arg(&wb, &wa, &base, &base, &common), &[]).is_none());
        assert!(!std::path::Path::new(&marker).exists(), "the nested filter ran under a pivot");
    }

    #[test]
    fn a_pivot_reads_a_child_that_committed_into_its_own_store() {
        let (_, _, wa, wb, base, common) = two("ownstore");
        for w in [&wa, &wb] {
            let name = std::path::Path::new(w).file_name().unwrap().to_string_lossy().to_string();
            let mut env = std::collections::HashMap::new();
            crate::supervise::treerun::branch_env(&mut env, &name, &common);
            std::fs::write(format!("{w}/a.txt"), format!("{name} edit\n")).unwrap();
            std::fs::write(format!("{w}/new_{name}.txt"), "n\n").unwrap();
            for args in [vec!["add", "-A"], vec!["-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "c"]] {
                let mut c = std::process::Command::new("git");
                c.args(["-C", w]).args(&args);
                for (k, v) in &env {
                    c.env(k, v);
                }
                assert!(c.output().unwrap().status.success());
            }
        }
        // A store that was not folded away is not read.
        assert!(patch(&arg(&wb, &wa, &base, &base, &common), &[]).is_some());
        for w in [&wa, &wb] {
            assert!(crate::branch_git::fold(&common, w, &base));
        }
        assert!(patch(&arg(&wb, &wa, &base, &base, &common), &[]).is_none());
        assert_eq!(std::fs::read_to_string(format!("{wa}/a.txt")).unwrap(), "wb edit\n");
        assert!(std::path::Path::new(&format!("{wa}/new_wb.txt")).exists());
        // Every blob the pivot staged is in the shared store, and the
        // repository stays whole.
        let ls = std::process::Command::new("git").args(["-C", &wa, "ls-files", "-s"]).output().unwrap();
        for line in String::from_utf8_lossy(&ls.stdout).lines() {
            let oid = line.split_whitespace().nth(1).unwrap();
            let ok = std::process::Command::new("git").args(["--git-dir", &common, "cat-file", "-e", oid]).status().unwrap();
            assert!(ok.success(), "staged blob not in the shared store: {line}");
        }
        for args in [vec!["fsck"], vec!["gc", "-q"], vec!["log", "--all", "--oneline"], vec!["rev-list", "--all"]] {
            let o = std::process::Command::new("git").args(["--git-dir", &common]).args(&args).output().unwrap();
            assert!(o.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&o.stderr));
        }
    }
}
