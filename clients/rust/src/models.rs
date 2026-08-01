//! Data model port of `opendaisugi/models.py`.
//!
//! `Permission`, `Envelope`, `InvariantDecl`, `PostconditionDecl` are plain
//! serde structs (the corpus always carries a full pydantic `model_dump`, so
//! every field is present, but `#[serde(default)]` keeps parsing tolerant).
//!
//! `ActionPlan`/`Step` are parsed by hand because a step is a discriminated
//! union on `"type"` AND because the predicate algebra (`predicate.rs`) needs
//! to resolve arbitrary dotted paths against a step's full JSON shape (the
//! same shape `StepBase.model_dump()` would produce) — so each `Step` keeps
//! its original `serde_json::Value` (`raw`) alongside the typed fields the
//! verifier's control-flow logic actually branches on.

use serde::Deserialize;
use serde_json::Value;
use std::collections::HashMap;

/// `models.SHELL_INTERPRETERS` — names treated as shell interpreters for
/// policy purposes.
pub const SHELL_INTERPRETERS: &[&str] = &[
    "sh", "bash", "zsh", "fish", "dash", "ksh", "csh", "tcsh", "xargs", "find", "python", "python3", "python2",
    "perl", "ruby", "node", "deno", "make", "awk", "gawk", "sed", "eval", "exec", "source", "env", "timeout",
    "nice", "nohup", "time", "stdbuf", "command", "setsid", "ionice", "sudo", "doas", "watch",
];

fn default_true() -> bool {
    true
}
fn default_max_exec() -> i64 {
    30
}
fn default_max_output() -> i64 {
    10
}
fn default_low() -> String {
    "low".to_string()
}
fn default_surface() -> String {
    "surface".to_string()
}

#[derive(Debug, Clone, Deserialize)]
pub struct Permission {
    #[serde(default)]
    pub file_read: Vec<String>,
    #[serde(default)]
    pub file_write: Vec<String>,
    #[serde(default)]
    pub network: bool,
    #[serde(default)]
    pub network_hosts: Vec<String>,
    #[serde(default)]
    pub shell: bool,
    #[serde(default)]
    pub shell_allowlist: Vec<String>,
    #[serde(default)]
    pub shell_allow_decomposition: bool,
    #[serde(default)]
    pub mcp_allowlist: Vec<String>,
    #[serde(default)]
    pub custom_step_allowlist: Vec<String>,
    #[serde(default = "default_max_exec")]
    pub max_execution_time_s: i64,
    #[serde(default = "default_max_output")]
    pub max_output_size_mb: i64,
    #[serde(default)]
    pub workspace_bounds: Option<([f64; 3], [f64; 3])>,
    #[serde(default)]
    pub obstacles: Vec<([f64; 3], [f64; 3])>,
    #[serde(default)]
    pub velocity_limit: Option<f64>,
    #[serde(default)]
    pub joint_limits: HashMap<String, (f64, f64)>,
    #[serde(default)]
    pub torque_limit: Option<f64>,
}

impl Default for Permission {
    fn default() -> Self {
        Permission {
            file_read: vec![],
            file_write: vec![],
            network: false,
            network_hosts: vec![],
            shell: false,
            shell_allowlist: vec![],
            shell_allow_decomposition: false,
            mcp_allowlist: vec![],
            custom_step_allowlist: vec![],
            max_execution_time_s: 30,
            max_output_size_mb: 10,
            workspace_bounds: None,
            obstacles: vec![],
            velocity_limit: None,
            joint_limits: HashMap::new(),
            torque_limit: None,
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct InvariantDecl {
    #[serde(rename = "type")]
    pub type_: String,
    #[serde(default = "default_true")]
    pub enforce: bool,
    #[serde(default)]
    pub expr: Option<Value>,
    #[serde(default)]
    pub description: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PostconditionDecl {
    #[serde(rename = "type")]
    pub type_: String,
    #[serde(default = "default_true")]
    pub enforce: bool,
    #[serde(default)]
    pub expr: Option<Value>,
    #[serde(default)]
    pub description: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Envelope {
    #[serde(default)]
    pub id: String,
    #[serde(default = "default_low")]
    pub stakes: String,
    #[serde(default = "default_surface")]
    pub shell_interpreter_policy: String,
    #[serde(default)]
    pub permissions: Permission,
    #[serde(default)]
    pub invariants: Vec<InvariantDecl>,
    #[serde(default)]
    pub postconditions: Vec<PostconditionDecl>,
}

pub const STRICT_STAKES: [&str; 2] = ["high", "physical"];

/// `resolve_strict` — explicit bool wins; None defaults to True for
/// high/physical stakes (verify.py `resolve_strict`).
pub fn resolve_strict(strict: Option<bool>, envelope: &Envelope) -> bool {
    resolve_strict_stakes(strict, &envelope.stakes)
}

pub fn resolve_strict_stakes(strict: Option<bool>, stakes: &str) -> bool {
    match strict {
        Some(b) => b,
        None => STRICT_STAKES.contains(&stakes),
    }
}

// --- ActionPlan / Step (hand-parsed: discriminated union + raw retention) ---

#[derive(Debug, Clone)]
pub struct ActionPlan {
    pub id: String,
    pub steps: Vec<Step>,
}

#[derive(Debug, Clone)]
pub struct Step {
    pub id: String,
    pub step_type: String,
    pub depends_on: Vec<String>,
    pub preferred_model: Option<String>,
    /// The full original JSON object for this step — used by the predicate
    /// algebra (`predicate.rs`) to resolve arbitrary dotted paths exactly as
    /// Python's `_resolve_path` would against `step.model_dump()`.
    pub raw: Value,
    pub kind: StepKind,
}

#[derive(Debug, Clone)]
pub enum StepKind {
    Shell {
        command: String,
    },
    FileRead {
        path: String,
    },
    FileWrite {
        path: String,
    },
    Network {
        url: String,
    },
    JointMove {
        joint_targets: HashMap<String, f64>,
        duration_s: f64,
        velocity_scale: f64,
    },
    CartesianMove {
        target_position: (f64, f64, f64),
    },
    Gripper,
    SimReset,
    Vla {
        target_pose: Option<(f64, f64, f64)>,
    },
    Task,
    Agentic {
        workspace: String,
        tools: Vec<String>,
    },
    Skill {
        skill_id: String,
        contract_envelope: Option<Box<Envelope>>,
    },
    Mcp {
        server: String,
        tool: String,
    },
    /// A step whose `"type"` is not one of the 13 built-in kinds. Cannot
    /// actually occur in a self-contained corpus case (conformance.py's
    /// `_plan_is_portable` filters custom-registered step types out of the
    /// recorded corpus, and no other type could pass the oracle's own
    /// pydantic validation) — handled anyway for wire robustness and so the
    /// strict-mode "unverifiable step type" rejection has somewhere to live.
    Unknown,
}

fn get_str<'a>(v: &'a Value, key: &str) -> Option<&'a str> {
    v.get(key).and_then(|x| x.as_str())
}

fn require_str(v: &Value, key: &str, step_type: &str) -> Result<String, String> {
    get_str(v, key)
        .map(|s| s.to_string())
        .ok_or_else(|| format!("step type '{step_type}' missing required field '{key}'"))
}

fn get_str_vec(v: &Value, key: &str) -> Vec<String> {
    v.get(key)
        .and_then(|x| x.as_array())
        .map(|a| a.iter().filter_map(|e| e.as_str().map(String::from)).collect())
        .unwrap_or_default()
}

fn get_f64(v: &Value, key: &str, default: f64) -> f64 {
    v.get(key).and_then(|x| x.as_f64()).unwrap_or(default)
}

fn get_vec3(v: &Value, key: &str, step_type: &str) -> Result<(f64, f64, f64), String> {
    let arr = v
        .get(key)
        .and_then(|x| x.as_array())
        .ok_or_else(|| format!("step type '{step_type}' missing required field '{key}'"))?;
    if arr.len() != 3 {
        return Err(format!("step type '{step_type}' field '{key}' is not a 3-tuple"));
    }
    let f = |i: usize| arr[i].as_f64().ok_or_else(|| format!("'{key}' element {i} not numeric"));
    Ok((f(0)?, f(1)?, f(2)?))
}

fn parse_vec3_opt(v: &Value) -> Option<(f64, f64, f64)> {
    let arr = v.as_array()?;
    if arr.len() != 3 {
        return None;
    }
    Some((arr[0].as_f64()?, arr[1].as_f64()?, arr[2].as_f64()?))
}

fn get_f64_map(v: &Value, key: &str) -> HashMap<String, f64> {
    let mut out = HashMap::new();
    if let Some(obj) = v.get(key).and_then(|x| x.as_object()) {
        for (k, val) in obj {
            if let Some(f) = val.as_f64() {
                out.insert(k.clone(), f);
            }
        }
    }
    out
}

fn parse_step(v: &Value) -> Result<Step, String> {
    let step_type = get_str(v, "type").ok_or("step is missing 'type'")?.to_string();
    let id = get_str(v, "id").unwrap_or("").to_string();
    let depends_on = get_str_vec(v, "depends_on");
    let preferred_model = get_str(v, "preferred_model").map(String::from);

    let kind = match step_type.as_str() {
        "shell" => StepKind::Shell { command: require_str(v, "command", &step_type)? },
        "file_read" => StepKind::FileRead { path: require_str(v, "path", &step_type)? },
        "file_write" => StepKind::FileWrite { path: require_str(v, "path", &step_type)? },
        "network" => StepKind::Network { url: require_str(v, "url", &step_type)? },
        "joint_move" => StepKind::JointMove {
            joint_targets: get_f64_map(v, "joint_targets"),
            duration_s: get_f64(v, "duration_s", 1.0),
            velocity_scale: get_f64(v, "velocity_scale", 1.0),
        },
        "cartesian_move" => StepKind::CartesianMove {
            target_position: get_vec3(v, "target_position", &step_type)?,
        },
        "gripper" => StepKind::Gripper,
        "sim_reset" => StepKind::SimReset,
        "vla" => StepKind::Vla {
            target_pose: v.get("target_pose").filter(|x| !x.is_null()).and_then(parse_vec3_opt),
        },
        "task" => StepKind::Task,
        "agentic" => StepKind::Agentic {
            workspace: require_str(v, "workspace", &step_type)?,
            tools: get_str_vec(v, "tools"),
        },
        "skill" => StepKind::Skill {
            skill_id: require_str(v, "skill_id", &step_type)?,
            contract_envelope: match v.get("contract_envelope") {
                None => None,
                Some(cv) if cv.is_null() => None,
                Some(cv) => Some(Box::new(
                    serde_json::from_value(cv.clone())
                        .map_err(|e| format!("contract_envelope: {e}"))?,
                )),
            },
        },
        "mcp" => StepKind::Mcp {
            server: require_str(v, "server", &step_type)?,
            tool: require_str(v, "tool", &step_type)?,
        },
        _ => StepKind::Unknown,
    };

    Ok(Step { id, step_type, depends_on, preferred_model, raw: v.clone(), kind })
}

pub fn parse_plan(v: &Value) -> Result<ActionPlan, String> {
    let id = get_str(v, "id").unwrap_or("plan_case").to_string();
    let steps_v = v.get("steps").and_then(|x| x.as_array()).ok_or("plan.steps missing")?;
    let mut steps = Vec::with_capacity(steps_v.len());
    for sv in steps_v {
        steps.push(parse_step(sv)?);
    }
    Ok(ActionPlan { id, steps })
}

/// The 13 built-in step types with a verification story (permission-stage
/// case, dedicated z3/robotics handler, or gating elsewhere). Mirrors
/// `verify._KNOWN_STEP_TYPES`.
pub const KNOWN_STEP_TYPES: [&str; 13] = [
    "shell",
    "network",
    "file_read",
    "file_write",
    "mcp",
    "task",
    "skill",
    "agentic",
    "joint_move",
    "cartesian_move",
    "gripper",
    "sim_reset",
    "vla",
];
