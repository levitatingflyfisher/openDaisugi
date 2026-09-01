//! `portability.export(pathway, fmt)` in all five formats.

use super::pathway::{json_mode, py_dumps_indent, Pathway};
use super::yamldump::safe_dump;
use super::PwErr;
use crate::gate::py::text::{is_alnum, lower, repr_list};
use crate::gate::pyjson::{py_float_repr, Object, Value};

/// `portability._SUPPORTED_FORMATS`, in its order.
pub const FORMATS: &[&str] = &["json", "skill", "mermaid", "md", "smtlib"];

/// `portability.BUNDLE_SCHEMA_VERSION`.
pub const BUNDLE_SCHEMA_VERSION: i64 = 1;

/// `export(pathway, fmt)`. `version` is what the bundle names as
/// opendaisugi_version.
pub fn export(p: &Pathway, format: &str, version: &str) -> Result<String, PwErr> {
    match format {
        "json" => {
            let bundle = bundle(p, version);
            Ok(py_dumps_indent(&bundle, 2))
        }
        "mermaid" => Ok(mermaid(p)),
        "md" => md(p),
        "smtlib" => Ok(smtlib(p, version)),
        "skill" => skill(p, version),
        _ => Err(PwErr::Invalid(format!("ValueError: unknown format {format:?}"))),
    }
}

fn bundle(p: &Pathway, version: &str) -> Value {
    let mut b = Object::new();
    b.set("opendaisugi_version", version);
    b.set("schema_version", Value::Int(BUNDLE_SCHEMA_VERSION.to_string()));
    b.set("pathway", json_mode(&Value::Obj(p.obj.clone())));
    Value::Obj(b)
}

fn obj(v: &Value) -> &Object {
    static EMPTY: std::sync::OnceLock<Object> = std::sync::OnceLock::new();
    v.as_obj().unwrap_or_else(|| EMPTY.get_or_init(Object::new))
}

fn str_list(v: &Value) -> Vec<String> {
    match v {
        Value::List(xs) => xs.iter().filter_map(|x| x.as_str().map(|s| s.to_string())).collect(),
        _ => vec![],
    }
}

/// `str(bool)`.
fn py_bool(v: &Value) -> &'static str {
    if matches!(v, Value::Bool(true)) {
        "True"
    } else {
        "False"
    }
}

/// `f"{xs or '[]'}"`: the list's repr, or [] when empty.
fn or_empty_list(v: &Value) -> String {
    let xs = str_list(v);
    if xs.is_empty() {
        "[]".into()
    } else {
        repr_list(&xs)
    }
}

/// `str(v)` for the plain values a step field holds.
fn py_str(v: &Value) -> String {
    match v {
        Value::Null => "None".into(),
        Value::Str(s) => s.clone(),
        Value::Int(t) => t.clone(),
        Value::Bool(_) => py_bool(v).into(),
        Value::Float(f) => py_float_repr(*f),
        other => crate::gate::pyjson::py_repr(other).unwrap_or_default(),
    }
}

/// `portability._step_detail`.
fn step_detail(step: &Object) -> String {
    let kind = step.value("type").as_str().unwrap_or("");
    let truthy = |k: &str| step.get(k).filter(|v| v.truthy());
    match kind {
        "task" => truthy("prompt").map(py_str).unwrap_or_default(),
        "skill" => truthy("skill_id").map(py_str).unwrap_or_default(),
        "mcp" => format!("{}/{}", py_str(step.value("server")), py_str(step.value("tool"))),
        _ => ["command", "path", "url"].iter().find_map(|k| truthy(k).map(py_str)).unwrap_or_default(),
    }
}

fn steps(p: &Pathway) -> Vec<&Object> {
    match obj(p.obj.value("plan_template")).value("steps") {
        Value::List(xs) => xs.iter().map(obj).collect(),
        _ => vec![],
    }
}

/// `s[:n]` in code points.
fn head(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

fn mermaid_escape(s: &str) -> String {
    head(&s.replace('"', "&quot;").replace('|', "\\|"), 120)
}

fn perms(p: &Pathway) -> &Object {
    obj(obj(p.obj.value("envelope")).value("permissions"))
}

fn mermaid(p: &Pathway) -> String {
    let mut lines: Vec<String> = vec!["```mermaid".into(), "flowchart TD".into()];
    for st in steps(p) {
        let id = st.value("id").as_str().unwrap_or("");
        let mut label = format!("{id}<br/>{}", st.value("type").as_str().unwrap_or(""));
        let d = step_detail(st);
        if !d.is_empty() {
            label.push_str(&format!("<br/><code>{}</code>", mermaid_escape(&d)));
        }
        lines.push(format!("    {id}[{label}]"));
    }
    for st in steps(p) {
        for dep in str_list(st.value("depends_on")) {
            lines.push(format!("    {dep} --> {}", st.value("id").as_str().unwrap_or("")));
        }
    }
    lines.push("```".into());
    let pm = perms(p);
    lines.extend([
        String::new(),
        "### Permissions".into(),
        format!("- shell: {} (allowlist: {})", py_bool(pm.value("shell")), or_empty_list(pm.value("shell_allowlist"))),
        format!("- file_read: {}", or_empty_list(pm.value("file_read"))),
        format!("- file_write: {}", or_empty_list(pm.value("file_write"))),
        format!("- network: {} (hosts: {})", py_bool(pm.value("network")), or_empty_list(pm.value("network_hosts"))),
    ]);
    lines.join("\n")
}

/// `time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime(t))`: the time
/// rounded down to a second.
fn gmtime(t: f64) -> Result<String, PwErr> {
    if !t.is_finite() || t.abs() > 253402300799.0 {
        return Err(PwErr::Invalid("OverflowError: time.gmtime cannot convert distilled_at".into()));
    }
    let sec = t.floor() as i64;
    let days = sec.div_euclid(86400);
    let rem = sec.rem_euclid(86400);
    let (y, m, d) = civil_from_days(days);
    if y < 1 {
        return Err(PwErr::Unreadable("distilled_at is before year 1".into()));
    }
    Ok(format!("{y:04}-{m:02}-{d:02} {:02}:{:02}:{:02} UTC", rem / 3600, rem % 3600 / 60, rem % 60))
}

/// Days since 1970-01-01 as a proleptic Gregorian date.
fn civil_from_days(z: i64) -> (i64, i64, i64) {
    let z = z + 719468;
    let era = z.div_euclid(146097);
    let doe = z.rem_euclid(146097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (if m <= 2 { y + 1 } else { y }, m, d)
}

fn md(p: &Pathway) -> Result<String, PwErr> {
    let env = obj(p.obj.value("envelope"));
    let pm = perms(p);
    let when = gmtime(match p.obj.value("distilled_at") {
        Value::Float(f) => *f,
        _ => 0.0,
    })?;
    let model = p.obj.value("embedding_model").as_str().unwrap_or("");
    let model = if model.is_empty() { "(unspecified)" } else { model };
    let n_traces = match p.obj.value("source_trace_ids") {
        Value::List(l) => l.len(),
        _ => 0,
    };
    let mut parts: Vec<String> = vec![
        format!("# Pathway: {}", p.task()),
        String::new(),
        format!("- **ID:** `{}`", p.id()),
        format!("- **Distilled at:** {when}"),
        format!("- **Hit count:** {}", py_str(p.obj.value("hit_count"))),
        format!("- **Source traces:** {n_traces}"),
        format!("- **Embedding model:** {model}"),
        String::new(),
        "## Envelope".into(),
        String::new(),
        format!("- **Generator:** {}", env.value("generated_by").as_str().unwrap_or("")),
        "- **Permissions:**".into(),
        format!("  - shell: {} (allowlist: {})", py_bool(pm.value("shell")), or_empty_list(pm.value("shell_allowlist"))),
        format!("  - file_read: {}", or_empty_list(pm.value("file_read"))),
        format!("  - file_write: {}", or_empty_list(pm.value("file_write"))),
        format!("  - network: {} (hosts: {})", py_bool(pm.value("network")), or_empty_list(pm.value("network_hosts"))),
    ];
    if let Value::List(invs) = env.value("invariants") {
        if !invs.is_empty() {
            parts.push("- **Invariants:**".into());
            for i in invs {
                let inv = obj(i);
                parts.push(format!("  - `{}`: {}", py_str(inv.value("type")), py_str(inv.value("description"))));
            }
        }
    }
    if let Value::List(pcs) = env.value("postconditions") {
        if !pcs.is_empty() {
            parts.push("- **Postconditions:**".into());
            for x in pcs {
                let pc = obj(x);
                parts.push(format!(
                    "  - `{}` → path={} expected={}",
                    py_str(pc.value("type")),
                    py_str(pc.value("path")),
                    py_str(pc.value("expected"))
                ));
            }
        }
    }
    parts.extend([String::new(), "## Plan template".into(), String::new()]);
    for st in steps(p) {
        parts.push(format!(
            "- `{}` ({}): `{}`",
            st.value("id").as_str().unwrap_or(""),
            st.value("type").as_str().unwrap_or(""),
            step_detail(st)
        ));
        let deps = str_list(st.value("depends_on"));
        if !deps.is_empty() {
            parts.push(format!("  - depends on: {}", deps.join(", ")));
        }
    }
    Ok(parts.join("\n") + "\n")
}

/// `portability._export_smtlib`: a comment header, then what z3py's
/// `Solver.to_smt2()` prints for the assertions it adds.
fn smtlib(p: &Pathway, version: &str) -> String {
    let env = obj(p.obj.value("envelope"));
    let pm = perms(p);
    let mut b = format!(
        ";; openDaisugi pathway proof artifact\n;; pathway_id: {}\n;; task: {}\n;; envelope_id: {}\n;; opendaisugi_version: {version}\n",
        p.id(),
        p.task(),
        env.value("id").as_str().unwrap_or("")
    );
    b.push_str("; benchmark generated from python API\n(set-info :status unknown)\n");
    b.push_str("(declare-fun shell () Bool)\n(declare-fun can_write () Bool)\n");
    let lower = |v: bool| if v { "true" } else { "false" };
    let mut assert = |name: &str, v: bool| b.push_str(&format!("(assert\n (= {name} {}))\n", lower(v)));
    let non_empty = |v: &Value| matches!(v, Value::List(l) if !l.is_empty());
    assert("shell", matches!(pm.value("shell"), Value::Bool(true)));
    assert("can_write", non_empty(pm.value("file_write")));
    if non_empty(pm.value("shell_allowlist")) {
        assert("shell", true);
    }
    if let Value::List(pcs) = env.value("postconditions") {
        for x in pcs {
            if obj(x).value("type").as_str() == Some("file_exists") {
                assert("can_write", true);
            }
        }
    }
    b.push_str("(check-sat)\n");
    b
}

/// `portability._export_skill`: YAML frontmatter as yaml.safe_dump
/// writes it, then the skill body and its inputs.
fn skill(p: &Pathway, version: &str) -> Result<String, PwErr> {
    let mut front = Object::new();
    front.set("name", slug(p.task()));
    front.set("description", p.task());
    front.set("daisugi", bundle(p, version));
    let head = safe_dump(&Value::Obj(front)).map_err(|u| PwErr::Unreadable(u.0))?;
    Ok(format!("---\n{head}---\n\n{}\n\n{}", skill_body(p), inputs_md(p)))
}

/// `portability._slug`: `str.isalnum()` characters kept, every other one
/// a dash, runs of dashes made one, 60 code points at most.
pub fn slug(text: &str) -> String {
    let s: String = lower(text).chars().map(|c| if is_alnum(c) { c } else { '-' }).collect();
    let mut s = s.trim_matches('-').to_string();
    while s.contains("--") {
        s = s.replace("--", "-");
    }
    let s = head(&s, 60);
    if s.is_empty() {
        "pathway".into()
    } else {
        s
    }
}

fn skill_body(p: &Pathway) -> String {
    let pm = perms(p);
    let lines = [
        format!("# {}", p.task()),
        String::new(),
        "A Z3-verified pathway compiled from successful journal traces by openDaisugi.".into(),
        "On import, the plan template is re-verified against the envelope declared".into(),
        "in this skill's frontmatter; imports fail closed if verification fails.".into(),
        String::new(),
        "## What this pathway does".into(),
        String::new(),
        p.task().to_string(),
        String::new(),
        "## Verified permissions".into(),
        String::new(),
        format!("- shell: `{}` (allowlist: `{}`)", py_bool(pm.value("shell")), or_empty_list(pm.value("shell_allowlist"))),
        format!("- file_read: `{}`", or_empty_list(pm.value("file_read"))),
        format!("- file_write: `{}`", or_empty_list(pm.value("file_write"))),
        format!("- network: `{}` (hosts: `{}`)", py_bool(pm.value("network")), or_empty_list(pm.value("network_hosts"))),
        String::new(),
        "## Usage".into(),
        String::new(),
        "Install with:".into(),
        String::new(),
        "```bash".into(),
        "daisugi pathways import path/to/this-skill.md".into(),
        "```".into(),
        String::new(),
        "openDaisugi will re-verify the plan template against the envelope above.".into(),
        "If verification passes, the pathway is admitted to the local PathwayStore".into(),
        "and becomes a Tier-0 cache hit for matching tasks.".into(),
        String::new(),
        "## Graduating this into a polished skill".into(),
        String::new(),
        "openDaisugi *distilled* this from real successes; it doesn't author".into(),
        "triggering descriptions or evals. To turn it into a durable, well-triggering".into(),
        "skill, hand this file to a skill-authoring tool (e.g. `skill-creator`): keep".into(),
        "the verified plan + envelope, and let it add the name, description, and evals.".into(),
    ];
    lines.join("\n") + "\n"
}

/// `portability._render_inputs_md`.
fn inputs_md(p: &Pathway) -> String {
    let params = match p.obj.value("parameters") {
        Value::List(l) => l,
        _ => return String::new(),
    };
    if params.is_empty() {
        return "## Inputs\n\nNone — this is a **fixed** pathway. When its steps are shell/file/network it runs \
                verbatim with zero inference; reuse just replays it."
            .into();
    }
    let mut lines: Vec<String> = vec![
        "## Inputs".into(),
        String::new(),
        "A **typed skill**: reuse binds these holes for the specific task, then re-verifies the concrete plan \
         against your envelope. A bound value may change the data, never the capability head."
            .into(),
        String::new(),
    ];
    for x in params {
        let po = obj(x);
        let obs = str_list(po.value("observed"));
        let ex: Vec<String> = obs.iter().take(3).map(|v| format!("`{v}`")).collect();
        let examples = if ex.is_empty() { "—".to_string() } else { ex.join(", ") };
        lines.push(format!(
            "- **{}** — fills the `{}` of a `{}` step; e.g. {examples}",
            po.value("name").as_str().unwrap_or(""),
            po.value("field").as_str().unwrap_or(""),
            po.value("head").as_str().unwrap_or("")
        ));
    }
    lines.join("\n")
}
