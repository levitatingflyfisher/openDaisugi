//! pydantic's lax validation (pydantic 2.13, pydantic-core 2.46) for the
//! models a pathway holds: `CompiledPathway`, `Envelope` and what it
//! holds, `ActionPlan` with its registered step types, and
//! `PathwayParameter`. A model is a table of fields; validating one gives
//! its `model_dump()` (fields in order, defaults filled in) or pydantic's
//! errors, in pydantic's order and words. The Go client's `pmodel` is the
//! reference.
//!
//! Two modes, as pydantic has them: `Python` (`model_validate` of what
//! `json.loads` gave) and `Json` (`model_validate_json`, read with jiter).
//! Their messages differ in a few places.

use std::sync::OnceLock;

use crate::gate::pydantic::{truncate_repr, DOCS_VERSION};
use crate::gate::pyjson::{py_repr, py_type_name, Object, Value};
use crate::gate::pymodel::{float_to_int, str_as_float, str_as_int};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    Json,
    Python,
}

/// One line of a ValidationError.
#[derive(Debug, Clone)]
pub struct Err {
    pub loc: Vec<String>,
    pub typ: String,
    pub msg: String,
    pub input: Value,
}

/// `pydantic_core.ValidationError`.
#[derive(Debug, Clone)]
pub struct ValidationError {
    pub title: String,
    pub errs: Vec<Err>,
}

/// The error type of a plan step this port does not read (a step given
/// as a string, which `coerce_step` decodes).
pub const UNREADABLE_STEP: &str = "unreadable_step";

impl ValidationError {
    /// `str(exc)`.
    pub fn text(&self) -> String {
        let n = self.errs.len();
        let mut out = format!("{n} validation error{} for {}", if n == 1 { "" } else { "s" }, self.title);
        for e in &self.errs {
            out.push('\n');
            if !e.loc.is_empty() {
                out.push_str(&e.loc.join("."));
                out.push('\n');
            }
            let repr = py_repr(&e.input).unwrap_or_else(|_| "...".into());
            out.push_str(&format!(
                "  {} [type={}, input_value={}, input_type={}]",
                e.msg,
                e.typ,
                truncate_repr(&repr),
                py_type_name(&e.input)
            ));
            out.push_str(&format!("\n    For further information visit https://errors.pydantic.dev/{DOCS_VERSION}/v/{}", e.typ));
        }
        out
    }

    /// Whether a plan step given as a string stopped the validation.
    pub fn unreadable_step(&self) -> bool {
        self.errs.iter().any(|e| e.typ == UNREADABLE_STEP)
    }
}

/// Where a validator is: a chain from the root, made into the error's
/// location only when there is an error.
#[derive(Clone, Copy)]
pub enum Loc<'a> {
    Root,
    Key(&'a Loc<'a>, &'a str),
    Idx(&'a Loc<'a>, usize),
}

impl Loc<'_> {
    pub fn to_vec(&self) -> Vec<String> {
        let mut out = vec![];
        let mut at = self;
        loop {
            match at {
                Loc::Root => break,
                Loc::Key(up, k) => {
                    out.push(k.to_string());
                    at = up;
                }
                Loc::Idx(up, i) => {
                    out.push(i.to_string());
                    at = up;
                }
            }
        }
        out.reverse();
        out
    }
}



/// A field's type.
pub enum Schema {
    Str,
    StrMax(usize),
    Any,
    Bool,
    Int,
    Float,
    /// `float` with `ge` and `le`.
    FloatRange(f64, f64),
    Nullable(Box<Schema>),
    List(Box<Schema>),
    Tuple(Vec<Schema>),
    Dict(Box<Schema>),
    Literal(&'static [&'static str]),
    Model(Id),
    /// `ActionPlan.steps`: `list[Any]` with `coerce_step` before and the
    /// `StepBase` check after.
    Steps,
    /// `ActionPlan.steps` as a model's reply is read: a step given as a
    /// string is decoded as `coerce_step` decodes it.
    StepsReply,
    /// A step held as `Any` with `coerce_step` before and the `StepBase`
    /// check after (`RefinementRecord.step`), nullable or not. A dict of a
    /// registered type validates as that step; anything else is
    /// `UNREADABLE_STEP`, which callers refuse.
    StepField(bool),
}

/// The models, by name, so the tables can refer to each other.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Id {
    Permission,
    Invariant,
    Postcondition,
    Fallback,
    Envelope,
    ActionPlan,
    PathwayParameter,
    CompiledPathway,
    Violation,
    VerificationResult,
    RefinementRecord,
    /// `ActionPlan` as a model's reply is read.
    ActionPlanReply,
    /// `distiller.GeneralizedTemplate`.
    GeneralizedTemplate,
}

pub struct Field {
    pub name: &'static str,
    pub schema: Schema,
    /// `None`: required.
    pub default: Option<fn() -> Value>,
}

pub struct Model {
    pub name: &'static str,
    pub fields: Vec<Field>,
    /// The after-validator Envelope and ActionPlan carry
    /// (`models.non_finite_error`): once the fields validate, each number
    /// in the result that is not finite is a `finite_number` error.
    pub finite: bool,
}

fn req(name: &'static str, schema: Schema) -> Field {
    Field { name, schema, default: None }
}

fn def(name: &'static str, schema: Schema, d: fn() -> Value) -> Field {
    Field { name, schema, default: Some(d) }
}

fn list(s: Schema) -> Schema {
    Schema::List(Box::new(s))
}

fn nullable(s: Schema) -> Schema {
    Schema::Nullable(Box::new(s))
}

fn dict(s: Schema) -> Schema {
    Schema::Dict(Box::new(s))
}

fn float3() -> Schema {
    Schema::Tuple(vec![Schema::Float, Schema::Float, Schema::Float])
}

fn float4() -> Schema {
    Schema::Tuple(vec![Schema::Float, Schema::Float, Schema::Float, Schema::Float])
}

fn bbox() -> Schema {
    Schema::Tuple(vec![float3(), float3()])
}

fn empty_list() -> Value {
    Value::List(vec![])
}
fn empty_dict() -> Value {
    Value::Obj(Object::new())
}
fn null() -> Value {
    Value::Null
}
fn f_false() -> Value {
    Value::Bool(false)
}
fn f_true() -> Value {
    Value::Bool(true)
}
fn int0() -> Value {
    Value::Int("0".into())
}
fn int1() -> Value {
    Value::Int("1".into())
}
fn float0() -> Value {
    Value::Float(0.0)
}
fn float1() -> Value {
    Value::Float(1.0)
}
fn empty_str() -> Value {
    Value::Str(String::new())
}

/// Eight random hex digits, as `uuid4().hex[:8]` gives them.
fn hex8() -> String {
    crate::gate::random_hex(4).unwrap_or_else(|_| "00000000".into())
}

fn new_envelope_id() -> Value {
    Value::Str(format!("env_{}", hex8()))
}

fn new_plan_id() -> Value {
    Value::Str(format!("plan_{}", hex8()))
}

fn fallback_default() -> Value {
    let (out, _) = model(Id::Fallback).fields(&Object::new(), &Loc::Root, Mode::Json);
    out
}

/// The registered step types, in registration order: the tag, the
/// model's name, and its own fields after `StepBase`'s.
pub struct Steps {
    pub order: Vec<&'static str>,
    pub models: Vec<Model>,
}

impl Steps {
    pub fn get(&self, tag: &str) -> Option<&Model> {
        self.order.iter().position(|t| *t == tag).map(|i| &self.models[i])
    }
}

struct Models {
    permission: Model,
    invariant: Model,
    postcondition: Model,
    fallback: Model,
    envelope: Model,
    action_plan: Model,
    parameter: Model,
    compiled: Model,
    violation: Model,
    result: Model,
    refinement: Model,
    plan_reply: Model,
    template: Model,
    steps: Steps,
}

fn models() -> &'static Models {
    static M: OnceLock<Models> = OnceLock::new();
    M.get_or_init(build)
}

pub fn model(id: Id) -> &'static Model {
    let m = models();
    match id {
        Id::Permission => &m.permission,
        Id::Invariant => &m.invariant,
        Id::Postcondition => &m.postcondition,
        Id::Fallback => &m.fallback,
        Id::Envelope => &m.envelope,
        Id::ActionPlan => &m.action_plan,
        Id::PathwayParameter => &m.parameter,
        Id::CompiledPathway => &m.compiled,
        Id::Violation => &m.violation,
        Id::VerificationResult => &m.result,
        Id::RefinementRecord => &m.refinement,
        Id::ActionPlanReply => &m.plan_reply,
        Id::GeneralizedTemplate => &m.template,
    }
}

pub fn steps() -> &'static Steps {
    &models().steps
}

fn build() -> Models {
    use Schema::{Any, Bool, Float, FloatRange, Int, Literal, Str, StrMax, Tuple};
    let permission = Model {
        name: "Permission",
        finite: false,
        fields: vec![
            def("file_read", list(Str), empty_list),
            def("file_write", list(Str), empty_list),
            def("network", Bool, f_false),
            def("network_hosts", list(Str), empty_list),
            def("shell", Bool, f_false),
            def("shell_allowlist", list(Str), empty_list),
            def("shell_allow_decomposition", Bool, f_false),
            def("mcp_allowlist", list(Str), empty_list),
            def("custom_step_allowlist", list(Str), empty_list),
            def("max_execution_time_s", Int, || Value::Int("30".into())),
            def("max_output_size_mb", Int, || Value::Int("10".into())),
            def("workspace_bounds", nullable(bbox()), null),
            def("obstacles", list(bbox()), empty_list),
            def("velocity_limit", nullable(Float), null),
            def("joint_limits", dict(Tuple(vec![Float, Float])), empty_dict),
            def("torque_limit", nullable(Float), null),
        ],
    };
    let invariant = Model {
        name: "Invariant",
        finite: false,
        fields: vec![
            req("type", Str),
            def("target", nullable(Str), null),
            def("scope", nullable(Str), null),
            req("description", Str),
            def("expr", Any, null),
            def("enforce", Bool, f_true),
        ],
    };
    let postcondition = Model {
        name: "Postcondition",
        finite: false,
        fields: vec![
            req("type", Str),
            def("path", nullable(Str), null),
            def("expected", nullable(Int), null),
            def("min", nullable(Int), null),
            def("max", nullable(Int), null),
            def("description", nullable(Str), null),
            def("expr", Any, null),
            def("enforce", Bool, f_true),
        ],
    };
    let fallback = Model {
        name: "FallbackStrategy",
        finite: false,
        fields: vec![
            def("strategy", Str, || Value::Str("tier2_recompute".into())),
            def("model", Str, || Value::Str("anthropic/claude-sonnet-4-20250514".into())),
            def("include_refinement", Bool, f_true),
        ],
    };
    let envelope = Model {
        name: "Envelope",
        finite: true,
        fields: vec![
            def("id", Str, new_envelope_id),
            req("generated_by", Str),
            req("task", Str),
            req("permissions", Schema::Model(Id::Permission)),
            def("invariants", list(Schema::Model(Id::Invariant)), empty_list),
            def("postconditions", list(Schema::Model(Id::Postcondition)), empty_list),
            def("fallback", Schema::Model(Id::Fallback), fallback_default),
            def("parent_envelope", nullable(Str), null),
            def("tightening_only", Bool, f_true),
            def("summary", nullable(StrMax(80)), null),
            def("cache_key", nullable(Str), null),
            def("stakes", Literal(&["low", "medium", "high", "physical"]), || Value::Str("low".into())),
            def("shell_interpreter_policy", Literal(&["surface", "strict", "allow"]), || Value::Str("surface".into())),
        ],
    };
    let action_plan = Model {
        name: "ActionPlan",
        finite: true,
        fields: vec![def("id", Str, new_plan_id), req("source", Str), req("task", Str), req("steps", Schema::Steps)],
    };
    let parameter = Model {
        name: "PathwayParameter",
        finite: false,
        fields: vec![
            req("name", Str),
            req("step_index", Int),
            req("step_id", Str),
            req("field", Str),
            req("head", Str),
            def("observed", list(Str), empty_list),
        ],
    };
    let compiled = Model {
        name: "CompiledPathway",
        finite: false,
        fields: vec![
            req("id", Str),
            req("task_description", Str),
            req("task_embedding", list(Float)),
            def("embedding_model", Str, empty_str),
            def("embedding_model_version", Str, empty_str),
            req("envelope", Schema::Model(Id::Envelope)),
            req("plan_template", Schema::Model(Id::ActionPlan)),
            req("source_trace_ids", list(Str)),
            def("version", Int, int1),
            def("hit_count", Int, int0),
            req("distilled_at", Float),
            def("last_activation_at", Float, float0),
            def("failure_count", Int, int0),
            def("activation_count", Int, int0),
            def("structure_signature", nullable(Str), null),
            def("parameters", list(Schema::Model(Id::PathwayParameter)), empty_list),
        ],
    };
    let violation = Model {
        name: "Violation",
        finite: false,
        fields: vec![
            req("stage", Str),
            req("message", Str),
            def("detail", dict(Any), empty_dict),
            def("suggested_remediation", nullable(Str), null),
        ],
    };
    let result = Model {
        name: "VerificationResult",
        finite: false,
        fields: vec![
            req("ok", Bool),
            def("violations", list(Schema::Model(Id::Violation)), empty_list),
            def("warnings", list(Str), empty_list),
            req("envelope_id", Str),
            req("plan_id", Str),
            req("duration_ms", Float),
            def("client", Str, || Value::Str("python".into())),
            def("fallback", nullable(Str), null),
            def("client_verdict", nullable(Bool), null),
        ],
    };
    let refinement = Model {
        name: "RefinementRecord",
        finite: false,
        fields: vec![
            req("step", Schema::StepField(false)),
            req("violations", list(Schema::Model(Id::Violation))),
            req("z3_counterexample", nullable(dict(Any))),
            req("envelope_id", Str),
            req("fallback_action", Literal(&["halted", "recomputed"])),
            def("recomputed_step", Schema::StepField(true), null),
            def("recomputed_verification", nullable(Schema::Model(Id::VerificationResult)), null),
            req("timestamp", Float),
            def("cache_key", nullable(Str), null),
        ],
    };
    let plan_reply = Model {
        name: "ActionPlan",
        finite: true,
        fields: vec![def("id", Str, new_plan_id), req("source", Str), req("task", Str), req("steps", Schema::StepsReply)],
    };
    let template = Model {
        name: "GeneralizedTemplate",
        finite: false,
        fields: vec![req("task_description", Str), req("plan_template", Schema::Model(Id::ActionPlanReply))],
    };
    let unit = || FloatRange(0.0, 1.0);
    let defs: Vec<(&'static str, &'static str, Vec<Field>)> = vec![
        ("shell", "ShellStep", vec![req("command", Str)]),
        ("file_read", "FileReadStep", vec![req("path", Str)]),
        ("file_write", "FileWriteStep", vec![req("path", Str), req("content", Str)]),
        (
            "network",
            "NetworkStep",
            vec![
                req("url", Str),
                def("method", Literal(&["GET"]), || Value::Str("GET".into())),
                def("headers", dict(Str), empty_dict),
            ],
        ),
        (
            "joint_move",
            "JointMoveStep",
            vec![req("joint_targets", dict(Float)), def("duration_s", Float, float1), def("velocity_scale", unit(), float1)],
        ),
        (
            "cartesian_move",
            "CartesianMoveStep",
            vec![
                req("target_position", float3()),
                def("target_orientation", nullable(float4()), null),
                def("duration_s", Float, float1),
                def("velocity_scale", unit(), float1),
            ],
        ),
        (
            "gripper",
            "GripperStep",
            vec![req("action", Literal(&["open", "close"])), def("hold_s", Float, || Value::Float(0.2))],
        ),
        ("sim_reset", "SimulationResetStep", vec![def("seed", nullable(Int), null)]),
        (
            "vla",
            "VLAStep",
            vec![
                req("task", Str),
                def("target_pose", nullable(float3()), null),
                def("max_actions", Int, || Value::Int("50".into())),
                def("timeout_s", Float, || Value::Float(5.0)),
            ],
        ),
        ("task", "TaskStep", vec![req("prompt", Str)]),
        (
            "agentic",
            "AgenticStep",
            vec![
                req("prompt", Str),
                req("workspace", Str),
                def("tools", list(Str), empty_list),
                def("max_turns", nullable(Int), null),
            ],
        ),
        (
            "skill",
            "SkillStep",
            vec![
                req("skill_id", Str),
                def("skill_input", dict(Any), empty_dict),
                def("contract_envelope", nullable(Schema::Model(Id::Envelope)), null),
            ],
        ),
        ("mcp", "MCPStep", vec![req("server", Str), req("tool", Str), def("arguments", dict(Any), empty_dict)]),
    ];
    let mut steps = Steps { order: vec![], models: vec![] };
    for (tag, name, own) in defs {
        let mut fields = vec![
            req("id", Str),
            def("depends_on", list(Str), empty_list),
            def("metadata", dict(Any), empty_dict),
            def("postcondition", nullable(Schema::Model(Id::Postcondition)), null),
            def("preferred_model", nullable(Str), null),
            Field { name: "type", schema: Literal(step_tag(tag)), default: Some(step_default(tag)) },
        ];
        fields.extend(own);
        steps.order.push(tag);
        steps.models.push(Model { name, finite: false, fields });
    }
    Models {
        permission,
        invariant,
        postcondition,
        fallback,
        envelope,
        action_plan,
        parameter,
        compiled,
        violation,
        result,
        refinement,
        plan_reply,
        template,
        steps,
    }
}

/// `Literal[tag]` for a step's `type` field.
fn step_tag(tag: &'static str) -> &'static [&'static str] {
    const TAGS: &[&[&str]] = &[
        &["shell"],
        &["file_read"],
        &["file_write"],
        &["network"],
        &["joint_move"],
        &["cartesian_move"],
        &["gripper"],
        &["sim_reset"],
        &["vla"],
        &["task"],
        &["agentic"],
        &["skill"],
        &["mcp"],
    ];
    TAGS.iter().find(|t| t[0] == tag).copied().expect("every step tag is listed")
}

fn step_default(tag: &'static str) -> fn() -> Value {
    match tag {
        "shell" => || Value::Str("shell".into()),
        "file_read" => || Value::Str("file_read".into()),
        "file_write" => || Value::Str("file_write".into()),
        "network" => || Value::Str("network".into()),
        "joint_move" => || Value::Str("joint_move".into()),
        "cartesian_move" => || Value::Str("cartesian_move".into()),
        "gripper" => || Value::Str("gripper".into()),
        "sim_reset" => || Value::Str("sim_reset".into()),
        "vla" => || Value::Str("vla".into()),
        "task" => || Value::Str("task".into()),
        "agentic" => || Value::Str("agentic".into()),
        "skill" => || Value::Str("skill".into()),
        _ => || Value::Str("mcp".into()),
    }
}

const TRUE_WORDS: &[&str] = &["1", "on", "t", "true", "y", "yes"];
const FALSE_WORDS: &[&str] = &["0", "off", "f", "false", "n", "no"];

/// One error about an input the validator owns.
fn err1(loc: &Loc, typ: &str, msg: impl Into<String>, v: Value) -> Vec<Err> {
    vec![Err { loc: loc.to_vec(), typ: typ.into(), msg: msg.into(), input: v }]
}

impl Schema {
    /// Validates a value the caller keeps.
    pub fn validate(&self, v: &Value, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
        self.take(v.clone(), loc, mode)
    }

    /// Validates a value, consuming it: strings and containers move into
    /// the result rather than being copied.
    pub fn take(&self, v: Value, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
        match self {
            Schema::Str => match v {
                Value::Str(_) => (v, vec![]),
                _ => (Value::Null, err1(loc, "string_type", "Input should be a valid string", v)),
            },
            Schema::StrMax(max) => match &v {
                Value::Str(s) => {
                    if s.chars().count() > *max {
                        (Value::Null, err1(loc, "string_too_long", format!("String should have at most {max} characters"), v))
                    } else {
                        (v, vec![])
                    }
                }
                _ => (Value::Null, err1(loc, "string_type", "Input should be a valid string", v)),
            },
            Schema::Any => (v, vec![]),
            Schema::Bool => validate_bool(v, loc),
            Schema::Int => validate_int(v, loc),
            Schema::Float => validate_float(v, loc, mode),
            Schema::FloatRange(lo, hi) => {
                let keep = v.clone();
                let (out, errs) = validate_float(v, loc, mode);
                if !errs.is_empty() {
                    return (out, errs);
                }
                let f = match out {
                    Value::Float(f) => f,
                    _ => 0.0,
                };
                if !(f >= *lo) {
                    return (
                        Value::Null,
                        err1(loc, "greater_than_equal", format!("Input should be greater than or equal to {}", go_num(*lo)), keep),
                    );
                }
                if !(f <= *hi) {
                    return (
                        Value::Null,
                        err1(loc, "less_than_equal", format!("Input should be less than or equal to {}", go_num(*hi)), keep),
                    );
                }
                (Value::Float(f), vec![])
            }
            Schema::Nullable(inner) => {
                if v.is_null() {
                    (Value::Null, vec![])
                } else {
                    inner.take(v, loc, mode)
                }
            }
            Schema::List(elem) => {
                let xs = match v {
                    Value::List(xs) => xs,
                    _ => {
                        let msg = if mode == Mode::Json { "Input should be a valid array" } else { "Input should be a valid list" };
                        return (Value::Null, err1(loc, "list_type", msg, v));
                    }
                };
                let mut out = Vec::with_capacity(xs.len());
                let mut errs = vec![];
                for (i, x) in xs.into_iter().enumerate() {
                    let (y, e) = elem.take(x, &Loc::Idx(loc, i), mode);
                    errs.extend(e);
                    out.push(y);
                }
                (Value::List(out), errs)
            }
            Schema::Tuple(items) => {
                let n = match &v {
                    Value::List(xs) => xs.len(),
                    _ => {
                        let msg = if mode == Mode::Json { "Input should be a valid array" } else { "Input should be a valid tuple" };
                        return (Value::Null, err1(loc, "tuple_type", msg, v));
                    }
                };
                if n > items.len() {
                    return (
                        Value::Null,
                        err1(
                            loc,
                            "too_long",
                            format!("Tuple should have at most {} items after validation, not {n}", items.len()),
                            v,
                        ),
                    );
                }
                let mut out = vec![];
                let mut errs = vec![];
                if n < items.len() {
                    // A short tuple reports the whole input as each missing
                    // item's input.
                    let Value::List(xs) = &v else { unreachable!("checked above") };
                    for (i, s) in items.iter().enumerate() {
                        let l = Loc::Idx(loc, i);
                        match xs.get(i) {
                            None => errs.push(Err {
                                loc: l.to_vec(),
                                typ: "missing".into(),
                                msg: "Field required".into(),
                                input: v.clone(),
                            }),
                            Some(x) => {
                                let (y, e) = s.validate(x, &l, mode);
                                errs.extend(e);
                                out.push(y);
                            }
                        }
                    }
                    return (Value::List(out), errs);
                }
                let Value::List(xs) = v else { unreachable!("checked above") };
                for (i, (s, x)) in items.iter().zip(xs).enumerate() {
                    let (y, e) = s.take(x, &Loc::Idx(loc, i), mode);
                    errs.extend(e);
                    out.push(y);
                }
                (Value::List(out), errs)
            }
            Schema::Dict(val) => {
                let mut o = match v {
                    Value::Obj(o) => o,
                    _ => {
                        let msg = if mode == Mode::Json { "Input should be an object" } else { "Input should be a valid dictionary" };
                        return (Value::Null, err1(loc, "dict_type", msg, v));
                    }
                };
                let keys: Vec<String> = o.keys().to_vec();
                let mut out = Object::with_capacity(keys.len());
                let mut errs = vec![];
                for k in keys {
                    let x = o.take_value(&k).unwrap_or(Value::Null);
                    let (y, e) = val.take(x, &Loc::Key(loc, &k), mode);
                    errs.extend(e);
                    out.push_new(k, y);
                }
                (Value::Obj(out), errs)
            }
            Schema::Literal(choices) => {
                if let Value::Str(s) = &v {
                    if choices.contains(&s.as_str()) {
                        return (v, vec![]);
                    }
                }
                let quoted: Vec<String> = choices.iter().map(|c| crate::gate::py::text::repr(c)).collect();
                let msg = if quoted.len() == 1 {
                    format!("Input should be {}", quoted[0])
                } else {
                    format!("Input should be {} or {}", quoted[..quoted.len() - 1].join(", "), quoted[quoted.len() - 1])
                };
                (Value::Null, err1(loc, "literal_error", msg, v))
            }
            Schema::Model(id) => model(*id).take(v, loc, mode),
            Schema::Steps => validate_steps(v, loc, mode),
            Schema::StepsReply => validate_reply_steps(v, loc, mode),
            Schema::StepField(nullable) => {
                if v.is_null() && *nullable {
                    return (Value::Null, vec![]);
                }
                if let Value::Obj(o) = &v {
                    if let Some(m) = o.get("type").and_then(|t| t.as_str()).and_then(|t| steps().get(t)) {
                        let (out, errs) = m.fields(o, &Loc::Root, Mode::Python);
                        if errs.is_empty() {
                            return (out, vec![]);
                        }
                    }
                }
                (Value::Null, err1(loc, UNREADABLE_STEP, "a step this binary does not read", v))
            }
        }
    }
}

/// Go's `%v` of a float bound: 0 and 1 print as integers.
fn go_num(f: f64) -> String {
    if f.fract() == 0.0 && f.abs() < 1e15 {
        format!("{}", f as i64)
    } else {
        crate::gate::pyjson::float_repr(f)
    }
}

fn validate_bool(v: Value, loc: &Loc) -> (Value, Vec<Err>) {
    const PARSING: (&str, &str) = ("bool_parsing", "Input should be a valid boolean, unable to interpret input");
    const TYPE: (&str, &str) = ("bool_type", "Input should be a valid boolean");
    let r = match &v {
        Value::Bool(b) => Ok(*b),
        Value::Str(s) => {
            let l = s.to_lowercase();
            if TRUE_WORDS.contains(&l.as_str()) {
                Ok(true)
            } else if FALSE_WORDS.contains(&l.as_str()) {
                Ok(false)
            } else {
                Err(PARSING)
            }
        }
        Value::Int(t) => match t.parse::<i64>() {
            Err(_) => Err(TYPE),
            Ok(0) => Ok(false),
            Ok(1) => Ok(true),
            Ok(_) => Err(PARSING),
        },
        Value::Float(f) => {
            if !f.is_finite() || f.fract() != 0.0 || f.abs() >= 9.223372036854775807e18 {
                Err(TYPE)
            } else if *f == 0.0 {
                Ok(false)
            } else if *f == 1.0 {
                Ok(true)
            } else {
                Err(PARSING)
            }
        }
        _ => Err(TYPE),
    };
    match r {
        Ok(b) => (Value::Bool(b), vec![]),
        Err((typ, msg)) => (Value::Null, err1(loc, typ, msg, v)),
    }
}

fn validate_int(v: Value, loc: &Loc) -> (Value, Vec<Err>) {
    let r = match &v {
        Value::Bool(b) => Ok(if *b { "1" } else { "0" }.to_string()),
        Value::Int(_) => return (v, vec![]),
        Value::Float(f) => float_to_int(*f),
        Value::Str(s) => str_as_int(s),
        _ => Err(("Input should be a valid integer", "int_type")),
    };
    match r {
        Ok(t) => (Value::Int(t), vec![]),
        Err((msg, typ)) => (Value::Null, err1(loc, typ, msg, v)),
    }
}

fn validate_float(v: Value, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
    let bad_type = ("Input should be a valid number", "float_type");
    let r = match &v {
        Value::Bool(b) => Ok(if *b { 1.0 } else { 0.0 }),
        Value::Int(t) => {
            let f: f64 = t.parse().unwrap_or(if t.starts_with('-') { f64::NEG_INFINITY } else { f64::INFINITY });
            if mode == Mode::Python && f.is_infinite() {
                // float(int) overflows in Python mode; in JSON mode jiter's
                // big int reads as an infinity.
                Err(bad_type)
            } else {
                Ok(f)
            }
        }
        Value::Float(f) => Ok(*f),
        Value::Str(s) => str_as_float(s),
        _ => Err(bad_type),
    };
    match r {
        Ok(f) => (Value::Float(f), vec![]),
        Err((msg, typ)) => (Value::Null, err1(loc, typ, msg, v)),
    }
}

/// `ActionPlan.steps`. A dict whose `type` is registered validates as
/// that step model in Python mode (`subclass.model_validate`); its errors
/// take the field's place, with no item index. A str item is
/// `decode_dict_text`'s case, which this port does not read.
fn validate_steps(v: Value, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
    let xs = match &v {
        Value::List(xs) => xs,
        _ => {
            let msg = if mode == Mode::Json { "Input should be a valid array" } else { "Input should be a valid list" };
            return (Value::Null, err1(loc, "list_type", msg, v));
        }
    };
    // Each item is checked before any is consumed: an error reports the
    // whole list as its input.
    let mut ms = Vec::with_capacity(xs.len());
    for x in xs {
        if matches!(x, Value::Str(_)) {
            return (Value::Null, err1(loc, UNREADABLE_STEP, "a plan step is a string", v));
        }
        let m = match x {
            Value::Obj(o) => match o.get("type") {
                Some(Value::Str(tag)) => steps().get(tag),
                _ => None,
            },
            _ => None,
        };
        match m {
            Some(m) => ms.push(m),
            None => {
                let r = py_repr(x).unwrap_or_else(|_| "...".into());
                return (
                    Value::Null,
                    err1(
                        loc,
                        "value_error",
                        format!(
                            "Value error, ActionPlan.steps item {r} is not a StepBase subclass. If authoring a custom \
                             step type, register it with @opendaisugi.step_type."
                        ),
                        v,
                    ),
                );
            }
        }
    }
    let Value::List(xs) = v else { unreachable!("checked above") };
    let mut out = Vec::with_capacity(xs.len());
    for (m, x) in ms.into_iter().zip(xs) {
        let Value::Obj(o) = x else { unreachable!("checked above") };
        let (y, errs) = m.take_fields(o, loc, Mode::Python);
        if !errs.is_empty() {
            return (Value::Null, errs);
        }
        out.push(y);
    }
    (Value::List(out), vec![])
}

/// `models.decode_dict_text`: text holding one dict, as JSON or as a
/// Python literal (the subset `literal::literal_eval` reads).
pub fn decode_dict_text(text: &str) -> Option<Object> {
    if text.chars().count() > 65_536 {
        return None;
    }
    let body = crate::gate::py::text::strip(text);
    if !body.starts_with('{') {
        return None;
    }
    if let Ok(Value::Obj(o)) = crate::gate::pyjson::loads_py(body, 900) {
        return Some(o);
    }
    match super::literal::literal_eval(body) {
        Some(Value::Obj(o)) => Some(o),
        _ => None,
    }
}

/// `ActionPlan.steps` as a model's reply is read. `coerce_step` runs over
/// every item first: a str that decodes to a dict of a registered type is
/// that dict, and a dict of a registered type validates as that step (its
/// errors take the field's place, with no item index). Then the
/// after-validator names the first item that is not a step.
fn validate_reply_steps(v: Value, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
    let xs = match &v {
        Value::List(xs) => xs,
        _ => {
            let msg = if mode == Mode::Json { "Input should be a valid array" } else { "Input should be a valid list" };
            return (Value::Null, err1(loc, "list_type", msg, v));
        }
    };
    let mut out: Vec<Result<Value, Value>> = Vec::with_capacity(xs.len());
    for x in xs {
        let mut item = x.clone();
        if let Value::Str(text) = x {
            if let Some(d) = decode_dict_text(text) {
                if d.get("type").and_then(|t| t.as_str()).is_some_and(|t| steps().get(t).is_some()) {
                    item = Value::Obj(d);
                }
            }
        }
        let m = match &item {
            Value::Obj(o) => o.get("type").and_then(|t| t.as_str()).and_then(|t| steps().get(t)),
            _ => None,
        };
        match (m, item) {
            (Some(m), Value::Obj(o)) => {
                let (y, errs) = m.take_fields(o, loc, Mode::Python);
                if !errs.is_empty() {
                    return (Value::Null, errs);
                }
                out.push(Ok(y));
            }
            (_, item) => out.push(Err(item)),
        }
    }
    let mut steps_out = Vec::with_capacity(out.len());
    for r in out {
        match r {
            Ok(y) => steps_out.push(y),
            Err(item) => {
                let r = py_repr(&item).unwrap_or_else(|_| "...".into());
                return (
                    Value::Null,
                    err1(
                        loc,
                        "value_error",
                        format!(
                            "Value error, ActionPlan.steps item {r} is not a StepBase subclass. If authoring a custom \
                             step type, register it with @opendaisugi.step_type."
                        ),
                        v,
                    ),
                );
            }
        }
    }
    (Value::List(steps_out), vec![])
}

impl Model {
    pub fn validate(&self, v: &Value, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
        self.take(v.clone(), loc, mode)
    }

    pub fn take(&self, v: Value, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
        match v {
            Value::Obj(o) => self.take_fields(o, loc, mode),
            _ => {
                let msg = if mode == Mode::Json {
                    "Input should be an object".to_string()
                } else {
                    format!("Input should be a valid dictionary or instance of {}", self.name)
                };
                (Value::Null, err1(loc, "model_type", msg, v))
            }
        }
    }

    /// The fields of a dict the validator keeps.
    pub fn fields(&self, o: &Object, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
        self.take_fields(o.clone(), loc, mode)
    }

    /// The fields of a dict, consuming it. A missing field reports the
    /// whole dict as its input.
    pub fn take_fields(&self, mut o: Object, loc: &Loc, mode: Mode) -> (Value, Vec<Err>) {
        let mut errs = vec![];
        let whole = if self.fields.iter().any(|f| f.default.is_none() && o.get(f.name).is_none()) {
            Some(Value::Obj(o.clone()))
        } else {
            None
        };
        let mut out = Object::with_capacity(self.fields.len());
        for f in &self.fields {
            match o.take_value(f.name) {
                None => match f.default {
                    None => errs.push(Err {
                        loc: Loc::Key(loc, f.name).to_vec(),
                        typ: "missing".into(),
                        msg: "Field required".into(),
                        input: whole.clone().unwrap_or(Value::Null),
                    }),
                    Some(d) => out.push_new(f.name.to_string(), d()),
                },
                Some(x) => {
                    let (y, e) = f.schema.take(x, &Loc::Key(loc, f.name), mode);
                    errs.extend(e);
                    out.push_new(f.name.to_string(), y);
                }
            }
        }
        let out = Value::Obj(out);
        if self.finite && errs.is_empty() {
            non_finite(&out, loc, &mut errs);
        }
        (out, errs)
    }
}

/// `models.non_finite_error`'s walk: one `finite_number` error for each
/// NaN, Infinity or -Infinity in a validated value, in document order.
pub fn non_finite(v: &Value, loc: &Loc, errs: &mut Vec<Err>) {
    match v {
        Value::Float(f) if !f.is_finite() => errs.push(Err {
            loc: loc.to_vec(),
            typ: "finite_number".into(),
            msg: "Input should be a finite number".into(),
            input: v.clone(),
        }),
        Value::List(l) => {
            for (i, e) in l.iter().enumerate() {
                non_finite(e, &Loc::Idx(loc, i), errs);
            }
        }
        Value::Obj(o) => {
            for k in o.keys() {
                non_finite(o.value(k), &Loc::Key(loc, k), errs);
            }
        }
        _ => {}
    }
}

/// A schema run on a value: the validated value or pydantic's error.
pub fn validate(title: &str, s: &Schema, v: &Value, mode: Mode) -> Result<Value, ValidationError> {
    take(title, s, v.clone(), mode)
}

/// `validate`, consuming the value.
pub fn take(title: &str, s: &Schema, v: Value, mode: Mode) -> Result<Value, ValidationError> {
    let (out, errs) = s.take(v, &Loc::Root, mode);
    if errs.is_empty() {
        Ok(out)
    } else {
        Err(ValidationError { title: title.into(), errs })
    }
}

/// A model run on a value.
pub fn validate_model(id: Id, v: &Value, mode: Mode) -> Result<Value, ValidationError> {
    take_model(id, v.clone(), mode)
}

/// `validate_model`, consuming the value.
pub fn take_model(id: Id, v: Value, mode: Mode) -> Result<Value, ValidationError> {
    let m = model(id);
    let (out, errs) = m.take(v, &Loc::Root, mode);
    if errs.is_empty() {
        Ok(out)
    } else {
        Err(ValidationError { title: m.name.into(), errs })
    }
}

/// jiter's parse of `text`, as `model_validate_json` reads it, or the
/// `json_invalid` error.
pub fn parse_json(title: &str, text: &str) -> Result<Value, ValidationError> {
    let bytes = text.as_bytes();
    match jiter::JsonValue::parse_with_config(bytes, true, jiter::PartialMode::Off) {
        Ok(v) => Ok(from_jiter(&v)),
        Err(e) => Err(ValidationError {
            title: title.into(),
            errs: vec![Err {
                loc: vec![],
                typ: "json_invalid".into(),
                msg: format!("Invalid JSON: {}", e.description(bytes)),
                input: Value::Str(text.to_string()),
            }],
        }),
    }
}

/// A value jiter parsed, as a Python value: a repeated key keeps its
/// first place and its last value, as a dict built from the pairs does.
fn from_jiter(v: &jiter::JsonValue) -> Value {
    use jiter::JsonValue as J;
    match v {
        J::Null => Value::Null,
        J::Bool(b) => Value::Bool(*b),
        J::Int(i) => Value::Int(i.to_string()),
        J::BigInt(b) => Value::Int(b.to_string()),
        J::Float(f) => Value::Float(*f),
        J::Str(s) => Value::Str(s.to_string()),
        J::Array(a) => Value::List(a.iter().map(from_jiter).collect()),
        J::Object(o) => {
            let mut out = Object::new();
            for (k, x) in o.iter() {
                out.set(k, from_jiter(x));
            }
            Value::Obj(out)
        }
    }
}

/// `Model.model_validate_json(text)`.
pub fn validate_json(id: Id, text: &str) -> Result<Value, ValidationError> {
    let v = parse_json(model(id).name, text)?;
    take_model(id, v, Mode::Json)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_missing_field_reads_as_pydantics() {
        let mut o = Object::new();
        o.set("id", "x");
        let e = validate_model(Id::PathwayParameter, &Value::Obj(o), Mode::Python).unwrap_err();
        assert!(e.text().starts_with("5 validation errors for PathwayParameter\nname\n  Field required [type=missing, input_value={'id': 'x'}, input_type=dict]"));
    }

    #[test]
    fn an_envelope_fills_its_defaults_in_order() {
        let text = r#"{"generated_by":"t","task":"t","permissions":{}}"#;
        let v = validate_json(Id::Envelope, text).unwrap();
        let o = v.as_obj().unwrap();
        let keys: Vec<&str> = o.keys().iter().map(|s| s.as_str()).collect();
        assert_eq!(keys[..4], ["id", "generated_by", "task", "permissions"]);
        assert_eq!(o.value("stakes"), &Value::Str("low".into()));
        assert!(o.value("id").as_str().unwrap().starts_with("env_"));
    }

    /// coerce_step runs over every item before the StepBase check: an
    /// invalid step after one that is no step is the error (the oracle's
    /// text for this reply).
    #[test]
    fn a_reply_names_the_invalid_step_first() {
        let text = r#"{"task_description": "t", "plan_template": {"source": "s", "task": "t", "steps": [{"type": "nope", "id": "x"}, {"type": "shell", "id": "a"}]}}"#;
        let v = parse_json("GeneralizedTemplate", text).unwrap();
        let e = take_model(Id::GeneralizedTemplate, v, Mode::Python).unwrap_err();
        let lines: Vec<String> = e.text().lines().take(3).map(|l| l.to_string()).collect();
        assert_eq!(
            lines,
            [
                "1 validation error for GeneralizedTemplate",
                "plan_template.steps.command",
                "  Field required [type=missing, input_value={'type': 'shell', 'id': 'a'}, input_type=dict]"
            ]
        );
        let text = r#"{"task_description": "t", "plan_template": {"source": "s", "task": "t", "steps": ["{'type': 'shell', 'id': 'a', 'command': 'ls'}"]}}"#;
        let v = parse_json("GeneralizedTemplate", text).unwrap();
        let o = take_model(Id::GeneralizedTemplate, v, Mode::Python).unwrap();
        let steps = o.as_obj().unwrap().value("plan_template").as_obj().unwrap().value("steps").clone();
        assert_eq!(crate::gate::pyjson::dumps(&steps, true).contains("\"command\": \"ls\""), true);
    }

    #[test]
    fn nan_in_an_envelope_is_invalid() {
        let text = r#"{"generated_by":"t","task":"t","permissions":{"velocity_limit":NaN}}"#;
        let e = validate_json(Id::Envelope, text).unwrap_err();
        assert!(e.text().contains("permissions.velocity_limit\n  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]"), "{}", e.text());
    }
}

