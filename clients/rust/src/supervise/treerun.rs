//! `opendaisugi/tree_run.py` and the sprig starter of
//! `agentic_executor`: the one starter of the delegation tree. The Go
//! client's `supervise.Starter` is the reference.

use std::collections::HashMap;

use num_bigint::BigInt;

use crate::agentprof::{Profile, Profiles, AMBIENT};
use crate::gate::py::text::{decode_utf8_replace, head, repr};
use crate::gate::pyjson::{loads_py, Object, Value};
use crate::tree::{self, Fail, SpawnOpts};

/// The descriptors a child reads its key and the profile bundle from.
pub const KEY_FD: i32 = 3;
pub const SPAWN_KEYS_FD: i32 = 4;
/// `agentic_executor.ROOT_SESSION`.
pub const ROOT_SESSION: &str = "root";

/// `verify.agentic_tool_granted` on a dumped envelope's permissions.
pub fn tool_granted(tool: &str, perms: &Object) -> bool {
    let Some(cap) = super::agentic::capability(tool) else {
        return false;
    };
    if tool == "Spawn" {
        let allow: Vec<String> = match perms.value("mcp_allowlist") {
            Value::List(l) => l
                .iter()
                .filter_map(|x| x.as_str().map(String::from))
                .collect(),
            _ => vec![],
        };
        return crate::glob_engine::head_allowed(crate::verify::SPAWN_MCP_KEY, &allow);
    }
    perms.value(cap).truthy()
}

/// `agentic_executor.sprig_wall`: sprig's --tools for a step's host tools.
pub fn sprig_wall(tools: &[String], envelope: &Object) -> Vec<String> {
    let perms = envelope
        .value("permissions")
        .as_obj()
        .cloned()
        .unwrap_or_default();
    let mut wall: Vec<String> = vec![];
    for t in tools {
        let Some(name) = super::agentic_sprig::sprig_tool(t) else {
            continue;
        };
        if !tool_granted(t, &perms) {
            continue;
        }
        if !wall.iter().any(|w| w == name) {
            wall.push(name.to_string());
        }
    }
    wall
}

/// `agentic_executor.child_env`: the key variables the table names
/// dropped, and for a profiled sprig child (or one that may spawn) every
/// ambient key and redirect too.
pub fn child_env(
    env: &HashMap<String, String>,
    ps: &Profiles,
    sprig: bool,
) -> HashMap<String, String> {
    let mut drop: Vec<String> = ps.key_names();
    if sprig {
        drop.extend(AMBIENT.iter().map(|s| s.to_string()));
    }
    env.iter()
        .filter(|(k, _)| !drop.contains(k) && !crate::agentprof::is_key_name(k))
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect()
}

/// `agentic_executor.sprig_start`: the argv (without the binary),
/// descriptors and profiled flag of a sprig child.
#[allow(clippy::too_many_arguments)]
pub fn sprig_start(
    self_path: &str,
    model: &str,
    gate_root: &str,
    data_dir: &str,
    session: &str,
    wall: &[String],
    profile: &Profile,
    profiled: bool,
    ps: &Profiles,
    max_turns: Option<&BigInt>,
    max_tokens: Option<&BigInt>,
) -> (Vec<String>, Vec<(i32, Vec<u8>)>) {
    let gate = format!(
        "{self_path} gate check --mode enforce --root {gate_root} --session {session} --captures-root {gate_root}/captures"
    );
    let mut argv: Vec<String> = vec![
        "--json".into(),
        "--gate".into(),
        "--gate-cmd".into(),
        gate,
        "--tools".into(),
        wall.join(","),
        "--model".into(),
        model.into(),
    ];
    let mut fds = vec![];
    let spawn = wall.iter().any(|w| w == "spawn");
    if profiled {
        argv.push("--wire".into());
        argv.push(profile.wire.clone());
        if profile.wire != "claude-code" {
            argv.push("--base-url".into());
            argv.push(
                profile
                    .base_url
                    .clone()
                    .filter(|b| !b.is_empty())
                    .unwrap_or_else(|| crate::agentprof::default_base(&profile.wire).into()),
            );
        }
    }
    if let Some(k) = profile.key.as_ref().filter(|k| !k.is_empty()) {
        argv.push("--key-fd".into());
        argv.push(KEY_FD.to_string());
        fds.push((KEY_FD, k.as_bytes().to_vec()));
    }
    let names = ps.key_names();
    if (profiled || spawn) && !names.is_empty() {
        argv.push("--scrub-env".into());
        argv.push(names.join(","));
    }
    if spawn {
        argv.push("--spawn-keys-fd".into());
        argv.push(SPAWN_KEYS_FD.to_string());
        argv.push("--tree-cmd".into());
        argv.push(format!(
            "{self_path} tree run --data-dir {data_dir} --root {gate_root}"
        ));
        fds.push((SPAWN_KEYS_FD, ps.bundle()));
    }
    argv.push("--session-dir".into());
    argv.push(format!("{gate_root}/sessions"));
    argv.push("--session".into());
    argv.push(session.into());
    if let Some(t) = max_turns {
        argv.push("--max-turns".into());
        argv.push(t.to_string());
    }
    if let Some(t) = max_tokens {
        argv.push("--max-tokens".into());
        argv.push(t.to_string());
    }
    argv.push("-".into());
    (argv, fds)
}

/// How a sprig child ended: the failure text, or its answer and object.
pub struct SprigOutcome {
    pub error: Option<String>,
    pub answer: String,
    pub obj: Object,
}

/// `agentic_executor.run_sprig_child`.
#[allow(clippy::too_many_arguments)]
pub fn run_sprig_child(
    sprig: &str,
    look_env: &HashMap<String, String>,
    argv: &[String],
    task: &str,
    cwd: &str,
    timeout_s: u64,
    env: &HashMap<String, String>,
    fds: &[(i32, Vec<u8>)],
    on_start: Option<&dyn Fn(i32)>,
    branch: Option<&dyn Fn() -> Vec<i32>>,
) -> SprigOutcome {
    let fail = |t: String| SprigOutcome {
        error: Some(t),
        answer: String::new(),
        obj: Object::new(),
    };
    let stdin = crate::gate::py::text::fsencode(task).unwrap_or_else(|_| task.as_bytes().to_vec());
    let start_text = format!("agentic sub-agent failed: cannot start sprig ({sprig})");
    let bin = if sprig.contains('/') {
        Some(sprig.to_string())
    } else {
        crate::cli::install::look_path(look_env, sprig)
    };
    let Some(bin) = bin else {
        return fail(start_text);
    };
    let ran = match super::agentic_sprig::run_group(
        &bin, argv, cwd, env, stdin, timeout_s, fds, on_start, branch,
    ) {
        Ok(x) => x,
        Err(_) => return fail(start_text),
    };
    if ran.too_big {
        return fail("agentic sub-agent failed: sprig wrote more than 16 MiB".into());
    }
    if ran.timed_out {
        return fail(format!(
            "agentic sub-agent failed: sprig ran past {timeout_s}s and was killed"
        ));
    }
    if ran.rc != 0 {
        return fail(super::agentic_sprig::exit_text(ran.rc, &ran.stderr));
    }
    let raw = decode_utf8_replace(&ran.stdout);
    let parsed = match loads_py(&raw, 900) {
        Ok(v) => v,
        Err(_) => {
            return fail(format!(
                "agentic sub-agent returned unparseable output: {}",
                repr(head(&raw, 300))
            ))
        }
    };
    let Value::Obj(obj) = parsed else {
        return fail(format!(
            "agentic sub-agent reply has no answer: {}",
            repr(head(&raw, 300))
        ));
    };
    let Value::Str(answer) = obj.value("answer").clone() else {
        return fail(format!(
            "agentic sub-agent reply has no answer: {}",
            repr(head(&raw, 300))
        ));
    };
    SprigOutcome {
        error: None,
        answer,
        obj,
    }
}

fn max_count() -> BigInt {
    BigInt::from(1u64 << 53)
}

/// Clamps a count to 0 ..= 2**53.
pub fn clamp_count(v: BigInt) -> BigInt {
    if v < BigInt::default() {
        BigInt::default()
    } else if v > max_count() {
        max_count()
    } else {
        v
    }
}

/// `tree_run._own_use`: the four usage counts added (ints only, clamped),
/// and the model turns.
pub fn own_use(obj: &Object) -> (Option<BigInt>, Option<BigInt>) {
    let mut tokens = None;
    if let Value::Obj(usage) = obj.value("usage") {
        // A negative count makes the report no report (MX-R-7).
        let mut sum = BigInt::default();
        let mut any = false;
        let mut negative = false;
        for f in [
            "input_tokens",
            "output_tokens",
            "cache_creation_input_tokens",
            "cache_read_input_tokens",
        ] {
            if let Value::Int(t) = usage.value(f) {
                if let Ok(n) = t.parse::<BigInt>() {
                    negative |= n < BigInt::default();
                    sum += n;
                    any = true;
                }
            }
        }
        if any && !negative {
            tokens = Some(clamp_count(sum));
        }
    }
    let turns = match obj.value("model_turns") {
        Value::Int(t) => t.parse::<BigInt>().ok().map(clamp_count),
        _ => None,
    };
    (tokens, turns)
}

/// `tree_run.NodeRun`.
pub struct NodeRun {
    pub status: &'static str,
    pub session: Option<String>,
    pub answer: Option<String>,
    pub tokens_used: Option<BigInt>,
    pub turns_used: Option<BigInt>,
    pub reasons: Vec<String>,
    pub ask_id: Option<String>,
    pub obj: Object,
}

impl NodeRun {
    fn failed(session: Option<String>, why: String) -> NodeRun {
        NodeRun {
            status: "failed",
            session,
            answer: None,
            tokens_used: None,
            turns_used: None,
            reasons: vec![why],
            ask_id: None,
            obj: Object::new(),
        }
    }

    /// `NodeRun.doc`.
    pub fn doc(&self) -> Object {
        let s = |v: &Option<String>| v.clone().map(Value::Str).unwrap_or(Value::Null);
        let b = |v: &Option<BigInt>| {
            v.as_ref()
                .map(|n| Value::Int(n.to_string()))
                .unwrap_or(Value::Null)
        };
        Object::new()
            .with(
                "status",
                if self.status == "started" {
                    "ran"
                } else {
                    self.status
                },
            )
            .with("session", s(&self.session))
            .with("answer", s(&self.answer))
            .with("tokens_used", b(&self.tokens_used))
            .with("turns_used", b(&self.turns_used))
            .with(
                "reasons",
                Value::List(self.reasons.iter().map(|r| Value::Str(r.clone())).collect()),
            )
            .with("ask_id", s(&self.ask_id))
    }
}

/// What `run_node` needs to start a sprig child.
pub struct Starter {
    pub self_path: String,
    pub temp_dir: String,
    pub sprig: String,
    pub env: HashMap<String, String>,
}

/// `run_node`'s keywords.
pub struct NodeOpts {
    pub parent: String,
    pub session: Option<String>,
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
    pub default_model: String,
    pub timeout_ms: u32,
}

fn profiled(name: &Option<String>) -> bool {
    name.as_deref()
        .is_some_and(|n| n != crate::agentprof::DEFAULT)
}

impl Starter {
    /// `tree_run.run_node`: start one child under the parent and wait.
    /// A Fail is the tree refusing the command itself.
    pub fn run_node(&self, data_dir: &str, gate_root: &str, o: NodeOpts) -> Result<NodeRun, Fail> {
        let profile = match o.profiles.lookup(o.profile_name.as_deref()) {
            Ok(p) => p,
            Err(why) => {
                return Ok(NodeRun::failed(
                    o.session,
                    format!("agentic sub-agent failed: {why}"),
                ))
            }
        };
        let mut model = o.model.clone();
        if model.is_empty() {
            model = profile.model.clone().unwrap_or_default();
        }
        if model.is_empty() {
            model = o.default_model.clone();
        }
        let wall = sprig_wall(&o.tools, &o.envelope);
        if wall.is_empty() {
            return Ok(NodeRun::failed(
                o.session,
                format!("no requested tool is backed by the envelope (requested {}); nothing to delegate", o.tools_repr),
            ));
        }
        let spawn = wall.iter().any(|w| w == "spawn");
        let mut words = vec![self.self_path.clone(), self.temp_dir.clone()];
        if spawn {
            words.push(self.self_path.clone());
        }
        if words.iter().any(|w| w.chars().any(char::is_whitespace)) {
            return Ok(NodeRun::failed(
                o.session,
                super::agentic_sprig::SPRIG_SPACE_TEXT.to_string(),
            ));
        }
        let sp = tree::spawn_opts(
            data_dir,
            gate_root,
            &o.envelope,
            &o.parent,
            o.session.as_deref(),
            o.tokens.clone(),
            o.turns.clone(),
            o.timeout_ms,
            SpawnOpts {
                profile: if profiled(&o.profile_name) {
                    o.profile_name.clone()
                } else {
                    None
                },
                model: Some(model.clone()),
                parent_tokens_used: o.parent_tokens_used.clone(),
                parent_turns_used: o.parent_turns_used.clone(),
            },
        )?;
        let session = sp.session.clone();
        if sp.status != "started" {
            return Ok(NodeRun {
                status: sp.status,
                session: Some(session),
                answer: None,
                tokens_used: None,
                turns_used: None,
                reasons: sp.reasons,
                ask_id: if sp.ask_id.is_empty() {
                    None
                } else {
                    Some(sp.ask_id)
                },
                obj: Object::new(),
            });
        }
        let mut run_turns = o.turns.clone();
        if let Some(mt) = &o.max_turns {
            if run_turns.as_ref().is_none_or(|t| mt < t) {
                run_turns = Some(mt.clone());
            }
        }
        let is_profiled = profiled(&o.profile_name);
        let (argv, fds) = sprig_start(
            &self.self_path,
            &model,
            gate_root,
            data_dir,
            &session,
            &wall,
            &profile,
            is_profiled,
            &o.profiles,
            run_turns.as_ref(),
            o.tokens.as_ref(),
        );
        let env = child_env(&self.env, &o.profiles, true);
        let record = |pid: i32| {
            let _ = crate::tree::record_group(data_dir, &session, pid);
        };
        let branch = || crate::tree::read_tree(data_dir).branch_groups(&session);
        let out = run_sprig_child(
            &self.sprig,
            &self.env,
            &argv,
            &o.task,
            &o.cwd,
            o.timeout_s,
            &env,
            &fds,
            Some(&record),
            Some(&branch),
        );
        let (own_tokens, own_turns) = if out.error.is_none() {
            own_use(&out.obj)
        } else {
            (None, None)
        };
        // Nothing under it may outlive it: end its running descendants
        // with no report, leaves first, then the node with its own use.
        tree::end_subtree(data_dir, gate_root, &session, false);
        let node = tree::end(data_dir, gate_root, &session, own_tokens, own_turns)?;
        if let Some(e) = out.error {
            return Ok(NodeRun {
                status: "failed",
                session: Some(session),
                answer: None,
                tokens_used: node.tokens_used,
                turns_used: node.turns_used,
                reasons: vec![e],
                ask_id: None,
                obj: Object::new(),
            });
        }
        Ok(NodeRun {
            status: "started",
            session: Some(session),
            answer: Some(out.answer),
            tokens_used: node.tokens_used,
            turns_used: node.turns_used,
            reasons: vec![],
            ask_id: None,
            obj: out.obj,
        })
    }
}

#[cfg(test)]
mod child_env_tests {
    use super::*;

    #[test]
    fn every_child_gets_a_scrubbed_environment() {
        let env: HashMap<String, String> = [
            "PATH",
            "HOME",
            "ANTHROPIC_API_KEY",
            "ANTHROPIC_AUTH_TOKEN",
            "CLAUDE_CODE_OAUTH_TOKEN",
            "OPENAI_API_KEY",
            "XAI_API_KEY",
            "GITHUB_TOKEN",
            "MY_KEY_VAR",
            "ANTHROPIC_BASE_URL",
            "SPRIG_BACKEND",
            "SPRIG_MODEL",
            "TOKENIZERS_PARALLELISM",
        ]
        .iter()
        .map(|k| (k.to_string(), "v".to_string()))
        .collect();
        let names = |m: HashMap<String, String>| {
            let mut v: Vec<String> = m.into_keys().collect();
            v.sort();
            v
        };
        let ps = Profiles::default();
        assert_eq!(
            names(child_env(&env, &ps, true)),
            ["HOME", "MY_KEY_VAR", "PATH", "TOKENIZERS_PARALLELISM"]
        );
        assert_eq!(
            names(child_env(&env, &ps, false)),
            [
                "ANTHROPIC_BASE_URL",
                "HOME",
                "MY_KEY_VAR",
                "PATH",
                "SPRIG_BACKEND",
                "SPRIG_MODEL",
                "TOKENIZERS_PARALLELISM"
            ]
        );
    }
}

#[cfg(test)]
mod own_use_tests {
    use super::*;

    fn obj(s: &str) -> Object {
        match crate::gate::pyjson::loads_py(s, 900) {
            Ok(Value::Obj(o)) => o,
            _ => panic!("{s}"),
        }
    }

    #[test]
    fn a_negative_count_is_no_report_and_the_sum_caps() {
        let (tok, turns) = own_use(&obj(
            r#"{"usage": {"input_tokens": 300, "output_tokens": -250}, "model_turns": 2}"#,
        ));
        assert_eq!((tok, turns), (None, Some(BigInt::from(2))));
        let (tok, _) = own_use(&obj(
            r#"{"usage": {"input_tokens": 4611686018427387904, "output_tokens": 4611686018427387904}}"#,
        ));
        assert_eq!(tok, Some(BigInt::from(1u64 << 53)));
    }
}
