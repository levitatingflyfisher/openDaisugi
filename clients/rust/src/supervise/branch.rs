//! `opendaisugi.tree_branch`: one task, N children in parallel, each in its
//! own git worktree. Every branch's budget is reserved under one lock (all
//! or none); the children run, at most `parallel` at once; the starter's
//! hard checks run in each worktree; the survivors are ranked by `rank`, a
//! judge may be asked, and a card opens for the choice. The Go client's
//! `internal/supervise/branch.go` is the reference.

use std::collections::HashMap;
use std::io::Write;
use std::path::Path;
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use num_bigint::BigInt;

use super::treerun::{NodeOpts, NodeRun, Starter};
use crate::agentprof::Profiles;
use crate::branch_git;
use crate::tree::{self, Fail, SpawnOpts};
use crate::warden::py::text::{decode_utf8_replace, strip};
use crate::warden::pyjson::{canonical_json_ascii, Object, Value};
use crate::warden::sha256::hexdigest;

pub const DEFAULT_PARALLEL: usize = 4;

fn git(args: &[&str]) -> (Vec<u8>, Vec<u8>, bool) {
    match branch_git::command().args(args).stdin(Stdio::null()).output() {
        Ok(o) => (o.stdout, o.stderr, o.status.success()),
        Err(_) => (vec![], vec![], false),
    }
}

fn git_in(input: &[u8], args: &[&str]) -> (Vec<u8>, Vec<u8>, bool) {
    let mut cmd = branch_git::command();
    cmd.args(args).stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::piped());
    let Ok(mut child) = cmd.spawn() else { return (vec![], vec![], false) };
    if let Some(mut si) = child.stdin.take() {
        let _ = si.write_all(input);
    }
    match child.wait_with_output() {
        Ok(o) => (o.stdout, o.stderr, o.status.success()),
        Err(_) => (vec![], vec![], false),
    }
}

fn text(b: &[u8]) -> String {
    strip(&decode_utf8_replace(b)).to_string()
}

/// `tree_branch.branches_dir`.
pub fn branches_dir(data_dir: &str) -> String {
    format!("{data_dir}/tree/branches")
}

fn git_top(cwd: &str) -> Result<String, String> {
    let (out, _, ok) = git(&["-C", cwd, "rev-parse", "--show-toplevel"]);
    if !ok {
        return Err(format!("branching needs a git workspace; {cwd} is not one"));
    }
    Ok(text(&out))
}

/// `tree_branch.repo_of`: the commit HEAD names and the repository's git
/// directory, as the parent sees them now. The parent records both on the
/// branch rows.
fn repo_of(top: &str) -> Result<(String, String), String> {
    let (head, _, ok) = git(&["-C", top, "rev-parse", "HEAD"]);
    let base = if ok { text(&head) } else { String::new() };
    match (tree::is_object_id(&base), branch_git::common_dir(top)) {
        (true, Some(common)) => Ok((base, common)),
        _ => Err(format!("branching needs a git workspace with a commit; {top} has none")),
    }
}

/// `tree_branch.make_worktree`: a detached worktree of `top`'s repository at
/// `base` (HEAD when empty).
pub fn make_worktree(top: &str, data_dir: &str, session: &str, base: &str) -> Result<String, String> {
    if !base.is_empty() && !tree::is_object_id(base) {
        return Err(format!("the worktree for {session} could not be made: base is not a commit id"));
    }
    let dir = branches_dir(data_dir);
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new().recursive(true).mode(0o700).create(&dir).map_err(|e| e.to_string())?;
    }
    let dest = format!("{dir}/{session}");
    let at = if base.is_empty() { "HEAD" } else { base };
    let (_, err, ok) = git(&["-C", top, "worktree", "add", "--detach", &dest, at]);
    if !ok {
        let e = text(&err);
        let last = e.lines().last().unwrap_or("");
        return Err(format!("the worktree for {session} could not be made: {last}"));
    }
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&dest, std::fs::Permissions::from_mode(0o700));
    }
    Ok(dest)
}

/// `tree_branch.branch_ref`: the base commit and repository the parent
/// recorded for a branch.
fn branch_ref(data_dir: &str, session: &str) -> Option<(String, String)> {
    let t = tree::read_tree(data_dir);
    let n = t.nodes.get(session)?;
    match (&n.base, &n.repo) {
        (Some(b), Some(r)) if branch_git::usable(b, r) => Some((b.clone(), r.clone())),
        _ => None,
    }
}

/// `tree_branch.remove_worktree`. It never fails. `repo` is the git
/// directory the parent recorded; the worktree's own `.git` pointer is never
/// asked, because the child can rewrite it.
pub fn remove_worktree(path: &str, repo: &str) {
    if !repo.is_empty() {
        let gd = format!("--git-dir={repo}");
        git(&[&gd, "worktree", "remove", "--force", path]);
        git(&[&gd, "worktree", "prune"]);
    }
    let _ = std::fs::remove_dir_all(path);
}

/// `tree_branch.discard_worktree`: remove a branch worktree whose fold
/// failed, and its admin directory in `repo` whatever the child did to it.
/// The parent takes its rights back and drops a lock first, then asks git,
/// then removes what is left of `<repo>/worktrees/<name>` itself. It never
/// fails.
pub fn discard_worktree(path: &str, repo: &str) {
    let name = std::path::Path::new(path).file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
    let admin = format!("{repo}/worktrees/{name}");
    branch_git::reclaim(&admin);
    branch_git::drop_tree(&format!("{admin}/locked"));
    remove_worktree(path, repo);
    branch_git::reclaim(&admin);
    branch_git::drop_tree(&admin);
    git(&[&format!("--git-dir={repo}"), "worktree", "prune"]);
}

/// The fence flag a branch run refuses.
pub const WEAK_IPC: &str = "--allow-weak-ipc";

/// The fence flag that confines nothing; a branch run refuses it (R5-2).
pub const NO_FENCE: &str = "--no-fence";

/// `tree_branch.NO_FENCE_TEXT`.
pub const NO_FENCE_TEXT: &str = "A branch fence cannot take --no-fence: the run would treat the branches as fenced while nothing confines them. Remove the flag, or run the branches without a fence. Nothing was started.";

/// `tree_branch.WEAK_IPC_TEXT`.
pub const WEAK_IPC_TEXT: &str = "A branch run cannot take --allow-weak-ipc: without the socket fence a child can ask the session bus to start a process the run cannot stop. Remove the flag, or run the branches without a fence. Nothing was started.";

/// `tree_branch.reftable_text`.
pub fn reftable_text(repo: &str, format: &str) -> String {
    format!(
        "{repo} keeps its refs in the {format} format, and a fenced branch run can fold a child's refs only in the files format. Run the branches without a fence, or convert the repository with git refs migrate --ref-format=files. Nothing was started."
    )
}

fn branch_diff(worktree: &str, base: &str, repo: &str) -> Vec<u8> {
    branch_git::diff(worktree, base, repo, &[], true, Duration::ZERO).unwrap_or_default()
}

/// `tree_branch.branch_hash` (W3K-10).
pub fn branch_hash(worktree: &str, base: &str, repo: &str, answer: &str) -> String {
    let mut data = branch_diff(worktree, base, repo);
    data.push(0);
    data.extend_from_slice(answer.as_bytes());
    format!("sha256:{}", hexdigest(&data))
}

fn diff_lines(worktree: &str, base: &str, repo: &str) -> usize {
    decode_utf8_replace(&branch_diff(worktree, base, repo))
        .lines()
        .filter(|l| (l.starts_with('+') || l.starts_with('-')) && !l.starts_with("+++") && !l.starts_with("---"))
        .count()
}

const SCRUB_KEEP: [&str; 6] = ["PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TERM"];

/// `tree_branch.scrubbed`.
pub fn scrubbed(environ: &HashMap<String, String>) -> HashMap<String, String> {
    environ
        .iter()
        .filter(|(k, _)| SCRUB_KEEP.contains(&k.as_str()) && !crate::agentprof::is_key_name(k))
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect()
}

/// One hard check of a branch.
#[derive(Clone)]
pub struct Check {
    pub name: String,
    pub argv: Vec<String>,
    pub required: bool,
}

fn run_checks(checks: &[Check], worktree: &str, env: &HashMap<String, String>, timeout_s: u64, ran: bool) -> Vec<Value> {
    let mut out = vec![];
    for c in checks {
        let mut res = "not_run";
        if ran {
            res = "fail";
            if let Some((prog, rest)) = c.argv.split_first() {
                let child = Command::new(prog)
                    .args(rest)
                    .current_dir(worktree)
                    .env_clear()
                    .envs(env)
                    .stdin(Stdio::null())
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .spawn();
                if let Ok(mut ch) = child {
                    let deadline = Instant::now() + Duration::from_secs(timeout_s);
                    loop {
                        match ch.try_wait() {
                            Ok(Some(st)) => {
                                if st.success() {
                                    res = "pass";
                                }
                                break;
                            }
                            Ok(None) if Instant::now() >= deadline => {
                                let _ = ch.kill();
                                let _ = ch.wait();
                                break;
                            }
                            Ok(None) => std::thread::sleep(Duration::from_millis(10)),
                            Err(_) => break,
                        }
                    }
                }
            }
        }
        let mut o = Object::new();
        o.set("name", c.name.as_str()).set("required", c.required).set("result", res);
        out.push(Value::Obj(o));
    }
    out
}

/// `run_branches`'s keywords.
pub struct BranchOpts {
    pub parent: String,
    pub envelope: Object,
    pub profiles: Profiles,
    pub profile_name: Option<String>,
    pub model: String,
    pub tokens: Option<BigInt>,
    pub turns: Option<BigInt>,
    pub parent_tokens_used: Option<BigInt>,
    pub parent_turns_used: Option<BigInt>,
    pub task: String,
    pub tools: Vec<String>,
    pub tools_repr: String,
    pub max_turns: Option<BigInt>,
    pub cwd: String,
    pub timeout_s: u64,
    pub timeout_ms: u32,
    pub attempts: usize,
    pub why: String,
    pub parallel: usize,
    pub checks: Vec<Check>,
    pub judge: Option<String>,
    /// The child starter; tests swap it. None is the Starter's `run_node`.
    pub start: Option<StartFn>,
    /// The fold; tests swap it to make a fold fail. None is `branch_git::fold`.
    pub fold: Option<fn(&str, &str, &str) -> bool>,
}

/// A child starter for `run_branches` (Go's `BranchOpts.Start`).
pub type StartFn = std::sync::Arc<dyn Fn(&str, &str, NodeOpts) -> NodeRun + Send + Sync>;

/// `tree_branch.BranchRun`.
pub struct BranchRun {
    pub status: &'static str,
    pub leader_session: Option<String>,
    pub answer: Option<String>,
    pub choice_id: Option<String>,
    pub ranking: Option<Object>,
    pub branches: Vec<Value>,
    pub reasons: Vec<String>,
    pub ranking_id: Option<String>,
    pub ask_id: Option<String>,
    /// The fence is wrong for a branch: exit 2.
    pub usage: bool,
}

impl BranchRun {
    fn refused(reasons: Vec<String>) -> BranchRun {
        BranchRun {
            status: "refused",
            leader_session: None,
            answer: None,
            choice_id: None,
            ranking: None,
            branches: vec![],
            reasons,
            ranking_id: None,
            ask_id: None,
            usage: false,
        }
    }

    /// `BranchRun.doc`.
    pub fn doc(&self) -> Object {
        let s = |v: &Option<String>| v.clone().map(Value::Str).unwrap_or(Value::Null);
        let mut o = Object::new();
        o.set("status", self.status).set("session", s(&self.leader_session)).set("answer", s(&self.answer));
        o.set("reasons", Value::List(self.reasons.iter().map(|r| Value::Str(r.clone())).collect()));
        o.set("branches", Value::List(self.branches.clone())).set("leader", s(&self.leader_session));
        o.set("choice_id", s(&self.choice_id));
        o.set("ranking", self.ranking.clone().map(Value::Obj).unwrap_or(Value::Null));
        o.set("ranking_id", s(&self.ranking_id)).set("ask_id", s(&self.ask_id));
        o
    }
}

/// The judge loop over a fit: ask while the fit names a next pair and a
/// vote was recorded. Returns the last fit.
pub type JudgeFn<'a> = &'a dyn Fn(&Value, &str, Object) -> Result<Object, String>;

fn opt_big(b: &Option<BigInt>) -> Value {
    b.as_ref().map(|n| Value::Int(n.to_string())).unwrap_or(Value::Null)
}

impl Starter {
    /// `tree_branch.run_branches`.
    pub fn run_branches(&self, data_dir: &str, warden_root: &str, o: BranchOpts, judge_fn: Option<JudgeFn>) -> Result<BranchRun, Fail> {
        let attempts = o.attempts;
        let parallel = if o.parallel == 0 { DEFAULT_PARALLEL } else { o.parallel }.min(attempts).max(1);
        let (top, base, repo) = match git_top(&o.cwd).and_then(|t| repo_of(&t).map(|(b, r)| (t, b, r))) {
            Ok(x) => x,
            Err(e) => return Ok(BranchRun::refused(vec![e])),
        };
        // The sessions, minted in order from the parent's proposals so far.
        let t = tree::read_tree(data_dir);
        let mut k = t.proposals.get(&o.parent).copied().unwrap_or(0);
        let mut sessions: Vec<String> = vec![];
        while sessions.len() < attempts {
            let sid = tree::mint_session(&o.parent, k);
            k += 1;
            if t.nodes.contains_key(&sid) || tree::envelope_exists(warden_root, &sid) {
                continue;
            }
            sessions.push(sid);
        }
        if !self.fence.is_empty() {
            let words = &self.fence[1..];
            if words.iter().any(|w| w == WEAK_IPC || w.starts_with(&format!("{WEAK_IPC}="))) {
                let mut r = BranchRun::refused(vec![WEAK_IPC_TEXT.to_string()]);
                r.usage = true;
                return Ok(r);
            }
            if words.iter().any(|w| w == NO_FENCE || w.starts_with(&format!("{NO_FENCE}="))) {
                let mut r = BranchRun::refused(vec![NO_FENCE_TEXT.to_string()]);
                r.usage = true;
                return Ok(r);
            }
            let fmt = branch_git::ref_format(&repo);
            if fmt != "files" {
                let mut r = BranchRun::refused(vec![reftable_text(&repo, &fmt)]);
                r.usage = true;
                return Ok(r);
            }
            let mut env_file = format!("{warden_root}/envelopes/default.json");
            for (i, w) in words.iter().enumerate() {
                if w == "--envelope" && i + 1 < words.len() {
                    env_file = words[i + 1].clone();
                } else if let Some(v) = w.strip_prefix("--envelope=") {
                    env_file = v.to_string();
                }
            }
            let home = self.env.get("HOME").cloned().unwrap_or_default();
            // The fence reads a relative grant against its workspace, which for
            // a branch child is the child's worktree.
            for sid in &sessions {
                let wt = format!("{}/{sid}", branches_dir(data_dir));
                if let Some(grant) = crate::supervise::treerun::fence_covers(words, &env_file, &repo, &home, &wt) {
                    let mut r = BranchRun::refused(vec![format!(
                        "A branch child needs the repository's git directory read only, but the fence also grants {grant} read and write, which covers {repo}. Nothing was started."
                    )]);
                    r.usage = true;
                    return Ok(r);
                }
            }
        }
        let canon = canonical_json_ascii(&Value::List(vec![
            o.parent.as_str().into(),
            o.task.as_str().into(),
            Value::List(sessions.iter().map(|s| Value::Str(s.clone())).collect()),
        ]));
        let ranking_id = format!("br_{}", &hexdigest(canon.as_bytes())[..12]);
        let mut made: Vec<String> = vec![];
        let undo_worktrees = |made: &[String]| {
            for w in made {
                remove_worktree(w, &repo);
            }
            let _ = std::fs::remove_dir(branches_dir(data_dir));
        };
        for sid in &sessions {
            match make_worktree(&top, data_dir, sid, &base) {
                Ok(w) => made.push(w),
                Err(e) => {
                    undo_worktrees(&made);
                    return Ok(BranchRun::refused(vec![e]));
                }
            }
        }
        let prof = o.profile_name.clone().filter(|p| p.as_str() != crate::agentprof::DEFAULT);
        let sp = tree::spawn_branches(
            data_dir,
            warden_root,
            &o.envelope,
            &o.parent,
            &sessions,
            o.tokens.clone(),
            o.turns.clone(),
            o.timeout_ms,
            SpawnOpts {
                profile: prof,
                model: if o.model.is_empty() { None } else { Some(o.model.clone()) },
                parent_tokens_used: o.parent_tokens_used.clone(),
                parent_turns_used: o.parent_turns_used.clone(),
                branch_of: Some(ranking_id.clone()),
                why: Some(o.why.clone()),
                base: Some(base.clone()),
                repo: Some(repo.clone()),
            },
        );
        let sp = match sp {
            Ok(s) => s,
            Err(e) => {
                undo_worktrees(&made);
                return Err(e);
            }
        };
        if sp.status != "started" {
            undo_worktrees(&made);
            let mut out = BranchRun::refused(sp.reasons);
            if sp.status == "asked" {
                out.status = "asked";
                out.ask_id = Some(sp.ask_id);
            }
            return Ok(out);
        }

        let results: Vec<Mutex<Option<(NodeRun, u128)>>> = (0..attempts).map(|_| Mutex::new(None)).collect();
        let next = AtomicUsize::new(0);
        let sweep = super::sweep::Sweep::new();
        std::thread::scope(|sc| {
            for _ in 0..parallel {
                sc.spawn(|| loop {
                    let i = next.fetch_add(1, Ordering::SeqCst);
                    if i >= attempts {
                        break;
                    }
                    let t0 = Instant::now();
                    let sid = sessions[i].clone();
                    let opts = NodeOpts {
                        parent: o.parent.clone(),
                        session: Some(sid.clone()),
                        envelope: o.envelope.clone(),
                        profiles: o.profiles.clone(),
                        profile_name: o.profile_name.clone(),
                        model: o.model.clone(),
                        tokens: o.tokens.clone(),
                        turns: o.turns.clone(),
                        parent_tokens_used: o.parent_tokens_used.clone(),
                        parent_turns_used: o.parent_turns_used.clone(),
                        task: o.task.clone(),
                        tools: o.tools.clone(),
                        tools_repr: o.tools_repr.clone(),
                        max_turns: o.max_turns.clone(),
                        cwd: made[i].clone(),
                        timeout_s: o.timeout_s,
                        default_model: "haiku".into(),
                        timeout_ms: o.timeout_ms,
                        reserved: true,
                        resume: String::new(),
                    };
                    let ran = match &o.start {
                        Some(f) => Ok(f(data_dir, warden_root, opts)),
                        None => self.run_node(data_dir, warden_root, opts),
                    };
                    let nr = match ran {
                        Ok(n) => n,
                        Err(f) => NodeRun {
                            status: "failed",
                            session: Some(sid),
                            answer: None,
                            tokens_used: None,
                            turns_used: None,
                            reasons: vec![format!("{f:?}")],
                            ask_id: None,
                            obj: Object::new(),
                            help: None,
                        },
                    };
                    *results[i].lock().unwrap() = Some((nr, t0.elapsed().as_millis()));
                });
            }
        });
        let results: Vec<(NodeRun, u128)> = results.into_iter().map(|m| m.into_inner().unwrap().expect("a branch ran")).collect();
        let swept = sweep.clear();

        // End every branch, in branch order, with its own use (W3K-13).
        for (sid, (nr, _)) in sessions.iter().zip(&results) {
            tree::end_subtree(data_dir, warden_root, sid, false);
            let _ = tree::end(data_dir, warden_root, sid, nr.tokens_used.clone(), nr.turns_used.clone());
        }
        if !swept {
            return Ok(BranchRun::refused(vec![super::sweep::SWEEP_TEXT.to_string()]));
        }
        // A fenced child wrote its objects to a store of its own. Fold it away
        // before any read. In a fenced run every worktree is folded, whatever
        // the child left: it may have deleted its store, or taken the parent's
        // rights on its admin directory. A branch that does not fold is out,
        // and its worktree is removed, so the repository names nothing of the
        // child's.
        let fenced = !self.fence.is_empty();
        let fold = o.fold.unwrap_or(branch_git::fold);
        let folded: Vec<bool> = made
            .iter()
            .map(|wt| {
                let ok = (!fenced && std::fs::symlink_metadata(branch_git::store(&repo, wt)).is_err())
                    || fold(&repo, wt, &base);
                if !ok {
                    discard_worktree(wt, &repo);
                }
                ok
            })
            .collect();

        let env = scrubbed(&self.env);
        let mut author = o.model.clone();
        if author.is_empty() {
            if let Ok(p) = o.profiles.lookup(o.profile_name.as_deref()) {
                author = p.model.unwrap_or_default();
            }
        }
        if author.is_empty() {
            author = "haiku".into();
        }
        let mut atts: Vec<Value> = vec![];
        let mut branches: Vec<Value> = vec![];
        let mut answers: HashMap<String, String> = HashMap::new();
        for (i, (sid, (nr, wall_ms))) in sessions.iter().zip(&results).enumerate() {
            let ran = (nr.status == "started" || nr.status == "ran") && folded[i];
            let answer = nr.answer.clone().unwrap_or_default();
            answers.insert(sid.clone(), answer.clone());
            let wt = &made[i];
            let mut first_test = Object::new();
            first_test.set("name", "the child ran").set("required", true).set("result", if ran { "pass" } else { "fail" });
            let mut tests = vec![Value::Obj(first_test)];
            tests.extend(run_checks(&o.checks, wt, &env, o.timeout_s, ran));
            let mut cost = Object::new();
            cost.set("wall_ms", Value::Int(wall_ms.to_string())).set("diff_lines", Value::Int(diff_lines(wt, &base, &repo).to_string()));
            if nr.tokens_used.is_some() {
                cost.set("tokens", opt_big(&nr.tokens_used));
            }
            let tr = strip(&answer);
            let first: String = if tr.is_empty() { String::new() } else { tr.replace("\r\n", "\n").replace('\r', "\n").lines().next().unwrap_or("").chars().take(80).collect() };
            let mut wh = Object::new();
            wh.set("kind", "worktree").set("path", wt.as_str()).set("base", base.as_str()).set("repo", repo.as_str());
            let mut att = Object::new();
            att.set("id", sid.as_str()).set("content_hash", branch_hash(wt, &base, &repo, &answer)).set("author", author.as_str());
            att.set("edge_proof", "ok").set("started_by_parent", true).set("tests", Value::List(tests.clone()));
            att.set("cost", cost).set("where", wh);
            if !first.is_empty() {
                let mut pv = Object::new();
                pv.set("summary", first.as_str());
                att.set("previews", pv);
            }
            atts.push(Value::Obj(att));
            let mut b = Object::new();
            b.set("session", sid.as_str()).set("status", if ran { "ran" } else { "failed" }).set("worktree", wt.as_str());
            b.set("tokens_used", opt_big(&nr.tokens_used)).set("turns_used", opt_big(&nr.turns_used));
            b.set("tests", Value::List(tests));
            branches.push(Value::Obj(b));
        }
        let mut doc = Object::new();
        doc.set("ranking_id", ranking_id.as_str()).set("task", o.task.as_str()).set("project", top.as_str());
        doc.set("attempts", Value::List(atts));
        if let Some(j) = &o.judge {
            let mut pol = Object::new();
            pol.set("judges", Value::List(vec![Value::Str(j.clone())]));
            doc.set("policy", pol);
        }
        let doc = Value::Obj(doc);
        let r = crate::rank::parse(&doc).map_err(Fail::Io)?;
        let mut res = crate::rank_judge::fit_doc(&r, data_dir);
        if o.judge.is_some() {
            if let Some(jf) = judge_fn {
                res = jf(&doc, data_dir, res).map_err(Fail::Io)?;
            }
        }
        if res.value("status").as_str() == Some("none_survived") {
            let mut reasons = vec![];
            if let Value::List(el) = res.value("eliminated") {
                for e in el {
                    if let Some(eo) = e.as_obj() {
                        let rs: Vec<String> = match eo.value("reasons") {
                            Value::List(l) => l.iter().map(|x| x.as_str().unwrap_or("").to_string()).collect(),
                            _ => vec![],
                        };
                        reasons.push(format!("{}: {}", eo.value("id").as_str().unwrap_or(""), rs.join("; ")));
                    }
                }
            }
            return Ok(BranchRun {
                status: "failed",
                leader_session: None,
                answer: None,
                choice_id: None,
                ranking: Some(res),
                branches,
                reasons,
                ranking_id: Some(ranking_id),
                ask_id: None,
                usage: false,
            });
        }
        let mut choice_id = None;
        let leader = res.value("leader").as_str().unwrap_or("").to_string();
        if matches!(res.value("order"), Value::List(l) if l.len() > 1) {
            let mut run = Object::new();
            run.set("run_id", ranking_id.as_str()).set("downstream", Value::List(vec![]));
            let adopt = format!("daisugi tree adopt {leader}");
            let row = crate::rank::opened_row(&r, &res, crate::rank::now(), Some(run), Some(&adopt));
            let id = row.value("choice_id").as_str().unwrap_or("").to_string();
            if !crate::rank::read_cards(data_dir).iter().any(|c| c.id() == id) {
                crate::rank::append_rows(data_dir, &[row]).map_err(|e| Fail::Io(e.to_string()))?;
            }
            choice_id = Some(id);
        }
        let answer = answers.get(&leader).cloned();
        Ok(BranchRun {
            status: "ran",
            leader_session: Some(leader),
            answer,
            choice_id,
            ranking: Some(res),
            branches,
            reasons: vec![],
            ranking_id: Some(ranking_id),
            ask_id: None,
            usage: false,
        })
    }
}

/// `tree_branch.prune_branches`: remove the worktrees of branches whose card
/// is closed, and those of ended branches that have no card once they are
/// older than the card decay (14 days). A running branch and a branch of an
/// open card stay, and so does a lone survivor that no card holds: `tree
/// adopt` is the only way its edits reach the parent.
pub fn prune_branches(data_dir: &str) -> Vec<String> {
    prune_branches_at(data_dir, crate::rank::now())
}

/// `prune_branches` at a given time.
pub fn prune_branches_at(data_dir: &str, now: f64) -> Vec<String> {
    let base = branches_dir(data_dir);
    let Ok(rd) = std::fs::read_dir(&base) else { return vec![] };
    let mut names: Vec<String> = rd.filter_map(|e| e.ok()).map(|e| e.file_name().to_string_lossy().to_string()).collect();
    names.sort();
    let t = tree::read_tree(data_dir);
    let cards = crate::rank::read_cards(data_dir);
    let mut removed = vec![];
    for name in names {
        let Some(node) = t.nodes.get(&name) else { continue };
        if node.state != "ended" {
            continue;
        }
        let rid = node.branch_of.clone().unwrap_or_default();
        match cards.iter().find(|c| c.opened.value("ranking_id").as_str() == Some(rid.as_str())) {
            None => {
                if !(node.ended_at != 0.0 && now - node.ended_at > crate::rank::DECAY_SECONDS) {
                    continue;
                }
            }
            Some(card) if card.close.is_none() => continue,
            Some(_) => {}
        }
        // A child may have taken the parent's rights on its admin dir, and a
        // sweep refusal skipped the fold: take them back first (R5-1).
        match node.repo.as_deref() {
            Some(repo) if !repo.is_empty() => discard_worktree(&format!("{base}/{name}"), repo),
            _ => remove_worktree(&format!("{base}/{name}"), ""),
        }
        removed.push(name);
    }
    let _ = std::fs::remove_dir(&base);
    removed
}

/// `tree_branch.Adopt`.
pub struct Adopt {
    pub status: &'static str,
    pub session: String,
    pub files: Vec<String>,
    pub why: String,
}

impl Adopt {
    fn new(status: &'static str, session: &str, files: Vec<String>, why: String) -> Adopt {
        Adopt { status, session: session.to_string(), files, why }
    }

    /// `Adopt.doc`.
    pub fn doc(&self) -> Object {
        let mut o = Object::new();
        o.set("status", self.status).set("session", self.session.as_str());
        o.set("files", Value::List(self.files.iter().map(|f| Value::Str(f.clone())).collect()));
        o.set("why", self.why.as_str());
        o
    }
}

/// `tree_branch.adopt`: apply a branch's edits to the parent's working
/// tree with `git apply --3way`, or change nothing.
pub fn adopt(data_dir: &str, session: &str, cwd: Option<&str>) -> Adopt {
    let wt = format!("{}/{session}", branches_dir(data_dir));
    if !tree::valid_session(session) || !Path::new(&wt).is_dir() {
        return Adopt::new("refused", session, vec![], format!("No branch worktree for {session}."));
    }
    let Some((base, repo)) = branch_ref(data_dir, session) else {
        return Adopt::new("refused", session, vec![], format!("The ledger has no recorded base for {session}; not adopted."));
    };
    if !branch_git::owns(&wt, &repo) {
        return Adopt::new("refused", session, vec![], format!("{}; nothing was read.", branch_git::lost(session, &repo)));
    }
    let cwd: String = match cwd {
        Some(c) => {
            if !branch_git::belongs(c, &repo) {
                return Adopt::new("refused", session, vec![], format!("{c} is not a work tree of {repo}."));
            }
            c.to_string()
        }
        None => Path::new(&repo).parent().map(|p| p.to_string_lossy().to_string()).unwrap_or_default(),
    };
    let (top_out, _, ok) = git(&["-C", &cwd, "rev-parse", "--show-toplevel"]);
    if !ok {
        return Adopt::new("refused", session, vec![], format!("{cwd} is not a git work tree."));
    }
    let top = text(&top_out);
    branch_git::intent(&wt);
    let patch = branch_git::diff(&wt, &base, &repo, &["--no-renames"], false, Duration::ZERO).unwrap_or_default();
    let names = branch_git::diff(&wt, &base, &repo, &["--name-only", "--no-renames", "-z"], false, Duration::ZERO).unwrap_or_default();
    let mut files: Vec<String> = decode_utf8_replace(&names).split('\0').filter(|f| !f.is_empty()).map(String::from).collect();
    files.sort();
    if patch.is_empty() || files.is_empty() {
        return Adopt::new("nothing", session, vec![], format!("{session} changed no file."));
    }
    let mut st_args: Vec<&str> = vec!["-C", &top, "status", "--porcelain", "-z", "--"];
    st_args.extend(files.iter().map(|s| s.as_str()));
    let (st, _, _) = git(&st_args);
    let mut dirty: Vec<String> =
        decode_utf8_replace(&st).split('\0').filter(|e| e.len() > 3).map(|e| e[3..].to_string()).collect();
    dirty.sort();
    dirty.dedup();
    if let Some(d) = dirty.first() {
        return Adopt::new("refused", session, files.clone(), format!("Refused: {d} has uncommitted changes in {top}; commit or stash first."));
    }
    let mut ls_args: Vec<&str> = vec!["-C", &top, "ls-tree", "-r", "--name-only", "-z", "HEAD", "--"];
    ls_args.extend(files.iter().map(|s| s.as_str()));
    let (tr, _, _) = git(&ls_args);
    let tracked: Vec<String> = decode_utf8_replace(&tr).split('\0').map(String::from).collect();
    let (_, errb, ok) = git_in(&patch, &["-C", &top, "apply", "--3way", "--whitespace=nowarn", "-"]);
    if !ok {
        let mut rs: Vec<&str> = vec!["-C", &top, "reset", "-q", "--"];
        rs.extend(files.iter().map(|s| s.as_str()));
        git(&rs);
        for f in &files {
            if tracked.contains(f) {
                git(&["-C", &top, "checkout", "HEAD", "--", f]);
            } else {
                let _ = std::fs::remove_file(format!("{top}/{f}"));
            }
        }
        let e = text(&errb);
        let last = e.lines().last().unwrap_or("");
        let mut why = format!("Refused: the edits of {session} do not apply cleanly to {top}");
        if last.is_empty() {
            why.push('.');
        } else {
            why.push_str(&format!(" ({last})."));
        }
        return Adopt::new("refused", session, files, why);
    }
    let n = files.len();
    Adopt::new("adopted", session, files, format!("Applied {n} files to {top}."))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn g(dir: &str, args: &[&str]) -> String {
        let out = Command::new("git")
            .args(["-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"])
            .args(args)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_SYSTEM", "/dev/null")
            .output()
            .expect("git runs");
        assert!(out.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&out.stderr));
        String::from_utf8_lossy(&out.stdout).into_owned()
    }

    fn setup(tag: &str) -> (String, String, String) {
        let root = std::env::temp_dir().join(format!("branch-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        let repo = format!("{}/repo", root.display());
        let data = format!("{}/data", root.display());
        std::fs::create_dir_all(&repo).unwrap();
        g(&repo, &["init", "-q", "-b", "main"]);
        std::fs::write(format!("{repo}/a.txt"), "one\n").unwrap();
        g(&repo, &["add", "-A"]);
        g(&repo, &["commit", "-qm", "base"]);
        let session = "n-aaaaaaaaaaaa".to_string();
        let (base, common) = repo_of(&repo).unwrap();
        let wt = make_worktree(&repo, &data, &session, &base).unwrap();
        std::fs::write(format!("{wt}/a.txt"), "two\n").unwrap();
        record_branch(&data, &session, &base, &common);
        (repo, data, session)
    }

    /// The parent's ledger rows for one branch: the root and the spawn row,
    /// which holds the base and the repository.
    fn record_branch(data: &str, session: &str, base: &str, repo: &str) {
        let dir = format!("{data}/tree");
        std::fs::create_dir_all(&dir).unwrap();
        let rows = format!(
            "{{\"event\": \"root\", \"session\": \"top\", \"envelope_id\": \"env_root\", \"tokens\": null, \"turns\": null, \"deadline\": null, \"ts\": 1.0}}\n\
             {{\"event\": \"spawn\", \"parent\": \"top\", \"session\": \"{session}\", \"envelope_id\": \"env_kid\", \"tokens\": null, \"turns\": null, \"deadline\": null, \"proved\": true, \"branch_of\": \"br_x\", \"why\": \"operator\", \"base\": \"{base}\", \"repo\": \"{repo}\", \"ts\": 2.0}}\n"
        );
        let mut f = std::fs::OpenOptions::new().create(true).append(true).open(format!("{dir}/ledger.jsonl")).unwrap();
        f.write_all(rows.as_bytes()).unwrap();
    }

    fn victim(root: &str) -> (String, String) {
        let v = format!("{root}/victim");
        std::fs::create_dir_all(&v).unwrap();
        g(&v, &["init", "-q", "-b", "main"]);
        std::fs::write(format!("{v}/secret.txt"), "keep me\n").unwrap();
        std::fs::write(format!("{v}/a.txt"), "victim\n").unwrap();
        g(&v, &["add", "-A"]);
        g(&v, &["commit", "-qm", "init"]);
        let vwt = format!("{root}/victim-wt");
        g(&v, &["worktree", "add", "-q", "--detach", &vwt, "HEAD"]);
        (v, vwt)
    }

    #[test]
    fn adopt_applies_the_branch_edits() {
        let (repo, data, session) = setup("ok");
        let res = adopt(&data, &session, None);
        assert_eq!(res.status, "adopted", "{}", res.why);
        assert_eq!(res.files, vec!["a.txt".to_string()]);
        assert_eq!(std::fs::read_to_string(format!("{repo}/a.txt")).unwrap(), "two\n");
    }

    #[test]
    fn adopt_refuses_a_dirty_file_and_a_conflict_and_changes_nothing() {
        let (repo, data, session) = setup("refuse");
        std::fs::write(format!("{repo}/a.txt"), "mine\n").unwrap();
        let res = adopt(&data, &session, None);
        assert_eq!(res.status, "refused");
        assert!(res.why.contains("a.txt has uncommitted changes"), "{}", res.why);
        assert_eq!(std::fs::read_to_string(format!("{repo}/a.txt")).unwrap(), "mine\n");
        g(&repo, &["commit", "-qam", "moved on"]);
        let res = adopt(&data, &session, None);
        assert_eq!(res.status, "refused");
        assert!(res.why.contains("do not apply cleanly"), "{}", res.why);
        assert_eq!(g(&repo, &["status", "--porcelain"]), "");
        for s in ["nope", "../x", ""] {
            assert_eq!(adopt(&data, s, None).status, "refused");
        }
    }

    #[test]
    fn the_branch_hash_follows_the_edits_and_the_answer() {
        let (_, data, session) = setup("hash");
        let wt = format!("{}/{session}", branches_dir(&data));
        let (base, repo) = branch_ref(&data, &session).unwrap();
        let h = branch_hash(&wt, &base, &repo, "answer");
        assert_eq!(h, branch_hash(&wt, &base, &repo, "answer"));
        assert_ne!(h, branch_hash(&wt, &base, &repo, "other"));
        assert!(h.starts_with("sha256:"));
    }

    #[test]
    fn a_base_note_that_names_an_output_file_writes_nothing() {
        let (repo, data, session) = setup("note");
        let wt = format!("{}/{session}", branches_dir(&data));
        let gd = g(&wt, &["rev-parse", "--path-format=absolute", "--git-dir"]).trim().to_string();
        let root = Path::new(&repo).parent().unwrap().display().to_string();
        let target = format!("{root}/pwned");
        std::fs::write(format!("{gd}/daisugi-base"), format!("--output={target}\n")).unwrap();
        let (base, rp) = branch_ref(&data, &session).unwrap();
        branch_hash(&wt, &base, &rp, "x");
        diff_lines(&wt, &base, &rp);
        assert_eq!(adopt(&data, &session, None).status, "adopted");
        assert!(!Path::new(&target).exists());
        let bad = format!("--output={target}");
        assert!(branch_git::diff(&wt, &bad, &rp, &[], true, Duration::ZERO).is_none());
        assert!(!Path::new(&target).exists());
    }

    #[test]
    fn a_redirected_dot_git_is_refused_and_no_repository_is_written() {
        let (repo, data, session) = setup("redirect");
        let root = Path::new(&repo).parent().unwrap().display().to_string();
        let (v, vwt) = victim(&root);
        let wt = format!("{}/{session}", branches_dir(&data));
        let vgd = g(&vwt, &["rev-parse", "--path-format=absolute", "--git-dir"]).trim().to_string();
        let before_wt = std::fs::read(format!("{vgd}/index")).unwrap();
        let before_main = std::fs::read(format!("{v}/.git/index")).unwrap();
        for target in [vgd.clone(), format!("{v}/.git")] {
            std::fs::write(format!("{wt}/.git"), format!("gitdir: {target}\n")).unwrap();
            let res = adopt(&data, &session, None);
            assert_eq!(res.status, "refused", "{target}");
            assert!(res.why.contains("no longer belongs to"), "{}", res.why);
        }
        assert_eq!(std::fs::read_to_string(format!("{v}/secret.txt")).unwrap(), "keep me\n");
        assert_eq!(g(&v, &["status", "--porcelain"]), "");
        assert_eq!(std::fs::read(format!("{vgd}/index")).unwrap(), before_wt);
        assert_eq!(std::fs::read(format!("{v}/.git/index")).unwrap(), before_main);
        let (base, rp) = branch_ref(&data, &session).unwrap();
        assert_eq!(diff_lines(&wt, &base, &rp), 0);
    }

    #[test]
    fn dot_git_at_the_main_git_dir_is_refused_and_the_main_index_stays() {
        for form in ["file", "symlink"] {
            let (repo, data, session) = setup(&format!("main-{form}"));
            let wt = format!("{}/{session}", branches_dir(&data));
            let main = format!("{repo}/.git");
            std::fs::write(format!("{wt}/b.txt"), "new\n").unwrap();
            let _ = std::fs::remove_file(format!("{wt}/.git"));
            if form == "file" {
                std::fs::write(format!("{wt}/.git"), format!("gitdir: {main}\n")).unwrap();
            } else {
                std::os::unix::fs::symlink(&main, format!("{wt}/.git")).unwrap();
            }
            let before = std::fs::read(format!("{main}/index")).unwrap();
            let (base, rp) = branch_ref(&data, &session).unwrap();
            diff_lines(&wt, &base, &rp);
            branch_hash(&wt, &base, &rp, "x");
            let res = adopt(&data, &session, None);
            assert_eq!(res.status, "refused", "{form}: {}", res.why);
            assert!(res.why.contains("no longer belongs to"), "{}", res.why);
            assert_eq!(std::fs::read(format!("{main}/index")).unwrap(), before, "{form}");
            assert_eq!(g(&repo, &["status", "--porcelain"]), "", "{form}");
        }
    }

    #[test]
    fn dot_git_at_a_siblings_git_dir_is_refused_and_the_sibling_index_stays() {
        let (repo, data, session) = setup("sibling");
        let (base, common) = branch_ref(&data, &session).unwrap();
        let sib_session = "n-bbbbbbbbbbbb";
        let sib = make_worktree(&repo, &data, sib_session, &base).unwrap();
        record_branch(&data, sib_session, &base, &common);
        let sgd = g(&sib, &["rev-parse", "--path-format=absolute", "--git-dir"]).trim().to_string();
        let wt = format!("{}/{session}", branches_dir(&data));
        std::fs::write(format!("{wt}/evil.txt"), "evil\n").unwrap();
        std::fs::write(format!("{wt}/.git"), format!("gitdir: {sgd}\n")).unwrap();
        let before = std::fs::read(format!("{sgd}/index")).unwrap();
        let status = g(&sib, &["status", "--porcelain"]);
        diff_lines(&wt, &base, &common);
        let res = adopt(&data, &session, None);
        assert_eq!(res.status, "refused");
        assert!(res.why.contains("no longer belongs to"), "{}", res.why);
        assert_eq!(std::fs::read(format!("{sgd}/index")).unwrap(), before);
        assert_eq!(g(&sib, &["status", "--porcelain"]), status);
    }

    /// The repository config and the hook a child could have written. The
    /// marker is what they create when they run.
    fn arm(repo: &str) -> String {
        use std::os::unix::fs::PermissionsExt;
        let root = Path::new(repo).parent().unwrap().display().to_string();
        let marker = format!("{root}/ran");
        let script = format!("#!/bin/sh\necho x >> {marker}\n");
        let hook = format!("{root}/hook.sh");
        std::fs::write(&hook, &script).unwrap();
        std::fs::set_permissions(&hook, std::fs::Permissions::from_mode(0o755)).unwrap();
        g(repo, &["config", "core.fsmonitor", &hook]);
        let post = format!("{repo}/.git/hooks/post-checkout");
        std::fs::write(&post, &script).unwrap();
        std::fs::set_permissions(&post, std::fs::Permissions::from_mode(0o755)).unwrap();
        marker
    }

    #[test]
    fn a_config_and_a_hook_the_child_wrote_do_not_run_under_the_parents_reads() {
        let (repo, data, session) = setup("armed");
        let wt = format!("{}/{session}", branches_dir(&data));
        let marker = arm(&repo);
        g(&wt, &["status"]);
        assert!(Path::new(&marker).exists(), "the control: an unguarded git runs the child's fsmonitor");
        let _ = std::fs::remove_file(&marker);
        let (base, rp) = branch_ref(&data, &session).unwrap();
        diff_lines(&wt, &base, &rp);
        branch_hash(&wt, &base, &rp, "x");
        branch_git::diff(&wt, &base, &rp, &[], true, Duration::ZERO);
        assert_eq!(adopt(&data, &session, None).status, "adopted");
        assert!(!Path::new(&marker).exists());
    }

    #[test]
    fn a_hook_the_child_wrote_does_not_run_on_a_refused_adopt() {
        let (repo, data, session) = setup("armed-refuse");
        std::fs::write(format!("{repo}/a.txt"), "other\n").unwrap();
        g(&repo, &["commit", "-qam", "moved on"]);
        let marker = arm(&repo);
        g(&repo, &["checkout", "HEAD", "--", "a.txt"]);
        assert!(Path::new(&marker).exists(), "the control: an unguarded checkout runs the hook");
        let _ = std::fs::remove_file(&marker);
        let res = adopt(&data, &session, None);
        assert_eq!(res.status, "refused");
        assert!(res.why.contains("do not apply cleanly"), "{}", res.why);
        assert!(!Path::new(&marker).exists());
    }

    #[test]
    fn adopt_into_another_repository_and_a_nul_session_are_refused() {
        let (repo, data, session) = setup("other");
        let root = Path::new(&repo).parent().unwrap().display().to_string();
        let (v, _) = victim(&root);
        assert_eq!(adopt(&data, &session, Some(&v)).status, "refused");
        assert_eq!(std::fs::read_to_string(format!("{v}/a.txt")).unwrap(), "victim\n");
        for s in ["a\0b", "default"] {
            assert_eq!(adopt(&data, s, None).status, "refused");
        }
    }

    #[test]
    fn a_lone_survivor_is_pruned_only_when_aged_out() {
        let (_, data, session) = setup("prune");
        let mut f = std::fs::OpenOptions::new().append(true).open(format!("{data}/tree/ledger.jsonl")).unwrap();
        writeln!(f, "{{\"event\": \"end\", \"session\": \"{session}\", \"tokens_used\": null, \"turns_used\": null, \"ts\": 3.0}}").unwrap();
        assert!(prune_branches_at(&data, 3.0 + 86400.0).is_empty());
        assert!(Path::new(&format!("{}/{session}", branches_dir(&data))).exists());
        assert_eq!(prune_branches_at(&data, 3.0 + 15.0 * 86400.0), vec![session]);
    }

    fn nested(wt: &str, root: &str) -> String {
        use std::os::unix::fs::PermissionsExt;
        let marker = format!("{root}/ran-nested");
        let script = format!("{root}/clean.sh");
        std::fs::write(&script, format!("#!/bin/sh\necho x >> {marker}\ncat\n")).unwrap();
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        let sub = format!("{wt}/sub");
        std::fs::create_dir_all(&sub).unwrap();
        g(&sub, &["init", "-q"]);
        std::fs::write(format!("{sub}/s.txt"), "aaaa\n").unwrap();
        std::fs::write(format!("{sub}/.gitattributes"), "* filter=x\n").unwrap();
        g(&sub, &["add", "-A"]);
        g(&sub, &["commit", "-qm", "n"]);
        g(&sub, &["config", "filter.x.clean", &script]);
        g(wt, &["add", "sub"]);
        marker
    }

    fn touch(wt: &str, text: &str, sec: u64) {
        let f = format!("{wt}/sub/s.txt");
        std::fs::write(&f, text).unwrap();
        let t = std::time::UNIX_EPOCH + Duration::from_secs(sec);
        std::fs::File::options().write(true).open(&f).unwrap().set_modified(t).unwrap();
    }

    #[test]
    fn a_nested_repository_the_child_made_runs_no_program_in_the_parent() {
        let (repo, data, session) = setup("nested");
        let wt = format!("{}/{session}", branches_dir(&data));
        let root = Path::new(&repo).parent().unwrap().display().to_string();
        let marker = nested(&wt, &root);
        let (base, rp) = branch_ref(&data, &session).unwrap();
        touch(&wt, "cccc\n", 2);
        g(&wt, &["diff", "--name-only", &base]);
        assert!(Path::new(&marker).exists(), "the control: an unguarded diff runs the nested filter");
        let _ = std::fs::remove_file(&marker);
        touch(&wt, "dddd\n", 5);
        diff_lines(&wt, &base, &rp);
        branch_hash(&wt, &base, &rp, "x");
        branch_git::diff(&wt, &base, &rp, &[], true, Duration::ZERO);
        assert_eq!(adopt(&data, &session, None).status, "adopted");
        assert!(!Path::new(&marker).exists(), "the nested repository's filter ran in the parent");
    }

    #[test]
    fn a_config_worktree_the_child_wrote_is_refused_and_runs_nothing() {
        let (repo, data, session) = setup("wtconfig");
        let wt = format!("{}/{session}", branches_dir(&data));
        g(&repo, &["config", "extensions.worktreeConfig", "true"]);
        let gd = g(&wt, &["rev-parse", "--path-format=absolute", "--git-dir"]).trim().to_string();
        let root = Path::new(&repo).parent().unwrap().display().to_string();
        let marker = format!("{root}/ran-wtconfig");
        let script = format!("{root}/clean.sh");
        std::fs::write(&script, format!("#!/bin/sh\necho x >> {marker}\ncat\n")).unwrap();
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        std::fs::write(format!("{gd}/config.worktree"), format!("[filter \"x\"]\n\tclean = {script}\n")).unwrap();
        std::fs::write(format!("{wt}/.gitattributes"), "* filter=x\n").unwrap();
        g(&wt, &["diff", "HEAD"]);
        assert!(Path::new(&marker).exists(), "the control: git runs the child's config.worktree");
        let _ = std::fs::remove_file(&marker);
        let (base, rp) = branch_ref(&data, &session).unwrap();
        let res = adopt(&data, &session, None);
        assert_eq!(res.status, "refused");
        assert!(res.why.contains("no longer belongs to"), "{}", res.why);
        assert!(branch_git::diff(&wt, &base, &rp, &[], true, Duration::ZERO).is_none());
        std::fs::remove_file(format!("{gd}/config.worktree")).unwrap();
        std::os::unix::fs::symlink(format!("{root}/nowhere"), format!("{gd}/config.worktree")).unwrap();
        assert!(!branch_git::owns(&wt, &rp), "a symlinked config.worktree counts too");
        assert!(!Path::new(&marker).exists());
    }

    #[test]
    fn a_non_fenced_starter_leaves_the_environment() {
        let mut none = std::collections::HashMap::new();
        crate::supervise::treerun::branch_env(&mut none, "n-aaaaaaaaaaaa", "");
        assert!(none.is_empty());
    }

    /// The operator's repository passes the checks a child's own store broke.
    fn whole(repo: &str) {
        for args in [vec!["fsck"], vec!["gc", "-q"], vec!["log", "--all", "--oneline"], vec!["rev-list", "--all"]] {
            let o = Command::new("git").args(["-C", repo]).args(&args).output().unwrap();
            assert!(o.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&o.stderr));
        }
    }

    #[test]
    fn a_store_that_was_not_folded_is_not_read() {
        let (_, data, session) = setup("unfolded");
        let wt = format!("{}/{session}", branches_dir(&data));
        let (base, common) = branch_ref(&data, &session).unwrap();
        std::fs::create_dir(format!("{common}/worktrees/{session}/objects")).unwrap();
        assert_eq!(adopt(&data, &session, None).status, "refused");
        assert!(branch_git::diff(&wt, &base, &common, &[], true, Duration::ZERO).is_none());
        assert!(branch_git::fold(&common, &wt, &base));
        assert_eq!(adopt(&data, &session, None).status, "adopted");
    }

    #[test]
    fn the_fold_follows_no_link_the_child_made() {
        let (repo, data, session) = setup("foldlinks");
        let wt = format!("{}/{session}", branches_dir(&data));
        let (base, common) = branch_ref(&data, &session).unwrap();
        let gd = format!("{common}/worktrees/{session}");
        g(&repo, &["commit", "-q", "--allow-empty", "-m", "operator moves on"]);
        let tip = g(&repo, &["rev-parse", "main"]);
        let config = std::fs::read(format!("{common}/config")).unwrap();
        let index = std::fs::read(format!("{common}/index")).unwrap();
        let keep = format!("{}/keep", Path::new(&repo).parent().unwrap().display());
        std::fs::create_dir_all(&keep).unwrap();
        std::fs::write(format!("{keep}/HEAD"), "keep\n").unwrap();
        std::fs::create_dir(format!("{gd}/objects")).unwrap();
        std::fs::write(format!("{gd}/HEAD"), "ref: refs/heads/main\n").unwrap();
        std::fs::remove_file(format!("{gd}/index")).unwrap();
        std::os::unix::fs::symlink(format!("{common}/index"), format!("{gd}/index")).unwrap();
        let _ = std::fs::remove_dir_all(format!("{gd}/logs"));
        std::os::unix::fs::symlink(&keep, format!("{gd}/logs")).unwrap();
        std::fs::write(format!("{gd}/HEAD.lock"), "x").unwrap();
        std::fs::create_dir(format!("{gd}/index.lock")).unwrap();
        assert!(branch_git::fold(&common, &wt, &base));
        assert_eq!(g(&repo, &["rev-parse", "main"]), tip, "the fold moved the operator's branch");
        assert_eq!(std::fs::read(format!("{common}/config")).unwrap(), config);
        assert_eq!(std::fs::read(format!("{common}/index")).unwrap(), index);
        assert_eq!(std::fs::read_to_string(format!("{keep}/HEAD")).unwrap(), "keep\n");
        assert_eq!(std::fs::read_to_string(format!("{gd}/HEAD")).unwrap().trim(), base);
        assert_eq!(adopt(&data, &session, None).status, "adopted");
        whole(&repo);
    }

    #[test]
    fn a_planted_reftable_directory_is_folded_away() {
        let (_, data, session) = setup("reftable");
        let wt = format!("{}/{session}", branches_dir(&data));
        let (base, common) = branch_ref(&data, &session).unwrap();
        let gd = format!("{common}/worktrees/{session}");
        std::fs::create_dir(format!("{gd}/objects")).unwrap();
        std::fs::create_dir(format!("{gd}/reftable")).unwrap();
        assert!(branch_git::fold(&common, &wt, &base));
        assert!(std::fs::symlink_metadata(format!("{gd}/objects")).is_err());
        assert!(std::fs::symlink_metadata(format!("{gd}/reftable")).is_err());
        assert_eq!(adopt(&data, &session, None).status, "adopted");
    }

    #[test]
    fn a_grant_is_read_as_the_fence_reads_it() {
        use crate::supervise::treerun::fence_covers;
        let root = std::env::temp_dir().join(format!("branch-grants-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        let root = root.display().to_string();
        let common = format!("{root}/repo/.git");
        std::fs::create_dir_all(&common).unwrap();
        let link = format!("{root}/plink");
        std::os::unix::fs::symlink(format!("{root}/repo"), &link).unwrap();
        let wt = format!("{root}/d/tree/branches/n-aaaaaaaaaaaa");
        let words = |g: &[&str]| ["--harness", "sprig"].iter().chain(g).map(|s| s.to_string()).collect::<Vec<_>>();
        for g in [
            vec!["--allow", "~/repo:rw"],
            vec!["--workdir=~"],
            vec!["--allow", &format!("{link}:rw")],
            vec!["--allow", "../../../../repo/.git:rw"],
        ] {
            assert!(fence_covers(&words(&g), "", &common, &root, &wt).is_some(), "{g:?}");
        }
        let ef = format!("{root}/env.json");
        std::fs::write(&ef, format!("{{\"file_write\": [\"{link}/.git/**\"]}}")).unwrap();
        assert!(fence_covers(&words(&[]), &ef, &common, &root, &wt).is_some());
        assert!(fence_covers(&words(&["--allow", "~/other:rw"]), "", &common, &root, &wt).is_none());
    }

    #[test]
    fn a_branch_refuses_to_start_when_another_grant_covers_the_repo() {
        use crate::supervise::treerun::{fence_covers, Starter};
        let (repo, data, _) = setup("covers");
        let wr = format!("{data}/warden");
        std::fs::create_dir_all(format!("{wr}/envelopes")).unwrap();
        let root = Path::new(&repo).parent().unwrap().display().to_string();
        let cases: Vec<Vec<String>> = vec![
            vec!["--allow".into(), format!("{repo}:rw")],
            vec!["--allow".into(), format!("{root}:rw")],
            vec!["--workdir".into(), format!("{repo}/.git/hooks")],
            vec![format!("--allow={repo}/.git:rw")],
        ];
        for g in cases {
            let mut fence: Vec<String> = vec!["fence".into(), "--harness".into(), "sprig".into()];
            fence.extend(g.clone());
            let s = Starter {
                self_path: "daisugi".into(),
                temp_dir: data.clone(),
                sprig: "sprig".into(),
                env: std::env::vars().collect(),
                fence,
            };
            let o = BranchOpts {
                parent: "top".into(),
                envelope: Object::new(),
                profiles: Default::default(),
                profile_name: None,
                model: String::new(),
                tokens: None,
                turns: None,
                parent_tokens_used: None,
                parent_turns_used: None,
                task: "t".into(),
                tools: vec![],
                tools_repr: "[]".into(),
                max_turns: None,
                cwd: repo.clone(),
                timeout_s: 5,
                timeout_ms: 1000,
                attempts: 2,
                why: "operator".into(),
                parallel: 2,
                checks: vec![],
                judge: None,
                start: None,
                fold: None,
            };
            let run = s.run_branches(&data, &wr, o, None).ok().unwrap();
            assert!(run.status == "refused" && run.usage && run.reasons.len() == 1 && run.reasons[0].contains("covers"), "{g:?}");
        }
        let words = |g: &[&str]| g.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        let common = format!("{repo}/.git");
        assert!(fence_covers(&words(&["--allow", &format!("{repo}:ro"), "--allow", &format!("{repo}/x")]), "", &common, "", &repo).is_none());
        assert!(fence_covers(&words(&["--allow", &format!("{repo}/src:rw")]), "", &common, "", &repo).is_none());
        let ef = format!("{wr}/envelopes/default.json");
        std::fs::write(&ef, format!("{{\"permissions\": {{\"file_write\": [\"{repo}/**\"]}}}}")).unwrap();
        assert!(fence_covers(&[], &ef, &common, "", &repo).is_some());
    }
}
