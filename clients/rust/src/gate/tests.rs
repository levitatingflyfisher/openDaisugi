//! Whole-call tests of the native gate. The differential check against the
//! oracle is `clients/gate_compare.py`; these pin the shapes a review found.

use super::*;
use std::path::PathBuf;

const TEST_ENVELOPE: &str = r#"{"id": "env_t", "generated_by": "t", "task": "t", "permissions": {
 "file_read": ["/work/**"], "file_write": ["/work/**"], "shell": true,
 "shell_allowlist": ["ls", "git"], "network": false}}"#;

struct Setup {
    dir: PathBuf,
    root: String,
    env: HashMap<String, String>,
}

impl Drop for Setup {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn setup() -> Setup {
    static N: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
    let n = N.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    let dir = std::env::temp_dir().join(format!("dgt-{}-{n}", std::process::id()));
    let root = dir.join("data/gate");
    std::fs::create_dir_all(root.join("envelopes")).unwrap();
    std::fs::write(root.join("envelopes/default.json"), TEST_ENVELOPE).unwrap();
    let mut env = HashMap::new();
    for (k, v) in [("HOME", "/home/user"), ("PATH", "/usr/bin:/bin")] {
        env.insert(k.to_string(), v.to_string());
    }
    Setup { dir, root: root.to_str().unwrap().to_string(), env }
}

impl Setup {
    fn envelope(&self, body: &str) {
        std::fs::write(PathBuf::from(&self.root).join("envelopes/default.json"), body).unwrap();
    }
    fn run(&self, mode: &str, payload: &str) -> GateResult {
        let argv: Vec<String> =
            ["--mode", mode, "--root", &self.root, "--format", "claude"].iter().map(|s| s.to_string()).collect();
        run(&argv, payload.as_bytes(), self.env.clone())
    }
    fn bash(&self, mode: &str, cmd: &str) -> GateResult {
        let p = Value::Obj(
            Object::new()
                .with("session_id", "s")
                .with("tool_name", "Bash")
                .with("cwd", "/work")
                .with("tool_input", Object::new().with("command", cmd)),
        );
        self.run(mode, &dumps(&p, true))
    }
}

fn s(b: &[u8]) -> String {
    String::from_utf8_lossy(b).into_owned()
}

#[test]
fn read_inside_the_envelope_is_allowed() {
    let t = setup();
    let res = t.run("enforce", r#"{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a.txt"},"cwd":"/work"}"#);
    assert!(res.native, "{}", res.why);
    assert_eq!((res.exit, s(&res.stdout)), (0, "{\"continue\": true}\n".to_string()));
    let log = std::fs::read_to_string(PathBuf::from(&t.root).join("shadow/s1.jsonl")).unwrap();
    assert!(log.contains(r#""reason": "verified in envelope""#), "{log}");
}

#[test]
fn an_unknown_tool_is_denied_in_enforce() {
    let t = setup();
    let res = t.run("enforce", r#"{"session_id":"s1","tool_name":"TodoWrite","tool_input":{}}"#);
    assert!(res.native && res.exit == 2, "{res:?}");
    assert_eq!(
        s(&res.stderr),
        "openDaisugi gate: DENIED — unrecognized tool 'TodoWrite' — not in the gate's classification map, denied by default\n"
    );
}

#[test]
fn shadow_allows_what_enforce_denies() {
    let t = setup();
    let res = t.bash("shadow", "rm -rf /work");
    assert!(res.native && res.exit == 0 && s(&res.stdout) == "{\"continue\": true}\n", "{res:?}");
}

/// A reviewer's fail-open: an extglob hid $(rm x) from one decomposition.
/// The gate parses with the oracle's own parser and refuses each of these.
#[test]
fn a_compound_command_the_parser_cannot_prove_is_denied() {
    let t = setup();
    t.envelope(
        r#"{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["echo","ls"],
         "shell_allow_decomposition":true,"file_read":["/work/**"],"file_write":["/work/**"]}}"#,
    );
    for cmd in [
        "echo @(a|$(rm x)) ; ls", "echo !(a|$(rm x)) ; ls", "echo +($(rm x)) ; ls", "echo *(`rm x`) ; ls",
        "echo ?(<(rm x)) ; ls", "ls @($(rm x)) | ls", "cat < /work/a 0< /etc/shadow",
    ] {
        let res = t.bash("enforce", cmd);
        assert!(res.native && res.exit == 2, "{cmd:?}: native={} exit={}", res.native, res.exit);
    }
    let res = t.bash("enforce", "ls && echo hi");
    assert!(res.native && res.exit == 0, "a plain compound command: {res:?}");
}

/// A glob with many ** makes the oracle's matcher exponential: past
/// GLOB_MATCH_STEP_LIMIT steps both deny with GlobTooComplex.
#[test]
fn a_glob_with_many_double_stars_is_too_complex() {
    let t = setup();
    t.envelope(
        r#"{"generated_by":"t","task":"t","permissions":{
         "file_read":["/**/a/**/a/**/a/**/a/**/a/**/a/**/a/**/a/**/a/b"]}}"#,
    );
    let path = format!("/{}c", "a/".repeat(40));
    let p = format!(r#"{{"session_id":"s","tool_name":"Read","cwd":"/work","tool_input":{{"file_path":"{path}"}}}}"#);
    let start = Instant::now();
    let res = t.run("enforce", &p);
    assert!(res.native && res.exit == 2 && start.elapsed() < Duration::from_secs(5), "{res:?}");
    assert!(s(&res.stderr).contains("is too complex to match: more than 100000 steps"), "{}", s(&res.stderr));
}

/// The native verifier runs under --verify-timeout.
#[test]
fn a_slow_native_check_runs_out_of_budget() {
    let r = Runner {
        env: HashMap::new(),
        home: "/home/user".into(),
        wd: OnceLock::new(),
        real_cache: Default::default(),
        root: "/r".into(),
        default_root: "/r".into(),
        mode: "enforce".into(),
        fmt: "claude".into(),
        session: None,
        captures: None,
        verify_timeout_s: 0.05,
        ask: false,
        ask_timeout_s: 90.0,
        checkpoints: false,
        t0: Instant::now(),
    };
    let late = r
        .run_bounded(4, |_| {
            std::thread::sleep(Duration::from_secs(2));
            Ok(())
        })
        .unwrap();
    assert!(late.is_none());
    assert_eq!(
        check::verify_late_reason(5.0),
        "gate internal error (denied fail-closed): verifier exceeded the gate's inner time budget (5.0s)."
    );
}

/// After a cd the gate cannot follow, a relative word that names a floor
/// directory by its last part is a floor hit, as in the oracle.
#[test]
fn a_floor_word_after_an_unfollowed_cd_is_denied() {
    let mut t = setup();
    t.envelope(
        r#"{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["cd","cp","ls"],
         "shell_allow_decomposition":true,"file_read":["/**"],"file_write":["/**"]}}"#,
    );
    t.env.insert("DAISUGI_GATE_ASSUME_SHELL_PARSER".into(), "1".into());
    for (cmd, deny) in [
        ("cd .. && cp a .config/coppice/x", true),
        ("cd .. && cp a opencode/x", false),
        ("cd .. && cp a b/c", false),
        ("cp a .config/coppice/x", false),
    ] {
        let res = t.bash("enforce", cmd);
        assert!(res.native, "{cmd:?}: undecided ({})", res.why);
        assert_eq!(s(&res.stderr).contains(rules::FLOOR_REFUSAL), deny, "{cmd:?}: {}", s(&res.stderr));
    }
}

/// Past 32 distinct directories the floor rule stops following cd, so a
/// relative word naming a floor directory counts as a hit, as in the oracle.
#[test]
fn past_thirty_two_cwds_the_floor_rule_stops_following_cd() {
    let mut t = setup();
    t.envelope(
        r#"{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["cd","cp","ls"],
         "shell_allow_decomposition":true,"file_read":["/**"],"file_write":["/**"]}}"#,
    );
    t.env.insert("DAISUGI_GATE_ASSUME_SHELL_PARSER".into(), "1".into());
    let cds = |n: usize| (0..n).map(|i| format!("cd /d{i}")).collect::<Vec<_>>().join(" ; ");
    // 31 cds plus the call's own cwd is 32 places: still followed.
    let res = t.bash("enforce", &format!("{} ; cp a .config/coppice/x", cds(31)));
    assert!(res.native && res.exit == 0, "{}", s(&res.stderr));
    // One more distinct directory, and the cwd is no longer followed.
    let res = t.bash("enforce", &format!("{} ; cp a .config/coppice/x", cds(32)));
    assert!(res.native && s(&res.stderr).contains(rules::FLOOR_REFUSAL), "{}", s(&res.stderr));
}

/// A line of 400 redirects made the floor rule place every word against
/// every cwd once per command. Each cwd is kept once, so the line answers
/// at once, allowed or denied with its tier.
#[test]
fn a_four_hundred_segment_line_answers_quickly() {
    let mut t = setup();
    t.envelope(
        r#"{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["echo","ls"],
         "shell_allow_decomposition":true,"file_read":["/work/**"],"file_write":["/work/**"]}}"#,
    );
    t.env.insert("DAISUGI_GATE_ASSUME_SHELL_PARSER".into(), "1".into());
    for (dir, exit) in [("/work", 0), ("/etc", 2)] {
        let line: Vec<String> = (0..400).map(|i| format!("echo {i} > {dir}/o{i}")).collect();
        let start = Instant::now();
        let res = t.bash("enforce", &line.join(" ; "));
        assert!(start.elapsed() < Duration::from_millis(900), "took {:?}", start.elapsed());
        assert!(res.native && res.exit == exit, "{res:?}");
    }
}

/// A thousand distinct cds: past 32 the line is no longer followed, and the
/// rules and the tier still answer at once.
#[test]
fn a_thousand_distinct_cds_answer_quickly() {
    let mut t = setup();
    t.envelope(
        r#"{"generated_by":"t","task":"t","permissions":{"shell":true,"shell_allowlist":["cd","echo"],
         "shell_allow_decomposition":true,"file_read":["/work/**"],"file_write":["/etc/**"]}}"#,
    );
    t.env.insert("DAISUGI_GATE_ASSUME_SHELL_PARSER".into(), "1".into());
    let line: Vec<String> = (0..1000).map(|i| format!("cd /work/d{i} ; echo {i} > o{i}")).collect();
    let start = Instant::now();
    let res = t.bash("enforce", &line.join(" ; "));
    assert!(start.elapsed() < Duration::from_secs(3), "took {:?}", start.elapsed());
    assert!(res.native && res.exit == 2, "{res:?}");
}

/// The web door covers every secret file a coppice data directory holds.
#[test]
fn a_read_of_a_coppice_secret_is_a_pane_rule_deny() {
    let t = setup();
    t.envelope(r#"{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":["/**"]}}"#);
    for (secret, deny) in [
        ("web/ca/ca.key", true),
        ("voice/token", true),
        ("web/ca/ca.crt", false),
        ("web/meta.json", false),
        // Spellings the plain text match misses; the placed path catches them.
        ("web//ca/./ca.key", true),
        ("web/x/../ca/leaf.key", true),
        ("voice/./token", true),
    ] {
        let p = format!(
            r#"{{"session_id":"s","tool_name":"Read","cwd":"/work","tool_input":{{"file_path":"/home/user/.opendaisugi/coppice/{secret}"}}}}"#
        );
        let res = t.run("enforce", &p);
        assert!(res.native, "{}", res.why);
        assert_eq!(s(&res.stderr).contains(rules::PANE_REFUSAL), deny, "{secret}: {}", s(&res.stderr));
    }
}

/// The state report carries the transcript path (absolute only), the
/// verdict and the floor's mode word.
#[test]
fn the_state_entry_carries_transcript_verdict_and_mode() {
    let t = setup();
    for (mode, sid, payload, want) in [
        (
            "enforce",
            "s1",
            r#"{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work","transcript_path":"/t/x.jsonl"}"#,
            r#""detail": "verdict=allow", "transcript_path": "/t/x.jsonl", "verdict": {"decision": "allow", "tool": "Read", "clause": ""}, "mode": "enforcing"}"#,
        ),
        (
            "shadow",
            "s2",
            r#"{"session_id":"s2","tool_name":"Bash","tool_input":{"command":"rm x"},"cwd":"/work","transcript_path":"rel/x.jsonl"}"#,
            r#""verdict": {"decision": "allow", "tool": "Bash", "clause": "permissions: Step 's0' shell command 'rm' not in allowlist ['ls', 'git']"}, "mode": "watching"}"#,
        ),
    ] {
        let res = t.run(mode, payload);
        assert!(res.native, "{}", res.why);
        let tree = std::fs::read_to_string(PathBuf::from(&t.root).parent().unwrap().join(format!("sessions/{sid}.jsonl"))).unwrap();
        let last = tree.trim_end().lines().last().unwrap();
        assert!(last.ends_with(want), "{last}\nwant suffix {want}");
    }
}

/// A call the port cannot decide is denied in its host format's contract.
#[test]
fn an_undecided_call_denies_in_each_format() {
    for (f, exit, out) in [("claude", 2, false), ("pi", 2, false), ("hermes", 0, true), ("openclaw", 0, true)] {
        let argv: Vec<OsString> = ["--mode", "shadow", "--format", f].iter().map(OsString::from).collect();
        let res = super::undecided_deny(&argv, "a test");
        assert!(!res.native && res.exit == exit, "{f}: {res:?}");
        let text = if out { s(&res.stdout) } else { s(&res.stderr) };
        assert!(text.contains("the Rust gate cannot decide this call: a test"), "{f}: {text}");
    }
}

/// Python refuses an integer past 4300 digits, and the payload is then not
/// parseable, so the call is denied.
#[test]
fn a_payload_with_a_huge_integer_is_denied() {
    let t = setup();
    let p = format!(
        r#"{{"session_id":"s1","tool_name":"Read","tool_input":{{"file_path":"/work/a"}},"cwd":"/work","x":{}}}"#,
        "1".repeat(4301)
    );
    let res = t.run("enforce", &p);
    assert!(res.exit == 2, "{res:?}");
}

/// HOME=/ joins as pathlib joins: the floor is /.config/coppice, not
/// //.config/coppice.
#[test]
fn home_at_the_root_places_the_floor_as_pathlib_does() {
    let mut t = setup();
    t.env.insert("HOME".into(), "/".into());
    t.envelope(
        r#"{"generated_by":"t","task":"t","permissions":{"file_read":["/**"],"file_write":["/**"]}}"#,
    );
    let res = t.run(
        "enforce",
        r#"{"session_id":"s1","tool_name":"Write","tool_input":{"file_path":"/.config/coppice/coppice.toml","content":"x"},"cwd":"/work"}"#,
    );
    assert!(res.native && s(&res.stderr).contains(rules::FLOOR_REFUSAL), "{res:?}");
}

/// PW-15: a Z3 `unknown` in the self-consistency check never allows. At
/// low stakes it is a warning, and the gate denies once every other stage
/// passes; at high or physical stakes it is the stage's own violation.
#[test]
fn a_z3_unknown_in_self_consistency_is_denied() {
    let t = setup();
    let read = r#"{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a.txt"},"cwd":"/work"}"#;
    let body = |stakes: &str, extra: &str| {
        format!(
            r#"{{"generated_by":"t","task":"{}","stakes":"{stakes}","permissions":{{"file_read":["/work/**"]}}{extra}}}"#,
            check::FORCE_Z3_UNKNOWN_TASK
        )
    };
    t.envelope(&body("low", ""));
    let res = t.run("enforce", read);
    assert!(res.native && res.exit == 2, "{res:?}");
    assert_eq!(
        s(&res.stderr),
        "openDaisugi gate: DENIED — the verifier could not finish a check in time; denied \
         (Z3 self-consistency check exceeded 500ms)\n"
    );
    let res = t.run("shadow", read);
    assert!(res.native && res.exit == 0, "{res:?}");
    let log = std::fs::read_to_string(PathBuf::from(&t.root).join("shadow/s1.jsonl")).unwrap();
    assert!(log.contains(r#""would_deny": true"#) && log.contains("could not finish a check in time"), "{log}");
    for stakes in ["high", "physical"] {
        t.envelope(&body(stakes, ""));
        let res = t.run("enforce", read);
        assert!(res.native && res.exit == 2, "{res:?}");
        assert_eq!(
            s(&res.stderr),
            "openDaisugi gate: DENIED — z3: verifier timed out (envelope self-consistency check); \
             raise the Z3 timeout\n",
            "{stakes}"
        );
    }
    // A later stage's violation still decides the call.
    t.envelope(&body(
        "low",
        r#","invariants":[{"type":"pred","description":"only b","expr":{"op":"forall_steps","pred":{"op":"equals","path":"path","value":"/work/b.txt"}}}]"#,
    ));
    let res = t.run("enforce", read);
    assert!(res.native && res.exit == 2, "{res:?}");
    assert_eq!(s(&res.stderr), "openDaisugi gate: DENIED — predicate: invariant 'pred' violated\n");
}
