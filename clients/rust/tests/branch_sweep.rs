//! Branch runs that read a child's work: the fold of a fenced child's own
//! object store, and the sweep of what a child left running. They live in a
//! test binary of their own, one at a time, because the sweep makes the test
//! process the child subreaper and stops every new child of it: a git another
//! test thread started would be stopped too.

use std::collections::HashMap;
use std::process::Command;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use daisugi_verify::supervise::branch::{adopt, remove_worktree, BranchOpts, BranchRun, StartFn};
use daisugi_verify::supervise::treerun::{branch_env, NodeOpts, NodeRun, Starter};
use daisugi_verify::warden::pyjson::{Object, Value};
use num_bigint::BigInt;

static ONE: Mutex<()> = Mutex::new(());

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

fn common_of(repo: &str) -> String {
    std::fs::canonicalize(format!("{repo}/.git")).unwrap().display().to_string()
}

/// The operator's repository passes the checks a child's own store broke.
fn whole(repo: &str) {
    for args in [vec!["fsck"], vec!["gc", "-q"], vec!["log", "--all", "--oneline"], vec!["rev-list", "--all"]] {
        let o = Command::new("git").args(["-C", repo]).args(&args).output().unwrap();
        assert!(o.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&o.stderr));
    }
}

fn env_obj(id: &str) -> Object {
    let text = format!(r#"{{"id": "{id}", "generated_by": "t", "task": "t", "permissions": {{"shell": true, "shell_allowlist": ["git"]}}}}"#);
    match daisugi_verify::pathways::pmodel::validate_json(daisugi_verify::pathways::pmodel::Id::Envelope, &text) {
        Ok(Value::Obj(o)) => o,
        other => panic!("{other:?}"),
    }
}

type Extra = std::sync::Arc<dyn Fn(&str, &str, &str, &HashMap<String, String>) + Send + Sync>;

/// A start that does what a fenced child does: edit its worktree, then add
/// and commit with the branch environment, so the objects land in the
/// child's own store. `extra` runs last.
fn committing_start(extra: Option<Extra>) -> StartFn {
    std::sync::Arc::new(move |data_dir: &str, _wr: &str, o: NodeOpts| {
        let sid = o.session.clone().unwrap();
        let common = daisugi_verify::tree::read_tree(data_dir).nodes[&sid].repo.clone().unwrap();
        let mut env: HashMap<String, String> = std::env::vars().collect();
        branch_env(&mut env, &sid, &common);
        let wt = o.cwd.clone();
        let run = |args: &[&str]| {
            let mut c = Command::new("git");
            c.args(["-C", &wt, "-c", "user.name=t", "-c", "user.email=t@t"]).args(args).env_clear().envs(&env);
            let out = c.output().unwrap();
            assert!(out.status.success(), "git {args:?}: {}", String::from_utf8_lossy(&out.stderr));
        };
        std::fs::write(format!("{wt}/a.txt"), format!("{sid}\n")).unwrap();
        std::fs::write(format!("{wt}/new.txt"), "fresh\n").unwrap();
        run(&["add", "-A"]);
        run(&["commit", "-qm", "c"]);
        std::fs::write(format!("{wt}/a.txt"), format!("{sid} again\n")).unwrap();
        run(&["add", "a.txt"]);
        if let Some(f) = &extra {
            f(&wt, &sid, &common, &env);
        }
        NodeRun {
            status: "started",
            session: Some(sid.clone()),
            answer: Some(format!("answer {sid}")),
            tokens_used: Some(BigInt::from(10)),
            turns_used: Some(BigInt::from(1)),
            reasons: vec![],
            ask_id: None,
            help: None,
            obj: Object::new(),
        }
    })
}

fn branch_run(tag: &str, extra: Option<Extra>) -> (String, String, BranchRun) {
    branch_run_with(tag, extra, vec![], None, &[])
}

/// `branch_run` with a fence prefix, a fold and `git init` flags.
fn branch_run_with(
    tag: &str,
    extra: Option<Extra>,
    fence: Vec<String>,
    fold: Option<fn(&str, &str, &str) -> bool>,
    init: &[&str],
) -> (String, String, BranchRun) {
    let root = std::env::temp_dir().join(format!("branch-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root);
    let repo = format!("{}/repo", root.display());
    let data = format!("{}/d", root.display());
    let wr = format!("{data}/warden");
    std::fs::create_dir_all(&repo).unwrap();
    let mut args = vec!["init", "-q", "-b", "main"];
    args.extend_from_slice(init);
    g(&repo, &args);
    std::fs::write(format!("{repo}/a.txt"), "one\n").unwrap();
    g(&repo, &["add", "-A"]);
    g(&repo, &["commit", "-qm", "base"]);
    daisugi_verify::tree::make_root(&data, &wr, &env_obj("env_root"), "top", Some(BigInt::from(1000)), Some(BigInt::from(10))).unwrap();
    let s = Starter { self_path: "daisugi".into(), temp_dir: data.clone(), sprig: "sprig".into(), env: std::env::vars().collect(), fence };
    let o = BranchOpts {
        parent: "top".into(),
        envelope: env_obj("env_kid"),
        profiles: Default::default(),
        profile_name: None,
        model: "m".into(),
        tokens: Some(BigInt::from(100)),
        turns: Some(BigInt::from(2)),
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
        start: Some(committing_start(extra)),
        fold,
    };
    let run = s.run_branches(&data, &wr, o, None).ok().unwrap();
    (repo, data, run)
}

fn sessions_of(run: &BranchRun) -> Vec<(String, String)> {
    run.branches
        .iter()
        .map(|b| match b {
            Value::Obj(o) => (o.value("session").as_str().unwrap().to_string(), o.value("worktree").as_str().unwrap().to_string()),
            _ => panic!(),
        })
        .collect()
}

#[test]
fn adopting_from_a_child_that_committed_leaves_the_operator_repository_whole() {
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    let (repo, data, run) = branch_run("whole", None);
    assert_eq!(run.status, "ran", "{:?}", run.reasons);
    let sess = run.leader_session.clone().unwrap();
    let common = common_of(&repo);
    whole(&repo); // while the branch worktrees exist
    for (sid, _) in sessions_of(&run) {
        assert!(std::fs::symlink_metadata(format!("{common}/worktrees/{sid}/objects")).is_err());
    }
    assert_eq!(adopt(&data, &sess, None).status, "adopted");
    assert_eq!(std::fs::read_to_string(format!("{repo}/a.txt")).unwrap(), format!("{sess} again\n"));
    whole(&repo);
    g(&repo, &["add", "-A"]);
    g(&repo, &["commit", "-qm", "adopted"]);
    for (_, wt) in sessions_of(&run) {
        remove_worktree(&wt, &common);
    }
    whole(&repo);
}

#[test]
fn an_alternates_file_the_child_wrote_is_never_opened() {
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let extra: Extra = std::sync::Arc::new(|wt: &str, sid: &str, common: &str, _env: &HashMap<String, String>| {
            let info = format!("{common}/worktrees/{sid}/objects/info");
            std::fs::create_dir_all(&info).unwrap();
            let c = std::ffi::CString::new(format!("{info}/alternates")).unwrap();
            unsafe {
                libc::mkfifo(c.as_ptr(), 0o600);
            }
            std::fs::write(format!("{wt}/a.txt"), format!("{sid} late\n")).unwrap();
        });
        let (_, data, run) = branch_run("fifo", Some(extra));
        let st = run.leader_session.map(|s| adopt(&data, &s, None).status).unwrap_or("no leader");
        let _ = tx.send(st);
    });
    let st = rx.recv_timeout(Duration::from_secs(30)).expect("a parent git call opened the child's alternates");
    assert_eq!(st, "adopted");
}

#[test]
fn a_process_the_child_left_is_stopped_before_the_parent_reads() {
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    let root = std::env::temp_dir().join(format!("branch-pids-{}", std::process::id()));
    let _ = std::fs::create_dir_all(&root);
    let pids = format!("{}/pids", root.display());
    let _ = std::fs::remove_file(&pids);
    let p2 = pids.clone();
    let extra: Extra = std::sync::Arc::new(move |wt: &str, sid: &str, common: &str, _env: &HashMap<String, String>| {
        let gd = format!("{common}/worktrees/{sid}");
        let lp = format!(
            "echo $$ >> {p2}; sleep 0.3; i=0; while [ $i -lt 300 ]; do : > {gd}/config.worktree; mkdir -p {gd}/objects; git -C {wt} add -A >/dev/null 2>&1; sleep 0.01; i=$((i+1)); done"
        );
        let st = Command::new("sh").args(["-c", &format!("setsid sh -c '{lp}' </dev/null >/dev/null 2>&1 &")]).status().unwrap();
        assert!(st.success());
        let end = Instant::now() + Duration::from_secs(5);
        while Instant::now() < end && !std::fs::read_to_string(&p2).unwrap_or_default().contains('\n') {
            std::thread::sleep(Duration::from_millis(10));
        }
    });
    let (repo, data, run) = branch_run("sweep", Some(extra));
    let left: Vec<i32> = std::fs::read_to_string(&pids).unwrap_or_default().split_whitespace().filter_map(|x| x.parse().ok()).collect();
    let alive: Vec<i32> = left.iter().copied().filter(|&p| unsafe { libc::kill(p, 0) } == 0).collect();
    for &p in &left {
        unsafe {
            libc::kill(p, libc::SIGKILL);
        }
    }
    assert_eq!(left.len(), 2);
    assert!(alive.is_empty(), "the processes the child left are still alive: {alive:?}");
    assert_eq!(run.status, "ran", "{:?}", run.reasons);
    std::thread::sleep(Duration::from_millis(600));
    let sess = run.leader_session.clone().unwrap();
    let common = common_of(&repo);
    assert!(std::fs::symlink_metadata(format!("{common}/worktrees/{sess}/config.worktree")).is_err());
    assert_eq!(adopt(&data, &sess, None).status, "adopted");
    whole(&repo);
}


/// Give the test back the rights a child took, so the temp dir can go.
fn writable(root: &str) {
    use std::os::unix::fs::PermissionsExt;
    let mut todo = vec![std::path::PathBuf::from(root)];
    while let Some(p) = todo.pop() {
        let Ok(m) = std::fs::symlink_metadata(&p) else { continue };
        if m.file_type().is_dir() {
            let _ = std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o755));
            if let Ok(rd) = std::fs::read_dir(&p) {
                todo.extend(rd.flatten().map(|e| e.path()));
            }
        }
    }
}

fn admin_of(wt: &str) -> String {
    let o = Command::new("git").args(["-C", wt, "rev-parse", "--path-format=absolute", "--git-dir"]).output().unwrap();
    String::from_utf8_lossy(&o.stdout).trim().to_string()
}

/// What a hostile child leaves in its admin directory after it committed.
fn left(kind: &'static str) -> Extra {
    use std::os::unix::fs::PermissionsExt;
    std::sync::Arc::new(move |wt: &str, _sid: &str, _common: &str, env: &HashMap<String, String>| {
        let admin = admin_of(wt);
        let mode = |p: &str, m: u32| std::fs::set_permissions(p, std::fs::Permissions::from_mode(m)).unwrap();
        match kind {
            "store" => std::fs::remove_dir_all(format!("{admin}/objects")).unwrap(),
            "admin" => mode(&admin, 0o555),
            "logs-refs" => {
                let o = Command::new("git")
                    .args(["-C", wt, "update-ref", "refs/worktree/keep", "HEAD"])
                    .env_clear()
                    .envs(env)
                    .output()
                    .unwrap();
                assert!(o.status.success(), "{}", String::from_utf8_lossy(&o.stderr));
                for part in ["logs", "refs/worktree", "refs"] {
                    mode(&format!("{admin}/{part}"), 0o555);
                }
            }
            "reftable" => {
                std::fs::create_dir(format!("{admin}/reftable")).unwrap();
                mode(&format!("{admin}/reftable"), 0o000);
            }
            _ => unreachable!(),
        }
    })
}

fn fence() -> Vec<String> {
    vec!["fence".into()]
}

#[test]
fn a_fenced_branch_is_folded_whatever_the_child_left() {
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    for kind in ["store", "admin", "logs-refs", "reftable"] {
        let (repo, data, run) = branch_run_with(&format!("left-{kind}"), Some(left(kind)), fence(), None, &[]);
        let root = std::path::Path::new(&repo).parent().unwrap().display().to_string();
        let common = common_of(&repo);
        // right after the run, while the branch worktrees exist
        let ok = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            whole(&repo);
            assert_eq!(run.status, "ran", "{:?}", run.reasons);
            for (sid, _) in sessions_of(&run) {
                for part in ["objects", "logs", "refs", "reftable"] {
                    assert!(std::fs::symlink_metadata(format!("{common}/worktrees/{sid}/{part}")).is_err(), "{kind}: {sid} {part}");
                }
            }
            assert_eq!(adopt(&data, run.leader_session.as_deref().unwrap(), None).status, "adopted");
            whole(&repo);
        }));
        writable(&root);
        let _ = std::fs::remove_dir_all(&root);
        assert!(ok.is_ok(), "{kind}");
    }
}

static FOLDS: Mutex<Vec<String>> = Mutex::new(Vec::new());

fn first_fold_fails(repo: &str, wt: &str, base: &str) -> bool {
    let mut f = FOLDS.lock().unwrap_or_else(|e| e.into_inner());
    f.push(std::path::Path::new(wt).file_name().unwrap().to_string_lossy().to_string());
    if f.len() == 1 {
        return false;
    }
    drop(f);
    daisugi_verify::branch_git::fold(repo, wt, base)
}

#[test]
fn a_branch_whose_fold_fails_is_removed_and_not_ranked() {
    use std::os::unix::fs::PermissionsExt;
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    FOLDS.lock().unwrap_or_else(|e| e.into_inner()).clear();
    let lock: Extra = std::sync::Arc::new(|wt: &str, _sid: &str, _common: &str, _env: &HashMap<String, String>| {
        let admin = admin_of(wt);
        std::fs::write(format!("{admin}/locked"), "mine\n").unwrap();
        std::fs::set_permissions(&admin, std::fs::Permissions::from_mode(0o555)).unwrap();
    });
    let (repo, _data, run) = branch_run_with("foldfails", Some(lock), fence(), Some(first_fold_fails), &[]);
    let root = std::path::Path::new(&repo).parent().unwrap().display().to_string();
    let bad = FOLDS.lock().unwrap_or_else(|e| e.into_inner())[0].clone();
    let common = common_of(&repo);
    let ok = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        whole(&repo);
        let b = run
            .branches
            .iter()
            .find_map(|b| match b {
                Value::Obj(o) if o.value("session").as_str() == Some(bad.as_str()) => Some(o.clone()),
                _ => None,
            })
            .unwrap();
        assert_eq!(b.value("status").as_str(), Some("failed"));
        assert!(std::fs::symlink_metadata(format!("{common}/worktrees/{bad}")).is_err(), "its admin directory is still there");
        assert!(std::fs::symlink_metadata(b.value("worktree").as_str().unwrap()).is_err(), "its worktree is still there");
        assert!(!g(&repo, &["worktree", "list", "--porcelain"]).contains(&bad));
        if let Some(r) = &run.ranking {
            if let Value::List(order) = r.value("order") {
                assert!(order.iter().all(|m| m.as_str() != Some(bad.as_str())), "it was ranked");
            }
        }
    }));
    writable(&root);
    let _ = std::fs::remove_dir_all(&root);
    assert!(ok.is_ok());
}

#[test]
fn a_reftable_repository_refuses_a_fenced_branch_run_up_front() {
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    let (_, data, run) = branch_run_with("reftable", None, fence(), None, &["--ref-format=reftable"]);
    assert!(run.status == "refused" && run.usage && run.reasons.len() == 1, "{:?}", run.reasons);
    assert!(run.reasons[0].contains("reftable") && !run.reasons[0].contains('\n'));
    assert!(std::fs::symlink_metadata(format!("{data}/tree/branches")).is_err());
    assert_eq!(daisugi_verify::tree::read_tree(&data).proposals.get("top").copied().unwrap_or(0), 0);
}

#[test]
fn a_branch_run_refuses_allow_weak_ipc() {
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    for w in ["--allow-weak-ipc", "--allow-weak-ipc=1"] {
        let f = vec!["fence".to_string(), "--harness".into(), "sprig".into(), w.into()];
        let (_, data, run) = branch_run_with(&format!("weak{}", w.len()), None, f, None, &[]);
        assert!(run.status == "refused" && run.usage && run.reasons.len() == 1, "{w}: {:?}", run.reasons);
        assert!(run.reasons[0].contains("--allow-weak-ipc"));
        assert!(std::fs::symlink_metadata(format!("{data}/tree/branches")).is_err());
        assert_eq!(daisugi_verify::tree::read_tree(&data).proposals.get("top").copied().unwrap_or(0), 0);
    }
}

#[test]
fn a_branch_run_refuses_no_fence() {
    // R5-2: a branch fence that holds --no-fence confines nothing.
    let _one = ONE.lock().unwrap_or_else(|e| e.into_inner());
    for w in ["--no-fence", "--no-fence=1"] {
        let f = vec!["fence".to_string(), "--harness".into(), "sprig".into(), w.into()];
        let (_, data, run) = branch_run_with(&format!("nofence{}", w.len()), None, f, None, &[]);
        assert!(run.status == "refused" && run.usage && run.reasons.len() == 1, "{w}: {:?}", run.reasons);
        assert_eq!(run.reasons[0], daisugi_verify::supervise::branch::NO_FENCE_TEXT);
        assert!(std::fs::symlink_metadata(format!("{data}/tree/branches")).is_err());
        assert_eq!(daisugi_verify::tree::read_tree(&data).proposals.get("top").copied().unwrap_or(0), 0);
    }
}
